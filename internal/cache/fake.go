package cache

import (
	"context"
	"time"

	"github.com/BetoFandino/brief/internal/projection"
)

// Fake is an in-memory Reader holding raw decoded objects.
//
// It is in the package rather than a _test.go file on purpose: every tool's
// tests need it, and the point of the narrow Reader interface is that the
// diagnostic layer can be exercised without a cluster. It projects on demand,
// so tests run through the real projection path instead of around it.
type Fake struct {
	// Objects are raw decoded Kubernetes objects, as a watch would deliver them.
	Objects []map[string]any
	// Resources maps Kind to the plural API resource name, which authorization
	// asks about. A Kind missing here is a test setup error, not a silent pass.
	Resources map[string]string

	AsOf   time.Time
	Lag    time.Duration
	Synced bool

	// Err, when set, is returned by every List, for testing degraded paths.
	Err error
}

func (f *Fake) Snapshot() Snapshot {
	asOf := f.AsOf
	if asOf.IsZero() {
		asOf = time.Date(2026, 10, 21, 14, 7, 12, 0, time.UTC)
	}
	return Snapshot{AsOf: asOf, Lag: f.Lag, Synced: f.Synced || f.AsOf.IsZero()}
}

func (f *Fake) List(_ context.Context, q Query) ([]projection.Object, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	if err := q.validate(); err != nil {
		return nil, err
	}

	wantKind := KindOf(q.GVK)
	var out []projection.Object
	for _, raw := range f.Objects {
		copied := deepCopy(raw)
		obj := projection.Project(copied, q.Profile, 0)
		if obj.Kind != wantKind {
			continue
		}
		if q.Namespace != "" && obj.Namespace != q.Namespace {
			continue
		}
		if q.Name != "" && obj.Name != q.Name {
			continue
		}
		if !matchesLabels(obj, q.Labels) {
			continue
		}
		obj.Resource = f.Resources[obj.Kind]
		out = append(out, obj)
		if q.Limit > 0 && len(out) >= q.Limit {
			break
		}
	}
	return out, nil
}

// deepCopy keeps Project, which strips in place, from mutating the fixture an
// earlier assertion already read.
func deepCopy(v map[string]any) map[string]any {
	out := make(map[string]any, len(v))
	for k, val := range v {
		out[k] = deepCopyValue(val)
	}
	return out
}

func deepCopyValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		return deepCopy(t)
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = deepCopyValue(item)
		}
		return out
	default:
		return v
	}
}
