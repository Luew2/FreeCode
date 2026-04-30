package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	localclipboard "github.com/Luew2/FreeCode/internal/adapters/clipboard/local"
	tomlconfig "github.com/Luew2/FreeCode/internal/adapters/config/toml"
	anthropic_messages "github.com/Luew2/FreeCode/internal/adapters/model/anthropic_messages"
	openai_chat "github.com/Luew2/FreeCode/internal/adapters/model/openai_chat"
	envsecrets "github.com/Luew2/FreeCode/internal/adapters/secrets/env"
	jsonllog "github.com/Luew2/FreeCode/internal/adapters/session/jsonl"
	"github.com/Luew2/FreeCode/internal/adapters/tools/builtin"
	"github.com/Luew2/FreeCode/internal/adapters/tui"
	"github.com/Luew2/FreeCode/internal/adapters/tui2"
	"github.com/Luew2/FreeCode/internal/adapters/workspace/editorcli"
	"github.com/Luew2/FreeCode/internal/adapters/workspace/gitcli"
	"github.com/Luew2/FreeCode/internal/adapters/workspace/localfs"
	"github.com/Luew2/FreeCode/internal/app/bench"
	"github.com/Luew2/FreeCode/internal/app/commands"
	"github.com/Luew2/FreeCode/internal/app/contextmgr"
	"github.com/Luew2/FreeCode/internal/app/orchestrator"
	"github.com/Luew2/FreeCode/internal/app/prompt"
	"github.com/Luew2/FreeCode/internal/app/swarm"
	"github.com/Luew2/FreeCode/internal/app/workbench"
	"github.com/Luew2/FreeCode/internal/core/agent"
	"github.com/Luew2/FreeCode/internal/core/model"
	"github.com/Luew2/FreeCode/internal/core/permission"
	"github.com/Luew2/FreeCode/internal/core/session"
	"github.com/Luew2/FreeCode/internal/ports"
	"golang.org/x/term"
)

func Run(args []string, stdout io.Writer, stderr io.Writer) int {
	return RunWithIO(args, strings.NewReader(""), stdout, stderr)
}

func RunWithIO(args []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) int {
	if len(args) == 0 {
		return runTUI(nil, stdin, stdout, stderr)
	}
	if strings.HasPrefix(args[0], "-") && args[0] != "-h" && args[0] != "--help" {
		return runTUI(args, stdin, stdout, stderr)
	}

	switch args[0] {
	case "help", "-h", "--help":
		_ = commands.PrintUsage(stdout)
		return 0
	case "version":
		if !hasExactArity(args, 1, stderr) {
			return 2
		}
		if err := commands.PrintVersion(stdout, commands.DefaultVersionInfo()); err != nil {
			fmt.Fprintf(stderr, "version: %v\n", err)
			return 1
		}
		return 0
	case "doctor":
		if !hasExactArity(args, 1, stderr) {
			return 2
		}
		status, err := buildDoctorStatus()
		if err != nil {
			fmt.Fprintf(stderr, "doctor: %v\n", err)
			return 1
		}
		if err := commands.PrintDoctor(stdout, status); err != nil {
			fmt.Fprintf(stderr, "doctor: %v\n", err)
			return 1
		}
		return 0
	case "bench":
		return runBench(args[1:], stdout, stderr)
	case "compact":
		return runCompact(args[1:], stdout, stderr)
	case "tui":
		return runTUI(args[1:], stdin, stdout, stderr)
	case "ask":
		return runAsk(args[1:], stdout, stderr)
	case "swarm":
		return runSwarm(args[1:], stdout, stderr)
	case "status":
		return runStatus(args[1:], stdout, stderr)
	case "diff":
		return runDiff(args[1:], stdout, stderr)
	case "provider":
		return runProvider(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n", args[0])
		_ = commands.PrintUsage(stderr)
		return 2
	}
}

func runCompact(args []string, stdout io.Writer, stderr io.Writer) int {
	var sessionPath string
	var sessionID string
	var maxTokens int
	fs := flag.NewFlagSet("compact", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&sessionPath, "session", filepath.Join(".freecode", "sessions", "latest.jsonl"), "session JSONL path")
	fs.StringVar(&sessionID, "session-id", "default", "session id")
	fs.IntVar(&maxTokens, "max-tokens", 4096, "maximum estimated tokens in compacted summary")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "compact accepts flags only\n")
		return 2
	}
	if err := commands.Compact(context.Background(), stdout, jsonllog.New(sessionPath), commands.CompactOptions{
		SessionID: sessionID,
		MaxTokens: maxTokens,
	}); err != nil {
		fmt.Fprintf(stderr, "compact: %v\n", err)
		return 1
	}
	return 0
}

