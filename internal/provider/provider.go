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
	// Provider is OpenRouter's routing object, and it is populated for the
	// openrouter route and for no other. It is what decides which upstream
	// endpoint serves a request and on what terms, and since the pi-run
	// preflight was retired on 2026-09-10 it is the entire enforcement of that
	// posture — nothing checks it before a launch any more, so it holds
	// because it is in the body that goes out.
	Provider *wireProviderObject `json:"provider,omitempty"`
}

// wireProviderObject is the `provider` field OpenRouter reads. Its shape is
// OpenRouter's, not ours, so the JSON names are theirs.
type wireProviderObject struct {
	ZDR            bool          `json:"zdr"`
	DataCollection string        `json:"data_collection"`
	Sort           string        `json:"sort,omitempty"`
	Ignore         []string      `json:"ignore,omitempty"`
	MaxPrice       *wireMaxPrice `json:"max_price,omitempty"`
}

type wireMaxPrice struct {
	Prompt     float64 `json:"prompt"`
	Completion float64 `json:"completion"`
}

// providerObjectFor renders a route's posture into the wire object, and
// returns nil for every route that is not the OpenRouter one.
//
// `zdr` and `data_collection` are OpenRouter concepts and say nothing about
// the Z.ai plan endpoint or a server on the house network, so sending them
// there would be noise at best and a false record of a guarantee at worst.
func providerObjectFor(route Route) *wireProviderObject {
	if route.Flavor != "openrouter" || route.Policy.Provider == nil {
		return nil
	}
	posture := route.Policy.Provider
	object := &wireProviderObject{
		DataCollection: posture.DataCollection,
		Sort:           posture.Sort,
	}
	if posture.ZDR != nil {
		object.ZDR = *posture.ZDR
	}
	if len(posture.Ignore) > 0 {
		object.Ignore = append([]string(nil), posture.Ignore...)
	}
	if posture.MaxPrice != nil {
		object.MaxPrice = &wireMaxPrice{Prompt: posture.MaxPrice.Prompt, Completion: posture.MaxPrice.Completion}
	}
	return object
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
	// ToolIndex is a dense tool ordinal from 0, normalized inside this
	// package. It is never the wire's own index: on the Anthropic wire that is
	// a content-block position offset by the thinking block, so tools start at
	// 1 there. Nothing downstream may treat it as an array position.
	ToolIndex int
	Input     string
	Usage     map[string]any
	// StopReason is the terminal reason in the coding wire's vocabulary —
	// `stop`, `tool_calls` — whichever wire served the request. It rides the
	// usage event, which is the last thing a stream emits.
	StopReason string
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

// OpenRouterEndpoint is the stock chat-completions endpoint, and the default
// value of SLBH_ENDPOINT. It lives here rather than in config so the policy
// author and the route resolver cannot disagree about the URL.
const OpenRouterEndpoint = "https://openrouter.ai/api/v1/chat/completions"

// openRouterEndpoint is the unexported spelling used inside this package.
const openRouterEndpoint = OpenRouterEndpoint

// The compiled-in context pin table that step one introduced was deleted in
// phase 1c, which is where the plan said it must not outlive. A route's window
// now comes from the routing policy's `contextWindow`, carried on Route and
// read back through HTTPProvider.PinnedContextWindow. The resolution path
// itself — pin beats discovery beats fallback, in resolveContextWindow — is
// unchanged, because that path was always the substance and the table never
// was.

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
	// which deliberately sends none. It is never read from the policy file:
	// route-to-credential mapping is compiled in, so no document can point a
	// key at an endpoint it was not minted for.
	APIKey string
	// ContextWindow is this route's pinned window, zero when unpinned. Its
	// source is the routing policy since phase 1c.
	ContextWindow int
	// Wire is the protocol this route speaks. Only a wire this build supports
	// ever reaches a Route: an unsupported one refuses at resolution.
	Wire string
	// Policy is this route's whole policy entry, carried so the instance can
	// answer for its own effort map and posture without a second lookup by
	// model name — which could describe a route the request did not take.
	Policy RoutePolicy
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
//
// The policy is the suspenders, and they are what make this total. A route
// with no policy entry refuses; a policy that is absent from both sources
// refuses everything; a route configured for a wire this build cannot speak
// refuses by name. What is compiled in is the requirement that policy exist
// and the mapping from a route to its credential — never the policy itself,
// which must change by converge rather than by a rebuild.
func ResolveRoute(model, endpoint string, endpointExplicit bool, policy Policy) (Route, error) {
	key, ok := RouteKey(model)
	if !ok {
		return Route{}, fmt.Errorf("no model specified")
	}
	if !policy.Defined() {
		return Route{}, fmt.Errorf(
			"model %q cannot be routed: no routing policy is in force. A managed policy.json is deployed by ansible into $SLBH_HOME; on an unmanaged host, author one from /models. Refusing rather than routing without a posture",
			model)
	}
	entry, found := policy.Route(key)
	if !found {
		return Route{}, fmt.Errorf(
			"route %q has no entry in the routing policy in force: refusing to route a model the policy does not describe. Add the route to the managed policy, or author one from /models",
			key)
	}
	if !WireSupported(entry.Wire) {
		return Route{}, fmt.Errorf(
			"route %q is configured for wire %q, which this build cannot speak: refusing rather than sending it down another wire's encoder, which these endpoints would answer 200 to. The policy and the binary that implements its wire must ship together",
			key, entry.Wire)
	}
	route := Route{Key: key, ContextWindow: entry.ContextWindow, Wire: entry.Wire, Policy: entry}
	// viaOverride records that a native route was redirected by an explicit
	// SLBH_ENDPOINT rather than being an OpenRouter route in its own right.
	// The two are not the same thing and phase 3's posture requirement applies
	// to one of them only.
	viaOverride := false

	if native, isNative := nativeRouteFor(key); isNative {
		if native.flavor == LocalProviderName {
			// The policy names the local endpoint, and SLBH_LOCAL_ENDPOINT
			// still overrides it — the same environment-over-file precedence
			// the model and effort settings have, kept because that variable
			// is how a developer points slbh at a loopback Ollama without
			// rewriting a policy file.
			route.Flavor, route.Endpoint = LocalProviderName, entry.Endpoint
			if override := strings.TrimSpace(os.Getenv("SLBH_LOCAL_ENDPOINT")); override != "" {
				route.Endpoint = override
			}
			return route, nil
		}
		if apiKey := os.Getenv(native.keyEnv); apiKey != "" {
			route.Flavor, route.Endpoint, route.APIKey = native.flavor, entry.Endpoint, apiKey
			return route, nil
		}
		if !endpointExplicit {
			return Route{}, fmt.Errorf(
				"model %q routes to the %s endpoint but %s is not set: refusing to fall through to OpenRouter, because a model reachable on a plan never runs through it (set %s, or set SLBH_ENDPOINT deliberately to override)",
				model, native.flavor, native.keyEnv, native.keyEnv)
		}
		// An explicit endpoint override was set: fall through to it below. The
		// route is no longer the one the policy entry describes, so the pin
		// goes — a window measured on the plan endpoint says nothing about
		// wherever the operator has pointed this.
		route.ContextWindow = 0
		viaOverride = true
	}

	// The policy names this route's endpoint. An explicit SLBH_ENDPOINT still
	// overrides it, which is the deliberate escape hatch banked decision 5
	// preserved, and it drops the pin for the same reason as above.
	target := entry.Endpoint
	if endpointExplicit && strings.TrimSpace(endpoint) != "" {
		target = endpoint
		route.ContextWindow = 0
	}
	if strings.TrimSpace(target) == "" {
		return Route{}, fmt.Errorf("route %q has no endpoint in the routing policy", key)
	}
	apiKey := os.Getenv("OPENROUTER_API_KEY")
	if apiKey == "" {
		return Route{}, fmt.Errorf("model %q routes to %s but OPENROUTER_API_KEY is not set", model, target)
	}
	// The refusal is total, and this is the half phase 3 adds. A route with no
	// entry already refused in phase 1c; a route whose entry describes no
	// posture refuses here. Routing to OpenRouter without stating zdr and
	// data_collection would send a request whose upstream is chosen on price
	// and availability alone, which is the exact guarantee this document
	// exists to carry — and nothing downstream would ever report its absence,
	// because a request with no `provider` object looks like a normal one.
	//
	// This is a per-route refusal rather than a whole-document one, on the
	// same reasoning as the wire refusal: rejecting the file would take every
	// other route down with this one.
	//
	// It does not apply to a native route the operator redirected with an
	// explicit SLBH_ENDPOINT. That policy entry describes the plan endpoint
	// and says nothing about wherever the override points, so requiring its
	// posture would be requiring the wrong document's answer — and would make
	// the sanctioned escape hatch unusable for exactly the native routes it
	// exists for. No posture is known for that destination, so none is sent
	// rather than one being invented: an override is the operator stepping
	// outside the managed path deliberately, and it is the one hole in this
	// guarantee that is signed for by hand.
	if err := route.Policy.Provider.Complete(); !viaOverride && err != nil {
		return Route{}, fmt.Errorf(
			"route %q goes to OpenRouter but its policy states no routing posture (%v): refusing rather than letting the request pick an upstream on price and availability alone. Add a provider block with zdr and data_collection",
			key, err)
	}
	route.Flavor, route.Endpoint, route.APIKey = "openrouter", target, apiKey
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
	Route Route
	// wire is the protocol strategy: headers, payload, parsing, error
	// envelopes and catalog shape. It is never nil — NewHTTP defaults it to
	// the OpenAI-shaped chat wire, which is what every route spoke before
	// phase 2 and what every non-plan route still speaks.
	wire           wireStrategy
	metadataMu     sync.Mutex
	contextWindows map[string]int
}

// SetHTTPClient replaces the transport. It exists because ForModel returns the
// Provider interface, so a caller that needs to capture exact bytes — the live
// transcript-replay test — can no longer reach the field directly.
func (p *HTTPProvider) SetHTTPClient(client *http.Client) { p.Client = client }

// TransportOverride is implemented by a Provider whose HTTP transport can be
// replaced. Asserting this is how a test swaps in a capturing transport
// without depending on the concrete type.
type TransportOverride interface{ SetHTTPClient(*http.Client) }

// Wire reports the protocol this instance speaks, for a caller that needs to
// report or assert which half of a two-wire deployment served a request.
func (p *HTTPProvider) Wire() string {
	if p.wire == nil {
		return WireOpenAIChat
	}
	return p.wire.name()
}

// effortValue maps a request's effort level through this route's descriptor.
//
// An instance with no route carries no policy — test fakes and embedder
// constructions that never went through route resolution — so the level passes
// through unchanged there. Every real request goes through ForModel.
func (p *HTTPProvider) effortValue(level string) (string, error) {
	if p.Route.Key == "" {
		return level, nil
	}
	return p.Route.Policy.EffortValue(p.Route.Key, level)
}

func (p *HTTPProvider) strategy() wireStrategy {
	if p.wire == nil {
		return openAIChatWire{}
	}
	return p.wire
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
	return &HTTPProvider{Endpoint: endpoint, APIKey: key, Client: &http.Client{Timeout: 0}, wire: openAIChatWire{}, contextWindows: make(map[string]int)}
}

// NewHTTPOnWire builds a provider for a named wire. It reports an error for a
// wire this build does not implement rather than defaulting to one, because
// defaulting is precisely the failure the wire refusal exists to prevent.
func NewHTTPOnWire(endpoint, key, wireName string) (*HTTPProvider, error) {
	strategy, ok := wireFor(wireName)
	if !ok {
		return nil, fmt.Errorf("no implementation for wire %q", wireName)
	}
	provider := NewHTTP(endpoint, key)
	provider.wire = strategy
	return provider, nil
}

// ForModel builds the provider for a model's authoritative route. It fails
// closed: a model whose native key is absent refuses rather than silently
// falling through to OpenRouter. endpointExplicit distinguishes an endpoint the
// operator set from one that merely defaulted, and only the former overrides a
// native route.
// It returns the Provider interface rather than the concrete transport,
// because a route now carries a wire and the strategy behind it is an
// implementation detail. A caller that genuinely needs the transport — the
// live test that captures exact request bytes — asserts TransportOverride.
func ForModel(model, endpointOverride string, endpointExplicit bool, policy Policy) (Provider, error) {
	route, err := ResolveRoute(model, endpointOverride, endpointExplicit, policy)
	if err != nil {
		return nil, err
	}
	strategy, ok := wireFor(route.Wire)
	if !ok {
		// Unreachable: ResolveRoute refuses an unsupported wire before it
		// builds a Route. Kept so a future wire added to the policy schema but
		// not to the factory fails here rather than defaulting to the wrong
		// encoder.
		return nil, fmt.Errorf("route %q resolved to wire %q, which this build cannot speak", route.Key, route.Wire)
	}
	instance := NewHTTP(route.Endpoint, route.APIKey)
	instance.Flavor = route.Flavor
	instance.Route = route
	instance.wire = strategy
	return instance, nil
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
	endpoint, err := p.catalogEndpoint()
	if err != nil {
		return 0, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, err
	}
	p.strategy().applyHeaders(request.Header, p.APIKey)
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
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8*1024*1024))
	if err != nil {
		return 0, err
	}
	models, err := p.strategy().decodeCatalog(data)
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

