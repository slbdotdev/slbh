package tui

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/glamour/v2"
	"charm.land/lipgloss/v2"
	"github.com/slbdotdev/slbh/internal/provider"
	"github.com/slbdotdev/slbh/internal/seam"
)

var (
	accent          = lipgloss.NewStyle().Foreground(lipgloss.Color("205"))
	dim             = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	green           = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	headerStyle     = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("220"))
	red             = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	assistantBubble = lipgloss.Color("24")
	userBubble      = lipgloss.Color("22")
	subagentBubble  = lipgloss.Color("132")
	nonChatBubble   = lipgloss.Color("236")
)

const nonChatBlockHeight = 10

// maxRetainedViewportEvents is a soft cap on display state; the runtime
// transcript remains complete. Keep every message from every agent, evicting
// control records first and then the oldest non-message output when full.
// Messages alone may exceed the cap so earlier chat stays scrollable.
const maxRetainedViewportEvents = 512

// These cache limits are floors. Each grows to twice the retained event count
// so a full refresh can reuse old blocks while streaming adds new versions.
const markdownRenderCacheLimit = 512

const renderedEventCacheLimit = 1024

// markdownThrottle bounds how often a streamed message is re-rendered.
const markdownThrottle = 100 * time.Millisecond

const scrollStep = 5

const headerBullet = "• "

const thinkingStartMetadataKey = "_tui_thinking_start"

var slashCommands = []string{
	"/exit",
	"/quit",
	"/q",
	"/clear",
	"/model",
	"/models",
	"/effort",
	"/agents",
	"/jobs",
	"/compact",
	"/mouse",
}

type eventMsg seam.Event

type eventBatchMsg seam.EventBatch

// markdownTickMsg catches up a block the render throttle held back.
type markdownTickMsg time.Time

// compactDoneMsg reports a /compact that ran off the update loop: a native
// agent's compaction waits on its model to write the summary.
type compactDoneMsg struct {
	replaced int
	err      error
}

type modelCatalogMsg struct {
	catalog []provider.Catalog
	err     error
}

type markdownCacheKey struct {
	source string
	width  int
}

// markdownRecentRender is the last render of one block, kept so a throttled
// refresh has something to show. The source is kept with it: a block is only
// allowed to reuse a render whose source its own source extends, which is what
// a growing stream does and what a different block never does.
type markdownRecentRender struct {
	source   string
	rendered string
}

type renderedEventCacheKey struct {
	cursor     seam.EventCursor
	agentID    string
	agentTitle string
	kind       string
	text       string
	tool       string
	width      int
	forwarded  bool
}

type Model struct {
	runtime            seam.Runtime
	viewport           viewport.Model
	input              textarea.Model
	events             []seam.Event
	eventCursor        seam.EventCursor
	agents             []seam.AgentSnapshot
	viewAgentID        string
	selected           int
	focusAgents        bool
	userScrolled       bool
	width              int
	height             int
	quitting           bool
	commandLine        string
	history            []string
	historyPath        string
	historyIndex       int
	historyDraft       string
	modelsOpen         bool
	modelsLoading      bool
	modelCatalog       []provider.Catalog
	modelExpanded      map[string]bool
	modelCursor        int
	modelNotice        string
	modelSubmenu       bool
	modelSubnode       modelTreeNode
	modelSubcursor     int
	modelSeat          string
	modelSubagent      string
	modelLeaf          string
	modelNoticeErr     bool
	modelNoticeOK      bool
	markdownWidth      int
	markdownRender     *glamour.TermRenderer
	markdownCache      map[markdownCacheKey]string
	markdownRecent     map[string]markdownRecentRender
	markdownRenderedAt time.Time
	markdownStale      bool
	markdownTicking    bool
	eventRenderCache   map[renderedEventCacheKey]string
	// onlyMessages records that the last retention scan found nothing but
	// messages, which are never evicted; until something evictable arrives,
	// appending more messages need not scan the whole history again.
	onlyMessages  bool
	frameProfiler *frameProfiler
	// mouseCapture is off by default so the terminal keeps its own click and
	// drag, which is what selecting and copying text needs. Turning it on
	// trades that away for wheel scrolling.
	mouseCapture bool
}

