// Package envelope is the single way a tool returns data.
//
// Three of the seven design principles hold because of this type rather than
// because a checklist repeats them:
//
//   - Principle 6, explicit freshness: as_of and the watch lag are constructor
//     arguments. There is no path to an envelope without them.
//   - Principle 3, hard token budget: the ceiling is enforced in Marshal, the
//     only serialization point. A tool cannot marshal around it.
//   - Principle 5, the caller's RBAC rules: objects enter only as
//     rbac.FilteredObjects, whose contents no other package can populate.
//
// Moving those three out of prose and into this type is the whole point. A rule
// a tool author has to remember is a rule that gets lost in the next rewrite of
// the tool surface, and that surface is expected to be rewritten several times.
package envelope

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/BetoFandino/brief/internal/projection"
	"github.com/BetoFandino/brief/internal/rbac"
)

// MaxHints caps next_hints. Three is the documented ceiling: hints steer the
// agent's next hop cheaply, but a long list is just another way to spend the
// context the response is trying to save.
const MaxHints = 3

// TokenCounter measures a response against the budget.
type TokenCounter interface {
	Count(b []byte) int
}

// HeuristicCounter approximates four bytes per token.
//
// It is deliberately an approximation with a seam: the v0.1 benchmark swaps in
// a real tokenizer, because the entire thesis of the project rests on that
// number and an invented one proves nothing.
type HeuristicCounter struct{}

func (HeuristicCounter) Count(b []byte) int { return (len(b) + 3) / 4 }

// Budget is a tool's token ceiling.
type Budget struct {
	Tokens  int
	Counter TokenCounter
}

func (b Budget) count(raw []byte) int {
	if b.Counter == nil {
		return HeuristicCounter{}.Count(raw)
	}
	return b.Counter.Count(raw)
}

// Event is an aggregated Kubernetes event: counted and collapsed, never a raw
// list of near-identical lines.
type Event struct {
	Reason string `json:"reason"`
	Count  int    `json:"count"`
	First  string `json:"first,omitempty"`
	Last   string `json:"last,omitempty"`
}

// LogPattern is a log template with its count and one exemplar, which is what
// makes logs affordable to read at all.
type LogPattern struct {
	Template string `json:"template"`
	Count    int    `json:"count"`
	SampleAt string `json:"sample_at,omitempty"`
}

// Hint is a suggested next call with its arguments pre-filled: the cheapest way
// to steer the agent's walk without putting that logic in the user's prompt.
type Hint struct {
	Tool string         `json:"tool"`
	Args map[string]any `json:"args,omitempty"`
}

// Envelope accumulates a tool's answer and serializes it once.
type Envelope struct {
	asOf    time.Time
	lag     time.Duration
	target  string
	summary string
	budget  Budget

	findings    []Finding
	events      []Event
	logPatterns []LogPattern
	objects     []projection.Object
	hints       []Hint

	denied []rbac.Scope
}

// New starts an envelope.
//
// asOf must be the cache's own timestamp, not time.Now(): the server answers
// from a cache that lies for a few hundred milliseconds, and the agent has to
// know which moment it is reading about so it does not report something as
// running when it died two seconds ago.
func New(asOf time.Time, lag time.Duration, b Budget) (*Envelope, error) {
	if asOf.IsZero() {
		return nil, fmt.Errorf("envelope: as_of is required (principle 6); pass the cache timestamp, not time.Now()")
	}
	if b.Tokens <= 0 {
		return nil, fmt.Errorf("envelope: a token budget is required (principle 3), got %d", b.Tokens)
	}
	if lag < 0 {
		return nil, fmt.Errorf("envelope: negative watch lag %s", lag)
	}
	return &Envelope{asOf: asOf, lag: lag, budget: b}, nil
}

// Target records what the answer is about, as a stable ID.
func (e *Envelope) Target(id projection.ID) { e.target = string(id) }

// Summary records the one-line conclusion.
func (e *Envelope) Summary(s string) { e.summary = s }

// AddFinding appends a diagnosed problem, rejecting unregistered codes.
func (e *Envelope) AddFinding(f Finding) error {
	if err := f.validate(); err != nil {
		return err
	}
	e.findings = append(e.findings, f)
	return nil
}

// AddEvents appends aggregated events.
func (e *Envelope) AddEvents(evs ...Event) { e.events = append(e.events, evs...) }

// AddLogPatterns appends log templates.
func (e *Envelope) AddLogPatterns(ps ...LogPattern) { e.logPatterns = append(e.logPatterns, ps...) }

// SetObjects places projected objects in the response.
//
// It takes rbac.FilteredObjects and nothing else, which is what makes principle
// 5 structural: there is no exported way to build one of those holding data
// that did not pass authorization. What was withheld is recorded so the
// response can say so instead of quietly looking like a smaller cluster.
func (e *Envelope) SetObjects(f rbac.FilteredObjects) {
	e.objects = f.Objects()
	e.denied = f.Denied()
}

// WithheldScopes returns the scopes the caller's RBAC refused, in full.
//
// This is for the audit log, which answers "what has the AI been reading?" for
// an operator who is authorized to know. It is deliberately not what the
// response discloses: see the withheld type.
func (e *Envelope) WithheldScopes() []rbac.Scope { return e.denied }

// AddHint appends a suggested next call, up to MaxHints.
func (e *Envelope) AddHint(h Hint) error {
	if len(e.hints) >= MaxHints {
		return fmt.Errorf("envelope: at most %d next_hints, refusing a %dth (%s)", MaxHints, len(e.hints)+1, h.Tool)
	}
	e.hints = append(e.hints, h)
	return nil
}