func runBench(args []string, stdout io.Writer, stderr io.Writer) int {
	var task string
	fs := flag.NewFlagSet("bench", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		_, _ = fmt.Fprintf(fs.Output(), "usage: freecode bench [--task all|TASK]\n\n")
		_, _ = fmt.Fprintf(fs.Output(), "Tasks: all, %s\n", strings.Join(benchTaskNames(), ", "))
	}
	fs.StringVar(&task, "task", "all", "benchmark task name or all")
	if hasHelpArg(args) {
		fs.SetOutput(stdout)
		fs.Usage()
		return 0
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "bench accepts flags only\n")
		return 2
	}

	results, err := bench.Run(context.Background(), bench.Options{Task: task, Tasks: benchTasks()})
	if err != nil {
		fmt.Fprintf(stderr, "bench: %v\n", err)
		return 2
	}
	if err := bench.FormatResults(stdout, results); err != nil {
		fmt.Fprintf(stderr, "bench: %v\n", err)
		return 1
	}
	if !bench.AllPassed(results) {
		return 1
	}
	return 0
}

func runAsk(args []string, stdout io.Writer, stderr io.Writer) int {
	var configPath string
	var sessionPath string
	var maxSteps int
	var maxInputTokens int
	var maxOutputTokens int
	var allowWrites bool
	var approvalValue string
	fs := flag.NewFlagSet("ask", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&configPath, "config", tomlconfig.DefaultPath, "config path")
	fs.StringVar(&sessionPath, "session", filepath.Join(".freecode", "sessions", "latest.jsonl"), "session JSONL path")
	fs.IntVar(&maxSteps, "max-steps", 0, "maximum model/tool loop steps")
	fs.IntVar(&maxInputTokens, "max-input-tokens", 0, "maximum estimated input tokens before compaction")
	fs.IntVar(&maxOutputTokens, "max-output-tokens", 0, "maximum output tokens requested from the model")
	fs.StringVar(&approvalValue, "approval", string(permission.ModeReadOnly), "approval mode: read-only, ask, auto, or danger")
	fs.BoolVar(&allowWrites, "allow-writes", false, "enable workspace write tools for this ask")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	question := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if question == "" {
		fmt.Fprintf(stderr, "ask requires a question\n")
		return 2
	}

	ctx := context.Background()
	approvalMode, err := permission.ParseMode(approvalValue)
	if err != nil {
		fmt.Fprintf(stderr, "ask: %v\n", err)
		return 2
	}
	if allowWrites && !hasFlag(args, "approval") {
		approvalMode = permission.ModeAuto
	}
	deps, err := buildAskDependencies(ctx, configPath, sessionPath, approvalMode)
	if err != nil {
		fmt.Fprintf(stderr, "ask: %v\n", err)
		return 1
	}

	err = commands.Ask(ctx, stdout, deps, commands.AskOptions{
		Question:        question,
		MaxSteps:        maxSteps,
		MaxInputTokens:  maxInputTokens,
		MaxOutputTokens: maxOutputTokens,
	})
	if err != nil {
		fmt.Fprintf(stderr, "ask: %v\n", err)
		return 1
	}
	return 0
}

func buildAskDependencies(ctx context.Context, configPath string, sessionPath string, approvalMode permission.Mode) (commands.AskDependencies, error) {
	workspace, err := localfs.New(".")
	if err != nil {
		return commands.AskDependencies{}, err
	}
	git, _ := gitcli.New(workspace.Root())
	return buildAskDependenciesWithRuntime(ctx, configPath, sessionPath, approvalMode, workspace, git, nil, nil)
}

func buildAskDependenciesWithRuntime(ctx context.Context, configPath string, sessionPath string, approvalMode permission.Mode, workspace *localfs.Workspace, git ports.Git, tools ports.ToolRegistry, eventLog ports.EventLog) (commands.AskDependencies, error) {
	settings, err := tomlconfig.New(configPath).Load(ctx)
	if err != nil {
		return commands.AskDependencies{}, err
	}
	ref := settings.ActiveModel
	if ref == (model.Ref{}) {
		return commands.AskDependencies{}, fmt.Errorf("active model is not configured")
	}
	provider, ok := settings.Providers[ref.Provider]
	if !ok {
		return commands.AskDependencies{}, fmt.Errorf("provider %q is not configured", ref.Provider)
	}
	client, err := buildModelClient(provider)
	if err != nil {
		return commands.AskDependencies{}, err
	}
	if workspace == nil {
		workspace, err = localfs.New(".")
		if err != nil {
			return commands.AskDependencies{}, err
		}
	}
	if git == nil {
		git, _ = gitcli.New(workspace.Root())
	}
	branch := ""
	if git != nil {
		if status, statusErr := git.Status(ctx); statusErr == nil {
			branch = status.Branch
		}
	}
	if tools == nil {
		tools = builtin.NewReadOnly(workspace.FileSystem())
		if approvalMode != permission.ModeReadOnly {
			tools = builtin.NewWritable(workspace.FileSystem(), builtin.NewStaticPermissionGate(permission.PolicyForMode(approvalMode)))
		}
	}
	writableRoots := []string(nil)
	builder := prompt.NewBuilder()
	if approvalMode != permission.ModeReadOnly {
		writableRoots = []string{workspace.Root()}
		builder.Developer = strings.TrimSpace(builder.Developer + "\n\nUse apply_patch for scoped workspace edits and report changed files and verification. First call apply_patch without accepted or with accepted=false to preview the patch; only call it again with accepted=true after the preview is available.")
		builder.Permissions = fmt.Sprintf("Reads are allowed inside the workspace. Approval mode is %s. apply_patch may write only after the patch preview has been produced, the preview_token is supplied, and the active permission policy allows the write. Do not attempt path traversal. Shell mutation, destructive git operations, and network tools are unavailable.", approvalMode)
	}

	configuredModel := settings.Models[ref]
	if eventLog == nil {
		eventLog = jsonllog.New(sessionPath)
	}
	return commands.AskDependencies{
		Model:  ref,
		Client: client,
		Tools:  tools,
		Log:    eventLog,
		Prompt: builder,
		Env: prompt.Environment{
			WorkspaceRoot: workspace.Root(),
			Shell:         os.Getenv("SHELL"),
			GitBranch:     branch,
			Platform:      runtime.GOOS + "/" + runtime.GOARCH,
			WritableRoots: writableRoots,
		},
		ContextBudget:           contextmgr.FromLimits(configuredModel.Limits),
		SessionContextMaxTokens: 4096,
	}, nil
}

