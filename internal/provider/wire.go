package provider

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// wireStrategy is one endpoint protocol: how a request is addressed, encoded,
// decoded and how its failures are read.
//
// The split is transport plus strategy rather than payload-plus-parser,
// because header construction differs too. `Stream` used to hardcode
// `Authorization: Bearer`, and the Anthropic shape needs `x-api-key` with its
// own version header; a payload-and-parser-only seam could not express that,
// which is why the strategy owns headers as well.
type wireStrategy interface {
	// name is the policy `wire` value this strategy implements.
	name() string
	// applyHeaders sets auth and protocol headers. The API key may be empty,
	// which is the local route deliberately sending no credential.
	applyHeaders(header http.Header, apiKey string)
	// payload builds the exact bytes posted for a request. It is the single
	// source for both the HTTP body and the transcript record, so the two
	// cannot drift.
	payload(p *HTTPProvider, req Request) ([]byte, error)
	// requestFromPayload reconstructs a Request from a transcripted body, so
	// transcripts stay byte-replayable on every route rather than only on the
	// wire that happened to be implemented first.
	requestFromPayload(payload []byte) (Request, error)
	// parseStream turns a response body — SSE on two wires, NDJSON on
	// ollama-chat — into normalized Events. No stream dialect escapes this
	// package.
	parseStream(body io.Reader, sink StreamSink) error
	// classifyError turns a non-2xx response into an error carrying whatever
	// the envelope held.
	classifyError(status int, body []byte) error
	// decodeCatalog reads this protocol's model catalog document.
	decodeCatalog(data []byte) ([]modelMetadata, error)
}

// wireFor returns the strategy for a policy wire value. It reports false for a
// wire this build does not implement, which is what ResolveRoute turns into a
// refusal.
func wireFor(name string) (wireStrategy, bool) {
	switch name {
	case WireOpenAIChat:
		return openAIChatWire{}, true
	case WireAnthropicMessages:
		return anthropicMessagesWire{}, true
	case WireOllamaChat:
		return ollamaChatWire{}, true
	}
	return nil, false
}

// StatusError is a refusal the endpoint made before the stream opened.
//
// Every refusal these endpoints produced in the capability matrix was an HTTP
// status rather than an in-stream `error` frame, on both wires and in all
// three envelopes, so classification is a status-class decision and this type
// carries what the envelope held.
type StatusError struct {
	// Status is the HTTP status code.
	Status int
	// Code is the provider's own error code, such as Z.ai's 1211 or 1261.
	Code string
	// Message is the human-readable message from the envelope.
	Message string
	// RequestID is preserved because it is the only handle Z.ai gives for a
	// support question, and it appears in the Anthropic envelope alone.
	RequestID string
	// Wire names the protocol that produced it, so a two-wire deployment can
	// tell which half failed.
	Wire string
}

func (e *StatusError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "provider returned HTTP %d", e.Status)
	if e.Wire != "" {
		fmt.Fprintf(&b, " on the %s wire", e.Wire)
	}
	if e.Code != "" {
		fmt.Fprintf(&b, " (code %s)", e.Code)
	}
	if e.Message != "" {
		fmt.Fprintf(&b, ": %s", e.Message)
	}
	if e.RequestID != "" {
		fmt.Fprintf(&b, " [request_id %s]", e.RequestID)
	}
	return b.String()
}

// Retryable reports whether retrying could plausibly help.
//
// 4xx is not retried and 5xx is, which is the plan's rule and is stricter than
// the usual convention in one place worth naming: a 429 is a 4xx here and so
// is **not** retried. That is deliberate. On this fleet a quota refusal stops
// the work and is reported at once, and three silent retries would spend three
// times the quota before anyone heard about it.
func (e *StatusError) Retryable() bool { return e.Status >= 500 }

// retryable is the optional capability Retry consults. An error that does not
// implement it is retried, which keeps transport failures — the case retrying
// exists for — behaving as they always did.
type retryable interface{ Retryable() bool }

// openAIChatWire is the OpenAI-shaped chat-completions protocol: OpenRouter,
// DeepSeek, Cerebras and Z.ai's coding endpoint. The local Ollama server left it for
// ollama-chat on 2026-09-18, because Ollama's /v1 shim discards sampler
// options.
type openAIChatWire struct{}

func (openAIChatWire) name() string { return WireOpenAIChat }

func (openAIChatWire) applyHeaders(header http.Header, apiKey string) {
	if apiKey != "" {
		header.Set("Authorization", "Bearer "+apiKey)
	}
	header.Set("Content-Type", "application/json")
	header.Set("Accept", "text/event-stream")
}

