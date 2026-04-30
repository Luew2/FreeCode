package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/Luew2/FreeCode/internal/adapters/tools/builtin"
	"github.com/Luew2/FreeCode/internal/app/orchestrator"
	"github.com/Luew2/FreeCode/internal/app/prompt"
	"github.com/Luew2/FreeCode/internal/core/agent"
	"github.com/Luew2/FreeCode/internal/core/model"
	"github.com/Luew2/FreeCode/internal/core/permission"
	"github.com/Luew2/FreeCode/internal/core/session"
	"github.com/Luew2/FreeCode/internal/ports"
)

type delegatingTools struct {
	inner     ports.ToolRegistry
	runner    orchestrator.AgentRunner
	log       ports.EventLog
	sessionID session.ID
	approval  permission.Mode
	agents    []agent.Definition
	parentID  string
	depth     int
	maxDepth  int
	depths    *sync.Map
	now       func() time.Time
}

const defaultMaxDelegationDepth = 2

type spawnAgentArgs struct {
	Role          string   `json:"role"`
	Agent         string   `json:"agent"`
	Task          string   `json:"task"`
	Prompt        string   `json:"prompt"`
	AllowedPaths  []string `json:"allowed_paths"`
	DeniedPaths   []string `json:"denied_paths"`
	MaxSteps      int      `json:"max_steps"`
	ParentAgentID string   `json:"parent_agent_id"`
}

func withDelegationTools(ctx context.Context, bundle runtimeBundle, log ports.EventLog, sessionID session.ID, approval permission.Mode, parentID string, inner ports.ToolRegistry) (ports.ToolRegistry, error) {
	client, err := buildModelClient(bundle.Settings.Provider)
	if err != nil {
		return nil, err
	}
	depths := &sync.Map{}
	var runner orchestrator.AgentRunner
	runner = orchestrator.AgentRunner{
		Model:  bundle.Settings.Model,
		Client: client,
		ToolsByRole: map[agent.Role]ports.ToolRegistry{
			agent.RoleOrchestrator: bundle.ReadTools,
			agent.RoleExplorer:     bundle.ReadTools,
			agent.RoleWorker:       bundle.WriteTools,
			agent.RoleVerifier:     bundle.VerifyTools,
			agent.RoleReviewer:     bundle.ReadTools,
		},
		Log: log,
		Trace: agent.Trace{
			ParentSession: sessionID,
		},
		ToolsForTask: func(task agent.Task) ports.ToolRegistry {
			taskDepth := taskDepth(depths, task.ID)
			var roleTools ports.ToolRegistry
			if task.Role == agent.RoleVerifier {
				policy := permission.MergePolicyWithMode(task.Permissions, task.Autonomy.Approval)
				if task.Autonomy.Approval == permission.ModeAuto {
					policy.Shell = permission.DecisionAllow
				}
				roleTools = builtin.NewVerifier(bundle.Workspace.FileSystem(), builtin.NewStaticPermissionGate(policy))
			} else if task.Role == agent.RoleWorker {
				policy := permission.MergePolicyWithMode(task.Permissions, task.Autonomy.Approval)
				policy.AllowedPaths = append([]string(nil), task.AllowedPaths...)
				policy.DeniedPaths = append([]string(nil), task.DeniedPaths...)
				roleTools = builtin.NewWritable(bundle.Workspace.FileSystem(), builtin.NewStaticPermissionGate(policy))
			} else {
				roleTools = bundle.ReadTools
			}
			if task.Role == agent.RoleOrchestrator {
				return delegatingTools{
					inner:     roleTools,
					runner:    runner,
					log:       log,
					sessionID: sessionID,
					approval:  approval,
					agents:    bundle.Settings.Agents,
					parentID:  string(task.ID),
					depth:     taskDepth,
					maxDepth:  defaultMaxDelegationDepth,
					depths:    depths,
					now:       time.Now,
				}
			}
			return roleTools
		},
		Environment: prompt.Environment{
			WorkspaceRoot: bundle.Workspace.Root(),
			Shell:         os.Getenv("SHELL"),
			GitBranch:     gitBranch(ctx, bundle.Git),
			Platform:      runtime.GOOS + "/" + runtime.GOARCH,
			WritableRoots: []string{bundle.Workspace.Root()},
		},
		ContextBudget:           bundle.Settings.ContextBudget,
		SessionContextMaxTokens: 4096,
	}
	return delegatingTools{
		inner:     inner,
		runner:    runner,
		log:       log,
		sessionID: sessionID,
		approval:  approval,
		agents:    bundle.Settings.Agents,
		parentID:  strings.TrimSpace(parentID),
		maxDepth:  defaultMaxDelegationDepth,
		depths:    depths,
		now:       time.Now,
	}, nil
}

