package tui

import (
	"fmt"
	"image/color"
	"reflect"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/glamour/v2"
	"charm.land/glamour/v2/ansi"
	"charm.land/glamour/v2/styles"
	"charm.land/lipgloss/v2"
	xansi "github.com/charmbracelet/x/ansi"
	"github.com/slbdotdev/slbh/internal/seam"
	"github.com/slbdotdev/slbh/internal/toolmarkup"
)

func (m *Model) renderEvent(event seam.Event, width int) string {
	switch event.Kind {
	case "assistant":
		return renderChatBlock(agentTitle(event, "agent"), m.renderMarkdown(markdownBlockKey(event), withoutToolMarkup(event.Text), width), width, assistantBubble)
	case "user":
		return renderChatBlock("user", event.Text, width, userBubble)
	case "child_result":
		return renderChatBlock(agentTitle(event, "subagent"), m.renderMarkdown(markdownBlockKey(event), event.Text, width), width, subagentBubble)
	case "job_result":
		label := "job"
		if name := metadataString(event, "tool"); name != "" {
			label = name
		}
		return renderChatBlock(label, m.renderMarkdown(markdownBlockKey(event), event.Text, width), width, subagentBubble)
	case "steer":
		if isForwardedAgentMessage(event) {
			return renderChatBlock(agentTitle(event, "subagent"), m.renderMarkdown(markdownBlockKey(event), event.Text, width), width, subagentBubble)
		}
		return rollingBlock(event.Text, width)
	case "usage":
		return rollingBlock(fmt.Sprint(event.Metadata), width)
	case "thinking", "tool", "error", "status", "runtime", "tool_result":
		// The event kind is already shown in the non-chat block header. Keep the
		// body to the event's content so the label is not duplicated there.
		return rollingBlock(event.Text, width)
	default:
		return rollingBlock(event.Text, width)
	}
}

func (m *Model) renderEventCached(event seam.Event, width int) string {
	// Markdown blocks can be served by the throttle with a deliberately stale
	// render while a stream is active. Keep their cache in renderMarkdown, where
	// the source and catch-up tick are visible, rather than caching that stale
	// outer chat block here.
	if isMarkdownEvent(event) {
		return m.renderEvent(event, width)
	}
	key := renderedEventCacheKey{
		cursor:     event.Cursor,
		agentID:    event.AgentID,
		agentTitle: event.AgentTitle,
		kind:       event.Kind,
		text:       event.Text,
		tool:       metadataString(event, "tool"),
		width:      width,
		forwarded:  isForwardedAgentMessage(event),
	}
	if rendered, ok := m.eventRenderCache[key]; ok {
		return rendered
	}
	rendered := m.renderEvent(event, width)
	if m.eventRenderCache == nil {
		m.eventRenderCache = make(map[renderedEventCacheKey]string)
	}
	if len(m.eventRenderCache) >= max(renderedEventCacheLimit, 2*len(m.events)) {
		clear(m.eventRenderCache)
	}
	m.eventRenderCache[key] = rendered
	return rendered
}

func isMarkdownEvent(event seam.Event) bool {
	switch event.Kind {
	case "assistant", "child_result", "job_result":
		return true
	case "steer":
		return isForwardedAgentMessage(event)
	default:
		return false
	}
}

// markdownBlockKey identifies one chat block for the render throttle. The
// cursor is its first fragment's, which merging keeps, so two replies from
// one agent never share a key: a reply that opened with the previous one's
// text would otherwise be shown that reply's render until the next tick.
func markdownBlockKey(event seam.Event) string {
	return fmt.Sprintf("%s\x00%s\x00%d", event.AgentID, event.Kind, event.Cursor)
}

// renderMarkdown styles agent-authored markdown for a chat block. A streamed
// message grows by one chunk per event and refreshView re-renders it every
// time, so an unthrottled render is quadratic in the length of the response.
// Between renders this returns the block's most recent rendered copy and marks
// the view stale; Update then schedules a catch-up tick so the final chunk is
// never left unrendered.
func (m *Model) renderMarkdown(key, source string, width int) string {
	if width < 1 || strings.TrimSpace(source) == "" {
		return source
	}
	renderer := m.markdownRendererForWidth(width)
	if renderer == nil {
		return source
	}
	cacheKey := markdownCacheKey{source: source, width: width}
	if rendered, ok := m.markdownCache[cacheKey]; ok {
		return rendered
	}
	if previous, ok := m.markdownRecent[key]; ok && strings.HasPrefix(source, previous.source) && time.Since(m.markdownRenderedAt) < markdownThrottle {
		m.markdownStale = true
		return previous.rendered
	}
	rendered, err := renderer.Render(source)
	if err != nil {
		// A markdown failure must never blank a message. The TUI owns the
		// terminal, so there is nowhere to report this but the block itself.
		return source
	}
	rendered = trimBlankEdges(rendered)
	m.markdownRenderedAt = time.Now()
	m.cacheRenderedMarkdown(cacheKey, rendered)
	if m.markdownRecent == nil {
		m.markdownRecent = make(map[string]markdownRecentRender)
	}
	m.markdownRecent[key] = markdownRecentRender{source: source, rendered: rendered}
	return rendered
}

