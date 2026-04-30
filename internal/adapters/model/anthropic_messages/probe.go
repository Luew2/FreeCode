package anthropic_messages

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/Luew2/FreeCode/internal/core/model"
	"github.com/Luew2/FreeCode/internal/ports"
)

var _ ports.ProtocolProbe = (*Probe)(nil)

type Probe struct {
	secrets ports.SecretStore
	client  *http.Client
}

func NewProbe(secrets ports.SecretStore, client *http.Client) *Probe {
	if client == nil {
		client = http.DefaultClient
	}
	return &Probe{secrets: secrets, client: client}
}

func (p *Probe) Probe(ctx context.Context, provider model.Provider, candidate model.ModelID) (ports.ProbeResult, error) {
	endpoint, err := MessagesEndpoint(provider.BaseURL)
	if err != nil {
		return ports.ProbeResult{}, &ProbeError{Protocol: Protocol, Endpoint: provider.BaseURL, Err: err}
	}
	body, err := json.Marshal(messagesRequest{
		Model:     string(candidate),
		MaxTokens: 1,
		Messages:  []anthropicMessage{{Role: "user", Content: []contentBlock{{Type: "text", Text: "ping"}}}},
	})
	if err != nil {
		return ports.ProbeResult{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return ports.ProbeResult{}, &ProbeError{Protocol: Protocol, Endpoint: endpoint, Err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	if provider.Secret.Name != "" {
		if p.secrets == nil {
			return ports.ProbeResult{}, &ProbeError{Protocol: Protocol, Endpoint: endpoint, Err: fmt.Errorf("secret store is not configured")}
		}
		apiKey, err := p.secrets.Get(ctx, provider.Secret.Name)
		if err != nil {
			return ports.ProbeResult{}, &ProbeError{Protocol: Protocol, Endpoint: endpoint, Err: err}
		}
		req.Header.Set("X-Api-Key", apiKey)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return ports.ProbeResult{}, &ProbeError{Protocol: Protocol, Endpoint: endpoint, Err: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ports.ProbeResult{}, &ProbeError{Protocol: Protocol, Endpoint: endpoint, StatusCode: resp.StatusCode, Body: readErrorBody(resp.Body)}
	}
	configured := model.NewModel(provider.ID, candidate)
	configured.Capabilities.Tools = true
	configured.Capabilities.Streaming = true
	configured.Limits.MaxOutputTokens = defaultMaxTokens
	return ports.ProbeResult{
		Protocol: Protocol,
		Endpoint: endpoint,
		Model:    configured,
		Metadata: map[string]string{
			"endpoint": endpoint,
		},
	}, nil
}

func MessagesEndpoint(baseURL string) (string, error) {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		return "", fmt.Errorf("base URL is required")
	}
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return "", err
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("base URL must include scheme and host")
	}
	path := strings.TrimRight(parsed.Path, "/")
	switch {
	case path == "":
		parsed.Path = "/v1/messages"
	case strings.HasSuffix(path, "/messages"):
		parsed.Path = path
	case strings.HasSuffix(path, "/v1"):
		parsed.Path = path + "/messages"
	default:
		parsed.Path = path + "/v1/messages"
	}
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}

type ProbeError struct {
	Protocol   model.ProtocolID
	Endpoint   string
	StatusCode int
	Body       string
	Err        error
}

func (e *ProbeError) Error() string {
	if e == nil {
		return ""
	}
	prefix := fmt.Sprintf("%s probe %s", e.Protocol, e.Endpoint)
	if e.StatusCode != 0 {
		if e.Body != "" {
			return fmt.Sprintf("%s returned status %d: %s", prefix, e.StatusCode, e.Body)
		}
		return fmt.Sprintf("%s returned status %d", prefix, e.StatusCode)
	}
	if e.Err != nil {
		return prefix + ": " + e.Err.Error()
	}
	return prefix
}
