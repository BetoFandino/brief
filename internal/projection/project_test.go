package projection

import "testing"

func noisyPod() map[string]any {
	return map[string]any{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{
			"name": "checkout-7d9f", "namespace": "prod",
			"uid": "8f14e45f", "resourceVersion": "918273645",
			"managedFields": []any{map[string]any{"manager": "kubelet"}},
			"labels":        map[string]any{"app": "checkout"},
			"annotations": map[string]any{
				LastAppliedConfig: `{"kind":"Pod"}`,
				"team":            "payments",
			},
		},
		"spec": map[string]any{
			"nodeName":           "ip-10-0-1-7",
			"serviceAccountName": "default",
			"schedulerName":      "default-scheduler",
			"dnsPolicy":          "ClusterFirst",
			"containers": []any{map[string]any{
				"name": "app", "image": "checkout:1.4",
				"terminationMessagePath": "/dev/termination-log",
			}},
		},
		"status": map[string]any{
			"phase": "Running", "hostIP": "10.0.1.7", "qosClass": "Burstable",
			"containerStatuses": []any{map[string]any{"name": "app", "restartCount": float64(14)}},
		},
	}
}

func TestMinimalProfileKeepsIdentityAndStateOnly(t *testing.T) {
	obj := Project(noisyPod(), Minimal, 0)

	if obj.ID != "prod/Pod/checkout-7d9f" {
		t.Errorf("ID = %q", obj.ID)
	}
	if obj.GVK != "v1/Pod" || obj.Kind != "Pod" || obj.Name != "checkout-7d9f" || obj.Namespace != "prod" {
		t.Errorf("identity = %+v", obj)
	}
	if obj.Str("status.phase") != "Running" {
		t.Errorf("minimal dropped the phase: %v", obj.Fields)
	}
	// Everything a listing does not need is gone, including fields that are
	// perfectly interesting for other purposes.
	for _, path := range []string{"status.containerStatuses", "spec.containers", "status.qosClass", "metadata.labels"} {
		if _, present := obj.Value(path); present {
			t.Errorf("minimal kept %s", path)
		}
	}
}

func TestDiagnosticProfileKeepsWhatExplainsAFailure(t *testing.T) {
	obj := Project(noisyPod(), Diagnostic, 0)

	for _, path := range []string{
		"status.phase", "status.containerStatuses", "spec.containers",
		"spec.nodeName", "metadata.labels",
	} {
		if _, present := obj.Value(path); !present {
			t.Errorf("diagnostic dropped %s, which explains failures", path)
		}
	}
	// And still not the API server's bookkeeping or the defaults nobody wrote.
	for _, path := range []string{
		"spec.schedulerName", "spec.dnsPolicy", "status.qosClass", "status.hostIP",
	} {
		if _, present := obj.Value(path); present {
			t.Errorf("diagnostic kept %s, which explains nothing", path)
		}
	}
}

func TestFullProfileKeepsTheStrippedObject(t *testing.T) {
	obj := Project(noisyPod(), Full, 0)

	if _, present := obj.Value("spec.schedulerName"); !present {
		t.Error("full dropped a field; it is the whole projection minus the noise")
	}
	// Noise is noise in every profile: Strip runs before the profile decides.
	md, _ := obj.Value("metadata")
	meta, _ := md.(map[string]any)
	for _, gone := range []string{"managedFields", "resourceVersion", "uid"} {
		if _, present := meta[gone]; present {
			t.Errorf("full kept %s", gone)
		}
	}
	anns, _ := Sub(meta, "annotations")["team"].(string)
	if anns != "payments" {
		t.Error("full lost an ordinary annotation")
	}
}

// Generic projection is weakest exactly where it is needed most — a homegrown
// CRD nobody wrote a table for. This pins the documented fallback so the
// weakness stays visible instead of being discovered in an issue.
func TestUnknownKindFallsBackToIdentityAndStatus(t *testing.T) {
	cr := map[string]any{
		"apiVersion": "acme.io/v1", "kind": "Widget",
		"metadata": map[string]any{
			"name": "w1", "namespace": "prod",
			"labels":        map[string]any{"tier": "gold"},
			"managedFields": []any{map[string]any{"manager": "widget-operator"}},
		},
		"spec":   map[string]any{"size": float64(3)},
		"status": map[string]any{"phase": "Degraded", "detail": "upstream timeout"},
	}

	obj := Project(cr, Diagnostic, 0)
	if obj.GVK != "acme.io/v1/Widget" {
		t.Errorf("GVK = %q", obj.GVK)
	}
	if obj.Str("status.phase") != "Degraded" || obj.Str("status.detail") != "upstream timeout" {
		t.Errorf("the fallback dropped status: %v", obj.Fields)
	}
	if obj.Labels()["tier"] != "gold" {
		t.Error("the fallback dropped labels")
	}
	// The noise still goes, which is the part that works for any kind.
	if _, present := obj.Value("metadata.managedFields"); present {
		t.Error("the fallback kept managedFields")
	}
}

