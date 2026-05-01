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
	width     int
	height    int
	selected  int
	yOffset   int
	follow    bool
	items     []workbench.TranscriptItem
	cache     map[chatCacheKey][]string
	layout    []chatCellLayout
	lineCount int
	layoutKey chatLayoutKey
}

type chatCacheKey struct {
	id        string
	width     int
	selected  bool
	streaming bool
	hash      uint64
}

type chatLayoutKey struct {
	width    int
	selected int
	hash     uint64
}

type chatCellLayout struct {
	index int
	start int
	end   int
	lines []string
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
	c.invalidateLayout()
	c.clamp()
	if c.follow {
		c.scrollToBottom()
	}
}

func (c *chatRenderer) SetItems(items []workbench.TranscriptItem) {
	c.items = append(c.items[:0], items...)
	c.invalidateLayout()
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
	c.ensureLayout()
	c.clampOffsetForLineCount(c.lineCount)
	if c.lineCount == 0 {
		return fitLines(nil, c.width, c.height)
	}
	start := clamp(c.yOffset, 0, max(0, c.lineCount-1))
	end := min(c.lineCount, start+c.height)
	return fitLines(c.visibleLines(start, end), c.width, c.height)
}

func (c *chatRenderer) MoveSelection(delta int) {
	if len(c.items) == 0 {
		return
	}
	c.selected = clamp(c.selected+delta, 0, len(c.items)-1)
	c.invalidateLayout()
	c.follow = c.selected == len(c.items)-1
	c.ensureSelectedVisible()
}

// SmartMove is the j/k entry point: when the currently selected message is
// larger than the viewport and partially clipped, scroll within it instead
// of jumping to the next/previous cell. Once the relevant edge of the cell
// is visible, the next press advances the selection. Without this the user
// has no way to read the middle of a long message — selecting it brings the
// top into view, but pressing k again would fly past it to the previous
// message.
func (c *chatRenderer) SmartMove(delta int) {
	if len(c.items) == 0 || delta == 0 {
		return
	}
	start, end := c.selectedLineRange()
	maxOffset := max(0, c.totalLineCount()-c.height)
	switch {
	case delta < 0:
		// Going up: if the start of the selected cell is above the viewport,
		// scroll the viewport up by a small step instead of moving selection.
		if start < c.yOffset {
			c.yOffset = max(start, c.yOffset-3)
			c.follow = false
			c.clampOffset()
			return
		}
	case delta > 0:
		// Going down: if the end of the selected cell is below the viewport,
		// scroll down by a small step instead of jumping to the cell edge.
		if end >= c.yOffset+c.height {
			c.yOffset = min(maxOffset, min(end-c.height+1, c.yOffset+3))
			c.follow = false
			c.clampOffset()
			return
		}
	}
	c.MoveSelection(delta)
}

func (c *chatRenderer) LineDown(n int) {
	c.yOffset += max(1, n)
	c.follow = false
	c.clampOffset()
	c.selectVisibleIfHidden(true)
}

func (c *chatRenderer) LineUp(n int) {
	c.yOffset -= max(1, n)
	c.follow = false
	c.clampOffset()
	c.selectVisibleIfHidden(false)
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
	c.invalidateLayout()
	c.follow = false
}

func (c *chatRenderer) Bottom() {
	c.FollowLatest()
}

func (c *chatRenderer) FollowLatest() {
	if len(c.items) > 0 {
		c.selected = len(c.items) - 1
	}
	c.invalidateLayout()
	c.follow = true
	c.scrollToBottom()
}

func (c *chatRenderer) IsAtBottom() bool {
	return c.yOffset >= max(0, c.totalLineCount()-c.height)
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
	c.ensureLayout()
	var lines []string
	for _, cell := range c.layout {
		lines = append(lines, cell.lines...)
	}
	return lines
}

func (c *chatRenderer) visibleLines(start int, end int) []string {
	var lines []string
	for _, cell := range c.layout {
		if cell.end <= start {
			continue
		}
		if cell.start >= end {
			break
		}
		from := max(start, cell.start) - cell.start
		to := min(end, cell.end) - cell.start
		if from >= 0 && to <= len(cell.lines) && from < to {
			lines = append(lines, cell.lines[from:to]...)
		}
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
	// Reserve cells for the 2-cell gutter and the 2-cell outer truncate
	// margin that fitLines applies in View(). Wrapping to c.width-2 alone
	// would still get clipped by fitLines and lose visible characters with a
	// "..." tail on every long line.
	bodyWidth := max(10, c.width-4)
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
		title := strings.ToLower(firstNonEmpty(item.Title, item.Meta["tool_call_name"]))
		if strings.Contains(title, "terminal_read") || strings.Contains(title, "terminal_write") {
			return false
		}
		if item.Status != "" && item.Status != "ok" && item.Status != "completed" && item.Status != "success" {
			return false
		}
		if item.Meta["tool_call_error"] != "" {
			return false
		}
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
		lines = append(lines, hardWrapToWidth(line, width)...)
	}
	return trimOuterBlankLines(lines)
}

