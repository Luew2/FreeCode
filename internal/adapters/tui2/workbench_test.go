package tui2

import (
	"context"
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/Luew2/FreeCode/internal/app/workbench"
	"github.com/Luew2/FreeCode/internal/core/permission"
	"github.com/Luew2/FreeCode/internal/core/session"
)

func TestCtrlKPaletteFiltersAndRunsCommand(t *testing.T) {
	controller := &fakeController{state: workbench.State{Approval: permission.ModeAsk, Commands: workbench.DefaultCommands()}}
	m := newModel(context.Background(), controller, controller.state)

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlK})
	if cmd != nil {
		t.Fatalf("ctrl+k returned cmd, want palette only")
	}
	m = next.(model)
	if m.mode != modePalette || m.overlay != overlayPalette {
		t.Fatalf("mode/overlay = %s/%d, want palette", m.mode, m.overlay)
	}
	m = sendRunes(t, m, "compact")
	if commands := m.filteredCommands(); len(commands) != 1 || commands[0].ID != "context.compact" {
		t.Fatalf("commands = %#v, want compact command", commands)
	}

	next, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatalf("enter returned nil cmd, want compact action")
	}
	msg := firstActionMsg(t, cmd)
	next, _ = next.(model).Update(msg)
	m = next.(model)
	if !controller.compacted {
		t.Fatalf("compacted = false, want true")
	}
	if m.mode != modeNormal || m.overlay != overlayNone {
		t.Fatalf("mode/overlay = %s/%d, want normal none", m.mode, m.overlay)
	}
}

func TestInsertEnterSubmitsPrompt(t *testing.T) {
	controller := &fakeController{state: workbench.State{Approval: permission.ModeAsk, Commands: workbench.DefaultCommands()}}
	m := newModel(context.Background(), controller, controller.state)

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'i'}})
	m = sendRunes(t, next.(model), "hello")
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatalf("enter returned nil cmd, want submit")
	}
	msg := firstActionMsg(t, cmd)
	next, _ = next.(model).Update(msg)
	m = next.(model)
	if controller.submitted.Text != "hello" || controller.submitted.Swarm {
		t.Fatalf("submitted = %#v, want normal hello prompt", controller.submitted)
	}
	if m.mode != modeNormal {
		t.Fatalf("mode = %s, want normal", m.mode)
	}
}

func TestColonSSubmitsSwarmThroughMain(t *testing.T) {
	controller := &fakeController{state: workbench.State{
		Approval: permission.ModeAuto,
		Commands: workbench.DefaultCommands(),
		ActiveConversation: workbench.ConversationTarget{
			Kind:  "agent",
			ID:    "a1",
			Title: "worker",
		},
	}}
	m := newModel(context.Background(), controller, controller.state)

	next, cmd := m.executeLine(":s implement this")
	if cmd == nil {
		t.Fatalf(":s returned nil cmd, want submit")
	}
	msg := firstActionMsg(t, cmd)
	next, _ = next.(model).Update(msg)
	if controller.submitted.Text != "implement this" || !controller.submitted.Swarm {
		t.Fatalf("submitted = %#v, want swarm prompt", controller.submitted)
	}
	if controller.submitted.Target.Kind != "main" || controller.submitted.Target.ID != "main" {
		t.Fatalf("target = %#v, want main orchestrator", controller.submitted.Target)
	}
}

func TestColonAgentTargetsSelectedAgent(t *testing.T) {
	controller := &fakeController{state: workbench.State{
		Approval: permission.ModeAuto,
		Commands: workbench.DefaultCommands(),
		Agents: []workbench.AgentItem{
			{ID: "a1", Name: "worker", Role: "worker", Status: "running", TaskID: "task-1"},
		},
	}}
	m := newModel(context.Background(), controller, controller.state)
	m.focus = focusAgents
	m.leftCursor = 1 // first agent row (index 0 is the orchestrator)

	next, cmd := m.executeLine(":agent investigate the bug")
	if cmd == nil {
		t.Fatalf(":agent returned nil cmd, want submit")
	}
	msg := firstActionMsg(t, cmd)
	next, _ = next.(model).Update(msg)
	if controller.submitted.Text != "investigate the bug" {
		t.Fatalf("submitted = %#v, want investigate the bug", controller.submitted)
	}
	if controller.submitted.Target.Kind != "agent" || controller.submitted.Target.ID != "a1" {
		t.Fatalf("target = %#v, want agent a1", controller.submitted.Target)
	}
	_ = next
}

func TestColonAgentRefusesWhenNothingSelected(t *testing.T) {
	controller := &fakeController{state: workbench.State{
		Approval: permission.ModeAuto,
		Commands: workbench.DefaultCommands(),
		Agents: []workbench.AgentItem{
			{ID: "a1", Name: "worker", Role: "worker", Status: "running", TaskID: "task-1"},
		},
	}}
	m := newModel(context.Background(), controller, controller.state)
	// leftCursor stays at 0 (the orchestrator), and ActiveConversation
	// defaults to main — neither resolves to an agent target.

	next, cmd := m.executeLine(":agent investigate the bug")
	if cmd != nil {
		t.Fatalf(":agent without selection returned cmd, want notice only")
	}
	m = next.(model)
	if controller.submitted.Text != "" {
		t.Fatalf("submitted = %#v, want no submission", controller.submitted)
	}
	if !strings.Contains(m.state.Notice, "select an agent first") {
		t.Fatalf("notice = %q, want 'select an agent first'", m.state.Notice)
	}
}

func TestColonAgentRoutesToActiveAgentConversation(t *testing.T) {
	controller := &fakeController{state: workbench.State{
		Approval: permission.ModeAuto,
		Commands: workbench.DefaultCommands(),
		Agents: []workbench.AgentItem{
			{ID: "a2", Name: "explorer", Role: "explorer", Status: "running", TaskID: "task-2"},
		},
		ActiveConversation: workbench.ConversationTarget{Kind: "agent", ID: "a2", Title: "explorer"},
	}}
	m := newModel(context.Background(), controller, controller.state)

	next, cmd := m.executeLine(":agent dig deeper")
	if cmd == nil {
		t.Fatalf(":agent returned nil cmd, want submit")
	}
	msg := firstActionMsg(t, cmd)
	next, _ = next.(model).Update(msg)
	if controller.submitted.Target.Kind != "agent" || controller.submitted.Target.ID != "a2" {
		t.Fatalf("target = %#v, want active agent a2", controller.submitted.Target)
	}
	_ = next
}

func TestInsertLeadingColonCommandRunsCommandInsteadOfChat(t *testing.T) {
	controller := &fakeController{state: workbench.State{Approval: permission.ModeAuto, Commands: workbench.DefaultCommands()}}
	m := newModel(context.Background(), controller, controller.state)

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'i'}})
	m = sendRunes(t, next.(model), ":s review this codebase")
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatalf("enter returned nil cmd, want swarm submit")
	}
	msg := firstActionMsg(t, cmd)
	next, _ = next.(model).Update(msg)
	if controller.submitted.Text != "review this codebase" || !controller.submitted.Swarm {
		t.Fatalf("submitted = %#v, want parsed swarm command", controller.submitted)
	}
}

func TestBusyQueuesSecondSubmitAndIgnoresStaleAction(t *testing.T) {
	// User flow: a turn is already running (m.busy=true) and the user types
	// another prompt. Old behaviour blocked it; new behaviour queues it so
	// long swarm runs don't make the chat composer unusable.
	controller := &fakeController{state: workbench.State{Approval: permission.ModeAuto, Commands: workbench.DefaultCommands()}}
	m := newModel(context.Background(), controller, controller.state)
	m.busy = true
	m.activeRun = 2
	m.actionSeq = 2
	m.composer.SetValue("hello")

	next, cmd := m.submit(false)
	if cmd != nil {
		t.Fatalf("busy submit returned cmd, want queue (no immediate fire)")
	}
	m = next.(model)
	if controller.submitted.Text != "" {
		t.Fatalf("submitted = %#v, want no immediate dispatch while busy", controller.submitted)
	}
	if len(m.submitQueue) != 1 || m.submitQueue[0].text != "hello" {
		t.Fatalf("submitQueue = %#v, want one queued prompt 'hello'", m.submitQueue)
	}
	if !strings.Contains(m.state.Notice, "queued") {
		t.Fatalf("notice = %q, want queued notice", m.state.Notice)
	}

	next, _ = m.Update(actionMsg{runID: 1, state: workbench.State{Notice: "stale"}})
	m = next.(model)
	if m.state.Notice == "stale" {
		t.Fatalf("stale action replaced state")
	}
	// Stale runID must not drain the queue either — that prompt belongs to
	// the still-active activeRun=2.
	if controller.submitted.Text != "" {
		t.Fatalf("submitted after stale = %#v, want no dispatch", controller.submitted)
	}
	if len(m.submitQueue) != 1 {
		t.Fatalf("submitQueue after stale = %d, want 1", len(m.submitQueue))
	}
}

func TestBusyQueueDrainsAfterCurrentRunCompletes(t *testing.T) {
	// User types two prompts during a single busy turn; both should queue,
	// the first should auto-fire when the active run lands, and the second
	// should still be pending behind it.
	controller := &fakeController{state: workbench.State{Approval: permission.ModeAuto, Commands: workbench.DefaultCommands()}}
	m := newModel(context.Background(), controller, controller.state)
	m.busy = true
	m.activeRun = 5
	m.actionSeq = 5

	m.composer.SetValue("first")
	next, _ := m.submit(false)
	m = next.(model)
	m.composer.SetValue("second")
	next, _ = m.submit(false)
	m = next.(model)
	if len(m.submitQueue) != 2 {
		t.Fatalf("submitQueue = %d, want 2", len(m.submitQueue))
	}

	next, cmd := m.Update(actionMsg{runID: 5, state: workbench.State{Approval: permission.ModeAuto, Notice: "turn done"}})
	if cmd == nil {
		t.Fatalf("actionMsg with queued items returned nil cmd, want drain")
	}
	m = next.(model)
	// After the first drain the model has already popped "first" off the
	// queue and started a new run for it. "second" is still queued behind.
	// (We don't execute the returned cmd yet — the controller hasn't been
	// hit, but the queue+busy bookkeeping happens synchronously.)
	if !m.busy {
		t.Fatalf("busy = false after drain, want true (queued prompt now running)")
	}
	if len(m.submitQueue) != 1 || m.submitQueue[0].text != "second" {
		t.Fatalf("submitQueue after first drain = %#v, want ['second']", m.submitQueue)
	}
	// Executing the cmd is what actually delivers the queued prompt to the
	// controller — which is the proof "first" went out before "second".
	msg := firstActionMsg(t, cmd)
	if controller.submitted.Text != "first" {
		t.Fatalf("submitted = %#v, want first queued prompt to fire first", controller.submitted)
	}
	// And consuming that follow-up actionMsg drains "second" too.
	next, _ = m.Update(msg)
	m = next.(model)
	if len(m.submitQueue) != 0 {
		t.Fatalf("submitQueue after both drains = %#v, want empty", m.submitQueue)
	}
}

