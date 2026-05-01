package tui2

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/Luew2/FreeCode/internal/app/workbench"
)

type tutorialInputMode string

const (
	tutorialNormal       tutorialInputMode = "normal"
	tutorialComposer     tutorialInputMode = "composer"
	tutorialInputCommand tutorialInputMode = "command"
)

type tutorialStepKind string

const (
	tutorialPress       tutorialStepKind = "press"
	tutorialPrompt      tutorialStepKind = "prompt"
	tutorialStepCommand tutorialStepKind = "command"
)

type tutorialGame struct {
	step      int
	inputMode tutorialInputMode
	input     string
	completed map[string]bool
	last      string
	log       []string
}

type tutorialStep struct {
	ID          string
	Title       string
	Description string
	CommandID   string
	Kind        tutorialStepKind
	Keys        []string
	Expected    []string
	Response    string
}

func (m model) openTutorial() model {
	m.mode = modeNormal
	m.overlay = overlayTutorial
	m.palette.Blur()
	m.command.Blur()
	m.composer.Blur()
	if m.tutorial.completed == nil {
		m.tutorial.completed = map[string]bool{}
	}
	m.tutorial.inputMode = tutorialNormal
	m.tutorial.input = ""
	m.tutorial.step = clamp(m.tutorial.step, 0, max(0, len(m.tutorialSteps())-1))
	if len(m.tutorial.log) == 0 {
		m.tutorial.log = []string{"tutorial: No API calls are made here. Commands and prompts are intercepted locally."}
	}
	m.state.Notice = "tutorial opened"
	return m
}

func (m model) handleTutorialKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	steps := m.tutorialSteps()
	if len(steps) == 0 {
		m.overlay = overlayNone
		return m, nil
	}
	m.tutorial.step = clamp(m.tutorial.step, 0, len(steps)-1)
	key := msg.String()
	current := steps[m.tutorial.step]

	if m.tutorial.inputMode != tutorialNormal {
		return m.handleTutorialInputKey(msg, current)
	}

	switch key {
	case "esc", "q":
		m.overlay = overlayNone
		m.mode = modeNormal
		m.state.Notice = "tutorial closed"
		return m, nil
	case "j", "down", "n":
		m.tutorial.Move(1, len(steps))
		return m, nil
	case "k", "up", "p":
		m.tutorial.Move(-1, len(steps))
		return m, nil
	case "home", "g":
		m.tutorial.step = 0
		return m, nil
	case "end", "G":
		m.tutorial.step = len(steps) - 1
		return m, nil
	}

	switch current.Kind {
	case tutorialPrompt:
		if key == "i" || key == "enter" {
			m.tutorial.inputMode = tutorialComposer
			m.tutorial.input = ""
			m.tutorial.last = "composer opened. Type: " + current.Expected[0]
			return m, nil
		}
		m.tutorialWrong(key, current, "press i, type the prompt, then Enter")
		return m, nil
	case tutorialStepCommand:
		if key == ":" || key == "!" {
			wantPrefix := tutorialCommandPrefix(current)
			if key != wantPrefix {
				m.tutorialWrong(key, current, "start with "+wantPrefix)
				return m, nil
			}
			m.tutorial.inputMode = tutorialInputCommand
			m.tutorial.input = key
			m.tutorial.last = "command line opened. Type: " + current.Expected[0]
			return m, nil
		}
		m.tutorialWrong(key, current, "type "+current.Expected[0])
		return m, nil
	default:
		if tutorialKeyMatches(key, current.Keys) {
			m.completeTutorialStep(current, tutorialDisplayKey(key))
			return m, nil
		}
		m.tutorialWrong(key, current, "press "+strings.Join(current.Keys, " or "))
		return m, nil
	}
}

