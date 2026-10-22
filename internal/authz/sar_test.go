package authz_test

import (
	"context"
	"testing"
	"time"

	authzv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/BetoFandino/brief/internal/authz"
	"github.com/BetoFandino/brief/internal/rbac"
)

// sarClient answers SubjectAccessReviews with allow for the listed namespaces,
// counting how many actually reached the API server.
func sarClient(allow map[string]bool, fail error) (*fake.Clientset, *int) {
	client := fake.NewSimpleClientset()
	calls := 0
	client.PrependReactor("create", "subjectaccessreviews",
		func(action k8stesting.Action) (bool, runtime.Object, error) {
			calls++
			if fail != nil {
				return true, nil, fail
			}
			review := action.(k8stesting.CreateAction).GetObject().(*authzv1.SubjectAccessReview)
			review.Status.Allowed = allow[review.Spec.ResourceAttributes.Namespace]
			return true, review, nil
		})
	return client, &calls
}

func TestSARCheckerEnforcesTheCallersPermissions(t *testing.T) {
	client, calls := sarClient(map[string]bool{"prod": true}, nil)
	c := authz.NewSARChecker(client, authz.Identity{User: "beto@example.com"}, time.Minute)

	allowed, err := c.Allowed(context.Background(), rbac.Scope{Verb: "get", Resource: "pods", Namespace: "prod"})
	if err != nil || !allowed {
		t.Errorf("prod: allowed=%v err=%v, want allowed", allowed, err)
	}
	allowed, err = c.Allowed(context.Background(), rbac.Scope{Verb: "get", Resource: "pods", Namespace: "secret-ns"})
	if err != nil {
		t.Fatal(err)
	}
	if allowed {
		t.Error("secret-ns was allowed")
	}
	if *calls != 2 {
		t.Errorf("made %d reviews for 2 distinct scopes", *calls)
	}
}

// Without the cache a single triage over a dozen namespaces becomes a hundred
// round trips, which would make the tool slower than the walk it replaces.
func TestSARAnswersAreCachedWithinTheTTL(t *testing.T) {
	client, calls := sarClient(map[string]bool{"prod": true}, nil)
	c := authz.NewSARChecker(client, authz.Identity{User: "u"}, time.Minute)

	scope := rbac.Scope{Verb: "get", Resource: "pods", Namespace: "prod"}
	for i := 0; i < 50; i++ {
		if _, err := c.Allowed(context.Background(), scope); err != nil {
			t.Fatal(err)
		}
	}
	if *calls != 1 {
		t.Errorf("50 identical questions cost %d reviews, want 1", *calls)
	}
}

// A revoked RoleBinding has to take effect while someone is still watching, so
// the cache must actually expire.
func TestSARCacheExpires(t *testing.T) {
	client, calls := sarClient(map[string]bool{"prod": true}, nil)
	c := authz.NewSARChecker(client, authz.Identity{User: "u"}, 10*time.Millisecond)

	scope := rbac.Scope{Verb: "get", Resource: "pods", Namespace: "prod"}
	if _, err := c.Allowed(context.Background(), scope); err != nil {
		t.Fatal(err)
	}
	time.Sleep(25 * time.Millisecond)
	if _, err := c.Allowed(context.Background(), scope); err != nil {
		t.Fatal(err)
	}
	if *calls != 2 {
		t.Errorf("the answer was reused past its TTL: %d reviews", *calls)
	}
}

// A SubjectAccessReview that cannot be answered is not permission.
func TestSARFailsClosed(t *testing.T) {
	client, _ := sarClient(nil, context.DeadlineExceeded)
	c := authz.NewSARChecker(client, authz.Identity{User: "u"}, time.Minute)

	allowed, err := c.Allowed(context.Background(), rbac.Scope{Verb: "get", Resource: "pods", Namespace: "prod"})
	if err == nil {
		t.Error("a failed review was swallowed")
	}
	if allowed {
		t.Error("a failed review degraded into permission")
	}
}

func TestAnonymousIdentityIsNamedInTheAuditLog(t *testing.T) {
	if got := (authz.Identity{}).String(); got != "anonymous" {
		t.Errorf("an empty identity renders as %q", got)
	}
	if got := (authz.Identity{User: "beto@example.com"}).String(); got != "beto@example.com" {
		t.Errorf("identity renders as %q", got)
	}
}