type runtimeBundle struct {
	Workspace   *localfs.Workspace
	Git         ports.Git
	Log         ports.EventLog
	Sessions    workbench.SessionIndex
	ReadTools   ports.ToolRegistry
	WriteTools  ports.ToolRegistry
	VerifyTools ports.ToolRegistry
	Approval    *workbench.ApprovalGate
	Settings    runtimeSettings

	SessionID     session.ID
	SessionPath   string
	SessionsDir   string
	IndexPath     string
	LogForSession func(session.ID) ports.EventLog
}

type runtimeSettings struct {
	Model           model.Ref
	Provider        model.Provider
	ModelLimits     model.Limits
	ContextBudget   contextmgr.Budget
	Agents          []agent.Definition
	EditorCommand   string
	EditorDoubleEsc bool
	ConfigRef       string
}

func buildRuntime(ctx context.Context, configPath string, launch sessionLaunch, approvalMode permission.Mode) (runtimeBundle, error) {
	workspace, err := localfs.New(".")
	if err != nil {
		return runtimeBundle{}, err
	}
	git, _ := gitcli.New(workspace.Root())
	settings, err := tomlconfig.New(configPath).Load(ctx)
	if err != nil {
		return runtimeBundle{}, err
	}
	ref := settings.ActiveModel
	if ref == (model.Ref{}) {
		return runtimeBundle{}, fmt.Errorf("active model is not configured")
	}
	provider, ok := settings.Providers[ref.Provider]
	if !ok {
		return runtimeBundle{}, fmt.Errorf("provider %q is not configured", ref.Provider)
	}
	sessionsDir := strings.TrimSpace(settings.SessionsDir)
	if sessionsDir == "" {
		sessionsDir = defaultSessionsDir
	}
	if launch.Explicit {
		if dir := strings.TrimSpace(filepath.Dir(launch.Path)); dir != "" && dir != "." {
			sessionsDir = dir
		}
	}
	if launch.ID == "" {
		launch.ID = newLaunchSessionID(time.Now())
	}
	if strings.TrimSpace(launch.Path) == "" {
		launch.Path = filepath.Join(sessionsDir, string(launch.ID)+".jsonl")
	}
	indexPath := filepath.Join(sessionsDir, "index.json")
	sessions := jsonllog.NewIndex(indexPath)
	logForSession := func(id session.ID) ports.EventLog {
		return jsonllog.New(sessionLogPath(ctx, sessions, workspace.Root(), sessionsDir, launch, id))
	}
	configuredModel := settings.Models[ref]
	approvalGate := workbench.NewApprovalGate(approvalMode)
	readTools := builtin.NewReadOnly(workspace.FileSystem())
	writeTools := builtin.NewWritable(workspace.FileSystem(), approvalGate)
	verifyPolicy := permission.PolicyForMode(approvalMode)
	if approvalMode == permission.ModeAuto {
		verifyPolicy.Shell = permission.DecisionAllow
	}
	verifyTools := builtin.NewVerifier(workspace.FileSystem(), builtin.NewStaticPermissionGate(verifyPolicy), workspace.Root())
	return runtimeBundle{
		Workspace:     workspace,
		Git:           git,
		Log:           jsonllog.New(launch.Path),
		Sessions:      sessions,
		ReadTools:     readTools,
		WriteTools:    writeTools,
		VerifyTools:   verifyTools,
		Approval:      approvalGate,
		SessionID:     launch.ID,
		SessionPath:   launch.Path,
		SessionsDir:   sessionsDir,
		IndexPath:     indexPath,
		LogForSession: logForSession,
		Settings: runtimeSettings{
			Model:           ref,
			Provider:        provider,
			ModelLimits:     configuredModel.Limits,
			ContextBudget:   contextmgr.FromLimits(configuredModel.Limits),
			Agents:          settings.Agents,
			EditorCommand:   settings.EditorCommand,
			EditorDoubleEsc: settings.EditorDoubleEsc,
			ConfigRef:       configPath,
		},
	}, nil
}