func New(runtime seam.Runtime) Model {
	input := textarea.New()
	input.Placeholder = "Message the seat agent… (Enter sends; Ctrl-J adds a line)"
	input.Prompt = ""
	input.ShowLineNumbers = false
	input.CharLimit = 0
	input.DynamicHeight = true
	input.MinHeight = 1
	input.Focus()
	view := viewport.New(viewport.WithWidth(80), viewport.WithHeight(20))
	viewID := seatID(runtime)
	historyPath := ""
	var history []string
	if runtime != nil {
		historyPath = filepath.Join(runtime.Home(), historyFileName)
		history, _ = loadHistory(historyPath)
	}
	return Model{runtime: runtime, viewport: view, input: input, viewAgentID: viewID, agents: activeAgents(runtime.Agents()), history: history, historyPath: historyPath, historyIndex: -1, modelCatalog: runtime.ModelCatalog(), modelExpanded: make(map[string]bool), markdownCache: make(map[markdownCacheKey]string), markdownRecent: make(map[string]markdownRecentRender), eventRenderCache: make(map[renderedEventCacheKey]string), frameProfiler: newFrameProfiler()}
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(textarea.Blink, waitEvents(m.runtime, m.eventCursor))
}

func waitEvents(runtime seam.Runtime, cursor seam.EventCursor) tea.Cmd {
	return func() tea.Msg {
		return eventBatchMsg(runtime.PollEvents(seam.EventQuery{After: cursor, Limit: 128, WaitMilliseconds: 250}))
	}
}

// Update schedules the markdown catch-up tick centrally. Any path that
// refreshes the view can leave a block holding a throttled, stale render —
// switching the viewed agent is one — and a path that returned no command of
// its own would otherwise strand that block until the next keystroke.
func (m Model) Update(msg tea.Msg) (result tea.Model, command tea.Cmd) {
	started := time.Now()
	defer func() {
		eventCount := len(m.events)
		if next, ok := result.(Model); ok {
			eventCount = len(next.events)
		} else if next, ok := result.(*Model); ok && next != nil {
			eventCount = len(next.events)
		}
		m.frameProfiler.record("update", fmt.Sprintf("%T", msg), started, time.Now(), eventCount)
	}()
	updated, cmd := m.update(msg)
	// The model-menu paths have a pointer receiver and return *Model, so both
	// forms have to be handled or those paths skip scheduling entirely.
	var next *Model
	switch typed := updated.(type) {
	case Model:
		next = &typed
	case *Model:
		next = typed
	default:
		return updated, cmd
	}
	if tick := next.markdownTickCmd(); tick != nil {
		return *next, tea.Batch(cmd, tick)
	}
	return *next, cmd
}

func (m Model) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case compactDoneMsg:
		if msg.err != nil {
			m.addLocal("error", msg.err.Error())
		} else {
			m.addLocal("status", fmt.Sprintf("compacted %d earlier messages; the full transcript remains available", msg.replaced))
		}
		return m, nil
	case modelCatalogMsg:
		m.modelsLoading = false
		m.modelCatalog = msg.catalog
		if _, err := m.runtime.Do(seam.SetModelCatalogCommand{Catalog: msg.catalog}); err != nil && msg.err == nil {
			msg.err = err
		}
		if msg.err != nil {
			m.modelNotice = msg.err.Error()
			m.modelNoticeErr = true
			m.modelNoticeOK = false
		} else {
			m.modelNotice = ""
			m.modelNoticeErr = false
			m.modelNoticeOK = false
		}
		return m, nil
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.resize()
		return m, nil
	case markdownTickMsg:
		m.markdownTicking = false
		m.refreshView()
		return m, nil
	case eventBatchMsg:
		batch := seam.EventBatch(msg)
		m.eventCursor = batch.Cursor
		m.receiveEvents(batch.Events)
		if batch.End {
			return m, nil
		}
		return m, waitEvents(m.runtime, m.eventCursor)
	case eventMsg:
		m.receiveEvents([]seam.Event{seam.Event(msg)})
		return m, waitEvents(m.runtime, m.eventCursor)
	case tea.MouseMsg:
		return m.updateMouse(msg)
	case tea.KeyPressMsg:
		if m.modelsOpen {
			return m.updateModelMenu(msg)
		}
		return m.updateKey(msg)
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

