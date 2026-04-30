package bootstrap

import (
	"context"
	"path/filepath"
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
