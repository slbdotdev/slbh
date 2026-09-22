package harness

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/slbdotdev/slbh/internal/provider"
	"github.com/slbdotdev/slbh/internal/seam"
)

// summaryProvider answers the summary request and records every request it
// is sent. err fails every attempt; stop sets the usage event's stop reason;
// tool makes the summarizer reach for a tool.
type summaryProvider struct {
	mu       sync.Mutex
	requests []provider.Request
	summary  string
	err      error
	stop     string
	tool     bool
}

func (p *summaryProvider) Stream(_ context.Context, req provider.Request, sink provider.StreamSink) error {
	p.mu.Lock()
	p.requests = append(p.requests, req)
	p.mu.Unlock()
	if p.err != nil {
		return p.err
	}
	if p.tool {
		if err := sink(provider.Event{Kind: provider.EventTool, ToolName: "bash", ToolCallID: "c", Input: "{}"}); err != nil {
			return err
		}
	}
	if err := sink(provider.Event{Kind: provider.EventText, Text: p.summary}); err != nil {
		return err
	}
	stop := p.stop
	if stop == "" {
		stop = "stop"
	}
	if err := sink(provider.Event{Kind: provider.EventUsage, Usage: map[string]any{"prompt_tokens": float64(900), "completion_tokens": float64(90)}, StopReason: stop}); err != nil {
		return err
	}
	return sink(provider.Event{Kind: provider.EventDone})
}

func bashCall(id, args string) provider.ToolCall {
	call := provider.ToolCall{ID: id, Type: "function"}
	call.Function.Name = "bash"
	call.Function.Arguments = args
	return call
}

// longTurn is one request followed by n call/result pairs, each result
// about size bytes: the shape of a single agentic turn.
func longTurn(n, size int) []provider.Message {
	history := []provider.Message{{Role: "user", Content: "INITIAL REQUEST: migrate every caller"}}
	for i := 0; i < n; i++ {
		id := "call-" + string(rune('a'+i))
		history = append(history,
			provider.Message{Role: "assistant", ReasoningContent: "think", ToolCalls: []provider.ToolCall{bashCall(id, `{"script":"step"}`)}},
			provider.Message{Role: "tool", ToolCallID: id, Name: "bash", Content: strings.Repeat("r", size)},
		)
	}
	return history
}

// The cut falls on a call, never on a result, and it can fall inside a
// single turn: the old rule walked back to a user message, and a turn has
// only the one it began with, so a long turn could never be compacted.
func TestCompactionCutsInsideALongTurnAtACall(t *testing.T) {
	history := longTurn(20, 4000)
	cut := compactionCut(history, 3000, 0)
	if cut == 0 {
		t.Fatal("a long single turn was not cut")
	}
	if history[cut].Role != "assistant" || len(history[cut].ToolCalls) == 0 {
		t.Fatalf("cut at %d lands on %q, want a tool call", cut, history[cut].Role)
	}
	kept := 0
	for _, message := range history[cut:] {
		kept += messageTokens(message)
	}
	// The budget is crossed on a result, and the cut moves on to the next
	// call, so the tail is within one pair below the budget, as in pi.
	pair := messageTokens(history[1]) + messageTokens(history[2])
	if kept > 3000 || kept < 3000-pair {
		t.Fatalf("kept %d estimated tokens, want within one %d-token pair below the 3000 budget", kept, pair)
	}

	// A tail that is all one result keeps the call before it.
	if cut := compactionCut(longTurn(3, 40000), 100, 0); cut != 5 {
		t.Fatalf("cut = %d, want 5, the last call", cut)
	}
	// A history inside the budget has nothing to compact.
	if cut := compactionCut(longTurn(2, 10), 20000, 0); cut != 0 {
		t.Fatalf("cut = %d for a history under the budget, want 0", cut)
	}
}

// keep > 0 is a message count, and a cut that would land on a result moves
// on to the next call.
func TestCompactionCutByMessageCount(t *testing.T) {
	history := longTurn(4, 10) // 9 messages
	if cut := compactionCut(history, 0, 4); cut != 5 {
		t.Fatalf("keep 4: cut = %d, want 5", cut)
	}
	if cut := compactionCut(history, 0, 3); cut != 7 {
		t.Fatalf("keep 3: cut = %d, want 7, past the result at 6", cut)
	}
	if cut := compactionCut(history, 0, 100); cut != 0 {
		t.Fatalf("keep 100: cut = %d, want 0", cut)
	}
}

