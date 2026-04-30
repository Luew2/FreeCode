package tui2

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/Luew2/FreeCode/internal/app/workbench"
	coremodel "github.com/Luew2/FreeCode/internal/core/model"
	"github.com/Luew2/FreeCode/internal/core/permission"
	"github.com/Luew2/FreeCode/internal/core/session"
)

type Controller interface {
	Load(ctx context.Context) (workbench.State, error)
	SubmitPrompt(ctx context.Context, request workbench.SubmitRequest) (workbench.State, error)
	Copy(ctx context.Context, id string, withFences bool) (workbench.State, error)
	Open(ctx context.Context, ref string) (workbench.State, error)
	Detail(ctx context.Context, id string) (workbench.State, error)
	Approve(ctx context.Context, id string) (workbench.State, error)
	Reject(ctx context.Context, id string) (workbench.State, error)
	SetApproval(ctx context.Context, mode permission.Mode) (workbench.State, error)
	Compact(ctx context.Context) (workbench.State, error)
	Palette(ctx context.Context) (workbench.State, error)
}

type pathOpener interface {
	OpenPath(ctx context.Context, path string, line int) (workbench.State, error)
}

type sessionCreator interface {
	NewSession(ctx context.Context, title string) (workbench.State, error)
}

type sessionResumer interface {
	ResumeSession(ctx context.Context, id session.ID) (workbench.State, error)
}

type sessionRenamer interface {
	RenameSession(ctx context.Context, title string) (workbench.State, error)
}

type conversationSelector interface {
	SetActiveConversation(ctx context.Context, target workbench.ConversationTarget) (workbench.State, error)
}

type shellRunner interface {
	RunShell(ctx context.Context, command string) (workbench.State, error)
}

type terminalSharer interface {
	ShareTerminal(ctx context.Context, title string, body string) (workbench.State, error)
}

type Options struct {
	In         io.Reader
	Out        io.Writer
	Workbench  Controller
	Initial    workbench.State
	InitialSet bool
	AltScreen  bool
}

func Run(ctx context.Context, opts Options) error {
	if opts.Workbench == nil {
		return errors.New("workbench controller is required")
	}
	if opts.In == nil {
		opts.In = os.Stdin
	}
	if opts.Out == nil {
		opts.Out = os.Stdout
	}
	configureRenderer(opts.Out)
	state := opts.Initial
	if !opts.InitialSet {
		loaded, err := opts.Workbench.Load(ctx)
		if err != nil {
			return err
		}
		state = loaded
	}
	model := newModel(ctx, opts.Workbench, state)
	model.forceRepaint = true
	options := []tea.ProgramOption{tea.WithInput(opts.In), tea.WithOutput(opts.Out), tea.WithMouseCellMotion(), tea.WithANSICompressor()}
	if opts.AltScreen {
		options = append(options, tea.WithAltScreen())
	}
	_, err := tea.NewProgram(model, options...).Run()
	return err
}

type uiMode string

const (
	modeNormal   uiMode = "NORMAL"
	modeInsert   uiMode = "INSERT"
	modeCommand  uiMode = "COMMAND"
	modePalette  uiMode = "PALETTE"
	modeApprove  uiMode = "APPROVAL"
	modeTerminal uiMode = "TERM"
)

type focusPane int

const (
	focusAgents focusPane = iota
	focusTranscript
	focusContext
)

type overlayKind int

const (
	overlayNone overlayKind = iota
	overlayPalette
	overlayApproval
	overlayDetail
	overlayCopy
)

type model struct {
	ctx        context.Context
	controller Controller
	state      workbench.State

	width      int
	height     int
	mode       uiMode
	focus      focusPane
	busy       bool
	pendingKey string
	actionSeq  int
	activeRun  int

	leftCursor      int
	contextCursor   int
	approvalCursor  int
	paletteCursor   int
	expandedFolders map[string]bool

	chat           chatRenderer
	detail         viewport.Model
	approval       viewport.Model
	composer       textarea.Model
	command        textinput.Model
	palette        textinput.Model
	term           *terminalSession
	terms          []*terminalSession
	termSlot       int
	sharedTermSlot int
	terminalShared bool
	overlay        overlayKind

	detailPending    bool
	followTranscript bool
	forceRepaint     bool
	streamLoading    bool
	// mouseCaptured tracks whether the terminal is forwarding mouse events
	// to the program (1002-style cell motion). When false the terminal does
	// native click-drag text selection, which is what you want for copying
	// from chat. The `m` key in normal mode toggles this; the existing copy
	// overlay also flips it on entry/exit.
	mouseCaptured bool
}

type actionMsg struct {
	state workbench.State
	err   error
	runID int
}

type loadMsg struct {
	state workbench.State
	err   error
	runID int
}

type tickMsg struct {
	runID int
}

type contextEntry struct {
	id       string
	kind     string
	label    string
	path     string
	name     string
	status   string
	line     int
	approval bool
	depth    int
	expanded bool
}

type agentDisplayRow struct {
	index int
	depth int
}

func newModel(ctx context.Context, controller Controller, state workbench.State) model {
	if state.Mode == "" {
		state.Mode = string(modeNormal)
	}
	composer := textarea.New()
	composer.Placeholder = "i to write, Enter to send, :s <task> for swarm"
	composer.ShowLineNumbers = false
	composer.Blur()

	command := textinput.New()
	command.Prompt = ":"
	command.Blur()

	palette := textinput.New()
	palette.Prompt = "search "
	palette.Blur()

	m := model{
		ctx:            ctx,
		controller:     controller,
		state:          state,
		width:          100,
		height:         30,
		mode:           modeNormal,
		focus:          focusTranscript,
		chat:           newChatRenderer(48, 16),
		detail:         viewport.New(28, 16),
		approval:       viewport.New(72, 18),
		composer:       composer,
		command:        command,
		palette:        palette,
		sharedTermSlot: -1,
		mouseCaptured:  true,
		expandedFolders: map[string]bool{
			"": true,
		},
	}
	m.syncViewports()
	if len(state.Approvals) > 0 {
		m.overlay = overlayApproval
		m.mode = modeApprove
		m.focus = focusContext
	}
	return m
}

func (m model) Init() tea.Cmd {
	return nil
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.resize()
		m.resizeTerminal()
		return m, nil
	case terminalOutputMsg:
		term := msg.term
		if term == nil {
			term = m.currentTerminal()
		}
		if term != nil {
			term.reading = false
			if msg.text != "" {
				term.appendOutput(msg.text)
			}
			if msg.err != nil {
				term.markStopped(msg.err)
				m.state.Notice = "terminal stopped"
				return m, nil
			}
			return m, m.terminalReadCmdFor(term)
		}
		return m, nil
	case actionMsg:
		if msg.runID != 0 && msg.runID != m.activeRun {
			return m, nil
		}
		prevMode, prevOverlay, prevFocus := m.mode, m.overlay, m.focus
		prevRightTab := m.activeRightTab()
		m.busy = false
		pendingDetail := m.detailPending
		m.detailPending = false
		pendingFollow := m.followTranscript
		m.followTranscript = false
		if msg.err != nil {
			m.state.Notice = "error: " + msg.err.Error()
			m.mode = modeNormal
			m.overlay = overlayNone
			return m, m.repaintIfStructureChanged(prevMode, prevOverlay, prevFocus)
		}
		m.state = preserveRightTab(msg.state, prevRightTab)
		if pendingFollow {
			m.followLatestTranscript()
		}
		m.clampCursors()
		m.mode = modeNormal
		m.overlay = overlayNone
		if pendingDetail && strings.TrimSpace(m.state.Detail.ID) != "" {
			m.overlay = overlayDetail
			m.focus = focusContext
			m.syncViewports()
			m.syncDetailViewport()
			return m, m.repaintCmd()
		}
		if len(m.state.Approvals) > 0 {
			m.mode = modeApprove
			m.overlay = overlayApproval
			m.focus = focusContext
		}
		m.syncViewports()
		return m, m.repaintIfStructureChanged(prevMode, prevOverlay, prevFocus)
	case loadMsg:
		if msg.runID != 0 && msg.runID != m.activeRun {
			return m, nil
		}
		m.streamLoading = false
		if msg.err == nil {
			wasAtBottom := m.chat.IsAtBottom()
			prevRightTab := m.activeRightTab()
			m.state = preserveRightTab(msg.state, prevRightTab)
			if m.busy && m.followTranscript && wasAtBottom {
				m.followLatestTranscript()
			}
			m.clampCursors()
			m.syncViewports()
		}
		return m, nil
	case tickMsg:
		if msg.runID != 0 && msg.runID != m.activeRun {
			return m, nil
		}
		if !m.busy {
			return m, nil
		}
		if m.streamLoading {
			return m, streamTickCmd(m.activeRun)
		}
		m.streamLoading = true
		return m, tea.Batch(m.loadCmd(m.activeRun), streamTickCmd(m.activeRun))
	case tea.MouseMsg:
		return m.handleMouse(msg)
	case tea.KeyMsg:
		next, cmd := m.handleKey(msg)
		return m.withTransitionRepaint(next, cmd)
	}
	return m, nil
}

func (m model) withTransitionRepaint(next tea.Model, cmd tea.Cmd) (tea.Model, tea.Cmd) {
	nextModel, ok := next.(model)
	if !ok {
		return next, cmd
	}
	if m.mode != nextModel.mode || m.overlay != nextModel.overlay {
		return nextModel, nextModel.repaintCmd(cmd)
	}
	return nextModel, cmd
}

func (m model) repaintCmd(cmds ...tea.Cmd) tea.Cmd {
	var batched []tea.Cmd
	for _, cmd := range cmds {
		if cmd != nil {
			batched = append(batched, cmd)
		}
	}
	if m.forceRepaint {
		batched = append(batched, tea.ClearScreen)
	}
	if len(batched) == 0 {
		return nil
	}
	return tea.Batch(batched...)
}

func (m model) repaintIfStructureChanged(prevMode uiMode, prevOverlay overlayKind, prevFocus focusPane) tea.Cmd {
	if m.mode != prevMode || m.overlay != prevOverlay || m.focus != prevFocus {
		return m.repaintCmd()
	}
	return nil
}

func (m model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.mode == modeTerminal {
		return m.handleTerminalKey(msg)
	}
	if msg.String() == "ctrl+c" {
		return m.quit()
	}
	switch m.mode {
	case modeInsert:
		return m.handleInsertKey(msg)
	case modeCommand:
		return m.handleCommandKey(msg)
	case modePalette:
		return m.handlePaletteKey(msg)
	case modeApprove:
		return m.handleApprovalKey(msg)
	case modeTerminal:
		return m.handleTerminalKey(msg)
	default:
		return m.handleNormalKey(msg)
	}
}

func (m model) handleTerminalKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+g", "esc":
		m.mode = modeNormal
		if term := m.currentTerminal(); term != nil {
			term.expanded = false
		}
		m.focus = focusContext
		m.state.Notice = "terminal input closed; shell still running"
		return m, nil
	}
	term := m.currentTerminal()
	if term == nil || !term.active {
		m.mode = modeNormal
		m.state.Notice = "terminal is stopped"
		return m, nil
	}
	term.writeKey(msg)
	return m, nil
}

func (m model) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	if m.overlay == overlayDetail || m.overlay == overlayCopy {
		switch msg.Type {
		case tea.MouseWheelUp:
			m.detail.LineUp(3)
		case tea.MouseWheelDown:
			m.detail.LineDown(3)
		}
		return m, nil
	}
	// Mouse wheel always scrolls the transcript regardless of focus. That
	// matches what users intuit ("the chat is the primary content; my wheel
	// scrolls the chat") and means scrolling chat doesn't require an extra
	// keypress to switch focus first. If/when the right pane gains a
	// genuinely scrollable surface we can route based on cursor X position.
	switch msg.Type {
	case tea.MouseWheelUp:
		m.chat.LineUp(3)
	case tea.MouseWheelDown:
		m.chat.LineDown(3)
	}
	return m, nil
}

func (m model) handleNormalKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	if m.overlay == overlayDetail || m.overlay == overlayCopy {
		return m.handleDetailOverlayKey(key)
	}
	if m.pendingKey == "g" {
		m.pendingKey = ""
		return m.handlePaneJump(key)
	}
	switch key {
	case "q":
		return m.quit()
	case "esc":
		if m.state.ActiveConversation.Kind == "agent" && m.overlay == overlayNone {
			return m.activateMainConversation()
		}
		m.pendingKey = ""
		m.overlay = overlayNone
		m.mode = modeNormal
		m.composer.Blur()
		m.command.Blur()
		m.palette.Blur()
		return m, nil
	case "g":
		m.pendingKey = "g"
		if m.focus == focusTranscript {
			m.chat.Top()
		}
		return m, nil
	case "ctrl+k", "?":
		return m.openPalette(), nil
	case "ctrl+a":
		if m.focus != focusContext {
			m.state.Notice = "approval shortcuts are available in the right pane"
			return m, nil
		}
		return m.runAction(func(ctx context.Context) (workbench.State, error) {
			return m.controller.SetApproval(ctx, m.state.Approval.Cycle())
		})
	case "b":
		return m.activateMainConversation()
	case "i":
		if m.focus != focusTranscript {
			m.state.Notice = "focus the chat pane to write a prompt"
			return m, nil
		}
		return m.startActiveConversationComposer()
	case ":":
		m.mode = modeCommand
		m.command.Prompt = ":"
		m.command.SetValue("")
		m.command.Focus()
		return m, nil
	case "!":
		m.mode = modeCommand
		m.command.Prompt = "!"
		m.command.SetValue("")
		m.command.Focus()
		return m, nil
	case "enter":
		return m.activateSelected()
	case "o", "e":
		return m.openSelected()
	case "d":
		return m.detailSelected()
	case "y":
		return m.copySelected(false)
	case "Y":
		return m.copySelected(true)
	case "v", "C":
		return m.openCopyMode()
	case "m":
		return m.toggleMouseCapture()
	case "a":
		// Allow approve from any pane: prefer the artifact selected in the
		// right (context) pane, but fall back to the topmost pending
		// approval so the user does not have to remember to focus a
		// different pane just to confirm a write the agent is waiting on.
		if m.focus == focusContext {
			return m.approveSelected()
		}
		if id, ok := m.firstPendingApprovalID(); ok {
			return m.runAction(func(ctx context.Context) (workbench.State, error) {
				return m.controller.Approve(ctx, id)
			})
		}
		m.state.Notice = "no pending approvals"
		return m, nil
	case "r":
		if m.focus == focusContext {
			return m.rejectSelected()
		}
		if id, ok := m.firstPendingApprovalID(); ok {
			return m.runAction(func(ctx context.Context) (workbench.State, error) {
				return m.controller.Reject(ctx, id)
			})
		}
		m.state.Notice = "no pending approvals"
		return m, nil
	case "A":
		// Cycling approval mode is workspace-wide, not pane-bound — no
		// reason to gate it on context pane focus.
		return m.runAction(func(ctx context.Context) (workbench.State, error) {
			return m.controller.SetApproval(ctx, permission.ModeAuto)
		})
	case "1":
		if m.focus != focusContext {
			return m, nil
		}
		m.setRightTab(workbench.RightTabFiles)
		return m, nil
	case "2":
		if m.focus != focusContext {
			return m, nil
		}
		m.setRightTab(workbench.RightTabArtifacts)
		return m, nil
	case "3":
		if m.focus != focusContext {
			return m, nil
		}
		m.setRightTab(workbench.RightTabGit)
		return m, nil
	case "4":
		if m.focus != focusContext {
			return m, nil
		}
		m.setRightTab(workbench.RightTabTerm)
		return m, nil
	case "z":
		if m.focus == focusContext && m.activeRightTab() == workbench.RightTabTerm {
			m.toggleTerminalExpanded()
			return m, nil
		}
		return m, nil
	case "ctrl+h", "ctrl+<":
		return m.movePane(-1), nil
	case "ctrl+l", "ctrl+>":
		return m.movePane(1), nil
	case "tab":
		return m.movePane(1), nil
	case "left":
		return m.handleLocalHorizontal(-1)
	case "right":
		return m.handleLocalHorizontal(1)
	case "h":
		return m.handleLocalHorizontal(-1)
	case "l":
		return m.handleLocalHorizontal(1)
	case "j", "down":
		m.moveSelection(1)
		return m, nil
	case "k", "up":
		m.moveSelection(-1)
		return m, nil
	case "G", "end":
		if m.focus == focusTranscript {
			m.chat.Bottom()
			return m, nil
		}
	case "home":
		if m.focus == focusTranscript {
			m.chat.Top()
			return m, nil
		}
	case "ctrl+f", "pgdown":
		if m.focus == focusTranscript {
			m.chat.PageDown()
			return m, nil
		}
		m.scrollFocused(6)
		return m, nil
	case "ctrl+b", "pgup":
		if m.focus == focusTranscript {
			m.chat.PageUp()
			return m, nil
		}
		m.scrollFocused(-6)
		return m, nil
	case "[", "shift+tab":
		if m.focus != focusContext {
			return m, nil
		}
		m.cycleRightTab(-1)
		return m, nil
	case "]":
		if m.focus != focusContext {
			return m, nil
		}
		m.cycleRightTab(1)
		return m, nil
	}
	return m, nil
}

