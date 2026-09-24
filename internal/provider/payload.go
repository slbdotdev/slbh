package provider

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// RequestPayload returns the exact request body used by Stream. It is kept as
// one function so transcript records and the actual HTTP request cannot drift.
func (p *HTTPProvider) RequestPayload(req Request) ([]byte, error) {
	if req.CacheKey == "" {
		req.CacheKey = StablePrefixKey(req)
	}
	return p.strategy().payload(p, req)
}

// RequestFromPayload reconstructs the provider request context from a
// transcripted wire payload, detecting which wire wrote it.
//
// Detection is by a field only one shape has, not by guessing: the OpenAI body
// always carries `stream_options` and the Anthropic body always carries
// `max_tokens`, and the Ollama body always carries `options`, because each is
// set unconditionally by its own payload builder. A transcript that carries neither is tried on the OpenAI wire
// first, which is what every record written before phase 2 is.
func RequestFromPayload(payload []byte) (Request, error) {
	var shape struct {
		StreamOptions *json.RawMessage `json:"stream_options"`
		MaxTokens     *int             `json:"max_tokens"`
		Options       *json.RawMessage `json:"options"`
	}
	if err := json.Unmarshal(payload, &shape); err != nil {
		return Request{}, fmt.Errorf("decode request payload: %w", err)
	}
	switch {
	case shape.StreamOptions != nil:
		return openAIChatWire{}.requestFromPayload(payload)
	case shape.MaxTokens != nil:
		return anthropicMessagesWire{}.requestFromPayload(payload)
	case shape.Options != nil:
		return ollamaChatWire{}.requestFromPayload(payload)
	}
	return openAIChatWire{}.requestFromPayload(payload)
}

// RequestFromPayloadOnWire decodes a transcripted payload on a named wire, for
// a caller that already knows which route produced it and should not depend on
// detection.
func RequestFromPayloadOnWire(wireName string, payload []byte) (Request, error) {
	strategy, ok := wireFor(wireName)
	if !ok {
		return Request{}, fmt.Errorf("no implementation for wire %q", wireName)
	}
	return strategy.requestFromPayload(payload)
}

// ContextPayload serializes the provider-independent context that is sent to
// inference. HTTPProvider records the fuller wire payload, while other
// providers can still leave a replayable context record in the transcript.
func ContextPayload(req Request) ([]byte, error) {
	return json.Marshal(struct {
		System   string    `json:"system"`
		Messages []Message `json:"messages"`
		Tools    []Tool    `json:"tools"`
	}{System: req.System, Messages: req.Messages, Tools: req.Tools})
}

func PayloadSHA256(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}
