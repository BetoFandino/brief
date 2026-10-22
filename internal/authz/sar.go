// Package authz resolves the caller's identity and answers authorization
// questions about them with SubjectAccessReview.
package authz

import (
	"context"
	"fmt"
	"sync"
	"time"

	authnv1 "k8s.io/api/authentication/v1"
	authzv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/BetoFandino/brief/internal/rbac"
)

// Identity is who is calling.
type Identity struct {
	User   string
	Groups []string
	UID    string
}

func (i Identity) String() string {
	if i.User == "" {
		return "anonymous"
	}
	return i.User
}

// Resolver turns a request's bearer token into an identity.
type Resolver interface {
	Resolve(ctx context.Context, token string) (Identity, error)
}

// TokenReviewResolver asks the API server who a token belongs to.
//
// The service needs `create tokenreviews`, which grants read access to nothing.
// This is the counterpart to SubjectAccessReview: one call learns who is
// asking, the other learns what they may see, and neither requires the
// impersonation verb that would make the pod a master key.
type TokenReviewResolver struct {
	Client kubernetes.Interface
	// Audiences the token must be valid for; empty accepts the API server's own.
	Audiences []string
}

func (r TokenReviewResolver) Resolve(ctx context.Context, token string) (Identity, error) {
	if token == "" {
		return Identity{}, fmt.Errorf("authz: no bearer token on the request")
	}
	review := &authnv1.TokenReview{
		Spec: authnv1.TokenReviewSpec{Token: token, Audiences: r.Audiences},
	}
	got, err := r.Client.AuthenticationV1().TokenReviews().Create(ctx, review, metav1.CreateOptions{})
	if err != nil {
		return Identity{}, fmt.Errorf("authz: token review: %w", err)
	}
	if !got.Status.Authenticated {
		return Identity{}, fmt.Errorf("authz: token is not authenticated: %s", got.Status.Error)
	}
	return Identity{
		User:   got.Status.User.Username,
		Groups: got.Status.User.Groups,
		UID:    got.Status.User.UID,
	}, nil
}

// StaticResolver returns a fixed identity. It is what `auth.mode:
// serviceAccountOnly` uses, and what tests use — a named type so that choosing
// it is visible in the code and in the audit log.
type StaticResolver struct{ Identity Identity }

func (r StaticResolver) Resolve(context.Context, string) (Identity, error) { return r.Identity, nil }

// SARChecker answers rbac questions for one identity via SubjectAccessReview,
// caching each answer briefly.
//
// Without the cache a single triage_cluster over a dozen namespaces becomes a
// hundred round trips to the API server, which would make the tool slower than
// the kubectl walk it replaces.
type SARChecker struct {
	client   kubernetes.Interface
	identity Identity
	ttl      time.Duration

	mu    sync.Mutex
	cache map[rbac.Scope]decision
	now   func() time.Time
}

type decision struct {
	allowed bool
	expires time.Time
}

// NewSARChecker builds a checker for one caller. A zero ttl uses one minute,
// the order of magnitude the design calls for: long enough to collapse a tool
// call's questions, short enough that revoking a RoleBinding takes effect while
// someone is still watching.
func NewSARChecker(client kubernetes.Interface, id Identity, ttl time.Duration) *SARChecker {
	if ttl <= 0 {
		ttl = time.Minute
	}
	return &SARChecker{
		client: client, identity: id, ttl: ttl,
		cache: map[rbac.Scope]decision{}, now: time.Now,
	}
}

func (c *SARChecker) Allowed(ctx context.Context, s rbac.Scope) (bool, error) {
	c.mu.Lock()
	if d, found := c.cache[s]; found && c.now().Before(d.expires) {
		c.mu.Unlock()
		return d.allowed, nil
	}
	c.mu.Unlock()

	review := &authzv1.SubjectAccessReview{
		Spec: authzv1.SubjectAccessReviewSpec{
			User:   c.identity.User,
			Groups: c.identity.Groups,
			UID:    c.identity.UID,
			ResourceAttributes: &authzv1.ResourceAttributes{
				Verb:      s.Verb,
				Resource:  s.Resource,
				Namespace: s.Namespace,
			},
		},
	}
	got, err := c.client.AuthorizationV1().SubjectAccessReviews().Create(ctx, review, metav1.CreateOptions{})
	if err != nil {
		// Fail closed. A SAR that cannot be answered is not permission.
		return false, fmt.Errorf("authz: subject access review for %s: %w", s, err)
	}

	c.mu.Lock()
	c.cache[s] = decision{allowed: got.Status.Allowed, expires: c.now().Add(c.ttl)}
	c.mu.Unlock()
	return got.Status.Allowed, nil
}

// Identity reports whose permissions this checker enforces, for the audit log.
func (c *SARChecker) Identity() Identity { return c.identity }
