// Package tools is brief's MCP surface: diagnostic questions, not API verbs.
//
// Every tool returns an *envelope.Envelope and nothing else, which is what
// keeps the response contract — freshness, the token ceiling, RBAC filtering —
// out of each tool's hands and in one place.
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

// Deps is what every tool reads from. Narrow by construction: a tool gets the
// cache, the analyzers and the caller's authorization, and nothing that would
// let it reach the API server on its own.
type Deps struct {
	Cache     cache.Reader
	Analyzers *analyzer.Registry
	// Counter measures responses against their budget. Nil uses the heuristic;
	// the benchmark supplies a real tokenizer.
	Counter envelope.TokenCounter
}

// Request is one tool call.
type Request struct {
	Args map[string]any
	// Caller authorizes every read on behalf of whoever invoked the tool. A nil
	// Caller is refused rather than defaulted: serving with the service's own
	// permissions has to be an explicit choice (see rbac.ServiceAccountOnly).
	Caller rbac.Checker
}

// Tool answers one diagnostic question.
type Tool interface {
	Name() string
	Description() string
	// Budget is the tool's token ceiling. Declared per tool, never inherited,
	// so that adding a tool means stating what it is allowed to spend.
	Budget() int
	Call(ctx context.Context, d Deps, r Request) (*envelope.Envelope, error)
}

// Registry holds the surface.
type Registry struct {
	byName map[string]Tool
	order  []string
}

// Default is the v0.1 surface: three tools, no graph, no history.
func Default() *Registry {
	return NewRegistry(ExplainWorkload{}, TriageCluster{}, ListObjects{})
}

func NewRegistry(ts ...Tool) *Registry {
	r := &Registry{byName: map[string]Tool{}}
	for _, t := range ts {
		r.byName[t.Name()] = t
		r.order = append(r.order, t.Name())
	}
	sort.Strings(r.order)
	return r
}

func (r *Registry) Get(name string) (Tool, bool) {
	t, ok := r.byName[name]
	return t, ok
}

func (r *Registry) Names() []string { return r.order }

func (r *Registry) All() []Tool {
	out := make([]Tool, 0, len(r.order))
	for _, n := range r.order {
		out = append(out, r.byName[n])
	}
	return out
}

// newEnvelope starts a response from the cache's own freshness. No tool builds
// one from time.Now(): the server answers from a cache that lies for a few
// hundred milliseconds, and the agent has to know which moment it is reading.
func newEnvelope(d Deps, t Tool) (*envelope.Envelope, error) {
	snap := d.Cache.Snapshot()
	return envelope.New(snap.AsOf, snap.Lag, envelope.Budget{
		Tokens: t.Budget(), Counter: d.Counter,
	})
}

func (r Request) str(key string) string {
	s, _ := r.Args[key].(string)
	return s
}

func (r Request) strOr(key, fallback string) string {
	if s := r.str(key); s != "" {
		return s
	}
	return fallback
}

func (r Request) intOr(key string, fallback int) int {
	switch n := r.Args[key].(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	}
	return fallback
}

func (r Request) labels(key string) map[string]string {
	raw, ok := r.Args[key].(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		if s, isString := v.(string); isString {
			out[k] = s
		}
	}
	return out
}

func (r Request) requireCaller() error {
	if r.Caller == nil {
		return fmt.Errorf("tools: no caller authorization; pass a rbac.Checker, or rbac.ServiceAccountOnly explicitly")
	}
	return nil
}

// neighbourhood fetches the related Kinds a set of analyzers declares it reads.
//
// In v0.1 "related" means the same namespace, which is a coarse stand-in for
// the resource graph. Analyzers are written against the Related map, so when
// v0.2 replaces this with real edges, none of them change.
func neighbourhood(ctx context.Context, d Deps, kinds []string, namespace string) map[string][]projection.Object {
	if len(kinds) == 0 {
		return nil
	}
	out := map[string][]projection.Object{}
	for _, kind := range kinds {
		gvk, known := wellKnownGVK[kind]
		if !known {
			continue
		}
		objs, err := d.Cache.List(ctx, cache.Query{
			GVK: gvk, Namespace: namespace, Profile: projection.Diagnostic,
		})
		if err != nil {
			// A neighbourhood that cannot be read degrades the finding's
			// evidence; it does not fail the answer the agent asked for.
			continue
		}
		out[kind] = objs
	}
	return out
}

// wellKnownGVK maps the Kinds the v0.1 analyzers reference to the GVK the cache
// is configured by. It mirrors the default watch.gvks of values.yaml.
var wellKnownGVK = map[string]string{
	"Pod":                   "v1/Pod",
	"Node":                  "v1/Node",
	"Service":               "v1/Service",
	"Event":                 "v1/Event",
	"PersistentVolumeClaim": "v1/PersistentVolumeClaim",
	"EndpointSlice":         "discovery.k8s.io/v1/EndpointSlice",
	"Deployment":            "apps/v1/Deployment",
	"ReplicaSet":            "apps/v1/ReplicaSet",
	"StatefulSet":           "apps/v1/StatefulSet",
	"DaemonSet":             "apps/v1/DaemonSet",
}

// aggregateEvents collapses events into counted reasons. A raw event list is
// mostly the same line repeated, which is the kind of padding this project
// exists to remove.
func aggregateEvents(objs []projection.Object, involving map[string]bool) []envelope.Event {
	type agg struct {
		count       int
		first, last string
	}
	byReason := map[string]*agg{}

	for _, e := range objs {
		if involving != nil {
			inv, _ := e.Value("involvedObject")
			m, _ := inv.(map[string]any)
			if !involving[projection.Field(m, "name")] {
				continue
			}
		}
		reason := e.Str("reason")
		if reason == "" {
			continue
		}
		count := int(e.IntOr("count", 1))
		a, seen := byReason[reason]
		if !seen {
			a = &agg{first: e.Str("firstTimestamp")}
			byReason[reason] = a
		}
		a.count += count
		if last := e.Str("lastTimestamp"); last > a.last {
			a.last = last
		}
		if first := e.Str("firstTimestamp"); first != "" && (a.first == "" || first < a.first) {
			a.first = first
		}
	}

	out := make([]envelope.Event, 0, len(byReason))
	for reason, a := range byReason {
		out = append(out, envelope.Event{Reason: reason, Count: a.count, First: a.first, Last: a.last})
	}
	// Loudest first: the reason repeated forty times is the one worth the tokens.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Reason < out[j].Reason
	})
	return out
}
