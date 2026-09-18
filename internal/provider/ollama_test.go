package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// ollamaRoute is the local route as the fleet deploys it on the native wire.
func ollamaRoute() RoutePolicy {
	return RoutePolicy{
		Endpoint:        "http://fractal.wyvern-temperature.ts.net:11434/api/chat",
		Wire:            WireOllamaChat,
		ContextWindow:   65536,
		MaxOutputTokens: 32768,
		Effort: EffortDescriptor{Field: "think", Levels: map[string]string{
			"low": "low", "medium": "medium", "high": "high", "xhigh": "high", "max": "high",
		}},
		Options: map[string]json.RawMessage{
			"temperature":      json.RawMessage(`1.0`),
			"top_k":            json.RawMessage(`20`),
			"top_p":            json.RawMessage(`0.95`),
			"presence_penalty": json.RawMessage(`1.5`),
		},
	}
}

func ollamaPolicy() Policy {
	return Policy{Version: PolicyVersion, Routes: map[string]RoutePolicy{LocalModelID: ollamaRoute()}}
}

// ollamaServer answers every POST with a fixed status and body, and records
// the request it was sent.
func ollamaServer(t *testing.T, status int, body string) (*httptest.Server, *[]byte, *http.Header) {
	t.Helper()
	var got []byte
	var header http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		header = r.Header.Clone()
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	return server, &got, &header
}

func ollamaProvider(t *testing.T, endpoint string) *HTTPProvider {
	t.Helper()
	t.Setenv("SLBH_LOCAL_ENDPOINT", endpoint)
	p := forModelHTTP(t, LocalModelID, OpenRouterEndpoint, false, ollamaPolicy())
	if p.Wire() != WireOllamaChat {
		t.Fatalf("wire = %q, want %q", p.Wire(), WireOllamaChat)
	}
	return p
}

func streamAll(p *HTTPProvider, req Request) ([]Event, error) {
	var events []Event
	err := p.Stream(context.Background(), req, func(event Event) error {
		events = append(events, event)
		return nil
	})
	return events, err
}

// The NDJSON bodies below are shaped on what Ollama 0.34.1 returned for the
// served tag on 2026-09-18, trimmed of timing fields.
const ollamaTextBody = `{"model":"q27-UD-Q2_K_XL-64k","message":{"role":"assistant","content":"","thinking":"The user"},"done":false}
{"model":"q27-UD-Q2_K_XL-64k","message":{"role":"assistant","content":"","thinking":" says hi."},"done":false}
{"model":"q27-UD-Q2_K_XL-64k","message":{"role":"assistant","content":"Hello"},"done":false}

{"model":"q27-UD-Q2_K_XL-64k","message":{"role":"assistant","content":" there."},"done":false}
{"model":"q27-UD-Q2_K_XL-64k","message":{"role":"assistant","content":""},"done":true,"done_reason":"stop","prompt_eval_count":363,"prompt_eval_cached_count":301,"eval_count":69}
`

