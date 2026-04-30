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
		go c.readStream(ctx, stream, events)
		return events, nil
	}

	completion, err := sdk.Chat.Completions.New(ctx, params)
	if err != nil {
		return nil, c.toClientError(err)
	}
	events := make(chan model.Event)
	go c.readNonStream(ctx, completion, events)
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

func (c *Client) readStream(ctx context.Context, stream *openaiStream, events chan<- model.Event) {
	defer close(events)
	defer stream.Close()

	if !emitModelEvent(ctx, events, model.Event{Type: model.EventStarted}) {
		return
	}
	acc := openai.ChatCompletionAccumulator{}
	// fallbackTools accumulates tool calls directly off each chunk's
	// Delta.ToolCalls regardless of whether the SDK's accumulator accepts
	// the chunk. The SDK rejects any chunk whose `id` does not match the
	// first chunk's id (streamaccumulator.go:104-109) — some compat
	// providers (Z.ai's GLM endpoint, certain OpenRouter routes, streaming
	// proxies) regenerate ids per chunk, which makes the SDK's accumulator
	// drop the entire stream's tool calls silently. Running our own
	// accumulator in parallel guarantees we never lose tool calls because
	// of an id-mismatch quirk.
	fallbackTools := newToolCallAccumulator()
	sdkAcceptedChunks := 0
	diag := &model.Diagnostics{}
	var lastChunkJSON string
	var firstContentfulChunkJSON string
	// Keep all raw chunks for forensic debugging. Capped at maxKeptChunks
	// pieces and maxRawTotal bytes so an out-of-control provider can't
	// blow up our session log.
	const maxKeptChunks = 60
	const maxRawTotal = 32 * 1024
	var capturedChunks []string
	capturedBytes := 0

	for stream.Next() {
		diag.ChunkCount++
		chunk := stream.Current()
		if acc.AddChunk(chunk) {
			sdkAcceptedChunks++
		}
		// Parse the raw JSON ourselves to recover wire-format fields the
		// openai-go SDK does not surface: reasoning_content (Z.ai's GLM,
		// DeepSeek, Qwen reasoning models), thinking (Anthropic-style on
		// some compat providers), function_call (legacy single-tool), and
		// tool_calls when they arrive in chunks the SDK accumulator
		// dropped. The smoking-gun symptom is finish_reason=tool_calls
		// with completion_tokens > 0 but our extracted text+tool_calls is
		// empty — those tokens lived in a field we did not read.
		if extra := parseExtendedDelta(chunk.RawJSON()); extra != nil {
			// Emit reasoning as a TextDelta with Reasoning=true so the
			// orchestrator/workbench can render it as a separate
			// "thinking" stream instead of folding it into the
			// user-facing assistant text. Folding caused the chat to
			// show reasoning concatenated to the final answer with no
			// separator, e.g.:
			//   "Let me check the file. Looks like there is a quote
			//    issue. Let me fix it..."
			// where the first sentence is reasoning and the rest is
			// the final reply.
			if extra.ReasoningContent != "" {
				diag.ReasoningTokens += approxTokens(extra.ReasoningContent)
				if !emitModelEvent(ctx, events, model.Event{Type: model.EventTextDelta, Text: extra.ReasoningContent, Reasoning: true}) {
					return
				}
			}
			if extra.Thinking != "" {
				diag.ReasoningTokens += approxTokens(extra.Thinking)
				if !emitModelEvent(ctx, events, model.Event{Type: model.EventTextDelta, Text: extra.Thinking, Reasoning: true}) {
					return
				}
			}
			if extra.LegacyFunctionCall != nil {
				fallbackTools.addLegacy(extra.LegacyFunctionCall)
			}
		}
		if raw := chunk.RawJSON(); raw != "" {
			lastChunkJSON = raw
			// Capture the first chunk whose choices[] is non-empty —
			// that's the chunk most likely to expose the wire format we
			// need to debug. Final chunks are usually just usage with
			// choices=[], which tells us nothing about the delta shape.
			if firstContentfulChunkJSON == "" && len(chunk.Choices) > 0 {
				firstContentfulChunkJSON = raw
			}
			if len(capturedChunks) < maxKeptChunks && capturedBytes+len(raw) <= maxRawTotal {
				capturedChunks = append(capturedChunks, raw)
				capturedBytes += len(raw)
			}
		}

		// Some providers stream a JSON envelope with an `error` field
		// instead of using HTTP status codes. The SDK's ssestream layer
		// will already surface those as a *StreamError on stream.Err(),
		// but a few compat providers nest the error inside an otherwise
		// normal chunk. Detect that here.
		if rawErr := extractInlineError(chunk.RawJSON()); rawErr != "" {
			emitModelEvent(ctx, events, model.Event{Type: model.EventError, Error: "provider error: " + rawErr})
			return
		}

		for _, choice := range chunk.Choices {
			if choice.Delta.Content != "" {
				diag.TextDeltaCount++
				if !emitModelEvent(ctx, events, model.Event{Type: model.EventTextDelta, Text: choice.Delta.Content}) {
					return
				}
			}
			if reason := string(choice.FinishReason); reason != "" {
				diag.FinishReason = reason
			}
			for _, call := range choice.Delta.ToolCalls {
				fallbackTools.add(call)
			}
		}
		if chunk.Usage.TotalTokens > 0 || chunk.Usage.PromptTokens > 0 || chunk.Usage.CompletionTokens > 0 {
			if !emitModelEvent(ctx, events, model.Event{Type: model.EventUsage, Usage: &model.Usage{
				InputTokens:  int(chunk.Usage.PromptTokens),
				OutputTokens: int(chunk.Usage.CompletionTokens),
				TotalTokens:  int(chunk.Usage.TotalTokens),
			}}) {
				return
			}
		}
	}
	if err := stream.Err(); err != nil {
		emitModelEvent(ctx, events, model.Event{Type: model.EventError, Error: classifyStreamError(err)})
		return
	}
	if diag.ChunkCount == 0 {
		emitModelEvent(ctx, events, model.Event{Type: model.EventError, Error: "stream response did not contain data frames"})
		return
	}
	if sdkAcceptedChunks < diag.ChunkCount {
		// Surface this in diagnostics — it's the most common cause of
		// "model said tool_calls but we got nothing" with compat providers.
		diag.RejectedChunks = diag.ChunkCount - sdkAcceptedChunks
	}

	// Always pick up finish_reason from the accumulated view when none of
	// the streamed deltas carried one (some providers only attach it to
	// the [DONE] marker). Done before tool-call emission so the diagnostic
	// is right regardless of which accumulator wins.
	for _, choice := range acc.Choices {
		if diag.FinishReason == "" {
			if reason := string(choice.FinishReason); reason != "" {
				diag.FinishReason = reason
			}
		}
	}
	// Choose the source for tool-call emission. Prefer the fallback
	// accumulator when:
	//   * the SDK rejected one or more chunks (envelope id rotation,
	//     mid-stream id flap, etc) — its view of the call is partial,
	//   * the SDK accumulated nothing at all — same root cause.
	// Otherwise prefer the SDK's view: it round-trips arguments through
	// partial JSON deltas correctly and is what most providers expect to
	// drive.
	useFallback := diag.RejectedChunks > 0
	emitted := false
	if !useFallback {
		for _, choice := range acc.Choices {
			for _, call := range choice.Message.ToolCalls {
				if call.ID == "" && call.Function.Name == "" && call.Function.Arguments == "" {
					diag.DroppedCalls++
					continue
				}
				diag.ToolCallCount++
				emitted = true
				if !emitModelEvent(ctx, events, model.Event{
					Type: model.EventToolCall,
					ToolCall: &model.ToolCall{
						ID:        call.ID,
						Name:      call.Function.Name,
						Arguments: []byte(call.Function.Arguments),
					},
				}) {
					return
				}
			}
		}
	}
	if !emitted {
		for _, call := range fallbackTools.complete() {
			if call.Name == "" && len(call.Arguments) == 0 {
				diag.DroppedCalls++
				continue
			}
			diag.ToolCallCount++
			diag.FallbackCalls++
			if !emitModelEvent(ctx, events, model.Event{Type: model.EventToolCall, ToolCall: &call}) {
				return
			}
		}
	}
	if diag.ChunkCount > 0 && len(lastChunkJSON) > 0 {
		// Cap at 2KB so a runaway provider can't blow up our log file.
		if len(lastChunkJSON) > 2048 {
			lastChunkJSON = lastChunkJSON[:2048] + "...[truncated]"
		}
		diag.RawLastChunk = lastChunkJSON
	}
	if firstContentfulChunkJSON != "" && firstContentfulChunkJSON != lastChunkJSON {
		if len(firstContentfulChunkJSON) > 2048 {
			firstContentfulChunkJSON = firstContentfulChunkJSON[:2048] + "...[truncated]"
		}
		diag.RawFirstChunk = firstContentfulChunkJSON
	}
	// Capture the provider's own completion-token tally so the orchestrator
	// can spot the smoking-gun "model produced N tokens but we captured 0"
	// pattern that says we're missing a wire-format field.
	if acc.Usage.CompletionTokens > 0 {
		diag.CompletionTokens = int(acc.Usage.CompletionTokens)
	}
	// Attach the captured raw chunks to diagnostics when debug mode is on
	// (always — the user explicitly opted in to verbose logging) or when
	// we suspect we missed content (the "model produced tokens but we got
	// nothing" case, or when the SDK silently rejected chunks). On normal
	// turns keeping all chunks would balloon the log file for no benefit.
	suspectMissedContent := (diag.CompletionTokens > 0 && diag.TextDeltaCount == 0 && diag.ToolCallCount == 0) || diag.RejectedChunks > 0
	if model.Debug() || suspectMissedContent {
		diag.RawChunks = capturedChunks
	}
	emitModelEvent(ctx, events, model.Event{Type: model.EventCompleted, Diagnostics: diag})
}

