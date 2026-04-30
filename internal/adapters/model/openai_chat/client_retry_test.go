package openai_chat

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Luew2/FreeCode/internal/adapters/model/transport"
	"github.com/Luew2/FreeCode/internal/core/model"
)

func TestClientRetriesTransient5xxThenSucceeds(t *testing.T) {
	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		count := atomic.AddInt32(&hits, 1)
		if count < 3 {
			http.Error(w, "upstream blip", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSEJSON(t, w, map[string]any{
			"choices": []any{
				map[string]any{"delta": map[string]any{"content": "hello"}},
			},
		})
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	client := newTestClient(t, server, model.Provider{ID: "local", BaseURL: server.URL, DefaultModel: "coder"}, nil)
	client.SetRetryPolicy(transport.RetryPolicy{MaxAttempts: 5, InitialDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond, Multiplier: 2})

	events, err := client.Stream(context.Background(), model.Request{
		Model:    model.NewRef("local", "coder"),
		Messages: []model.Message{model.TextMessage(model.RoleUser, "ping")},
		Stream:   true,
	})
	if err != nil {
		t.Fatalf("Stream returned error: %v", err)
	}
	gotEvents := collectEvents(t, events)
	assertEventTypes(t, gotEvents, []model.EventType{
		model.EventStarted,
		model.EventTextDelta,
		model.EventCompleted,
	})
	if got := atomic.LoadInt32(&hits); got != 3 {
		t.Fatalf("attempts = %d, want 3", got)
	}
}

func TestClientDoesNotRetryOnNonRetryable4xx(t *testing.T) {
	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		http.Error(w, "bad request", http.StatusBadRequest)
	}))
	defer server.Close()

	client := newTestClient(t, server, model.Provider{ID: "local", BaseURL: server.URL, DefaultModel: "coder"}, nil)
	client.SetRetryPolicy(transport.RetryPolicy{MaxAttempts: 5, InitialDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond, Multiplier: 2})

	_, err := client.Stream(context.Background(), model.Request{
		Model:    model.NewRef("local", "coder"),
		Messages: []model.Message{model.TextMessage(model.RoleUser, "ping")},
		Stream:   true,
	})
	if err == nil {
		t.Fatal("Stream returned nil error for 400")
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("attempts = %d, want 1", got)
	}
}

func TestClientStreamsToolCallsWhenIndexOmittedOnFollowupDeltas(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// Anthropic-via-OpenAI-compat and some local providers omit `index`
		// on follow-up deltas. Without slot matching by id, the accumulator
		// would split this into two zero-arg ToolCall events.
		writeSSEJSON(t, w, map[string]any{
			"choices": []any{
				map[string]any{
					"delta": map[string]any{
						"tool_calls": []any{
							map[string]any{
								"index": 0,
								"id":    "call_1",
								"function": map[string]any{
									"name":      "read_file",
									"arguments": "{\"path\"",
								},
							},
						},
					},
				},
			},
		})
		writeSSEJSON(t, w, map[string]any{
			"choices": []any{
				map[string]any{
					"delta": map[string]any{
						"tool_calls": []any{
							map[string]any{
								// no index, no id
								"function": map[string]any{
									"arguments": ":\"README.md\"}",
								},
							},
						},
					},
				},
			},
		})
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	client := newTestClient(t, server, model.Provider{ID: "local", BaseURL: server.URL, DefaultModel: "coder"}, nil)
	events, err := client.Stream(context.Background(), model.Request{
		Model:    model.NewRef("local", "coder"),
		Messages: []model.Message{model.TextMessage(model.RoleUser, "read it")},
		Stream:   true,
	})
	if err != nil {
		t.Fatalf("Stream returned error: %v", err)
	}
	gotEvents := collectEvents(t, events)
	assertEventTypes(t, gotEvents, []model.EventType{
		model.EventStarted,
		model.EventToolCall,
		model.EventCompleted,
	})
	if call := gotEvents[1].ToolCall; call == nil || call.ID != "call_1" || string(call.Arguments) != `{"path":"README.md"}` {
		t.Fatalf("tool call = %#v, want call_1 with full arguments", gotEvents[1].ToolCall)
	}
}

