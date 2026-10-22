package cache

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"

	"github.com/BetoFandino/brief/internal/projection"
)

// Config declares what the informer cache watches. It mirrors the watch block
// of values.yaml.
type Config struct {
	// GVKs to cache, as "group/version/Kind" or "version/Kind". Never a
	// wildcard: caching everything on a cluster with 40,000 objects is how this
	// project dies on its first serious install.
	GVKs []string
	// Namespaces to restrict to; empty means all.
	Namespaces []string
	// LabelSelector narrows every watch further.
	LabelSelector string
	// MaxAnnotationBytes is the ceiling above which an annotation's value is
	// replaced by a size marker.
	MaxAnnotationBytes int
	// ResyncPeriod bounds how stale Snapshot.Lag can get on a quiet cluster.
	ResyncPeriod time.Duration
}

func (c Config) withDefaults() Config {
	if c.MaxAnnotationBytes == 0 {
		c.MaxAnnotationBytes = 4096
	}
	if c.ResyncPeriod == 0 {
		c.ResyncPeriod = 30 * time.Second
	}
	return c
}

// Informer is a Reader backed by a controller-runtime cache kept current by
// watch. It never calls the API server to serve a request.
type Informer struct {
	cfg   Config
	cache ctrlcache.Cache
	gvks  map[string]schema.GroupVersionKind
	// resources maps Kind to the plural API resource name, resolved once at
	// startup from the discovery RESTMapper, because authorization asks about
	// resources and the rbac package has no mapper of its own.
	resources map[string]string

	mu       sync.RWMutex
	lastSeen time.Time
}

// NewInformer builds the cache. It does not start it; call Start.
func NewInformer(restCfg *rest.Config, cfg Config) (*Informer, error) {
	cfg = cfg.withDefaults()
	if len(cfg.GVKs) == 0 {
		return nil, fmt.Errorf("cache: no GVKs declared; brief caches only what values.yaml asks for, never everything")
	}

	in := &Informer{
		cfg:       cfg,
		gvks:      map[string]schema.GroupVersionKind{},
		resources: map[string]string{},
	}

	httpClient, err := rest.HTTPClientFor(restCfg)
	if err != nil {
		return nil, fmt.Errorf("cache: http client: %w", err)
	}
	mapper, err := apiutil.NewDynamicRESTMapper(restCfg, httpClient)
	if err != nil {
		return nil, fmt.Errorf("cache: rest mapper: %w", err)
	}

	scheme := runtime.NewScheme()
	byObject := map[client.Object]ctrlcache.ByObject{}

	selector, err := parseLabelSelector(cfg.LabelSelector)
	if err != nil {
		return nil, err
	}

	for _, raw := range cfg.GVKs {
		gvk, err := parseGVK(raw)
		if err != nil {
			return nil, err
		}
		in.gvks[gvk.Kind] = gvk

		mapping, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		if err != nil {
			return nil, fmt.Errorf("cache: %s is not served by this cluster: %w", raw, err)
		}
		in.resources[gvk.Kind] = mapping.Resource.Resource

		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(gvk)
		byObject[u] = ctrlcache.ByObject{Label: selector}
	}

	opts := ctrlcache.Options{
		Scheme:                      scheme,
		Mapper:                      mapper,
		ByObject:                    byObject,
		SyncPeriod:                  &cfg.ResyncPeriod,
		ReaderFailOnMissingInformer: true,
		// The transform runs before the object is stored, so the saving lands
		// in cache memory and not only in the response. Stripping at
		// serialization time would leave the cache just as large, and cache
		// size is what decides whether this is installable at all.
		DefaultTransform: in.transform,
	}
	if len(cfg.Namespaces) > 0 {
		opts.DefaultNamespaces = map[string]ctrlcache.Config{}
		for _, ns := range cfg.Namespaces {
			opts.DefaultNamespaces[ns] = ctrlcache.Config{}
		}
	}

	c, err := ctrlcache.New(restCfg, opts)
	if err != nil {
		return nil, fmt.Errorf("cache: %w", err)
	}
	in.cache = c
	return in, nil
}