func TestOllamaStreamTextAndThinking(t *testing.T) {
	server, _, header := ollamaServer(t, http.StatusOK, ollamaTextBody)
	p := ollamaProvider(t, server.URL+"/api/chat")
	events, err := streamAll(p, Request{Model: LocalModelID, System: "s", Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	var text, thinking strings.Builder
	var usage []Event
	for _, event := range events {
		switch event.Kind {
		case EventText:
			text.WriteString(event.Text)
		case EventReasoning:
			thinking.WriteString(event.Text)
		case EventUsage:
			usage = append(usage, event)
		case EventTool:
			t.Fatalf("a text turn produced a tool event: %#v", event)
		}
	}
	if text.String() != "Hello there." || thinking.String() != "The user says hi." {
		t.Fatalf("text = %q thinking = %q", text.String(), thinking.String())
	}
	if len(usage) != 1 || usage[0].StopReason != "stop" {
		t.Fatalf("usage events = %#v", usage)
	}
	want := map[string]any{
		"prompt_tokens": 363, "completion_tokens": 69, "total_tokens": 432,
		"prompt_tokens_details": map[string]any{"cached_tokens": 301},
	}
	if !reflect.DeepEqual(usage[0].Usage, want) {
		t.Fatalf("usage = %#v, want %#v", usage[0].Usage, want)
	}
	if events[len(events)-1].Kind != EventDone {
		t.Fatalf("last event = %#v, want done", events[len(events)-1])
	}
	if header.Get("Authorization") != "" {
		t.Fatalf("the local route sent a credential: %q", header.Get("Authorization"))
	}
}

func TestOllamaStreamToolCallIsWholeAndStopsAsToolCalls(t *testing.T) {
	// Measured: the call arrives on the done chunk with object arguments, and
	// Ollama reports done_reason "stop" for it.
	body := `{"message":{"role":"assistant","content":"","thinking":"need it"},"done":false}
{"message":{"role":"assistant","content":"","tool_calls":[{"id":"3xfQ","function":{"index":0,"name":"get_weather","arguments":{"city":"Paris"}}},{"function":{"index":1,"name":"get_time","arguments":{}}}]},"done":true,"done_reason":"stop","prompt_eval_count":305,"prompt_eval_cached_count":0,"eval_count":47}
`
	events := collectEvents(t, ollamaChatWire{}, body)
	var tools []Event
	stop := ""
	for _, event := range events {
		switch event.Kind {
		case EventTool:
			tools = append(tools, event)
		case EventUsage:
			stop = event.StopReason
		}
	}
	if len(tools) != 2 {
		t.Fatalf("tool events = %#v", tools)
	}
	if tools[0].ToolName != "get_weather" || tools[0].ToolCallID != "3xfQ" || tools[0].ToolIndex != 0 || tools[0].Input != `{"city":"Paris"}` {
		t.Fatalf("first tool = %#v", tools[0])
	}
	// A call with no id still gets one, so its result can be paired.
	if tools[1].ToolName != "get_time" || tools[1].ToolCallID == "" || tools[1].ToolIndex != 1 || tools[1].Input != `{}` {
		t.Fatalf("second tool = %#v", tools[1])
	}
	if stop != "tool_calls" {
		t.Fatalf("stop reason = %q, want tool_calls", stop)
	}
}

func TestOllamaStreamLengthStopIsReported(t *testing.T) {
	body := `{"message":{"role":"assistant","content":"","thinking":"The"},"done":false}
{"message":{"role":"assistant","content":""},"done":true,"done_reason":"length","prompt_eval_count":11,"eval_count":40}
`
	events := collectEvents(t, ollamaChatWire{}, body)
	usage := events[len(events)-1]
	if usage.Kind != EventUsage || usage.StopReason != "length" || !IsOutputLimitStop(usage.StopReason) {
		t.Fatalf("terminal event = %#v", usage)
	}
	if _, cached := usage.Usage["prompt_tokens_details"]; cached {
		t.Fatalf("a chunk with no cached count synthesized one: %#v", usage.Usage)
	}
}

func TestOllamaStreamFailures(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
		is   error
	}{
		{
			name: "truncated before done",
			body: `{"message":{"role":"assistant","content":"Hel"},"done":false}` + "\n",
			is:   errTruncatedStream,
		},
		{
			name: "malformed line mid-stream",
			body: `{"message":{"role":"assistant","content":"Hel"},"done":false}` + "\n" + `{"message":{"role":` + "\n",
			want: "decode provider event",
		},
		{
			name: "error line mid-stream",
			body: `{"message":{"role":"assistant","content":"Hel"},"done":false}` + "\n" + `{"error":"an error was encountered while running the model"}` + "\n",
			want: "an error was encountered while running the model",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ollamaChatWire{}.parseStream(strings.NewReader(tc.body), func(Event) error { return nil })
			if err == nil {
				t.Fatal("stream parsed without error")
			}
			if tc.is != nil && !errors.Is(err, tc.is) {
				t.Fatalf("err = %v, want %v", err, tc.is)
			}
			if tc.want != "" && !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestOllamaHTTPErrorsKeepTheirStatusClass(t *testing.T) {
	for _, tc := range []struct {
		status    int
		body      string
		message   string
		retryable bool
	}{
		// Verbatim from the server on 2026-09-18.
		{http.StatusNotFound, `{"error":"model 'no-such-model' not found"}`, "model 'no-such-model' not found", false},
		{http.StatusBadRequest, `{"error":"invalid think value: \"xhigh\" (must be \"high\", \"medium\", \"low\", \"max\", true, or false)"}`, `invalid think value: "xhigh"`, false},
		{http.StatusInternalServerError, `{"error":"{\"error\":{\"code\":500,\"message\":\"Jinja Exception: Unexpected reasoning effort max.\"}}"}`, "Unexpected reasoning effort max", true},
		{http.StatusBadGateway, `upstream down`, "upstream down", true},
	} {
		server, _, _ := ollamaServer(t, tc.status, tc.body)
		p := ollamaProvider(t, server.URL+"/api/chat")
		_, err := streamAll(p, Request{Model: LocalModelID, Messages: []Message{{Role: "user", Content: "hi"}}})
		var status *StatusError
		if !errors.As(err, &status) {
			t.Fatalf("HTTP %d: err = %v, want a StatusError", tc.status, err)
		}
		if status.Status != tc.status || status.Wire != WireOllamaChat || !strings.Contains(status.Message, tc.message) || status.Retryable() != tc.retryable {
			t.Fatalf("HTTP %d: error = %#v", tc.status, status)
		}
	}
}

func TestOllamaPayloadCarriesThinkOptionsAndHistory(t *testing.T) {
	server, got, header := ollamaServer(t, http.StatusOK, ollamaTextBody)
	p := ollamaProvider(t, server.URL+"/api/chat")
	req := sampleRequest()
	req.Model = LocalModelID
	req.Effort = "xhigh"
	req.Messages[2].Name = "get_weather"
	if _, err := streamAll(p, req); err != nil {
		t.Fatal(err)
	}
	if header.Get("Accept") != "application/x-ndjson" {
		t.Fatalf("Accept = %q", header.Get("Accept"))
	}
	var body struct {
		Model    string                     `json:"model"`
		Stream   bool                       `json:"stream"`
		Think    string                     `json:"think"`
		Options  map[string]json.RawMessage `json:"options"`
		Messages []map[string]any           `json:"messages"`
		Tools    []map[string]any           `json:"tools"`
		// None of the other wires' spellings may leak onto this one.
		ReasoningEffort *string `json:"reasoning_effort"`
		MaxTokens       *int    `json:"max_tokens"`
	}
	if err := json.Unmarshal(*got, &body); err != nil {
		t.Fatal(err)
	}
	if body.Model != "q27-UD-Q2_K_XL-64k" || !body.Stream {
		t.Fatalf("model = %q stream = %v", body.Model, body.Stream)
	}
	// The policy maps xhigh onto high; the level reaches `think`, and only there.
	if body.Think != "high" || body.ReasoningEffort != nil || body.MaxTokens != nil {
		t.Fatalf("think = %q reasoning_effort = %v max_tokens = %v", body.Think, body.ReasoningEffort, body.MaxTokens)
	}
	wantOptions := map[string]string{
		"num_ctx": "65536", "num_predict": "32768",
		"top_k": "20", "top_p": "0.95", "presence_penalty": "1.5",
		// The request's own temperature (0.0) beats the policy's 1.0.
		"temperature": "0",
	}
	if len(body.Options) != len(wantOptions) {
		t.Fatalf("options = %s", *got)
	}
	for key, want := range wantOptions {
		if string(body.Options[key]) != want {
			t.Fatalf("options.%s = %s, want %s", key, body.Options[key], want)
		}
	}
	if len(body.Messages) != 4 || body.Messages[0]["role"] != "system" || body.Messages[0]["content"] != "you are terse" {
		t.Fatalf("messages = %#v", body.Messages)
	}
	assistant := body.Messages[2]
	if assistant["thinking"] != "need the tool" {
		t.Fatalf("assistant thinking = %#v", assistant)
	}
	calls, _ := assistant["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("assistant tool_calls = %#v", assistant)
	}
	function := calls[0].(map[string]any)["function"].(map[string]any)
	// Arguments go back as an object, not as a string of JSON.
	if args, ok := function["arguments"].(map[string]any); !ok || args["city"] != "Denver" {
		t.Fatalf("tool call arguments = %#v", function["arguments"])
	}
	tool := body.Messages[3]
	if tool["role"] != "tool" || tool["tool_name"] != "get_weather" || tool["tool_call_id"] != "call_abc" || tool["content"] != `{"tempC":7}` {
		t.Fatalf("tool result = %#v", tool)
	}
	if len(body.Tools) != 1 || body.Tools[0]["type"] != "function" {
		t.Fatalf("tools = %#v", body.Tools)
	}
}

func TestOllamaEffortMapping(t *testing.T) {
	p := ollamaProvider(t, "http://127.0.0.1:1/api/chat")
	for level, want := range map[string]string{"low": "low", "medium": "medium", "high": "high", "xhigh": "high", "max": "high", "": ""} {
		payload, err := p.RequestPayload(Request{Model: LocalModelID, Effort: level, Messages: []Message{{Role: "user", Content: "hi"}}})
		if err != nil {
			t.Fatalf("effort %q: %v", level, err)
		}
		var body map[string]json.RawMessage
		if err := json.Unmarshal(payload, &body); err != nil {
			t.Fatal(err)
		}
		raw, present := body["think"]
		if want == "" {
			// No level asks for nothing and sends nothing — the template's own
			// default then applies, exactly as an omitted reasoning_effort did.
			if present {
				t.Fatalf("empty effort sent think = %s", raw)
			}
			continue
		}
		if string(raw) != `"`+want+`"` {
			t.Fatalf("effort %q sent think = %s, want %q", level, raw, want)
		}
	}
	// A level the route does not map refuses, as on every wire.
	route := ollamaRoute()
	delete(route.Effort.Levels, "max")
	policy := Policy{Version: PolicyVersion, Routes: map[string]RoutePolicy{LocalModelID: route}}
	restricted := forModelHTTP(t, LocalModelID, OpenRouterEndpoint, false, policy)
	if _, err := restricted.RequestPayload(Request{Model: LocalModelID, Effort: "max"}); err == nil || !strings.Contains(err.Error(), "does not support effort") {
		t.Fatalf("unmapped level err = %v", err)
	}
}

func TestOllamaTranscriptRoundTripsExactly(t *testing.T) {
	p := ollamaProvider(t, "http://127.0.0.1:1/api/chat")
	req := sampleRequest()
	req.Model = LocalModelID
	req.Messages[2].Name = "get_weather"
	req.Effort = "high"
	payload, err := p.RequestPayload(req)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := RequestFromPayload(payload)
	if err != nil {
		t.Fatal(err)
	}
	// The wire slug comes back rather than the route key, as on the other
	// wires, and the cache key has no field on this wire at all.
	if decoded.Model != "q27-UD-Q2_K_XL-64k" || decoded.CacheKey != "" {
		t.Fatalf("decoded model = %q cache key = %q", decoded.Model, decoded.CacheKey)
	}
	decoded.Model, decoded.CacheKey = req.Model, req.CacheKey
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
	onWire, err := RequestFromPayloadOnWire(WireOllamaChat, payload)
	if err != nil || onWire.System != req.System {
		t.Fatalf("RequestFromPayloadOnWire = %#v, %v", onWire, err)
	}
}

func TestOllamaPolicyValidation(t *testing.T) {
	if err := ollamaPolicy().Validate(); err != nil {
		t.Fatalf("the deployed local route does not validate: %v", err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*RoutePolicy)
		want   string
	}{
		{"reasoning_effort spelling", func(r *RoutePolicy) { r.Effort.Field = "reasoning_effort" }, `only spelling that is honoured on that wire is "think"`},
		{"unknown option", func(r *RoutePolicy) { r.Options["repeat_penalti"] = json.RawMessage(`1.1`) }, "options.repeat_penalti"},
		{"runner option", func(r *RoutePolicy) { r.Options["draft_num_predict"] = json.RawMessage(`2`) }, "reload the shared model"},
		{"num_ctx in options", func(r *RoutePolicy) { r.Options["num_ctx"] = json.RawMessage(`65536`) }, "the window is contextWindow"},
		{"num_predict in options", func(r *RoutePolicy) { r.Options["num_predict"] = json.RawMessage(`32768`) }, "the generation bound is maxOutputTokens"},
		{"string value", func(r *RoutePolicy) { r.Options["top_p"] = json.RawMessage(`"0.95"`) }, "options.top_p must be a number"},
		{"fractional integer", func(r *RoutePolicy) { r.Options["top_k"] = json.RawMessage(`20.5`) }, "options.top_k must be an integer"},
		{"probability above one", func(r *RoutePolicy) { r.Options["min_p"] = json.RawMessage(`1.5`) }, "options.min_p is above 1"},
		{"negative penalty", func(r *RoutePolicy) { r.Options["presence_penalty"] = json.RawMessage(`-1`) }, "options.presence_penalty is negative"},
		{"stop not strings", func(r *RoutePolicy) { r.Options["stop"] = json.RawMessage(`[1]`) }, "options.stop must be an array of strings"},
		{"options on another wire", func(r *RoutePolicy) {
			r.Wire, r.Effort.Field = WireOpenAIChat, "reasoning_effort"
		}, "only the ollama-chat wire sends"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			route := ollamaRoute()
			tc.mutate(&route)
			err := route.validate(LocalModelID)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
	// Every sampler key the wire admits validates with a sane value.
	route := ollamaRoute()
	route.Options = map[string]json.RawMessage{
		"temperature": json.RawMessage(`0.7`), "top_k": json.RawMessage(`40`), "top_p": json.RawMessage(`0.9`),
		"min_p": json.RawMessage(`0.05`), "typical_p": json.RawMessage(`1`), "repeat_penalty": json.RawMessage(`1.1`),
		"repeat_last_n": json.RawMessage(`-1`), "presence_penalty": json.RawMessage(`1.5`), "frequency_penalty": json.RawMessage(`0`),
		"seed": json.RawMessage(`-1`), "stop": json.RawMessage(`["<|im_end|>"]`),
	}
	if err := route.validate(LocalModelID); err != nil {
		t.Fatalf("the full sampler set does not validate: %v", err)
	}
	// And an options block decodes from the document, with an unknown key
	// refused on read rather than ignored.
	var policy Policy
	document := `{"version":1,"routes":{"local/q27-UD-Q2_K_XL-64k":{"endpoint":"http://h:11434/api/chat","wire":"ollama-chat","contextWindow":65536,"effort":{"field":"think","levels":{"medium":"medium"}},"options":{"top_k":20,"mirostat":2}}}}`
	if err := json.Unmarshal([]byte(document), &policy); err != nil {
		t.Fatal(err)
	}
	if err := policy.Validate(); err == nil || !strings.Contains(err.Error(), "options.mirostat") {
		t.Fatalf("unknown option in a document err = %v", err)
	}
}

func TestCommittedManagedPolicyPutsTheLocalRouteOnTheNativeWire(t *testing.T) {
	route, ok := committedManagedPolicy(t).Routes[LocalModelID]
	if !ok {
		t.Fatalf("the managed artifact has no %s route", LocalModelID)
	}
	if route.Wire != WireOllamaChat || route.Effort.Field != "think" || !strings.HasSuffix(route.Endpoint, "/api/chat") {
		t.Fatalf("managed local route = %#v", route)
	}
	if route.ContextWindow != LocalContextWindow || route.MaxOutputTokens != 32768 {
		t.Fatalf("managed local route window/bound = %d/%d", route.ContextWindow, route.MaxOutputTokens)
	}
	if string(route.Options["presence_penalty"]) != "1.5" {
		t.Fatalf("managed local route carries no presence_penalty guard: %s", route.Options["presence_penalty"])
	}
}

func TestDefaultLocalPolicyPutsTheLocalRouteOnTheNativeWire(t *testing.T) {
	t.Setenv("SLBH_LOCAL_ENDPOINT", "")
	policy := DefaultLocalPolicy([]string{LocalModelID, "local/other-tag"})
	if err := policy.Validate(); err != nil {
		t.Fatal(err)
	}
	route := policy.Routes[LocalModelID]
	if route.Wire != WireOllamaChat || route.Endpoint != localDefaultURL || route.Effort.Field != "think" || route.ContextWindow != LocalContextWindow {
		t.Fatalf("default local route = %#v", route)
	}
	// Another tag's window is not known to the binary, so none is pinned and
	// no num_ctx is sent: the Modelfile's stands.
	if other := policy.Routes["local/other-tag"]; other.Wire != WireOllamaChat || other.ContextWindow != 0 {
		t.Fatalf("default route for another local tag = %#v", other)
	}
}
