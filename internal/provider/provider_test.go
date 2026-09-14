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
	p, err := ForModel(LocalModelID, "https://example.invalid/v1/chat/completions")
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
