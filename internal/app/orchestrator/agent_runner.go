package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Luew2/FreeCode/internal/app/contextmgr"
	"github.com/Luew2/FreeCode/internal/app/prompt"
	"github.com/Luew2/FreeCode/internal/core/agent"
	"github.com/Luew2/FreeCode/internal/core/model"
	"github.com/Luew2/FreeCode/internal/core/permission"
	"github.com/Luew2/FreeCode/internal/core/session"
	"github.com/Luew2/FreeCode/internal/ports"
)

type AgentRunner struct {
	Model                   model.Ref
	Client                  ports.ModelClient
	ToolsByRole             map[agent.Role]ports.ToolRegistry
	ToolsForTask            func(agent.Task) ports.ToolRegistry
	Trace                   agent.Trace
	Log                     ports.EventLog
	Prompt                  prompt.Builder
	Environment             prompt.Environment
	ContextBudget           contextmgr.Budget
	SessionContextMaxTokens int
}

func (r AgentRunner) RunAgent(ctx context.Context, task agent.Task) (agent.Result, error) {
	builder := r.Prompt
	if builder == (prompt.Builder{}) {
		builder = promptForTask(task)
	}
	builder.Developer = strings.TrimSpace(builder.Developer + "\n" + rolePrompt(task) + "\n\n" + taskPacketPrompt(task))
	log := r.Log
	if r.Trace.ParentSession != "" {
		log = traceLog{inner: r.Log, trace: r.traceFor(task)}
	}
	budget := r.ContextBudget
	if task.Budget.MaxTokens > 0 {
		budget.MaxInputTokens = task.Budget.MaxTokens
	}
	sessionContext, err := r.sessionContext(ctx)
	if err != nil {
		return agent.Result{TaskID: task.ID, Role: task.Role, Status: agent.StatusFailed, Summary: err.Error()}, err
	}
	response, err := Runner{
		Model:  r.Client,
		Tools:  r.toolsFor(task),
		Log:    log,
		Prompt: builder,
	}.Run(ctx, Request{
		SessionID:      r.taskSession(task),
		Model:          r.Model,
		UserRequest:    subagentUserRequest(task),
		Environment:    r.Environment,
		MaxSteps:       task.Budget.MaxSteps,
		ContextBudget:  budget,
		SessionContext: sessionContext,
	})
	if err != nil {
		return agent.Result{TaskID: task.ID, Role: task.Role, Status: agent.StatusFailed, Summary: err.Error()}, err
	}
	return parseAgentResult(task, response.Text), nil
}

func (r AgentRunner) sessionContext(ctx context.Context) (string, error) {
	if r.Log == nil || r.Trace.ParentSession == "" {
		return "", nil
	}
	maxTokens := r.SessionContextMaxTokens
	if maxTokens <= 0 {
		maxTokens = 4096
	}
	summary, _, err := contextmgr.BuildSessionContext(ctx, r.Log, r.Trace.ParentSession, maxTokens)
	return summary, err
}

func (r AgentRunner) toolsFor(task agent.Task) ports.ToolRegistry {
	if r.ToolsForTask != nil {
		if tools := r.ToolsForTask(task); tools != nil {
			return tools
		}
	}
	if len(r.ToolsByRole) == 0 {
		return nil
	}
	if tools, ok := r.ToolsByRole[task.Role]; ok {
		return tools
	}
	return r.ToolsByRole[agent.RoleOrchestrator]
}

func rolePrompt(task agent.Task) string {
	switch task.Role {
	case agent.RoleOrchestrator:
		return "You are an orchestrator for a FreeCode swarm run. Produce a concise plan, use spawn_agent when available for separable child tasks, synthesize child handoffs, and identify blockers. You may create nested orchestration only when a subproblem itself needs coordination. Use tools — do not answer with prose alone."
	case agent.RoleExplorer:
		return "You are an explorer for a FreeCode swarm run. Your job is to actually read the codebase: call read_file, search, and other available tools to gather concrete findings. Do not summarize from memory or guess — every claim in your handoff must be backed by something you read in this session."
	case agent.RoleWorker:
		return "You are the worker for a FreeCode swarm run. Implement only the assigned goal and report changed files. Make edits with the patch tool when allowed; do not just describe changes you would make."
	case agent.RoleVerifier:
		return "You are the verifier for a FreeCode swarm run. Run or recommend focused checks and report tests. When run_check is available, actually invoke it — do not narrate what running tests would do."
	case agent.RoleReviewer:
		return "You are the reviewer for a FreeCode swarm run. Lead with findings by severity; block on correctness risks. Read the relevant files with the available tools before offering judgments."
	default:
		return "You are a bounded FreeCode subagent. Stay within the task packet. Use the available tools to do real work — do not answer with prose alone."
	}
}

