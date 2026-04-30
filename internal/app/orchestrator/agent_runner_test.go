package orchestrator

import (
	"context"
	"strings"
	"testing"

	"github.com/Luew2/FreeCode/internal/core/agent"
	"github.com/Luew2/FreeCode/internal/core/model"
)

func TestAgentRunnerAddsRolePromptAndRunsTask(t *testing.T) {
	client := &scriptedModelClient{
		scripts: [][]model.Event{{
			{Type: model.EventTextDelta, Text: `{"status":"blocked","summary":"review failed","findings":["bug"]}`},
			{Type: model.EventCompleted},
		}},
	}
	log := &memoryEventLog{}
	result, err := AgentRunner{
		Model:  model.NewRef("local", "coder"),
		Client: client,
		Log:    log,
		Trace:  agent.Trace{ParentSession: "parent"},
	}.RunAgent(context.Background(), agent.Task{
		ID:           "task-1",
		Role:         agent.RoleReviewer,
		Agent:        "reviewer",
		Goal:         "review the change",
		AllowedPaths: []string{"internal"},
		Permissions:  agent.DefaultDefinitions()[3].Permissions,
		Budget:       agent.Budget{MaxSteps: 2},
	})
	if err != nil {
		t.Fatalf("RunAgent returned error: %v", err)
	}
	if result.Status != agent.StatusBlocked || result.Summary != "review failed" || len(result.Findings) != 1 {
		t.Fatalf("result = %#v, want parsed blocked review", result)
	}
	if len(client.requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(client.requests))
	}
	foundReviewerPrompt := false
	foundTaskPacket := false
	for _, message := range client.requests[0].Messages {
		for _, part := range message.Content {
			if strings.Contains(part.Text, "reviewer for a FreeCode swarm run") {
				foundReviewerPrompt = true
			}
			if strings.Contains(part.Text, `"allowed_paths"`) && strings.Contains(part.Text, `"internal"`) && strings.Contains(part.Text, `"handoff_required"`) {
				foundTaskPacket = true
			}
		}
	}
	if !foundReviewerPrompt {
		t.Fatalf("messages = %#v, want reviewer role prompt", client.requests[0].Messages)
	}
	if !foundTaskPacket {
		t.Fatalf("messages = %#v, want full task packet", client.requests[0].Messages)
	}
	if strings.Contains(fmtMessages(client.requests[0].Messages), "Do not implement writes") {
		t.Fatalf("messages = %#v, should not include default read-only prompt", client.requests[0].Messages)
	}
	if len(log.events) == 0 || log.events[0].SessionID != "parent" || log.events[0].Actor != "reviewer" {
		t.Fatalf("events = %#v, want traced parent-session reviewer events", log.events)
	}
}

func TestAgentRunnerOrchestratorPromptEncouragesChildAgents(t *testing.T) {
	client := &scriptedModelClient{
		scripts: [][]model.Event{{
			{Type: model.EventTextDelta, Text: `{"status":"completed","summary":"planned"}`},
			{Type: model.EventCompleted},
		}},
	}
	_, err := AgentRunner{
		Model:  model.NewRef("local", "coder"),
		Client: client,
		Log:    &memoryEventLog{},
		Trace:  agent.Trace{ParentSession: "parent"},
	}.RunAgent(context.Background(), agent.Task{
		ID:          "task-orch",
		Role:        agent.RoleOrchestrator,
		Agent:       "orchestrator",
		Goal:        "coordinate feature",
		Permissions: agent.DefaultDefinitions()[0].Permissions,
		Budget:      agent.Budget{MaxSteps: 2},
	})
	if err != nil {
		t.Fatalf("RunAgent returned error: %v", err)
	}
	text := fmtMessages(client.requests[0].Messages)
	if !strings.Contains(text, "use spawn_agent when available") ||
		!strings.Contains(text, "delegate independent child tasks") ||
		!strings.Contains(text, "Synthesize every child handoff") {
		t.Fatalf("messages = %q, want nested orchestration guidance", text)
	}
}

func fmtMessages(messages []model.Message) string {
	var out strings.Builder
	for _, message := range messages {
		for _, part := range message.Content {
			out.WriteString(part.Text)
			out.WriteByte('\n')
		}
	}
	return out.String()
}
