//go:build live_integration

package provider

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// The live counterpart of ollama_test.go: the committed managed route, sent to
// the real server. Gated behind the tag and SLBH_LIVE_OLLAMA=1 because it holds
// the shared GPU; every generation is bounded small. SLBH_LOCAL_ENDPOINT still
// overrides the endpoint.

type liveResult struct {
	text, thinking string
	tools          []Event
	stop           string
	usage          map[string]any
}

func liveOllama(t *testing.T, route RoutePolicy, req Request) liveResult {
	t.Helper()
	policy := Policy{Version: PolicyVersion, Routes: map[string]RoutePolicy{LocalModelID: route}}
	resolved, err := ForModel(LocalModelID, "", false, policy)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
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

func liveRoute(t *testing.T) RoutePolicy {
	if os.Getenv("SLBH_LIVE_OLLAMA") != "1" {
		t.Skip("set SLBH_LIVE_OLLAMA=1 to run against the real Ollama server")
	}
	route, ok := committedManagedPolicy(t).Routes[LocalModelID]
	if !ok {
		t.Fatal("the managed artifact has no local route")
	}
	return route
}

func bound(n int) *int { return &n }

func TestLiveOllamaReplyStops(t *testing.T) {
	route := liveRoute(t)
	result := liveOllama(t, route, Request{
		Model: LocalModelID, Effort: "medium", MaxTokens: bound(2048),
		Messages: []Message{{Role: "user", Content: "Reply with exactly SLBH_OLLAMA_OK and nothing else."}},
	})
	t.Logf("stop=%q usage=%v text=%q thinking=%d chars", result.stop, result.usage, result.text, len(result.thinking))
	if result.stop != "stop" || !strings.Contains(result.text, "SLBH_OLLAMA_OK") {
		t.Fatalf("reply did not complete: stop=%q text=%q", result.stop, result.text)
	}
}

func TestLiveOllamaLengthStop(t *testing.T) {
	route := liveRoute(t)
	result := liveOllama(t, route, Request{
		Model: LocalModelID, Effort: "medium", MaxTokens: bound(16),
		Messages: []Message{{Role: "user", Content: "Write a long essay about rivers."}},
	})
	t.Logf("stop=%q usage=%v", result.stop, result.usage)
	if result.stop != "length" || !IsOutputLimitStop(result.stop) {
		t.Fatalf("a 16-token bound ended %q, want length", result.stop)
	}
	if tokens, _ := result.usage["completion_tokens"].(int); tokens != 16 {
		t.Fatalf("completion_tokens = %v, want 16", result.usage["completion_tokens"])
	}
}

func TestLiveOllamaToolRoundTrip(t *testing.T) {
	route := liveRoute(t)
	tools := []Tool{{Name: "get_weather", Description: "Get the current weather for a city.", Parameters: map[string]any{
		"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}}, "required": []string{"city"},
	}}}
	req := Request{
		Model: LocalModelID, Effort: "low", MaxTokens: bound(2048), Tools: tools,
		System:   "Use tools when asked. Be brief.",
		Messages: []Message{{Role: "user", Content: "Call get_weather for Paris now."}},
	}
	first := liveOllama(t, route, req)
	t.Logf("first: stop=%q tools=%+v usage=%v", first.stop, first.tools, first.usage)
	if first.stop != "tool_calls" || len(first.tools) != 1 || first.tools[0].ToolName != "get_weather" {
		t.Fatalf("no tool call: stop=%q tools=%+v text=%q", first.stop, first.tools, first.text)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(first.tools[0].Input), &args); err != nil || !strings.EqualFold(args["city"].(string), "Paris") {
		t.Fatalf("tool arguments %q: %v", first.tools[0].Input, err)
	}
	call := ToolCall{ID: first.tools[0].ToolCallID, Type: "function"}
	call.Function.Name, call.Function.Arguments = first.tools[0].ToolName, first.tools[0].Input
	req.Messages = append(req.Messages,
		Message{Role: "assistant", ReasoningContent: first.thinking, Content: first.text, ToolCalls: []ToolCall{call}},
		Message{Role: "tool", ToolCallID: call.ID, Name: call.Function.Name, Content: "Sunny, 21C, station code WX-7731"},
	)
	second := liveOllama(t, route, req)
	t.Logf("second: stop=%q text=%q usage=%v", second.stop, second.text, second.usage)
	if second.stop != "stop" || !strings.Contains(second.text, "WX-7731") {
		t.Fatalf("the tool result did not reach the answer: stop=%q text=%q", second.stop, second.text)
	}
}

// TestLiveOllamaOptionsApply is the sampler A/B v8-phase0 used, through this
// wire: with a fixed seed the output is deterministic, so identical options
// must reproduce it byte for byte, and changing only presence_penalty — the
// guard this route carries, and a field /v1 would have kept — must change it.
func TestLiveOllamaOptionsApply(t *testing.T) {
	route := liveRoute(t)
	req := Request{
		Model: LocalModelID, Effort: "medium", MaxTokens: bound(60),
		Messages: []Message{{Role: "user", Content: "Name twelve unusual animals, comma separated."}},
	}
	withSeed := func(presence string) RoutePolicy {
		variant := route
		variant.Options = map[string]json.RawMessage{}
		for key, value := range route.Options {
			variant.Options[key] = value
		}
		variant.Options["seed"] = json.RawMessage(`7`)
		variant.Options["presence_penalty"] = json.RawMessage(presence)
		return variant
	}
	sample := func(r liveResult) string { return r.thinking + "|" + r.text }
	guarded := sample(liveOllama(t, withSeed(`1.5`), req))
	again := sample(liveOllama(t, withSeed(`1.5`), req))
	unguarded := sample(liveOllama(t, withSeed(`0`), req))
	t.Logf("guarded:   %q", guarded)
	t.Logf("unguarded: %q", unguarded)
	if guarded != again {
		t.Fatal("the seeded sample is not deterministic, so this A/B proves nothing")
	}
	if guarded == unguarded {
		t.Fatal("presence_penalty changed nothing: the option did not reach the sampler")
	}
}
