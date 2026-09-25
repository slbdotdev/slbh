package tui

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/slbdotdev/slbh/internal/seam"
)

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
		line += a.Title + " [" + formatAgentStatus(a, time.Now()) + "]"
		if a.ContextWindow > 0 {
			line += " " + formatContextStats(a)
		}
		lines = append(lines, accent.Render(wrapToWidth(line, max(1, m.chatWidth()))))
	}
	return strings.Join(lines, "\n")
}

// formatAgentStatus appends how long the agent has held its status. The
// event poll wakes the TUI at least every 250ms, so the count stays live.
func formatAgentStatus(a seam.AgentSnapshot, now time.Time) string {
	if a.StatusSince.IsZero() {
		return a.Status
	}
	return a.Status + " " + formatElapsed(now.Sub(a.StatusSince))
}

func formatElapsed(d time.Duration) string {
	seconds := int(max(0, d) / time.Second)
	switch {
	case seconds < 60:
		return fmt.Sprintf("%ds", seconds)
	case seconds < 3600:
		return fmt.Sprintf("%dm%02ds", seconds/60, seconds%60)
	default:
		return fmt.Sprintf("%dh%02dm", seconds/3600, seconds%3600/60)
	}
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
		line := fmt.Sprintf("%s depth=%d status=%s", a.Title, a.Depth, formatAgentStatus(a, time.Now()))
		if a.ContextWindow > 0 {
			line += " context=" + formatContextStats(a)
		}
		lines = append(lines, line)
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
