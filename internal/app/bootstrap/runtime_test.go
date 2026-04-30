package bootstrap

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tomlconfig "github.com/Luew2/FreeCode/internal/adapters/config/toml"
	"github.com/Luew2/FreeCode/internal/app/commands"
	"github.com/Luew2/FreeCode/internal/core/config"
	"github.com/Luew2/FreeCode/internal/core/model"
	"github.com/Luew2/FreeCode/internal/core/permission"
)

func TestBuildCreatesRuntimeAndWorkbench(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, ".freecode", "config.toml")
	settings := config.DefaultSettings()
	providerID := model.ProviderID("local")
	ref := model.NewRef(providerID, "coder")
	settings.ActiveModel = ref
	settings.Providers[providerID] = model.Provider{
		ID:           providerID,
		Name:         "local",
		Protocol:     commands.ProviderProtocolOpenAIChat,
		BaseURL:      "http://example.test/v1",
		DefaultModel: "coder",
		Enabled:      true,
	}
	settings.Models[ref] = model.NewModel(providerID, "coder")
	if err := tomlconfig.New(configPath).Save(context.Background(), settings); err != nil {
		t.Fatalf("Save config: %v", err)
	}

	runtime, err := Build(context.Background(), Options{
		ConfigPath:      configPath,
		WorkspaceRoot:   root,
		ApprovalMode:    permission.ModeAsk,
		StartNewSession: true,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer runtime.Close()
	if runtime.WorkspaceRoot == "" || runtime.Model != ref || runtime.Tools == nil || runtime.EventLog == nil {
		t.Fatalf("runtime = %#v", runtime)
	}
	service, err := runtime.Workbench(context.Background())
	if err != nil {
		t.Fatalf("Workbench: %v", err)
	}
	if service.WorkspaceRoot == "" || service.Model != ref || service.Submit == nil {
		t.Fatalf("service = %#v", service)
	}
}

func TestBuildIncludesMCPToolsWhenEnabled(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, ".freecode", "config.toml")
	settings := config.DefaultSettings()
	providerID := model.ProviderID("local")
	ref := model.NewRef(providerID, "coder")
	settings.ActiveModel = ref
	settings.Providers[providerID] = model.Provider{
		ID:           providerID,
		Name:         "local",
		Protocol:     commands.ProviderProtocolOpenAIChat,
		BaseURL:      "http://example.test/v1",
		DefaultModel: "coder",
		Enabled:      true,
	}
	settings.Models[ref] = model.NewModel(providerID, "coder")
	settings.MCP.Enabled = true
	settings.MCP.Servers = map[string]config.MCPServer{
		"fake": {
			Enabled:      true,
			Transport:    "stdio",
			Command:      os.Args[0],
			Args:         []string{"-test.run=TestBootstrapFakeMCPServer", "--"},
			Env:          []string{"FREECODE_BOOTSTRAP_FAKE_MCP"},
			ToolsPrefix:  "fake",
			Capabilities: []string{"read_workspace"},
		},
	}
	t.Setenv("FREECODE_BOOTSTRAP_FAKE_MCP", "1")
	if err := tomlconfig.New(configPath).Save(context.Background(), settings); err != nil {
		t.Fatalf("Save config: %v", err)
	}

	runtime, err := Build(context.Background(), Options{
		ConfigPath:    configPath,
		WorkspaceRoot: root,
		ApprovalMode:  permission.ModeAsk,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer runtime.Close()
	found := false
	for _, tool := range runtime.Tools.Tools() {
		if tool.Name == "mcp_fake_echo" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("runtime tools = %#v, want mcp_fake_echo", runtime.Tools.Tools())
	}
	service, err := runtime.Workbench(context.Background())
	if err != nil {
		t.Fatalf("Workbench: %v", err)
	}
	state, err := service.MCPStatus(context.Background())
	if err != nil {
		t.Fatalf("MCPStatus: %v", err)
	}
	if !strings.Contains(state.Detail.Body, "mcp_fake_echo") && !strings.Contains(state.Detail.Body, "tools=1") {
		t.Fatalf("MCPStatus body = %s", state.Detail.Body)
	}
}

func TestAskDependenciesPreserveReadOnlyTools(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, ".freecode", "config.toml")
	settings := config.DefaultSettings()
	providerID := model.ProviderID("local")
	ref := model.NewRef(providerID, "coder")
	settings.ActiveModel = ref
	settings.Providers[providerID] = model.Provider{
		ID:           providerID,
		Name:         "local",
		Protocol:     commands.ProviderProtocolOpenAIChat,
		BaseURL:      "http://example.test/v1",
		DefaultModel: "coder",
		Enabled:      true,
	}
	settings.Models[ref] = model.NewModel(providerID, "coder")
	if err := tomlconfig.New(configPath).Save(context.Background(), settings); err != nil {
		t.Fatalf("Save config: %v", err)
	}
	runtime, err := Build(context.Background(), Options{
		ConfigPath:    configPath,
		WorkspaceRoot: root,
		ApprovalMode:  permission.ModeReadOnly,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	for _, tool := range runtime.Tools.Tools() {
		if tool.Name == "apply_patch" {
			t.Fatalf("read-only runtime exposed apply_patch")
		}
	}
	deps, err := runtime.AskDependencies(context.Background())
	if err != nil {
		t.Fatalf("AskDependencies: %v", err)
	}
	for _, tool := range deps.Tools.Tools() {
		if tool.Name == "apply_patch" {
			t.Fatalf("read-only ask exposed apply_patch")
		}
	}
}

func TestBootstrapFakeMCPServer(t *testing.T) {
	if os.Getenv("FREECODE_BOOTSTRAP_FAKE_MCP") != "1" {
		return
	}
	decoder := json.NewDecoder(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	for {
		var request map[string]any
		if err := decoder.Decode(&request); err != nil {
			return
		}
		method, _ := request["method"].(string)
		id, _ := request["id"].(float64)
		if id == 0 && method == "notifications/initialized" {
			continue
		}
		switch method {
		case "initialize":
			_ = encoder.Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      int(id),
				"result": map[string]any{
					"protocolVersion": "2025-11-25",
					"serverInfo":      map[string]any{"name": "bootstrap-fake", "version": "test"},
				},
			})
		case "tools/list":
			_ = encoder.Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      int(id),
				"result": map[string]any{
					"tools": []map[string]any{{
						"name":        "echo",
						"description": "Echo",
						"inputSchema": map[string]any{"type": "object"},
					}},
				},
			})
		default:
			_ = encoder.Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      int(id),
				"error":   map[string]any{"code": -32601, "message": "unknown method"},
			})
		}
	}
}
