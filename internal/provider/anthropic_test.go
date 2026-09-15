package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// anthropicPolicy is the managed fleet shape: the plan route on the Anthropic
// wire with output_config.effort and the measured pin.
func anthropicPolicy() Policy {
	return Policy{Version: PolicyVersion, Routes: map[string]RoutePolicy{
		"zai/glm-5.3-flash": {
			Endpoint:        "https://api.z.ai/api/anthropic/v1/messages",
			Wire:            WireAnthropicMessages,
			CatalogEndpoint: "https://api.z.ai/api/anthropic/v1/models",
			ContextWindow:   1000000,
			Effort:          EffortDescriptor{Field: "output_config.effort", Levels: identityLevels()},
		},
	}}
}

// sampleRequest is the one Request both wires are asked to encode, so the two
// payloads are compared against a single input rather than two.
func sampleRequest() Request {
	temperature := 0.0
	return Request{
		Model:  "zai/glm-5.3-flash",
		Effort: "high",
		System: "you are terse",
		Messages: []Message{
			{Role: "user", Content: "weather in Denver?"},
			{Role: "assistant", Content: "checking", ReasoningContent: "need the tool", ToolCalls: []ToolCall{{
				ID: "call_abc", Type: "function",
				Function: struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				}{Name: "get_weather", Arguments: `{"city":"Denver","unit":"c"}`},
			}}},
			{Role: "tool", ToolCallID: "call_abc", Content: `{"tempC":7}`},
		},
		Tools:       []Tool{{Name: "get_weather", Description: "weather", Parameters: map[string]any{"type": "object"}}},
		CacheKey:    "slbh-test-cache",
		Temperature: &temperature,
	}
}

func TestBothWiresEncodeOneRequest(t *testing.T) {
	t.Setenv("ZAI_API_KEY", "test-key-not-a-credential")
	req := sampleRequest()

	anthropic := forModelHTTP(t, "zai/glm-5.3-flash", OpenRouterEndpoint, false, anthropicPolicy())
	if anthropic.Wire() != WireAnthropicMessages {
		t.Fatalf("wire = %q, want %q", anthropic.Wire(), WireAnthropicMessages)
	}
	anthropicBody, err := anthropic.RequestPayload(req)
	if err != nil {
		t.Fatal(err)
	}
	coding := forModelHTTP(t, "zai/glm-5.3-flash", OpenRouterEndpoint, false, testPolicy())
	codingBody, err := coding.RequestPayload(req)
	if err != nil {
		t.Fatal(err)
	}

	var anthropicJSON, codingJSON map[string]any
	if err := json.Unmarshal(anthropicBody, &anthropicJSON); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(codingBody, &codingJSON); err != nil {
		t.Fatal(err)
	}

	// The slug is the bare one on both wires, unsuffixed. The `[1m]` spellings
	// Z.ai's own guide publishes are rejected 1211 on both.
	for name, body := range map[string]map[string]any{"anthropic": anthropicJSON, "coding": codingJSON} {
		if body["model"] != "glm-5.3-flash" {
			t.Fatalf("%s wire model = %v, want glm-5.3-flash", name, body["model"])
		}
	}

	// Effort: output_config.effort on one wire, reasoning_effort on the other,
	// and each wire carries only its own spelling.
	outputConfig, ok := anthropicJSON["output_config"].(map[string]any)
	if !ok || outputConfig["effort"] != "high" {
		t.Fatalf("anthropic effort = %v, want output_config.effort=high", anthropicJSON["output_config"])
	}
	if _, present := anthropicJSON["reasoning_effort"]; present {
		t.Fatal("anthropic payload carries a bare reasoning_effort, which is measured inert on that endpoint")
	}
	if _, present := anthropicJSON["thinking"]; present {
		t.Fatal("anthropic payload carries a thinking object, which is measured inert on that endpoint")
	}
	if codingJSON["reasoning_effort"] != "high" {
		t.Fatalf("coding effort = %v, want reasoning_effort=high", codingJSON["reasoning_effort"])
	}
	if _, present := codingJSON["output_config"]; present {
		t.Fatal("coding payload carries output_config, which that wire has no equivalent for")
	}

	// No cache_control breakpoint is sent, and none is needed. The cache on
	// this plan is implicit, content-addressed and shared between the two
	// endpoints: 5,504 of 5,551 prompt tokens hit with no breakpoint set at
	// all, on a prefix warmed by the *other* wire. A breakpoint is permitted
	// but is not the mechanism, and must not be credited with the win.
	if strings.Contains(string(anthropicBody), "cache_control") {
		t.Fatal("the payload sets a cache_control breakpoint; caching here is implicit and content-addressed")
	}

	// The system prompt is a top-level block array on the Anthropic wire and a
	// leading system message on the other.
	if _, ok := anthropicJSON["system"].([]any); !ok {
		t.Fatalf("anthropic system = %T, want a block array", anthropicJSON["system"])
	}
	codingMessages, _ := codingJSON["messages"].([]any)
	if len(codingMessages) == 0 {
		t.Fatal("coding payload has no messages")
	}
	if first, _ := codingMessages[0].(map[string]any); first["role"] != "system" {
		t.Fatalf("coding payload does not lead with a system message: %v", codingMessages[0])
	}
}