func (m *Model) receiveEvents(events []seam.Event) {
	footerBefore := -1
	if m.width > 0 && m.height > 0 {
		footerBefore = m.footerHeight()
	}
	viewChanged := false
	for _, event := range events {
		if !isViewportEvent(event) {
			continue
		}
		m.appendViewportEvent(event)
		if event.AgentID == m.viewAgentID {
			viewChanged = true
		}
	}
	selectedID := ""
	if m.selected >= 0 && m.selected < len(m.agents) {
		selectedID = m.agents[m.selected].ID
	}
	m.agents = activeAgents(m.runtime.Agents())
	if !containsAgent(m.agents, m.viewAgentID) {
		m.viewAgentID = seatID(m.runtime)
		m.userScrolled = false
		viewChanged = true
	}
	// Keep the selection on the same agent when rows above it come or go;
	// only a stopped selection falls back to the nearest remaining row.
	for i, agent := range m.agents {
		if agent.ID == selectedID {
			m.selected = i
			break
		}
	}
	if m.selected >= len(m.agents) {
		m.selected = max(0, len(m.agents)-1)
	}
	// Agent and job events can change the footer height. Reflow the chat
	// viewport before rendering so newly spawned agents cannot push the last
	// panel row below the terminal.
	footerAfter := -1
	if m.width > 0 && m.height > 0 {
		footerAfter = m.footerHeight()
	}
	if footerBefore != footerAfter {
		m.resize()
	} else if viewChanged {
		m.refreshView()
	}
	if m.width < 1 || m.height < 1 {
		if viewChanged {
			m.refreshView()
		}
	}
}

// appendViewportEvent folds adjacent stream fragments before applying the
// display bound, avoiding one retained event per chunk.
func (m *Model) appendViewportEvent(event seam.Event) {
	if len(m.events) > 0 {
		previous := &m.events[len(m.events)-1]
		if canMergeViewportEvents(*previous, event) {
			previous.Text += event.Text
			if event.Kind == "thinking" {
				start := previous.Time
				if previous.Metadata != nil {
					if saved, ok := previous.Metadata[thinkingStartMetadataKey].(time.Time); ok {
						start = saved
					}
				}
				metadata := make(map[string]any, len(previous.Metadata)+1)
				for key, value := range previous.Metadata {
					metadata[key] = value
				}
				metadata[thinkingStartMetadataKey] = start
				previous.Metadata = metadata
				if !event.Time.IsZero() {
					previous.Time = event.Time
				}
			}
			return
		}
	}
	m.events = append(m.events, event)
	if !isViewportEvent(event) || !isMessage(event) {
		m.onlyMessages = false
	}
	for len(m.events) > maxRetainedViewportEvents && !m.onlyMessages {
		drop := -1
		for i, retained := range m.events {
			if !isViewportEvent(retained) {
				drop = i
				break
			}
			if drop == -1 && !isMessage(retained) {
				drop = i
			}
		}
		if drop == -1 {
			m.onlyMessages = true
			break
		}
		copy(m.events[drop:], m.events[drop+1:])
		m.events[len(m.events)-1] = seam.Event{}
		m.events = m.events[:len(m.events)-1]
	}
}

func canMergeViewportEvents(previous, event seam.Event) bool {
	if previous.AgentID != event.AgentID || previous.Kind != event.Kind {
		return false
	}
	switch event.Kind {
	case "assistant", "thinking":
		return true
	case "tool":
		return toolIndex(previous) == toolIndex(event)
	default:
		return false
	}
}

func (m Model) updateKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if isControlKey(msg, 'c') || isControlKey(msg, 'q') {
		m.quitting = true
		_, _ = m.runtime.Do(seam.CloseCommand{})
		return m, tea.Quit
	}
	if m.focusAgents {
		switch msg.Code {
		case tea.KeyEsc, tea.KeyUp:
			if msg.Code == tea.KeyEsc || m.selected == 0 {
				m.focusAgents = false
				m.input.Focus()
				return m, nil
			}
			m.selected--
		case tea.KeyDown:
			if m.selected < len(m.agents)-1 {
				m.selected++
			}
		case tea.KeyEnter:
			if len(m.agents) > 0 {
				m.viewAgentID = m.agents[m.selected].ID
				m.refreshView()
			}
			m.focusAgents = false
			m.input.Focus()
		}
		return m, nil
	}
	if msg.Mod.Contains(tea.ModAlt) && msg.Code == tea.KeyUp {
		m.recallHistory(-1)
		return m, nil
	}
	if msg.Mod.Contains(tea.ModAlt) && msg.Code == tea.KeyDown {
		m.recallHistory(1)
		return m, nil
	}
	if msg.Mod.Contains(tea.ModCtrl) && msg.Code == tea.KeyDown {
		m.focusAgentList()
		return m, nil
	}
	// Plain arrows belong to the input's cursor and only act at its edges. With
	// mouse capture off the terminal sends the wheel as arrows, so the edges
	// scroll: Up on the top row scrolls up, and Down on the bottom row scrolls
	// back down before it leaves the input for the first root subagent.
	if msg.Mod == 0 && msg.Code == tea.KeyUp && m.inputCursorOnTopRow() {
		m.scrollUp()
		return m, nil
	}
	if msg.Mod == 0 && msg.Code == tea.KeyDown && m.inputCursorOnBottomRow() {
		if !m.viewport.AtBottom() {
			m.scrollDown()
			return m, nil
		}
		m.focusAgentList()
		return m, nil
	}
	if msg.Code == tea.KeyEnter {
		cmd := m.submit()
		if m.quitting {
			return m, tea.Quit
		}
		return m, cmd
	}
	switch {
	case msg.Code == tea.KeyPgUp || isControlKey(msg, 'u'):
		m.scrollUp()
		return m, nil
	case msg.Code == tea.KeyPgDown || isControlKey(msg, 'd'):
		m.scrollDown()
		return m, nil
	case msg.Code == tea.KeyEsc:
		if m.viewAgentID != seatID(m.runtime) {
			m.viewAgentID = seatID(m.runtime)
			m.refreshView()
		}
		return m, nil
	case msg.Code == tea.KeyTab:
		if m.completeSlashCommand() {
			return m, nil
		}
	case isControlKey(msg, 'j'):
		// Enter submits the message, so insert the textarea newline explicitly.
		m.input.InsertString("\n")
		m.resetHistoryNavigation()
		m.syncInputHeight()
		return m, nil
	}
	return m.updateInput(msg)
}

