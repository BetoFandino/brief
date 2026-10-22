package analyzer

import (
	"testing"

	"github.com/BetoFandino/brief/internal/envelope"
	"github.com/BetoFandino/brief/internal/projection"
)

// project builds an Input the way the cache would, so these tests run through
// the real projection path rather than hand-built field maps.
func project(t *testing.T, raw map[string]any, related map[string][]projection.Object) Input {
	t.Helper()
	return Input{Object: projection.Project(raw, projection.Diagnostic, 0), Related: related}
}

func codes(findings []envelope.Finding) []envelope.Code {
	out := make([]envelope.Code, 0, len(findings))
	for _, f := range findings {
		out = append(out, f.Code)
	}
	return out
}

func hasCode(findings []envelope.Finding, want envelope.Code) *envelope.Finding {
	for i := range findings {
		if findings[i].Code == want {
			return &findings[i]
		}
	}
	return nil
}

func pod(ns, name string, status, spec map[string]any) map[string]any {
	return map[string]any{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{"name": name, "namespace": ns},
		"spec":     spec, "status": status,
	}
}

// --- the oom fixture ---

func TestOOMIsReportedWithItsComputedEvidence(t *testing.T) {
	in := project(t, pod("eval-oom", "memory-hog-7d9f",
		map[string]any{
			"phase": "Running",
			"containerStatuses": []any{map[string]any{
				"name": "hog", "restartCount": float64(14),
				"lastState": map[string]any{"terminated": map[string]any{
					"exitCode": float64(137), "reason": "OOMKilled", "finishedAt": "2026-10-21T13:52:04Z",
				}},
				"state": map[string]any{"waiting": map[string]any{"reason": "CrashLoopBackOff"}},
			}},
		},
		map[string]any{"containers": []any{map[string]any{
			"name": "hog", "image": "busybox:1.36",
			"resources": map[string]any{"limits": map[string]any{"memory": "32Mi"}},
		}}}), nil)

	got := Default().Run(in)

	f := hasCode(got, envelope.CodeContainerOOMKilled)
	if f == nil {
		t.Fatalf("no OOM finding; got %v", codes(got))
	}
	if f.Severity != envelope.SeverityCritical {
		t.Errorf("severity = %v, want critical", f.Severity)
	}
	// The model must not have to know that 137 means OOM, nor count restarts.
	if f.Evidence["exit_code"] != int64(137) {
		t.Errorf("exit_code = %v (%T)", f.Evidence["exit_code"], f.Evidence["exit_code"])
	}
	if f.Evidence["restarts"] != int64(14) {
		t.Errorf("restarts = %v", f.Evidence["restarts"])
	}
	if f.Evidence["limit_memory"] != "32Mi" {
		t.Errorf("limit_memory = %v, want the limit pulled from the spec", f.Evidence["limit_memory"])
	}
}

func TestOOMIsFoundInLastStateNotJustCurrentState(t *testing.T) {
	// A pod in CrashLoopBackOff has its OOM kill recorded in lastState. An
	// analyzer that only reads the current state misses the cause of the very
	// loop it is diagnosing.
	in := project(t, pod("ns", "p", map[string]any{
		"containerStatuses": []any{map[string]any{
			"name": "app", "restartCount": float64(3),
			"state":     map[string]any{"waiting": map[string]any{"reason": "CrashLoopBackOff"}},
			"lastState": map[string]any{"terminated": map[string]any{"exitCode": float64(137)}},
		}},
	}, nil), nil)

	got := Default().Run(in)
	if hasCode(got, envelope.CodeContainerOOMKilled) == nil {
		t.Errorf("OOM in lastState was missed; got %v", codes(got))
	}
	if hasCode(got, envelope.CodeCrashLoopBackOff) == nil {
		t.Errorf("the crash loop itself was not reported; got %v", codes(got))
	}
}

// --- the image-not-found fixture ---

func TestMissingImageIsNotReportedAsACredentialsFailure(t *testing.T) {
	in := project(t, pod("eval-image", "ghost-image-1", map[string]any{
		"containerStatuses": []any{map[string]any{
			"name": "app",
			"state": map[string]any{"waiting": map[string]any{
				"reason":  "ImagePullBackOff",
				"message": `manifest for busybox:this-tag-does-not-exist-brief-eval not found: manifest unknown`,
			}},
		}},
	}, map[string]any{"containers": []any{map[string]any{"name": "app", "image": "busybox:nope"}}}), nil)

	got := Default().Run(in)
	if hasCode(got, envelope.CodeImagePullNotFound) == nil {
		t.Errorf("want IMAGE_NOT_FOUND; got %v", codes(got))
	}
	// The two have opposite remedies, so collapsing them is a coin flip.
	if hasCode(got, envelope.CodeImagePullAuth) != nil {
		t.Error("a missing tag was reported as a credentials failure")
	}
}

