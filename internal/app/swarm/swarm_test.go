package swarm

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Luew2/FreeCode/internal/core/agent"
	"github.com/Luew2/FreeCode/internal/core/permission"
	"github.com/Luew2/FreeCode/internal/core/session"
)

func TestSwarmPlansRunsAndLogsAgentHandoffs(t *testing.T) {
	log := &memoryLog{}
	runner := &scriptedRunner{}

	response, err := UseCase{
		Agents: agent.DefaultDefinitions(),
		Runner: runner,
		Log:    log,
		Now:    func() time.Time { return time.Unix(10, 0) },
	}.Run(context.Background(), Request{
		SessionID: "s1",
		Goal:      "add a TUI",
		Approval:  permission.ModeAuto,
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if response.Status != agent.StatusCompleted {
		t.Fatalf("status = %q, want completed", response.Status)
	}
	if len(response.Plan) != 5 {
		t.Fatalf("plan len = %d, want 5", len(response.Plan))
	}
	if response.Plan[1].Role != agent.RoleExplorer {
		t.Fatalf("plan[1] = %#v, want explorer", response.Plan[1])
	}
	if response.Plan[2].Role != agent.RoleWorker || response.Plan[2].Autonomy.Mode != agent.AutonomySwarm || response.Plan[2].Autonomy.Approval != permission.ModeAuto {
		t.Fatalf("worker task = %#v, want swarm auto worker", response.Plan[2])
	}
	if response.Plan[2].Permissions.Write != permission.DecisionAllow {
		t.Fatalf("worker write decision = %q, want allow under auto", response.Plan[2].Permissions.Write)
	}
	if len(response.Plan[2].AllowedPaths) == 0 {
		t.Fatalf("worker task = %#v, want assigned write scope", response.Plan[2])
	}
	if len(runner.tasks) != 5 {
		t.Fatalf("runner tasks = %d, want 5", len(runner.tasks))
	}
	if !contains(runner.tasks[2].Goal, "orchestrator done") || !contains(runner.tasks[3].Goal, "README.md") || !contains(runner.tasks[4].Goal, "go test ./...") {
		t.Fatalf("task handoffs = %#v, want prior summaries, changed files, and verification", runner.tasks)
	}
	if len(log.events) < 6 {
		t.Fatalf("events = %#v, want swarm and task events", log.events)
	}
	foundReviewer := false
	for _, event := range log.events {
		if event.Actor == "reviewer" && event.Payload["status"] == string(agent.StatusCompleted) {
			foundReviewer = true
		}
	}
	if !foundReviewer {
		t.Fatalf("events = %#v, want reviewer completion event", log.events)
	}
}

func TestSwarmStopsWhenReviewerBlocks(t *testing.T) {
	runner := &scriptedRunner{blockedRole: agent.RoleReviewer}

	response, err := UseCase{
		Agents: agent.DefaultDefinitions(),
		Runner: runner,
	}.Run(context.Background(), Request{Goal: "ship it", Approval: permission.ModeAsk})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if response.Status != agent.StatusBlocked {
		t.Fatalf("status = %q, want blocked", response.Status)
	}
	if len(response.Results) != 5 {
		t.Fatalf("results = %d, want stop after reviewer result", len(response.Results))
	}
}

type scriptedRunner struct {
	tasks       []agent.Task
	blockedRole agent.Role
}

func (r *scriptedRunner) RunAgent(ctx context.Context, task agent.Task) (agent.Result, error) {
	r.tasks = append(r.tasks, task)
	status := agent.StatusCompleted
	if task.Role == r.blockedRole {
		status = agent.StatusBlocked
	}
	return agent.Result{
		TaskID:       task.ID,
		Role:         task.Role,
		Status:       status,
		Summary:      string(task.Role) + " done",
		ChangedFiles: changedFilesFor(task.Role),
		TestsRun:     testsRunFor(task.Role),
	}, nil
}

func changedFilesFor(role agent.Role) []string {
	if role == agent.RoleWorker {
		return []string{"README.md"}
	}
	return nil
}

func testsRunFor(role agent.Role) []string {
	if role == agent.RoleVerifier {
		return []string{"go test ./..."}
	}
	return nil
}

func contains(value string, needle string) bool {
	return strings.Contains(value, needle)
}

type memoryLog struct {
	events []session.Event
}

func (l *memoryLog) Append(ctx context.Context, event session.Event) error {
	l.events = append(l.events, event)
	return nil
}

func (l *memoryLog) Stream(ctx context.Context, id session.ID) (<-chan session.Event, error) {
	ch := make(chan session.Event)
	close(ch)
	return ch, nil
}