func TestAnthropicToolRoundTripUsesContentBlocks(t *testing.T) {
	t.Setenv("ZAI_API_KEY", "test-key-not-a-credential")
	p := forModelHTTP(t, "zai/glm-5.3-flash", OpenRouterEndpoint, false, anthropicPolicy())
	body, err := p.RequestPayload(sampleRequest())
	if err != nil {
		t.Fatal(err)
	}
	var decoded anthropicWireRequest
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Messages) != 3 {
		t.Fatalf("message count = %d, want 3: %#v", len(decoded.Messages), decoded.Messages)
	}

	// The assistant turn becomes thinking + text + tool_use blocks.
	assistant := decoded.Messages[1]
	if assistant.Role != "assistant" {
		t.Fatalf("second message role = %q", assistant.Role)
	}
	types := make([]string, 0, len(assistant.Content))
	for _, block := range assistant.Content {
		types = append(types, block.Type)
	}
	if !reflect.DeepEqual(types, []string{"thinking", "text", "tool_use"}) {
		t.Fatalf("assistant block types = %v, want thinking,text,tool_use", types)
	}
	toolUse := assistant.Content[2]
	if toolUse.ID != "call_abc" || toolUse.Name != "get_weather" {
		t.Fatalf("tool_use block = %#v", toolUse)
	}
	if string(toolUse.Input) != `{"city":"Denver","unit":"c"}` {
		t.Fatalf("tool_use input = %s", toolUse.Input)
	}

	// The tool result becomes a user message carrying a tool_result block,
	// which is how this API continues a round trip. A `tool` role would be
	// rejected outright.
	result := decoded.Messages[2]
	if result.Role != "user" || len(result.Content) != 1 || result.Content[0].Type != "tool_result" {
		t.Fatalf("tool result message = %#v", result)
	}
	if result.Content[0].ToolUseID != "call_abc" || result.Content[0].Content != `{"tempC":7}` {
		t.Fatalf("tool_result block = %#v", result.Content[0])
	}

	// Tools use input_schema, not function.parameters.
	if len(decoded.Tools) != 1 || decoded.Tools[0].Name != "get_weather" || decoded.Tools[0].InputSchema == nil {
		t.Fatalf("tools = %#v", decoded.Tools)
	}
}