func TestCredentialsFailureIsNotReportedAsAMissingImage(t *testing.T) {
	in := project(t, pod("ns", "p", map[string]any{
		"containerStatuses": []any{map[string]any{
			"name": "app",
			"state": map[string]any{"waiting": map[string]any{
				"reason":  "ErrImagePull",
				"message": `pull access denied, repository does not exist or may require authorization: unauthorized`,
			}},
		}},
	}, nil), nil)

	got := Default().Run(in)
	if hasCode(got, envelope.CodeImagePullAuth) == nil {
		t.Errorf("want IMAGE_PULL_AUTH_FAILED; got %v", codes(got))
	}
	if hasCode(got, envelope.CodeImagePullNotFound) != nil {
		t.Error("a credentials failure was reported as a missing image")
	}
}

// --- the node-tainted fixture ---

func TestUnschedulablePodCountsNodesWithoutInventingABreakdown(t *testing.T) {
	nodes := []projection.Object{
		projection.Project(map[string]any{
			"apiVersion": "v1", "kind": "Node", "metadata": map[string]any{"name": "w1"},
			"spec": map[string]any{"taints": []any{map[string]any{"key": "node-role/db", "effect": "NoSchedule"}}},
		}, projection.Diagnostic, 0),
		projection.Project(map[string]any{
			"apiVersion": "v1", "kind": "Node", "metadata": map[string]any{"name": "w2"}, "spec": map[string]any{},
		}, projection.Diagnostic, 0),
	}

	in := project(t, pod("eval-taint", "wants-db-node-1", map[string]any{
		"phase": "Pending",
		"conditions": []any{map[string]any{
			"type": "PodScheduled", "status": "False", "reason": "Unschedulable",
			"message": "0/3 nodes are available: 1 node(s) had untolerated taint {node-role/db: true}",
		}},
	}, map[string]any{"nodeSelector": map[string]any{"brief-eval/tainted": "true"}}),
		map[string][]projection.Object{"Node": nodes})

	got := Default().Run(in)
	f := hasCode(got, envelope.CodePodUnschedulable)
	if f == nil {
		t.Fatalf("want POD_UNSCHEDULABLE; got %v", codes(got))
	}
	if f.Evidence["nodes_considered"] != 2 {
		t.Errorf("nodes_considered = %v, want 2", f.Evidence["nodes_considered"])
	}
	if f.Evidence["nodes_with_taints"] != 1 {
		t.Errorf("nodes_with_taints = %v, want 1", f.Evidence["nodes_with_taints"])
	}
	if f.Evidence["node_selector"] == nil {
		t.Error("the nodeSelector that pinned the pod is not in the evidence")
	}
}

func TestScheduledPodsProduceNoSchedulingFinding(t *testing.T) {
	in := project(t, pod("ns", "p", map[string]any{
		"phase":      "Running",
		"conditions": []any{map[string]any{"type": "PodScheduled", "status": "True"}},
	}, nil), nil)

	if got := Default().Run(in); len(got) != 0 {
		t.Errorf("a healthy pod produced findings: %v", codes(got))
	}
}

// --- replicas ---

func TestReplicasDownIsCriticalWhenNothingIsServing(t *testing.T) {
	deploy := func(want, ready any) map[string]any {
		status := map[string]any{}
		if ready != nil {
			status["readyReplicas"] = ready
		}
		return map[string]any{
			"apiVersion": "apps/v1", "kind": "Deployment",
			"metadata": map[string]any{"name": "checkout", "namespace": "prod"},
			"spec":     map[string]any{"replicas": want}, "status": status,
		}
	}

	// Degraded but serving.
	got := Default().Run(project(t, deploy(float64(5), float64(2)), nil))
	f := hasCode(got, envelope.CodeReplicasUnavailable)
	if f == nil {
		t.Fatalf("want REPLICAS_UNAVAILABLE; got %v", codes(got))
	}
	if f.Severity != envelope.SeverityHigh {
		t.Errorf("2/5 ready: severity = %v, want high", f.Severity)
	}
	if f.Evidence["missing_replicas"] != int64(3) {
		t.Errorf("missing_replicas = %v, want 3 computed server-side", f.Evidence["missing_replicas"])
	}

	// Nothing serving at all: readyReplicas is omitted by Kubernetes, not zero.
	got = Default().Run(project(t, deploy(float64(5), nil), nil))
	f = hasCode(got, envelope.CodeReplicasUnavailable)
	if f == nil {
		t.Fatalf("an absent readyReplicas was read as healthy; got %v", codes(got))
	}
	if f.Severity != envelope.SeverityCritical {
		t.Errorf("0/5 ready: severity = %v, want critical", f.Severity)
	}

	// Healthy.
	if got := Default().Run(project(t, deploy(float64(3), float64(3)), nil)); len(got) != 0 {
		t.Errorf("a healthy deployment produced findings: %v", codes(got))
	}
}

// --- the pvc-unbound fixture ---

