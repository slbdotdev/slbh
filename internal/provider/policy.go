package provider

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// PolicyVersion is the only document version this build reads. A document
// carrying anything else is refused rather than best-guessed: the whole point
// of the file is that the binary can be sure what it is enforcing.
const PolicyVersion = 1

// The wire protocols a route may name. These are the values that may appear in
// the policy's `wire` field, not a list of what this build can speak — see
// WireSupported, which is deliberately the narrower set.
const (
	WireOpenAIChat        = "openai-chat"
	WireAnthropicMessages = "anthropic-messages"
)

// effortFieldForWire fixes the one effort spelling each wire may use.
//
// This is the whole defence against a level the endpoint silently discards,
// and it is structural rather than a runtime check. On Z.ai's Anthropic
// endpoint `thinking.budget_tokens` and a bare `reasoning_effort` are both
// accepted with HTTP 200 and then ignored — a 1024-token budget produced
// 3,393 to 5,614 output tokens and the five budgets' ranges overlap completely
// (measured 2026-09-13, fact 10 of org/slbh-capability-matrix-2026-09-13.md).
// Only `output_config.effort` is honoured there, with an ordered ladder. slbh
// must not try to infer from reasoning volume whether a setting took, so the
// inert spellings are simply not representable: a policy naming one is
// rejected when it is read.
var effortFieldForWire = map[string]string{
	WireOpenAIChat:        "reasoning_effort",
	WireAnthropicMessages: "output_config.effort",
}

// wireSupported names the protocols this build can actually speak.
//
// It is deliberately narrower than the wires the schema admits. A managed
// policy is deployed by a converge and a binary by an artifact copy, and the
// two can arrive in either order, so a policy may legitimately name a wire
// this build has no implementation for. That route then refuses, loudly and by
// name, which is the only safe reading: the alternative is to send an
// Anthropic-shaped conversation down an OpenAI-shaped encoder and have the
// endpoint answer 200 to something nobody meant.
//
// WireAnthropicMessages joined this set in phase 2, in the same change that
// implemented it. The set stays a set rather than collapsing into "every wire
// the schema admits", because the whole point is that the two can diverge: a
// future policy version may name a protocol a deployed binary has never heard
// of, and that binary must refuse rather than guess.
var wireSupported = map[string]bool{
	WireOpenAIChat:        true,
	WireAnthropicMessages: true,
}

// WireSupported reports whether this build can speak a wire.
func WireSupported(wire string) bool { return wireSupported[wire] }

// withoutWireSupport removes a wire from the supported set and returns a
// function that restores it. It exists for the test that proves the refusal
// still fires: once both wires are implemented there is no schema-valid wire
// this build cannot speak, so the only honest way to exercise that guard is to
// take one away.
func withoutWireSupport(wire string) func() {
	previous, had := wireSupported[wire]
	delete(wireSupported, wire)
	return func() {
		if had {
			wireSupported[wire] = previous
		}
	}
}

// MaxPrice is the OpenRouter per-million-token price ceiling.
type MaxPrice struct {
	Prompt     float64 `json:"prompt"`
	Completion float64 `json:"completion"`
}

// ProviderPosture is the OpenRouter routing object: what may serve a request
// and on what terms. It is populated only for an OpenRouter route. Since the
// pi-run preflight was retired on 2026-09-10 this document is the entire
// enforcement of that posture — nothing checks it before a launch any more, so
// it holds because it is in the file the harness reads.
type ProviderPosture struct {
	// ZDR is a pointer so an omitted field is distinguishable from a
	// deliberate false. That distinction is the whole refusal: a posture that
	// simply forgot `zdr` would otherwise decode to false and quietly ship a
	// request with the org's zero-data-retention guarantee dropped, which is
	// indistinguishable on the wire from one that never had it.
	ZDR            *bool     `json:"zdr"`
	DataCollection string    `json:"data_collection"`
	Sort           string    `json:"sort,omitempty"`
	Ignore         []string  `json:"ignore,omitempty"`
	MaxPrice       *MaxPrice `json:"max_price,omitempty"`
}

// Complete reports whether a posture carries the two fields that are the
// guarantee rather than a preference.
//
// `sort`, `ignore` and `max_price` are routing preferences and their absence
// costs money or latency. `zdr` and `data_collection` are the posture itself,
// and their absence costs the guarantee, so only those two are required.
func (p *ProviderPosture) Complete() error {
	if p == nil {
		return fmt.Errorf("no provider posture")
	}
	if p.ZDR == nil {
		return fmt.Errorf("posture does not state zdr")
	}
	if strings.TrimSpace(p.DataCollection) == "" {
		return fmt.Errorf("posture does not state data_collection")
	}
	return nil
}

// BoolPtr is a helper for building a posture in code, since ZDR is a pointer.
func BoolPtr(value bool) *bool { return &value }