func (t delegatingTools) Tools() []model.ToolSpec {
	var tools []model.ToolSpec
	if t.inner != nil {
		tools = append(tools, t.inner.Tools()...)
	}
	if !t.canDelegate() {
		return tools
	}
	tools = append(tools, model.ToolSpec{
		Name: "spawn_agent",
		Description: "Spawn one bounded FreeCode subagent. The subagent runs with its own tool budget and returns a structured handoff. " +
			"Use the role to scope its capabilities: 'explorer' (read-only research), 'worker' (read+write+patch within allowed_paths), 'verifier' (run checks), 'reviewer' (read-only review), 'orchestrator' (nested coordination). " +
			"The task field must be concrete and actionable — include the specific files or directories to look at, the question to answer, and what shape the answer should take. Vague tasks like 'review the codebase' produce empty handoffs.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"role":            map[string]any{"type": "string", "enum": []string{"explorer", "worker", "verifier", "reviewer", "orchestrator"}, "description": "Capability profile for the subagent."},
				"agent":           map[string]any{"type": "string", "description": "Optional configured agent name. Defaults from role."},
				"task":            map[string]any{"type": "string", "description": "Concrete bounded task. Name the files/dirs, the specific question, and the expected handoff shape (e.g. 'Read internal/foo/* and report the public API surface as a bulleted list')."},
				"prompt":          map[string]any{"type": "string", "description": "Alias for task."},
				"allowed_paths":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Paths the subagent may read/write under. Defaults to the workspace root for workers."},
				"denied_paths":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				"max_steps":       map[string]any{"type": "integer", "description": "Override the subagent's per-task step budget."},
				"parent_agent_id": map[string]any{"type": "string", "description": "Optional UI parent id. Defaults to main."},
			},
			"required": []string{"role", "task"},
		},
	})
	return tools
}

func (t delegatingTools) RunTool(ctx context.Context, call model.ToolCall) (ports.ToolResult, error) {
	if call.Name != "spawn_agent" {
		if t.inner == nil {
			return ports.ToolResult{}, fmt.Errorf("tool registry is not configured")
		}
		return t.inner.RunTool(ctx, call)
	}
	if !t.canDelegate() {
		return ports.ToolResult{}, fmt.Errorf("spawn_agent delegation depth limit reached")
	}
	var args spawnAgentArgs
	if err := json.Unmarshal(call.Arguments, &args); err != nil {
		return ports.ToolResult{}, fmt.Errorf("spawn_agent arguments: %w", err)
	}
	task, definition, err := t.agentTask(args)
	if err != nil {
		return ports.ToolResult{}, err
	}
	parentID := strings.TrimSpace(args.ParentAgentID)
	if parentID == "" {
		parentID = firstText(t.parentID, "main")
	}
	if t.depths != nil {
		t.depths.Store(string(task.ID), t.depth+1)
	}
	_ = t.logAgent(ctx, task, definition, agent.StatusRunning, task.Goal, parentID, agent.Result{})
	result, runErr := t.runner.RunAgent(ctx, task)
	if runErr != nil {
		result = agent.Result{TaskID: task.ID, Role: task.Role, Status: agent.StatusFailed, Summary: runErr.Error()}
	}
	_ = t.logAgent(ctx, task, definition, result.Status, result.Summary, parentID, result)
	content := agentResultContent(result)
	metadata := map[string]string{
		"task_id":         string(task.ID),
		"agent":           task.Agent,
		"role":            string(task.Role),
		"status":          string(result.Status),
		"parent_agent_id": parentID,
		"depth":           fmt.Sprintf("%d", t.depth+1),
	}
	if runErr != nil {
		metadata["error"] = runErr.Error()
	}
	return ports.ToolResult{CallID: call.ID, Content: content, Metadata: metadata}, runErr
}

func (t delegatingTools) agentTask(args spawnAgentArgs) (agent.Task, agent.Definition, error) {
	definition, role, err := selectAgentDefinition(t.agents, args.Role, args.Agent)
	if err != nil {
		return agent.Task{}, agent.Definition{}, err
	}
	goal := strings.TrimSpace(firstText(args.Task, args.Prompt))
	if goal == "" {
		return agent.Task{}, agent.Definition{}, fmt.Errorf("spawn_agent requires task")
	}
	budget := agent.DefaultBudget()
	if definition.MaxSteps > 0 {
		budget.MaxSteps = definition.MaxSteps
	}
	if args.MaxSteps > 0 {
		budget.MaxSteps = args.MaxSteps
	}
	policy := permission.MergePolicyWithMode(definition.Permissions, t.approval)
	task := agent.Task{
		ID:           agent.ID(fmt.Sprintf("task-%d", t.currentTime().UnixNano())),
		Goal:         goal,
		Role:         role,
		Agent:        firstText(definition.Name, string(role)),
		Workspace:    workspaceModeForRole(role),
		AllowedPaths: append([]string(nil), args.AllowedPaths...),
		DeniedPaths:  append([]string(nil), args.DeniedPaths...),
		Permissions:  policy,
		Autonomy: agent.Autonomy{
			Mode:             agent.AutonomyInteractive,
			Approval:         t.approval,
			StopForQuestions: true,
		},
		Budget: budget,
		Handoff: agent.HandoffRequirements{
			ChangedFiles:  role == agent.RoleWorker,
			TestsRun:      role == agent.RoleVerifier,
			OpenQuestions: true,
		},
	}
	if role == agent.RoleWorker && len(task.AllowedPaths) == 0 {
		task.AllowedPaths = []string{"."}
	}
	return task, definition, nil
}