func (m Model) updateMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	mouse := msg.Mouse()
	switch {
	case mouse.Button == tea.MouseWheelUp:
		m.scrollUpBy(m.viewport.MouseWheelDelta)
	case mouse.Button == tea.MouseWheelDown:
		m.scrollDownBy(m.viewport.MouseWheelDelta)
	}
	return m, nil
}

// inputCursorOnTopRow and inputCursorOnBottomRow compare visual rows, so a
// long line that wraps is walked row by row before the cursor leaves it.
func (m *Model) inputCursorOnTopRow() bool {
	return m.input.Line() == 0 && m.input.LineInfo().RowOffset == 0
}

func (m *Model) inputCursorOnBottomRow() bool {
	info := m.input.LineInfo()
	return m.input.Line() == m.input.LineCount()-1 && info.RowOffset >= info.Height-1
}

// focusAgentList moves focus to the agent list with the first root subagent
// selected, or the seat when there is no subagent.
func (m *Model) focusAgentList() {
	m.selected = 0
	for i, agent := range m.agents {
		if agent.Depth == 1 {
			m.selected = i
			break
		}
	}
	m.focusAgents = true
	m.input.Blur()
}

func (m *Model) scrollUp() {
	m.scrollUpBy(scrollStep)
}

func (m *Model) scrollUpBy(lines int) {
	if lines < 1 {
		lines = 1
	}
	m.viewport.ScrollUp(lines)
	m.userScrolled = !m.viewport.AtBottom()
}

func (m *Model) scrollDown() {
	m.scrollDownBy(scrollStep)
}

func (m *Model) scrollDownBy(lines int) {
	if lines < 1 {
		lines = 1
	}
	m.viewport.ScrollDown(lines)
	m.userScrolled = !m.viewport.AtBottom()
}

func (m *Model) submit() tea.Cmd {
	text := strings.TrimSpace(m.input.Value())
	if text == "" {
		return nil
	}
	m.input.Reset()
	m.resetHistoryNavigation()
	m.recordHistory(text)
	if strings.HasPrefix(text, "/") {
		return m.handleCommand(text)
	}
	a, ok := agentSnapshot(m.runtime, m.viewAgentID)
	if !ok {
		a, ok = seatSnapshot(m.runtime)
	}
	if !ok {
		return nil
	}
	var err error
	if a.ID == seatID(m.runtime) {
		_, err = m.runtime.Do(seam.SendPromptCommand{AgentID: a.ID, Prompt: text})
	} else {
		_, err = m.runtime.Do(seam.SteerAgentCommand{AgentID: a.ID, Message: text})
	}
	if err != nil {
		_, _ = m.runtime.Do(seam.EmitStatusCommand{Kind: "error", Text: err.Error()})
	}
	return nil
}

func (m *Model) updateInput(msg tea.Msg) (Model, tea.Cmd) {
	before := m.input.Value()
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	if m.input.Value() != before {
		m.resetHistoryNavigation()
	}
	m.syncInputHeight()
	return *m, cmd
}

func (m *Model) resetHistoryNavigation() {
	m.historyIndex = -1
	m.historyDraft = ""
}

