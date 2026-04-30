package openai_chat

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/Luew2/FreeCode/internal/core/model"
	"github.com/Luew2/FreeCode/internal/ports"
)

func TestClientStreamsTextDeltas(t *testing.T) {
	var gotPath string
	var gotAccept string
	var gotBody chatRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAccept = r.Header.Get("Accept")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("Decode request body: %v", err)
		}

		w.Header().Set("Content-Type", "text/event-stream")
		writeSSEJSON(t, w, map[string]any{
			"choices": []any{
				map[string]any{"delta": map[string]any{"content": "hel"}},
			},
		})
		writeSSEJSON(t, w, map[string]any{
			"choices": []any{
				map[string]any{"delta": map[string]any{"content": "lo"}},
			},
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

	if gotPath != "/v1/chat/completions" {
		t.Fatalf("path = %q, want /v1/chat/completions", gotPath)
	}
	if gotAccept != "text/event-stream" {
		t.Fatalf("Accept = %q, want text/event-stream", gotAccept)
	}
	if gotBody.Model != "coder" {
		t.Fatalf("request model = %q, want coder", gotBody.Model)
	}
	if !gotBody.Stream {
		t.Fatal("request stream = false, want true")
	}
	if gotBody.Messages[0].Role != "user" || gotBody.Messages[0].Content != "ping" {
		t.Fatalf("request message = %#v, want user ping", gotBody.Messages[0])
	}
	assertEventTypes(t, gotEvents, []model.EventType{
		model.EventStarted,
		model.EventTextDelta,
		model.EventTextDelta,
		model.EventCompleted,
	})
	if got := gotEvents[1].Text + gotEvents[2].Text; got != "hello" {
		t.Fatalf("streamed text = %q, want hello", got)
	}
}

func TestClientStreamsToolCalls(t *testing.T) {
	var gotBody chatRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("Decode request body: %v", err)
		}

		w.Header().Set("Content-Type", "text/event-stream")
		writeSSEJSON(t, w, map[string]any{
			"choices": []any{
				map[string]any{
					"delta": map[string]any{
						"tool_calls": []any{
							map[string]any{
								"index": 0,
								"id":    "call_1",
								"type":  "function",
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
								"index": 0,
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
		Tools: []model.ToolSpec{
			{
				Name:        "read_file",
				Description: "Read a file",
				InputSchema: map[string]any{"type": "object"},
			},
		},
		Stream: true,
	})
	if err != nil {
		t.Fatalf("Stream returned error: %v", err)
	}
	gotEvents := collectEvents(t, events)

	if len(gotBody.Tools) != 1 {
		t.Fatalf("request tools = %d, want 1", len(gotBody.Tools))
	}
	if gotBody.Tools[0].Type != "function" || gotBody.Tools[0].Function.Name != "read_file" {
		t.Fatalf("request tool = %#v, want read_file function", gotBody.Tools[0])
	}
	assertEventTypes(t, gotEvents, []model.EventType{
		model.EventStarted,
		model.EventToolCall,
		model.EventCompleted,
	})
	toolCall := gotEvents[1].ToolCall
	if toolCall == nil {
		t.Fatal("tool call event has nil ToolCall")
	}
	if toolCall.ID != "call_1" || toolCall.Name != "read_file" {
		t.Fatalf("tool call = %#v, want call_1 read_file", toolCall)
	}
	if got := string(toolCall.Arguments); got != `{"path":"README.md"}` {
		t.Fatalf("tool call arguments = %q, want README path JSON", got)
	}
}

func TestClientSerializesToolHistory(t *testing.T) {
	var gotBody chatRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("Decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices": [{"message": {"content": "ok"}}]}`)
	}))
	defer server.Close()

	client := newTestClient(t, server, model.Provider{ID: "local", BaseURL: server.URL, DefaultModel: "coder"}, nil)
	toolResult := model.TextMessage(model.RoleTool, "file contents")
	toolResult.ToolCallID = "call_1"
	events, err := client.Stream(context.Background(), model.Request{
		Model: model.NewRef("local", "coder"),
		Messages: []model.Message{
			model.TextMessage(model.RoleUser, "read it"),
			{
				Role: model.RoleAssistant,
				ToolCalls: []model.ToolCall{
					{
						ID:        "call_1",
						Name:      "read_file",
						Arguments: []byte(`{"path":"README.md"}`),
					},
				},
			},
			toolResult,
		},
	})
	if err != nil {
		t.Fatalf("Stream returned error: %v", err)
	}
	_ = collectEvents(t, events)

	if len(gotBody.Messages) != 3 {
		t.Fatalf("request messages = %d, want 3", len(gotBody.Messages))
	}
	assistant := gotBody.Messages[1]
	if assistant.Role != "assistant" || len(assistant.ToolCalls) != 1 {
		t.Fatalf("assistant history message = %#v, want one tool call", assistant)
	}
	toolCall := assistant.ToolCalls[0]
	if toolCall.ID != "call_1" || toolCall.Type != "function" || toolCall.Function.Name != "read_file" {
		t.Fatalf("serialized tool call = %#v, want call_1 read_file function", toolCall)
	}
	if toolCall.Function.Arguments != `{"path":"README.md"}` {
		t.Fatalf("serialized arguments = %q, want README path JSON", toolCall.Function.Arguments)
	}
	tool := gotBody.Messages[2]
	if tool.Role != "tool" || tool.ToolCallID != "call_1" || tool.Content != "file contents" {
		t.Fatalf("tool result message = %#v, want tool call result", tool)
	}
}

func TestClientStreamsPlainContentFallback(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: hello\n\n")
		_, _ = io.WriteString(w, "data: world\n\n")
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
		model.EventTextDelta,
		model.EventCompleted,
	})
	if got := gotEvents[1].Text + gotEvents[2].Text; got != "helloworld" {
		t.Fatalf("plain streamed text = %q, want helloworld", got)
	}
}

func TestClientReadsNonStreamingContent(t *testing.T) {
	var gotAccept string
	var gotBody chatRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAccept = r.Header.Get("Accept")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("Decode request body: %v", err)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"choices": [{"message": {"role": "assistant", "content": "done"}}],
			"usage": {"prompt_tokens": 3, "completion_tokens": 2, "total_tokens": 5}
		}`)
	}))
	defer server.Close()

	client := newTestClient(t, server, model.Provider{ID: "local", BaseURL: server.URL, DefaultModel: "coder"}, nil)
	events, err := client.Stream(context.Background(), model.Request{
		Model:    model.NewRef("local", "coder"),
		Messages: []model.Message{model.TextMessage(model.RoleUser, "ping")},
		Stream:   false,
	})
	if err != nil {
		t.Fatalf("Stream returned error: %v", err)
	}
	gotEvents := collectEvents(t, events)

	if gotAccept != "application/json" {
		t.Fatalf("Accept = %q, want application/json", gotAccept)
	}
	if gotBody.Stream {
		t.Fatal("request stream = true, want false")
	}
	assertEventTypes(t, gotEvents, []model.EventType{
		model.EventStarted,
		model.EventTextDelta,
		model.EventUsage,
		model.EventCompleted,
	})
	if gotEvents[1].Text != "done" {
		t.Fatalf("non-streaming text = %q, want done", gotEvents[1].Text)
	}
	if gotEvents[2].Usage == nil || gotEvents[2].Usage.TotalTokens != 5 {
		t.Fatalf("usage = %#v, want total tokens 5", gotEvents[2].Usage)
	}
}

func TestClientReadsNonStreamingToolCalls(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"choices": [{
				"message": {
					"tool_calls": [{
						"id": "call_1",
						"type": "function",
						"function": {
							"name": "read_file",
							"arguments": "{\"path\":\"README.md\"}"
						}
					}]
				}
			}]
		}`)
	}))
	defer server.Close()

	client := newTestClient(t, server, model.Provider{ID: "local", BaseURL: server.URL, DefaultModel: "coder"}, nil)
	events, err := client.Stream(context.Background(), model.Request{
		Model:    model.NewRef("local", "coder"),
		Messages: []model.Message{model.TextMessage(model.RoleUser, "read it")},
		Stream:   false,
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
	toolCall := gotEvents[1].ToolCall
	if toolCall == nil {
		t.Fatal("tool call event has nil ToolCall")
	}
	if toolCall.ID != "call_1" || toolCall.Name != "read_file" {
		t.Fatalf("tool call = %#v, want call_1 read_file", toolCall)
	}
	if got := string(toolCall.Arguments); got != `{"path":"README.md"}` {
		t.Fatalf("tool call arguments = %q, want README path JSON", got)
	}
}

func TestClientStatusErrorIncludesEndpointAndStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	}))
	defer server.Close()

	client := newTestClient(t, server, model.Provider{ID: "local", BaseURL: server.URL, DefaultModel: "coder"}, nil)
	events, err := client.Stream(context.Background(), model.Request{
		Model:    model.NewRef("local", "coder"),
		Messages: []model.Message{model.TextMessage(model.RoleUser, "ping")},
		Stream:   true,
	})
	if err == nil {
		t.Fatal("Stream returned nil error")
	}
	if events != nil {
		t.Fatal("Stream returned events channel for status error")
	}
	message := err.Error()
	for _, want := range []string{server.URL + "/v1/chat/completions", "status 429", "rate limited"} {
		if !strings.Contains(message, want) {
			t.Fatalf("error %q does not contain %q", message, want)
		}
	}
}

func TestClientEmitsEventErrorForStreamProviderError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSEJSON(t, w, map[string]any{
			"error": map[string]any{
				"message": "model overloaded",
				"type":    "server_error",
			},
		})
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
		model.EventError,
	})
	if !strings.Contains(gotEvents[1].Error, "model overloaded") {
		t.Fatalf("stream provider error = %q, want provider message", gotEvents[1].Error)
	}
}

func TestClientEmitsEventErrorForNonStreamingProviderError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"error": {"message": "bad request", "type": "invalid_request_error"}}`)
	}))
	defer server.Close()

	client := newTestClient(t, server, model.Provider{ID: "local", BaseURL: server.URL, DefaultModel: "coder"}, nil)
	events, err := client.Stream(context.Background(), model.Request{
		Model:    model.NewRef("local", "coder"),
		Messages: []model.Message{model.TextMessage(model.RoleUser, "ping")},
		Stream:   false,
	})
	if err != nil {
		t.Fatalf("Stream returned error: %v", err)
	}
	gotEvents := collectEvents(t, events)

	assertEventTypes(t, gotEvents, []model.EventType{
		model.EventStarted,
		model.EventError,
	})
	if !strings.Contains(gotEvents[1].Error, "bad request") {
		t.Fatalf("non-streaming provider error = %q, want provider message", gotEvents[1].Error)
	}
}

func TestClientEmitsEventErrorForMalformedStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[\n\n")
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
		model.EventError,
	})
	if !strings.Contains(gotEvents[1].Error, "invalid streamed chat completion JSON") {
		t.Fatalf("stream error = %q, want malformed JSON message", gotEvents[1].Error)
	}
}

func TestClientUsesBearerAuthFromSecretStore(t *testing.T) {
	var gotAuthorization string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthorization = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices": [{"message": {"content": "ok"}}]}`)
	}))
	defer server.Close()

	client := newTestClient(t, server, model.Provider{
		ID:           "local",
		BaseURL:      server.URL,
		DefaultModel: "coder",
		Secret:       model.SecretRef{Name: "LOCAL_API_KEY", Source: "env"},
	}, staticSecretStore{"LOCAL_API_KEY": "sk-test"})
	events, err := client.Stream(context.Background(), model.Request{
		Model:    model.NewRef("local", "coder"),
		Messages: []model.Message{model.TextMessage(model.RoleUser, "ping")},
	})
	if err != nil {
		t.Fatalf("Stream returned error: %v", err)
	}
	_ = collectEvents(t, events)

	if gotAuthorization != "Bearer sk-test" {
		t.Fatalf("Authorization = %q, want bearer token", gotAuthorization)
	}
}

