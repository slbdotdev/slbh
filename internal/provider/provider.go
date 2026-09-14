package provider

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

type Message struct {
	Role string `json:"role"`
	// Content must remain present even for assistant messages that contain
	// only tool_calls. DeepSeek rejects those messages when content is omitted.
	Content string `json:"content"`
	// ReasoningContent is returned by thinking models and must be replayed on
	// assistant continuations, especially when the response contains tools.
	ReasoningContent string     `json:"reasoning_content,omitempty"`
	Name             string     `json:"name,omitempty"`
	ToolCallID       string     `json:"tool_call_id,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
}

type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

type Request struct {
	Model       string
	Effort      string
	System      string
	Messages    []Message
	Tools       []Tool
	CacheKey    string
	Temperature *float64
}

// RequestPayloadProvider exposes the exact JSON payload that a provider will
// send for an inference request. The harness uses this to make transcript
// request records byte-replayable without recording credentials or headers.
type RequestPayloadProvider interface {
	RequestPayload(Request) ([]byte, error)
}

type wireRequest struct {
	Model            string        `json:"model"`
	Stream           bool          `json:"stream"`
	Messages         []Message     `json:"messages"`
	ReasoningEffort  string        `json:"reasoning_effort,omitempty"`
	IncludeReasoning bool          `json:"include_reasoning,omitempty"`
	PromptCacheKey   string        `json:"prompt_cache_key"`
	Temperature      *float64      `json:"temperature,omitempty"`
	StreamOptions    streamOptions `json:"stream_options"`
	Tools            []wireTool    `json:"tools,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type wireTool struct {
	Type     string           `json:"type"`
	Function wireToolFunction `json:"function"`
}

type wireToolFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

type EventKind string

const (
	EventText      EventKind = "text"
	EventReasoning EventKind = "reasoning"
	EventTool      EventKind = "tool"
	EventUsage     EventKind = "usage"
	EventError     EventKind = "error"
	EventDone      EventKind = "done"
)

type Event struct {
	Kind       EventKind
	Text       string
	ToolName   string
	ToolCallID string
	ToolIndex  int
	Input      string
	Usage      map[string]any
	Err        error
}

type StreamSink func(Event) error

type Provider interface {
	Stream(context.Context, Request, StreamSink) error
}

// ContextWindowProvider is optional because not every OpenAI-compatible API
// exposes model metadata. Callers must use FallbackContextWindow when a
// provider does not implement this capability or returns no usable value.
type ContextWindowProvider interface {
	ContextWindow(context.Context, string) (int, error)
}

const FallbackContextWindow = 128000

// contextWindowPins is the route-keyed context window table. A pinned route is
// one whose real ceiling cannot be discovered: neither Z.ai catalog publishes a
// context length at all (measured 2026-09-13, fact 2 of
// org/slbh-capability-matrix-2026-09-13.md), so ContextWindow errors on that
// route and FallbackContextWindow is what runs — understating a measured
// ~1,000,605-token ceiling by a factor of eight, and compacting a
// zai/glm-5.3-flash agent at roughly 89,600 tokens.
//
// This table is deliberately temporary. A context window is a capability fact
// about an endpoint rather than routing or posture policy, so pinning one here
// does not breach the ruling against compiled-in policy, and it fails safe in
// the only direction that matters: too small merely compacts early. The managed
// policy file replaces this source in phase 1c, which deletes this map. It must
// not outlive that phase.
//
// Keys are authoritative route keys, so the bare `glm-5.3-flash` spelling
// needs no entry of its own: RouteKey folds it into the canonical one. The
// `[1m]` variants Z.ai's own Claude Code guide publishes are rejected 1211 on
// both wires (fact 11), so they are not aliases of the plain slug and must
// never resolve to this pin.
var contextWindowPins = map[string]int{
	"zai/glm-5.3-flash": 1000000,
}

// PinnedContextWindow reports the pinned context window for a model route. A
// pinned window is authoritative over both catalog discovery and
// FallbackContextWindow, because a route is pinned precisely when discovery
// cannot answer for it.
//
// This keys on the authoritative route key, so every spelling that addresses
// the route resolves to the same pin and no caller normalizes separately.
func PinnedContextWindow(model string) (int, bool) {
	key, ok := RouteKey(model)
	if !ok {
		return 0, false
	}
	window, found := contextWindowPins[key]
	return window, found
}

