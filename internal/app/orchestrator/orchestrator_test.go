package orchestrator

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Luew2/FreeCode/internal/app/contextmgr"
	"github.com/Luew2/FreeCode/internal/app/prompt"
	"github.com/Luew2/FreeCode/internal/core/artifact"
	"github.com/Luew2/FreeCode/internal/core/model"
	"github.com/Luew2/FreeCode/internal/core/session"
	"github.com/Luew2/FreeCode/internal/ports"
)

func TestRunnerLoopsThroughToolCalls(t *testing.T) {
	client := &scriptedModelClient{
		scripts: [][]model.Event{
			{
				{Type: model.EventStarted},
				{Type: model.EventToolCall, ToolCall: &model.ToolCall{ID: "call_1", Name: "read_file", Arguments: []byte(`{"path":"README.md"}`)}},
				{Type: model.EventCompleted},
			},
			{
				{Type: model.EventStarted},
				{Type: model.EventTextDelta, Text: "repo summary"},
				{Type: model.EventCompleted},
			},
		},
	}
	tools := &fakeTools{result: "file contents"}
	log := &memoryEventLog{}

	response, err := Runner{
		Model: client,
		Tools: tools,
		Log:   log,
		Now:   func() time.Time { return time.Unix(1, 0).UTC() },
	}.Run(context.Background(), Request{
		SessionID:   "s1",
		Model:       model.NewRef("local", "coder"),
		UserRequest: "summarize",
		Environment: prompt.Environment{
			WorkspaceRoot: "/repo",
		},
		MaxSteps: 4,
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if response.Text != "repo summary" {
		t.Fatalf("response text = %q, want repo summary", response.Text)
	}
	if response.ToolCalls != 1 {
		t.Fatalf("tool calls = %d, want 1", response.ToolCalls)
	}
	if tools.calls != 1 {
		t.Fatalf("tool calls executed = %d, want 1", tools.calls)
	}
	if len(client.requests) != 2 {
		t.Fatalf("model requests = %d, want 2", len(client.requests))
	}

	second := client.requests[1]
	if len(second.Messages) < 2 {
		t.Fatalf("second request messages = %d, want tool loop history", len(second.Messages))
	}
	assistant := second.Messages[len(second.Messages)-2]
	if assistant.Role != model.RoleAssistant || len(assistant.ToolCalls) != 1 {
		t.Fatalf("assistant history = %#v, want tool call history", assistant)
	}
	tool := second.Messages[len(second.Messages)-1]
	if tool.Role != model.RoleTool || tool.ToolCallID != "call_1" || !strings.Contains(tool.Content[0].Text, "file contents") {
		t.Fatalf("tool history = %#v, want tool result", tool)
	}

	foundToolEvent := false
	for _, event := range log.events {
		if event.Type == session.EventTool && event.Text == "file contents" {
			foundToolEvent = true
		}
	}
	if !foundToolEvent {
		t.Fatalf("events = %#v, want persisted tool event", log.events)
	}
}

func TestRunnerRequiresTerminalReadAfterTerminalWrite(t *testing.T) {
	client := &scriptedModelClient{
		scripts: [][]model.Event{
			{
				{Type: model.EventToolCall, ToolCall: &model.ToolCall{ID: "call_1", Name: "terminal_write", Arguments: []byte(`{"command":"ls"}`)}},
				{Type: model.EventCompleted},
			},
			{
				{Type: model.EventTextDelta, Text: "done"},
				{Type: model.EventCompleted},
			},
		},
	}
	_, err := Runner{
		Model: client,
		Tools: &fakeTools{
			result: "terminal 1 sent command",
			specs:  []model.ToolSpec{{Name: "terminal_write"}, {Name: "terminal_read"}},
		},
	}.Run(context.Background(), Request{
		SessionID:   "s1",
		Model:       model.NewRef("local", "coder"),
		UserRequest: "run ls",
		MaxSteps:    3,
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if len(client.requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(client.requests))
	}
	found := false
	for _, message := range client.requests[1].Messages {
		if message.Role != model.RoleDeveloper {
			continue
		}
		for _, part := range message.Content {
			if strings.Contains(part.Text, "call terminal_read") {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("second request messages = %#v, want terminal_read follow-up instruction", client.requests[1].Messages)
	}
}

func TestRunnerRetriesPromiseWithoutToolUse(t *testing.T) {
	client := &scriptedModelClient{
		scripts: [][]model.Event{
			{
				{Type: model.EventTextDelta, Text: "I'll start by exploring the codebase structure."},
				{Type: model.EventCompleted},
			},
			{
				{Type: model.EventToolCall, ToolCall: &model.ToolCall{ID: "call_1", Name: "read_file", Arguments: []byte(`{"path":"README.md"}`)}},
				{Type: model.EventCompleted},
			},
			{
				{Type: model.EventTextDelta, Text: "done"},
				{Type: model.EventCompleted},
			},
		},
	}
	tools := &fakeTools{result: "readme", specs: []model.ToolSpec{{Name: "read_file"}}}
	response, err := Runner{
		Model: client,
		Tools: tools,
	}.Run(context.Background(), Request{
		SessionID:   "s1",
		Model:       model.NewRef("local", "coder"),
		UserRequest: "review",
		MaxSteps:    4,
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if response.Text != "done" || response.ToolCalls != 1 || tools.calls != 1 {
		t.Fatalf("response=%#v calls=%d, want final after forced tool follow-through", response, tools.calls)
	}
	if len(client.requests) != 3 {
		t.Fatalf("requests = %d, want promise retry, tool turn, final", len(client.requests))
	}
	found := false
	for _, message := range client.requests[1].Messages {
		if message.Role != model.RoleDeveloper {
			continue
		}
		for _, part := range message.Content {
			if strings.Contains(part.Text, "did not call a tool") {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("second request messages = %#v, want tool follow-through instruction", client.requests[1].Messages)
	}
}

func TestRunnerStopsAtMaxSteps(t *testing.T) {
	_, err := Runner{
		Model: &scriptedModelClient{
			scripts: [][]model.Event{
				{{Type: model.EventToolCall, ToolCall: &model.ToolCall{ID: "call_1", Name: "read_file", Arguments: []byte(`{"path":"a"}`)}}},
			},
		},
		Tools: &fakeTools{result: "again"},
	}.Run(context.Background(), Request{
		Model:       model.NewRef("local", "coder"),
		UserRequest: "loop",
		MaxSteps:    1,
	})
	if err == nil {
		t.Fatal("Run returned nil error")
	}
	if !strings.Contains(err.Error(), "stopped after 1 steps") {
		t.Fatalf("error = %q, want max steps message", err.Error())
	}
}

func TestRunnerLogsToolArtifactPayload(t *testing.T) {
	client := &scriptedModelClient{
		scripts: [][]model.Event{
			{
				{Type: model.EventToolCall, ToolCall: &model.ToolCall{ID: "call_1", Name: "apply_patch", Arguments: []byte(`{}`)}},
				{Type: model.EventCompleted},
			},
			{
				{Type: model.EventTextDelta, Text: "done"},
				{Type: model.EventCompleted},
			},
		},
	}
	log := &memoryEventLog{}
	patchArtifact := &artifact.Artifact{
		ID:       artifact.NewID(artifact.KindPatch, 1),
		Kind:     artifact.KindPatch,
		Title:    "Patch",
		Body:     "--- a/README.md\n+++ b/README.md\n@@\n-old\n+new\n",
		URI:      "patch:p1",
		Metadata: map[string]string{"changed_files": "README.md"},
	}

	_, err := Runner{
		Model: client,
		Tools: &fakeTools{
			result:   "applied patch p1",
			artifact: patchArtifact,
			metadata: map[string]string{"patch_id": "p1"},
		},
		Log: log,
	}.Run(context.Background(), Request{
		SessionID:   "s1",
		Model:       model.NewRef("local", "coder"),
		UserRequest: "patch",
		MaxSteps:    4,
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	for _, event := range log.events {
		if event.Type != session.EventTool {
			continue
		}
		artifactValue, ok := event.Payload["artifact"].(map[string]any)
		if !ok {
			t.Fatalf("tool payload = %#v, want artifact payload", event.Payload)
		}
		if artifactValue["id"] != "p1" || artifactValue["kind"] != "patch" || artifactValue["body"] == "" || event.Payload["patch_id"] != "p1" {
			t.Fatalf("tool payload = %#v, want patch artifact metadata", event.Payload)
		}
		return
	}
	t.Fatalf("events = %#v, want tool event", log.events)
}

func TestRunnerReturnsLogAppendError(t *testing.T) {
	_, err := Runner{
		Model: &scriptedModelClient{
			scripts: [][]model.Event{
				{{Type: model.EventTextDelta, Text: "never reached"}},
			},
		},
		Log: &failingEventLog{err: errors.New("disk full")},
	}.Run(context.Background(), Request{
		SessionID:   "s1",
		Model:       model.NewRef("local", "coder"),
		UserRequest: "summarize",
	})
	if err == nil {
		t.Fatal("Run returned nil error")
	}
	if !strings.Contains(err.Error(), "append session event") || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("error = %q, want append failure", err.Error())
	}
}

func TestRunnerUsesSessionContextAndTokenBudget(t *testing.T) {
	client := &scriptedModelClient{
		scripts: [][]model.Event{
			{
				{Type: model.EventTextDelta, Text: "done"},
				{Type: model.EventCompleted},
			},
		},
	}
	log := &memoryEventLog{}
	_, err := Runner{
		Model: client,
		Tools: &fakeTools{result: "unused"},
		Log:   log,
	}.Run(context.Background(), Request{
		SessionID:      "s1",
		Model:          model.NewRef("local", "coder"),
		UserRequest:    "current request",
		SessionContext: "prior context",
		TurnContext:    "shared terminal output",
		ContextBudget:  contextmgr.Budget{MaxInputTokens: 640, MaxOutputTokens: 64},
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if len(client.requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(client.requests))
	}
	request := client.requests[0]
	if request.MaxOutputTokens != 64 {
		t.Fatalf("MaxOutputTokens = %d, want 64", request.MaxOutputTokens)
	}
	if contextmgr.EstimateMessages(request.Messages)+contextmgr.EstimateTools(request.Tools) > 640 {
		t.Fatalf("request exceeded budget: %#v", request.Messages)
	}
	foundSessionContext := false
	for _, message := range request.Messages {
		if message.Role == model.RoleDeveloper && len(message.Content) > 0 && strings.Contains(message.Content[0].Text, "Session context") {
			foundSessionContext = true
			if !strings.Contains(message.Content[0].Text, "Active turn context") || !strings.Contains(message.Content[0].Text, "shared terminal output") {
				t.Fatalf("session context message = %q, want active turn context", message.Content[0].Text)
			}
		}
	}
	if !foundSessionContext {
		t.Fatalf("messages = %#v, want session context", request.Messages)
	}
}

type scriptedModelClient struct {
	requests []model.Request
	scripts  [][]model.Event
}

func (c *scriptedModelClient) Stream(ctx context.Context, request model.Request) (<-chan model.Event, error) {
	c.requests = append(c.requests, request)
	if len(c.scripts) == 0 {
		return nil, errors.New("no scripted response")
	}
	events := c.scripts[0]
	c.scripts = c.scripts[1:]

	ch := make(chan model.Event, len(events))
	for _, event := range events {
		ch <- event
	}
	close(ch)
	return ch, nil
}

type fakeTools struct {
	calls    int
	result   string
	artifact *artifact.Artifact
	metadata map[string]string
	specs    []model.ToolSpec
}

func (t *fakeTools) Tools() []model.ToolSpec {
	if len(t.specs) > 0 {
		return t.specs
	}
	return []model.ToolSpec{{Name: "read_file"}}
}

func (t *fakeTools) RunTool(ctx context.Context, call model.ToolCall) (ports.ToolResult, error) {
	t.calls++
	return ports.ToolResult{CallID: call.ID, Content: t.result, Artifact: t.artifact, Metadata: t.metadata}, nil
}

type memoryEventLog struct {
	events []session.Event
}

func (l *memoryEventLog) Append(ctx context.Context, event session.Event) error {
	l.events = append(l.events, event)
	return nil
}

func (l *memoryEventLog) Stream(ctx context.Context, id session.ID) (<-chan session.Event, error) {
	ch := make(chan session.Event)
	close(ch)
	return ch, nil
}

type failingEventLog struct {
	err error
}

func (l *failingEventLog) Append(ctx context.Context, event session.Event) error {
	return l.err
}

func (l *failingEventLog) Stream(ctx context.Context, id session.ID) (<-chan session.Event, error) {
	ch := make(chan session.Event)
	close(ch)
	return ch, nil
}
