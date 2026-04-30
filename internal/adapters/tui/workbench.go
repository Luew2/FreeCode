package tui

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/Luew2/FreeCode/internal/app/workbench"
	"github.com/Luew2/FreeCode/internal/core/permission"
)

const defaultWidth = 100

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
	RunShell(ctx context.Context, command string) (workbench.State, error)
}

type Options struct {
	In         io.Reader
	Out        io.Writer
	Workbench  Controller
	Initial    workbench.State
	InitialSet bool
	Width      int
}

func Run(ctx context.Context, opts Options) error {
	if opts.In == nil {
		opts.In = strings.NewReader("")
	}
	if opts.Out == nil {
		return errors.New("tui output is required")
	}
	if opts.Width <= 0 {
		opts.Width = defaultWidth
	}

	state := opts.Initial
	if !opts.InitialSet {
		if opts.Workbench == nil {
			return errors.New("workbench controller is required")
		}
		loaded, err := opts.Workbench.Load(ctx)
		if err != nil {
			return err
		}
		state = loaded
	}
	if state.Mode == "" {
		state.Mode = "NORMAL"
	}
	if _, err := io.WriteString(opts.Out, Render(state, opts.Width)); err != nil {
		return err
	}

	scanner := bufio.NewScanner(opts.In)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		next, quit, err := handleCommand(ctx, opts.Workbench, state, line)
		if err != nil {
			state.Notice = "error: " + err.Error()
		} else if !quit {
			state = next
		}
		if quit {
			return nil
		}
		if _, err := io.WriteString(opts.Out, "\n"+Render(state, opts.Width)); err != nil {
			return err
		}
	}
	return scanner.Err()
}

