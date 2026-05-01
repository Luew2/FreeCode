package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/Luew2/FreeCode/internal/core/config"
	"github.com/Luew2/FreeCode/internal/core/model"
	"github.com/Luew2/FreeCode/internal/core/permission"
	"github.com/Luew2/FreeCode/internal/ports"
)

func TestManagerStartsServerListsToolsAndCallsTool(t *testing.T) {
	manager := testManager(t, config.MCPServer{
		Enabled:      true,
		Transport:    "stdio",
		Command:      os.Args[0],
		Args:         []string{"-test.run=TestFakeMCPServer", "--"},
		Capabilities: []string{"read_workspace"},
	})
	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer manager.Close()

	provider := manager.Provider(permission.ModeReadOnly, nil)
	specs := provider.Tools()
	if len(specs) != 2 {
		t.Fatalf("Tools = %#v, want 2 paginated tools", specs)
	}
	result, err := provider.RunTool(context.Background(), model.ToolCall{
		ID:        "call_1",
		Name:      "mcp_fake_search",
		Arguments: []byte(`{"query":"freecode"}`),
	})
	if err != nil {
		t.Fatalf("RunTool: %v", err)
	}
	if !strings.Contains(result.Content, "search: freecode") || result.Metadata["mcp_server"] != "fake" {
		t.Fatalf("result = %#v", result)
	}
}

func TestProviderFiltersReadOnlyAndRequestsPermission(t *testing.T) {
	manager := testManager(t, config.MCPServer{
		Enabled:     true,
		Transport:   "stdio",
		Command:     os.Args[0],
		Args:        []string{"-test.run=TestFakeMCPServer", "--"},
		ToolsPrefix: "fake",
		ToolCapabilities: map[string][]string{
			"search":     []string{"read_workspace"},
			"write_file": []string{"write_workspace"},
		},
	})
	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer manager.Close()

	readOnly := manager.Provider(permission.ModeReadOnly, nil)
	for _, spec := range readOnly.Tools() {
		if spec.Name == "mcp_fake_write_file" {
			t.Fatalf("write tool exposed in read-only mode: %#v", readOnly.Tools())
		}
	}

	gate := &recordingGate{decision: permission.DecisionAllow}
	ask := manager.Provider(permission.ModeAsk, gate)
	if _, err := ask.RunTool(context.Background(), model.ToolCall{Name: "mcp_fake_write_file", Arguments: []byte(`{}`)}); err != nil {
		t.Fatalf("RunTool: %v", err)
	}
	if len(gate.requests) != 1 || gate.requests[0].Action != permission.ActionWrite {
		t.Fatalf("permission requests = %#v, want write request", gate.requests)
	}

	askGate := &recordingGate{decision: permission.DecisionAsk}
	needsApproval := manager.Provider(permission.ModeAsk, askGate)
	_, err := needsApproval.RunTool(context.Background(), model.ToolCall{Name: "mcp_fake_write_file", Arguments: []byte(`{}`)})
	if !errors.Is(err, permission.ErrApprovalRequired) {
		t.Fatalf("RunTool ask decision = %v, want approval required", err)
	}
	request, ok := permission.ApprovalRequest(err)
	if !ok || request.Action != permission.ActionWrite || request.Subject != "mcp_fake_write_file" {
		t.Fatalf("approval request = %#v/%v", request, ok)
	}
}

func TestManagerReportsFailedServerWithoutFailingStart(t *testing.T) {
	manager := NewManager(Options{
		WorkspaceRoot: t.TempDir(),
		Settings: config.MCPSettings{
			Enabled: true,
			Servers: map[string]config.MCPServer{
				"missing": {Enabled: true, Transport: "stdio", Command: "definitely-not-a-freecode-mcp-server"},
			},
		},
	})
	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	status := manager.Status(permission.ModeAsk)
	if len(status.Servers) != 1 || status.Servers[0].State != "failed" || status.Servers[0].LastError == "" {
		t.Fatalf("status = %#v, want failed server with error", status)
	}
}

