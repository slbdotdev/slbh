package harness

import (
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
	prompt := systemPrompt(r.Root())
	for _, phrase := range []string{
		"launch_subagent returns immediately",
		"do not block this turn waiting for a child",
		"Do not use quick_bash, long_job, sleep, polling, or shell wait loops",
		"end your turn",
		"later [result from ...] message",
		"responsible for ending each subagent with end_subagent",
		"subagents stay alive indefinitely",
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