func Render(state workbench.State, width int) string {
	if width <= 0 {
		width = defaultWidth
	}
	leftWidth := 22
	rightWidth := 28
	midWidth := width - leftWidth - rightWidth - 6
	if midWidth < 28 {
		midWidth = 28
	}

	var b strings.Builder
	header := fmt.Sprintf("freecode  provider:%s  model:%s  %s  tokens:~%d  approval:%s",
		emptyDash(state.Provider),
		emptyDash(state.Model),
		branchLabel(state.Branch),
		state.TokenEstimate,
		state.Approval,
	)
	b.WriteString(truncate(header, width))
	b.WriteByte('\n')
	b.WriteString(strings.Repeat("=", min(width, len(header))))
	b.WriteByte('\n')
	b.WriteString(fmt.Sprintf("%-*s | %-*s | %-*s\n", leftWidth, "Sessions / Agents", midWidth, "Transcript", rightWidth, "Context / Artifacts"))
	b.WriteString(fmt.Sprintf("%-*s-+-%-*s-+-%-*s\n", leftWidth, strings.Repeat("-", leftWidth), midWidth, strings.Repeat("-", midWidth), rightWidth, strings.Repeat("-", rightWidth)))

	left := agentLines(state.Agents, leftWidth)
	mid := transcriptLines(state.Transcript, midWidth)
	right := artifactLines(state.Artifacts, rightWidth)
	rows := max(len(left), max(len(mid), len(right)))
	if rows == 0 {
		rows = 1
	}
	for i := 0; i < rows; i++ {
		b.WriteString(fmt.Sprintf("%-*s | %-*s | %-*s\n",
			leftWidth, lineAt(left, i, leftWidth),
			midWidth, lineAt(mid, i, midWidth),
			rightWidth, lineAt(right, i, rightWidth),
		))
	}

	if state.Detail.ID != "" {
		b.WriteString("\n")
		b.WriteString(truncate(state.Detail.ID+" "+state.Detail.Title, width))
		b.WriteByte('\n')
		for _, line := range splitPreview(state.Detail.Body, width) {
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	if state.Notice != "" {
		b.WriteString("\n")
		b.WriteString(truncate(state.Notice, width))
		b.WriteByte('\n')
	}
	b.WriteString("\n")
	b.WriteString(truncate(fmt.Sprintf("%s  i/:i <prompt>  :s/:swarm <prompt>  :! shell  :agent <prompt>  y/Y copy  o/:o open  d detail  a/r approve/reject  :compact  ? help  q quit", modeLabel(state.Mode)), width))
	b.WriteByte('\n')
	return b.String()
}

func handleCommand(ctx context.Context, controller Controller, state workbench.State, line string) (workbench.State, bool, error) {
	switch {
	case line == "q" || line == ":q" || line == "quit":
		return state, true, nil
	case line == "?" || line == ":help" || line == "ctrl-k" || line == ":o":
		if controller == nil {
			return state, false, errors.New("workbench controller is required")
		}
		next, err := controller.Palette(ctx)
		return next, false, err
	case line == "ctrl-a" || line == ":approval cycle":
		return setApproval(ctx, controller, state.Approval.Cycle())
	case strings.HasPrefix(line, "!"):
		return runShell(ctx, controller, strings.TrimSpace(strings.TrimPrefix(line, "!")))
	case strings.HasPrefix(line, ":!"):
		return runShell(ctx, controller, strings.TrimSpace(strings.TrimPrefix(line, ":!")))
	case line == ":compact" || line == "/compact":
		if controller == nil {
			return state, false, errors.New("workbench controller is required")
		}
		next, err := controller.Compact(ctx)
		return next, false, err
	case strings.HasPrefix(line, ":approval "):
		mode, err := permission.ParseMode(strings.TrimSpace(strings.TrimPrefix(line, ":approval ")))
		if err != nil {
			return state, false, err
		}
		if mode == permission.ModeDanger {
			state.Notice = "type :danger confirm to enable danger mode"
			return state, false, nil
		}
		return setApproval(ctx, controller, mode)
	case line == ":danger":
		state.Notice = "type :danger confirm to enable danger mode"
		return state, false, nil
	case line == ":danger confirm":
		return setApproval(ctx, controller, permission.ModeDanger)
	case strings.HasPrefix(line, "i "):
		return submit(ctx, controller, state.Approval, strings.TrimSpace(strings.TrimPrefix(line, "i ")), false)
	case strings.HasPrefix(line, ":i "):
		return submit(ctx, controller, state.Approval, strings.TrimSpace(strings.TrimPrefix(line, ":i ")), false)
	case strings.HasPrefix(line, "send "):
		return submit(ctx, controller, state.Approval, strings.TrimSpace(strings.TrimPrefix(line, "send ")), false)
	case strings.HasPrefix(line, ":send "):
		return submit(ctx, controller, state.Approval, strings.TrimSpace(strings.TrimPrefix(line, ":send ")), false)
	case strings.HasPrefix(line, ":swarm "):
		return submit(ctx, controller, state.Approval, strings.TrimSpace(strings.TrimPrefix(line, ":swarm ")), true)
	case strings.HasPrefix(line, ":s "):
		return submit(ctx, controller, state.Approval, strings.TrimSpace(strings.TrimPrefix(line, ":s ")), true)
	case strings.HasPrefix(line, "swarm "):
		return submit(ctx, controller, state.Approval, strings.TrimSpace(strings.TrimPrefix(line, "swarm ")), true)
	case strings.HasPrefix(line, ":agent "):
		return submit(ctx, controller, state.Approval, strings.TrimSpace(strings.TrimPrefix(line, ":agent ")), false)
	case strings.HasPrefix(line, "y ") || strings.HasPrefix(line, "Y "):
		if controller == nil {
			return state, false, errors.New("workbench controller is required")
		}
		withFences := strings.HasPrefix(line, "Y ")
		next, err := controller.Copy(ctx, strings.TrimSpace(line[2:]), withFences)
		return next, false, err
	case strings.HasPrefix(line, "o "):
		if controller == nil {
			return state, false, errors.New("workbench controller is required")
		}
		next, err := controller.Open(ctx, strings.TrimSpace(line[2:]))
		return next, false, err
	case strings.HasPrefix(line, ":o "):
		if controller == nil {
			return state, false, errors.New("workbench controller is required")
		}
		next, err := controller.Open(ctx, strings.TrimSpace(line[3:]))
		return next, false, err
	case strings.HasPrefix(line, "d "):
		if controller == nil {
			return state, false, errors.New("workbench controller is required")
		}
		next, err := controller.Detail(ctx, strings.TrimSpace(line[2:]))
		return next, false, err
	case strings.HasPrefix(line, ":d "):
		if controller == nil {
			return state, false, errors.New("workbench controller is required")
		}
		next, err := controller.Detail(ctx, strings.TrimSpace(line[3:]))
		return next, false, err
	case strings.HasPrefix(line, "a "):
		if controller == nil {
			return state, false, errors.New("workbench controller is required")
		}
		next, err := controller.Approve(ctx, strings.TrimSpace(line[2:]))
		return next, false, err
	case strings.HasPrefix(line, "r "):
		if controller == nil {
			return state, false, errors.New("workbench controller is required")
		}
		next, err := controller.Reject(ctx, strings.TrimSpace(line[2:]))
		return next, false, err
	default:
		return state, false, fmt.Errorf("unknown command %q", line)
	}
}

func runShell(ctx context.Context, controller Controller, command string) (workbench.State, bool, error) {
	if controller == nil {
		return workbench.State{}, false, errors.New("workbench controller is required")
	}
	next, err := controller.RunShell(ctx, command)
	return next, false, err
}

func submit(ctx context.Context, controller Controller, approval permission.Mode, text string, swarm bool) (workbench.State, bool, error) {
	if controller == nil {
		return workbench.State{}, false, errors.New("workbench controller is required")
	}
	next, err := controller.SubmitPrompt(ctx, workbench.SubmitRequest{Text: text, Approval: approval, Swarm: swarm})
	return next, false, err
}

func setApproval(ctx context.Context, controller Controller, mode permission.Mode) (workbench.State, bool, error) {
	if controller == nil {
		return workbench.State{}, false, errors.New("workbench controller is required")
	}
	next, err := controller.SetApproval(ctx, mode)
	return next, false, err
}

func transcriptLines(items []workbench.TranscriptItem, width int) []string {
	var lines []string
	for _, item := range items {
		prefix := item.ID + " " + item.Actor + " "
		for _, line := range compactLines(item.Text, width-len(prefix)) {
			lines = append(lines, truncate(prefix+line, width))
			prefix = strings.Repeat(" ", len(prefix))
		}
	}
	if len(lines) == 0 {
		return []string{"No messages yet"}
	}
	return lines
}

func artifactLines(items []workbench.Item, width int) []string {
	var lines []string
	for _, item := range items {
		title := item.Title
		if title == "" {
			title = item.URI
		}
		lines = append(lines, truncate(fmt.Sprintf("%-4s %-7s %s", item.ID, item.Kind, title), width))
	}
	if len(lines) == 0 {
		return []string{"No artifacts yet"}
	}
	return lines
}

func agentLines(items []workbench.AgentItem, width int) []string {
	lines := []string{"current"}
	for _, item := range items {
		label := item.Name
		if label == "" {
			label = item.Role
		}
		if item.Status != "" {
			label += " " + item.Status
		}
		lines = append(lines, truncate(item.ID+" "+label, width))
	}
	if len(lines) == 1 {
		lines = append(lines, "agents idle")
	}
	return lines
}

func splitPreview(text string, width int) []string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if len(lines) > 10 {
		lines = append(lines[:10], "...")
	}
	for i := range lines {
		lines[i] = truncate(lines[i], width)
	}
	return lines
}

func compactLines(text string, width int) []string {
	text = strings.TrimSpace(strings.ReplaceAll(text, "\t", " "))
	if text == "" {
		return []string{""}
	}
	raw := strings.Split(text, "\n")
	var lines []string
	for _, line := range raw {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		lines = append(lines, truncate(line, width))
	}
	if len(lines) == 0 {
		return []string{""}
	}
	return lines
}

func lineAt(lines []string, index int, width int) string {
	if index >= len(lines) {
		return ""
	}
	return truncate(lines[index], width)
}

func truncate(value string, width int) string {
	if width <= 0 {
		return ""
	}
	if len(value) <= width {
		return value
	}
	if width <= 3 {
		return value[:width]
	}
	return value[:width-3] + "..."
}

func branchLabel(branch string) string {
	if strings.TrimSpace(branch) == "" {
		return "branch:-"
	}
	return "branch:" + branch
}

func emptyDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return value
}

func modeLabel(value string) string {
	if strings.TrimSpace(value) == "" {
		return "NORMAL"
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
