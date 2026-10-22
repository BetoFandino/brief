// Package projection turns a Kubernetes object into its minimal diagnostic
// form: only what deviates from the defaults, plus the status that explains a
// failure.
//
// v0.1 implements the half of that which needs no Kubernetes dependency —
// removing structural noise, which is also the larger half of the saving. In a
// typical object managedFields alone can double the payload. Diffing against
// schema defaults needs the API server's OpenAPI plus structured-merge-diff and
// arrives with the cache layer.
package projection

// Profile selects how much of an object survives projection.
type Profile string

const (
	// Minimal keeps identity and state. The profile every listing uses.
	Minimal Profile = "minimal"
	// Diagnostic keeps identity, state, and whatever explains a failure.
	Diagnostic Profile = "diagnostic"
	// Full keeps the complete projection, for inspecting one object. A
	// listing that uses Full is a bug: see the MCP surface rules in README.md.
	Full Profile = "full"
)

// ID is the stable reference for an object, in ns/kind/name form (kind/name
// when cluster-scoped). It is reusable verbatim as another tool's input, so the
// agent can chain calls without rebuilding references.
type ID string

// NewID builds the stable ID for an object. Namespace is empty for
// cluster-scoped kinds.
func NewID(namespace, kind, name string) ID {
	if namespace == "" {
		return ID(kind + "/" + name)
	}
	return ID(namespace + "/" + kind + "/" + name)
}

// Object is a projected Kubernetes object: the fields that survived, with the
// identity hoisted out so callers never have to dig for it.
//
// Resource carries the plural API resource name ("pods"), not the Kind, because
// that is what SubjectAccessReview asks about. Whoever builds the Object has a
// RESTMapper; the rbac package does not.
type Object struct {
	ID        ID             `json:"id"`
	GVK       string         `json:"gvk"`
	Resource  string         `json:"-"`
	Namespace string         `json:"namespace,omitempty"`
	Kind      string         `json:"kind"`
	Name      string         `json:"name"`
	Profile   Profile        `json:"profile,omitempty"`
	Fields    map[string]any `json:"fields,omitempty"`
}
