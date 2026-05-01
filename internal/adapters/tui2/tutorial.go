package tui2

import (
	"fmt"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/Luew2/FreeCode/internal/app/workbench"
)

type tutorialGame struct {
	step      int
	completed map[string]bool
	last      string
}

type tutorialStep struct {
	ID          string
	Title       string
	Description string
	CommandID   string
	Keys        []string
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
	m.tutorial.step = clamp(m.tutorial.step, 0, max(0, len(m.tutorialSteps())-1))
	m.state.Notice = "tutorial opened"
	return m
}

func (m model) handleTutorialKey(key string) (tea.Model, tea.Cmd) {
	steps := m.tutorialSteps()
	if len(steps) == 0 {
		m.overlay = overlayNone
		return m, nil
	}
	current := steps[clamp(m.tutorial.step, 0, len(steps)-1)]
	switch key {
	case "esc", "q":
		m.overlay = overlayNone
		m.mode = modeNormal
		m.state.Notice = "tutorial closed"
		return m, nil
	}
	if tutorialKeyMatches(key, current.Keys) {
		if m.tutorial.completed == nil {
			m.tutorial.completed = map[string]bool{}
		}
		m.tutorial.completed[current.ID] = true
		m.tutorial.last = "passed: " + current.Title
		if m.tutorial.step < len(steps)-1 {
			m.tutorial.step++
		}
		return m, nil
	}
	switch key {
	case "j", "down", "n", "enter":
		m.tutorial.Move(1)
		return m, nil
	case "k", "up", "p":
		m.tutorial.Move(-1)
		return m, nil
	case "home", "g":
		m.tutorial.step = 0
		return m, nil
	case "end", "G":
		m.tutorial.step = len(steps) - 1
		return m, nil
	}
	m.tutorial.last = fmt.Sprintf("try %s for %s", strings.Join(current.Keys, " or "), current.Title)
	return m, nil
}

func (g *tutorialGame) Move(delta int) {
	g.step += delta
	if g.step < 0 {
		g.step = 0
	}
}

func (m model) tutorialView() string {
	steps := m.tutorialSteps()
	width := m.modalContentWidth(92)
	height := max(12, min(max(12, m.height-6), 34))
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
	command := m.commandByID(current.CommandID)
	keybinding := strings.Join(current.Keys, " or ")
	if command.Keybinding != "" {
		keybinding = command.Keybinding
	}

	lines := []string{
		titleStyle.Render("FreeCode Tutorial"),
		mutedStyle.Render(fmt.Sprintf("progress %d/%d  j/k or n/p browse  press the requested key to score  Esc close", completed, len(steps))),
		"",
		selectedStyle.Render(fmt.Sprintf("Mission %d: %s", m.tutorial.step+1, current.Title)),
		current.Description,
		fmt.Sprintf("Try: %s", keybinding),
	}
	if command.Description != "" {
		lines = append(lines, mutedStyle.Render("Why: "+truncate(command.Description, max(20, width-8))))
	}
	if m.tutorial.completed[current.ID] {
		lines = append(lines, selectedStyle.Render("Complete"))
	}
	if m.tutorial.last != "" {
		lines = append(lines, mutedStyle.Render(m.tutorial.last))
	}
	lines = append(lines, "", titleStyle.Render("Training Board"))
	start, end := visibleRange(len(steps), m.tutorial.step, max(4, height-len(lines)-4))
	if start > 0 {
		lines = append(lines, mutedStyle.Render("..."))
	}
	for i := start; i < end; i++ {
		step := steps[i]
		mark := " "
		if m.tutorial.completed[step.ID] {
			mark = "x"
		}
		key := strings.Join(step.Keys, "/")
		if cmd := m.commandByID(step.CommandID); cmd.Keybinding != "" {
			key = cmd.Keybinding
		}
		row := fmt.Sprintf("[%s] %-18s %s", mark, key, step.Title)
		lines = append(lines, selectableLine(i == m.tutorial.step, "", row))
	}
	if end < len(steps) {
		lines = append(lines, mutedStyle.Render("..."))
	}
	lines = append(lines, "", mutedStyle.Render("Full command catalog remains in Ctrl+K; this game drills the muscle-memory path."))
	return modalStyle.Width(width).Render(fitLines(lines, width-4, height))
}