// catalogEndpoint is where this route's model catalog lives.
//
// The policy's `catalogEndpoint` wins where it is set, because on the
// Anthropic wire the catalog URL is not derivable from the inference URL:
// modelsEndpoint's suffix-stripping turns `/api/anthropic/v1/messages` into
// `/api/anthropic/v1/messages/models`, which is not a path that exists. The
// real one is `/api/anthropic/v1/models`, and only the policy knows it.
func (p *HTTPProvider) catalogEndpoint() (string, error) {
	if endpoint := strings.TrimSpace(p.Route.Policy.CatalogEndpoint); endpoint != "" {
		return endpoint, nil
	}
	return modelsEndpoint(p.Endpoint)
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
	return p.strategy().payload(p, req)
}

// RequestFromPayload reconstructs the provider request context from a
// transcripted wire payload, detecting which wire wrote it.
//
// Detection is by a field only one shape has, not by guessing: the OpenAI body
// always carries `stream_options` and the Anthropic body always carries
// `max_tokens`, because both are set unconditionally by their own payload
// builders. A transcript that carries neither is tried on the OpenAI wire
// first, which is what every record written before phase 2 is.
func RequestFromPayload(payload []byte) (Request, error) {
	var shape struct {
		StreamOptions *json.RawMessage `json:"stream_options"`
		MaxTokens     *int             `json:"max_tokens"`
	}
	if err := json.Unmarshal(payload, &shape); err != nil {
		return Request{}, fmt.Errorf("decode request payload: %w", err)
	}
	switch {
	case shape.StreamOptions != nil:
		return openAIChatWire{}.requestFromPayload(payload)
	case shape.MaxTokens != nil:
		return anthropicMessagesWire{}.requestFromPayload(payload)
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
	// Headers belong to the wire strategy, not to the transport: the Anthropic
	// shape needs x-api-key and a version header where the OpenAI one needs a
	// bearer token, and a seam that only owned payload and parsing could not
	// express that.
	p.strategy().applyHeaders(request.Header, p.APIKey)
	resp, err := p.Client.Do(request)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 32*1024))
		return p.strategy().classifyError(resp.StatusCode, data)
	}
	if err := p.strategy().parseStream(resp.Body, sink); err != nil {
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
	// The terminal reason is carried on the usage event so both wires report it
	// the same way. On this wire it arrives on the last content chunk, ahead of
	// the usage-only chunk that follows it.
	stopReason := ""
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
				FinishReason string `json:"finish_reason"`
				Delta        struct {
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
			if choice.FinishReason != "" {
				stopReason = choice.FinishReason
			}
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
			if err := sink(Event{Kind: EventUsage, Usage: chunk.Usage, StopReason: stopReason}); err != nil {
				return err
			}
		}
	}
	return scanner.Err()
}

// Retry executes an operation with short stepped delays. It intentionally
// leaves cancellation to the caller and never retries context cancellation.
//
// It used to retry every stream error three times. A refusal on these
// endpoints is a status-class decision instead: a 4xx will say the same thing
// three times, and retrying it wastes quota and delays the report. An error
// that does not classify itself is still retried, which keeps transport
// failures — the case retrying exists for — behaving as before.
//
// Most refusals are an HTTP status before the stream opens, which is all the
// capability matrix produced. An in-stream `error` frame was observed live
// under overload on 2026-09-14, so the Anthropic wire maps that frame's type
// onto the status its pre-stream equivalent would have carried and both halves
// classify under the one rule.
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
		if classified, ok := err.(retryable); ok && !classified.Retryable() {
			return err
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