// EffortDescriptor maps slbh's effort levels onto one endpoint's spelling of
// them. Field is the wire field; Levels maps an slbh level to the value sent.
//
// A level absent from Levels, or present with an empty value, is a level this
// route does not support, and asking for it refuses the request. Neither
// omitting the field nor walking the level down to the nearest supported one
// is acceptable: slbh runs the benchmark campaigns, and both would let a sweep
// record results at an effort nobody configured. That is not hypothetical —
// pi's clampThinkingLevel walked `max` down to `high` twice on 2026-09-08
// before an explicit map was written.
type EffortDescriptor struct {
	Field  string            `json:"field"`
	Levels map[string]string `json:"levels"`
}

// RoutePolicy is one route's entry: where it goes, how it speaks, how big its
// context really is, how it spells effort, and what posture governs it.
type RoutePolicy struct {
	Endpoint        string           `json:"endpoint"`
	Wire            string           `json:"wire"`
	CatalogEndpoint string           `json:"catalogEndpoint,omitempty"`
	ContextWindow   int              `json:"contextWindow,omitempty"`
	// MaxOutputTokens bounds a single generation on this route, zero to take
	// the wire's own default. It is per-route rather than global because the
	// routes differ by more than an order of magnitude in what a legitimate
	// turn costs and in what an unbounded one costs: a cloud route runs away
	// in seconds, while the local 27B decodes at about 76 tokens per second,
	// where an unbounded generation against a 196,608-token window is some
	// forty minutes of held GPU before anything stops it.
	//
	// Bounding also restores the telemetry. An unbounded request can only ever
	// come back `stop`, so `finish_reason` carries no signal; with a bound, a
	// generation that would not end reports `length` and is visible as what it
	// is rather than as a hang.
	MaxOutputTokens int              `json:"maxOutputTokens,omitempty"`
	Effort          EffortDescriptor `json:"effort"`
	Provider        *ProviderPosture `json:"provider,omitempty"`
}

// Policy is the routing policy document, keyed by authoritative route key.
//
// Keys are route keys and not model names. The same model reached on the plan
// and on OpenRouter is two routes with two endpoints, two wires and two
// postures, and every spelling that addresses one of them folds to a single
// key in RouteKey, so the pin, the posture and the effort map can never
// disagree about which route a request took.
type Policy struct {
	Version int                    `json:"version"`
	Routes  map[string]RoutePolicy `json:"routes"`
}

// Defined reports whether a policy was resolved at all. The zero Policy means
// no policy from either source, which refuses every route.
func (p Policy) Defined() bool { return p.Version != 0 || len(p.Routes) > 0 }

// Route returns the policy entry for an authoritative route key.
func (p Policy) Route(key string) (RoutePolicy, bool) {
	route, ok := p.Routes[key]
	return route, ok
}

// Validate checks the whole document. It reports the first problem it finds,
// naming the route, because a policy error is something the managed file or
// /models can fix and the message is what tells the operator which.
func (p Policy) Validate() error {
	if p.Version != PolicyVersion {
		return fmt.Errorf("routing policy version is %d, but this build reads version %d only", p.Version, PolicyVersion)
	}
	if len(p.Routes) == 0 {
		return fmt.Errorf("routing policy defines no routes")
	}
	for _, key := range sortedKeys(p.Routes) {
		if strings.TrimSpace(key) == "" {
			return fmt.Errorf("routing policy has a route with an empty key")
		}
		if err := p.Routes[key].validate(key); err != nil {
			return err
		}
	}
	return nil
}

func (r RoutePolicy) validate(key string) error {
	if strings.TrimSpace(r.Endpoint) == "" {
		return fmt.Errorf("route %q has no endpoint", key)
	}
	if parsed, err := url.Parse(r.Endpoint); err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return fmt.Errorf("route %q endpoint %q is not an absolute URL", key, r.Endpoint)
	}
	if r.CatalogEndpoint != "" {
		if parsed, err := url.Parse(r.CatalogEndpoint); err != nil || parsed.Scheme == "" || parsed.Host == "" {
			return fmt.Errorf("route %q catalogEndpoint %q is not an absolute URL", key, r.CatalogEndpoint)
		}
	}
	wanted, known := effortFieldForWire[r.Wire]
	if !known {
		return fmt.Errorf("route %q names wire %q, which is not a wire protocol slbh knows (%s)",
			key, r.Wire, strings.Join([]string{WireOpenAIChat, WireAnthropicMessages}, ", "))
	}
	if r.ContextWindow < 0 {
		return fmt.Errorf("route %q has a negative contextWindow", key)
	}
	if r.MaxOutputTokens < 0 {
		return fmt.Errorf("route %q has a negative maxOutputTokens", key)
	}
	if r.ContextWindow > 0 && r.MaxOutputTokens > r.ContextWindow {
		return fmt.Errorf(
			"route %q sets maxOutputTokens %d above its own contextWindow %d, so the bound could never be reached",
			key, r.MaxOutputTokens, r.ContextWindow)
	}
	if strings.TrimSpace(r.Effort.Field) == "" {
		return fmt.Errorf("route %q has no effort field", key)
	}
	if r.Effort.Field != wanted {
		// The rejected spellings are rejected by measurement, not by taste.
		return fmt.Errorf(
			"route %q on wire %q spells effort %q, but the only spelling that is honoured on that wire is %q; `thinking.budget_tokens` and a bare `reasoning_effort` are accepted and discarded there and are not permitted",
			key, r.Wire, r.Effort.Field, wanted)
	}
	if len(r.Effort.Levels) == 0 {
		return fmt.Errorf("route %q has an empty effort level map, so no effort could ever be sent", key)
	}
	return nil
}