// markdownTickCmd schedules one catch-up render when the throttle held a
// stale block back. It is a no-op when nothing is stale or a tick is already
// in flight, so a quiet stream cannot accumulate timers.
func (m *Model) markdownTickCmd() tea.Cmd {
	if !m.markdownStale || m.markdownTicking {
		return nil
	}
	m.markdownStale = false
	m.markdownTicking = true
	return tea.Tick(markdownThrottle, func(at time.Time) tea.Msg { return markdownTickMsg(at) })
}

// trimBlankEdges drops the padding glamour puts above and below a document,
// which would otherwise show as empty painted rows inside the block. That
// padding is bare newlines, so trimming those alone cannot reach a blank line
// that belongs to the content: a blank line inside a fenced code block is
// rendered padded to the block width, not as an empty string. The one
// exception is the indented row glamour closes a code block with, which is
// dropped when it lands last so a message ending in code does not end on a
// near-empty row.
func trimBlankEdges(text string) string {
	text = strings.Trim(text, "\n")
	lines := strings.Split(text, "\n")
	if len(lines) > 1 && strings.TrimSpace(xansi.Strip(lines[len(lines)-1])) == "" {
		lines = lines[:len(lines)-1]
	}
	return strings.Join(lines, "\n")
}

func (m *Model) cacheRenderedMarkdown(key markdownCacheKey, rendered string) {
	if m.markdownCache == nil {
		m.markdownCache = make(map[markdownCacheKey]string)
	}
	if len(m.markdownCache) >= max(markdownRenderCacheLimit, 2*len(m.events)) {
		clear(m.markdownCache)
		clear(m.markdownRecent)
	}
	m.markdownCache[key] = rendered
}

// markdownRendererForWidth keeps one renderer per width. A renderer is bound
// to its word-wrap width, so a resize rebuilds it and drops the output cached
// at the old width.
func (m *Model) markdownRendererForWidth(width int) *glamour.TermRenderer {
	if width < 1 {
		return nil
	}
	if m.markdownWidth == width {
		return m.markdownRender
	}
	m.markdownWidth = width
	m.markdownRender = nil
	m.markdownCache = make(map[markdownCacheKey]string)
	m.markdownRecent = make(map[string]markdownRecentRender)
	clear(m.eventRenderCache)
	renderer, err := glamour.NewTermRenderer(
		glamour.WithStyles(backgroundFreeMarkdownStyle()),
		glamour.WithWordWrap(width),
	)
	if err == nil {
		m.markdownRender = renderer
	}
	return m.markdownRender
}

func backgroundFreeMarkdownStyle() ansi.StyleConfig {
	// StyleConfig is all value fields, so this copies the dark style, but
	// Chroma is reached through a pointer shared with glamour's own
	// package-level variable. Copy it before editing so clearing backgrounds
	// below cannot reach back into the library's configuration.
	style := styles.DarkStyleConfig
	if style.CodeBlock.Chroma != nil {
		chroma := *style.CodeBlock.Chroma
		style.CodeBlock.Chroma = &chroma
		clearMarkdownBackgrounds(reflect.ValueOf(style.CodeBlock.Chroma).Elem())
	}
	zero := uint(0)
	style.Document.Margin = &zero
	style.Document.Indent = &zero
	// Inline code keeps its color but loses glamour's padding, which is a pair
	// of non-breaking spaces and copies out of the terminal as U+00A0.
	style.Code.Prefix = ""
	style.Code.Suffix = ""
	style.Code.BlockPrefix = ""
	style.Code.BlockSuffix = ""
	clearMarkdownBackgrounds(reflect.ValueOf(&style).Elem())
	return style
}

// clearMarkdownBackgrounds nils every BackgroundColor in a style value.
// messageBlock keeps its painted background continuous by rewriting SGR resets
// in the content to foreground-only resets, so a background set inside the
// content is never reset and smears down the rest of the block. It does not
// follow pointers: the one struct pointer in a StyleConfig is Chroma, which is
// shared with glamour's package-level style and is copied and swept by its
// caller instead.
func clearMarkdownBackgrounds(value reflect.Value) {
	if value.Kind() != reflect.Struct {
		return
	}
	for i := 0; i < value.NumField(); i++ {
		field := value.Field(i)
		if value.Type().Field(i).Name == "BackgroundColor" {
			if field.CanSet() {
				field.SetZero()
			}
			continue
		}
		clearMarkdownBackgrounds(field)
	}
}

