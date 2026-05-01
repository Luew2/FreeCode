package workbench

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Luew2/FreeCode/internal/adapters/tools/builtin"
	"github.com/Luew2/FreeCode/internal/adapters/workspace/localfs"
	"github.com/Luew2/FreeCode/internal/core/artifact"
	"github.com/Luew2/FreeCode/internal/core/model"
	"github.com/Luew2/FreeCode/internal/core/permission"
	"github.com/Luew2/FreeCode/internal/core/session"
	"github.com/Luew2/FreeCode/internal/ports"
)

func TestServiceLoadsArtifactsAndCopiesCode(t *testing.T) {
	log := &memoryLog{events: []session.Event{{
		ID:        "e1",
		SessionID: "s1",
		Type:      session.EventAssistantMessage,
		At:        time.Now(),
		Actor:     "assistant",
		Text:      "Here:\n```go\nfmt.Println(\"hi\")\n```",
	}, {
		ID:        "e2",
		SessionID: "s1",
		Type:      session.EventAgent,
		At:        time.Now(),
		Actor:     "worker",
		Text:      "worker done",
		Payload:   map[string]any{"role": "worker", "status": "completed"},
	}}}
	clipboard := &recordingClipboard{}
	service := &Service{
		Log:       log,
		Git:       fakeGit{status: ports.GitStatus{Branch: "main"}},
		Clipboard: clipboard,
		Approval:  NewApprovalGate(permission.ModeAsk),
		SessionID: "s1",
		Model:     model.NewRef("local", "coder"),
	}

	state, err := service.Copy(context.Background(), "c1", true)
	if err != nil {
		t.Fatalf("Copy returned error: %v", err)
	}
	if clipboard.text != "```go\nfmt.Println(\"hi\")\n```" {
		t.Fatalf("clipboard = %q, want fenced code", clipboard.text)
	}
	if state.Branch != "main" || state.TokenEstimate == 0 {
		t.Fatalf("state = %#v, want branch and token estimate", state)
	}
	if len(state.Agents) != 1 || state.Agents[0].Name != "worker" || state.Agents[0].Status != "completed" {
		t.Fatalf("agents = %#v, want worker completion", state.Agents)
	}
}

func TestServiceLoadsAgentMetadataAndDetailsAgent(t *testing.T) {
	log := &memoryLog{events: []session.Event{{
		ID:        "e1",
		SessionID: "s1",
		Type:      session.EventAgent,
		At:        time.Now(),
		Actor:     "worker",
		Text:      "implemented pane jumps",
		Payload: map[string]any{
			"task_id":        "task-7",
			"role":           "worker",
			"status":         "completed",
			"changed_files":  []any{"README.md", "internal/adapters/tui2/workbench.go"},
			"tests_run":      []any{"go test ./internal/adapters/tui2"},
			"findings":       []any{"none"},
			"open_questions": []any{"ship it?"},
			"current_step":   "verified",
		},
	}}}
	service := &Service{Log: log, SessionID: "s1"}

	state, err := service.Load(context.Background())
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if len(state.Agents) != 1 {
		t.Fatalf("agents = %#v, want one agent", state.Agents)
	}
	agent := state.Agents[0]
	if agent.TaskID != "task-7" || agent.Summary != "implemented pane jumps" || agent.CurrentStep != "verified" {
		t.Fatalf("agent = %#v, want task metadata", agent)
	}
	if len(agent.ChangedFiles) != 2 || agent.ChangedFiles[0] != "README.md" {
		t.Fatalf("changed files = %#v, want README and workbench", agent.ChangedFiles)
	}
	if len(agent.TestsRun) != 1 || agent.TestsRun[0] != "go test ./internal/adapters/tui2" {
		t.Fatalf("tests = %#v, want go test", agent.TestsRun)
	}

	state, err = service.Detail(context.Background(), "a1")
	if err != nil {
		t.Fatalf("Detail returned error: %v", err)
	}
	if !strings.Contains(state.Detail.Body, "Task: task-7") ||
		!strings.Contains(state.Detail.Body, "Changed files:") ||
		!strings.Contains(state.Detail.Body, "go test ./internal/adapters/tui2") ||
		!strings.Contains(state.Detail.Body, "Questions:") {
		t.Fatalf("detail body missing agent metadata:\n%s", state.Detail.Body)
	}
}

func TestServiceFiltersAgentTranscriptIntoAgentConversation(t *testing.T) {
	log := &memoryLog{events: []session.Event{{
		ID:        "e1",
		SessionID: "s1",
		Type:      session.EventUserMessage,
		At:        time.Now(),
		Actor:     "user",
		Text:      "build the thing",
	}, {
		ID:        "e2",
		SessionID: "s1",
		Type:      session.EventAgent,
		At:        time.Now(),
		Actor:     "worker",
		Text:      "worker started",
		Payload: map[string]any{
			"task_id": "task-1",
			"role":    "worker",
			"status":  "running",
		},
	}, {
		ID:        "e3",
		SessionID: "s1",
		Type:      session.EventAssistantMessage,
		At:        time.Now(),
		Actor:     "worker",
		Text:      "subagent private trace",
		Payload: map[string]any{
			"task_session": "task-1",
		},
	}}}
	service := &Service{Log: log, SessionID: "s1"}

	state, err := service.Load(context.Background())
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if len(state.Transcript) != 2 {
		t.Fatalf("main transcript = %#v, want user plus agent card", state.Transcript)
	}
	if state.Transcript[1].Kind != TranscriptAgent || state.Transcript[1].Meta["agent_id"] != "a1" {
		t.Fatalf("agent transcript card = %#v, want agent a1", state.Transcript[1])
	}
	if strings.Contains(fmt.Sprint(state.Transcript), "subagent private trace") {
		t.Fatalf("main transcript leaked subagent trace: %#v", state.Transcript)
	}

	state, err = service.SetActiveConversation(context.Background(), ConversationTarget{Kind: "agent", ID: "a1"})
	if err != nil {
		t.Fatalf("SetActiveConversation returned error: %v", err)
	}
	if state.ActiveConversation.Kind != "agent" || state.ActiveConversation.ID != "a1" {
		t.Fatalf("active conversation = %#v, want agent a1", state.ActiveConversation)
	}
	if len(state.Transcript) != 2 {
		t.Fatalf("agent transcript = %#v, want agent card plus trace", state.Transcript)
	}
	if state.Transcript[0].Kind != TranscriptAgent || state.Transcript[1].Text != "subagent private trace" {
		t.Fatalf("agent transcript = %#v, want agent card plus private trace", state.Transcript)
	}
}

