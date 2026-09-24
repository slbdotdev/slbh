package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"charm.land/glamour/v2/styles"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/slbdotdev/slbh/internal/config"
	"github.com/slbdotdev/slbh/internal/harness"
	"github.com/slbdotdev/slbh/internal/provider"
	"github.com/slbdotdev/slbh/internal/seam"
)

type quietProvider struct{}

func (quietProvider) Stream(context.Context, provider.Request, provider.StreamSink) error { return nil }

func launchTestSubagent(t *testing.T, runtime *harness.Runtime, title string) seam.AgentSnapshot {
	t.Helper()
	input, err := json.Marshal(map[string]string{"title": title, "brief": ""})
	if err != nil {
		t.Fatal(err)
	}
	id, err := runtime.ExecuteTool(seatID(runtime), "launch_subagent", string(input))
	if err != nil {
		t.Fatal(err)
	}
	for _, agent := range runtime.Agents() {
		if agent.ID == id {
			return agent
		}
	}
	t.Fatalf("launch_subagent returned unknown agent %q", id)
	return seam.AgentSnapshot{}
}

func endTestSubagent(t *testing.T, runtime *harness.Runtime, childID string) {
	t.Helper()
	input, err := json.Marshal(map[string]string{"agent_id": childID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.ExecuteTool(seatID(runtime), "end_subagent", string(input)); err != nil {
		t.Fatal(err)
	}
}

func waitForRuntimeKind(t *testing.T, runtime seam.Runtime, cursor *seam.EventCursor, kind string) seam.Event {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		batch := runtime.PollEvents(seam.EventQuery{After: *cursor, WaitMilliseconds: 50})
		*cursor = batch.Cursor
		for _, event := range batch.Events {
			if event.Kind == kind {
				return event
			}
		}
	}
	t.Fatalf("runtime event %q did not arrive", kind)
	return seam.Event{}
}

func TestViewFillsTerminalAndWrapsContent(t *testing.T) {
	runtime, err := harness.New(config.Config{Home: t.TempDir(), SeatModel: "test", SeatEffort: "high"}, harness.Options{Provider: func(string) (provider.Provider, error) { return quietProvider{}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	m := New(runtime)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = updated.(Model)
	m.events = append(m.events,
		seam.Event{AgentID: seatID(runtime), AgentTitle: "seat", Kind: "user", Text: "hi"},
		seam.Event{AgentID: seatID(runtime), AgentTitle: "seat", Kind: "error", Text: strings.Repeat("long error ", 20)},
	)
	m.refreshView()
	view := m.View().Content
	if got := lipgloss.Height(view); got != 24 {
		t.Fatalf("view height=%d, want 24", got)
	}
	if strings.Contains(view, "long error long error long error long error long error long error long error long error long error long error long error long error long error long error long error long error long error long error long error long error") {
		t.Fatal("long content was not wrapped")
	}
	if got := ansi.Strip(m.agentPanel()); !strings.Contains(got, "seat [idle]") {
		t.Fatalf("seat-only runtime should show the seat agent: %q", got)
	}
	m.agents = append(m.agents, seam.AgentSnapshot{ID: "child", Title: "child", Status: "idle", Depth: 1})
	if got := lipgloss.Width(m.agentPanel()); got != 80 {
		t.Fatalf("agent list width=%d, want full terminal width", got)
	}
	if strings.Contains(m.statusLine(), "Enter send") || strings.Contains(m.statusLine(), "agents 1") || strings.Contains(m.statusLine(), "jobs 0") {
		t.Fatal("footer contains hidden help or zero-count metadata")
	}
	seat, ok := seatSnapshot(runtime)
	if !ok {
		t.Fatal("runtime has no seat snapshot")
	}
	if !strings.Contains(m.statusLine(), seat.Model+" "+seat.Effort) || !strings.Contains(m.statusLine(), "--/--") || !strings.Contains(m.statusLine(), " · --") {
		t.Fatal("footer should show the actual model identifier followed by effort")
	}
}

func TestAgentPanelKeepsAllAgentsInsideTerminal(t *testing.T) {
	runtime, err := harness.New(config.Config{Home: t.TempDir(), SeatModel: "test", SeatEffort: "high"}, harness.Options{Provider: func(string) (provider.Provider, error) { return quietProvider{}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	m := New(runtime)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = updated.(Model)
	child := launchTestSubagent(t, runtime, "child")
	updated, _ = m.Update(eventMsg(seam.Event{AgentID: child.ID, AgentTitle: child.Title, Kind: "status", Text: "subagent launched"}))
	m = updated.(Model)

	view := ansi.Strip(m.View().Content)
	if got := lipgloss.Height(view); got != 24 {
		t.Fatalf("view height=%d, want 24: %q", got, view)
	}
	if !strings.Contains(view, "child [idle]") {
		t.Fatalf("agent panel clipped child row: %q", view)
	}
	if !strings.Contains(view, "  • child [idle]") {
		t.Fatalf("level 1 child has incorrect marker or indentation: %q", view)
	}
}

func TestAgentPanelUsesPinkTreeMarkers(t *testing.T) {
	runtime, err := harness.New(config.Config{Home: t.TempDir(), SeatModel: "test", SeatEffort: "high"}, harness.Options{Provider: func(string) (provider.Provider, error) { return quietProvider{}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	m := New(runtime)
	m.width = 80
	m.agents = []seam.AgentSnapshot{
		{ID: "seat", Title: "seat", Status: "idle", Depth: 0},
		{ID: "child", Title: "child", Status: "idle", Depth: 1},
		{ID: "leaf", Title: "leaf", Status: "idle", Depth: 2},
	}
	panel := m.agentPanel()
	plain := ansi.Strip(panel)
	if strings.Contains(plain, "AGENTS") {
		t.Fatalf("agent header was not removed: %q", plain)
	}
	for _, want := range []string{"seat [idle]", "  • child [idle]", "    ⚬ leaf [idle]"} {
		if !strings.Contains(plain, want) {
			t.Fatalf("agent panel missing %q: %q", want, plain)
		}
	}
	if !strings.Contains(panel, "38;5;205") {
		t.Fatalf("agent lines are not pink: %q", panel)
	}
}

func TestReceiveEventsMergesStreamsAndRetainsDisplayHistory(t *testing.T) {
	runtime, err := harness.New(config.Config{Home: t.TempDir(), SeatModel: "test", SeatEffort: "high"}, harness.Options{Provider: func(string) (provider.Provider, error) { return quietProvider{}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	seat := seatID(runtime)
	m := New(runtime)
	m.receiveEvents([]seam.Event{
		{AgentID: seat, Kind: "user", Text: "question"},
		{AgentID: seat, Kind: "assistant", Text: "first"},
		{AgentID: seat, Kind: "assistant", Text: " second"},
		{AgentID: seat, Kind: "status", Text: "idle"},
	})
	if len(m.events) != 2 {
		t.Fatalf("display history retained %d events, want user plus merged assistant", len(m.events))
	}
	if got := m.events[1].Text; got != "first second" {
		t.Fatalf("merged assistant text=%q, want %q", got, "first second")
	}

	toolEvents := make([]seam.Event, 612)
	for i := range toolEvents {
		toolEvents[i] = seam.Event{
			AgentID: seat,
			Kind:    "tool_result",
			Text:    fmt.Sprintf("result-%d", i),
			Metadata: map[string]any{
				"call_id": fmt.Sprintf("call-%d", i),
			},
		}
	}
	m.receiveEvents(toolEvents)
	if got := len(m.events); got != maxRetainedViewportEvents {
		t.Fatalf("display history length=%d, want %d", got, maxRetainedViewportEvents)
	}
	if got := m.events[0].Text; got != "question" {
		t.Fatalf("display history lost first message: %q", got)
	}
	if got := m.events[len(m.events)-1].Text; got != "result-611" {
		t.Fatalf("display history lost newest event: %q", got)
	}
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = updated.(Model)
	m.viewport.GotoTop()
	if got := ansi.Strip(m.viewport.View()); !strings.Contains(got, "question") {
		t.Fatalf("first message is not reachable at the top: %q", got)
	}
	m.viewport.GotoBottom()
	if got := ansi.Strip(m.View().Content); !strings.Contains(got, "result-611") {
		t.Fatalf("newest stream disappeared: %q", got)
	}
}

func TestDisplayHistoryEvictsControlsAndToolOutputBeforeMessages(t *testing.T) {
	runtime, err := harness.New(config.Config{Home: t.TempDir(), SeatModel: "test", SeatEffort: "high"}, harness.Options{Provider: func(string) (provider.Provider, error) { return quietProvider{}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	seat := seatID(runtime)
	child := "child"
	m := New(runtime)
	messages := []seam.Event{
		{AgentID: seat, Kind: "user", Text: "first prompt"},
		{AgentID: seat, Kind: "assistant", Text: "first answer"},
		{AgentID: child, Kind: "user", Text: "child prompt"},
		{AgentID: child, Kind: "child_result", Text: "child answer"},
	}
	events := append([]seam.Event(nil), messages...)
	payload := strings.Repeat("request payload ", 4096)
	for i := 0; i < 700; i++ {
		for _, kind := range []string{"inference_request", "status", "usage", "turn_done"} {
			events = append(events, seam.Event{AgentID: seat, Kind: kind, Metadata: map[string]any{"request": payload}})
		}
		events = append(events, seam.Event{AgentID: seat, Kind: "tool_result", Text: fmt.Sprintf("result-%d", i)})
	}
	m.receiveEvents(events)
	if got := len(m.events); got != maxRetainedViewportEvents {
		t.Fatalf("display history length=%d, want %d", got, maxRetainedViewportEvents)
	}
	for i, want := range messages {
		if got := m.events[i]; got.AgentID != want.AgentID || got.Kind != want.Kind || got.Text != want.Text {
			t.Fatalf("message %d changed to %+v", i, got)
		}
	}
	for _, event := range m.events {
		if !isViewportEvent(event) {
			t.Fatalf("control record retained: %q", event.Kind)
		}
	}
	if got := m.events[len(m.events)-1].Text; got != "result-699" {
		t.Fatalf("newest tool output=%q", got)
	}

	oldestTool := m.events[len(messages)].Text
	m.events = m.events[:len(m.events)-1]
	m.appendViewportEvent(seam.Event{AgentID: seat, Kind: "inference_request", Metadata: map[string]any{"request": payload}})
	m.appendViewportEvent(seam.Event{AgentID: seat, Kind: "tool_result", Text: "new result"})
	if got := m.events[len(messages)].Text; got != oldestTool {
		t.Fatalf("tool output evicted before control record: %q", got)
	}
	for _, event := range m.events {
		if !isViewportEvent(event) {
			t.Fatalf("control record retained after eviction: %q", event.Kind)
		}
	}

	messageOnly := Model{}
	for i := 0; i <= maxRetainedViewportEvents; i++ {
		agentID := seat
		if i%2 == 1 {
			agentID = child
		}
		messageOnly.appendViewportEvent(seam.Event{AgentID: agentID, Kind: "user", Text: fmt.Sprintf("message-%d", i)})
	}
	if got := len(messageOnly.events); got != maxRetainedViewportEvents+1 {
		t.Fatalf("message-only history retained %d messages", got)
	}
}

func TestRenderCachesHoldRetainedMessageHistory(t *testing.T) {
	m := Model{}
	for i := 0; i <= renderedEventCacheLimit; i++ {
		m.events = append(m.events, seam.Event{AgentID: "seat", Kind: "user", Text: fmt.Sprintf("message-%d", i)})
	}
	for _, event := range m.events {
		m.renderEventCached(event, 40)
	}
	if got := len(m.eventRenderCache); got != len(m.events) {
		t.Fatalf("event render cache retained %d of %d messages", got, len(m.events))
	}
	for i := 0; i <= markdownRenderCacheLimit; i++ {
		m.cacheRenderedMarkdown(markdownCacheKey{source: fmt.Sprintf("message-%d", i), width: 40}, "rendered")
	}
	if got := len(m.markdownCache); got != markdownRenderCacheLimit+1 {
		t.Fatalf("markdown render cache retained %d messages", got)
	}
}

func TestModelMenuAssignsSeatSubagentAndLeafSlots(t *testing.T) {
	runtime, err := harness.New(config.Config{Home: t.TempDir(), SeatModel: "seat", SeatEffort: "high", SubagentModel: "child"}, harness.Options{Provider: func(string) (provider.Provider, error) { return quietProvider{}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	m := New(runtime)
	m.width, m.height = 80, 24
	m.modelsOpen = true
	m.modelCatalog = []provider.Catalog{{Name: "deepseek", Models: []provider.ModelInfo{{ID: "deepseek/model"}}}}
	m.modelExpanded = map[string]bool{"deepseek": true}
	m.modelSeat, m.modelSubagent, m.modelLeaf = "seat", "child", ""
	m.modelCursor = 1

	updated, _ := m.updateModelMenu(tea.KeyPressMsg{Code: 'l'})
	m = *updated.(*Model)
	if got := runtime.Config().LeafModel; got != "deepseek/model" {
		t.Fatalf("leaf model = %q, want deepseek/model", got)
	}
	if !strings.Contains(ansi.Strip(m.modelMenuView()), "[l] model") {
		t.Fatalf("leaf marker or provider prefix is wrong: %q", ansi.Strip(m.modelMenuView()))
	}
	if !strings.Contains(ansi.Strip(m.modelMenuView()), "Save and Close") {
		t.Fatal("model menu is missing Save and Close")
	}
	if !strings.Contains(ansi.Strip(m.modelMenuView()), "Slots: seat=seat · subagent=child · leaf=deepseek/model") {
		t.Fatalf("model menu is missing slot summary: %q", ansi.Strip(m.modelMenuView()))
	}

	updated, _ = m.updateModelMenu(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = *updated.(*Model)
	if !m.modelSubmenu || !strings.Contains(ansi.Strip(m.modelMenuView()), "subagent") {
		t.Fatalf("Enter did not open the slot submenu: %q", ansi.Strip(m.modelMenuView()))
	}
	updated, _ = m.updateModelMenu(tea.KeyPressMsg{Code: tea.KeyEsc})
	m = *updated.(*Model)
	if m.modelSubmenu {
		t.Fatal("Esc did not return from the slot submenu")
	}
}

func TestEndedSubagentLeavesActivePanelButKeepsTranscript(t *testing.T) {
	runtime, err := harness.New(config.Config{Home: t.TempDir(), SeatModel: "test", SeatEffort: "high"}, harness.Options{Provider: func(string) (provider.Provider, error) { return quietProvider{}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	m := New(runtime)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = updated.(Model)
	child := launchTestSubagent(t, runtime, "child")
	updated, _ = m.Update(eventMsg(seam.Event{AgentID: child.ID, AgentTitle: child.Title, Kind: "status", Text: "subagent launched"}))
	m = updated.(Model)
	m.viewAgentID = child.ID
	m.focusAgents = true
	m.selected = 1

	endTestSubagent(t, runtime, child.ID)
	updated, _ = m.Update(eventMsg(seam.Event{AgentID: child.ID, AgentTitle: child.Title, Kind: "status", Text: "stopped"}))
	m = updated.(Model)

	if containsAgent(m.agents, child.ID) {
		t.Fatalf("stopped child remains in active agents: %#v", m.agents)
	}
	if m.viewAgentID != seatID(runtime) {
		t.Fatalf("view stayed on ended child %q, want seat %q", m.viewAgentID, seatID(runtime))
	}
	if strings.Contains(m.agentPanel(), "child") {
		t.Fatalf("ended child remains selectable in panel: %q", m.agentPanel())
	}
	if _, err := runtime.TranscriptPath(child.ID); err != nil {
		t.Fatalf("ended child transcript was not preserved: %v", err)
	}
}

func TestStatusLineFormatsContextAndCacheStats(t *testing.T) {
	agent := seam.AgentSnapshot{
		ContextWindow:   128000,
		ContextUsed:     1326,
		CacheHitTokens:  1152,
		CacheMissTokens: 246,
	}
	if got := formatContextStats(agent); got != "1.3k/126.7k" {
		t.Fatalf("context stats = %q", got)
	}
	if got := formatCacheStats(agent); got != "82%" {
		t.Fatalf("cache stats = %q", got)
	}
	if got := formatContextStats(seam.AgentSnapshot{}); got != "--/--" {
		t.Fatalf("empty context stats = %q", got)
	}
	if got := formatCacheStats(seam.AgentSnapshot{}); got != "--" {
		t.Fatalf("empty cache stats = %q", got)
	}
}

func TestMessageBlocksAreSpacedAndColored(t *testing.T) {
	runtime, err := harness.New(config.Config{Home: t.TempDir(), SeatModel: "test", SeatEffort: "high"}, harness.Options{Provider: func(string) (provider.Provider, error) { return quietProvider{}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	width := 40
	renderModel := Model{}
	user := seam.Event{AgentID: seatID(runtime), AgentTitle: "seat", Kind: "user", Text: "hello"}
	thinking := seam.Event{AgentID: seatID(runtime), AgentTitle: "seat", Kind: "thinking", Text: "working"}
	assistant := seam.Event{AgentID: seatID(runtime), AgentTitle: "seat-agent", Kind: "assistant", Text: "done"}

	if got := lipgloss.Width(renderModel.renderEvent(user, width)); got != width {
		t.Fatalf("user block width=%d, want %d", got, width)
	}
	if got := lipgloss.Width(renderModel.renderEvent(assistant, width)); got != width {
		t.Fatalf("assistant block width=%d, want %d", got, width)
	}

	userBlock := renderModel.renderEvent(user, width)
	assistantBlock := renderModel.renderEvent(assistant, width)
	if !strings.Contains(userBlock, "48;5;22") {
		t.Fatalf("user block has no colored background: %q", userBlock)
	}
	userHeader := strings.Split(userBlock, "\n")[0]
	if strings.Contains(userHeader, "48;5;22") {
		t.Fatalf("user header is still inside the colored block: %q", userHeader)
	}
	if !strings.Contains(userHeader, "38;5;220") {
		t.Fatalf("user header is not yellow: %q", userHeader)
	}
	if header := ansi.Strip(userHeader); !strings.Contains(header, "• user") || strings.Contains(header, "user>") {
		t.Fatalf("unexpected user header: %q", header)
	}
	if !strings.Contains(assistantBlock, "48;5;24") {
		t.Fatalf("assistant block has no colored background: %q", assistantBlock)
	}
	assistantHeader := strings.Split(assistantBlock, "\n")[0]
	if strings.Contains(assistantHeader, "48;5;24") {
		t.Fatalf("assistant header is still inside the colored block: %q", assistantHeader)
	}
	if !strings.Contains(assistantHeader, "38;5;220") {
		t.Fatalf("assistant header is not yellow: %q", assistantHeader)
	}
	if header := ansi.Strip(assistantHeader); !strings.Contains(header, "• seat-agent") || strings.Contains(header, "agent>") {
		t.Fatalf("unexpected assistant header: %q", header)
	}

	m := New(runtime)
	m.width = width
	m.height = 12
	m.events = []seam.Event{user, thinking, assistant}
	m.refreshView()
	content := ansi.Strip(m.viewport.View())
	lines := strings.Split(content, "\n")
	hasBlankAfter := func(prefix string) bool {
		for i, line := range lines[:len(lines)-1] {
			if strings.HasPrefix(line, prefix) {
				return strings.TrimSpace(lines[i+1]) == ""
			}
		}
		return false
	}
	if !hasBlankAfter("hello") {
		t.Fatalf("user block is not separated from following text: %q", content)
	}
	if !hasBlankAfter("working") {
		t.Fatalf("text is not separated from following assistant block: %q", content)
	}
	if strings.Contains(content, "thinking · working") {
		t.Fatalf("thinking label was duplicated in its block body: %q", content)
	}
}

func TestAgentChatBlocksRenderMarkdownWithoutBackgroundSGR(t *testing.T) {
	m := Model{}
	source := "# Rendered heading\n\nThis is **bold**.\n\n- first\n- second\n\n```go\nfmt.Println(\"hello\")\n```"
	event := seam.Event{Kind: "assistant", AgentTitle: "seat", Text: source}
	block := m.renderEvent(event, 60)
	plain := ansi.Strip(block)
	if strings.Contains(plain, "**") {
		t.Fatalf("bold markdown markers survived rendering: %q", plain)
	}
	if !strings.Contains(plain, "Rendered heading") {
		t.Fatalf("rendered heading is missing: %q", plain)
	}
	if strings.Contains(plain, "```") || !strings.Contains(plain, "fmt.Println") {
		t.Fatalf("fenced code block was not rendered: %q", plain)
	}

	markdown := m.renderMarkdown(markdownBlockKey(event), source, 60)
	backgroundSGR := regexp.MustCompile("\\x1b\\[(4[0-7]|10[0-7]|48;5;[0-9]+|48;2;[0-9]+;[0-9]+;[0-9]+)m")
	if match := backgroundSGR.FindString(markdown); match != "" {
		t.Fatalf("markdown renderer emitted background-setting SGR %q in %q", match, markdown)
	}
}

func TestLiteralEventKindsDoNotRenderMarkdown(t *testing.T) {
	m := Model{}
	source := "# literal **markdown** `source`"
	for _, event := range []seam.Event{
		{Kind: "user", Text: source},
		{Kind: "tool_result", Text: source},
	} {
		rendered := ansi.Strip(m.renderEvent(event, 80))
		if !strings.Contains(rendered, source) {
			t.Fatalf("%s event changed markdown-looking source: %q", event.Kind, rendered)
		}
	}
}

func TestMarkdownRenderingCachesBySourceAndWidth(t *testing.T) {
	runtime, err := harness.New(config.Config{Home: t.TempDir(), SeatModel: "test", SeatEffort: "high"}, harness.Options{Provider: func(string) (provider.Provider, error) { return quietProvider{}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	m := New(runtime)
	m.width, m.height = 60, 24
	assistantText := "A **formatted** response with enough words to wrap differently at a much narrower terminal width."
	m.events = []seam.Event{
		{AgentID: seatID(runtime), Kind: "user", Text: "show me"},
		{AgentID: seatID(runtime), Kind: "assistant", Text: assistantText},
	}
	m.refreshView()
	wide := m.viewport.View()
	wideMarkdown := m.markdownCache[markdownCacheKey{source: assistantText, width: 60}]
	wideRenderer := m.markdownRender
	if len(m.markdownCache) != 1 {
		t.Fatalf("markdown cache has %d entries, want 1", len(m.markdownCache))
	}
	m.refreshView()
	if repeated := m.viewport.View(); repeated != wide {
		t.Fatalf("repeated refresh changed output:\nfirst: %q\nagain: %q", wide, repeated)
	}
	if m.markdownRender != wideRenderer || len(m.markdownCache) != 1 {
		t.Fatal("repeated refresh rebuilt the renderer or missed the cached output")
	}

	m.width = 24
	m.refreshView()
	narrow := m.viewport.View()
	narrowMarkdown := m.markdownCache[markdownCacheKey{source: assistantText, width: 24}]
	if narrow == wide {
		t.Fatalf("width change reused stale rendered output: %q", narrow)
	}
	if narrowMarkdown == wideMarkdown || lipgloss.Height(ansi.Strip(narrowMarkdown)) <= lipgloss.Height(ansi.Strip(wideMarkdown)) {
		t.Fatalf("markdown was not newly wrapped for width 24:\nwide: %q\nnarrow: %q", wideMarkdown, narrowMarkdown)
	}
	if m.markdownRender == wideRenderer || m.markdownWidth != 24 {
		t.Fatal("width change did not rebuild the width-bound renderer")
	}
	for key := range m.markdownCache {
		if key.width != 24 {
			t.Fatalf("cache retained stale width %d after resize", key.width)
		}
	}

	m.clearCurrentView()
	if len(m.markdownCache) != 0 {
		t.Fatalf("clear retained %d cached markdown entries", len(m.markdownCache))
	}
}

func TestMarkdownRenderingFallsBackAtNonPositiveWidths(t *testing.T) {
	m := Model{}
	event := seam.Event{Kind: "assistant", Text: "**literal fallback**"}
	for _, width := range []int{0, -1} {
		t.Run(fmt.Sprintf("width_%d", width), func(t *testing.T) {
			defer func() {
				if recovered := recover(); recovered != nil {
					t.Fatalf("rendering width %d panicked: %v", width, recovered)
				}
			}()
			if rendered := ansi.Strip(m.renderEvent(event, width)); !strings.Contains(rendered, event.Text) {
				t.Fatalf("width %d did not use literal fallback: %q", width, rendered)
			}
		})
	}
}

func TestChildResultsRenderAsNamedPinkMessageBlocks(t *testing.T) {
	runtime, err := harness.New(config.Config{Home: t.TempDir(), SeatModel: "test", SeatEffort: "high"}, harness.Options{Provider: func(string) (provider.Provider, error) { return quietProvider{}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	renderModel := Model{}
	childResult := seam.Event{AgentID: seatID(runtime), AgentTitle: "researcher", Kind: "child_result", Text: "findings"}
	block := renderModel.renderEvent(childResult, 40)
	if !strings.Contains(block, "48;5;132") {
		t.Fatalf("child result has no pink background: %q", block)
	}
	parts := strings.Split(block, "\n")
	header := parts[0]
	if strings.Contains(header, "48;5;132") {
		t.Fatalf("child result header is inside the colored block: %q", header)
	}
	if !strings.Contains(header, "38;5;220") || !strings.Contains(ansi.Strip(header), "• researcher") {
		t.Fatalf("child result header is not named and yellow: %q", header)
	}
	if len(parts) < 2 || !strings.Contains(ansi.Strip(parts[1]), "findings") {
		t.Fatalf("child result body missing: %q", block)
	}
	if !isMessage(childResult) {
		t.Fatal("child result is not treated as a chat message")
	}

	forwarded := seam.Event{AgentID: seatID(runtime), AgentTitle: "researcher", Kind: "steer", Text: "progress update", Metadata: map[string]any{"sender": "child-id"}}
	forwardedBlock := renderModel.renderEvent(forwarded, 40)
	if !strings.Contains(forwardedBlock, "48;5;132") || !strings.Contains(ansi.Strip(strings.Split(forwardedBlock, "\n")[0]), "• researcher") {
		t.Fatalf("forwarded child message is not a named pink block: %q", forwardedBlock)
	}
	if !isMessage(forwarded) {
		t.Fatal("forwarded child message is not treated as a chat message")
	}
}

func TestLeadingControlEventsStayOutOfMessageViewport(t *testing.T) {
	runtime, err := harness.New(config.Config{Home: t.TempDir(), SeatModel: "test", SeatEffort: "high"}, harness.Options{Provider: func(string) (provider.Provider, error) { return quietProvider{}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	m := New(runtime)
	m.width = 40
	m.events = []seam.Event{
		{AgentID: seatID(runtime), Kind: "runtime", Text: "runtime started"},
		{AgentID: seatID(runtime), Kind: "status", Text: "thinking"},
		{AgentID: seatID(runtime), Kind: "user", Text: "hi"},
		{AgentID: seatID(runtime), Kind: "status", Text: "idle"},
		{AgentID: seatID(runtime), Kind: "usage", Text: "token usage should stay hidden"},
		{AgentID: seatID(runtime), Kind: "inference_request", Text: "wire payload should stay hidden"},
		{AgentID: seatID(runtime), Kind: "thinking", Text: "working"},
		{AgentID: seatID(runtime), Kind: "turn_done", Text: "lifecycle event should stay hidden"},
	}
	m.refreshView()
	content := ansi.Strip(m.viewport.View())
	if strings.Contains(content, "runtime started") || strings.Contains(content, "status · thinking") || strings.Contains(content, "status · idle") || strings.Contains(content, "usage ·") || strings.Contains(content, "token usage should stay hidden") {
		t.Fatalf("control events leaked into the message viewport: %q", content)
	}
	if !strings.Contains(content, "• user") || !strings.Contains(content, "hi") || !strings.Contains(content, "thinking") || !strings.Contains(content, "working") {
		t.Fatalf("post-user content missing from the message viewport: %q", content)
	}
	if strings.Contains(content, "inference_request") || strings.Contains(content, "wire payload should stay hidden") {
		t.Fatalf("inference request leaked into the message viewport: %q", content)
	}
	if strings.Contains(content, "turn_done") || strings.Contains(content, "lifecycle event should stay hidden") {
		t.Fatalf("turn completion leaked into the message viewport: %q", content)
	}
}

func TestClearCommandKeepsSubsequentMessagesVisible(t *testing.T) {
	runtime, err := harness.New(config.Config{Home: t.TempDir(), SeatModel: "test", SeatEffort: "high"}, harness.Options{Provider: func(string) (provider.Provider, error) { return quietProvider{}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	m := New(runtime)
	m.width = 40
	seatID := seatID(runtime)
	cursor := runtime.PollEvents(seam.EventQuery{}).Cursor
	_, _ = runtime.Do(seam.SendPromptCommand{AgentID: seatID, Prompt: "old prompt"})
	waitForRuntimeKind(t, runtime, &cursor, "turn_done")
	m.events = []seam.Event{
		{AgentID: seatID, Kind: "user", Text: "old prompt"},
		{AgentID: seatID, Kind: "assistant", Text: "old answer"},
	}
	m.refreshView()
	if content := ansi.Strip(m.viewport.View()); !strings.Contains(content, "old answer") {
		t.Fatalf("initial message missing before clear: %q", content)
	}

	m.handleCommand("/clear")
	transcript, err := runtime.TranscriptPath(seatID)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(transcript)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "old prompt") {
		t.Fatalf("cleared transcript retained old prompt: %s", data)
	}
	if content := ansi.Strip(m.viewport.View()); strings.Contains(content, "old prompt") || strings.Contains(content, "old answer") {
		t.Fatalf("cleared messages remain visible: %q", content)
	}

	_, _ = runtime.Do(seam.SendPromptCommand{AgentID: seatID, Prompt: "new prompt"})
	waitForRuntimeKind(t, runtime, &cursor, "turn_done")
	data, err = os.ReadFile(transcript)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "new prompt") || strings.Contains(string(data), "old prompt") {
		t.Fatalf("post-clear transcript has wrong history: %s", data)
	}

	updated, _ := m.Update(eventMsg(seam.Event{AgentID: seatID, Kind: "status", Text: "thinking"}))
	m = updated.(Model)
	updated, _ = m.Update(eventMsg(seam.Event{AgentID: seatID, Kind: "user", Text: "new prompt"}))
	m = updated.(Model)
	updated, _ = m.Update(eventMsg(seam.Event{AgentID: seatID, Kind: "assistant", Text: "new answer"}))
	m = updated.(Model)
	content := ansi.Strip(m.viewport.View())
	if !strings.Contains(content, "new prompt") || !strings.Contains(content, "new answer") {
		t.Fatalf("messages after clear are missing: %q", content)
	}
}

func TestNonChatBlocksRollAtTenLines(t *testing.T) {
	runtime, err := harness.New(config.Config{Home: t.TempDir(), SeatModel: "test", SeatEffort: "high"}, harness.Options{Provider: func(string) (provider.Provider, error) { return quietProvider{}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	lines := make([]string, 12)
	for i := range lines {
		lines[i] = fmt.Sprintf("line %02d", i+1)
	}
	text := strings.Join(lines, "\n")
	renderModel := Model{}

	thinking := ansi.Strip(renderModel.renderEvent(seam.Event{Kind: "thinking", Text: text}, 40))
	if got := lipgloss.Height(thinking); got != nonChatBlockHeight {
		t.Fatalf("thinking block height=%d, want %d", got, nonChatBlockHeight)
	}
	if strings.Contains(thinking, "line 01") || !strings.Contains(thinking, "line 12") {
		t.Fatalf("thinking block did not keep the newest lines: %q", thinking)
	}

	toolResult := ansi.Strip(renderModel.renderEvent(seam.Event{Kind: "tool_result", Text: text}, 40))
	if got := lipgloss.Height(toolResult); got != nonChatBlockHeight {
		t.Fatalf("tool result block height=%d, want %d", got, nonChatBlockHeight)
	}
	if strings.Contains(toolResult, "line 01") || !strings.Contains(toolResult, "line 12") {
		t.Fatalf("tool result block did not keep the newest lines: %q", toolResult)
	}

	short := renderModel.renderEvent(seam.Event{Kind: "thinking", Text: "working"}, 40)
	if got := lipgloss.Height(short); got != 1 {
		t.Fatalf("short thinking block height=%d, want 1", got)
	}

	nonChat := renderModel.renderEvent(seam.Event{Kind: "thinking", Text: "working"}, 40)
	if !strings.Contains(nonChat, "48;5;236") {
		t.Fatalf("non-chat block has no gray background: %q", nonChat)
	}
	header := renderNonChatBlock(
		[]seam.Event{{Kind: "thinking", Text: "working"}},
		[]string{nonChat},
		40,
	)
	if firstLine := strings.Split(header, "\n")[0]; strings.Contains(firstLine, "48;5;236") {
		t.Fatalf("non-chat header is still inside the gray block: %q", firstLine)
	}
	if firstLine := strings.Split(header, "\n")[0]; !strings.Contains(firstLine, "38;5;220") {
		t.Fatalf("non-chat header is not yellow: %q", firstLine)
	}
	if firstLine := ansi.Strip(strings.Split(header, "\n")[0]); !strings.Contains(firstLine, "• thinking") {
		t.Fatalf("non-chat header lacks bullet prefix: %q", firstLine)
	}

	m := New(runtime)
	m.width = 40
	m.events = []seam.Event{
		{AgentID: seatID(runtime), Kind: "user", Text: "hi"},
		{AgentID: seatID(runtime), Kind: "thinking", Text: text},
		{AgentID: seatID(runtime), Kind: "tool_result", Text: text, Metadata: map[string]any{"name": "quick_bash"}},
	}
	m.refreshView()
	if got := m.viewport.TotalLineCount(); got != nonChatBlockHeight+4 {
		t.Fatalf("consecutive non-chat content used %d lines, want %d", got, nonChatBlockHeight+4)
	}
	content := ansi.Strip(m.viewport.View())
	if !strings.Contains(content, "quick_bash(1)") || strings.Contains(content, "line 01") || !strings.Contains(content, "line 12") {
		t.Fatalf("shared non-chat block did not keep the newest lines: %q", content)
	}
}

func TestNonChatHeaderTalliesThinkingTimeAndToolCalls(t *testing.T) {
	start := time.Unix(100, 0)
	events := []seam.Event{
		{Kind: "thinking", Time: start, Text: "first"},
		{Kind: "thinking", Time: start.Add(2 * time.Second), Text: "second"},
		{Kind: "tool", Time: start.Add(2 * time.Second), Text: `{`, Metadata: map[string]any{"name": "edit_file", "call_id": "edit-1", "index": 0}},
		{Kind: "tool", Time: start.Add(2 * time.Second), Text: `}`, Metadata: map[string]any{"name": "edit_file", "call_id": "edit-1", "index": 0}},
		{Kind: "tool_result", Time: start.Add(3 * time.Second), Text: "edited", Metadata: map[string]any{"name": "edit_file", "call_id": "edit-1"}},
		{Kind: "tool", Time: start.Add(3 * time.Second), Text: `{}`, Metadata: map[string]any{"name": "edit_file", "call_id": "edit-2", "index": 1}},
		{Kind: "tool_result", Time: start.Add(4 * time.Second), Text: "edited", Metadata: map[string]any{"name": "edit_file", "call_id": "edit-2"}},
		{Kind: "thinking", Time: start.Add(10 * time.Second), Text: "next"},
		{Kind: "thinking", Time: start.Add(13 * time.Second), Text: "thought"},
		{Kind: "tool", Time: start.Add(20 * time.Second), Text: `{}`, Metadata: map[string]any{"name": "read_file", "call_id": "read-1", "index": 2}},
		{Kind: "thinking", Time: start.Add(20 * time.Second), Text: "single chunk"},
		{Kind: "tool", Time: start.Add(24 * time.Second), Text: `{}`, Metadata: map[string]any{"name": "read_file", "call_id": "read-2", "index": 3}},
	}

	header := ansi.Strip(nonChatHeader(events))
	for _, want := range []string{"• thinking(16s)", "edit_file(2)", "read_file(2)"} {
		if !strings.Contains(header, want) {
			t.Fatalf("header=%q, missing %q", header, want)
		}
	}
	unnamedResults := []seam.Event{
		{Kind: "tool_result", Metadata: map[string]any{"call_id": "unnamed-1"}},
		{Kind: "tool_result", Metadata: map[string]any{"call_id": "unnamed-2"}},
		{Kind: "tool_result", Metadata: map[string]any{"call_id": "unnamed-3"}},
	}
	if got := ansi.Strip(nonChatHeader(unnamedResults)); !strings.Contains(got, "• tool(3)") {
		t.Fatalf("unnamed tool header=%q, want tool(3)", got)
	}
}

func TestMessageViewportScrollsWithKeyboardAndMouse(t *testing.T) {
	runtime, err := harness.New(config.Config{Home: t.TempDir(), SeatModel: "test", SeatEffort: "high"}, harness.Options{Provider: func(string) (provider.Provider, error) { return quietProvider{}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	m := New(runtime)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 40, Height: 12})
	m = updated.(Model)
	m.events = append(m.events, seam.Event{AgentID: seatID(runtime), AgentTitle: "seat", Kind: "user", Text: "hi"})
	for i := 0; i < 20; i++ {
		m.events = append(m.events, seam.Event{AgentID: seatID(runtime), AgentTitle: "seat", Kind: "assistant", Text: fmt.Sprintf("message %02d %s", i, strings.Repeat("content ", 8))})
	}
	m.refreshView()
	if !m.viewport.AtBottom() {
		t.Fatal("viewport should start at the bottom")
	}
	m.history = []string{"earlier prompt"}
	updated, _ = m.updateKey(tea.KeyPressMsg{Code: tea.KeyUp})
	m = updated.(Model)
	if m.viewport.AtBottom() || m.input.Value() != "" {
		t.Fatalf("Up should scroll an empty-input viewport without recalling history (offset=%d input=%q)", m.viewport.YOffset(), m.input.Value())
	}
	upOffset := m.viewport.YOffset()
	updated, _ = m.updateKey(tea.KeyPressMsg{Code: tea.KeyDown})
	m = updated.(Model)
	if m.viewport.YOffset() <= upOffset || m.focusAgents {
		t.Fatalf("Down should scroll the viewport (offset=%d focusAgents=%v)", m.viewport.YOffset(), m.focusAgents)
	}

	updated, _ = m.updateKey(tea.KeyPressMsg{Code: tea.KeyPgUp})
	m = updated.(Model)
	if m.viewport.AtBottom() || !m.userScrolled {
		t.Fatalf("PageUp did not move the viewport or mark it as user-scrolled (offset=%d max=%d lines=%d height=%d)", m.viewport.YOffset(), m.viewport.TotalLineCount()-m.viewport.Height(), m.viewport.TotalLineCount(), m.viewport.Height())
	}
	yOffset := m.viewport.YOffset()
	updated, _ = m.Update(eventMsg(seam.Event{AgentID: seatID(runtime), AgentTitle: "seat", Kind: "status", Text: "streaming"}))
	m = updated.(Model)
	if m.viewport.YOffset() != yOffset {
		t.Fatalf("stream refresh changed scrolled offset from %d to %d", yOffset, m.viewport.YOffset())
	}

	updated, _ = m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	m = updated.(Model)
	if m.viewport.YOffset() <= yOffset {
		t.Fatal("mouse wheel down did not move the viewport")
	}
	for !m.viewport.AtBottom() {
		updated, _ = m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
		m = updated.(Model)
	}
	if m.userScrolled {
		t.Fatal("viewport remained marked as scrolled after reaching the bottom")
	}
	updated, _ = m.updateKey(tea.KeyPressMsg{Code: tea.KeyDown, Mod: tea.ModCtrl})
	m = updated.(Model)
	if !m.focusAgents {
		t.Fatal("Ctrl-Down did not focus the agent list")
	}
}

func TestInputFrameExpandsForMultilineMessages(t *testing.T) {
	runtime, err := harness.New(config.Config{Home: t.TempDir(), SeatModel: "test", SeatEffort: "high"}, harness.Options{Provider: func(string) (provider.Provider, error) { return quietProvider{}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	m := New(runtime)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 40, Height: 12})
	m = updated.(Model)
	if got := lipgloss.Height(ansi.Strip(m.inputFrame())); got != 3 {
		t.Fatalf("single-line input frame height=%d, want 3", got)
	}
	m.input.SetValue("first\nsecond")
	m.syncInputHeight()
	if got := lipgloss.Height(ansi.Strip(m.inputFrame())); got != 4 {
		t.Fatalf("multiline input frame height=%d, want 4", got)
	}
	lines := strings.Split(ansi.Strip(m.inputFrame()), "\n")
	if !strings.Contains(lines[0], "─") || !strings.Contains(lines[len(lines)-1], "─") {
		t.Fatalf("input frame lacks horizontal rules: %q", lines)
	}
	m.input.Reset()
	m.input.InsertString(strings.Repeat("x", 80))
	m.syncInputHeight()
	if got := lipgloss.Height(ansi.Strip(m.inputFrame())); got != 5 {
		t.Fatalf("long input frame height=%d, want two content lines plus a viewport row and rules", got)
	}
	m.input.SetValue(strings.Repeat("x", 60))
	if got := lipgloss.Height(ansi.Strip(m.inputFrame())); got != 4 {
		t.Fatalf("soft-wrapped input frame height=%d, want two content lines plus rules", got)
	}
	m.input.SetValue(strings.Repeat("x", 40) + "\n" + strings.Repeat("x", 20))
	center := strings.Split(ansi.Strip(m.inputFrame()), "\n")
	if !strings.Contains(center[1], strings.Repeat("x", 40)) || !strings.Contains(center[3], strings.Repeat("x", 20)) {
		t.Fatalf("explicit multiline input lost its first line: %q", center)
	}
	m.input.SetValue(strings.Repeat("abcd ", 8) + "s")
	if rendered := ansi.Strip(m.inputFrame()); !strings.Contains(rendered, "abcd") || !strings.Contains(rendered, "s") {
		t.Fatalf("spaced line lost content at the wrap boundary: %q", rendered)
	}
	m.input.Reset()
	m.input.InsertString(strings.Repeat("a", 40))
	m.syncInputHeight()
	updated, _ = m.updateKey(tea.KeyPressMsg{Code: 'a', Text: "a"})
	m = updated.(Model)
	boundaryLines := strings.Split(ansi.Strip(m.inputFrame()), "\n")
	if len(boundaryLines) < 4 || !strings.Contains(boundaryLines[1], strings.Repeat("a", 40)) || !strings.Contains(boundaryLines[2], "a") {
		t.Fatalf("boundary keystroke lost the first row: %q", boundaryLines)
	}
	m.input.Reset()
	m.input.InsertString("first")
	updated, _ = m.updateKey(tea.KeyPressMsg{Code: 'j', Mod: tea.ModCtrl})
	m = updated.(Model)
	if got := m.input.Value(); got != "first\n" {
		t.Fatalf("Ctrl-J input=%q, want a newline", got)
	}
}

func TestExpandedInputKeepsFooterVisible(t *testing.T) {
	runtime, err := harness.New(config.Config{Home: t.TempDir(), SeatModel: "test", SeatEffort: "high"}, harness.Options{Provider: func(string) (provider.Provider, error) { return quietProvider{}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	m := New(runtime)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 40, Height: 12})
	m = updated.(Model)
	m.input.SetValue(strings.Repeat("a", 240))
	m.syncInputHeight()

	view := ansi.Strip(m.View().Content)
	if got := lipgloss.Height(view); got != 12 {
		t.Fatalf("expanded view height=%d, want 12: %q", got, view)
	}
	if !strings.Contains(view, runtime.ID()) || !strings.Contains(view, "test high") {
		t.Fatalf("expanded view lost the status footer: %q", view)
	}
	if got := m.viewport.Height(); got < 1 {
		t.Fatalf("expanded input left no chat viewport row: %d", got)
	}
}

func TestHistoryIsMachineGlobalAndBashStyle(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, historyFileName)
	if err := appendHistory(path, "first\nline"); err != nil {
		t.Fatal(err)
	}
	if err := appendHistory(path, "second"); err != nil {
		t.Fatal(err)
	}
	history, err := loadHistory(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(history, "|"), "first\nline|second"; got != want {
		t.Fatalf("loaded history=%q, want %q", got, want)
	}

	runtime, err := harness.New(config.Config{Home: home, SeatModel: "test", SeatEffort: "high"}, harness.Options{Provider: func(string) (provider.Provider, error) { return quietProvider{}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	m := New(runtime)
	m.input.SetValue("draft")
	updated, _ := m.updateKey(tea.KeyPressMsg{Code: tea.KeyUp})
	m = updated.(Model)
	if got := m.input.Value(); got != "draft" {
		t.Fatalf("plain Up changed draft to %q", got)
	}
	updated, _ = m.updateKey(tea.KeyPressMsg{Code: tea.KeyUp, Mod: tea.ModAlt})
	m = updated.(Model)
	if got := m.input.Value(); got != "second" {
		t.Fatalf("first Up recalled %q, want second", got)
	}
	updated, _ = m.updateKey(tea.KeyPressMsg{Code: tea.KeyUp, Mod: tea.ModAlt})
	m = updated.(Model)
	if got := m.input.Value(); got != "first\nline" {
		t.Fatalf("second Up recalled %q, want multiline first message", got)
	}
	updated, _ = m.updateKey(tea.KeyPressMsg{Code: tea.KeyDown, Mod: tea.ModAlt})
	m = updated.(Model)
	updated, _ = m.updateKey(tea.KeyPressMsg{Code: tea.KeyDown, Mod: tea.ModAlt})
	m = updated.(Model)
	if got := m.input.Value(); got != "draft" {
		t.Fatalf("Down past newest history returned %q, want draft", got)
	}
}

func TestSlashCommandTabCompletion(t *testing.T) {
	runtime, err := harness.New(config.Config{Home: t.TempDir(), SeatModel: "test", SeatEffort: "high"}, harness.Options{Provider: func(string) (provider.Provider, error) { return quietProvider{}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	m := New(runtime)
	m.input.SetValue("/mo")
	updated, _ := m.updateKey(tea.KeyPressMsg{Code: tea.KeyTab})
	m = updated.(Model)
	if got := m.input.Value(); got != "/model" {
		t.Fatalf("completed command=%q, want /model", got)
	}
	m.input.SetValue("/c")
	updated, _ = m.updateKey(tea.KeyPressMsg{Code: tea.KeyTab})
	m = updated.(Model)
	if got := m.input.Value(); got != "/c" {
		t.Fatalf("ambiguous command changed to %q", got)
	}
}

func TestMouseCaptureIsOffUntilToggled(t *testing.T) {
	runtime, err := harness.New(config.Config{Home: t.TempDir(), SeatModel: "test", SeatEffort: "high"}, harness.Options{Provider: func(string) (provider.Provider, error) { return quietProvider{}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	m := New(runtime)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 60, Height: 20})
	m = updated.(Model)
	if got := m.View().MouseMode; got != tea.MouseModeNone {
		t.Fatalf("default MouseMode=%v, want MouseModeNone so the terminal can select text", got)
	}

	m.handleCommand("/mouse")
	if got := m.View().MouseMode; got != tea.MouseModeCellMotion {
		t.Fatalf("MouseMode after /mouse=%v, want MouseModeCellMotion", got)
	}
	if status := ansi.Strip(m.statusLine()); !strings.Contains(status, "mouse") {
		t.Fatalf("status line=%q, want a mouse indicator while capture is on", status)
	}

	m.handleCommand("/mouse")
	if got := m.View().MouseMode; got != tea.MouseModeNone {
		t.Fatalf("MouseMode after second /mouse=%v, want MouseModeNone", got)
	}
	if status := ansi.Strip(m.statusLine()); strings.Contains(status, "mouse") {
		t.Fatalf("status line=%q, want no mouse indicator while capture is off", status)
	}
}

func TestChatBlocksOpenOnTheirFirstLineOfText(t *testing.T) {
	m := Model{}
	event := seam.Event{Kind: "assistant", AgentTitle: "seat", Text: "first line of the answer"}
	body := strings.Split(m.renderEvent(event, 40), "\n")
	if len(body) < 2 {
		t.Fatalf("block has no body: %q", body)
	}
	if got := strings.TrimSpace(ansi.Strip(body[1])); got != "first line of the answer" {
		t.Fatalf("block opens with %q, want the first line of text and no blank painted row", got)
	}

	// Glamour pads a document with styled blank lines at both ends. They carry
	// escape sequences, so emptiness has to be judged on the stripped text.
	fenced := seam.Event{Kind: "assistant", Text: "text\n\n```go\nfunc main() {}\n```\n"}
	lines := strings.Split(m.renderEvent(fenced, 40), "\n")
	if last := strings.TrimSpace(ansi.Strip(lines[len(lines)-1])); last == "" {
		t.Fatalf("block ends on a blank painted row: %q", lines[len(lines)-1])
	}
}

func TestInlineCodeKeepsTextSelectable(t *testing.T) {
	m := Model{}
	event := seam.Event{Kind: "assistant", Text: "call `fmt.Println` to print"}
	rendered := ansi.Strip(m.renderEvent(event, 60))
	if strings.ContainsRune(rendered, '\u00a0') {
		t.Fatalf("inline code padded with non-breaking spaces, which copy as U+00A0: %q", rendered)
	}
	if !strings.Contains(rendered, "call fmt.Println to print") {
		t.Fatalf("inline code did not render as plain adjacent text: %q", rendered)
	}
}

func TestStreamedMarkdownRendersAreThrottled(t *testing.T) {
	m := Model{markdownCache: map[markdownCacheKey]string{}, markdownRecent: map[string]markdownRecentRender{}}
	event := seam.Event{AgentID: "seat", Kind: "assistant"}
	key := markdownBlockKey(event)

	full := strings.Repeat("A paragraph with **bold** text that wraps across the block. ", 10)
	for i := 1; i <= len(full); i += 8 {
		m.renderMarkdown(key, full[:i], 60)
	}
	chunks := len(full)/8 + 1
	if len(m.markdownCache) >= chunks {
		t.Fatalf("throttle did not bound rendering: %d renders for %d chunks", len(m.markdownCache), chunks)
	}
	if !m.markdownStale {
		t.Fatal("skipped renders did not mark the view stale")
	}

	// A stale view schedules exactly one catch-up tick, never a backlog.
	if cmd := m.markdownTickCmd(); cmd == nil {
		t.Fatal("stale view scheduled no catch-up tick")
	}
	if cmd := m.markdownTickCmd(); cmd != nil {
		t.Fatal("a second tick was scheduled while one was already in flight")
	}

	// Once the throttle window passes, the newest text renders in full.
	m.markdownTicking = false
	m.markdownRenderedAt = time.Now().Add(-2 * markdownThrottle)
	rendered := ansi.Strip(m.renderMarkdown(key, full, 60))
	if strings.Contains(rendered, "**") {
		t.Fatalf("catch-up render left raw markdown: %q", rendered)
	}
}

func TestMarkdownStyleClearsEveryBackgroundTheDarkStyleSets(t *testing.T) {
	// These are the four BackgroundColor fields the stock dark style actually
	// populates. Asserting on a field that is already nil upstream would pass
	// against a sweep that did nothing at all.
	dark := styles.DarkStyleConfig
	upstream := map[string]*string{
		"H1":                dark.H1.BackgroundColor,
		"Code":              dark.Code.BackgroundColor,
		"Chroma.Error":      dark.CodeBlock.Chroma.Error.BackgroundColor,
		"Chroma.Background": dark.CodeBlock.Chroma.Background.BackgroundColor,
	}
	for name, field := range upstream {
		if field == nil {
			t.Fatalf("%s has no background upstream, so this test proves nothing", name)
		}
	}

	before := *dark.CodeBlock.Chroma.Background.BackgroundColor
	style := backgroundFreeMarkdownStyle()
	swept := map[string]*string{
		"H1":                style.H1.BackgroundColor,
		"Code":              style.Code.BackgroundColor,
		"Chroma.Error":      style.CodeBlock.Chroma.Error.BackgroundColor,
		"Chroma.Background": style.CodeBlock.Chroma.Background.BackgroundColor,
	}
	for name, field := range swept {
		if field != nil {
			t.Fatalf("%s kept background %q", name, *field)
		}
	}
	if got := *styles.DarkStyleConfig.CodeBlock.Chroma.Background.BackgroundColor; got != before {
		t.Fatalf("building the style mutated glamour's package-level dark style: %q -> %q", before, got)
	}
}

// A throttled block must never be stranded. Switching the viewed agent
// refreshes the view without producing a command of its own, so the catch-up
// tick has to be scheduled centrally by Update rather than per call site.
func TestThrottledBlocksAreNeverStrandedByCommandlessUpdates(t *testing.T) {
	runtime, err := harness.New(config.Config{Home: t.TempDir(), SeatModel: "test", SeatEffort: "high"}, harness.Options{Provider: func(string) (provider.Provider, error) { return quietProvider{}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	seat := seatID(runtime)
	m := New(runtime)
	m.width, m.height = 60, 24
	m.events = []seam.Event{
		{AgentID: seat, Kind: "user", Text: "go"},
		{AgentID: seat, Kind: "assistant", Text: "a **partial** answer"},
	}
	m.refreshView()

	// Grow the message and refresh inside the throttle window, as a stream
	// does. The newest text cannot have been rendered yet.
	m.events[1].Text = "a **partial** answer that has since grown considerably longer"
	m.markdownRenderedAt = time.Now()
	m.refreshView()
	if !m.markdownStale {
		t.Fatal("throttled render did not mark the view stale")
	}

	// Esc returns the view to the seat and returns no command of its own.
	updated, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	next, ok := updated.(Model)
	if !ok {
		t.Fatalf("Update returned %T, want Model", updated)
	}
	if cmd == nil {
		t.Fatal("a commandless path stranded a stale block with no catch-up tick")
	}
	if !next.markdownTicking {
		t.Fatal("catch-up tick was not recorded as in flight")
	}

	// Delivering the tick renders the newest text in full.
	next.markdownRenderedAt = time.Now().Add(-2 * markdownThrottle)
	caught, _ := next.Update(markdownTickMsg(time.Now()))
	view := ansi.Strip(caught.(Model).viewport.View())
	if strings.Contains(view, "**") || !strings.Contains(view, "grown considerably longer") {
		t.Fatalf("catch-up tick did not render the newest text: %q", view)
	}
}

// The commandless-path test above exercises Esc with the model menu closed,
// which returns a Model. The model-menu paths have a pointer receiver and
// return *Model, so they need their own coverage.
func TestModelMenuPathsAlsoScheduleTheCatchUpTick(t *testing.T) {
	runtime, err := harness.New(config.Config{Home: t.TempDir(), SeatModel: "test", SeatEffort: "high"}, harness.Options{Provider: func(string) (provider.Provider, error) { return quietProvider{}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	seat := seatID(runtime)
	m := New(runtime)
	m.width, m.height = 60, 24
	m.events = []seam.Event{
		{AgentID: seat, Kind: "user", Text: "go"},
		{AgentID: seat, Kind: "assistant", Text: "a **partial** answer"},
	}
	m.refreshView()
	m.events[1].Text = "a **partial** answer that has since grown considerably longer"
	m.markdownRenderedAt = time.Now()
	m.refreshView()
	if !m.markdownStale {
		t.Fatal("throttled render did not mark the view stale")
	}

	m.modelsOpen = true
	updated, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	if _, ok := updated.(Model); !ok {
		t.Fatalf("Update returned %T, want a normalized Model", updated)
	}
	if cmd == nil {
		t.Fatal("the model-menu path stranded a stale block with no catch-up tick")
	}
}

func TestSeparateBlocksFromOneAgentDoNotBorrowEachOthersRenders(t *testing.T) {
	m := Model{markdownCache: map[markdownCacheKey]string{}, markdownRecent: map[string]markdownRecentRender{}}
	key := markdownBlockKey(seam.Event{AgentID: "seat", Kind: "assistant"})

	first := "the **first** answer, which is a good deal longer than the second"
	m.renderMarkdown(key, first, 60)

	// A later block of the same kind from the same agent shares the key. Inside
	// the throttle window it must not be served the earlier block's text.
	m.markdownRenderedAt = time.Now()
	second := ansi.Strip(m.renderMarkdown(key, "the s", 60))
	if strings.Contains(second, "first") {
		t.Fatalf("a new block was served the previous block's render: %q", second)
	}
	if !strings.Contains(second, "the s") {
		t.Fatalf("a new block did not render its own text: %q", second)
	}

	// A growing stream does extend its own source, so it may reuse.
	m.markdownRenderedAt = time.Now()
	grown := ansi.Strip(m.renderMarkdown(key, "the second one", 60))
	if !strings.Contains(grown, "the s") {
		t.Fatalf("a streaming block did not reuse its own previous render: %q", grown)
	}
}

func TestFencedCodeKeepsItsOwnBlankLines(t *testing.T) {
	m := Model{}
	// The blank lines here belong to the code block, not to glamour's document
	// padding, and a greedy edge trim silently eats them.
	event := seam.Event{Kind: "assistant", Text: "```go\n\nfunc main() {}\n\n```"}
	rendered := m.renderEvent(event, 40)
	if !strings.Contains(ansi.Strip(rendered), "func main() {}") {
		t.Fatalf("code content was lost: %q", rendered)
	}
	body := strings.Split(rendered, "\n")[1:]
	if len(body) < 3 {
		t.Fatalf("code block collapsed to %d lines, losing its blank lines: %q", len(body), body)
	}
}
