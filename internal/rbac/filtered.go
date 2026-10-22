// Package rbac honors the caller's own permissions.
//
// The service answers from a cache built with its own ServiceAccount, which is
// broad by necessity, so every response has to be filtered by what the caller
// could have read themselves with their own kubeconfig (principle 5). A shared
// server that reads with its own permissions is privilege escalation dressed as
// a productivity tool.
//
// The guarantee here is structural rather than documentary. FilteredObjects
// keeps its objects in an unexported field, so no package outside this one can
// put an object inside one, and the envelope accepts objects in no other form.
// A tool cannot serialize data that never passed through Filter — not because a
// checklist forbids it, but because it does not compile.
//
// Why SubjectAccessReview and not impersonation: impersonation filters exactly
// and is simpler, but it needs the `impersonate` verb on users and groups,
// which is effectively a master key — compromising the pod becomes compromising
// any identity in the cluster. SAR needs only `create subjectaccessreviews`,
// which grants read access to nothing. The cost is this hand-written filtering,
// and granularity at namespace and resource level rather than per object.
package rbac

import (
	"context"
	"fmt"
	"sort"

	"github.com/BetoFandino/brief/internal/projection"
)

// Scope is one authorization question: may this subject perform verb on this
// resource in this namespace?
type Scope struct {
	Verb      string `json:"verb"`
	Resource  string `json:"resource"`
	Namespace string `json:"namespace,omitempty"`
}

func (s Scope) String() string {
	if s.Namespace == "" {
		return fmt.Sprintf("%s %s (cluster)", s.Verb, s.Resource)
	}
	return fmt.Sprintf("%s %s in %s", s.Verb, s.Resource, s.Namespace)
}

// Checker answers authorization questions for one caller.
//
// Implementations are expected to cache per (user, verb, resource, namespace)
// with a TTL on the order of a minute, so that a single tool call does not
// become a hundred SubjectAccessReviews against the API server.
type Checker interface {
	Allowed(ctx context.Context, s Scope) (bool, error)
}

// FilteredObjects holds objects that have passed authorization, plus the record
// of what was checked so the audit log can answer "what has the AI been
// reading?" and the response can say what it withheld.
//
// Its zero value is a legitimate empty result: there is no data in it to leak.
type FilteredObjects struct {
	objs    []projection.Object
	allowed []Scope
	denied  []Scope
}

// Objects returns the objects the caller is allowed to see.
func (f FilteredObjects) Objects() []projection.Object { return f.objs }

// Len reports how many objects survived filtering.
func (f FilteredObjects) Len() int { return len(f.objs) }

// Allowed returns the scopes that were granted, for the audit log.
func (f FilteredObjects) Allowed() []Scope { return f.allowed }

// Denied returns the scopes that were refused. A response that withheld
// objects has to say so: silently returning less than was asked for would have
// the agent conclude the cluster is smaller than it is.
func (f FilteredObjects) Denied() []Scope { return f.denied }

// Filter keeps only the objects the caller may read.
//
// Objects are grouped by (namespace, resource) and each group costs one
// authorization question, because that is the granularity SubjectAccessReview
// resolves. A list of 300 pods across 4 namespaces is 4 questions, not 300.
func Filter(ctx context.Context, c Checker, verb string, objs []projection.Object) (FilteredObjects, error) {
	if c == nil {
		return FilteredObjects{}, fmt.Errorf("rbac: nil Checker; use ServiceAccountOnly for single-operator clusters, explicitly")
	}

	var out FilteredObjects
	decisions := make(map[Scope]bool)

	for _, obj := range objs {
		if obj.Resource == "" {
			return FilteredObjects{}, fmt.Errorf("rbac: object %s carries no Resource, so it cannot be authorized", obj.ID)
		}
		scope := Scope{Verb: verb, Resource: obj.Resource, Namespace: obj.Namespace}

		allowed, known := decisions[scope]
		if !known {
			var err error
			allowed, err = c.Allowed(ctx, scope)
			if err != nil {
				return FilteredObjects{}, fmt.Errorf("rbac: checking %s: %w", scope, err)
			}
			decisions[scope] = allowed
		}
		if allowed {
			out.objs = append(out.objs, obj)
		}
	}

	for scope, allowed := range decisions {
		if allowed {
			out.allowed = append(out.allowed, scope)
		} else {
			out.denied = append(out.denied, scope)
		}
	}
	sortScopes(out.allowed)
	sortScopes(out.denied)
	return out, nil
}

func sortScopes(s []Scope) {
	sort.Slice(s, func(i, j int) bool {
		if s[i].Namespace != s[j].Namespace {
			return s[i].Namespace < s[j].Namespace
		}
		return s[i].Resource < s[j].Resource
	})
}

// ServiceAccountOnly allows everything the service itself can read.
//
// It is the `auth.mode: serviceAccountOnly` of the chart: correct for a
// single-operator cluster, and privilege escalation on a shared one. It is a
// named type rather than a nil Checker so that choosing it is visible in the
// code and in the audit log, and never the result of forgetting to pass one.
type ServiceAccountOnly struct{}

func (ServiceAccountOnly) Allowed(context.Context, Scope) (bool, error) { return true, nil }
