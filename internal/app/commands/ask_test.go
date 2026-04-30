package commands

import (
	"bytes"
	"context"
	"testing"

	"github.com/Luew2/FreeCode/internal/core/model"
)

func TestPruneOrphanToolMessagesDropsUnclaimedToolMessages(t *testing.T) {
	// Replays the OpenAI 400 the user just hit: history contains a tool
	// message with no preceding assistant tool_calls in scope. Without
	// pruning, the next request fails server-side with status 400.
	user := model.TextMessage(model.RoleUser, "list files")
	assistantWithCall := model.Message{
		Role: model.RoleAssistant,
		ToolCalls: []model.ToolCall{
			{ID: "call_1", Name: "list", Arguments: []byte(`{}`)},
		},
	}
	tool1 := model.TextMessage(model.RoleTool, "ok")
	tool1.ToolCallID = "call_1"
	orphanTool := model.TextMessage(model.RoleTool, "stale")
	orphanTool.ToolCallID = "call_999_dropped"
	finalUser := model.TextMessage(model.RoleUser, "follow up")

	got := pruneOrphanToolMessages([]model.Message{user, assistantWithCall, tool1, orphanTool, finalUser})
	if len(got) != 4 {
		t.Fatalf("messages = %d, want 4 (orphan dropped)", len(got))
	}
	for _, m := range got {
		if m.Role == model.RoleTool && m.ToolCallID == "call_999_dropped" {
			t.Fatalf("orphan tool message survived: %#v", m)
		}
	}
	if got[2].Role != model.RoleTool || got[2].ToolCallID != "call_1" {
		t.Fatalf("kept-tool message lost: %#v", got[2])
	}
}

func TestPruneOrphanToolMessagesPreservesClaimedToolGroup(t *testing.T) {
	user := model.TextMessage(model.RoleUser, "do two things")
	assistant := model.Message{
		Role: model.RoleAssistant,
		ToolCalls: []model.ToolCall{
			{ID: "a", Name: "one"},
			{ID: "b", Name: "two"},
		},
	}
	a := model.TextMessage(model.RoleTool, "result a")
	a.ToolCallID = "a"
	b := model.TextMessage(model.RoleTool, "result b")
	b.ToolCallID = "b"

	got := pruneOrphanToolMessages([]model.Message{user, assistant, a, b})
	if len(got) != 4 {
		t.Fatalf("messages = %d, want 4 (none dropped)", len(got))
	}
}

func TestPruneOrphanToolMessagesDropsLeadingTool(t *testing.T) {
	// Pure leading tool message with no preceding assistant — the
	// HistoryWithBudget already strips this case but defending here too
	// keeps pruneOrphanToolMessages safe to reuse anywhere.
	stale := model.TextMessage(model.RoleTool, "stale leading tool")
	stale.ToolCallID = "old_id"
	user := model.TextMessage(model.RoleUser, "new question")
	got := pruneOrphanToolMessages([]model.Message{stale, user})
	if len(got) != 1 {
		t.Fatalf("messages = %d, want 1 (leading tool dropped)", len(got))
	}
	if got[0].Role != model.RoleUser {
		t.Fatalf("kept message = %#v, want user", got[0])
	}
}

func TestAskPrintsOrchestratorResponse(t *testing.T) {
	client := &singleResponseModelClient{
		events: []model.Event{
			{Type: model.EventTextDelta, Text: "hello"},
			{Type: model.EventCompleted},
		},
	}

	var out bytes.Buffer
	err := Ask(context.Background(), &out, AskDependencies{
		Model:  model.NewRef("local", "coder"),
		Client: client,
	}, AskOptions{Question: "say hi"})
	if err != nil {
		t.Fatalf("Ask returned error: %v", err)
	}
	if out.String() != "hello\n" {
		t.Fatalf("output = %q, want hello", out.String())
	}
}

type singleResponseModelClient struct {
	events []model.Event
}

func (c *singleResponseModelClient) Stream(ctx context.Context, request model.Request) (<-chan model.Event, error) {
	ch := make(chan model.Event, len(c.events))
	for _, event := range c.events {
		ch <- event
	}
	close(ch)
	return ch, nil
}
