package projection

import "strings"

// Project turns a decoded Kubernetes object into its projected form for a
// profile: the noise removed, and only the paths that profile declares.
//
// v0.1 selects paths from a per-kind table rather than diffing against the
// schema defaults. That ordering is deliberate — the table is what the three
// v0.1 tools need, and it is honest about the case the README warns of: for a
// kind with no entry the result is identity plus the whole status, which is
// mediocre exactly where generic projection is needed most, on a homegrown CRD.
// A declarative Projection per GVK is the fix, and it lands with the analyzers.
func Project(u map[string]any, profile Profile, maxAnnotationBytes int) Object {
	Strip(u, maxAnnotationBytes)

	obj := Object{
		GVK:       gvkOf(u),
		Kind:      stringAt(u, "kind"),
		Name:      stringAt(u, "metadata.name"),
		Namespace: stringAt(u, "metadata.namespace"),
		Profile:   profile,
	}
	obj.ID = NewID(obj.Namespace, obj.Kind, obj.Name)

	if profile == Full {
		obj.Fields = u
		return obj
	}

	paths, known := keepPaths[obj.Kind][profile]
	if !known {
		// No table entry: identity plus the whole status. Documented as the
		// weak case rather than silently pretending to project.
		paths = fallbackPaths[profile]
	}
	obj.Fields = pick(u, paths)
	return obj
}

// keepPaths declares what survives projection, per kind and profile.
//
// Minimal answers "what is this and what state is it in". Diagnostic adds
// whatever explains a failure and nothing else: the fields an analyzer reads to
// emit a reason code. Anything not listed is noise for that profile, including
// fields that are perfectly interesting for other purposes.
var keepPaths = map[string]map[Profile][]string{
	"Pod": {
		Minimal: {"status.phase", "spec.nodeName"},
		Diagnostic: {
			"status.phase", "status.conditions", "status.containerStatuses",
			"status.initContainerStatuses", "status.message", "status.reason",
			"spec.nodeName", "spec.nodeSelector", "spec.tolerations",
			"spec.containers", "spec.volumes",
			"metadata.ownerReferences", "metadata.labels",
			"metadata.deletionTimestamp", "metadata.finalizers",
		},
	},
	"Deployment": {
		Minimal: {"spec.replicas", "status.readyReplicas", "status.availableReplicas"},
		Diagnostic: {
			"spec.replicas", "spec.selector", "spec.strategy",
			"status.replicas", "status.readyReplicas", "status.availableReplicas",
			"status.unavailableReplicas", "status.updatedReplicas", "status.conditions",
			"metadata.labels",
		},
	},
	"ReplicaSet": {
		Minimal: {"spec.replicas", "status.readyReplicas"},
		Diagnostic: {
			"spec.replicas", "spec.selector", "status.replicas", "status.readyReplicas",
			"status.conditions", "metadata.ownerReferences",
		},
	},
	"StatefulSet": {
		Minimal: {"spec.replicas", "status.readyReplicas"},
		Diagnostic: {
			"spec.replicas", "spec.selector", "spec.volumeClaimTemplates",
			"status.replicas", "status.readyReplicas", "status.conditions", "metadata.labels",
		},
	},
	"DaemonSet": {
		Minimal: {"status.desiredNumberScheduled", "status.numberReady"},
		Diagnostic: {
			"spec.selector", "status.desiredNumberScheduled", "status.numberReady",
			"status.numberUnavailable", "status.conditions", "metadata.labels",
		},
	},
	"Service": {
		Minimal: {"spec.type", "spec.selector", "spec.ports"},
		Diagnostic: {
			"spec.type", "spec.selector", "spec.ports", "spec.clusterIP",
			"status.loadBalancer", "metadata.labels",
		},
	},
	"PersistentVolumeClaim": {
		Minimal: {"status.phase", "spec.storageClassName"},
		Diagnostic: {
			"status.phase", "status.conditions", "spec.storageClassName",
			"spec.accessModes", "spec.resources", "spec.volumeName",
		},
	},
	"Node": {
		Minimal: {"status.conditions", "spec.unschedulable"},
		Diagnostic: {
			"status.conditions", "status.allocatable", "status.capacity",
			"spec.taints", "spec.unschedulable", "metadata.labels",
		},
	},
	"EndpointSlice": {
		Minimal:    {"endpoints", "ports", "addressType"},
		Diagnostic: {"endpoints", "ports", "addressType", "metadata.labels", "metadata.ownerReferences"},
	},
	"Event": {
		Minimal: {"reason", "count", "type", "involvedObject", "lastTimestamp"},
		Diagnostic: {
			"reason", "count", "type", "message", "involvedObject",
			"firstTimestamp", "lastTimestamp",
		},
	},
}

var fallbackPaths = map[Profile][]string{
	Minimal:    {"status.phase", "status.conditions"},
	Diagnostic: {"status", "metadata.labels", "metadata.ownerReferences"},
}

// pick builds a new map holding only the given dotted paths. A path whose leaf
// is a map or slice brings that whole subtree.
func pick(src map[string]any, paths []string) map[string]any {
	out := map[string]any{}
	for _, p := range paths {
		if v, ok := valueAt(src, p); ok {
			set(out, strings.Split(p, "."), v)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func valueAt(src map[string]any, path string) (any, bool) {
	cur := any(src)
	for _, seg := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[seg]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

func set(dst map[string]any, segs []string, v any) {
	for i, seg := range segs {
		if i == len(segs)-1 {
			dst[seg] = v
			return
		}
		next, ok := dst[seg].(map[string]any)
		if !ok {
			next = map[string]any{}
			dst[seg] = next
		}
		dst = next
	}
}

func stringAt(src map[string]any, path string) string {
	if v, ok := valueAt(src, path); ok {
		if s, isString := v.(string); isString {
			return s
		}
	}
	return ""
}

// gvkOf renders the group/version/kind triple the cache is configured by.
func gvkOf(u map[string]any) string {
	apiVersion := stringAt(u, "apiVersion")
	kind := stringAt(u, "kind")
	if apiVersion == "" {
		return kind
	}
	return apiVersion + "/" + kind
}
