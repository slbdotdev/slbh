package tui

import (
	"context"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
	"github.com/slbdotdev/slbh/internal/config"
	"github.com/slbdotdev/slbh/internal/harness"
	"github.com/slbdotdev/slbh/internal/provider"
)

type quietProvider struct{}

func (quietProvider) Stream(context.Context, provider.Request, provider.StreamSink) error { return nil }

func TestViewFillsTerminalAndWrapsContent(t *testing.T) {
	runtime, err := harness.New(config.Config{Home: t.TempDir(), RootModel: "test", RootEffort: "high"}, harness.Options{Provider: func(string) (provider.Provider, error) { return quietProvider{}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	m := New(runtime)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = updated.(Model)
	m.events = append(m.events, harness.Event{AgentID: runtime.Root().ID, AgentTitle: "root", Kind: "error", Text: strings.Repeat("long error ", 20)})
	m.refreshView()
	view := m.View()
	if got := lipgloss.Height(view); got != 24 {
		t.Fatalf("view height=%d, want 24", got)
	}
	if strings.Contains(view, "long error long error long error long error long error long error long error long error long error long error long error long error long error long error long error long error long error long error long error long error") {
		t.Fatal("long content was not wrapped")
	}
	if got := m.agentPanel(); got != "" {
		t.Fatal("root-only runtime should hide the agent list")
	}
	m.agents = append(m.agents, harness.AgentSnapshot{ID: "child", Title: "child", Status: "idle", Depth: 1})
	if got := lipgloss.Width(m.agentPanel()); got != 80 {
		t.Fatalf("agent list width=%d, want full terminal width", got)
	}
	if strings.Contains(m.statusLine(), "Enter send") || strings.Contains(m.statusLine(), "agents 1") || strings.Contains(m.statusLine(), "jobs 0") {
		t.Fatal("footer contains hidden help or zero-count metadata")
	}
	if strings.Contains(m.statusLine(), " / ") || !strings.Contains(m.statusLine(), runtime.Root().Model+" "+runtime.Root().Effort) {
		t.Fatal("footer should show the actual model identifier followed by effort")
	}
}

func TestMessageBlocksAreSpacedAndColored(t *testing.T) {
	runtime, err := harness.New(config.Config{Home: t.TempDir(), RootModel: "test", RootEffort: "high"}, harness.Options{Provider: func(string) (provider.Provider, error) { return quietProvider{}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	width := 40
	user := harness.Event{AgentID: runtime.Root().ID, AgentTitle: "root", Kind: "user", Text: "hello"}
	thinking := harness.Event{AgentID: runtime.Root().ID, AgentTitle: "root", Kind: "thinking", Text: "working"}
	assistant := harness.Event{AgentID: runtime.Root().ID, AgentTitle: "root", Kind: "assistant", Text: "done"}

	if got := lipgloss.Width(renderEvent(user, width)); got != lipgloss.Width("you> hello") {
		t.Fatalf("user block width=%d, want content width %d", got, lipgloss.Width("you> hello"))
	}
	if got := lipgloss.Width(renderEvent(assistant, width)); got != lipgloss.Width("root> done") {
		t.Fatalf("assistant block width=%d, want content width %d", got, lipgloss.Width("root> done"))
	}

	profile := lipgloss.DefaultRenderer().ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI256)
	t.Cleanup(func() { lipgloss.SetColorProfile(profile) })
	userBlock := renderEvent(user, width)
	assistantBlock := renderEvent(assistant, width)
	if !strings.Contains(userBlock, "48;5;24") {
		t.Fatalf("user block has no colored background: %q", userBlock)
	}
	if !strings.Contains(assistantBlock, "48;5;236") {
		t.Fatalf("assistant block has no colored background: %q", assistantBlock)
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
	if !hasBlankAfter("you> hello") {
		t.Fatalf("user block is not separated from following text: %q", content)
	}
	if !hasBlankAfter("thinking · working") {
		t.Fatalf("text is not separated from following assistant block: %q", content)
	}
}
