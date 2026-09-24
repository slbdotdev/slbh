package harness

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/slbdotdev/slbh/internal/config"
	"github.com/slbdotdev/slbh/internal/provider"
	"github.com/slbdotdev/slbh/internal/seam"
)

// scriptedCompactionProvider plays one agentic turn against a pinned window:
// the first request calls bash for a large output, which pushes the next
// request over 70% of the window, and the answer after compaction is taken
// from whatever the summary carried. Summary requests can be held open on
// gate so a test can look at the agent while the summary is being written.
type scriptedCompactionProvider struct {
	window  int
	summary string
	err     error
	entered chan struct{}
	gate    chan struct{}

	mu        sync.Mutex
	turns     []provider.Request
	summaries []provider.Request
}

func (p *scriptedCompactionProvider) PinnedContextWindow() (int, bool) { return p.window, true }

func (p *scriptedCompactionProvider) Stream(ctx context.Context, req provider.Request, sink provider.StreamSink) error {
	if req.System == compactSystemPrompt {
		p.mu.Lock()
		p.summaries = append(p.summaries, req)
		p.mu.Unlock()
		if p.entered != nil {
			p.entered <- struct{}{}
		}
		if p.gate != nil {
			select {
			case <-p.gate:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if p.err != nil {
			return p.err
		}
		if err := sink(provider.Event{Kind: provider.EventText, Text: p.summary}); err != nil {
			return err
		}
		if err := sink(provider.Event{Kind: provider.EventUsage, StopReason: "stop", Usage: map[string]any{"prompt_tokens": float64(10), "completion_tokens": float64(10)}}); err != nil {
			return err
		}
		return sink(provider.Event{Kind: provider.EventDone})
	}
	p.mu.Lock()
	p.turns = append(p.turns, req)
	turn := len(p.turns)
	p.mu.Unlock()
	if turn == 1 {
		if err := sink(provider.Event{Kind: provider.EventTool, ToolName: "bash", ToolCallID: "big", Input: `{"script":"head -c 20000 /dev/zero | tr '\\0' x"}`}); err != nil {
			return err
		}
		if err := sink(provider.Event{Kind: provider.EventUsage, StopReason: "tool_calls"}); err != nil {
			return err
		}
		return sink(provider.Event{Kind: provider.EventDone})
	}
	answer := "codeword lost"
	if summary, ok := previousSummary(req.Messages); ok && strings.Contains(summary, "PELICAN-42") {
		answer = "PELICAN-42"
	}
	if err := sink(provider.Event{Kind: provider.EventText, Text: answer}); err != nil {
		return err
	}
	if err := sink(provider.Event{Kind: provider.EventUsage, StopReason: "stop"}); err != nil {
		return err
	}
	return sink(provider.Event{Kind: provider.EventDone})
}

func compactionRuntime(t *testing.T, p provider.Provider) *Runtime {
	t.Helper()
	r, err := New(config.Config{Home: t.TempDir(), SeatModel: "test", SeatEffort: "high", SubagentModel: "test-child", SubagentEffort: "high", ToolShape: config.ToolShapeFull}, Options{Provider: func(string) (provider.Provider, error) { return p, nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

// agentEvents returns every event for one agent emitted so far.
func agentEvents(r *Runtime, agentID string) []seam.Event {
	var events []seam.Event
	for _, event := range r.PollEvents(seam.EventQuery{}).Events {
		if event.AgentID == agentID {
			events = append(events, event)
		}
	}
	return events
}

// requireOrder fails unless each predicate matches an event after the one
// the previous predicate matched.
func requireOrder(t *testing.T, events []seam.Event, steps ...struct {
	name  string
	match func(seam.Event) bool
}) {
	t.Helper()
	at := 0
	for _, step := range steps {
		found := false
		for ; at < len(events); at++ {
			if step.match(events[at]) {
				found = true
				at++
				break
			}
		}
		if !found {
			var kinds []string
			for _, event := range events {
				kinds = append(kinds, event.Kind+":"+event.Text)
			}
			t.Fatalf("no %s in order; events: %.2000s", step.name, strings.Join(kinds, " | "))
		}
	}
}

type orderStep = struct {
	name  string
	match func(seam.Event) bool
}

func statusIs(status string) orderStep {
	return orderStep{"status " + status, func(e seam.Event) bool { return e.Kind == "status" && e.Text == status }}
}

func waitEntered(t *testing.T, entered chan struct{}) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the summary request was never sent")
	}
}

// Automatic compaction through the real turn loop: a tool result crosses 70%
// of the pinned window, the agent shows compacting while the summary is
// written, the history is replaced, and the turn finishes from the summary.
func TestTurnCompactionReportsProgressAndContinues(t *testing.T) {
	p := &scriptedCompactionProvider{window: 8000, summary: "## Goal\nReply with the codeword PELICAN-42.", entered: make(chan struct{}, 1), gate: make(chan struct{})}
	r := compactionRuntime(t, p)
	seat := r.seat()
	seat.Send("The codeword is PELICAN-42. Run the command, then reply with the codeword.")

	waitEntered(t, p.entered)
	if got := seat.Snapshot().Status; got != "compacting" {
		t.Fatalf("status while the summary is written = %q, want compacting", got)
	}
	close(p.gate)
	waitAgentTurn(t, r, seat.ID)

	requireOrder(t, agentEvents(r, seat.ID),
		statusIs("thinking"),
		orderStep{"bash tool result", func(e seam.Event) bool { return e.Kind == "tool_result" }},
		statusIs("compacting"),
		orderStep{"compacting line", func(e seam.Event) bool {
			return e.Kind == "compacting" && strings.HasPrefix(e.Text, "compacting 1 earlier message")
		}},
		orderStep{"summary request", func(e seam.Event) bool {
			return e.Kind == "inference_request" && e.Metadata["purpose"] == "compaction"
		}},
		orderStep{"summary", func(e seam.Event) bool { return e.Kind == "compact" && e.Metadata["mode"] == "summary" }},
		statusIs("thinking"),
		orderStep{"answer from the summary", func(e seam.Event) bool { return e.Kind == "assistant" && e.Text == "PELICAN-42" }},
		orderStep{"turn_done", func(e seam.Event) bool { return e.Kind == "turn_done" }},
	)
	if len(p.summaries) != 1 || len(p.turns) != 2 {
		t.Fatalf("%d summary and %d turn requests, want 1 and 2", len(p.summaries), len(p.turns))
	}
	if _, ok := previousSummary(p.turns[1].Messages); !ok {
		t.Fatalf("the request after compaction does not open with the summary: %q", p.turns[1].Messages[0].Content)
	}
	if got := seat.Snapshot().Status; got != "idle" {
		t.Fatalf("status after the turn = %q, want idle", got)
	}
}

// /compact shows the same progress on an idle agent and returns it to idle.
func TestCompactCommandReportsProgress(t *testing.T) {
	p := &scriptedCompactionProvider{window: 8000, summary: "## Goal\nsummary", entered: make(chan struct{}, 1), gate: make(chan struct{})}
	r := compactionRuntime(t, p)
	seat := r.seat()
	seat.mu.Lock()
	seat.history = longTurn(10, 10)
	seat.mu.Unlock()

	done := make(chan error, 1)
	go func() {
		_, err := r.compact(seat.ID, 4)
		done <- err
	}()
	waitEntered(t, p.entered)
	if got := seat.Snapshot().Status; got != "compacting" {
		t.Fatalf("status during /compact = %q, want compacting", got)
	}
	close(p.gate)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	requireOrder(t, agentEvents(r, seat.ID),
		statusIs("compacting"),
		orderStep{"compacting line", func(e seam.Event) bool { return e.Kind == "compacting" && e.Text == "compacting 17 earlier messages" }},
		orderStep{"summary", func(e seam.Event) bool { return e.Kind == "compact" && e.Metadata["mode"] == "summary" }},
		statusIs("idle"),
	)
	if got := seat.Snapshot().Status; got != "idle" {
		t.Fatalf("status after /compact = %q, want idle", got)
	}
}

// A failed summary still ends the compacting status, and a status set while
// the summary was being written, such as a turn that started, is kept.
func TestCompactionStatusIsRestoredOnlyIfUnchanged(t *testing.T) {
	t.Run("summary fails", func(t *testing.T) {
		p := &scriptedCompactionProvider{window: 8000, err: summaryRefusal{errors.New("402 balance")}}
		r := compactionRuntime(t, p)
		seat := r.seat()
		seat.setStatus("thinking")
		got := seat.compactHistoryIfNeeded(context.Background(), p, "m", "low", longTurn(20, 4000), 20000, "system", nil)
		if !strings.HasPrefix(got[0].Content, "[compacted ") {
			t.Fatalf("fallback marker missing: %q", got[0].Content)
		}
		if status := seat.Snapshot().Status; status != "thinking" {
			t.Fatalf("status after a failed summary = %q, want thinking", status)
		}
		requireOrder(t, agentEvents(r, seat.ID),
			statusIs("compacting"),
			orderStep{"warning", func(e seam.Event) bool { return e.Kind == "warning" }},
			orderStep{"dropped", func(e seam.Event) bool { return e.Kind == "compact" && e.Metadata["mode"] == "drop" }},
			statusIs("thinking"),
		)
	})
	t.Run("status changed meanwhile", func(t *testing.T) {
		p := &scriptedCompactionProvider{window: 8000, summary: "## Goal\nx", entered: make(chan struct{}, 1), gate: make(chan struct{})}
		r := compactionRuntime(t, p)
		seat := r.seat()
		result := make(chan []provider.Message, 1)
		go func() {
			result <- seat.compactHistoryIfNeeded(context.Background(), p, "m", "low", longTurn(20, 4000), 20000, "system", nil)
		}()
		waitEntered(t, p.entered)
		seat.setStatus("waiting")
		close(p.gate)
		<-result
		if status := seat.Snapshot().Status; status != "waiting" {
			t.Fatalf("status = %q, want the one set during the summary", status)
		}
	})
}
