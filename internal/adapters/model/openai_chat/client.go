// Package openai_chat speaks the OpenAI-compatible /v1/chat/completions
// protocol via the official openai-go SDK. The SDK handles SSE parsing,
// retries, base-URL routing and tool-call accumulation; the wrapper here
// adds FreeCode-specific concerns: secret resolution at request time,
// the GLM message-normalization fix, and translation between
// model.Request/model.Event and the SDK's typed parameters.
package openai_chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/packages/ssestream"
	"github.com/openai/openai-go/v3/shared"

	"github.com/Luew2/FreeCode/internal/adapters/model/transport"
	"github.com/Luew2/FreeCode/internal/core/model"
	"github.com/Luew2/FreeCode/internal/ports"
)

var _ ports.ModelClient = (*Client)(nil)

// Once-only registration of a tolerant SSE decoder. The SDK's stock decoder
// dispatches an event for every blank line, even when no `data:` field has
// been seen since the last dispatch — that turns the very common "comment
// then blank line" provider keepalive pattern into an empty-payload
// json.Unmarshal, which fails. The decoder here only dispatches when at
// least one data line has been seen, matching the SSE spec.
var registerTolerantDecoderOnce sync.Once

func registerTolerantDecoder() {
	registerTolerantDecoderOnce.Do(func() {
		ssestream.RegisterDecoder("text/event-stream", newTolerantSSEDecoder)
	})
}

// Client wraps the openai-go SDK with the same Stream(...) signature the
// orchestrator expects. It is safe for concurrent use; per-request state
// lives entirely inside Stream.
type Client struct {
	provider model.Provider
	endpoint string
	baseURL  string
	secrets  ports.SecretStore
	client   *http.Client
	policy   transport.RetryPolicy
}

// NewClient builds a client. A nil http.Client falls back to a streaming-safe
// default (no overall response timeout, but bounded connect/handshake/header
// phases) shared across requests.
func NewClient(provider model.Provider, secrets ports.SecretStore, client *http.Client) (*Client, error) {
	endpoint, err := ChatCompletionsEndpoint(provider.BaseURL)
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
	registerTolerantDecoder()
	return &Client{
		provider: provider,
		endpoint: endpoint,
		baseURL:  baseURL,
		secrets:  secrets,
		client:   client,
		policy:   transport.DefaultRetryPolicy(),
	}, nil
}

// SetRetryPolicy overrides the retry policy. The openai-go SDK only exposes
// MaxAttempts via WithMaxRetries; the additional knobs are accepted for
// signature compatibility but ignored. Tests still rely on being able to
// dial back attempts without changing the public surface.
func (c *Client) SetRetryPolicy(policy transport.RetryPolicy) {
	if c == nil {
		return
	}
	c.policy = policy
}