// transform strips structural noise before the object enters the cache, and
// records that the cache has just heard from the API server.
func (in *Informer) transform(obj any) (any, error) {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return obj, nil
	}
	projection.Strip(u.Object, in.cfg.MaxAnnotationBytes)

	in.mu.Lock()
	in.lastSeen = time.Now()
	in.mu.Unlock()
	return u, nil
}

// Start blocks until ctx is cancelled, keeping the cache current.
func (in *Informer) Start(ctx context.Context) error { return in.cache.Start(ctx) }

// WaitForSync blocks until every declared informer has done its initial LIST.
func (in *Informer) WaitForSync(ctx context.Context) bool {
	ok := in.cache.WaitForCacheSync(ctx)
	if ok {
		in.mu.Lock()
		if in.lastSeen.IsZero() {
			in.lastSeen = time.Now()
		}
		in.mu.Unlock()
	}
	return ok
}

// Resources exposes the Kind-to-resource mapping resolved at startup.
func (in *Informer) Resources() map[string]string { return in.resources }

func (in *Informer) Snapshot() Snapshot {
	in.mu.RLock()
	last := in.lastSeen
	in.mu.RUnlock()

	if last.IsZero() {
		return Snapshot{AsOf: time.Now(), Lag: 0, Synced: false}
	}
	return Snapshot{AsOf: last, Lag: time.Since(last), Synced: true}
}

func (in *Informer) List(ctx context.Context, q Query) ([]projection.Object, error) {
	if err := q.validate(); err != nil {
		return nil, err
	}

	gvk, known := in.gvks[KindOf(q.GVK)]
	if !known {
		return nil, fmt.Errorf("cache: %s is not watched; add it to watch.gvks in values.yaml", q.GVK)
	}

	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{
		Group: gvk.Group, Version: gvk.Version, Kind: gvk.Kind + "List",
	})

	var opts []client.ListOption
	if q.Namespace != "" {
		opts = append(opts, client.InNamespace(q.Namespace))
	}
	if len(q.Labels) > 0 {
		opts = append(opts, client.MatchingLabels(q.Labels))
	}
	if err := in.cache.List(ctx, list, opts...); err != nil {
		return nil, fmt.Errorf("cache: listing %s: %w", q.GVK, err)
	}

	out := make([]projection.Object, 0, len(list.Items))
	for i := range list.Items {
		item := list.Items[i]
		if q.Name != "" && item.GetName() != q.Name {
			continue
		}
		// The cache hands back its own stored object; projecting a copy keeps
		// the path-picking from mutating what every other caller will read.
		obj := projection.Project(item.DeepCopy().Object, q.Profile, in.cfg.MaxAnnotationBytes)
		obj.Resource = in.resources[obj.Kind]
		out = append(out, obj)
		if q.Limit > 0 && len(out) >= q.Limit {
			break
		}
	}
	return out, nil
}

func parseGVK(s string) (schema.GroupVersionKind, error) {
	parts := strings.Split(s, "/")
	switch len(parts) {
	case 2: // core group: "v1/Pod"
		return schema.GroupVersionKind{Version: parts[0], Kind: parts[1]}, nil
	case 3: // "apps/v1/Deployment"
		return schema.GroupVersionKind{Group: parts[0], Version: parts[1], Kind: parts[2]}, nil
	default:
		return schema.GroupVersionKind{}, fmt.Errorf("cache: %q is not a GVK; want version/Kind or group/version/Kind", s)
	}
}

func parseLabelSelector(s string) (labels.Selector, error) {
	if s == "" {
		return labels.Everything(), nil
	}
	sel, err := labels.Parse(s)
	if err != nil {
		return nil, fmt.Errorf("cache: label selector %q: %w", s, err)
	}
	return sel, nil
}
