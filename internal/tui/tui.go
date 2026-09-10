package tui

import (
	"fmt"
	"image/color"
	"path/filepath"
	"strings"
	"time"

	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/slbdotdev/slbh/internal/harness"
)

var (
	accent             = lipgloss.NewStyle().Foreground(lipgloss.Color("205"))
	dim                = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	chatLabelStyle     = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("255"))
	yellow             = lipgloss.NewStyle().Foreground(lipgloss.Color("220"))
	red                = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	assistantBubble    = lipgloss.Color("24")
	userBubble         = lipgloss.Color("22")
	nonChatBubble      = lipgloss.Color("236")
	nonChatHeaderStyle = yellow.Bold(true)
)

const nonChatBlockHeight = 10

const scrollStep = 5

var slashCommands = []string{
	"/exit",
	"/quit",
	"/q",
	"/clear",
	"/model",
	"/effort",
	"/agents",
	"/jobs",
	"/compact",
}

type eventMsg harness.Event

type Model struct {
	runtime      *harness.Runtime
	viewport     viewport.Model
	input        textarea.Model
	events       []harness.Event
	agents       []harness.AgentSnapshot
	viewAgentID  string
	selected     int
	focusAgents  bool
	userScrolled bool
	width        int
	height       int
	quitting     bool
	commandLine  string
	history      []string
	historyPath  string
	historyIndex int
	historyDraft string
}

func New(runtime *harness.Runtime) Model {
	input := textarea.New()
	input.Placeholder = "Message the root agent… (Enter sends; Ctrl-J adds a line)"
	input.Prompt = ""
	input.ShowLineNumbers = false
	input.CharLimit = 0
	input.DynamicHeight = true
	input.MinHeight = 1
	input.Focus()
	view := viewport.New(viewport.WithWidth(80), viewport.WithHeight(20))
	root := runtime.Root()
	viewID := ""
	if root != nil {
		viewID = root.ID
	}
	historyPath := ""
	var history []string
	if runtime != nil {
		historyPath = filepath.Join(runtime.Home(), historyFileName)
		history, _ = loadHistory(historyPath)
	}
	return Model{runtime: runtime, viewport: view, input: input, viewAgentID: viewID, agents: runtime.Agents(), history: history, historyPath: historyPath, historyIndex: -1}
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(textarea.Blink, waitEvent(m.runtime))
}