func TestAnthropicTranscriptRoundTripsExactly(t *testing.T) {
	// Byte-replayable transcripts on both wires, settled 2026-09-13. The plan
	// path is the route the campaigns run on, which is where replay is most
	// likely to be needed.
	t.Setenv("ZAI_API_KEY", "test-key-not-a-credential")
	p := forModelHTTP(t, "zai/glm-5.3-flash", OpenRouterEndpoint, false, anthropicPolicy())
	req := sampleRequest()
	payload, err := p.RequestPayload(req)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := RequestFromPayload(payload)
	if err != nil {
		t.Fatal(err)
	}
	// The model comes back as the wire slug rather than the route key, which
	// is what the coding wire's round trip does too.
	decoded.Model = req.Model
	if !reflect.DeepEqual(req, decoded) {
		t.Fatalf("decoded request differs:\nwant %#v\n got %#v", req, decoded)
	}
	reencoded, err := p.RequestPayload(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if string(payload) != string(reencoded) {
		t.Fatalf("payload changed across a transcript round trip:\n%s\n%s", payload, reencoded)
	}
}

func TestRequestFromPayloadDetectsTheWire(t *testing.T) {
	t.Setenv("ZAI_API_KEY", "test-key-not-a-credential")
	anthropic := forModelHTTP(t, "zai/glm-5.3-flash", OpenRouterEndpoint, false, anthropicPolicy())
	coding := forModelHTTP(t, "zai/glm-5.3-flash", OpenRouterEndpoint, false, testPolicy())

	anthropicBody, err := anthropic.RequestPayload(sampleRequest())
	if err != nil {
		t.Fatal(err)
	}
	codingBody, err := coding.RequestPayload(sampleRequest())
	if err != nil {
		t.Fatal(err)
	}
	// Detection is by a field only one shape has, so each decodes to a request
	// whose system prompt survived — the thing a wrong decoder would lose.
	for name, payload := range map[string][]byte{"anthropic": anthropicBody, "coding": codingBody} {
		decoded, err := RequestFromPayload(payload)
		if err != nil {
			t.Fatalf("%s payload: %v", name, err)
		}
		if decoded.System != "you are terse" {
			t.Fatalf("%s payload decoded system = %q", name, decoded.System)
		}
		if len(decoded.Messages) != 3 {
			t.Fatalf("%s payload decoded %d messages, want 3", name, len(decoded.Messages))
		}
	}
}

// anthropicStreamBody is a realistic Messages stream: a thinking block first,
// then two tool_use blocks whose arguments arrive fragmented, with usage split
// across message_start and message_delta.
const anthropicStreamBody = `event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":46,"cache_read_input_tokens":5504}}}

event: ping
data: {"type":"ping"}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"weighing it"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"call_one","name":"get_weather"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"Denver\"}"}}

event: content_block_start
data: {"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"call_two","name":"get_time"}}

event: content_block_delta
data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"city\":\"Osaka\"}"}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":83}}

event: message_stop
data: {"type":"message_stop"}
`

func collectEvents(t *testing.T, strategy wireStrategy, body string) []Event {
	t.Helper()
	var events []Event
	if err := strategy.parseStream(strings.NewReader(body), func(event Event) error {
		events = append(events, event)
		return nil
	}); err != nil {
		t.Fatalf("parse stream: %v", err)
	}
	return events
}

func TestAnthropicStreamNormalization(t *testing.T) {
	events := collectEvents(t, anthropicMessagesWire{}, anthropicStreamBody)

	// `ping` carries nothing and must never become content.
	for _, event := range events {
		if event.Kind == EventText && event.Text == "" {
			t.Fatal("an empty text event was emitted; ping must not be treated as content")
		}
	}

	var reasoning strings.Builder
	arguments := map[int]string{}
	names := map[int]string{}
	ids := map[int]string{}
	var usage []Event
	for _, event := range events {
		switch event.Kind {
		case EventReasoning:
			reasoning.WriteString(event.Text)
		case EventTool:
			arguments[event.ToolIndex] += event.Input
			if event.ToolName != "" {
				names[event.ToolIndex] = event.ToolName
			}
			if event.ToolCallID != "" {
				ids[event.ToolIndex] = event.ToolCallID
			}
		case EventUsage:
			usage = append(usage, event)
		}
	}

	if reasoning.String() != "weighing it" {
		t.Fatalf("reasoning = %q", reasoning.String())
	}

	// Index normalization: the wire numbered these blocks 1 and 2, behind the
	// thinking block at 0. They must leave the package as a dense tool ordinal
	// from 0, because nothing downstream may treat the wire index as an array
	// position.
	if len(arguments) != 2 {
		t.Fatalf("tool count = %d, want 2: %#v", len(arguments), arguments)
	}
	if _, ok := arguments[0]; !ok {
		t.Fatalf("tools are not renumbered from 0: %#v", arguments)
	}
	if names[0] != "get_weather" || ids[0] != "call_one" {
		t.Fatalf("first tool = %q/%q", names[0], ids[0])
	}
	if names[1] != "get_time" || ids[1] != "call_two" {
		t.Fatalf("second tool = %q/%q", names[1], ids[1])
	}

	// Fragmented arguments accumulate into valid JSON. The other wire sends
	// each call's arguments whole, so no test may assert one chunk per call.
	if arguments[0] != `{"city":"Denver"}` {
		t.Fatalf("fragmented arguments = %q", arguments[0])
	}
	if !json.Valid([]byte(arguments[0])) {
		t.Fatalf("accumulated arguments are not valid JSON: %q", arguments[0])
	}
	if arguments[1] != `{"city":"Osaka"}` {
		t.Fatalf("whole-chunk arguments = %q", arguments[1])
	}

	// Usage is merged and emitted exactly once. Emitting the partial object
	// from message_start and the fuller one from message_delta would make the
	// harness count the same cached tokens twice.
	if len(usage) != 1 {
		t.Fatalf("usage events = %d, want exactly 1", len(usage))
	}
	if usage[0].StopReason != "tool_calls" {
		t.Fatalf("stop reason = %q, want tool_calls (mapped from tool_use)", usage[0].StopReason)
	}
}

func TestAnthropicUsageNormalizationOnACachedPrefix(t *testing.T) {
	// The trap, measured on one identical 5,550-token prefix: the coding wire
	// reported prompt_tokens 5551 with cached_tokens 5504, and this wire
	// reported input_tokens 46 with cache_read_input_tokens 5504. On an
	// uncached prefix the correct and the naive mapping are indistinguishable,
	// so this asserts on a cached one.
	events := collectEvents(t, anthropicMessagesWire{}, anthropicStreamBody)
	var usage map[string]any
	for _, event := range events {
		if event.Kind == EventUsage {
			usage = event.Usage
		}
	}
	if usage == nil {
		t.Fatal("no usage event")
	}
	if got := usage["prompt_tokens"]; got != 46+5504 {
		t.Fatalf("prompt_tokens = %v, want %d (input_tokens + cache_read_input_tokens)", got, 46+5504)
	}
	if got := usage["prompt_tokens"]; got == 46 {
		t.Fatal("input_tokens was mapped straight onto prompt_tokens; compaction would never fire on a cached conversation")
	}
	if got := usage["completion_tokens"]; got != 83 {
		t.Fatalf("completion_tokens = %v, want 83", got)
	}
	details, ok := usage["prompt_tokens_details"].(map[string]any)
	if !ok || details["cached_tokens"] != 5504 {
		t.Fatalf("cached tokens = %v, want 5504", usage["prompt_tokens_details"])
	}
	if got := usage["total_tokens"]; got != 46+5504+83 {
		t.Fatalf("total_tokens = %v", got)
	}

	// reasoning_tokens is absent rather than zero. This wire reports no
	// equivalent, and on a wire that can silently discard an effort setting
	// that record is the only way the inertness would ever be noticed after
	// the fact — so a synthesized zero would be a lie exactly where it counts.
	if _, present := usage["completion_tokens_details"]; present {
		t.Fatalf("anthropic usage synthesized completion_tokens_details: %v", usage["completion_tokens_details"])
	}
}

func TestBothWiresProduceEquivalentEventSequences(t *testing.T) {
	// Equivalent streams, equivalent normalized events. The two dialects
	// differ in every surface detail; what must match is what leaves the
	// package.
	const codingBody = `data: {"choices":[{"delta":{"reasoning_content":"weighing it"}}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_one","function":{"name":"get_weather","arguments":"{\"city\":\"Denver\"}"}}]}}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_two","function":{"name":"get_time","arguments":"{\"city\":\"Osaka\"}"}}]},"finish_reason":"tool_calls"}]}

data: {"choices":[],"usage":{"prompt_tokens":5550,"completion_tokens":83,"prompt_tokens_details":{"cached_tokens":5504}}}

data: [DONE]
`
	type normalized struct {
		reasoning string
		tools     map[int]string
		names     map[int]string
		stop      string
	}
	reduce := func(events []Event) normalized {
		out := normalized{tools: map[int]string{}, names: map[int]string{}}
		for _, event := range events {
			switch event.Kind {
			case EventReasoning:
				out.reasoning += event.Text
			case EventTool:
				out.tools[event.ToolIndex] += event.Input
				if event.ToolName != "" {
					out.names[event.ToolIndex] = event.ToolName
				}
			case EventUsage:
				out.stop = event.StopReason
			}
		}
		return out
	}

	anthropic := reduce(collectEvents(t, anthropicMessagesWire{}, anthropicStreamBody))
	coding := reduce(collectEvents(t, openAIChatWire{}, codingBody))
	if !reflect.DeepEqual(anthropic, coding) {
		t.Fatalf("normalized sequences differ:\nanthropic %#v\n coding   %#v", anthropic, coding)
	}
	if anthropic.stop != "tool_calls" {
		t.Fatalf("stop reason = %q on both wires, want tool_calls", anthropic.stop)
	}
}

func TestThreeErrorEnvelopesAreClassified(t *testing.T) {
	// Three envelopes, not two. Every refusal these endpoints made was an HTTP
	// status before the stream opened, so classification is a status-class
	// decision over whatever body arrived.
	t.Run("coding wire", func(t *testing.T) {
		err := openAIChatWire{}.classifyError(400, []byte(`{"error":{"code":"1211","message":"Unknown Model, please check the model code."}}`))
		var status *StatusError
		if !errors.As(err, &status) {
			t.Fatalf("error is %T, want *StatusError", err)
		}
		if status.Code != "1211" || !strings.Contains(status.Message, "Unknown Model") {
			t.Fatalf("classified = %#v", status)
		}
		if status.Retryable() {
			t.Fatal("a 400 must not be retried")
		}
	})

	t.Run("anthropic wire", func(t *testing.T) {
		err := anthropicMessagesWire{}.classifyError(400, []byte(`{"type":"error","error":{"type":"invalid_request_error","code":"1211","message":"[1211][Unknown Model]"},"request_id":"req_xyz"}`))
		var status *StatusError
		if !errors.As(err, &status) {
			t.Fatalf("error is %T, want *StatusError", err)
		}
		if status.Code != "1211" {
			t.Fatalf("code = %q", status.Code)
		}
		// request_id is the only handle Z.ai gives for a support question, so
		// it has to survive into the error text.
		if status.RequestID != "req_xyz" || !strings.Contains(status.Error(), "req_xyz") {
			t.Fatalf("request id lost: %#v / %s", status, status.Error())
		}
	})

	t.Run("fastapi 422", func(t *testing.T) {
		body := `{"detail":[{"type":"missing","loc":["body","tools",0,"name"],"msg":"Field required","input":{}}]}`
		err := anthropicMessagesWire{}.classifyError(422, []byte(body))
		var status *StatusError
		if !errors.As(err, &status) {
			t.Fatalf("error is %T, want *StatusError", err)
		}
		if status.Status != 422 {
			t.Fatalf("status = %d", status.Status)
		}
		// A classifier that knew only the Anthropic envelope would render this
		// with an empty message, which is the failure this case exists for.
		if status.Message == "" || !strings.Contains(status.Message, "body.tools.0.name") {
			t.Fatalf("422 detail lost: %q", status.Message)
		}
		if status.Retryable() {
			t.Fatal("a 422 must not be retried")
		}
	})

	t.Run("5xx is retryable", func(t *testing.T) {
		for _, wire := range []wireStrategy{openAIChatWire{}, anthropicMessagesWire{}} {
			err := wire.classifyError(503, []byte(`upstream unavailable`))
			var status *StatusError
			if !errors.As(err, &status) || !status.Retryable() {
				t.Fatalf("%s: a 503 must be retried, got %#v", wire.name(), err)
			}
		}
	})
}

func TestRetryHonoursClassification(t *testing.T) {
	// A 4xx says the same thing three times. Retrying it wastes quota and
	// delays the report, and a 429 is deliberately inside that rule: on this
	// fleet a quota refusal stops the work and is reported at once.
	attempts := 0
	err := Retry(context.Background(), 3, func() error {
		attempts++
		return &StatusError{Status: 429, Wire: WireOpenAIChat, Message: "rate limited"}
	})
	if attempts != 1 {
		t.Fatalf("a 4xx was attempted %d times, want 1", attempts)
	}
	if err == nil {
		t.Fatal("the refusal was swallowed")
	}

	attempts = 0
	_ = Retry(context.Background(), 3, func() error {
		attempts++
		return &StatusError{Status: 500, Wire: WireOpenAIChat}
	})
	if attempts != 3 {
		t.Fatalf("a 5xx was attempted %d times, want 3", attempts)
	}

	// An error that does not classify itself is still retried, which is what
	// keeps transport failures behaving as they always did.
	attempts = 0
	_ = Retry(context.Background(), 3, func() error {
		attempts++
		return io.ErrUnexpectedEOF
	})
	if attempts != 3 {
		t.Fatalf("an unclassified error was attempted %d times, want 3", attempts)
	}
}

func TestAnthropicCatalogDecoderAndEndpoint(t *testing.T) {
	// The Anthropic catalog is Anthropic-shaped — created_at is an RFC3339
	// string where the OpenAI shape has a Unix integer — so modelMetadata
	// cannot read it and this wire needs its own decoder.
	const body = `{"data":[{"id":"glm-5.3-flash","type":"model","display_name":"GLM 5.3 Flash","created_at":"2026-01-02T03:04:05Z"}],"firstId":"glm-5.3-flash","hasMore":false,"lastId":"glm-5.3-flash"}`
	models, err := anthropicMessagesWire{}.decodeCatalog([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].ID != "glm-5.3-flash" {
		t.Fatalf("models = %#v", models)
	}
	if models[0].Created == 0 {
		t.Fatal("created_at was not parsed; the RFC3339 string must become Unix seconds")
	}
	// Neither catalog on this plan publishes a context length, so the catalog
	// is for the /models menu and never for sizing — the pin is the only
	// source of that number.
	if models[0].ContextLength != 0 {
		t.Fatalf("context length = %d; no plan catalog publishes one", models[0].ContextLength)
	}
	// The OpenAI decoder cannot read this document, which is why the second
	// decoder exists rather than a shared one.
	if openAIModels, err := (openAIChatWire{}).decodeCatalog([]byte(body)); err == nil && len(openAIModels) > 0 && openAIModels[0].Created != 0 {
		t.Fatal("the OpenAI decoder read the Anthropic document; the two shapes are not interchangeable")
	}

	// The catalog URL comes from policy, because suffix-stripping the
	// inference URL produces /v1/messages/models, which does not exist.
	t.Setenv("ZAI_API_KEY", "test-key-not-a-credential")
	p := forModelHTTP(t, "zai/glm-5.3-flash", OpenRouterEndpoint, false, anthropicPolicy())
	endpoint, err := p.catalogEndpoint()
	if err != nil {
		t.Fatal(err)
	}
	if endpoint != "https://api.z.ai/api/anthropic/v1/models" {
		t.Fatalf("catalog endpoint = %q", endpoint)
	}
	if derived, _ := modelsEndpoint(p.Endpoint); derived == endpoint {
		t.Fatal("suffix-stripping happens to match; this test no longer proves the policy is consulted")
	}
}

func TestAnthropicStreamSendsCanonicalHeaders(t *testing.T) {
	// The endpoint accepts either auth shape and does not enforce the version
	// header. The wire is the contract, not what this proxy tolerates.
	var got http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1}}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer server.Close()

	p, err := NewHTTPOnWire(server.URL+"/v1/messages", "test-key-not-a-credential", WireAnthropicMessages)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Stream(context.Background(), Request{Model: "glm-5.3-flash", System: "s"}, func(Event) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if got.Get("x-api-key") != "test-key-not-a-credential" {
		t.Fatalf("x-api-key = %q", got.Get("x-api-key"))
	}
	if got.Get("anthropic-version") != AnthropicVersion {
		t.Fatalf("anthropic-version = %q, want %s", got.Get("anthropic-version"), AnthropicVersion)
	}
	if got.Get("Authorization") != "" {
		t.Fatalf("the Anthropic wire sent a bearer Authorization header: %q", got.Get("Authorization"))
	}
}

func TestNewHTTPOnWireRefusesAnUnknownWire(t *testing.T) {
	if _, err := NewHTTPOnWire("https://example.invalid/v1", "k", "responses-api"); err == nil {
		t.Fatal("an unknown wire was constructed rather than refused")
	}
}

// anthropicOverloadStream is the frame observed live on 2026-09-14: the stream
// opened, carried content, and then produced an `error` frame mid-flight. The
// capability matrix produced none from either endpoint, so this shape had no
// test until the endpoint produced one under overload.
const anthropicOverloadStream = `event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":12}}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"par"}}

event: error
data: {"type":"error","error":{"type":"overloaded_error","message":"[500][Operation failed][20260914112046b125d352f3a64642]"}}

`

// anthropicRateLimitStream is the same shape carrying the one in-stream type
// this fleet must never retry.
const anthropicRateLimitStream = `event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":12}}}

event: error
data: {"type":"error","error":{"type":"rate_limit_error","message":"quota exhausted"}}

`

// drainStream runs a stream to its end and returns whatever it failed with,
// discarding events. It exists so a Retry body is one line.
func drainStream(body string) error {
	return anthropicMessagesWire{}.parseStream(strings.NewReader(body), func(Event) error { return nil })
}

func TestAnthropicStreamRejectsATruncatedStream(t *testing.T) {
	// `message_stop` is this wire's terminal marker. It was parsed into the
	// default arm and discarded, so a stream that stopped early flushed its
	// partial usage and reported a clean turn.
	body := "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":11}}}\n\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"half\"}}\n\n"
	if err := drainStream(body); !errors.Is(err, errTruncatedStream) {
		t.Fatalf("truncated stream returned %v, want errTruncatedStream", err)
	}
}

