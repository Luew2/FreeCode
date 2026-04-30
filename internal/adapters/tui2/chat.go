package tui2

import (
	"hash/fnv"
	"sort"
	"strconv"
	"strings"

	"github.com/charmbracelet/glamour"
	glamouransi "github.com/charmbracelet/glamour/ansi"
	xansi "github.com/charmbracelet/x/ansi"

	"github.com/Luew2/FreeCode/internal/app/workbench"
)

type chatRenderer struct {
	width    int
	height   int
	selected int
	yOffset  int
	follow   bool
	items    []workbench.TranscriptItem
	cache    map[chatCacheKey][]string
}

type chatCacheKey struct {
	id        string
	width     int
	selected  bool
	streaming bool
	hash      uint64
}

func newChatRenderer(width int, height int) chatRenderer {
	return chatRenderer{
		width:    max(10, width),
		height:   max(1, height),
		selected: 0,
		follow:   true,
		cache:    map[chatCacheKey][]string{},
	}
}

func (c *chatRenderer) SetSize(width int, height int) {
	c.width = max(10, width)
	c.height = max(1, height)
	c.clamp()
	if c.follow {
		c.scrollToBottom()
	}
}

func (c *chatRenderer) SetItems(items []workbench.TranscriptItem) {
	c.items = append(c.items[:0], items...)
	if c.cache == nil {
		c.cache = map[chatCacheKey][]string{}
	}
	c.clamp()
	if c.follow || c.selected >= len(c.items)-2 {
		c.FollowLatest()
		return
	}
	c.clampOffset()
}

func (c *chatRenderer) View() string {
	if len(c.items) == 0 {
		return fitLines([]string{mutedStyle.Render("No messages yet")}, c.width, c.height)
	}
	lines := c.allLines()
	c.clampOffsetForLineCount(len(lines))
	if len(lines) == 0 {
		return fitLines(nil, c.width, c.height)
	}
	start := clamp(c.yOffset, 0, max(0, len(lines)-1))
	end := min(len(lines), start+c.height)
	return fitLines(lines[start:end], c.width, c.height)
}

func (c *chatRenderer) MoveSelection(delta int) {
	if len(c.items) == 0 {
		return
	}
	c.selected = clamp(c.selected+delta, 0, len(c.items)-1)
	c.follow = c.selected == len(c.items)-1
	c.ensureSelectedVisible()
}

func (c *chatRenderer) LineDown(n int) {
	c.yOffset += max(1, n)
	c.follow = false
	c.clampOffset()
}

func (c *chatRenderer) LineUp(n int) {
	c.yOffset -= max(1, n)
	c.follow = false
	c.clampOffset()
}

func (c *chatRenderer) PageDown() {
	c.LineDown(max(1, c.height-1))
}

func (c *chatRenderer) PageUp() {
	c.LineUp(max(1, c.height-1))
}

func (c *chatRenderer) Top() {
	c.yOffset = 0
	c.selected = 0
	c.follow = false
}

func (c *chatRenderer) Bottom() {
	c.FollowLatest()
}

func (c *chatRenderer) FollowLatest() {
	if len(c.items) > 0 {
		c.selected = len(c.items) - 1
	}
	c.follow = true
	c.scrollToBottom()
}

func (c *chatRenderer) IsAtBottom() bool {
	lines := c.allLines()
	return c.yOffset >= max(0, len(lines)-c.height)
}

func (c *chatRenderer) SelectedID() (string, bool) {
	item, ok := c.SelectedItem()
	if !ok {
		return "", false
	}
	return item.ID, strings.TrimSpace(item.ID) != ""
}

func (c *chatRenderer) SelectedItem() (workbench.TranscriptItem, bool) {
	if c.selected < 0 || c.selected >= len(c.items) {
		return workbench.TranscriptItem{}, false
	}
	return c.items[c.selected], true
}

func (c *chatRenderer) allLines() []string {
	var lines []string
	for i, item := range c.items {
		if i > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, c.renderCell(item, i == c.selected)...)
	}
	return lines
}

