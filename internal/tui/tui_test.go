package tui

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/slbdotdev/slbh/internal/config"
	"github.com/slbdotdev/slbh/internal/harness"
	"github.com/slbdotdev/slbh/internal/provider"
)

type quietProvider struct{}

func (quietProvider) Stream(context.Context, provider.Request, provider.StreamSink) error { return nil }

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
		harness.Event{AgentID: runtime.Seat().ID, AgentTitle: "seat", Kind: "user", Text: "hi"},
		harness.Event{AgentID: runtime.Seat().ID, AgentTitle: "seat", Kind: "error", Text: strings.Repeat("long error ", 20)},
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
	m.agents = append(m.agents, harness.AgentSnapshot{ID: "child", Title: "child", Status: "idle", Depth: 1})
	if got := lipgloss.Width(m.agentPanel()); got != 80 {
		t.Fatalf("agent list width=%d, want full terminal width", got)
	}
	if strings.Contains(m.statusLine(), "Enter send") || strings.Contains(m.statusLine(), "agents 1") || strings.Contains(m.statusLine(), "jobs 0") {
		t.Fatal("footer contains hidden help or zero-count metadata")
	}
	if !strings.Contains(m.statusLine(), runtime.Seat().Model+" "+runtime.Seat().Effort) || !strings.Contains(m.statusLine(), "--/--") || !strings.Contains(m.statusLine(), " · --") {
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
	child, err := runtime.LaunchSubagent(runtime.Seat().ID, "child", "")
	if err != nil {
		t.Fatal(err)
	}
	updated, _ = m.Update(eventMsg(harness.Event{AgentID: child.ID, AgentTitle: child.Title, Kind: "status", Text: "subagent launched"}))
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
	m.agents = []harness.AgentSnapshot{
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
	child, err := runtime.LaunchSubagent(runtime.Seat().ID, "child", "")
	if err != nil {
		t.Fatal(err)
	}
	updated, _ = m.Update(eventMsg(harness.Event{AgentID: child.ID, AgentTitle: child.Title, Kind: "status", Text: "subagent launched"}))
	m = updated.(Model)
	m.viewAgentID = child.ID
	m.focusAgents = true
	m.selected = 1

	if err := runtime.EndSubagent(runtime.Seat().ID, child.ID); err != nil {
		t.Fatal(err)
	}
	updated, _ = m.Update(eventMsg(harness.Event{AgentID: child.ID, AgentTitle: child.Title, Kind: "status", Text: "stopped"}))
	m = updated.(Model)

	if containsAgent(m.agents, child.ID) {
		t.Fatalf("stopped child remains in active agents: %#v", m.agents)
	}
	if m.viewAgentID != runtime.Seat().ID {
		t.Fatalf("view stayed on ended child %q, want seat %q", m.viewAgentID, runtime.Seat().ID)
	}
	if strings.Contains(m.agentPanel(), "child") {
		t.Fatalf("ended child remains selectable in panel: %q", m.agentPanel())
	}
	if _, err := runtime.TranscriptPath(child.ID); err != nil {
		t.Fatalf("ended child transcript was not preserved: %v", err)
	}
}

func TestStatusLineFormatsContextAndCacheStats(t *testing.T) {
	agent := harness.AgentSnapshot{
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
	if got := formatContextStats(harness.AgentSnapshot{}); got != "--/--" {
		t.Fatalf("empty context stats = %q", got)
	}
	if got := formatCacheStats(harness.AgentSnapshot{}); got != "--" {
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
	user := harness.Event{AgentID: runtime.Seat().ID, AgentTitle: "seat", Kind: "user", Text: "hello"}
	thinking := harness.Event{AgentID: runtime.Seat().ID, AgentTitle: "seat", Kind: "thinking", Text: "working"}
	assistant := harness.Event{AgentID: runtime.Seat().ID, AgentTitle: "seat-agent", Kind: "assistant", Text: "done"}

	if got := lipgloss.Width(renderEvent(user, width)); got != width {
		t.Fatalf("user block width=%d, want %d", got, width)
	}
	if got := lipgloss.Width(renderEvent(assistant, width)); got != width {
		t.Fatalf("assistant block width=%d, want %d", got, width)
	}

	userBlock := renderEvent(user, width)
	assistantBlock := renderEvent(assistant, width)
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
	m.events = []harness.Event{user, thinking, assistant}
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

func TestChildResultsRenderAsNamedPinkMessageBlocks(t *testing.T) {
	runtime, err := harness.New(config.Config{Home: t.TempDir(), SeatModel: "test", SeatEffort: "high"}, harness.Options{Provider: func(string) (provider.Provider, error) { return quietProvider{}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	childResult := harness.Event{AgentID: runtime.Seat().ID, AgentTitle: "researcher", Kind: "child_result", Text: "findings"}
	block := renderEvent(childResult, 40)
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

	forwarded := harness.Event{AgentID: runtime.Seat().ID, AgentTitle: "researcher", Kind: "steer", Text: "progress update", Metadata: map[string]any{"sender": "child-id"}}
	forwardedBlock := renderEvent(forwarded, 40)
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
	m.events = []harness.Event{
		{AgentID: runtime.Seat().ID, Kind: "runtime", Text: "runtime started"},
		{AgentID: runtime.Seat().ID, Kind: "status", Text: "thinking"},
		{AgentID: runtime.Seat().ID, Kind: "user", Text: "hi"},
		{AgentID: runtime.Seat().ID, Kind: "status", Text: "idle"},
		{AgentID: runtime.Seat().ID, Kind: "usage", Text: "token usage should stay hidden"},
		{AgentID: runtime.Seat().ID, Kind: "inference_request", Text: "wire payload should stay hidden"},
		{AgentID: runtime.Seat().ID, Kind: "thinking", Text: "working"},
		{AgentID: runtime.Seat().ID, Kind: "turn_done", Text: "lifecycle event should stay hidden"},
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
	seatID := runtime.Seat().ID
	runtime.Seat().Send("old prompt")
	deadline := time.After(time.Second)
	for {
		select {
		case event := <-runtime.Events():
			if event.Kind == "turn_done" {
				goto initialTurnDone
			}
		case <-deadline:
			t.Fatal("initial turn did not finish")
		}
	}
initialTurnDone:
	m.events = []harness.Event{
		{AgentID: seatID, Kind: "user", Text: "old prompt"},
		{AgentID: seatID, Kind: "assistant", Text: "old answer"},
	}
	m.refreshView()
	if content := ansi.Strip(m.viewport.View()); !strings.Contains(content, "old answer") {
		t.Fatalf("initial message missing before clear: %q", content)
	}

	m.handleCommand("/clear")
	if history := runtime.Seat().History(); len(history) != 0 {
		t.Fatalf("agent history survived clear: %#v", history)
	}
	if content := ansi.Strip(m.viewport.View()); strings.Contains(content, "old prompt") || strings.Contains(content, "old answer") {
		t.Fatalf("cleared messages remain visible: %q", content)
	}

	runtime.Seat().Send("new prompt")
	deadline = time.After(time.Second)
	for {
		select {
		case event := <-runtime.Events():
			if event.Kind == "turn_done" {
				goto newTurnDone
			}
		case <-deadline:
			t.Fatal("post-clear turn did not finish")
		}
	}
newTurnDone:
	history := runtime.Seat().History()
	if len(history) != 1 || history[0].Content != "new prompt" {
		t.Fatalf("post-clear turn reused old history: %#v", history)
	}

	updated, _ := m.Update(eventMsg(harness.Event{AgentID: seatID, Kind: "status", Text: "thinking"}))
	m = updated.(Model)
	updated, _ = m.Update(eventMsg(harness.Event{AgentID: seatID, Kind: "user", Text: "new prompt"}))
	m = updated.(Model)
	updated, _ = m.Update(eventMsg(harness.Event{AgentID: seatID, Kind: "assistant", Text: "new answer"}))
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

	thinking := ansi.Strip(renderEvent(harness.Event{Kind: "thinking", Text: text}, 40))
	if got := lipgloss.Height(thinking); got != nonChatBlockHeight {
		t.Fatalf("thinking block height=%d, want %d", got, nonChatBlockHeight)
	}
	if strings.Contains(thinking, "line 01") || !strings.Contains(thinking, "line 12") {
		t.Fatalf("thinking block did not keep the newest lines: %q", thinking)
	}

	toolResult := ansi.Strip(renderEvent(harness.Event{Kind: "tool_result", Text: text}, 40))
	if got := lipgloss.Height(toolResult); got != nonChatBlockHeight {
		t.Fatalf("tool result block height=%d, want %d", got, nonChatBlockHeight)
	}
	if strings.Contains(toolResult, "line 01") || !strings.Contains(toolResult, "line 12") {
		t.Fatalf("tool result block did not keep the newest lines: %q", toolResult)
	}

	short := renderEvent(harness.Event{Kind: "thinking", Text: "working"}, 40)
	if got := lipgloss.Height(short); got != 1 {
		t.Fatalf("short thinking block height=%d, want 1", got)
	}

	nonChat := renderEvent(harness.Event{Kind: "thinking", Text: "working"}, 40)
	if !strings.Contains(nonChat, "48;5;236") {
		t.Fatalf("non-chat block has no gray background: %q", nonChat)
	}
	header := renderNonChatBlock(
		[]harness.Event{{Kind: "thinking", Text: "working"}},
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
	m.events = []harness.Event{
		{AgentID: runtime.Seat().ID, Kind: "user", Text: "hi"},
		{AgentID: runtime.Seat().ID, Kind: "thinking", Text: text},
		{AgentID: runtime.Seat().ID, Kind: "tool_result", Text: text, Metadata: map[string]any{"name": "quick_bash"}},
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
	events := []harness.Event{
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
	unnamedResults := []harness.Event{
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
	m.events = append(m.events, harness.Event{AgentID: runtime.Seat().ID, AgentTitle: "seat", Kind: "user", Text: "hi"})
	for i := 0; i < 20; i++ {
		m.events = append(m.events, harness.Event{AgentID: runtime.Seat().ID, AgentTitle: "seat", Kind: "assistant", Text: fmt.Sprintf("message %02d %s", i, strings.Repeat("content ", 8))})
	}
	m.refreshView()
	if !m.viewport.AtBottom() {
		t.Fatal("viewport should start at the bottom")
	}

	updated, _ = m.updateKey(tea.KeyPressMsg{Code: tea.KeyPgUp})
	m = updated.(Model)
	if m.viewport.AtBottom() || !m.userScrolled {
		t.Fatalf("PageUp did not move the viewport or mark it as user-scrolled (offset=%d max=%d lines=%d height=%d)", m.viewport.YOffset(), m.viewport.TotalLineCount()-m.viewport.Height(), m.viewport.TotalLineCount(), m.viewport.Height())
	}
	yOffset := m.viewport.YOffset()
	updated, _ = m.Update(eventMsg(harness.Event{AgentID: runtime.Seat().ID, AgentTitle: "seat", Kind: "status", Text: "streaming"}))
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
	if got := m.input.Value(); got != "second" {
		t.Fatalf("first Up recalled %q, want second", got)
	}
	updated, _ = m.updateKey(tea.KeyPressMsg{Code: tea.KeyUp})
	m = updated.(Model)
	if got := m.input.Value(); got != "first\nline" {
		t.Fatalf("second Up recalled %q, want multiline first message", got)
	}
	updated, _ = m.updateKey(tea.KeyPressMsg{Code: tea.KeyDown})
	m = updated.(Model)
	updated, _ = m.updateKey(tea.KeyPressMsg{Code: tea.KeyDown})
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
