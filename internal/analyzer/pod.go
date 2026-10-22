package analyzer

import (
	"strings"

	"github.com/BetoFandino/brief/internal/envelope"
	"github.com/BetoFandino/brief/internal/projection"
)

// oomExitCode is 128 plus SIGKILL. The model should never have to know that,
// which is the entire argument for computing evidence server-side.
const oomExitCode = 137

// podContainers diagnoses what a pod's containers are doing: killed for memory,
// restarting, unable to pull an image, or exiting non-zero.
type podContainers struct{}

func (podContainers) Name() string    { return "pod/containers" }
func (podContainers) Kind() string    { return "Pod" }
func (podContainers) Needs() []string { return nil }

func (podContainers) Analyze(in Input) []envelope.Finding {
	obj := in.Object
	id := string(obj.ID)
	var out []envelope.Finding

	statuses := obj.List("status.containerStatuses")
	statuses = append(statuses, obj.List("status.initContainerStatuses")...)

	for _, cs := range statuses {
		name := projection.Field(cs, "name")
		restarts, _ := projection.Num(cs, "restartCount")

		// An OOM kill is recorded in lastState once the container has been
		// restarted, so looking only at the current state misses the cause of
		// the very loop being diagnosed.
		for _, state := range []map[string]any{
			projection.Sub(cs, "lastState", "terminated"),
			projection.Sub(cs, "state", "terminated"),
		} {
			if state == nil {
				continue
			}
			code, hasCode := projection.Num(state, "exitCode")
			reason := projection.Field(state, "reason")

			switch {
			case reason == "OOMKilled" || (hasCode && code == oomExitCode):
				out = append(out, envelope.Finding{
					Code: envelope.CodeContainerOOMKilled, Severity: envelope.SeverityCritical,
					Objects: []string{id},
					Evidence: map[string]any{
						"container": name, "exit_code": code, "restarts": restarts,
						"limit_memory": memoryLimitOf(obj, name),
						"since":        projection.Field(state, "finishedAt"),
					},
					Hint: "the container asked for more memory than its limit allows; compare the limit against peak usage before raising it",
				})
			case hasCode && code != 0:
				out = append(out, envelope.Finding{
					Code: envelope.CodeContainerNonZeroExit, Severity: envelope.SeverityHigh,
					Objects: []string{id},
					Evidence: map[string]any{
						"container": name, "exit_code": code, "restarts": restarts,
						"reason": reason, "since": projection.Field(state, "finishedAt"),
					},
				})
			}
			break // the current state is only consulted when lastState is absent
		}

		waiting := projection.Sub(cs, "state", "waiting")
		if waiting == nil {
			continue
		}
		reason := projection.Field(waiting, "reason")
		message := projection.Field(waiting, "message")

		switch reason {
		case "CrashLoopBackOff":
			out = append(out, envelope.Finding{
				Code: envelope.CodeCrashLoopBackOff, Severity: envelope.SeverityCritical,
				Objects: []string{id},
				Evidence: map[string]any{
					"container": name, "restarts": restarts, "message": message,
				},
			})
		case "ErrImagePull", "ImagePullBackOff", "ErrImageNeverPull", "InvalidImageName":
			out = append(out, imagePullFinding(id, name, imageOf(obj, name), message))
		}
	}
	return out
}

// imagePullFinding separates a missing image from a credentials failure. Both
// surface as ImagePullBackOff and they have opposite remedies, so collapsing
// them into one code would hand the agent a coin flip.
func imagePullFinding(id, container, image, message string) envelope.Finding {
	lower := strings.ToLower(message)
	authSignals := []string{"unauthorized", "authentication required", "forbidden", "denied", "no basic auth"}
	for _, signal := range authSignals {
		if strings.Contains(lower, signal) {
			return envelope.Finding{
				Code: envelope.CodeImagePullAuth, Severity: envelope.SeverityHigh,
				Objects: []string{id},
				Evidence: map[string]any{
					"container": container, "image": image, "message": message,
				},
				Hint: "the registry rejected the credentials; check imagePullSecrets, not the tag",
			}
		}
	}
	return envelope.Finding{
		Code: envelope.CodeImagePullNotFound, Severity: envelope.SeverityHigh,
		Objects: []string{id},
		Evidence: map[string]any{
			"container": container, "image": image, "message": message,
		},
		Hint: "the registry has no such image or tag; check the tag before the credentials",
	}
}

// podScheduling diagnoses a pod that no node will take.
type podScheduling struct{}

func (podScheduling) Name() string    { return "pod/scheduling" }
func (podScheduling) Kind() string    { return "Pod" }
func (podScheduling) Needs() []string { return []string{"Node"} }

func (podScheduling) Analyze(in Input) []envelope.Finding {
	obj := in.Object
	cond, found := obj.Condition("PodScheduled")
	if !found || projection.Field(cond, "status") != "False" {
		return nil
	}

	evidence := map[string]any{
		"reason":  projection.Field(cond, "reason"),
		"message": projection.Field(cond, "message"),
	}
	if nodes := in.Related["Node"]; len(nodes) > 0 {
		evidence["nodes_considered"] = len(nodes)
		// The per-node predicate breakdown — "9 nodes on insufficient CPU, 3 on
		// taint node-role/db" — needs the real kube-scheduler framework and
		// belongs to v0.2. Counting the candidates is what v0.1 can say
		// truthfully, and saying less beats inventing a breakdown.
		if tainted := countTainted(nodes); tainted > 0 {
			evidence["nodes_with_taints"] = tainted
		}
	}
	if sel, ok := obj.Value("spec.nodeSelector"); ok {
		if m, isMap := sel.(map[string]any); isMap && len(m) > 0 {
			evidence["node_selector"] = m
		}
	}

	return []envelope.Finding{{
		Code: envelope.CodePodUnschedulable, Severity: envelope.SeverityHigh,
		Objects: []string{string(obj.ID)}, Evidence: evidence,
	}}
}

func countTainted(nodes []projection.Object) int {
	n := 0
	for _, node := range nodes {
		if len(node.List("spec.taints")) > 0 {
			n++
		}
	}
	return n
}

func imageOf(obj projection.Object, container string) string {
	for _, c := range obj.List("spec.containers") {
		if projection.Field(c, "name") == container {
			return projection.Field(c, "image")
		}
	}
	return ""
}

func memoryLimitOf(obj projection.Object, container string) string {
	for _, c := range obj.List("spec.containers") {
		if projection.Field(c, "name") != container {
			continue
		}
		if limits := projection.Sub(c, "resources", "limits"); limits != nil {
			return projection.Field(limits, "memory")
		}
	}
	return ""
}