func (openAIChatWire) payload(p *HTTPProvider, req Request) ([]byte, error) {
	effort, err := p.effortValue(req.Effort)
	if err != nil {
		return nil, err
	}
	body := wireRequest{
		Model:            p.modelID(req.Model),
		Stream:           true,
		Messages:         messagesFor(p.Flavor, req),
		ReasoningEffort:  effort,
		IncludeReasoning: effort != "" && sendsReasoningFields(p.Flavor),
		PromptCacheKey:   req.CacheKey,
		MaxTokens:        p.maxOutputTokens(req, DefaultMaxOutputTokens),
		Temperature:      req.Temperature,
		StreamOptions:    streamOptions{IncludeUsage: true},
		Provider:         providerObjectFor(p.Route),
	}
	if len(req.Tools) > 0 {
		body.Tools = make([]wireTool, 0, len(req.Tools))
		for _, tool := range req.Tools {
			body.Tools = append(body.Tools, wireTool{
				Type: "function",
				Function: wireToolFunction{
					Name:        tool.Name,
					Description: tool.Description,
					Parameters:  tool.Parameters,
				},
			})
		}
	}
	return json.Marshal(body)
}

// sendsReasoningFields reports whether a flavor's endpoint accepts the two
// reasoning round-trip fields this wire otherwise carries: OpenRouter's
// `include_reasoning` opt-in on the way out, and `reasoning_content` on an
// assistant turn replayed on the way back in.
//
// It is a per-flavor fact rather than a per-wire one, because one wire carries
// several vendors' idea of the OpenAI shape. Cerebras refuses both outright,
// measured 2026-09-20 against qwen-3.8-27b:
//
//	include_reasoning: property 'include_reasoning' is unsupported
//	messages.2.assistant.reasoning_content: property '...' is unsupported
//
// Both are HTTP 400 before a token streams, and the second one fires only on
// the second turn of a tool round trip — the first turn of a conversation
// carries no assistant message — so it is the sort of failure that reaches
// production looking like a tool bug.
//
// Nothing is lost on the way out: Cerebras streams `delta.reasoning` unasked,
// which parseSSE already reads beside DeepSeek's `reasoning_content`. What is
// given up is the replay — the model does not see its own earlier thinking on
// a later turn, because this endpoint has nowhere to put it. That is this
// endpoint's design rather than slbh's choice, and the alternative is not
// having the route.
//
// Omitting rather than tolerating is scoped deliberately: the fields are
// dropped only where they are known to be refused, so a vendor that starts
// honouring them is a one-line change here and not a silent loss of reasoning
// on every route.
func sendsReasoningFields(flavor string) bool {
	return flavor != "cerebras"
}

// messagesFor renders the conversation this flavor's endpoint will accept,
// with the system turn ahead of it. It copies rather than editing req.Messages
// in place: the caller's history is the harness's own, replayed on every turn
// and transcripted, and stripping a field from it here would delete the
// reasoning from the record as well as from the request.
func messagesFor(flavor string, req Request) []Message {
	messages := append([]Message{{Role: "system", Content: req.System}}, req.Messages...)
	if sendsReasoningFields(flavor) {
		return messages
	}
	stripped := make([]Message, len(messages))
	for i, message := range messages {
		message.ReasoningContent = ""
		stripped[i] = message
	}
	return stripped
}

func (openAIChatWire) requestFromPayload(payload []byte) (Request, error) {
	var body wireRequest
	if err := json.Unmarshal(payload, &body); err != nil {
		return Request{}, fmt.Errorf("decode request payload: %w", err)
	}
	if len(body.Messages) == 0 || body.Messages[0].Role != "system" {
		return Request{}, fmt.Errorf("request payload has no leading system message")
	}
	request := Request{
		Model:       body.Model,
		Effort:      body.ReasoningEffort,
		System:      body.Messages[0].Content,
		Messages:    append([]Message(nil), body.Messages[1:]...),
		CacheKey:    body.PromptCacheKey,
		Temperature: body.Temperature,
	}
	if len(body.Tools) > 0 {
		request.Tools = make([]Tool, 0, len(body.Tools))
		for _, tool := range body.Tools {
			request.Tools = append(request.Tools, Tool{
				Name:        tool.Function.Name,
				Description: tool.Function.Description,
				Parameters:  tool.Function.Parameters,
			})
		}
	}
	return request, nil
}

func (openAIChatWire) parseStream(body io.Reader, sink StreamSink) error {
	return parseSSE(body, sink)
}

// classifyError reads the coding wire's envelope, `{"error":{"code",
// "message"}}`. A body that is not that shape still produces a StatusError, so
// the status class still governs retry.
func (openAIChatWire) classifyError(status int, body []byte) error {
	err := &StatusError{Status: status, Wire: WireOpenAIChat}
	var envelope struct {
		Error struct {
			Code    json.RawMessage `json:"code"`
			Message string          `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &envelope) == nil {
		err.Code = rawString(envelope.Error.Code)
		err.Message = envelope.Error.Message
	}
	if err.Message == "" {
		err.Message = strings.TrimSpace(string(body))
	}
	return err
}

func (openAIChatWire) decodeCatalog(data []byte) ([]modelMetadata, error) {
	var payload struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("decode model catalog: %w", err)
	}
	return decodeModels(payload.Data)
}

// rawString renders a JSON value that may be a string or a number as a string.
// Z.ai's error codes arrive as strings ("1211") in some envelopes and as bare
// numbers in others, and a classifier that assumed one would silently lose the
// code in the other.
func rawString(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return ""
	}
	var asString string
	if json.Unmarshal(raw, &asString) == nil {
		return asString
	}
	return strings.Trim(trimmed, `"`)
}