func TestClientReturnsErrorWhenSecretStoreMissing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("server should not receive request when auth setup fails")
	}))
	defer server.Close()

	client := newTestClient(t, server, model.Provider{
		ID:           "local",
		BaseURL:      server.URL,
		DefaultModel: "coder",
		Secret:       model.SecretRef{Name: "LOCAL_API_KEY", Source: "env"},
	}, nil)
	events, err := client.Stream(context.Background(), model.Request{
		Model:    model.NewRef("local", "coder"),
		Messages: []model.Message{model.TextMessage(model.RoleUser, "ping")},
	})
	if err == nil {
		t.Fatal("Stream returned nil error")
	}
	if events != nil {
		t.Fatal("Stream returned events channel for auth setup error")
	}
	if !strings.Contains(err.Error(), "secret store is not configured") {
		t.Fatalf("error = %q, want secret store message", err.Error())
	}
}

func TestClientReturnsErrorWhenModelIDMissing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("server should not receive request when model setup fails")
	}))
	defer server.Close()

	client := newTestClient(t, server, model.Provider{ID: "local", BaseURL: server.URL}, nil)
	events, err := client.Stream(context.Background(), model.Request{
		Messages: []model.Message{model.TextMessage(model.RoleUser, "ping")},
	})
	if err == nil {
		t.Fatal("Stream returned nil error")
	}
	if events != nil {
		t.Fatal("Stream returned events channel for model setup error")
	}
	if !strings.Contains(err.Error(), "model id is empty") {
		t.Fatalf("error = %q, want model id message", err.Error())
	}
}

func newTestClient(t *testing.T, server *httptest.Server, provider model.Provider, secrets ports.SecretStore) *Client {
	t.Helper()
	client, err := NewClient(provider, secrets, server.Client())
	if err != nil {
		t.Fatalf("NewClient returned error: %v", err)
	}
	return client
}

func writeSSEJSON(t *testing.T, w io.Writer, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("Marshal SSE payload: %v", err)
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
		t.Fatalf("Write SSE payload: %v", err)
	}
}

func collectEvents(t *testing.T, events <-chan model.Event) []model.Event {
	t.Helper()
	var collected []model.Event
	for event := range events {
		collected = append(collected, event)
	}
	return collected
}

func assertEventTypes(t *testing.T, events []model.Event, want []model.EventType) {
	t.Helper()
	got := make([]model.EventType, 0, len(events))
	for _, event := range events {
		got = append(got, event.Type)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("event types = %v, want %v", got, want)
	}
}