func buildSwarmUseCase(ctx context.Context, bundle runtimeBundle, log ports.EventLog, sessionID session.ID) (swarm.UseCase, error) {
	client, err := buildModelClient(bundle.Settings.Provider)
	if err != nil {
		return swarm.UseCase{}, err
	}
	if log == nil {
		log = bundle.Log
	}
	return swarm.UseCase{
		Agents: bundle.Settings.Agents,
		Runner: orchestrator.AgentRunner{
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
				if task.Role == agent.RoleVerifier {
					policy := permission.MergePolicyWithMode(task.Permissions, task.Autonomy.Approval)
					if task.Autonomy.Approval == permission.ModeAuto {
						policy.Shell = permission.DecisionAllow
					}
					return builtin.NewVerifier(bundle.Workspace.FileSystem(), builtin.NewStaticPermissionGate(policy), bundle.Workspace.Root())
				}
				if task.Role != agent.RoleWorker {
					return bundle.ReadTools
				}
				policy := permission.MergePolicyWithMode(task.Permissions, task.Autonomy.Approval)
				policy.AllowedPaths = append([]string(nil), task.AllowedPaths...)
				policy.DeniedPaths = append([]string(nil), task.DeniedPaths...)
				return builtin.NewWritable(bundle.Workspace.FileSystem(), builtin.NewStaticPermissionGate(policy))
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
		},
		Log: log,
	}, nil
}

func buildModelClient(provider model.Provider) (ports.ModelClient, error) {
	switch provider.Protocol {
	case commands.ProviderProtocolOpenAIChat:
		return openai_chat.NewClient(provider, envsecrets.New(), nil)
	case commands.ProviderProtocolAnthropicMessages:
		return anthropic_messages.NewClient(provider, envsecrets.New(), nil)
	default:
		return nil, fmt.Errorf("unsupported provider protocol %q", provider.Protocol)
	}
}

func gitBranch(ctx context.Context, git ports.Git) string {
	if git == nil {
		return ""
	}
	status, err := git.Status(ctx)
	if err != nil {
		return ""
	}
	return status.Branch
}

const defaultSessionsDir = ".freecode/sessions"

type sessionLaunch struct {
	ID       session.ID
	Path     string
	Explicit bool
}

func newSessionLaunch(sessionPath string, explicit bool) sessionLaunch {
	if explicit {
		return sessionLaunch{ID: sessionIDFromPath(sessionPath), Path: sessionPath, Explicit: true}
	}
	id := newLaunchSessionID(time.Now())
	return sessionLaunch{ID: id, Path: filepath.Join(defaultSessionsDir, string(id)+".jsonl")}
}

func newLaunchSessionID(now time.Time) session.ID {
	utc := now.UTC()
	return session.ID(fmt.Sprintf("session-%s-%09d", utc.Format("20060102-150405"), utc.Nanosecond()))
}

func sessionIDFromPath(path string) session.ID {
	if filepath.Base(path) == "latest.jsonl" || filepath.Clean(path) == filepath.Clean(filepath.Join(defaultSessionsDir, "latest.jsonl")) {
		return "default"
	}
	name := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	id := strings.Trim(sanitizeSessionID(name), ".-_")
	if id == "" {
		return "default"
	}
	return session.ID(id)
}

func sanitizeSessionID(value string) string {
	var builder strings.Builder
	lastDash := false
	for _, r := range value {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '.' || r == '-'
		if ok {
			builder.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			builder.WriteByte('-')
			lastDash = true
		}
	}
	return builder.String()
}

func sessionLogPath(ctx context.Context, index workbench.SessionIndex, workspaceRoot string, sessionsDir string, launch sessionLaunch, id session.ID) string {
	if id == launch.ID && launch.Path != "" {
		return launch.Path
	}
	if index != nil {
		if summaries, err := index.List(ctx, workspaceRoot); err == nil {
			for _, summary := range summaries {
				if summary.ID == id && strings.TrimSpace(summary.LogPath) != "" {
					return summary.LogPath
				}
			}
		}
	}
	return filepath.Join(sessionsDir, string(id)+".jsonl")
}

