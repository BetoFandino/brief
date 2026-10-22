package projection

import "fmt"

// LastAppliedConfig holds a full copy of the manifest the client sent. It is the
// second largest source of noise after managedFields and has no diagnostic
// value whatsoever: whatever it says, the object itself says better.
const LastAppliedConfig = "kubectl.kubernetes.io/last-applied-configuration"

// metadataNoise are the metadata fields the API server maintains for
// controllers and that nobody reads while diagnosing an incident.
//
// Deliberately absent: finalizers (they explain an object stuck Terminating),
// ownerReferences (the graph is built from them), creationTimestamp (age is
// diagnostic), labels and annotations (routing and convention failures live
// there), and deletionTimestamp.
var metadataNoise = []string{
	"managedFields",
	"resourceVersion",
	"uid",
	"selfLink",
	"generation",
}

// Strip removes structural noise from a decoded Kubernetes object, in place,
// and reports roughly how many bytes it dropped.
//
// It is meant to run inside the informer's TransformFunc, before the object is
// stored, so the saving lands in cache memory and not only in the response.
// Stripping at serialization time would leave the cache just as large, which is
// what decides whether this is viable on a cluster with 40,000 objects.
//
// maxAnnotationBytes is the ceiling above which an annotation's value is
// replaced by a marker recording its size; 0 disables that check. The value is
// replaced rather than deleted so that "an annotation changed" stays
// diagnosable — the same reasoning that has Secrets report key names and sizes
// instead of values.
func Strip(obj map[string]any, maxAnnotationBytes int) int {
	md, ok := obj["metadata"].(map[string]any)
	if !ok {
		return 0
	}

	dropped := 0
	for _, key := range metadataNoise {
		if v, present := md[key]; present {
			dropped += approxSize(v)
			delete(md, key)
		}
	}

	anns, ok := md["annotations"].(map[string]any)
	if !ok {
		return dropped
	}
	for key, v := range anns {
		s, isString := v.(string)
		if !isString {
			continue
		}
		switch {
		case key == LastAppliedConfig:
			dropped += len(s)
			delete(anns, key)
		case maxAnnotationBytes > 0 && len(s) > maxAnnotationBytes:
			dropped += len(s)
			anns[key] = fmt.Sprintf("<dropped by brief: %d bytes>", len(s))
		}
	}
	if len(anns) == 0 {
		delete(md, "annotations")
	}
	return dropped
}

// approxSize estimates the serialized size of a decoded JSON value.
//
// It is an estimate on purpose: this runs in the informer transform on every
// object of every watched GVK, so it must not marshal. The number feeds a
// metric, not a budget.
func approxSize(v any) int {
	switch t := v.(type) {
	case nil:
		return 4
	case bool:
		return 5
	case string:
		return len(t) + 2
	case float64, float32, int, int32, int64:
		return 8
	case map[string]any:
		n := 2
		for key, val := range t {
			n += len(key) + 4 + approxSize(val)
		}
		return n
	case []any:
		n := 2
		for _, val := range t {
			n += approxSize(val) + 1
		}
		return n
	default:
		return 8
	}
}
