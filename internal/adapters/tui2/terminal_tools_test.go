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

// TestFilterReadOnlyTerminalToolsHidesTerminalWrite covers the read-only
// approval mode regression: before the fix, the Submit callback merged
// the unfiltered terminal registry into the session even when the user
// had set approval=read-only, so the model could write to the user's
// terminal. The filter must drop terminal_write from both the advertised
// schema and the runnable surface.
func TestFilterReadOnlyTerminalToolsHidesTerminalWrite(t *testing.T) {
	term := &terminalSession{active: true, lines: []string{"hello"}}
	full := newTerminalToolRegistry(term, 1)

	// Sanity check: the unfiltered registry exposes terminal_write.
	hasWrite := false
	for _, spec := range full.Tools() {
		if spec.Name == "terminal_write" {
			hasWrite = true
		}
	}
	if !hasWrite {
		t.Fatalf("unfiltered registry must expose terminal_write")
	}

	readOnly := FilterReadOnlyTerminalTools(full)
	if readOnly == nil {
		t.Fatalf("FilterReadOnlyTerminalTools returned nil")
	}
	for _, spec := range readOnly.Tools() {
		if spec.Name == "terminal_write" {
			t.Fatalf("read-only tools = %#v, want terminal_write filtered", readOnly.Tools())
		}
	}

	// Even if the model bypasses the schema and asks for terminal_write
	// directly, RunTool must reject it with a clear error.
	_, err := readOnly.RunTool(context.Background(), coremodel.ToolCall{
		ID:        "call_1",
		Name:      "terminal_write",
		Arguments: []byte(`{"command":"echo hi"}`),
	})
	if err == nil {
		t.Fatalf("read-only RunTool returned nil error for terminal_write")
	}
	if !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("error = %q, want read-only message", err.Error())
	}

	// terminal_read must still work in read-only mode — that is the
	// whole point of the filter (preserve observation, drop mutation).
	result, err := readOnly.RunTool(context.Background(), coremodel.ToolCall{
		ID:        "call_2",
		Name:      "terminal_read",
		Arguments: []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("read-only terminal_read returned error: %v", err)
	}
	if !strings.Contains(result.Content, "hello") {
		t.Fatalf("read-only terminal_read content = %q, want shared tail", result.Content)
	}
}

// TestFilterReadOnlyTerminalToolsPassesThroughNonTerminalRegistries
// guards the cli.go code path that calls FilterReadOnlyTerminalTools on
// whatever request.TerminalTools the workbench passed in: when the user
// did not share a terminal, request.TerminalTools is nil and the filter
// must return nil without panicking.
func TestFilterReadOnlyTerminalToolsPassesThroughNonTerminalRegistries(t *testing.T) {
	if got := FilterReadOnlyTerminalTools(nil); got != nil {
		t.Fatalf("nil registry should pass through, got %#v", got)
	}
}
