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

func TestTruncatedStreamStillReportsItsUsage(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"type":"message_start","message":{"usage":{"input_tokens":120,"cache_read_input_tokens":880}}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}`,
		`data: {"type":"message_delta","delta":{},"usage":{"output_tokens":40}}`,
	}, "\n") + "\n"
	var usages []map[string]any
	err := anthropicMessagesWire{}.parseStream(strings.NewReader(stream), func(event Event) error {
		if event.Kind == EventUsage {
			usages = append(usages, event.Usage)
		}
		return nil
	})
	if err == nil {
		t.Fatal("a stream without message_stop parsed as complete")
	}
	if len(usages) != 1 || usages[0]["incomplete"] != true || usages[0]["prompt_tokens"] != 1000 || usages[0]["completion_tokens"] != 40 {
		t.Fatalf("usage = %v", usages)
	}
}
