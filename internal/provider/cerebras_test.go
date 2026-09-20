package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The Cerebras route: a native family on the openai-chat wire whose endpoint
// differs from the others in one measured way — it refuses
// `include_reasoning` — and whose catalog publishes ids and nothing else.
// Both facts are load-bearing and both are asserted here rather than left to
// the live test, which runs only when someone asks for it.

// cerebrasLevels is the effort map the managed policy carries for this route:
// the four levels the endpoint accepts, minus `none`, and deliberately without
// `max` or `xhigh`. Cerebras answers those with HTTP 400 (`reasoning_effort:
// Input should be 'none', 'low', 'medium' or 'high'`, measured 2026-09-20), and
// a route that mapped them onto `high` would record a sweep at an effort
// nobody asked for.
func cerebrasLevels() map[string]string {
	return map[string]string{"low": "low", "medium": "medium", "high": "high"}
}

func cerebrasPolicy() Policy {
	policy := testPolicy()
	policy.Routes["cerebras/qwen-3.8-27b"] = RoutePolicy{
		Endpoint:        "https://api.cerebras.ai/v1/chat/completions",
		CatalogEndpoint: "https://api.cerebras.ai/v1/models",
		Wire:            WireOpenAIChat,
		ContextWindow:   131072,
		MaxOutputTokens: 32768,
		DefaultEffort:   "medium",
		Effort:          EffortDescriptor{Field: "reasoning_effort", Levels: cerebrasLevels()},
	}
	return policy
}

func TestCerebrasRouteIsNativeAndCarriesItsOwnKey(t *testing.T) {
	t.Setenv("CEREBRAS_API_KEY", "test-key-not-a-credential")
	t.Setenv("OPENROUTER_API_KEY", "other-key-not-a-credential")

	route, err := ResolveRoute("cerebras/qwen-3.8-27b", OpenRouterEndpoint, false, cerebrasPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if route.Flavor != "cerebras" {
		t.Fatalf("flavor = %q, want cerebras", route.Flavor)
	}
	if route.Endpoint != "https://api.cerebras.ai/v1/chat/completions" {
		t.Fatalf("endpoint = %q", route.Endpoint)
	}
	if route.APIKey != "test-key-not-a-credential" {
		t.Fatalf("route did not carry CEREBRAS_API_KEY")
	}
	if route.Wire != WireOpenAIChat {
		t.Fatalf("wire = %q, want %q", route.Wire, WireOpenAIChat)
	}
	// The pin is the only answer there is: the catalog publishes no
	// context_length, so nothing could discover one.
	if route.ContextWindow != 131072 {
		t.Fatalf("context window = %d, want the policy pin 131072", route.ContextWindow)
	}
}

func TestCerebrasRefusesRatherThanFallingThroughToOpenRouter(t *testing.T) {
	// The standing rule, applied to a new family: an absent native key refuses
	// by name instead of quietly sending the request somewhere it would be
	// billed differently and served by an upstream nobody chose.
	t.Setenv("CEREBRAS_API_KEY", "")
	t.Setenv("OPENROUTER_API_KEY", "other-key-not-a-credential")

	_, err := ResolveRoute("cerebras/qwen-3.8-27b", OpenRouterEndpoint, false, cerebrasPolicy())
	if err == nil {
		t.Fatal("a Cerebras route with no key resolved instead of refusing")
	}
	if !strings.Contains(err.Error(), "CEREBRAS_API_KEY") {
		t.Fatalf("refusal does not name the missing variable: %v", err)
	}
}

func TestCerebrasPayloadOmitsIncludeReasoning(t *testing.T) {
	// Measured 2026-09-20: Cerebras answers `include_reasoning` with HTTP 400
	// `property 'include_reasoning' is unsupported`, so a request carrying an
	// effort would never have streamed a token. The effort itself must still
	// go, and the reasoning still arrives — as `delta.reasoning`, unasked.
	t.Setenv("CEREBRAS_API_KEY", "test-key-not-a-credential")

	p := forModelHTTP(t, "cerebras/qwen-3.8-27b", OpenRouterEndpoint, false, cerebrasPolicy())
	payload, err := p.RequestPayload(Request{Model: "cerebras/qwen-3.8-27b", System: "s", Effort: "medium"})
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(payload, &body); err != nil {
		t.Fatal(err)
	}
	if _, present := body["include_reasoning"]; present {
		t.Fatalf("Cerebras payload carries include_reasoning: %s", payload)
	}
	if body["reasoning_effort"] != "medium" {
		t.Fatalf("reasoning_effort = %v, want medium", body["reasoning_effort"])
	}
	// The wire model id is the bare one: the `cerebras/` prefix addresses the
	// route and is not a name the endpoint knows.
	if body["model"] != "qwen-3.8-27b" {
		t.Fatalf("model = %v, want the unprefixed qwen-3.8-27b", body["model"])
	}
}

func TestCerebrasPayloadStripsReplayedReasoningContent(t *testing.T) {
	// The second half of the same refusal, and the one a first-turn test would
	// have missed: an assistant turn replayed with its `reasoning_content` is
	// HTTP 400 `messages.2.assistant.reasoning_content ... is unsupported`
	// (measured 2026-09-20), so a tool round trip failed on its second turn
	// while the first looked perfect.
	t.Setenv("CEREBRAS_API_KEY", "test-key-not-a-credential")

	history := []Message{
		{Role: "user", Content: "call the tool"},
		{Role: "assistant", Content: "calling", ReasoningContent: "the model's earlier thinking"},
		{Role: "tool", ToolCallID: "call-1", Name: "get_weather", Content: "Sunny"},
	}
	p := forModelHTTP(t, "cerebras/qwen-3.8-27b", OpenRouterEndpoint, false, cerebrasPolicy())
	payload, err := p.RequestPayload(Request{Model: "cerebras/qwen-3.8-27b", System: "s", Messages: history})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(payload), "reasoning_content") {
		t.Fatalf("Cerebras payload replays reasoning_content: %s", payload)
	}
	// The caller's history is the harness's own record and must come back
	// untouched: the reasoning is stripped from the request, not from the
	// transcript.
	if history[1].ReasoningContent != "the model's earlier thinking" {
		t.Fatal("encoding the request erased the reasoning from the caller's history")
	}
}

