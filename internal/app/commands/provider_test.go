package commands

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/Luew2/FreeCode/internal/core/config"
	"github.com/Luew2/FreeCode/internal/core/model"
	"github.com/Luew2/FreeCode/internal/ports"
)

func TestAddProviderSkipProbeStoresConfig(t *testing.T) {
	store := newMemoryConfigStore()
	probe := &recordingProbe{}
	var out bytes.Buffer

	err := AddProvider(context.Background(), &out, store, probe, ProviderAddOptions{
		Name:            "local",
		BaseURL:         "http://example.test/v1",
		APIKeyEnv:       "LOCAL_API_KEY",
		Model:           "coder",
		Protocol:        ProviderProtocolOpenAIChat,
		ContextWindow:   128000,
		MaxOutputTokens: 8192,
		SkipProbe:       true,
	})
	if err != nil {
		t.Fatalf("AddProvider returned error: %v", err)
	}
	if probe.calls != 0 {
		t.Fatalf("probe calls = %d, want 0", probe.calls)
	}

	provider := store.settings.Providers["local"]
	if provider.Protocol != "openai-chat" {
		t.Fatalf("Protocol = %q, want openai-chat", provider.Protocol)
	}
	if provider.Secret.Name != "LOCAL_API_KEY" {
		t.Fatalf("Secret.Name = %q, want LOCAL_API_KEY", provider.Secret.Name)
	}
	ref := model.NewRef("local", "coder")
	if _, ok := store.settings.Models[ref]; !ok {
		t.Fatalf("model %q was not stored", ref.String())
	}
	configured := store.settings.Models[ref]
	if configured.Limits.ContextWindow != 128000 || configured.Limits.MaxOutputTokens != 8192 {
		t.Fatalf("limits = %#v, want configured context/output limits", configured.Limits)
	}
	if !strings.Contains(out.String(), "local") || !strings.Contains(out.String(), "coder") {
		t.Fatalf("output = %q, want provider and model", out.String())
	}
}

func TestAddProviderAutoUsesProbe(t *testing.T) {
	store := newMemoryConfigStore()
	probe := &recordingProbe{
		result: ports.ProbeResult{
			Protocol: "openai-chat",
			Endpoint: "http://example.test/v1/chat/completions",
			Model:    model.NewModel("local", "coder"),
			Metadata: map[string]string{"probe_endpoint": "http://example.test/v1/chat/completions"},
		},
	}

	err := AddProvider(context.Background(), &bytes.Buffer{}, store, probe, ProviderAddOptions{
		Name:      "local",
		BaseURL:   "http://example.test",
		APIKeyEnv: "LOCAL_API_KEY",
		Model:     "coder",
		Protocol:  ProviderProtocolAuto,
	})
	if err != nil {
		t.Fatalf("AddProvider returned error: %v", err)
	}
	if probe.calls != 1 {
		t.Fatalf("probe calls = %d, want 1", probe.calls)
	}
	if probe.lastProtocol != ProviderProtocolAuto {
		t.Fatalf("probe protocol = %q, want auto", probe.lastProtocol)
	}
	if got := store.settings.Providers["local"].Metadata["probe_endpoint"]; got != "http://example.test/v1/chat/completions" {
		t.Fatalf("probe_endpoint = %q", got)
	}
}

func TestListProvidersShowsProviderAndModel(t *testing.T) {
	store := newMemoryConfigStore()
	ref := model.NewRef("local", "coder")
	store.settings.Providers["local"] = model.Provider{
		ID:           "local",
		Protocol:     "openai-chat",
		BaseURL:      "http://example.test/v1",
		DefaultModel: "coder",
		Enabled:      true,
	}
	store.settings.Models[ref] = model.NewModel("local", "coder")

	var out bytes.Buffer
	if err := ListProviders(context.Background(), &out, store); err != nil {
		t.Fatalf("ListProviders returned error: %v", err)
	}
	if !strings.Contains(out.String(), "local") || !strings.Contains(out.String(), "coder") {
		t.Fatalf("output = %q, want provider and model", out.String())
	}
}

type memoryConfigStore struct {
	settings config.Settings
}

func newMemoryConfigStore() *memoryConfigStore {
	return &memoryConfigStore{settings: config.DefaultSettings()}
}

func (s *memoryConfigStore) Load(ctx context.Context) (config.Settings, error) {
	return s.settings, nil
}

func (s *memoryConfigStore) Save(ctx context.Context, settings config.Settings) error {
	s.settings = settings
	return nil
}

type recordingProbe struct {
	calls        int
	lastProtocol model.ProtocolID
	result       ports.ProbeResult
	err          error
}

func (p *recordingProbe) Probe(ctx context.Context, provider model.Provider, candidate model.ModelID) (ports.ProbeResult, error) {
	p.calls++
	p.lastProtocol = provider.Protocol
	if p.err != nil {
		return ports.ProbeResult{}, p.err
	}
	if p.result.Protocol == "" {
		return ports.ProbeResult{
			Protocol: "openai-chat",
			Model:    model.NewModel(provider.ID, candidate),
		}, nil
	}
	return p.result, nil
}

func TestAddProviderValidation(t *testing.T) {
	err := AddProvider(context.Background(), &bytes.Buffer{}, newMemoryConfigStore(), nil, ProviderAddOptions{})
	if err == nil {
		t.Fatal("AddProvider returned nil error")
	}
	if !strings.Contains(err.Error(), "--name") {
		t.Fatalf("error = %q, want --name", err.Error())
	}
}

func TestAddProviderRejectsPastedAPIKeyAsEnvName(t *testing.T) {
	err := AddProvider(context.Background(), &bytes.Buffer{}, newMemoryConfigStore(), nil, ProviderAddOptions{
		Name:      "local",
		BaseURL:   "http://example.test/v1",
		APIKeyEnv: "sk-test-secret",
		Model:     "coder",
		Protocol:  ProviderProtocolOpenAIChat,
		SkipProbe: true,
	})
	if err == nil {
		t.Fatal("AddProvider returned nil error")
	}
	if !strings.Contains(err.Error(), "environment variable name") {
		t.Fatalf("error = %q, want env var validation", err.Error())
	}
}