func TestCancelClearsQueuedSubmits(t *testing.T) {
	controller := &fakeController{state: workbench.State{Approval: permission.ModeAuto, Commands: workbench.DefaultCommands()}}
	m := newModel(context.Background(), controller, controller.state)
	runCtx, cancel := context.WithCancel(context.Background())
	m.busy = true
	m.activeRun = 7
	m.activeCtx = runCtx
	m.activeCancel = cancel
	m.submitQueue = []queuedSubmit{
		{text: "a", approval: permission.ModeAuto},
		{text: "b", approval: permission.ModeAuto},
	}

	next, cmd := m.executeLine(":cancel")
	if cmd != nil {
		t.Fatalf(":cancel returned cmd, want immediate notice update")
	}
	m = next.(model)
	if len(m.submitQueue) != 0 {
		t.Fatalf("submitQueue after :cancel = %d, want 0", len(m.submitQueue))
	}
	if !strings.Contains(m.state.Notice, "cancelled active run and 2") {
		t.Fatalf("notice = %q, want active cancel message", m.state.Notice)
	}
	select {
	case <-runCtx.Done():
	default:
		t.Fatalf("run context was not cancelled")
	}
}

func TestNoqueueClearsQueuedSubmitsWithoutCancellingActiveRun(t *testing.T) {
	controller := &fakeController{state: workbench.State{Approval: permission.ModeAuto, Commands: workbench.DefaultCommands()}}
	m := newModel(context.Background(), controller, controller.state)
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.busy = true
	m.activeRun = 7
	m.activeCtx = runCtx
	m.activeCancel = cancel
	m.submitQueue = []queuedSubmit{
		{text: "a", approval: permission.ModeAuto},
		{text: "b", approval: permission.ModeAuto},
	}

	next, cmd := m.executeLine(":noqueue")
	if cmd != nil {
		t.Fatalf(":noqueue returned cmd, want immediate notice update")
	}
	m = next.(model)
	if len(m.submitQueue) != 0 {
		t.Fatalf("submitQueue after :noqueue = %d, want 0", len(m.submitQueue))
	}
	select {
	case <-runCtx.Done():
		t.Fatalf(":noqueue cancelled active run")
	default:
	}
	if !strings.Contains(m.state.Notice, "cleared 2") {
		t.Fatalf("notice = %q, want cleared-count message", m.state.Notice)
	}
}

func TestApprovalModalApprovesPendingItem(t *testing.T) {
	controller := &fakeController{state: workbench.State{
		Approval: permission.ModeAsk,
		Commands: workbench.DefaultCommands(),
		Approvals: []workbench.ApprovalItem{{
			ID:      "p1",
			Title:   "edit README",
			Action:  "write",
			Subject: "README.md",
			Body:    "--- a/README.md\n+++ b/README.md\n",
		}},
	}}
	m := newModel(context.Background(), controller, controller.state)
	if m.mode != modeApprove || m.overlay != overlayApproval {
		t.Fatalf("mode/overlay = %s/%d, want approval modal", m.mode, m.overlay)
	}

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	if cmd == nil {
		t.Fatalf("approve returned nil cmd")
	}
	msg := firstActionMsg(t, cmd)
	next, _ = next.(model).Update(msg)
	if controller.approved != "p1" {
		t.Fatalf("approved = %q, want p1", controller.approved)
	}
}

func TestUppercaseADoesNotEscalateApprovalMode(t *testing.T) {
	controller := &fakeController{state: workbench.State{Approval: permission.ModeAsk, Commands: workbench.DefaultCommands()}}
	m := newModel(context.Background(), controller, controller.state)
	m.mode = modeNormal

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'A'}})
	if cmd != nil {
		t.Fatalf("A returned cmd, want no approval mutation")
	}
	m = next.(model)
	if controller.state.Approval != permission.ModeAsk || m.state.Approval != permission.ModeAsk {
		t.Fatalf("approval mode changed controller/model = %s/%s", controller.state.Approval, m.state.Approval)
	}
}

func TestApprovalModalNavigationDetailsSelectedApproval(t *testing.T) {
	controller := &fakeController{state: workbench.State{
		Approval: permission.ModeAsk,
		Commands: workbench.DefaultCommands(),
		Approvals: []workbench.ApprovalItem{
			{ID: "p1", Title: "first", Action: "write", Subject: "a.go", Body: "first patch"},
			{ID: "p2", Title: "second", Action: "write", Subject: "b.go", Body: "second patch"},
		},
	}}
	m := newModel(context.Background(), controller, controller.state)

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	if cmd != nil {
		t.Fatalf("down returned unexpected cmd")
	}
	next, cmd = next.(model).Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	if cmd == nil {
		t.Fatalf("detail returned nil cmd")
	}
	msg := firstActionMsg(t, cmd)
	next, _ = next.(model).Update(msg)
	if controller.detailed != "p2" {
		t.Fatalf("detailed = %q, want p2", controller.detailed)
	}
}

func TestPaneJumpShortcutsFocusPanesAndApprovalInspector(t *testing.T) {
	controller := &fakeController{state: workbench.State{
		Approval: permission.ModeAsk,
		Commands: workbench.DefaultCommands(),
		Approvals: []workbench.ApprovalItem{{
			ID:      "p1",
			Title:   "edit README",
			Action:  "write",
			Subject: "README.md",
			Body:    "diff",
		}},
	}}
	m := newModel(context.Background(), controller, controller.state)
	m.mode = modeNormal
	m.overlay = overlayNone

	m = pressKeys(t, m, "g", "a")
	if m.focus != focusAgents || m.mode != modeNormal || m.overlay != overlayNone {
		t.Fatalf("ga focus/mode/overlay = %d/%s/%d, want agents normal none", m.focus, m.mode, m.overlay)
	}
	m = pressKeys(t, m, "g", "t")
	if m.focus != focusTranscript {
		t.Fatalf("gt focus = %d, want transcript", m.focus)
	}
	m = pressKeys(t, m, "g", "m")
	if m.focus != focusTranscript {
		t.Fatalf("gm focus = %d, want transcript", m.focus)
	}
	m = pressKeys(t, m, "g", "f")
	if m.focus != focusContext || m.overlay != overlayNone {
		t.Fatalf("gf focus/overlay = %d/%d, want context none", m.focus, m.overlay)
	}
	m = pressKeys(t, m, "g", "d")
	if m.focus != focusContext || m.mode != modeApprove || m.overlay != overlayApproval {
		t.Fatalf("gd focus/mode/overlay = %d/%s/%d, want approval inspector", m.focus, m.mode, m.overlay)
	}
}

func TestApprovalGChordPreservesSingleGTopScroll(t *testing.T) {
	body := strings.Repeat("line\n", 40)
	controller := &fakeController{state: workbench.State{
		Approval: permission.ModeAsk,
		Commands: workbench.DefaultCommands(),
		Approvals: []workbench.ApprovalItem{{
			ID:      "p1",
			Title:   "edit README",
			Action:  "write",
			Subject: "README.md",
			Body:    body,
		}},
	}}
	m := newModel(context.Background(), controller, controller.state)
	m.approval.SetYOffset(12)

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'g'}})
	if cmd != nil {
		t.Fatalf("single g returned unexpected cmd")
	}
	m = next.(model)
	if m.approval.YOffset != 0 || m.pendingKey != "g" {
		t.Fatalf("single g offset/pending = %d/%q, want top with pending chord", m.approval.YOffset, m.pendingKey)
	}
	m = pressKeys(t, m, "a")
	if m.focus != focusAgents || m.mode != modeNormal || m.overlay != overlayNone {
		t.Fatalf("ga in approval focus/mode/overlay = %d/%s/%d, want agents normal none", m.focus, m.mode, m.overlay)
	}
}

func TestCommandModeDirectIDGrammarRunsActions(t *testing.T) {
	controller := &fakeController{state: workbench.State{
		Approval: permission.ModeAsk,
		Commands: workbench.DefaultCommands(),
		Approvals: []workbench.ApprovalItem{{
			ID:      "p1",
			Title:   "edit README",
			Action:  "write",
			Subject: "README.md",
			Body:    "diff",
		}},
	}}
	m := newModel(context.Background(), controller, controller.state)

	cases := []struct {
		line string
		want func(t *testing.T)
	}{
		{line: ":y c2", want: func(t *testing.T) {
			if controller.copied != "c2" || controller.copiedWithFences {
				t.Fatalf("copy = %q/%v, want c2 false", controller.copied, controller.copiedWithFences)
			}
		}},
		{line: ":Y c2", want: func(t *testing.T) {
			if controller.copied != "c2" || !controller.copiedWithFences {
				t.Fatalf("copy = %q/%v, want c2 true", controller.copied, controller.copiedWithFences)
			}
		}},
		{line: ":d p1", want: func(t *testing.T) {
			if controller.detailed != "p1" {
				t.Fatalf("detailed = %q, want p1", controller.detailed)
			}
		}},
		{line: ":a p1", want: func(t *testing.T) {
			if controller.approved != "p1" {
				t.Fatalf("approved = %q, want p1", controller.approved)
			}
		}},
		{line: ":r p1", want: func(t *testing.T) {
			if controller.rejected != "p1" {
				t.Fatalf("rejected = %q, want p1", controller.rejected)
			}
		}},
	}

	for _, tc := range cases {
		next, cmd := m.executeLine(tc.line)
		if cmd == nil {
			t.Fatalf("%s returned nil cmd", tc.line)
		}
		msg := firstActionMsg(t, cmd)
		next, _ = next.(model).Update(msg)
		m = next.(model)
		tc.want(t)
		controller.state.Approvals = []workbench.ApprovalItem{{
			ID:      "p1",
			Title:   "edit README",
			Action:  "write",
			Subject: "README.md",
			Body:    "diff",
		}}
		m.state.Approvals = controller.state.Approvals
	}
}

func TestCommandModeOpenFileUsesExecHandoff(t *testing.T) {
	controller := &fakeController{state: workbench.State{
		Commands: workbench.DefaultCommands(),
		Artifacts: []workbench.Item{{
			ID:    "f4",
			Kind:  "file",
			Title: "README.md",
			URI:   "file:README.md",
			Meta:  map[string]string{"path": "README.md"},
		}},
	}}
	m := newModel(context.Background(), controller, controller.state)

	next, cmd := m.executeLine(":o f4:12")
	if cmd == nil {
		t.Fatalf("open file returned nil cmd, want exec handoff")
	}
	m = next.(model)
	if controller.opened != "" {
		t.Fatalf("opened = %q, want no controller open", controller.opened)
	}
	if m.state.Notice != "opening f4:12 in external Neovim" {
		t.Fatalf("notice = %q, want editor opening notice", m.state.Notice)
	}
}

func TestApprovalInspectorIncludesChangedFilesAndScrollsBody(t *testing.T) {
	body := "--- a/README.md\n+++ b/README.md\n"
	for i := 0; i < 40; i++ {
		body += "+line\n"
	}
	controller := &fakeController{state: workbench.State{
		Approval: permission.ModeAsk,
		Commands: workbench.DefaultCommands(),
		Approvals: []workbench.ApprovalItem{{
			ID:      "p1",
			Title:   "edit README",
			Action:  "write",
			Subject: "README.md, internal/app/app.go",
			Reason:  "apply_patch:digest",
			Body:    body,
		}},
	}}
	m := newModel(context.Background(), controller, controller.state)
	m.width = 100
	m.height = 24
	m.resize()

	view := m.approvalView()
	if !containsAll(view, "changed files: README.md, internal/app/app.go", "Changed files", "README.md", "internal/app/app.go") {
		t.Fatalf("approval view missing changed-file summary:\n%s", view)
	}

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlF})
	if cmd != nil {
		t.Fatalf("ctrl+f returned unexpected cmd")
	}
	m = next.(model)
	if m.approval.YOffset == 0 {
		t.Fatalf("approval YOffset = 0, want scrolled body")
	}
}

