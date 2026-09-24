package intern

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/slbdotdev/slbh/internal/config"
	"github.com/slbdotdev/slbh/internal/harness"
	"github.com/slbdotdev/slbh/internal/provider"
	"github.com/slbdotdev/slbh/internal/seam"
)

// pollGate is a real runtime that signals the Intern's first PollEvents, so a
// Seat turn can be started only once the watcher is listening, with no sleep.
type pollGate struct {
	seam.Runtime
	once    sync.Once
	polling chan struct{}
}

func (g *pollGate) PollEvents(query seam.EventQuery) seam.EventBatch {
	g.once.Do(func() { close(g.polling) })
	return g.Runtime.PollEvents(query)
}

// seatRecorder answers every Seat turn and keeps each request's messages.
type seatRecorder struct {
	mu       sync.Mutex
	requests [][]provider.Message
	calls    chan struct{}
}

func (p *seatRecorder) Stream(_ context.Context, request provider.Request, sink provider.StreamSink) error {
	p.mu.Lock()
	p.requests = append(p.requests, append([]provider.Message(nil), request.Messages...))
	p.mu.Unlock()
	p.calls <- struct{}{}
	return sink(provider.Event{Kind: provider.EventText, Text: "SEAT_ANSWER"})
}

// TestInternWatchesARealRuntimeAndSteersTheSeat runs the Intern against a real
// harness.Runtime instead of the fake one: the Seat's completed turn reaches
// the Intern through the runtime's own event stream, the Intern's ask_seat
// becomes an [intern] steer that wakes the idle Seat, and the Seat's next
// request carries it. The Intern never appears as an agent and runs no job.
func TestInternWatchesARealRuntimeAndSteersTheSeat(t *testing.T) {
	const question = "Is the boundary stale?"
	seatProvider := &seatRecorder{calls: make(chan struct{}, 8)}
	rt, err := harness.New(config.Config{
		Home:        t.TempDir(),
		SeatModel:   "test",
		SeatEffort:  "high",
		InternModel: "local/test-intern",
		Instructions: config.Instructions{Layers: map[string]string{
			config.InstructionIntern: "Ask careful questions.",
		}},
	}, harness.Options{Provider: func(string) (provider.Provider, error) { return seatProvider, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	internProvider := newScriptedProvider(func(index int, _ context.Context, _ provider.Request, sink provider.StreamSink) error {
		if index == 0 {
			return sink(provider.Event{Kind: provider.EventTool, ToolIndex: 0, ToolCallID: "ask-1", ToolName: "ask_seat", Input: `{"question":"` + question + `"}`})
		}
		return nil
	})
	gate := &pollGate{Runtime: rt, polling: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, gate, Options{Provider: internProvider}) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Intern: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("Intern did not stop when its context ended")
		}
	}()
	select {
	case <-gate.polling:
	case err := <-done:
		t.Fatalf("Intern stopped before watching: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("Intern never polled the runtime")
	}

	seatID := rt.Agents()[0].ID
	if _, err := rt.Do(seam.SendPromptCommand{AgentID: seatID, Prompt: "FIRST_PROMPT"}); err != nil {
		t.Fatal(err)
	}

	// The Intern's first inference sees the Seat's completed turn.
	if got := waitCall(t, internProvider); got != 0 {
		t.Fatalf("first Intern call = %d", got)
	}
	var seen strings.Builder
	for _, message := range internProvider.request(0).Messages {
		seen.WriteString(message.Content)
	}
	if !strings.Contains(seen.String(), "FIRST_PROMPT") || !strings.Contains(seen.String(), "SEAT_ANSWER") {
		t.Fatalf("the Intern did not see the Seat's turn: %q", seen.String())
	}

	// Its question wakes the Seat, whose next request carries the signed steer.
	deadline := time.After(5 * time.Second)
	for steered := false; !steered; {
		select {
		case <-seatProvider.calls:
		case <-deadline:
			t.Fatal("the Seat never received the Intern's question")
		}
		seatProvider.mu.Lock()
		last := seatProvider.requests[len(seatProvider.requests)-1]
		seatProvider.mu.Unlock()
		for _, message := range last {
			if strings.Contains(message.Content, "[intern] "+question) {
				steered = true
			}
		}
	}

	for _, agent := range rt.Agents() {
		if agent.ID != seatID {
			t.Fatalf("the Intern appears among the agents: %#v", agent)
		}
	}
	if jobs := rt.JobSnapshots(); len(jobs) != 0 {
		t.Fatalf("the read-only Intern ran jobs: %#v", jobs)
	}
}