// TestClientMergesToolCallChunksWhenProviderRotatesID covers a class of
// compat providers (some vLLM front-ends, certain proxies) that change the
// streamed tool_call `id` between chunks within ONE logical call. The
// fallback accumulator must key on `index` rather than `(index, id)` so it
// doesn't split one call into two partial entries — which would either
// emit half-formed ToolCalls (no args) or drop the call entirely.
//
// To trigger the fallback path (rather than the SDK's own accumulator we
// run alongside), we also rotate the outer chat-completion `id` per chunk
// — the SDK rejects mismatched chunk envelopes (streamaccumulator.go:108),
// so all calls must come from our fallback. This is the smoking-gun case
// the original bug report describes.
//
// Deterministic id rule: the FIRST non-empty id seen for a given index
// wins. Providers that flap the id are signaling proxy noise; locking the
// id stops the emitted ToolCall.ID from depending on which chunk happened
// to be the last to land.
func TestClientMergesToolCallChunksWhenProviderRotatesID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// First chunk: outer id "envelope-1", opens tool index 0 with
		// id "a" and the function name.
		writeSSEJSON(t, w, map[string]any{
			"id": "envelope-1",
			"choices": []any{
				map[string]any{
					"delta": map[string]any{
						"tool_calls": []any{
							map[string]any{
								"index": 0,
								"id":    "a",
								"function": map[string]any{
									"name": "foo",
								},
							},
						},
					},
				},
			},
		})
		// Second chunk: outer id "envelope-2" (forces SDK acc to reject
		// this chunk, exercising the fallback), same tool index, DIFFERENT
		// tool id, carries the arguments. Without index-first keying in
		// the fallback this would land in a separate slot and the merged
		// call would be lost.
		writeSSEJSON(t, w, map[string]any{
			"id": "envelope-2",
			"choices": []any{
				map[string]any{
					"delta": map[string]any{
						"tool_calls": []any{
							map[string]any{
								"index": 0,
								"id":    "b",
								"function": map[string]any{
									"arguments": `{"path":"README.md"}`,
								},
							},
						},
					},
				},
			},
		})
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	client := newTestClient(t, server, model.Provider{ID: "local", BaseURL: server.URL, DefaultModel: "coder"}, nil)
	events, err := client.Stream(context.Background(), model.Request{
		Model:    model.NewRef("local", "coder"),
		Messages: []model.Message{model.TextMessage(model.RoleUser, "merge it")},
		Stream:   true,
	})
	if err != nil {
		t.Fatalf("Stream returned error: %v", err)
	}
	gotEvents := collectEvents(t, events)

	// Count tool-call events. Splitting the call across slots would
	// produce two events with partial fields each.
	var toolCalls []model.ToolCall
	for _, e := range gotEvents {
		if e.Type == model.EventToolCall && e.ToolCall != nil {
			toolCalls = append(toolCalls, *e.ToolCall)
		}
	}
	if len(toolCalls) != 1 {
		t.Fatalf("tool calls = %d, want 1 (provider id flap should not split a single logical call). events: %#v", len(toolCalls), toolCalls)
	}
	call := toolCalls[0]
	if call.Name != "foo" {
		t.Fatalf("tool name = %q, want foo", call.Name)
	}
	if string(call.Arguments) != `{"path":"README.md"}` {
		t.Fatalf("tool arguments = %q, want merged arguments JSON", string(call.Arguments))
	}
	// Deterministic id rule: keep the FIRST non-empty id ("a"). Locking
	// the id avoids flapping based on chunk arrival order. Tests assert
	// the rule explicitly so a regression to "last id wins" is loud.
	if call.ID != "a" {
		t.Fatalf("tool id = %q, want %q (first non-empty id wins for id-rotating providers)", call.ID, "a")
	}
}

func TestClientStreamsTwoConcurrentToolCallsOnSeparateIndexes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSEJSON(t, w, map[string]any{
			"choices": []any{
				map[string]any{
					"delta": map[string]any{
						"tool_calls": []any{
							map[string]any{
								"index":    0,
								"id":       "call_1",
								"function": map[string]any{"name": "read_file", "arguments": "{\"path\":\"a\"}"},
							},
							map[string]any{
								"index":    1,
								"id":       "call_2",
								"function": map[string]any{"name": "read_file", "arguments": "{\"path\":\"b\"}"},
							},
						},
					},
				},
			},
		})
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	client := newTestClient(t, server, model.Provider{ID: "local", BaseURL: server.URL, DefaultModel: "coder"}, nil)
	events, err := client.Stream(context.Background(), model.Request{
		Model:    model.NewRef("local", "coder"),
		Messages: []model.Message{model.TextMessage(model.RoleUser, "read both")},
		Stream:   true,
	})
	if err != nil {
		t.Fatalf("Stream returned error: %v", err)
	}
	gotEvents := collectEvents(t, events)
	assertEventTypes(t, gotEvents, []model.EventType{
		model.EventStarted,
		model.EventToolCall,
		model.EventToolCall,
		model.EventCompleted,
	})
	if gotEvents[1].ToolCall.ID != "call_1" || gotEvents[2].ToolCall.ID != "call_2" {
		t.Fatalf("tool call ordering = %s,%s want call_1,call_2", gotEvents[1].ToolCall.ID, gotEvents[2].ToolCall.ID)
	}
}

