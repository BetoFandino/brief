// Package shim bridges two worlds that cannot be joined: the agent, which
// speaks MCP over stdio on a laptop, and the service, which lives in the
// cluster behind the API server.
//
// It holds no diagnostic logic. It translates, authenticates with the user's
// kubeconfig, and multiplexes clusters. If it ever starts deciding anything, it
// has stopped being a shim and become a second server to maintain.
//
// One hard rule follows from the transport: stdout is the protocol channel, so
// nothing here may print to it. Every log goes to stderr.
package shim

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Backend reaches the in-cluster service through the API server's proxy.
type Backend struct {
	// Client is built from the user's kubeconfig, so every request carries
	// their identity — client certificate, OIDC token, or an exec plugin for
	// EKS, GKE or AKS. No new credential system is invented.
	Client *http.Client
	// BaseURL is the proxied service path on the API server:
	// https://<apiserver>/api/v1/namespaces/<ns>/services/<svc>:<port>/proxy
	BaseURL string
	// Token, when set, is forwarded as the bearer credential the service
	// resolves with TokenReview to learn who is calling.
	Token string
	// Context names the cluster, for error messages that say which one.
	Context string
}

// ToolDescriptor is a tool as the service advertises it.
type ToolDescriptor struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Budget      int    `json:"budget_tokens"`
}

// Tools asks the service what it offers. The surface is discovered rather than
// compiled in, so adding a tool server-side does not mean shipping a new shim.
func (b Backend) Tools(ctx context.Context) ([]ToolDescriptor, error) {
	body, err := b.do(ctx, http.MethodGet, "/v1/tools", nil)
	if err != nil {
		return nil, err
	}
	var payload struct {
		Tools []ToolDescriptor `json:"tools"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("shim: tool list from %s is not valid JSON: %w", b.describe(), err)
	}
	return payload.Tools, nil
}

// Call invokes one tool and returns the service's response bytes verbatim.
//
// Verbatim matters: the envelope already carries as_of, the budget accounting
// and the truncation flag. Re-encoding it here would be a second chance to
// break the contract for no gain.
func (b Backend) Call(ctx context.Context, name string, args json.RawMessage) ([]byte, error) {
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	return b.do(ctx, http.MethodPost, "/v1/tools/"+url.PathEscape(name), args)
}

func (b Backend) do(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(b.BaseURL, "/")+path, reader)
	if err != nil {
		return nil, fmt.Errorf("shim: building request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if b.Token != "" {
		req.Header.Set("Authorization", "Bearer "+b.Token)
	}

	client := b.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("shim: reaching brief in %s: %w", b.describe(), err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("shim: reading response from %s: %w", b.describe(), err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &HTTPError{Status: resp.StatusCode, Body: raw, Cluster: b.describe()}
	}
	return raw, nil
}

func (b Backend) describe() string {
	if b.Context == "" {
		return "the cluster"
	}
	return "cluster " + b.Context
}

// HTTPError is a non-200 from the service, surfaced with the cluster named.
//
// Naming the cluster in every error is deliberate: diagnosing production while
// believing it is staging is the most expensive mistake this tool could help
// someone make.
type HTTPError struct {
	Status  int
	Body    []byte
	Cluster string
}

func (e *HTTPError) Error() string {
	var payload struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(e.Body, &payload); err == nil && payload.Error != "" {
		return fmt.Sprintf("%s: %s (HTTP %d)", e.Cluster, payload.Error, e.Status)
	}
	trimmed := strings.TrimSpace(string(e.Body))
	if len(trimmed) > 200 {
		trimmed = trimmed[:200] + "…"
	}
	if trimmed == "" {
		trimmed = http.StatusText(e.Status)
	}
	return fmt.Sprintf("%s: %s (HTTP %d)", e.Cluster, trimmed, e.Status)
}
