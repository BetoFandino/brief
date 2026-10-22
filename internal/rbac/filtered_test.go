package rbac_test

import (
	"context"
	"errors"
	"testing"

	"github.com/BetoFandino/brief/internal/projection"
	"github.com/BetoFandino/brief/internal/rbac"
)

type checkerFunc func(rbac.Scope) (bool, error)

func (f checkerFunc) Allowed(_ context.Context, s rbac.Scope) (bool, error) { return f(s) }

func pod(ns, name string) projection.Object {
	return projection.Object{
		ID: projection.NewID(ns, "Pod", name), Kind: "Pod", Resource: "pods",
		Namespace: ns, Name: name, GVK: "v1/Pod",
	}
}

func TestFilterKeepsOnlyWhatTheCallerMayRead(t *testing.T) {
	objs := []projection.Object{pod("prod", "a"), pod("secret-ns", "b"), pod("prod", "c")}

	got, err := rbac.Filter(context.Background(),
		checkerFunc(func(s rbac.Scope) (bool, error) { return s.Namespace == "prod", nil }),
		"get", objs)
	if err != nil {
		t.Fatalf("Filter: %v", err)
	}

	if got.Len() != 2 {
		t.Fatalf("kept %d objects, want 2", got.Len())
	}
	for _, o := range got.Objects() {
		if o.Namespace != "prod" {
			t.Errorf("kept an object from %q", o.Namespace)
		}
	}
	if len(got.Denied()) != 1 || got.Denied()[0].Namespace != "secret-ns" {
		t.Errorf("Denied() = %v, want the secret-ns scope", got.Denied())
	}
	if len(got.Allowed()) != 1 || got.Allowed()[0].Namespace != "prod" {
		t.Errorf("Allowed() = %v, want the prod scope", got.Allowed())
	}
}

// One authorization question per (namespace, resource) is what keeps a single
// tool call from becoming a hundred SubjectAccessReviews.
func TestFilterAsksOneQuestionPerNamespaceAndResource(t *testing.T) {
	var asked int
	var objs []projection.Object
	for i := 0; i < 50; i++ {
		objs = append(objs, pod("prod", "p"), pod("staging", "p"))
	}

	if _, err := rbac.Filter(context.Background(),
		checkerFunc(func(rbac.Scope) (bool, error) { asked++; return true, nil }), "get", objs); err != nil {
		t.Fatal(err)
	}
	if asked != 2 {
		t.Errorf("asked %d questions for 100 objects across 2 namespaces, want 2", asked)
	}
}

func TestFilterRefusesObjectsItCannotAuthorize(t *testing.T) {
	bare := projection.Object{ID: "prod/Pod/a", Kind: "Pod", Namespace: "prod"} // no Resource
	if _, err := rbac.Filter(context.Background(), rbac.ServiceAccountOnly{}, "get",
		[]projection.Object{bare}); err == nil {
		t.Error("authorized an object carrying no API resource name")
	}
}

// serviceAccountOnly has to be an explicit choice, never the result of
// forgetting to pass a Checker.
func TestFilterRefusesANilChecker(t *testing.T) {
	if _, err := rbac.Filter(context.Background(), nil, "get", nil); err == nil {
		t.Error("accepted a nil Checker")
	}
}

// A failed SubjectAccessReview must not degrade into "allowed". Failing closed
// is the only safe direction here.
func TestFilterFailsClosedWhenTheCheckerErrors(t *testing.T) {
	boom := errors.New("apiserver unreachable")
	got, err := rbac.Filter(context.Background(),
		checkerFunc(func(rbac.Scope) (bool, error) { return false, boom }),
		"get", []projection.Object{pod("prod", "a")})
	if err == nil {
		t.Fatal("Filter swallowed a checker failure")
	}
	if !errors.Is(err, boom) {
		t.Errorf("error does not wrap the cause: %v", err)
	}
	if got.Len() != 0 {
		t.Errorf("returned %d objects alongside an error", got.Len())
	}
}

func TestServiceAccountOnlyAllowsEverything(t *testing.T) {
	got, err := rbac.Filter(context.Background(), rbac.ServiceAccountOnly{}, "get",
		[]projection.Object{pod("prod", "a"), pod("secret-ns", "b")})
	if err != nil {
		t.Fatal(err)
	}
	if got.Len() != 2 {
		t.Errorf("kept %d objects, want 2", got.Len())
	}
	if len(got.Denied()) != 0 {
		t.Errorf("Denied() = %v, want empty", got.Denied())
	}
}
