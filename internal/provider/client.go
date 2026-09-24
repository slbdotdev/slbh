package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

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

// maxOutputTokens resolves the generation bound for a request on this route:
// the request's own value, then the route's policy, then the wire default
// passed by the caller. A caller that passes zero wants no bound sent.
//
// The ordering matches resolveContextWindow — the explicit value beats the
// pin beats the fallback — so a reader who knows one knows the other.
func (p *HTTPProvider) maxOutputTokens(req Request, wireDefault int) int {
	if req.MaxTokens != nil && *req.MaxTokens > 0 {
		return *req.MaxTokens
	}
	if p.Route.Policy.MaxOutputTokens > 0 {
		return p.Route.Policy.MaxOutputTokens
	}
	return wireDefault
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

// NormalizeModel trims a model name and otherwise leaves it as the user
// wrote it. It used to rewrite `deepseek/deepseek-v4.1-flash` to the direct
// DeepSeek API's `deepseek-v4-flash`; that string is OpenRouter's id for the
// model, and DeepSeek is used only through OpenRouter, so the rewrite went.
func NormalizeModel(model string) string {
	return strings.TrimSpace(model)
}

func (p *HTTPProvider) modelID(model string) string {
	model = NormalizeModel(model)
	switch p.Flavor {
	case LocalProviderName:
		return strings.TrimPrefix(model, LocalProviderName+"/")
	case RemoteProviderName:
		return strings.TrimPrefix(model, RemoteProviderName+"/")
	case "zai":
		return strings.TrimPrefix(model, "zai/")
	case "cerebras":
		return strings.TrimPrefix(model, "cerebras/")
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
