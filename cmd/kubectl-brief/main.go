// Command kubectl-brief is the local shim, distributed as a krew plugin.
//
// The agent starts it as a child process and they speak MCP over stdio. It
// authenticates to the API server with the user's own kubeconfig and reaches
// the in-cluster service through the service proxy, so brief never needs an
// Ingress, a certificate or a credential system of its own.
//
// Because stdio is the protocol channel, this binary must never write to
// stdout. Every diagnostic goes to stderr.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/BetoFandino/brief/internal/shim"
)

func main() {
	// stderr, always: stdout belongs to the protocol.
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	if err := run(log); err != nil {
		log.Error("kubectl-brief exited", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	fs := flag.NewFlagSet("kubectl-brief", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var (
		kubeContext = fs.String("context", "", "kubeconfig context to use; defaults to the current one")
		namespace   = fs.String("service-namespace", "brief", "namespace the brief Service lives in")
		service     = fs.String("service", "brief", "name of the brief Service")
		port        = fs.String("service-port", "8080", "port on the brief Service")
		timeout     = fs.Duration("timeout", 30*time.Second, "per-call timeout")
	)

	// `kubectl brief serve` arrives as a positional argument; accept it and
	// anything else krew-shaped without treating it as an error.
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "serve" {
		args = args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	loader := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(),
		&clientcmd.ConfigOverrides{CurrentContext: *kubeContext},
	)
	restCfg, err := loader.ClientConfig()
	if err != nil {
		return fmt.Errorf("kubeconfig: %w", err)
	}

	resolved := *kubeContext
	if resolved == "" {
		if raw, err := loader.RawConfig(); err == nil {
			resolved = raw.CurrentContext
		}
	}

	httpClient, err := rest.HTTPClientFor(restCfg)
	if err != nil {
		return fmt.Errorf("http client from kubeconfig: %w", err)
	}
	httpClient.Timeout = *timeout

	backend := shim.Backend{
		Client: httpClient,
		BaseURL: fmt.Sprintf("%s/api/v1/namespaces/%s/services/%s:%s/proxy",
			restCfg.Host, *namespace, *service, *port),
		Token:   restCfg.BearerToken,
		Context: resolved,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Discovering the surface here means a failure to reach the cluster is
	// reported now, with the cluster named, instead of surfacing as a confusing
	// tool error on the agent's first call.
	srv, err := shim.NewMCPServer(ctx, backend)
	if err != nil {
		return err
	}

	log.Info("serving MCP over stdio", "cluster", resolved, "service", fmt.Sprintf("%s/%s:%s", *namespace, *service, *port))
	if err := srv.Run(ctx, &mcp.StdioTransport{}); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}
