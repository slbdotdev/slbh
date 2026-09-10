package tui

import (
	"context"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
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
}
