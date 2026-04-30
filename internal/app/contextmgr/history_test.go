package contextmgr

import (
	"context"
	"testing"
	"time"

	"github.com/Luew2/FreeCode/internal/core/model"
	"github.com/Luew2/FreeCode/internal/core/session"
)

type fakeLog struct {
	events []session.Event
}

func (l *fakeLog) Append(_ context.Context, event session.Event) error {
	l.events = append(l.events, event)
	return nil
}

func (l *fakeLog) Stream(_ context.Context, id session.ID) (<-chan session.Event, error) {
	out := make(chan session.Event)
	go func() {
		defer close(out)
		for _, e := range l.events {
			if id == "" || e.SessionID == id {
				out <- e
			}
		}
	}()
	return out, nil
}

func TestLoadMessageHistoryReplaysUserAssistantToolGroup(t *testing.T) {
	log := &fakeLog{}
	now := time.Now()
	sessionID := session.ID("s1")
	events := []session.Event{
		{ID: "e1", SessionID: sessionID, Type: session.EventUserMessage, At: now, Actor: "user", Text: "list the files"},
		{ID: "e2", SessionID: sessionID, Type: session.EventAssistantMessage, At: now, Actor: "assistant", Text: "calling tool", Payload: map[string]any{
			"step":   1,
			"status": "tool_calls",
			"tool_calls": []any{
				map[string]any{
					"id":        "call_1",
					"name":      "list_dir",
					"arguments": `{"path":"."}`,
				},
			},
		}},
		{ID: "e3", SessionID: sessionID, Type: session.EventTool, At: now, Actor: "tool", Text: "README.md\nmain.go", Payload: map[string]any{
			"call_id":   "call_1",
			"name":      "list_dir",
			"arguments": `{"path":"."}`,
		}},
		{ID: "e4", SessionID: sessionID, Type: session.EventAssistantMessage, At: now, Actor: "assistant", Text: "There are two files.", Payload: map[string]any{"step": 2, "status": "final"}},
	}
	for _, e := range events {
		_ = log.Append(context.Background(), e)
	}

	got, err := LoadMessageHistory(context.Background(), log, sessionID)
	if err != nil {
		t.Fatalf("LoadMessageHistory returned error: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("messages = %d, want 4", len(got))
	}
	if got[0].Role != model.RoleUser || got[0].Content[0].Text != "list the files" {
		t.Fatalf("user message = %#v", got[0])
	}
	if got[1].Role != model.RoleAssistant || len(got[1].ToolCalls) != 1 {
		t.Fatalf("assistant tool-call message = %#v", got[1])
	}
	if got[1].ToolCalls[0].ID != "call_1" || got[1].ToolCalls[0].Name != "list_dir" {
		t.Fatalf("tool call = %#v, want call_1 list_dir", got[1].ToolCalls[0])
	}
	if string(got[1].ToolCalls[0].Arguments) != `{"path":"."}` {
		t.Fatalf("tool call arguments = %q, want path=.", string(got[1].ToolCalls[0].Arguments))
	}
	if got[2].Role != model.RoleTool || got[2].ToolCallID != "call_1" {
		t.Fatalf("tool message = %#v, want tool_call_id=call_1", got[2])
	}
	if got[3].Role != model.RoleAssistant || got[3].Content[0].Text != "There are two files." {
		t.Fatalf("final assistant message = %#v", got[3])
	}
}

func TestLoadMessageHistorySkipsReasoningAssistantEvents(t *testing.T) {
	log := &fakeLog{}
	now := time.Now()
	sessionID := session.ID("s1")
	events := []session.Event{
		{ID: "e1", SessionID: sessionID, Type: session.EventUserMessage, At: now, Actor: "user", Text: "find the bug"},
		{ID: "e2", SessionID: sessionID, Type: session.EventAssistantMessage, At: now, Actor: "assistant", Text: "let me think about this carefully...", Payload: map[string]any{"step": 1, "status": "reasoning"}},
		{ID: "e3", SessionID: sessionID, Type: session.EventAssistantMessage, At: now, Actor: "assistant", Text: "The bug is in handler.go.", Payload: map[string]any{"step": 2, "status": "final"}},
	}
	for _, e := range events {
		_ = log.Append(context.Background(), e)
	}

	got, err := LoadMessageHistory(context.Background(), log, sessionID)
	if err != nil {
		t.Fatalf("LoadMessageHistory returned error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("messages = %d, want 2 (reasoning event filtered out)", len(got))
	}
	for _, message := range got {
		for _, part := range message.Content {
			if part.Text == "let me think about this carefully..." {
				t.Fatalf("messages = %#v, must not include reasoning text", got)
			}
		}
	}
	if got[0].Role != model.RoleUser || got[0].Content[0].Text != "find the bug" {
		t.Fatalf("messages[0] = %#v, want user prompt", got[0])
	}
	if got[1].Role != model.RoleAssistant || got[1].Content[0].Text != "The bug is in handler.go." {
		t.Fatalf("messages[1] = %#v, want final assistant reply", got[1])
	}
}

func TestLoadMessageHistorySkipsMalformedAssistantPayload(t *testing.T) {
	log := &fakeLog{}
	sessionID := session.ID("s1")
	_ = log.Append(context.Background(), session.Event{
		ID: "e1", SessionID: sessionID, Type: session.EventUserMessage, At: time.Now(), Text: "hello",
	})
	_ = log.Append(context.Background(), session.Event{
		ID: "e2", SessionID: sessionID, Type: session.EventAssistantMessage, At: time.Now(), Text: "",
		Payload: map[string]any{"step": 1, "tool_calls": "not-an-array"},
	})

	got, err := LoadMessageHistory(context.Background(), log, sessionID)
	if err != nil {
		t.Fatalf("LoadMessageHistory error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("messages = %d, want 1 (empty assistant skipped)", len(got))
	}
}

func TestHistoryWithBudgetClipsOldestMessages(t *testing.T) {
	messages := []model.Message{
		model.TextMessage(model.RoleUser, longString(2000)),
		model.TextMessage(model.RoleAssistant, "ok one"),
		model.TextMessage(model.RoleUser, "follow up"),
		model.TextMessage(model.RoleAssistant, "ok two"),
	}
	clipped := HistoryWithBudget(messages, 200)
	if len(clipped) >= len(messages) {
		t.Fatalf("HistoryWithBudget did not trim, got %d", len(clipped))
	}
	if clipped[len(clipped)-1].Content[0].Text != "ok two" {
		t.Fatalf("expected to keep most recent assistant message, got %v", clipped)
	}
}

func TestHistoryWithBudgetKeepsAllWhenWithinLimit(t *testing.T) {
	messages := []model.Message{
		model.TextMessage(model.RoleUser, "hi"),
		model.TextMessage(model.RoleAssistant, "hello"),
	}
	got := HistoryWithBudget(messages, 100000)
	if len(got) != len(messages) {
		t.Fatalf("HistoryWithBudget trimmed unexpectedly, got %d want %d", len(got), len(messages))
	}
}

func longString(n int) string {
	out := make([]byte, n)
	for i := range out {
		out[i] = 'x'
	}
	return string(out)
}
