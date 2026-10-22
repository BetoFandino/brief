package tools

import (
	"context"
	"fmt"
	"sort"

	"github.com/BetoFandino/brief/internal/analyzer"
	"github.com/BetoFandino/brief/internal/cache"
	"github.com/BetoFandino/brief/internal/envelope"
	"github.com/BetoFandino/brief/internal/projection"
	"github.com/BetoFandino/brief/internal/rbac"
)

// TriageCluster answers "what is broken right now?"
//
// It runs every analyzer over every watched Kind and returns the findings
// grouped by cause. Without the graph it cannot yet collapse a hundred symptoms
// into the one node that caused them — that is v0.2 — so it groups by reason
// code, which is the honest version of the same idea.
type TriageCluster struct{}

func (TriageCluster) Name() string { return "triage_cluster" }

func (TriageCluster) Description() string {
	return "List what is broken right now, grouped by cause, with the worst first. Optionally scoped to a namespace and a minimum severity."
}

// Triage reads the whole cluster, so it is allowed more than a single-workload
// answer — and still a hard ceiling.
func (TriageCluster) Budget() int { return 8000 }

func (t TriageCluster) Call(ctx context.Context, d Deps, r Request) (*envelope.Envelope, error) {
	if err := r.requireCaller(); err != nil {
		return nil, err
	}
	namespace := r.str("namespace")
	floor := severityFloor(r.strOr("min_severity", "low"))

	e, err := newEnvelope(d, t)
	if err != nil {
		return nil, err
	}
	if namespace != "" {
		e.Target(projection.ID("ns/" + namespace))
	}

	var (
		findings  []envelope.Finding
		inspected int
		subjects  []projection.Object
	)

	for _, kind := range d.Analyzers.Kinds() {
		gvk, known := wellKnownGVK[kind]
		if !known {
			continue
		}
		objs, err := d.Cache.List(ctx, cache.Query{
			GVK: gvk, Namespace: namespace, Profile: projection.Diagnostic,
		})
		if err != nil {
			continue
		}

		// Authorize before analyzing, not after. Running analyzers over objects
		// the caller cannot read would put their contents in the evidence of a
		// finding, which is the same leak by a longer route.
		allowed, err := rbac.Filter(ctx, r.Caller, "get", objs)
		if err != nil {
			return nil, err
		}

		related := neighbourhood(ctx, d, d.Analyzers.NeedsFor(kind), namespace)
		for _, obj := range allowed.Objects() {
			inspected++
			got := d.Analyzers.Run(analyzer.Input{Object: obj, Related: related})
			if len(got) == 0 {
				continue
			}
			for _, f := range got {
				if severityRank(f.Severity) <= floor {
					findings = append(findings, f)
				}
			}
			subjects = append(subjects, obj)
		}
	}

	grouped := groupByCause(findings)
	for _, f := range grouped {
		if err := e.AddFinding(f); err != nil {
			return nil, err
		}
	}

	// Only the objects a finding actually names are worth their tokens here.
	// Triage is a verdict, not a listing.
	named := map[string]bool{}
	for _, f := range grouped {
		for _, id := range f.Objects {
			named[id] = true
		}
	}
	var shown []projection.Object
	for _, obj := range subjects {
		if named[string(obj.ID)] {
			shown = append(shown, obj)
		}
	}
	filtered, err := rbac.Filter(ctx, r.Caller, "get", shown)
	if err != nil {
		return nil, err
	}
	e.SetObjects(filtered)

	e.Summary(summarizeTriage(inspected, grouped, namespace))
	t.hint(e, grouped)
	return e, nil
}

// groupByCause collapses findings that share a reason code into one finding
// naming every affected object. Twelve pods OOM-killed by the same limit is one
// cause, not twelve answers.
func groupByCause(findings []envelope.Finding) []envelope.Finding {
	type group struct {
		f    envelope.Finding
		objs []string
		seen map[string]bool
	}
	order := []envelope.Code{}
	groups := map[envelope.Code]*group{}

	for _, f := range findings {
		g, exists := groups[f.Code]
		if !exists {
			g = &group{f: f, seen: map[string]bool{}}
			groups[f.Code] = g
			order = append(order, f.Code)
		}
		if severityRank(f.Severity) < severityRank(g.f.Severity) {
			g.f.Severity = f.Severity
		}
		for _, id := range f.Objects {
			if !g.seen[id] {
				g.seen[id] = true
				g.objs = append(g.objs, id)
			}
		}
	}

	out := make([]envelope.Finding, 0, len(order))
	for _, code := range order {
		g := groups[code]
		f := g.f
		sort.Strings(g.objs)
		f.Objects = g.objs
		if len(g.objs) > 1 {
			// The shared evidence of the first instance would be misleading
			// across many objects; the count is the fact that holds.
			f.Evidence = map[string]any{"affected_objects": len(g.objs), "example": g.f.Evidence}
		}
		out = append(out, f)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return severityRank(out[i].Severity) < severityRank(out[j].Severity)
	})
	return out
}

func summarizeTriage(inspected int, grouped []envelope.Finding, namespace string) string {
	scope := "cluster-wide"
	if namespace != "" {
		scope = "namespace " + namespace
	}
	if len(grouped) == 0 {
		return fmt.Sprintf("%s: nothing diagnosed across %d objects", scope, inspected)
	}
	affected := 0
	for _, f := range grouped {
		affected += len(f.Objects)
	}
	return fmt.Sprintf("%s: %d distinct causes affecting %d objects, out of %d inspected; worst is %s",
		scope, len(grouped), affected, inspected, grouped[0].Code)
}

func (TriageCluster) hint(e *envelope.Envelope, grouped []envelope.Finding) {
	for _, f := range grouped {
		if len(f.Objects) == 0 {
			continue
		}
		ns, kind, name := splitID(f.Objects[0])
		if kind == "" {
			continue
		}
		_ = e.AddHint(envelope.Hint{Tool: "explain_workload", Args: map[string]any{
			"namespace": ns, "name": name, "kind": kind,
		}})
		return
	}
}

// splitID reverses projection.NewID.
func splitID(id string) (namespace, kind, name string) {
	parts := splitN(id, '/')
	switch len(parts) {
	case 3:
		return parts[0], parts[1], parts[2]
	case 2:
		return "", parts[0], parts[1]
	}
	return "", "", ""
}

func splitN(s string, sep byte) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == sep {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

// severityFloor turns a min_severity argument into a rank cutoff.
func severityFloor(s string) int {
	switch envelope.Severity(s) {
	case envelope.SeverityCritical:
		return 0
	case envelope.SeverityHigh:
		return 1
	case envelope.SeverityMedium:
		return 2
	default:
		return 3
	}
}
