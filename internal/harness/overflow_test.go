package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/slbdotdev/slbh/internal/provider"
	"github.com/slbdotdev/slbh/internal/seam"
)

// denseServer refuses, as ninfer-serve does, any request over its window,
// counting one token per byte: the dense output that slbh's four-bytes-per-
// token estimate undercounts by up to 3.3x. The same figure is pinned as the
// route's window, so the 70% check never fires and only a refusal can.
//
// Every agent follows one script: answer a fresh message with a bash call
// (calls of them at once, each printing output bytes), and answer the
// results with "done". Summary requests are held to the same limit.
type denseServer struct {
	window int
	calls  int
	output int

	mu       sync.Mutex
	refusals int
}

func (s *denseServer) PinnedContextWindow() (int, bool) { return s.window, true }

// requestBytes counts what a server tokenizes: the system prompt, the tool
// definitions and every message.
func requestBytes(req provider.Request) int {
	tools, _ := json.Marshal(req.Tools)
	n := len(req.System) + len(tools)
	for _, message := range req.Messages {
		n += len(message.Content) + len(message.ReasoningContent)
		for _, call := range message.ToolCalls {
			n += len(call.Function.Arguments)
		}
	}
	return n
}

func (s *denseServer) Stream(ctx context.Context, req provider.Request, sink provider.StreamSink) error {
	if requestBytes(req) > s.window {
		s.mu.Lock()
		s.refusals++
		s.mu.Unlock()
		return &provider.StatusError{Status: 400, Code: "context_length_exceeded", Message: fmt.Sprintf("prepared prompt exceeds Engine max_context %d", s.window), Wire: provider.WireOpenAIChat}
	}
	done := func(stop string) error {
		if err := sink(provider.Event{Kind: provider.EventUsage, StopReason: stop}); err != nil {
			return err
		}
		return sink(provider.Event{Kind: provider.EventDone})
	}
	if req.System == compactSystemPrompt {
		if err := sink(provider.Event{Kind: provider.EventText, Text: "## Goal\nfinish the task"}); err != nil {
			return err
		}
		return done("stop")
	}
	if last := req.Messages[len(req.Messages)-1]; last.Role == "tool" {
		if err := sink(provider.Event{Kind: provider.EventText, Text: "done"}); err != nil {
			return err
		}
		return done("stop")
	}
	script := fmt.Sprintf(`{"script":"head -c %d /dev/zero | tr '\\0' 7"}`, s.output)
	for i := 0; i < s.calls; i++ {
		if err := sink(provider.Event{Kind: provider.EventTool, ToolName: "bash", ToolCallID: fmt.Sprintf("call-%d", i), ToolIndex: i, Input: script}); err != nil {
			return err
		}
	}
	return done("tool_calls")
}

func overflowRuntime(t *testing.T, server *denseServer) *Runtime {
	t.Helper()
	return compactionRuntime(t, server)
}

// waitIdleTurn waits for agentID's next turn_done, or the error that ends a
// failed turn without one, and returns the events it emitted meanwhile.
func waitIdleTurn(t *testing.T, r *Runtime, agentID string, after seam.EventCursor) ([]seam.Event, seam.EventCursor) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var events []seam.Event
	cursor := after
	for time.Now().Before(deadline) {
		batch := r.PollEvents(seam.EventQuery{After: cursor, Limit: 256, WaitMilliseconds: 100})
		cursor = batch.Cursor
		for _, event := range batch.Events {
			if event.AgentID != agentID {
				continue
			}
			events = append(events, event)
			if event.Kind == "turn_done" || event.Kind == "error" {
				return events, cursor
			}
		}
	}
	t.Fatalf("agent %s did not finish its turn", agentID)
	return nil, cursor
}

func requireRecovered(t *testing.T, events []seam.Event, wantWarning string) {
	t.Helper()
	var warned, answered bool
	for _, event := range events {
		switch {
		case event.Kind == "error" || (event.Kind == "status" && event.Text == "error"):
			t.Fatalf("agent failed instead of recovering: %s", event.Text)
		case event.Kind == "warning" && event.Metadata["purpose"] == "overflow":
			warned = warned || strings.Contains(event.Text, wantWarning)
		case event.Kind == "assistant" && event.Text == "done":
			answered = true
		}
	}
	if !warned || !answered {
		var seen []string
		for _, event := range events {
			if event.Kind == "warning" || event.Kind == "request_error" {
				seen = append(seen, event.Kind+": "+event.Text)
			}
		}
		t.Fatalf("warned %q: %v, answered: %v; saw %q", wantWarning, warned, answered, seen)
	}
}

func TestOverflowRecoveryNeverLeavesAnAgentStuck(t *testing.T) {
	cases := []struct {
		name    string
		server  *denseServer
		seed    []provider.Message
		prompt  string
		warning string
	}{
		{
			name:    "one tool result over the window",
			server:  &denseServer{window: 20000, calls: 1, output: 16000},
			prompt:  "run it",
			warning: "cut 1 oversized tool result",
		},
		{
			name:    "parallel results over the window together",
			server:  &denseServer{window: 20000, calls: 3, output: 6000},
			prompt:  "run them",
			warning: "cut 3 oversized tool result",
		},
		{
			name:    "a pasted message over the window",
			server:  &denseServer{window: 20000, calls: 1, output: 100},
			prompt:  strings.Repeat("pasted log line 12345\n", 800),
			warning: "cut 1 oversized message",
		},
		{
			name:    "many small dense messages",
			server:  &denseServer{window: 20000, calls: 1, output: 100},
			seed:    denseHistory(40, 500),
			prompt:  "continue",
			warning: "compacting all but the latest message",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := tc.server
			r := overflowRuntime(t, server)
			seat := r.seat()
			if tc.seed != nil {
				seat.mu.Lock()
				seat.history = tc.seed
				seat.mu.Unlock()
			}
			seat.Send(tc.prompt)
			events, cursor := waitIdleTurn(t, r, seat.ID, seam.EventCursor(0))
			requireRecovered(t, events, tc.warning)
			if server.refusals == 0 {
				t.Fatal("the server never refused, so nothing was recovered from")
			}
			// Not stuck: the next message is answered as well.
			seat.Send("again")
			events, _ = waitIdleTurn(t, r, seat.ID, cursor)
			for _, event := range events {
				if event.Kind == "error" {
					t.Fatalf("the follow-up failed: %s", event.Text)
				}
			}
			if status := seat.Snapshot().Status; status != "idle" {
				t.Fatalf("status = %q, want idle", status)
			}
		})
	}
}

// A subagent recovers the same way, and its parent, answering the delivered
// result with its own oversized call, does too.
func TestOverflowRecoveryInASubagentAndItsParent(t *testing.T) {
	server := &denseServer{window: 20000, calls: 1, output: 16000}
	r := overflowRuntime(t, server)
	seat := r.seat()
	child, err := r.launchSubagentSpec(seat.ID, LaunchSpec{Title: "child", Model: "test", Brief: "run it"})
	if err != nil {
		t.Fatal(err)
	}
	events, _ := waitIdleTurn(t, r, child.ID, seam.EventCursor(0))
	requireRecovered(t, events, "cut 1 oversized tool result")
	events, _ = waitIdleTurn(t, r, seat.ID, seam.EventCursor(0))
	requireRecovered(t, events, "cut 1 oversized tool result")
}

func denseHistory(n, size int) []provider.Message {
	history := make([]provider.Message, 0, n)
	for i := 0; i < n; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		history = append(history, provider.Message{Role: role, Content: strings.Repeat(fmt.Sprint(i%10), size)})
	}
	return history
}
