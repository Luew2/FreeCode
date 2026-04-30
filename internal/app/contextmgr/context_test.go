package contextmgr

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Luew2/FreeCode/internal/core/model"
	"github.com/Luew2/FreeCode/internal/core/session"
)

func TestPrepareSetsMaxOutputAndCompactsPrompt(t *testing.T) {
	messages := []model.Message{
		model.TextMessage(model.RoleSystem, "system"),
		model.TextMessage(model.RoleDeveloper, "developer"),
		model.TextMessage(model.RoleUser, strings.Repeat("old context ", 200)),
		model.TextMessage(model.RoleAssistant, strings.Repeat("old answer ", 200)),
		model.TextMessage(model.RoleUser, "current request"),
	}
	prepared, err := Prepare(messages, nil, Budget{MaxInputTokens: 80, MaxOutputTokens: 32})
	if err != nil {
		t.Fatalf("Prepare returned error: %v", err)
	}
	if !prepared.Compacted {
		t.Fatalf("Compacted = false, want true")
	}
	if prepared.MaxOutputTokens != 32 {
		t.Fatalf("MaxOutputTokens = %d, want 32", prepared.MaxOutputTokens)
	}
	if prepared.InputTokens > prepared.MaxInputTokens {
		t.Fatalf("InputTokens = %d, max = %d", prepared.InputTokens, prepared.MaxInputTokens)
	}
	joined := joinMessages(prepared.Messages)
	if !strings.Contains(joined, "Context compacted") || !strings.Contains(joined, "current request") {
		t.Fatalf("prepared messages = %q, want compact marker and current request", joined)
	}
}

func TestBuildSessionContextUsesLatestCompactionAndRecentEvents(t *testing.T) {
	log := &memoryLog{}
	ctx := context.Background()
	events := []session.Event{
		{ID: "e1", SessionID: "s1", Type: session.EventUserMessage, Actor: "user", Text: "old request"},
		{ID: "e2", SessionID: "s1", Type: session.EventContextCompacted, Actor: "summarizer", Text: "summary so far"},
		{ID: "e3", SessionID: "s1", Type: session.EventAssistantMessage, Actor: "assistant", Text: "recent answer"},
	}
	for _, event := range events {
		if err := log.Append(ctx, event); err != nil {
			t.Fatalf("Append returned error: %v", err)
		}
	}
	summary, tokens, err := BuildSessionContext(ctx, log, "s1", 128)
	if err != nil {
		t.Fatalf("BuildSessionContext returned error: %v", err)
	}
	if tokens == 0 {
		t.Fatalf("tokens = 0, want estimate")
	}
	if !strings.Contains(summary, "summary so far") || !strings.Contains(summary, "recent answer") {
		t.Fatalf("summary = %q, want compact summary and recent event", summary)
	}
}

func TestBuildSessionContextExcludesLocalOnlyShellArtifacts(t *testing.T) {
	log := &memoryLog{}
	ctx := context.Background()
	events := []session.Event{
		{ID: "e1", SessionID: "s1", Type: session.EventUserMessage, Actor: "user", Text: "please run tests"},
		{
			ID:        "e2",
			SessionID: "s1",
			Type:      session.EventArtifact,
			Actor:     "user",
			Text:      "! echo secret-token",
			Payload: map[string]any{
				"artifact": map[string]any{
					"id":    "sh1",
					"kind":  "shell",
					"title": "! echo secret-token",
					"body":  "secret-token",
					"metadata": map[string]any{
						"local_only":       "true",
						"share_with_model": "false",
					},
				},
			},
		},
		{ID: "e3", SessionID: "s1", Type: session.EventAssistantMessage, Actor: "assistant", Text: "tests passed"},
	}
	for _, event := range events {
		if err := log.Append(ctx, event); err != nil {
			t.Fatalf("Append returned error: %v", err)
		}
	}
	summary, _, err := BuildSessionContext(ctx, log, "s1", 4096)
	if err != nil {
		t.Fatalf("BuildSessionContext returned error: %v", err)
	}
	if strings.Contains(summary, "secret-token") || strings.Contains(summary, "! echo") {
		t.Fatalf("summary = %q, want local shell artifact excluded", summary)
	}
	if !strings.Contains(summary, "please run tests") || !strings.Contains(summary, "tests passed") {
		t.Fatalf("summary = %q, want non-local events retained", summary)
	}
}