// A summary the history opens with is never cut: it is updated.
func TestCompactionCutNeverTakesThePreviousSummary(t *testing.T) {
	history := append([]provider.Message{{Role: "user", Content: compactSummaryPrefix + "OLD" + compactSummarySuffix}}, longTurn(3, 10)[1:]...)
	if previous, ok := previousSummary(history); !ok || previous != "OLD" {
		t.Fatalf("previousSummary = %q, %v", previous, ok)
	}
	if cut := compactionCut(history, 0, len(history)-1); cut != 0 {
		t.Fatalf("cut = %d, want 0: only the summary lies before the tail", cut)
	}
	if cut := compactionCut(history, 0, 2); cut != 5 {
		t.Fatalf("cut = %d, want 5", cut)
	}
}

func TestSerializeForSummary(t *testing.T) {
	history := []provider.Message{
		{Role: "user", Content: "fix it"},
		{Role: "assistant", Content: "looking", ReasoningContent: "BATCH-THOUGHT", ToolCalls: []provider.ToolCall{bashCall("1", `{"script":"ls"}`)}},
		{Role: "tool", ToolCallID: "1", Content: strings.Repeat("x", compactResultMaxChars+50)},
		{Role: "assistant", ReasoningContent: "BATCH-THOUGHT", ToolCalls: []provider.ToolCall{bashCall("2", `{"script":"cat a"}`)}},
		{Role: "tool", ToolCallID: "2", Content: "a"},
	}
	text := serializeForSummary(history)
	for _, want := range []string{
		"[User]: fix it",
		"[Assistant]: looking",
		`[Assistant tool calls]: bash({"script":"ls"})`,
		"[... 50 more characters truncated]",
		`[Assistant tool calls]: bash({"script":"cat a"})`,
		"[Tool result]: a",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("serialization missing %q:\n%s", want, text)
		}
	}
	if strings.Count(text, "BATCH-THOUGHT") != 1 {
		t.Fatalf("a batch's reasoning is written %d times, want once", strings.Count(text, "BATCH-THOUGHT"))
	}
}

func compactionEvents(t *testing.T, r *Runtime) []seam.Event {
	t.Helper()
	var events []seam.Event
	for _, event := range r.PollEvents(seam.EventQuery{}).Events {
		if event.Kind == "compact" || event.Kind == "warning" || event.Metadata["purpose"] == "compaction" {
			events = append(events, event)
		}
	}
	return events
}

// At the limit, the turn loop replaces all but the tail with a summary from
// the agent's own model: one request, no tools, the summarizer's system
// prompt, the initial request inside the conversation it summarizes.
func TestTurnCompactionSummarizesWithTheAgentsModel(t *testing.T) {
	r := testRuntime(t)
	agent := r.seat()
	p := &summaryProvider{summary: "## Goal\nmigrate every caller"}
	history := longTurn(20, 4000)
	window := 20000 // budget 14,000, tail 5,000

	got := agent.compactHistoryIfNeeded(context.Background(), p, "the-model", "low", history, window, "system", nil)
	if len(p.requests) != 1 {
		t.Fatalf("%d summary requests, want 1", len(p.requests))
	}
	req := p.requests[0]
	if req.Model != "the-model" || req.Effort != "low" || req.System != compactSystemPrompt || len(req.Tools) != 0 {
		t.Fatalf("summary request = model %q effort %q, %d tools, system %q", req.Model, req.Effort, len(req.Tools), req.System)
	}
	if req.MaxTokens == nil || *req.MaxTokens != compactSummaryMaxTokens {
		t.Fatalf("summary bound = %v, want %d", req.MaxTokens, compactSummaryMaxTokens)
	}
	prompt := req.Messages[0].Content
	if !strings.Contains(prompt, "[User]: INITIAL REQUEST") || !strings.HasSuffix(prompt, compactPrompt) || strings.Contains(prompt, "<previous-summary>") {
		t.Fatalf("summary prompt does not summarize the initial request with the first-time prompt:\n%.400s", prompt)
	}

	if got[0].Content != compactSummaryPrefix+"## Goal\nmigrate every caller"+compactSummarySuffix {
		t.Fatalf("history opens with %q, want the summary", got[0].Content)
	}
	cut := compactionCut(history, compactKeepBudget(window), 0)
	if len(got) != 1+len(history)-cut || got[1].Role != "assistant" {
		t.Fatalf("compacted history has %d messages, want the summary and the %d-message tail", len(got), len(history)-cut)
	}
	for _, message := range got {
		if strings.Contains(message.Content, "INITIAL REQUEST") {
			t.Fatal("the initial request was kept verbatim; it belongs in the summary")
		}
	}

	var usage, compact bool
	for _, event := range compactionEvents(t, r) {
		switch event.Kind {
		case "usage":
			usage = true
		case "compact":
			compact = event.Metadata["mode"] == "summary" && event.Text == "## Goal\nmigrate every caller"
		}
	}
	if !usage || !compact {
		t.Fatalf("usage recorded %v, summary event %v; the summary's cost and text must both be on record", usage, compact)
	}
	if agent.Snapshot().ContextUsed == 900 {
		t.Fatal("the summary request's prompt tokens became the agent's context reading")
	}

	// The next compaction updates the summary rather than summarizing it.
	grown := append(got, longTurn(20, 4000)[1:]...)
	p.summary = "## Goal\nupdated"
	again := agent.compactHistoryIfNeeded(context.Background(), p, "the-model", "low", grown, window, "system", nil)
	update := p.requests[1].Messages[0].Content
	if !strings.Contains(update, "<previous-summary>\n## Goal\nmigrate every caller\n</previous-summary>") || !strings.HasSuffix(update, compactUpdatePrompt) {
		t.Fatalf("second compaction did not update the previous summary:\n%.400s", update)
	}
	if strings.Contains(update, "[User]: "+compactSummaryPrefix) {
		t.Fatal("the previous summary was serialized as conversation")
	}
	if previous, _ := previousSummary(again); previous != "## Goal\nupdated" || strings.Count(again[0].Content, "<summary>") != 1 {
		t.Fatalf("history opens with %q, want the one updated summary", again[0].Content)
	}
}