func (m model) handleInsertKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.mode = modeNormal
		m.composer.Blur()
		return m, nil
	case "enter":
		return m.submit(false)
	case "ctrl+k":
		m.composer.Blur()
		return m.openPalette(), nil
	}
	var cmd tea.Cmd
	m.composer, cmd = m.composer.Update(msg)
	m.sanitizeComposer()
	return m, cmd
}

func (m model) handleCommandKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.mode = modeNormal
		m.command.Blur()
		return m, nil
	case "tab":
		m.completeCommandInput()
		return m, nil
	case "enter":
		line := m.commandLine()
		m.command.Blur()
		return m.executeLine(line)
	}
	var cmd tea.Cmd
	m.command, cmd = m.command.Update(msg)
	if clean := sanitizeTerminalText(m.command.Value()); clean != m.command.Value() {
		m.command.SetValue(clean)
	}
	return m, cmd
}

func (m model) commandLine() string {
	value := strings.TrimSpace(m.command.Value())
	prompt := m.command.Prompt
	switch {
	case prompt == "":
		if strings.HasPrefix(value, ":") || strings.HasPrefix(value, "!") {
			return value
		}
		return ":" + value
	case prompt == ":" && strings.HasPrefix(value, ":"):
		return value
	case prompt == "!" && strings.HasPrefix(value, "!"):
		return value
	default:
		return prompt + value
	}
}

func (m model) handlePaletteKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "ctrl+k":
		m.mode = modeNormal
		m.overlay = overlayNone
		m.palette.Blur()
		return m, nil
	case "up":
		if m.paletteCursor > 0 {
			m.paletteCursor--
		}
		return m, nil
	case "down", "tab":
		commands := m.filteredCommands()
		if m.paletteCursor < len(commands)-1 {
			m.paletteCursor++
		}
		return m, nil
	case "enter":
		commands := m.filteredCommands()
		if len(commands) == 0 {
			return m, nil
		}
		if m.paletteCursor >= len(commands) {
			m.paletteCursor = len(commands) - 1
		}
		if !commands[m.paletteCursor].Enabled {
			m.state.Notice = disabledNotice(commands[m.paletteCursor])
			return m, nil
		}
		m.palette.Blur()
		return m.executeCommand(commands[m.paletteCursor].ID)
	}
	var cmd tea.Cmd
	m.palette, cmd = m.palette.Update(msg)
	if clean := sanitizeTerminalText(m.palette.Value()); clean != m.palette.Value() {
		m.palette.SetValue(clean)
	}
	if m.paletteCursor >= len(m.filteredCommands()) {
		m.paletteCursor = max(0, len(m.filteredCommands())-1)
	}
	return m, cmd
}

func (m model) handleDetailOverlayKey(key string) (tea.Model, tea.Cmd) {
	switch key {
	case "esc", "q":
		m.overlay = overlayNone
		m.mode = modeNormal
		// Restore whatever the user's last mouse-capture preference was,
		// rather than always re-enabling capture and surprising users who
		// turned it off.
		if m.mouseCaptured {
			return m, tea.EnableMouseCellMotion
		}
		return m, tea.DisableMouse
	case "ctrl+f", "pgdown":
		m.detail.ViewDown()
		return m, nil
	case "ctrl+b", "pgup":
		m.detail.ViewUp()
		return m, nil
	case "j", "down":
		m.detail.LineDown(1)
		return m, nil
	case "k", "up":
		m.detail.LineUp(1)
		return m, nil
	case "g", "home":
		m.detail.SetYOffset(0)
		return m, nil
	case "G", "end":
		m.detail.SetYOffset(1 << 20)
		return m, nil
	case "y":
		next, cmd := m.copySelected(false)
		return next, cmd
	case "Y":
		next, cmd := m.copySelected(true)
		return next, cmd
	}
	return m, nil
}

func (m model) handleApprovalKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	if m.pendingKey == "g" {
		m.pendingKey = ""
		if next, cmd, ok := m.handlePaneJumpInApproval(key); ok {
			return next, cmd
		}
	}
	switch key {
	case "esc":
		m.mode = modeNormal
		m.overlay = overlayNone
		return m, nil
	case "ctrl+k", "?":
		return m.openPalette(), nil
	case "a":
		return m.approveSelected()
	case "r":
		return m.rejectSelected()
	case "A":
		return m.runAction(func(ctx context.Context) (workbench.State, error) {
			return m.controller.SetApproval(ctx, permission.ModeAuto)
		})
	case "ctrl+f", "pgdown":
		m.approval.ViewDown()
		return m, nil
	case "ctrl+b", "pgup":
		m.approval.ViewUp()
		return m, nil
	case "g", "home":
		m.approval.SetYOffset(0)
		if key == "g" {
			m.pendingKey = "g"
		}
		return m, nil
	case "G", "end":
		m.approval.SetYOffset(1 << 20)
		return m, nil
	case "d":
		return m.detailSelected()
	case "j", "down":
		m.moveApproval(1)
		return m, nil
	case "k", "up":
		m.moveApproval(-1)
		return m, nil
	}
	return m, nil
}

func (m model) handlePaneJump(key string) (tea.Model, tea.Cmd) {
	switch key {
	case "a":
		m.focus = focusAgents
		m.overlay = overlayNone
		m.mode = modeNormal
		return m, nil
	case "t", "m":
		m.focus = focusTranscript
		m.overlay = overlayNone
		m.mode = modeNormal
		return m, nil
	case "f":
		m.focus = focusContext
		m.overlay = overlayNone
		m.mode = modeNormal
		return m, nil
	case "d":
		m.focus = focusContext
		if len(m.state.Approvals) > 0 {
			m.mode = modeApprove
			m.overlay = overlayApproval
			m.syncApprovalViewport()
			return m, nil
		}
		m.mode = modeNormal
		m.overlay = overlayNone
		if m.state.Detail.ID == "" {
			m.state.Notice = "no active detail"
		}
		return m, nil
	default:
		return m, nil
	}
}

func (m model) movePane(delta int) model {
	m.focus = focusPane((int(m.focus) + delta + 3) % 3)
	return m
}

func (m model) handleLocalHorizontal(delta int) (tea.Model, tea.Cmd) {
	if m.focus == focusContext {
		m.cycleRightTab(delta)
		return m, nil
	}
	if m.focus == focusTranscript {
		m.state.Notice = "use Ctrl+H/Ctrl+L to change panes"
		return m, nil
	}
	if m.focus == focusAgents {
		return m.activateRelativeConversation(delta)
	}
	return m, nil
}

func (m model) handlePaneJumpInApproval(key string) (tea.Model, tea.Cmd, bool) {
	switch key {
	case "a", "t", "m", "f":
		next, cmd := m.handlePaneJump(key)
		return next, cmd, true
	case "d":
		m.focus = focusContext
		m.mode = modeApprove
		m.overlay = overlayApproval
		m.syncApprovalViewport()
		return m, nil, true
	default:
		return m, nil, false
	}
}

func (m model) View() string {
	if m.width <= 0 || m.height <= 0 {
		m.width = 100
		m.height = 30
		m.resize()
	}
	header := m.headerView()
	body := m.bodyView()
	composer := m.composerView()
	notice := m.noticeView()
	view := lipgloss.JoinVertical(lipgloss.Left, header, body, composer, notice)
	view = fitFrame(view, m.width, m.height)
	switch m.overlay {
	case overlayPalette:
		return placeOverlay(view, m.paletteView(), m.width, m.height)
	case overlayApproval:
		return placeOverlay(view, m.approvalView(), m.width, m.height)
	case overlayDetail:
		return placeOverlay(view, m.detailOverlayView(), m.width, m.height)
	case overlayCopy:
		return placeOverlay(view, m.copyOverlayView(), m.width, m.height)
	default:
		return view
	}
}

func (m model) headerView() string {
	conversation := firstNonEmpty(m.state.ActiveConversation.Title, "main")
	if m.state.ActiveConversation.Kind == "agent" {
		conversation = "agent:" + conversation
	}
	status := fmt.Sprintf("freecode  provider:%s  model:%s  branch:%s  chat:%s  tokens:~%d  approval:%s",
		dash(m.state.Provider),
		dash(m.state.Model),
		dash(m.state.Branch),
		dash(conversation),
		m.state.TokenEstimate,
		dash(string(m.state.Approval)),
	)
	if m.busy {
		status += "  " + spinnerGlyph() + " running"
	}
	return headerStyle.Width(m.width).Render(truncate(status, m.width-2))
}

// spinnerGlyph returns a Braille spinner frame indexed by wall-clock time so
// it animates as the 75ms streamTickCmd repeatedly re-renders the view. No
// model state needed — the next render naturally picks the next frame.
func spinnerGlyph() string {
	frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	idx := int(time.Now().UnixMilli()/100) % len(frames)
	return frames[idx]
}

// focusedPaneView renders only the focused pane at full body width. Used
// when mouse capture is released so the user can mouse-select cleanly
// without accidentally grabbing content from other panes. The selected
// pane gets full width so wrapped lines are not cramped.
func (m model) focusedPaneView(width int, height int) string {
	width = max(10, width)
	height = max(1, height)
	switch {
	case m.terminalExpanded():
		return paneStyle(true).Width(width).Height(height).Render(m.expandedTerminalView(width, height))
	case m.focus == focusAgents:
		return paneStyle(true).Width(width).Height(height).Render(m.leftView(width, height))
	case m.focus == focusContext:
		return paneStyle(true).Width(width).Height(height).Render(m.rightView(width, height))
	default:
		return paneStyle(true).Width(width).Height(height).Render(m.centerView(width, height))
	}
}

func (m model) bodyView() string {
	bodyH := m.bodyHeight()
	// When mouse capture is off the user is trying to mouse-select. Render
	// only the focused pane fullscreen so terminal selection is naturally
	// constrained to that pane's content — left/right panes never appear on
	// the screen so the drag rectangle can't accidentally pick up agent
	// names or right-pane file lists. Re-enabling mouse with `m` restores
	// the multi-pane layout.
	if !m.mouseCaptured {
		return m.focusedPaneView(m.width, bodyH)
	}
	leftW, centerW, rightW := m.paneWidths()
	var panes []string
	if leftW > 0 {
		panes = append(panes, paneStyle(m.focus == focusAgents).Width(leftW).Height(bodyH).Render(m.leftView(leftW, bodyH)))
	}
	if m.terminalExpanded() {
		termW := centerW + rightW
		if termW > 0 {
			panes = append(panes, paneStyle(m.focus == focusContext).Width(termW).Height(bodyH).Render(m.expandedTerminalView(termW, bodyH)))
		}
		return lipgloss.JoinHorizontal(lipgloss.Top, panes...)
	}
	if centerW > 0 {
		panes = append(panes, paneStyle(m.focus == focusTranscript).Width(centerW).Height(bodyH).Render(m.centerView(centerW, bodyH)))
	}
	if rightW > 0 {
		panes = append(panes, paneStyle(m.focus == focusContext).Width(rightW).Height(bodyH).Render(m.rightView(rightW, bodyH)))
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, panes...)
}

func (m model) composerView() string {
	width := max(1, m.width-4)
	switch m.mode {
	case modeTerminal:
		return composerStyle.Width(width).Render("TERM  shell owns keys  Esc/Ctrl+G normal  Ctrl+C interrupt  :st share from normal")
	case modeCommand:
		label := "COMMAND"
		if m.command.Prompt == "!" {
			label = "SHELL"
		}
		return composerStyle.Width(width).Render(label + "  Tab complete  Enter run  Esc cancel\n" + m.command.View())
	case modeInsert:
		return composerStyle.Width(width).Render("INSERT  Enter send  Esc normal\n" + m.composer.View())
	default:
		status := "NORMAL  i prompt  : command  ! shell  :term terminal  :st share  Ctrl+K palette  Ctrl+H/L panes  h/l left/right local  j/k select  Enter activate  y copy  v select  m mouse"
		if m.busy {
			status += "  running"
		}
		return composerStyle.Width(width).Render(truncate(status, width))
	}
}

func (m model) noticeView() string {
	notice := strings.TrimSpace(m.state.Notice)
	if !m.state.Editor.Available && (strings.TrimSpace(m.state.Editor.Error) != "" || strings.TrimSpace(m.state.Editor.Command) != "") {
		detail := strings.TrimSpace(m.state.Editor.Error)
		if detail == "" {
			detail = strings.TrimSpace(m.state.Editor.Command)
		}
		notice = "editor unavailable: " + detail
	}
	// Pending approvals get priority over the static cheatsheet so the user
	// always sees the queue and the relevant command shortcuts when there
	// is something to approve.
	if pending := m.pendingApprovalStrip(); pending != "" {
		notice = pending
	}
	if notice == "" {
		notice = "j/k move  Ctrl+H/L panes  h/l left/right local  Enter activate  :s swarm  :! shell  :term terminal  d detail  y copy  v select  m mouse  q quit"
	}
	return noticeStyle.Width(m.width).Render(truncate(notice, m.width-2))
}

func (m model) leftView(width int, height int) string {
	var lines []string
	lines = append(lines, titleStyle.Render("Agents"))
	orchestrator := "main orchestrator"
	if m.state.SessionID != "" {
		orchestrator += "  " + string(m.state.SessionID)
	}
	mainID := "main"
	if m.activeConversationID() == "main" {
		mainID = "*main"
	}
	lines = append(lines, selectableLine(m.leftCursor == 0, mainID, orchestrator))
	if len(m.state.Agents) == 0 {
		lines = append(lines, mutedStyle.Render("agents idle"))
	}
	for rowCursor, row := range m.agentDisplayRows() {
		agent := m.state.Agents[row.index]
		id := agent.ID
		if m.activeConversationID() == agent.ID {
			id = "*" + id
		}
		indent := strings.Repeat("  ", row.depth)
		lines = append(lines, selectableLine(m.leftCursor == rowCursor+1, indent+id, agentRowTitle(agent)))
		for _, line := range agentRowSublines(agent, max(10, width-6), 3) {
			lines = append(lines, mutedStyle.Render(indent+"  "+line))
		}
	}
	return fitLines(lines, width, height)
}