func renderChatBlock(label, text string, width int, background color.Color) string {
	header := renderHeader(label)
	body := messageBlock(text, width, background)
	return header + "\n" + body
}

func renderHeader(label string) string {
	return headerStyle.Render(headerBullet + label)
}

func isMessage(event seam.Event) bool {
	return event.Kind == "user" || event.Kind == "assistant" || event.Kind == "child_result" || event.Kind == "job_result" || isForwardedAgentMessage(event)
}

func isForwardedAgentMessage(event seam.Event) bool {
	if event.Kind != "steer" || event.Metadata == nil {
		return false
	}
	_, ok := event.Metadata["sender"]
	return ok
}

func agentTitle(event seam.Event, fallback string) string {
	if title := strings.TrimSpace(event.AgentTitle); title != "" {
		return title
	}
	return fallback
}

func isViewportEvent(event seam.Event) bool {
	// Request payloads, request completions, status updates, and usage
	// reports are durable control-plane records, not chat output. Keep them in Model.events and the
	// runtime transcript while omitting them from the message viewport.
	return event.Kind != "inference_request" && event.Kind != "request_done" && event.Kind != "turn_done" && event.Kind != "status" && event.Kind != "usage"
}

func messageBlock(text string, width int, background color.Color) string {
	// Keep foreground resets in the content scoped so the background remains
	// continuous across the whole rectangle.
	text = strings.ReplaceAll(text, "\x1b[0m", "\x1b[39m")
	text = strings.ReplaceAll(text, "\x1b[m", "\x1b[39m")
	return lipgloss.NewStyle().Width(width).Background(background).Render(text)
}

func isControlKey(msg tea.KeyPressMsg, code rune) bool {
	return msg.Code == code && msg.Mod.Contains(tea.ModCtrl)
}

func wrapToWidth(text string, width int) string {
	if width < 1 {
		return text
	}
	return lipgloss.NewStyle().Width(width).Render(text)
}

// rollingBlock keeps non-chat output from expanding the message viewport.
// The complete event remains in the runtime transcript; only the newest lines
// are rendered so tool-heavy turns do not expand the viewport indefinitely.
func rollingBlock(text string, width int) string {
	wrapped := wrapToWidth(text, width)
	lines := strings.Split(wrapped, "\n")
	if len(lines) > nonChatBlockHeight {
		lines = lines[len(lines)-nonChatBlockHeight:]
	}
	return messageBlock(strings.Join(lines, "\n"), width, nonChatBubble)
}

func renderNonChatBlock(events []seam.Event, parts []string, width int) string {
	if len(events) == 0 {
		return ""
	}
	header := nonChatHeader(events)
	body := rollingBlock(strings.Join(parts, "\n"), width)
	return header + "\n" + body
}

func nonChatHeader(events []seam.Event) string {
	labels := make([]string, 0, len(events))
	if hasThinking(events) {
		labels = append(labels, fmt.Sprintf("thinking(%ds)", totalThinkingSeconds(events)))
	}
	for _, tally := range toolCallTallies(events) {
		labels = append(labels, fmt.Sprintf("%s(%d)", tally.name, tally.count))
	}
	seen := make(map[string]bool)
	for _, event := range events {
		if event.Kind == "thinking" || event.Kind == "tool" || event.Kind == "tool_result" {
			continue
		}
		label := responseType(event)
		if label != "" && !seen[label] {
			labels = append(labels, label)
			seen[label] = true
		}
	}
	if len(labels) > 0 {
		return renderHeader(strings.Join(labels, " · "))
	}
	return renderHeader(events[len(events)-1].Kind)
}

func hasThinking(events []seam.Event) bool {
	for _, event := range events {
		if event.Kind == "thinking" {
			return true
		}
	}
	return false
}