func TestPaletteRowsShowDescriptionAndDisabledState(t *testing.T) {
	commands := []workbench.Command{{
		ID:             "disabled.example",
		Title:          "Disabled command",
		Category:       "Danger",
		Description:    "Explains the effect and risk before running.",
		Keybinding:     ":disabled",
		Enabled:        false,
		DisabledReason: "not available now",
	}}
	controller := &fakeController{state: workbench.State{Commands: commands}}
	m := newModel(context.Background(), controller, controller.state).openPalette()

	view := m.paletteView()
	if !containsAll(view, "Disabled command", "disabled", "not available now", "Explains the effect and risk") {
		t.Fatalf("palette view missing command details:\n%s", view)
	}

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil {
		t.Fatalf("disabled command returned cmd")
	}
	m = next.(model)
	if m.state.Notice != "Disabled command disabled: not available now" {
		t.Fatalf("notice = %q, want disabled reason", m.state.Notice)
	}
}

func TestAIVimPolishCopyRendersAgenticEmptyStates(t *testing.T) {
	controller := &fakeController{state: workbench.State{
		Approval: permission.ModeAsk,
		Commands: workbench.DefaultCommands(),
	}}
	m := newModel(context.Background(), controller, controller.state)
	m.width = 100
	m.height = 30
	m.resize()

	if view := stripANSI(m.leftView(42, 8)); !containsAll(view, "Agent Buffers", "No workers yet", ":s <task>") {
		t.Fatalf("agent buffer empty state missing polish copy:\n%s", view)
	}
	if view := stripANSI(m.centerView(58, 8)); !containsAll(view, "Main Orchestrator Buffer", "Agent buffer is empty", ":s stages work") {
		t.Fatalf("center empty state missing agent-buffer copy:\n%s", view)
	}
	m.setRightTab(workbench.RightTabOps)
	if view := stripANSI(m.opsView(58, 8, []string{titleStyle.Render(m.rightTabHeader())})); !containsAll(view, "Ops control plane", "run queue", "next-turn context") {
		t.Fatalf("ops view missing control-plane labels:\n%s", view)
	}
	if view := stripANSI(m.noticeView()); !containsAll(view, "agents are buffers", "Ops is control plane", "Vim/edit is direct intervention") {
		t.Fatalf("notice missing mental-model hint:\n%s", view)
	}
}

func TestRightPaneWrapsInformationalRowsInsteadOfEllipsizing(t *testing.T) {
	longName := "internal/super/deep/path/with/an_extremely_long_filename_that_should_wrap_instead_of_ellipsis.go"
	controller := &fakeController{state: workbench.State{
		Commands: workbench.DefaultCommands(),
		RightTab: workbench.RightTabFiles,
		Files: []workbench.WorkspaceFile{{
			ID:   "f1",
			Path: longName,
			Name: "an_extremely_long_filename_that_should_wrap_instead_of_ellipsis.go",
			Kind: "file",
		}},
	}}
	m := newModel(context.Background(), controller, controller.state)
	m.focus = focusContext
	m.expandedFolders["internal"] = true
	m.expandedFolders["internal/super"] = true
	m.expandedFolders["internal/super/deep"] = true
	m.expandedFolders["internal/super/deep/path"] = true
	m.expandedFolders["internal/super/deep/path/with"] = true

	view := stripANSI(m.rightView(34, 18))
	if strings.Contains(view, "...") {
		t.Fatalf("right pane contains horizontal ellipsis, want wrapped rows:\n%s", view)
	}
	for _, line := range strings.Split(view, "\n") {
		if printableWidth(line) > 34 {
			t.Fatalf("right pane line width = %d > 34: %q\nfull:\n%s", printableWidth(line), line, view)
		}
	}
}

func TestShowBuffersUsesAgentBufferMentalModel(t *testing.T) {
	controller := &fakeController{state: workbench.State{
		Commands: workbench.DefaultCommands(),
		Agents:   []workbench.AgentItem{{ID: "a1", Name: "worker", Role: "worker", Status: "running"}},
	}}
	m := newModel(context.Background(), controller, controller.state)

	next, cmd := m.showBuffers()
	if cmd != nil {
		t.Fatalf("showBuffers returned unexpected cmd")
	}
	m = next.(model)
	view := stripANSI(m.detail.View())
	if !containsAll(view, "Agent buffers:", "Main plans; agent", "a1") {
		t.Fatalf("buffer detail missing agent-buffer copy:\n%s", view)
	}
}

func TestAgentTaskBoardRendersCompactMetadata(t *testing.T) {
	controller := &fakeController{state: workbench.State{
		Commands: workbench.DefaultCommands(),
		Agents: []workbench.AgentItem{{
			ID:            "a7",
			Name:          "worker",
			Role:          "worker",
			Status:        "completed",
			TaskID:        "task-7",
			Summary:       "implemented pane jumps",
			ChangedFiles:  []string{"README.md", "internal/adapters/tui2/workbench.go"},
			TestsRun:      []string{"go test ./internal/adapters/tui2"},
			Findings:      []string{"none"},
			Questions:     []string{"ship?"},
			BlockedReason: "",
		}},
	}}
	m := newModel(context.Background(), controller, controller.state)
	m.width = 92

	view := m.leftView(32, 12)
	if !containsAll(view, "a7 worker completed", "task task-7", "files README.md", "tests go test") {
		t.Fatalf("agent board missing metadata:\n%s", view)
	}
}

func TestAgentTaskBoardRendersParentChildTree(t *testing.T) {
	controller := &fakeController{state: workbench.State{
		Commands: workbench.DefaultCommands(),
		Agents: []workbench.AgentItem{
			{ID: "a1", Name: "planner", Role: "orchestrator", Status: "running", TaskID: "task-parent"},
			{ID: "a2", Name: "worker", Role: "worker", Status: "completed", TaskID: "task-child", Meta: map[string]string{"parent_agent_id": "task-parent"}},
		},
	}}
	m := newModel(context.Background(), controller, controller.state)

	view := stripANSI(m.leftView(40, 12))
	if !strings.Contains(view, "a1 planner orchestrator running") {
		t.Fatalf("agent tree missing parent:\n%s", view)
	}
	if !strings.Contains(view, "  a2 worker completed") {
		t.Fatalf("agent tree missing indented child:\n%s", view)
	}
	m.focus = focusAgents
	m.leftCursor = 2
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatalf("enter on child returned nil cmd")
	}
	msg := firstActionMsg(t, cmd)
	next, _ = next.(model).Update(msg)
	m = next.(model)
	if m.state.ActiveConversation.ID != "a2" {
		t.Fatalf("active conversation = %#v, want child agent a2", m.state.ActiveConversation)
	}
}

func TestAgentTaskBoardNarrowDoesNotOverflow(t *testing.T) {
	controller := &fakeController{state: workbench.State{
		Commands: workbench.DefaultCommands(),
		Agents: []workbench.AgentItem{{
			ID:            "a7",
			Name:          "very-long-worker-name",
			Role:          "worker",
			Status:        "blocked",
			TaskID:        "task-with-a-very-long-identifier",
			ChangedFiles:  []string{"internal/adapters/tui2/workbench.go", "internal/app/workbench/workbench.go", "README.md"},
			TestsRun:      []string{"go test ./internal/adapters/tui2 ./internal/app/workbench"},
			BlockedReason: "waiting for a very long user decision about a complicated approval",
		}},
	}}
	m := newModel(context.Background(), controller, controller.state)
	view := m.leftView(24, 10)
	for _, line := range strings.Split(view, "\n") {
		if width := printableWidth(line); width > 22 {
			t.Fatalf("line width = %d, want <= 22:\n%s\nfull view:\n%s", width, line, view)
		}
	}
}

func TestFilesTabPreservesNestedTreeIndentation(t *testing.T) {
	controller := &fakeController{state: workbench.State{
		Commands: workbench.DefaultCommands(),
		RightTab: workbench.RightTabFiles,
		Files: []workbench.WorkspaceFile{
			{Path: "cmd/freecode/main.go", Name: "main.go", Kind: "file"},
			{Path: "internal/app/workbench/workbench.go", Name: "workbench.go", Kind: "file"},
			{Path: "README.md", Name: "README.md", Kind: "file"},
		},
	}}
	m := newModel(context.Background(), controller, controller.state)
	m.focus = focusContext
	m.expandedFolders["cmd"] = true
	m.expandedFolders["cmd/freecode"] = true
	m.expandedFolders["internal"] = true
	m.expandedFolders["internal/app"] = true
	m.expandedFolders["internal/app/workbench"] = true

	view := stripANSI(m.rightView(48, 16))
	if !strings.Contains(view, "[-] cmd/") || !strings.Contains(view, "  [-] freecode/") || !strings.Contains(view, "    f1  file") {
		t.Fatalf("file tree missing cmd child indentation:\n%s", view)
	}
	if !strings.Contains(view, "  [-] app/") || !strings.Contains(view, "    [-] workbench/") || !strings.Contains(view, "      f2  file") {
		t.Fatalf("file tree missing nested folder indentation:\n%s", view)
	}
}

func TestPaneWidthsCollapseForNarrowTerminals(t *testing.T) {
	controller := &fakeController{state: workbench.State{Commands: workbench.DefaultCommands()}}
	m := newModel(context.Background(), controller, controller.state)
	m.width = 60

	left, center, right := m.paneWidths()
	if left != 0 || center <= 0 || right != 0 {
		t.Fatalf("pane widths = %d/%d/%d, want transcript-only narrow layout", left, center, right)
	}
	if center+4 > m.width {
		t.Fatalf("center content width %d overflows terminal width %d", center, m.width)
	}

	m.width = 90
	m.focus = focusContext
	left, center, right = m.paneWidths()
	if left != 0 || center <= 0 || right <= 0 {
		t.Fatalf("pane widths = %d/%d/%d, want two-pane focused layout", left, center, right)
	}
	if center+right+8 > m.width {
		t.Fatalf("two-pane content width %d overflows terminal width %d", center+right+8, m.width)
	}
}

func TestTranscriptSelectionScrollsIntoView(t *testing.T) {
	var transcript []workbench.TranscriptItem
	for i := 1; i <= 12; i++ {
		transcript = append(transcript, workbench.TranscriptItem{
			ID:    fmt.Sprintf("m%d", i),
			Kind:  workbench.TranscriptAssistant,
			Actor: "assistant",
			Text:  "line one\nline two",
		})
	}
	controller := &fakeController{state: workbench.State{Commands: workbench.DefaultCommands(), Transcript: transcript}}
	m := newModel(context.Background(), controller, controller.state)
	m.chat.SetSize(40, 5)
	m.chat.SetItems(transcript)

	for i := 0; i < 10; i++ {
		m.moveSelection(1)
	}
	if m.chat.yOffset == 0 {
		t.Fatalf("chat yOffset = 0, want selected message scrolled into view")
	}
}

func TestTranscriptJKAlwaysMovesSelectedMessage(t *testing.T) {
	controller := &fakeController{state: workbench.State{
		Commands: workbench.DefaultCommands(),
		Transcript: []workbench.TranscriptItem{
			{
				ID:    "m1",
				Kind:  workbench.TranscriptAssistant,
				Actor: "assistant",
				Text:  strings.Repeat("long selected message that spans several rows. ", 20),
			},
			{ID: "m2", Kind: workbench.TranscriptAssistant, Actor: "assistant", Text: "next"},
		},
	}}
	m := newModel(context.Background(), controller, controller.state)
	m.focus = focusTranscript
	m.chat.SetSize(48, 5)
	m.chat.SetItems(controller.state.Transcript)
	m.chat.Top()

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	if cmd != nil {
		t.Fatalf("j returned unexpected cmd")
	}
	m = next.(model)
	if id, _ := m.chat.SelectedID(); id != "m2" {
		t.Fatalf("selected = %q, want m2 after j", id)
	}
}

