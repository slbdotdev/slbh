package provider

import (
	"errors"
	"fmt"
	"testing"
)

// The recorded refusals, parsed by each wire's own classifier, are overflows;
// other refusals of the same status are not.
func TestIsContextOverflow(t *testing.T) {
	overflow := map[string]error{
		"ninfer-serve":    openAIChatWire{}.classifyError(400, []byte(`{"error":{"code":"context_length_exceeded","message":"prepared prompt exceeds Engine max_context 196608","param":"messages","type":"invalid_request_error"}}`)),
		"z.ai coding":     openAIChatWire{}.classifyError(400, []byte(`{"error":{"code":"1261","message":"Prompt exceeds max length"}}`)),
		"z.ai anthropic":  anthropicMessagesWire{}.classifyError(400, []byte(`{"type":"error","error":{"type":"invalid_request_error","code":"1261","message":"prompt is too long"}}`)),
		"openai wording":  openAIChatWire{}.classifyError(400, []byte(`{"error":{"message":"This model's maximum context length is 131072 tokens. However, you requested 140000 tokens."}}`)),
		"llama.cpp":       openAIChatWire{}.classifyError(400, []byte(`{"error":{"message":"the request exceeds the available context size, try increasing it"}}`)),
		"ollama":          &StatusError{Status: 400, Message: "the input length exceeds the context length"},
		"payload too big": &StatusError{Status: 413, Message: "Request Entity Too Large"},
		"wrapped":         fmt.Errorf("turn: %w", &StatusError{Status: 400, Code: "context_length_exceeded"}),
	}
	for name, err := range overflow {
		if !IsContextOverflow(err) {
			t.Errorf("%s: %v not recognised as a context overflow", name, err)
		}
	}
	other := map[string]error{
		"unknown model":       openAIChatWire{}.classifyError(400, []byte(`{"error":{"code":"1211","message":"Unknown Model, please check the model code."}}`)),
		"quota":               &StatusError{Status: 429, Message: "rate limited"},
		"server":              &StatusError{Status: 500, Message: "context length computation failed"},
		"transport":           errors.New("connection reset"),
		"mentions the window": openAIChatWire{}.classifyError(400, []byte(`{"error":{"message":"context window selection is unsupported"}}`)),
		"bad parameter":       openAIChatWire{}.classifyError(400, []byte(`{"error":{"message":"invalid context length parameter"}}`)),
		"token rate limit":    &StatusError{Status: 429, Message: "too many tokens per minute"},
	}
	for name, err := range other {
		if IsContextOverflow(err) {
			t.Errorf("%s: %v taken for a context overflow", name, err)
		}
	}
}