// SupportedLevels lists the effort levels this route can actually send, in a
// stable order, for an error message the operator can act on.
func (r RoutePolicy) SupportedLevels() []string {
	levels := make([]string, 0, len(r.Effort.Levels))
	for level, value := range r.Effort.Levels {
		if strings.TrimSpace(value) != "" {
			levels = append(levels, level)
		}
	}
	sort.Strings(levels)
	return levels
}

// EffortValue maps an slbh effort level onto this route's wire value. An empty
// level asks for nothing and sends nothing, which is not an error.
//
// An unmappable level refuses, naming the route, the level asked for and what
// the route supports. Note what this does not catch, and cannot: a level that
// maps cleanly and is then silently discarded by the endpoint. No runtime
// check can see that — the defence against it is that only a measured-working
// spelling is representable in the schema at all, plus the live ladder in the
// plan's acceptance criteria.
func (r RoutePolicy) EffortValue(key, level string) (string, error) {
	level = strings.TrimSpace(level)
	if level == "" {
		return "", nil
	}
	value, ok := r.Effort.Levels[level]
	if !ok || strings.TrimSpace(value) == "" {
		supported := r.SupportedLevels()
		listed := "none"
		if len(supported) > 0 {
			listed = strings.Join(supported, ", ")
		}
		return "", fmt.Errorf(
			"route %q does not support effort %q; it supports %s. Refusing rather than dropping or downgrading the level, because a sweep must never record results at an effort nobody asked for. Fix the route's effort map in the managed policy or in /models",
			key, level, listed)
	}
	return value, nil
}

func sortedKeys(routes map[string]RoutePolicy) []string {
	keys := make([]string, 0, len(routes))
	for key := range routes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// DefaultLocalPolicy authors a working policy for a set of models from what
// the binary already knows about their routes.
//
// This is what keeps the fail-closed refusal from being a brick. On a host
// ansible does not manage — a fresh checkout, a machine outside the fleet, a
// box mid-provision — /models writes this into the app-owned config.json and
// slbh runs. Two properties make it safe to generate rather than to demand the
// user hand-write it:
//
// Every route it writes is on a wire this build can actually speak, so the
// plan route is authored on the coding endpoint rather than the Anthropic one.
// A generated policy that refused on its own wire would defeat the purpose.
//
// And an OpenRouter route is written with the posture, not without it. A
// locally authored policy may still be weaker than the managed one — that is
// an accepted cost on an unmanaged host — but it is not weaker by omission
// here, and a route with no posture would refuse under the total refusal rule
// anyway.
func DefaultLocalPolicy(models []string) Policy {
	policy := Policy{Version: PolicyVersion, Routes: map[string]RoutePolicy{}}
	for _, model := range models {
		key, ok := RouteKey(model)
		if !ok {
			continue
		}
		if _, exists := policy.Routes[key]; exists {
			continue
		}
		route := RoutePolicy{
			Wire:   WireOpenAIChat,
			Effort: EffortDescriptor{Field: effortFieldForWire[WireOpenAIChat], Levels: identityEffortLevels()},
		}
		if native, isNative := nativeRouteFor(key); isNative {
			switch native.flavor {
			case LocalProviderName:
				route.Endpoint = localEndpoint()
			default:
				route.Endpoint = native.endpoint
				route.CatalogEndpoint, _ = modelsEndpoint(native.endpoint)
			}
		} else {
			route.Endpoint = openRouterEndpoint
			route.CatalogEndpoint, _ = modelsEndpoint(openRouterEndpoint)
			route.Provider = &ProviderPosture{ZDR: BoolPtr(true), DataCollection: "deny", Sort: "throughput"}
		}
		if route.Endpoint == "" {
			continue
		}
		policy.Routes[key] = route
	}
	return policy
}

// identityEffortLevels is the map every route slbh authors locally gets: the
// level is sent as written. It never clamps. The local Ollama route's known
// effort defect (org/pending.md) stays visible rather than being papered over
// here, which is the owner's ruling of 2026-09-14.
func identityEffortLevels() map[string]string {
	return map[string]string{"low": "low", "medium": "medium", "high": "high", "xhigh": "xhigh", "max": "max"}
}
