package harness

import (
	"context"
	"strings"
	"testing"

	"github.com/slbdotdev/slbh/internal/provider"
)

func TestContextBudgetUsesSeventyPercentOfDiscoveredWindow(t *testing.T) {
	if got := contextBudget(1000000); got != 700000 {
		t.Fatalf("budget = %d, want 700000", got)
	}
	if got := contextBudget(provider.FallbackContextWindow); got != 89600 {
		t.Fatalf("fallback budget = %d, want 89600", got)
	}

	history := make([]provider.Message, 26)
	for i := range history {
		history[i] = provider.Message{Role: "user", Content: strings.Repeat("x", 5000)}
	}
	if contextLimitReached(1000000, "system", history, nil) {
		t.Fatal("history of roughly 33k estimated tokens reached the 700k-token budget")
	}
}

func TestCompactMessagesPreservesTurnBoundary(t *testing.T) {
	call := provider.ToolCall{ID: "call-1", Type: "function"}
	call.Function.Name = "quick_bash"
	history := []provider.Message{
		{Role: "user", Content: "old request"},
		{Role: "assistant", ToolCalls: []provider.ToolCall{call}},
		{Role: "tool", ToolCallID: "call-1", Name: "quick_bash", Content: "old result"},
		{Role: "user", Content: "recent request"},
		{Role: "assistant", Content: "recent answer"},
		{Role: "user", Content: "latest request"},
		{Role: "assistant", Content: "latest answer"},
	}

	compacted, dropped := compactMessages(history, 4)
	if dropped != 3 {
		t.Fatalf("dropped = %d, want 3", dropped)
	}
	if len(compacted) != 5 || compacted[0].Role != "user" || !strings.HasPrefix(compacted[0].Content, "[compacted 3") {
		t.Fatalf("unexpected compacted prefix: %#v", compacted[:min(len(compacted), 2)])
	}
	if compacted[1].Role != "user" {
		t.Fatalf("retained suffix begins with %q, want user", compacted[1].Role)
	}
	for i, message := range compacted[1:] {
		if message.Role != "tool" {
			continue
		}
		if i == 0 || compacted[i].Role != "assistant" || len(compacted[i].ToolCalls) == 0 {
			t.Fatalf("orphaned tool result at retained index %d", i+1)
		}
	}
}

func TestSystemPromptDirectsAsyncChildHandling(t *testing.T) {
	r := testRuntime(t)
	prompt := systemPrompt(r.Seat())
	for _, phrase := range []string{
		"launch_subagent returns immediately",
		"do not block this turn waiting for a child",
		"Do not use quick_bash, long_job, quick_py, long_py, sleep, polling, or shell wait loops",
		"end your turn",
		"mandatory mid-turn steer",
		"next API/tool call boundary",
		"In-flight API and tool calls finish normally",
		"Deferring a message until the end of a turn is a failure",
		"responsible for ending each subagent with end_subagent",
		"subagents stay alive indefinitely",
		"three relevant words joined by hyphens",
		"guidance, not a validation rule",
	} {
		if !strings.Contains(prompt, phrase) {
			t.Fatalf("system prompt missing async guidance %q: %s", phrase, prompt)
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// windowProvider is a ContextWindowProvider that reports a discoverable
// window, so a pin can be proved to beat discovery and not merely the
// fallback. discovery records whether the catalog was consulted at all.
type windowProvider struct {
	window    int
	discovery *bool
}

func (windowProvider) Stream(context.Context, provider.Request, provider.StreamSink) error {
	return nil
}

func (p windowProvider) ContextWindow(context.Context, string) (int, error) {
	if p.discovery != nil {
		*p.discovery = true
	}
	return p.window, nil
}

// plainProvider implements no context-window capability, which is the case
// FallbackContextWindow exists for.
type plainProvider struct{}

func (plainProvider) Stream(context.Context, provider.Request, provider.StreamSink) error {
	return nil
}

func TestResolveContextWindowPrefersPinOverDiscoveryAndFallback(t *testing.T) {
	// The pin beats discovery. A route is pinned because its catalog cannot
	// answer, so a catalog that answers anyway must not win: asserting only
	// against the fallback would pass even if the pin were consulted last.
	discovered := false
	agent := &Agent{Model: "zai/glm-5.3-flash"}
	got := agent.resolveContextWindow(context.Background(), windowProvider{window: 200000, discovery: &discovered})
	if got != 1000000 {
		t.Fatalf("pinned window = %d, want 1000000", got)
	}
	if discovered {
		t.Fatal("catalog discovery was consulted for a pinned route")
	}

	// The pin beats the fallback on a provider with no discovery at all.
	agent = &Agent{Model: "zai/glm-5.3-flash"}
	if got := agent.resolveContextWindow(context.Background(), plainProvider{}); got != 1000000 {
		t.Fatalf("pinned window without discovery = %d, want 1000000", got)
	}

	// The bare slug addresses the same route and carries the same pin.
	agent = &Agent{Model: "glm-5.3-flash"}
	if got := agent.resolveContextWindow(context.Background(), plainProvider{}); got != 1000000 {
		t.Fatalf("bare-slug pinned window = %d, want 1000000", got)
	}
}

func TestResolveContextWindowFallsBackForUnpinnedRoutes(t *testing.T) {
	// An unpinned route with no discovery still falls back to 128,000.
	agent := &Agent{Model: "deepseek-v4-flash"}
	if got := agent.resolveContextWindow(context.Background(), plainProvider{}); got != provider.FallbackContextWindow {
		t.Fatalf("unpinned window = %d, want %d", got, provider.FallbackContextWindow)
	}

	// An unpinned route with discovery still uses the discovered figure: the
	// pin table must not suppress the path that already worked.
	agent = &Agent{Model: "vendor/model"}
	if got := agent.resolveContextWindow(context.Background(), windowProvider{window: 262144}); got != 262144 {
		t.Fatalf("discovered window = %d, want 262144", got)
	}

	// A `[1m]` spelling is rejected 1211 on both wires and is not an alias of
	// the plain slug, so it must not inherit the pin.
	agent = &Agent{Model: "zai/glm-5.3-flash[1m]"}
	if got := agent.resolveContextWindow(context.Background(), plainProvider{}); got != provider.FallbackContextWindow {
		t.Fatalf("[1m] window = %d, want the fallback %d", got, provider.FallbackContextWindow)
	}
}

func TestCompactionFiresAtSeventyPercentOfThePinnedWindow(t *testing.T) {
	agent := &Agent{Model: "zai/glm-5.3-flash"}
	window := agent.resolveContextWindow(context.Background(), plainProvider{})

	// Roughly 120k estimated tokens: over the 89,600-token fallback budget
	// that governs this route today, and well under the pinned 700,000 one.
	// This is the whole point of the pin, so it is asserted on both budgets.
	history := make([]provider.Message, 96)
	for i := range history {
		history[i] = provider.Message{Role: "user", Content: strings.Repeat("x", 5000)}
	}
	if !contextLimitReached(provider.FallbackContextWindow, "system", history, nil) {
		t.Fatal("history did not reach the fallback budget, so the pin is not what this test measures")
	}
	if contextLimitReached(window, "system", history, nil) {
		t.Fatalf("compaction fired below 70%% of the pinned %d-token window", window)
	}
}
