package tui2

import (
	"fmt"
	"strings"
	"testing"

	xansi "github.com/charmbracelet/x/ansi"

	"github.com/Luew2/FreeCode/internal/app/workbench"
)

func TestChatRendererSpacingAndSanitization(t *testing.T) {
	chat := newChatRenderer(50, 20)
	chat.SetItems([]workbench.TranscriptItem{
		{ID: "m1", Kind: workbench.TranscriptUser, Actor: "user", Text: "\x1b]11;rgb:0000\x07hello"},
		{ID: "m2", Kind: workbench.TranscriptAssistant, Actor: "assistant", Text: "## Answer\n\nworld"},
	})

	view := stripANSI(chat.View())
	if strings.Contains(view, "]11;rgb") || strings.Contains(view, "\x1b") {
		t.Fatalf("view contains terminal junk:\n%q", view)
	}
	if !strings.Contains(view, "m1 you\n  hello\n\n▌ m2 assistant") {
		t.Fatalf("view spacing = %q, want one blank line between cells", view)
	}
	if strings.Contains(view, "\n\n\n") {
		t.Fatalf("view has excessive blank lines:\n%s", view)
	}
}

func TestChatRendererWrapsToWidth(t *testing.T) {
	chat := newChatRenderer(28, 20)
	chat.SetItems([]workbench.TranscriptItem{{
		ID:    "m1",
		Kind:  workbench.TranscriptAssistant,
		Actor: "assistant",
		Text:  "this is a long line that should wrap inside the chat pane instead of running wide",
	}})

	for _, line := range strings.Split(chat.View(), "\n") {
		if width := xansi.StringWidth(line); width > 28 {
			t.Fatalf("line width = %d, want <= 28 for %q\nfull:\n%s", width, line, chat.View())
		}
	}
}

func TestChatRendererPreservesTrustedMarkdownANSI(t *testing.T) {
	chat := newChatRenderer(60, 20)
	chat.SetItems([]workbench.TranscriptItem{{
		ID:    "m1",
		Kind:  workbench.TranscriptAssistant,
		Actor: "assistant",
		Text:  "## Heading\n\n```go\nfmt.Println(\"hi\")\n```",
	}})

	if view := chat.View(); !strings.Contains(view, "\x1b[") {
		t.Fatalf("view has no ANSI styling, want glamour/chroma output:\n%s", view)
	}
}

func TestChatRendererSelectionAndFollowTail(t *testing.T) {
	chat := newChatRenderer(40, 4)
	var items []workbench.TranscriptItem
	for i := 1; i <= 8; i++ {
		items = append(items, workbench.TranscriptItem{
			ID:    fmt.Sprintf("m%d", i),
			Kind:  workbench.TranscriptAssistant,
			Actor: "assistant",
			Text:  "line",
		})
	}
	chat.SetItems(items)
	if id, _ := chat.SelectedID(); id != "m8" {
		t.Fatalf("selected = %q, want tail m8", id)
	}
	if !chat.IsAtBottom() {
		t.Fatalf("chat should follow tail")
	}

	chat.MoveSelection(-3)
	if id, _ := chat.SelectedID(); id != "m5" {
		t.Fatalf("selected = %q, want m5", id)
	}
	if chat.follow {
		t.Fatalf("follow = true after moving up, want false")
	}

	chat.Bottom()
	if id, _ := chat.SelectedID(); id != "m8" || !chat.follow || !chat.IsAtBottom() {
		t.Fatalf("bottom selected/follow/atBottom = %q/%v/%v, want m8/true/true", id, chat.follow, chat.IsAtBottom())
	}
}

func TestChatRendererStreamingCacheInvalidatesOnTextChange(t *testing.T) {
	chat := newChatRenderer(50, 10)
	chat.SetItems([]workbench.TranscriptItem{{
		ID:        "m1",
		Kind:      workbench.TranscriptStreaming,
		Actor:     "assistant streaming",
		Text:      "partial",
		Streaming: true,
	}})
	first := chat.View()

	chat.SetItems([]workbench.TranscriptItem{{
		ID:        "m1",
		Kind:      workbench.TranscriptStreaming,
		Actor:     "assistant streaming",
		Text:      "partial done",
		Streaming: true,
	}})
	second := chat.View()
	if first == second || !strings.Contains(stripANSI(second), "partial done") {
		t.Fatalf("streaming view did not update\nfirst:\n%s\nsecond:\n%s", first, second)
	}
}

func TestChatRendererCollapsesUnselectedToolBodies(t *testing.T) {
	chat := newChatRenderer(80, 20)
	chat.SetItems([]workbench.TranscriptItem{
		{ID: "m1", Kind: workbench.TranscriptTool, Title: "terminal_write", Text: `{"command":"ls"}`, Status: "requested"},
		{ID: "m2", Kind: workbench.TranscriptAssistant, Text: "done"},
	})

	view := stripANSI(chat.View())
	if strings.Contains(view, `{"command":"ls"}`) {
		t.Fatalf("unselected tool body is visible:\n%s", view)
	}
	if !strings.Contains(view, "m1 tool terminal_write requested") {
		t.Fatalf("collapsed tool header missing:\n%s", view)
	}

	chat.MoveSelection(-1)
	view = stripANSI(chat.View())
	if strings.Contains(view, `{"command":"ls"}`) {
		t.Fatalf("selected tool body should stay hidden in transcript:\n%s", view)
	}
}
