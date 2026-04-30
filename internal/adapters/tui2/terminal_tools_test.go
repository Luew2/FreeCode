package tui2

import (
	"context"
	"strings"
	"testing"

	coremodel "github.com/Luew2/FreeCode/internal/core/model"
)

func TestTerminalReadToolReturnsSharedTerminalTail(t *testing.T) {
	term := &terminalSession{active: true, lines: []string{"one", "two", "three"}}
	tools := newTerminalToolRegistry(term, 2)
	result, err := tools.RunTool(context.Background(), coremodel.ToolCall{
		ID:        "call_1",
		Name:      "terminal_read",
		Arguments: []byte(`{"lines":2}`),
	})
	if err != nil {
		t.Fatalf("terminal_read returned error: %v", err)
	}
	if !strings.Contains(result.Content, "terminal 2") ||
		!strings.Contains(result.Content, "two") ||
		!strings.Contains(result.Content, "three") ||
		strings.Contains(result.Content, "one") {
		t.Fatalf("content = %q, want selected terminal tail", result.Content)
	}
}
