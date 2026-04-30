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
	"sort"
	"strings"

	"github.com/Luew2/FreeCode/internal/core/model"
	"github.com/Luew2/FreeCode/internal/ports"
)

var _ ports.ModelClient = (*Client)(nil)

type Client struct {
	provider model.Provider
	endpoint string
	secrets  ports.SecretStore
	client   *http.Client
}

func NewClient(provider model.Provider, secrets ports.SecretStore, client *http.Client) (*Client, error) {
	endpoint, err := ChatCompletionsEndpoint(provider.BaseURL)
	if err != nil {
		return nil, &ClientError{Endpoint: provider.BaseURL, Err: err}
	}
	if client == nil {
		client = http.DefaultClient
	}
	return &Client{
		provider: provider,
		endpoint: endpoint,
		secrets:  secrets,
		client:   client,
	}, nil
}

func (c *Client) Stream(ctx context.Context, request model.Request) (<-chan model.Event, error) {
	if c == nil {
		return nil, errors.New("openai chat client is nil")
	}

	chatRequest, err := toChatRequest(c.provider, request)
	if err != nil {
		return nil, &ClientError{Endpoint: c.endpoint, Err: err}
	}
	body, err := json.Marshal(chatRequest)
	if err != nil {
		return nil, &ClientError{Endpoint: c.endpoint, Err: err}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, &ClientError{Endpoint: c.endpoint, Err: err}
	}
	req.Header.Set("Content-Type", "application/json")
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
		return nil, &ClientError{
			Endpoint:   c.endpoint,
			StatusCode: resp.StatusCode,
			Body:       readErrorBody(resp.Body),
		}
	}

	events := make(chan model.Event)
	if request.Stream {
		go c.readStream(resp, events)
	} else {
		go c.readNonStream(resp, events)
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
	req.Header.Set("Authorization", "Bearer "+apiKey)
	return nil
}

func (c *Client) readStream(resp *http.Response, events chan<- model.Event) {
	defer close(events)
	defer resp.Body.Close()

	events <- model.Event{Type: model.EventStarted}
	accumulator := newToolCallAccumulator()
	seenData := false

	err := scanServerSentEvents(resp.Body, func(payload string) error {
		seenData = true
		trimmed := strings.TrimSpace(payload)
		if trimmed == "" {
			return nil
		}
		if trimmed == "[DONE]" {
			return nil
		}
		if !looksLikeJSON(trimmed) {
			events <- model.Event{Type: model.EventTextDelta, Text: payload}
			return nil
		}

		var chunk chatStreamChunk
		if err := json.Unmarshal([]byte(trimmed), &chunk); err != nil {
			return fmt.Errorf("invalid streamed chat completion JSON: %w", err)
		}
		if chunk.Error != nil {
			return fmt.Errorf("provider error: %s", chunk.Error.message())
		}
		if chunk.Usage != nil {
			events <- model.Event{Type: model.EventUsage, Usage: chunk.Usage.toModelUsage()}
		}

		for _, choice := range chunk.Choices {
			if choice.Delta.Content != nil {
				events <- model.Event{Type: model.EventTextDelta, Text: *choice.Delta.Content}
			}
			for _, toolCall := range choice.Delta.ToolCalls {
				accumulator.add(toolCall)
			}
		}
		return nil
	})
	if err != nil {
		events <- model.Event{Type: model.EventError, Error: err.Error()}
		return
	}
	if !seenData {
		events <- model.Event{Type: model.EventError, Error: "stream response did not contain data frames"}
		return
	}

	for _, toolCall := range accumulator.complete() {
		events <- model.Event{Type: model.EventToolCall, ToolCall: &toolCall}
	}
	events <- model.Event{Type: model.EventCompleted}
}