// subagentUserRequest produces the user-role message a subagent receives.
// The orchestrator's previous design sent a generic "Execute this task
// packet" string here, which weaker / instruction-following models
// interpreted as conversational chit-chat rather than a directive — they
// would acknowledge the request without calling any tools. Putting the
// concrete goal in the user message gives the model an actual question to
// answer and matches how interactive agents like opencode/Claude shape
// their delegated work.
func subagentUserRequest(task agent.Task) string {
	goal := strings.TrimSpace(task.Goal)
	if goal == "" {
		return "Execute the task packet in the developer message and return the required JSON handoff."
	}
	return goal + "\n\nUse the available tools to do this work concretely — read, search, edit, run, or delegate as appropriate. When done, return the JSON handoff specified in the task packet."
}

func promptForTask(task agent.Task) prompt.Builder {
	builder := prompt.NewBuilder()
	builder.Developer = "Use tool calls when repository context is needed. Follow the task packet exactly. Stay within assigned paths and permissions. If you say you will inspect, run, edit, verify, or delegate something and the needed tool is available, call the tool in the same turn before the final handoff. Return the required JSON handoff."
	if task.Role == agent.RoleOrchestrator {
		builder.Developer += " When spawn_agent is available, delegate independent child tasks instead of doing every step yourself. Use explorer agents for context, worker agents for scoped edits, verifier agents for checks, reviewer agents for correctness review, and orchestrator agents only for nested subproblems that need their own coordination. Synthesize every child handoff before finishing."
	}
	if task.Role == agent.RoleWorker && task.Permissions.Write != permission.DecisionDeny {
		builder.Permissions = "Reads are allowed inside the workspace. apply_patch may write only inside allowed_paths after previewing the patch and supplying the preview_token. Shell mutation, destructive git operations, and network tools are unavailable unless explicitly granted in the task packet."
		return builder
	}
	if task.Role == agent.RoleVerifier && task.Permissions.Shell != permission.DecisionDeny {
		builder.Permissions = "Reads are allowed inside the workspace. run_check may execute focused verification commands permitted by the task packet. Writes, patch application, destructive git operations, and network tools are unavailable unless explicitly granted in the task packet."
		return builder
	}
	builder.Permissions = "Reads are allowed inside the workspace. Writes, patch application, shell mutation, destructive git operations, and network tools are unavailable unless explicitly granted in the task packet."
	return builder
}

func taskPacketPrompt(task agent.Task) string {
	type packet struct {
		ID           string            `json:"id"`
		Goal         string            `json:"goal"`
		Role         string            `json:"role"`
		Agent        string            `json:"agent"`
		Workspace    string            `json:"workspace"`
		AllowedPaths []string          `json:"allowed_paths"`
		DeniedPaths  []string          `json:"denied_paths"`
		Permissions  map[string]string `json:"permissions"`
		Autonomy     map[string]string `json:"autonomy"`
		Budget       map[string]any    `json:"budget"`
		Handoff      map[string]bool   `json:"handoff_required"`
		Output       map[string]string `json:"required_output"`
	}
	value := packet{
		ID:           string(task.ID),
		Goal:         task.Goal,
		Role:         string(task.Role),
		Agent:        task.Agent,
		Workspace:    string(task.Workspace),
		AllowedPaths: append([]string(nil), task.AllowedPaths...),
		DeniedPaths:  append([]string(nil), task.DeniedPaths...),
		Permissions: map[string]string{
			"read":            string(task.Permissions.Read),
			"write":           string(task.Permissions.Write),
			"shell":           string(task.Permissions.Shell),
			"network":         string(task.Permissions.Network),
			"destructive_git": string(task.Permissions.DestructiveGit),
		},
		Autonomy: map[string]string{
			"mode":               string(task.Autonomy.Mode),
			"approval":           string(task.Autonomy.Approval),
			"stop_for_questions": boolString(task.Autonomy.StopForQuestions),
		},
		Budget: map[string]any{
			"max_steps":    task.Budget.MaxSteps,
			"max_tokens":   task.Budget.MaxTokens,
			"max_cost_usd": task.Budget.MaxCostUSD,
		},
		Handoff: map[string]bool{
			"changed_files":  task.Handoff.ChangedFiles,
			"tests_run":      task.Handoff.TestsRun,
			"open_questions": task.Handoff.OpenQuestions,
		},
		Output: map[string]string{
			"format": "Return final response as JSON object with status, summary, changed_files, tests_run, findings, and open_questions.",
			"status": "completed | blocked | failed",
		},
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return "Task packet unavailable."
	}
	return "Task packet:\n```json\n" + string(data) + "\n```"
}