func (m model) agentDisplayRows() []agentDisplayRow {
	if len(m.state.Agents) == 0 {
		return nil
	}
	byID := map[string]int{}
	for i, agent := range m.state.Agents {
		for _, id := range []string{agent.ID, agent.TaskID, agent.Name} {
			if strings.TrimSpace(id) != "" {
				byID[id] = i
			}
		}
	}
	children := map[int][]int{}
	var roots []int
	for i, agent := range m.state.Agents {
		parent := firstNonEmpty(agent.Meta["parent_agent_id"], agent.Meta["parent_task_id"], agent.Meta["parent"])
		parent = strings.TrimSpace(parent)
		parentIndex, ok := byID[parent]
		if parent == "" || parent == "main" || !ok || parentIndex == i {
			roots = append(roots, i)
			continue
		}
		children[parentIndex] = append(children[parentIndex], i)
	}
	var rows []agentDisplayRow
	seen := map[int]bool{}
	var walk func(index int, depth int)
	walk = func(index int, depth int) {
		if seen[index] {
			return
		}
		seen[index] = true
		rows = append(rows, agentDisplayRow{index: index, depth: min(depth, 8)})
		for _, child := range children[index] {
			walk(child, depth+1)
		}
	}
	for _, root := range roots {
		walk(root, 0)
	}
	for i := range m.state.Agents {
		walk(i, 0)
	}
	return rows
}

func (m model) agentAtCursor() (workbench.AgentItem, bool) {
	if m.leftCursor <= 0 {
		return workbench.AgentItem{}, false
	}
	rows := m.agentDisplayRows()
	index := m.leftCursor - 1
	if index < 0 || index >= len(rows) {
		return workbench.AgentItem{}, false
	}
	stateIndex := rows[index].index
	if stateIndex < 0 || stateIndex >= len(m.state.Agents) {
		return workbench.AgentItem{}, false
	}
	return m.state.Agents[stateIndex], true
}

func agentRowTitle(agent workbench.AgentItem) string {
	name := firstNonEmpty(agent.Name, agent.Role, "agent")
	role := strings.TrimSpace(agent.Role)
	status := strings.TrimSpace(agent.Status)
	var parts []string
	parts = append(parts, name)
	if role != "" && role != name {
		parts = append(parts, role)
	}
	if status != "" {
		parts = append(parts, status)
	}
	return strings.Join(parts, " ")
}

func agentRowSublines(agent workbench.AgentItem, width int, limit int) []string {
	candidates := []string{
		prefixedValue("task", agent.TaskID),
		prefixedValue("step", agent.CurrentStep),
		prefixedValue("files", compactList(agent.ChangedFiles, width-len("files "))),
		prefixedValue("tests", compactList(agent.TestsRun, width-len("tests "))),
		prefixedValue("blocked", agent.BlockedReason),
		prefixedValue("findings", compactList(agent.Findings, width-len("findings "))),
		prefixedValue("questions", compactList(agent.Questions, width-len("questions "))),
		prefixedValue("summary", firstNonEmpty(agent.Summary, agent.Text)),
	}
	var lines []string
	for _, candidate := range candidates {
		if strings.TrimSpace(candidate) == "" {
			continue
		}
		lines = append(lines, truncate(candidate, width))
		if limit > 0 && len(lines) >= limit {
			break
		}
	}
	return lines
}

func agentConversationTitle(agent workbench.AgentItem) string {
	return firstNonEmpty(agent.Name, agent.Role, agent.ID, "agent")
}

func (m model) centerView(width int, height int) string {
	m.chat.SetSize(max(10, width-2), max(3, height-2))
	m.chat.SetItems(m.state.Transcript)
	title := firstNonEmpty(m.state.ActiveConversation.Title, "Main orchestrator")
	if m.state.ActiveConversation.Kind == "agent" {
		title = "Agent: " + title
	}
	return titleStyle.Render(title) + "\n" + m.chat.View()
}

func (m model) rightView(width int, height int) string {
	var lines []string
	lines = append(lines, titleStyle.Render(m.rightTabHeader()))
	if m.activeRightTab() == workbench.RightTabTerm {
		content := m.terminalContentView(width, max(0, height-len(lines)))
		if content != "" {
			lines = append(lines, strings.Split(content, "\n")...)
		}
		return fitLines(lines, width, height)
	}
	entries := m.contextEntries()
	if len(entries) == 0 {
		lines = append(lines, mutedStyle.Render(m.emptyRightTabText()))
	}
	available := max(0, height-len(lines))
	entryHeight := 1
	if m.activeRightTab() == workbench.RightTabGit {
		entryHeight = 2
	}
	visibleEntries := max(1, available/entryHeight)
	start, end := visibleRange(len(entries), m.contextCursor, visibleEntries)
	if start > 0 && available > 0 {
		lines = append(lines, mutedStyle.Render("..."))
		available--
	}
	for i := start; i < end; i++ {
		row := m.contextEntryLines(entries[i], width, m.contextCursor == i)
		for _, line := range row {
			if len(lines) >= height {
				break
			}
			lines = append(lines, line)
		}
	}
	if end < len(entries) && len(lines) < height {
		lines = append(lines, mutedStyle.Render("..."))
	}
	return fitLines(lines, width, height)
}

func (m model) expandedTerminalView(width int, height int) string {
	lines := []string{titleStyle.Render("Terminal")}
	content := m.terminalContentView(width, max(0, height-1))
	if content != "" {
		lines = append(lines, strings.Split(content, "\n")...)
	}
	return fitLines(lines, width, height)
}

func (m model) terminalContentView(width int, height int) string {
	if height <= 0 {
		return ""
	}
	if m.term == nil || (!m.term.active && len(m.term.lines) == 0 && m.term.partial == "") {
		term := m.currentTerminal()
		if term != nil {
			m.term = term
		}
	}
	term := m.currentTerminal()
	if term == nil || (!term.active && len(term.lines) == 0 && term.partial == "") {
		return fitLines([]string{
			mutedStyle.Render(fmt.Sprintf("Terminal %d is ready. Press Enter to focus and start it.", m.termSlot+1)),
			mutedStyle.Render("Run :t to create another terminal, or :t N to select one."),
			mutedStyle.Render("Terminal output is local-only until :st or :share-term."),
		}, width, height)
	}
	bodyHeight := max(1, height-1)
	shared := ""
	if m.terminalShared && m.sharedTermSlot == m.termSlot {
		shared = "  shared"
	}
	status := fmt.Sprintf("term %d/%d%s  Esc/Ctrl+G returns to FreeCode", m.termSlot+1, max(1, len(m.terms)), shared)
	if m.mode != modeTerminal {
		status = fmt.Sprintf("term %d/%d%s  Enter focus  z expand  :st share with agent  :st off revoke", m.termSlot+1, max(1, len(m.terms)), shared)
	}
	lines := []string{mutedStyle.Render(truncate(status, max(1, width-1)))}
	lines = append(lines, strings.Split(term.view(width, bodyHeight), "\n")...)
	return fitLines(lines, width, height)
}

func (m model) contextEntryLines(entry contextEntry, width int, active bool) []string {
	prefix := entry.id
	if entry.approval {
		prefix = "!" + entry.id
	}
	switch entry.kind {
	case "folder":
		marker := "[+]"
		if entry.expanded {
			marker = "[-]"
		}
		indent := strings.Repeat("  ", entry.depth)
		head := fmt.Sprintf("%s%s %s/", indent, marker, entry.name)
		if active {
			head = selectedStyle.Render(truncate(head, max(1, width-2)))
		}
		return []string{head}
	case "file", "git":
		name := firstNonEmpty(entry.name, basePath(entry.path), entry.label, entry.id)
		status := firstNonEmpty(entry.status, entry.kind)
		indent := strings.Repeat("  ", entry.depth)
		head := fmt.Sprintf("%s%s  %-8s %s", indent, prefix, status, name)
		if entry.kind == "file" {
			if active {
				head = selectedStyle.Render(truncate(head, max(1, width-2)))
			}
			return []string{head}
		}
		path := parentPath(entry.path)
		if path == "" {
			path = entry.path
		}
		if path == name || path == "." {
			path = ""
		}
		if active {
			head = selectedStyle.Render(truncate(head, max(1, width-2)))
		}
		if path == "" {
			return []string{head}
		}
		return []string{
			head,
			mutedStyle.Render("    " + compactPath(path, max(8, width-6))),
		}
	default:
		return []string{selectableLine(active, prefix, entry.label)}
	}
}

func (m model) rightTabHeader() string {
	tab := m.activeRightTab()
	labels := []struct {
		tab   workbench.RightTab
		label string
	}{
		{workbench.RightTabFiles, "Files"},
		{workbench.RightTabArtifacts, "Artifacts"},
		{workbench.RightTabGit, "Git"},
		{workbench.RightTabTerm, "Term"},
	}
	var parts []string
	for _, item := range labels {
		label := item.label
		if count := m.rightTabCount(item.tab); count > 0 {
			label = fmt.Sprintf("%s(%d)", label, count)
		}
		if tab == item.tab {
			parts = append(parts, "["+label+"]")
			continue
		}
		parts = append(parts, label)
	}
	return strings.Join(parts, " ")
}

func (m model) rightTabCount(tab workbench.RightTab) int {
	switch tab {
	case workbench.RightTabFiles:
		return len(m.state.Files)
	case workbench.RightTabGit:
		return len(m.state.GitFiles)
	case workbench.RightTabTerm:
		if len(m.terms) > 0 {
			return len(m.terms)
		}
		if m.term != nil {
			return 1
		}
		return 0
	default:
		return len(m.state.Approvals) + len(m.state.Artifacts)
	}
}

func (m model) emptyRightTabText() string {
	switch m.activeRightTab() {
	case workbench.RightTabFiles:
		return "No workspace files"
	case workbench.RightTabGit:
		return "No git changes"
	case workbench.RightTabTerm:
		return "Terminal is stopped. Run :term to start it."
	default:
		return "No approvals or artifacts"
	}
}

func (m model) paletteView() string {
	commands := m.filteredCommands()
	width := m.modalContentWidth(82)
	maxRows := max(3, (m.height-9)/2)
	start, end := visibleRange(len(commands), m.paletteCursor, maxRows)
	var lines []string
	lines = append(lines, titleStyle.Render("Command Palette"))
	lines = append(lines, m.palette.View())
	if strings.TrimSpace(m.palette.Value()) == "" {
		lines = append(lines, mutedStyle.Render("Search by command, shortcut, category, or keyword. Enter runs the selected command."))
	}
	if len(commands) == 0 {
		lines = append(lines, mutedStyle.Render("No commands found"))
	} else {
		if start > 0 {
			lines = append(lines, mutedStyle.Render("..."))
		}
		for i := start; i < end; i++ {
			command := commands[i]
			key := command.Keybinding
			if key == "" {
				key = command.Key
			}
			state := ""
			if !command.Enabled {
				state = "disabled"
			}
			row := fmt.Sprintf("%-12s %-14s %s", key, command.Category, command.Title)
			if state != "" {
				row += "  " + state
			}
			if command.DisabledReason != "" {
				row += "  " + command.DisabledReason
			}
			lines = append(lines, selectableLine(m.paletteCursor == i, "", row))
			if command.Description != "" {
				lines = append(lines, mutedStyle.Render("  "+truncate(command.Description, max(10, width-4))))
			}
		}
		if end < len(commands) {
			lines = append(lines, mutedStyle.Render("..."))
		}
	}
	return modalStyle.Width(width).Render(strings.Join(lines, "\n"))
}

func (m model) detailOverlayView() string {
	width := m.modalContentWidth(96)
	height := max(8, min(max(8, m.height-6), 30))
	m.detail.Width = max(10, width-4)
	m.detail.Height = max(3, height-5)
	m.syncDetailViewport()
	title := strings.TrimSpace(strings.Join([]string{m.state.Detail.ID, m.state.Detail.Title}, " "))
	if title == "" {
		title = "Detail"
	}
	lines := []string{
		titleStyle.Render(title),
		mutedStyle.Render("j/k or Ctrl+F/B scroll  Esc close  y copies selected item"),
		"",
		m.detail.View(),
	}
	return modalStyle.Width(width).Render(strings.Join(lines, "\n"))
}

func (m model) copyOverlayView() string {
	width := m.modalContentWidth(96)
	height := max(8, min(max(8, m.height-6), 30))
	m.detail.Width = max(10, width-4)
	m.detail.Height = max(3, height-6)
	lines := []string{
		titleStyle.Render("Copy Selection"),
		mutedStyle.Render("Mouse reporting is off here. Select text with the terminal, press Cmd+C, then Esc."),
		mutedStyle.Render("Use y/Y outside this overlay for direct clipboard copy."),
		"",
		m.detail.View(),
	}
	return modalStyle.Width(width).Render(strings.Join(lines, "\n"))
}

func (m model) approvalView() string {
	approval, ok := m.selectedApproval()
	if !ok {
		return modalStyle.Width(m.modalContentWidth(72)).Render("No pending approvals")
	}
	width := m.modalContentWidth(96)
	height := max(8, min(max(8, m.height-6), 28))
	m.approval.Width = max(10, width-2)
	m.approval.Height = max(3, height-6)
	m.approval.SetContent(m.approvalContent(approval))
	lines := []string{
		titleStyle.Render("Approval Required"),
		fmt.Sprintf("%s of %d  %s  %s", approval.ID, len(m.state.Approvals), approval.Action, approval.Title),
		approvalSubjectLine(approval),
		mutedStyle.Render("a approve/apply  r reject  A auto approval  d detail  Ctrl+F/B scroll  Esc close"),
		"",
	}
	lines = append(lines, m.approval.View())
	return modalStyle.Width(width).Render(strings.Join(lines, "\n"))
}

func approvalSubjectLine(approval workbench.ApprovalItem) string {
	subject := sanitizeTerminalText(approval.Subject)
	if strings.TrimSpace(subject) == "" {
		return "subject:-"
	}
	if approval.Action == string(permission.ActionWrite) {
		return "changed files: " + subject
	}
	return "subject: " + subject
}

func (m model) openPalette() model {
	m.mode = modePalette
	m.overlay = overlayPalette
	m.palette.SetValue("")
	m.palette.Focus()
	m.paletteCursor = 0
	return m
}