func (m *Model) recallHistory(direction int) bool {
	if len(m.history) == 0 {
		return false
	}
	if direction < 0 {
		if m.historyIndex == -1 {
			m.historyDraft = m.input.Value()
			m.historyIndex = len(m.history) - 1
		} else if m.historyIndex > 0 {
			m.historyIndex--
		}
	} else {
		if m.historyIndex == -1 {
			return false
		}
		if m.historyIndex < len(m.history)-1 {
			m.historyIndex++
		} else {
			m.input.SetValue(m.historyDraft)
			m.resetHistoryNavigation()
			m.syncInputHeight()
			return true
		}
	}
	m.input.SetValue(m.history[m.historyIndex])
	m.syncInputHeight()
	return true
}

func (m *Model) recordHistory(text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	m.history = append(m.history, text)
	if m.historyPath == "" {
		return
	}
	if err := appendHistory(m.historyPath, text); err != nil {
		m.addLocal("error", "history: "+err.Error())
	}
}

func (m *Model) completeSlashCommand() bool {
	value := m.input.Value()
	if strings.Contains(value, "\n") || !strings.HasPrefix(value, "/") {
		return false
	}
	if strings.IndexAny(value, " \t") >= 0 {
		return false
	}
	partial := strings.ToLower(value)
	matches := make([]string, 0, len(slashCommands))
	for _, command := range slashCommands {
		if strings.HasPrefix(command, partial) {
			matches = append(matches, command)
		}
	}
	if len(matches) == 0 {
		return false
	}
	match := ""
	for _, candidate := range matches {
		prefixOfOther := false
		for _, other := range matches {
			if other != candidate && strings.HasPrefix(other, candidate) {
				prefixOfOther = true
				break
			}
		}
		if prefixOfOther {
			if match != "" {
				return false
			}
			match = candidate
		}
	}
	if match == "" || match == partial {
		return false
	}
	m.input.SetValue(match)
	m.resetHistoryNavigation()
	m.syncInputHeight()
	return true
}

func (m *Model) handleCommand(command string) tea.Cmd {
	parts := strings.Fields(command)
	name := strings.ToLower(parts[0])
	arg := ""
	if len(parts) > 1 {
		arg = strings.Join(parts[1:], " ")
	}
	switch name {
	case "/exit", "/quit", "/q":
		m.quitting = true
		_, _ = m.runtime.Do(seam.CloseCommand{})
		return nil
	case "/clear":
		if _, err := m.runtime.Do(seam.ClearCommand{AgentID: m.viewAgentID}); err != nil {
			m.addLocal("error", err.Error())
			return nil
		}
		m.clearCurrentView()
	case "/models":
		return m.openModelMenu()
	case "/model":
		if arg != "" {
			cfg := m.runtime.Config()
			approved := append([]string(nil), cfg.ApprovedModels...)
			if approved == nil {
				approved = []string{}
			}
			if !containsModel(approved, arg) {
				approved = append(approved, arg)
			}
			if _, err := m.runtime.Do(seam.ConfigureModelsCommand{SeatModel: arg, SubagentModel: cfg.SubagentModel, Approved: approved}); err != nil {
				m.addLocal("error", "save model configuration: "+err.Error())
			} else {
				m.addLocal("status", "seat model set to "+arg)
			}
		}
	case "/effort":
		if arg != "" {
			if id := seatID(m.runtime); id != "" {
				if _, err := m.runtime.Do(seam.SetAgentEffortCommand{AgentID: id, Effort: arg, Persist: true}); err != nil {
					m.addLocal("error", err.Error())
					return nil
				}
			}
			m.addLocal("status", "seat effort set to "+arg)
		}
	case "/agents":
		m.addLocal("status", formatAgents(m.agents))
	case "/jobs":
		m.addLocal("status", fmt.Sprintf("%v", m.runtime.JobSnapshots()))
	case "/mouse":
		m.mouseCapture = !m.mouseCapture
		if m.mouseCapture {
			m.addLocal("status", "mouse capture on: wheel scrolls the viewport; terminal text selection needs Shift-drag")
		} else {
			m.addLocal("status", "mouse capture off: drag to select text; PgUp/PgDn and Ctrl-U/Ctrl-D scroll")
		}
	case "/compact":
		agentID := m.viewAgentID
		// The runtime reports the compaction itself: a compacting line and
		// status while the summary is written, then the summary.
		return func() tea.Msg {
			reply, err := m.runtime.Do(seam.CompactCommand{AgentID: agentID})
			return compactDoneMsg{replaced: reply.Dropped, err: err}
		}
	default:
		m.addLocal("error", "unknown command: "+name)
	}
	return nil
}

func (m *Model) addLocal(kind, text string) {
	m.appendViewportEvent(seam.Event{Time: time.Now(), AgentID: m.viewAgentID, AgentTitle: "local", Kind: kind, Text: text})
	m.refreshView()
}