func registerLaunchSession(ctx context.Context, bundle runtimeBundle) error {
	if bundle.Sessions == nil {
		return nil
	}
	if err := ensureSessionFile(bundle.SessionPath); err != nil {
		return err
	}
	now := time.Now().UTC()
	return bundle.Sessions.Create(ctx, workbench.SessionSummary{
		ID:            bundle.SessionID,
		Title:         sessionTitle(bundle.Workspace.Root(), bundle.SessionID),
		WorkspaceRoot: bundle.Workspace.Root(),
		Branch:        gitBranch(ctx, bundle.Git),
		Model:         bundle.Settings.Model.String(),
		CreatedAt:     now,
		UpdatedAt:     now,
		LogPath:       bundle.SessionPath,
	})
}

func ensureSessionFile(path string) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	return file.Close()
}

func sessionTitle(workspaceRoot string, id session.ID) string {
	name := strings.TrimSpace(filepath.Base(workspaceRoot))
	if name == "" || name == "." || name == string(filepath.Separator) {
		return string(id)
	}
	return name + " " + string(id)
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

type mergedToolRegistry struct {
	base  ports.ToolRegistry
	extra ports.ToolRegistry
}

func mergeToolRegistries(base ports.ToolRegistry, extra ports.ToolRegistry) ports.ToolRegistry {
	if base == nil {
		return extra
	}
	if extra == nil {
		return base
	}
	return mergedToolRegistry{base: base, extra: extra}
}

func (r mergedToolRegistry) Tools() []model.ToolSpec {
	var tools []model.ToolSpec
	if r.base != nil {
		tools = append(tools, r.base.Tools()...)
	}
	if r.extra != nil {
		tools = append(tools, r.extra.Tools()...)
	}
	return tools
}

func (r mergedToolRegistry) RunTool(ctx context.Context, call model.ToolCall) (ports.ToolResult, error) {
	if r.extra != nil {
		for _, tool := range r.extra.Tools() {
			if tool.Name == call.Name {
				return r.extra.RunTool(ctx, call)
			}
		}
	}
	if r.base == nil {
		return ports.ToolResult{}, fmt.Errorf("tool registry is not configured")
	}
	return r.base.RunTool(ctx, call)
}

func runMainOwnedSwarm(ctx context.Context, configPath string, bundle runtimeBundle, service *workbench.Service, request workbench.SubmitRequest) error {
	if service == nil {
		return fmt.Errorf("workbench service is not configured")
	}
	sessionID := service.SessionID
	tools := ports.ToolRegistry(bundle.WriteTools)
	if request.Approval == permission.ModeReadOnly {
		tools = bundle.ReadTools
	}
	terminalTools := request.TerminalTools
	if request.Approval == permission.ModeReadOnly {
		terminalTools = tui2.FilterReadOnlyTerminalTools(terminalTools)
	}
	tools = mergeToolRegistries(tools, terminalTools)
	tools, err := withDelegationTools(ctx, bundle, service.Log, sessionID, request.Approval, "main", tools)
	if err != nil {
		return err
	}
	deps, err := buildAskDependenciesWithRuntime(ctx, configPath, "", request.Approval, bundle.Workspace, bundle.Git, tools, service.Log)
	if err != nil {
		return err
	}
	if err := logSwarmLifecycle(ctx, service.Log, sessionID, agent.StatusRunning, request.Text); err != nil {
		return err
	}
	spawnBefore := countToolEvents(ctx, service.Log, sessionID, "spawn_agent")
	// Send the user's prompt as the actual user message and the swarm
	// orchestration scaffolding (role, planning rules, current git
	// context) as the turn-scoped developer message. The earlier
	// behaviour stuffed everything into Question, which made the chat
	// transcript appear as if the user had typed the orchestrator
	// instructions verbatim.
	turnContext := combineTurnContexts(request.TurnContext, swarmDelegationContext(request.Approval, bundle.Git))
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
	// If the orchestrator decided no delegation was needed, treat that as a
	// completed run with a notice rather than a failure. The previous
	// behaviour surfaced as "swarm doesn't work" any time the model decided
	// to answer directly — even when the answer was correct.
	status := agent.StatusCompleted
	notice := "swarm completed"
	if spawnAfter <= spawnBefore {
		notice = "swarm completed without spawning any agents (model answered directly)"
	}
	_ = response // response text is already streamed into the session log via the orchestrator
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

// swarmDelegationContext returns the orchestrator scaffolding for a swarm
// run as a turn-scoped developer message. It deliberately does NOT include
// the user's prompt — that flows separately as the user message — so the
// chat transcript shows the user's actual ask, not the orchestrator role
// instructions concatenated to it.
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

func swarmSynthesisPrompt(goal string, response swarm.Response, git ports.Git) string {
	var lines []string
	lines = append(lines,
		"Swarm completed for request:",
		strings.TrimSpace(goal),
		"",
		"Agent handoffs:",
	)
	for _, result := range response.Results {
		lines = append(lines, fmt.Sprintf("- %s %s: %s", result.Role, result.Status, strings.TrimSpace(result.Summary)))
		if len(result.ChangedFiles) > 0 {
			lines = append(lines, "  changed files: "+strings.Join(result.ChangedFiles, ", "))
		}
		if len(result.TestsRun) > 0 {
			lines = append(lines, "  tests: "+strings.Join(result.TestsRun, ", "))
		}
		if len(result.Findings) > 0 {
			lines = append(lines, "  findings: "+strings.Join(result.Findings, "; "))
		}
		if len(result.OpenQuestions) > 0 {
			lines = append(lines, "  questions: "+strings.Join(result.OpenQuestions, "; "))
		}
	}
	lines = append(lines, "", "Overall swarm status: "+string(response.Status))
	if status := gitStatusSummary(context.Background(), git); status != "" {
		lines = append(lines, "", "Current git context:", status)
	}
	lines = append(lines, "", "Synthesize the outcome for the user. Be direct. Mention changed files, checks, blockers, and next steps.")
	return strings.Join(lines, "\n")
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

func runTUI(args []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) int {
	var configPath string
	var sessionPath string
	var approvalValue string
	var plain bool
	fs := flag.NewFlagSet("tui", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&configPath, "config", tomlconfig.DefaultPath, "config path")
	fs.StringVar(&sessionPath, "session", filepath.Join(".freecode", "sessions", "latest.jsonl"), "session JSONL path")
	fs.StringVar(&approvalValue, "approval", string(permission.ModeAsk), "approval mode: read-only, ask, auto, or danger")
	fs.BoolVar(&plain, "plain", false, "use the line-based fallback UI")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "tui accepts flags only\n")
		return 2
	}

	ctx := context.Background()
	approvalMode, err := permission.ParseMode(approvalValue)
	if err != nil {
		fmt.Fprintf(stderr, "tui: %v\n", err)
		return 2
	}
	launch := newSessionLaunch(sessionPath, hasFlag(args, "session"))
	bundle, err := buildRuntime(ctx, configPath, launch, approvalMode)
	if err != nil {
		fmt.Fprintf(stderr, "tui: %v\n", err)
		return 1
	}
	if err := registerLaunchSession(ctx, bundle); err != nil {
		fmt.Fprintf(stderr, "tui: %v\n", err)
		return 1
	}
	providerName := ""
	if bundle.Settings.Model.Provider != "" {
		providerName = string(bundle.Settings.Model.Provider)
	}
	editorCommand := strings.TrimSpace(bundle.Settings.EditorCommand)
	if editorCommand == "" {
		editorCommand = "nvim"
	}
	editor := &editorcli.Editor{Command: editorCommand}
	var workbenchService *workbench.Service
	workbenchService = &workbench.Service{
		Log:             bundle.Log,
		Git:             bundle.Git,
		Editor:          editor,
		Files:           bundle.Workspace.FileSystem(),
		Clipboard:       localclipboard.New(stdout),
		Tools:           bundle.WriteTools,
		Approval:        bundle.Approval,
		Sessions:        bundle.Sessions,
		Config:          tomlconfig.New(configPath),
		LogForSession:   bundle.LogForSession,
		SessionID:       bundle.SessionID,
		WorkspaceRoot:   bundle.Workspace.Root(),
		EditorCommand:   editor.Command,
		EditorDoubleEsc: bundle.Settings.EditorDoubleEsc,
		Provider:        providerName,
		Model:           bundle.Settings.Model,
		Submit: func(ctx context.Context, request workbench.SubmitRequest) error {
			tools := ports.ToolRegistry(bundle.WriteTools)
			if request.Approval == permission.ModeReadOnly {
				tools = bundle.ReadTools
			}
			terminalTools := request.TerminalTools
			if request.Approval == permission.ModeReadOnly {
				terminalTools = tui2.FilterReadOnlyTerminalTools(terminalTools)
			}
			tools = mergeToolRegistries(tools, terminalTools)
			activeSessionID := workbenchService.SessionID
			if request.Swarm {
				return runMainOwnedSwarm(ctx, configPath, bundle, workbenchService, request)
			}
			eventLog := workbenchService.Log
			if request.Target.Kind == "agent" {
				eventLog = metadataEventLog{inner: workbenchService.Log, metadata: directedAgentMetadata(ctx, workbenchService, request.Target)}
				request.Text = directedAgentPrompt(request.Target, request.Text)
			}
			parentID := "main"
			if request.Target.Kind == "agent" && strings.TrimSpace(request.Target.ID) != "" {
				parentID = request.Target.ID
			}
			tools, err := withDelegationTools(ctx, bundle, eventLog, activeSessionID, request.Approval, parentID, tools)
			if err != nil {
				return err
			}
			deps, err := buildAskDependenciesWithRuntime(ctx, configPath, "", request.Approval, bundle.Workspace, bundle.Git, tools, eventLog)
			if err != nil {
				return err
			}
			return commands.Ask(ctx, io.Discard, deps, commands.AskOptions{
				Question:       request.Text,
				SessionID:      string(activeSessionID),
				TurnContext:    request.TurnContext,
				IncludeHistory: true,
			})
		},
	}

	if plain || !isTerminal(stdout) {
		err = tui.Run(ctx, tui.Options{
			In:        stdin,
			Out:       stdout,
			Workbench: workbenchService,
		})
	} else {
		err = tui2.Run(ctx, tui2.Options{
			In:        stdin,
			Out:       stdout,
			Workbench: workbenchService,
			AltScreen: true,
		})
	}
	if err != nil {
		fmt.Fprintf(stderr, "tui: %v\n", err)
		return 1
	}
	return 0
}

func isTerminal(w io.Writer) bool {
	file, ok := w.(*os.File)
	if !ok {
		return false
	}
	return term.IsTerminal(int(file.Fd()))
}

func runSwarm(args []string, stdout io.Writer, stderr io.Writer) int {
	var configPath string
	var sessionPath string
	var approvalValue string
	var maxSteps int
	var maxInputTokens int
	var maxOutputTokens int
	fs := flag.NewFlagSet("swarm", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&configPath, "config", tomlconfig.DefaultPath, "config path")
	fs.StringVar(&sessionPath, "session", filepath.Join(".freecode", "sessions", "latest.jsonl"), "session JSONL path")
	fs.StringVar(&approvalValue, "approval", string(permission.ModeAsk), "approval mode: read-only, ask, auto, or danger")
	fs.IntVar(&maxSteps, "max-steps", 0, "maximum steps per swarm agent")
	fs.IntVar(&maxInputTokens, "max-input-tokens", 0, "maximum estimated input tokens before compaction")
	fs.IntVar(&maxOutputTokens, "max-output-tokens", 0, "maximum output tokens requested from the model")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	goal := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if goal == "" {
		fmt.Fprintf(stderr, "swarm requires a task\n")
		return 2
	}

	ctx := context.Background()
	approvalMode, err := permission.ParseMode(approvalValue)
	if err != nil {
		fmt.Fprintf(stderr, "swarm: %v\n", err)
		return 2
	}
	launch := newSessionLaunch(sessionPath, hasFlag(args, "session"))
	bundle, err := buildRuntime(ctx, configPath, launch, approvalMode)
	if err != nil {
		fmt.Fprintf(stderr, "swarm: %v\n", err)
		return 1
	}
	if err := registerLaunchSession(ctx, bundle); err != nil {
		fmt.Fprintf(stderr, "swarm: %v\n", err)
		return 1
	}
	if maxInputTokens > 0 {
		bundle.Settings.ContextBudget.MaxInputTokens = maxInputTokens
	}
	if maxOutputTokens > 0 {
		bundle.Settings.ContextBudget.MaxOutputTokens = maxOutputTokens
	}
	useCase, err := buildSwarmUseCase(ctx, bundle, bundle.Log, bundle.SessionID)
	if err != nil {
		fmt.Fprintf(stderr, "swarm: %v\n", err)
		return 1
	}
	response, err := useCase.Run(ctx, swarm.Request{
		SessionID: bundle.SessionID,
		Goal:      goal,
		Approval:  approvalMode,
		MaxSteps:  maxSteps,
	})
	if err != nil {
		fmt.Fprintf(stderr, "swarm: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "swarm %s\n", response.Status)
	for _, result := range response.Results {
		fmt.Fprintf(stdout, "- %s %s: %s\n", result.Role, result.Status, strings.TrimSpace(result.Summary))
	}
	return 0
}

func runStatus(args []string, stdout io.Writer, stderr io.Writer) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "status accepts no arguments\n")
		return 2
	}
	git, err := gitForCWD()
	if err != nil {
		fmt.Fprintf(stderr, "status: %v\n", err)
		return 1
	}
	if err := commands.PrintStatus(context.Background(), stdout, git); err != nil {
		fmt.Fprintf(stderr, "status: %v\n", err)
		return 1
	}
	return 0
}

