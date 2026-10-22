package analyzer

import (
	"github.com/BetoFandino/brief/internal/envelope"
	"github.com/BetoFandino/brief/internal/projection"
)

// workloadReplicas reports a workload running fewer replicas than it was asked
// for. It applies to every controller kind, since the arithmetic is the same and
// only the field names differ.
type workloadReplicas struct{}

func (workloadReplicas) Name() string    { return "workload/replicas" }
func (workloadReplicas) Kind() string    { return "Deployment" }
func (workloadReplicas) Needs() []string { return nil }

func (workloadReplicas) Analyze(in Input) []envelope.Finding {
	obj := in.Object

	// An absent replicas means the default of one; an absent readyReplicas
	// means none are ready, which is the case that matters most.
	want := obj.IntOr("spec.replicas", 1)
	ready := obj.IntOr("status.readyReplicas", 0)
	if ready >= want {
		return nil
	}

	severity := envelope.SeverityHigh
	if ready == 0 {
		// Nothing is serving. This is the whole workload down, not a degraded
		// one, and the agent should see that distinction without doing the
		// comparison itself.
		severity = envelope.SeverityCritical
	}

	evidence := map[string]any{
		"desired_replicas": want,
		"ready_replicas":   ready,
		"missing_replicas": want - ready,
	}
	if unavailable, ok := obj.Int("status.unavailableReplicas"); ok {
		evidence["unavailable_replicas"] = unavailable
	}
	if cond, found := obj.Condition("Available"); found && projection.Field(cond, "status") == "False" {
		evidence["available_condition"] = projection.Field(cond, "reason")
		evidence["message"] = projection.Field(cond, "message")
	}
	if cond, found := obj.Condition("Progressing"); found && projection.Field(cond, "status") == "False" {
		evidence["progressing_condition"] = projection.Field(cond, "reason")
	}

	return []envelope.Finding{{
		Code: envelope.CodeReplicasUnavailable, Severity: severity,
		Objects: []string{string(obj.ID)}, Evidence: evidence,
	}}
}

// pvcBinding reports a claim that never bound. The pod that mounts it is
// Pending, but the cause is here, two hops away — reporting the pod is the
// failure mode this analyzer exists to prevent.
type pvcBinding struct{}

func (pvcBinding) Name() string    { return "pvc/binding" }
func (pvcBinding) Kind() string    { return "PersistentVolumeClaim" }
func (pvcBinding) Needs() []string { return nil }

func (pvcBinding) Analyze(in Input) []envelope.Finding {
	obj := in.Object
	phase := obj.Str("status.phase")
	if phase == "Bound" || phase == "" {
		return nil
	}

	evidence := map[string]any{"phase": phase}
	if sc := obj.Str("spec.storageClassName"); sc != "" {
		evidence["storage_class"] = sc
	}
	if requested := projection.Sub(obj.Fields, "spec", "resources", "requests"); requested != nil {
		evidence["requested_storage"] = projection.Field(requested, "storage")
	}
	if modes, ok := obj.Value("spec.accessModes"); ok {
		evidence["access_modes"] = modes
	}

	return []envelope.Finding{{
		Code: envelope.CodePVCUnbound, Severity: envelope.SeverityHigh,
		Objects: []string{string(obj.ID)}, Evidence: evidence,
		Hint: "no volume satisfies the claim; check that the StorageClass exists and that a provisioner serves it",
	}}
}

// serviceEndpoints reports a Service whose selector matches no ready pod.
//
// This is the failure with no error anywhere: every pod is healthy, the Service
// exists, and traffic goes nowhere. An agent that looks for objects in a bad
// state calls this cluster fine, which is exactly why the edge has to be
// checked rather than the objects.
type serviceEndpoints struct{}

func (serviceEndpoints) Name() string    { return "service/endpoints" }
func (serviceEndpoints) Kind() string    { return "Service" }
func (serviceEndpoints) Needs() []string { return []string{"EndpointSlice", "Pod"} }

func (serviceEndpoints) Analyze(in Input) []envelope.Finding {
	obj := in.Object

	selector := map[string]any{}
	if v, ok := obj.Value("spec.selector"); ok {
		selector, _ = v.(map[string]any)
	}
	// A Service with no selector is wired by hand and has no pods to match.
	if len(selector) == 0 {
		return nil
	}
	if obj.Str("spec.type") == "ExternalName" {
		return nil
	}

	ready := 0
	for _, slice := range in.Related["EndpointSlice"] {
		if slice.Labels()["kubernetes.io/service-name"] != obj.Name {
			continue
		}
		for _, endpoint := range slice.List("endpoints") {
			if conditions := projection.Sub(endpoint, "conditions"); conditions != nil {
				if isReady, ok := conditions["ready"].(bool); ok && isReady {
					ready++
				}
			}
		}
	}
	if ready > 0 {
		return nil
	}

	evidence := map[string]any{"selector": selector, "ready_endpoints": 0}

	// Naming the near miss is the difference between "no endpoints" and "your
	// selector says checkut and your pods say checkout".
	if near := nearestLabels(selector, in.Related["Pod"]); near != nil {
		evidence["pods_in_namespace_with_similar_labels"] = near
	}

	return []envelope.Finding{{
		Code: envelope.CodeNoEndpoints, Severity: envelope.SeverityHigh,
		Objects: []string{string(obj.ID)}, Evidence: evidence,
		Hint: "nothing is in an error state: the Service selector matches no ready pod, so compare it against the pods' own labels",
	}}
}

// nearestLabels returns the label set of a pod that shares a selector key but
// not its value — the shape a typo takes.
func nearestLabels(selector map[string]any, pods []projection.Object) map[string]string {
	for _, pod := range pods {
		labels := pod.Labels()
		if len(labels) == 0 {
			continue
		}
		for key, want := range selector {
			got, present := labels[key]
			if present && got != want {
				return labels
			}
		}
	}
	return nil
}
