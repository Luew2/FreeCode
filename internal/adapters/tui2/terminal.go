package tui2

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	tea "github.com/charmbracelet/bubbletea"
	xansi "github.com/charmbracelet/x/ansi"
	"github.com/creack/pty"
)

const terminalMaxLines = 2000

type terminalOutputMsg struct {
	term *terminalSession
	text string
	err  error
}

type terminalSession struct {
	mu       sync.Mutex
	active   bool
	reading  bool
	expanded bool
	shell    string
	ptmx     *os.File
	cmd      *exec.Cmd
	output   chan terminalOutputMsg
	lines    []string
	partial  string
	carriage bool
	input    string
}

func newTerminalSession() *terminalSession {
	return &terminalSession{}
}

func (t *terminalSession) start(root string, width int, height int) error {
	if t == nil {
		return errors.New("terminal is unavailable")
	}
	if t.active {
		return nil
	}
	shell := strings.TrimSpace(os.Getenv("SHELL"))
	if shell == "" {
		shell = "/bin/sh"
	}
	cmd := terminalCommand(shell)
	if strings.TrimSpace(root) != "" {
		cmd.Dir = root
	}
	cmd.Env = terminalEnv(os.Environ())
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{
		Cols: uint16(clamp(width, 20, 240)),
		Rows: uint16(clamp(height, 4, 80)),
	})
	if err != nil {
		return err
	}
	t.active = true
	t.shell = shell
	t.ptmx = ptmx
	t.cmd = cmd
	t.output = make(chan terminalOutputMsg, 64)
	t.lines = append(t.lines, "terminal started: "+shell)
	go t.readLoop()
	return nil
}

func terminalCommand(shell string) *exec.Cmd {
	switch filepath.Base(shell) {
	case "zsh":
		return exec.Command(shell, "-f")
	case "bash":
		return exec.Command(shell, "--noprofile", "--norc")
	case "fish":
		return exec.Command(shell, "--no-config")
	default:
		return exec.Command(shell)
	}
}

func terminalEnv(base []string) []string {
	drop := map[string]bool{
		"PS1": true, "PROMPT": true, "RPROMPT": true, "RPS1": true,
		"STARSHIP_CONFIG": true, "POWERLEVEL9K_DISABLE_CONFIGURATION_WIZARD": true,
		"CLICOLOR_FORCE": true, "FORCE_COLOR": true, "NO_COLOR": true,
		"TERM": true, "FREECODE_TERMINAL": true,
	}
	env := make([]string, 0, len(base)+8)
	for _, item := range base {
		key, _, ok := strings.Cut(item, "=")
		if ok && drop[key] {
			continue
		}
		env = append(env, item)
	}
	env = append(env,
		"TERM=dumb",
		"NO_COLOR=1",
		"FORCE_COLOR=0",
		"CLICOLOR_FORCE=0",
		"FREECODE_TERMINAL=1",
		"PS1=\\u:\\w \\$ ",
		"PROMPT=%n:%~ %# ",
		"RPROMPT=",
	)
	return env
}

func (t *terminalSession) readLoop() {
	buf := make([]byte, 4096)
	for {
		n, err := t.ptmx.Read(buf)
		if n > 0 {
			t.output <- terminalOutputMsg{text: string(buf[:n])}
		}
		if err != nil {
			if err != io.EOF {
				t.output <- terminalOutputMsg{err: err}
			}
			close(t.output)
			return
		}
	}
}

func (t *terminalSession) stop() {
	if t == nil {
		return
	}
	if t.ptmx != nil {
		_ = t.ptmx.Close()
	}
	if t.cmd != nil && t.cmd.Process != nil {
		_ = t.cmd.Process.Kill()
		_, _ = t.cmd.Process.Wait()
	}
	t.active = false
}

func (t *terminalSession) resize(width int, height int) {
	if t == nil || t.ptmx == nil || !t.active {
		return
	}
	_ = pty.Setsize(t.ptmx, &pty.Winsize{
		Cols: uint16(clamp(width, 20, 240)),
		Rows: uint16(clamp(height, 4, 80)),
	})
}

func (t *terminalSession) writeKey(msg tea.KeyMsg) {
	if t == nil || !t.active {
		return
	}
	var text string
	switch msg.String() {
	case "enter":
		if t.shouldClearForInput() {
			t.clearDisplay()
		}
		t.input = ""
		text = "\r"
	case "backspace", "ctrl+h":
		t.input = removeLastRune(t.input)
		text = "\x7f"
	case "delete":
		text = "\x1b[3~"
	case "tab":
		text = "\t"
	case "esc":
		text = "\x1b"
	case "ctrl+c":
		t.input = ""
		text = "\x03"
	case "ctrl+d":
		text = "\x04"
	case "ctrl+l":
		t.clearDisplay()
		text = "\x0c"
	case "left":
		text = "\x1b[D"
	case "right":
		text = "\x1b[C"
	case "up":
		text = "\x1b[A"
	case "down":
		text = "\x1b[B"
	case "home":
		text = "\x1b[H"
	case "end":
		text = "\x1b[F"
	default:
		if len(msg.Runes) > 0 {
			text = string(msg.Runes)
			t.input += text
		}
	}
	if text != "" && t.ptmx != nil {
		_, _ = t.ptmx.Write([]byte(text))
	}
}