func (c *Client) Stream(ctx context.Context, request model.Request) (<-chan model.Event, error) {
	if c == nil {
		return nil, errors.New("openai chat client is nil")
	}

	chatReq, err := toChatRequest(c.provider, request)
	if err != nil {
		return nil, &ClientError{Endpoint: c.endpoint, Err: err}
	}

	apiKey, err := c.resolveAPIKey(ctx)
	if err != nil {
		return nil, &ClientError{Endpoint: c.endpoint, Err: err}
	}

	opts := c.buildOptions(apiKey, request.Stream)
	params, err := chatReq.toSDKParams()
	if err != nil {
		return nil, &ClientError{Endpoint: c.endpoint, Err: err}
	}

	sdk := openai.NewClient(opts...)
	if request.Stream {
		stream := sdk.Chat.Completions.NewStreaming(ctx, params)
		// The SDK executes the underlying HTTP request inside NewStreaming.
		// If that fails (4xx/5xx after retries, transport error, context
		// cancellation), the error is parked on the stream. Surface it
		// synchronously from Stream() so callers can short-circuit before
		// waiting on the channel.
		if streamErr := stream.Err(); streamErr != nil {
			_ = stream.Close()
			return nil, c.toClientError(streamErr)
		}
		events := make(chan model.Event)
		go c.readStream(stream, events)
		return events, nil
	}

	completion, err := sdk.Chat.Completions.New(ctx, params)
	if err != nil {
		return nil, c.toClientError(err)
	}
	events := make(chan model.Event)
	go c.readNonStream(completion, events)
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
		// Some compat servers (vLLM, llama.cpp's server) reject streaming
		// requests that don't advertise text/event-stream. The OpenAI API
		// itself ignores the override.
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

func (c *Client) readStream(stream *openaiStream, events chan<- model.Event) {
	defer close(events)
	defer stream.Close()

	events <- model.Event{Type: model.EventStarted}
	acc := openai.ChatCompletionAccumulator{}
	seenChunk := false

	for stream.Next() {
		seenChunk = true
		chunk := stream.Current()
		acc.AddChunk(chunk)

		// Some providers stream a JSON envelope with an `error` field
		// instead of using HTTP status codes. The SDK's ssestream layer
		// will already surface those as a *StreamError on stream.Err(),
		// but a few compat providers nest the error inside an otherwise
		// normal chunk. Detect that here.
		if rawErr := extractInlineError(chunk.RawJSON()); rawErr != "" {
			events <- model.Event{Type: model.EventError, Error: "provider error: " + rawErr}
			return
		}

		for _, choice := range chunk.Choices {
			if choice.Delta.Content != "" {
				events <- model.Event{Type: model.EventTextDelta, Text: choice.Delta.Content}
			}
		}
		if chunk.Usage.TotalTokens > 0 || chunk.Usage.PromptTokens > 0 || chunk.Usage.CompletionTokens > 0 {
			events <- model.Event{Type: model.EventUsage, Usage: &model.Usage{
				InputTokens:  int(chunk.Usage.PromptTokens),
				OutputTokens: int(chunk.Usage.CompletionTokens),
				TotalTokens:  int(chunk.Usage.TotalTokens),
			}}
		}
	}
	if err := stream.Err(); err != nil {
		events <- model.Event{Type: model.EventError, Error: classifyStreamError(err)}
		return
	}
	if !seenChunk {
		events <- model.Event{Type: model.EventError, Error: "stream response did not contain data frames"}
		return
	}

	for _, choice := range acc.Choices {
		for _, call := range choice.Message.ToolCalls {
			if call.ID == "" && call.Function.Name == "" && call.Function.Arguments == "" {
				continue
			}
			events <- model.Event{
				Type: model.EventToolCall,
				ToolCall: &model.ToolCall{
					ID:        call.ID,
					Name:      call.Function.Name,
					Arguments: []byte(call.Function.Arguments),
				},
			}
		}
	}
	events <- model.Event{Type: model.EventCompleted}
}

func (c *Client) readNonStream(completion *openai.ChatCompletion, events chan<- model.Event) {
	defer close(events)
	events <- model.Event{Type: model.EventStarted}

	// Some OpenAI-compatible providers return HTTP 200 with an `error`
	// envelope in the JSON body instead of a non-2xx status. The SDK
	// considers the request successful (status was 200), so we have to
	// detect that ourselves.
	if rawErr := extractInlineError(completion.RawJSON()); rawErr != "" {
		events <- model.Event{Type: model.EventError, Error: "provider error: " + rawErr}
		return
	}

	for _, choice := range completion.Choices {
		if choice.Message.Content != "" {
			events <- model.Event{Type: model.EventTextDelta, Text: choice.Message.Content}
		}
		for _, call := range choice.Message.ToolCalls {
			fn := call.Function
			events <- model.Event{
				Type: model.EventToolCall,
				ToolCall: &model.ToolCall{
					ID:        call.ID,
					Name:      fn.Name,
					Arguments: []byte(fn.Arguments),
				},
			}
		}
	}
	if completion.Usage.TotalTokens > 0 || completion.Usage.PromptTokens > 0 || completion.Usage.CompletionTokens > 0 {
		events <- model.Event{Type: model.EventUsage, Usage: &model.Usage{
			InputTokens:  int(completion.Usage.PromptTokens),
			OutputTokens: int(completion.Usage.CompletionTokens),
			TotalTokens:  int(completion.Usage.TotalTokens),
		}}
	}
	events <- model.Event{Type: model.EventCompleted}
}

// extractInlineError returns the human-readable error message from a
// provider-supplied `{"error": {...}}` envelope, or "" if no such
// envelope is present.
func extractInlineError(rawJSON string) string {
	if rawJSON == "" {
		return ""
	}
	var envelope struct {
		Error *struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(rawJSON), &envelope); err != nil {
		return ""
	}
	if envelope.Error == nil {
		return ""
	}
	if envelope.Error.Message != "" {
		return envelope.Error.Message
	}
	if envelope.Error.Type != "" {
		return envelope.Error.Type
	}
	return ""
}

// toClientError maps an error from the openai-go SDK back into the
// ClientError shape the orchestrator and tests expect ("openai-chat
// endpoint <URL> returned status N: <body>" for HTTP failures, or
// "openai-chat endpoint <URL> failed: <err>" otherwise).
func (c *Client) toClientError(err error) error {
	if err == nil {
		return nil
	}
	var apiErr *openai.Error
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

// classifyStreamError translates errors raised by the openai-go SDK and its
// SSE decoder into the strings the existing tests assert on. We preserve
// "provider error: ..." for upstream streamed-error frames and "invalid
// streamed chat completion JSON: ..." for malformed payloads.
func classifyStreamError(err error) string {
	if err == nil {
		return ""
	}
	// ssestream wraps in-band errors as "received error while streaming: ...".
	// Reshape into our prior "provider error: ..." prefix so downstream
	// UI/tests stay stable.
	var streamErr *ssestream.StreamError
	if errors.As(err, &streamErr) {
		return "provider error: " + extractProviderMessage(streamErr.Message)
	}
	msg := err.Error()
	if strings.HasPrefix(msg, "received error while streaming: ") {
		body := strings.TrimPrefix(msg, "received error while streaming: ")
		return "provider error: " + extractProviderMessage(body)
	}
	if isJSONParseError(err) {
		return "invalid streamed chat completion JSON: " + msg
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
	var unmarshalErr *json.UnmarshalTypeError
	if errors.As(err, &unmarshalErr) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "invalid character") || strings.Contains(msg, "unexpected end of JSON input") || strings.HasPrefix(msg, "json: ")
}

// ClientError wraps any failure from the chat completions endpoint with the
// URL we were talking to. We keep StatusCode + Body separate from Err so
// callers can switch on them.
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

// sdkBaseURL converts a user-supplied provider base URL into the form the
// openai-go SDK expects. The SDK calls path "chat/completions" relative to
// the configured base, so we have to ensure the base ends with a `/v1/`
// segment (or whatever versioned root the provider exposes).
//
// Existing FreeCode config formats that we have to keep accepting:
//   - "https://api.openai.com/v1"               → "https://api.openai.com/v1/"
//   - "https://api.openai.com/"                 → "https://api.openai.com/v1/"
//   - "https://api.openai.com"                  → "https://api.openai.com/v1/"
//   - "https://api.openai.com/v1/chat/completions" (rare; advanced) →
//     "https://api.openai.com/v1/" (drop the path; the SDK appends it)
//   - "https://api.z.ai/api/coding/paas/v4"     → "https://api.z.ai/api/coding/paas/v4/"
//     (Z.ai uses a non-standard prefix; preserve it verbatim.)
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
		parsed.Path = "/v1/"
	case strings.HasSuffix(path, "/chat/completions"):
		parsed.Path = strings.TrimSuffix(path, "/chat/completions") + "/"
	default:
		// Auto-inject /v1 only when the user-supplied base has no obvious
		// version segment. Otherwise keep their path verbatim.
		if hasVersionSegment(path) {
			parsed.Path = path + "/"
		} else {
			parsed.Path = path + "/v1/"
		}
	}
	return parsed.String(), nil
}