func (c *chatRenderer) renderCell(item workbench.TranscriptItem, selected bool) []string {
	key := chatCacheKey{
		id:        item.ID,
		width:     c.width,
		selected:  selected,
		streaming: item.Streaming,
		hash:      chatItemHash(item),
	}
	if cached, ok := c.cache[key]; ok {
		return append([]string(nil), cached...)
	}
	bodyWidth := max(10, c.width-2)
	lines := []string{c.renderHeader(item, selected)}
	if !chatBodyCollapsed(item, selected) {
		for _, line := range c.renderBody(item, bodyWidth) {
			lines = append(lines, chatGutter(item, selected)+line)
		}
	}
	for i := range lines {
		lines[i] = xansi.Truncate(lines[i], c.width, "…")
	}
	c.cache[key] = append([]string(nil), lines...)
	return lines
}

func chatBodyCollapsed(item workbench.TranscriptItem, selected bool) bool {
	switch item.Kind {
	case workbench.TranscriptTool:
		return true
	default:
		return false
	}
}

func (c *chatRenderer) renderHeader(item workbench.TranscriptItem, selected bool) string {
	label := chatHeaderLabel(item)
	if item.Status != "" {
		label += " " + item.Status
	}
	if item.Streaming {
		label += " ..."
	}
	if item.ID != "" {
		label = item.ID + " " + label
	}
	line := chatGutter(item, selected) + label
	if selected {
		return selectedStyle.Render(line)
	}
	return mutedStyle.Render(line)
}

func (c *chatRenderer) renderBody(item workbench.TranscriptItem, width int) []string {
	text := strings.TrimSpace(sanitizeTerminalText(item.Text))
	if text == "" {
		switch item.Kind {
		case workbench.TranscriptThinking:
			text = "thinking"
		case workbench.TranscriptTool:
			text = strings.TrimSpace(item.Title)
		case workbench.TranscriptPatch:
			text = strings.TrimSpace(item.Title)
		case workbench.TranscriptShell:
			text = strings.TrimSpace(item.Title)
		}
	}
	if text == "" {
		return nil
	}
	rendered, err := renderChatMarkdown(text, width)
	if err != nil {
		rendered = xansi.Wordwrap(text, width, " /")
	}
	rendered = strings.TrimRight(rendered, "\n")
	if rendered == "" {
		return nil
	}
	var lines []string
	for _, line := range strings.Split(rendered, "\n") {
		line = strings.TrimRight(line, " ")
		if xansi.StringWidth(line) > width {
			for _, wrapped := range strings.Split(xansi.Wordwrap(line, width, " /"), "\n") {
				lines = append(lines, xansi.Truncate(wrapped, width, "…"))
			}
			continue
		}
		lines = append(lines, xansi.Truncate(line, width, "…"))
	}
	return trimOuterBlankLines(lines)
}

func chatHeaderLabel(item workbench.TranscriptItem) string {
	title := strings.TrimSpace(sanitizeTerminalText(item.Title))
	switch item.Kind {
	case workbench.TranscriptUser:
		return "you"
	case workbench.TranscriptAssistant:
		return "assistant"
	case workbench.TranscriptStreaming:
		return "assistant"
	case workbench.TranscriptThinking:
		return "thinking"
	case workbench.TranscriptTool:
		return strings.TrimSpace("tool " + firstNonEmpty(title, item.Meta["tool_call_name"]))
	case workbench.TranscriptPatch:
		return strings.TrimSpace("patch " + title)
	case workbench.TranscriptShell:
		command := strings.TrimSpace(item.Meta["command"])
		if command != "" {
			return "local shell !" + command
		}
		return strings.TrimSpace("local shell " + title)
	case workbench.TranscriptAgent:
		return strings.TrimSpace("agent " + firstNonEmpty(title, item.Actor))
	case workbench.TranscriptContext:
		return "context"
	case workbench.TranscriptError:
		return "error"
	default:
		return firstNonEmpty(title, item.Actor, "message")
	}
}

