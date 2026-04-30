package swarm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Luew2/FreeCode/internal/core/agent"
	"github.com/Luew2/FreeCode/internal/core/permission"
	"github.com/Luew2/FreeCode/internal/core/session"
	"github.com/Luew2/FreeCode/internal/ports"
)

type Runner interface {
	RunAgent(ctx context.Context, task agent.Task) (agent.Result, error)
}

type UseCase struct {
	Agents []agent.Definition
	Runner Runner
	Log    ports.EventLog
	Now    func() time.Time
}

type Request struct {
	SessionID session.ID
	Goal      string
	Approval  permission.Mode
	MaxSteps  int
}

type Response struct {
	Plan    []agent.Task
	Results []agent.Result
	Status  agent.Status
}

func (u UseCase) Run(ctx context.Context, request Request) (Response, error) {
	goal := strings.TrimSpace(request.Goal)
	if goal == "" {
		return Response{}, errors.New("swarm goal is required")
	}
	if request.SessionID == "" {
		request.SessionID = "default"
	}
	if request.Approval == "" {
		request.Approval = permission.ModeAsk
	}
	plan, err := u.plan(goal, request.Approval, request.MaxSteps)
	if err != nil {
		return Response{}, err
	}
	if err := u.log(ctx, request.SessionID, "swarm", agent.StatusRunning, goal, map[string]any{"tasks": taskPayloads(plan)}); err != nil {
		return Response{}, err
	}
	if u.Runner == nil {
		return Response{}, errors.New("swarm runner is required")
	}

	response := Response{Plan: plan, Status: agent.StatusCompleted}
	for _, task := range plan {
		task = enrichTaskWithHandoffs(task, response.Results)
		if err := u.log(ctx, request.SessionID, task.Agent, agent.StatusRunning, task.Goal, taskPayload(task)); err != nil {
			return Response{}, err
		}
		result, err := u.Runner.RunAgent(ctx, task)
		if err != nil {
			result = agent.Result{TaskID: task.ID, Role: task.Role, Status: agent.StatusFailed, Summary: err.Error()}
		}
		response.Results = append(response.Results, result)
		if err := u.log(ctx, request.SessionID, task.Agent, result.Status, result.Summary, resultPayload(result)); err != nil {
			return Response{}, err
		}
		switch result.Status {
		case agent.StatusBlocked, agent.StatusFailed:
			response.Status = result.Status
			return response, nil
		}
	}
	if err := u.log(ctx, request.SessionID, "swarm", response.Status, "swarm completed", map[string]any{"status": string(response.Status)}); err != nil {
		return Response{}, err
	}
	return response, nil
}

func (u UseCase) plan(goal string, approval permission.Mode, maxSteps int) ([]agent.Task, error) {
	defs := definitionsByRole(u.Agents)
	roles := []agent.Role{agent.RoleOrchestrator, agent.RoleExplorer, agent.RoleWorker, agent.RoleVerifier, agent.RoleReviewer}
	tasks := make([]agent.Task, 0, len(roles))
	for i, role := range roles {
		definition, ok := defs[role]
		if !ok {
			return nil, fmt.Errorf("agent role %q is not configured", role)
		}
		task := taskFromDefinition(i+1, goal, definition, approval)
		if maxSteps > 0 {
			task.Budget.MaxSteps = maxSteps
		}
		tasks = append(tasks, task)
	}
	return tasks, nil
}

func taskFromDefinition(index int, goal string, definition agent.Definition, approval permission.Mode) agent.Task {
	workspace := agent.WorkspaceSameTree
	if definition.Role == agent.RoleReviewer {
		workspace = agent.WorkspaceReadOnly
	}
	allowedPaths := append([]string(nil), definition.Permissions.AllowedPaths...)
	if definition.Role == agent.RoleWorker && len(allowedPaths) == 0 {
		allowedPaths = []string{"."}
	}
	policy := permission.MergePolicyWithMode(definition.Permissions, approval)
	policy.AllowedPaths = append([]string(nil), allowedPaths...)
	budget := agent.DefaultBudget()
	if definition.MaxSteps > 0 {
		budget.MaxSteps = definition.MaxSteps
	}
	return agent.Task{
		ID:           agent.ID(fmt.Sprintf("task-%d", index)),
		Goal:         roleGoal(goal, definition.Role),
		Role:         definition.Role,
		Agent:        definition.Name,
		Workspace:    workspace,
		AllowedPaths: allowedPaths,
		DeniedPaths:  append([]string(nil), definition.Permissions.DeniedPaths...),
		Permissions:  policy,
		Autonomy: agent.Autonomy{
			Mode:             agent.AutonomySwarm,
			Approval:         approval,
			StopForQuestions: true,
		},
		Budget: budget,
		Handoff: agent.HandoffRequirements{
			ChangedFiles:  definition.Role == agent.RoleWorker,
			TestsRun:      definition.Role == agent.RoleVerifier,
			OpenQuestions: true,
		},
	}
}