// withheld discloses that a response is partial without saying what it
// withheld. The count is what the agent needs: it must not read a filtered
// answer as a complete one. The scopes themselves are deliberately absent —
// naming a namespace the caller may not read would tell them it exists, which
// is the very disclosure principle 5 forbids. They go to the audit log instead,
// where the operator, who is authorized to see them, can read them.
type withheld struct {
	Scopes int `json:"scopes"`
}

type omitted struct {
	Objects     int    `json:"objects,omitempty"`
	Events      int    `json:"events,omitempty"`
	LogPatterns int    `json:"log_patterns,omitempty"`
	Findings    int    `json:"findings,omitempty"`
	Note        string `json:"note,omitempty"`
}

func (o omitted) empty() bool {
	return o.Objects == 0 && o.Events == 0 && o.LogPatterns == 0 && o.Findings == 0 && o.Note == ""
}

type wire struct {
	AsOf        string              `json:"as_of"`
	LagMS       int64               `json:"lag_ms"`
	Target      string              `json:"target,omitempty"`
	Summary     string              `json:"summary,omitempty"`
	Findings    []Finding           `json:"findings,omitempty"`
	Events      []Event             `json:"events,omitempty"`
	LogPatterns []LogPattern        `json:"log_patterns,omitempty"`
	Objects     []projection.Object `json:"objects,omitempty"`
	Truncated   bool                `json:"truncated"`
	Omitted     *omitted            `json:"omitted,omitempty"`
	Cursor      string              `json:"cursor,omitempty"`
	Withheld    *withheld           `json:"withheld_by_rbac,omitempty"`
	NextHints   []Hint              `json:"next_hints,omitempty"`
}

// Marshal serializes the envelope, enforcing the token ceiling.
//
// This is the only place a tool's output becomes bytes. On exhaustion the
// response truncates, declares it, summarizes what it dropped and hands back a
// cursor — never a silent cut mid-JSON. A response that blows up the context
// window is a server failure, not an agent failure.
func (e *Envelope) Marshal() ([]byte, error) {
	e.sortFindings()

	// Truncation accounting lives in this call, not on the envelope, so
	// marshalling twice yields the same bytes rather than compounding omissions.
	var om omitted
	w := e.assemble(&om)

	for {
		raw, err := json.Marshal(w)
		if err != nil {
			return nil, fmt.Errorf("envelope: marshal: %w", err)
		}
		if e.budget.count(raw) <= e.budget.Tokens {
			return raw, nil
		}
		if !shrink(&w, &om) {
			// Everything droppable is gone and the floor still does not fit.
			// Returning it anyway would blow the caller's window, which is the
			// one outcome principle 3 exists to prevent.
			return nil, fmt.Errorf("envelope: irreducible response is %d tokens, budget is %d; raise the tool's budget or narrow its scope",
				e.budget.count(raw), e.budget.Tokens)
		}
	}
}

func (e *Envelope) sortFindings() {
	sort.SliceStable(e.findings, func(i, j int) bool {
		return e.findings[i].Severity.rank() < e.findings[j].Severity.rank()
	})
}

func (e *Envelope) assemble(om *omitted) wire {
	w := wire{
		AsOf:        e.asOf.UTC().Format(time.RFC3339),
		LagMS:       e.lag.Milliseconds(),
		Target:      e.target,
		Summary:     e.summary,
		Findings:    e.findings,
		Events:      e.events,
		LogPatterns: e.logPatterns,
		Objects:     e.objects,
		NextHints:   e.hints,
	}
	if n := len(e.denied); n > 0 {
		w.Withheld = &withheld{Scopes: n}
		om.Note = "the caller's RBAC withheld part of this answer"
	}
	applyOmitted(&w, om)
	return w
}

func applyOmitted(w *wire, om *omitted) {
	if om.empty() {
		w.Truncated = false
		w.Omitted = nil
		w.Cursor = ""
		return
	}
	w.Truncated = om.Objects+om.Events+om.LogPatterns+om.Findings > 0
	o := *om
	w.Omitted = &o
	if w.Truncated {
		w.Cursor = "objects:" + strconv.Itoa(len(w.Objects))
	}
}

// shrink drops the tail of the largest droppable section and reports whether
// anything was dropped.
//
// Order matters: log patterns and events are the most compressible, objects
// next, findings last — a finding is the answer the agent came for. Within
// findings the least severe goes first. next_hints are never dropped: three
// pre-filled suggestions cost almost nothing and are what keep the agent's walk
// cheap after a truncated answer.
func shrink(w *wire, om *omitted) bool {
	chunk := func(n int) int {
		if c := n / 4; c > 0 {
			return c
		}
		return 1
	}

	switch {
	case len(w.LogPatterns) > 0:
		n := chunk(len(w.LogPatterns))
		w.LogPatterns = w.LogPatterns[:len(w.LogPatterns)-n]
		om.LogPatterns += n
	case len(w.Events) > 0:
		n := chunk(len(w.Events))
		w.Events = w.Events[:len(w.Events)-n]
		om.Events += n
	case len(w.Objects) > 0:
		n := chunk(len(w.Objects))
		w.Objects = w.Objects[:len(w.Objects)-n]
		om.Objects += n
	case len(w.Findings) > 1:
		w.Findings = w.Findings[:len(w.Findings)-1]
		om.Findings++
	default:
		return false
	}
	applyOmitted(w, om)
	return true
}
