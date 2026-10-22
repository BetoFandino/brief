// Command brief-server is the in-cluster service: the informer cache, the
// projection layer, the analyzers, and the HTTP surface the shim reaches
// through the API server's service proxy.
//
// It exposes no Ingress and initiates no traffic of its own beyond the watches
// that keep the cache current and the authorization calls that filter every
// answer. It does not reach the internet and must not be given a route to it.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/BetoFandino/brief/internal/analyzer"
	"github.com/BetoFandino/brief/internal/cache"
	"github.com/BetoFandino/brief/internal/server"
	"github.com/BetoFandino/brief/internal/tools"
)

// defaultGVKs mirrors the watch.gvks of values.yaml. It is a list and never a
// wildcard: caching everything on a cluster with 40,000 objects is how this
// project dies on its first serious install.
var defaultGVKs = []string{
	"v1/Pod",
	"v1/Node",
	"v1/Service",
	"v1/Event",
	"v1/PersistentVolumeClaim",
	"discovery.k8s.io/v1/EndpointSlice",
	"apps/v1/Deployment",
	"apps/v1/ReplicaSet",
	"apps/v1/StatefulSet",
	"apps/v1/DaemonSet",
}

func main() {
	if err := run(); err != nil {
		slog.Error("brief-server exited", "error", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		addr        = flag.String("addr", ":8080", "address to serve on")
		authMode    = flag.String("auth-mode", string(server.ModeSAR), "sar | serviceAccountOnly")
		gvks        = flag.String("gvks", strings.Join(defaultGVKs, ","), "comma-separated GVKs to cache")
		namespaces  = flag.String("namespaces", "", "comma-separated namespaces to restrict to; empty means all")
		labelSel    = flag.String("label-selector", "", "narrow every watch")
		maxAnnBytes = flag.Int("max-annotation-bytes", 4096, "annotation values above this are replaced by a size marker")
		resync      = flag.Duration("resync", 30*time.Second, "informer resync period; bounds reported staleness")
		sarTTL      = flag.Duration("sar-ttl", time.Minute, "how long an authorization answer is cached")
	)
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	restCfg, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("kubeconfig: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return fmt.Errorf("kubernetes client: %w", err)
	}

	informer, err := cache.NewInformer(restCfg, cache.Config{
		GVKs:               split(*gvks),
		Namespaces:         split(*namespaces),
		LabelSelector:      *labelSel,
		MaxAnnotationBytes: *maxAnnBytes,
		ResyncPeriod:       *resync,
	})
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cacheErr := make(chan error, 1)
	go func() { cacheErr <- informer.Start(ctx) }()

	log.Info("waiting for the cache to sync", "gvks", split(*gvks))
	if !informer.WaitForSync(ctx) {
		return errors.New("cache did not sync")
	}
	log.Info("cache synced", "resources", informer.Resources())

	srv, err := server.New(server.Options{
		Mode:   server.Mode(*authMode),
		Client: clientset,
		SARTTL: *sarTTL,
		Log:    log,
		Tools:  tools.Default(),
		Deps: tools.Deps{
			Cache:     informer,
			Analyzers: analyzer.Default(),
		},
	})
	if err != nil {
		return err
	}

	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	httpErr := make(chan error, 1)
	go func() {
		log.Info("serving", "addr", *addr, "auth_mode", *authMode)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			httpErr <- err
		}
	}()

	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case err := <-cacheErr:
		if err != nil {
			return fmt.Errorf("cache stopped: %w", err)
		}
	case err := <-httpErr:
		return fmt.Errorf("http: %w", err)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return httpSrv.Shutdown(shutdownCtx)
}

func split(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}
