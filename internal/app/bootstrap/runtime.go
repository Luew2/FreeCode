package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	localclipboard "github.com/Luew2/FreeCode/internal/adapters/clipboard/local"
	tomlconfig "github.com/Luew2/FreeCode/internal/adapters/config/toml"
	mcpadapter "github.com/Luew2/FreeCode/internal/adapters/mcp"
	anthropic_messages "github.com/Luew2/FreeCode/internal/adapters/model/anthropic_messages"
	openai_chat "github.com/Luew2/FreeCode/internal/adapters/model/openai_chat"
	envsecrets "github.com/Luew2/FreeCode/internal/adapters/secrets/env"
	jsonllog "github.com/Luew2/FreeCode/internal/adapters/session/jsonl"
	"github.com/Luew2/FreeCode/internal/adapters/tools/builtin"
	"github.com/Luew2/FreeCode/internal/adapters/workspace/editorcli"
	"github.com/Luew2/FreeCode/internal/adapters/workspace/gitcli"
	"github.com/Luew2/FreeCode/internal/adapters/workspace/localfs"
	"github.com/Luew2/FreeCode/internal/app/commands"
	"github.com/Luew2/FreeCode/internal/app/contextmgr"
	"github.com/Luew2/FreeCode/internal/app/orchestrator"
	"github.com/Luew2/FreeCode/internal/app/prompt"
	"github.com/Luew2/FreeCode/internal/app/swarm"
	"github.com/Luew2/FreeCode/internal/app/toolregistry"
	"github.com/Luew2/FreeCode/internal/app/workbench"
	"github.com/Luew2/FreeCode/internal/core/agent"
	"github.com/Luew2/FreeCode/internal/core/model"
	"github.com/Luew2/FreeCode/internal/core/permission"
	"github.com/Luew2/FreeCode/internal/core/session"
	"github.com/Luew2/FreeCode/internal/ports"
)

const DefaultSessionsDir = ".freecode/sessions"

type Options struct {
	ConfigPath        string
	SessionPath       string
	WorkspaceRoot     string
	ApprovalMode      permission.Mode
	StartNewSession   bool
	ClipboardTerminal io.Writer
}

type Runtime struct {
	Workspace     ports.FileSystem
	WorkspaceRoot string
	Git           ports.Git
	EventLog      ports.EventLog
	Sessions      workbench.SessionIndex
	Tools         ports.ToolRegistry

	ReadTools   ports.ToolRegistry
	WriteTools  ports.ToolRegistry
	VerifyTools ports.ToolRegistry
	Approval    *workbench.ApprovalGate
	MCP         *mcpadapter.Manager

	SessionID   session.ID
	SessionPath string
	SessionsDir string
	IndexPath   string

	ConfigPath        string
	Config            ports.ConfigStore
	Model             model.Ref
	Provider          model.Provider
	ModelLimits       model.Limits
	ContextBudget     contextmgr.Budget
	Agents            []agent.Definition
	EditorCommand     string
	EditorDoubleEsc   bool
	ClipboardTerminal io.Writer

	workspace     *localfs.Workspace
	logForSession func(session.ID) ports.EventLog
}

