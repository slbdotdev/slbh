//go:build live_integration

package provider

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// The live counterpart of cerebras_test.go: the committed managed route, sent
// to the real endpoint. Gated behind the tag and SLBH_LIVE_CEREBRAS=1 because
// it spends the account's tokens-per-minute budget — 150,000 on this key, which
// one careless long-context probe exhausts — so every generation here is
// bounded small.

func liveCerebras(t *testing.T, req Request) liveResult {
	t.Helper()
	if os.Getenv("SLBH_LIVE_CEREBRAS") != "1" {
		t.Skip("set SLBH_LIVE_CEREBRAS=1 to run against the real Cerebras endpoint")
	}
	if strings.TrimSpace(os.Getenv("CEREBRAS_API_KEY")) == "" {
		t.Skip("CEREBRAS_API_KEY is not set")
	}
	const model = "cerebras/qwen-3.8-27b"
	policy := committedManagedPolicy(t)
	if _, ok := policy.Routes[model]; !ok {
		t.Fatal("the managed artifact has no Cerebras route")
	}
	resolved, err := ForModel(model, "", false, policy)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var result liveResult
	var text, thinking strings.Builder
	calls := map[int]*Event{}
	err = resolved.Stream(ctx, req, func(event Event) error {
		switch event.Kind {
		case EventText:
			text.WriteString(event.Text)
		case EventReasoning:
			thinking.WriteString(event.Text)
		case EventTool:
			copied := event
			calls[event.ToolIndex] = &copied
		case EventUsage:
			result.stop, result.usage = event.StopReason, event.Usage
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < len(calls); index++ {
		result.tools = append(result.tools, *calls[index])
	}
	result.text, result.thinking = text.String(), thinking.String()
	return result
}

// TestLiveCerebrasStreamsReasoningWithoutTheOptIn is the whole reason the
// flavor exists as its own arm. `include_reasoning` is refused by this
// endpoint, so the request goes without it — and the reasoning must arrive
// anyway, or dropping the field would have cost the thinking stream silently.
func TestLiveCerebrasStreamsReasoningWithoutTheOptIn(t *testing.T) {
	result := liveCerebras(t, Request{
		Model: "cerebras/qwen-3.8-27b", Effort: "medium", MaxTokens: bound(512),
		System:   "Be brief.",
		Messages: []Message{{Role: "user", Content: "Reply with exactly SLBH_CEREBRAS_OK and nothing else."}},
	})
	t.Logf("stop=%q usage=%v text=%q thinking=%d chars", result.stop, result.usage, result.text, len(result.thinking))
	if result.stop != "stop" || !strings.Contains(result.text, "SLBH_CEREBRAS_OK") {
		t.Fatalf("reply did not complete: stop=%q text=%q", result.stop, result.text)
	}
	if result.thinking == "" {
		t.Fatal("no reasoning arrived: the endpoint streams delta.reasoning unasked, so an empty one means the stream was not read")
	}
}

// TestLiveCerebrasRefusesAnUnsupportedEffort proves the refusal is slbh's and
// lands before the request: the endpoint's own answer to `max` is an HTTP 400,
// and a route that reached it would have spent a round trip to learn what the
// effort map already knows.
func TestLiveCerebrasRefusesAnUnsupportedEffort(t *testing.T) {
	if os.Getenv("SLBH_LIVE_CEREBRAS") != "1" {
		t.Skip("set SLBH_LIVE_CEREBRAS=1 to run against the real Cerebras endpoint")
	}
	if strings.TrimSpace(os.Getenv("CEREBRAS_API_KEY")) == "" {
		t.Skip("CEREBRAS_API_KEY is not set")
	}
	resolved, err := ForModel("cerebras/qwen-3.8-27b", "", false, committedManagedPolicy(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err = resolved.Stream(ctx, Request{
		Model: "cerebras/qwen-3.8-27b", Effort: "max", MaxTokens: bound(16),
		Messages: []Message{{Role: "user", Content: "hi"}},
	}, func(Event) error { return nil })
	if err == nil {
		t.Fatal("effort max reached the endpoint instead of being refused")
	}
	if !strings.Contains(err.Error(), "does not support effort") {
		t.Fatalf("the endpoint refused this, not slbh: %v", err)
	}
}

// TestLiveCerebrasToolRoundTrip is the capability the Seat needs before this
// route can carry an agent rather than a chat: a tool call out, a tool result
// back, and the result reaching the answer.
func TestLiveCerebrasToolRoundTrip(t *testing.T) {
	tools := []Tool{{Name: "get_weather", Description: "Get the current weather for a city.", Parameters: map[string]any{
		"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}}, "required": []string{"city"},
	}}}
	req := Request{
		Model: "cerebras/qwen-3.8-27b", Effort: "low", MaxTokens: bound(512), Tools: tools,
		System:   "Use tools when asked. Be brief.",
		Messages: []Message{{Role: "user", Content: "Call get_weather for Paris now."}},
	}
	first := liveCerebras(t, req)
	t.Logf("first: stop=%q tools=%+v usage=%v", first.stop, first.tools, first.usage)
	if first.stop != "tool_calls" || len(first.tools) != 1 || first.tools[0].ToolName != "get_weather" {
		t.Fatalf("no tool call: stop=%q tools=%+v text=%q", first.stop, first.tools, first.text)
	}
	call := ToolCall{ID: first.tools[0].ToolCallID, Type: "function"}
	call.Function.Name, call.Function.Arguments = first.tools[0].ToolName, first.tools[0].Input
	req.Messages = append(req.Messages,
		Message{Role: "assistant", ReasoningContent: first.thinking, Content: first.text, ToolCalls: []ToolCall{call}},
		Message{Role: "tool", ToolCallID: call.ID, Name: call.Function.Name, Content: "Sunny, 21C, station code WX-7731"},
	)
	second := liveCerebras(t, req)
	t.Logf("second: stop=%q text=%q usage=%v", second.stop, second.text, second.usage)
	if second.stop != "stop" || !strings.Contains(second.text, "WX-7731") {
		t.Fatalf("the tool result did not reach the answer: stop=%q text=%q", second.stop, second.text)
	}
}