func (t *terminalSession) shouldClearForInput() bool {
	command := strings.TrimSpace(t.input)
	return command == "clear" || command == "reset"
}

func (t *terminalSession) clearDisplay() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lines = nil
	t.partial = ""
	t.carriage = false
}

func (t *terminalSession) appendOutput(text string) {
	if t == nil || text == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if terminalClearsScreen(text) {
		t.lines = nil
		t.partial = ""
		t.carriage = false
	}
	text = sanitizeTerminalOutput(text)
	if text == "" {
		return
	}
	current := t.partial
	carriage := t.carriage
	t.carriage = false
	for _, r := range text {
		switch r {
		case '\r':
			carriage = true
		case '\b', 0x7f:
			current = removeLastRune(current)
			carriage = false
		case '\n':
			if strings.TrimSpace(current) != "" {
				t.lines = append(t.lines, current)
			}
			current = ""
			carriage = false
		default:
			if carriage {
				current = ""
				carriage = false
			}
			current += string(r)
		}
	}
	t.partial = current
	t.carriage = carriage
	t.trim()
}

func sanitizeTerminalOutput(text string) string {
	if text == "" {
		return ""
	}
	text = ansiPattern.ReplaceAllString(text, "")
	text = oscColorPattern.ReplaceAllString(text, "$1")
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' || r == '\b' {
			return r
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, text)
}

func terminalClearsScreen(text string) bool {
	return strings.Contains(text, "\x1bc") ||
		(strings.Contains(text, "\x1b[H") && (strings.Contains(text, "\x1b[2J") || strings.Contains(text, "\x1b[J"))) ||
		strings.Contains(text, "\x1b[3J")
}

func removeLastRune(text string) string {
	if text == "" {
		return ""
	}
	runes := []rune(text)
	return string(runes[:len(runes)-1])
}

func (t *terminalSession) markStopped(err error) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.active = false
	t.reading = false
	if err != nil {
		t.lines = append(t.lines, "terminal stopped: "+err.Error())
	} else {
		t.lines = append(t.lines, "terminal stopped")
	}
	t.trim()
}

func (t *terminalSession) trim() {
	if len(t.lines) > terminalMaxLines {
		t.lines = append([]string(nil), t.lines[len(t.lines)-terminalMaxLines:]...)
	}
}

func (t *terminalSession) view(width int, height int) string {
	if t == nil {
		return fitLines([]string{mutedStyle.Render("No terminal")}, width, height)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	var lines []string
	lines = append(lines, t.lines...)
	if t.partial != "" {
		lines = append(lines, t.partial)
	}
	if len(lines) == 0 {
		lines = []string{"Terminal is stopped. Run :term to start it."}
	}
	start := max(0, len(lines)-height)
	visible := lines[start:]
	for i, line := range visible {
		visible[i] = xansi.Truncate(line, width, "…")
	}
	return fitLines(visible, width, height)
}

func (t *terminalSession) tail(limit int) string {
	if t == nil {
		return ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	var lines []string
	lines = append(lines, t.lines...)
	if t.partial != "" {
		lines = append(lines, t.partial)
	}
	if limit <= 0 || limit > len(lines) {
		limit = len(lines)
	}
	if limit == 0 {
		return ""
	}
	return strings.TrimSpace(strings.Join(lines[len(lines)-limit:], "\n"))
}

func (t *terminalSession) directStatus(slot int) string {
	if t == nil {
		return "terminal is unavailable"
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	state := "stopped"
	if t.active {
		state = "running"
	}
	return strings.TrimSpace(strings.Join([]string{
		"terminal: " + strconv.Itoa(slot),
		"state: " + state,
		"shell: " + t.shell,
		"tail:",
		t.tailLocked(80),
	}, "\n"))
}

func (t *terminalSession) directWrite(text string, enter bool) error {
	if t == nil {
		return errors.New("terminal is unavailable")
	}
	t.mu.Lock()
	if !t.active || t.ptmx == nil {
		t.mu.Unlock()
		return errors.New("terminal is not running")
	}
	if enter && (strings.TrimSpace(text) == "clear" || strings.TrimSpace(text) == "reset") {
		t.lines = nil
		t.partial = ""
	}
	raw := text
	if enter {
		t.input = ""
		raw += "\r"
	} else {
		t.input += text
	}
	ptmx := t.ptmx
	t.mu.Unlock()
	_, err := ptmx.Write([]byte(raw))
	return err
}

func (t *terminalSession) tailLocked(limit int) string {
	var lines []string
	lines = append(lines, t.lines...)
	if t.partial != "" {
		lines = append(lines, t.partial)
	}
	if limit <= 0 || limit > len(lines) {
		limit = len(lines)
	}
	if limit == 0 {
		return ""
	}
	return strings.TrimSpace(strings.Join(lines[len(lines)-limit:], "\n"))
}