func (c *Client) readNonStream(resp *http.Response, events chan<- model.Event) {
	defer close(events)
	defer resp.Body.Close()

	events <- model.Event{Type: model.EventStarted}

	var response chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		events <- model.Event{Type: model.EventError, Error: fmt.Sprintf("decode chat completion response: %v", err)}
		return
	}
	if response.Error != nil {
		events <- model.Event{Type: model.EventError, Error: "provider error: " + response.Error.message()}
		return
	}

	for _, choice := range response.Choices {
		if choice.Message.Content.Text != "" {
			events <- model.Event{Type: model.EventTextDelta, Text: choice.Message.Content.Text}
		}
		for _, toolCall := range choice.Message.ToolCalls {
			events <- model.Event{
				Type: model.EventToolCall,
				ToolCall: &model.ToolCall{
					ID:        toolCall.ID,
					Name:      toolCall.Function.Name,
					Arguments: []byte(toolCall.Function.Arguments),
				},
			}
		}
	}
	if response.Usage != nil {
		events <- model.Event{Type: model.EventUsage, Usage: response.Usage.toModelUsage()}
	}
	events <- model.Event{Type: model.EventCompleted}
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
	prefix := fmt.Sprintf("openai-chat endpoint %s", e.Endpoint)
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

func (e *ClientError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Tools       []chatTool    `json:"tools,omitempty"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
	Temperature *float64      `json:"temperature,omitempty"`
	Stream      bool          `json:"stream"`
}

type chatTool struct {
	Type     string           `json:"type"`
	Function chatToolFunction `json:"function"`
}

type chatToolFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
	Arguments   string         `json:"arguments,omitempty"`
}

type chatResponse struct {
	Choices []chatResponseChoice `json:"choices"`
	Usage   *chatUsage           `json:"usage,omitempty"`
	Error   *chatProviderError   `json:"error,omitempty"`
}

type chatResponseChoice struct {
	Message chatResponseMessage `json:"message"`
}

type chatResponseMessage struct {
	Content   chatContent    `json:"content"`
	ToolCalls []chatToolCall `json:"tool_calls,omitempty"`
}

type chatStreamChunk struct {
	Choices []chatStreamChoice `json:"choices"`
	Usage   *chatUsage         `json:"usage,omitempty"`
	Error   *chatProviderError `json:"error,omitempty"`
}

type chatStreamChoice struct {
	Delta chatStreamDelta `json:"delta"`
}

type chatStreamDelta struct {
	Content   *string        `json:"content,omitempty"`
	ToolCalls []chatToolCall `json:"tool_calls,omitempty"`
}

type chatToolCall struct {
	Index    int              `json:"index,omitempty"`
	ID       string           `json:"id,omitempty"`
	Type     string           `json:"type,omitempty"`
	Function chatToolFunction `json:"function"`
}

type chatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

func (u chatUsage) toModelUsage() *model.Usage {
	return &model.Usage{
		InputTokens:  u.PromptTokens,
		OutputTokens: u.CompletionTokens,
		TotalTokens:  u.TotalTokens,
	}
}

type chatProviderError struct {
	Message string `json:"message"`
	Type    string `json:"type,omitempty"`
	Code    any    `json:"code,omitempty"`
}

func (e chatProviderError) message() string {
	if e.Message != "" {
		return e.Message
	}
	if e.Type != "" {
		return e.Type
	}
	return "unknown error"
}

type chatContent struct {
	Text string
}

func (c *chatContent) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if bytes.Equal(trimmed, []byte("null")) {
		return nil
	}

	var text string
	if err := json.Unmarshal(trimmed, &text); err == nil {
		c.Text = text
		return nil
	}

	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(trimmed, &parts); err != nil {
		return err
	}
	for _, part := range parts {
		if part.Type == "" || part.Type == "text" {
			c.Text += part.Text
		}
	}
	return nil
}

func toChatRequest(provider model.Provider, request model.Request) (chatRequest, error) {
	modelID := string(request.Model.ID)
	if modelID == "" {
		modelID = string(provider.DefaultModel)
	}
	if modelID == "" {
		return chatRequest{}, errors.New("model id is empty")
	}

	chatMessages := make([]chatMessage, 0, len(request.Messages))
	for _, message := range request.Messages {
		chatMessages = append(chatMessages, chatMessage{
			Role:       string(message.Role),
			Name:       message.Name,
			Content:    textContent(message.Content),
			ToolCallID: message.ToolCallID,
			ToolCalls:  toChatToolCalls(message.ToolCalls),
		})
	}

	tools := make([]chatTool, 0, len(request.Tools))
	for _, tool := range request.Tools {
		tools = append(tools, chatTool{
			Type: "function",
			Function: chatToolFunction{
				Name:        tool.Name,
				Description: tool.Description,
				Parameters:  tool.InputSchema,
			},
		})
	}

	return chatRequest{
		Model:       modelID,
		Messages:    chatMessages,
		Tools:       tools,
		MaxTokens:   request.MaxOutputTokens,
		Temperature: request.Temperature,
		Stream:      request.Stream,
	}, nil
}

func toChatToolCalls(calls []model.ToolCall) []chatToolCall {
	if len(calls) == 0 {
		return nil
	}
	converted := make([]chatToolCall, 0, len(calls))
	for index, call := range calls {
		converted = append(converted, chatToolCall{
			Index: index,
			ID:    call.ID,
			Type:  "function",
			Function: chatToolFunction{
				Name:      call.Name,
				Arguments: string(call.Arguments),
			},
		})
	}
	return converted
}

func textContent(parts []model.ContentPart) string {
	var builder strings.Builder
	for _, part := range parts {
		if part.Type == model.ContentText {
			builder.WriteString(part.Text)
		}
	}
	return builder.String()
}

type toolCallAccumulator struct {
	calls map[int]*partialToolCall
}

type partialToolCall struct {
	id        string
	name      string
	arguments strings.Builder
}

func newToolCallAccumulator() *toolCallAccumulator {
	return &toolCallAccumulator{calls: map[int]*partialToolCall{}}
}

func (a *toolCallAccumulator) add(call chatToolCall) {
	partial := a.calls[call.Index]
	if partial == nil {
		partial = &partialToolCall{}
		a.calls[call.Index] = partial
	}
	if call.ID != "" {
		partial.id = call.ID
	}
	if call.Function.Name != "" {
		partial.name = call.Function.Name
	}
	if call.Function.Arguments != "" {
		partial.arguments.WriteString(call.Function.Arguments)
	}
}

func (a *toolCallAccumulator) complete() []model.ToolCall {
	indices := make([]int, 0, len(a.calls))
	for index := range a.calls {
		indices = append(indices, index)
	}
	sort.Ints(indices)

	completed := make([]model.ToolCall, 0, len(indices))
	for _, index := range indices {
		partial := a.calls[index]
		completed = append(completed, model.ToolCall{
			ID:        partial.id,
			Name:      partial.name,
			Arguments: []byte(partial.arguments.String()),
		})
	}
	return completed
}

func scanServerSentEvents(r io.Reader, onData func(string) error) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	var dataLines []string
	flush := func() error {
		if len(dataLines) == 0 {
			return nil
		}
		payload := strings.Join(dataLines, "\n")
		dataLines = dataLines[:0]
		return onData(payload)
	}

	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" {
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}

		field, value, ok := strings.Cut(line, ":")
		if !ok {
			return fmt.Errorf("malformed server-sent event line %q", line)
		}
		if strings.HasPrefix(value, " ") {
			value = strings.TrimPrefix(value, " ")
		}
		switch field {
		case "data":
			dataLines = append(dataLines, value)
		case "event", "id", "retry":
		default:
			return fmt.Errorf("unsupported server-sent event field %q", field)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read streamed chat completion: %w", err)
	}
	return flush()
}

func looksLikeJSON(value string) bool {
	return strings.HasPrefix(value, "{") || strings.HasPrefix(value, "[")
}
