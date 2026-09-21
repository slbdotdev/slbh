package provider

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestToolResultIsErrorReachesOnlyTheAnthropicWire(t *testing.T) {
	message := Message{Role: "tool", ToolCallID: "toolu_1", Content: "Exit code 3\nout", IsError: true}
	anthropic, err := json.Marshal(anthropicMessages([]Message{message}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(anthropic), `"is_error":true`) {
		t.Fatalf("anthropic wire lacks is_error: %s", anthropic)
	}
	openai, err := json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(openai), "is_error") {
		t.Fatalf("OpenAI-shaped message carries is_error: %s", openai)
	}
	plain, _ := json.Marshal(anthropicMessages([]Message{{Role: "tool", ToolCallID: "toolu_2", Content: "ok"}}))
	if strings.Contains(string(plain), "is_error") {
		t.Fatalf("a successful result carries is_error: %s", plain)
	}
}
