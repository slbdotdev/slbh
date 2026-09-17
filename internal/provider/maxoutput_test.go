package provider

import (
	"encoding/json"
	"strings"
	"testing"
)

// boundIn pulls `max_tokens` out of an encoded body, reporting whether the
// field was present at all. Absence and zero are different things here:
// absence is an unbounded generation, which is the defect these tests exist
// to prevent regressing.
func boundIn(t *testing.T, payload []byte) (int, bool) {
	t.Helper()
	var body struct {
		MaxTokens *int `json:"max_tokens"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if body.MaxTokens == nil {
		return 0, false
	}
	return *body.MaxTokens, true
}

func boundedRequest() Request {
	return Request{
		Model:    "vendor/model",
		Effort:   "high",
		System:   "system",
		Messages: []Message{{Role: "user", Content: "hello"}},
	}
}

// An unbounded generation is the defect: with no `max_tokens` the endpoint can
// only ever return `stop`, so a model that will not terminate is a hang rather
// than a `length` a caller can read. Every openai-chat request carries a bound.
func TestOpenAIWireAlwaysSendsABound(t *testing.T) {
	p := NewHTTP("https://example.invalid/v1/chat/completions", "test-key")
	payload, err := p.RequestPayload(boundedRequest())
	if err != nil {
		t.Fatal(err)
	}
	bound, present := boundIn(t, payload)
	if !present {
		t.Fatalf("openai-chat request carries no max_tokens, so the generation is unbounded: %s", payload)
	}
	if bound != DefaultMaxOutputTokens {
		t.Fatalf("bound = %d, want the wire default %d", bound, DefaultMaxOutputTokens)
	}
}

// The routes differ by more than an order of magnitude in what an unbounded
// generation costs, so the policy's per-route value beats the wire default.
func TestRouteBoundBeatsWireDefault(t *testing.T) {
	p := NewHTTP("https://example.invalid/v1/chat/completions", "")
	p.Route = Route{
		Key:    "local/q27-UD-IQ3_S-128k",
		Flavor: "local",
		Wire:   WireOpenAIChat,
		Policy: RoutePolicy{
			MaxOutputTokens: 4096,
			Effort:          EffortDescriptor{Field: "reasoning_effort", Levels: map[string]string{"high": "high"}},
		},
	}
	payload, err := p.RequestPayload(boundedRequest())
	if err != nil {
		t.Fatal(err)
	}
	if bound, _ := boundIn(t, payload); bound != 4096 {
		t.Fatalf("bound = %d, want the route's 4096", bound)
	}
}

// A caller that sets its own bound beats both, matching the precedence
// resolveContextWindow already establishes: explicit beats pin beats fallback.
func TestRequestBoundBeatsRoute(t *testing.T) {
	p := NewHTTP("https://example.invalid/v1/chat/completions", "")
	p.Route = Route{Wire: WireOpenAIChat, Policy: RoutePolicy{MaxOutputTokens: 4096}}
	req := boundedRequest()
	explicit := 512
	req.MaxTokens = &explicit
	payload, err := p.RequestPayload(req)
	if err != nil {
		t.Fatal(err)
	}
	if bound, _ := boundIn(t, payload); bound != 512 {
		t.Fatalf("bound = %d, want the request's 512", bound)
	}
}

// The Anthropic wire sends max_tokens because the Messages API requires it,
// and its ceiling was chosen to mean "no limit". A route value still beats it,
// but the default must not silently become the openai wire's smaller bound —
// that would change deliberation on the plan route, which this change is not
// for.
func TestAnthropicWireKeepsItsOwnDefault(t *testing.T) {
	p, err := NewHTTPOnWire("https://example.invalid/v1/messages", "test-key", WireAnthropicMessages)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := p.RequestPayload(boundedRequest())
	if err != nil {
		t.Fatal(err)
	}
	bound, present := boundIn(t, payload)
	if !present {
		t.Fatalf("anthropic-messages request carries no max_tokens: %s", payload)
	}
	if bound != anthropicMaxTokens {
		t.Fatalf("bound = %d, want the anthropic default %d", bound, anthropicMaxTokens)
	}

	p.Route = Route{Wire: WireAnthropicMessages, Policy: RoutePolicy{MaxOutputTokens: 8192}}
	payload, err = p.RequestPayload(boundedRequest())
	if err != nil {
		t.Fatal(err)
	}
	if bound, _ := boundIn(t, payload); bound != 8192 {
		t.Fatalf("bound = %d, want the route's 8192", bound)
	}
}

// The bound is route-derived, so like stream_options and the provider object
// it is regenerated on re-encode rather than carried back into the Request.
// Decoding it would break the exact round trip the transcript contract needs.
func TestBoundIsRegeneratedRatherThanDecoded(t *testing.T) {
	p := NewHTTP("https://example.invalid/v1/chat/completions", "test-key")
	payload, err := p.RequestPayload(boundedRequest())
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := RequestFromPayload(payload)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.MaxTokens != nil {
		t.Fatalf("decoded request carries MaxTokens %d; a wire default must not become caller intent", *decoded.MaxTokens)
	}
	reencoded, err := p.RequestPayload(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if string(payload) != string(reencoded) {
		t.Fatalf("payload changed across a transcript round trip:\nwant %s\n got %s", payload, reencoded)
	}
}

func TestPolicyRejectsAnImpossibleBound(t *testing.T) {
	for _, tc := range []struct {
		name  string
		route RoutePolicy
		want  string
	}{
		{
			name:  "negative",
			route: RoutePolicy{MaxOutputTokens: -1},
			want:  "negative maxOutputTokens",
		},
		{
			name:  "above its own window",
			route: RoutePolicy{ContextWindow: 4096, MaxOutputTokens: 8192},
			want:  "above its own contextWindow",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			route := tc.route
			route.Endpoint = "https://example.invalid/v1/chat/completions"
			route.Wire = WireOpenAIChat
			route.Effort = EffortDescriptor{Field: "reasoning_effort", Levels: map[string]string{"high": "high"}}
			err := route.validate("some/route")
			if err == nil {
				t.Fatalf("validate accepted %#v", tc.route)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not name %q", err, tc.want)
			}
		})
	}
}

// A bound equal to the window is legitimate: it is the largest generation the
// route could ever produce, and refusing it would make the check a difficulty
// gate rather than a validity one.
func TestPolicyAcceptsABoundAtTheWindow(t *testing.T) {
	route := RoutePolicy{
		Endpoint:        "https://example.invalid/v1/chat/completions",
		Wire:            WireOpenAIChat,
		ContextWindow:   8192,
		MaxOutputTokens: 8192,
		Effort:          EffortDescriptor{Field: "reasoning_effort", Levels: map[string]string{"high": "high"}},
	}
	if err := route.validate("some/route"); err != nil {
		t.Fatalf("validate rejected a bound equal to the window: %v", err)
	}
}
