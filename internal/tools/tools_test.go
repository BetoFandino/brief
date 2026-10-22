package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/BetoFandino/brief/internal/analyzer"
	"github.com/BetoFandino/brief/internal/cache"
	"github.com/BetoFandino/brief/internal/envelope"
	"github.com/BetoFandino/brief/internal/rbac"
)

func deps(objs ...map[string]any) Deps {
	return Deps{
		Cache: &cache.Fake{
			Objects: objs,
			Resources: map[string]string{
				"Pod": "pods", "Deployment": "deployments", "Service": "services",
				"Node": "nodes", "Event": "events", "EndpointSlice": "endpointslices",
				"PersistentVolumeClaim": "persistentvolumeclaims",
			},
		},
		Analyzers: analyzer.Default(),
	}
}

func call(t *testing.T, tool Tool, d Deps, args map[string]any, caller rbac.Checker) map[string]any {
	t.Helper()
	e, err := tool.Call(context.Background(), d, Request{Args: args, Caller: caller})
	if err != nil {
		t.Fatalf("%s: %v", tool.Name(), err)
	}
	raw, err := e.Marshal()
	if err != nil {
		t.Fatalf("%s: marshal: %v", tool.Name(), err)
	}
	if spent := (envelope.HeuristicCounter{}).Count(raw); spent > tool.Budget() {
		t.Errorf("%s spent %d tokens against its %d budget", tool.Name(), spent, tool.Budget())
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("%s: response is not valid JSON: %v", tool.Name(), err)
	}
	if m["as_of"] == nil || m["as_of"] == "" {
		t.Errorf("%s returned no as_of", tool.Name())
	}
	if _, present := m["truncated"]; !present {
		t.Errorf("%s omitted the truncated flag", tool.Name())
	}
	return m
}

// The contract the /new-tool skill enforces: a tool without a declared budget
// cannot exist, and every tool must introduce itself.
func TestEveryToolDeclaresItsContract(t *testing.T) {
	for _, tool := range Default().All() {
		if tool.Name() == "" {
			t.Error("a tool has no name")
		}
		if tool.Budget() <= 0 {
			t.Errorf("%s declares no token budget", tool.Name())
		}
		if len(tool.Description()) < 40 {
			t.Errorf("%s has no usable description for the agent", tool.Name())
		}
	}
	if got := Default().Names(); len(got) != 3 {
		t.Errorf("the v0.1 surface is %v, want exactly three tools", got)
	}
}

func TestToolsRefuseToServeWithoutCallerAuthorization(t *testing.T) {
	for _, tool := range Default().All() {
		_, err := tool.Call(context.Background(), deps(), Request{Args: map[string]any{
			"namespace": "prod", "name": "checkout", "gvk": "v1/Pod",
		}})
		if err == nil {
			t.Errorf("%s served a call with no caller authorization", tool.Name())
		}
	}
}

// --- explain_workload ---

func oomCluster() []map[string]any {
	return []map[string]any{
		{
			"apiVersion": "apps/v1", "kind": "Deployment",
			"metadata": map[string]any{"name": "checkout", "namespace": "prod"},
			"spec": map[string]any{
				"replicas": float64(5),
				"selector": map[string]any{"matchLabels": map[string]any{"app": "checkout"}},
			},
			"status": map[string]any{"readyReplicas": float64(2), "replicas": float64(5)},
		},
		{
			"apiVersion": "v1", "kind": "Pod",
			"metadata": map[string]any{
				"name": "checkout-7d9f-xk2", "namespace": "prod",
				"labels": map[string]any{"app": "checkout"},
			},
			"spec": map[string]any{"containers": []any{map[string]any{
				"name": "app", "image": "checkout:1.4",
				"resources": map[string]any{"limits": map[string]any{"memory": "512Mi"}},
			}}},
			"status": map[string]any{
				"phase": "Running",
				"containerStatuses": []any{map[string]any{
					"name": "app", "restartCount": float64(14),
					"lastState": map[string]any{"terminated": map[string]any{
						"exitCode": float64(137), "reason": "OOMKilled",
					}},
					"state": map[string]any{"waiting": map[string]any{"reason": "CrashLoopBackOff"}},
				}},
			},
		},
		{
			"apiVersion": "v1", "kind": "Event",
			"metadata":       map[string]any{"name": "ev1", "namespace": "prod"},
			"reason":         "BackOff",
			"count":          float64(47),
			"type":           "Warning",
			"involvedObject": map[string]any{"name": "checkout-7d9f-xk2", "kind": "Pod"},
			"firstTimestamp": "2026-10-21T13:52:11Z",
			"lastTimestamp":  "2026-10-21T14:06:58Z",
		},
	}
}