// Route is the resolved identity of one provider route: the authoritative key
// that policy lookup, the effort map and the context pin all key on, together
// with the transport facts needed to reach it and the policy that governs it.
//
// A provider instance carries its own Route, so nothing downstream re-derives
// a route from a model string. That distinction is load-bearing rather than
// tidy: the same model name reaches different endpoints depending on which
// credentials are present and whether an endpoint override was set, so a pin
// or a posture keyed on the name alone can describe a route the request did
// not actually take.
type Route struct {
	// Key is the authoritative route key, such as "zai/glm-5.3-flash".
	Key string
	// Flavor is the provider family: zai, deepseek, openrouter or local.
	Flavor string
	// Endpoint is the URL this route posts inference to.
	Endpoint string
	// APIKey is this route's credential. Empty only for the local route,
	// which deliberately sends none.
	APIKey string
	// ContextWindow is this route's pinned window, zero when unpinned. Phase
	// 1c replaces the source of this value with the managed policy file.
	ContextWindow int
}

// RouteKey derives the authoritative route key for a model name. Every
// spelling that addresses the same route collapses to one key here, so policy
// lookup, the effort map and the context pin never each normalize separately.
// It reports false only for a name that addresses nothing at all.
//
// It deliberately does not invent the `[1m]` spellings. Z.ai's own Claude Code
// guide publishes `glm-5.3-flash[1m]`, and both wires reject it with code 1211
// (measured 2026-09-13, fact 11), so it is not an alias of the plain slug and
// must never be folded into one.
func RouteKey(model string) (string, bool) {
	model = NormalizeModel(model)
	if model == "" {
		return "", false
	}
	switch {
	case strings.HasPrefix(model, LocalProviderName+"/"):
		return model, true
	case strings.EqualFold(model, localWireModelID):
		return LocalProviderName + "/" + localWireModelID, true
	case strings.HasPrefix(model, "glm-"):
		// The bare plan slug addresses the plan route; the `zai/` spelling is
		// canonical so both reach one policy entry.
		return "zai/" + model, true
	}
	return model, true
}

// nativeRoute is the fixed transport for one native provider family.
type nativeRoute struct {
	flavor   string
	endpoint string
	keyEnv   string
}

// nativeRouteFor matches an authoritative route key to its native family. It
// matches on the canonical key alone, which is why the bare `glm-` spelling
// does not appear here: RouteKey has already folded it into `zai/`.
func nativeRouteFor(key string) (nativeRoute, bool) {
	switch {
	case strings.HasPrefix(key, LocalProviderName+"/"):
		return nativeRoute{flavor: LocalProviderName}, true
	case strings.HasPrefix(key, "deepseek/") || strings.HasPrefix(key, "deepseek-"):
		return nativeRoute{flavor: "deepseek", endpoint: "https://api.deepseek.com/chat/completions", keyEnv: "DEEPSEEK_API_KEY"}, true
	case strings.HasPrefix(key, "zai/"):
		return nativeRoute{flavor: "zai", endpoint: "https://api.z.ai/api/coding/paas/v4/chat/completions", keyEnv: "ZAI_API_KEY"}, true
	}
	return nativeRoute{}, false
}