func TestBuildSessionContextIncludesSharedTerminalArtifacts(t *testing.T) {
	log := &memoryLog{}
	ctx := context.Background()
	event := session.Event{
		ID:        "e1",
		SessionID: "s1",
		Type:      session.EventArtifact,
		Actor:     "user",
		Text:      "npm test\nPASS",
		Payload: map[string]any{
			"artifact": map[string]any{
				"id":    "term1",
				"kind":  "terminal",
				"title": "Shared terminal output",
				"body":  "npm test\nPASS",
				"metadata": map[string]any{
					"share_with_model": "true",
				},
			},
		},
	}
	if err := log.Append(ctx, event); err != nil {
		t.Fatalf("Append returned error: %v", err)
	}
	summary, _, err := BuildSessionContext(ctx, log, "s1", 512)
	if err != nil {
		t.Fatalf("BuildSessionContext returned error: %v", err)
	}
	if !strings.Contains(summary, "npm test") || !strings.Contains(summary, "PASS") {
		t.Fatalf("summary = %q, want shared terminal output included", summary)
	}
	if !strings.Contains(summary, "shared_terminal user: Shared terminal output") {
		t.Fatalf("summary = %q, want explicit shared terminal label", summary)
	}
}

func TestBuildSessionContextIncludesToolNameAndArguments(t *testing.T) {
	log := &memoryLog{}
	ctx := context.Background()
	event := session.Event{
		ID:        "e1",
		SessionID: "s1",
		Type:      session.EventTool,
		Actor:     "tool",
		Text:      "terminal 1 sent command",
		Payload: map[string]any{
			"name":      "terminal_write",
			"arguments": `{"command":"ls"}`,
		},
	}
	if err := log.Append(ctx, event); err != nil {
		t.Fatalf("Append returned error: %v", err)
	}
	summary, _, err := BuildSessionContext(ctx, log, "s1", 512)
	if err != nil {
		t.Fatalf("BuildSessionContext returned error: %v", err)
	}
	if !strings.Contains(summary, "tool terminal_write") || !strings.Contains(summary, `{"command":"ls"}`) {
		t.Fatalf("summary = %q, want tool name and arguments", summary)
	}
}

func TestPrepareDoesNotTruncateDeveloperPolicyOrCurrentUserRequest(t *testing.T) {
	messages := []model.Message{
		model.TextMessage(model.RoleSystem, "system"),
		model.TextMessage(model.RoleDeveloper, "permission text must survive"),
		model.TextMessage(model.RoleUser, strings.Repeat("older context ", 80)),
		model.TextMessage(model.RoleAssistant, strings.Repeat("older answer ", 80)),
		model.TextMessage(model.RoleUser, "current request must survive"),
	}
	prepared, err := Prepare(messages, nil, Budget{MaxInputTokens: 96})
	if err != nil {
		t.Fatalf("Prepare returned error: %v", err)
	}
	joined := joinMessages(prepared.Messages)
	if !strings.Contains(joined, "permission text must survive") || !strings.Contains(joined, "current request must survive") {
		t.Fatalf("prepared messages = %q, want protected developer and current user text", joined)
	}
}

func TestPrepareErrorsWhenProtectedPromptExceedsBudget(t *testing.T) {
	messages := []model.Message{
		model.TextMessage(model.RoleSystem, strings.Repeat("system ", 100)),
		model.TextMessage(model.RoleDeveloper, "developer"),
		model.TextMessage(model.RoleUser, "current"),
	}
	_, err := Prepare(messages, nil, Budget{MaxInputTokens: 32})
	if err == nil || !strings.Contains(err.Error(), "protected prompt") {
		t.Fatalf("err = %v, want protected prompt budget error", err)
	}
}

func TestCompactSessionAppendsCheckpoint(t *testing.T) {
	log := &memoryLog{}
	ctx := context.Background()
	if err := log.Append(ctx, session.Event{ID: "e1", SessionID: "s1", Type: session.EventUserMessage, Actor: "user", Text: "hello"}); err != nil {
		t.Fatalf("Append returned error: %v", err)
	}
	event, err := CompactSession(ctx, log, "s1", 128, func() time.Time { return time.Unix(1, 0).UTC() })
	if err != nil {
		t.Fatalf("CompactSession returned error: %v", err)
	}
	if event.Type != session.EventContextCompacted || !strings.Contains(event.Text, "hello") {
		t.Fatalf("event = %#v, want compacted event with summary", event)
	}
	if len(log.events) != 2 {
		t.Fatalf("events = %d, want appended checkpoint", len(log.events))
	}
}

func joinMessages(messages []model.Message) string {
	var b strings.Builder
	for _, message := range messages {
		for _, part := range message.Content {
			b.WriteString(part.Text)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

type memoryLog struct {
	events []session.Event
}

func (l *memoryLog) Append(ctx context.Context, event session.Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	l.events = append(l.events, event)
	return nil
}

func (l *memoryLog) Stream(ctx context.Context, id session.ID) (<-chan session.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ch := make(chan session.Event)
	go func() {
		defer close(ch)
		for _, event := range l.events {
			if id == "" || event.SessionID == id {
				ch <- event
			}
		}
	}()
	return ch, nil
}