func TestTranscriptCtrlEYScrollsContentWithoutTailSnap(t *testing.T) {
	var transcript []workbench.TranscriptItem
	for i := 1; i <= 8; i++ {
		transcript = append(transcript, workbench.TranscriptItem{
			ID:    fmt.Sprintf("m%d", i),
			Kind:  workbench.TranscriptAssistant,
			Actor: "assistant",
			Text:  "line one\nline two",
		})
	}
	controller := &fakeController{state: workbench.State{Commands: workbench.DefaultCommands(), Transcript: transcript}}
	m := newModel(context.Background(), controller, controller.state)
	m.focus = focusTranscript
	m.chat.SetSize(40, 5)
	m.chat.SetItems(transcript)

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlY})
	m = next.(model)
	if m.chat.IsAtBottom() {
		t.Fatalf("Ctrl+Y did not scroll away from bottom")
	}
	scrolledOffset := m.chat.yOffset
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlE})
	m = next.(model)
	if m.chat.yOffset <= scrolledOffset {
		t.Fatalf("Ctrl+E yOffset = %d, want > %d", m.chat.yOffset, scrolledOffset)
	}
}

func TestTranscriptWrapsAndSanitizesTerminalJunk(t *testing.T) {
	controller := &fakeController{state: workbench.State{
		Commands: workbench.DefaultCommands(),
		Transcript: []workbench.TranscriptItem{{
			ID:    "m1",
			Kind:  workbench.TranscriptUser,
			Actor: "user",
			Text:  "\x1b]11;rgb:0000/0000/0000\x1b\\hello this is a long line that should wrap inside the transcript pane instead of running out horizontally",
		}},
	}}
	m := newModel(context.Background(), controller, controller.state)
	m.chat.SetSize(24, 20)
	m.chat.SetItems(controller.state.Transcript)

	content := stripANSI(m.chat.View())
	if strings.Contains(content, "]11;rgb") || strings.Contains(content, "\x1b") {
		t.Fatalf("content contains terminal junk:\n%q", content)
	}
	for _, line := range strings.Split(content, "\n") {
		if len(line) > 24 {
			t.Fatalf("line %q length = %d, want wrapped <= 24\nfull:\n%s", line, len(line), content)
		}
	}
}

func TestTranscriptRendersCompactMessages(t *testing.T) {
	controller := &fakeController{state: workbench.State{
		Commands: workbench.DefaultCommands(),
		Transcript: []workbench.TranscriptItem{
			{ID: "m1", Kind: workbench.TranscriptUser, Actor: "user", Text: "hello"},
			{ID: "m2", Kind: workbench.TranscriptAssistant, Actor: "assistant", Text: "hi"},
		},
	}}
	m := newModel(context.Background(), controller, controller.state)
	m.chat.SetSize(60, 20)
	m.chat.SetItems(controller.state.Transcript)

	content := stripANSI(m.chat.View())
	if !strings.Contains(content, "m1 you\n  hello\n\n▌ m2 assistant\n▌ hi") {
		t.Fatalf("content = %q, want one blank line between message cells", content)
	}
	if strings.Contains(content, "\n\n\n") {
		t.Fatalf("content has excessive spacer rows:\n%s", content)
	}
}

func TestMouseToggleFullscreensFocusedPane(t *testing.T) {
	controller := &fakeController{state: workbench.State{
		Commands: workbench.DefaultCommands(),
		Transcript: []workbench.TranscriptItem{
			{ID: "m1", Kind: workbench.TranscriptUser, Actor: "user", Text: "hello chat content"},
		},
		Agents: []workbench.AgentItem{
			{ID: "a1", Name: "worker", Role: "worker", Status: "running"},
		},
	}}
	m := newModel(context.Background(), controller, controller.state)
	m.width = 140
	m.height = 30
	m.resize()
	m.focus = focusAgents

	// Multi-pane view: agents pane is on screen so its content should appear.
	multi := stripANSI(m.bodyView())
	if !strings.Contains(multi, "worker") {
		t.Fatalf("multi-pane view missing agents content:\n%s", multi)
	}
	if !strings.Contains(multi, "hello chat content") {
		t.Fatalf("multi-pane view missing chat content:\n%s", multi)
	}

	// Re-focus the transcript pane for the fullscreen check.
	m.focus = focusTranscript

	// Toggle mouse off → should fullscreen the focused (transcript) pane.
	next, _ := m.toggleMouseCapture()
	m = next.(model)
	if m.mouseCaptured {
		t.Fatalf("mouseCaptured = true after toggle, want false")
	}
	body := stripANSI(m.bodyView())
	if !strings.Contains(body, "hello chat content") {
		t.Fatalf("fullscreen view missing focused pane content:\n%s", body)
	}
	if strings.Contains(body, "a1 worker") {
		t.Fatalf("fullscreen view should hide non-focused panes, but agents content is visible:\n%s", body)
	}

	// Switching focus while in copy mode should fullscreen the new focus.
	m.focus = focusAgents
	body = stripANSI(m.bodyView())
	if !strings.Contains(body, "worker") {
		t.Fatalf("focusAgents fullscreen missing agents content:\n%s", body)
	}
	if strings.Contains(body, "hello chat content") {
		t.Fatalf("focusAgents fullscreen leaked chat content:\n%s", body)
	}

	// Toggle back: multi-pane returns.
	m.focus = focusTranscript
	next, _ = m.toggleMouseCapture()
	m = next.(model)
	if !m.mouseCaptured {
		t.Fatalf("mouseCaptured = false after second toggle, want true")
	}
	multi = stripANSI(m.bodyView())
	if !strings.Contains(multi, "worker") || !strings.Contains(multi, "hello chat content") {
		t.Fatalf("multi-pane view did not return:\n%s", multi)
	}
}

func TestComposerSanitizesTerminalJunkWhileTyping(t *testing.T) {
	controller := &fakeController{state: workbench.State{Commands: workbench.DefaultCommands()}}
	m := newModel(context.Background(), controller, controller.state)
	m.mode = modeInsert
	m.composer.Focus()

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("\x1b]11;rgb:0ehi")})
	m = next.(model)
	if strings.Contains(m.composer.Value(), "]11;rgb") || strings.Contains(m.composer.Value(), "\x1b") {
		t.Fatalf("composer contains terminal junk: %q", m.composer.Value())
	}
	m.composer.SetValue("")

	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("11;rgb:0ehi")})
	m = next.(model)
	if got := m.composer.Value(); got != "hi" {
		t.Fatalf("composer value = %q, want terminal fragment stripped to hi", got)
	}
}

func TestMouseWheelScrollsTranscript(t *testing.T) {
	var transcript []workbench.TranscriptItem
	for i := 1; i <= 12; i++ {
		transcript = append(transcript, workbench.TranscriptItem{ID: fmt.Sprintf("m%d", i), Actor: "assistant", Text: "line"})
	}
	controller := &fakeController{state: workbench.State{Commands: workbench.DefaultCommands(), Transcript: transcript}}
	m := newModel(context.Background(), controller, controller.state)
	m.focus = focusTranscript
	m.chat.SetSize(40, 4)
	m.chat.SetItems(transcript)
	m.chat.Top()

	next, cmd := m.Update(tea.MouseMsg{Type: tea.MouseWheelDown})
	if cmd != nil {
		t.Fatalf("mouse wheel returned unexpected cmd")
	}
	m = next.(model)
	if m.chat.yOffset == 0 {
		t.Fatalf("chat yOffset = 0, want mouse wheel scroll")
	}
}

func TestSettingsOpensScrollableDetailOverlay(t *testing.T) {
	controller := &fakeController{state: workbench.State{
		Commands:      workbench.DefaultCommands(),
		Provider:      "lilac",
		Model:         "zai-org/glm-5.1",
		WorkspaceRoot: strings.Repeat("very-long-workspace/", 8),
	}}
	m := newModel(context.Background(), controller, controller.state)
	m.width = 80
	m.height = 18
	m.resize()

	next, cmd := m.executeLine(":settings")
	if cmd != nil {
		t.Fatalf(":settings returned unexpected cmd")
	}
	m = next.(model)
	if m.overlay != overlayDetail {
		t.Fatalf("overlay = %d, want detail", m.overlay)
	}
	view := m.View()
	if !strings.Contains(stripANSI(view), "provider: lilac") {
		t.Fatalf("settings overlay missing provider:\n%s", view)
	}
	if got := lineCount(view); got > m.height {
		t.Fatalf("view line count = %d, want <= %d\n%s", got, m.height, view)
	}
}

func TestContextCommandShowsNextTurnContextPreview(t *testing.T) {
	controller := &fakeController{state: workbench.State{
		Commands: workbench.DefaultCommands(),
		ContextPreview: workbench.ContextPreview{
			Summary:            "Main orchestrator, 2 transcript cells, ~20 tokens",
			Included:           []string{"new prompt", "visible main conversation history"},
			Excluded:           []string{"unshared terminal state"},
			TokenEstimate:      20,
			ActiveConversation: "Main orchestrator",
		},
	}}
	m := newModel(context.Background(), controller, controller.state)

	next, cmd := m.executeLine(":context")
	if cmd != nil {
		t.Fatalf(":context returned unexpected cmd")
	}
	m = next.(model)
	if m.overlay != overlayDetail || !containsAll(m.state.Detail.Body, "included next turn", "visible main conversation history", "unshared terminal state") {
		t.Fatalf("context detail = overlay %d body:\n%s", m.overlay, m.state.Detail.Body)
	}
}

func TestCopyModeShowsSelectedText(t *testing.T) {
	controller := &fakeController{state: workbench.State{
		Commands: workbench.DefaultCommands(),
		Transcript: []workbench.TranscriptItem{{
			ID:    "m1",
			Actor: "assistant",
			Text:  "copy this response",
		}},
	}}
	m := newModel(context.Background(), controller, controller.state)

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'v'}})
	if cmd == nil {
		t.Fatalf("copy mode returned nil cmd, want mouse disable")
	}
	m = next.(model)
	if m.overlay != overlayCopy {
		t.Fatalf("overlay = %d, want copy", m.overlay)
	}
	if !strings.Contains(stripANSI(m.View()), "copy this response") {
		t.Fatalf("copy overlay missing selected text:\n%s", m.View())
	}

	next, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if cmd == nil {
		t.Fatalf("copy mode esc returned nil cmd, want mouse enable")
	}
	m = next.(model)
	if m.overlay != overlayNone {
		t.Fatalf("overlay after esc = %d, want none", m.overlay)
	}
}

func TestPaletteDangerRequiresExplicitCommandConfirmation(t *testing.T) {
	controller := &fakeController{state: workbench.State{Approval: permission.ModeAsk, Commands: workbench.DefaultCommands()}}
	m := newModel(context.Background(), controller, controller.state).openPalette()
	m = sendRunes(t, m, "danger")

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil {
		t.Fatalf("danger palette command returned cmd, want notice only")
	}
	m = next.(model)
	if controller.state.Approval == permission.ModeDanger || m.state.Approval == permission.ModeDanger {
		t.Fatalf("approval mode changed to danger without confirmation")
	}
	if m.state.Notice != "type :danger confirm to enable danger mode" {
		t.Fatalf("notice = %q, want danger confirmation prompt", m.state.Notice)
	}
}