func TestAnthropicStreamAcceptsAStopReasonWithoutMessageStop(t *testing.T) {
	// The same leniency as the other wire: a delivered stop_reason means the
	// message completed, whatever the endpoint did with the terminal frame.
	body := "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":11}}}\n\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":4}}\n\n"
	if err := drainStream(body); err != nil {
		t.Fatalf("a stop_reason without message_stop was rejected: %v", err)
	}
}

func TestInStreamErrorFrameCarriesItsStatusClass(t *testing.T) {
	// An in-stream frame used to be raised as StatusError{Status: 0}, and
	// Retryable() reads 0 as "not 5xx", so Retry returned on the first
	// attempt. A plain fmt.Errorf would have been retried three times; the
	// typed error actively opted out of the behaviour retrying exists for.
	// Mapping the frame's `type` onto the status the same condition carries
	// before the stream opens puts both halves under one rule.
	cases := []struct {
		frameType string
		status    int
	}{
		{"invalid_request_error", http.StatusBadRequest},
		{"authentication_error", http.StatusUnauthorized},
		{"billing_error", http.StatusForbidden},
		{"permission_error", http.StatusForbidden},
		{"not_found_error", http.StatusNotFound},
		{"request_too_large", http.StatusRequestEntityTooLarge},
		{"rate_limit_error", http.StatusTooManyRequests},
		{"api_error", http.StatusInternalServerError},
		// 529 is Anthropic's own overload status. It is non-standard, so there
		// is no net/http constant, and the literal is asserted here rather
		// than the production constant so the test pins the wire value.
		{"overloaded_error", 529},
	}
	for _, c := range cases {
		body := fmt.Sprintf("event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":%q,\"message\":\"m\"}}\n\n", c.frameType)
		err := drainStream(body)
		var status *StatusError
		if !errors.As(err, &status) {
			t.Fatalf("%s: error is %T, want *StatusError", c.frameType, err)
		}
		if status.Status != c.status {
			t.Fatalf("%s: status = %d, want %d", c.frameType, status.Status, c.status)
		}
		if status.Code != c.frameType {
			t.Fatalf("%s: code = %q; the frame's own type is the only handle on it", c.frameType, status.Code)
		}
		if status.Wire != WireAnthropicMessages {
			t.Fatalf("%s: wire = %q", c.frameType, status.Wire)
		}
	}

	// An unrecognised type defaults to the retryable side, which is what this
	// package already does for an error that carries no classification at all.
	err := drainStream("event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"some_future_error\",\"message\":\"m\"}}\n\n")
	var status *StatusError
	if !errors.As(err, &status) || !status.Retryable() {
		t.Fatalf("an unrecognised in-stream type is not retryable: %#v", err)
	}
}

