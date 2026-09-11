package tui

import (
	"context"
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
	"github.com/slbdotdev/slbh/internal/provider"
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

const scrollStep = 5

const headerBullet = "• "

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
}

type eventMsg harness.Event

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

type Model struct {
	runtime        *harness.Runtime
	viewport       viewport.Model
	input          textarea.Model
	events         []harness.Event
	agents         []harness.AgentSnapshot
	viewAgentID    string
	selected       int
	focusAgents    bool
	userScrolled   bool
	width          int
	height         int
	quitting       bool
	commandLine    string
	history        []string
	historyPath    string
	historyIndex   int
	historyDraft   string
	modelsOpen     bool
	modelsLoading  bool
	modelCatalog   []provider.Catalog
	modelExpanded  map[string]bool
	modelCursor    int
	modelNotice    string
	modelSubmenu   bool
	modelSubnode   modelTreeNode
	modelSubcursor int
	modelSeat      string
	modelSubagent  string
	modelLeaf      string
	modelNoticeErr bool
	modelNoticeOK  bool
}

func New(runtime *harness.Runtime) Model {
	input := textarea.New()
	input.Placeholder = "Message the seat agent… (Enter sends; Ctrl-J adds a line)"
	input.Prompt = ""
	input.ShowLineNumbers = false
	input.CharLimit = 0
	input.DynamicHeight = true
	input.MinHeight = 1
	input.Focus()
	view := viewport.New(viewport.WithWidth(80), viewport.WithHeight(20))
	seat := runtime.Seat()
	viewID := ""
	if seat != nil {
		viewID = seat.ID
	}
	historyPath := ""
	var history []string
	if runtime != nil {
		historyPath = filepath.Join(runtime.Home(), historyFileName)
		history, _ = loadHistory(historyPath)
	}
	return Model{runtime: runtime, viewport: view, input: input, viewAgentID: viewID, agents: activeAgents(runtime.Agents()), history: history, historyPath: historyPath, historyIndex: -1, modelCatalog: runtime.ModelCatalog(), modelExpanded: make(map[string]bool)}
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
	case modelCatalogMsg:
		m.modelsLoading = false
		m.modelCatalog = msg.catalog
		m.runtime.SetModelCatalog(msg.catalog)
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
	case eventMsg:
		m.events = append(m.events, harness.Event(msg))
		m.agents = activeAgents(m.runtime.Agents())
		if !containsAgent(m.agents, m.viewAgentID) {
			m.viewAgentID = seatID(m.runtime)
			m.userScrolled = false
		}
		if m.selected >= len(m.agents) {
			m.selected = max(0, len(m.agents)-1)
		}
		// Agent and job events can change the footer height. Reflow the chat
		// viewport before rendering so newly spawned agents cannot push the
		// last panel row below the terminal.
		m.resize()
		if m.width < 1 || m.height < 1 {
			m.refreshView()
		}
		return m, waitEvent(m.runtime)
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
	a, ok := m.runtime.Agent(m.viewAgentID)
	if !ok {
		a = m.runtime.Seat()
	}
	if a == nil {
		return nil
	}
	var err error
	if a.ID == seatID(m.runtime) {
		err = a.Send(text)
	} else {
		err = a.Steer(text)
	}
	if err != nil {
		m.runtime.EmitStatus("error", err.Error())
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
		_ = m.runtime.Close()
		return nil
	case "/clear":
		if err := m.runtime.Clear(m.viewAgentID); err != nil {
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
			if err := m.runtime.ConfigureModels(arg, cfg.SubagentModel, approved); err != nil {
				m.addLocal("error", "save model configuration: "+err.Error())
			} else {
				m.addLocal("status", "seat model set to "+arg)
			}
		}
	case "/effort":
		if arg != "" {
			if seat := m.runtime.Seat(); seat != nil {
				seat.SetEffort(arg)
			}
			cfg := m.runtime.Config()
			cfg.SeatEffort = arg
			if err := cfg.Save(); err != nil {
				m.addLocal("error", "save effort configuration: "+err.Error())
				return nil
			}
			m.addLocal("status", "seat effort set to "+arg)
		}
	case "/agents":
		m.addLocal("status", formatAgents(m.agents))
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
	return nil
}

func (m *Model) addLocal(kind, text string) {
	m.events = append(m.events, harness.Event{Time: time.Now(), AgentID: m.viewAgentID, AgentTitle: "local", Kind: kind, Text: text})
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
		catalog, err := provider.DiscoverCatalog(ctx, m.runtime.Config().Endpoint)
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
	}
	return m, nil
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
	if err := m.runtime.ConfigureModelSlots(m.modelSeat, m.modelSubagent, m.modelLeaf, approvedSlots(m.modelSeat, m.modelSubagent, m.modelLeaf)); err != nil {
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
	if err := m.runtime.ConfigureModelSlots(m.modelSeat, m.modelSubagent, m.modelLeaf, approvedSlots(m.modelSeat, m.modelSubagent, m.modelLeaf)); err != nil {
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
		"r=seat · s=subagent · l=leaf · Enter=assign · Esc=save and close",
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
		return renderChatBlock(agentTitle(event, "agent"), event.Text, width, assistantBubble)
	case "user":
		return renderChatBlock("user", event.Text, width, userBubble)
	case "child_result":
		return renderChatBlock(agentTitle(event, "subagent"), event.Text, width, subagentBubble)
	case "steer":
		if isForwardedAgentMessage(event) {
			return renderChatBlock(agentTitle(event, "subagent"), event.Text, width, subagentBubble)
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

func renderChatBlock(label, text string, width int, background color.Color) string {
	header := renderHeader(label)
	body := messageBlock(text, width, background)
	return header + "\n" + body
}

func renderHeader(label string) string {
	return headerStyle.Render(headerBullet + label)
}

func isMessage(event harness.Event) bool {
	return event.Kind == "user" || event.Kind == "assistant" || event.Kind == "child_result" || isForwardedAgentMessage(event)
}

func isForwardedAgentMessage(event harness.Event) bool {
	if event.Kind != "steer" || event.Metadata == nil {
		return false
	}
	_, ok := event.Metadata["sender"]
	return ok
}

func agentTitle(event harness.Event, fallback string) string {
	if title := strings.TrimSpace(event.AgentTitle); title != "" {
		return title
	}
	return fallback
}

func isViewportEvent(event harness.Event) bool {
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
	header := nonChatHeader(events)
	body := rollingBlock(strings.Join(parts, "\n"), width)
	return header + "\n" + body
}

func nonChatHeader(events []harness.Event) string {
	label := events[len(events)-1].Kind
	for i := len(events) - 1; i >= 0; i-- {
		if label := responseType(events[i]); label != "" {
			return renderHeader(label)
		}
	}
	return renderHeader(label)
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
	} else if seat := m.runtime.Seat(); seat != nil {
		current = seat.Snapshot()
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

func seatID(runtime *harness.Runtime) string {
	if seat := runtime.Seat(); seat != nil {
		return seat.ID
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

func activeAgents(agents []harness.AgentSnapshot) []harness.AgentSnapshot {
	active := make([]harness.AgentSnapshot, 0, len(agents))
	for _, agent := range agents {
		if agent.Status != "stopped" {
			active = append(active, agent)
		}
	}
	return active
}

func containsAgent(agents []harness.AgentSnapshot, id string) bool {
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