// ResolveRoute derives the authoritative route for a model and fails closed.
//
// Routing used to default to OpenRouter and leave it only when a native key
// happened to be present, so an absent ZAI_API_KEY silently redirected a plan
// model to OpenRouter. That is fail-open, and it is against the standing rule
// that a model reachable on a plan never runs through OpenRouter. A native
// model whose key is missing now refuses instead.
//
// endpointExplicit is the one escape: an endpoint the operator set
// deliberately is honoured as an override, where the same value arrived at by
// default is not. Without that distinction the refusal could not tell an
// intentional override from the stock OpenRouter default and would either
// never fire or override the operator.
func ResolveRoute(model, endpoint string, endpointExplicit bool) (Route, error) {
	key, ok := RouteKey(model)
	if !ok {
		return Route{}, fmt.Errorf("no model specified")
	}
	route := Route{Key: key, ContextWindow: contextWindowPins[key]}

	if native, isNative := nativeRouteFor(key); isNative {
		if native.flavor == LocalProviderName {
			route.Flavor, route.Endpoint = LocalProviderName, localEndpoint()
			return route, nil
		}
		if apiKey := os.Getenv(native.keyEnv); apiKey != "" {
			route.Flavor, route.Endpoint, route.APIKey = native.flavor, native.endpoint, apiKey
			return route, nil
		}
		if !endpointExplicit {
			return Route{}, fmt.Errorf(
				"model %q routes to the %s endpoint but %s is not set: refusing to fall through to OpenRouter, because a model reachable on a plan never runs through it (set %s, or set SLBH_ENDPOINT deliberately to override)",
				model, native.flavor, native.keyEnv, native.keyEnv)
		}
		// An explicit endpoint override was set: fall through to it below. The
		// route is no longer the native one, so it keeps no native pin.
		route.ContextWindow = 0
	}

	if strings.TrimSpace(endpoint) == "" {
		return Route{}, fmt.Errorf("model %q has no route: no native provider matched and no endpoint is configured", model)
	}
	apiKey := os.Getenv("OPENROUTER_API_KEY")
	if apiKey == "" {
		return Route{}, fmt.Errorf("model %q routes to %s but OPENROUTER_API_KEY is not set", model, endpoint)
	}
	route.Flavor, route.Endpoint, route.APIKey = "openrouter", endpoint, apiKey
	return route, nil
}

const (
	LocalProviderName  = "local"
	LocalModelID       = "local/q27-IQ2_M-96k"
	localWireModelID   = "q27-IQ2_M-96k"
	LocalContextWindow = 98304
	localDefaultURL    = "http://fractal.wyvern-temperature.ts.net:11434/v1/chat/completions"
)

func localEndpoint() string {
	if endpoint := strings.TrimSpace(os.Getenv("SLBH_LOCAL_ENDPOINT")); endpoint != "" {
		return endpoint
	}
	return localDefaultURL
}

type HTTPProvider struct {
	Endpoint string
	APIKey   string
	Client   *http.Client
	Flavor   string
	// Route is the resolved route this instance serves, carrying its own
	// policy. An instance built by ForModel always has one; one built directly
	// by NewHTTP has the zero Route and therefore no pinned window.
	Route          Route
	metadataMu     sync.Mutex
	contextWindows map[string]int
}

// RoutePolicyProvider is implemented by a provider instance that carries its
// own route's policy. It is optional because not every Provider is
// route-resolved: test fakes and wrappers need not carry a route.
//
// The harness asks the instance rather than looking a pin up by model name,
// because only the instance knows which endpoint the request will actually
// reach. A model name that usually addresses the plan reaches OpenRouter under
// an explicit endpoint override, and a pin keyed on the name would then
// describe the wrong route.
type RoutePolicyProvider interface {
	PinnedContextWindow() (int, bool)
}

// PinnedContextWindow reports this route's pinned context window.
func (p *HTTPProvider) PinnedContextWindow() (int, bool) {
	if p.Route.ContextWindow > 0 {
		return p.Route.ContextWindow, true
	}
	return 0, false
}

func NewHTTP(endpoint, key string) *HTTPProvider {
	return &HTTPProvider{Endpoint: endpoint, APIKey: key, Client: &http.Client{Timeout: 0}, contextWindows: make(map[string]int)}
}

// ForModel builds the provider for a model's authoritative route. It fails
// closed: a model whose native key is absent refuses rather than silently
// falling through to OpenRouter. endpointExplicit distinguishes an endpoint the
// operator set from one that merely defaulted, and only the former overrides a
// native route.
func ForModel(model, endpointOverride string, endpointExplicit bool) (*HTTPProvider, error) {
	route, err := ResolveRoute(model, endpointOverride, endpointExplicit)
	if err != nil {
		return nil, err
	}
	provider := NewHTTP(route.Endpoint, route.APIKey)
	provider.Flavor = route.Flavor
	provider.Route = route
	return provider, nil
}

// NormalizeModel keeps the old draft spelling from producing a provider 400
// while leaving user-selected model names untouched otherwise.
func NormalizeModel(model string) string {
	model = strings.TrimSpace(model)
	switch model {
	case "deepseek/deepseek-v4.1-flash", "deepseek-v4.1-flash":
		return "deepseek-v4-flash"
	default:
		return model
	}
}

