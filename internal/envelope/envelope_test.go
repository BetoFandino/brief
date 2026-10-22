package envelope

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/BetoFandino/brief/internal/projection"
	"github.com/BetoFandino/brief/internal/rbac"
)

var asOf = time.Date(2026, 10, 21, 14, 7, 12, 0, time.UTC)

func mustNew(t *testing.T, tokens int) *Envelope {
	t.Helper()
	e, err := New(asOf, 180*time.Millisecond, Budget{Tokens: tokens})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e
}

func decode(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	return m
}

// Principle 6: there is no path to an envelope without a cache timestamp.
func TestNewRefusesAnEnvelopeWithoutFreshness(t *testing.T) {
	if _, err := New(time.Time{}, 0, Budget{Tokens: 4000}); err == nil {
		t.Error("New accepted a zero as_of")
	}
	if _, err := New(asOf, 0, Budget{Tokens: 0}); err == nil {
		t.Error("New accepted a zero token budget")
	}
	if _, err := New(asOf, -time.Second, Budget{Tokens: 4000}); err == nil {
		t.Error("New accepted a negative watch lag")
	}
}

func TestEveryResponseCarriesFreshnessAndTruncationFlag(t *testing.T) {
	e := mustNew(t, 4000)
	e.Summary("2/5 replicas ready")

	got := decode(t, mustMarshal(t, e))
	if got["as_of"] != "2026-10-21T14:07:12Z" {
		t.Errorf("as_of = %v", got["as_of"])
	}
	if got["lag_ms"] != float64(180) {
		t.Errorf("lag_ms = %v", got["lag_ms"])
	}
	// truncated is never omitted: the agent must not have to infer it from
	// absence.
	if _, present := got["truncated"]; !present {
		t.Error("truncated is missing from an untruncated response")
	}
	if got["truncated"] != false {
		t.Errorf("truncated = %v on a response that fit", got["truncated"])
	}
}

// Principle 3: the ceiling is enforced at the only serialization point.
func TestMarshalTruncatesToBudgetAndSaysSo(t *testing.T) {
	e := mustNew(t, 220)
	e.Summary("many things are broken")
	for i := 0; i < 400; i++ {
		e.AddEvents(Event{Reason: "BackOff", Count: i, First: "13:52:11Z", Last: "14:06:58Z"})
	}

	raw := mustMarshal(t, e)
	if spent := (HeuristicCounter{}).Count(raw); spent > 220 {
		t.Errorf("response spent %d tokens against a 220 budget", spent)
	}

	got := decode(t, raw)
	if got["truncated"] != true {
		t.Fatal("an over-budget response did not declare truncation")
	}
	if got["cursor"] == nil || got["cursor"] == "" {
		t.Error("truncated response handed back no cursor")
	}
	om, ok := got["omitted"].(map[string]any)
	if !ok {
		t.Fatal("truncated response did not summarize what it dropped")
	}
	if om["events"] == nil {
		t.Error("omitted does not account for the dropped events")
	}
	// A truncated response is still well-formed JSON, never a cut mid-object:
	// decode() above would have failed otherwise.
}