func (c *Client) readNonStream(ctx context.Context, completion *openai.ChatCompletion, events chan<- model.Event) {
	defer close(events)
	if !emitModelEvent(ctx, events, model.Event{Type: model.EventStarted}) {
		return
	}

	// Some OpenAI-compatible providers return HTTP 200 with an `error`
	// envelope in the JSON body instead of a non-2xx status. The SDK
	// considers the request successful (status was 200), so we have to
	// detect that ourselves.
	if rawErr := extractInlineError(completion.RawJSON()); rawErr != "" {
		emitModelEvent(ctx, events, model.Event{Type: model.EventError, Error: "provider error: " + rawErr})
		return
	}

	diag := &model.Diagnostics{ChunkCount: 1}
	for _, choice := range completion.Choices {
		if reason := string(choice.FinishReason); reason != "" {
			diag.FinishReason = reason
		}
		if choice.Message.Content != "" {
			diag.TextDeltaCount = 1
			if !emitModelEvent(ctx, events, model.Event{Type: model.EventTextDelta, Text: choice.Message.Content}) {
				return
			}
		}
		for _, call := range choice.Message.ToolCalls {
			fn := call.Function
			if call.ID == "" && fn.Name == "" && fn.Arguments == "" {
				diag.DroppedCalls++
				continue
			}
			diag.ToolCallCount++
			if !emitModelEvent(ctx, events, model.Event{
				Type: model.EventToolCall,
				ToolCall: &model.ToolCall{
					ID:        call.ID,
					Name:      fn.Name,
					Arguments: []byte(fn.Arguments),
				},
			}) {
				return
			}
		}
	}
	if completion.Usage.TotalTokens > 0 || completion.Usage.PromptTokens > 0 || completion.Usage.CompletionTokens > 0 {
		if !emitModelEvent(ctx, events, model.Event{Type: model.EventUsage, Usage: &model.Usage{
			InputTokens:  int(completion.Usage.PromptTokens),
			OutputTokens: int(completion.Usage.CompletionTokens),
			TotalTokens:  int(completion.Usage.TotalTokens),
		}}) {
			return
		}
	}
	if raw := completion.RawJSON(); raw != "" {
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

// toolCallAccumulator stitches per-chunk tool_call deltas back into
// complete ToolCall records. We run this in parallel with the SDK's own
// accumulator so we recover tool calls even when the SDK silently rejects
// chunks (most commonly because of an id mismatch between chunks — a quirk
// of certain compat providers and streaming proxies). Slots are keyed by
// (index, id) so simultaneous parallel tool calls don't collide.
type toolCallAccumulator struct {
	bySlot map[accumulatorKey]*partialToolCall
	order  []accumulatorKey
}

type accumulatorKey struct {
	index int
	id    string
}

type partialToolCall struct {
	id        string
	name      string
	arguments strings.Builder
}

func newToolCallAccumulator() *toolCallAccumulator {
	return &toolCallAccumulator{bySlot: map[accumulatorKey]*partialToolCall{}}
}

func (a *toolCallAccumulator) add(call openai.ChatCompletionChunkChoiceDeltaToolCall) {
	key := a.keyFor(call)
	partial := a.bySlot[key]
	if partial == nil {
		partial = &partialToolCall{}
		a.bySlot[key] = partial
		a.order = append(a.order, key)
	}
	// Keep the FIRST non-empty id we ever see for this slot. Some compat
	// providers regenerate the id on every chunk; locking the id once
	// stops the emitted ToolCall.ID from flapping based on which chunk
	// happened to be processed last.
	if call.ID != "" && partial.id == "" {
		partial.id = call.ID
	}
	if call.Function.Name != "" {
		partial.name = call.Function.Name
	}
	if call.Function.Arguments != "" {
		partial.arguments.WriteString(call.Function.Arguments)
	}
}

// keyFor picks (or invents) the accumulator slot for a streamed
// tool_call delta. The cardinal rule: the chunk's `index` is the
// authoritative grouping key, NOT its id. Some compat providers
// (vLLM, certain OpenAI proxies) regenerate the tool_call id on every
// chunk for a single call — preferring the id over the index would
// split one logical call into multiple partial entries (e.g. a
// name-only entry and an arguments-only entry, neither well-formed).
//
// Decision rule:
//  1. If we already have a slot with this index, ALWAYS reuse it
//     (regardless of whether the chunk's id matches the slot's, or even
//     whether the chunk has an id at all). The first non-empty id we
//     see wins for the slot's id field; later differing ids are
//     ignored for keying purposes but harmless because the slot's
//     stored id is set only once.
//  2. Otherwise the index is genuinely new: open a fresh slot keyed
//     on (index, id). The id-in-key disambiguates only when the same
//     index hasn't been opened yet — a non-issue in normal streams.
func (a *toolCallAccumulator) keyFor(call openai.ChatCompletionChunkChoiceDeltaToolCall) accumulatorKey {
	idx := int(call.Index)
	if idx < 0 {
		idx = -1
	}
	// Look for an existing slot with this index first; the index is the
	// stable identifier across chunks for both well-behaved providers
	// (id only in the opening chunk) and id-quirk providers (different
	// id on every chunk).
	for _, key := range a.order {
		if key.index == idx {
			return key
		}
	}
	// Index is new: open a fresh slot. Preserve the id in the key to
	// avoid colliding with a later "no id, reuse by index" lookup that
	// shouldn't be possible at this point but is cheap insurance.
	if call.ID != "" {
		return accumulatorKey{index: idx, id: call.ID}
	}
	return accumulatorKey{index: idx}
}

func (a *toolCallAccumulator) complete() []model.ToolCall {
	out := make([]model.ToolCall, 0, len(a.order))
	for _, key := range a.order {
		partial := a.bySlot[key]
		if partial == nil {
			continue
		}
		if partial.id == "" && partial.name == "" && partial.arguments.Len() == 0 {
			continue
		}
		out = append(out, model.ToolCall{
			ID:        partial.id,
			Name:      partial.name,
			Arguments: []byte(partial.arguments.String()),
		})
	}
	return out
}

// extendedDelta carries fields the openai-go SDK does not natively expose
// but compat providers commonly use to stream content. Captured via a raw
// JSON re-parse of each chunk so we can recover content the SDK silently
// drops because the field names are non-standard.
type extendedDelta struct {
	ReasoningContent   string
	Thinking           string
	LegacyFunctionCall *legacyFunctionCall
}

type legacyFunctionCall struct {
	Name      string
	Arguments string
}

// parseExtendedDelta returns nil if the chunk has no extended fields we
// care about. The "happy path" (a chunk with only the SDK-known fields)
// avoids any allocation past the unmarshal of a small struct.
func parseExtendedDelta(raw string) *extendedDelta {
	if raw == "" {
		return nil
	}
	var envelope struct {
		Choices []struct {
			Delta struct {
				// Different compat providers spell the reasoning channel
				// differently. Z.AI's GLM-5.1 uses bare "reasoning"
				// (confirmed via raw chunk dump), DeepSeek and many
				// OpenAI-compat reasoning models use "reasoning_content",
				// Anthropic-style proxies sometimes use "thinking". We
				// accept all three; whichever is non-empty is the
				// reasoning stream for this provider.
				Reasoning        string `json:"reasoning"`
				ReasoningContent string `json:"reasoning_content"`
				Thinking         string `json:"thinking"`
				FunctionCall     *struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function_call"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		return nil
	}
	if len(envelope.Choices) == 0 {
		return nil
	}
	delta := envelope.Choices[0].Delta
	// Concatenate any non-empty reasoning fields rather than picking one.
	// In practice we expect at most one to be populated per chunk, but if a
	// provider duplicates content across "reasoning" and "reasoning_content"
	// (some proxies do, some are in transition between names) joining them
	// is strictly safer than picking and losing the other.
	reasoning := delta.Reasoning + delta.ReasoningContent
	if reasoning == "" && delta.Thinking == "" && delta.FunctionCall == nil {
		return nil
	}
	out := &extendedDelta{
		ReasoningContent: reasoning,
		Thinking:         delta.Thinking,
	}
	if delta.FunctionCall != nil {
		out.LegacyFunctionCall = &legacyFunctionCall{
			Name:      delta.FunctionCall.Name,
			Arguments: delta.FunctionCall.Arguments,
		}
	}
	return out
}

// approxTokens estimates token count for a text string. Used only for
// diagnostics; the real provider counts win for billing.
func approxTokens(s string) int {
	if s == "" {
		return 0
	}
	return (len(s) + 3) / 4
}

// addLegacy folds a streamed legacy `function_call` (single-call,
// pre-tool_calls API) into the same accumulator as modern tool_calls so
// the orchestrator does not need to know the difference. Treats
// successive deltas as additive (the same way openai-go would for
// tool_calls).
func (a *toolCallAccumulator) addLegacy(call *legacyFunctionCall) {
	if call == nil {
		return
	}
	key := accumulatorKey{index: 0, id: "legacy_function_call"}
	partial := a.bySlot[key]
	if partial == nil {
		partial = &partialToolCall{id: "legacy_function_call"}
		a.bySlot[key] = partial
		a.order = append(a.order, key)
	}
	if call.Name != "" {
		partial.name = call.Name
	}
	if call.Arguments != "" {
		partial.arguments.WriteString(call.Arguments)
	}
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