func (m model) executeCommand(id string) (tea.Model, tea.Cmd) {
	m.mode = modeNormal
	m.overlay = overlayNone
	switch id {
	case "prompt.insert":
		return m.startActiveConversationComposer()
	case "prompt.send":
		return m.submit(false)
	case "conversation.main":
		return m.activateMainConversation()
	case "agent.followup":
		return m.startAgentFollowup()
	case "agent.swarm":
		m.state.Notice = "usage: :s <task> or :swarm <task>"
		return m, nil
	case "shell.run":
		m.mode = modeCommand
		m.overlay = overlayNone
		m.command.Prompt = "!"
		m.command.SetValue("")
		m.command.Focus()
		return m, nil
	case "terminal.open", "tab.term":
		return m.openNewTerminal(false)
	case "terminal.share":
		return m.shareTerminalDirect("")
	case "item.open":
		return m.openSelected()
	case "item.detail":
		return m.detailSelected()
	case "item.copy":
		return m.copySelected(false)
	case "item.copy.full":
		return m.copySelected(true)
	case "item.copy.select":
		return m.openCopyMode()
	case "mouse.toggle":
		return m.toggleMouseCapture()
	case "approval.approve":
		return m.approveSelected()
	case "approval.reject":
		return m.rejectSelected()
	case "approval.cycle":
		return m.runAction(func(ctx context.Context) (workbench.State, error) {
			return m.controller.SetApproval(ctx, m.state.Approval.Cycle())
		})
	case "approval.auto":
		return m.runAction(func(ctx context.Context) (workbench.State, error) {
			return m.controller.SetApproval(ctx, permission.ModeAuto)
		})
	case "approval.danger":
		m.state.Notice = "type :danger confirm to enable danger mode"
		return m, nil
	case "context.compact":
		return m.runAction(m.controller.Compact)
	case "palette.open":
		return m.openPalette(), nil
	case "session.list", "workspace.sessions":
		return m.showSessions()
	case "session.new", "workspace.new":
		return m.newSession("")
	case "session.rename", "workspace.rename":
		m.state.Notice = "usage: :rename <title>"
		return m, nil
	case "session.resume", "workspace.resume":
		m.state.Notice = "usage: :resume <id>"
		return m, nil
	case "tab.files", "workspace.files":
		m.setRightTab(workbench.RightTabFiles)
		return m, nil
	case "tab.artifacts", "workspace.artifacts":
		m.setRightTab(workbench.RightTabArtifacts)
		return m, nil
	case "tab.git", "workspace.git":
		m.setRightTab(workbench.RightTabGit)
		return m, nil
	case "workspace.term":
		return m.openNewTerminal(false)
	case "file.edit":
		m.state.Notice = "usage: :edit <file>"
		return m, nil
	case "settings.open", "workspace.settings":
		return m.showSettings()
	case "quit":
		return m.quit()
	default:
		m.state.Notice = "unknown command " + id
		return m, nil
	}
}

func (m model) executeLine(line string) (tea.Model, tea.Cmd) {
	line = strings.TrimSpace(line)
	if line == ":" || line == "" {
		m.mode = modeNormal
		return m, nil
	}
	switch {
	case line == ":q" || line == "q" || line == "quit":
		return m.quit()
	case strings.HasPrefix(line, "!"):
		return m.runShell(strings.TrimSpace(strings.TrimPrefix(line, "!")))
	case strings.HasPrefix(line, ":!"):
		return m.runShell(strings.TrimSpace(strings.TrimPrefix(line, ":!")))
	case line == ":help" || line == "?" || line == ":palette":
		return m.openPalette(), nil
	case line == ":main" || line == ":back":
		return m.activateMainConversation()
	case line == ":ls" || line == ":buffers":
		return m.showBuffers()
	case strings.HasPrefix(line, ":b "):
		return m.activateConversationRef(strings.TrimSpace(strings.TrimPrefix(line, ":b ")))
	case line == ":bn" || line == ":bnext":
		return m.activateRelativeConversation(1)
	case line == ":bp" || line == ":bprevious":
		return m.activateRelativeConversation(-1)
	case line == ":sessions":
		return m.showSessions()
	case line == ":new" || strings.HasPrefix(line, ":new "):
		return m.newSession(strings.TrimSpace(strings.TrimPrefix(line, ":new")))
	case strings.HasPrefix(line, ":rename "):
		return m.renameSession(strings.TrimSpace(strings.TrimPrefix(line, ":rename ")))
	case strings.HasPrefix(line, ":resume "):
		return m.resumeSession(strings.TrimSpace(strings.TrimPrefix(line, ":resume ")))
	case line == ":files":
		m.setRightTab(workbench.RightTabFiles)
		return m, nil
	case line == ":artifacts":
		m.setRightTab(workbench.RightTabArtifacts)
		return m, nil
	case line == ":git":
		m.setRightTab(workbench.RightTabGit)
		return m, nil
	case line == ":term" || line == ":terminal" || line == ":t":
		return m.openNewTerminal(false)
	case strings.HasPrefix(line, ":term "):
		return m.openTerminalSlot(parseTerminalSlot(strings.TrimSpace(strings.TrimPrefix(line, ":term "))), false)
	case strings.HasPrefix(line, ":terminal "):
		return m.openTerminalSlot(parseTerminalSlot(strings.TrimSpace(strings.TrimPrefix(line, ":terminal "))), false)
	case strings.HasPrefix(line, ":t "):
		return m.openTerminalSlot(parseTerminalSlot(strings.TrimSpace(strings.TrimPrefix(line, ":t "))), false)
	case line == ":term!" || line == ":terminal!" || line == ":t!":
		return m.focusTerminal()
	case strings.HasPrefix(line, ":term! "):
		return m.openTerminalSlot(parseTerminalSlot(strings.TrimSpace(strings.TrimPrefix(line, ":term! "))), true)
	case strings.HasPrefix(line, ":terminal! "):
		return m.openTerminalSlot(parseTerminalSlot(strings.TrimSpace(strings.TrimPrefix(line, ":terminal! "))), true)
	case strings.HasPrefix(line, ":t! "):
		return m.openTerminalSlot(parseTerminalSlot(strings.TrimSpace(strings.TrimPrefix(line, ":t! "))), true)
	case line == ":share-term" || line == ":share-terminal" || line == ":st":
		return m.shareTerminalDirect("")
	case strings.HasPrefix(line, ":share-term "):
		return m.shareTerminalDirect(strings.TrimSpace(strings.TrimPrefix(line, ":share-term ")))
	case strings.HasPrefix(line, ":share-terminal "):
		return m.shareTerminalDirect(strings.TrimSpace(strings.TrimPrefix(line, ":share-terminal ")))
	case strings.HasPrefix(line, ":st "):
		return m.shareTerminalDirect(strings.TrimSpace(strings.TrimPrefix(line, ":st ")))
	case line == ":tabn" || line == ":tabnext" || line == ":tn":
		m.cycleRightTab(1)
		return m, nil
	case line == ":tabp" || line == ":tabprevious" || line == ":tp":
		m.cycleRightTab(-1)
		return m, nil
	case line == ":Explore" || line == ":Ex":
		m.setRightTab(workbench.RightTabFiles)
		return m, nil
	case strings.HasPrefix(line, ":edit "):
		return m.editFile(strings.TrimSpace(strings.TrimPrefix(line, ":edit ")))
	case strings.HasPrefix(line, ":e "):
		return m.editFile(strings.TrimSpace(strings.TrimPrefix(line, ":e ")))
	case line == ":settings":
		return m.showSettings()
	case line == ":compact" || line == "/compact":
		return m.runAction(m.controller.Compact)
	case line == ":approval auto":
		return m.runAction(func(ctx context.Context) (workbench.State, error) {
			return m.controller.SetApproval(ctx, permission.ModeAuto)
		})
	case line == ":approval ask":
		return m.runAction(func(ctx context.Context) (workbench.State, error) {
			return m.controller.SetApproval(ctx, permission.ModeAsk)
		})
	case line == ":approval read-only":
		return m.runAction(func(ctx context.Context) (workbench.State, error) {
			return m.controller.SetApproval(ctx, permission.ModeReadOnly)
		})
	case line == ":danger" || line == ":approval danger":
		m.mode = modeNormal
		m.state.Notice = "type :danger confirm to enable danger mode"
		return m, nil
	case line == ":danger confirm":
		return m.runAction(func(ctx context.Context) (workbench.State, error) {
			return m.controller.SetApproval(ctx, permission.ModeDanger)
		})
	case line == ":w":
		m.state.Notice = "external Neovim owns file writes"
		return m, nil
	case line == ":qa":
		return m.quit()
	case strings.HasPrefix(line, ":i "):
		m.composer.SetValue(strings.TrimSpace(strings.TrimPrefix(line, ":i ")))
		return m.submit(false)
	case strings.HasPrefix(line, ":send "):
		m.composer.SetValue(strings.TrimSpace(strings.TrimPrefix(line, ":send ")))
		return m.submit(false)
	case strings.HasPrefix(line, ":swarm "):
		m.composer.SetValue(strings.TrimSpace(strings.TrimPrefix(line, ":swarm ")))
		return m.submit(true)
	case strings.HasPrefix(line, ":s "):
		m.composer.SetValue(strings.TrimSpace(strings.TrimPrefix(line, ":s ")))
		return m.submit(true)
	case strings.HasPrefix(line, ":agent "):
		m.composer.SetValue(strings.TrimSpace(strings.TrimPrefix(line, ":agent ")))
		return m.submit(false)
	case line == ":o":
		return m.openSelected()
	case strings.HasPrefix(line, ":o "):
		return m.openRef(strings.TrimSpace(strings.TrimPrefix(line, ":o ")))
	case strings.HasPrefix(line, ":d "):
		return m.detailRef(strings.TrimSpace(strings.TrimPrefix(line, ":d ")))
	case strings.HasPrefix(line, ":y "):
		return m.copyRef(strings.TrimSpace(strings.TrimPrefix(line, ":y ")), false)
	case strings.HasPrefix(line, ":Y "):
		return m.copyRef(strings.TrimSpace(strings.TrimPrefix(line, ":Y ")), true)
	case strings.HasPrefix(line, ":a "):
		return m.approveRef(strings.TrimSpace(strings.TrimPrefix(line, ":a ")))
	case strings.HasPrefix(line, ":r "):
		return m.rejectRef(strings.TrimSpace(strings.TrimPrefix(line, ":r ")))
	case line == ":A" || line == ":a all" || line == ":a *":
		return m.approveRef("all")
	case line == ":R" || line == ":r all" || line == ":r *":
		return m.rejectRef("all")
	case line == ":debug" || line == ":debug toggle":
		return m.toggleDebugMode()
	case line == ":debug on":
		return m.setDebugMode(true)
	case line == ":debug off":
		return m.setDebugMode(false)
	default:
		m.mode = modeNormal
		m.state.Notice = "unknown command " + line
		return m, nil
	}
}

func (m model) showSessions() (tea.Model, tea.Cmd) {
	var lines []string
	if len(m.state.Sessions) == 0 {
		lines = append(lines, "No sessions")
	}
	for _, summary := range m.state.Sessions {
		title := firstNonEmpty(summary.Title, string(summary.ID))
		line := strings.TrimSpace(fmt.Sprintf("%s  %s  %s", summary.ID, title, summary.Branch))
		lines = append(lines, line)
	}
	m.state.Detail = workbench.Item{ID: "sessions", Kind: "sessions", Title: "Sessions", Body: strings.Join(lines, "\n")}
	m.state.Notice = "sessions listed"
	m.focus = focusContext
	m.overlay = overlayDetail
	m.detail.SetYOffset(0)
	m.syncDetailViewport()
	return m, nil
}

func (m model) showSettings() (tea.Model, tea.Cmd) {
	lines := []string{
		"provider: " + dash(m.state.Provider),
		"model: " + dash(m.state.Model),
		"branch: " + dash(m.state.Branch),
		"approval: " + dash(string(m.state.Approval)),
		"workspace: " + dash(m.state.WorkspaceRoot),
	}
	if m.state.Editor.Command != "" || m.state.Editor.Error != "" {
		lines = append(lines, "editor: external "+dash(firstNonEmpty(m.state.Editor.Command, m.state.Editor.Error)))
	}
	m.state.Detail = workbench.Item{ID: "settings", Kind: "settings", Title: "Settings", Body: strings.Join(lines, "\n")}
	m.state.Notice = "settings"
	m.focus = focusContext
	m.overlay = overlayDetail
	m.detail.SetYOffset(0)
	m.syncDetailViewport()
	return m, nil
}

func (m model) showBuffers() (tea.Model, tea.Cmd) {
	lines := []string{"Conversations:"}
	mainMarker := " "
	if m.state.ActiveConversation.Kind != "agent" {
		mainMarker = "%"
	}
	lines = append(lines, fmt.Sprintf("%s main  Main orchestrator", mainMarker))
	for _, agent := range m.state.Agents {
		marker := " "
		if m.state.ActiveConversation.Kind == "agent" && m.state.ActiveConversation.ID == agent.ID {
			marker = "%"
		}
		lines = append(lines, fmt.Sprintf("%s %-5s %s", marker, agent.ID, agentRowTitle(agent)))
	}
	lines = append(lines, "", "Use :b main, :b a1, :bn, or :bp")
	m.detail.SetContent(strings.Join(lines, "\n"))
	m.detail.SetYOffset(0)
	m.overlay = overlayDetail
	m.focus = focusTranscript
	return m, nil
}

func (m model) newSession(title string) (tea.Model, tea.Cmd) {
	creator, ok := m.controller.(sessionCreator)
	if !ok {
		m.mode = modeNormal
		m.state.Notice = "new session is unavailable"
		return m, nil
	}
	return m.runAction(func(ctx context.Context) (workbench.State, error) {
		return creator.NewSession(ctx, title)
	})
}

func (m model) renameSession(title string) (tea.Model, tea.Cmd) {
	if title == "" {
		m.mode = modeNormal
		m.state.Notice = "rename requires a title"
		return m, nil
	}
	renamer, ok := m.controller.(sessionRenamer)
	if !ok {
		m.mode = modeNormal
		m.state.Notice = "rename session is unavailable"
		return m, nil
	}
	return m.runAction(func(ctx context.Context) (workbench.State, error) {
		return renamer.RenameSession(ctx, title)
	})
}

func (m model) resumeSession(id string) (tea.Model, tea.Cmd) {
	if id == "" {
		m.mode = modeNormal
		m.state.Notice = "resume requires a session id"
		return m, nil
	}
	resumer, ok := m.controller.(sessionResumer)
	if !ok {
		m.mode = modeNormal
		m.state.Notice = "resume session is unavailable"
		return m, nil
	}
	return m.runAction(func(ctx context.Context) (workbench.State, error) {
		return resumer.ResumeSession(ctx, session.ID(id))
	})
}

func (m model) editFile(ref string) (tea.Model, tea.Cmd) {
	if ref == "" {
		m.mode = modeNormal
		m.state.Notice = "edit requires a file"
		return m, nil
	}
	id, line := workbench.SplitArtifactLine(ref)
	if file, ok := m.findWorkspaceFile(id); ok {
		return m.openPath(file.Path, line, firstNonEmpty(file.ID, file.Path))
	}
	return m.openRef(ref)
}

func (m model) runShell(command string) (tea.Model, tea.Cmd) {
	command = strings.TrimSpace(command)
	if command == "" {
		m.mode = modeNormal
		m.state.Notice = "shell command is empty"
		return m, nil
	}
	runner, ok := m.controller.(shellRunner)
	if !ok {
		m.mode = modeNormal
		m.state.Notice = "shell command is unavailable"
		return m, nil
	}
	return m.runAction(func(ctx context.Context) (workbench.State, error) {
		return runner.RunShell(ctx, command)
	})
}

func (m model) openNewTerminal(focus bool) (tea.Model, tea.Cmd) {
	m.ensureTerminalList()
	term := newTerminalSession()
	m.terms = append(m.terms, term)
	m.termSlot = len(m.terms) - 1
	m.term = term
	return m.startTerminal(false, focus)
}

func (m model) openTerminalSlot(slot int, focus bool) (tea.Model, tea.Cmd) {
	m.ensureTerminalList()
	if slot <= 0 {
		m.mode = modeNormal
		m.state.Notice = "terminal number is invalid"
		return m, nil
	}
	if slot > len(m.terms) {
		m.mode = modeNormal
		m.state.Notice = fmt.Sprintf("terminal %d does not exist", slot)
		return m, nil
	}
	m.termSlot = slot - 1
	m.term = m.terms[m.termSlot]
	return m.startTerminal(false, focus)
}

