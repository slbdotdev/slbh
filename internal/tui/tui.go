package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/slbdotdev/slbh/internal/harness"
)

var (
	accent = lipgloss.NewStyle().Foreground(lipgloss.Color("205"))
	dim    = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	green  = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	yellow = lipgloss.NewStyle().Foreground(lipgloss.Color("220"))
	red    = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
)

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
	clearView    bool
	commandLine  string
}

func New(runtime *harness.Runtime) Model {
	input := textarea.New()
	input.Placeholder = "Message the root agent… (Enter sends; Down opens agents)"
	input.Prompt = "┃ "
	input.CharLimit = 0
	input.SetHeight(3)
	input.Focus()
	view := viewport.New(80, 20)
	root := runtime.Root()
	viewID := ""
	if root != nil {
		viewID = root.ID
	}
	return Model{runtime: runtime, viewport: view, input: input, viewAgentID: viewID, agents: runtime.Agents()}
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
	case tea.KeyMsg:
		return m.updateKey(msg)
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

func (m Model) updateKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.Type == tea.KeyCtrlC || (msg.Type == tea.KeyCtrlQ) {
		m.quitting = true
		_ = m.runtime.Close()
		return m, tea.Quit
	}
	if m.focusAgents {
		switch msg.Type {
		case tea.KeyEsc, tea.KeyUp:
			if msg.Type == tea.KeyEsc || m.selected == 0 {
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
				m.clearView = false
				m.refreshView()
			}
			m.focusAgents = false
			m.input.Focus()
		}
		return m, nil
	}
	if msg.Type == tea.KeyDown && !strings.Contains(m.input.Value(), "\n") {
		m.focusAgents = true
		m.input.Blur()
		return m, nil
	}
	if msg.Type == tea.KeyEnter {
		m.submit()
		if m.quitting {
			return m, tea.Quit
		}
		return m, nil
	}
	switch msg.Type {
	case tea.KeyPgUp, tea.KeyCtrlU:
		m.viewport.LineUp(5)
		m.userScrolled = true
		return m, nil
	case tea.KeyPgDown, tea.KeyCtrlD:
		m.viewport.LineDown(5)
		m.userScrolled = !m.viewport.AtBottom()
		return m, nil
	case tea.KeyEsc:
		if m.viewAgentID != rootID(m.runtime) {
			m.viewAgentID = rootID(m.runtime)
			m.refreshView()
		}
		return m, nil
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

func (m *Model) submit() {
	text := strings.TrimSpace(m.input.Value())
	if text == "" {
		return
	}
	m.input.Reset()
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
		m.clearView = true
		m.refreshView()
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
	m.input.SetWidth(contentWidth)
	m.viewport.Width = contentWidth
	m.viewport.Height = m.chatHeight()
	m.refreshView()
}

func (m Model) chatWidth() int {
	return m.width
}

func (m *Model) refreshView() {
	if m.width > 0 {
		m.viewport.Width = m.chatWidth()
	}
	var visible []harness.Event
	if !m.clearView {
		for _, event := range m.events {
			if event.AgentID != m.viewAgentID {
				continue
			}
			if len(visible) > 0 && visible[len(visible)-1].AgentID == event.AgentID && visible[len(visible)-1].Kind == event.Kind && (event.Kind == "assistant" || event.Kind == "thinking") {
				visible[len(visible)-1].Text += event.Text
				continue
			}
			visible = append(visible, event)
		}
	}
	lines := make([]string, 0, len(visible))
	for _, event := range visible {
		lines = append(lines, wrapToWidth(renderEvent(event), max(1, m.chatWidth())))
	}
	m.viewport.SetContent(strings.Join(lines, "\n"))
	if !m.userScrolled {
		m.viewport.GotoBottom()
	}
}

func renderEvent(event harness.Event) string {
	title := event.AgentTitle
	if title == "" {
		title = "agent"
	}
	switch event.Kind {
	case "assistant":
		return accent.Render(title+"> ") + event.Text
	case "user":
		return green.Render("you> ") + event.Text
	case "thinking":
		return dim.Render("thinking · ") + event.Text
	case "tool":
		return yellow.Render("tool · ") + event.Text
	case "error":
		return red.Render("error · ") + event.Text
	case "steer":
		return yellow.Render("steer · ") + event.Text
	case "usage":
		return dim.Render("usage · ") + fmt.Sprint(event.Metadata)
	case "status", "runtime", "tool_result":
		return dim.Render(event.Kind+" · ") + event.Text
	default:
		return event.Text
	}
}

func (m Model) View() string {
	if m.quitting {
		return dim.Render("shutting down…")
	}
	chat := m.viewport.View()
	bar := m.input.View()
	agents := m.agentPanel()
	status := m.statusLine()
	// Keep the agent list in the bottom control area. Besides matching the
	// layout contract, this makes Down from the input naturally enter it.
	parts := []string{chat, bar}
	if agents != "" {
		parts = append(parts, agents)
	}
	parts = append(parts, status)
	return strings.Join(parts, "\n")
}

func (m Model) chatHeight() int {
	return max(1, m.height-m.footerHeight())
}

func (m Model) footerHeight() int {
	// The newlines joining blocks do not consume an additional terminal row
	// beyond the first row of the next block.
	agentHeight := 0
	if agents := m.agentPanel(); agents != "" {
		agentHeight = lipgloss.Height(agents)
	}
	return lipgloss.Height(m.input.View()) + agentHeight + lipgloss.Height(m.statusLine())
}

func wrapToWidth(text string, width int) string {
	if width < 1 {
		return text
	}
	return lipgloss.NewStyle().Width(width).Render(text)
}

func (m Model) statusLine() string {
	root := m.runtime.Root()
	model := "-"
	effort := "-"
	if root != nil {
		model = root.Model
		effort = root.Effort
	}
	width := max(1, m.chatWidth())
	parts := []string{m.runtime.ID(), model + " " + effort}
	if jobs := len(m.runtime.Jobs().List()); jobs > 0 {
		parts = append(parts, fmt.Sprintf("jobs %d", jobs))
	}
	if agents := len(m.agents); agents > 1 {
		parts = append(parts, fmt.Sprintf("agents %d", agents))
	}
	return wrapToWidth(dim.Render(strings.Join(parts, " · ")), width)
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