func Build(ctx context.Context, opts Options) (*Runtime, error) {
	if strings.TrimSpace(opts.ConfigPath) == "" {
		opts.ConfigPath = tomlconfig.DefaultPath
	}
	if strings.TrimSpace(opts.SessionPath) == "" {
		opts.SessionPath = filepath.Join(DefaultSessionsDir, "latest.jsonl")
	}
	if opts.ApprovalMode == "" {
		opts.ApprovalMode = permission.ModeAsk
	}
	root := strings.TrimSpace(opts.WorkspaceRoot)
	if root == "" {
		root = "."
	}
	workspace, err := localfs.New(root)
	if err != nil {
		return nil, err
	}
	git, _ := gitcli.New(workspace.Root())
	store := tomlconfig.New(opts.ConfigPath)
	settings, err := store.Load(ctx)
	if err != nil {
		return nil, err
	}
	ref := settings.ActiveModel
	if ref == (model.Ref{}) {
		return nil, fmt.Errorf("active model is not configured")
	}
	provider, ok := settings.Providers[ref.Provider]
	if !ok {
		return nil, fmt.Errorf("provider %q is not configured", ref.Provider)
	}

	sessionsDir := strings.TrimSpace(settings.SessionsDir)
	if sessionsDir == "" {
		sessionsDir = DefaultSessionsDir
	}
	launch := newSessionLaunch(opts.SessionPath, !opts.StartNewSession)
	if opts.StartNewSession {
		launch = newSessionLaunch("", false)
	} else if dir := strings.TrimSpace(filepath.Dir(launch.Path)); dir != "" && dir != "." {
		sessionsDir = dir
	}
	if launch.ID == "" {
		launch.ID = newLaunchSessionID(time.Now())
	}
	if strings.TrimSpace(launch.Path) == "" {
		launch.Path = filepath.Join(sessionsDir, string(launch.ID)+".jsonl")
	}

	indexPath := filepath.Join(sessionsDir, "index.json")
	sessions := jsonllog.NewIndex(indexPath)
	configuredModel := settings.Models[ref]
	approvalGate := workbench.NewApprovalGate(opts.ApprovalMode)
	readTools := builtin.NewReadOnly(workspace.FileSystem())
	writeTools := builtin.NewWritable(workspace.FileSystem(), approvalGate)
	verifyPolicy := permission.PolicyForMode(opts.ApprovalMode)
	if opts.ApprovalMode == permission.ModeAuto {
		verifyPolicy.Shell = permission.DecisionAllow
	}
	verifyTools := builtin.NewVerifier(workspace.FileSystem(), builtin.NewStaticPermissionGate(verifyPolicy), workspace.Root())
	activeTools := ports.ToolRegistry(writeTools)
	if opts.ApprovalMode == permission.ModeReadOnly {
		activeTools = readTools
	}
	var mcpManager *mcpadapter.Manager
	if settings.MCP.Enabled {
		mcpManager = mcpadapter.NewManager(mcpadapter.Options{
			Settings:      settings.MCP,
			WorkspaceRoot: workspace.Root(),
			Version:       commands.DefaultVersionInfo().Version,
		})
		if err := mcpManager.Start(ctx); err != nil {
			return nil, err
		}
	}
	composite, err := composeActiveTools(activeTools, mcpManager, opts.ApprovalMode, approvalGate)
	if err != nil {
		return nil, err
	}

	r := &Runtime{
		Workspace:         workspace.FileSystem(),
		WorkspaceRoot:     workspace.Root(),
		Git:               git,
		EventLog:          jsonllog.New(launch.Path),
		Sessions:          sessions,
		Tools:             composite,
		ReadTools:         readTools,
		WriteTools:        writeTools,
		VerifyTools:       verifyTools,
		Approval:          approvalGate,
		MCP:               mcpManager,
		SessionID:         launch.ID,
		SessionPath:       launch.Path,
		SessionsDir:       sessionsDir,
		IndexPath:         indexPath,
		ConfigPath:        opts.ConfigPath,
		Config:            store,
		Model:             ref,
		Provider:          provider,
		ModelLimits:       configuredModel.Limits,
		ContextBudget:     contextmgr.FromLimits(configuredModel.Limits),
		Agents:            settings.Agents,
		EditorCommand:     settings.EditorCommand,
		EditorDoubleEsc:   settings.EditorDoubleEsc,
		ClipboardTerminal: opts.ClipboardTerminal,
		workspace:         workspace,
	}
	r.logForSession = func(id session.ID) ports.EventLog {
		return jsonllog.New(sessionLogPath(ctx, sessions, workspace.Root(), sessionsDir, launch, id))
	}
	return r, nil
}

func (r *Runtime) Close() error {
	if r == nil {
		return nil
	}
	if closer, ok := r.Tools.(interface{ Close() error }); ok {
		if err := closer.Close(); err != nil {
			return err
		}
	}
	if r.MCP != nil {
		return r.MCP.Close()
	}
	return nil
}