func runDiff(args []string, stdout io.Writer, stderr io.Writer) int {
	fs := flag.NewFlagSet("diff", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	git, err := gitForCWD()
	if err != nil {
		fmt.Fprintf(stderr, "diff: %v\n", err)
		return 1
	}
	if err := commands.PrintDiff(context.Background(), stdout, git, fs.Args()); err != nil {
		fmt.Fprintf(stderr, "diff: %v\n", err)
		return 1
	}
	return 0
}

func gitForCWD() (ports.Git, error) {
	workspace, err := localfs.New(".")
	if err != nil {
		return nil, err
	}
	return gitcli.New(workspace.Root())
}

func hasExactArity(args []string, want int, stderr io.Writer) bool {
	if len(args) == want {
		return true
	}
	fmt.Fprintf(stderr, "%s accepts no extra arguments\n", args[0])
	return false
}

func hasFlag(args []string, name string) bool {
	long := "--" + name
	for _, arg := range args {
		if arg == long || strings.HasPrefix(arg, long+"=") {
			return true
		}
	}
	return false
}

func hasHelpArg(args []string) bool {
	for _, arg := range args {
		if arg == "help" || arg == "-h" || arg == "--help" {
			return true
		}
	}
	return false
}

func runProvider(args []string, stdout io.Writer, stderr io.Writer) int {
	if len(args) == 0 {
		_ = printProviderUsage(stderr)
		return 2
	}

	switch args[0] {
	case "add":
		return runProviderAdd(args[1:], stdout, stderr)
	case "list":
		return runProviderList(args[1:], stdout, stderr)
	case "use":
		return runProviderUse(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		_ = printProviderUsage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown provider command %q\n\n", args[0])
		_ = printProviderUsage(stderr)
		return 2
	}
}

func runProviderUse(args []string, stdout io.Writer, stderr io.Writer) int {
	var configPath string
	fs := flag.NewFlagSet("provider use", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&configPath, "config", tomlconfig.DefaultPath, "config path")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintf(stderr, "usage: provider use <name|provider/model>\n")
		return 2
	}
	store := tomlconfig.New(configPath)
	if err := commands.UseProvider(context.Background(), stdout, store, fs.Arg(0)); err != nil {
		fmt.Fprintf(stderr, "provider use: %v\n", err)
		return 1
	}
	return 0
}

func runProviderAdd(args []string, stdout io.Writer, stderr io.Writer) int {
	var opts commands.ProviderAddOptions
	var configPath string
	fs := flag.NewFlagSet("provider add", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&opts.Name, "name", "", "provider name")
	fs.StringVar(&opts.BaseURL, "base-url", "", "provider base URL")
	fs.StringVar(&opts.APIKeyEnv, "api-key-env", "", "environment variable containing the API key")
	fs.StringVar(&opts.Model, "model", "", "provider model id")
	fs.StringVar(&opts.Protocol, "protocol", commands.ProviderProtocolAuto, "provider protocol: openai-chat, anthropic-messages, or auto")
	fs.IntVar(&opts.ContextWindow, "context-window", 0, "model context window in tokens")
	fs.IntVar(&opts.MaxOutputTokens, "max-output-tokens", 0, "maximum output tokens for model requests")
	fs.StringVar(&configPath, "config", tomlconfig.DefaultPath, "config path")
	fs.BoolVar(&opts.SkipProbe, "skip-probe", false, "skip protocol probe")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "provider add accepts flags only\n")
		return 2
	}

	store := tomlconfig.New(configPath)
	secrets := envsecrets.New()
	probe := newProtocolProbeChain(secrets)
	if err := commands.AddProvider(context.Background(), stdout, store, probe, opts); err != nil {
		fmt.Fprintf(stderr, "provider add: %v\n", err)
		return 1
	}
	return 0
}