func TestTruncationSacrificesNoiseBeforeAnswers(t *testing.T) {
	e := mustNew(t, 320)
	if err := e.AddFinding(Finding{
		Code: CodeContainerOOMKilled, Severity: SeverityCritical,
		Objects: []string{"prod/Pod/checkout-7d9f"},
	}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 200; i++ {
		e.AddLogPatterns(LogPattern{Template: "failed to allocate <N> bytes", Count: i})
		e.AddEvents(Event{Reason: "BackOff", Count: i})
	}

	got := decode(t, mustMarshal(t, e))
	findings, ok := got["findings"].([]any)
	if !ok || len(findings) == 0 {
		t.Fatal("truncation dropped the finding while noise remained")
	}
	first := findings[0].(map[string]any)
	if first["code"] != string(CodeContainerOOMKilled) {
		t.Errorf("surviving finding = %v", first["code"])
	}
}

func TestMarshalRefusesWhenEvenTheFloorDoesNotFit(t *testing.T) {
	e := mustNew(t, 2)
	e.Summary(strings.Repeat("this summary alone blows the budget ", 20))
	if _, err := e.Marshal(); err == nil {
		t.Error("Marshal emitted a response that cannot fit its budget")
	}
}

func TestFindingsAreOrderedBySeverity(t *testing.T) {
	e := mustNew(t, 4000)
	for _, f := range []Finding{
		{Code: CodePVCUnbound, Severity: SeverityLow, Objects: []string{"prod/PVC/data"}},
		{Code: CodeContainerOOMKilled, Severity: SeverityCritical, Objects: []string{"prod/Pod/a"}},
		{Code: CodeNoEndpoints, Severity: SeverityMedium, Objects: []string{"prod/Service/s"}},
	} {
		if err := e.AddFinding(f); err != nil {
			t.Fatal(err)
		}
	}

	got := decode(t, mustMarshal(t, e))
	findings := got["findings"].([]any)
	want := []string{"critical", "medium", "low"}
	for i, w := range want {
		if sev := findings[i].(map[string]any)["severity"]; sev != w {
			t.Errorf("findings[%d].severity = %v, want %v", i, sev, w)
		}
	}
}

func TestUnregisteredReasonCodesAreRefused(t *testing.T) {
	e := mustNew(t, 4000)
	err := e.AddFinding(Finding{
		Code: "SOMETHING_I_INVENTED", Severity: SeverityHigh, Objects: []string{"prod/Pod/a"},
	})
	if err == nil {
		t.Error("an unregistered reason code was accepted")
	}
	if err := e.AddFinding(Finding{Code: CodeCrashLoopBackOff, Severity: SeverityHigh}); err == nil {
		t.Error("a finding naming no objects was accepted")
	}
}

func TestReasonCodesAreNormalized(t *testing.T) {
	for _, c := range Codes() {
		s := string(c)
		if s != strings.ToUpper(s) {
			t.Errorf("code %q is not upper case", s)
		}
		if strings.ContainsAny(s, " -.") {
			t.Errorf("code %q is not SCREAMING_SNAKE_CASE", s)
		}
		if d, ok := Describe(c); !ok || d == "" {
			t.Errorf("code %q has no description", s)
		}
	}
}

func TestHintsAreCappedAtThree(t *testing.T) {
	e := mustNew(t, 4000)
	for i := 0; i < MaxHints; i++ {
		if err := e.AddHint(Hint{Tool: "what_changed"}); err != nil {
			t.Fatalf("hint %d refused: %v", i, err)
		}
	}
	if err := e.AddHint(Hint{Tool: "resource_pressure"}); err == nil {
		t.Errorf("a %dth hint was accepted", MaxHints+1)
	}
}

// --- principle 5 ---

type allowOnly struct{ namespaces map[string]bool }

func (a allowOnly) Allowed(_ context.Context, s rbac.Scope) (bool, error) {
	return a.namespaces[s.Namespace], nil
}

func obj(ns, name string) projection.Object {
	return projection.Object{
		ID: projection.NewID(ns, "Pod", name), Kind: "Pod", Resource: "pods",
		Namespace: ns, Name: name, GVK: "v1/Pod",
	}
}

func TestResponseCarriesOnlyWhatTheCallerMayRead(t *testing.T) {
	objs := []projection.Object{obj("prod", "a"), obj("secret-ns", "b"), obj("prod", "c")}

	filtered, err := rbac.Filter(context.Background(),
		allowOnly{namespaces: map[string]bool{"prod": true}}, "get", objs)
	if err != nil {
		t.Fatalf("Filter: %v", err)
	}
	if filtered.Len() != 2 {
		t.Fatalf("Filter kept %d objects, want 2", filtered.Len())
	}

	e := mustNew(t, 4000)
	e.SetObjects(filtered)
	raw := mustMarshal(t, e)

	// Nothing from the unreadable namespace may appear anywhere in the bytes —
	// not as an object, and not as the name of something that was withheld.
	// Naming it would tell the caller that namespace exists, which is the
	// disclosure principle 5 forbids.
	if strings.Contains(string(raw), "secret-ns") {
		t.Errorf("response named a namespace the caller cannot read:\n%s", raw)
	}

	// Withholding still has to be visible, or the agent reads a filtered answer
	// as a complete one and concludes the cluster is smaller than it is.
	got := decode(t, raw)
	w, ok := got["withheld_by_rbac"].(map[string]any)
	if !ok {
		t.Fatalf("response did not disclose that it withheld anything: %s", raw)
	}
	if w["scopes"] != float64(1) {
		t.Errorf("withheld_by_rbac.scopes = %v, want 1", w["scopes"])
	}

	// The operator, who is authorized, gets the detail for the audit log.
	if scopes := e.WithheldScopes(); len(scopes) != 1 || scopes[0].Namespace != "secret-ns" {
		t.Errorf("WithheldScopes() = %v, want the secret-ns scope for the audit log", scopes)
	}
}

func TestMarshalIsIdempotent(t *testing.T) {
	e := mustNew(t, 240)
	for i := 0; i < 300; i++ {
		e.AddEvents(Event{Reason: "BackOff", Count: i, First: "13:52:11Z", Last: "14:06:58Z"})
	}

	first := mustMarshal(t, e)
	second := mustMarshal(t, e)
	if string(first) != string(second) {
		t.Errorf("marshalling twice compounded the truncation accounting:\n 1st: %s\n 2nd: %s", first, second)
	}
}

func mustMarshal(t *testing.T, e *Envelope) []byte {
	t.Helper()
	raw, err := e.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	return raw
}