func (r *Runtime) RegisterLaunchSession(ctx context.Context) error {
	if r == nil || r.Sessions == nil {
		return nil
	}
	if err := ensureSessionFile(r.SessionPath); err != nil {
		return err
	}
	now := time.Now().UTC()
	return r.Sessions.Create(ctx, workbench.SessionSummary{
		ID:            r.SessionID,
		Title:         sessionTitle(r.WorkspaceRoot, r.SessionID),
		WorkspaceRoot: r.WorkspaceRoot,
		Branch:        gitBranch(ctx, r.Git),
		Model:         r.Model.String(),
		CreatedAt:     now,
		UpdatedAt:     now,
		LogPath:       r.SessionPath,
	})
}

func (r *Runtime) Workbench(ctx context.Context) (*workbench.Service, error) {
	if r == nil {
		return nil, fmt.Errorf("runtime is not configured")
	}
	providerName := ""
	if r.Model.Provider != "" {
		providerName = string(r.Model.Provider)
	}
	editorCommand := strings.TrimSpace(r.EditorCommand)
	if editorCommand == "" {
		editorCommand = "nvim"
	}
	editor := &editorcli.Editor{Command: editorCommand}
	service := &workbench.Service{
		Log:             r.EventLog,
		Git:             r.Git,
		Editor:          editor,
		Files:           r.Workspace,
		Clipboard:       localclipboard.New(r.ClipboardTerminal),
		Tools:           r.WriteTools,
		Approval:        r.Approval,
		Sessions:        r.Sessions,
		MCP:             r.MCP,
		Config:          r.Config,
		LogForSession:   r.logForSession,
		SessionID:       r.SessionID,
		WorkspaceRoot:   r.WorkspaceRoot,
		EditorCommand:   editor.Command,
		EditorDoubleEsc: r.EditorDoubleEsc,
		Provider:        providerName,
		Model:           r.Model,
	}
	service.Submit = func(ctx context.Context, request workbench.SubmitRequest) error {
		return r.submit(ctx, service, request)
	}
	service.ContinueApproval = func(ctx context.Context, request workbench.ApprovalContinuationRequest) error {
		return r.continueApproval(ctx, service, request)
	}
	return service, nil
}

func (r *Runtime) AskDependencies(ctx context.Context) (commands.AskDependencies, error) {
	return r.askDependencies(ctx, r.currentToolsForApproval(r.ApprovalMode()), r.EventLog)
}

func (r *Runtime) SwarmUseCase(ctx context.Context, log ports.EventLog, sessionID session.ID) (swarm.UseCase, error) {
	client, err := buildModelClient(r.Provider)
	if err != nil {
		return swarm.UseCase{}, err
	}
	if log == nil {
		log = r.EventLog
	}
	return swarm.UseCase{
		Agents: r.Agents,
		Runner: orchestrator.AgentRunner{
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
				if task.Role == agent.RoleVerifier {
					policy := permission.MergePolicyWithMode(task.Permissions, task.Autonomy.Approval)
					if task.Autonomy.Approval == permission.ModeAuto {
						policy.Shell = permission.DecisionAllow
					}
					return builtin.NewVerifier(r.workspace.FileSystem(), builtin.NewStaticPermissionGate(policy), r.workspace.Root())
				}
				if task.Role != agent.RoleWorker {
					return r.ReadTools
				}
				policy := permission.MergePolicyWithMode(task.Permissions, task.Autonomy.Approval)
				policy.AllowedPaths = append([]string(nil), task.AllowedPaths...)
				policy.DeniedPaths = append([]string(nil), task.DeniedPaths...)
				return builtin.NewWritable(r.workspace.FileSystem(), builtin.NewStaticPermissionGate(policy))
			},
			Environment:             r.environment(ctx, []string{r.WorkspaceRoot}),
			ContextBudget:           r.ContextBudget,
			SessionContextMaxTokens: 4096,
		},
		Log: log,
	}, nil
}