func TestExplainWorkloadAnswersTheWholeWalkInOneCall(t *testing.T) {
	got := call(t, ExplainWorkload{}, deps(oomCluster()...),
		map[string]any{"namespace": "prod", "name": "checkout"}, rbac.ServiceAccountOnly{})

	if got["target"] != "prod/Deployment/checkout" {
		t.Errorf("target = %v", got["target"])
	}

	// The replica arithmetic is computed, not left to the model.
	summary, _ := got["summary"].(string)
	if !strings.Contains(summary, "2/5 replicas ready") {
		t.Errorf("summary does not state the replica counts: %q", summary)
	}

	findings, _ := got["findings"].([]any)
	if len(findings) == 0 {
		t.Fatal("no findings for a workload whose pods are OOM-killed")
	}
	var seen []string
	for _, f := range findings {
		seen = append(seen, f.(map[string]any)["code"].(string))
	}
	// The deployment's own degraded state AND the pod-level cause, from the one
	// call: that is the N+1 walk collapsed.
	for _, want := range []string{"REPLICAS_UNAVAILABLE", "CONTAINER_OOM_KILLED"} {
		found := false
		for _, code := range seen {
			if code == want {
				found = true
			}
		}
		if !found {
			t.Errorf("missing %s; got %v", want, seen)
		}
	}

	// Events arrive aggregated, not as a raw repeated list.
	events, _ := got["events"].([]any)
	if len(events) != 1 {
		t.Fatalf("events = %v, want one aggregated reason", events)
	}
	ev := events[0].(map[string]any)
	if ev["reason"] != "BackOff" || ev["count"] != float64(47) {
		t.Errorf("aggregated event = %v", ev)
	}

	// The critical finding sorts first.
	if first := findings[0].(map[string]any); first["severity"] != "critical" {
		t.Errorf("findings are not worst-first: %v", first)
	}

	if hints, _ := got["next_hints"].([]any); len(hints) == 0 {
		t.Error("no next_hints to steer the following call")
	}
}

func TestExplainWorkloadWithholdsWhatTheCallerCannotRead(t *testing.T) {
	denyAll := checkerFunc(func(rbac.Scope) (bool, error) { return false, nil })

	got := call(t, ExplainWorkload{}, deps(oomCluster()...),
		map[string]any{"namespace": "prod", "name": "checkout"}, denyAll)

	if got["objects"] != nil {
		t.Errorf("an unauthorized caller got objects: %v", got["objects"])
	}
	if got["findings"] != nil {
		t.Errorf("an unauthorized caller got findings: %v", got["findings"])
	}
	summary, _ := got["summary"].(string)
	if !strings.Contains(summary, "not authorized") {
		t.Errorf("summary does not say the answer was refused: %q", summary)
	}
}

func TestExplainWorkloadOnAMissingWorkloadIsAnAnswerNotACrash(t *testing.T) {
	_, err := ExplainWorkload{}.Call(context.Background(), deps(oomCluster()...), Request{
		Args: map[string]any{"namespace": "prod", "name": "no-such-thing"}, Caller: rbac.ServiceAccountOnly{},
	})
	var notFound *cache.NotFoundError
	if err == nil {
		t.Fatal("a missing workload did not error")
	}
	if !asNotFound(err, &notFound) {
		t.Errorf("error is not a NotFoundError: %v", err)
	}
}

// --- triage_cluster ---

func TestTriageGroupsManySymptomsIntoOneCause(t *testing.T) {
	objs := oomCluster()
	// Two more pods killed by the same limit. One cause, not three answers.
	for _, name := range []string{"checkout-7d9f-aaa", "checkout-7d9f-bbb"} {
		objs = append(objs, map[string]any{
			"apiVersion": "v1", "kind": "Pod",
			"metadata": map[string]any{
				"name": name, "namespace": "prod",
				"labels": map[string]any{"app": "checkout"},
			},
			"spec": map[string]any{"containers": []any{map[string]any{"name": "app"}}},
			"status": map[string]any{"containerStatuses": []any{map[string]any{
				"name": "app", "restartCount": float64(9),
				"lastState": map[string]any{"terminated": map[string]any{"exitCode": float64(137)}},
			}}},
		})
	}

	got := call(t, TriageCluster{}, deps(objs...),
		map[string]any{"namespace": "prod"}, rbac.ServiceAccountOnly{})

	findings, _ := got["findings"].([]any)
	oom := findingByCode(findings, "CONTAINER_OOM_KILLED")
	if oom == nil {
		t.Fatalf("no grouped OOM finding: %v", findings)
	}
	objects, _ := oom["objects"].([]any)
	if len(objects) != 3 {
		t.Errorf("the OOM cause names %d objects, want all 3 affected pods", len(objects))
	}
	evidence, _ := oom["evidence"].(map[string]any)
	if evidence["affected_objects"] != float64(3) {
		t.Errorf("grouped evidence does not count the affected objects: %v", evidence)
	}
	if summary, _ := got["summary"].(string); !strings.Contains(summary, "distinct causes") {
		t.Errorf("summary does not report distinct causes: %q", summary)
	}
}