func (m model) startTerminal(expand bool, focus bool) (tea.Model, tea.Cmd) {
	term := m.currentTerminal()
	if term == nil {
		return m.openNewTerminal(focus)
	}
	m.setRightTab(workbench.RightTabTerm)
	term.expanded = expand
	m.mode = modeNormal
	if focus {
		m.mode = modeTerminal
	}
	m.overlay = overlayNone
	width, height := m.terminalPTYSize()
	if !focus {
		if term.active {
			term.resize(width, height)
		}
		m.state.Notice = fmt.Sprintf("terminal %d opened; Enter focuses input", m.termSlot+1)
		return m, nil
	}
	if err := term.start(m.state.WorkspaceRoot, width, height); err != nil {
		m.mode = modeNormal
		m.state.Notice = "terminal error: " + err.Error()
		return m, nil
	}
	term.resize(width, height)
	m.state.Notice = fmt.Sprintf("terminal %d focused; Esc or Ctrl+G returns", m.termSlot+1)
	return m, m.terminalReadCmd()
}

func (m model) focusTerminal() (tea.Model, tea.Cmd) {
	m.ensureTerminalList()
	if len(m.terms) == 0 {
		return m.openNewTerminal(true)
	}
	m.term = m.terms[clamp(m.termSlot, 0, len(m.terms)-1)]
	return m.startTerminal(false, true)
}

func (m model) quit() (tea.Model, tea.Cmd) {
	m.ensureTerminalList()
	for _, term := range m.terms {
		term.stop()
	}
	return m, tea.Quit
}

func (m model) shareTerminalDirect(value string) (tea.Model, tea.Cmd) {
	value = strings.TrimSpace(strings.ToLower(value))
	switch value {
	case "off", "disable", "disabled", "none":
		m.terminalShared = false
		m.sharedTermSlot = -1
		m.state.Notice = "terminal sharing disabled"
		return m, nil
	case "status":
		if term, slot := m.sharedTerminal(); term != nil {
			m.state.Notice = fmt.Sprintf("terminal %d is shared with agent tools", slot+1)
			return m, nil
		}
		m.state.Notice = "no terminal is shared"
		return m, nil
	}

	slot := m.termSlot + 1
	if value != "" {
		slot = parseTerminalSlot(value)
		if slot <= 0 {
			m.state.Notice = "usage: :st [terminal-number|off|status]"
			return m, nil
		}
	}
	m.ensureTerminalList()
	if len(m.terms) == 0 && slot == 1 {
		term := newTerminalSession()
		m.terms = append(m.terms, term)
		m.term = term
		m.termSlot = 0
	} else if slot > len(m.terms) {
		m.state.Notice = fmt.Sprintf("terminal %d does not exist", slot)
		return m, nil
	} else {
		m.termSlot = slot - 1
		m.term = m.terms[m.termSlot]
	}
	m.setRightTab(workbench.RightTabTerm)
	term := m.currentTerminal()
	width, height := m.terminalPTYSize()
	if !term.active {
		if err := term.start(m.state.WorkspaceRoot, width, height); err != nil {
			m.state.Notice = "terminal share error: " + err.Error()
			return m, nil
		}
	}
	term.resize(width, height)
	m.terminalShared = true
	m.sharedTermSlot = m.termSlot
	m.state.Notice = fmt.Sprintf("terminal %d shared with agent tools", m.termSlot+1)
	return m, m.terminalReadCmdFor(term)
}

func (m model) shareTerminal(limit int) (tea.Model, tea.Cmd) {
	term := m.currentTerminal()
	if term == nil {
		m.state.Notice = "terminal has no output"
		return m, nil
	}
	body := term.tail(limit)
	if strings.TrimSpace(body) == "" {
		m.state.Notice = "terminal has no output"
		return m, nil
	}
	sharer, ok := m.controller.(terminalSharer)
	if !ok {
		m.state.Notice = "terminal sharing is unavailable"
		return m, nil
	}
	title := "Shared terminal output"
	if limit > 0 {
		title = fmt.Sprintf("Shared terminal output (%d lines)", limit)
	}
	m.detailPending = false
	return m.runAction(func(ctx context.Context) (workbench.State, error) {
		return sharer.ShareTerminal(ctx, title, body)
	})
}

func (m *model) toggleTerminalExpanded() {
	term := m.currentTerminal()
	if term == nil {
		m.ensureTerminalList()
		if len(m.terms) == 0 {
			term = newTerminalSession()
			m.terms = append(m.terms, term)
			m.term = term
			m.termSlot = 0
		}
	}
	term = m.currentTerminal()
	term.expanded = !term.expanded
	m.resizeTerminal()
	if term.expanded {
		m.state.Notice = "terminal expanded"
		return
	}
	m.state.Notice = "terminal docked"
}

func (m model) terminalExpanded() bool {
	term := m.currentTerminal()
	return term != nil && term.expanded && m.activeRightTab() == workbench.RightTabTerm
}

func (m model) terminalReadCmd() tea.Cmd {
	term := m.currentTerminal()
	return m.terminalReadCmdFor(term)
}

func (m model) terminalReadCmdFor(term *terminalSession) tea.Cmd {
	if term == nil || !term.active || term.reading || term.output == nil {
		return nil
	}
	term.reading = true
	return func() tea.Msg {
		msg, ok := <-term.output
		if !ok {
			return terminalOutputMsg{term: term, err: io.EOF}
		}
		msg.term = term
		return msg
	}
}

func (m model) terminalPTYSize() (int, int) {
	bodyH := m.bodyHeight()
	_, centerW, rightW := m.paneWidths()
	if m.terminalExpanded() {
		return max(20, centerW+rightW-4), max(4, bodyH-4)
	}
	return max(20, rightW-4), max(4, bodyH-4)
}

func (m model) resizeTerminal() {
	term := m.currentTerminal()
	if term == nil || !term.active {
		return
	}
	width, height := m.terminalPTYSize()
	term.resize(width, height)
}

func (m *model) ensureTerminalList() {
	if len(m.terms) == 0 && m.term != nil {
		m.terms = []*terminalSession{m.term}
		m.termSlot = 0
	}
	if len(m.terms) > 0 {
		m.termSlot = clamp(m.termSlot, 0, len(m.terms)-1)
		m.term = m.terms[m.termSlot]
	}
}

func (m model) currentTerminal() *terminalSession {
	if len(m.terms) > 0 {
		index := clamp(m.termSlot, 0, len(m.terms)-1)
		return m.terms[index]
	}
	return m.term
}

func (m model) sharedTerminal() (*terminalSession, int) {
	if !m.terminalShared || m.sharedTermSlot < 0 {
		return nil, -1
	}
	if len(m.terms) > 0 {
		if m.sharedTermSlot >= len(m.terms) {
			return nil, -1
		}
		return m.terms[m.sharedTermSlot], m.sharedTermSlot
	}
	if m.sharedTermSlot == 0 {
		return m.term, 0
	}
	return nil, -1
}

func sharedTerminalAccessContext(term *terminalSession, slot int) string {
	if term == nil {
		return ""
	}
	tail := term.tail(120)
	if strings.TrimSpace(tail) == "" {
		tail = "(terminal has no visible output yet)"
	}
	return fmt.Sprintf("Terminal %d is directly shared with the agent for this turn. The terminal_read and terminal_write tools are available. If the user asks whether you can see the terminal, call terminal_read. If the user asks you to run a command in it, call terminal_write and then terminal_read; do not answer in prose without using the terminal tools.\nCurrent terminal tail:\n%s", slot, tail)
}

func parseTerminalShareLimit(value string) int {
	value = strings.TrimSpace(strings.ToLower(value))
	if value == "" || value == "tail" || value == "recent" {
		return 200
	}
	if value == "all" {
		return 0
	}
	if parsed, err := strconv.Atoi(value); err == nil && parsed > 0 {
		return parsed
	}
	return 200
}

func parseTerminalSlot(value string) int {
	if parsed, err := strconv.Atoi(strings.TrimSpace(value)); err == nil && parsed > 0 {
		return parsed
	}
	return -1
}

func (m model) findWorkspaceFile(ref string) (workbench.WorkspaceFile, bool) {
	ref = strings.TrimSpace(ref)
	for _, file := range append(append([]workbench.WorkspaceFile(nil), m.state.Files...), m.state.GitFiles...) {
		if ref == file.ID || ref == file.Path || ref == file.Name {
			return file, true
		}
	}
	return workbench.WorkspaceFile{}, false
}

func (m model) submit(swarm bool) (tea.Model, tea.Cmd) {
	if m.busy {
		m.state.Notice = "agent is still running; wait for it to finish before sending another prompt"
		return m, nil
	}
	m.sanitizeComposer()
	text := strings.TrimSpace(sanitizeTerminalText(m.composer.Value()))
	if text == "" {
		m.mode = modeInsert
		m.composer.Focus()
		m.state.Notice = "prompt is empty"
		return m, nil
	}
	if !swarm && composerTextIsCommand(text) {
		m.composer.SetValue("")
		m.composer.Blur()
		m.mode = modeNormal
		return m.executeLine(text)
	}
	target := m.state.ActiveConversation
	if swarm {
		target = workbench.ConversationTarget{Kind: "main", ID: "main", Title: "Main orchestrator"}
		m.state.ActiveConversation = target
	}
	request := workbench.SubmitRequest{Text: text, Approval: m.state.Approval, Swarm: swarm, Target: target}
	if term, slot := m.sharedTerminal(); term != nil {
		request.TerminalTools = newTerminalToolRegistry(term, slot+1)
		request.TurnContext = sharedTerminalAccessContext(term, slot+1)
	}
	m.composer.SetValue("")
	m.composer.Blur()
	m.mode = modeNormal
	m.overlay = overlayNone
	m = m.startRun()
	m.followTranscript = true
	runID := m.activeRun
	return m, tea.Batch(m.actionCmd(runID, func(ctx context.Context) (workbench.State, error) {
		return m.controller.SubmitPrompt(ctx, request)
	}), streamTickCmd(runID))
}

func composerTextIsCommand(text string) bool {
	text = strings.TrimSpace(text)
	if strings.HasPrefix(text, ":") {
		return true
	}
	if strings.HasPrefix(text, "!") {
		return len(text) == 1 || !strings.HasPrefix(text, "!=")
	}
	return false
}

func (m model) openSelected() (tea.Model, tea.Cmd) {
	if m.focus == focusAgents {
		return m.activateSelectedConversation()
	}
	if m.focus == focusContext {
		if m.activeRightTab() == workbench.RightTabTerm {
			return m.focusTerminal()
		}
		entry, ok := m.selectedContextEntry()
		if !ok {
			m.state.Notice = "nothing selected"
			return m, nil
		}
		switch entry.kind {
		case "folder":
			m.toggleFolder(entry.path)
			return m, nil
		case "file":
			return m.openPath(entry.path, entry.line, firstNonEmpty(entry.id, entry.path))
		case "git":
			return m.openPath(entry.path, entry.line, firstNonEmpty(entry.id, entry.path))
		case "approval", "artifact":
			return m.detailRef(entry.id)
		}
	}
	id, ok := m.selectedID()
	if !ok {
		m.state.Notice = "nothing selected"
		return m, nil
	}
	return m.openRef(id)
}

func (m model) activateSelected() (tea.Model, tea.Cmd) {
	if m.focus == focusAgents {
		return m.activateSelectedConversation()
	}
	if m.focus == focusTranscript {
		if item, ok := m.chat.SelectedItem(); ok && item.Kind == workbench.TranscriptAgent {
			return m.activateAgentTranscriptItem(item)
		}
		return m.startActiveConversationComposer()
	}
	if m.focus == focusContext {
		if m.activeRightTab() == workbench.RightTabTerm {
			return m.focusTerminal()
		}
		entry, ok := m.selectedContextEntry()
		if !ok {
			m.state.Notice = "nothing selected"
			return m, nil
		}
		if entry.kind == "folder" {
			m.toggleFolder(entry.path)
			return m, nil
		}
		if entry.kind == "git" {
			return m.detailRef(firstNonEmpty(entry.id, entry.path))
		}
	}
	return m.openSelected()
}

func (m model) activateAgentTranscriptItem(item workbench.TranscriptItem) (tea.Model, tea.Cmd) {
	id := firstNonEmpty(item.Meta["agent_id"], item.Meta["task_id"], item.Meta["agent"], item.ID)
	if strings.TrimSpace(id) == "" {
		m.state.Notice = "agent trace is unavailable"
		return m, nil
	}
	target := workbench.ConversationTarget{Kind: "agent", ID: id, Title: firstNonEmpty(item.Title, item.Actor, id)}
	selector, ok := m.controller.(conversationSelector)
	if !ok {
		m.state.ActiveConversation = target
		m.state.Notice = "conversation: " + target.Title
		m.focus = focusTranscript
		return m, nil
	}
	m.focus = focusTranscript
	return m.runAction(func(ctx context.Context) (workbench.State, error) {
		return selector.SetActiveConversation(ctx, target)
	})
}

func (m model) startConversationComposer() (tea.Model, tea.Cmd) {
	target, ok := m.selectedConversationTarget()
	if !ok {
		m.state.Notice = "nothing selected"
		return m, nil
	}
	m.state.ActiveConversation = target
	m.mode = modeInsert
	m.overlay = overlayNone
	m.focus = focusTranscript
	m.composer.Focus()
	m.state.Notice = "writing to " + target.Title
	return m, nil
}

func (m model) startActiveConversationComposer() (tea.Model, tea.Cmd) {
	if strings.TrimSpace(m.state.ActiveConversation.ID) == "" {
		m.state.ActiveConversation = workbench.ConversationTarget{Kind: "main", ID: "main", Title: "Main orchestrator"}
	}
	m.composer.SetValue("")
	m.mode = modeInsert
	m.overlay = overlayNone
	m.focus = focusTranscript
	m.composer.Focus()
	m.state.Notice = "writing to " + firstNonEmpty(m.state.ActiveConversation.Title, "Main orchestrator")
	return m, nil
}

func (m model) activateSelectedConversation() (tea.Model, tea.Cmd) {
	target, ok := m.selectedConversationTarget()
	if !ok {
		m.state.Notice = "nothing selected"
		return m, nil
	}
	selector, ok := m.controller.(conversationSelector)
	if !ok {
		m.state.ActiveConversation = target
		m.state.Notice = "conversation: " + target.Title
		m.focus = focusTranscript
		return m, nil
	}
	m.focus = focusTranscript
	return m.runAction(func(ctx context.Context) (workbench.State, error) {
		return selector.SetActiveConversation(ctx, target)
	})
}

func (m model) selectedConversationTarget() (workbench.ConversationTarget, bool) {
	target := workbench.ConversationTarget{Kind: "main", ID: "main", Title: "Main orchestrator"}
	if m.leftCursor == 0 {
		return target, true
	}
	agent, ok := m.agentAtCursor()
	if !ok {
		return workbench.ConversationTarget{}, false
	}
	return workbench.ConversationTarget{Kind: "agent", ID: agent.ID, Title: agentConversationTitle(agent)}, true
}

func (m model) activateMainConversation() (tea.Model, tea.Cmd) {
	target := workbench.ConversationTarget{Kind: "main", ID: "main", Title: "Main orchestrator"}
	selector, ok := m.controller.(conversationSelector)
	if !ok {
		m.state.ActiveConversation = target
		m.state.Notice = "conversation: " + target.Title
		m.focus = focusTranscript
		return m, nil
	}
	m.focus = focusTranscript
	return m.runAction(func(ctx context.Context) (workbench.State, error) {
		return selector.SetActiveConversation(ctx, target)
	})
}

func (m model) activateConversationRef(ref string) (tea.Model, tea.Cmd) {
	ref = strings.TrimSpace(ref)
	if ref == "" || ref == "main" || ref == "%" {
		return m.activateMainConversation()
	}
	for i, agent := range m.state.Agents {
		if ref != agent.ID && ref != agent.TaskID && ref != agent.Name && ref != agent.Role {
			continue
		}
		for rowCursor, row := range m.agentDisplayRows() {
			if row.index == i {
				m.leftCursor = rowCursor + 1
				break
			}
		}
		return m.activateSelectedConversation()
	}
	m.state.Notice = "unknown conversation " + ref
	return m, nil
}