func totalThinkingSeconds(events []seam.Event) int {
	var total time.Duration
	var segmentStart, segmentEnd time.Time
	addSegment := func() {
		if !segmentStart.IsZero() && segmentEnd.After(segmentStart) {
			total += segmentEnd.Sub(segmentStart)
		}
		segmentStart = time.Time{}
		segmentEnd = time.Time{}
	}
	finishSegment := func(boundary time.Time) {
		if !boundary.IsZero() && boundary.After(segmentEnd) {
			segmentEnd = boundary
		}
		addSegment()
	}
	for _, event := range events {
		if event.Kind != "thinking" {
			// A provider may emit a single reasoning chunk. In that case the
			// following event is the only available end marker for the thought.
			finishSegment(event.Time)
			continue
		}
		start, end := thinkingSpan(event)
		if start.IsZero() || end.IsZero() {
			continue
		}
		if segmentStart.IsZero() {
			segmentStart, segmentEnd = start, end
			continue
		}
		if start.Before(segmentStart) {
			segmentStart = start
		}
		if end.After(segmentEnd) {
			segmentEnd = end
		}
	}
	finishSegment(time.Time{})
	return int(total / time.Second)
}

func thinkingSpan(event seam.Event) (time.Time, time.Time) {
	start, end := event.Time, event.Time
	if event.Metadata != nil {
		if saved, ok := event.Metadata[thinkingStartMetadataKey].(time.Time); ok {
			start = saved
		}
	}
	return start, end
}

type toolTally struct {
	name  string
	count int
}

type toolCallRecord struct {
	name       string
	tallyIndex int
}

func toolCallTallies(events []seam.Event) []toolTally {
	tallies := make([]toolTally, 0)
	records := make([]toolCallRecord, 0)
	byID := make(map[string]int)
	byIndex := make(map[string]int)
	findTally := func(name string) int {
		for index := range tallies {
			if tallies[index].name == name {
				return index
			}
		}
		tallies = append(tallies, toolTally{name: name})
		return len(tallies) - 1
	}
	for _, event := range events {
		if event.Kind != "tool" && event.Kind != "tool_result" {
			continue
		}
		name := toolName(event)
		callID := metadataString(event, "call_id")
		index := metadataString(event, "index")
		recordIndex := -1
		if callID != "" {
			if existing, ok := byID[callID]; ok {
				recordIndex = existing
			}
		} else if index != "" {
			if existing, ok := byIndex[index]; ok {
				recordIndex = existing
			}
		}
		if recordIndex < 0 {
			tallyIndex := findTally(name)
			tallies[tallyIndex].count++
			records = append(records, toolCallRecord{name: name, tallyIndex: tallyIndex})
			recordIndex = len(records) - 1
		} else if name != "tool" && records[recordIndex].name == "tool" {
			// Some providers send the call name only on the first or a later
			// streamed fragment. Move the already-counted call if its name
			// becomes available later.
			oldTally := records[recordIndex].tallyIndex
			tallies[oldTally].count--
			newTally := findTally(name)
			tallies[newTally].count++
			records[recordIndex].name = name
			records[recordIndex].tallyIndex = newTally
		}
		if callID != "" {
			byID[callID] = recordIndex
		}
		if index != "" {
			byIndex[index] = recordIndex
		}
	}
	return tallies
}

func toolName(event seam.Event) string {
	if name := metadataString(event, "name"); name != "" {
		return name
	}
	return "tool"
}

func metadataString(event seam.Event, key string) string {
	if event.Metadata == nil {
		return ""
	}
	value, ok := event.Metadata[key]
	if !ok || value == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(value))
}

// opensNonChatBlock reports whether event starts a new gray block after
// previous. A thinking span after other output opens the next round's block,
// and a compaction gets a block of its own: one rolling window over several
// rounds let each new thought, or a long summary, push the earlier output out
// through the top.
func opensNonChatBlock(previous, event seam.Event) bool {
	return (event.Kind == "thinking" && previous.Kind != "thinking") || event.Kind == "compacting" || previous.Kind == "compact"
}

func responseType(event seam.Event) string {
	switch event.Kind {
	case "thinking":
		return "thinking"
	case "compacting":
		return "compacting"
	case "compact":
		return "compacted"
	case "tool", "tool_result":
		name, _ := event.Metadata["name"].(string)
		if name != "" {
			return "tool " + name
		}
		return "tool"
	case "error":
		return "error"
	case "steer":
		return "steer"
	default:
		return ""
	}
}

// withoutToolMarkup replaces tool-call markup that ends an assistant reply,
// which a model server returned as text instead of running (the runtime then
// warns and the turn goes on), with a note naming the calls, so the chat shows
// what happened rather than raw markup. It applies while the reply streams.
func withoutToolMarkup(text string) string {
	prose, names, trailing := toolmarkup.Split(text)
	if !trailing {
		return text
	}
	note := "*(tool call written as text, not run)*"
	if len(names) > 0 {
		note = "*(tool call written as text, not run: " + strings.Join(names, ", ") + ")*"
	}
	if prose == "" {
		return note
	}
	return prose + "\n\n" + note
}
