package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	anthropic_messages "github.com/Luew2/FreeCode/internal/adapters/model/anthropic_messages"
	openai_chat "github.com/Luew2/FreeCode/internal/adapters/model/openai_chat"
	"github.com/Luew2/FreeCode/internal/app/commands"
	"github.com/Luew2/FreeCode/internal/core/model"
	"github.com/Luew2/FreeCode/internal/ports"
)

type protocolProbeChain struct {
	openai    ports.ProtocolProbe
	anthropic ports.ProtocolProbe
	secrets   ports.SecretStore
	client    *http.Client
}

func newProtocolProbeChain(secrets ports.SecretStore) protocolProbeChain {
	client := http.DefaultClient
	return protocolProbeChain{
		openai:    openai_chat.NewProbe(secrets, client),
		anthropic: anthropic_messages.NewProbe(secrets, client),
		secrets:   secrets,
		client:    client,
	}
}

func (p protocolProbeChain) Probe(ctx context.Context, provider model.Provider, candidate model.ModelID) (ports.ProbeResult, error) {
	switch string(provider.Protocol) {
	case commands.ProviderProtocolOpenAIChat:
		return p.openai.Probe(ctx, provider, candidate)
	case commands.ProviderProtocolAnthropicMessages:
		return p.anthropic.Probe(ctx, provider, candidate)
	case "", commands.ProviderProtocolAuto:
		modelMetadata := p.discoverModels(ctx, provider)
		openAIResult, openAIErr := p.openai.Probe(ctx, providerWithProtocol(provider, openai_chat.Protocol), candidate)
		if openAIErr == nil {
			openAIResult.Metadata = mergeMetadata(openAIResult.Metadata, modelMetadata)
			return openAIResult, nil
		}
		anthropicResult, anthropicErr := p.anthropic.Probe(ctx, providerWithProtocol(provider, anthropic_messages.Protocol), candidate)
		if anthropicErr == nil {
			anthropicResult.Metadata = mergeMetadata(anthropicResult.Metadata, modelMetadata)
			return anthropicResult, nil
		}
		return ports.ProbeResult{}, fmt.Errorf("auto provider probe failed: openai-chat: %v; anthropic-messages: %v", openAIErr, anthropicErr)
	default:
		return ports.ProbeResult{}, fmt.Errorf("unsupported provider protocol %q", strings.TrimSpace(string(provider.Protocol)))
	}
}

func (p protocolProbeChain) discoverModels(ctx context.Context, provider model.Provider) map[string]string {
	endpoint, err := modelsEndpoint(provider.BaseURL)
	if err != nil {
		return map[string]string{"models_error": err.Error()}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return map[string]string{"models_endpoint": endpoint, "models_error": err.Error()}
	}
	if provider.Secret.Name != "" && p.secrets != nil {
		if key, err := p.secrets.Get(ctx, provider.Secret.Name); err == nil && key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
			req.Header.Set("X-Api-Key", key)
		}
	}
	client := p.client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return map[string]string{"models_endpoint": endpoint, "models_error": err.Error()}
	}
	defer resp.Body.Close()
	metadata := map[string]string{"models_endpoint": endpoint, "models_status": fmt.Sprintf("%d", resp.StatusCode)}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return metadata
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
		Models []struct {
			ID string `json:"id"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		metadata["models_error"] = err.Error()
		return metadata
	}
	var ids []string
	for _, item := range body.Data {
		if strings.TrimSpace(item.ID) != "" {
			ids = append(ids, item.ID)
		}
	}
	for _, item := range body.Models {
		if strings.TrimSpace(item.ID) != "" {
			ids = append(ids, item.ID)
		}
	}
	if len(ids) > 0 {
		metadata["models"] = strings.Join(ids, ",")
	}
	return metadata
}

func modelsEndpoint(baseURL string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return "", err
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("base URL must include scheme and host")
	}
	path := strings.TrimRight(parsed.Path, "/")
	switch {
	case path == "":
		parsed.Path = "/v1/models"
	case strings.HasSuffix(path, "/v1"):
		parsed.Path = path + "/models"
	case strings.HasSuffix(path, "/models"):
		parsed.Path = path
	default:
		parsed.Path = path + "/v1/models"
	}
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}

func mergeMetadata(primary map[string]string, secondary map[string]string) map[string]string {
	if len(primary) == 0 && len(secondary) == 0 {
		return nil
	}
	merged := map[string]string{}
	for key, value := range secondary {
		merged[key] = value
	}
	for key, value := range primary {
		merged[key] = value
	}
	return merged
}

func providerWithProtocol(provider model.Provider, protocol model.ProtocolID) model.Provider {
	provider.Protocol = protocol
	return provider
}