func TestServiceConsolidatesAgentStatusEvents(t *testing.T) {
	log := &memoryLog{events: []session.Event{{
		ID:        "e1",
		SessionID: "s1",
		Type:      session.EventAgent,
		At:        time.Now(),
		Actor:     "worker",
		Text:      "worker running",
		Payload: map[string]any{
			"task_id": "task-1",
			"role":    "worker",
			"status":  "running",
		},
	}, {
		ID:        "e2",
		SessionID: "s1",
		Type:      session.EventAgent,
		At:        time.Now(),
		Actor:     "worker",
		Text:      "worker done",
		Payload: map[string]any{
			"task_id":       "task-1",
			"role":          "worker",
			"status":        "completed",
			"changed_files": []any{"README.md"},
		},
	}}}
	service := &Service{Log: log, SessionID: "s1"}

	state, err := service.Load(context.Background())
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if len(state.Agents) != 1 {
		t.Fatalf("agents = %#v, want one consolidated agent", state.Agents)
	}
	if state.Agents[0].ID != "a1" || state.Agents[0].Status != "completed" || len(state.Agents[0].ChangedFiles) != 1 {
		t.Fatalf("agent = %#v, want completed a1 with changed files", state.Agents[0])
	}
}

func TestServiceKeepsSwarmLifecycleOutOfAgentList(t *testing.T) {
	log := &memoryLog{events: []session.Event{{
		ID:        "e1",
		SessionID: "s1",
		Type:      session.EventAgent,
		At:        time.Now(),
		Actor:     "swarm",
		Text:      "run everything",
		Payload: map[string]any{
			"status": "running",
		},
	}, {
		ID:        "e2",
		SessionID: "s1",
		Type:      session.EventAgent,
		At:        time.Now(),
		Actor:     "swarm",
		Text:      "swarm completed",
		Payload: map[string]any{
			"status": "completed",
		},
	}}}
	service := &Service{Log: log, SessionID: "s1"}

	state, err := service.Load(context.Background())
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if len(state.Agents) != 0 {
		t.Fatalf("agents = %#v, want no fake swarm conversations", state.Agents)
	}
	if len(state.Transcript) != 2 || state.Transcript[0].Kind != TranscriptContext || state.Transcript[1].Kind != TranscriptContext {
		t.Fatalf("transcript = %#v, want swarm status context cells", state.Transcript)
	}
}

func TestServiceCanCopyAndOpenTranscriptMessage(t *testing.T) {
	log := &memoryLog{events: []session.Event{{
		ID:        "e1",
		SessionID: "s1",
		Type:      session.EventAssistantMessage,
		At:        time.Now(),
		Actor:     "assistant",
		Text:      "full assistant message",
	}}}
	clipboard := &recordingClipboard{}
	service := &Service{Log: log, Clipboard: clipboard, SessionID: "s1"}

	state, err := service.Copy(context.Background(), "m1", false)
	if err != nil {
		t.Fatalf("Copy returned error: %v", err)
	}
	if clipboard.text != "full assistant message" {
		t.Fatalf("clipboard = %q, want message text", clipboard.text)
	}
	state, err = service.Open(context.Background(), "m1")
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	if state.Detail.ID != "m1" || state.Detail.Kind != "message" || state.Detail.Body != "full assistant message" {
		t.Fatalf("detail = %#v, want message detail", state.Detail)
	}
}

func TestApprovePreviewPatchGrantsAndAppliesExactPatch(t *testing.T) {
	const patchDigest = "digest123"
	log := &memoryLog{events: []session.Event{{
		ID:        "e1",
		SessionID: "s1",
		Type:      session.EventTool,
		At:        time.Now(),
		Actor:     "tool",
		Text:      "preview patch p1",
		Payload: map[string]any{
			"name":      "apply_patch",
			"arguments": `{"summary":"edit","changes":[{"path":"README.md","old_text":"old","new_text":"new"}]}`,
			"artifact": map[string]any{
				"id":    "p1",
				"kind":  "patch",
				"title": "edit",
				"body":  "--- a/README.md\n+++ b/README.md\n",
				"metadata": map[string]any{
					"changed_files": "README.md",
					"patch_digest":  patchDigest,
					"preview_token": "tok",
					"state":         "preview",
				},
			},
		},
	}}}
	tools := &recordingTools{}
	gate := NewApprovalGate(permission.ModeAsk)
	service := &Service{
		Log:       log,
		Tools:     tools,
		Approval:  gate,
		SessionID: "s1",
		Now:       func() time.Time { return time.Unix(10, 0) },
	}

	state, err := service.Approve(context.Background(), "p1")
	if err != nil {
		t.Fatalf("Approve returned error: %v", err)
	}
	if tools.call.Name != "apply_patch" {
		t.Fatalf("tool call = %#v, want apply_patch", tools.call)
	}
	var args map[string]any
	if err := json.Unmarshal(tools.call.Arguments, &args); err != nil {
		t.Fatalf("Unmarshal call args: %v", err)
	}
	if args["accepted"] != true || args["preview_token"] != "tok" {
		t.Fatalf("args = %#v, want accepted true and preview token", args)
	}
	decision, err := gate.Decide(context.Background(), permission.Request{
		Action:  permission.ActionWrite,
		Subject: "README.md",
		Reason:  "apply_patch:" + patchDigest,
	})
	if err != nil {
		t.Fatalf("Decide returned error: %v", err)
	}
	if decision != permission.DecisionAllow {
		t.Fatalf("decision = %q, want allow", decision)
	}
	if !strings.Contains(state.Notice, "approved and applied p1") {
		t.Fatalf("notice = %q, want applied notice", state.Notice)
	}
	if len(log.events) < 3 {
		t.Fatalf("events = %#v, want applied tool and approval events appended", log.events)
	}
	if log.events[len(log.events)-2].Type != session.EventTool || log.events[len(log.events)-1].Type != session.EventPermissionRequested {
		t.Fatalf("events = %#v, want tool apply followed by approval event", log.events)
	}
}

func TestApprovalGateRejectsExactRequest(t *testing.T) {
	gate := NewApprovalGate(permission.ModeAuto)
	request := permission.Request{Action: permission.ActionWrite, Subject: "README.md", Reason: "apply_patch:digest"}
	gate.Reject(request)

	decision, err := gate.Decide(context.Background(), request)
	if err != nil {
		t.Fatalf("Decide returned error: %v", err)
	}
	if decision != permission.DecisionDeny {
		t.Fatalf("decision = %q, want deny", decision)
	}
	other, err := gate.Decide(context.Background(), permission.Request{Action: permission.ActionWrite, Subject: "README.md", Reason: "apply_patch:other"})
	if err != nil {
		t.Fatalf("Decide other returned error: %v", err)
	}
	if other != permission.DecisionAllow {
		t.Fatalf("other decision = %q, want allow", other)
	}
}

