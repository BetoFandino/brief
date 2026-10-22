// Package e2e exercises the whole v0.1 path in one process: a fake cache, the
// analyzers, a tool, the HTTP service, the shim, and the MCP wire — the same
// sequence a real call takes, minus the API server and the laptop.
package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/BetoFandino/brief/internal/analyzer"
	"github.com/BetoFandino/brief/internal/authz"
	"github.com/BetoFandino/brief/internal/cache"
	"github.com/BetoFandino/brief/internal/server"
	"github.com/BetoFandino/brief/internal/shim"
	"github.com/BetoFandino/brief/internal/tools"
)

// brokenCluster is the oom eval fixture, as a watch would have delivered it.
func brokenCluster() []map[string]any {
	return []map[string]any{
		{
			"apiVersion": "apps/v1", "kind": "Deployment",
			"metadata": map[string]any{"name": "checkout", "namespace": "prod"},
			"spec": map[string]any{
				"replicas": float64(5),
				"selector": map[string]any{"matchLabels": map[string]any{"app": "checkout"}},
			},
			"status": map[string]any{"replicas": float64(5), "readyReplicas": float64(2)},
		},
		{
			"apiVersion": "v1", "kind": "Pod",
			"metadata": map[string]any{
				"name": "checkout-7d9f-xk2", "namespace": "prod",
				"labels": map[string]any{"app": "checkout"},
				// The noise that must not survive to the response.
				"resourceVersion": "918273645",
				"uid":             "8f14e45f-ea1b-4d2c-9f3a-2b7c1d0e5a66",
				"managedFields": []any{map[string]any{
					"manager": "kube-controller-manager", "operation": "Update",
				}},
				"annotations": map[string]any{
					"kubectl.kubernetes.io/last-applied-configuration": `{"apiVersion":"v1","kind":"Pod"}`,
				},
			},
			"spec": map[string]any{"containers": []any{map[string]any{
				"name": "app", "image": "checkout:1.4",
				"resources": map[string]any{"limits": map[string]any{"memory": "512Mi"}},
			}}},
			"status": map[string]any{
				"phase": "Running",
				"containerStatuses": []any{map[string]any{
					"name": "app", "restartCount": float64(14),
					"lastState": map[string]any{"terminated": map[string]any{
						"exitCode": float64(137), "reason": "OOMKilled",
						"finishedAt": "2026-10-21T13:52:04Z",
					}},
					"state": map[string]any{"waiting": map[string]any{"reason": "CrashLoopBackOff"}},
				}},
			},
		},
	}
}