func (p *HTTPProvider) modelID(model string) string {
	model = NormalizeModel(model)
	switch p.Flavor {
	case LocalProviderName:
		return strings.TrimPrefix(model, LocalProviderName+"/")
	case "deepseek":
		return strings.TrimPrefix(model, "deepseek/")
	case "zai":
		return strings.TrimPrefix(model, "zai/")
	case "openrouter":
		if strings.HasPrefix(model, "deepseek-") {
			return "deepseek/" + model
		}
	}
	return model
}

// ContextWindow discovers the active model's context length from the
// provider's live model catalog. The value is cached on this provider instance
// so a runtime does not repeatedly fetch metadata for the same model.
func (p *HTTPProvider) ContextWindow(ctx context.Context, model string) (int, error) {
	wanted := p.modelID(model)
	if p.Flavor == LocalProviderName && strings.EqualFold(wanted, localWireModelID) {
		return LocalContextWindow, nil
	}
	p.metadataMu.Lock()
	if p.contextWindows == nil {
		p.contextWindows = make(map[string]int)
	}
	if window := p.contextWindows[wanted]; window > 0 {
		p.metadataMu.Unlock()
		return window, nil
	}
	p.metadataMu.Unlock()

	if p.APIKey == "" {
		return 0, fmt.Errorf("provider API key is not configured")
	}
	endpoint, err := modelsEndpoint(p.Endpoint)
	if err != nil {
		return 0, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, err
	}
	request.Header.Set("Authorization", "Bearer "+p.APIKey)
	request.Header.Set("Accept", "application/json")
	client := p.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(request)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 8*1024))
		return 0, fmt.Errorf("model metadata returned %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	var catalog struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8*1024*1024)).Decode(&catalog); err != nil {
		return 0, fmt.Errorf("decode model metadata: %w", err)
	}
	models, err := decodeModels(catalog.Data)
	if err != nil {
		return 0, err
	}
	for _, metadata := range models {
		if !sameModelID(wanted, metadata.ID) {
			continue
		}
		window := metadata.ContextLength
		if window <= 0 {
			window = metadata.TopProvider.ContextLength
		}
		if window <= 0 {
			return 0, fmt.Errorf("model %q has no usable context_length", wanted)
		}
		p.metadataMu.Lock()
		p.contextWindows[wanted] = window
		p.metadataMu.Unlock()
		return window, nil
	}
	return 0, fmt.Errorf("model %q was not found in provider metadata", wanted)
}

type modelMetadata struct {
	ID            string `json:"id"`
	Created       int64  `json:"created"`
	ContextLength int    `json:"context_length"`
	TopProvider   struct {
		ContextLength int `json:"context_length"`
	} `json:"top_provider"`
}

func decodeModels(data json.RawMessage) ([]modelMetadata, error) {
	data = bytes.TrimSpace(data)
	var models []modelMetadata
	if len(data) > 0 && data[0] == '[' {
		if err := json.Unmarshal(data, &models); err != nil {
			return nil, fmt.Errorf("decode model metadata list: %w", err)
		}
		return models, nil
	}
	var model modelMetadata
	if err := json.Unmarshal(data, &model); err != nil {
		return nil, fmt.Errorf("decode model metadata item: %w", err)
	}
	return []modelMetadata{model}, nil
}

func modelsEndpoint(endpoint string) (string, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("parse provider endpoint: %w", err)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("provider endpoint must be an absolute URL")
	}
	path := strings.TrimSuffix(parsed.Path, "/")
	for _, suffix := range []string{"/chat/completions", "/completions"} {
		if strings.HasSuffix(path, suffix) {
			path = strings.TrimSuffix(path, suffix)
			break
		}
	}
	parsed.Path = strings.TrimSuffix(path, "/") + "/models"
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}

func sameModelID(wanted, candidate string) bool {
	wanted = strings.ToLower(strings.TrimSpace(wanted))
	candidate = strings.ToLower(strings.TrimSpace(candidate))
	if wanted == candidate {
		return true
	}
	// Provider variants such as :free or :thinking share the base metadata.
	return strings.SplitN(wanted, ":", 2)[0] == strings.SplitN(candidate, ":", 2)[0]
}