func (t delegatingTools) logAgent(ctx context.Context, task agent.Task, definition agent.Definition, status agent.Status, text string, parentID string, result agent.Result) error {
	if t.log == nil {
		return nil
	}
	payload := map[string]any{
		"task_id":         string(task.ID),
		"agent":           task.Agent,
		"role":            string(task.Role),
		"status":          string(status),
		"parent_agent_id": parentID,
		"depth":           t.depth + 1,
		"goal":            task.Goal,
		"allowed_paths":   task.AllowedPaths,
		"denied_paths":    task.DeniedPaths,
	}
	if definition.Description != "" {
		payload["description"] = definition.Description
	}
	if len(result.ChangedFiles) > 0 {
		payload["changed_files"] = result.ChangedFiles
	}
	if len(result.TestsRun) > 0 {
		payload["tests_run"] = result.TestsRun
	}
	if len(result.Findings) > 0 {
		payload["findings"] = result.Findings
	}
	if len(result.OpenQuestions) > 0 {
		payload["open_questions"] = result.OpenQuestions
	}
	return t.log.Append(ctx, session.Event{
		ID:        session.EventID(fmt.Sprintf("spawn-%s-%s-%d", task.Agent, status, t.currentTime().UnixNano())),
		SessionID: t.sessionID,
		Type:      session.EventAgent,
		At:        t.currentTime(),
		Actor:     task.Agent,
		Text:      text,
		Payload:   payload,
	})
}

func (t delegatingTools) canDelegate() bool {
	maxDepth := t.maxDepth
	if maxDepth <= 0 {
		maxDepth = defaultMaxDelegationDepth
	}
	return t.depth < maxDepth
}

func taskDepth(depths *sync.Map, id agent.ID) int {
	if depths == nil {
		return 1
	}
	value, ok := depths.Load(string(id))
	if !ok {
		return 1
	}
	depth, ok := value.(int)
	if !ok || depth < 1 {
		return 1
	}
	return depth
}

func (t delegatingTools) currentTime() time.Time {
	if t.now != nil {
		return t.now()
	}
	return time.Now()
}

func selectAgentDefinition(definitions []agent.Definition, roleValue string, nameValue string) (agent.Definition, agent.Role, error) {
	if len(definitions) == 0 {
		definitions = agent.DefaultDefinitions()
	}
	role := agent.Role(strings.TrimSpace(roleValue))
	name := strings.TrimSpace(nameValue)
	for _, definition := range definitions {
		if name != "" && definition.Name == name {
			return definition, definition.Role, nil
		}
	}
	for _, definition := range definitions {
		if role != "" && definition.Role == role {
			return definition, definition.Role, nil
		}
	}
	if role == "" {
		role = agent.RoleExplorer
	}
	switch role {
	case agent.RoleOrchestrator, agent.RoleExplorer, agent.RoleWorker, agent.RoleVerifier, agent.RoleReviewer, agent.RoleSummarizer:
		return agent.Definition{Name: string(role), Role: role, Permissions: permission.DefaultPolicy(), MaxSteps: agent.DefaultBudget().MaxSteps}, role, nil
	default:
		return agent.Definition{}, "", fmt.Errorf("unknown agent role %q", roleValue)
	}
}

func workspaceModeForRole(role agent.Role) agent.WorkspaceMode {
	switch role {
	case agent.RoleExplorer, agent.RoleReviewer:
		return agent.WorkspaceReadOnly
	default:
		return agent.WorkspaceSameTree
	}
}

func agentResultContent(result agent.Result) string {
	payload := map[string]any{
		"task_id":        string(result.TaskID),
		"role":           string(result.Role),
		"status":         string(result.Status),
		"summary":        strings.TrimSpace(result.Summary),
		"changed_files":  result.ChangedFiles,
		"tests_run":      result.TestsRun,
		"findings":       result.Findings,
		"open_questions": result.OpenQuestions,
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return strings.TrimSpace(result.Summary)
	}
	return string(data)
}

func firstText(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