func parseAgentResult(task agent.Task, text string) agent.Result {
	result := agent.Result{
		TaskID:  task.ID,
		Role:    task.Role,
		Status:  agent.StatusCompleted,
		Summary: strings.TrimSpace(text),
	}
	raw := extractJSONObject(text)
	if raw == "" {
		return result
	}
	// Tolerant decode: parse into map[string]any so that a single field with
	// an unexpected shape (e.g. structured finding objects when we expected
	// strings) does not throw away the whole handoff. We then extract each
	// known field with a best-effort coercion.
	var generic map[string]any
	if err := json.Unmarshal([]byte(raw), &generic); err != nil {
		return result
	}
	if status, ok := generic["status"].(string); ok {
		trimmed := agent.Status(strings.TrimSpace(status))
		switch trimmed {
		case agent.StatusCompleted, agent.StatusBlocked, agent.StatusFailed:
			result.Status = trimmed
		}
	}
	if summary, ok := generic["summary"].(string); ok && strings.TrimSpace(summary) != "" {
		result.Summary = strings.TrimSpace(summary)
	}
	result.ChangedFiles = stringSliceField(generic["changed_files"])
	result.TestsRun = stringSliceField(generic["tests_run"])
	result.Findings = stringSliceField(generic["findings"])
	result.OpenQuestions = stringSliceField(generic["open_questions"])
	return result
}

// stringSliceField coerces a JSON-decoded value into []string, tolerating
// the realistic shapes a subagent might emit:
//   - already []string (legacy)
//   - []any with string elements (json.Unmarshal default)
//   - []any with structured elements ({file: "x", note: "y"}) — each
//     non-string element is re-encoded as JSON so callers see a stable
//     textual representation rather than the whole field being lost.
//
// Returns nil for nil/non-array input so the result mirrors the original
// "field absent" semantics.
func stringSliceField(value any) []string {
	switch v := value.(type) {
	case nil:
		return nil
	case []string:
		return append([]string(nil), v...)
	case []any:
		out := make([]string, 0, len(v))
		for _, raw := range v {
			out = append(out, stringifyEntry(raw))
		}
		return out
	default:
		return nil
	}
}

func stringifyEntry(raw any) string {
	if raw == nil {
		return ""
	}
	if s, ok := raw.(string); ok {
		return s
	}
	if data, err := json.Marshal(raw); err == nil {
		return string(data)
	}
	return fmt.Sprintf("%v", raw)
}

func extractJSONObject(text string) string {
	text = strings.TrimSpace(text)
	if strings.HasPrefix(text, "```") {
		text = strings.TrimPrefix(text, "```json")
		text = strings.TrimPrefix(text, "```")
		text = strings.TrimSuffix(text, "```")
		text = strings.TrimSpace(text)
	}
	start := strings.IndexByte(text, '{')
	end := strings.LastIndexByte(text, '}')
	if start < 0 || end < start {
		return ""
	}
	return text[start : end+1]
}

func (r AgentRunner) taskSession(task agent.Task) session.ID {
	if r.Trace.TaskSession != "" {
		return r.Trace.TaskSession
	}
	if r.Trace.ParentSession != "" {
		return r.Trace.ParentSession
	}
	return session.ID(task.ID)
}

func (r AgentRunner) traceFor(task agent.Task) agent.Trace {
	trace := r.Trace
	if trace.Actor == "" {
		trace.Actor = task.Agent
	}
	if trace.TaskSession == "" {
		trace.TaskSession = session.ID(task.ID)
	}
	return trace
}

type traceLog struct {
	inner ports.EventLog
	trace agent.Trace
}

func (l traceLog) Append(ctx context.Context, event session.Event) error {
	if l.inner == nil {
		return nil
	}
	if l.trace.ParentSession != "" {
		event.SessionID = l.trace.ParentSession
	}
	if l.trace.Actor != "" {
		event.Actor = l.trace.Actor
	}
	if event.Payload == nil {
		event.Payload = map[string]any{}
	}
	event.Payload["agent"] = l.trace.Actor
	event.Payload["task_session"] = string(l.trace.TaskSession)
	return l.inner.Append(ctx, event)
}

func (l traceLog) Stream(ctx context.Context, id session.ID) (<-chan session.Event, error) {
	if l.inner == nil {
		ch := make(chan session.Event)
		close(ch)
		return ch, nil
	}
	return l.inner.Stream(ctx, id)
}

func boolString(value bool) string {
	if value {
		return "true"
	}
	return "false"
}