func TestProjectSurvivesMalformedObjects(t *testing.T) {
	// Whatever the cluster actually sent, including shapes nobody promised.
	for name, raw := range map[string]map[string]any{
		"empty":            {},
		"no metadata":      {"kind": "Pod", "apiVersion": "v1"},
		"status is a list": {"kind": "Pod", "apiVersion": "v1", "status": []any{"odd"}},
		"metadata is text": {"kind": "Pod", "metadata": "not-a-map"},
	} {
		t.Run(name, func(t *testing.T) {
			obj := Project(raw, Diagnostic, 0)
			if _, present := obj.Value("status.phase"); present {
				t.Error("found a phase in an object that has none")
			}
		})
	}
}

func TestAccessorsReadWhatIsThereAndNothingElse(t *testing.T) {
	obj := Project(noisyPod(), Diagnostic, 0)

	if got := obj.Str("status.phase"); got != "Running" {
		t.Errorf("Str = %q", got)
	}
	if got := obj.Str("status.nope"); got != "" {
		t.Errorf("Str of an absent path = %q, want empty", got)
	}
	// A string read as a number is an absence, not a zero that looks like data.
	if _, ok := obj.Int("status.phase"); ok {
		t.Error("Int read a string as a number")
	}

	statuses := obj.List("status.containerStatuses")
	if len(statuses) != 1 {
		t.Fatalf("List = %v", statuses)
	}
	if n, ok := Num(statuses[0], "restartCount"); !ok || n != 14 {
		t.Errorf("Num = %d, %v", n, ok)
	}
	if Field(statuses[0], "name") != "app" {
		t.Error("Field did not read the container name")
	}
	if obj.List("status.phase") != nil {
		t.Error("List read a string as a slice")
	}
	if obj.Labels()["app"] != "checkout" {
		t.Errorf("Labels = %v", obj.Labels())
	}
}

func TestIntOrSuppliesTheKubernetesDefault(t *testing.T) {
	// Kubernetes omits readyReplicas rather than writing zero, and "none are
	// ready" is the case that matters most.
	obj := Project(map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": "d", "namespace": "prod"},
		"spec":     map[string]any{"replicas": float64(5)},
		"status":   map[string]any{},
	}, Diagnostic, 0)

	if got := obj.IntOr("status.readyReplicas", 0); got != 0 {
		t.Errorf("absent readyReplicas = %d, want the 0 fallback", got)
	}
	if got := obj.IntOr("spec.replicas", 1); got != 5 {
		t.Errorf("spec.replicas = %d", got)
	}
	if got := obj.IntOr("spec.nope", 7); got != 7 {
		t.Errorf("fallback = %d, want 7", got)
	}
}

func TestConditionFindsByType(t *testing.T) {
	obj := Project(map[string]any{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{"name": "p", "namespace": "ns"},
		"status": map[string]any{"conditions": []any{
			map[string]any{"type": "Ready", "status": "False"},
			map[string]any{"type": "PodScheduled", "status": "True"},
		}},
	}, Diagnostic, 0)

	cond, found := obj.Condition("PodScheduled")
	if !found || Field(cond, "status") != "True" {
		t.Errorf("Condition(PodScheduled) = %v, %v", cond, found)
	}
	if _, found := obj.Condition("Nonexistent"); found {
		t.Error("found a condition that is not there")
	}
}

func TestSubAndBoolReadNestedShapes(t *testing.T) {
	obj := Project(map[string]any{
		"apiVersion": "v1", "kind": "Node",
		"metadata": map[string]any{"name": "n1"},
		"spec":     map[string]any{"unschedulable": true},
		"status":   map[string]any{"allocatable": map[string]any{"cpu": "4"}},
	}, Diagnostic, 0)

	if b, ok := obj.Bool("spec.unschedulable"); !ok || !b {
		t.Errorf("Bool = %v, %v", b, ok)
	}
	if _, ok := obj.Bool("metadata.name"); ok {
		t.Error("Bool read a string as a boolean")
	}
	if got := Field(Sub(obj.Fields, "status", "allocatable"), "cpu"); got != "4" {
		t.Errorf("Sub+Field = %q", got)
	}
	if Sub(obj.Fields, "status", "nope") != nil {
		t.Error("Sub invented a map")
	}
	if Sub(nil, "a") != nil {
		t.Error("Sub(nil) did not return nil")
	}
}
