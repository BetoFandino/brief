// Package analyzer turns projected objects into findings with normalized reason
// codes.
//
// This is where the diagnostics proper live. Every analyzer computes its own
// evidence — exit codes, restart counts, the comparison between a limit and
// actual usage — because handing the model inputs to compute from is how a
// diagnosis fails plausibly, which is the worst way for it to fail
// (principle 4).
//
// v0.1 analyzers are compiled in. They are nonetheless registered and run
// through the same path a declarative CEL analyzer will use in v1, because an
// internal shortcut for the project's own analyzers would make principle 7 — a
// new analyzer is addable without forking — quietly false.
package analyzer

import (
	"sort"

	"github.com/BetoFandino/brief/internal/envelope"
	"github.com/BetoFandino/brief/internal/projection"
)

// Input is one object plus the neighbourhood its analyzer asked for.
//
// Related is keyed by Kind. In v0.1 the runner fills it from the same namespace;
// in v0.2 it comes from the resource graph. Analyzers are written against this
// shape so that change does not reach them.
type Input struct {
	Object  projection.Object
	Related map[string][]projection.Object
}

// Analyzer examines one object and reports what is wrong with it.
type Analyzer interface {
	// Name identifies the analyzer in logs and metrics.
	Name() string
	// Kind is the object Kind it applies to.
	Kind() string
	// Needs lists neighbouring Kinds the analyzer reads. The runner is free to
	// skip an analyzer whose needs it cannot satisfy rather than feed it a
	// half-populated neighbourhood.
	Needs() []string
	// Analyze returns findings, or nothing when the object is healthy.
	Analyze(Input) []envelope.Finding
}

// Registry holds the analyzers to run.
type Registry struct {
	byKind map[string][]Analyzer
}

func NewRegistry(as ...Analyzer) *Registry {
	r := &Registry{byKind: map[string][]Analyzer{}}
	for _, a := range as {
		r.Add(a)
	}
	return r
}

// Default is the v0.1 analyzer set: the failures the three v0.1 tools can reach
// without the graph or the history layer.
func Default() *Registry {
	return NewRegistry(
		podContainers{},
		podScheduling{},
		workloadReplicas{},
		pvcBinding{},
		serviceEndpoints{},
	)
}

func (r *Registry) Add(a Analyzer) { r.byKind[a.Kind()] = append(r.byKind[a.Kind()], a) }

// Kinds lists the Kinds any analyzer cares about, so a caller knows what to
// feed the registry without hardcoding the list.
func (r *Registry) Kinds() []string {
	out := make([]string, 0, len(r.byKind))
	for k := range r.byKind {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// NeedsFor lists the neighbouring Kinds the analyzers for a Kind read.
func (r *Registry) NeedsFor(kind string) []string {
	seen := map[string]bool{}
	var out []string
	for _, a := range r.byKind[kind] {
		for _, need := range a.Needs() {
			if !seen[need] {
				seen[need] = true
				out = append(out, need)
			}
		}
	}
	sort.Strings(out)
	return out
}

// Run analyzes one object with every analyzer registered for its Kind.
func (r *Registry) Run(in Input) []envelope.Finding {
	var out []envelope.Finding
	for _, a := range r.byKind[in.Object.Kind] {
		out = append(out, a.Analyze(in)...)
	}
	return out
}
