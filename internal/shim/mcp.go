package shim

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Version is reported to the agent on handshake.
const Version = "0.1.0-dev"

// NewMCPServer builds the MCP server the agent talks to over stdio.
//
// The tool list comes from the service, so the surface can change server-side
// without shipping a new binary to every laptop. Argument schemas are declared
// here because MCP needs them for the model to fill arguments at all; the
// service remains the authority on what it accepts.
func NewMCPServer(ctx context.Context, b Backend) (*mcp.Server, error) {
	descriptors, err := b.Tools(ctx)
	if err != nil {
		return nil, err
	}
	if len(descriptors) == 0 {
		return nil, fmt.Errorf("shim: %s advertises no tools", b.describe())
	}

	srv := mcp.NewServer(&mcp.Implementation{
		Name:    "brief",
		Title:   "brief — read-only Kubernetes diagnostics",
		Version: Version,
	}, nil)

	for _, d := range descriptors {
		srv.AddTool(&mcp.Tool{
			Name:        d.Name,
			Description: describeForAgent(d),
			InputSchema: schemaFor(d.Name),
			Annotations: &mcp.ToolAnnotations{
				// Every tool is read-only, in every mode, by design. Saying so
				// in the annotation lets a client skip approval prompts it
				// would otherwise raise for a tool that might mutate.
				ReadOnlyHint: true,
			},
		}, handlerFor(b, d.Name))
	}
	return srv, nil
}

// describeForAgent appends the budget to the description, so the model can see
// what an answer is allowed to cost before it spends the call.
func describeForAgent(d ToolDescriptor) string {
	if d.Budget <= 0 {
		return d.Description
	}
	return fmt.Sprintf("%s (responses are capped at %d tokens and declare truncation)", d.Description, d.Budget)
}

func handlerFor(b Backend, name string) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		raw, err := b.Call(ctx, name, req.Params.Arguments)
		if err != nil {
			// Hand the failure back as tool content rather than a protocol
			// error: the agent can read it, reason about it and try another
			// call, which a transport-level error would deny it.
			return &mcp.CallToolResult{
				IsError: true,
				Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}},
			}, nil
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: string(raw)}},
		}, nil
	}
}

// schemaFor declares each tool's arguments.
//
// These mirror the service's own validation; the service rejects what it does
// not accept regardless. Keeping them here is what lets the model fill
// arguments at all, since MCP has no way to ask mid-call.
func schemaFor(tool string) json.RawMessage {
	switch tool {
	case "explain_workload":
		return json.RawMessage(`{
  "type": "object",
  "properties": {
    "namespace": {"type": "string", "description": "Namespace of the workload."},
    "name": {"type": "string", "description": "Name of the workload."},
    "kind": {"type": "string", "description": "Workload kind. Defaults to Deployment.",
             "enum": ["Deployment", "StatefulSet", "DaemonSet", "ReplicaSet", "Pod"]}
  },
  "required": ["namespace", "name"],
  "additionalProperties": false
}`)
	case "triage_cluster":
		return json.RawMessage(`{
  "type": "object",
  "properties": {
    "namespace": {"type": "string", "description": "Restrict to one namespace. Omit for the whole cluster."},
    "min_severity": {"type": "string", "description": "Lowest severity to report. Defaults to low.",
                     "enum": ["critical", "high", "medium", "low"]}
  },
  "additionalProperties": false
}`)
	case "list_objects":
		return json.RawMessage(`{
  "type": "object",
  "properties": {
    "gvk": {"type": "string", "description": "Group/version/Kind, e.g. v1/Pod or apps/v1/Deployment."},
    "namespace": {"type": "string"},
    "name": {"type": "string", "description": "Restrict to one object by name."},
    "labels": {"type": "object", "description": "Label equality filters.", "additionalProperties": {"type": "string"}},
    "profile": {"type": "string", "description": "How much of each object to return. Listings may not use full.",
                "enum": ["minimal", "diagnostic", "full"]},
    "limit": {"type": "integer", "description": "Maximum rows. Capped server-side."}
  },
  "required": ["gvk"],
  "additionalProperties": false
}`)
	default:
		// A tool the service grew that this shim has no schema for: accept
		// anything and let the service validate, rather than hide the tool.
		return json.RawMessage(`{"type": "object", "additionalProperties": true}`)
	}
}