func TestReasoningFieldsSurviveOnEveryOtherOpenAIChatRoute(t *testing.T) {
	// The omission is scoped to the flavor that refuses the field. Dropping it
	// everywhere would silently cost the reasoning stream on the routes that
	// only echo it when asked.
	t.Setenv("DEEPSEEK_API_KEY", "test-key-not-a-credential")
	t.Setenv("OPENROUTER_API_KEY", "test-key-not-a-credential")

	replayed := []Message{{Role: "assistant", Content: "earlier", ReasoningContent: "earlier thinking"}}
	for _, model := range []string{"deepseek-v4-flash", "vendor/model"} {
		p := forModelHTTP(t, model, OpenRouterEndpoint, false, cerebrasPolicy())
		payload, err := p.RequestPayload(Request{Model: model, System: "s", Effort: "medium", Messages: replayed})
		if err != nil {
			t.Fatalf("%s: %v", model, err)
		}
		var body map[string]any
		if err := json.Unmarshal(payload, &body); err != nil {
			t.Fatal(err)
		}
		if body["include_reasoning"] != true {
			t.Fatalf("%s lost include_reasoning: %s", model, payload)
		}
		if !strings.Contains(string(payload), "reasoning_content") {
			t.Fatalf("%s lost the replayed reasoning_content: %s", model, payload)
		}
	}
}

func TestCerebrasRefusesAnEffortLevelTheEndpointDoesNotHave(t *testing.T) {
	// `max` is not walked down to `high`. The endpoint has four levels and the
	// route maps three; asking for a fourth refuses with the supported set
	// named, which is the rule the effort map exists to keep.
	t.Setenv("CEREBRAS_API_KEY", "test-key-not-a-credential")

	p := forModelHTTP(t, "cerebras/qwen-3.8-27b", OpenRouterEndpoint, false, cerebrasPolicy())
	_, err := p.RequestPayload(Request{Model: "cerebras/qwen-3.8-27b", System: "s", Effort: "max"})
	if err == nil {
		t.Fatal("effort max was encoded instead of refused")
	}
	if !strings.Contains(err.Error(), "low, medium, high") && !strings.Contains(err.Error(), "does not support effort") {
		t.Fatalf("refusal does not name the supported levels: %v", err)
	}
}