func TestWorkspacePaneNavigationAndRightTabSwitching(t *testing.T) {
	controller := &fakeController{state: workbench.State{
		Commands: workbench.DefaultCommands(),
		Files:    []workbench.WorkspaceFile{{ID: "f1", Path: "README.md", Status: "tracked"}},
		GitFiles: []workbench.WorkspaceFile{{ID: "g1", Path: "main.go", Status: "modified"}},
	}}
	m := newModel(context.Background(), controller, controller.state)

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlH})
	m = next.(model)
	if m.focus != focusAgents {
		t.Fatalf("ctrl+h focus = %d, want agents", m.focus)
	}
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlL})
	m = next.(model)
	if m.focus != focusTranscript {
		t.Fatalf("ctrl+l focus = %d, want transcript", m.focus)
	}
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlL})
	m = next.(model)
	if m.focus != focusContext {
		t.Fatalf("second ctrl+l focus = %d, want context", m.focus)
	}
	m = pressKeys(t, m, "]")
	if m.state.RightTab != workbench.RightTabArtifacts {
		t.Fatalf("right tab = %q, want artifacts", m.state.RightTab)
	}
	m = pressKeys(t, m, "[")
	if m.state.RightTab != workbench.RightTabFiles {
		t.Fatalf("right tab = %q, want files", m.state.RightTab)
	}
	m = pressKeys(t, m, "1")
	if m.state.RightTab != workbench.RightTabFiles || m.focus != focusContext {
		t.Fatalf("1 tab/focus = %q/%d, want files context", m.state.RightTab, m.focus)
	}
	m = pressKeys(t, m, "3")
	if m.state.RightTab != workbench.RightTabGit || m.focus != focusContext {
		t.Fatalf("3 tab/focus = %q/%d, want git context", m.state.RightTab, m.focus)
	}
	m = pressKeys(t, m, "5")
	if m.state.RightTab != workbench.RightTabOps || m.focus != focusContext {
		t.Fatalf("5 tab/focus = %q/%d, want ops context", m.state.RightTab, m.focus)
	}
	m.focus = focusTranscript
	m = pressKeys(t, m, "1")
	if m.state.RightTab != workbench.RightTabOps || m.focus != focusTranscript {
		t.Fatalf("1 outside context tab/focus = %q/%d, want unchanged ops transcript", m.state.RightTab, m.focus)
	}
}

func TestOpsTabRendersOperationalStateAndContextInspector(t *testing.T) {
	controller := &fakeController{state: workbench.State{
		Commands: workbench.DefaultCommands(),
		Approvals: []workbench.ApprovalItem{{
			ID:      "p1",
			Title:   "edit README",
			Action:  "write",
			Subject: "README.md",
			Body:    "diff",
		}},
		Agents: []workbench.AgentItem{{ID: "a1", Name: "worker", Status: "running", Summary: "editing README"}},
		ContextPreview: workbench.ContextPreview{
			Summary:            "Main orchestrator, 1 transcript cells, ~12 tokens",
			Included:           []string{"new prompt", "visible main conversation history"},
			Excluded:           []string{"local-only shell output"},
			TokenEstimate:      12,
			ActiveConversation: "Main orchestrator",
		},
	}}
	m := newModel(context.Background(), controller, controller.state)
	m.focus = focusContext
	m.state.RightTab = workbench.RightTabOps
	m.mode = modeNormal
	m.overlay = overlayNone
	m.busy = true
	m.activeRun = 4
	m.submitQueue = []queuedSubmit{{text: "follow up"}}

	view := stripANSI(m.rightView(58, 16))
	if !containsAll(view, "Ops", "run queue", "active run #4", "next-turn context", "approval edit README", "worker running") {
		t.Fatalf("ops view missing expected state:\n%s", view)
	}
	m.contextCursor = 2 // ctx entry after run + queued prompt
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil {
		t.Fatalf("context inspector returned unexpected cmd")
	}
	m = next.(model)
	if m.overlay != overlayDetail || !strings.Contains(m.state.Detail.Body, "included next turn") {
		t.Fatalf("context detail = overlay %d item %#v", m.overlay, m.state.Detail)
	}
}

func TestPaneFocusMoveDoesNotForceFullRepaint(t *testing.T) {
	controller := &fakeController{state: workbench.State{Commands: workbench.DefaultCommands()}}
	m := newModel(context.Background(), controller, controller.state)
	m.focus = focusTranscript

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlL})
	if cmd != nil {
		t.Fatalf("focus-only pane move returned repaint cmd")
	}
	m = next.(model)
	if m.focus != focusContext {
		t.Fatalf("focus = %d, want context", m.focus)
	}
	m.focus = focusTranscript
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRight})
	m = next.(model)
	if m.focus != focusTranscript {
		t.Fatalf("right arrow focus = %d, want local action without pane move", m.focus)
	}
}

func TestModeSwitchesDoNotReturnRepaintCommands(t *testing.T) {
	controller := &fakeController{state: workbench.State{Commands: workbench.DefaultCommands()}}
	m := newModel(context.Background(), controller, controller.state)
	m.focus = focusTranscript

	for _, msg := range []tea.KeyMsg{
		{Type: tea.KeyCtrlK},
		{Type: tea.KeyEsc},
		{Type: tea.KeyRunes, Runes: []rune(":")},
		{Type: tea.KeyEsc},
		{Type: tea.KeyRunes, Runes: []rune("i")},
	} {
		next, cmd := m.Update(msg)
		if cmd != nil {
			t.Fatalf("mode switch %q returned repaint cmd", msg.String())
		}
		m = next.(model)
	}
}

func TestBangRunsShellCommand(t *testing.T) {
	controller := &fakeController{state: workbench.State{Commands: workbench.DefaultCommands()}}
	m := newModel(context.Background(), controller, controller.state)

	next, cmd := m.executeLine("!go test ./...")
	if cmd == nil {
		t.Fatalf("! command returned nil cmd")
	}
	msg := firstActionMsg(t, cmd)
	next, _ = next.(model).Update(msg)
	m = next.(model)
	if controller.shellCommand != "go test ./..." {
		t.Fatalf("shell command = %q, want go test ./...", controller.shellCommand)
	}
	if m.overlay != overlayNone || m.state.Detail.Kind != "shell" {
		t.Fatalf("overlay/detail = %d/%#v, want quiet shell detail state", m.overlay, m.state.Detail)
	}
}

func TestShareTermCommandEnablesDirectTerminalTools(t *testing.T) {
	controller := &fakeController{state: workbench.State{Commands: workbench.DefaultCommands()}}
	m := newModel(context.Background(), controller, controller.state)
	m.terms = []*terminalSession{
		{active: true, lines: []string{"one"}},
		{active: true, lines: []string{"two"}},
	}
	m.term = m.terms[0]
	m.termSlot = 0

	next, cmd := m.executeLine(":st 2")
	m = next.(model)
	if cmd != nil {
		t.Fatalf(":st returned unexpected cmd for already-active terminal without output channel")
	}
	if !m.terminalShared || m.sharedTermSlot != 1 || m.termSlot != 1 {
		t.Fatalf("shared/slot/current = %v/%d/%d, want terminal 2 shared and selected", m.terminalShared, m.sharedTermSlot, m.termSlot)
	}
	if !strings.Contains(m.state.Notice, "terminal 2 shared") {
		t.Fatalf("notice = %q, want terminal 2 shared", m.state.Notice)
	}
}

func TestShareOutputAttachesTerminalWithoutLiveTools(t *testing.T) {
	controller := &fakeController{state: workbench.State{Commands: workbench.DefaultCommands()}}
	m := newModel(context.Background(), controller, controller.state)
	m.terms = []*terminalSession{
		{active: true, lines: []string{"one"}},
		{active: true, lines: []string{"two"}},
	}
	m.term = m.terms[0]
	m.termSlot = 0

	next, cmd := m.executeLine(":sto 2")
	if cmd == nil {
		t.Fatalf(":sto returned nil cmd, want terminal attachment action")
	}
	msg := firstActionMsg(t, cmd)
	next, _ = next.(model).Update(msg)
	m = next.(model)
	if m.terminalShared {
		t.Fatalf(":sto enabled live terminal sharing")
	}
	if controller.sharedTerminalTitle == "" || !strings.Contains(controller.sharedTerminalBody, "two") {
		t.Fatalf("shared terminal title/body = %q/%q, want terminal 2 output attached", controller.sharedTerminalTitle, controller.sharedTerminalBody)
	}
}

func TestSubmitIncludesSharedTerminalToolsAndContext(t *testing.T) {
	controller := &fakeController{state: workbench.State{Commands: workbench.DefaultCommands()}}
	m := newModel(context.Background(), controller, controller.state)
	m.state.RightTab = workbench.RightTabTerm
	m.terms = []*terminalSession{{active: true, lines: []string{"npm test", "PASS"}}}
	m.term = m.terms[0]
	m.termSlot = 0
	m.terminalShared = true
	m.sharedTermSlot = 0
	m.composer.SetValue("can you see the terminal?")

	next, cmd := m.submit(false)
	if cmd == nil {
		t.Fatalf("submit returned nil cmd")
	}
	msg := firstActionMsg(t, cmd)
	next, _ = next.(model).Update(msg)
	m = next.(model)
	if controller.submitted.TerminalTools == nil {
		t.Fatalf("TerminalTools = nil, want shared terminal tools")
	}
	if !strings.Contains(controller.submitted.TurnContext, "Terminal 1 is directly shared") ||
		!strings.Contains(controller.submitted.TurnContext, "npm test") {
		t.Fatalf("turn context = %q, want direct terminal context", controller.submitted.TurnContext)
	}
	if m.activeRightTab() != workbench.RightTabTerm {
		t.Fatalf("right tab = %q, want terminal preserved after submit", m.activeRightTab())
	}
}

func TestEnterOnTranscriptStartsEmptyComposer(t *testing.T) {
	controller := &fakeController{state: workbench.State{
		Commands: workbench.DefaultCommands(),
		Transcript: []workbench.TranscriptItem{{
			ID:    "m1",
			Kind:  workbench.TranscriptUser,
			Title: "you",
			Text:  "previous prompt",
		}},
	}}
	m := newModel(context.Background(), controller, controller.state)
	m.focus = focusTranscript
	m.chat.SetItems(controller.state.Transcript)
	m.chat.FollowLatest()

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil {
		t.Fatalf("enter returned unexpected cmd")
	}
	m = next.(model)
	if m.mode != modeInsert || strings.TrimSpace(m.composer.Value()) != "" {
		t.Fatalf("mode/composer = %s/%q, want empty insert composer", m.mode, m.composer.Value())
	}
}

func TestTerminalTabTogglesExpandedOnlyFromContext(t *testing.T) {
	controller := &fakeController{state: workbench.State{Commands: workbench.DefaultCommands(), RightTab: workbench.RightTabTerm}}
	m := newModel(context.Background(), controller, controller.state)
	m.focus = focusContext
	m.term = &terminalSession{lines: []string{"ready"}}

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'z'}})
	m = next.(model)
	if !m.term.expanded {
		t.Fatalf("terminal expanded = false, want true")
	}
	m.focus = focusTranscript
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'z'}})
	m = next.(model)
	if !m.term.expanded {
		t.Fatalf("terminal expanded changed outside context")
	}
}

