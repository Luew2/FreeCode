package openai_chat

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/Luew2/FreeCode/internal/core/model"
	"github.com/Luew2/FreeCode/internal/ports"
)

const Protocol model.ProtocolID = "openai-chat"

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
	endpoint, err := ChatCompletionsEndpoint(provider.BaseURL)
	if err != nil {
		return ports.ProbeResult{}, &ProbeError{Protocol: Protocol, Endpoint: provider.BaseURL, Err: err}
	}
	if candidate == "" {
		return ports.ProbeResult{}, &ProbeError{Protocol: Protocol, Endpoint: endpoint, Err: errors.New("model id is empty")}
	}

	body, err := json.Marshal(chatCompletionsProbeRequest{
		Model: string(candidate),
		Messages: []chatMessage{
			{Role: "user", Content: "ping"},
		},
		MaxTokens: 1,
		Stream:    true,
	})
	if err != nil {
		return ports.ProbeResult{}, &ProbeError{Protocol: Protocol, Endpoint: endpoint, Err: err}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return ports.ProbeResult{}, &ProbeError{Protocol: Protocol, Endpoint: endpoint, Err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	if provider.Secret.Name != "" {
		if p.secrets == nil {
			return ports.ProbeResult{}, &ProbeError{Protocol: Protocol, Endpoint: endpoint, Err: errors.New("secret store is not configured")}
		}
		apiKey, err := p.secrets.Get(ctx, provider.Secret.Name)
		if err != nil {
			return ports.ProbeResult{}, &ProbeError{Protocol: Protocol, Endpoint: endpoint, Err: err}
		}
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return ports.ProbeResult{}, &ProbeError{Protocol: Protocol, Endpoint: endpoint, Err: err}
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ports.ProbeResult{}, &ProbeError{
			Protocol:   Protocol,
			Endpoint:   endpoint,
			StatusCode: resp.StatusCode,
			Body:       readErrorBody(resp.Body),
		}
	}
	if err := validateStreamShape(resp.Body); err != nil {
		return ports.ProbeResult{}, &ProbeError{Protocol: Protocol, Endpoint: endpoint, Err: err}
	}

	probedModel := model.NewModel(provider.ID, candidate)
	probedModel.Capabilities.Streaming = true
	return ports.ProbeResult{
		Protocol: Protocol,
		Endpoint: endpoint,
		Model:    probedModel,
		Metadata: map[string]string{
			"probe_endpoint": endpoint,
		},
	}, nil
}

func ChatCompletionsEndpoint(baseURL string) (string, error) {
	if strings.TrimSpace(baseURL) == "" {
		return "", errors.New("base url is empty")
	}
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return "", err
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("base url must include scheme and host")
	}

	path := strings.TrimRight(parsed.Path, "/")
	switch {
	case path == "":
		parsed.Path = "/v1/chat/completions"
	case strings.HasSuffix(path, "/chat/completions"):
		parsed.Path = path
	case strings.HasSuffix(path, "/v1"):
		parsed.Path = path + "/chat/completions"
	default:
		parsed.Path = path + "/v1/chat/completions"
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
	prefix := fmt.Sprintf("probe %s endpoint %s", e.Protocol, e.Endpoint)
	if e.StatusCode != 0 {
		if e.Body != "" {
			return fmt.Sprintf("%s returned status %d: %s", prefix, e.StatusCode, e.Body)
		}
		return fmt.Sprintf("%s returned status %d", prefix, e.StatusCode)
	}
	if e.Err != nil {
		return fmt.Sprintf("%s failed: %v", prefix, e.Err)
	}
	return prefix + " failed"
}

func (e *ProbeError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

type chatCompletionsProbeRequest struct {
	Model     string        `json:"model"`
	Messages  []chatMessage `json:"messages"`
	MaxTokens int           `json:"max_tokens"`
	Stream    bool          `json:"stream"`
}

type chatMessage struct {
	Role       string         `json:"role"`
	Name       string         `json:"name,omitempty"`
	Content    string         `json:"content,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	ToolCalls  []chatToolCall `json:"tool_calls,omitempty"`
}

type streamChunk struct {
	Choices []json.RawMessage `json:"choices"`
}

func validateStreamShape(r io.Reader) error {
	scanner := bufio.NewScanner(io.LimitReader(r, 16*1024))
	seenData := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, ":") || strings.HasPrefix(line, "event:") || strings.HasPrefix(line, "id:") || strings.HasPrefix(line, "retry:") {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			return fmt.Errorf("expected streamed chat completion data, got %q", line)
		}

		seenData = true
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			continue
		}

		var chunk streamChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			return fmt.Errorf("invalid streamed chat completion JSON: %w", err)
		}
		if len(chunk.Choices) == 0 {
			return errors.New("streamed chat completion chunk has no choices")
		}
		return nil
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read streamed chat completion: %w", err)
	}
	if !seenData {
		return errors.New("stream response did not contain data frames")
	}
	return errors.New("stream response did not contain a chat completion chunk")
}

func readErrorBody(r io.Reader) string {
	data, err := io.ReadAll(io.LimitReader(r, 1024))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