func TestCerebrasRouteKeyAndWireModelAreInverse(t *testing.T) {
	// The bare name is deliberately not folded into this family: the models
	// Cerebras serves are open-weight ones the desktop and OpenRouter serve
	// too, so `qwen-3.8-27b` on its own addresses no native route here.
	key, ok := RouteKey("cerebras/qwen-3.8-27b")
	if !ok || key != "cerebras/qwen-3.8-27b" {
		t.Fatalf("RouteKey = %q (ok=%v)", key, ok)
	}
	if native, isNative := nativeRouteFor("qwen-3.8-27b"); isNative {
		t.Fatalf("the bare model name was captured by the %s family", native.flavor)
	}
}

func TestCatalogListsCerebrasWithThePolicyPinForItsWindow(t *testing.T) {
	// Cerebras's /v1/models publishes ids, `created: 0` and no context length,
	// so the menu would otherwise offer a model whose size it could not state
	// while the policy pinned it all along.
	catalog := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-key-not-a-credential" {
			t.Errorf("catalog fetch sent %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[
			{"id":"qwen-3.8-27b","object":"model","created":0,"owned_by":"Cerebras"},
			{"id":"gpt-oss-120b","object":"model","created":0,"owned_by":"Cerebras"}]}`))
	}))
	defer catalog.Close()

	t.Setenv("CEREBRAS_API_KEY", "test-key-not-a-credential")
	t.Setenv("OPENROUTER_API_KEY", "")
	t.Setenv("DEEPSEEK_API_KEY", "")
	t.Setenv("ZAI_API_KEY", "")

	policy := Policy{Version: PolicyVersion, Routes: map[string]RoutePolicy{
		"cerebras/qwen-3.8-27b": {
			Endpoint:        "https://api.cerebras.ai/v1/chat/completions",
			CatalogEndpoint: catalog.URL,
			Wire:            WireOpenAIChat,
			ContextWindow:   131072,
			Effort:          EffortDescriptor{Field: "reasoning_effort", Levels: cerebrasLevels()},
		},
	}}

	catalogs, err := DiscoverCatalog(context.Background(), policy)
	if err != nil {
		t.Fatal(err)
	}
	var models []ModelInfo
	for _, branch := range catalogs {
		if branch.Name == "cerebras" {
			models = branch.Models
		}
	}
	if len(models) != 2 {
		t.Fatalf("cerebras branch has %d models, want 2: %+v", len(models), catalogs)
	}
	byID := map[string]ModelInfo{}
	for _, model := range models {
		byID[model.ID] = model
	}
	pinned, ok := byID["cerebras/qwen-3.8-27b"]
	if !ok {
		t.Fatalf("catalog did not qualify the id with its family: %+v", models)
	}
	if pinned.ContextWindow != 131072 {
		t.Fatalf("window = %d, want the policy pin 131072", pinned.ContextWindow)
	}
	// The model the policy says nothing about keeps its unknown window rather
	// than borrowing another route's pin.
	if unpinned := byID["cerebras/gpt-oss-120b"]; unpinned.ContextWindow != 0 {
		t.Fatalf("an unrouted model took a window of %d", unpinned.ContextWindow)
	}
}

func TestCommittedCerebrasRouteMatchesTheEndpoint(t *testing.T) {
	// The managed artifact's Cerebras entry, checked against what the endpoint
	// actually answers to (measured 2026-09-20 against qwen-3.8-27b): four
	// effort levels of which three are slbh's, a 131,072-token ceiling the
	// catalog does not publish, and the OpenAI-shaped wire.
	route, ok := committedManagedPolicy(t).Routes["cerebras/qwen-3.8-27b"]
	if !ok {
		t.Fatal("the managed artifact has no Cerebras route")
	}
	if route.Wire != WireOpenAIChat || route.Effort.Field != "reasoning_effort" {
		t.Fatalf("managed cerebras route = %#v", route)
	}
	if route.ContextWindow != 131072 {
		t.Fatalf("pin = %d, want the measured 131072 (`limit is 131072`, HTTP 400 context_length_exceeded)", route.ContextWindow)
	}
	if route.CatalogEndpoint == "" {
		t.Fatal("managed cerebras route has no catalogEndpoint")
	}
	for _, level := range []string{"max", "xhigh"} {
		if _, mapped := route.Effort.Levels[level]; mapped {
			t.Fatalf("effort %q is mapped, but the endpoint accepts only none, low, medium and high", level)
		}
	}
	for _, level := range []string{"low", "medium", "high"} {
		if route.Effort.Levels[level] != level {
			t.Fatalf("effort %q maps to %q, want itself", level, route.Effort.Levels[level])
		}
	}
}