func TestTerminalCtrlGDocksAndReturnsToChat(t *testing.T) {
	controller := &fakeController{state: workbench.State{Commands: workbench.DefaultCommands(), RightTab: workbench.RightTabTerm}}
	m := newModel(context.Background(), controller, controller.state)
	m.mode = modeTerminal
	m.focus = focusContext
	m.term = &terminalSession{active: true, expanded: true, lines: []string{"ready"}}

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlG})
	if cmd != nil {
		t.Fatalf("ctrl+g returned unexpected cmd")
	}
	m = next.(model)
	if m.mode != modeNormal || m.focus != focusContext || m.term.expanded {
		t.Fatalf("mode/focus/expanded = %s/%d/%v, want normal context docked", m.mode, m.focus, m.term.expanded)
	}
}

func TestTerminalEscPassesThroughAndKeepsTerminalMode(t *testing.T) {
	controller := &fakeController{state: workbench.State{Commands: workbench.DefaultCommands(), RightTab: workbench.RightTabTerm}}
	m := newModel(context.Background(), controller, controller.state)
	m.mode = modeTerminal
	m.focus = focusContext
	m.term = &terminalSession{active: true, lines: []string{"ready"}}

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if cmd != nil {
		t.Fatalf("esc returned unexpected cmd")
	}
	m = next.(model)
	if m.mode != modeTerminal || m.focus != focusContext {
		t.Fatalf("mode/focus = %s/%d, want terminal context", m.mode, m.focus)
	}
}

func TestTermCommandOpensWithoutFocusingOrExpanding(t *testing.T) {
	controller := &fakeController{state: workbench.State{Commands: workbench.DefaultCommands()}}
	m := newModel(context.Background(), controller, controller.state)

	next, cmd := m.executeLine(":term")
	if cmd != nil {
		t.Fatalf(":term returned unexpected cmd")
	}
	m = next.(model)
	if m.mode != modeNormal || m.focus != focusTranscript || m.activeRightTab() != workbench.RightTabTerm || m.term.expanded || len(m.terms) != 1 {
		t.Fatalf("mode/focus/tab/expanded/terms = %s/%d/%s/%v/%d, want normal transcript term docked", m.mode, m.focus, m.activeRightTab(), m.term.expanded, len(m.terms))
	}
}

func TestTermCommandCreatesAndSelectsNumberedTerminals(t *testing.T) {
	controller := &fakeController{state: workbench.State{Commands: workbench.DefaultCommands()}}
	m := newModel(context.Background(), controller, controller.state)

	next, _ := m.executeLine(":t")
	m = next.(model)
	next, _ = m.executeLine(":t")
	m = next.(model)
	if len(m.terms) != 2 || m.termSlot != 1 {
		t.Fatalf("terms/slot = %d/%d, want two terminals selected second", len(m.terms), m.termSlot)
	}
	next, _ = m.executeLine(":t 1")
	m = next.(model)
	if m.termSlot != 0 || m.term != m.terms[0] {
		t.Fatalf("slot = %d, want first terminal selected", m.termSlot)
	}
	next, _ = m.executeLine(":t 3")
	m = next.(model)
	if m.state.Notice != "terminal 3 does not exist" || m.termSlot != 0 {
		t.Fatalf("notice/slot = %q/%d, want missing terminal error without switching", m.state.Notice, m.termSlot)
	}
}

func TestCtrlHLMovePanesAndPlainHLStaysLocal(t *testing.T) {
	controller := &fakeController{state: workbench.State{Commands: workbench.DefaultCommands(), RightTab: workbench.RightTabFiles}}
	m := newModel(context.Background(), controller, controller.state)
	m.focus = focusTranscript

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlL})
	m = next.(model)
	if m.focus != focusContext {
		t.Fatalf("focus = %d, want context after Ctrl+L", m.focus)
	}
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
	m = next.(model)
	if m.focus != focusContext || m.activeRightTab() != workbench.RightTabArtifacts {
		t.Fatalf("focus/tab = %d/%s, want local tab cycle on right pane", m.focus, m.activeRightTab())
	}
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlH})
	m = next.(model)
	if m.focus != focusTranscript {
		t.Fatalf("focus = %d, want transcript after Ctrl+H", m.focus)
	}
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
	m = next.(model)
	if m.focus != focusTranscript {
		t.Fatalf("focus = %d, want plain l not to leave transcript", m.focus)
	}
}

func TestEnterOnTermTabFocusesWithoutExpanding(t *testing.T) {
	controller := &fakeController{state: workbench.State{Commands: workbench.DefaultCommands(), RightTab: workbench.RightTabTerm}}
	m := newModel(context.Background(), controller, controller.state)
	m.focus = focusContext
	m.term = &terminalSession{active: true, lines: []string{"ready"}}

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil {
		t.Fatalf("enter returned unexpected cmd")
	}
	m = next.(model)
	if m.mode != modeTerminal || m.focus != focusContext || m.term.expanded {
		t.Fatalf("mode/focus/expanded = %s/%d/%v, want terminal context not expanded", m.mode, m.focus, m.term.expanded)
	}
}

func TestTerminalUsesPlainShellEnvironment(t *testing.T) {
	zsh := terminalCommand("/bin/zsh")
	if len(zsh.Args) < 2 || zsh.Args[1] != "-f" {
		t.Fatalf("zsh args = %#v, want -f", zsh.Args)
	}
	env := terminalEnv([]string{"TERM=xterm-256color", "PROMPT=fancy", "PATH=/bin"})
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "PROMPT=fancy") || strings.Contains(joined, "TERM=xterm-256color") {
		t.Fatalf("env kept fancy terminal settings:\n%s", joined)
	}
	if !strings.Contains(joined, "TERM=dumb") || !strings.Contains(joined, "PROMPT=%n:%~ %# ") || !strings.Contains(joined, "PS1=\\u:\\w \\$ ") || !strings.Contains(joined, "PATH=/bin") {
		t.Fatalf("env missing plain terminal settings:\n%s", joined)
	}
}

func TestTerminalOutputCollapsesCRLFAndClear(t *testing.T) {
	tm := newTerminalSession()
	tm.partial = "freecode % ls"
	tm.appendOutput("\r\ncmd\n\r\n")
	if len(tm.lines) != 2 || tm.lines[0] != "freecode % ls" || tm.lines[1] != "cmd" {
		t.Fatalf("lines = %#v partial=%q, want compact command and output", tm.lines, tm.partial)
	}
	tm.appendOutput("\x1b[H\x1b[2Jfreecode % ")
	if len(tm.lines) != 0 || tm.partial != "freecode % " {
		t.Fatalf("after clear lines=%#v partial=%q, want cleared screen prompt", tm.lines, tm.partial)
	}
}

func TestTerminalOutputAppliesBackspaceEcho(t *testing.T) {
	tm := newTerminalSession()
	tm.partial = "freecode % abcd"
	tm.appendOutput("\b \b")
	if tm.partial != "freecode % abc" {
		t.Fatalf("partial = %q, want erased character", tm.partial)
	}
	tm.active = true
	tm.input = "clear"
	tm.lines = []string{"old"}
	tm.writeKey(tea.KeyMsg{Type: tea.KeyEnter})
	if len(tm.lines) != 0 || tm.partial != "" || tm.input != "" {
		t.Fatalf("clear enter lines/partial/input = %#v/%q/%q, want cleared", tm.lines, tm.partial, tm.input)
	}
	tm.input = "abc"
	tm.writeKey(tea.KeyMsg{Type: tea.KeyBackspace})
	if tm.input != "ab" {
		t.Fatalf("input = %q, want backspace to update input buffer", tm.input)
	}
}

func TestEscUnwindsPaletteCommandAndComposer(t *testing.T) {
	controller := &fakeController{state: workbench.State{Commands: workbench.DefaultCommands()}}
	m := newModel(context.Background(), controller, controller.state)

	m = m.openPalette()
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = next.(model)
	if m.mode != modeNormal || m.overlay != overlayNone {
		t.Fatalf("palette esc mode/overlay = %s/%d, want normal none", m.mode, m.overlay)
	}

	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{':'}})
	m = next.(model)
	m = sendRunes(t, m, "files")
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = next.(model)
	if m.mode != modeNormal || m.command.Focused() {
		t.Fatalf("command esc mode/focus = %s/%v, want normal blurred", m.mode, m.command.Focused())
	}

	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'i'}})
	m = next.(model)
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = next.(model)
	if m.mode != modeNormal || m.composer.Focused() {
		t.Fatalf("insert esc mode/focus = %s/%v, want normal blurred", m.mode, m.composer.Focused())
	}
}

func TestBottomInputIsSharedByMode(t *testing.T) {
	controller := &fakeController{state: workbench.State{Commands: workbench.DefaultCommands()}}
	m := newModel(context.Background(), controller, controller.state)
	m.composer.SetValue("draft prompt")
	m.command.SetValue("files")

	normal := stripANSI(m.composerView())
	if strings.Contains(normal, "draft prompt") || strings.Contains(normal, ":files") {
		t.Fatalf("normal bottom showed input contents:\n%s", normal)
	}
	m.mode = modeInsert
	insert := stripANSI(m.composerView())
	if !strings.Contains(insert, "draft prompt") || strings.Contains(insert, ":files") {
		t.Fatalf("insert bottom did not show only composer:\n%s", insert)
	}
	m.mode = modeCommand
	command := stripANSI(m.composerView())
	if !strings.Contains(command, ":files") || strings.Contains(command, "draft prompt") {
		t.Fatalf("command bottom did not show only command:\n%s", command)
	}
}

func TestFilesActivationUsesOpenPath(t *testing.T) {
	controller := &fakeController{state: workbench.State{
		Commands: workbench.DefaultCommands(),
		RightTab: workbench.RightTabFiles,
		Files:    []workbench.WorkspaceFile{{ID: "f1", Path: "README.md", Name: "README.md"}},
	}}
	m := newModel(context.Background(), controller, controller.state)
	m.focus = focusContext

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatalf("enter on file returned nil cmd")
	}
	m = next.(model)
	if m.state.Notice != "opening README.md in external Neovim" {
		t.Fatalf("notice = %q, want editor notice", m.state.Notice)
	}
}

func TestFilesPaneShowsFoldersAndRevealsNestedBasename(t *testing.T) {
	controller := &fakeController{state: workbench.State{
		Commands: workbench.DefaultCommands(),
		RightTab: workbench.RightTabFiles,
		Files: []workbench.WorkspaceFile{{
			ID:         "f1",
			Path:       "internal/adapters/tui2/workbench.go",
			Name:       "workbench.go",
			StatusLine: "??",
		}},
	}}
	m := newModel(context.Background(), controller, controller.state)
	m.focus = focusContext
	view := stripANSI(m.rightView(36, 8))

	if !strings.Contains(view, "[+] internal/") {
		t.Fatalf("files pane missing collapsed folder:\n%s", view)
	}
	if strings.Contains(view, "workbench.go") {
		t.Fatalf("collapsed files pane showed nested basename:\n%s", view)
	}

	m.toggleFolder("internal")
	m.toggleFolder("internal/adapters")
	m.toggleFolder("internal/adapters/tui2")
	view = stripANSI(m.rightView(44, 12))
	if !strings.Contains(view, "workbench.go") {
		t.Fatalf("expanded files pane missing basename:\n%s", view)
	}
	if !strings.Contains(view, "tui2/") {
		t.Fatalf("expanded files pane missing nested folder context:\n%s", view)
	}
}