func runProviderList(args []string, stdout io.Writer, stderr io.Writer) int {
	var configPath string
	fs := flag.NewFlagSet("provider list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&configPath, "config", tomlconfig.DefaultPath, "config path")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "provider list accepts flags only\n")
		return 2
	}

	if err := commands.ListProviders(context.Background(), stdout, tomlconfig.New(configPath)); err != nil {
		fmt.Fprintf(stderr, "provider list: %v\n", err)
		return 1
	}
	return 0
}

func printProviderUsage(w io.Writer) error {
	_, err := fmt.Fprintf(w, `Usage:
  %s provider add --name NAME --base-url URL --api-key-env ENV --model MODEL --protocol openai-chat|anthropic-messages|auto [--context-window N] [--max-output-tokens N] [--config PATH] [--skip-probe]
  %s provider list [--config PATH]
  %s provider use NAME|PROVIDER/MODEL [--config PATH]
`, commands.AppName, commands.AppName, commands.AppName)
	return err
}

func buildDoctorStatus() (commands.DoctorStatus, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return commands.DoctorStatus{}, err
	}
	absCWD, err := filepath.Abs(cwd)
	if err != nil {
		return commands.DoctorStatus{}, err
	}

	_, goModOK := findUp(absCWD, "go.mod")
	gitPath, gitOK := findUp(absCWD, ".git")

	checks := []commands.DoctorCheck{
		{
			Name:   "go.mod",
			OK:     goModOK,
			Detail: foundDetail(goModOK, "found", "not found"),
		},
		{
			Name:   "git",
			OK:     gitOK,
			Detail: foundDetail(gitOK, gitPath, "not found"),
		},
	}

	return commands.DoctorStatus{
		Version: commands.DefaultVersionInfo(),
		WorkDir: absCWD,
		Runtime: commands.RuntimeStatus{
			GoVersion: runtime.Version(),
			GOOS:      runtime.GOOS,
			GOARCH:    runtime.GOARCH,
		},
		Checks: checks,
	}, nil
}

func findUp(start string, name string) (string, bool) {
	dir := filepath.Clean(start)
	for {
		candidate := filepath.Join(dir, name)
		if _, err := os.Stat(candidate); err == nil {
			return candidate, true
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

func foundDetail(ok bool, yes string, no string) string {
	if ok {
		return yes
	}
	return no
}
