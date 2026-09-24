package harness

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/slbdotdev/slbh/internal/config"
	"github.com/slbdotdev/slbh/internal/provider"
)

// The markup is what the rented ninfer-serve returned on 2026-09-24 when a
// lean agent named read_file beside bash.
const leakedMarkup = "<tool_call>\n<function=bash>\n<parameter=script>\nuname -r\n</parameter>\n</function>\n</tool_call>\n<tool_call>\n<function=read_file>\n<parameter=path>\n/etc/hostname\n</parameter>\n</function>\n</tool_call>"

func TestLeakedToolCallsDetection(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    []string
	}{
		{"qwen xml, two calls", leakedMarkup, []string{"bash", "read_file"}},
		{"after prose", "Let me look.\n\n" + leakedMarkup, []string{"bash", "read_file"}},
		{"json form", "<tool_call>\n{\"name\": \"read_file\", \"arguments\": {\"path\": \"x\"}}\n</tool_call>", []string{"read_file"}},
		{"unclosed at the end", "<tool_call>\n<function=read_file>\n<parameter=path>\nx", []string{"read_file"}},
		{"followed by prose", leakedMarkup + "\n\nThat is how a call looks.", nil},
		{"inside a code fence", "The format is:\n```\n" + leakedMarkup + "\n```", nil},
		{"no names", "<tool_call>\n</tool_call>", nil},
		{"plain answer", "done", nil},
	}
	for _, tc := range cases {
		names, ok := leakedToolCalls(tc.content)
		if ok != (tc.want != nil) || strings.Join(names, ",") != strings.Join(tc.want, ",") {
			t.Errorf("%s: leakedToolCalls = %v, %v; want %v", tc.name, names, ok, tc.want)
		}
	}
}

// markupServer answers a fresh message with leaked markup leaks times, then
// with a real bash call; a tool result with "done"; a delivered child result
// with "ack". It records every request.
type markupServer struct {
	leaks  int
	answer string

	mu       sync.Mutex
	requests []provider.Request
	leaked   int
}

func (s *markupServer) PinnedContextWindow() (int, bool) { return 200000, true }

func (s *markupServer) Stream(ctx context.Context, req provider.Request, sink provider.StreamSink) error {
	s.mu.Lock()
	s.requests = append(s.requests, req)
	leak := s.leaked < s.leaks
	if leak {
		s.leaked++
	}
	s.mu.Unlock()
	text := func(t string) error {
		if err := sink(provider.Event{Kind: provider.EventText, Text: t}); err != nil {
			return err
		}
		if err := sink(provider.Event{Kind: provider.EventUsage, StopReason: "stop"}); err != nil {
			return err
		}
		return sink(provider.Event{Kind: provider.EventDone})
	}
	last := req.Messages[len(req.Messages)-1]
	switch {
	case last.Role == "tool":
		return text("done")
	case strings.HasPrefix(last.Content, "[result from"):
		return text("ack")
	case s.answer != "":
		return text(s.answer)
	case leak:
		return text(leakedMarkup)
	}
	if err := sink(provider.Event{Kind: provider.EventTool, ToolName: "bash", ToolCallID: "real", Input: `{"script":"true"}`}); err != nil {
		return err
	}
	if err := sink(provider.Event{Kind: provider.EventUsage, StopReason: "tool_calls"}); err != nil {
		return err
	}
	return sink(provider.Event{Kind: provider.EventDone})
}

// markupRuntime runs the lean shape, which offers no read_file: the shape the
// leak was seen on.
func markupRuntime(t *testing.T, p provider.Provider) *Runtime {
	t.Helper()
	r, err := New(config.Config{Home: t.TempDir(), SeatModel: "test", SeatEffort: "high", SubagentModel: "test", SubagentEffort: "high", ToolShape: config.ToolShapeLean}, Options{Provider: func(string) (provider.Provider, error) { return p, nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

func TestLeakedToolCallMarkupIsAnsweredNotTakenAsTheAnswer(t *testing.T) {
	server := &markupServer{leaks: 1}
	r := markupRuntime(t, server)
	seat := r.seat()
	seat.Send("look around")
	events, _ := waitIdleTurn(t, r, seat.ID, 0)

	var warned, answered, ran bool
	for _, event := range events {
		switch {
		case event.Kind == "error":
			t.Fatalf("turn failed: %s", event.Text)
		case event.Kind == "warning" && event.Metadata["purpose"] == "tool_call_markup":
			warned = strings.Contains(event.Text, "read_file is not one of your tools")
		case event.Kind == "tool_start":
			ran = true
		case event.Kind == "assistant" && event.Text == "done":
			answered = true
		}
	}
	if !warned || !ran || !answered {
		t.Fatalf("warned about read_file %v, ran the real call %v, answered %v", warned, ran, answered)
	}
	note := server.requests[1].Messages[len(server.requests[1].Messages)-1]
	if note.Role != "user" || !strings.Contains(note.Content, "read_file is not one of your tools") || !strings.Contains(note.Content, "bash") {
		t.Fatalf("the retry did not tell the model what went wrong: %q", note.Content)
	}
}

// A subagent that leaks markup recovers and delivers its real answer, not
// the markup, to its parent.
func TestLeakedToolCallMarkupIsNotDeliveredByASubagent(t *testing.T) {
	server := &markupServer{leaks: 1}
	r := markupRuntime(t, server)
	seat := r.seat()
	child, err := r.launchSubagentSpec(seat.ID, LaunchSpec{Title: "child", Model: "test", Brief: "look around"})
	if err != nil {
		t.Fatal(err)
	}
	waitIdleTurn(t, r, child.ID, 0)
	waitIdleTurn(t, r, seat.ID, 0)
	for _, message := range seat.History() {
		if strings.HasPrefix(message.Content, "[result from child]") {
			if strings.Contains(message.Content, "<tool_call>") || !strings.Contains(message.Content, "done") {
				t.Fatalf("parent received %q", message.Content)
			}
			return
		}
	}
	t.Fatal("the parent never received the child's result")
}

// A model that keeps writing markup ends the turn with the reason after
// maxToolMarkupRetries notes, instead of spending the round limit silently.
func TestLeakedToolCallMarkupStopsAfterRepeatedReplies(t *testing.T) {
	server := &markupServer{leaks: 1000}
	r := markupRuntime(t, server)
	seat := r.seat()
	seat.Send("look around")
	events, _ := waitIdleTurn(t, r, seat.ID, 0)
	last := events[len(events)-1]
	if last.Kind != "error" || !strings.Contains(last.Text, "tool-call markup") {
		t.Fatalf("turn ended with %s %q, want the markup error", last.Kind, last.Text)
	}
	if got := len(server.requests); got != maxToolMarkupRetries+1 {
		t.Fatalf("%d requests, want %d", got, maxToolMarkupRetries+1)
	}
}

// An answer that shows markup in a code fence is an answer.
func TestToolCallMarkupInACodeFenceIsAnAnswer(t *testing.T) {
	server := &markupServer{answer: "The format is:\n```\n" + leakedMarkup + "\n```"}
	r := markupRuntime(t, server)
	seat := r.seat()
	seat.Send("how does a call look?")
	events, _ := waitIdleTurn(t, r, seat.ID, 0)
	for _, event := range events {
		if event.Kind == "warning" || event.Kind == "error" {
			t.Fatalf("a fenced example was treated as a leaked call: %s", event.Text)
		}
	}
	if len(server.requests) != 1 || events[len(events)-1].Kind != "turn_done" {
		t.Fatalf("%d requests, last event %s", len(server.requests), events[len(events)-1].Kind)
	}
}