func TestTriageHonorsTheSeverityFloor(t *testing.T) {
	objs := []map[string]any{{
		"apiVersion": "v1", "kind": "PersistentVolumeClaim",
		"metadata": map[string]any{"name": "data", "namespace": "prod"},
		"spec":     map[string]any{"storageClassName": "nope"},
		"status":   map[string]any{"phase": "Pending"},
	}}

	// PVC_UNBOUND is high, so a critical floor excludes it.
	got := call(t, TriageCluster{}, deps(objs...),
		map[string]any{"namespace": "prod", "min_severity": "critical"}, rbac.ServiceAccountOnly{})
	if got["findings"] != nil {
		t.Errorf("a critical floor let a high finding through: %v", got["findings"])
	}

	got = call(t, TriageCluster{}, deps(objs...),
		map[string]any{"namespace": "prod", "min_severity": "high"}, rbac.ServiceAccountOnly{})
	if findingByCode(mustList(t, got, "findings"), "PVC_UNBOUND") == nil {
		t.Error("a high floor dropped a high finding")
	}
}

func TestTriageOnAHealthyClusterSaysSo(t *testing.T) {
	got := call(t, TriageCluster{}, deps(map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": "fine", "namespace": "prod"},
		"spec":     map[string]any{"replicas": float64(2)},
		"status":   map[string]any{"readyReplicas": float64(2)},
	}), map[string]any{"namespace": "prod"}, rbac.ServiceAccountOnly{})

	if got["findings"] != nil {
		t.Errorf("a healthy cluster produced findings: %v", got["findings"])
	}
	if summary, _ := got["summary"].(string); !strings.Contains(summary, "nothing diagnosed") {
		t.Errorf("summary = %q", summary)
	}
}

// --- list_objects ---

func TestListObjectsIsTheInconvenientEscapeHatch(t *testing.T) {
	d := deps(oomCluster()...)

	// A GVK is mandatory: the tool will not guess what you meant.
	if _, err := (ListObjects{}).Call(context.Background(), d, Request{
		Args: map[string]any{"namespace": "prod"}, Caller: rbac.ServiceAccountOnly{},
	}); err == nil {
		t.Error("list_objects served a call with no gvk")
	}

	// A listing may never use the full profile.
	if _, err := (ListObjects{}).Call(context.Background(), d, Request{
		Args:   map[string]any{"gvk": "v1/Pod", "profile": "full"},
		Caller: rbac.ServiceAccountOnly{},
	}); err == nil {
		t.Error("list_objects served a full-profile listing")
	}

	// And it points back at the tools that would have answered the question.
	got := call(t, ListObjects{}, d,
		map[string]any{"gvk": "v1/Pod", "namespace": "prod"}, rbac.ServiceAccountOnly{})
	hints := mustList(t, got, "next_hints")
	if len(hints) == 0 || hints[0].(map[string]any)["tool"] != "triage_cluster" {
		t.Errorf("the escape hatch does not steer back to the intent tools: %v", hints)
	}
}

func TestListObjectsCapsTheLimitHowevermuchIsAsked(t *testing.T) {
	var objs []map[string]any
	for i := 0; i < 600; i++ {
		objs = append(objs, map[string]any{
			"apiVersion": "v1", "kind": "Pod",
			"metadata": map[string]any{"name": "p", "namespace": "prod"},
			"status":   map[string]any{"phase": "Running"},
		})
	}

	e, err := (ListObjects{}).Call(context.Background(), deps(objs...), Request{
		Args:   map[string]any{"gvk": "v1/Pod", "namespace": "prod", "limit": float64(10000)},
		Caller: rbac.ServiceAccountOnly{},
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := e.Marshal()
	if err != nil {
		t.Fatalf("a capped listing still blew its budget: %v", err)
	}
	if spent := (envelope.HeuristicCounter{}).Count(raw); spent > (ListObjects{}).Budget() {
		t.Errorf("spent %d of %d tokens", spent, (ListObjects{}).Budget())
	}
}

// --- helpers ---

type checkerFunc func(rbac.Scope) (bool, error)

func (f checkerFunc) Allowed(_ context.Context, s rbac.Scope) (bool, error) { return f(s) }

func findingByCode(findings []any, code string) map[string]any {
	for _, f := range findings {
		m, ok := f.(map[string]any)
		if ok && m["code"] == code {
			return m
		}
	}
	return nil
}

func mustList(t *testing.T, m map[string]any, key string) []any {
	t.Helper()
	v, ok := m[key].([]any)
	if !ok {
		t.Fatalf("%s is absent or not a list: %v", key, m[key])
	}
	return v
}

func asNotFound(err error, target **cache.NotFoundError) bool {
	e, ok := err.(*cache.NotFoundError)
	if ok {
		*target = e
	}
	return ok
}
