package anthropic_messages

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Luew2/FreeCode/internal/adapters/model/transport"
	"github.com/Luew2/FreeCode/internal/core/model"
	"github.com/Luew2/FreeCode/internal/ports"
)

func TestClientRetriesOn5xxThenSucceeds(t *testing.T) {
	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		count := atomic.AddInt32(&hits, 1)
		if count < 3 {
			http.Error(w, "upstream", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":"hi"}}`+"\n\n")
	}))
	defer server.Close()

	client := newAnthropicClient(t, server, model.Provider{ID: "anthropic", BaseURL: server.URL, DefaultModel: "claude"}, nil)
	client.SetRetryPolicy(transport.RetryPolicy{MaxAttempts: 5, InitialDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond, Multiplier: 2})

	events, err := client.Stream(context.Background(), model.Request{
		Model:    model.NewRef("anthropic", "claude"),
		Messages: []model.Message{model.TextMessage(model.RoleUser, "ping")},
		Stream:   true,
	})
	if err != nil {
		t.Fatalf("Stream returned error: %v", err)
	}
	collected := make([]model.EventType, 0)
	for e := range events {
		collected = append(collected, e.Type)
	}
	if got := atomic.LoadInt32(&hits); got != 3 {
		t.Fatalf("attempts = %d, want 3", got)
	}
	if !contains(collected, model.EventStarted) || !contains(collected, model.EventCompleted) {
		t.Fatalf("event types = %v, want started+completed present", collected)
	}
}

func TestClientToleratesUnknownAnthropicSSEFields(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, ": ping\n")
		_, _ = io.WriteString(w, "event: content_block_start\n")
		_, _ = io.WriteString(w, "id: x1\n")
		_, _ = io.WriteString(w, `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":"hello"}}`+"\n\n")
		_, _ = io.WriteString(w, "event: message_stop\n\n")
	}))
	defer server.Close()

	client := newAnthropicClient(t, server, model.Provider{ID: "anthropic", BaseURL: server.URL, DefaultModel: "claude"}, nil)
	events, err := client.Stream(context.Background(), model.Request{
		Model:    model.NewRef("anthropic", "claude"),
		Messages: []model.Message{model.TextMessage(model.RoleUser, "ping")},
		Stream:   true,
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	var text strings.Builder
	for e := range events {
		if e.Type == model.EventTextDelta {
			text.WriteString(e.Text)
		}
	}
	if text.String() != "hello" {
		t.Fatalf("text = %q, want hello", text.String())
	}
}

func TestToMessagesRequestMapsRequiredToolChoiceToAny(t *testing.T) {
	provider := model.Provider{DefaultModel: "claude"}
	got := toMessagesRequest(provider, model.Request{
		Model:      model.NewRef("anthropic", "claude"),
		Messages:   []model.Message{model.TextMessage(model.RoleUser, "do it")},
		Tools:      []model.ToolSpec{{Name: "read_file", InputSchema: map[string]any{"type": "object"}}},
		ToolChoice: "required",
	})
	if got.ToolChoice == nil {
		t.Fatal("ToolChoice = nil, want non-nil")
	}
	if got.ToolChoice.Type != "any" {
		t.Fatalf("ToolChoice.Type = %q, want any (Anthropic's name for 'must call some tool')", got.ToolChoice.Type)
	}
}

func TestToMessagesRequestMapsToolNameToSpecificToolChoice(t *testing.T) {
	got := toMessagesRequest(model.Provider{DefaultModel: "claude"}, model.Request{
		Model:      model.NewRef("anthropic", "claude"),
		Messages:   []model.Message{model.TextMessage(model.RoleUser, "do it")},
		Tools:      []model.ToolSpec{{Name: "read_file", InputSchema: map[string]any{"type": "object"}}},
		ToolChoice: "read_file",
	})
	if got.ToolChoice == nil || got.ToolChoice.Type != "tool" || got.ToolChoice.Name != "read_file" {
		t.Fatalf("ToolChoice = %#v, want tool/read_file", got.ToolChoice)
	}
}

func TestToMessagesRequestOmitsToolChoiceByDefault(t *testing.T) {
	got := toMessagesRequest(model.Provider{DefaultModel: "claude"}, model.Request{
		Model:    model.NewRef("anthropic", "claude"),
		Messages: []model.Message{model.TextMessage(model.RoleUser, "ping")},
		Tools:    []model.ToolSpec{{Name: "read_file"}},
	})
	if got.ToolChoice != nil {
		t.Fatalf("ToolChoice = %#v, want nil (default)", got.ToolChoice)
	}
}

func contains(items []model.EventType, target model.EventType) bool {
	for _, item := range items {
		if item == target {
			return true
		}
	}
	return false
}

func newAnthropicClient(t *testing.T, server *httptest.Server, provider model.Provider, secrets ports.SecretStore) *Client {
	t.Helper()
	client, err := NewClient(provider, secrets, server.Client())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return client
}