func TestServiceLoadsPendingApprovalsUntilResolved(t *testing.T) {
	log := &memoryLog{events: []session.Event{{
		ID:        "e1",
		SessionID: "s1",
		Type:      session.EventTool,
		At:        time.Now(),
		Actor:     "tool",
		Text:      "preview patch p1",
		Payload: map[string]any{
			"artifact": map[string]any{
				"id":    "p1",
				"kind":  "patch",
				"title": "edit README",
				"body":  "--- a/README.md\n+++ b/README.md\n",
				"metadata": map[string]any{
					"changed_files": "README.md",
					"patch_digest":  "digest123",
					"preview_token": "tok",
					"state":         "preview",
				},
			},
		},
	}}}
	service := &Service{Log: log, SessionID: "s1", Approval: NewApprovalGate(permission.ModeAsk)}

	state, err := service.Load(context.Background())
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if len(state.Approvals) != 1 || state.Approvals[0].ID != "p1" || state.Approvals[0].Subject != "README.md" {
		t.Fatalf("approvals = %#v, want pending p1 for README", state.Approvals)
	}

	log.events = append(log.events, session.Event{
		ID:        "perm-1",
		SessionID: "s1",
		Type:      session.EventPermissionRequested,
		At:        time.Now(),
		Actor:     "user",
		Text:      "rejected p1",
		Payload:   map[string]any{"artifact": "p1", "approval": "rejected"},
	})
	state, err = service.Load(context.Background())
	if err != nil {
		t.Fatalf("Load after reject returned error: %v", err)
	}
	if len(state.Approvals) != 0 {
		t.Fatalf("approvals = %#v, want resolved approval removed", state.Approvals)
	}
}

func TestApproveFailureKeepsPatchApprovalPending(t *testing.T) {
	log := &memoryLog{events: []session.Event{{
		ID:        "e1",
		SessionID: "s1",
		Type:      session.EventTool,
		At:        time.Now(),
		Actor:     "tool",
		Text:      "preview patch p1",
		Payload: map[string]any{
			"name":      "apply_patch",
			"arguments": `{"summary":"edit","changes":[{"path":"README.md","old_text":"old","new_text":"new"}]}`,
			"artifact": map[string]any{
				"id":    "p1",
				"kind":  "patch",
				"title": "edit README",
				"body":  "--- a/README.md\n+++ b/README.md\n",
				"metadata": map[string]any{
					"changed_files": "README.md",
					"patch_digest":  "digest123",
					"preview_token": "tok",
					"state":         "preview",
				},
			},
		},
	}}}
	service := &Service{
		Log:       log,
		Tools:     failingTools{err: "disk full"},
		Approval:  NewApprovalGate(permission.ModeAsk),
		SessionID: "s1",
	}

	if _, err := service.Approve(context.Background(), "p1"); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("Approve error = %v, want disk full", err)
	}
	state, err := service.Load(context.Background())
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if len(state.Approvals) != 1 || state.Approvals[0].ID != "p1" {
		t.Fatalf("approvals = %#v, want p1 still pending", state.Approvals)
	}
	for _, event := range log.events {
		if event.Type == session.EventPermissionRequested {
			t.Fatalf("events = %#v, failed approve should not log resolved approval", log.events)
		}
	}
}

