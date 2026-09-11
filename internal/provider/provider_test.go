package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