// hardWrapToWidth wraps a single line to width, falling back to a hard break
// inside long unbreakable runs (URLs, file paths, JSON). Wordwrap alone keeps
// such tokens whole and lets them overflow off the right edge — those bytes
// would then be lost to truncation. We word-wrap first to preserve nice
// boundaries, then split any over-wide leftover at exact width measured in
// terminal cells. ANSI control sequences are stripped from width measurements
// via xansi.StringWidth.
func hardWrapToWidth(line string, width int) []string {
	if width <= 0 {
		return []string{line}
	}
	if xansi.StringWidth(line) <= width {
		return []string{line}
	}
	// First pass: word-wrap on whitespace so paragraphs of regular text still
	// look natural.
	wrapped := xansi.Wordwrap(line, width, " /")
	var out []string
	for _, candidate := range strings.Split(wrapped, "\n") {
		out = append(out, breakOversizedLine(candidate, width)...)
	}
	return out
}

// breakOversizedLine takes a line that has already been word-wrapped and
// emits one or more sub-lines none of which exceed width. It uses the
// xansi-aware wrapper to split exactly on cell width so multi-byte characters
// stay intact.
func breakOversizedLine(line string, width int) []string {
	if xansi.StringWidth(line) <= width || width <= 0 {
		return []string{line}
	}
	wrapped := xansi.Wrap(line, width, "")
	parts := strings.Split(wrapped, "\n")
	for i := range parts {
		// xansi.Wrap can still leave a trailing remainder if the total length
		// is not a clean multiple. Truncate (with no ellipsis) so we never
		// emit an over-wide line and lose visible text.
		if xansi.StringWidth(parts[i]) > width {
			parts[i] = xansi.Truncate(parts[i], width, "")
		}
	}
	return parts
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
		return firstNonEmpty(title, "context")
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
	if selected {
		return selectedStyle.Render(gutter)
	}
	switch item.Kind {
	case workbench.TranscriptError:
		return errorStyle.Render(gutter)
	case workbench.TranscriptTool, workbench.TranscriptPatch, workbench.TranscriptShell, workbench.TranscriptAgent, workbench.TranscriptContext, workbench.TranscriptThinking:
		return mutedStyle.Render(gutter)
	default:
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
	c.ensureLayout()
	for _, cell := range c.layout {
		if cell.index == c.selected {
			start := cell.start
			if len(cell.lines) > 0 && strings.TrimSpace(xansi.Strip(cell.lines[0])) == "" {
				start++
			}
			return start, max(start, cell.end-1)
		}
	}
	return 0, 0
}

func (c *chatRenderer) selectVisibleIfHidden(preferBottom bool) {
	if len(c.items) == 0 {
		return
	}
	start, end := c.selectedLineRange()
	viewStart := c.yOffset
	viewEnd := c.yOffset + c.height
	if end >= viewStart && start < viewEnd {
		return
	}
	c.ensureLayout()
	chosen := -1
	for _, cell := range c.layout {
		cellStart := cell.start
		if len(cell.lines) > 0 && strings.TrimSpace(xansi.Strip(cell.lines[0])) == "" {
			cellStart++
		}
		cellEnd := max(cellStart, cell.end-1)
		if cellEnd < viewStart || cellStart >= viewEnd {
			continue
		}
		chosen = cell.index
		if !preferBottom {
			break
		}
	}
	if chosen >= 0 && chosen != c.selected {
		c.selected = chosen
		c.invalidateLayout()
	}
}

func (c *chatRenderer) scrollToBottom() {
	c.yOffset = max(0, c.totalLineCount()-c.height)
}

func (c *chatRenderer) clamp() {
	c.selected = clamp(c.selected, 0, max(0, len(c.items)-1))
	c.clampOffset()
}

func (c *chatRenderer) clampOffset() {
	c.clampOffsetForLineCount(c.totalLineCount())
}

func (c *chatRenderer) clampOffsetForLineCount(count int) {
	c.yOffset = clamp(c.yOffset, 0, max(0, count-c.height))
}

func (c *chatRenderer) totalLineCount() int {
	c.ensureLayout()
	return c.lineCount
}

func (c *chatRenderer) invalidateLayout() {
	c.layout = nil
	c.lineCount = 0
	c.layoutKey = chatLayoutKey{}
}

func (c *chatRenderer) ensureLayout() {
	key := chatLayoutKey{width: c.width, selected: c.selected, hash: chatItemsHash(c.items)}
	if c.layout != nil && c.layoutKey == key {
		return
	}
	c.layoutKey = key
	c.layout = c.layout[:0]
	line := 0
	for i, item := range c.items {
		var lines []string
		if i > 0 {
			lines = append(lines, "")
			line++
		}
		cellLines := c.renderCell(item, i == c.selected)
		lines = append(lines, cellLines...)
		start := line - 0
		if i > 0 {
			start = line - 1
		}
		end := start + len(lines)
		c.layout = append(c.layout, chatCellLayout{index: i, start: start, end: end, lines: lines})
		line = end
	}
	c.lineCount = line
}

func chatItemsHash(items []workbench.TranscriptItem) uint64 {
	h := fnv.New64a()
	for _, item := range items {
		_, _ = h.Write([]byte(strconv.FormatUint(chatItemHash(item), 10)))
		_, _ = h.Write([]byte{0})
	}
	return h.Sum64()
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