func hasVersionSegment(path string) bool {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 0 {
		return false
	}
	last := parts[len(parts)-1]
	if len(last) < 2 || last[0] != 'v' {
		return false
	}
	for _, ch := range last[1:] {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	return true
}

// chatRequest is the on-wire shape we eventually marshal. We retain it as a
// package-private struct because the test suite captures the request body via
// json.Decode into chatRequest to assert on its fields. The struct also
// drives toSDKParams below; keeping a single intermediate representation
// makes that conversion testable in isolation.
type chatRequest struct {
	Model         string             `json:"model"`
	Messages      []chatMessage      `json:"messages"`
	Tools         []chatTool         `json:"tools,omitempty"`
	ToolChoice    string             `json:"tool_choice,omitempty"`
	MaxTokens     int                `json:"max_tokens,omitempty"`
	Temperature   *float64           `json:"temperature,omitempty"`
	Stream        bool               `json:"stream"`
	StreamOptions *chatStreamOptions `json:"stream_options,omitempty"`
}

type chatStreamOptions struct {
	IncludeUsage bool `json:"include_usage,omitempty"`
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

// chatToolCall mirrors the OpenAI on-wire shape and is only used for test
// assertion deserialization.
type chatToolCall struct {
	Index    *int             `json:"index,omitempty"`
	ID       string           `json:"id,omitempty"`
	Type     string           `json:"type,omitempty"`
	Function chatToolFunction `json:"function"`
}

type chatMessage struct {
	Role       string         `json:"role"`
	Name       string         `json:"name,omitempty"`
	Content    string         `json:"content,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	ToolCalls  []chatToolCall `json:"tool_calls,omitempty"`
}

func toChatRequest(provider model.Provider, request model.Request) (chatRequest, error) {
	modelID := string(request.Model.ID)
	if modelID == "" {
		modelID = string(provider.DefaultModel)
	}
	if modelID == "" {
		return chatRequest{}, errors.New("model id is empty")
	}

	chatMessages := normalizeMessages(request.Messages)

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

	chat := chatRequest{
		Model:       modelID,
		Messages:    chatMessages,
		Tools:       tools,
		MaxTokens:   request.MaxOutputTokens,
		Temperature: request.Temperature,
		Stream:      request.Stream,
	}
	// Explicit tool_choice="auto" is OpenAI's default but several
	// OpenAI-compatible providers (notably Z.ai's GLM endpoint and some
	// OpenRouter routes) only honor tool calls when the field is present.
	if len(tools) > 0 {
		chat.ToolChoice = "auto"
	}
	if request.ToolChoice != "" {
		chat.ToolChoice = request.ToolChoice
	}
	if request.Stream {
		chat.StreamOptions = &chatStreamOptions{IncludeUsage: true}
	}
	return chat, nil
}

// toSDKParams converts our internal chatRequest into the openai-go typed
// params. The SDK ultimately marshals these to the same JSON wire format.
func (r chatRequest) toSDKParams() (openai.ChatCompletionNewParams, error) {
	params := openai.ChatCompletionNewParams{
		Model: shared.ChatModel(r.Model),
	}
	for _, msg := range r.Messages {
		converted, err := toSDKMessage(msg)
		if err != nil {
			return openai.ChatCompletionNewParams{}, err
		}
		params.Messages = append(params.Messages, converted)
	}
	if len(r.Tools) > 0 {
		params.Tools = make([]openai.ChatCompletionToolUnionParam, 0, len(r.Tools))
		for _, tool := range r.Tools {
			params.Tools = append(params.Tools, openai.ChatCompletionFunctionTool(shared.FunctionDefinitionParam{
				Name:        tool.Function.Name,
				Description: openai.String(tool.Function.Description),
				Parameters:  openai.FunctionParameters(tool.Function.Parameters),
			}))
		}
	}
	if r.ToolChoice != "" {
		params.ToolChoice = toSDKToolChoice(r.ToolChoice)
	}
	if r.MaxTokens > 0 {
		params.MaxTokens = openai.Int(int64(r.MaxTokens))
	}
	if r.Temperature != nil {
		params.Temperature = openai.Float(*r.Temperature)
	}
	if r.StreamOptions != nil && r.StreamOptions.IncludeUsage {
		params.StreamOptions = openai.ChatCompletionStreamOptionsParam{
			IncludeUsage: openai.Bool(true),
		}
	}
	return params, nil
}

func toSDKMessage(msg chatMessage) (openai.ChatCompletionMessageParamUnion, error) {
	switch msg.Role {
	case "system", "developer":
		return openai.SystemMessage(msg.Content), nil
	case "user":
		return openai.UserMessage(msg.Content), nil
	case "assistant":
		assistant := openai.ChatCompletionAssistantMessageParam{}
		if msg.Content != "" {
			assistant.Content = openai.ChatCompletionAssistantMessageParamContentUnion{
				OfString: param.NewOpt(msg.Content),
			}
		}
		if len(msg.ToolCalls) > 0 {
			assistant.ToolCalls = make([]openai.ChatCompletionMessageToolCallUnionParam, 0, len(msg.ToolCalls))
			for _, call := range msg.ToolCalls {
				assistant.ToolCalls = append(assistant.ToolCalls, openai.ChatCompletionMessageToolCallUnionParam{
					OfFunction: &openai.ChatCompletionMessageFunctionToolCallParam{
						ID: call.ID,
						Function: openai.ChatCompletionMessageFunctionToolCallFunctionParam{
							Name:      call.Function.Name,
							Arguments: call.Function.Arguments,
						},
					},
				})
			}
		}
		return openai.ChatCompletionMessageParamUnion{OfAssistant: &assistant}, nil
	case "tool":
		return openai.ToolMessage(msg.Content, msg.ToolCallID), nil
	default:
		return openai.ChatCompletionMessageParamUnion{}, fmt.Errorf("unsupported role %q", msg.Role)
	}
}

func toSDKToolChoice(choice string) openai.ChatCompletionToolChoiceOptionUnionParam {
	normalized := strings.ToLower(strings.TrimSpace(choice))
	switch normalized {
	case "auto", "required", "none":
		return openai.ChatCompletionToolChoiceOptionUnionParam{
			OfAuto: param.NewOpt(normalized),
		}
	default:
		return openai.ChatCompletionToolChoiceOptionUnionParam{
			OfFunctionToolChoice: &openai.ChatCompletionNamedToolChoiceParam{
				Function: openai.ChatCompletionNamedToolChoiceFunctionParam{
					Name: choice,
				},
			},
		}
	}
}

// normalizeMessages collapses the prompt builder's [system, developer,
// developer (perms), developer (env), user, ...] shape into a form every
// OpenAI-compatible provider treats consistently:
//
//   - Leading system/developer messages collapse into a single "system"
//     message at the start. Many providers (Z.ai GLM, OpenRouter routes,
//     vLLM, llama.cpp's server) silently drop or de-prioritize messages
//     after the first system slot; the original layout caused tool-use
//     directives to be invisible to the model and the model would happily
//     reply with text instead of calling tools.
//   - Mid-stream developer messages (e.g. the orchestrator's
//     tool-followthrough nudge) are converted to "system" since OpenAI
//     accepts repeated system messages and most compat providers do too.
//   - Tool/assistant/user messages pass through unchanged.
//
// We deliberately do NOT preserve OpenAI's "developer" role: it is only
// distinct on o-series models and even there is treated as a stronger
// system prompt. Sending "system" universally is the safer default.
func normalizeMessages(messages []model.Message) []chatMessage {
	out := make([]chatMessage, 0, len(messages))
	var systemBuilder strings.Builder
	flushSystem := func() {
		if systemBuilder.Len() == 0 {
			return
		}
		out = append(out, chatMessage{
			Role:    "system",
			Content: strings.TrimSpace(systemBuilder.String()),
		})
		systemBuilder.Reset()
	}

	consumedLeading := false
	for _, message := range messages {
		isSystemLike := message.Role == model.RoleSystem || message.Role == model.RoleDeveloper

		if !consumedLeading && isSystemLike {
			text := textContent(message.Content)
			if strings.TrimSpace(text) == "" {
				continue
			}
			if systemBuilder.Len() > 0 {
				systemBuilder.WriteString("\n\n")
			}
			systemBuilder.WriteString(text)
			continue
		}
		if !consumedLeading {
			flushSystem()
			consumedLeading = true
		}

		role := string(message.Role)
		if message.Role == model.RoleDeveloper {
			role = "system"
		}
		out = append(out, chatMessage{
			Role:       role,
			Name:       message.Name,
			Content:    textContent(message.Content),
			ToolCallID: message.ToolCallID,
			ToolCalls:  toChatToolCalls(message.ToolCalls),
		})
	}
	flushSystem()
	return out
}

func toChatToolCalls(calls []model.ToolCall) []chatToolCall {
	if len(calls) == 0 {
		return nil
	}
	converted := make([]chatToolCall, 0, len(calls))
	for index, call := range calls {
		idx := index
		converted = append(converted, chatToolCall{
			Index: &idx,
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

// openaiStream is a tiny alias so internal functions can take the
// concrete generic stream type without re-stating it everywhere.
type openaiStream = ssestream.Stream[openai.ChatCompletionChunk]
