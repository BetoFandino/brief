package tools

import (
	"context"
	"fmt"

	"github.com/BetoFandino/brief/internal/cache"
	"github.com/BetoFandino/brief/internal/envelope"
	"github.com/BetoFandino/brief/internal/projection"
	"github.com/BetoFandino/brief/internal/rbac"
)

// ListObjects is the escape hatch.
//
// It exists so the agent is not stuck when a question fits no intent tool, and
// it is deliberately the least convenient tool to reach for: it demands a GVK,
// it defaults to the minimal profile, it refuses the full profile outright, and
// its description says to prefer the intent tools. A surface where this is the
// easy path has handed the model the walk that brief exists to perform.
type ListObjects struct{}

func (ListObjects) Name() string { return "list_objects" }

func (ListObjects) Description() string {
	return "Escape hatch: list objects of one GVK with optional namespace and label filters. Prefer explain_workload or triage_cluster — they answer the question instead of handing back rows to interpret."
}

func (ListObjects) Budget() int { return 6000 }

// maxListLimit caps a listing however large a caller asks for, because the
// response ceiling is not negotiable and failing at the budget is worse than
// returning a bounded page with a cursor.
const maxListLimit = 500

func (t ListObjects) Call(ctx context.Context, d Deps, r Request) (*envelope.Envelope, error) {
	if err := r.requireCaller(); err != nil {
		return nil, err
	}
	gvk := r.str("gvk")
	if gvk == "" {
		return nil, fmt.Errorf("list_objects: gvk is required, e.g. v1/Pod or apps/v1/Deployment")
	}

	profile := projection.Profile(r.strOr("profile", string(projection.Minimal)))
	switch profile {
	case projection.Minimal, projection.Diagnostic:
	case projection.Full:
		// A listing never uses full. One object at a time may.
		if r.str("name") == "" {
			return nil, fmt.Errorf("list_objects: profile %q is only for a single object; pass name, or use %q",
				projection.Full, projection.Diagnostic)
		}
	default:
		return nil, fmt.Errorf("list_objects: unknown profile %q", profile)
	}

	limit := r.intOr("limit", 100)
	if limit <= 0 || limit > maxListLimit {
		limit = maxListLimit
	}

	e, err := newEnvelope(d, t)
	if err != nil {
		return nil, err
	}

	objs, err := d.Cache.List(ctx, cache.Query{
		GVK: gvk, Namespace: r.str("namespace"), Name: r.str("name"),
		Labels: r.labels("labels"), Profile: profile, Limit: limit,
	})
	if err != nil {
		return nil, err
	}

	filtered, err := rbac.Filter(ctx, r.Caller, "list", objs)
	if err != nil {
		return nil, err
	}
	e.SetObjects(filtered)

	scope := "cluster-wide"
	if ns := r.str("namespace"); ns != "" {
		scope = "namespace " + ns
	}
	e.Summary(fmt.Sprintf("%d %s in %s, %s profile", filtered.Len(), gvk, scope, profile))

	// Point back at the tools that would have answered the question, so the
	// escape hatch does not quietly become the default path.
	_ = e.AddHint(envelope.Hint{Tool: "triage_cluster", Args: map[string]any{
		"namespace": r.str("namespace"),
	}})
	return e, nil
}
