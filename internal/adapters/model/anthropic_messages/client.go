package anthropic_messages

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/Luew2/FreeCode/internal/core/model"
	"github.com/Luew2/FreeCode/internal/ports"
)

const (
	Protocol         model.ProtocolID = "anthropic-messages"
	defaultMaxTokens                  = 1024
)

var _ ports.ModelClient = (*Client)(nil)

type Client struct {
	provider model.Provider
	endpoint string
	secrets  ports.SecretStore
	client   *http.Client
}

func NewClient(provider model.Provider, secrets ports.SecretStore, client *http.Client) (*Client, error) {
	endpoint, err := MessagesEndpoint(provider.BaseURL)
	if err != nil {
		return nil, &ClientError{Endpoint: provider.BaseURL, Err: err}
	}
	if client == nil {
		client = http.DefaultClient
	}
	return &Client{provider: provider, endpoint: endpoint, secrets: secrets, client: client}, nil
}

func (c *Client) Stream(ctx context.Context, request model.Request) (<-chan model.Event, error) {
	if c == nil {
		return nil, errors.New("anthropic messages client is nil")
	}
	body, err := json.Marshal(toMessagesRequest(c.provider, request))
	if err != nil {
		return nil, &ClientError{Endpoint: c.endpoint, Err: err}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, &ClientError{Endpoint: c.endpoint, Err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	if request.Stream {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	if err := c.authorize(ctx, req); err != nil {
		return nil, &ClientError{Endpoint: c.endpoint, Err: err}
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, &ClientError{Endpoint: c.endpoint, Err: err}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		return nil, &ClientError{Endpoint: c.endpoint, StatusCode: resp.StatusCode, Body: readErrorBody(resp.Body)}
	}
	events := make(chan model.Event)
	if request.Stream {
		go c.readStream(resp, events)
	} else {
		go c.readResponse(resp, events)
	}
	return events, nil
}

func (c *Client) authorize(ctx context.Context, req *http.Request) error {
	if c.provider.Secret.Name == "" {
		return nil
	}
	if c.secrets == nil {
		return errors.New("secret store is not configured")
	}
	apiKey, err := c.secrets.Get(ctx, c.provider.Secret.Name)
	if err != nil {
		return err
	}
	req.Header.Set("X-Api-Key", apiKey)
	return nil
}

func (c *Client) readStream(resp *http.Response, events chan<- model.Event) {
	defer close(events)
	defer resp.Body.Close()

	events <- model.Event{Type: model.EventStarted}
	toolBlocks := map[int]*toolBlock{}
	err := scanServerSentEvents(resp.Body, func(payload string) error {
		trimmed := strings.TrimSpace(payload)
		if trimmed == "" || trimmed == "[DONE]" {
			return nil
		}
		var event streamEvent
		if err := json.Unmarshal([]byte(trimmed), &event); err != nil {
			return fmt.Errorf("invalid streamed Anthropic messages JSON: %w", err)
		}
		if event.Error != nil {
			return fmt.Errorf("provider error: %s", event.Error.Message)
		}
		switch event.Type {
		case "content_block_start":
			if event.ContentBlock.Type == "text" && event.ContentBlock.Text != "" {
				events <- model.Event{Type: model.EventTextDelta, Text: event.ContentBlock.Text}
			}
			if event.ContentBlock.Type == "tool_use" {
				block := &toolBlock{ID: event.ContentBlock.ID, Name: event.ContentBlock.Name}
				if len(event.ContentBlock.Input) > 0 && strings.TrimSpace(string(event.ContentBlock.Input)) != "{}" {
					block.arguments.Write(event.ContentBlock.Input)
				}
				toolBlocks[event.Index] = block
			}
		case "content_block_delta":
			if event.Delta.Type == "text_delta" {
				events <- model.Event{Type: model.EventTextDelta, Text: event.Delta.Text}
			}
			if event.Delta.Type == "input_json_delta" {
				block := toolBlocks[event.Index]
				if block == nil {
					block = &toolBlock{}
					toolBlocks[event.Index] = block
				}
				block.arguments.WriteString(event.Delta.PartialJSON)
			}
		case "message_delta":
			if event.Usage.OutputTokens > 0 || event.Usage.InputTokens > 0 {
				usage := event.Usage.toModelUsage()
				events <- model.Event{Type: model.EventUsage, Usage: &usage}
			}
		}
		return nil
	})
	if err != nil {
		events <- model.Event{Type: model.EventError, Error: err.Error()}
		return
	}
	indexes := make([]int, 0, len(toolBlocks))
	for index := range toolBlocks {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	for _, index := range indexes {
		block := toolBlocks[index]
		events <- model.Event{Type: model.EventToolCall, ToolCall: &model.ToolCall{
			ID:        block.ID,
			Name:      block.Name,
			Arguments: normalizeArguments(block.arguments.String()),
		}}
	}
	events <- model.Event{Type: model.EventCompleted}
}

func (c *Client) readResponse(resp *http.Response, events chan<- model.Event) {
	defer close(events)
	defer resp.Body.Close()

	events <- model.Event{Type: model.EventStarted}
	var response messagesResponse
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		events <- model.Event{Type: model.EventError, Error: fmt.Sprintf("decode Anthropic messages response: %v", err)}
		return
	}
	if response.Error != nil {
		events <- model.Event{Type: model.EventError, Error: "provider error: " + response.Error.Message}
		return
	}
	for _, block := range response.Content {
		switch block.Type {
		case "text":
			events <- model.Event{Type: model.EventTextDelta, Text: block.Text}
		case "tool_use":
			events <- model.Event{Type: model.EventToolCall, ToolCall: &model.ToolCall{
				ID:        block.ID,
				Name:      block.Name,
				Arguments: normalizeArguments(string(block.Input)),
			}}
		}
	}
	if response.Usage.InputTokens > 0 || response.Usage.OutputTokens > 0 {
		usage := response.Usage.toModelUsage()
		events <- model.Event{Type: model.EventUsage, Usage: &usage}
	}
	events <- model.Event{Type: model.EventCompleted}
}

type messagesRequest struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens"`
	Stream    bool               `json:"stream,omitempty"`
	System    string             `json:"system,omitempty"`
	Messages  []anthropicMessage `json:"messages"`
	Tools     []anthropicTool    `json:"tools,omitempty"`
}

type anthropicMessage struct {
	Role    string         `json:"role"`
	Content []contentBlock `json:"content"`
}

type contentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   string          `json:"content,omitempty"`
}

type anthropicTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema,omitempty"`
}