// RequestPayload returns the exact request body used by Stream. It is kept as
// one function so transcript records and the actual HTTP request cannot drift.
func (p *HTTPProvider) RequestPayload(req Request) ([]byte, error) {
	if req.CacheKey == "" {
		req.CacheKey = StablePrefixKey(req)
	}
	body := wireRequest{
		Model:            p.modelID(req.Model),
		Stream:           true,
		Messages:         append([]Message{{Role: "system", Content: req.System}}, req.Messages...),
		ReasoningEffort:  req.Effort,
		IncludeReasoning: req.Effort != "",
		PromptCacheKey:   req.CacheKey,
		Temperature:      req.Temperature,
		StreamOptions:    streamOptions{IncludeUsage: true},
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

// RequestFromPayload reconstructs the provider request context from a
// transcripted wire payload. The returned Messages omit the leading system
// message because Request stores that context separately.
func RequestFromPayload(payload []byte) (Request, error) {
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

func (p *HTTPProvider) Stream(ctx context.Context, req Request, sink StreamSink) error {
	if p.Flavor != LocalProviderName && p.APIKey == "" {
		return fmt.Errorf("provider API key is not configured (set OPENROUTER_API_KEY, DEEPSEEK_API_KEY, or ZAI_API_KEY)")
	}
	// OpenRouter accepts prompt_cache_key; native providers safely ignore the
	// extra metadata in their compatible endpoint. The prefix itself is kept
	// stable by Runtime and is never mixed with user turns.
	payload, err := p.RequestPayload(req)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, p.Endpoint, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	if p.APIKey != "" {
		request.Header.Set("Authorization", "Bearer "+p.APIKey)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "text/event-stream")
	resp, err := p.Client.Do(request)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 32*1024))
		return fmt.Errorf("provider returned %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	if err := parseSSE(resp.Body, sink); err != nil {
		return err
	}
	return sink(Event{Kind: EventDone})
}

func StablePrefixKey(req Request) string {
	// Hash the complete stable prefix, including tool descriptions. User turns
	// are deliberately absent so the key remains reusable across a session.
	encoded, _ := json.Marshal(struct {
		Model  string `json:"model"`
		System string `json:"system"`
		Tools  []Tool `json:"tools"`
	}{Model: req.Model, System: req.System, Tools: req.Tools})
	sum := sha256.Sum256(encoded)
	return "slbh-" + hex.EncodeToString(sum[:])
}

func parseSSE(reader io.Reader, sink StreamSink) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 16*1024), 2*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
					Reasoning        string `json:"reasoning"`
					ToolCalls        []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
			Usage map[string]any `json:"usage"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return fmt.Errorf("decode provider event: %w", err)
		}
		if chunk.Error != nil {
			return fmt.Errorf("provider stream error: %s", chunk.Error.Message)
		}
		for _, choice := range chunk.Choices {
			if choice.Delta.Content != "" {
				if err := sink(Event{Kind: EventText, Text: choice.Delta.Content}); err != nil {
					return err
				}
			}
			reasoning := choice.Delta.ReasoningContent
			if reasoning == "" {
				reasoning = choice.Delta.Reasoning
			}
			if reasoning != "" {
				if err := sink(Event{Kind: EventReasoning, Text: reasoning}); err != nil {
					return err
				}
			}
			for _, call := range choice.Delta.ToolCalls {
				if err := sink(Event{Kind: EventTool, ToolName: call.Function.Name, ToolCallID: call.ID, ToolIndex: call.Index, Input: call.Function.Arguments}); err != nil {
					return err
				}
			}
		}
		if len(chunk.Usage) > 0 {
			if err := sink(Event{Kind: EventUsage, Usage: chunk.Usage}); err != nil {
				return err
			}
		}
	}
	return scanner.Err()
}

// Retry executes an operation with short stepped delays. It intentionally
// leaves cancellation to the caller and never retries context cancellation.
func Retry(ctx context.Context, attempts int, fn func() error) error {
	if attempts < 1 {
		attempts = 1
	}
	var err error
	for attempt := 0; attempt < attempts; attempt++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		err = fn()
		if err == nil {
			return nil
		}
		if attempt+1 < attempts {
			delay := time.Duration(attempt+1) * 300 * time.Millisecond
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
	}
	return err
}
