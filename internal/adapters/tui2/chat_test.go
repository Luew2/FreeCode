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

func TestChatRendererScrollDoesNotMoveSelectionAndGRestoresFollow(t *testing.T) {
	chat := newChatRenderer(50, 5)
	var items []workbench.TranscriptItem
	for i := 1; i <= 10; i++ {
		items = append(items, workbench.TranscriptItem{
			ID:    fmt.Sprintf("m%d", i),
			Kind:  workbench.TranscriptAssistant,
			Actor: "assistant",
			Text:  "line",
		})
	}
	chat.SetItems(items)
	chat.LineUp(3)
	if id, _ := chat.SelectedID(); id != "m10" {
		t.Fatalf("selected = %q, want scroll without selection move", id)
	}
	if chat.follow {
		t.Fatalf("follow = true after manual scroll, want false")
	}
	chat.Bottom()
	if id, _ := chat.SelectedID(); id != "m10" || !chat.follow || !chat.IsAtBottom() {
		t.Fatalf("bottom selected/follow/atBottom = %q/%v/%v", id, chat.follow, chat.IsAtBottom())
	}
}

func TestChatRendererVisibleLayoutClampsScrolling(t *testing.T) {
	chat := newChatRenderer(40, 4)
	chat.SetItems([]workbench.TranscriptItem{{
		ID:    "m1",
		Kind:  workbench.TranscriptAssistant,
		Actor: "assistant",
		Text:  strings.Repeat("long line ", 40),
	}})
	chat.LineDown(1000)
	if !chat.IsAtBottom() {
		t.Fatalf("chat should clamp to bottom after large line down")
	}
	chat.LineUp(1000)
	if chat.yOffset != 0 {
		t.Fatalf("yOffset = %d, want clamped top", chat.yOffset)
	}
	for _, line := range strings.Split(chat.View(), "\n") {
		if width := xansi.StringWidth(line); width > 40 {
			t.Fatalf("line width = %d, want <= 40 for %q\n%s", width, line, chat.View())
		}
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

func TestChatRendererHardWrapsLongUnbreakableTokens(t *testing.T) {
	chat := newChatRenderer(30, 30)
	longURL := "https://example.com/path/with/no-spaces-at-all/and-a-very-long-segment-that-cannot-wordwrap-cleanly"
	chat.SetItems([]workbench.TranscriptItem{{
		ID:    "m1",
		Kind:  workbench.TranscriptAssistant,
		Actor: "assistant",
		Text:  "see " + longURL,
	}})
	view := chat.View()
	for _, line := range strings.Split(view, "\n") {
		if width := xansi.StringWidth(line); width > 30 {
			t.Fatalf("line width = %d, want <= 30 for %q\n%s", width, line, view)
		}
	}
	clean := stripANSI(view)
	// Reassemble the body across wrapped lines, dropping the leading gutter
	// and the header. We expect every byte of the URL to be preserved.
	var body strings.Builder
	for _, line := range strings.Split(clean, "\n") {
		// Strip the 2-char gutter ("▌ " or "  ") if present.
		trimmed := strings.TrimLeft(line, " ▌")
		body.WriteString(strings.TrimSpace(trimmed))
	}
	if !strings.Contains(body.String(), longURL) {
		t.Fatalf("expected full URL preserved across wrapped lines, got:\nbody=%q\nfull view:\n%s", body.String(), clean)
	}
	if strings.Contains(clean, "...") {
		t.Fatalf("expected no truncation ellipsis when wrapping long token, got:\n%s", clean)
	}
	if strings.Contains(clean, "…") {
		t.Fatalf("expected no unicode ellipsis when wrapping long token, got:\n%s", clean)
	}
}

func TestChatRendererCollapsesUnselectedToolBodies(t *testing.T) {
	chat := newChatRenderer(80, 20)
	chat.SetItems([]workbench.TranscriptItem{
		{ID: "m1", Kind: workbench.TranscriptTool, Title: "read_file", Text: `{"path":"README.md"}`, Status: "completed"},
		{ID: "m2", Kind: workbench.TranscriptAssistant, Text: "done"},
	})

	view := stripANSI(chat.View())
	if strings.Contains(view, `{"path":"README.md"}`) {
		t.Fatalf("unselected tool body is visible:\n%s", view)
	}
	if !strings.Contains(view, "m1 tool read_file completed") {
		t.Fatalf("collapsed tool header missing:\n%s", view)
	}

	chat.MoveSelection(-1)
	view = stripANSI(chat.View())
	if strings.Contains(view, `{"path":"README.md"}`) {
		t.Fatalf("selected tool body should stay hidden in transcript:\n%s", view)
	}
}

func TestChatRendererKeepsTerminalToolBodiesVisible(t *testing.T) {
	chat := newChatRenderer(80, 20)
	chat.SetItems([]workbench.TranscriptItem{
		{ID: "m1", Kind: workbench.TranscriptTool, Title: "terminal_write", Text: `{"command":"ls"}`, Status: "requested"},
	})

	view := stripANSI(chat.View())
	if !strings.Contains(view, `{"command":"ls"}`) {
		t.Fatalf("terminal tool body is hidden:\n%s", view)
	}
}