func (m model) handleTutorialInputKey(msg tea.KeyMsg, step tutorialStep) (tea.Model, tea.Cmd) {
	key := msg.String()
	switch key {
	case "esc":
		m.tutorial.inputMode = tutorialNormal
		m.tutorial.input = ""
		m.tutorial.last = fmt.Sprintf("reset. Try again: %s", tutorialInstruction(step))
		return m, nil
	case "backspace", "ctrl+h":
		if len(m.tutorial.input) > 0 {
			m.tutorial.input = m.tutorial.input[:len(m.tutorial.input)-1]
		}
		return m, nil
	case "enter":
		if tutorialInputMatches(m.tutorial.input, step.Expected) {
			m.completeTutorialStep(step, m.tutorial.input)
			m.tutorial.inputMode = tutorialNormal
			m.tutorial.input = ""
			return m, nil
		}
		m.tutorial.last = fmt.Sprintf("seems you typed %q wrong. Hit Esc, then %s.", m.tutorial.input, tutorialInstruction(step))
		return m, nil
	}
	if text := tutorialTypedText(msg); text != "" {
		m.tutorial.input += sanitizeTerminalText(text)
	}
	return m, nil
}

func (m *model) completeTutorialStep(step tutorialStep, typed string) {
	if m.tutorial.completed == nil {
		m.tutorial.completed = map[string]bool{}
	}
	m.tutorial.completed[step.ID] = true
	m.tutorial.last = "passed: " + step.Title
	if typed != "" {
		m.tutorial.log = append(m.tutorial.log, "you: "+typed)
	}
	if step.Response != "" {
		m.tutorial.log = append(m.tutorial.log, "freecode: "+step.Response)
	}
	if m.tutorial.step < len(m.tutorialSteps())-1 {
		m.tutorial.step++
	}
}

func (m *model) tutorialWrong(typed string, step tutorialStep, hint string) {
	typed = tutorialDisplayKey(typed)
	m.tutorial.last = fmt.Sprintf("seems you typed %q wrong; %s.", typed, hint)
}

func (g *tutorialGame) Move(delta int, count int) {
	g.step = clamp(g.step+delta, 0, max(0, count-1))
	g.inputMode = tutorialNormal
	g.input = ""
}

func (m model) tutorialView() string {
	steps := m.tutorialSteps()
	width := m.modalContentWidth(94)
	contentWidth := max(20, width-8)
	height := max(14, min(max(14, m.height-6), 36))
	if len(steps) == 0 {
		return modalStyle.Width(width).Render("No tutorial steps available")
	}
	m.tutorial.step = clamp(m.tutorial.step, 0, len(steps)-1)
	current := steps[m.tutorial.step]
	completed := 0
	for _, step := range steps {
		if m.tutorial.completed[step.ID] {
			completed++
		}
	}

	lines := []string{titleStyle.Render("FreeCode Tutorial")}
	appendTutorialWrapped(&lines, "muted", fmt.Sprintf("progress %d/%d  j/k browse  type inside this overlay  Esc resets input/closes from normal", completed, len(steps)), contentWidth)
	lines = append(lines, "", selectedStyle.Render(fmt.Sprintf("Mission %d: %s", m.tutorial.step+1, current.Title)))
	appendTutorialWrapped(&lines, "", current.Description, contentWidth)
	appendTutorialWrapped(&lines, "", "Task: "+tutorialInstruction(current), contentWidth)
	if command := m.commandByID(current.CommandID); command.Description != "" {
		appendTutorialWrapped(&lines, "muted", "Why: "+command.Description, contentWidth)
	}
	lines = append(lines, "")
	lines = append(lines, m.tutorialInputLine(current))
	if m.tutorial.completed[current.ID] {
		lines = append(lines, selectedStyle.Render("Complete"))
	}
	if m.tutorial.last != "" {
		appendTutorialWrapped(&lines, "muted", m.tutorial.last, contentWidth)
	}
	lines = append(lines, "", titleStyle.Render("Simulated Output"))
	lines = append(lines, m.tutorialLogLines(contentWidth, max(3, height-len(lines)-8))...)
	lines = append(lines, "", titleStyle.Render("Training Board"))
	start, end := visibleRange(len(steps), m.tutorial.step, max(4, height-len(lines)-2))
	for i := start; i < end; i++ {
		step := steps[i]
		mark := " "
		if m.tutorial.completed[step.ID] {
			mark = "x"
		}
		row := fmt.Sprintf("[%s] %-18s %s", mark, tutorialShortTarget(step), step.Title)
		wrapped := wrapLines(row, contentWidth, 0)
		for j, line := range wrapped {
			if j > 0 {
				line = "    " + line
			}
			lines = append(lines, selectableLine(i == m.tutorial.step, "", line))
		}
	}
	appendTutorialWrapped(&lines, "muted", "No real prompts, shell commands, terminal sharing, or API requests happen in tutorial mode.", contentWidth)
	return modalStyle.Width(width).Render(fitLines(lines, contentWidth+2, height))
}

