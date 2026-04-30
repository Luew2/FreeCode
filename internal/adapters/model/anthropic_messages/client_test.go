package anthropic_messages

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Luew2/FreeCode/internal/core/model"
)

func TestClientStreamsTextAndToolUse(t *testing.T) {
	var gotPath string
	var gotKey string
	var gotBody messagesRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("X-Api-Key")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("Decode request: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		// Anthropic always emits content_block_start before any deltas.
		// Earlier versions of this test omitted the start for the leading
		// text block; the SDK's typed accumulator requires the start, so
		// we now mirror the real wire shape.
		writeSSE(t, w, map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}})
		writeSSE(t, w, map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": "hi"}})
		writeSSE(t, w, map[string]any{"type": "content_block_stop", "index": 0})
		writeSSE(t, w, map[string]any{"type": "content_block_start", "index": 1, "content_block": map[string]any{"type": "tool_use", "id": "toolu_1", "name": "read_file", "input": map[string]any{}}})
		writeSSE(t, w, map[string]any{"type": "content_block_delta", "index": 1, "delta": map[string]any{"type": "input_json_delta", "partial_json": `{"path":"README.md"}`}})
		writeSSE(t, w, map[string]any{"type": "content_block_stop", "index": 1})
		writeSSE(t, w, map[string]any{"type": "message_delta", "usage": map[string]any{"input_tokens": 3, "output_tokens": 2}})
		writeSSE(t, w, map[string]any{"type": "message_stop"})
	}))
	defer server.Close()

	client, err := NewClient(model.Provider{
		ID:           "anthropic",
		BaseURL:      server.URL,
		Secret:       model.SecretRef{Name: "ANTHROPIC_TEST_KEY", Source: "env"},
		DefaultModel: "claude",
	}, fakeSecrets{"ANTHROPIC_TEST_KEY": "secret"}, server.Client())
	if err != nil {
		t.Fatalf("NewClient returned error: %v", err)
	}
	events, err := client.Stream(context.Background(), model.Request{
		Model:    model.NewRef("anthropic", "claude"),
		Messages: []model.Message{model.TextMessage(model.RoleUser, "hello")},
		Tools:    []model.ToolSpec{{Name: "read_file", InputSchema: map[string]any{"type": "object"}}},
		Stream:   true,
	})
	if err != nil {
		t.Fatalf("Stream returned error: %v", err)
	}
	gotEvents := collectEvents(events)
	if gotPath != "/v1/messages" {
		t.Fatalf("path = %q, want /v1/messages", gotPath)
	}
	if gotKey != "secret" {
		t.Fatalf("key = %q, want secret", gotKey)
	}
	if gotBody.Model != "claude" || gotBody.MaxTokens != defaultMaxTokens || !gotBody.Stream {
		t.Fatalf("body = %#v, want model/default max tokens/stream", gotBody)
	}
	if len(gotBody.Tools) != 1 || gotBody.Tools[0].Name != "read_file" {
		t.Fatalf("tools = %#v, want read_file", gotBody.Tools)
	}
	if gotEvents[0].Type != model.EventStarted || gotEvents[len(gotEvents)-1].Type != model.EventCompleted {
		t.Fatalf("events = %#v, want started/completed", gotEvents)
	}
	foundText := false
	foundTool := false
	foundUsage := false
	for _, event := range gotEvents {
		if event.Type == model.EventTextDelta && event.Text == "hi" {
			foundText = true
		}
		if event.Type == model.EventToolCall && event.ToolCall != nil && event.ToolCall.Name == "read_file" && string(event.ToolCall.Arguments) == `{"path":"README.md"}` {
			foundTool = true
		}
		if event.Type == model.EventUsage && event.Usage.TotalTokens == 5 {
			foundUsage = true
		}
	}
	if !foundText || !foundTool || !foundUsage {
		t.Fatalf("events = %#v, want text, tool, and usage", gotEvents)
	}
}

