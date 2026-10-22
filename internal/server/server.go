// Package server is the in-cluster HTTP surface.
//
// It exposes no Ingress and holds no transport security of its own: it is
// reached only through the API server's service proxy, which has already
// authenticated the caller and checked their permission on services/proxy. That
// is the whole reason the project invents no credential system.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"k8s.io/client-go/kubernetes"

	"github.com/BetoFandino/brief/internal/authz"
	"github.com/BetoFandino/brief/internal/cache"
	"github.com/BetoFandino/brief/internal/rbac"
	"github.com/BetoFandino/brief/internal/tools"
)

// Mode decides whose permissions a response is filtered by.
type Mode string

const (
	// ModeSAR filters by the caller's own RBAC, via SubjectAccessReview. The
	// default, and the only mode fit for a shared cluster.
	ModeSAR Mode = "sar"
	// ModeServiceAccountOnly answers with the service's own permissions.
	// Correct for a single-operator cluster and privilege escalation on any
	// other, which is why it has to be asked for by name.
	ModeServiceAccountOnly Mode = "serviceAccountOnly"
)

// Options configures the server.
type Options struct {
	Mode      Mode
	Deps      tools.Deps
	Tools     *tools.Registry
	Client    kubernetes.Interface
	Resolver  authz.Resolver
	SARTTL    time.Duration
	Log       *slog.Logger
	ReadyFunc func() bool
}

// Server answers tool calls out of the cache.
type Server struct {
	opts Options
	log  *slog.Logger
}

func New(opts Options) (*Server, error) {
	if opts.Tools == nil {
		opts.Tools = tools.Default()
	}
	if opts.Deps.Cache == nil {
		return nil, fmt.Errorf("server: no cache")
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	switch opts.Mode {
	case ModeSAR:
		if opts.Client == nil {
			return nil, fmt.Errorf("server: mode %q needs a Kubernetes client for SubjectAccessReview", ModeSAR)
		}
		if opts.Resolver == nil {
			opts.Resolver = authz.TokenReviewResolver{Client: opts.Client}
		}
	case ModeServiceAccountOnly:
		if opts.Resolver == nil {
			opts.Resolver = authz.StaticResolver{Identity: authz.Identity{User: "brief:serviceAccountOnly"}}
		}
	default:
		return nil, fmt.Errorf("server: unknown auth mode %q; want %q or %q", opts.Mode, ModeSAR, ModeServiceAccountOnly)
	}
	return &Server{opts: opts, log: opts.Log}, nil
}

// Handler builds the routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /readyz", s.readyz)
	mux.HandleFunc("GET /v1/tools", s.listTools)
	mux.HandleFunc("POST /v1/tools/{name}", s.callTool)
	return mux
}

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// readyz reports not-ready until the informers have done their initial LIST.
// Serving from a cold cache would answer "nothing is broken" about a cluster
// the service has not looked at yet, which is worse than not answering.
func (s *Server) readyz(w http.ResponseWriter, _ *http.Request) {
	ready := s.opts.Deps.Cache.Snapshot().Synced
	if s.opts.ReadyFunc != nil {
		ready = s.opts.ReadyFunc()
	}
	if !ready {
		http.Error(w, "cache has not synced", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// toolDescriptor is what the shim turns into an MCP tool definition.
type toolDescriptor struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Budget      int    `json:"budget_tokens"`
}

func (s *Server) listTools(w http.ResponseWriter, _ *http.Request) {
	out := make([]toolDescriptor, 0, len(s.opts.Tools.All()))
	for _, t := range s.opts.Tools.All() {
		out = append(out, toolDescriptor{Name: t.Name(), Description: t.Description(), Budget: t.Budget()})
	}
	writeJSON(w, http.StatusOK, map[string]any{"tools": out})
}

func (s *Server) callTool(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	name := r.PathValue("name")

	tool, known := s.opts.Tools.Get(name)
	if !known {
		writeError(w, http.StatusNotFound, fmt.Sprintf("no tool named %q; GET /v1/tools lists them", name))
		return
	}

	var args map[string]any
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&args); err != nil && !errors.Is(err, context.Canceled) {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("arguments are not valid JSON: %v", err))
			return
		}
	}

	identity, checker, err := s.callerOf(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, err.Error())
		return
	}

	env, err := tool.Call(r.Context(), s.opts.Deps, tools.Request{Args: args, Caller: checker})
	if err != nil {
		var notFound *cache.NotFoundError
		status := http.StatusBadRequest
		if errors.As(err, &notFound) {
			status = http.StatusNotFound
		}
		s.audit(identity, name, args, 0, time.Since(started), err)
		writeError(w, status, err.Error())
		return
	}

	raw, err := env.Marshal()
	if err != nil {
		s.audit(identity, name, args, 0, time.Since(started), err)
		writeError(w, http.StatusInsufficientStorage, err.Error())
		return
	}

	s.audit(identity, name, args, len(raw), time.Since(started), nil)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}

// callerOf resolves who is asking and builds the checker that will filter their
// answer.
func (s *Server) callerOf(r *http.Request) (authz.Identity, rbac.Checker, error) {
	if s.opts.Mode == ModeServiceAccountOnly {
		id, _ := s.opts.Resolver.Resolve(r.Context(), "")
		return id, rbac.ServiceAccountOnly{}, nil
	}

	token := bearerToken(r)
	id, err := s.opts.Resolver.Resolve(r.Context(), token)
	if err != nil {
		return authz.Identity{}, nil, err
	}
	if id.User == "" {
		return authz.Identity{}, nil, fmt.Errorf("server: could not establish the caller's identity")
	}
	return id, authz.NewSARChecker(s.opts.Client, id, s.opts.SARTTL), nil
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if after, found := strings.CutPrefix(h, "Bearer "); found {
		return strings.TrimSpace(after)
	}
	return ""
}

// audit records every tool call: who, what, with which arguments, how much came
// back. The operator has to be able to answer "what has the AI been reading?"
// without instrumenting anything else.
func (s *Server) audit(id authz.Identity, tool string, args map[string]any, bytes int, took time.Duration, err error) {
	attrs := []any{
		"event", "tool_call",
		"caller", id.String(),
		"tool", tool,
		"args", args,
		"bytes", bytes,
		"duration_ms", took.Milliseconds(),
	}
	if err != nil {
		s.log.Error("tool call failed", append(attrs, "error", err.Error())...)
		return
	}
	s.log.Info("tool call", attrs...)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}
