// Package anthropic_messages speaks the Anthropic Messages API via the
// official anthropic-sdk-go SDK. The SDK handles SSE parsing, retries,
// base-URL routing and content-block accumulation; the wrapper here adds
// FreeCode-specific concerns: secret resolution at request time, system /
// developer message merging, and translation between model.Request /
// model.Event and the SDK's typed parameters.
package anthropic_messages

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
	"github.com/anthropics/anthropic-sdk-go/packages/ssestream"

	"github.com/Luew2/FreeCode/internal/adapters/model/transport"
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
	baseURL  string
	secrets  ports.SecretStore
	client   *http.Client
	policy   transport.RetryPolicy
}

func NewClient(provider model.Provider, secrets ports.SecretStore, client *http.Client) (*Client, error) {
	endpoint, err := MessagesEndpoint(provider.BaseURL)
	if err != nil {
		return nil, &ClientError{Endpoint: provider.BaseURL, Err: err}
	}
	baseURL, err := sdkBaseURL(provider.BaseURL)
	if err != nil {
		return nil, &ClientError{Endpoint: provider.BaseURL, Err: err}
	}
	if client == nil {
		client = transport.DefaultHTTPClient()
	}
	return &Client{
		provider: provider,
		endpoint: endpoint,
		baseURL:  baseURL,
		secrets:  secrets,
		client:   client,
		policy:   transport.DefaultRetryPolicy(),
	}, nil
}

// SetRetryPolicy overrides the retry policy. The anthropic-sdk-go SDK only
// exposes MaxAttempts via WithMaxRetries; the additional knobs are accepted
// for signature compatibility but ignored.
func (c *Client) SetRetryPolicy(policy transport.RetryPolicy) {
	if c == nil {
		return
	}
	c.policy = policy
}

func (c *Client) Stream(ctx context.Context, request model.Request) (<-chan model.Event, error) {
	if c == nil {
		return nil, errors.New("anthropic messages client is nil")
	}

	apiKey, err := c.resolveAPIKey(ctx)
	if err != nil {
		return nil, &ClientError{Endpoint: c.endpoint, Err: err}
	}

	opts := c.buildOptions(apiKey, request.Stream)
	internal := toMessagesRequest(c.provider, request)
	params := internal.toSDKParams()

	sdk := anthropic.NewClient(opts...)
	if request.Stream {
		stream := sdk.Messages.NewStreaming(ctx, params)
		if streamErr := stream.Err(); streamErr != nil {
			_ = stream.Close()
			return nil, c.toClientError(streamErr)
		}
		events := make(chan model.Event)
		go c.readStream(ctx, stream, events)
		return events, nil
	}

	message, err := sdk.Messages.New(ctx, params)
	if err != nil {
		return nil, c.toClientError(err)
	}
	events := make(chan model.Event)
	go c.readResponse(ctx, message, events)
	return events, nil
}

func (c *Client) buildOptions(apiKey string, streaming bool) []option.RequestOption {
	maxRetries := c.policy.MaxAttempts - 1
	if maxRetries < 0 {
		maxRetries = 0
	}
	opts := []option.RequestOption{
		option.WithBaseURL(c.baseURL),
		option.WithHTTPClient(c.client),
		option.WithMaxRetries(maxRetries),
	}
	if streaming {
		opts = append(opts, option.WithHeader("Accept", "text/event-stream"))
	}
	if apiKey != "" {
		opts = append(opts, option.WithAPIKey(apiKey))
	}
	return opts
}

func (c *Client) resolveAPIKey(ctx context.Context) (string, error) {
	if c.provider.Secret.Name == "" {
		return "", nil
	}
	if c.secrets == nil {
		return "", errors.New("secret store is not configured")
	}
	return c.secrets.Get(ctx, c.provider.Secret.Name)
}

