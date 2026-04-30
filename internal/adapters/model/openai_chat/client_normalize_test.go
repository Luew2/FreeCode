package openai_chat

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

func TestToChatRequestMergesLeadingSystemAndDeveloper(t *testing.T) {
	messages := []model.Message{
		model.TextMessage(model.RoleSystem, "you are FreeCode"),
		model.TextMessage(model.RoleDeveloper, "use tools when needed"),
		model.TextMessage(model.RoleDeveloper, "permissions: writes allowed"),
		model.TextMessage(model.RoleDeveloper, "environment: workspace=/repo"),
		model.TextMessage(model.RoleUser, "list files"),
	}
	got, err := toChatRequest(model.Provider{DefaultModel: "coder"}, model.Request{Model: model.NewRef("p", "coder"), Messages: messages})
	if err != nil {
		t.Fatalf("toChatRequest: %v", err)
	}
	if len(got.Messages) != 2 {
		t.Fatalf("messages = %d, want 2 (single system + user)", len(got.Messages))
	}
	if got.Messages[0].Role != "system" {
		t.Fatalf("first message role = %q, want system", got.Messages[0].Role)
	}
	expected := []string{"you are FreeCode", "use tools when needed", "permissions: writes allowed", "environment: workspace=/repo"}
	for _, want := range expected {
		if !strings.Contains(got.Messages[0].Content, want) {
			t.Fatalf("system content missing %q\ngot: %s", want, got.Messages[0].Content)
		}
	}
	if got.Messages[1].Role != "user" || got.Messages[1].Content != "list files" {
		t.Fatalf("user message = %#v, want user list files", got.Messages[1])
	}
}

func TestToChatRequestConvertsMidStreamDeveloperToSystem(t *testing.T) {
	messages := []model.Message{
		model.TextMessage(model.RoleSystem, "scaffolding"),
		model.TextMessage(model.RoleUser, "go"),
		model.TextMessage(model.RoleAssistant, "I'll inspect the file"),
		// orchestrator's tool-followthrough nudge: a mid-stream developer message.
		model.TextMessage(model.RoleDeveloper, "your last reply described tool work but did not call a tool"),
		model.TextMessage(model.RoleUser, "follow-up"),
	}
	got, err := toChatRequest(model.Provider{DefaultModel: "coder"}, model.Request{Model: model.NewRef("p", "coder"), Messages: messages})
	if err != nil {
		t.Fatalf("toChatRequest: %v", err)
	}
	roles := rolesOf(got.Messages)
	want := []string{"system", "user", "assistant", "system", "user"}
	if !equalRoles(roles, want) {
		t.Fatalf("roles = %v, want %v", roles, want)
	}
}

func TestToChatRequestEmitsToolChoiceAutoWhenToolsPresent(t *testing.T) {
	got, err := toChatRequest(model.Provider{DefaultModel: "coder"}, model.Request{
		Model:    model.NewRef("p", "coder"),
		Messages: []model.Message{model.TextMessage(model.RoleUser, "ping")},
		Tools:    []model.ToolSpec{{Name: "read_file", InputSchema: map[string]any{"type": "object"}}},
	})
	if err != nil {
		t.Fatalf("toChatRequest: %v", err)
	}
	if got.ToolChoice != "auto" {
		t.Fatalf("ToolChoice = %q, want auto", got.ToolChoice)
	}
}

func TestToChatRequestRespectsExplicitToolChoiceRequired(t *testing.T) {
	got, err := toChatRequest(model.Provider{DefaultModel: "coder"}, model.Request{
		Model:      model.NewRef("p", "coder"),
		Messages:   []model.Message{model.TextMessage(model.RoleUser, "ping")},
		Tools:      []model.ToolSpec{{Name: "read_file", InputSchema: map[string]any{"type": "object"}}},
		ToolChoice: "required",
	})
	if err != nil {
		t.Fatalf("toChatRequest: %v", err)
	}
	if got.ToolChoice != "required" {
		t.Fatalf("ToolChoice = %q, want required (override of auto default)", got.ToolChoice)
	}
}

func TestToChatRequestOmitsToolChoiceWithoutTools(t *testing.T) {
	got, err := toChatRequest(model.Provider{DefaultModel: "coder"}, model.Request{
		Model:    model.NewRef("p", "coder"),
		Messages: []model.Message{model.TextMessage(model.RoleUser, "ping")},
	})
	if err != nil {
		t.Fatalf("toChatRequest: %v", err)
	}
	if got.ToolChoice != "" {
		t.Fatalf("ToolChoice = %q, want empty when no tools", got.ToolChoice)
	}
}

func TestClientPreservesAssistantToolHistoryUnderNormalization(t *testing.T) {
	// Round-trip the full agent loop shape — system + dev + user + assistant
	// w/ tool_calls + tool result + new user — through the client and verify
	// the tool history is still intact after role normalization.
	var captured chatRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&captured)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer server.Close()

	client := newTestClient(t, server, model.Provider{ID: "local", BaseURL: server.URL, DefaultModel: "coder"}, nil)
	tool := model.TextMessage(model.RoleTool, "file contents")
	tool.ToolCallID = "call_1"
	events, err := client.Stream(context.Background(), model.Request{
		Model: model.NewRef("local", "coder"),
		Messages: []model.Message{
			model.TextMessage(model.RoleSystem, "system"),
			model.TextMessage(model.RoleDeveloper, "dev"),
			model.TextMessage(model.RoleUser, "read"),
			{Role: model.RoleAssistant, ToolCalls: []model.ToolCall{{ID: "call_1", Name: "read_file", Arguments: []byte(`{"path":"x"}`)}}},
			tool,
			model.TextMessage(model.RoleUser, "what did you find?"),
		},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	_ = collectEvents(t, events)

	roles := rolesOf(captured.Messages)
	want := []string{"system", "user", "assistant", "tool", "user"}
	if !equalRoles(roles, want) {
		t.Fatalf("roles = %v, want %v", roles, want)
	}
	assistant := captured.Messages[2]
	if len(assistant.ToolCalls) != 1 || assistant.ToolCalls[0].ID != "call_1" {
		t.Fatalf("assistant tool history dropped: %#v", assistant)
	}
	if captured.Messages[3].Role != "tool" || captured.Messages[3].ToolCallID != "call_1" {
		t.Fatalf("tool result lost tool_call_id: %#v", captured.Messages[3])
	}
}

func TestNormalizeMessagesEmptySystemContentDoesNotInjectBlankPrefix(t *testing.T) {
	messages := []model.Message{
		model.TextMessage(model.RoleSystem, "   "),
		model.TextMessage(model.RoleUser, "hello"),
	}
	got := normalizeMessages(messages)
	if len(got) != 1 {
		t.Fatalf("messages = %d, want 1 (skip empty system)", len(got))
	}
	if got[0].Role != "user" {
		t.Fatalf("first role = %q, want user", got[0].Role)
	}
}

func rolesOf(messages []chatMessage) []string {
	out := make([]string, len(messages))
	for i, m := range messages {
		out[i] = m.Role
	}
	return out
}

func equalRoles(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