func toMessagesRequest(provider model.Provider, request model.Request) messagesRequest {
	maxTokens := request.MaxOutputTokens
	if maxTokens <= 0 {
		maxTokens = defaultMaxTokens
	}
	value := messagesRequest{
		Model:     string(request.Model.ID),
		MaxTokens: maxTokens,
		Stream:    request.Stream,
	}
	if value.Model == "" {
		value.Model = string(provider.DefaultModel)
	}
	for _, message := range request.Messages {
		switch message.Role {
		case model.RoleSystem, model.RoleDeveloper:
			value.System = joinNonEmpty(value.System, messageText(message))
		case model.RoleAssistant:
			blocks := textBlocks(message)
			for _, call := range message.ToolCalls {
				blocks = append(blocks, contentBlock{
					Type:  "tool_use",
					ID:    call.ID,
					Name:  call.Name,
					Input: normalizeArguments(string(call.Arguments)),
				})
			}
			if len(blocks) > 0 {
				value.Messages = append(value.Messages, anthropicMessage{Role: "assistant", Content: blocks})
			}
		case model.RoleTool:
			value.Messages = append(value.Messages, anthropicMessage{Role: "user", Content: []contentBlock{{
				Type:      "tool_result",
				ToolUseID: message.ToolCallID,
				Content:   messageText(message),
			}}})
		default:
			blocks := textBlocks(message)
			if len(blocks) > 0 {
				value.Messages = append(value.Messages, anthropicMessage{Role: "user", Content: blocks})
			}
		}
	}
	for _, tool := range request.Tools {
		value.Tools = append(value.Tools, anthropicTool{Name: tool.Name, Description: tool.Description, InputSchema: tool.InputSchema})
	}
	return value
}

func textBlocks(message model.Message) []contentBlock {
	text := messageText(message)
	if strings.TrimSpace(text) == "" {
		return nil
	}
	return []contentBlock{{Type: "text", Text: text}}
}

func messageText(message model.Message) string {
	var parts []string
	for _, part := range message.Content {
		if strings.TrimSpace(part.Text) != "" {
			parts = append(parts, part.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func joinNonEmpty(left string, right string) string {
	left = strings.TrimSpace(left)
	right = strings.TrimSpace(right)
	if left == "" {
		return right
	}
	if right == "" {
		return left
	}
	return left + "\n\n" + right
}

func normalizeArguments(raw string) json.RawMessage {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return json.RawMessage(`{}`)
	}
	if json.Valid([]byte(raw)) {
		return json.RawMessage(raw)
	}
	data, _ := json.Marshal(raw)
	return data
}

type toolBlock struct {
	ID        string
	Name      string
	arguments strings.Builder
}

type streamEvent struct {
	Type         string        `json:"type"`
	Index        int           `json:"index"`
	Delta        streamDelta   `json:"delta"`
	ContentBlock contentBlock  `json:"content_block"`
	Usage        usagePayload  `json:"usage"`
	Error        *errorPayload `json:"error"`
}

type streamDelta struct {
	Type        string `json:"type"`
	Text        string `json:"text"`
	PartialJSON string `json:"partial_json"`
}

type messagesResponse struct {
	Content []contentBlock `json:"content"`
	Usage   usagePayload   `json:"usage"`
	Error   *errorPayload  `json:"error"`
}

type usagePayload struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

func (u usagePayload) toModelUsage() model.Usage {
	return model.Usage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, TotalTokens: u.InputTokens + u.OutputTokens}
}

type errorPayload struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

type ClientError struct {
	Endpoint   string
	StatusCode int
	Body       string
	Err        error
}

func (e *ClientError) Error() string {
	if e == nil {
		return ""
	}
	prefix := fmt.Sprintf("anthropic-messages endpoint %s", e.Endpoint)
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

func readErrorBody(body io.Reader) string {
	data, err := io.ReadAll(io.LimitReader(body, 4096))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func scanServerSentEvents(r io.Reader, handle func(string) error) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	var data []string
	flush := func() error {
		if len(data) == 0 {
			return nil
		}
		payload := strings.Join(data, "\n")
		data = nil
		return handle(payload)
	}
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return flush()
}