func (m model) tutorialInputLine(step tutorialStep) string {
	switch m.tutorial.inputMode {
	case tutorialComposer:
		return composerStyle.Width(max(20, m.modalContentWidth(82))).Render("TUTORIAL INSERT  Enter checks  Esc reset\n> " + m.tutorial.input)
	case tutorialInputCommand:
		return composerStyle.Width(max(20, m.modalContentWidth(82))).Render("TUTORIAL COMMAND  Enter checks  Esc reset\n" + m.tutorial.input)
	default:
		switch step.Kind {
		case tutorialPrompt:
			return mutedStyle.Render("waiting: press i to open the tutorial composer")
		case tutorialStepCommand:
			return mutedStyle.Render("waiting: type " + step.Expected[0])
		default:
			return mutedStyle.Render("waiting: press " + strings.Join(step.Keys, " or "))
		}
	}
}

func (m model) tutorialLogLines(width int, limit int) []string {
	if len(m.tutorial.log) == 0 {
		return []string{mutedStyle.Render("tutorial output will appear here")}
	}
	contentWidth := max(20, width)
	var lines []string
	for _, line := range m.tutorial.log {
		lines = append(lines, wrapLines(line, contentWidth, 0)...)
	}
	if limit > 0 && len(lines) > limit {
		lines = append([]string{mutedStyle.Render("more simulated output above")}, lines[len(lines)-limit+1:]...)
	}
	return lines
}

func appendTutorialWrapped(lines *[]string, style string, text string, width int) {
	for _, line := range wrapLines(text, width, 0) {
		switch style {
		case "muted":
			*lines = append(*lines, mutedStyle.Render(line))
		default:
			*lines = append(*lines, line)
		}
	}
}