func chatGutter(item workbench.TranscriptItem, selected bool) string {
	gutter := "  "
	if selected {
		gutter = "▌ "
	}
	switch item.Kind {
	case workbench.TranscriptError:
		return errorStyle.Render(gutter)
	case workbench.TranscriptTool, workbench.TranscriptPatch, workbench.TranscriptShell, workbench.TranscriptAgent, workbench.TranscriptContext, workbench.TranscriptThinking:
		return mutedStyle.Render(gutter)
	default:
		if selected {
			return selectedStyle.Render(gutter)
		}
		return gutter
	}
}

func renderChatMarkdown(text string, width int) (string, error) {
	renderer, err := glamour.NewTermRenderer(
		glamour.WithStyles(chatMarkdownStyle()),
		glamour.WithWordWrap(width),
		glamour.WithPreservedNewLines(),
	)
	if err != nil {
		return "", err
	}
	return renderer.Render(text)
}

func chatMarkdownStyle() glamouransi.StyleConfig {
	cyan := "14"
	muted := "8"
	green := "10"
	bold := true
	return glamouransi.StyleConfig{
		Document: glamouransi.StyleBlock{},
		Heading: glamouransi.StyleBlock{
			StylePrimitive: glamouransi.StylePrimitive{Color: &cyan, Bold: &bold},
		},
		Code: glamouransi.StyleBlock{
			StylePrimitive: glamouransi.StylePrimitive{Color: &green},
		},
		CodeBlock: glamouransi.StyleCodeBlock{
			StyleBlock: glamouransi.StyleBlock{
				StylePrimitive: glamouransi.StylePrimitive{Color: &green},
			},
			Theme: "dracula",
		},
		BlockQuote: glamouransi.StyleBlock{
			StylePrimitive: glamouransi.StylePrimitive{Color: &muted},
		},
		Link: glamouransi.StylePrimitive{Color: &cyan},
	}
}

func (c *chatRenderer) ensureSelectedVisible() {
	start, end := c.selectedLineRange()
	if start < c.yOffset {
		c.yOffset = start
		return
	}
	if end >= c.yOffset+c.height {
		c.yOffset = end - c.height + 1
	}
	c.clampOffset()
}

func (c *chatRenderer) selectedLineRange() (int, int) {
	line := 0
	for i, item := range c.items {
		if i > 0 {
			line++
		}
		count := max(1, len(c.renderCell(item, i == c.selected)))
		start := line
		end := line + count - 1
		if i == c.selected {
			return start, end
		}
		line += count
	}
	return 0, 0
}

func (c *chatRenderer) scrollToBottom() {
	lines := c.allLines()
	c.yOffset = max(0, len(lines)-c.height)
}

func (c *chatRenderer) clamp() {
	c.selected = clamp(c.selected, 0, max(0, len(c.items)-1))
	c.clampOffset()
}

func (c *chatRenderer) clampOffset() {
	c.clampOffsetForLineCount(len(c.allLines()))
}

func (c *chatRenderer) clampOffsetForLineCount(count int) {
	c.yOffset = clamp(c.yOffset, 0, max(0, count-c.height))
}

func chatItemHash(item workbench.TranscriptItem) uint64 {
	h := fnv.New64a()
	writeHash := func(value string) {
		_, _ = h.Write([]byte(value))
		_, _ = h.Write([]byte{0})
	}
	writeHash(item.ID)
	writeHash(string(item.Kind))
	writeHash(item.Actor)
	writeHash(item.Title)
	writeHash(item.Text)
	writeHash(item.Status)
	writeHash(item.ArtifactID)
	writeHash(strconv.FormatBool(item.Streaming))
	if len(item.Meta) > 0 {
		keys := make([]string, 0, len(item.Meta))
		for key := range item.Meta {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			writeHash(key)
			writeHash(item.Meta[key])
		}
	}
	return h.Sum64()
}

func trimOuterBlankLines(lines []string) []string {
	start := 0
	for start < len(lines) && strings.TrimSpace(xansi.Strip(lines[start])) == "" {
		start++
	}
	end := len(lines)
	for end > start && strings.TrimSpace(xansi.Strip(lines[end-1])) == "" {
		end--
	}
	return lines[start:end]
}