func TestRetryHonoursInStreamErrorFrames(t *testing.T) {
	// The live defect, end to end. An `overloaded_error` is a server-side
	// transient and is the textbook retryable case; before the type was
	// mapped it failed the turn on the first attempt.
	attempts := 0
	err := Retry(context.Background(), 3, func() error {
		attempts++
		return drainStream(anthropicOverloadStream)
	})
	if attempts != 3 {
		t.Fatalf("an in-stream overloaded_error was attempted %d times, want 3", attempts)
	}
	var status *StatusError
	if !errors.As(err, &status) {
		t.Fatalf("error is %T, want *StatusError", err)
	}
	if !status.Retryable() || status.Code != "overloaded_error" {
		t.Fatalf("classified = %#v", status)
	}
	if !strings.Contains(status.Error(), "20260914112046b125d352f3a64642") {
		t.Fatalf("the provider's own message was dropped: %s", status.Error())
	}

	// The fleet ruling is unchanged by the fix: a quota refusal stops the work
	// and is reported at once, because three silent retries would spend three
	// times the quota before anyone heard about it. A blanket "Status 0 is
	// retryable" would have broken exactly this.
	attempts = 0
	err = Retry(context.Background(), 3, func() error {
		attempts++
		return drainStream(anthropicRateLimitStream)
	})
	if attempts != 1 {
		t.Fatalf("an in-stream rate_limit_error was attempted %d times, want 1", attempts)
	}
	if !errors.As(err, &status) || status.Status != http.StatusTooManyRequests {
		t.Fatalf("classified = %#v", err)
	}
	if status.Retryable() {
		t.Fatal("a 429 must not be retried, in-stream or not")
	}
}

func TestInStreamErrorFrameKeepsTheRequestID(t *testing.T) {
	// request_id sits beside `error`, not inside it, and the in-stream frame
	// struct parsed neither. The pre-stream classifier already preserved it and
	// StatusError.Error() already prints it, so an overload that arrived
	// mid-stream was the one path that dropped the only handle Z.ai gives for
	// investigating the failure.
	body := "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":12}}}\n\n" +
		"data: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"code\":\"500\"," +
		"\"message\":\"operation failed\"},\"request_id\":\"req_123\"}\n\n"
	err := drainStream(body)
	var status *StatusError
	if !errors.As(err, &status) {
		t.Fatalf("error %v is not a StatusError", err)
	}
	if status.RequestID != "req_123" {
		t.Fatalf("RequestID = %q, want req_123", status.RequestID)
	}
	if !strings.Contains(status.Error(), "req_123") {
		t.Fatalf("the operator's handle is missing from the error text: %q", status.Error())
	}
	// The frame's numeric code must not disturb the settled type-based
	// classification that keeps rate_limit_error non-retryable.
	if status.Code != "overloaded_error" || !status.Retryable() {
		t.Fatalf("classification changed: code=%q retryable=%v", status.Code, status.Retryable())
	}
}