func TestGitEnterDetailsAndOEditsChangedFile(t *testing.T) {
	controller := &fakeController{state: workbench.State{
		Commands: workbench.DefaultCommands(),
		RightTab: workbench.RightTabGit,
		GitFiles: []workbench.WorkspaceFile{{ID: "g1", Path: "README.md", Name: "README.md", StatusLine: "M README.md"}},
	}}
	m := newModel(context.Background(), controller, controller.state)
	m.focus = focusContext

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatalf("enter on git file returned nil cmd")
	}
	msg := firstActionMsg(t, cmd)
	next, _ = next.(model).Update(msg)
	if controller.detailed != "g1" {
		t.Fatalf("detailed = %q, want g1", controller.detailed)
	}

	next, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'o'}})
	if cmd == nil {
		t.Fatalf("o on git file returned nil cmd")
	}
	m = next.(model)
	if m.state.Notice != "opening README.md in external Neovim" {
		t.Fatalf("notice = %q, want edit notice", m.state.Notice)
	}
}

func TestSessionCommands(t *testing.T) {
	controller := &fakeController{state: workbench.State{
		Commands: workbench.DefaultCommands(),
		Sessions: []workbench.SessionSummary{{ID: session.ID("s1"), Title: "First"}},
	}}
	m := newModel(context.Background(), controller, controller.state)

	next, cmd := m.executeLine(":sessions")
	if cmd != nil {
		t.Fatalf(":sessions returned unexpected cmd")
	}
	m = next.(model)
	if m.state.Detail.ID != "sessions" || !strings.Contains(m.state.Detail.Body, "s1") {
		t.Fatalf("sessions detail = %#v, want session list", m.state.Detail)
	}

	next, cmd = m.executeLine(":resume s1")
	if cmd == nil {
		t.Fatalf(":resume returned nil cmd")
	}
	msg := firstActionMsg(t, cmd)
	next, _ = next.(model).Update(msg)
	m = next.(model)
	if controller.resumed != session.ID("s1") {
		t.Fatalf("resumed = %q, want s1", controller.resumed)
	}

	next, cmd = m.executeLine(":new")
	if cmd == nil {
		t.Fatalf(":new returned nil cmd")
	}
	msg = firstActionMsg(t, cmd)
	next, _ = next.(model).Update(msg)
	if !controller.newSessionCalled {
		t.Fatalf("new session was not called")
	}
}

func TestPaletteWorkspaceCommandsUseCanonicalIDs(t *testing.T) {
	controller := &fakeController{state: workbench.State{
		Commands: workbench.DefaultCommands(),
		Sessions: []workbench.SessionSummary{{ID: session.ID("s1"), Title: "First"}},
	}}
	m := newModel(context.Background(), controller, controller.state).openPalette()

	cases := []struct {
		query string
		want  string
	}{
		{query: "sessions", want: "session.list"},
		{query: "files", want: "tab.files"},
		{query: "artifacts", want: "tab.artifacts"},
		{query: "settings", want: "settings.open"},
	}
	for _, tc := range cases {
		m.palette.SetValue(tc.query)
		m.paletteCursor = 0
		commands := m.filteredCommands()
		if len(commands) == 0 || commands[0].ID != tc.want {
			t.Fatalf("query %q commands = %#v, want first %s", tc.query, commands, tc.want)
		}
		next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
		if cmd != nil {
			msg := firstActionMsg(t, cmd)
			next, _ = next.(model).Update(msg)
		}
		m = next.(model).openPalette()
		if strings.Contains(m.state.Notice, "unknown command") {
			t.Fatalf("query %q notice = %q", tc.query, m.state.Notice)
		}
	}
}

func TestAgentActivationTargetsComposerSubmission(t *testing.T) {
	controller := &fakeController{state: workbench.State{
		Commands: workbench.DefaultCommands(),
		Agents: []workbench.AgentItem{{
			ID:      "a1",
			Name:    "worker",
			Role:    "worker",
			Status:  "running",
			Summary: "editing README",
		}},
	}}
	m := newModel(context.Background(), controller, controller.state)
	m.focus = focusAgents
	m.leftCursor = 1

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatalf("enter on agent returned nil cmd, want conversation switch")
	}
	msg := firstActionMsg(t, cmd)
	next, _ = next.(model).Update(msg)
	m = next.(model)
	if m.mode != modeNormal || m.focus != focusTranscript || m.composer.Focused() {
		t.Fatalf("mode/focus/composer = %s/%d/%v, want normal transcript blurred", m.mode, m.focus, m.composer.Focused())
	}
	if m.state.ActiveConversation.Kind != "agent" || m.state.ActiveConversation.ID != "a1" {
		t.Fatalf("active conversation = %#v, want agent a1", m.state.ActiveConversation)
	}

	next, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'i'}})
	if cmd != nil {
		t.Fatalf("i in agent conversation returned unexpected cmd")
	}
	m = next.(model)
	if m.mode != modeInsert || !m.composer.Focused() {
		t.Fatalf("mode/focus = %s/%v, want insert focused", m.mode, m.composer.Focused())
	}
	m.composer.SetValue("continue")
	next, cmd = m.submit(false)
	if cmd == nil {
		t.Fatalf("submit returned nil cmd")
	}
	msg = firstActionMsg(t, cmd)
	next, _ = next.(model).Update(msg)
	if controller.submitted.Target.Kind != "agent" || controller.submitted.Target.ID != "a1" || controller.submitted.Swarm {
		t.Fatalf("submitted = %#v, want non-swarm agent target", controller.submitted)
	}
}

func TestEnterOnMainActivatesMainConversation(t *testing.T) {
	controller := &fakeController{state: workbench.State{Commands: workbench.DefaultCommands()}}
	m := newModel(context.Background(), controller, controller.state)
	m.focus = focusAgents
	m.leftCursor = 0

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatalf("enter on main returned nil cmd, want conversation switch")
	}
	msg := firstActionMsg(t, cmd)
	next, _ = next.(model).Update(msg)
	m = next.(model)
	if m.mode != modeNormal || m.focus != focusTranscript || m.composer.Focused() {
		t.Fatalf("mode/focus/composer = %s/%d/%v, want normal transcript blurred", m.mode, m.focus, m.composer.Focused())
	}
	if m.state.ActiveConversation.Kind != "main" || m.state.ActiveConversation.ID != "main" {
		t.Fatalf("active conversation = %#v, want main", m.state.ActiveConversation)
	}
}

func TestEnterOnTranscriptStartsActiveConversationComposer(t *testing.T) {
	controller := &fakeController{state: workbench.State{
		Commands: workbench.DefaultCommands(),
		ActiveConversation: workbench.ConversationTarget{
			Kind:  "main",
			ID:    "main",
			Title: "Main orchestrator",
		},
		Transcript: []workbench.TranscriptItem{{ID: "m1", Actor: "assistant", Text: "hello"}},
	}}
	m := newModel(context.Background(), controller, controller.state)
	m.focus = focusTranscript

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil {
		t.Fatalf("enter on transcript returned unexpected cmd")
	}
	m = next.(model)
	if m.mode != modeInsert || !m.composer.Focused() {
		t.Fatalf("mode/focus = %s/%v, want insert focused", m.mode, m.composer.Focused())
	}
	if m.state.ActiveConversation.ID != "main" {
		t.Fatalf("active conversation = %#v, want main", m.state.ActiveConversation)
	}
}

func TestEnterOnTranscriptAgentOpensAgentConversation(t *testing.T) {
	controller := &fakeController{state: workbench.State{
		Commands: workbench.DefaultCommands(),
		Transcript: []workbench.TranscriptItem{{
			ID:    "m1",
			Kind:  workbench.TranscriptAgent,
			Actor: "worker",
			Title: "worker completed",
			Text:  "edited files",
			Meta:  map[string]string{"agent_id": "a1"},
		}},
	}}
	m := newModel(context.Background(), controller, controller.state)
	m.focus = focusTranscript
	m.chat.selected = 0

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatalf("enter on agent transcript returned nil cmd, want conversation switch")
	}
	msg := firstActionMsg(t, cmd)
	next, _ = next.(model).Update(msg)
	m = next.(model)
	if m.state.ActiveConversation.Kind != "agent" || m.state.ActiveConversation.ID != "a1" {
		t.Fatalf("active conversation = %#v, want agent a1", m.state.ActiveConversation)
	}
}

func TestEscFromAgentConversationReturnsToMain(t *testing.T) {
	controller := &fakeController{state: workbench.State{
		Commands: workbench.DefaultCommands(),
		ActiveConversation: workbench.ConversationTarget{
			Kind:  "agent",
			ID:    "a1",
			Title: "worker",
		},
	}}
	m := newModel(context.Background(), controller, controller.state)
	m.focus = focusTranscript

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if cmd == nil {
		t.Fatalf("esc from agent returned nil cmd, want main conversation switch")
	}
	msg := firstActionMsg(t, cmd)
	next, _ = next.(model).Update(msg)
	m = next.(model)
	if m.state.ActiveConversation.Kind != "main" || m.state.ActiveConversation.ID != "main" {
		t.Fatalf("active conversation = %#v, want main", m.state.ActiveConversation)
	}
}

func TestPaletteAgentFollowupUsesSelectedAgent(t *testing.T) {
	controller := &fakeController{state: workbench.State{
		Commands: workbench.DefaultCommands(),
		Agents: []workbench.AgentItem{{
			ID:     "a1",
			Name:   "worker",
			Role:   "worker",
			Status: "running",
		}},
	}}
	m := newModel(context.Background(), controller, controller.state)
	m.focus = focusAgents
	m.leftCursor = 1

	next, cmd := m.executeCommand("agent.followup")
	if cmd != nil {
		t.Fatalf("agent.followup returned unexpected cmd")
	}
	m = next.(model)
	if m.mode != modeInsert || m.state.ActiveConversation.Kind != "agent" || m.state.ActiveConversation.ID != "a1" {
		t.Fatalf("mode/target = %s/%#v, want insert agent a1", m.mode, m.state.ActiveConversation)
	}
	m.composer.SetValue("status?")
	next, cmd = m.submit(false)
	if cmd == nil {
		t.Fatalf("submit returned nil cmd")
	}
	msg := firstActionMsg(t, cmd)
	next, _ = next.(model).Update(msg)
	if controller.submitted.Target.Kind != "agent" || controller.submitted.Target.ID != "a1" || controller.submitted.Swarm {
		t.Fatalf("submitted = %#v, want non-swarm selected agent target", controller.submitted)
	}
}

func TestCommandCompletionCompletesCommandsFilesAndSessions(t *testing.T) {
	controller := &fakeController{state: workbench.State{
		Commands: workbench.DefaultCommands(),
		Files:    []workbench.WorkspaceFile{{ID: "f1", Path: "README.md"}, {ID: "f2", Path: "main.go"}},
		Sessions: []workbench.SessionSummary{{ID: session.ID("session-alpha")}},
	}}
	m := newModel(context.Background(), controller, controller.state)

	m.command.SetValue("ses")
	m.completeCommandInput()
	if got := m.command.Value(); got != "sessions" {
		t.Fatalf("command completion = %q, want sessions", got)
	}
	m.command.SetValue("edit READ")
	m.completeCommandInput()
	if got := m.command.Value(); got != "edit README.md" {
		t.Fatalf("file completion = %q, want edit README.md", got)
	}
	m.command.SetValue("resume session-a")
	m.completeCommandInput()
	if got := m.command.Value(); got != "resume session-alpha" {
		t.Fatalf("session completion = %q, want resume session-alpha", got)
	}
}