func (c *Client) readStream(ctx context.Context, stream *messagesStream, events chan<- model.Event) {
	defer close(events)
	defer stream.Close()

	if !emitModelEvent(ctx, events, model.Event{Type: model.EventStarted}) {
		return
	}
	acc := anthropic.Message{}
	streamErr := false
	diag := &model.Diagnostics{}

	for stream.Next() {
		diag.ChunkCount++
		event := stream.Current()
		if err := acc.Accumulate(event); err != nil {
			emitModelEvent(ctx, events, model.Event{Type: model.EventError, Error: err.Error()})
			streamErr = true
			return
		}
		switch event.Type {
		case "content_block_start":
			block := event.ContentBlock
			if block.Type == "text" && block.Text != "" {
				diag.TextDeltaCount++
				if !emitModelEvent(ctx, events, model.Event{Type: model.EventTextDelta, Text: block.Text}) {
					return
				}
			}
		case "content_block_delta":
			if event.Delta.Type == "text_delta" {
				diag.TextDeltaCount++
				if !emitModelEvent(ctx, events, model.Event{Type: model.EventTextDelta, Text: event.Delta.Text}) {
					return
				}
			}
		case "message_start":
			usage := event.Message.Usage
			if usage.InputTokens > 0 || usage.OutputTokens > 0 {
				if !emitModelEvent(ctx, events, model.Event{Type: model.EventUsage, Usage: &model.Usage{
					InputTokens:  int(usage.InputTokens),
					OutputTokens: int(usage.OutputTokens),
					TotalTokens:  int(usage.InputTokens + usage.OutputTokens),
				}}) {
					return
				}
			}
		case "message_delta":
			usage := event.Usage
			if usage.InputTokens > 0 || usage.OutputTokens > 0 {
				if !emitModelEvent(ctx, events, model.Event{Type: model.EventUsage, Usage: &model.Usage{
					InputTokens:  int(usage.InputTokens),
					OutputTokens: int(usage.OutputTokens),
					TotalTokens:  int(usage.InputTokens + usage.OutputTokens),
				}}) {
					return
				}
			}
			if reason := string(event.Delta.StopReason); reason != "" {
				diag.FinishReason = reason
			}
		}
	}
	if err := stream.Err(); err != nil {
		emitModelEvent(ctx, events, model.Event{Type: model.EventError, Error: classifyStreamError(err)})
		return
	}
	if streamErr {
		return
	}
	if diag.FinishReason == "" {
		if reason := string(acc.StopReason); reason != "" {
			diag.FinishReason = reason
		}
	}
	for _, block := range acc.Content {
		if block.Type == "tool_use" {
			args := normalizeArguments(string(block.Input))
			diag.ToolCallCount++
			if !emitModelEvent(ctx, events, model.Event{Type: model.EventToolCall, ToolCall: &model.ToolCall{
				ID:        block.ID,
				Name:      block.Name,
				Arguments: args,
			}}) {
				return
			}
		}
	}
	if raw := acc.RawJSON(); raw != "" {
		if len(raw) > 2048 {
			raw = raw[:2048] + "...[truncated]"
		}
		diag.RawLastChunk = raw
	}
	emitModelEvent(ctx, events, model.Event{Type: model.EventCompleted, Diagnostics: diag})
}

func (c *Client) readResponse(ctx context.Context, message *anthropic.Message, events chan<- model.Event) {
	defer close(events)
	if !emitModelEvent(ctx, events, model.Event{Type: model.EventStarted}) {
		return
	}

	diag := &model.Diagnostics{ChunkCount: 1, FinishReason: string(message.StopReason)}
	for _, block := range message.Content {
		switch block.Type {
		case "text":
			if block.Text != "" {
				diag.TextDeltaCount++
				if !emitModelEvent(ctx, events, model.Event{Type: model.EventTextDelta, Text: block.Text}) {
					return
				}
			}
		case "tool_use":
			args := normalizeArguments(string(block.Input))
			diag.ToolCallCount++
			if !emitModelEvent(ctx, events, model.Event{Type: model.EventToolCall, ToolCall: &model.ToolCall{
				ID:        block.ID,
				Name:      block.Name,
				Arguments: args,
			}}) {
				return
			}
		}
	}
	if message.Usage.InputTokens > 0 || message.Usage.OutputTokens > 0 {
		if !emitModelEvent(ctx, events, model.Event{Type: model.EventUsage, Usage: &model.Usage{
			InputTokens:  int(message.Usage.InputTokens),
			OutputTokens: int(message.Usage.OutputTokens),
			TotalTokens:  int(message.Usage.InputTokens + message.Usage.OutputTokens),
		}}) {
			return
		}
	}
	if raw := message.RawJSON(); raw != "" {
		if len(raw) > 2048 {
			raw = raw[:2048] + "...[truncated]"
		}
		diag.RawLastChunk = raw
	}
	emitModelEvent(ctx, events, model.Event{Type: model.EventCompleted, Diagnostics: diag})
}

func emitModelEvent(ctx context.Context, events chan<- model.Event, event model.Event) bool {
	select {
	case <-ctx.Done():
		return false
	case events <- event:
		return true
	}
}

func (c *Client) toClientError(err error) error {
	if err == nil {
		return nil
	}
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) && apiErr.Response != nil {
		body := readResponseBody(apiErr.Response)
		return &ClientError{Endpoint: c.endpoint, StatusCode: apiErr.StatusCode, Body: body}
	}
	return &ClientError{Endpoint: c.endpoint, Err: err}
}