func roleGoal(goal string, role agent.Role) string {
	switch role {
	case agent.RoleOrchestrator:
		return "Create an execution plan for: " + goal
	case agent.RoleExplorer:
		return "Inspect the codebase and report relevant context for: " + goal
	case agent.RoleWorker:
		return "Implement the approved plan for: " + goal
	case agent.RoleVerifier:
		return "Run focused verification for: " + goal
	case agent.RoleReviewer:
		return "Review the completed work for: " + goal
	default:
		return goal
	}
}

func enrichTaskWithHandoffs(task agent.Task, results []agent.Result) agent.Task {
	if len(results) == 0 {
		return task
	}
	var context []string
	var changedFiles []string
	var testsRun []string
	var findings []string
	for _, result := range results {
		if strings.TrimSpace(result.Summary) != "" {
			context = append(context, fmt.Sprintf("%s: %s", result.Role, result.Summary))
		}
		changedFiles = appendUnique(changedFiles, result.ChangedFiles...)
		testsRun = appendUnique(testsRun, result.TestsRun...)
		findings = appendUnique(findings, result.Findings...)
	}
	if len(context) > 0 {
		task.Goal += "\n\nPrior handoffs:\n- " + strings.Join(context, "\n- ")
	}
	switch task.Role {
	case agent.RoleVerifier:
		if len(changedFiles) > 0 {
			task.Goal += "\nChanged files to verify: " + strings.Join(changedFiles, ", ")
		}
	case agent.RoleReviewer:
		if len(changedFiles) > 0 {
			task.Goal += "\nChanged files to review: " + strings.Join(changedFiles, ", ")
		}
		if len(testsRun) > 0 {
			task.Goal += "\nVerification already run: " + strings.Join(testsRun, ", ")
		}
		if len(findings) > 0 {
			task.Goal += "\nExisting findings: " + strings.Join(findings, "; ")
		}
	}
	return task
}

func appendUnique(values []string, additions ...string) []string {
	seen := map[string]bool{}
	for _, value := range values {
		seen[value] = true
	}
	for _, value := range additions {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		values = append(values, value)
		seen[value] = true
	}
	return values
}

func definitionsByRole(definitions []agent.Definition) map[agent.Role]agent.Definition {
	if len(definitions) == 0 {
		definitions = agent.DefaultDefinitions()
	}
	result := map[agent.Role]agent.Definition{}
	for _, definition := range definitions {
		if _, exists := result[definition.Role]; !exists {
			result[definition.Role] = definition
		}
	}
	return result
}

func (u UseCase) log(ctx context.Context, sessionID session.ID, actor string, status agent.Status, text string, payload map[string]any) error {
	if u.Log == nil {
		return nil
	}
	if payload == nil {
		payload = map[string]any{}
	}
	payload["status"] = string(status)
	return u.Log.Append(ctx, session.Event{
		ID:        session.EventID(fmt.Sprintf("swarm-%s-%s-%d", actor, status, u.now().UnixNano())),
		SessionID: sessionID,
		Type:      session.EventAgent,
		At:        u.now(),
		Actor:     actor,
		Text:      text,
		Payload:   payload,
	})
}

func (u UseCase) now() time.Time {
	if u.Now != nil {
		return u.Now()
	}
	return time.Now()
}

func taskPayloads(tasks []agent.Task) []map[string]any {
	payloads := make([]map[string]any, 0, len(tasks))
	for _, task := range tasks {
		payloads = append(payloads, taskPayload(task))
	}
	return payloads
}

func taskPayload(task agent.Task) map[string]any {
	return map[string]any{
		"task_id":       string(task.ID),
		"agent":         task.Agent,
		"role":          string(task.Role),
		"goal":          task.Goal,
		"workspace":     string(task.Workspace),
		"approval":      string(task.Autonomy.Approval),
		"max_steps":     task.Budget.MaxSteps,
		"changed_files": task.Handoff.ChangedFiles,
		"tests_run":     task.Handoff.TestsRun,
	}
}

func resultPayload(result agent.Result) map[string]any {
	return map[string]any{
		"task_id":        string(result.TaskID),
		"role":           string(result.Role),
		"status":         string(result.Status),
		"changed_files":  append([]string(nil), result.ChangedFiles...),
		"tests_run":      append([]string(nil), result.TestsRun...),
		"findings":       append([]string(nil), result.Findings...),
		"open_questions": append([]string(nil), result.OpenQuestions...),
	}
}