func (m model) tutorialSteps() []tutorialStep {
	step := func(id, title, description string, kind tutorialStepKind, expected []string, response string, keys ...string) tutorialStep {
		if command := m.commandByID(id); command.ID != "" {
			if title == "" {
				title = command.Title
			}
			if description == "" {
				description = command.Description
			}
		}
		return tutorialStep{ID: id, Title: title, Description: description, CommandID: id, Kind: kind, Expected: expected, Response: response, Keys: keys}
	}
	return []tutorialStep{
		step("prompt.insert", "Start a chat turn", "Open the composer and type a harmless prompt. The tutorial intercepts it instead of calling a model.", tutorialPrompt, []string{"explain this repo"}, "That would send a normal chat prompt to the active conversation. No API request was sent."),
		step("tab.ops", "Open the Ops board", "Practice a Vim command that jumps to the operational view.", tutorialStepCommand, []string{":ops"}, "The right pane would show active runs, queues, approvals, terminal sharing, and context."),
		step("context.inspect", "Inspect next context", "Learn the command that answers: what will the model see next?", tutorialStepCommand, []string{":context"}, "The context inspector would open with conversation tail, memory, terminal sharing state, and token estimate."),
		step("agent.swarm", "Stage a swarm task", "Type a swarm command. In the real app this asks the main orchestrator to plan and allocate agents.", tutorialStepCommand, []string{":s review this codebase"}, "A swarm request would be queued for the orchestrator. No model call was made."),
		step("shell.run", "Run a one-shot shell", "Practice the Vim-style local shell command. Tutorial mode does not run your shell.", tutorialStepCommand, []string{"!pwd"}, "A local-only shell cell would be appended to chat. No command was executed."),
		step("terminal.open", "Open persistent terminal", "Practice opening the persistent terminal tab.", tutorialStepCommand, []string{":term", ":t"}, "The Term tab would open without stealing focus."),
		step("terminal.share", "Share terminal control", "Practice the explicit opt-in command that gives the agent terminal_read and terminal_write.", tutorialStepCommand, []string{":st"}, "Terminal 1 would become live-shared with the agent, visibly marked in Ops and Term."),
		step("terminal.share_output", "Attach terminal output", "Practice one-time output sharing without live terminal control.", tutorialStepCommand, []string{":sto"}, "Recent terminal output would be attached to the next model turn only."),
		step("buffer.list", "List conversation buffers", "Treat main and agent chats like Vim buffers.", tutorialStepCommand, []string{":ls", ":buffers"}, "A buffer list would show main plus agent conversations."),
		step("buffer.switch", "Switch to main buffer", "Practice direct buffer navigation.", tutorialStepCommand, []string{":b main"}, "The center pane would switch back to the main orchestrator conversation."),
		step("palette.open", "Open command palette", "Use the searchable command catalog when you forget a command.", tutorialPress, nil, "The palette would open; search by title, key, category, or keyword.", "ctrl+k", "?"),
		step("item.copy", "Copy selected item", "Use keyboard copy for the selected message, file, artifact, or code block.", tutorialPress, nil, "The selected item would be copied to the clipboard.", "y"),
		step("item.copy.select", "Use visual selection", "Release mouse capture when you want terminal-native selection and Cmd+C.", tutorialPress, nil, "FreeCode would enter visual copy mode for native terminal selection.", "v"),
		step("mouse.toggle", "Toggle mouse capture", "Switch between pane mouse scrolling and native text selection.", tutorialPress, nil, "Mouse capture would toggle.", "m"),
		step("diagnostics.doctor", "Run doctor", "Practice the diagnostic command for provider, config, git, terminal, editor, and MCP setup.", tutorialStepCommand, []string{":doctor"}, "Doctor would run diagnostics and show actionable setup errors."),
	}
}

func (m model) commandByID(id string) workbench.Command {
	for _, command := range m.commandRegistry().Commands() {
		if command.ID == id {
			return command
		}
	}
	return workbench.Command{}
}

func tutorialKeyMatches(key string, wants []string) bool {
	key = normalizeTutorialKey(key)
	for _, want := range wants {
		if key == normalizeTutorialKey(want) {
			return true
		}
	}
	return false
}

func tutorialInputMatches(input string, expected []string) bool {
	input = normalizeTutorialLine(input)
	for _, want := range expected {
		if input == normalizeTutorialLine(want) {
			return true
		}
	}
	return false
}

func tutorialInstruction(step tutorialStep) string {
	switch step.Kind {
	case tutorialPrompt:
		return fmt.Sprintf("press i, type %q, then Enter", step.Expected[0])
	case tutorialStepCommand:
		return "type " + strings.Join(step.Expected, " or ")
	default:
		return "press " + strings.Join(step.Keys, " or ")
	}
}

func tutorialShortTarget(step tutorialStep) string {
	switch step.Kind {
	case tutorialPrompt:
		return "i + prompt"
	case tutorialStepCommand:
		return strings.Join(step.Expected, "/")
	default:
		return strings.Join(step.Keys, "/")
	}
}

func tutorialCommandPrefix(step tutorialStep) string {
	for _, want := range step.Expected {
		want = strings.TrimSpace(want)
		if strings.HasPrefix(want, "!") {
			return "!"
		}
	}
	return ":"
}

func tutorialTypedText(msg tea.KeyMsg) string {
	if len(msg.Runes) > 0 {
		return string(msg.Runes)
	}
	if msg.String() == "space" {
		return " "
	}
	return ""
}

func tutorialDisplayKey(key string) string {
	if key == " " || key == "space" {
		return "space"
	}
	return key
}

func normalizeTutorialKey(key string) string {
	key = strings.TrimSpace(strings.ToLower(key))
	switch key {
	case "return":
		return "enter"
	default:
		return key
	}
}

func normalizeTutorialLine(line string) string {
	return strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(line)), " "))
}