func (m model) tutorialSteps() []tutorialStep {
	commands := m.commandRegistry().Commands()
	byID := map[string]workbench.Command{}
	for _, command := range commands {
		byID[command.ID] = command
	}
	step := func(id, title, description string, keys ...string) tutorialStep {
		if command, ok := byID[id]; ok {
			if title == "" {
				title = command.Title
			}
			if description == "" {
				description = command.Description
			}
			if len(keys) == 0 {
				keys = nonCommandKeys(command)
			}
		}
		return tutorialStep{ID: id, Title: title, Description: description, CommandID: id, Keys: keys}
	}
	return []tutorialStep{
		step("prompt.insert", "Start talking", "Press i to open the composer for the active agent chat."),
		step("prompt.send", "Send from the composer", "Enter sends the focused prompt. In normal chat, Enter opens the composer.", "enter"),
		step("context.cancel", "Know the stop command", "Use :cancel when a model/tool run needs to stop.", ":cancel"),
		step("agent.swarm", "Launch swarm mode", "Use :s or :swarm for a long-running orchestrated task.", ":s", ":swarm"),
		step("item.open", "Open the selected thing", "Enter or o activates the selected agent, file, artifact, or message.", "enter", "o"),
		step("item.detail", "Inspect details", "d opens the selected item without changing panes."),
		step("item.copy", "Copy quickly", "y copies the selected message, file path, artifact, or code block."),
		step("item.copy.select", "Use visual copy", "v releases mouse capture and expands the focused pane for terminal-native selection."),
		step("mouse.toggle", "Toggle mouse mode", "m toggles between terminal mouse capture and normal text selection."),
		step("palette.open", "Open the palette", "Ctrl+K opens searchable commands and cheat sheets.", "ctrl+k", "?"),
		step("shell.run", "Run a quick shell command", "! runs a one-shot local command and logs it as local-only context."),
		step("terminal.open", "Open a persistent terminal", ":term opens the terminal tab; Enter focuses it from that tab.", ":term", ":t"),
		step("terminal.share", "Share terminal control", ":st grants the agent live read/write access to the selected terminal.", ":st"),
		step("terminal.share_output", "Attach terminal output", ":sto attaches recent terminal output without granting live control.", ":sto"),
		step("context.inspect", "Preview model context", ":context shows what the next model turn will see.", ":context"),
		step("tab.ops", "Watch operations", ":ops opens active runs, queues, approvals, and terminal sharing state.", ":ops"),
		step("buffer.list", "List conversations", ":ls treats main and agents as Vim-style buffers.", ":ls", ":buffers"),
		step("buffer.switch", "Switch conversations", ":b main or :b a1 jumps between conversation buffers.", ":b"),
		step("diagnostics.doctor", "Run doctor", ":doctor checks provider, config, git, terminal, editor, and MCP setup.", ":doctor"),
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
		if strings.HasPrefix(strings.TrimSpace(want), ":") && key == ":" {
			return true
		}
		if key == normalizeTutorialKey(want) {
			return true
		}
	}
	return false
}

func normalizeTutorialKey(key string) string {
	key = strings.TrimSpace(strings.ToLower(key))
	if key == ":" {
		return key
	}
	key = strings.TrimPrefix(key, ":")
	switch key {
	case "return":
		return "enter"
	case "ctrl+k", "ctrl+h", "ctrl+l", "ctrl+e", "ctrl+y":
		return key
	default:
		return key
	}
}

func nonCommandKeys(command workbench.Command) []string {
	var keys []string
	for _, key := range command.Keybindings {
		key = strings.TrimSpace(key)
		if key == "" || strings.HasPrefix(key, ":") {
			continue
		}
		keys = append(keys, strings.ToLower(key))
	}
	sort.Strings(keys)
	if len(keys) == 0 && command.Keybinding != "" && !strings.HasPrefix(command.Keybinding, ":") {
		keys = append(keys, strings.ToLower(command.Keybinding))
	}
	return keys
}