// Without a summary the old behaviour stands in, and says why.
func TestTurnCompactionFallsBackToDropping(t *testing.T) {
	for name, p := range map[string]*summaryProvider{
		"provider error": {err: summaryRefusal{errors.New("402 balance")}},
		"cut off":        {summary: "## Goal\npartial", stop: "length"},
		"tool call":      {summary: "## Goal", tool: true},
		"empty":          {summary: "  "},
	} {
		t.Run(name, func(t *testing.T) {
			r := testRuntime(t)
			agent := r.seat()
			summarized := append([]provider.Message{{Role: "user", Content: compactSummaryPrefix + "KEEP ME" + compactSummarySuffix}}, longTurn(20, 4000)[1:]...)
			got := agent.compactHistoryIfNeeded(context.Background(), p, "m", "low", summarized, 20000, "system", nil)
			if previous, ok := previousSummary(got); !ok || previous != "KEEP ME" {
				t.Fatalf("fallback lost the previous summary: %q", got[0].Content)
			}
			if !strings.HasPrefix(got[1].Content, "[compacted ") {
				t.Fatalf("fallback marker missing: %q", got[1].Content)
			}
			if len(got) >= len(summarized) {
				t.Fatal("fallback dropped nothing")
			}
			var warned bool
			for _, event := range compactionEvents(t, r) {
				warned = warned || (event.Kind == "warning" && strings.Contains(event.Text, "compaction summary failed"))
			}
			if !warned {
				t.Fatal("the fallback did not warn")
			}
		})
	}
}

// Under the limit nothing is sent.
func TestTurnCompactionBelowTheLimitSendsNothing(t *testing.T) {
	r := testRuntime(t)
	p := &summaryProvider{summary: "x"}
	history := longTurn(3, 10)
	if got := r.seat().compactHistoryIfNeeded(context.Background(), p, "m", "low", history, 1_000_000, "system", nil); len(got) != len(history) || len(p.requests) != 0 {
		t.Fatalf("compacted under the limit: %d requests", len(p.requests))
	}
}

// /compact summarizes an idle agent with the runtime's provider for its
// model, and refuses a busy one.
func TestCompactCommandSummarizesAnIdleAgent(t *testing.T) {
	r := testRuntime(t)
	seat := r.seat()
	seat.mu.Lock()
	seat.history = longTurn(10, 10)
	seat.mu.Unlock()

	replaced, err := r.compact(seat.ID, 4)
	if err != nil {
		t.Fatal(err)
	}
	if replaced != 17 {
		t.Fatalf("replaced = %d, want 17", replaced)
	}
	seat.mu.RLock()
	history := append([]provider.Message(nil), seat.history...)
	seat.mu.RUnlock()
	if _, ok := previousSummary(history); !ok || len(history) != 5 {
		t.Fatalf("history after /compact: %d messages, opens with %q", len(history), history[0].Content)
	}

	seat.mu.Lock()
	seat.busy = true
	seat.mu.Unlock()
	if _, err := r.compact(seat.ID, 2); err == nil || !strings.Contains(err.Error(), "mid-turn") {
		t.Fatalf("busy agent compacted: %v", err)
	}
}