func (r *Runtime) submit(ctx context.Context, service *workbench.Service, request workbench.SubmitRequest) error {
	tools := r.currentToolsForApproval(request.Approval)
	terminalTools := request.TerminalTools
	if request.Approval == permission.ModeReadOnly {
		terminalTools = toolregistry.FilterToolNames(terminalTools, map[string]string{"terminal_write": "read-only approval mode"})
	}
	tools, err := composeTools(tools, terminalTools)
	if err != nil {
		return err
	}
	activeSessionID := service.SessionID
	if request.Swarm {
		return r.runMainOwnedSwarm(ctx, service, request, tools)
	}
	eventLog := service.Log
	if request.Target.Kind == "agent" {
		eventLog = metadataEventLog{inner: service.Log, metadata: directedAgentMetadata(ctx, service, request.Target)}
		request.Text = directedAgentPrompt(request.Target, request.Text)
	}
	parentID := "main"
	if request.Target.Kind == "agent" && strings.TrimSpace(request.Target.ID) != "" {
		parentID = request.Target.ID
	}
	tools, err = r.withDelegationTools(ctx, eventLog, activeSessionID, request.Approval, parentID, tools)
	if err != nil {
		return err
	}
	deps, err := r.askDependencies(ctx, tools, eventLog)
	if err != nil {
		return err
	}
	return commands.Ask(ctx, io.Discard, deps, commands.AskOptions{
		Question:       request.Text,
		SessionID:      string(activeSessionID),
		TurnContext:    request.TurnContext,
		IncludeHistory: true,
	})
}

func (r *Runtime) continueApproval(ctx context.Context, service *workbench.Service, request workbench.ApprovalContinuationRequest) error {
	if service == nil {
		return fmt.Errorf("workbench service is not configured")
	}
	calls, err := service.ApprovalContinuationToolCalls(ctx, request.Item)
	if err != nil {
		return err
	}
	if len(calls) == 0 {
		return fmt.Errorf("approval %s has no resumable tool call", request.ApprovalID)
	}
	tools := r.currentToolsForApproval(r.ApprovalMode())
	parentID := "main"
	if request.Target.Kind == "agent" && strings.TrimSpace(request.Target.ID) != "" {
		parentID = request.Target.ID
	}
	tools, err = r.withDelegationTools(ctx, service.Log, service.SessionID, r.ApprovalMode(), parentID, tools)
	if err != nil {
		return err
	}
	for i, call := range calls {
		result, runErr := tools.RunTool(ctx, call)
		if runErr != nil {
			if logErr := service.LogToolError(ctx, call, runErr); logErr != nil {
				return logErr
			}
			if errors.Is(runErr, permission.ErrApprovalRequired) {
				for _, skipped := range calls[i+1:] {
					if logErr := service.LogToolSkipped(ctx, skipped, call.Name); logErr != nil {
						return logErr
					}
				}
				return commands.ErrApprovalRequired
			}
			return runErr
		}
		if err := service.LogToolResult(ctx, call, result); err != nil {
			return err
		}
	}
	deps, err := r.askDependencies(ctx, tools, service.Log)
	if err != nil {
		return err
	}
	_, err = commands.ContinueAfterApproval(ctx, io.Discard, deps, commands.ContinueOptions{
		SessionID:   string(service.SessionID),
		TurnContext: approvalContinuationContext(request),
	})
	return err
}

func approvalContinuationContext(request workbench.ApprovalContinuationRequest) string {
	parts := []string{"Approval continuation:"}
	if request.Permission.Action != "" {
		parts = append(parts, "approved action: "+string(request.Permission.Action))
	}
	if strings.TrimSpace(request.Permission.Subject) != "" {
		parts = append(parts, "approved subject: "+request.Permission.Subject)
	}
	if strings.TrimSpace(request.Permission.Reason) != "" {
		parts = append(parts, "approved reason: "+request.Permission.Reason)
	}
	parts = append(parts, "The approved tool call has been executed. Continue the outstanding user request from the new tool output.")
	return strings.Join(parts, "\n")
}

