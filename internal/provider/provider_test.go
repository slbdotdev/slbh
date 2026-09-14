package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
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
	descriptionChanged := base
	descriptionChanged.Tools = []Tool{{Name: "read_file", Description: "different", Parameters: map[string]any{"type": "object"}}}
	if StablePrefixKey(first) == StablePrefixKey(descriptionChanged) {
		t.Fatal("tool description changes must change the cache key")
	}
}

func TestRequestPayloadRoundTripsExactly(t *testing.T) {
	temperature := 0.2
	req := Request{
		Model:  "vendor/model",
		Effort: "high",
		System: "stable system",
		Messages: []Message{
			{Role: "user", Content: "hello"},
			{Role: "assistant", Content: "answer", ReasoningContent: "thought"},
		},
		Tools:       []Tool{{Name: "read_file", Description: "read", Parameters: map[string]any{"type": "object"}}},
		CacheKey:    "slbh-test-cache",
		Temperature: &temperature,
	}
	p := NewHTTP("https://example.invalid/v1/chat/completions", "test-key")
	payload, err := p.RequestPayload(req)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := RequestFromPayload(payload)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(req, decoded) {
		t.Fatalf("decoded request differs:\nwant %#v\n got %#v", req, decoded)
	}
	reencoded, err := p.RequestPayload(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(payload, reencoded) {
		t.Fatalf("request payload changed after transcript round trip:\nwant %s\n got %s", payload, reencoded)
	}
	var body map[string]any
	if err := json.Unmarshal(payload, &body); err != nil {
		t.Fatal(err)
	}
	streamOptions, ok := body["stream_options"].(map[string]any)
	if !ok || streamOptions["include_usage"] != true {
		t.Fatalf("stream usage reporting was not enabled: %s", payload)
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

func TestForModelRoutesLocalOllamaWithoutKey(t *testing.T) {
	t.Setenv("SLBH_LOCAL_ENDPOINT", "http://127.0.0.1:11434/v1/chat/completions")
	p, err := ForModel(LocalModelID, "https://example.invalid/v1/chat/completions", true)
	if err != nil {
		t.Fatal(err)
	}
	if p.Flavor != LocalProviderName || p.Endpoint != "http://127.0.0.1:11434/v1/chat/completions" || p.APIKey != "" {
		t.Fatalf("local provider = %#v", p)
	}
	if got := p.modelID(LocalModelID); got != localWireModelID {
		t.Fatalf("wire model = %q, want %q", got, localWireModelID)
	}
	if got, err := p.ContextWindow(context.Background(), LocalModelID); err != nil || got != LocalContextWindow {
		t.Fatalf("local context window = %d, err = %v", got, err)
	}
}

func TestLocalProviderStreamsWithoutAuthorization(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "" {
			t.Fatalf("local provider sent authorization header %q", got)
		}
		var body struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.Model != localWireModelID {
			t.Fatalf("wire model = %q, want %q", body.Model, localWireModelID)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()

	p := NewHTTP(server.URL+"/v1/chat/completions", "")
	p.Flavor = LocalProviderName
	var got string
	err := p.Stream(context.Background(), Request{Model: LocalModelID}, func(event Event) error {
		if event.Kind == EventText {
			got += event.Text
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != "ok" {
		t.Fatalf("streamed text = %q, want ok", got)
	}
}

func TestContextWindowReadsAndCachesLiveMetadata(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/models" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"vendor/model","context_length":262144}]}`))
	}))
	defer server.Close()

	p := NewHTTP(server.URL+"/v1/chat/completions", "test-key")
	for i := 0; i < 2; i++ {
		got, err := p.ContextWindow(context.Background(), "vendor/model")
		if err != nil {
			t.Fatal(err)
		}
		if got != 262144 {
			t.Fatalf("context window = %d, want 262144", got)
		}
	}
	if calls != 1 {
		t.Fatalf("metadata calls = %d, want one cached lookup", calls)
	}
}

func TestOpenRouterCatalogFiltersModelsOlderThanOneYear(t *testing.T) {
	now := time.Now().Unix()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"data":[{"id":"new/model","created":%d},{"id":"old/model","created":%d},{"id":"unknown/model"}]}`, now, now-366*24*60*60)
	}))
	defer server.Close()

	catalog, err := fetchCatalog(context.Background(), catalogSpec{name: "openrouter", endpoint: server.URL + "/v1/chat/completions", key: "test-key"})
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Models) != 1 || catalog.Models[0].ID != "new/model" {
		t.Fatalf("fresh OpenRouter models = %#v, want only new/model", catalog.Models)
	}
}

func TestDiscoverCatalogIncludesLocalWorkhorseWithoutKey(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "")
	t.Setenv("ZAI_API_KEY", "")
	t.Setenv("OPENROUTER_API_KEY", "")
	t.Setenv("SLBH_LOCAL_ENDPOINT", "http://127.0.0.1:11434/v1/chat/completions")
	catalogs, err := DiscoverCatalog(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(catalogs) != 1 || catalogs[0].Name != LocalProviderName || catalogs[0].Endpoint != "http://127.0.0.1:11434/v1/chat/completions" {
		t.Fatalf("catalogs = %#v", catalogs)
	}
	if len(catalogs[0].Models) != 1 || catalogs[0].Models[0].ID != LocalModelID || catalogs[0].Models[0].ContextWindow != LocalContextWindow {
		t.Fatalf("local catalog = %#v", catalogs[0])
	}
}

func TestProviderCatalogPrefersNativeRoute(t *testing.T) {
	catalogs := []Catalog{
		{Name: "openrouter", Models: []ModelInfo{{ID: "deepseek/deepseek-chat"}}},
		{Name: "deepseek", Models: []ModelInfo{{ID: "deepseek/deepseek-chat"}}},
	}
	markPreferred(catalogs)
	if !catalogs[1].Models[0].Preferred || catalogs[0].Models[0].Preferred {
		t.Fatalf("native route was not preferred: %#v", catalogs)
	}
}

func TestToolCallMessageIncludesEmptyContent(t *testing.T) {
	data, err := json.Marshal(Message{Role: "assistant", ToolCalls: []ToolCall{{ID: "call-1", Type: "function"}}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"content":""`) {
		t.Fatalf("tool-call message omitted content: %s", data)
	}
}

func TestToolCallMessageRetainsReasoningContent(t *testing.T) {
	data, err := json.Marshal(Message{Role: "assistant", ReasoningContent: "plan", ToolCalls: []ToolCall{{ID: "call-1", Type: "function"}}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"reasoning_content":"plan"`) {
		t.Fatalf("tool-call message omitted reasoning content: %s", data)
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

func TestPinnedContextWindow(t *testing.T) {
	// Both spellings that address the plan route today carry the pin.
	for _, model := range []string{"zai/glm-5.3-flash", "glm-5.3-flash"} {
		window, ok := PinnedContextWindow(model)
		if !ok {
			t.Fatalf("%q is not pinned", model)
		}
		if window != 1000000 {
			t.Fatalf("%q pinned at %d, want 1000000", model, window)
		}
	}

	// Surrounding whitespace is normalized away, as it is everywhere else a
	// model name is accepted.
	if window, ok := PinnedContextWindow("  zai/glm-5.3-flash  "); !ok || window != 1000000 {
		t.Fatalf("padded slug pinned at %d (ok=%v), want 1000000", window, ok)
	}

	// The `[1m]` spellings Z.ai's own Claude Code guide publishes are rejected
	// 1211 on both wires, so they are not aliases and must not be pinned. A
	// pin invented for them would size a window for a model that cannot run.
	for _, model := range []string{"zai/glm-5.3-flash[1m]", "glm-5.3-flash[1m]", "zai/glm-5.3[1m]"} {
		if window, ok := PinnedContextWindow(model); ok {
			t.Fatalf("%q must not be pinned, got %d", model, window)
		}
	}

	// Unpinned routes report no pin rather than a zero window, so the caller
	// can tell "no pin" from "pinned at nothing".
	for _, model := range []string{"deepseek-v4-flash", LocalModelID, "vendor/model", ""} {
		if window, ok := PinnedContextWindow(model); ok {
			t.Fatalf("%q must not be pinned, got %d", model, window)
		}
	}
}

func TestRouteKeyNormalizesEverySpellingOfARoute(t *testing.T) {
	// Many spellings collapse onto one authoritative key. This is the whole
	// point of deriving it in one place: policy lookup, the effort map and the
	// context pin would otherwise each normalize, and could disagree.
	for _, tc := range []struct{ model, want string }{
		{"zai/glm-5.3-flash", "zai/glm-5.3-flash"},
		{"glm-5.3-flash", "zai/glm-5.3-flash"},
		{"  glm-5.3-flash  ", "zai/glm-5.3-flash"},
		{"glm-5.3", "zai/glm-5.3"},
		{LocalModelID, LocalModelID},
		{localWireModelID, LocalModelID},
		{"deepseek-v4-flash", "deepseek-v4-flash"},
		// NormalizeModel's existing alias folding still applies underneath.
		{"deepseek/deepseek-v4.1-flash", "deepseek-v4-flash"},
		{"deepseek-v4.1-flash", "deepseek-v4-flash"},
		// The OpenRouter namespace is a different route to the same model and
		// keeps its own key, so phase 3 can carry a posture for it.
		{"z-ai/glm-5.3-flash", "z-ai/glm-5.3-flash"},
	} {
		got, ok := RouteKey(tc.model)
		if !ok {
			t.Fatalf("RouteKey(%q) reported no route", tc.model)
		}
		if got != tc.want {
			t.Fatalf("RouteKey(%q) = %q, want %q", tc.model, got, tc.want)
		}
	}

	// A `[1m]` spelling is rejected 1211 on both wires, so it is not an alias
	// and must keep a key of its own rather than being folded into the plain
	// slug. Folding it would route a model that cannot run and hand it a pin.
	for _, model := range []string{"glm-5.3-flash[1m]", "zai/glm-5.3-flash[1m]", "glm-5.3[1m]"} {
		got, ok := RouteKey(model)
		if !ok {
			t.Fatalf("RouteKey(%q) reported no route", model)
		}
		if got == "zai/glm-5.3-flash" || got == "zai/glm-5.3" {
			t.Fatalf("RouteKey(%q) = %q: a [1m] spelling was normalized into the plain slug", model, got)
		}
		if _, pinned := PinnedContextWindow(model); pinned {
			t.Fatalf("%q must not inherit the plan route's context pin", model)
		}
	}

	// Only a name that addresses nothing reports false.
	for _, model := range []string{"", "   "} {
		if got, ok := RouteKey(model); ok {
			t.Fatalf("RouteKey(%q) = %q, want no route", model, got)
		}
	}
}

func TestRouteKeyAndWireModelAreInverse(t *testing.T) {
	// The other direction: the authoritative key maps back to the id each
	// route puts on the wire. The plan route sends the bare slug (fact 11),
	// and OpenRouter sends its own namespaced spelling.
	t.Setenv("ZAI_API_KEY", "test-key-not-a-credential")
	p, err := ForModel("glm-5.3-flash", "https://openrouter.ai/api/v1/chat/completions", false)
	if err != nil {
		t.Fatal(err)
	}
	if p.Route.Key != "zai/glm-5.3-flash" {
		t.Fatalf("route key = %q, want zai/glm-5.3-flash", p.Route.Key)
	}
	if got := p.modelID(p.Route.Key); got != "glm-5.3-flash" {
		t.Fatalf("plan wire model = %q, want glm-5.3-flash", got)
	}
	if got := (&HTTPProvider{Flavor: "openrouter"}).modelID("deepseek-v4-flash"); got != "deepseek/deepseek-v4-flash" {
		t.Fatalf("openrouter wire model = %q, want deepseek/deepseek-v4-flash", got)
	}
}

func TestResolveRouteRefusesRatherThanFallingThroughToOpenRouter(t *testing.T) {
	// The fail-open defect this replaces: ForModel defaulted to OpenRouter and
	// left it only if a native key happened to be present, so an absent
	// ZAI_API_KEY silently redirected a plan model to OpenRouter.
	t.Setenv("ZAI_API_KEY", "")
	t.Setenv("OPENROUTER_API_KEY", "test-key-not-a-credential")
	for _, model := range []string{"zai/glm-5.3-flash", "glm-5.3-flash"} {
		route, err := ResolveRoute(model, "https://openrouter.ai/api/v1/chat/completions", false)
		if err == nil {
			t.Fatalf("ResolveRoute(%q) returned flavor %q instead of refusing", model, route.Flavor)
		}
		if !strings.Contains(err.Error(), "ZAI_API_KEY") {
			t.Fatalf("refusal for %q does not name the missing key: %v", model, err)
		}
	}

	// The same refusal for the other native family.
	t.Setenv("DEEPSEEK_API_KEY", "")
	if _, err := ResolveRoute("deepseek-v4-flash", "https://openrouter.ai/api/v1/chat/completions", false); err == nil {
		t.Fatal("a deepseek model with no DEEPSEEK_API_KEY did not refuse")
	}
}

func TestResolveRouteHonoursAnExplicitEndpointOverride(t *testing.T) {
	// An endpoint the operator set deliberately is the sanctioned escape from
	// the refusal. The identical value arrived at by default is not, which is
	// why provenance and not the value is what routing consults.
	t.Setenv("ZAI_API_KEY", "")
	t.Setenv("OPENROUTER_API_KEY", "test-key-not-a-credential")
	const endpoint = "https://openrouter.ai/api/v1/chat/completions"

	if _, err := ResolveRoute("zai/glm-5.3-flash", endpoint, false); err == nil {
		t.Fatal("a defaulted endpoint must not override a native route")
	}
	route, err := ResolveRoute("zai/glm-5.3-flash", endpoint, true)
	if err != nil {
		t.Fatalf("an explicit endpoint must override: %v", err)
	}
	if route.Flavor != "openrouter" || route.Endpoint != endpoint {
		t.Fatalf("override route = %#v", route)
	}
	// The pin belongs to the plan route, not to this one. Carrying it across
	// would size a 1,000,000-token window for an endpoint that never agreed to
	// serve one.
	if route.ContextWindow != 0 {
		t.Fatalf("override route kept the plan pin: %d", route.ContextWindow)
	}
}

func TestResolveRouteRefusesAnUnroutableModel(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "test-key-not-a-credential")
	// No model at all addresses no route.
	if _, err := ResolveRoute("  ", "https://openrouter.ai/api/v1/chat/completions", false); err == nil {
		t.Fatal("an empty model did not refuse")
	}
	// A non-native model with no endpoint has nowhere to go.
	if _, err := ResolveRoute("vendor/model", "", false); err == nil {
		t.Fatal("a model with no native route and no endpoint did not refuse")
	}
	// A non-native model with an endpoint but no credential refuses at routing
	// time rather than deferring the failure to the first request.
	t.Setenv("OPENROUTER_API_KEY", "")
	if _, err := ResolveRoute("vendor/model", "https://openrouter.ai/api/v1/chat/completions", false); err == nil {
		t.Fatal("a model with no OPENROUTER_API_KEY did not refuse")
	}
}

func TestResolveRouteKeepsTheLocalRouteKeylessAndUnpinned(t *testing.T) {
	// The local route sends no credential by design and must keep routing with
	// every provider key absent: fail-closed is about credentials a route
	// needs, and this one needs none.
	t.Setenv("SLBH_LOCAL_ENDPOINT", "http://127.0.0.1:11434/v1/chat/completions")
	t.Setenv("OPENROUTER_API_KEY", "")
	t.Setenv("ZAI_API_KEY", "")
	t.Setenv("DEEPSEEK_API_KEY", "")
	route, err := ResolveRoute(LocalModelID, "", false)
	if err != nil {
		t.Fatalf("local route refused: %v", err)
	}
	if route.Flavor != LocalProviderName || route.APIKey != "" {
		t.Fatalf("local route = %#v", route)
	}
	if route.Endpoint != "http://127.0.0.1:11434/v1/chat/completions" {
		t.Fatalf("local endpoint = %q", route.Endpoint)
	}
	// The local window comes from the provider's own catalog answer, not from
	// the pin table, so phases 0-3 leave that route's sizing untouched.
	if route.ContextWindow != 0 {
		t.Fatalf("local route carries a pin: %d", route.ContextWindow)
	}
}
