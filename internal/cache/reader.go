// Package cache is the read side of the service: everything a tool call needs,
// served from memory, without touching the API server.
//
// The Reader interface is deliberately narrow. The tools depend on it rather
// than on controller-runtime, which means the whole diagnostic layer is
// testable against a fake with no cluster, no API server and no kind — and it
// keeps the informer machinery in one place that can be replaced without
// touching a single tool.
package cache

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/BetoFandino/brief/internal/projection"
)

// Query asks the cache for objects of one GVK.
//
// Name set means "this one object"; empty means a listing. There is no separate
// Get on the interface because a fake then has one method to implement and one
// place where profile and limit are honored.
type Query struct {
	GVK       string
	Namespace string
	Name      string
	Labels    map[string]string
	Profile   projection.Profile
	Limit     int
}

func (q Query) validate() error {
	if q.GVK == "" {
		return fmt.Errorf("cache: query needs a GVK")
	}
	if q.Profile == "" {
		return fmt.Errorf("cache: query for %s needs an output profile", q.GVK)
	}
	if q.Profile == projection.Full && q.Name == "" {
		return fmt.Errorf("cache: profile %q is only for a single object, not a listing of %s", projection.Full, q.GVK)
	}
	return nil
}

// Snapshot reports how fresh the cache is.
//
// AsOf is the last moment the cache is known to have reflected the cluster: the
// most recent watch event or resync. Lag is the age of that moment at serve
// time, so on a quiet cluster it grows even though the data is perfectly
// current. That over-reports staleness and never under-reports it, which is the
// only safe direction for principle 6 — and the resync period bounds it.
type Snapshot struct {
	AsOf   time.Time
	Lag    time.Duration
	Synced bool
}

// Reader serves projected objects out of memory.
type Reader interface {
	Snapshot() Snapshot
	List(ctx context.Context, q Query) ([]projection.Object, error)
}

// Get fetches exactly one object, turning an empty result into an error so
// callers do not have to.
func Get(ctx context.Context, r Reader, q Query) (projection.Object, error) {
	if q.Name == "" {
		return projection.Object{}, fmt.Errorf("cache: Get needs a Name")
	}
	objs, err := r.List(ctx, q)
	if err != nil {
		return projection.Object{}, err
	}
	if len(objs) == 0 {
		return projection.Object{}, &NotFoundError{GVK: q.GVK, Namespace: q.Namespace, Name: q.Name}
	}
	return objs[0], nil
}

// NotFoundError says an object is absent from the cache. A tool turns this into
// an answer ("no such workload"), never into a stack trace.
type NotFoundError struct {
	GVK, Namespace, Name string
}

func (e *NotFoundError) Error() string {
	if e.Namespace == "" {
		return fmt.Sprintf("no %s named %q in the cache", e.GVK, e.Name)
	}
	return fmt.Sprintf("no %s named %q in namespace %q in the cache", e.GVK, e.Name, e.Namespace)
}

// KindOf extracts the Kind from a "group/version/Kind" or "version/Kind" GVK.
func KindOf(gvk string) string {
	if i := strings.LastIndex(gvk, "/"); i >= 0 {
		return gvk[i+1:]
	}
	return gvk
}

func matchesLabels(obj projection.Object, want map[string]string) bool {
	if len(want) == 0 {
		return true
	}
	got, _ := valueAt(obj.Fields, "metadata.labels")
	labels, ok := got.(map[string]any)
	if !ok {
		return false
	}
	for k, v := range want {
		if s, _ := labels[k].(string); s != v {
			return false
		}
	}
	return true
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
