package bootstrap

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/Luew2/FreeCode/internal/adapters/tools/builtin"
	"github.com/Luew2/FreeCode/internal/app/commands"
	"github.com/Luew2/FreeCode/internal/app/orchestrator"
	"github.com/Luew2/FreeCode/internal/app/prompt"
	"github.com/Luew2/FreeCode/internal/app/workbench"
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

func (r *Runtime) withDelegationTools(ctx context.Context, log ports.EventLog, sessionID session.ID, approval permission.Mode, parentID string, inner ports.ToolRegistry) (ports.ToolRegistry, error) {
	client, err := buildModelClient(r.Provider)
	if err != nil {
		return nil, err
	}
	depths := &sync.Map{}
	var runner orchestrator.AgentRunner
	runner = orchestrator.AgentRunner{
		Model:  r.Model,
		Client: client,
		ToolsByRole: map[agent.Role]ports.ToolRegistry{
			agent.RoleOrchestrator: r.ReadTools,
			agent.RoleExplorer:     r.ReadTools,
			agent.RoleWorker:       r.WriteTools,
			agent.RoleVerifier:     r.VerifyTools,
			agent.RoleReviewer:     r.ReadTools,
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
				roleTools = builtin.NewVerifier(r.workspace.FileSystem(), builtin.NewStaticPermissionGate(policy), r.workspace.Root())
			} else if task.Role == agent.RoleWorker {
				policy := permission.MergePolicyWithMode(task.Permissions, task.Autonomy.Approval)
				policy.AllowedPaths = append([]string(nil), task.AllowedPaths...)
				policy.DeniedPaths = append([]string(nil), task.DeniedPaths...)
				roleTools = builtin.NewWritable(r.workspace.FileSystem(), builtin.NewStaticPermissionGate(policy))
			} else {
				roleTools = r.ReadTools
			}
			if task.Role == agent.RoleOrchestrator {
				return delegatingTools{
					inner:     roleTools,
					runner:    runner,
					log:       log,
					sessionID: sessionID,
					approval:  approval,
					agents:    r.Agents,
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
			WorkspaceRoot: r.WorkspaceRoot,
			Shell:         os.Getenv("SHELL"),
			GitBranch:     gitBranch(ctx, r.Git),
			Platform:      runtime.GOOS + "/" + runtime.GOARCH,
			WritableRoots: []string{r.WorkspaceRoot},
		},
		ContextBudget:           r.ContextBudget,
		SessionContextMaxTokens: 4096,
	}
	return delegatingTools{
		inner:     inner,
		runner:    runner,
		log:       log,
		sessionID: sessionID,
		approval:  approval,
		agents:    r.Agents,
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
				"denied_paths":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
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
	limit := t.maxDepth
	if limit <= 0 {
		limit = defaultMaxDelegationDepth
	}
	return t.depth < limit
}

func (t delegatingTools) currentTime() time.Time {
	if t.now != nil {
		return t.now()
	}
	return time.Now()
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

func (r *Runtime) runMainOwnedSwarm(ctx context.Context, service *workbench.Service, request workbench.SubmitRequest, baseTools ports.ToolRegistry) error {
	if service == nil {
		return fmt.Errorf("workbench service is not configured")
	}
	sessionID := service.SessionID
	tools, err := r.withDelegationTools(ctx, service.Log, sessionID, request.Approval, "main", baseTools)
	if err != nil {
		return err
	}
	deps, err := r.askDependencies(ctx, tools, service.Log)
	if err != nil {
		return err
	}
	if err := logSwarmLifecycle(ctx, service.Log, sessionID, agent.StatusRunning, request.Text); err != nil {
		return err
	}
	spawnBefore := countToolEvents(ctx, service.Log, sessionID, "spawn_agent")
	turnContext := combineTurnContexts(request.TurnContext, swarmDelegationContext(request.Approval, r.Git))
	response, err := commands.AskWithResponse(ctx, io.Discard, deps, commands.AskOptions{
		Question:       strings.TrimSpace(request.Text),
		SessionID:      string(sessionID),
		TurnContext:    turnContext,
		MaxSteps:       16,
		IncludeHistory: true,
	})
	if err != nil {
		_ = logSwarmLifecycle(ctx, service.Log, sessionID, agent.StatusFailed, err.Error())
		return err
	}
	spawnAfter := countToolEvents(ctx, service.Log, sessionID, "spawn_agent")
	status := agent.StatusCompleted
	notice := "swarm completed"
	if spawnAfter <= spawnBefore {
		notice = "swarm completed without spawning any agents (model answered directly)"
	}
	_ = response
	return logSwarmLifecycle(ctx, service.Log, sessionID, status, notice)
}

func countToolEvents(ctx context.Context, log ports.EventLog, sessionID session.ID, name string) int {
	if log == nil {
		return 0
	}
	stream, err := log.Stream(ctx, sessionID)
	if err != nil {
		return 0
	}
	count := 0
	for event := range stream {
		if event.Type != session.EventTool || event.Payload == nil {
			continue
		}
		if strings.TrimSpace(fmt.Sprint(event.Payload["name"])) == name {
			count++
		}
	}
	return count
}

func logSwarmLifecycle(ctx context.Context, log ports.EventLog, sessionID session.ID, status agent.Status, text string) error {
	if log == nil {
		return nil
	}
	eventText := text
	payload := map[string]any{
		"status":          string(status),
		"role":            string(agent.RoleOrchestrator),
		"parent_agent_id": "main",
		"command":         ":s",
	}
	if status == agent.StatusRunning {
		payload["swarm_goal"] = text
		eventText = "swarm request: " + text
	}
	return log.Append(ctx, session.Event{
		ID:        session.EventID(fmt.Sprintf("swarm-main-%s-%d", status, time.Now().UnixNano())),
		SessionID: sessionID,
		Type:      session.EventAgent,
		At:        time.Now(),
		Actor:     "swarm",
		Text:      eventText,
		Payload:   payload,
	})
}

func swarmDelegationContext(approval permission.Mode, git ports.Git) string {
	lines := []string{
		"You are the main FreeCode orchestrator for a dynamic swarm run.",
		"Create a concise plan, then use spawn_agent to delegate as many bounded tasks as the request actually needs.",
		"Prefer explorer agents for context gathering, worker agents for scoped edits, verifier agents for checks, and reviewer agents for correctness review.",
		"Use orchestrator child agents only when a subproblem is large enough to need its own child-agent coordination.",
		"Do not use a fixed agent count. Spawn zero agents for trivial tasks, one agent for isolated work, or multiple agents for independent slices.",
		"If the user request is a short follow-up fragment, infer its meaning from the active turn context and visible conversation history before asking for clarification.",
		"After each handoff returns, synthesize progress and decide whether another child agent is needed.",
		"Finish with a direct summary of what changed, what agents did, verification, and any unresolved risks.",
		"Approval mode: " + string(approval) + ".",
	}
	if status := gitStatusSummary(context.Background(), git); status != "" {
		lines = append(lines, "", "Current git context:", status)
	}
	return strings.Join(lines, "\n")
}

func combineTurnContexts(base string, extra string) string {
	base = strings.TrimSpace(base)
	extra = strings.TrimSpace(extra)
	switch {
	case base == "":
		return extra
	case extra == "":
		return base
	default:
		return base + "\n\n" + extra
	}
}

func gitStatusSummary(ctx context.Context, git ports.Git) string {
	if git == nil {
		return ""
	}
	status, err := git.Status(ctx)
	if err != nil {
		return ""
	}
	if len(status.ChangedFiles) == 0 {
		return "clean on " + status.Branch
	}
	return fmt.Sprintf("branch %s; changed files: %s", status.Branch, strings.Join(status.ChangedFiles, ", "))
}

func directedAgentPrompt(target workbench.ConversationTarget, text string) string {
	title := strings.TrimSpace(target.Title)
	if title == "" {
		title = strings.TrimSpace(target.ID)
	}
	if title == "" {
		title = "selected agent"
	}
	return fmt.Sprintf("Directed follow-up for %s (%s):\n\n%s", title, target.ID, text)
}

func directedAgentMetadata(ctx context.Context, service *workbench.Service, target workbench.ConversationTarget) map[string]any {
	metadata := map[string]any{}
	if id := strings.TrimSpace(target.ID); id != "" {
		metadata["agent_id"] = id
		metadata["task_session"] = id
	}
	if service == nil {
		return metadata
	}
	state, err := service.Load(ctx)
	if err != nil {
		return metadata
	}
	for _, agent := range state.Agents {
		if agent.ID != target.ID && agent.TaskID != target.ID && agent.Name != target.ID {
			continue
		}
		if agent.Name != "" {
			metadata["agent"] = agent.Name
		}
		if agent.Role != "" {
			metadata["role"] = agent.Role
		}
		if agent.TaskID != "" {
			metadata["task_id"] = agent.TaskID
			metadata["task_session"] = agent.TaskID
		}
		break
	}
	return metadata
}

type metadataEventLog struct {
	inner    ports.EventLog
	metadata map[string]any
}

func (l metadataEventLog) Append(ctx context.Context, event session.Event) error {
	if l.inner == nil {
		return nil
	}
	if len(l.metadata) > 0 {
		if event.Payload == nil {
			event.Payload = map[string]any{}
		}
		for key, value := range l.metadata {
			if strings.TrimSpace(fmt.Sprint(value)) == "" {
				continue
			}
			if existing, ok := event.Payload[key]; ok && strings.TrimSpace(fmt.Sprint(existing)) != "" {
				continue
			}
			event.Payload[key] = value
		}
	}
	return l.inner.Append(ctx, event)
}

func (l metadataEventLog) Stream(ctx context.Context, id session.ID) (<-chan session.Event, error) {
	if l.inner == nil {
		ch := make(chan session.Event)
		close(ch)
		return ch, nil
	}
	return l.inner.Stream(ctx, id)
}
