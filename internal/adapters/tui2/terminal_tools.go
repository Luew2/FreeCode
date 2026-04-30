package tui2

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	coremodel "github.com/Luew2/FreeCode/internal/core/model"
	"github.com/Luew2/FreeCode/internal/ports"
)

var _ ports.ToolRegistry = terminalToolRegistry{}

type terminalToolRegistry struct {
	term *terminalSession
	slot int
}

type terminalReadArgs struct {
	Lines int `json:"lines"`
}

type terminalWriteArgs struct {
	Text    string `json:"text"`
	Command string `json:"command"`
	Enter   *bool  `json:"enter"`
}

func newTerminalToolRegistry(term *terminalSession, slot int) ports.ToolRegistry {
	if term == nil {
		return nil
	}
	return terminalToolRegistry{term: term, slot: slot}
}

func (r terminalToolRegistry) Tools() []coremodel.ToolSpec {
	return []coremodel.ToolSpec{
		{
			Name:        "terminal_read",
			Description: "Read recent output from the explicitly shared FreeCode terminal. If the user asks whether you can see or use the terminal, call this tool instead of answering from memory.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"lines": map[string]any{"type": "integer", "description": "Number of recent terminal lines to read. Defaults to 120."},
				},
			},
		},
		{
			Name:        "terminal_write",
			Description: "Write text or a command to the explicitly shared FreeCode terminal. If the user asks you to run a terminal command, call this tool; do not merely say you will run it. Use terminal_read after commands.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"text":    map[string]any{"type": "string", "description": "Raw text to send to the terminal."},
					"command": map[string]any{"type": "string", "description": "Command to send to the terminal. If set, Enter defaults to true."},
					"enter":   map[string]any{"type": "boolean", "description": "Whether to press Enter after sending text."},
				},
			},
		},
	}
}

func (r terminalToolRegistry) RunTool(ctx context.Context, call coremodel.ToolCall) (ports.ToolResult, error) {
	switch call.Name {
	case "terminal_read":
		var args terminalReadArgs
		if len(call.Arguments) > 0 {
			if err := json.Unmarshal(call.Arguments, &args); err != nil {
				return ports.ToolResult{}, fmt.Errorf("terminal_read arguments: %w", err)
			}
		}
		lines := args.Lines
		if lines <= 0 {
			lines = 120
		}
		output := r.term.tail(lines)
		if strings.TrimSpace(output) == "" {
			output = "(terminal has no visible output)"
		}
		return ports.ToolResult{
			CallID:  call.ID,
			Content: fmt.Sprintf("terminal %d recent output:\n%s", r.slot, output),
			Metadata: map[string]string{
				"terminal_slot": fmt.Sprintf("%d", r.slot),
				"tool":          "terminal_read",
			},
		}, nil
	case "terminal_write":
		var args terminalWriteArgs
		if err := json.Unmarshal(call.Arguments, &args); err != nil {
			return ports.ToolResult{}, fmt.Errorf("terminal_write arguments: %w", err)
		}
		text := args.Text
		enter := false
		if strings.TrimSpace(args.Command) != "" {
			text = args.Command
			enter = true
		}
		if args.Enter != nil {
			enter = *args.Enter
		}
		if strings.TrimSpace(text) == "" {
			return ports.ToolResult{}, fmt.Errorf("terminal_write requires text or command")
		}
		if err := r.term.directWrite(text, enter); err != nil {
			return ports.ToolResult{}, err
		}
		action := "wrote text"
		if enter {
			action = "sent command"
		}
		return ports.ToolResult{
			CallID:  call.ID,
			Content: fmt.Sprintf("terminal %d %s. Call terminal_read next before answering so you can inspect the output.", r.slot, action),
			Metadata: map[string]string{
				"terminal_slot": fmt.Sprintf("%d", r.slot),
				"tool":          "terminal_write",
			},
		}, nil
	default:
		return ports.ToolResult{}, fmt.Errorf("unknown terminal tool %q", call.Name)
	}
}
