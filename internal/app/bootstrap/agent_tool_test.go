package bootstrap

import (
	"context"
	"strings"
	"testing"

	coremodel "github.com/Luew2/FreeCode/internal/core/model"
	"github.com/Luew2/FreeCode/internal/core/session"
)

func TestMetadataEventLogTagsDirectedAgentEvents(t *testing.T) {
	inner := &recordingEventLog{}
	log := metadataEventLog{
		inner: inner,
		metadata: map[string]any{
			"agent_id":     "a1",
			"task_session": "task-1",
			"role":         "worker",
		},
	}

	err := log.Append(context.Background(), session.Event{
		ID:      "e1",
		Type:    session.EventUserMessage,
		Actor:   "user",
		Text:    "continue",
		Payload: map[string]any{"step": 1},
	})
	if err != nil {
		t.Fatalf("Append returned error: %v", err)
	}
	if len(inner.events) != 1 {
		t.Fatalf("events = %#v, want one event", inner.events)
	}
	payload := inner.events[0].Payload
	if payload["agent_id"] != "a1" || payload["task_session"] != "task-1" || payload["role"] != "worker" || payload["step"] != 1 {
		t.Fatalf("payload = %#v, want directed metadata plus original step", payload)
	}
}

func TestDelegatingToolsAdvertisesSpawnAgent(t *testing.T) {
	tools := delegatingTools{}
	specs := tools.Tools()
	found := false
	for _, spec := range specs {
		if spec.Name == "spawn_agent" {
			found = true
			if !strings.Contains(spec.Description, "subagent") {
				t.Fatalf("spawn_agent description = %q, want subagent language", spec.Description)
			}
		}
	}
	if !found {
		t.Fatalf("tools = %#v, want spawn_agent", specs)
	}
}

func TestDelegatingToolsHideSpawnAgentAtDepthLimit(t *testing.T) {
	tools := delegatingTools{depth: 2, maxDepth: 2}
	for _, spec := range tools.Tools() {
		if spec.Name == "spawn_agent" {
			t.Fatalf("tools = %#v, want no spawn_agent at depth limit", tools.Tools())
		}
	}
	_, err := tools.RunTool(context.Background(), coremodel.ToolCall{ID: "call_1", Name: "spawn_agent", Arguments: []byte(`{"role":"explorer","task":"read"}`)})
	if err == nil || !strings.Contains(err.Error(), "depth limit") {
		t.Fatalf("RunTool error = %v, want depth limit", err)
	}
}

func TestSwarmDelegationContextAsksMainToSpawnDynamicAgents(t *testing.T) {
	prompt := swarmDelegationContext("auto", nil)
	if !strings.Contains(prompt, "use spawn_agent") ||
		!strings.Contains(prompt, "Do not use a fixed agent count") ||
		!strings.Contains(prompt, "as many bounded tasks as the request actually needs") ||
		!strings.Contains(prompt, "orchestrator child agents") {
		t.Fatalf("context = %q, want dynamic main-owned swarm instructions", prompt)
	}
	if strings.Contains(prompt, "ship feature") {
		t.Fatalf("context contains user goal verbatim; should be turn-context only:\n%s", prompt)
	}
}

type recordingEventLog struct {
	events []session.Event
}

func (l *recordingEventLog) Append(ctx context.Context, event session.Event) error {
	l.events = append(l.events, event)
	return nil
}

func (l *recordingEventLog) Stream(ctx context.Context, id session.ID) (<-chan session.Event, error) {
	ch := make(chan session.Event)
	close(ch)
	return ch, nil
}