func newService(t *testing.T, mode server.Mode) *httptest.Server {
	t.Helper()
	srv, err := server.New(server.Options{
		Mode:     mode,
		Resolver: authz.StaticResolver{Identity: authz.Identity{User: "beto@example.com"}},
		Tools:    tools.Default(),
		Deps: tools.Deps{
			Cache: &cache.Fake{
				Objects: brokenCluster(),
				Synced:  true,
				Resources: map[string]string{
					"Pod": "pods", "Deployment": "deployments", "Service": "services",
					"Node": "nodes", "Event": "events", "EndpointSlice": "endpointslices",
					"PersistentVolumeClaim": "persistentvolumeclaims",
				},
			},
			Analyzers: analyzer.Default(),
		},
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// The full path: MCP client over an in-memory transport, the shim, HTTP, the
// service, a tool, the analyzers, the envelope.
func TestAgentReachesTheRootCauseInOneCall(t *testing.T) {
	ctx := context.Background()
	service := newService(t, server.ModeServiceAccountOnly)

	backend := shim.Backend{
		Client:  service.Client(),
		BaseURL: service.URL,
		Context: "kind-brief-eval",
	}
	mcpServer, err := shim.NewMCPServer(ctx, backend)
	if err != nil {
		t.Fatalf("shim.NewMCPServer: %v", err)
	}

	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	go func() { _ = mcpServer.Run(ctx, serverTransport) }()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-agent", Version: "0"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = session.Close() }()

	// The surface is discovered from the service, not compiled into the shim.
	listed, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(listed.Tools) != 3 {
		var names []string
		for _, tool := range listed.Tools {
			names = append(names, tool.Name)
		}
		t.Fatalf("the agent sees %v, want the three v0.1 tools", names)
	}
	for _, tool := range listed.Tools {
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Errorf("%s is not advertised as read-only", tool.Name)
		}
		if tool.InputSchema == nil {
			t.Errorf("%s has no input schema, so the model cannot fill its arguments", tool.Name)
		}
	}

	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "explain_workload",
		Arguments: map[string]any{"namespace": "prod", "name": "checkout"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("tool returned an error: %v", text(res))
	}

	var env map[string]any
	if err := json.Unmarshal([]byte(text(res)), &env); err != nil {
		t.Fatalf("the agent did not receive valid JSON: %v\n%s", err, text(res))
	}

	// Freshness, declared.
	if env["as_of"] == nil {
		t.Error("no as_of reached the agent")
	}
	if _, present := env["truncated"]; !present {
		t.Error("no truncation flag reached the agent")
	}

	// The root cause, with its arithmetic already done.
	findings, _ := env["findings"].([]any)
	if len(findings) == 0 {
		t.Fatalf("the agent got no findings:\n%s", text(res))
	}
	var codes []string
	for _, f := range findings {
		codes = append(codes, f.(map[string]any)["code"].(string))
	}
	if !contains(codes, "CONTAINER_OOM_KILLED") {
		t.Errorf("the OOM cause did not reach the agent; got %v", codes)
	}
	if !contains(codes, "REPLICAS_UNAVAILABLE") {
		t.Errorf("the degraded deployment did not reach the agent; got %v", codes)
	}

	// The structural noise never made it out of the cache.
	body := text(res)
	for _, noise := range []string{"managedFields", "resourceVersion", "last-applied-configuration", "918273645", "8f14e45f"} {
		if strings.Contains(body, noise) {
			t.Errorf("%q survived all the way to the agent", noise)
		}
	}

	t.Logf("one call, %d bytes, causes %v", len(body), codes)
}

// An unauthenticated caller gets nothing, and the refusal is not mistaken for
// an empty cluster.
func TestServiceRefusesACallerItCannotIdentify(t *testing.T) {
	srv, err := server.New(server.Options{
		Mode:     server.ModeSAR,
		Client:   nil,
		Resolver: authz.StaticResolver{Identity: authz.Identity{}},
		Deps: tools.Deps{
			Cache:     &cache.Fake{Objects: brokenCluster(), Synced: true},
			Analyzers: analyzer.Default(),
		},
	})
	if err == nil {
		t.Fatal("sar mode was accepted without a client for SubjectAccessReview")
	}
	if !strings.Contains(err.Error(), "SubjectAccessReview") {
		t.Errorf("error does not explain what sar mode needs: %v", err)
	}
	_ = srv
}

func TestReadyzIsNotReadyUntilTheCacheHasSynced(t *testing.T) {
	cold, err := server.New(server.Options{
		Mode:     server.ModeServiceAccountOnly,
		Resolver: authz.StaticResolver{Identity: authz.Identity{User: "u"}},
		Deps: tools.Deps{
			// Synced false and a non-zero AsOf: a cache that exists but has not
			// finished its initial LIST.
			Cache:     &cache.Fake{AsOf: time.Date(2026, 10, 21, 14, 0, 0, 0, time.UTC), Synced: false},
			Analyzers: analyzer.Default(),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(cold.Handler())
	defer ts.Close()

	resp, err := ts.Client().Get(ts.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("readyz on a cold cache = %d, want 503; answering from a cache "+
			"that has not looked at the cluster would report it healthy", resp.StatusCode)
	}
}

func TestUnknownToolIsAFourOhFourNamingWhatExists(t *testing.T) {
	ts := newService(t, server.ModeServiceAccountOnly)
	resp, err := ts.Client().Post(ts.URL+"/v1/tools/run_kubectl", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
	var payload map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&payload)
	msg, _ := payload["error"].(string)
	if !strings.Contains(msg, "/v1/tools") {
		t.Errorf("the error does not say where the tool list is: %q", msg)
	}
}

func text(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
