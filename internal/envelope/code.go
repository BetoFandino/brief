package envelope

import (
	"fmt"
	"sort"
)

// Severity ranks a finding. Truncation drops the least severe first, so this
// ordering is load-bearing, not cosmetic.
type Severity string

const (
	SeverityCritical Severity = "critical"
	SeverityHigh     Severity = "high"
	SeverityMedium   Severity = "medium"
	SeverityLow      Severity = "low"
)

func (s Severity) rank() int {
	switch s {
	case SeverityCritical:
		return 0
	case SeverityHigh:
		return 1
	case SeverityMedium:
		return 2
	case SeverityLow:
		return 3
	default:
		return 4
	}
}

// Code is a normalized reason code. The server emits codes; the model writes
// the prose. Free text here would have every analyzer invent its own wording
// for the same failure, and the agent would have no stable thing to branch on.
type Code string

// The v0.1 code set. It covers the failures the three v0.1 tools can actually
// reach without the graph or the history layer.
const (
	CodeContainerOOMKilled    Code = "CONTAINER_OOM_KILLED"
	CodeCrashLoopBackOff      Code = "CONTAINER_CRASH_LOOP"
	CodeImagePullAuth         Code = "IMAGE_PULL_AUTH_FAILED"
	CodeImagePullNotFound     Code = "IMAGE_NOT_FOUND"
	CodeContainerNonZeroExit  Code = "CONTAINER_EXITED_NONZERO"
	CodeReadinessProbeFailing Code = "READINESS_PROBE_FAILING"
	CodeLivenessProbeFailing  Code = "LIVENESS_PROBE_FAILING"
	CodePodUnschedulable      Code = "POD_UNSCHEDULABLE"
	CodePVCUnbound            Code = "PVC_UNBOUND"
	CodeReplicasUnavailable   Code = "REPLICAS_UNAVAILABLE"
	CodeNoEndpoints           Code = "SERVICE_HAS_NO_ENDPOINTS"
)

var registered = map[Code]string{
	CodeContainerOOMKilled:    "container was killed for exceeding its memory limit",
	CodeCrashLoopBackOff:      "container is restarting repeatedly",
	CodeImagePullAuth:         "image pull failed on registry credentials",
	CodeImagePullNotFound:     "image or tag does not exist",
	CodeContainerNonZeroExit:  "container exited with a non-zero status",
	CodeReadinessProbeFailing: "readiness probe is failing, so the pod takes no traffic",
	CodeLivenessProbeFailing:  "liveness probe is failing, so the kubelet keeps restarting it",
	CodePodUnschedulable:      "no node satisfies the pod's requirements",
	CodePVCUnbound:            "PersistentVolumeClaim is not bound",
	CodeReplicasUnavailable:   "fewer replicas are ready than the spec asks for",
	CodeNoEndpoints:           "Service selector matches no ready pods",
}

// Describe returns the human-readable meaning of a code, and whether it is
// registered at all.
func Describe(c Code) (string, bool) {
	d, ok := registered[c]
	return d, ok
}

// Codes lists every registered code, sorted. Used by the test that keeps the
// set normalized, and by describe_schema-style introspection.
func Codes() []Code {
	out := make([]Code, 0, len(registered))
	for c := range registered {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Finding is one diagnosed problem: a code, what it applies to, and the
// evidence already computed. Evidence holds facts, never inputs for the model
// to compute from — a fact miscalculated by the model is indistinguishable from
// a correct one (principle 4).
type Finding struct {
	Code     Code           `json:"code"`
	Severity Severity       `json:"severity"`
	Objects  []string       `json:"objects"`
	Evidence map[string]any `json:"evidence,omitempty"`
	Hint     string         `json:"hint,omitempty"`
}

func (f Finding) validate() error {
	if _, ok := registered[f.Code]; !ok {
		return fmt.Errorf("envelope: reason code %q is not registered; add it to code.go so the agent has something stable to branch on", f.Code)
	}
	if f.Severity.rank() == 4 {
		return fmt.Errorf("envelope: finding %s carries no valid severity", f.Code)
	}
	if len(f.Objects) == 0 {
		return fmt.Errorf("envelope: finding %s names no objects", f.Code)
	}
	return nil
}