func (m model) activateRelativeConversation(delta int) (tea.Model, tea.Cmd) {
	total := len(m.agentDisplayRows()) + 1
	if total <= 1 {
		return m.activateMainConversation()
	}
	index := m.leftCursor
	if m.state.ActiveConversation.Kind == "agent" {
		for rowCursor, row := range m.agentDisplayRows() {
			agent := m.state.Agents[row.index]
			if agent.ID == m.state.ActiveConversation.ID || agent.TaskID == m.state.ActiveConversation.ID || agent.Name == m.state.ActiveConversation.ID {
				index = rowCursor + 1
				break
			}
		}
	}
	index = (index + delta) % total
	if index < 0 {
		index += total
	}
	m.leftCursor = index
	return m.activateSelectedConversation()
}

func (m model) activeConversationID() string {
	id := strings.TrimSpace(m.state.ActiveConversation.ID)
	if id == "" && m.state.ActiveConversation.Kind == "main" {
		return "main"
	}
	return id
}

func (m model) startAgentFollowup() (tea.Model, tea.Cmd) {
	if m.focus == focusAgents && m.leftCursor > 0 {
		if agent, ok := m.agentAtCursor(); ok {
			m.state.ActiveConversation = workbench.ConversationTarget{
				Kind:  "agent",
				ID:    agent.ID,
				Title: agentConversationTitle(agent),
			}
		}
	}
	if m.state.ActiveConversation.Kind != "agent" || strings.TrimSpace(m.state.ActiveConversation.ID) == "" {
		m.state.Notice = "select an agent first"
		return m, nil
	}
	m.mode = modeInsert
	m.overlay = overlayNone
	m.composer.Focus()
	m.state.Notice = "write a directed follow-up to " + m.state.ActiveConversation.Title
	return m, nil
}

func (m model) openRef(id string) (tea.Model, tea.Cmd) {
	m.busy = true
	m.state.Notice = "opening " + id + " in external Neovim"
	cmd := &openCommand{ctx: m.ctx, controller: m.controller, ref: id}
	return m, tea.Exec(cmd, func(err error) tea.Msg {
		return actionMsg{state: cmd.state, err: err}
	})
}

func (m model) openPath(path string, line int, ref string) (tea.Model, tea.Cmd) {
	path = strings.TrimSpace(path)
	if path == "" {
		return m.openRef(ref)
	}
	opener, ok := m.controller.(pathOpener)
	if !ok {
		return m.openRef(firstNonEmpty(ref, path))
	}
	m.busy = true
	m.state.Notice = "opening " + path + " in external Neovim"
	cmd := &openPathCommand{ctx: m.ctx, controller: opener, path: path, line: line}
	return m, tea.Exec(cmd, func(err error) tea.Msg {
		return actionMsg{state: cmd.state, err: err}
	})
}

func (m model) detailSelected() (tea.Model, tea.Cmd) {
	id, ok := m.selectedID()
	if !ok {
		m.state.Notice = "nothing selected"
		return m, nil
	}
	return m.detailRef(id)
}

func (m model) detailRef(id string) (tea.Model, tea.Cmd) {
	m.detailPending = true
	return m.runAction(func(ctx context.Context) (workbench.State, error) {
		return m.controller.Detail(ctx, id)
	})
}

func (m model) copySelected(withFences bool) (tea.Model, tea.Cmd) {
	id, ok := m.selectedID()
	if !ok {
		m.state.Notice = "nothing selected"
		return m, nil
	}
	return m.runAction(func(ctx context.Context) (workbench.State, error) {
		return m.controller.Copy(ctx, id, withFences)
	})
}

func (m model) copyRef(id string, withFences bool) (tea.Model, tea.Cmd) {
	if strings.TrimSpace(id) == "" {
		m.state.Notice = "copy requires an item id"
		return m, nil
	}
	return m.runAction(func(ctx context.Context) (workbench.State, error) {
		return m.controller.Copy(ctx, id, withFences)
	})
}

func (m model) openCopyMode() (tea.Model, tea.Cmd) {
	text, ok := m.selectedCopyText()
	if !ok {
		m.state.Notice = "nothing selected"
		return m, nil
	}
	width := max(20, m.modalContentWidth(96)-8)
	m.detail.SetContent(strings.Join(wrapLines(text, width, 0), "\n"))
	m.detail.SetYOffset(0)
	m.overlay = overlayCopy
	m.mode = modeNormal
	m.state.Notice = "copy mode: select text with mouse and press Cmd+C, Esc returns"
	m.mouseCaptured = false
	return m, tea.DisableMouse
}

// toggleMouseCapture flips terminal mouse capture. With capture off the
// terminal does native click-drag selection AND we fullscreen the focused
// pane so the selection is constrained to that pane's content (the user
// asked for "select only within the focused panel not everywhere"). With
// capture on, scroll wheel scrolls panes and the multi-pane layout returns.
// Default is on; pressing `m` flips it. Hgh/Ctrl+H/L still moves focus
// while in copy mode if the user wants a different pane fullscreened.
func (m model) toggleMouseCapture() (tea.Model, tea.Cmd) {
	m.mouseCaptured = !m.mouseCaptured
	if m.mouseCaptured {
		m.state.Notice = "mouse capture on: multi-pane layout; scroll wheel scrolls; press m to copy a single pane"
		return m, tea.EnableMouseCellMotion
	}
	m.state.Notice = "copy mode: focused pane is fullscreen; click-drag to select, Cmd/Ctrl+C to copy; press m to return"
	return m, tea.DisableMouse
}

func (m model) selectedCopyText() (string, bool) {
	switch m.focus {
	case focusAgents:
		if m.leftCursor == 0 {
			lines := []string{"main orchestrator"}
			if m.state.SessionID != "" {
				lines = append(lines, "session: "+string(m.state.SessionID))
			}
			return strings.Join(lines, "\n"), true
		}
		if agent, ok := m.agentAtCursor(); ok {
			lines := []string{
				strings.TrimSpace(agent.ID + " " + agentRowTitle(agent)),
				prefixedValue("task", agent.TaskID),
				prefixedValue("step", agent.CurrentStep),
				prefixedValue("summary", firstNonEmpty(agent.Summary, agent.Text)),
				prefixedValue("blocked", agent.BlockedReason),
			}
			return strings.Join(nonEmptyLines(lines), "\n"), true
		}
	case focusTranscript:
		if item, ok := m.chat.SelectedItem(); ok {
			return strings.TrimSpace(item.ID + " " + item.Actor + "\n" + item.Text), true
		}
	case focusContext:
		if m.activeRightTab() == workbench.RightTabTerm {
			text := ""
			if term := m.currentTerminal(); term != nil {
				text = term.tail(200)
			}
			return text, strings.TrimSpace(text) != ""
		}
		entry, ok := m.selectedContextEntry()
		if !ok {
			break
		}
		switch entry.kind {
		case "approval":
			if approval, ok := m.findApproval(entry.id); ok {
				return approvalSubjectLine(approval) + "\n\n" + m.approvalContent(approval), true
			}
		case "artifact":
			if artifact, ok := m.findArtifact(entry.id); ok {
				return strings.TrimSpace(strings.Join([]string{artifact.ID, artifact.Kind, artifact.Title, artifact.URI, artifact.Body}, "\n")), true
			}
		case "file", "git":
			return firstNonEmpty(entry.path, entry.label, entry.id), true
		}
	}
	if strings.TrimSpace(m.state.Detail.ID+m.state.Detail.Body) != "" {
		return strings.TrimSpace(m.state.Detail.ID + " " + m.state.Detail.Title + "\n" + m.state.Detail.Body), true
	}
	return "", false
}

func (m model) findApproval(id string) (workbench.ApprovalItem, bool) {
	for _, approval := range m.state.Approvals {
		if approval.ID == id {
			return approval, true
		}
	}
	return workbench.ApprovalItem{}, false
}

func (m model) findArtifact(id string) (workbench.Item, bool) {
	for _, artifact := range m.state.Artifacts {
		if artifact.ID == id {
			return artifact, true
		}
	}
	return workbench.Item{}, false
}

func (m model) approveSelected() (tea.Model, tea.Cmd) {
	approval, ok := m.selectedApproval()
	if !ok {
		m.state.Notice = "no pending approval"
		return m, nil
	}
	return m.runAction(func(ctx context.Context) (workbench.State, error) {
		return m.controller.Approve(ctx, approval.ID)
	})
}

func (m model) approveRef(arg string) (tea.Model, tea.Cmd) {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		m.state.Notice = "approve requires an approval id, range like p1-p3, or 'all'"
		return m, nil
	}
	ids := m.resolveApprovalSpec(arg)
	if len(ids) == 0 {
		m.state.Notice = "no matching pending approvals for " + arg
		return m, nil
	}
	return m.runAction(func(ctx context.Context) (workbench.State, error) {
		var last workbench.State
		var err error
		for _, id := range ids {
			last, err = m.controller.Approve(ctx, id)
			if err != nil {
				return last, err
			}
		}
		last.Notice = fmt.Sprintf("approved %d item(s)", len(ids))
		return last, nil
	})
}

func (m model) rejectSelected() (tea.Model, tea.Cmd) {
	approval, ok := m.selectedApproval()
	if !ok {
		m.state.Notice = "no pending approval"
		return m, nil
	}
	return m.runAction(func(ctx context.Context) (workbench.State, error) {
		return m.controller.Reject(ctx, approval.ID)
	})
}

func (m model) rejectRef(arg string) (tea.Model, tea.Cmd) {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		m.state.Notice = "reject requires an approval id, range like p1-p3, or 'all'"
		return m, nil
	}
	ids := m.resolveApprovalSpec(arg)
	if len(ids) == 0 {
		m.state.Notice = "no matching pending approvals for " + arg
		return m, nil
	}
	return m.runAction(func(ctx context.Context) (workbench.State, error) {
		var last workbench.State
		var err error
		for _, id := range ids {
			last, err = m.controller.Reject(ctx, id)
			if err != nil {
				return last, err
			}
		}
		last.Notice = fmt.Sprintf("rejected %d item(s)", len(ids))
		return last, nil
	})
}

func (m model) runAction(fn func(context.Context) (workbench.State, error)) (tea.Model, tea.Cmd) {
	if m.busy {
		m.state.Notice = "agent is still running; wait for the current action to finish"
		return m, nil
	}
	m.mode = modeNormal
	m.overlay = overlayNone
	m = m.startRun()
	return m, m.actionCmd(m.activeRun, fn)
}

func (m model) startRun() model {
	m.actionSeq++
	m.activeRun = m.actionSeq
	m.busy = true
	m.streamLoading = false
	return m
}

func (m model) actionCmd(runID int, fn func(context.Context) (workbench.State, error)) tea.Cmd {
	return func() tea.Msg {
		state, err := fn(m.ctx)
		return actionMsg{state: state, err: err, runID: runID}
	}
}

func (m model) loadCmd(runID int) tea.Cmd {
	return func() tea.Msg {
		state, err := m.controller.Load(m.ctx)
		return loadMsg{state: state, err: err, runID: runID}
	}
}

func streamTickCmd(runID int) tea.Cmd {
	return tea.Tick(75*time.Millisecond, func(time.Time) tea.Msg { return tickMsg{runID: runID} })
}

func (m model) selectedID() (string, bool) {
	switch m.focus {
	case focusAgents:
		if m.leftCursor == 0 {
			return firstNonEmpty(m.state.ActiveConversation.ID, string(m.state.SessionID)), strings.TrimSpace(firstNonEmpty(m.state.ActiveConversation.ID, string(m.state.SessionID))) != ""
		}
		if agent, ok := m.agentAtCursor(); ok {
			return agent.ID, true
		}
	case focusTranscript:
		return m.chat.SelectedID()
	case focusContext:
		entry, ok := m.selectedContextEntry()
		if ok {
			return firstNonEmpty(entry.id, entry.path), true
		}
	}
	return "", false
}

func (m model) selectedContextEntry() (contextEntry, bool) {
	entries := m.contextEntries()
	if m.contextCursor >= 0 && m.contextCursor < len(entries) {
		return entries[m.contextCursor], true
	}
	return contextEntry{}, false
}

func (m model) selectedApproval() (workbench.ApprovalItem, bool) {
	if len(m.state.Approvals) == 0 {
		return workbench.ApprovalItem{}, false
	}
	index := clamp(m.approvalCursor, 0, len(m.state.Approvals)-1)
	return m.state.Approvals[index], true
}

func (m model) contextEntries() []contextEntry {
	var entries []contextEntry
	switch m.activeRightTab() {
	case workbench.RightTabFiles:
		return workspaceTreeEntries(m.state.Files, m.expandedFolders)
	case workbench.RightTabGit:
		for _, file := range m.state.GitFiles {
			entries = append(entries, workspaceFileEntry(file, "git"))
		}
		return entries
	case workbench.RightTabTerm:
		return nil
	}
	for _, approval := range m.state.Approvals {
		label := approval.Title
		if label == "" {
			label = approval.Subject
		}
		entries = append(entries, contextEntry{id: approval.ID, kind: "approval", label: label, approval: true})
	}
	for _, item := range m.state.Artifacts {
		label := item.Title
		if label == "" {
			label = item.URI
		}
		if label == "" {
			label = item.Kind
		}
		entries = append(entries, contextEntry{id: item.ID, kind: "artifact", label: fmt.Sprintf("%-7s %s", item.Kind, label)})
	}
	return entries
}

func workspaceFileEntry(file workbench.WorkspaceFile, kind string) contextEntry {
	id := firstNonEmpty(file.ID, file.Path, file.Name)
	path := sanitizeTerminalText(firstNonEmpty(file.Path, file.Name, file.ID))
	name := sanitizeTerminalText(firstNonEmpty(file.Name, basePath(path), file.ID))
	label := firstNonEmpty(path, name, file.ID)
	status := firstNonEmpty(file.StatusLine, file.Status, file.Kind)
	return contextEntry{
		id:     sanitizeTerminalText(id),
		kind:   kind,
		label:  sanitizeTerminalText(label),
		path:   path,
		name:   name,
		status: sanitizeTerminalText(status),
	}
}

type fileTreeNode struct {
	name  string
	path  string
	dirs  map[string]*fileTreeNode
	files []workbench.WorkspaceFile
}

func workspaceTreeEntries(files []workbench.WorkspaceFile, expanded map[string]bool) []contextEntry {
	if expanded == nil {
		expanded = map[string]bool{"": true}
	}
	root := &fileTreeNode{dirs: map[string]*fileTreeNode{}}
	for _, file := range files {
		path := strings.Trim(strings.TrimSpace(filepathSlash(firstNonEmpty(file.Path, file.Name, file.ID))), "/")
		if path == "" {
			continue
		}
		parts := strings.Split(path, "/")
		node := root
		for i, part := range parts[:len(parts)-1] {
			if node.dirs == nil {
				node.dirs = map[string]*fileTreeNode{}
			}
			child, ok := node.dirs[part]
			if !ok {
				childPath := strings.Join(parts[:i+1], "/")
				child = &fileTreeNode{name: part, path: childPath, dirs: map[string]*fileTreeNode{}}
				node.dirs[part] = child
			}
			node = child
		}
		if file.Path == "" {
			file.Path = path
		}
		if file.Name == "" || strings.Contains(file.Name, "/") {
			file.Name = parts[len(parts)-1]
		}
		node.files = append(node.files, file)
	}
	var entries []contextEntry
	appendFileTreeEntries(root, expanded, 0, &entries)
	fileIndex := 0
	dirIndex := 0
	for i := range entries {
		switch entries[i].kind {
		case "folder":
			dirIndex++
			entries[i].id = fmt.Sprintf("d%d", dirIndex)
		case "file":
			fileIndex++
			entries[i].id = fmt.Sprintf("f%d", fileIndex)
		}
	}
	return entries
}

