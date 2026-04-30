package tomlconfig

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Luew2/FreeCode/internal/core/config"
	"github.com/Luew2/FreeCode/internal/core/model"
)

func TestStoreRoundTripsProviderConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".freecode", "config.toml")
	store := New(path)

	settings := config.DefaultSettings()
	provider := model.Provider{
		ID:           "lilac",
		Name:         "lilac",
		Protocol:     "openai-chat",
		BaseURL:      "https://api.example.test/v1",
		Secret:       model.SecretRef{Name: "LILAC_API_KEY", Source: "env"},
		DefaultModel: "qwen3-coder",
		Enabled:      true,
		Metadata:     map[string]string{"probe_endpoint": "https://api.example.test/v1/chat/completions"},
	}
	ref := model.NewRef("lilac", "qwen3-coder")
	configuredModel := model.NewModel("lilac", "qwen3-coder")
	configuredModel.Capabilities.Streaming = true
	configuredModel.Capabilities.Tools = true
	configuredModel.Limits.ContextWindow = 262144
	configuredModel.Limits.MaxOutputTokens = 32768
	settings.Providers[provider.ID] = provider
	settings.Models[ref] = configuredModel
	settings.ActiveModel = ref

	if err := store.Save(context.Background(), settings); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}

	loaded, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	gotProvider := loaded.Providers["lilac"]
	if gotProvider.Protocol != "openai-chat" {
		t.Fatalf("Protocol = %q, want openai-chat", gotProvider.Protocol)
	}
	if gotProvider.Secret.Name != "LILAC_API_KEY" {
		t.Fatalf("Secret.Name = %q, want LILAC_API_KEY", gotProvider.Secret.Name)
	}
	if gotProvider.Secret.Source != "env" {
		t.Fatalf("Secret.Source = %q, want env", gotProvider.Secret.Source)
	}
	if gotProvider.DefaultModel != "qwen3-coder" {
		t.Fatalf("DefaultModel = %q, want qwen3-coder", gotProvider.DefaultModel)
	}

	gotModel, ok := loaded.Models[ref]
	if !ok {
		t.Fatalf("loaded model missing ref %q", ref.String())
	}
	if gotModel.Limits.ContextWindow != 262144 {
		t.Fatalf("ContextWindow = %d, want 262144", gotModel.Limits.ContextWindow)
	}
	if !gotModel.Capabilities.Tools || !gotModel.Capabilities.Streaming {
		t.Fatalf("Capabilities = %#v, want tools and streaming", gotModel.Capabilities)
	}
	if loaded.ActiveModel != ref {
		t.Fatalf("ActiveModel = %q, want %q", loaded.ActiveModel.String(), ref.String())
	}
}

func TestStoreDoesNotWriteAPIKeyValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".freecode", "config.toml")
	store := New(path)

	settings := config.DefaultSettings()
	settings.Providers["local"] = model.Provider{
		ID:       "local",
		Name:     "local",
		Protocol: "openai-chat",
		BaseURL:  "https://api.example.test/v1",
		Secret:   model.SecretRef{Name: "LOCAL_API_KEY", Source: "env"},
		Enabled:  true,
	}

	if err := store.Save(context.Background(), settings); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile returned error: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "LOCAL_API_KEY") {
		t.Fatalf("config content does not contain env var name: %s", content)
	}
	if strings.Contains(content, "sk-test-secret") {
		t.Fatalf("config content contains API key value: %s", content)
	}
	if strings.Contains(content, "[[agents]]") {
		t.Fatalf("config content contains default agent definitions: %s", content)
	}
	if strings.Contains(content, "[permissions]") {
		t.Fatalf("config content contains default permissions: %s", content)
	}
	for _, unexpected := range []string{"sessions_dir", "editor_command", "editor_double_esc", "secret_source", "enabled = true"} {
		if strings.Contains(content, unexpected) {
			t.Fatalf("config content contains default noise %q: %s", unexpected, content)
		}
	}
}

func TestStoreRoundTripsEditorSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".freecode", "config.toml")
	store := New(path)

	settings := config.DefaultSettings()
	settings.EditorCommand = "nvim --clean"
	settings.EditorDoubleEsc = true

	if err := store.Save(context.Background(), settings); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	loaded, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if loaded.EditorCommand != "nvim --clean" || !loaded.EditorDoubleEsc {
		t.Fatalf("editor settings = %q/%v, want nvim --clean/true", loaded.EditorCommand, loaded.EditorDoubleEsc)
	}
}

func TestStoreRoundTripsMCPSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".freecode", "config.toml")
	store := New(path)

	settings := config.DefaultSettings()
	settings.MCP.Enabled = true
	settings.MCP.Servers = map[string]config.MCPServer{
		"exa": {
			Enabled:          true,
			Transport:        "stdio",
			Command:          "npx",
			Args:             []string{"-y", "exa-mcp-server"},
			Env:              []string{"EXA_API_KEY"},
			ToolsPrefix:      "exa",
			Capabilities:     []string{"network"},
			ToolCapabilities: map[string][]string{"web_search": []string{"network"}},
			StartupTimeoutMS: 5000,
			CallTimeoutMS:    60000,
			MaxOutputBytes:   32768,
		},
	}

	if err := store.Save(context.Background(), settings); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	loaded, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	got := loaded.MCP.Servers["exa"]
	if !loaded.MCP.Enabled || got.Command != "npx" || got.Args[1] != "exa-mcp-server" || got.Env[0] != "EXA_API_KEY" {
		t.Fatalf("MCP settings = %#v", loaded.MCP)
	}
	if got.ToolCapabilities["web_search"][0] != "network" {
		t.Fatalf("tool capabilities = %#v", got.ToolCapabilities)
	}
}

func TestStoreRejectsInvalidMCPConfig(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{
			name: "transport",
			content: `
[mcp]
enabled = true

[mcp.servers.bad]
transport = "http"
command = "server"
`,
			want: "unsupported",
		},
		{
			name: "command",
			content: `
[mcp]
enabled = true

[mcp.servers.bad]
transport = "stdio"
`,
			want: "command is required",
		},
		{
			name: "capability",
			content: `
[mcp]
enabled = true

[mcp.servers.bad]
transport = "stdio"
command = "server"
capabilities = ["telepathy"]
`,
			want: "unknown",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, tc.name+".toml")
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatalf("write config: %v", err)
			}
			_, err := New(path).Load(context.Background())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Load err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestStoreAtomicWriteRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".freecode")
	path := filepath.Join(dir, "config.toml")
	store := New(path)

	settings := config.DefaultSettings()
	settings.Providers["local"] = model.Provider{
		ID:           "local",
		Name:         "local",
		Protocol:     "openai-chat",
		BaseURL:      "https://api.example.test/v1",
		Secret:       model.SecretRef{Name: "LOCAL_API_KEY", Source: "env"},
		DefaultModel: "coder",
		Enabled:      true,
	}
	settings.ActiveModel = model.NewRef("local", "coder")
	settings.Models[settings.ActiveModel] = model.NewModel("local", "coder")

	if err := store.Save(context.Background(), settings); err != nil {
		t.Fatalf("first save: %v", err)
	}
	// Save again — exercising the rename-over-existing path.
	if err := store.Save(context.Background(), settings); err != nil {
		t.Fatalf("second save: %v", err)
	}

	loaded, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.Providers["local"].BaseURL != "https://api.example.test/v1" {
		t.Fatalf("provider lost across atomic save round trip: %#v", loaded.Providers["local"])
	}

	// No leftover temp files in the config directory — the rename
	// should consume the temp and there should be no orphans.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read config dir: %v", err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".tmp") {
			t.Fatalf("found leftover temp file %q after atomic save", entry.Name())
		}
	}
}

func TestLoadMissingConfigReturnsDefaults(t *testing.T) {
	loaded, err := New(filepath.Join(t.TempDir(), ".freecode", "config.toml")).Load(context.Background())
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if loaded.Version != config.CurrentVersion {
		t.Fatalf("Version = %d, want %d", loaded.Version, config.CurrentVersion)
	}
	if loaded.Providers == nil {
		t.Fatal("Providers map is nil")
	}
}
