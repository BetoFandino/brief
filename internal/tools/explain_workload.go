package tools

import (
	"context"
	"fmt"

	"github.com/BetoFandino/brief/internal/analyzer"
	"github.com/BetoFandino/brief/internal/cache"
	"github.com/BetoFandino/brief/internal/envelope"
	"github.com/BetoFandino/brief/internal/projection"
	"github.com/BetoFandino/brief/internal/rbac"
)

// ExplainWorkload answers "what is wrong with this?" in one call.
//
// It replaces the seven-to-twelve hop walk a human runs — deployment, then
// replicaset, then pods, then events, then logs — and returns the projection,
// the replica state, the findings and the aggregated events together.
type ExplainWorkload struct{}

func (ExplainWorkload) Name() string { return "explain_workload" }

func (ExplainWorkload) Description() string {
	return "Explain what is wrong with a workload: its state, the diagnosed causes with evidence, and its aggregated events. Replaces walking deployment to replicaset to pods to events by hand."
}

func (ExplainWorkload) Budget() int { return 4000 }

func (t ExplainWorkload) Call(ctx context.Context, d Deps, r Request) (*envelope.Envelope, error) {
	if err := r.requireCaller(); err != nil {
		return nil, err
	}
	namespace := r.str("namespace")
	name := r.str("name")
	if namespace == "" || name == "" {
		return nil, fmt.Errorf("explain_workload: namespace and name are required")
	}
	kind := r.strOr("kind", "Deployment")
	gvk, known := wellKnownGVK[kind]
	if !known {
		return nil, fmt.Errorf("explain_workload: kind %q is not watched; use list_objects for an arbitrary GVK", kind)
	}

	e, err := newEnvelope(d, t)
	if err != nil {
		return nil, err
	}

	workload, err := cache.Get(ctx, d.Cache, cache.Query{
		GVK: gvk, Namespace: namespace, Name: name, Profile: projection.Diagnostic,
	})
	if err != nil {
		return nil, err
	}
	e.Target(workload.ID)

	// Everything the response carries passes authorization first. The workload
	// itself included: an unauthorized caller gets an empty answer, not a
	// projection of something they cannot read.
	filtered, err := rbac.Filter(ctx, r.Caller, "get", []projection.Object{workload})
	if err != nil {
		return nil, err
	}
	if filtered.Len() == 0 {
		e.Summary(fmt.Sprintf("not authorized to read %s in namespace %s", kind, namespace))
		e.SetObjects(filtered)
		return e, nil
	}

	findings := d.Analyzers.Run(analyzer.Input{
		Object:  workload,
		Related: neighbourhood(ctx, d, d.Analyzers.NeedsFor(kind), namespace),
	})

	// The workload's own pods, found through its selector — the hop the agent
	// would otherwise spend a call on.
	pods := t.podsOf(ctx, d, workload, namespace)
	podNeeds := neighbourhood(ctx, d, d.Analyzers.NeedsFor("Pod"), namespace)
	for _, pod := range pods {
		findings = append(findings, d.Analyzers.Run(analyzer.Input{Object: pod, Related: podNeeds})...)
	}

	for _, f := range findings {
		if err := e.AddFinding(f); err != nil {
			return nil, err
		}
	}

	names := map[string]bool{workload.Name: true}
	for _, p := range pods {
		names[p.Name] = true
	}
	if events, err := d.Cache.List(ctx, cache.Query{
		GVK: wellKnownGVK["Event"], Namespace: namespace, Profile: projection.Diagnostic,
	}); err == nil {
		e.AddEvents(aggregateEvents(events, names)...)
	}

	shown := append([]projection.Object{}, filtered.Objects()...)
	if podsFiltered, err := rbac.Filter(ctx, r.Caller, "get", pods); err == nil {
		shown = append(shown, podsFiltered.Objects()...)
	}
	combined, err := rbac.Filter(ctx, r.Caller, "get", shown)
	if err != nil {
		return nil, err
	}
	e.SetObjects(combined)

	e.Summary(summarizeWorkload(workload, pods, findings))
	t.hint(e, workload, findings)
	return e, nil
}

// podsOf finds the pods a workload owns, by its selector labels.
func (ExplainWorkload) podsOf(ctx context.Context, d Deps, workload projection.Object, namespace string) []projection.Object {
	sel, ok := workload.Value("spec.selector.matchLabels")
	if !ok {
		return nil
	}
	raw, isMap := sel.(map[string]any)
	if !isMap || len(raw) == 0 {
		return nil
	}
	labels := make(map[string]string, len(raw))
	for k, v := range raw {
		if s, isString := v.(string); isString {
			labels[k] = s
		}
	}

	pods, err := d.Cache.List(ctx, cache.Query{
		GVK: wellKnownGVK["Pod"], Namespace: namespace,
		Labels: labels, Profile: projection.Diagnostic,
	})
	if err != nil {
		return nil
	}
	return pods
}

func summarizeWorkload(workload projection.Object, pods []projection.Object, findings []envelope.Finding) string {
	want := workload.IntOr("spec.replicas", 1)
	ready := workload.IntOr("status.readyReplicas", 0)

	if len(findings) == 0 {
		return fmt.Sprintf("%d/%d replicas ready; nothing diagnosed", ready, want)
	}

	// The counts and the cause come from the server. The model writes the prose
	// around them; it is never asked to derive them.
	worst := findings[0]
	for _, f := range findings[1:] {
		if severityRank(f.Severity) < severityRank(worst.Severity) {
			worst = f
		}
	}
	description, _ := envelope.Describe(worst.Code)
	return fmt.Sprintf("%d/%d replicas ready across %d pods; worst finding %s (%s)",
		ready, want, len(pods), worst.Code, description)
}

func severityRank(s envelope.Severity) int {
	switch s {
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

// hint pre-fills the next useful call. Steering the walk from the server costs
// almost nothing and keeps that logic out of the user's prompt.
func (ExplainWorkload) hint(e *envelope.Envelope, workload projection.Object, findings []envelope.Finding) {
	for _, f := range findings {
		switch f.Code {
		case envelope.CodeContainerOOMKilled, envelope.CodeReplicasUnavailable:
			_ = e.AddHint(envelope.Hint{Tool: "triage_cluster", Args: map[string]any{
				"namespace": workload.Namespace, "min_severity": "high",
			}})
			return
		case envelope.CodePodUnschedulable:
			_ = e.AddHint(envelope.Hint{Tool: "list_objects", Args: map[string]any{
				"gvk": "v1/Node", "profile": "diagnostic",
			}})
			return
		}
	}
}
