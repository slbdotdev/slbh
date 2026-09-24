package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/slbdotdev/slbh/internal/config"
	"github.com/slbdotdev/slbh/internal/harness"
	"github.com/slbdotdev/slbh/internal/provider"
	"github.com/slbdotdev/slbh/internal/seam"
)

// echoProvider answers every turn by quoting the latest user message in upper
// case, so a rendered answer proves which message reached which agent.
type echoProvider struct{}

func (echoProvider) Stream(_ context.Context, request provider.Request, sink provider.StreamSink) error {
	last := ""
	for _, message := range request.Messages {
		if message.Role == "user" {
			last = message.Content
		}
	}
	return sink(provider.Event{Kind: provider.EventText, Text: "REPLY " + strings.ToUpper(last)})
}

// pumpUntilTurnDone runs the TUI's own event loop — the eventBatchMsg and the
// waitEvents command it returns — until agentID finishes a turn.
func pumpUntilTurnDone(t *testing.T, m Model, agentID string) Model {
	t.Helper()
	cmd := waitEvents(m.runtime, m.eventCursor)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		msg := cmd()
		batch, ok := msg.(eventBatchMsg)
		if !ok {
			t.Fatalf("event loop produced %T", msg)
		}
		updated, next := m.Update(msg)
		m = updated.(Model)
		for _, event := range seam.EventBatch(batch).Events {
			if event.AgentID == agentID && event.Kind == "turn_done" {
				return m
			}
			if event.AgentID == agentID && event.Kind == "error" {
				t.Fatalf("agent %s failed: %s", agentID, event.Text)
			}
		}
		if next == nil {
			t.Fatal("event loop stopped")
		}
		cmd = waitEvents(m.runtime, m.eventCursor)
	}
	t.Fatalf("agent %s did not finish a turn", agentID)
	return m
}

func pressEnter(t *testing.T, m Model, text string) Model {
	t.Helper()
	m.input.SetValue(text)
	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	return updated.(Model)
}

// TestEnterRoutesThroughTheRuntimeToTheViewedAgent drives the documented Enter
// path end to end: from the Seat's view a message is a prompt to the Seat, and
// from a child's view it is a steer to that child. Each answer comes back
// through the runtime's event stream and renders in that agent's view only.
func TestEnterRoutesThroughTheRuntimeToTheViewedAgent(t *testing.T) {
	runtime, err := harness.New(config.Config{Home: t.TempDir(), SeatModel: "test", SeatEffort: "high", SubagentModel: "test-child", SubagentEffort: "high"}, harness.Options{Provider: func(string) (provider.Provider, error) { return echoProvider{}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	m := New(runtime)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	m = updated.(Model)
	seat := seatID(runtime)

	m = pressEnter(t, m, "hello seat")
	m = pumpUntilTurnDone(t, m, seat)
	view := ansi.Strip(m.viewport.View())
	if !strings.Contains(view, "hello seat") || !strings.Contains(view, "REPLY HELLO SEAT") {
		t.Fatalf("the Seat's prompt and answer did not render: %q", view)
	}

	child := launchTestSubagent(t, runtime, "helper")
	m.viewAgentID = child.ID
	m.refreshView()
	m = pressEnter(t, m, "to the child")
	m = pumpUntilTurnDone(t, m, child.ID)
	view = ansi.Strip(m.viewport.View())
	if !strings.Contains(view, "TO THE CHILD") {
		t.Fatalf("the child's answer to the steer did not render in its view: %q", view)
	}
	if strings.Contains(view, "HELLO SEAT") {
		t.Fatalf("the child's view shows the Seat's conversation: %q", view)
	}

	m.viewAgentID = seat
	m.refreshView()
	if view := ansi.Strip(m.viewport.View()); strings.Contains(view, "to the child") {
		t.Fatalf("the steer reached the Seat's view: %q", view)
	}
}