func TestUnboundPVCNamesTheStorageClass(t *testing.T) {
	in := project(t, map[string]any{
		"apiVersion": "v1", "kind": "PersistentVolumeClaim",
		"metadata": map[string]any{"name": "data", "namespace": "eval-pvc"},
		"spec": map[string]any{
			"storageClassName": "nonexistent-class-brief-eval",
			"accessModes":      []any{"ReadWriteOnce"},
			"resources":        map[string]any{"requests": map[string]any{"storage": "1Gi"}},
		},
		"status": map[string]any{"phase": "Pending"},
	}, nil)

	got := Default().Run(in)
	f := hasCode(got, envelope.CodePVCUnbound)
	if f == nil {
		t.Fatalf("want PVC_UNBOUND; got %v", codes(got))
	}
	if f.Evidence["storage_class"] != "nonexistent-class-brief-eval" {
		t.Errorf("storage_class = %v", f.Evidence["storage_class"])
	}
	if f.Evidence["requested_storage"] != "1Gi" {
		t.Errorf("requested_storage = %v", f.Evidence["requested_storage"])
	}
}

func TestBoundPVCProducesNoFinding(t *testing.T) {
	in := project(t, map[string]any{
		"apiVersion": "v1", "kind": "PersistentVolumeClaim",
		"metadata": map[string]any{"name": "data", "namespace": "prod"},
		"spec":     map[string]any{"storageClassName": "standard"},
		"status":   map[string]any{"phase": "Bound"},
	}, nil)
	if got := Default().Run(in); len(got) != 0 {
		t.Errorf("a bound claim produced findings: %v", codes(got))
	}
}

// --- the service-selector fixture: the failure with no error anywhere ---

func TestServiceWithNoEndpointsNamesTheNearMiss(t *testing.T) {
	healthyPod := projection.Project(pod("eval-selector", "checkout-abc", nil, nil), projection.Diagnostic, 0)
	healthyPod.Fields = map[string]any{"metadata": map[string]any{
		"labels": map[string]any{"app": "checkout"},
	}}

	svc := map[string]any{
		"apiVersion": "v1", "kind": "Service",
		"metadata": map[string]any{"name": "checkout", "namespace": "eval-selector"},
		"spec": map[string]any{
			"type":     "ClusterIP",
			"selector": map[string]any{"app": "checkut"}, // the typo
		},
	}

	got := Default().Run(project(t, svc, map[string][]projection.Object{
		"EndpointSlice": nil,
		"Pod":           {healthyPod},
	}))

	f := hasCode(got, envelope.CodeNoEndpoints)
	if f == nil {
		t.Fatalf("want SERVICE_HAS_NO_ENDPOINTS; got %v", codes(got))
	}
	// Naming the near miss is the difference between "no endpoints" and
	// "your selector says checkut and your pods say checkout".
	near, ok := f.Evidence["pods_in_namespace_with_similar_labels"].(map[string]string)
	if !ok || near["app"] != "checkout" {
		t.Errorf("the near-miss label set is missing: %v", f.Evidence)
	}
}

func TestServiceWithReadyEndpointsProducesNoFinding(t *testing.T) {
	slice := projection.Project(map[string]any{
		"apiVersion": "discovery.k8s.io/v1", "kind": "EndpointSlice",
		"metadata": map[string]any{
			"name": "checkout-xyz", "namespace": "prod",
			"labels": map[string]any{"kubernetes.io/service-name": "checkout"},
		},
		"endpoints": []any{map[string]any{"conditions": map[string]any{"ready": true}}},
	}, projection.Diagnostic, 0)

	svc := map[string]any{
		"apiVersion": "v1", "kind": "Service",
		"metadata": map[string]any{"name": "checkout", "namespace": "prod"},
		"spec":     map[string]any{"selector": map[string]any{"app": "checkout"}},
	}

	got := Default().Run(project(t, svc, map[string][]projection.Object{"EndpointSlice": {slice}}))
	if len(got) != 0 {
		t.Errorf("a Service with ready endpoints produced findings: %v", codes(got))
	}
}

func TestSelectorlessServicesAreNotDiagnosed(t *testing.T) {
	// Wired by hand, so there are no pods to match and nothing to report.
	svc := map[string]any{
		"apiVersion": "v1", "kind": "Service",
		"metadata": map[string]any{"name": "external-db", "namespace": "prod"},
		"spec":     map[string]any{"ports": []any{map[string]any{"port": float64(5432)}}},
	}
	if got := Default().Run(project(t, svc, nil)); len(got) != 0 {
		t.Errorf("a selectorless Service produced findings: %v", codes(got))
	}
}

// Every finding an analyzer emits has to pass the envelope's own validation, or
// the code is unregistered and the agent has nothing stable to branch on.
func TestEveryAnalyzerEmitsRegisteredCodes(t *testing.T) {
	reg := Default()
	if len(reg.Kinds()) == 0 {
		t.Fatal("the default registry is empty")
	}
	for _, c := range envelope.Codes() {
		if _, ok := envelope.Describe(c); !ok {
			t.Errorf("code %s has no description", c)
		}
	}
	if needs := reg.NeedsFor("Service"); len(needs) == 0 {
		t.Error("the Service analyzers declare no neighbourhood, so the runner cannot feed them")
	}
}
