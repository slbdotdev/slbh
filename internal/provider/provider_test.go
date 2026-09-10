package provider

import (
	"strings"
	"testing"
)

func TestStablePrefixKeyIgnoresUserTurns(t *testing.T) {
	base := Request{Model: "deepseek/deepseek-chat", System: "stable system", Tools: []Tool{{Name: "read_file", Parameters: map[string]any{"type": "object"}}}}
	first := base
	first.Messages = []Message{{Role: "user", Content: "one"}}
	second := base
	second.Messages = []Message{{Role: "user", Content: "two"}}
	if StablePrefixKey(first) != StablePrefixKey(second) {
		t.Fatal("user turns must not change the stable prefix key")
	}
	changed := base
	changed.System = "different system"
	if StablePrefixKey(first) == StablePrefixKey(changed) {
		t.Fatal("system prefix changes must change the cache key")
	}
}

func TestNormalizeDeepSeekModel(t *testing.T) {
	if got := NormalizeModel("deepseek/deepseek-v4.1-flash"); got != "deepseek-v4-flash" {
		t.Fatalf("got %q", got)
	}
	if got := (&HTTPProvider{Flavor: "deepseek"}).modelID("deepseek-v4-flash"); got != "deepseek-v4-flash" {
		t.Fatalf("native model id got %q", got)
	}
	if got := (&HTTPProvider{Flavor: "openrouter"}).modelID("deepseek-v4-flash"); got != "deepseek/deepseek-v4-flash" {
		t.Fatalf("OpenRouter model id got %q", got)
	}
}

func TestParseSSE(t *testing.T) {
	data := strings.NewReader("data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"think\",\"content\":\"hello\",\"tool_calls\":[{\"id\":\"c1\",\"function\":{\"name\":\"glob\",\"arguments\":\"{}\"}}]}}]}\n\ndata: {\"usage\":{\"total_tokens\":4}}\n\ndata: [DONE]\n")
	var got []Event
	if err := parseSSE(data, func(event Event) error { got = append(got, event); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 || got[0].Kind != EventText || got[1].Kind != EventReasoning || got[2].Kind != EventTool || got[3].Kind != EventUsage {
		t.Fatalf("unexpected events: %#v", got)
	}
}