func appendFileTreeEntries(node *fileTreeNode, expanded map[string]bool, depth int, entries *[]contextEntry) {
	dirNames := make([]string, 0, len(node.dirs))
	for name := range node.dirs {
		dirNames = append(dirNames, name)
	}
	sort.Strings(dirNames)
	for _, name := range dirNames {
		child := node.dirs[name]
		isExpanded := expanded[child.path]
		*entries = append(*entries, contextEntry{
			id:       "dir:" + child.path,
			kind:     "folder",
			label:    child.name + "/",
			path:     child.path,
			name:     child.name,
			status:   "folder",
			depth:    depth,
			expanded: isExpanded,
		})
		if isExpanded {
			appendFileTreeEntries(child, expanded, depth+1, entries)
		}
	}
	sort.SliceStable(node.files, func(i, j int) bool {
		return firstNonEmpty(node.files[i].Name, node.files[i].Path) < firstNonEmpty(node.files[j].Name, node.files[j].Path)
	})
	for _, file := range node.files {
		entry := workspaceFileEntry(file, "file")
		entry.depth = depth
		*entries = append(*entries, entry)
	}
}

func (m model) filteredCommands() []workbench.Command {
	commands := m.state.Commands
	if len(commands) == 0 {
		commands = workbench.DefaultCommands()
	}
	commands = mergeCommands(commands, workspaceCommands())
	return workbench.FilterCommands(commands, m.palette.Value())
}

func workspaceCommands() []workbench.Command {
	return []workbench.Command{
		{ID: "session.list", Title: "List sessions", Category: "Workspace", Description: "Inspect saved sessions for this workspace.", Keybinding: ":sessions", Key: ":sessions", Keywords: []string{"sessions", "resume"}, Enabled: true},
		{ID: "session.new", Title: "New session", Category: "Workspace", Description: "Start a new session in this workspace.", Keybinding: ":new", Key: ":new", Keywords: []string{"session", "new"}, Enabled: true},
		{ID: "session.rename", Title: "Rename session", Category: "Workspace", Description: "Rename the current session.", Keybinding: ":rename <title>", Key: ":rename <title>", Keywords: []string{"session", "title"}, Enabled: true},
		{ID: "session.resume", Title: "Resume session", Category: "Workspace", Description: "Resume a saved session by id.", Keybinding: ":resume <id>", Key: ":resume <id>", Keywords: []string{"session", "resume"}, Enabled: true},
		{ID: "tab.files", Title: "Show files", Category: "Workspace", Description: "Switch the right pane to workspace files.", Keybinding: ":files", Key: ":files", Keywords: []string{"files", "right pane"}, Enabled: true},
		{ID: "tab.artifacts", Title: "Show artifacts", Category: "Workspace", Description: "Switch the right pane to approvals and artifacts.", Keybinding: ":artifacts", Key: ":artifacts", Keywords: []string{"artifacts", "approvals"}, Enabled: true},
		{ID: "tab.git", Title: "Show git changes", Category: "Workspace", Description: "Switch the right pane to git changes.", Keybinding: ":git", Key: ":git", Keywords: []string{"git", "diff"}, Enabled: true},
		{ID: "terminal.open", Title: "Open terminal", Category: "Shell", Description: "Open the persistent local terminal.", Keybinding: ":term", Key: ":term", Keywords: []string{"terminal", "shell", "pty"}, Enabled: true},
		{ID: "terminal.share", Title: "Share terminal with agent", Category: "Shell", Description: "Enable direct terminal_read and terminal_write tools for the selected terminal.", Keybinding: ":st [n]", Key: ":st", Keywords: []string{"terminal", "share", "context", "control"}, Enabled: true},
		{ID: "settings.open", Title: "Settings", Category: "Workspace", Description: "Show current provider, model, approval, and editor settings.", Keybinding: ":settings", Key: ":settings", Keywords: []string{"settings", "config"}, Enabled: true},
	}
}

func mergeCommands(primary []workbench.Command, extra []workbench.Command) []workbench.Command {
	seen := make(map[string]bool, len(primary)+len(extra))
	merged := append([]workbench.Command(nil), primary...)
	for _, command := range primary {
		seen[command.ID] = true
	}
	for _, command := range extra {
		if seen[command.ID] {
			continue
		}
		merged = append(merged, command)
	}
	return merged
}

func (m *model) completeCommandInput() {
	value := m.command.Value()
	completed, ok := m.completeCommandLine(value)
	if ok {
		m.command.SetValue(completed)
		m.command.SetCursor(len(completed))
	}
}

func (m model) completeCommandLine(value string) (string, bool) {
	value = strings.TrimLeft(value, ":")
	trailingSpace := strings.HasSuffix(value, " ")
	fields := strings.Fields(value)
	if len(fields) == 0 {
		return "", false
	}
	if len(fields) == 1 && !trailingSpace {
		if completion, ok := uniqueCompletion(fields[0], commandNames()); ok {
			if commandTakesArgument(completion) {
				completion += " "
			}
			return completion, true
		}
		return value, false
	}
	command := fields[0]
	prefix := ""
	if !trailingSpace && len(fields) > 1 {
		prefix = fields[len(fields)-1]
	}
	switch command {
	case "edit", "o", "d":
		if completion, ok := uniqueCompletion(prefix, m.fileCompletions()); ok {
			return command + " " + completion, true
		}
	case "resume":
		if completion, ok := uniqueCompletion(prefix, m.sessionCompletions()); ok {
			return command + " " + completion, true
		}
	case "b":
		if completion, ok := uniqueCompletion(prefix, m.conversationCompletions()); ok {
			return command + " " + completion, true
		}
	}
	return value, false
}

func commandNames() []string {
	return []string{"sessions", "new", "rename", "resume", "files", "artifacts", "git", "term", "terminal", "t", "share-term", "share-terminal", "st", "edit", "e", "agent", "swarm", "s", "settings", "main", "back", "b", "bn", "bnext", "bp", "bprevious", "ls", "buffers", "tabn", "tabnext", "tabp", "tabprevious", "Explore", "Ex", "w", "q", "qa", "quit", "help", "palette", "compact", "approval", "danger", "i", "send", "o", "d", "y", "Y", "a", "r", "!"}
}

func commandTakesArgument(command string) bool {
	switch command {
	case "rename", "resume", "edit", "e", "agent", "swarm", "s", "share-term", "share-terminal", "st", "i", "send", "b", "o", "d", "y", "Y", "a", "r", "approval", "!":
		return true
	default:
		return false
	}
}

func (m model) fileCompletions() []string {
	var values []string
	for _, file := range append(append([]workbench.WorkspaceFile(nil), m.state.Files...), m.state.GitFiles...) {
		for _, value := range []string{file.ID, file.Path, file.Name} {
			if strings.TrimSpace(value) != "" {
				values = append(values, value)
			}
		}
	}
	return values
}

func (m model) sessionCompletions() []string {
	var values []string
	for _, summary := range m.state.Sessions {
		if summary.ID != "" {
			values = append(values, string(summary.ID))
		}
	}
	return values
}

func (m model) conversationCompletions() []string {
	values := []string{"main"}
	for _, agent := range m.state.Agents {
		for _, value := range []string{agent.ID, agent.TaskID, agent.Name, agent.Role} {
			if strings.TrimSpace(value) != "" {
				values = append(values, value)
			}
		}
	}
	return values
}

func uniqueCompletion(prefix string, values []string) (string, bool) {
	prefix = strings.TrimSpace(prefix)
	var matches []string
	for _, value := range values {
		if strings.HasPrefix(strings.ToLower(value), strings.ToLower(prefix)) {
			matches = append(matches, value)
		}
	}
	if len(matches) != 1 {
		return "", false
	}
	return matches[0], true
}

func (m *model) resize() {
	if m.width <= 0 {
		m.width = 100
	}
	if m.height <= 0 {
		m.height = 30
	}
	m.composer.SetWidth(max(20, m.width-4))
	m.composer.SetHeight(2)
	m.command.Width = max(10, m.width-6)
	m.palette.Width = min(64, max(10, m.width-20))
	m.syncViewports()
}

func (m *model) syncViewports() {
	_, centerW, rightW := m.paneWidths()
	bodyH := m.bodyHeight()
	m.chat.SetSize(max(10, centerW-4), max(3, bodyH-3))
	m.chat.SetItems(m.state.Transcript)
	m.detail.Width = max(10, rightW-4)
	m.detail.Height = max(3, bodyH-3)
	m.syncDetailViewport()
	m.syncApprovalViewport()
}

func (m *model) clampCursors() {
	m.leftCursor = clamp(m.leftCursor, 0, len(m.agentDisplayRows()))
	m.chat.SetItems(m.state.Transcript)
	m.contextCursor = clamp(m.contextCursor, 0, max(0, len(m.contextEntries())-1))
	m.approvalCursor = clamp(m.approvalCursor, 0, max(0, len(m.state.Approvals)-1))
}

func (m *model) followLatestTranscript() {
	m.chat.SetItems(m.state.Transcript)
	m.chat.FollowLatest()
}

// toggleDebugMode flips :debug on/off. With debug on the model clients
// log every chunk of every turn (not just suspicious ones) and the TUI
// border turns orange so the user has a constant visible reminder that
// extra logging is active.
func (m model) toggleDebugMode() (tea.Model, tea.Cmd) {
	return m.setDebugMode(!coremodel.Debug())
}

func (m model) setDebugMode(on bool) (tea.Model, tea.Cmd) {
	coremodel.SetDebug(on)
	if on {
		m.state.Notice = "debug mode ON — orange border. Every model chunk is dumped as a model_response_dump artifact. :debug off to disable."
	} else {
		m.state.Notice = "debug mode OFF"
	}
	return m, nil
}

// firstPendingApprovalID returns the id of the topmost pending approval, so
// `a`/`r` from any pane can act on it without first switching focus to the
// right context pane. Returns false when nothing is pending.
func (m model) firstPendingApprovalID() (string, bool) {
	for _, approval := range m.state.Approvals {
		if strings.TrimSpace(approval.ID) != "" {
			return approval.ID, true
		}
	}
	return "", false
}

// resolveApprovalSpec turns a user argument into the concrete approval ids
// to act on. Accepted forms:
//
//   - "all" / "*": every pending approval, in queue order
//   - "p3": a specific id
//   - "p1-p3" / "1-3": an inclusive range over the queue's numeric suffix
//   - "p1 p2 p4" / "p1,p2,p4": a list of ids
//
// Unknown ids that don't match anything pending are silently dropped so the
// user can retry without typing a partial command.
func (m model) resolveApprovalSpec(arg string) []string {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return nil
	}
	pendingByID := map[string]bool{}
	pendingOrder := make([]string, 0, len(m.state.Approvals))
	for _, ap := range m.state.Approvals {
		if strings.TrimSpace(ap.ID) != "" && !pendingByID[ap.ID] {
			pendingByID[ap.ID] = true
			pendingOrder = append(pendingOrder, ap.ID)
		}
	}
	if strings.EqualFold(arg, "all") || arg == "*" {
		return pendingOrder
	}
	// Range: p1-p3 or 1-3.
	if lo, hi, ok := parseRangeSpec(arg); ok {
		var ids []string
		for _, id := range pendingOrder {
			if num, ok := approvalIDNumber(id); ok && num >= lo && num <= hi {
				ids = append(ids, id)
			}
		}
		return ids
	}
	// List: comma- or space-separated.
	tokens := strings.FieldsFunc(arg, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t'
	})
	var ids []string
	for _, tok := range tokens {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		if pendingByID[tok] {
			ids = append(ids, tok)
			continue
		}
		// Bare number ("3") matches "p3".
		prefixed := "p" + tok
		if pendingByID[prefixed] {
			ids = append(ids, prefixed)
		}
	}
	return ids
}

func parseRangeSpec(arg string) (int, int, bool) {
	idx := strings.IndexByte(arg, '-')
	if idx < 0 {
		return 0, 0, false
	}
	left := strings.TrimSpace(arg[:idx])
	right := strings.TrimSpace(arg[idx+1:])
	if left == "" || right == "" {
		return 0, 0, false
	}
	lo, lok := approvalNumberToken(left)
	hi, hok := approvalNumberToken(right)
	if !lok || !hok || hi < lo {
		return 0, 0, false
	}
	return lo, hi, true
}

