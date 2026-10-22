package projection

import "testing"

func TestStripRemovesTheNoiseAndKeepsWhatDiagnoses(t *testing.T) {
	obj := map[string]any{
		"metadata": map[string]any{
			"name":              "checkout-7d9f",
			"namespace":         "prod",
			"uid":               "8f14e45f-ea1b-4d2c-9f3a-2b7c1d0e5a66",
			"resourceVersion":   "918273645",
			"generation":        float64(4),
			"selfLink":          "/api/v1/namespaces/prod/pods/checkout-7d9f",
			"creationTimestamp": "2026-10-21T13:00:00Z",
			"managedFields": []any{
				map[string]any{"manager": "kube-controller-manager", "operation": "Update"},
			},
			"finalizers":      []any{"kubernetes.io/pvc-protection"},
			"ownerReferences": []any{map[string]any{"kind": "ReplicaSet", "name": "checkout-7d9f"}},
			"labels":          map[string]any{"app": "checkout"},
			"annotations": map[string]any{
				LastAppliedConfig: `{"apiVersion":"v1","kind":"Pod","metadata":{"name":"checkout"}}`,
				"team":            "payments",
			},
		},
	}

	dropped := Strip(obj, 0)
	if dropped == 0 {
		t.Fatal("Strip reported dropping nothing")
	}

	md := obj["metadata"].(map[string]any)
	for _, gone := range []string{"managedFields", "resourceVersion", "uid", "selfLink", "generation"} {
		if _, present := md[gone]; present {
			t.Errorf("metadata.%s survived Strip", gone)
		}
	}
	// These explain real failures: a stuck Terminating, the owner chain the
	// graph is built from, age, and routing conventions.
	for _, kept := range []string{"name", "namespace", "creationTimestamp", "finalizers", "ownerReferences", "labels"} {
		if _, present := md[kept]; !present {
			t.Errorf("metadata.%s was dropped but is diagnostic", kept)
		}
	}

	anns := md["annotations"].(map[string]any)
	if _, present := anns[LastAppliedConfig]; present {
		t.Error("last-applied-configuration survived Strip")
	}
	if anns["team"] != "payments" {
		t.Errorf("ordinary annotation was altered: %v", anns["team"])
	}
}

func TestStripMarksOversizedAnnotationsInsteadOfHidingThem(t *testing.T) {
	big := make([]byte, 2048)
	for i := range big {
		big[i] = 'x'
	}
	obj := map[string]any{
		"metadata": map[string]any{
			"annotations": map[string]any{"blob": string(big), "small": "ok"},
		},
	}

	Strip(obj, 1024)

	anns := obj["metadata"].(map[string]any)["annotations"].(map[string]any)
	got, ok := anns["blob"].(string)
	if !ok {
		t.Fatal("oversized annotation was deleted; a changed annotation must stay diagnosable")
	}
	if got == string(big) {
		t.Error("oversized annotation was not dropped")
	}
	if want := "<dropped by brief: 2048 bytes>"; got != want {
		t.Errorf("marker = %q, want %q", got, want)
	}
	if anns["small"] != "ok" {
		t.Error("annotation under the ceiling was altered")
	}
}

func TestStripSurvivesObjectsWithoutMetadata(t *testing.T) {
	if n := Strip(map[string]any{"kind": "Pod"}, 1024); n != 0 {
		t.Errorf("Strip on a metadata-less object reported %d bytes dropped", n)
	}
}

func TestNewIDOmitsNamespaceWhenClusterScoped(t *testing.T) {
	if got := NewID("prod", "Deployment", "checkout"); got != "prod/Deployment/checkout" {
		t.Errorf("namespaced ID = %q", got)
	}
	if got := NewID("", "Node", "ip-10-0-1-7"); got != "Node/ip-10-0-1-7" {
		t.Errorf("cluster-scoped ID = %q", got)
	}
}