func readResponseBody(resp *http.Response) string {
	if resp == nil || resp.Body == nil {
		return ""
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func classifyStreamError(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if strings.HasPrefix(msg, "received error while streaming: ") {
		body := strings.TrimPrefix(msg, "received error while streaming: ")
		return "provider error: " + extractProviderMessage(body)
	}
	if isJSONParseError(err) {
		return "invalid streamed Anthropic messages JSON: " + msg
	}
	return msg
}

func extractProviderMessage(body string) string {
	body = strings.TrimSpace(body)
	body = strings.TrimPrefix(body, "received error while streaming: ")
	body = strings.TrimSpace(body)
	if body == "" {
		return "unknown error"
	}
	var envelope struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	}
	if err := json.Unmarshal([]byte(body), &envelope); err == nil {
		if envelope.Message != "" {
			return envelope.Message
		}
		if envelope.Type != "" {
			return envelope.Type
		}
	}
	return body
}

func isJSONParseError(err error) bool {
	var syntaxErr *json.SyntaxError
	if errors.As(err, &syntaxErr) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "invalid character") || strings.Contains(msg, "unexpected end of JSON input") || strings.HasPrefix(msg, "json: ")
}

// sdkBaseURL converts a user-supplied provider base URL into the form the
// anthropic-sdk-go SDK expects. The SDK calls path "v1/messages" relative
// to the configured base, so we have to ensure the base is a host root
// (Anthropic's own deployments) or a vendor root that the SDK can append
// `v1/messages` to.
func sdkBaseURL(baseURL string) (string, error) {
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
	parsed.RawQuery = ""
	parsed.Fragment = ""

	path := strings.TrimRight(parsed.Path, "/")
	switch {
	case path == "":
		parsed.Path = "/"
	case strings.HasSuffix(path, "/v1/messages"):
		parsed.Path = strings.TrimSuffix(path, "/v1/messages") + "/"
	case strings.HasSuffix(path, "/v1"):
		// User specified the version themselves — drop it; SDK re-adds.
		parsed.Path = strings.TrimSuffix(path, "/v1") + "/"
	default:
		parsed.Path = path + "/"
	}
	return parsed.String(), nil
}

// messagesRequest is the on-wire shape we eventually marshal. We retain it
// as a package-private struct because the test suite captures the request
// body via json.Decode into messagesRequest to assert on its fields. It
// also drives toSDKParams below.
type messagesRequest struct {
	Model      string               `json:"model"`
	MaxTokens  int                  `json:"max_tokens"`
	Stream     bool                 `json:"stream,omitempty"`
	System     systemField          `json:"system,omitempty"`
	Messages   []anthropicMessage   `json:"messages"`
	Tools      []anthropicTool      `json:"tools,omitempty"`
	ToolChoice *anthropicToolChoice `json:"tool_choice,omitempty"`
}

// systemField is either an Anthropic-flavored array of {type:text,text:...}
// blocks (the SDK's preferred wire form) or a plain string (the form the
// original FreeCode client used). Tests inspect the field as a string;
// downstream callers don't care which shape goes on the wire.
type systemField string

func (s systemField) MarshalJSON() ([]byte, error) {
	if s == "" {
		return []byte("null"), nil
	}
	return json.Marshal(string(s))
}

func (s *systemField) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		*s = ""
		return nil
	}
	var str string
	if err := json.Unmarshal(trimmed, &str); err == nil {
		*s = systemField(str)
		return nil
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(trimmed, &blocks); err != nil {
		return err
	}
	var b strings.Builder
	for _, blk := range blocks {
		if blk.Type == "" || blk.Type == "text" {
			b.WriteString(blk.Text)
		}
	}
	*s = systemField(b.String())
	return nil
}

// anthropicToolChoice mirrors Anthropic's tool_choice shape: {"type": "auto"},
// {"type": "any"} (force one of the provided tools), {"type": "tool",
// "name": "..."} (force a specific tool), or {"type": "none"} (disable
// tools). We only emit it when the caller asks for a non-default —
// Anthropic defaults to "auto" otherwise.
type anthropicToolChoice struct {
	Type string `json:"type"`
	Name string `json:"name,omitempty"`
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
	// Content is the tool_result payload. Anthropic accepts a plain string
	// or an array of {type:text,text:...} blocks; the SDK marshals to the
	// array form. We normalize both shapes back to a string for callers
	// (and tests) that just want the textual content.
	Content toolResultContent `json:"content,omitempty"`
}

type toolResultContent string

func (c toolResultContent) MarshalJSON() ([]byte, error) {
	if c == "" {
		return []byte(`""`), nil
	}
	return json.Marshal(string(c))
}

