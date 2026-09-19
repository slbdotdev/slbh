package harness

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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
	if contextLimitReached(1000000, "system", history, nil, contextAnchor{}) {
		t.Fatal("history of roughly 33k estimated tokens reached the 700k-token budget")
	}
}

// The run that motivated anchoring: a thinking model on a 64k local window
// replayed ~65k estimated tokens of reasoning the template never sent, while
// the server reported 13.5k. The real count must win for the prefix it
// measured, and only the messages after it may be estimated.
func TestContextEstimateAnchorsOnReportedPromptTokens(t *testing.T) {
	const window = 65536
	system := "system"
	reasoning := strings.Repeat("r", 240000) // ~60k estimated tokens
	history := []provider.Message{
		{Role: "user", Content: "question"},
		{Role: "assistant", Content: "", ReasoningContent: reasoning},
	}
	if !contextLimitReached(window, system, history, nil, contextAnchor{}) {
		t.Fatal("fixture does not reach the budget unanchored, so the anchor is not what this test measures")
	}

	agent := &Agent{}
	agent.recordRequestContext(provider.Request{System: system, Messages: history}, window)
	agent.recordUsage(map[string]any{"prompt_tokens": float64(13500)})
	anchor := agent.contextAnchor
	if anchor.tokens != 13500 || anchor.messages != len(history) {
		t.Fatalf("anchor = %+v, want 13500 tokens over %d messages", anchor, len(history))
	}
	if got := estimateContextTokens(system, nil, history, anchor); got != 13500 {
		t.Fatalf("estimate of the measured prompt = %d, want the reported 13500", got)
	}

	// Messages appended after the measured request are estimated on top.
	grown := append(append([]provider.Message(nil), history...), provider.Message{Role: "tool", ToolCallID: "c1", Content: strings.Repeat("x", 4000)})
	got := estimateContextTokens(system, nil, grown, anchor)
	if got <= 13500 || got > 13500+1100 {
		t.Fatalf("anchored estimate = %d, want 13500 plus roughly 1k for the new tool result", got)
	}
	if contextLimitReached(window, system, grown, nil, anchor) {
		t.Fatal("compaction fired on a 14.5k-token prompt in a 64k window")
	}
	agent.recordRequestContext(provider.Request{System: system, Messages: grown}, window)
	if agent.contextUsed != got {
		t.Fatalf("status line context = %d, want the anchored %d", agent.contextUsed, got)
	}

	// A changed prefix (compaction, another system prompt) voids the anchor.
	compacted := append([]provider.Message{{Role: "user", Content: "[compacted]"}}, grown[1:]...)
	if got := estimateContextTokens(system, nil, compacted, anchor); got < 60000 {
		t.Fatalf("estimate over a changed prefix = %d, want the unanchored byte estimate", got)
	}
	if got := estimateContextTokens("other system", nil, grown, anchor); got < 60000 {
		t.Fatalf("estimate under another system prompt = %d, want the unanchored byte estimate", got)
	}

	// A new model or a cleared session starts over.
	agent.SetModel("other/model")
	if agent.contextAnchor != (contextAnchor{}) || agent.pendingAnchor != (contextAnchor{}) {
		t.Fatal("anchor survived a model change")
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
	prompt := systemPrompt(r.seat())
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

// windowProvider is a ContextWindowProvider that reports a discoverable window
// and no route policy, so a pin can be proved to beat discovery and not merely
// the fallback. discovery records whether the catalog was consulted at all.
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

// plainProvider implements neither capability, which is the case
// FallbackContextWindow exists for.
type plainProvider struct{}

func (plainProvider) Stream(context.Context, provider.Request, provider.StreamSink) error {
	return nil
}

// pinnedProvider carries a route policy and a discoverable window at once, so
// the two can be told apart by which one wins.
type pinnedProvider struct {
	windowProvider
	pin int
}

func (p pinnedProvider) PinnedContextWindow() (int, bool) {
	if p.pin > 0 {
		return p.pin, true
	}
	return 0, false
}

// planPolicy loads the committed local-policy artifact rather than writing a
// policy inline, so the pin these tests assert is the one a real document
// carries and not one the test invented. It is the /models-authored example,
// which puts the plan route on the coding wire — the wire this build speaks.
func planPolicy(t *testing.T) provider.Policy {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "config", "testdata", "config-with-local-policy.json"))
	if err != nil {
		t.Fatalf("read local policy artifact: %v", err)
	}
	var doc struct {
		LocalPolicy provider.Policy `json:"local_policy"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("decode local policy artifact: %v", err)
	}
	if err := doc.LocalPolicy.Validate(); err != nil {
		t.Fatalf("committed local policy artifact is invalid: %v", err)
	}
	return doc.LocalPolicy
}

// zaiRouteProvider builds the real route-resolved provider for the plan model,
// which is what carries the pin in production. The key is a dummy: the route is
// resolved and inspected, never dialled.
func zaiRouteProvider(t *testing.T) provider.Provider {
	t.Helper()
	t.Setenv("ZAI_API_KEY", "test-key-not-a-credential")
	p, err := provider.ForModel("zai/glm-5.3-flash", provider.OpenRouterEndpoint, false, planPolicy(t))
	if err != nil {
		t.Fatalf("resolve plan route: %v", err)
	}
	return p
}

func TestResolveContextWindowPrefersPinOverDiscoveryAndFallback(t *testing.T) {
	// The pin beats discovery. A route is pinned because its catalog cannot
	// answer, so a catalog that answers anyway must not win: asserting only
	// against the fallback would pass even if the pin were consulted last.
	discovered := false
	agent := &Agent{Model: "zai/glm-5.3-flash"}
	got := agent.resolveContextWindow(context.Background(), pinnedProvider{
		windowProvider: windowProvider{window: 200000, discovery: &discovered},
		pin:            1000000,
	})
	if got != 1000000 {
		t.Fatalf("pinned window = %d, want 1000000", got)
	}
	if discovered {
		t.Fatal("catalog discovery was consulted for a pinned route")
	}

	// The real route-resolved provider carries the pin, so the production path
	// and not only the fake reports 1,000,000.
	agent = &Agent{Model: "zai/glm-5.3-flash"}
	if got := agent.resolveContextWindow(context.Background(), zaiRouteProvider(t)); got != 1000000 {
		t.Fatalf("plan route window = %d, want 1000000", got)
	}
}

func TestResolveContextWindowFallsBackForUnpinnedRoutes(t *testing.T) {
	// An unpinned route with no discovery falls back to 128,000.
	agent := &Agent{Model: "deepseek-v4-flash"}
	if got := agent.resolveContextWindow(context.Background(), plainProvider{}); got != provider.FallbackContextWindow {
		t.Fatalf("unpinned window = %d, want %d", got, provider.FallbackContextWindow)
	}

	// An unpinned route with discovery still uses the discovered figure: the
	// pin path must not suppress the one that already worked.
	agent = &Agent{Model: "vendor/model"}
	if got := agent.resolveContextWindow(context.Background(), windowProvider{window: 262144}); got != 262144 {
		t.Fatalf("discovered window = %d, want 262144", got)
	}

	// A provider that carries a route but no pin falls back rather than
	// reporting a zero window as if it were authoritative.
	agent = &Agent{Model: "vendor/model"}
	if got := agent.resolveContextWindow(context.Background(), pinnedProvider{pin: 0}); got != provider.FallbackContextWindow {
		t.Fatalf("unpinned route window = %d, want %d", got, provider.FallbackContextWindow)
	}
}

func TestCompactionFiresAtSeventyPercentOfThePinnedWindow(t *testing.T) {
	agent := &Agent{Model: "zai/glm-5.3-flash"}
	window := agent.resolveContextWindow(context.Background(), zaiRouteProvider(t))

	// Roughly 120k estimated tokens: over the 89,600-token fallback budget
	// that governs this route today, and well under the pinned 700,000 one.
	// That gap is the whole point of the pin, so both budgets are asserted.
	history := make([]provider.Message, 96)
	for i := range history {
		history[i] = provider.Message{Role: "user", Content: strings.Repeat("x", 5000)}
	}
	if !contextLimitReached(provider.FallbackContextWindow, "system", history, nil, contextAnchor{}) {
		t.Fatal("history did not reach the fallback budget, so the pin is not what this test measures")
	}
	if contextLimitReached(window, "system", history, nil, contextAnchor{}) {
		t.Fatalf("compaction fired below 70%% of the pinned %d-token window", window)
	}
}