func TestClientStreamRequestsIncludeUsage(t *testing.T) {
	var gotBody chatRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("Decode body: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	client := newTestClient(t, server, model.Provider{ID: "local", BaseURL: server.URL, DefaultModel: "coder"}, nil)
	events, err := client.Stream(context.Background(), model.Request{
		Model:    model.NewRef("local", "coder"),
		Messages: []model.Message{model.TextMessage(model.RoleUser, "ping")},
		Stream:   true,
	})
	if err != nil {
		t.Fatalf("Stream returned error: %v", err)
	}
	_ = collectEvents(t, events)

	if gotBody.StreamOptions == nil || !gotBody.StreamOptions.IncludeUsage {
		t.Fatalf("stream_options = %#v, want include_usage=true", gotBody.StreamOptions)
	}
}

func TestClientStreamToleratesUnknownSSEFields(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// Provider-specific fields and comments must not abort the stream.
		_, _ = io.WriteString(w, ": keepalive ping\n\n")
		_, _ = io.WriteString(w, "id: 1\n")
		_, _ = io.WriteString(w, "event: chunk\n")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	client := newTestClient(t, server, model.Provider{ID: "local", BaseURL: server.URL, DefaultModel: "coder"}, nil)
	events, err := client.Stream(context.Background(), model.Request{
		Model:    model.NewRef("local", "coder"),
		Messages: []model.Message{model.TextMessage(model.RoleUser, "ping")},
		Stream:   true,
	})
	if err != nil {
		t.Fatalf("Stream returned error: %v", err)
	}
	gotEvents := collectEvents(t, events)
	assertEventTypes(t, gotEvents, []model.EventType{
		model.EventStarted,
		model.EventTextDelta,
		model.EventCompleted,
	})
	if gotEvents[1].Text != "hi" {
		t.Fatalf("delta = %q, want hi", gotEvents[1].Text)
	}
}

func TestClientStreamSurfacesUsageDuringStreaming(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSEJSON(t, w, map[string]any{
			"choices": []any{map[string]any{"delta": map[string]any{"content": "ok"}}},
		})
		writeSSEJSON(t, w, map[string]any{
			"choices": []any{},
			"usage":   map[string]any{"prompt_tokens": 1, "completion_tokens": 2, "total_tokens": 3},
		})
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	client := newTestClient(t, server, model.Provider{ID: "local", BaseURL: server.URL, DefaultModel: "coder"}, nil)
	events, err := client.Stream(context.Background(), model.Request{
		Model:    model.NewRef("local", "coder"),
		Messages: []model.Message{model.TextMessage(model.RoleUser, "ping")},
		Stream:   true,
	})
	if err != nil {
		t.Fatalf("Stream returned error: %v", err)
	}
	gotEvents := collectEvents(t, events)
	var usage *model.Usage
	for _, e := range gotEvents {
		if e.Type == model.EventUsage {
			usage = e.Usage
		}
	}
	if usage == nil || usage.TotalTokens != 3 {
		t.Fatalf("usage = %#v, want total tokens 3", usage)
	}
}

func TestClientReturnsErrorWithoutRetryWhenContextCanceled(t *testing.T) {
	var hits int32
	block := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		<-block
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	client := newTestClient(t, server, model.Provider{ID: "local", BaseURL: server.URL, DefaultModel: "coder"}, nil)
	client.SetRetryPolicy(transport.RetryPolicy{MaxAttempts: 5, InitialDelay: 100 * time.Millisecond, MaxDelay: 100 * time.Millisecond, Multiplier: 2})

	_, err := client.Stream(ctx, model.Request{
		Model:    model.NewRef("local", "coder"),
		Messages: []model.Message{model.TextMessage(model.RoleUser, "ping")},
		Stream:   true,
	})
	close(block)
	if err == nil {
		t.Fatal("Stream returned nil error after cancellation")
	}
}