func (c *toolResultContent) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		*c = ""
		return nil
	}
	var str string
	if err := json.Unmarshal(trimmed, &str); err == nil {
		*c = toolResultContent(str)
		return nil
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(trimmed, &blocks); err != nil {
		return err
	}
	var b strings.Builder
	for _, blk := range blocks {
		if blk.Type == "" || blk.Type == "text" {
			b.WriteString(blk.Text)
		}
	}
	*c = toolResultContent(b.String())
	return nil
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
			value.System = systemField(joinNonEmpty(string(value.System), messageText(message)))
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
				Content:   toolResultContent(messageText(message)),
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
	switch strings.ToLower(strings.TrimSpace(request.ToolChoice)) {
	case "", "auto":
		// default; omit field
	case "required", "any":
		// Anthropic spells "must call some tool" as "any".
		value.ToolChoice = &anthropicToolChoice{Type: "any"}
	case "none":
		value.ToolChoice = &anthropicToolChoice{Type: "none"}
	default:
		// Caller passed a tool name → force that tool.
		value.ToolChoice = &anthropicToolChoice{Type: "tool", Name: request.ToolChoice}
	}
	return value
}

// toSDKParams converts our internal messagesRequest into anthropic-sdk-go
// typed params. The SDK marshals these to the same JSON wire format.
func (r messagesRequest) toSDKParams() anthropic.MessageNewParams {
	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(r.Model),
		MaxTokens: int64(r.MaxTokens),
	}
	if r.System != "" {
		params.System = []anthropic.TextBlockParam{{Text: string(r.System)}}
	}
	for _, msg := range r.Messages {
		mp := anthropic.MessageParam{
			Role: anthropic.MessageParamRole(msg.Role),
		}
		for _, block := range msg.Content {
			mp.Content = append(mp.Content, blockToParam(block))
		}
		params.Messages = append(params.Messages, mp)
	}
	for _, tool := range r.Tools {
		schema := anthropic.ToolInputSchemaParam{}
		if tool.InputSchema != nil {
			if props, ok := tool.InputSchema["properties"]; ok {
				schema.Properties = props
			}
			schema.Required = requiredStrings(tool.InputSchema["required"])
			extras := map[string]any{}
			for k, v := range tool.InputSchema {
				if k == "type" || k == "properties" || k == "required" {
					continue
				}
				extras[k] = v
			}
			if len(extras) > 0 {
				schema.ExtraFields = extras
			}
		}
		params.Tools = append(params.Tools, anthropic.ToolUnionParam{
			OfTool: &anthropic.ToolParam{
				Name:        tool.Name,
				Description: param.NewOpt(tool.Description),
				InputSchema: schema,
			},
		})
	}
	if r.ToolChoice != nil {
		switch r.ToolChoice.Type {
		case "auto":
			params.ToolChoice = anthropic.ToolChoiceUnionParam{OfAuto: &anthropic.ToolChoiceAutoParam{}}
		case "any":
			params.ToolChoice = anthropic.ToolChoiceUnionParam{OfAny: &anthropic.ToolChoiceAnyParam{}}
		case "tool":
			params.ToolChoice = anthropic.ToolChoiceParamOfTool(r.ToolChoice.Name)
		case "none":
			params.ToolChoice = anthropic.ToolChoiceUnionParam{OfNone: &anthropic.ToolChoiceNoneParam{}}
		}
	}
	return params
}

func blockToParam(block contentBlock) anthropic.ContentBlockParamUnion {
	switch block.Type {
	case "text":
		return anthropic.NewTextBlock(block.Text)
	case "tool_use":
		var input any
		if len(block.Input) > 0 {
			_ = json.Unmarshal(block.Input, &input)
		}
		return anthropic.NewToolUseBlock(block.ID, input, block.Name)
	case "tool_result":
		result := anthropic.NewToolResultBlock(block.ToolUseID)
		if block.Content != "" && result.OfToolResult != nil {
			result.OfToolResult.Content = []anthropic.ToolResultBlockParamContentUnion{{
				OfText: &anthropic.TextBlockParam{Text: string(block.Content)},
			}}
		}
		return result
	}
	return anthropic.ContentBlockParamUnion{}
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

// requiredStrings coerces a JSON-schema "required" field into the
// []string shape the Anthropic SDK expects. The previous implementation
// only handled []string values, so tools whose InputSchema was decoded
// from JSON (where slices arrive as []any) silently dropped the field
// and let invalid arg combinations through. Accept both shapes and
// coerce non-string elements via fmt.Sprint as a last resort.
func requiredStrings(value any) []string {
	switch v := value.(type) {
	case nil:
		return nil
	case []string:
		return append([]string(nil), v...)
	case []any:
		out := make([]string, 0, len(v))
		for _, raw := range v {
			if s, ok := raw.(string); ok {
				out = append(out, s)
				continue
			}
			out = append(out, fmt.Sprint(raw))
		}
		return out
	default:
		return nil
	}
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

func (e *ClientError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// messagesStream is a tiny alias so internal functions can take the
// concrete generic stream type without re-stating it everywhere.
type messagesStream = ssestream.Stream[anthropic.MessageStreamEventUnion]
