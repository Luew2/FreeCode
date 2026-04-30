package orchestrator

import (
	"context"
	"strings"
	"testing"

	"github.com/Luew2/FreeCode/internal/core/model"
	"github.com/Luew2/FreeCode/internal/core/session"
)

func TestRunnerThreadsPriorMessagesIntoFirstRequest(t *testing.T) {
	client := &scriptedModelClient{
		scripts: [][]model.Event{
			{
				{Type: model.EventTextDelta, Text: "thanks for the recap"},
				{Type: model.EventCompleted},
			},
		},
	}
	prior := []model.Message{
		model.TextMessage(model.RoleUser, "list files"),
		{
			Role: model.RoleAssistant,
			ToolCalls: []model.ToolCall{
				{ID: "call_99", Name: "list_dir", Arguments: []byte(`{"path":"."}`)},
			},
		},
		{
			Role:       model.RoleTool,
			ToolCallID: "call_99",
			Content: []model.ContentPart{
				{Type: model.ContentText, Text: "README.md\nmain.go"},
			},
		},
	}
	_, err := Runner{
		Model: client,
		Tools: &fakeTools{result: "ok"},
		Log:   &memoryEventLog{},
	}.Run(context.Background(), Request{
		SessionID:     "s1",
		Model:         model.NewRef("local", "coder"),
		UserRequest:   "what files did we see?",
		PriorMessages: prior,
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if len(client.requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(client.requests))
	}
	got := client.requests[0].Messages
	// We expect the prior 3 messages to be present, followed by the new user request as the last entry.
	if got[len(got)-1].Role != model.RoleUser || got[len(got)-1].Content[0].Text != "what files did we see?" {
		t.Fatalf("last message = %#v, want new user request", got[len(got)-1])
	}
	var foundToolCallID string
	for _, msg := range got {
		if msg.Role == model.RoleAssistant {
			for _, call := range msg.ToolCalls {
				if call.ID == "call_99" {
					foundToolCallID = call.ID
				}
			}
		}
	}
	if foundToolCallID != "call_99" {
		t.Fatalf("expected prior assistant tool_call call_99 to be replayed, got messages = %#v", got)
	}
}

func TestRunnerForcesToolChoiceOnFollowThroughTurn(t *testing.T) {
	// First turn: model produces text-only "I'll inspect..." which trips the
	// follow-through heuristic. The orchestrator should issue the second
	// request with ToolChoice="required" so models that ignore the polite
	// developer-message nudge are forced into a tool call.
	client := &scriptedModelClient{
		scripts: [][]model.Event{
			{
				{Type: model.EventTextDelta, Text: "I'll inspect the README"},
				{Type: model.EventCompleted},
			},
			{
				{Type: model.EventToolCall, ToolCall: &model.ToolCall{ID: "call_1", Name: "read_file", Arguments: []byte(`{"path":"README.md"}`)}},
				{Type: model.EventCompleted},
			},
			{
				{Type: model.EventTextDelta, Text: "summary of repo"},
				{Type: model.EventCompleted},
			},
		},
	}
	tools := &fakeTools{result: "readme contents", specs: []model.ToolSpec{{Name: "read_file"}}}
	_, err := Runner{Model: client, Tools: tools, Log: &memoryEventLog{}}.Run(context.Background(), Request{
		SessionID:   "s1",
		Model:       model.NewRef("local", "coder"),
		UserRequest: "summarize the readme",
		MaxSteps:    8,
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if len(client.requests) < 2 {
		t.Fatalf("requests = %d, want at least 2", len(client.requests))
	}
	// First request: no tool choice forced — the model has free choice.
	if got := client.requests[0].ToolChoice; got != "" {
		t.Fatalf("first request ToolChoice = %q, want empty (default auto)", got)
	}
	// Second request (after followthrough): must force tool_choice=required.
	if got := client.requests[1].ToolChoice; got != "required" {
		t.Fatalf("second request ToolChoice = %q, want required after followthrough", got)
	}
	// Third request (after the forced tool call resolved): back to default.
	if len(client.requests) >= 3 {
		if got := client.requests[2].ToolChoice; got != "" {
			t.Fatalf("third request ToolChoice = %q, want empty after forced turn completes", got)
		}
	}
}

func TestRunnerDoesNotForceToolWhenNoToolsAvailable(t *testing.T) {
	// Without tools registered the followthrough heuristic itself should not
	// fire (needsToolFollowThrough already returns false when len(tools)==0),
	// but defend against future regressions: force flag must never produce a
	// "required" toolChoice when there are no tools to call.
	client := &scriptedModelClient{
		scripts: [][]model.Event{
			{
				{Type: model.EventTextDelta, Text: "I'll inspect this"},
				{Type: model.EventCompleted},
			},
		},
	}
	_, err := Runner{Model: client, Tools: nil, Log: &memoryEventLog{}}.Run(context.Background(), Request{
		SessionID:   "s1",
		Model:       model.NewRef("local", "coder"),
		UserRequest: "go",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for i, req := range client.requests {
		if req.ToolChoice == "required" {
			t.Fatalf("request %d ToolChoice = %q, want non-required when tools absent", i, req.ToolChoice)
		}
	}
}

func TestRunnerFiresToolFollowThroughAtMostOncePerRun(t *testing.T) {
	// Three text-only turns. Without the once-per-run guard the orchestrator
	// would keep injecting follow-through prompts until max_steps and then
	// surface "agent stopped after N steps without final response", which is
	// the bug the user reported (model just stops). With the guard it should
	// nudge once, accept the second tool-less response as final, and return.
	client := &scriptedModelClient{
		scripts: [][]model.Event{
			{
				{Type: model.EventTextDelta, Text: "I'll check the README for you"},
				{Type: model.EventCompleted},
			},
			{
				{Type: model.EventTextDelta, Text: "I'll start by inspecting the file"},
				{Type: model.EventCompleted},
			},
			{
				{Type: model.EventTextDelta, Text: "should not be reached"},
				{Type: model.EventCompleted},
			},
		},
	}
	tools := &fakeTools{result: "unused", specs: []model.ToolSpec{{Name: "read_file"}}}
	log := &memoryEventLog{}
	resp, err := Runner{Model: client, Tools: tools, Log: log}.Run(context.Background(), Request{
		SessionID:   "s1",
		Model:       model.NewRef("local", "coder"),
		UserRequest: "summarize the readme",
		MaxSteps:    8,
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if len(client.requests) != 2 {
		t.Fatalf("model requests = %d, want 2 (initial + one follow-through)", len(client.requests))
	}
	if resp.Text != "I'll start by inspecting the file" {
		t.Fatalf("response text = %q, want second tool-less reply accepted as final", resp.Text)
	}

	var followThroughs, finals int
	for _, e := range log.events {
		if e.Type == session.EventAssistantMessage {
			switch e.Payload["status"] {
			case "needs_tool_followthrough":
				followThroughs++
			case "final":
				finals++
			}
		}
	}
	if followThroughs != 1 {
		t.Fatalf("needs_tool_followthrough events = %d, want exactly 1", followThroughs)
	}
	if finals != 1 {
		t.Fatalf("final events = %d, want 1", finals)
	}
}

func TestRunnerLogsAssistantTurnWithToolCallsForReplay(t *testing.T) {
	client := &scriptedModelClient{
		scripts: [][]model.Event{
			{
				{Type: model.EventToolCall, ToolCall: &model.ToolCall{ID: "call_1", Name: "read_file", Arguments: []byte(`{"path":"x"}`)}},
				{Type: model.EventCompleted},
			},
			{
				{Type: model.EventTextDelta, Text: "all done"},
				{Type: model.EventCompleted},
			},
		},
	}
	log := &memoryEventLog{}
	_, err := Runner{
		Model: client,
		Tools: &fakeTools{result: "data"},
		Log:   log,
	}.Run(context.Background(), Request{
		SessionID:   "s1",
		Model:       model.NewRef("local", "coder"),
		UserRequest: "go",
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	var assistantTurns []session.Event
	for _, e := range log.events {
		if e.Type == session.EventAssistantMessage {
			assistantTurns = append(assistantTurns, e)
		}
	}
	if len(assistantTurns) != 2 {
		t.Fatalf("EventAssistantMessage count = %d, want 2 (one per turn)", len(assistantTurns))
	}
	first := assistantTurns[0]
	calls, ok := first.Payload["tool_calls"].([]map[string]any)
	if !ok {
		t.Fatalf("first assistant payload tool_calls = %#v, want []map[string]any", first.Payload["tool_calls"])
	}
	if len(calls) != 1 || calls[0]["id"] != "call_1" {
		t.Fatalf("first turn tool_calls = %#v, want call_1", calls)
	}
	second := assistantTurns[1]
	if !strings.Contains(strings.ToLower(second.Text), "all done") {
		t.Fatalf("final assistant text = %q, want all done", second.Text)
	}
}