func approvalNumberToken(token string) (int, bool) {
	token = strings.TrimSpace(token)
	token = strings.TrimPrefix(token, "p")
	n, err := strconv.Atoi(token)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

func approvalIDNumber(id string) (int, bool) {
	digits := strings.TrimPrefix(id, "p")
	n, err := strconv.Atoi(digits)
	if err != nil {
		return 0, false
	}
	return n, true
}

// pendingApprovalStrip renders a one-line summary of the pending approvals
// queue, suitable for the notice line. Empty string when nothing is pending.
func (m model) pendingApprovalStrip() string {
	if len(m.state.Approvals) == 0 {
		return ""
	}
	ids := make([]string, 0, len(m.state.Approvals))
	for _, ap := range m.state.Approvals {
		if id := strings.TrimSpace(ap.ID); id != "" {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return ""
	}
	if len(ids) > 6 {
		more := len(ids) - 6
		ids = append(ids[:6], fmt.Sprintf("+%d", more))
	}
	return fmt.Sprintf("pending [%s] — a/r approve/reject; :a all / :a p1-p3 / :A all", strings.Join(ids, " "))
}

func (m *model) moveSelection(delta int) {
	switch m.focus {
	case focusAgents:
		m.leftCursor = clamp(m.leftCursor+delta, 0, len(m.agentDisplayRows()))
	case focusTranscript:
		// SmartMove scrolls within the selected message when it overflows
		// the viewport before advancing to the next/previous cell — so the
		// user can read the middle of long messages with j/k instead of
		// jumping past them.
		m.chat.SmartMove(delta)
	case focusContext:
		m.contextCursor = clamp(m.contextCursor+delta, 0, max(0, len(m.contextEntries())-1))
	}
}

func (m *model) scrollFocused(delta int) {
	switch m.focus {
	case focusTranscript:
		if delta > 0 {
			m.chat.LineDown(delta)
			return
		}
		m.chat.LineUp(-delta)
	case focusContext:
		m.contextCursor = clamp(m.contextCursor+delta, 0, max(0, len(m.contextEntries())-1))
	}
}

func (m *model) sanitizeComposer() {
	if clean := sanitizeTerminalText(m.composer.Value()); clean != m.composer.Value() {
		m.composer.SetValue(clean)
	}
}

func (m *model) setRightTab(tab workbench.RightTab) {
	m.state.RightTab = tab
	m.focus = focusContext
	m.contextCursor = 0
	m.clampCursors()
}

func (m *model) cycleRightTab(delta int) {
	tabs := []workbench.RightTab{workbench.RightTabFiles, workbench.RightTabArtifacts, workbench.RightTabGit, workbench.RightTabTerm}
	current := 0
	active := m.activeRightTab()
	for i, tab := range tabs {
		if tab == active {
			current = i
			break
		}
	}
	next := (current + delta) % len(tabs)
	if next < 0 {
		next += len(tabs)
	}
	m.setRightTab(tabs[next])
}

func (m *model) toggleFolder(path string) {
	path = strings.Trim(strings.TrimSpace(filepathSlash(path)), "/")
	if m.expandedFolders == nil {
		m.expandedFolders = map[string]bool{"": true}
	}
	m.expandedFolders[path] = !m.expandedFolders[path]
	m.clampCursors()
	if m.expandedFolders[path] {
		m.state.Notice = "opened " + path
		return
	}
	m.state.Notice = "closed " + path
}

func (m model) activeRightTab() workbench.RightTab {
	switch m.state.RightTab {
	case workbench.RightTabFiles, workbench.RightTabArtifacts, workbench.RightTabGit, workbench.RightTabTerm:
		return m.state.RightTab
	default:
		return workbench.RightTabArtifacts
	}
}

func preserveRightTab(state workbench.State, previous workbench.RightTab) workbench.State {
	if previous == "" || previous == workbench.RightTabFiles {
		return state
	}
	if state.RightTab == "" || state.RightTab == workbench.RightTabFiles {
		state.RightTab = previous
	}
	return state
}

func (m *model) moveApproval(delta int) {
	m.approvalCursor = clamp(m.approvalCursor+delta, 0, max(0, len(m.state.Approvals)-1))
	if len(m.state.Approvals) == 0 {
		return
	}
	approval := m.state.Approvals[m.approvalCursor]
	m.approval.SetYOffset(0)
	m.syncApprovalViewport()
	for i, entry := range m.contextEntries() {
		if entry.id == approval.ID {
			m.contextCursor = i
			break
		}
	}
}

func (m model) paneWidths() (int, int, int) {
	if m.width < 70 {
		width := max(10, m.width-4)
		switch m.focus {
		case focusAgents:
			return width, 0, 0
		case focusContext:
			return 0, 0, width
		default:
			return 0, width, 0
		}
	}
	if m.width < 105 {
		total := max(24, m.width-8)
		side := min(42, max(26, total/2))
		center := max(20, total-side)
		if m.focus == focusAgents {
			return side, center, 0
		}
		return 0, center, side
	}
	total := max(36, m.width-12)
	left := max(18, total/5)
	right := max(24, total/4)
	switch m.focus {
	case focusAgents:
		left = max(24, total/4)
		right = max(24, total/5)
	case focusContext:
		left = max(16, total/6)
		right = max(34, (total*2)/5)
	default:
		right = max(28, total/4)
	}
	center := total - left - right
	if center < 30 {
		deficit := 30 - center
		right = max(26, right-deficit)
		center = total - left - right
		if center < 30 {
			left = max(16, left-(30-center))
			center = total - left - right
		}
	}
	return left, center, right
}

func (m model) bodyHeight() int {
	return max(3, m.height-8)
}

func (m model) modalContentWidth(maxWidth int) int {
	return min(maxWidth, max(12, m.width-8))
}

func disabledNotice(command workbench.Command) string {
	if strings.TrimSpace(command.DisabledReason) != "" {
		return command.Title + " disabled: " + command.DisabledReason
	}
	return command.Title + " is disabled"
}

func (m *model) syncDetailViewport() {
	if strings.TrimSpace(m.state.Detail.ID+m.state.Detail.Body+m.state.Detail.Title) == "" {
		m.detail.SetContent("")
		return
	}
	width := max(10, m.detail.Width-2)
	var lines []string
	if strings.TrimSpace(m.state.Detail.Kind) != "" {
		lines = append(lines, "kind: "+sanitizeTerminalText(m.state.Detail.Kind))
	}
	if strings.TrimSpace(m.state.Detail.URI) != "" {
		lines = append(lines, "uri: "+sanitizeTerminalText(m.state.Detail.URI))
	}
	if len(lines) > 0 {
		lines = append(lines, "")
	}
	body := strings.TrimSpace(firstNonEmpty(m.state.Detail.Body, m.state.Detail.Title))
	lines = append(lines, wrapLines(body, width, 0)...)
	m.detail.SetContent(strings.Join(lines, "\n"))
}

func (m *model) syncApprovalViewport() {
	approval, ok := m.selectedApproval()
	if !ok {
		m.approval.SetContent("")
		return
	}
	m.approval.SetContent(m.approvalContent(approval))
}

func (m model) approvalContent(approval workbench.ApprovalItem) string {
	var lines []string
	subject := sanitizeTerminalText(approval.Subject)
	reason := sanitizeTerminalText(approval.Reason)
	if strings.TrimSpace(subject) != "" {
		lines = append(lines, "Changed files")
		for _, file := range splitSubject(subject) {
			lines = append(lines, "  "+file)
		}
		lines = append(lines, "")
	}
	if strings.TrimSpace(reason) != "" {
		lines = append(lines, "Reason")
		lines = append(lines, "  "+reason)
		lines = append(lines, "")
	}
	lines = append(lines, "Diff / request")
	body := strings.TrimSpace(sanitizeTerminalText(approval.Body))
	if body == "" {
		body = "(no body)"
	}
	lines = append(lines, body)
	return strings.Join(lines, "\n")
}

func splitSubject(subject string) []string {
	fields := strings.FieldsFunc(subject, func(r rune) bool {
		return r == ',' || r == '\n' || r == '\t'
	})
	var result []string
	for _, field := range fields {
		if trimmed := strings.TrimSpace(field); trimmed != "" {
			result = append(result, trimmed)
		}
	}
	if len(result) == 0 {
		return []string{strings.TrimSpace(subject)}
	}
	return result
}

type openCommand struct {
	ctx        context.Context
	controller Controller
	ref        string
	state      workbench.State
}

func (c *openCommand) Run() error {
	state, err := c.controller.Open(c.ctx, c.ref)
	c.state = state
	return err
}

func (c *openCommand) SetStdin(io.Reader)  {}
func (c *openCommand) SetStdout(io.Writer) {}
func (c *openCommand) SetStderr(io.Writer) {}

type openPathCommand struct {
	ctx        context.Context
	controller pathOpener
	path       string
	line       int
	state      workbench.State
}

func (c *openPathCommand) Run() error {
	state, err := c.controller.OpenPath(c.ctx, c.path, c.line)
	c.state = state
	return err
}

func (c *openPathCommand) SetStdin(io.Reader)  {}
func (c *openPathCommand) SetStdout(io.Writer) {}
func (c *openPathCommand) SetStderr(io.Writer) {}

var (
	tuiRenderer = newSafeRenderer(io.Discard)

	headerStyle   = tuiRenderer.NewStyle().Foreground(lipgloss.Color("15")).Background(lipgloss.Color("8")).Padding(0, 1)
	titleStyle    = tuiRenderer.NewStyle().Bold(true).Foreground(lipgloss.Color("14"))
	mutedStyle    = tuiRenderer.NewStyle().Foreground(lipgloss.Color("8"))
	errorStyle    = tuiRenderer.NewStyle().Foreground(lipgloss.Color("9"))
	selectedStyle = tuiRenderer.NewStyle().Foreground(lipgloss.Color("14"))
	borderStyle   = tuiRenderer.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("8")).Padding(0, 1)
	activeStyle   = borderStyle.Copy().BorderForeground(lipgloss.Color("14"))
	composerStyle = tuiRenderer.NewStyle().Border(lipgloss.NormalBorder()).BorderForeground(lipgloss.Color("8")).Padding(0, 1)
	noticeStyle   = tuiRenderer.NewStyle().Foreground(lipgloss.Color("8")).Padding(0, 1)
	modalStyle    = tuiRenderer.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("14")).Padding(1, 2).Background(lipgloss.Color("0"))
)

func newSafeRenderer(w io.Writer) *lipgloss.Renderer {
	r := lipgloss.NewRenderer(w, termenv.WithProfile(termenv.ANSI256), termenv.WithTTY(false))
	r.SetColorProfile(termenv.ANSI256)
	r.SetHasDarkBackground(true)
	lipgloss.SetDefaultRenderer(r)
	return r
}

func configureRenderer(w io.Writer) {
	if w == nil {
		w = io.Discard
	}
	tuiRenderer.SetOutput(termenv.NewOutput(w, termenv.WithProfile(termenv.ANSI256), termenv.WithTTY(false)))
	tuiRenderer.SetColorProfile(termenv.ANSI256)
	tuiRenderer.SetHasDarkBackground(true)
	lipgloss.SetDefaultRenderer(tuiRenderer)
}

func paneStyle(active bool) lipgloss.Style {
	// Orange border when debug mode is on — a constant visible reminder
	// that the session is logging every model chunk to disk and that
	// :debug needs to be flipped off when the user is done diagnosing.
	if coremodel.Debug() {
		debugColor := lipgloss.Color("208") // ANSI orange
		if active {
			return activeStyle.Copy().BorderForeground(debugColor)
		}
		return borderStyle.Copy().BorderForeground(debugColor)
	}
	if active {
		return activeStyle
	}
	return borderStyle
}

func selectableLine(active bool, id string, label string) string {
	id = sanitizeTerminalText(id)
	label = sanitizeTerminalText(label)
	var parts []string
	if strings.TrimSpace(id) != "" {
		parts = append(parts, id)
	}
	if strings.TrimSpace(label) != "" {
		parts = append(parts, label)
	}
	text := strings.Join(parts, " ")
	if active {
		return selectedStyle.Render(text)
	}
	return text
}

func fitLines(lines []string, width int, height int) string {
	if height <= 0 {
		return ""
	}
	if len(lines) > height {
		lines = append(lines[:height-1], mutedStyle.Render("..."))
	}
	for i := range lines {
		lines[i] = truncate(lines[i], max(1, width-2))
	}
	return strings.Join(lines, "\n")
}

func fitFrame(view string, width int, height int) string {
	if height <= 0 {
		return ""
	}
	width = max(1, width)
	lines := strings.Split(view, "\n")
	if len(lines) > height {
		lines = lines[:height]
	}
	for len(lines) < height {
		lines = append(lines, "")
	}
	for i := range lines {
		lines[i] = truncate(lines[i], width)
		if pad := width - lipgloss.Width(lines[i]); pad > 0 {
			lines[i] += strings.Repeat(" ", pad)
		}
	}
	return strings.Join(lines, "\n")
}

func wrapLines(text string, width int, limit int) []string {
	text = strings.TrimSpace(sanitizeTerminalText(text))
	if text == "" {
		return []string{""}
	}
	var lines []string
	for _, raw := range strings.Split(text, "\n") {
		raw = strings.TrimSpace(raw)
		for len(raw) > width && width > 0 {
			lines = append(lines, raw[:width])
			raw = raw[width:]
			if limit > 0 && len(lines) >= limit {
				return append(lines, "...")
			}
		}
		lines = append(lines, raw)
		if limit > 0 && len(lines) >= limit {
			return append(lines, "...")
		}
	}
	return lines
}

func visibleRange(total int, cursor int, capacity int) (int, int) {
	if total <= 0 || capacity <= 0 {
		return 0, 0
	}
	if capacity >= total {
		return 0, total
	}
	cursor = clamp(cursor, 0, total-1)
	start := cursor - capacity/2
	start = clamp(start, 0, total-capacity)
	return start, start + capacity
}

var (
	ansiPattern     = regexp.MustCompile(`\x1b(?:\[[0-?]*[ -/]*[@-~]|\][^\x07]*(?:\x07|\x1b\\)?|[@-_])`)
	oscColorPattern = regexp.MustCompile(`(^|\s)\]?(?:10|11);rgb:[0-9A-Fa-f/]+`)
)

func sanitizeTerminalText(text string) string {
	if text == "" {
		return ""
	}
	text = ansiPattern.ReplaceAllString(text, "")
	text = oscColorPattern.ReplaceAllString(text, "$1")
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, text)
}

func placeOverlay(base string, overlay string, width int, height int) string {
	lines := strings.Split(base, "\n")
	if height <= 0 {
		return ""
	}
	if len(lines) > height {
		lines = lines[:height]
	}
	for len(lines) < height {
		lines = append(lines, strings.Repeat(" ", width))
	}
	overlayLines := strings.Split(overlay, "\n")
	maxOverlayLines := max(1, height-2)
	if len(overlayLines) > maxOverlayLines {
		overlayLines = append(overlayLines[:maxOverlayLines-1], mutedStyle.Render("..."))
	}
	start := max(0, (height-len(overlayLines))/2)
	for i, line := range overlayLines {
		line = truncate(line, max(1, width))
		left := max(0, (width-lipgloss.Width(line))/2)
		padded := strings.Repeat(" ", left) + line
		if start+i < len(lines) {
			lines[start+i] = truncate(padded, max(1, width))
		}
	}
	return strings.Join(lines, "\n")
}

func dash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return value
}

func prefixedValue(prefix string, value string) string {
	value = strings.TrimSpace(sanitizeTerminalText(value))
	if value == "" {
		return ""
	}
	return prefix + " " + value
}

func compactList(values []string, width int) string {
	if len(values) == 0 {
		return ""
	}
	width = max(8, width)
	var kept []string
	for i, value := range values {
		value = strings.TrimSpace(sanitizeTerminalText(value))
		if value == "" {
			continue
		}
		candidate := strings.Join(append(append([]string(nil), kept...), value), ", ")
		remaining := len(values) - i - 1
		if remaining > 0 {
			candidate += fmt.Sprintf(" +%d", remaining)
		}
		if lipgloss.Width(candidate) > width && len(kept) > 0 {
			return strings.Join(kept, ", ") + fmt.Sprintf(" +%d", remaining+1)
		}
		kept = append(kept, value)
	}
	return strings.Join(kept, ", ")
}

func basePath(path string) string {
	path = strings.Trim(strings.TrimSpace(sanitizeTerminalText(path)), "/")
	if path == "" {
		return ""
	}
	if idx := strings.LastIndex(path, "/"); idx >= 0 {
		return path[idx+1:]
	}
	return path
}

func parentPath(path string) string {
	path = strings.Trim(strings.TrimSpace(sanitizeTerminalText(path)), "/")
	if path == "" {
		return ""
	}
	if idx := strings.LastIndex(path, "/"); idx >= 0 {
		parent := path[:idx]
		if parent == "" {
			return "."
		}
		return parent
	}
	return "."
}

func compactPath(path string, width int) string {
	path = strings.TrimSpace(sanitizeTerminalText(path))
	if width <= 0 {
		return ""
	}
	if lipgloss.Width(path) <= width {
		return path
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) <= 2 {
		return truncate(path, width)
	}
	tail := strings.Join(parts[len(parts)-2:], "/")
	prefix := parts[0] + "/.../"
	candidate := prefix + tail
	if lipgloss.Width(candidate) <= width {
		return candidate
	}
	return truncate(".../"+tail, width)
}

func filepathSlash(path string) string {
	return strings.ReplaceAll(sanitizeTerminalText(path), "\\", "/")
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(sanitizeTerminalText(value)); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func nonEmptyLines(values []string) []string {
	var lines []string
	for _, value := range values {
		if trimmed := strings.TrimSpace(sanitizeTerminalText(value)); trimmed != "" {
			lines = append(lines, trimmed)
		}
	}
	return lines
}

func truncate(value string, width int) string {
	if width <= 0 {
		return ""
	}
	if lipgloss.Width(value) <= width {
		return value
	}
	runes := []rune(value)
	if width <= 3 {
		return string(runes[:min(len(runes), width)])
	}
	for lipgloss.Width(string(runes)) > width-3 && len(runes) > 0 {
		runes = runes[:len(runes)-1]
	}
	return string(runes) + "..."
}

func clamp(value int, low int, high int) int {
	if high < low {
		return low
	}
	if value < low {
		return low
	}
	if value > high {
		return high
	}
	return value
}

func min(a int, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a int, b int) int {
	if a > b {
		return a
	}
	return b
}