func TestPublicToolNameSanitizesAndShortens(t *testing.T) {
	name, err := publicToolName("exa search", "", strings.Repeat("web search ", 20))
	if err != nil {
		t.Fatalf("publicToolName: %v", err)
	}
	if !strings.HasPrefix(name, "mcp_exa_search_") || len(name) > maxToolNameLength {
		t.Fatalf("name = %q len=%d", name, len(name))
	}
}

func TestFakeMCPServer(t *testing.T) {
	if os.Getenv("FREECODE_MCP_FAKE_SERVER") != "1" {
		return
	}
	decoder := json.NewDecoder(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	for {
		var request requestEnvelope
		if err := decoder.Decode(&request); err != nil {
			return
		}
		switch request.Method {
		case methodInitialize:
			_ = encoder.Encode(responseEnvelope{JSONRPC: "2.0", ID: request.ID, Result: mustJSON(initializeResult{
				ProtocolVersion: protocolVersion,
				ServerInfo:      implementation{Name: "fake", Version: "test"},
			})})
		case methodInitialized:
		case methodToolsList:
			var params listToolsParams
			_ = json.Unmarshal(mustJSON(request.Params), &params)
			if params.Cursor == "" {
				_ = encoder.Encode(responseEnvelope{JSONRPC: "2.0", ID: request.ID, Result: mustJSON(listToolsResult{
					Tools:      []tool{{Name: "search", Description: "Search", InputSchema: map[string]any{"type": "object"}}},
					NextCursor: "next",
				})})
			} else {
				_ = encoder.Encode(responseEnvelope{JSONRPC: "2.0", ID: request.ID, Result: mustJSON(listToolsResult{
					Tools: []tool{{Name: "write_file", Description: "Write", InputSchema: map[string]any{"type": "object"}}},
				})})
			}
		case methodToolsCall:
			var params callToolParams
			_ = json.Unmarshal(mustJSON(request.Params), &params)
			text := "called " + params.Name
			if query, _ := params.Arguments["query"].(string); query != "" {
				text = "search: " + query
			}
			_ = encoder.Encode(responseEnvelope{JSONRPC: "2.0", ID: request.ID, Result: mustJSON(callToolResult{
				Content:           []contentBlock{{Type: "text", Text: text}},
				StructuredContent: map[string]any{"ok": true},
			})})
		default:
			_ = encoder.Encode(responseEnvelope{JSONRPC: "2.0", ID: request.ID, Error: &rpcError{Code: -32601, Message: "method not found"}})
		}
	}
}

func testManager(t *testing.T, server config.MCPServer) *Manager {
	t.Helper()
	if len(server.Args) == 0 {
		server.Args = []string{"-test.run=TestFakeMCPServer", "--"}
	}
	server.Env = append(server.Env, "FREECODE_MCP_FAKE_SERVER")
	t.Setenv("FREECODE_MCP_FAKE_SERVER", "1")
	manager := NewManager(Options{
		WorkspaceRoot: t.TempDir(),
		Version:       "test",
		Settings: config.MCPSettings{
			Enabled: true,
			Servers: map[string]config.MCPServer{"fake": server},
		},
	})
	return manager
}

func mustJSON(value any) json.RawMessage {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return data
}

type recordingGate struct {
	decision permission.Decision
	requests []permission.Request
}

func (g *recordingGate) Decide(ctx context.Context, request permission.Request) (permission.Decision, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	g.requests = append(g.requests, request)
	return g.decision, nil
}

func TestCommandForFakeServerIsRunnable(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=TestFakeMCPServer", "--")
	cmd.Env = append(os.Environ(), "FREECODE_MCP_FAKE_SERVER=1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
}

var _ ports.PermissionGate = (*recordingGate)(nil)

func Example_publicToolName() {
	name, _ := publicToolName("exa", "", "web search")
	fmt.Println(name)
	// Output: mcp_exa_web_search
}