func TestApprovePreviewPatchAfterRestartReplaysPreviewToken(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("old\n"), 0o600); err != nil {
		t.Fatalf("WriteFile README: %v", err)
	}
	workspace, err := localfs.New(root)
	if err != nil {
		t.Fatalf("New workspace: %v", err)
	}
	args := `{"summary":"edit","changes":[{"path":"README.md","old_text":"old\n","new_text":"new\n"}]}`
	firstGate := NewApprovalGate(permission.ModeAsk)
	firstTools := builtin.NewWritable(workspace.FileSystem(), firstGate)
	preview, err := firstTools.RunTool(context.Background(), model.ToolCall{ID: "call_1", Name: "apply_patch", Arguments: []byte(args)})
	if err != nil {
		t.Fatalf("preview returned error: %v", err)
	}

	// Simulate a process restart: the logged preview remains, but the in-memory
	// preview-token map inside ApplyPatch is gone.
	log := &memoryLog{events: []session.Event{toolEventFromResult("s1", args, preview)}}
	restartedGate := NewApprovalGate(permission.ModeAsk)
	restartedTools := builtin.NewWritable(workspace.FileSystem(), restartedGate)
	service := &Service{Log: log, Tools: restartedTools, Approval: restartedGate, SessionID: "s1"}

	if _, err := service.Approve(context.Background(), "p1"); err != nil {
		t.Fatalf("Approve after restart returned error: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(root, "README.md"))
	if err != nil {
		t.Fatalf("ReadFile README: %v", err)
	}
	if string(data) != "new\n" {
		t.Fatalf("README = %q, want applied patch after restart", string(data))
	}
}

func TestServiceLoadsShellPermissionApproval(t *testing.T) {
	log := &memoryLog{events: []session.Event{{
		ID:        "e1",
		SessionID: "s1",
		Type:      session.EventTool,
		At:        time.Now(),
		Actor:     "tool",
		Text:      "shell permission requires approval",
		Payload: map[string]any{
			"call_id":   "call_1",
			"name":      "run_check",
			"arguments": `{"command":"go test ./..."}`,
			"error":     "shell permission requires approval",
		},
	}}}
	gate := NewApprovalGate(permission.ModeAsk)
	service := &Service{Log: log, Approval: gate, SessionID: "s1"}

	state, err := service.Load(context.Background())
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if len(state.Approvals) != 1 || state.Approvals[0].ID != "u1" || state.Approvals[0].Action != "shell" {
		t.Fatalf("approvals = %#v, want shell approval u1", state.Approvals)
	}
	state, err = service.Approve(context.Background(), "u1")
	if err != nil {
		t.Fatalf("Approve shell returned error: %v", err)
	}
	if len(state.Approvals) != 0 {
		t.Fatalf("approvals = %#v, want shell approval resolved", state.Approvals)
	}
	decision, err := gate.Decide(context.Background(), permission.Request{Action: permission.ActionShell, Subject: "go test ./...", Reason: "run_check"})
	if err != nil {
		t.Fatalf("Decide returned error: %v", err)
	}
	if decision != permission.DecisionAllow {
		t.Fatalf("decision = %q, want allow", decision)
	}
}

func TestFilterCommandsSearchesStructuredMetadata(t *testing.T) {
	commands := DefaultCommands()

	filtered := FilterCommands(commands, "swarm")
	if len(filtered) == 0 || filtered[0].ID != "agent.swarm" {
		t.Fatalf("filtered = %#v, want swarm command first", filtered)
	}
	filtered = FilterCommands(commands, "ctrl+k")
	if len(filtered) != 1 || filtered[0].ID != "palette.open" {
		t.Fatalf("filtered = %#v, want palette command by keybinding", filtered)
	}
	filtered = FilterCommands(commands, "clipboard")
	if len(filtered) < 2 {
		t.Fatalf("filtered = %#v, want copy commands by keyword", filtered)
	}
	help := renderCommands(commands)
	if strings.Contains(help, ":swarm/:agent") {
		t.Fatalf("help = %q, should not advertise :agent as swarm", help)
	}
	if !strings.Contains(help, ":agent <prompt>") || !strings.Contains(help, ":swarm <prompt>") {
		t.Fatalf("help = %q, want separate agent and swarm commands", help)
	}
}

func TestCommandRegistryPaletteCompletionAndCopyContract(t *testing.T) {
	registry := DefaultCommandRegistry()
	palette := registry.Palette("mouse")
	if len(palette) == 0 {
		t.Fatalf("palette missing mouse command")
	}
	seen := map[string]Command{}
	for _, command := range registry.Palette("") {
		seen[command.ID] = command
	}
	for _, id := range []string{"item.copy", "item.copy.full", "item.copy.select", "mouse.toggle", "tab.ops", "terminal.share_output", "buffer.list", "model.list", "mcp.status", "mcp.tools", "mcp.reload", "mcp.doctor", "help.tutorial"} {
		if _, ok := seen[id]; !ok {
			t.Fatalf("palette missing %s", id)
		}
	}
	if seen["item.copy"].Description == seen["item.copy.select"].Description {
		t.Fatalf("copy and visual commands should have distinct descriptions")
	}
	if !strings.Contains(seen["item.copy"].Keybinding, "y") || !strings.Contains(seen["item.copy.select"].Keybinding, "v") || !strings.Contains(seen["mouse.toggle"].Keybinding, "m") {
		t.Fatalf("copy contract keybindings y/v/m missing: copy=%q visual=%q mouse=%q", seen["item.copy"].Keybinding, seen["item.copy.select"].Keybinding, seen["mouse.toggle"].Keybinding)
	}

	state := State{
		Files:    []WorkspaceFile{{ID: "f1", Path: "internal/app/workbench/workbench.go"}},
		Sessions: []SessionSummary{{ID: "session-1"}},
		Agents:   []AgentItem{{ID: "a1", Name: "worker"}},
	}
	if got, ok := registry.Complete("ed", state); !ok || got != "edit " {
		t.Fatalf("Complete edit = %q/%v, want edit + space", got, ok)
	}
	if got, ok := registry.Complete("edit internal/app/work", state); !ok || got != "edit internal/app/workbench/workbench.go" {
		t.Fatalf("Complete file = %q/%v", got, ok)
	}
	if got, ok := registry.Complete("resume sess", state); !ok || got != "resume session-1" {
		t.Fatalf("Complete session = %q/%v", got, ok)
	}
	if got, ok := registry.Complete("b wor", state); !ok || got != "b worker" {
		t.Fatalf("Complete buffer = %q/%v", got, ok)
	}
	if invocation, ok := registry.ResolveLine(":mcp tools"); !ok || invocation.ID != "mcp.tools" {
		t.Fatalf("ResolveLine :mcp tools = %#v/%v", invocation, ok)
	}
	if invocation, ok := registry.ResolveLine(":tutorial"); !ok || invocation.ID != "help.tutorial" {
		t.Fatalf("ResolveLine :tutorial = %#v/%v", invocation, ok)
	}
}

func TestServiceMCPStatusToolsAndReload(t *testing.T) {
	mcp := &fakeMCPController{status: ports.MCPStatus{
		Enabled: true,
		Servers: []ports.MCPServerStatus{{Name: "exa", State: "ready", Command: "npx -y exa-mcp-server", ToolCount: 1}},
		Tools: []ports.MCPToolStatus{{
			PublicName:   "mcp_exa_web_search",
			ServerName:   "exa",
			OriginalName: "web_search",
			Visible:      true,
			Capabilities: []string{"network"},
		}},
	}}
	service := &Service{SessionID: "s1", MCP: mcp, Approval: NewApprovalGate(permission.ModeAsk)}
	status, err := service.MCPStatus(context.Background())
	if err != nil {
		t.Fatalf("MCPStatus: %v", err)
	}
	if !strings.Contains(status.Detail.Body, "exa") {
		t.Fatalf("status body = %s", status.Detail.Body)
	}
	tools, err := service.MCPTools(context.Background())
	if err != nil {
		t.Fatalf("MCPTools: %v", err)
	}
	if !strings.Contains(tools.Detail.Body, "mcp_exa_web_search") {
		t.Fatalf("tools body = %s", tools.Detail.Body)
	}
	if _, err := service.MCPReload(context.Background()); err != nil {
		t.Fatalf("MCPReload: %v", err)
	}
	if mcp.reloads != 1 {
		t.Fatalf("reloads = %d, want 1", mcp.reloads)
	}
}

func TestServiceLoadsSessionsWorkspaceFilesGitAndCompletions(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("hello\n"), 0o600); err != nil {
		t.Fatalf("WriteFile README: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "internal"), 0o755); err != nil {
		t.Fatalf("MkdirAll internal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "internal", "app.go"), []byte("package internal\n"), 0o600); err != nil {
		t.Fatalf("WriteFile app.go: %v", err)
	}
	workspace, err := localfs.New(root)
	if err != nil {
		t.Fatalf("New workspace: %v", err)
	}
	sessions := &memorySessionIndex{sessions: []SessionSummary{{
		ID:            "s1",
		Title:         "Current work",
		WorkspaceRoot: workspace.Root(),
		UpdatedAt:     time.Unix(20, 0),
	}}}
	service := &Service{
		Log:           &memoryLog{},
		Git:           fakeGit{status: ports.GitStatus{Branch: "main", ChangedFiles: []string{" M README.md"}}, diff: "diff --git a/README.md b/README.md\n"},
		Files:         workspace.FileSystem(),
		Sessions:      sessions,
		SessionID:     "s1",
		WorkspaceRoot: workspace.Root(),
		Model:         model.NewRef("local", "coder"),
	}

	state, err := service.Load(context.Background())
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if len(state.Sessions) != 1 || state.Sessions[0].ID != "s1" {
		t.Fatalf("sessions = %#v, want s1", state.Sessions)
	}
	if len(state.Files) != 2 || state.Files[0].Path != "README.md" {
		t.Fatalf("files = %#v, want sorted workspace files", state.Files)
	}
	if len(state.GitFiles) != 1 || state.GitFiles[0].ID != "g1" || state.GitFiles[0].Path != "README.md" {
		t.Fatalf("git files = %#v, want changed README", state.GitFiles)
	}
	if len(state.CompletionCandidates) == 0 {
		t.Fatalf("completion candidates empty")
	}

	state, err = service.Detail(context.Background(), "g1")
	if err != nil {
		t.Fatalf("Detail git file returned error: %v", err)
	}
	if state.Detail.Kind != "git" || !strings.Contains(state.Detail.Body, "diff --git") {
		t.Fatalf("git detail = %#v, want diff body", state.Detail)
	}
}

func TestServiceShowsInFlightModelStream(t *testing.T) {
	log := &memoryLog{events: []session.Event{{
		ID:        "e1",
		SessionID: "s1",
		Type:      session.EventUserMessage,
		At:        time.Now(),
		Actor:     "user",
		Text:      "hello",
	}, {
		ID:        "e2",
		SessionID: "s1",
		Type:      session.EventModel,
		At:        time.Now(),
		Actor:     "model",
		Payload:   map[string]any{"event_type": string(model.EventStarted)},
	}, {
		ID:        "e3",
		SessionID: "s1",
		Type:      session.EventModel,
		At:        time.Now(),
		Actor:     "model",
		Text:      "streaming ",
		Payload:   map[string]any{"event_type": string(model.EventTextDelta)},
	}, {
		ID:        "e4",
		SessionID: "s1",
		Type:      session.EventModel,
		At:        time.Now(),
		Actor:     "model",
		Text:      "reply",
		Payload:   map[string]any{"event_type": string(model.EventTextDelta)},
	}}}
	service := &Service{Log: log, SessionID: "s1", Model: model.NewRef("local", "coder")}

	state, err := service.Load(context.Background())
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if len(state.Transcript) != 2 {
		t.Fatalf("transcript = %#v, want user plus streaming assistant", state.Transcript)
	}
	streaming := state.Transcript[1]
	if streaming.Kind != TranscriptStreaming || !streaming.Streaming || streaming.Actor != "assistant streaming" || streaming.Text != "streaming reply" {
		t.Fatalf("streaming transcript = %#v, want assistant streaming reply", streaming)
	}

	log.events = append(log.events, session.Event{
		ID:        "e5",
		SessionID: "s1",
		Type:      session.EventAssistantMessage,
		At:        time.Now(),
		Actor:     "assistant",
		Text:      "final reply",
	})
	state, err = service.Load(context.Background())
	if err != nil {
		t.Fatalf("Load with final returned error: %v", err)
	}
	if len(state.Transcript) != 2 || state.Transcript[1].Actor != "assistant" || state.Transcript[1].Text != "final reply" {
		t.Fatalf("transcript after final = %#v, want final assistant without duplicate stream", state.Transcript)
	}
	if state.Transcript[1].Kind != TranscriptAssistant || state.Transcript[1].Streaming {
		t.Fatalf("final transcript = %#v, want finalized assistant kind", state.Transcript[1])
	}
}

func TestServiceKeepsAssistantDraftBeforeToolCall(t *testing.T) {
	log := &memoryLog{events: []session.Event{{
		ID:        "e1",
		SessionID: "s1",
		Type:      session.EventUserMessage,
		At:        time.Now(),
		Actor:     "user",
		Text:      "run pwd",
	}, {
		ID:        "e2",
		SessionID: "s1",
		Type:      session.EventModel,
		At:        time.Now(),
		Actor:     "model",
		Payload:   map[string]any{"event_type": string(model.EventStarted)},
	}, {
		ID:        "e3",
		SessionID: "s1",
		Type:      session.EventModel,
		At:        time.Now(),
		Actor:     "model",
		Text:      "I will check the terminal.",
		Payload:   map[string]any{"event_type": string(model.EventTextDelta)},
	}, {
		ID:        "e4",
		SessionID: "s1",
		Type:      session.EventModel,
		At:        time.Now(),
		Actor:     "model",
		Payload: map[string]any{
			"event_type": string(model.EventToolCall),
			"tool_call": map[string]any{
				"id":        "call_1",
				"name":      "terminal_read",
				"arguments": `{"lines":20}`,
			},
		},
	}}}
	service := &Service{Log: log, SessionID: "s1", Model: model.NewRef("local", "coder")}

	state, err := service.Load(context.Background())
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if len(state.Transcript) != 3 {
		t.Fatalf("transcript = %#v, want user, assistant draft, tool", state.Transcript)
	}
	if state.Transcript[1].Kind != TranscriptAssistant || state.Transcript[1].Text != "I will check the terminal." {
		t.Fatalf("draft = %#v, want assistant draft preserved", state.Transcript[1])
	}
	if state.Transcript[2].Kind != TranscriptTool || state.Transcript[2].Title != "terminal_read" {
		t.Fatalf("tool = %#v, want terminal_read tool cell", state.Transcript[2])
	}
}

func TestServiceMapsToolContextPatchAndErrorTranscriptCells(t *testing.T) {
	log := &memoryLog{events: []session.Event{{
		ID:        "e1",
		SessionID: "s1",
		Type:      session.EventTool,
		At:        time.Now(),
		Actor:     "tool",
		Text:      "preview patch",
		Payload: map[string]any{
			"name": "apply_patch",
			"artifact": map[string]any{
				"id":    "p1",
				"kind":  "patch",
				"title": "edit README",
				"body":  "diff",
				"metadata": map[string]any{
					"state": "preview",
				},
			},
		},
	}, {
		ID:        "e2",
		SessionID: "s1",
		Type:      session.EventContextCompacted,
		At:        time.Now(),
		Actor:     "context",
		Text:      "context compacted",
	}, {
		ID:        "e3",
		SessionID: "s1",
		Type:      session.EventError,
		At:        time.Now(),
		Actor:     "model",
		Text:      "failed",
	}}}
	service := &Service{Log: log, SessionID: "s1", Approval: NewApprovalGate(permission.ModeAsk)}

	state, err := service.Load(context.Background())
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	kinds := make([]TranscriptKind, 0, len(state.Transcript))
	for _, item := range state.Transcript {
		kinds = append(kinds, item.Kind)
	}
	want := []TranscriptKind{TranscriptPatch, TranscriptContext, TranscriptError}
	if fmt.Sprint(kinds) != fmt.Sprint(want) {
		t.Fatalf("transcript kinds = %#v, want %#v", kinds, want)
	}
	if state.Transcript[0].ArtifactID != "p1" || state.Transcript[0].Status != "preview" {
		t.Fatalf("patch transcript = %#v, want artifact/status metadata", state.Transcript[0])
	}
}

func TestOpenPathRequiresConfiguredNeovim(t *testing.T) {
	service := &Service{
		Log:           &memoryLog{},
		Editor:        &fakeEditor{},
		EditorCommand: "definitely-not-freecode-nvim",
		SessionID:     "s1",
	}
	state, err := service.OpenPath(context.Background(), "README.md", 1)
	if err == nil {
		t.Fatal("OpenPath returned nil error, want missing editor error")
	}
	if !strings.Contains(err.Error(), "Neovim is required") {
		t.Fatalf("error = %q, want Neovim requirement", err)
	}
	if !strings.Contains(state.Editor.Error, "definitely-not-freecode-nvim") {
		t.Fatalf("editor state = %#v, want missing command error", state.Editor)
	}
}

func TestOpenPathLogsUserEditAndShowsGitTab(t *testing.T) {
	log := &memoryLog{}
	git := &sequenceGit{statuses: []ports.GitStatus{
		{Branch: "main"},
		{Branch: "main", ChangedFiles: []string{" M README.md"}},
		{Branch: "main", ChangedFiles: []string{" M README.md"}},
	}}
	service := &Service{
		Log:       log,
		Git:       git,
		Editor:    fakeEditor{},
		SessionID: "s1",
		Now:       func() time.Time { return time.Unix(10, 0) },
	}

	state, err := service.OpenPath(context.Background(), "README.md", 1)
	if err != nil {
		t.Fatalf("OpenPath returned error: %v", err)
	}
	if state.RightTab != RightTabGit {
		t.Fatalf("right tab = %q, want git", state.RightTab)
	}
	if len(log.events) != 1 || log.events[0].Type != session.EventArtifact {
		t.Fatalf("events = %#v, want user edit artifact", log.events)
	}
	artifactPayload, ok := log.events[0].Payload["artifact"].(map[string]any)
	if !ok || artifactPayload["kind"] != "user_edit" || !strings.Contains(fmt.Sprint(artifactPayload["body"]), "README.md") {
		t.Fatalf("artifact payload = %#v, want user_edit README", log.events[0].Payload)
	}
}

func TestOpenPathLogsAlreadyDirtyFileContentEdit(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "README.md")
	if err := os.WriteFile(path, []byte("before\n"), 0o600); err != nil {
		t.Fatalf("WriteFile before: %v", err)
	}
	log := &memoryLog{}
	git := &sequenceGit{statuses: []ports.GitStatus{
		{Branch: "main", ChangedFiles: []string{" M README.md"}},
		{Branch: "main", ChangedFiles: []string{" M README.md"}},
		{Branch: "main", ChangedFiles: []string{" M README.md"}},
	}}
	service := &Service{
		Log:           log,
		Git:           git,
		Editor:        mutatingEditor{path: path, body: "after\n"},
		SessionID:     "s1",
		WorkspaceRoot: root,
		Now:           func() time.Time { return time.Unix(11, 0) },
	}

	state, err := service.OpenPath(context.Background(), "README.md", 1)
	if err != nil {
		t.Fatalf("OpenPath returned error: %v", err)
	}
	if state.RightTab != RightTabGit {
		t.Fatalf("right tab = %q, want git", state.RightTab)
	}
	if len(log.events) != 1 || !strings.Contains(log.events[0].Text, "README.md") {
		t.Fatalf("events = %#v, want user edit for already-dirty README", log.events)
	}
}

func TestRunShellLogsArtifactAndOpensDetail(t *testing.T) {
	log := &memoryLog{}
	service := &Service{
		Log:           log,
		SessionID:     "s1",
		WorkspaceRoot: t.TempDir(),
		Approval:      NewApprovalGate(permission.ModeAuto),
		Now:           func() time.Time { return time.Unix(12, 0) },
	}

	state, err := service.RunShell(context.Background(), "printf freecode")
	if err != nil {
		t.Fatalf("RunShell returned error: %v", err)
	}
	if state.Detail.Kind != "shell" || !strings.Contains(state.Detail.Body, "freecode") {
		t.Fatalf("detail = %#v, want shell output", state.Detail)
	}
	if state.Detail.Meta["local_only"] != "true" || state.Detail.Meta["share_with_model"] != "false" {
		t.Fatalf("detail metadata = %#v, want local-only shell artifact", state.Detail.Meta)
	}
	if len(state.Transcript) != 1 || state.Transcript[0].Kind != TranscriptShell || state.Transcript[0].Actor != "local" {
		t.Fatalf("transcript = %#v, want local shell cell", state.Transcript)
	}
	if !strings.Contains(state.Transcript[0].Text, "freecode") || strings.Contains(state.Transcript[0].Text, "printf freecode") {
		t.Fatalf("shell transcript text = %q, want output without command echo", state.Transcript[0].Text)
	}
	if state.TokenEstimate != 0 {
		t.Fatalf("token estimate = %d, want local shell excluded from model token estimate", state.TokenEstimate)
	}
	if len(log.events) != 1 || log.events[0].Type != session.EventArtifact {
		t.Fatalf("events = %#v, want shell artifact", log.events)
	}
	payload, ok := log.events[0].Payload["artifact"].(map[string]any)
	if !ok {
		t.Fatalf("artifact payload = %#v, want artifact map", log.events[0].Payload)
	}
	metadata, ok := payload["metadata"].(map[string]any)
	if !ok || metadata["local_only"] != "true" || metadata["share_with_model"] != "false" {
		t.Fatalf("artifact metadata = %#v, want local-only metadata", payload["metadata"])
	}
}

func TestShareTerminalLogsModelVisibleContext(t *testing.T) {
	log := &memoryLog{}
	service := &Service{
		Log:       log,
		SessionID: "s1",
		Now:       func() time.Time { return time.Unix(13, 0) },
	}

	state, err := service.ShareTerminal(context.Background(), "Recent terminal", "go test ./...\nPASS")
	if err != nil {
		t.Fatalf("ShareTerminal returned error: %v", err)
	}
	if state.Detail.Kind != "terminal" || state.Detail.Meta["share_with_model"] != "true" {
		t.Fatalf("detail = %#v, want shared terminal artifact", state.Detail)
	}
	if state.RightTab != RightTabTerm {
		t.Fatalf("right tab = %q, want term", state.RightTab)
	}
	if len(state.Transcript) != 1 || state.Transcript[0].Kind != TranscriptContext || state.Transcript[0].Title != "Recent terminal" {
		t.Fatalf("transcript = %#v, want attached terminal context cell", state.Transcript)
	}
	if state.TokenEstimate == 0 {
		t.Fatalf("token estimate = 0, want shared terminal included in context estimate")
	}
	if len(log.events) != 1 || log.events[0].Type != session.EventArtifact || !strings.Contains(log.events[0].Text, "PASS") {
		t.Fatalf("events = %#v, want model-visible terminal artifact text", log.events)
	}
}

func TestLoadBuildsOperationsAndContextPreview(t *testing.T) {
	log := &memoryLog{}
	ctx := context.Background()
	events := []session.Event{
		{ID: "u1", SessionID: "s1", Type: session.EventUserMessage, Actor: "user", Text: "review this"},
		{ID: "tool1", SessionID: "s1", Type: session.EventTool, Actor: "tool", Text: "ok", Payload: map[string]any{"name": "read_file"}},
	}
	for _, event := range events {
		if err := log.Append(ctx, event); err != nil {
			t.Fatalf("Append returned error: %v", err)
		}
	}
	service := &Service{Log: log, SessionID: "s1"}

	state, err := service.Load(ctx)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if state.ContextPreview.Summary == "" || !strings.Contains(strings.Join(state.ContextPreview.Included, "\n"), "new prompt") {
		t.Fatalf("context preview = %#v, want populated next-turn preview", state.ContextPreview)
	}
	if len(state.Operations.Items) == 0 {
		t.Fatalf("operations = %#v, want operation items", state.Operations)
	}
}

func TestSubmitPromptPassesVisibleConversationAndSharedTerminalContext(t *testing.T) {
	log := &memoryLog{}
	ctx := context.Background()
	events := []session.Event{
		{
			ID:        "a1",
			SessionID: "s1",
			Type:      session.EventAgent,
			Actor:     "explorer",
			Text:      "inspect auth",
			Payload: map[string]any{
				"task_id": "task-1",
				"role":    "explorer",
				"status":  "running",
			},
		},
		{
			ID:        "m1",
			SessionID: "s1",
			Type:      session.EventUserMessage,
			Actor:     "user",
			Text:      "earlier agent question",
			Payload:   map[string]any{"task_session": "task-1"},
		},
		{
			ID:        "m2",
			SessionID: "s1",
			Type:      session.EventAssistantMessage,
			Actor:     "assistant",
			Text:      "earlier agent answer",
			Payload:   map[string]any{"task_session": "task-1"},
		},
		{
			ID:        "term",
			SessionID: "s1",
			Type:      session.EventArtifact,
			Actor:     "user",
			Text:      "go test ./...\nPASS",
			Payload: map[string]any{
				"artifact": map[string]any{
					"id":    "term1",
					"kind":  "terminal",
					"title": "Recent terminal",
					"body":  "go test ./...\nPASS",
					"metadata": map[string]any{
						"share_with_model": "true",
					},
				},
			},
		},
	}
	for _, event := range events {
		if err := log.Append(ctx, event); err != nil {
			t.Fatalf("Append returned error: %v", err)
		}
	}
	var submitted SubmitRequest
	service := &Service{
		Log:       log,
		SessionID: "s1",
		Submit: func(ctx context.Context, request SubmitRequest) error {
			submitted = request
			return nil
		},
	}

	_, err := service.SubmitPrompt(ctx, SubmitRequest{
		Text:   "continue",
		Target: ConversationTarget{Kind: "agent", ID: "a1", Title: "explorer"},
	})
	if err != nil {
		t.Fatalf("SubmitPrompt returned error: %v", err)
	}
	if !strings.Contains(submitted.TurnContext, "Selected agent conversation history") ||
		!strings.Contains(submitted.TurnContext, "earlier agent question") ||
		!strings.Contains(submitted.TurnContext, "earlier agent answer") {
		t.Fatalf("turn context = %q, want selected agent history", submitted.TurnContext)
	}
	if !strings.Contains(submitted.TurnContext, "Terminal output explicitly attached") ||
		!strings.Contains(submitted.TurnContext, "go test ./...") ||
		!strings.Contains(submitted.TurnContext, "PASS") {
		t.Fatalf("turn context = %q, want explicit shared terminal output", submitted.TurnContext)
	}
}

func TestSubmitPromptDoesNotAttachStaleSharedTerminalAfterAssistantReply(t *testing.T) {
	log := &memoryLog{}
	ctx := context.Background()
	events := []session.Event{
		{
			ID:        "term",
			SessionID: "s1",
			Type:      session.EventArtifact,
			Actor:     "user",
			Text:      "SECRET=abc",
			Payload: map[string]any{
				"artifact": map[string]any{
					"id":    "term1",
					"kind":  "terminal",
					"title": "Recent terminal",
					"body":  "SECRET=abc",
					"metadata": map[string]any{
						"share_with_model": "true",
					},
				},
			},
		},
		{ID: "a1", SessionID: "s1", Type: session.EventAssistantMessage, Actor: "assistant", Text: "handled terminal"},
	}
	for _, event := range events {
		if err := log.Append(ctx, event); err != nil {
			t.Fatalf("Append returned error: %v", err)
		}
	}
	var submitted SubmitRequest
	service := &Service{
		Log:       log,
		SessionID: "s1",
		Submit: func(ctx context.Context, request SubmitRequest) error {
			submitted = request
			return nil
		},
	}

	_, err := service.SubmitPrompt(ctx, SubmitRequest{Text: "next"})
	if err != nil {
		t.Fatalf("SubmitPrompt returned error: %v", err)
	}
	if strings.Contains(submitted.TurnContext, "SECRET=abc") {
		t.Fatalf("turn context = %q, want stale shared terminal omitted from active turn context", submitted.TurnContext)
	}
}

func TestSessionLifecycleSwitchesActiveSession(t *testing.T) {
	index := &memorySessionIndex{}
	logs := map[session.ID]*memoryLog{}
	service := &Service{
		Sessions:      index,
		SessionID:     "old",
		WorkspaceRoot: "/workspace/freecode",
		Model:         model.NewRef("local", "coder"),
		Now:           func() time.Time { return time.Unix(100, 123).UTC() },
		LogForSession: func(id session.ID) ports.EventLog {
			logs[id] = &memoryLog{}
			return logs[id]
		},
	}

	state, err := service.NewSession(context.Background(), "Fresh work")
	if err != nil {
		t.Fatalf("NewSession returned error: %v", err)
	}
	if state.SessionID == "old" || string(state.SessionID) == "" {
		t.Fatalf("session id = %q, want new id", state.SessionID)
	}
	if service.Log != logs[state.SessionID] {
		t.Fatalf("active log was not switched")
	}
	if len(index.sessions) != 1 || index.sessions[0].Title != "Fresh work" {
		t.Fatalf("sessions = %#v, want indexed Fresh work", index.sessions)
	}

	resumed, err := service.ResumeSession(context.Background(), state.SessionID)
	if err != nil {
		t.Fatalf("ResumeSession returned error: %v", err)
	}
	if resumed.Notice != "resumed session "+string(state.SessionID) {
		t.Fatalf("notice = %q, want resumed session", resumed.Notice)
	}

	renamed, err := service.RenameSession(context.Background(), "Renamed work")
	if err != nil {
		t.Fatalf("RenameSession returned error: %v", err)
	}
	if renamed.Notice != "renamed session "+string(state.SessionID) || index.sessions[0].Title != "Renamed work" {
		t.Fatalf("renamed notice/index = %q/%#v", renamed.Notice, index.sessions)
	}
}

func TestServiceApprovePreviewPatchAppliesRealFileChange(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("old\n"), 0o600); err != nil {
		t.Fatalf("WriteFile README: %v", err)
	}
	workspace, err := localfs.New(root)
	if err != nil {
		t.Fatalf("New workspace: %v", err)
	}
	gate := NewApprovalGate(permission.ModeAsk)
	tools := builtin.NewWritable(workspace.FileSystem(), gate)

	preview, err := tools.RunTool(context.Background(), model.ToolCall{
		ID:        "call_1",
		Name:      "apply_patch",
		Arguments: []byte(`{"summary":"edit","changes":[{"path":"README.md","old_text":"old\n","new_text":"new\n"}]}`),
	})
	if err != nil {
		t.Fatalf("preview returned error: %v", err)
	}
	log := &memoryLog{events: []session.Event{toolEventFromResult("s1", `{"summary":"edit","changes":[{"path":"README.md","old_text":"old\n","new_text":"new\n"}]}`, preview)}}
	service := &Service{Log: log, Tools: tools, Approval: gate, SessionID: "s1"}

	if _, err := service.Approve(context.Background(), "p1"); err != nil {
		t.Fatalf("Approve returned error: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(root, "README.md"))
	if err != nil {
		t.Fatalf("ReadFile README: %v", err)
	}
	if string(data) != "new\n" {
		t.Fatalf("README = %q, want applied patch", string(data))
	}
}

func TestServiceRejectPreviewPatchKeepsRealPatchBlocked(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("old\n"), 0o600); err != nil {
		t.Fatalf("WriteFile README: %v", err)
	}
	workspace, err := localfs.New(root)
	if err != nil {
		t.Fatalf("New workspace: %v", err)
	}
	gate := NewApprovalGate(permission.ModeAsk)
	tools := builtin.NewWritable(workspace.FileSystem(), gate)
	args := `{"summary":"edit","changes":[{"path":"README.md","old_text":"old\n","new_text":"new\n"}]}`
	preview, err := tools.RunTool(context.Background(), model.ToolCall{ID: "call_1", Name: "apply_patch", Arguments: []byte(args)})
	if err != nil {
		t.Fatalf("preview returned error: %v", err)
	}
	log := &memoryLog{events: []session.Event{toolEventFromResult("s1", args, preview)}}
	service := &Service{Log: log, Tools: tools, Approval: gate, SessionID: "s1"}

	if _, err := service.Reject(context.Background(), "p1"); err != nil {
		t.Fatalf("Reject returned error: %v", err)
	}
	accepted := `{"summary":"edit","accepted":true,"preview_token":"` + preview.Metadata["preview_token"] + `","changes":[{"path":"README.md","old_text":"old\n","new_text":"new\n"}]}`
	_, err = tools.RunTool(context.Background(), model.ToolCall{ID: "call_2", Name: "apply_patch", Arguments: []byte(accepted)})
	if err == nil || !strings.Contains(err.Error(), "write permission denied") {
		t.Fatalf("accepted apply error = %v, want denied", err)
	}
	data, err := os.ReadFile(filepath.Join(root, "README.md"))
	if err != nil {
		t.Fatalf("ReadFile README: %v", err)
	}
	if string(data) != "old\n" {
		t.Fatalf("README = %q, rejected patch changed file", string(data))
	}
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
	go func() {
		defer close(ch)
		for _, event := range l.events {
			if id == "" || event.SessionID == id {
				ch <- event
			}
		}
	}()
	return ch, nil
}

type fakeGit struct {
	status ports.GitStatus
	diff   string
}

func (g fakeGit) Status(ctx context.Context) (ports.GitStatus, error) {
	return g.status, nil
}

func (g fakeGit) Diff(ctx context.Context, paths []string) (string, error) {
	return g.diff, nil
}

type sequenceGit struct {
	statuses []ports.GitStatus
	diff     string
	calls    int
}

func (g *sequenceGit) Status(ctx context.Context) (ports.GitStatus, error) {
	if len(g.statuses) == 0 {
		return ports.GitStatus{}, nil
	}
	index := g.calls
	if index >= len(g.statuses) {
		index = len(g.statuses) - 1
	}
	g.calls++
	return g.statuses[index], nil
}

func (g *sequenceGit) Diff(ctx context.Context, paths []string) (string, error) {
	return g.diff, nil
}

type memorySessionIndex struct {
	sessions []SessionSummary
}

func (i *memorySessionIndex) List(ctx context.Context, workspaceRoot string) ([]SessionSummary, error) {
	var out []SessionSummary
	for _, summary := range i.sessions {
		if workspaceRoot != "" && summary.WorkspaceRoot != workspaceRoot {
			continue
		}
		out = append(out, summary)
	}
	return out, nil
}

func (i *memorySessionIndex) Create(ctx context.Context, summary SessionSummary) error {
	for idx := range i.sessions {
		if i.sessions[idx].ID == summary.ID {
			i.sessions[idx] = summary
			return nil
		}
	}
	i.sessions = append(i.sessions, summary)
	return nil
}

func (i *memorySessionIndex) Rename(ctx context.Context, id session.ID, title string) error {
	for idx := range i.sessions {
		if i.sessions[idx].ID == id {
			i.sessions[idx].Title = title
			return nil
		}
	}
	return errors.New("missing session")
}

type fakeEditor struct{}

func (fakeEditor) Open(ctx context.Context, path string, line int) error {
	return nil
}

type mutatingEditor struct {
	path string
	body string
}

func (e mutatingEditor) Open(ctx context.Context, path string, line int) error {
	return os.WriteFile(e.path, []byte(e.body), 0o600)
}

type recordingClipboard struct {
	text string
}

func (c *recordingClipboard) Copy(ctx context.Context, text string) error {
	c.text = text
	return nil
}

type recordingTools struct {
	call model.ToolCall
}

type failingTools struct {
	err string
}

func toolEventFromResult(sessionID session.ID, arguments string, result ports.ToolResult) session.Event {
	payload := map[string]any{
		"name":      "apply_patch",
		"arguments": arguments,
	}
	if result.Artifact != nil {
		payload["artifact"] = artifactPayload(*result.Artifact)
	}
	for key, value := range result.Metadata {
		payload[key] = value
	}
	return session.Event{
		ID:        "e1",
		SessionID: sessionID,
		Type:      session.EventTool,
		At:        time.Now(),
		Actor:     "tool",
		Text:      result.Content,
		Payload:   payload,
	}
}

func (r *recordingTools) Tools() []model.ToolSpec {
	return nil
}

func (r *recordingTools) RunTool(ctx context.Context, call model.ToolCall) (ports.ToolResult, error) {
	r.call = call
	return ports.ToolResult{
		CallID:  call.ID,
		Content: "applied patch p2",
		Artifact: &artifact.Artifact{
			ID:    artifact.NewID(artifact.KindPatch, 2),
			Kind:  artifact.KindPatch,
			Title: "applied",
			Body:  "--- a/README.md\n+++ b/README.md\n",
			Metadata: map[string]string{
				"patch_id":      "p2",
				"changed_files": "README.md",
				"state":         "applied",
			},
		},
		Metadata: map[string]string{"patch_id": "p2", "state": "applied"},
	}, nil
}

func (t failingTools) Tools() []model.ToolSpec {
	return nil
}

func (t failingTools) RunTool(ctx context.Context, call model.ToolCall) (ports.ToolResult, error) {
	return ports.ToolResult{}, errors.New(t.err)
}

type fakeMCPController struct {
	status  ports.MCPStatus
	reloads int
}

func (m *fakeMCPController) Status(mode permission.Mode) ports.MCPStatus {
	return m.status
}

func (m *fakeMCPController) Reload(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.reloads++
	return nil
}