func TestTutorialCommandInterceptsPromptWithoutSubmitting(t *testing.T) {
	controller := &fakeController{state: workbench.State{Commands: workbench.DefaultCommands()}}
	m := newModel(context.Background(), controller, controller.state)

	next, cmd := m.executeLine(":tutorial")
	if cmd != nil {
		t.Fatalf(":tutorial returned cmd, want local overlay only")
	}
	m = next.(model)
	if m.overlay != overlayTutorial {
		t.Fatalf("overlay = %v, want tutorial", m.overlay)
	}
	if view := stripANSI(m.View()); !strings.Contains(view, "FreeCode Tutorial") || !strings.Contains(view, "Mission 1") {
		t.Fatalf("tutorial view missing expected content:\n%s", view)
	}

	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("i")})
	m = next.(model)
	if m.tutorial.inputMode != tutorialComposer {
		t.Fatalf("tutorial input mode = %q, want composer", m.tutorial.inputMode)
	}
	m = sendRunes(t, m, "explain this repo")
	next, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil {
		t.Fatalf("tutorial prompt returned cmd, want no API/controller dispatch")
	}
	m = next.(model)
	if !m.tutorial.completed["prompt.insert"] {
		t.Fatalf("tutorial did not mark prompt.insert complete")
	}
	if m.tutorial.step != 1 {
		t.Fatalf("tutorial step = %d, want 1 after passing first mission", m.tutorial.step)
	}
	if controller.submitted.Text != "" {
		t.Fatalf("tutorial submitted prompt to controller: %#v", controller.submitted)
	}
	if got := strings.Join(m.tutorial.log, "\n"); !strings.Contains(got, "No API request was sent") {
		t.Fatalf("tutorial log = %q, want simulated no-api response", got)
	}
}

func TestTutorialCommandGivesCorrectionForWrongCommand(t *testing.T) {
	controller := &fakeController{state: workbench.State{Commands: workbench.DefaultCommands()}}
	m := newModel(context.Background(), controller, controller.state)
	m = m.openTutorial()
	m.tutorial.step = 1 // :ops mission

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(":")})
	m = next.(model)
	m = sendRunes(t, m, "git")
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil {
		t.Fatalf("wrong tutorial command returned cmd, want local correction only")
	}
	m = next.(model)
	if m.tutorial.completed["tab.ops"] {
		t.Fatalf("wrong command completed :ops mission")
	}
	if !strings.Contains(m.tutorial.last, `typed ":git" wrong`) || !strings.Contains(m.tutorial.last, "Hit Esc") {
		t.Fatalf("tutorial correction = %q", m.tutorial.last)
	}
}

func TestTutorialWrapsTextWithoutHorizontalEllipses(t *testing.T) {
	controller := &fakeController{state: workbench.State{Commands: workbench.DefaultCommands()}}
	m := newModel(context.Background(), controller, controller.state)
	m.width = 150
	m.height = 52
	m = m.openTutorial()
	m.tutorial.step = 3
	m.tutorial.log = []string{
		"tutorial: No API calls are made here. Commands and prompts are intercepted locally.",
		"you: explain this repo",
		"freecode: That would send a normal chat prompt to the active conversation. No API request was sent.",
		"you: :ops",
		"freecode: The right pane would show active runs, queues, approvals, terminal sharing, and context.",
		"you: :context",
		"freecode: The context inspector would open with conversation tail, memory, terminal sharing state, and token estimate.",
	}

	view := stripANSI(m.tutorialView())
	if strings.Contains(view, "...") {
		t.Fatalf("tutorial view contains horizontal truncation ellipses, want wrapped text:\n%s", view)
	}
	for _, line := range strings.Split(view, "\n") {
		if printableWidth(line) > m.width {
			t.Fatalf("tutorial line width = %d > terminal width %d: %q\nfull:\n%s", printableWidth(line), m.width, line, view)
		}
	}
}

func TestMCPStatusCommandOpensDetailOverlay(t *testing.T) {
	controller := &fakeController{state: workbench.State{Commands: workbench.DefaultCommands()}}
	m := newModel(context.Background(), controller, controller.state)

	next, cmd := m.executeLine(":mcp status")
	if cmd == nil {
		t.Fatalf(":mcp status returned nil cmd, want action")
	}
	msg := firstActionMsg(t, cmd)
	next, _ = next.(model).Update(msg)
	m = next.(model)
	if m.overlay != overlayDetail || m.state.Detail.ID != "mcp-status" {
		t.Fatalf("overlay/detail = %v/%#v, want MCP detail overlay", m.overlay, m.state.Detail)
	}
}

func sendRunes(t *testing.T, m model, value string) model {
	t.Helper()
	for _, r := range value {
		next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = next.(model)
	}
	return m
}

func pressKeys(t *testing.T, m model, keys ...string) model {
	t.Helper()
	for _, key := range keys {
		next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)})
		m = next.(model)
	}
	return m
}

func printableWidth(value string) int {
	return lipgloss.Width(stripANSI(value))
}

func stripANSI(value string) string {
	var b strings.Builder
	inEscape := false
	inCSI := false
	for i := 0; i < len(value); i++ {
		ch := value[i]
		if inEscape {
			if ch == '[' {
				inEscape = false
				inCSI = true
				continue
			}
			inEscape = false
			continue
		}
		if inCSI {
			if ch >= '@' && ch <= '~' {
				inCSI = false
			}
			continue
		}
		if ch == 0x1b {
			inEscape = true
			continue
		}
		b.WriteByte(ch)
	}
	return b.String()
}

func firstActionMsg(t *testing.T, cmd tea.Cmd) tea.Msg {
	t.Helper()
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, child := range batch {
			if child == nil {
				continue
			}
			msg = child()
			if _, tick := msg.(tickMsg); tick {
				continue
			}
			return msg
		}
		t.Fatalf("batch did not contain an action message")
	}
	return msg
}

func containsAll(value string, needles ...string) bool {
	for _, needle := range needles {
		if !strings.Contains(value, needle) {
			return false
		}
	}
	return true
}

func lineCount(value string) int {
	if value == "" {
		return 0
	}
	return strings.Count(value, "\n") + 1
}

type fakeController struct {
	state               workbench.State
	submitted           workbench.SubmitRequest
	approved            string
	rejected            string
	detailed            string
	copied              string
	copiedWithFences    bool
	opened              string
	openedPath          string
	openedLine          int
	compacted           bool
	newSessionCalled    bool
	newSessionTitle     string
	resumed             session.ID
	renamed             string
	activeConversation  workbench.ConversationTarget
	shellCommand        string
	sharedTerminalTitle string
	sharedTerminalBody  string
}

func (c *fakeController) Load(ctx context.Context) (workbench.State, error) {
	return c.state, nil
}

func (c *fakeController) SubmitPrompt(ctx context.Context, request workbench.SubmitRequest) (workbench.State, error) {
	c.submitted = request
	c.state.Notice = "prompt sent"
	return c.state, nil
}

func (c *fakeController) Copy(ctx context.Context, id string, withFences bool) (workbench.State, error) {
	c.copied = id
	c.copiedWithFences = withFences
	c.state.Notice = "copied " + id
	return c.state, nil
}

func (c *fakeController) Open(ctx context.Context, ref string) (workbench.State, error) {
	c.opened = ref
	c.state.Notice = "opened " + ref
	return c.state, nil
}

func (c *fakeController) OpenPath(ctx context.Context, path string, line int) (workbench.State, error) {
	c.openedPath = path
	c.openedLine = line
	c.state.Notice = "opened " + path
	return c.state, nil
}

func (c *fakeController) Detail(ctx context.Context, id string) (workbench.State, error) {
	c.detailed = id
	c.state.Detail = workbench.Item{ID: id, Title: "detail"}
	return c.state, nil
}

func (c *fakeController) Approve(ctx context.Context, id string) (workbench.State, error) {
	c.approved = id
	c.state.Approvals = nil
	c.state.Notice = "approved " + id
	return c.state, nil
}

func (c *fakeController) Reject(ctx context.Context, id string) (workbench.State, error) {
	c.rejected = id
	c.state.Approvals = nil
	c.state.Notice = "rejected " + id
	return c.state, nil
}

func (c *fakeController) SetApproval(ctx context.Context, mode permission.Mode) (workbench.State, error) {
	c.state.Approval = mode
	c.state.Notice = "approval: " + string(mode)
	return c.state, nil
}

func (c *fakeController) Compact(ctx context.Context) (workbench.State, error) {
	c.compacted = true
	c.state.Notice = "compacted context"
	return c.state, nil
}

func (c *fakeController) Palette(ctx context.Context) (workbench.State, error) {
	c.state.Detail = workbench.Item{ID: "commands", Title: "Command Palette"}
	return c.state, nil
}

func (c *fakeController) NewSession(ctx context.Context, title string) (workbench.State, error) {
	c.newSessionCalled = true
	c.newSessionTitle = title
	c.state.Notice = "new session"
	return c.state, nil
}

func (c *fakeController) ResumeSession(ctx context.Context, id session.ID) (workbench.State, error) {
	c.resumed = id
	c.state.SessionID = id
	c.state.Notice = "resumed session " + string(id)
	return c.state, nil
}

func (c *fakeController) RenameSession(ctx context.Context, title string) (workbench.State, error) {
	c.renamed = title
	c.state.Notice = "renamed session"
	return c.state, nil
}

func (c *fakeController) SetActiveConversation(ctx context.Context, target workbench.ConversationTarget) (workbench.State, error) {
	c.activeConversation = target
	c.state.ActiveConversation = target
	c.state.Notice = "conversation: " + target.Title
	return c.state, nil
}

func (c *fakeController) RunShell(ctx context.Context, command string) (workbench.State, error) {
	c.shellCommand = command
	c.state.Detail = workbench.Item{ID: "sh1", Kind: "shell", Title: "! " + command, Body: "ok"}
	c.state.Notice = "shell completed"
	return c.state, nil
}

func (c *fakeController) ShareTerminal(ctx context.Context, title string, body string) (workbench.State, error) {
	c.sharedTerminalTitle = title
	c.sharedTerminalBody = body
	c.state.Detail = workbench.Item{ID: "term1", Kind: "terminal", Title: title, Body: body}
	c.state.Notice = "terminal shared"
	return c.state, nil
}

func (c *fakeController) MCPStatus(ctx context.Context) (workbench.State, error) {
	c.state.Detail = workbench.Item{ID: "mcp-status", Kind: "mcp", Title: "MCP status", Body: "mcp: ready"}
	c.state.Notice = "mcp status"
	return c.state, nil
}

func (c *fakeController) MCPTools(ctx context.Context) (workbench.State, error) {
	c.state.Detail = workbench.Item{ID: "mcp-tools", Kind: "mcp", Title: "MCP tools", Body: "mcp tool list"}
	c.state.Notice = "mcp tools"
	return c.state, nil
}

func (c *fakeController) MCPReload(ctx context.Context) (workbench.State, error) {
	c.state.Detail = workbench.Item{ID: "mcp-status", Kind: "mcp", Title: "MCP status", Body: "mcp reloaded"}
	c.state.Notice = "mcp reloaded"
	return c.state, nil
}

func (c *fakeController) MCPDoctor(ctx context.Context) (workbench.State, error) {
	c.state.Detail = workbench.Item{ID: "mcp-doctor", Kind: "mcp", Title: "MCP doctor", Body: "mcp doctor"}
	c.state.Notice = "mcp doctor"
	return c.state, nil
}
