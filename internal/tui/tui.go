package tui

import (
	"context"
	"fmt"
	"image/color"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/glamour/v2"
	"charm.land/glamour/v2/ansi"
	"charm.land/glamour/v2/styles"
	"charm.land/lipgloss/v2"
	xansi "github.com/charmbracelet/x/ansi"
	"github.com/slbdotdev/slbh/internal/config"
	"github.com/slbdotdev/slbh/internal/provider"
	"github.com/slbdotdev/slbh/internal/seam"
	"github.com/slbdotdev/slbh/internal/toolmarkup"
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

type modelTreeNode struct {
	provider int
	model    int
	branch   bool
	save     bool
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
	frameProfiler      *frameProfiler
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
	m.agents = activeAgents(m.runtime.Agents())
	if !containsAgent(m.agents, m.viewAgentID) {
		m.viewAgentID = seatID(m.runtime)
		m.userScrolled = false
		viewChanged = true
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
	for len(m.events) > maxRetainedViewportEvents {
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

func (m *Model) openModelMenu() tea.Cmd {
	if m.modelExpanded == nil {
		m.modelExpanded = make(map[string]bool)
	}
	cfg := m.runtime.Config()
	m.modelSeat, m.modelSubagent, m.modelLeaf = "", "", ""
	if cfg.ModelApproved(cfg.SeatModel) {
		m.modelSeat = cfg.SeatModel
	}
	if cfg.ModelApproved(cfg.SubagentModel) {
		m.modelSubagent = cfg.SubagentModel
	}
	leaf := cfg.LeafModel
	if leaf == "" {
		leaf = cfg.SubagentModel
	}
	if cfg.ModelApproved(leaf) {
		m.modelLeaf = leaf
	}
	m.modelsOpen = true
	m.modelsLoading = true
	m.modelNotice = "loading provider catalogs…"
	m.modelNoticeErr = false
	m.modelNoticeOK = false
	m.input.Blur()
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		catalog, err := provider.DiscoverCatalog(ctx, m.runtime.Config().Policy)
		return modelCatalogMsg{catalog: catalog, err: err}
	}
}

func (m *Model) updateModelMenu(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if msg.Code == tea.KeyEsc {
		if m.modelSubmenu {
			m.modelSubmenu = false
			return m, nil
		}
		m.saveAndCloseModelMenu()
		return m, nil
	}
	if m.modelSubmenu {
		return m.updateModelSubmenu(msg)
	}
	if m.modelsLoading {
		return m, nil
	}
	nodes := m.modelNodes()
	if len(nodes) > 0 && m.modelCursor >= len(nodes) {
		m.modelCursor = len(nodes) - 1
	}
	switch msg.Code {
	case tea.KeyUp:
		if m.modelCursor > 0 {
			m.modelCursor--
		}
	case tea.KeyDown:
		if m.modelCursor < len(nodes)-1 {
			m.modelCursor++
		}
	case tea.KeyLeft:
		if len(nodes) > 0 && !nodes[m.modelCursor].save {
			m.modelExpanded[m.modelCatalog[nodes[m.modelCursor].provider].Name] = false
		}
	case tea.KeyRight:
		if len(nodes) > 0 && nodes[m.modelCursor].branch {
			m.modelExpanded[m.modelCatalog[nodes[m.modelCursor].provider].Name] = true
		}
	case tea.KeyEnter:
		if len(nodes) > 0 {
			node := nodes[m.modelCursor]
			switch {
			case node.save:
				m.saveAndCloseModelMenu()
			case node.branch:
				name := m.modelCatalog[node.provider].Name
				m.modelExpanded[name] = !m.modelExpanded[name]
			default:
				m.modelSubmenu = true
				m.modelSubnode = node
				m.modelSubcursor = 0
			}
		}
	case 'r', 'R', 's', 'S', 'l', 'L':
		if len(nodes) > 0 && !nodes[m.modelCursor].branch && !nodes[m.modelCursor].save {
			slot := "seat"
			switch strings.ToLower(string(msg.Code)) {
			case "s":
				slot = "subagent"
			case "l":
				slot = "leaf"
			}
			m.assignModel(nodes[m.modelCursor], slot)
		}
	case 'p', 'P':
		m.authorLocalPolicy()
	}
	return m, nil
}

// authorLocalPolicy writes a working routing policy into the app-owned
// config.json for the models this configuration actually uses.
//
// This is the escape hatch that keeps the fail-closed refusal from being a
// brick on a host ansible does not manage. Every route it writes is on a wire
// this build can speak, so the policy it authors works rather than merely
// parsing. On a managed host the write still happens and is still inert,
// because the managed file wins — and the notice says so outright, since a
// user who edits a local policy and sees nothing change has no other way to
// tell why.
func (m *Model) authorLocalPolicy() {
	cfg := m.runtime.Config()
	models := []string{m.modelSeat, m.modelSubagent, m.modelLeaf, cfg.SeatModel, cfg.SubagentModel, cfg.LeafModel}
	models = append(models, cfg.ApprovedModels...)
	policy := provider.DefaultLocalPolicy(models)
	if len(policy.Routes) == 0 {
		m.modelNotice = "no models selected yet, so there is no route to author a policy for"
		m.modelNoticeErr = true
		m.modelNoticeOK = false
		return
	}
	reply, err := m.runtime.Do(seam.AuthorLocalPolicyCommand{Policy: policy})
	if err != nil {
		m.modelNotice = "author local policy: " + err.Error()
		m.modelNoticeErr = true
		m.modelNoticeOK = false
		return
	}
	routes := len(policy.Routes)
	if reply.PolicySource.Kind == config.PolicyManaged {
		m.modelNotice = fmt.Sprintf(
			"local policy written for %d route(s), but it is not in force: the managed policy at %s wins and slbh never writes that file",
			routes, reply.PolicySource.Path)
		m.modelNoticeErr = false
		m.modelNoticeOK = true
		return
	}
	m.modelNotice = fmt.Sprintf("local policy authored for %d route(s); policy source is now %s", routes, reply.PolicySource.Describe())
	m.modelNoticeErr = false
	m.modelNoticeOK = true
}

func (m *Model) updateModelSubmenu(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	options := []string{"seat", "subagent", "leaf"}
	switch msg.Code {
	case tea.KeyUp:
		if m.modelSubcursor > 0 {
			m.modelSubcursor--
		}
	case tea.KeyDown:
		if m.modelSubcursor < len(options)-1 {
			m.modelSubcursor++
		}
	case tea.KeyEnter:
		m.assignModel(m.modelSubnode, options[m.modelSubcursor])
		m.modelSubmenu = false
	case 'r', 'R':
		m.assignModel(m.modelSubnode, "seat")
		m.modelSubmenu = false
	case 's', 'S':
		m.assignModel(m.modelSubnode, "subagent")
		m.modelSubmenu = false
	case 'l', 'L':
		m.assignModel(m.modelSubnode, "leaf")
		m.modelSubmenu = false
	}
	return m, nil
}

func (m Model) modelNodes() []modelTreeNode {
	nodes := make([]modelTreeNode, 0)
	for i, catalog := range m.modelCatalog {
		nodes = append(nodes, modelTreeNode{provider: i, model: -1, branch: true})
		if !m.modelExpanded[catalog.Name] {
			continue
		}
		for j := range catalog.Models {
			nodes = append(nodes, modelTreeNode{provider: i, model: j})
		}
	}
	nodes = append(nodes, modelTreeNode{save: true})
	return nodes
}

func (m *Model) assignModel(node modelTreeNode, slot string) {
	model := m.modelCatalog[node.provider].Models[node.model].ID
	switch slot {
	case "seat":
		m.modelSeat = model
	case "subagent":
		m.modelSubagent = model
	case "leaf":
		m.modelLeaf = model
	default:
		return
	}
	if _, err := m.runtime.Do(seam.ConfigureModelSlotsCommand{SeatModel: m.modelSeat, SubagentModel: m.modelSubagent, LeafModel: m.modelLeaf, Approved: approvedSlots(m.modelSeat, m.modelSubagent, m.modelLeaf)}); err != nil {
		m.modelNotice = "save failed: " + err.Error()
		m.modelNoticeErr = true
		m.modelNoticeOK = false
		return
	}
	m.modelNotice = slot + " model set to " + displayModelID(m.modelCatalog[node.provider].Name, model)
	m.modelNoticeErr = false
	m.modelNoticeOK = true
}

func (m *Model) saveAndCloseModelMenu() {
	if _, err := m.runtime.Do(seam.ConfigureModelSlotsCommand{SeatModel: m.modelSeat, SubagentModel: m.modelSubagent, LeafModel: m.modelLeaf, Approved: approvedSlots(m.modelSeat, m.modelSubagent, m.modelLeaf)}); err != nil {
		m.modelNotice = "save failed: " + err.Error()
		m.modelNoticeErr = true
		m.modelNoticeOK = false
		return
	}
	m.modelsOpen = false
	m.modelsLoading = false
	m.modelSubmenu = false
	m.input.Focus()
	m.addLocal("status", "model configuration saved")
}

func approvedSlots(seat, subagent, leaf string) []string {
	result := make([]string, 0, 3)
	for _, model := range []string{seat, subagent, leaf} {
		if model != "" && !containsModel(result, model) {
			result = append(result, model)
		}
	}
	return result
}

func (m Model) modelSlotMarker(model string) string {
	markers := make([]string, 0, 3)
	if strings.EqualFold(m.modelSeat, model) && model != "" {
		markers = append(markers, "r")
	}
	if strings.EqualFold(m.modelSubagent, model) && model != "" {
		markers = append(markers, "s")
	}
	if strings.EqualFold(m.modelLeaf, model) && model != "" {
		markers = append(markers, "l")
	}
	return strings.Join(markers, "/")
}

func modelSlotValue(model string) string {
	if model == "" {
		return "none"
	}
	return model
}

func (m Model) modelSubmenuView() string {
	width := max(1, m.width)
	model := m.modelCatalog[m.modelSubnode.provider].Models[m.modelSubnode.model]
	options := []string{"seat", "subagent", "leaf"}
	lines := []string{
		accent.Render("ASSIGN MODEL"),
		"Choose a slot for " + displayModelID(m.modelCatalog[m.modelSubnode.provider].Name, model.ID) + ":",
		"",
	}
	for i, option := range options {
		prefix := "  "
		if i == m.modelSubcursor {
			prefix = "> "
		}
		lines = append(lines, prefix+option)
	}
	lines = append(lines, "", dim.Render("Enter assign · r/s/l assign directly · Esc back"))
	return wrapToWidth(strings.Join(lines, "\n"), width)
}

func displayModelID(providerName, model string) string {
	prefix := strings.ToLower(strings.TrimSpace(providerName)) + "/"
	if strings.HasPrefix(strings.ToLower(model), prefix) {
		return model[len(prefix):]
	}
	return model
}

func containsModel(models []string, wanted string) bool {
	for _, model := range models {
		if strings.EqualFold(strings.TrimSpace(model), strings.TrimSpace(wanted)) {
			return true
		}
	}
	return false
}

func (m Model) modelMenuView() string {
	if m.modelSubmenu {
		return m.modelSubmenuView()
	}
	width := max(1, m.width)
	lines := []string{
		accent.Render("MODELS"),
		"Providers with configured keys; OpenRouter shows models created within the last year.",
		fmt.Sprintf("Slots: seat=%s · subagent=%s · leaf=%s", modelSlotValue(m.modelSeat), modelSlotValue(m.modelSubagent), modelSlotValue(m.modelLeaf)),
		// Which policy is in force, and from where. A user on a managed host
		// who edits a local policy sees no change, and this line is the only
		// thing that tells them the managed file is winning.
		wrapToWidth("Routing policy: "+m.runtime.PolicySource().Describe(), width),
		// The per-layer role documents, from the same managed directory. A
		// converge that did not land leaves agents running on baked mechanics
		// alone, which is a legitimate state and therefore a silent one unless
		// it is reported here.
		wrapToWidth("Layer instructions: "+m.runtime.InstructionSource().Describe(), width),
		// Skill loading degrades independently of the layer documents. Report
		// absent directories and rejected skill metadata rather than silently
		// presenting a smaller role inventory.
		wrapToWidth("Layer skills: "+m.runtime.SkillSource().Describe(), width),
		"r=seat · s=subagent · l=leaf · p=author local policy · Enter=assign · Esc=save and close",
		"",
	}
	if m.modelsLoading {
		lines = append(lines, "Loading provider catalogs…")
	} else {
		if len(m.modelCatalog) == 0 {
			lines = append(lines, "No provider keys found or no models returned.")
		}
		nodes := m.modelNodes()
		available := max(1, m.height-len(lines)-2)
		start := max(0, m.modelCursor-available/2)
		if start+available > len(nodes) {
			start = max(0, len(nodes)-available)
		}
		end := min(len(nodes), start+available)
		for i := start; i < end; i++ {
			node := nodes[i]
			prefix := "  "
			if i == m.modelCursor {
				prefix = "> "
			}
			if node.save {
				lines = append(lines, accent.Render(wrapToWidth(prefix+"Save and Close", width)))
				continue
			}
			catalog := m.modelCatalog[node.provider]
			if node.branch {
				marker := "▸"
				if m.modelExpanded[catalog.Name] {
					marker = "▾"
				}
				line := fmt.Sprintf("%s%s %s (%d models)", prefix, marker, catalog.Name, len(catalog.Models))
				if catalog.Err != "" {
					line += " [" + catalog.Err + "]"
				}
				lines = append(lines, dim.Render(wrapToWidth(line, width)))
				continue
			}
			model := catalog.Models[node.model]
			lines = append(lines, wrapToWidth(fmt.Sprintf("%s  [%s] %s", prefix, m.modelSlotMarker(model.ID), displayModelID(catalog.Name, model.ID)), width))
		}
	}
	if m.modelNotice != "" {
		notice := dim
		if m.modelNoticeErr {
			notice = red
		} else if m.modelNoticeOK {
			notice = green
		}
		lines = append(lines, "", notice.Render(wrapToWidth(m.modelNotice, width)))
	}
	return strings.Join(lines, "\n")
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func (m *Model) resize() {
	if m.width < 1 || m.height < 1 {
		return
	}
	contentWidth := max(1, m.chatWidth())
	m.input.MaxHeight = m.inputMaxHeight()
	m.input.SetWidth(contentWidth)
	m.viewport.SetWidth(contentWidth)
	m.viewport.SetHeight(m.chatHeight())
	m.refreshView()
}

func (m *Model) syncInputHeight() {
	if m.width > 0 {
		width := max(1, m.chatWidth())
		m.input.MaxHeight = m.inputMaxHeight()
		m.input.SetWidth(width)
		m.viewport.SetWidth(width)
		if m.height > 0 {
			m.viewport.SetHeight(m.chatHeight())
			if !m.userScrolled {
				m.viewport.GotoBottom()
			}
		}
	}
}

func (m Model) inputDisplayHeight() int {
	input := m.input
	input.MaxHeight = m.inputMaxHeight()
	input.SetWidth(max(1, m.chatWidth()))
	return max(1, input.Height())
}

func (m Model) inputMaxHeight() int {
	if m.height <= 0 {
		return 0
	}
	agentHeight := 0
	if agents := m.agentPanel(); agents != "" {
		agentHeight = lipgloss.Height(agents)
	}
	statusHeight := lipgloss.Height(m.statusLine())
	// Keep one row available for chat. The input frame adds two horizontal
	// rules. The newlines joining blocks move to the next row; they do not add
	// an extra blank row to the rendered layout.
	reserved := 1 + 2 + agentHeight + statusHeight
	return max(1, m.height-reserved)
}

func (m Model) chatWidth() int {
	return m.width
}

func (m *Model) refreshView() {
	if m.width > 0 {
		m.viewport.SetWidth(m.chatWidth())
	}
	width := max(1, m.chatWidth())
	m.markdownRendererForWidth(width)
	hasUser := false
	for _, event := range m.events {
		if event.AgentID == m.viewAgentID && event.Kind == "user" {
			hasUser = true
			break
		}
	}
	var visible []seam.Event
	userSeen := !hasUser
	for _, event := range m.events {
		if event.AgentID != m.viewAgentID {
			continue
		}
		if !isViewportEvent(event) {
			continue
		}
		if !userSeen {
			if event.Kind != "user" {
				continue
			}
			userSeen = true
		}
		if len(visible) > 0 && canMergeViewportEvents(visible[len(visible)-1], event) {
			visible[len(visible)-1].Text += event.Text
			if event.Kind == "thinking" {
				// Keep the start of a streamed reasoning span while moving the
				// merged event's timestamp forward to its latest chunk. The
				// metadata is local to this rendered copy of the event.
				previous := &visible[len(visible)-1]
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
			continue
		}
		visible = append(visible, event)
	}
	lines := make([]string, 0, len(visible))
	for i := 0; i < len(visible); {
		if !isMessage(visible[i]) {
			end := i
			parts := make([]string, 0, 1)
			for end < len(visible) && !isMessage(visible[end]) {
				if end > i && opensNonChatBlock(visible[end-1], visible[end]) {
					break
				}
				parts = append(parts, m.renderEventCached(visible[end], width))
				end++
			}
			if len(lines) > 0 {
				lines = append(lines, "")
			}
			lines = append(lines, renderNonChatBlock(visible[i:end], parts, width))
			i = end
			continue
		}
		if len(lines) > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, m.renderEventCached(visible[i], width))
		i++
	}
	m.viewport.SetContent(strings.Join(lines, "\n"))
	if !m.userScrolled {
		m.viewport.GotoBottom()
	}
}

func (m *Model) clearCurrentView() {
	retained := make([]seam.Event, 0, len(m.events))
	for _, event := range m.events {
		if event.AgentID != m.viewAgentID {
			retained = append(retained, event)
		}
	}
	m.events = retained
	m.userScrolled = false
	clear(m.markdownCache)
	clear(m.markdownRecent)
	clear(m.eventRenderCache)
	m.refreshView()
}

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

func markdownBlockKey(event seam.Event) string {
	return event.AgentID + "\x00" + event.Kind
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
	// Request payloads, status updates, and usage reports are durable
	// control-plane records, not chat output. Keep them in Model.events and the
	// runtime transcript while omitting them from the message viewport.
	return event.Kind != "inference_request" && event.Kind != "turn_done" && event.Kind != "status" && event.Kind != "usage"
}

func messageBlock(text string, width int, background color.Color) string {
	// Keep foreground resets in the content scoped so the background remains
	// continuous across the whole rectangle.
	text = strings.ReplaceAll(text, "\x1b[0m", "\x1b[39m")
	text = strings.ReplaceAll(text, "\x1b[m", "\x1b[39m")
	return lipgloss.NewStyle().Width(width).Background(background).Render(text)
}

func toolIndex(event seam.Event) string {
	if event.Metadata == nil {
		return ""
	}
	return fmt.Sprint(event.Metadata["index"])
}

func (m Model) View() (view tea.View) {
	started := time.Now()
	defer func() {
		m.frameProfiler.record("view", "frame", started, time.Now(), len(m.events))
	}()
	var content string
	if m.quitting {
		content = dim.Render("shutting down…")
	} else if m.modelsOpen {
		content = m.modelMenuView()
	} else {
		chat := m.viewport.View()
		if chatHeight := lipgloss.Height(chat); chatHeight < m.chatHeight() {
			chat += strings.Repeat("\n", m.chatHeight()-chatHeight)
		}
		bar := m.inputFrame()
		agents := m.agentPanel()
		status := m.statusLine()
		// Keep the agent list in the bottom control area.
		parts := []string{chat, bar}
		if agents != "" {
			parts = append(parts, agents)
		}
		parts = append(parts, status)
		content = strings.Join(parts, "\n")
	}
	view = tea.NewView(content)
	view.AltScreen = true
	view.MouseMode = tea.MouseModeNone
	if m.mouseCapture {
		view.MouseMode = tea.MouseModeCellMotion
	}
	return view
}

func (m Model) chatHeight() int {
	return max(1, m.height-m.footerHeight())
}

func (m Model) footerHeight() int {
	agentHeight := 0
	if agents := m.agentPanel(); agents != "" {
		agentHeight = lipgloss.Height(agents)
	}
	return lipgloss.Height(m.inputFrame()) + agentHeight + lipgloss.Height(m.statusLine())
}

func (m Model) inputFrame() string {
	width := max(1, m.chatWidth())
	input := m.input
	input.MaxHeight = m.inputMaxHeight()
	input.SetWidth(width)
	center := strings.TrimRight(input.View(), "\n")
	horizontal := dim.Render(strings.Repeat("─", width))
	return strings.Join([]string{horizontal, center, horizontal}, "\n")
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

func (m Model) statusLine() string {
	current := seam.AgentSnapshot{Model: "-", Effort: "-"}
	if agent, ok := agentSnapshot(m.runtime, m.viewAgentID); ok {
		current = agent
	} else if seat, ok := seatSnapshot(m.runtime); ok {
		current = seat
	}
	width := max(1, m.chatWidth())
	model := current.Model
	if model == "" {
		model = "no-model"
	}
	parts := []string{
		m.runtime.ID(),
		model + " " + current.Effort,
		formatContextStats(current),
		formatCacheStats(current),
	}
	if jobs := runningJobs(m.runtime.JobSnapshots()); jobs > 0 {
		parts = append(parts, fmt.Sprintf("jobs %d", jobs))
	}
	if agents := len(m.agents); agents > 1 {
		parts = append(parts, fmt.Sprintf("agents %d", agents))
	}
	if m.mouseCapture {
		parts = append(parts, "mouse")
	}
	return wrapToWidth(dim.Render(strings.Join(parts, " · ")), width)
}

// runningJobs counts jobs still running. The runtime keeps finished jobs for
// inspection, so their total only ever grows and says nothing about now.
func runningJobs(jobs []seam.JobSnapshot) int {
	running := 0
	for _, job := range jobs {
		if job.Status == "running" {
			running++
		}
	}
	return running
}

func formatContextStats(agent seam.AgentSnapshot) string {
	if agent.ContextWindow <= 0 {
		return "--/--"
	}
	// Used over the route's whole window, so the total stays fixed while the
	// numerator grows.
	used := max(0, agent.ContextUsed)
	return fmt.Sprintf("%s/%s", formatTokens(used), formatTokens(agent.ContextWindow))
}

func formatCacheStats(agent seam.AgentSnapshot) string {
	total := agent.CacheHitTokens + agent.CacheMissTokens
	if total <= 0 {
		return "--"
	}
	return fmt.Sprintf("%.0f%%", float64(agent.CacheHitTokens)*100/float64(total))
}

func formatTokens(tokens int) string {
	switch {
	case tokens < 1000:
		return fmt.Sprintf("%d", tokens)
	case tokens < 1000000:
		return strings.TrimSuffix(fmt.Sprintf("%.1f", float64(tokens)/1000), ".0") + "k"
	default:
		return strings.TrimSuffix(fmt.Sprintf("%.1f", float64(tokens)/1000000), ".0") + "M"
	}
}

func (m Model) agentPanel() string {
	if len(m.agents) == 0 {
		return ""
	}
	lines := make([]string, 0, len(m.agents))
	for i, a := range m.agents {
		line := strings.Repeat("  ", a.Depth)
		if m.focusAgents && i == m.selected {
			line += "> "
		} else {
			switch {
			case a.Depth == 1:
				line += "• "
			case a.Depth >= 2:
				line += "⚬ "
			}
		}
		line += a.Title + " [" + a.Status + "]"
		lines = append(lines, accent.Render(wrapToWidth(line, max(1, m.chatWidth()))))
	}
	return strings.Join(lines, "\n")
}

func seatID(runtime seam.Runtime) string {
	if seat, ok := seatSnapshot(runtime); ok {
		return seat.ID
	}
	return ""
}

func seatSnapshot(runtime seam.Runtime) (seam.AgentSnapshot, bool) {
	if runtime == nil {
		return seam.AgentSnapshot{}, false
	}
	for _, agent := range runtime.Agents() {
		if agent.Depth == 0 {
			return agent, true
		}
	}
	return seam.AgentSnapshot{}, false
}

func agentSnapshot(runtime seam.Runtime, agentID string) (seam.AgentSnapshot, bool) {
	if runtime == nil {
		return seam.AgentSnapshot{}, false
	}
	for _, agent := range runtime.Agents() {
		if agent.ID == agentID {
			return agent, true
		}
	}
	return seam.AgentSnapshot{}, false
}

func formatAgents(agents []seam.AgentSnapshot) string {
	var lines []string
	for _, a := range agents {
		lines = append(lines, fmt.Sprintf("%s depth=%d status=%s", a.Title, a.Depth, a.Status))
	}
	return strings.Join(lines, "\n")
}

func activeAgents(agents []seam.AgentSnapshot) []seam.AgentSnapshot {
	active := make([]seam.AgentSnapshot, 0, len(agents))
	for _, agent := range agents {
		if agent.Status != "stopped" {
			active = append(active, agent)
		}
	}
	return active
}

func containsAgent(agents []seam.AgentSnapshot, id string) bool {
	for _, agent := range agents {
		if agent.ID == id {
			return true
		}
	}
	return false
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