func TestClientSerializesToolHistory(t *testing.T) {
	var gotBody messagesRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("Decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"content":[{"type":"text","text":"ok"}]}`)
	}))
	defer server.Close()

	client, err := NewClient(model.Provider{ID: "anthropic", BaseURL: server.URL, DefaultModel: "claude"}, nil, server.Client())
	if err != nil {
		t.Fatalf("NewClient returned error: %v", err)
	}
	events, err := client.Stream(context.Background(), model.Request{
		Model: model.NewRef("anthropic", "claude"),
		Messages: []model.Message{
			model.TextMessage(model.RoleSystem, "system"),
			{Role: model.RoleAssistant, ToolCalls: []model.ToolCall{{ID: "toolu_1", Name: "read_file", Arguments: []byte(`{"path":"README.md"}`)}}},
			func() model.Message {
				msg := model.TextMessage(model.RoleTool, "contents")
				msg.ToolCallID = "toolu_1"
				return msg
			}(),
		},
		MaxOutputTokens: 7,
	})
	if err != nil {
		t.Fatalf("Stream returned error: %v", err)
	}
	_ = collectEvents(events)
	if gotBody.System != "system" || gotBody.MaxTokens != 7 {
		t.Fatalf("body = %#v, want system and max tokens", gotBody)
	}
	if len(gotBody.Messages) != 2 {
		t.Fatalf("messages = %#v, want assistant tool_use and user tool_result", gotBody.Messages)
	}
	if gotBody.Messages[0].Content[0].Type != "tool_use" || gotBody.Messages[1].Content[0].Type != "tool_result" {
		t.Fatalf("messages = %#v, want tool history", gotBody.Messages)
	}
}

func TestProbeDetectsMessagesEndpoint(t *testing.T) {
	var gotPath string
	var gotKey string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("X-Api-Key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"content":[{"type":"text","text":"pong"}]}`)
	}))
	defer server.Close()

	result, err := NewProbe(fakeSecrets{"ANTHROPIC_TEST_KEY": "secret"}, server.Client()).Probe(context.Background(), model.Provider{
		ID:      "anthropic",
		BaseURL: server.URL + "/v1",
		Secret:  model.SecretRef{Name: "ANTHROPIC_TEST_KEY", Source: "env"},
	}, "claude")
	if err != nil {
		t.Fatalf("Probe returned error: %v", err)
	}
	if gotPath != "/v1/messages" || gotKey != "secret" {
		t.Fatalf("path/key = %q/%q, want messages endpoint and key", gotPath, gotKey)
	}
	if result.Protocol != Protocol || !strings.HasSuffix(result.Endpoint, "/v1/messages") {
		t.Fatalf("result = %#v, want anthropic protocol endpoint", result)
	}
}

func collectEvents(events <-chan model.Event) []model.Event {
	var got []model.Event
	for event := range events {
		got = append(got, event)
	}
	return got
}

// writeSSE emits an Anthropic-style server-sent event. The Anthropic SDK's
// stream parser dispatches on the SSE `event:` field (not the JSON `type`
// field), so when the test data has a top-level `type` we mirror it as
// `event:` to match the real on-the-wire shape.
func writeSSE(t *testing.T, w io.Writer, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("Marshal SSE: %v", err)
	}
	if eventType, ok := extractEventType(value); ok {
		_, _ = io.WriteString(w, "event: ")
		_, _ = io.WriteString(w, eventType)
		_, _ = io.WriteString(w, "\n")
	}
	_, _ = io.WriteString(w, "data: ")
	_, _ = w.Write(data)
	_, _ = io.WriteString(w, "\n\n")
}

func extractEventType(value any) (string, bool) {
	m, ok := value.(map[string]any)
	if !ok {
		return "", false
	}
	t, ok := m["type"].(string)
	if !ok || t == "" {
		return "", false
	}
	return t, true
}

type fakeSecrets map[string]string

func (s fakeSecrets) Get(ctx context.Context, name string) (string, error) {
	return s[name], ctx.Err()
}

func (s fakeSecrets) Set(ctx context.Context, name string, value string) error {
	return ctx.Err()
}

func (s fakeSecrets) Delete(ctx context.Context, name string) error {
	return ctx.Err()
}