func waitEvent(runtime *harness.Runtime) tea.Cmd {
	return func() tea.Msg {
		event, ok := <-runtime.Events()
		if !ok {
			return nil
		}
		return eventMsg(event)
	}
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.resize()
		return m, nil
	case eventMsg:
		m.events = append(m.events, harness.Event(msg))
		m.agents = m.runtime.Agents()
		m.refreshView()
		return m, waitEvent(m.runtime)
	case tea.MouseMsg:
		return m.updateMouse(msg)
	case tea.KeyPressMsg:
		return m.updateKey(msg)
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

func (m Model) updateKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if isControlKey(msg, 'c') || isControlKey(msg, 'q') {
		m.quitting = true
		_ = m.runtime.Close()
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
	if msg.Code == tea.KeyUp {
		if m.recallHistory(-1) {
			return m, nil
		}
	}
	if msg.Code == tea.KeyDown && m.historyIndex != -1 {
		if m.recallHistory(1) {
			return m, nil
		}
	}
	if msg.Code == tea.KeyDown && !strings.Contains(m.input.Value(), "\n") {
		m.focusAgents = true
		m.input.Blur()
		return m, nil
	}
	if msg.Code == tea.KeyEnter {
		m.submit()
		if m.quitting {
			return m, tea.Quit
		}
		return m, nil
	}
	switch {
	case msg.Code == tea.KeyPgUp || isControlKey(msg, 'u'):
		m.scrollUp()
		return m, nil
	case msg.Code == tea.KeyPgDown || isControlKey(msg, 'd'):
		m.scrollDown()
		return m, nil
	case msg.Code == tea.KeyEsc:
		if m.viewAgentID != rootID(m.runtime) {
			m.viewAgentID = rootID(m.runtime)
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

func (m *Model) scrollUp() {
	m.scrollUpBy(scrollStep)
}

func (m *Model) scrollUpBy(lines int) {
	if lines < 1 {
		lines = 1
	}
	m.viewport.ScrollUp(lines)
	m.userScrolled = true
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

func (m *Model) submit() {
	text := strings.TrimSpace(m.input.Value())
	if text == "" {
		return
	}
	m.input.Reset()
	m.resetHistoryNavigation()
	m.recordHistory(text)
	if strings.HasPrefix(text, "/") {
		m.handleCommand(text)
		return
	}
	a, ok := m.runtime.Agent(m.viewAgentID)
	if !ok {
		a = m.runtime.Root()
	}
	if a == nil {
		return
	}
	if a.ID == rootID(m.runtime) {
		a.Send(text)
	} else {
		a.Steer(text)
	}
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
	var match string
	for _, command := range slashCommands {
		if strings.HasPrefix(command, partial) {
			if match != "" {
				return false
			}
			match = command
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

func (m *Model) handleCommand(command string) {
	parts := strings.Fields(command)
	name := strings.ToLower(parts[0])
	arg := ""
	if len(parts) > 1 {
		arg = strings.Join(parts[1:], " ")
	}
	switch name {
	case "/exit", "/quit", "/q":
		m.quitting = true
		_ = m.runtime.Close()
		return
	case "/clear":
		if err := m.runtime.Clear(m.viewAgentID); err != nil {
			m.addLocal("error", err.Error())
			return
		}
		m.clearCurrentView()
	case "/model":
		if arg != "" {
			if root := m.runtime.Root(); root != nil {
				root.Model = arg
			}
			m.addLocal("status", "root model set to "+arg)
		}
	case "/effort":
		if arg != "" {
			if root := m.runtime.Root(); root != nil {
				root.Effort = arg
			}
			m.addLocal("status", "root effort set to "+arg)
		}
	case "/agents":
		m.addLocal("status", formatAgents(m.runtime.Agents()))
	case "/jobs":
		m.addLocal("status", fmt.Sprintf("%v", m.runtime.Jobs().List()))
	case "/compact":
		if dropped, err := m.runtime.Compact(m.viewAgentID, 24); err != nil {
			m.addLocal("error", err.Error())
		} else {
			m.addLocal("status", fmt.Sprintf("compacted %d earlier messages; recent transcript remains available", dropped))
		}
	default:
		m.addLocal("error", "unknown command: "+name)
	}
}

func (m *Model) addLocal(kind, text string) {
	m.events = append(m.events, harness.Event{Time: time.Now(), AgentID: m.viewAgentID, AgentTitle: "local", Kind: kind, Text: text})
	m.refreshView()
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
		}
		m.refreshView()
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
	var visible []harness.Event
	userSeen := false
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
		if len(visible) > 0 && visible[len(visible)-1].AgentID == event.AgentID && visible[len(visible)-1].Kind == event.Kind && (event.Kind == "assistant" || event.Kind == "thinking" || (event.Kind == "tool" && toolIndex(visible[len(visible)-1]) == toolIndex(event))) {
			visible[len(visible)-1].Text += event.Text
			continue
		}
		visible = append(visible, event)
	}
	lines := make([]string, 0, len(visible))
	width := max(1, m.chatWidth())
	for i := 0; i < len(visible); {
		if !isMessage(visible[i]) {
			end := i
			parts := make([]string, 0, 1)
			for end < len(visible) && !isMessage(visible[end]) {
				parts = append(parts, renderEvent(visible[end], width))
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
		lines = append(lines, renderEvent(visible[i], width))
		i++
	}
	m.viewport.SetContent(strings.Join(lines, "\n"))
	if !m.userScrolled {
		m.viewport.GotoBottom()
	}
}

func (m *Model) clearCurrentView() {
	retained := make([]harness.Event, 0, len(m.events))
	for _, event := range m.events {
		if event.AgentID != m.viewAgentID {
			retained = append(retained, event)
		}
	}
	m.events = retained
	m.userScrolled = false
	m.refreshView()
}

func renderEvent(event harness.Event, width int) string {
	switch event.Kind {
	case "assistant":
		return messageBlock(chatLabelStyle.Render("agent> ")+event.Text, width, assistantBubble)
	case "user":
		return messageBlock(chatLabelStyle.Render("user> ")+event.Text, width, userBubble)
	case "thinking":
		return rollingBlock(dim.Render("thinking · ")+event.Text, width)
	case "tool":
		name, _ := event.Metadata["name"].(string)
		if name == "" {
			name = "call"
		}
		return rollingBlock(yellow.Render("tool "+name+" · ")+event.Text, width)
	case "error":
		return rollingBlock(red.Render("error · ")+event.Text, width)
	case "steer":
		return rollingBlock(yellow.Render("steer · ")+event.Text, width)
	case "usage":
		return rollingBlock(dim.Render("usage · ")+fmt.Sprint(event.Metadata), width)
	case "status", "runtime", "tool_result":
		return rollingBlock(dim.Render(event.Kind+" · ")+event.Text, width)
	default:
		return rollingBlock(event.Text, width)
	}
}

func isMessage(event harness.Event) bool {
	return event.Kind == "user" || event.Kind == "assistant"
}

func isViewportEvent(event harness.Event) bool {
	// Request payloads are durable audit/replay records, not chat output.
	return event.Kind != "inference_request" && event.Kind != "turn_done"
}

func messageBlock(text string, width int, background color.Color) string {
	// The label is foreground-styled before this block is rendered. Lipgloss's
	// foreground style emits a full reset, which would clear the block
	// background before the message text. Keep the foreground reset scoped so
	// the background remains continuous across the whole rectangle.
	text = strings.ReplaceAll(text, "\x1b[0m", "\x1b[39m")
	text = strings.ReplaceAll(text, "\x1b[m", "\x1b[39m")
	return lipgloss.NewStyle().Width(width).Background(background).Render(text)
}

func toolIndex(event harness.Event) string {
	if event.Metadata == nil {
		return ""
	}
	return fmt.Sprint(event.Metadata["index"])
}

func (m Model) View() tea.View {
	var content string
	if m.quitting {
		content = dim.Render("shutting down…")
	} else {
		chat := m.viewport.View()
		if chatHeight := lipgloss.Height(chat); chatHeight < m.chatHeight() {
			chat += strings.Repeat("\n", m.chatHeight()-chatHeight)
		}
		bar := m.inputFrame()
		agents := m.agentPanel()
		status := m.statusLine()
		// Keep the agent list in the bottom control area. Besides matching the
		// layout contract, this makes Down from the input naturally enter it.
		parts := []string{chat, bar}
		if agents != "" {
			parts = append(parts, agents)
		}
		parts = append(parts, status)
		content = strings.Join(parts, "\n")
	}
	view := tea.NewView(content)
	view.AltScreen = true
	view.MouseMode = tea.MouseModeCellMotion
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
// The complete event remains in Model.events (and in the runtime transcript);
// only this rendered copy is limited to the newest visual lines.
func rollingBlock(text string, width int) string {
	wrapped := wrapToWidth(text, width)
	lines := strings.Split(wrapped, "\n")
	if len(lines) > nonChatBlockHeight {
		lines = lines[len(lines)-nonChatBlockHeight:]
	}
	return messageBlock(strings.Join(lines, "\n"), width, nonChatBubble)
}

func renderNonChatBlock(events []harness.Event, parts []string, width int) string {
	if len(events) == 0 {
		return ""
	}
	header := messageBlock(nonChatHeader(events), width, nonChatBubble)
	body := rollingBlock(strings.Join(parts, "\n"), width)
	return header + "\n" + body
}

func nonChatHeader(events []harness.Event) string {
	for i := len(events) - 1; i >= 0; i-- {
		if label := responseType(events[i]); label != "" {
			return nonChatHeaderStyle.Render(label)
		}
	}
	return nonChatHeaderStyle.Render(events[len(events)-1].Kind)
}

func responseType(event harness.Event) string {
	switch event.Kind {
	case "thinking":
		return "thinking"
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

func (m Model) statusLine() string {
	current := harness.AgentSnapshot{Model: "-", Effort: "-"}
	if agent, ok := m.runtime.Agent(m.viewAgentID); ok {
		current = agent.Snapshot()
	} else if root := m.runtime.Root(); root != nil {
		current = root.Snapshot()
	}
	width := max(1, m.chatWidth())
	parts := []string{
		m.runtime.ID(),
		current.Model + " " + current.Effort,
		formatContextStats(current),
		formatCacheStats(current),
	}
	if jobs := len(m.runtime.Jobs().List()); jobs > 0 {
		parts = append(parts, fmt.Sprintf("jobs %d", jobs))
	}
	if agents := len(m.agents); agents > 1 {
		parts = append(parts, fmt.Sprintf("agents %d", agents))
	}
	return wrapToWidth(dim.Render(strings.Join(parts, " · ")), width)
}

func formatContextStats(agent harness.AgentSnapshot) string {
	if agent.ContextWindow <= 0 {
		return "--/--"
	}
	used := max(0, agent.ContextUsed)
	available := agent.ContextWindow - used
	if available < 0 {
		available = 0
	}
	return fmt.Sprintf("%s/%s", formatTokens(used), formatTokens(available))
}

func formatCacheStats(agent harness.AgentSnapshot) string {
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
	if len(m.agents) <= 1 {
		return ""
	}
	lines := []string{accent.Render("AGENTS")}
	for i, a := range m.agents {
		marker := "  "
		if m.focusAgents && i == m.selected {
			marker = "> "
		}
		prefix := strings.Repeat("  ", a.Depth)
		lines = append(lines, marker+prefix+a.Title+" ["+a.Status+"]")
	}
	wrapped := make([]string, 0, len(lines))
	for _, line := range lines {
		wrapped = append(wrapped, wrapToWidth(line, max(1, m.chatWidth())))
	}
	return strings.Join(wrapped, "\n")
}

func rootID(runtime *harness.Runtime) string {
	if root := runtime.Root(); root != nil {
		return root.ID
	}
	return ""
}

func formatAgents(agents []harness.AgentSnapshot) string {
	var lines []string
	for _, a := range agents {
		lines = append(lines, fmt.Sprintf("%s depth=%d status=%s", a.Title, a.Depth, a.Status))
	}
	return strings.Join(lines, "\n")
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