func (r *Runtime) askDependencies(ctx context.Context, tools ports.ToolRegistry, eventLog ports.EventLog) (commands.AskDependencies, error) {
	client, err := buildModelClient(r.Provider)
	if err != nil {
		return commands.AskDependencies{}, err
	}
	if tools == nil {
		tools = r.currentToolsForApproval(r.ApprovalMode())
	}
	writableRoots := []string(nil)
	builder := prompt.NewBuilder()
	if r.ApprovalMode() != permission.ModeReadOnly {
		writableRoots = []string{r.WorkspaceRoot}
		builder.Developer = strings.TrimSpace(builder.Developer + "\n\nUse apply_patch for scoped workspace edits and report changed files and verification. First call apply_patch without accepted or with accepted=false to preview the patch; only call it again with accepted=true after the preview is available.")
		builder.Permissions = fmt.Sprintf("Reads are allowed inside the workspace. Approval mode is %s. apply_patch may write only after the patch preview has been produced, the preview_token is supplied, and the active permission policy allows the write. Do not attempt path traversal. Built-in shell mutation, destructive git operations, and network tools are unavailable.", r.ApprovalMode())
		if r.MCP != nil {
			builder.Permissions += " Configured MCP tools may expose additional read, write, shell, network, or external-write capabilities; use only exposed tools and expect FreeCode to enforce their approval policy."
		}
	}
	if eventLog == nil {
		eventLog = r.EventLog
	}
	return commands.AskDependencies{
		Model:                   r.Model,
		Client:                  client,
		Tools:                   tools,
		Log:                     eventLog,
		Prompt:                  builder,
		Env:                     r.environment(ctx, writableRoots),
		ContextBudget:           r.ContextBudget,
		SessionContextMaxTokens: 4096,
	}, nil
}

func (r *Runtime) ApprovalMode() permission.Mode {
	if r == nil || r.Approval == nil {
		return permission.ModeAsk
	}
	return r.Approval.Mode()
}

func (r *Runtime) currentToolsForApproval(mode permission.Mode) ports.ToolRegistry {
	var base ports.ToolRegistry
	if mode == permission.ModeReadOnly {
		base = r.ReadTools
	} else {
		base = r.WriteTools
	}
	tools, err := composeActiveTools(base, r.MCP, mode, r.Approval)
	if err != nil {
		return base
	}
	return tools
}

func (r *Runtime) environment(ctx context.Context, writableRoots []string) prompt.Environment {
	return prompt.Environment{
		WorkspaceRoot: r.WorkspaceRoot,
		Shell:         os.Getenv("SHELL"),
		GitBranch:     gitBranch(ctx, r.Git),
		Platform:      runtime.GOOS + "/" + runtime.GOARCH,
		WritableRoots: writableRoots,
	}
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
	return sessionLaunch{ID: id, Path: filepath.Join(DefaultSessionsDir, string(id)+".jsonl")}
}

func newLaunchSessionID(now time.Time) session.ID {
	utc := now.UTC()
	return session.ID(fmt.Sprintf("session-%s-%09d", utc.Format("20060102-150405"), utc.Nanosecond()))
}

func sessionIDFromPath(path string) session.ID {
	if filepath.Base(path) == "latest.jsonl" || filepath.Clean(path) == filepath.Clean(filepath.Join(DefaultSessionsDir, "latest.jsonl")) {
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

func composeTools(base ports.ToolRegistry, extra ports.ToolRegistry) (ports.ToolRegistry, error) {
	return toolregistry.NewCompositeToolRegistry(toolregistry.FromRegistry(base), toolregistry.FromRegistry(extra))
}

func composeActiveTools(base ports.ToolRegistry, manager *mcpadapter.Manager, mode permission.Mode, gate ports.PermissionGate) (ports.ToolRegistry, error) {
	providers := []toolregistry.ToolProvider{toolregistry.FromRegistry(base)}
	if manager != nil {
		providers = append(providers, manager.Provider(mode, gate))
	}
	return toolregistry.NewCompositeToolRegistry(providers...)
}
