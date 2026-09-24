package provider

import (
	"fmt"
	"os"
	"strings"
)

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
	// Flavor is the provider family: zai, cerebras, openrouter or
	// local.
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
	case strings.HasPrefix(key, RemoteProviderName+"/"):
		// No endpoint and no keyEnv, exactly as the local arm: both are
		// answered from the policy, and the empty keyEnv is what keeps this
		// flavor out of every credential path below rather than being
		// something the caller must remember.
		return nativeRoute{flavor: RemoteProviderName}, true
	// There is no DeepSeek family here, deliberately. The org reaches DeepSeek
	// only through OpenRouter (slb-org models.md, 2026-09-22), and OpenRouter's
	// ids for it begin `deepseek/`, so a native arm on that prefix sent them
	// to api.deepseek.com with DEEPSEEK_API_KEY instead: the one account the
	// org does not use, which failed as an empty balance.
	case strings.HasPrefix(key, "zai/"):
		return nativeRoute{flavor: "zai", endpoint: "https://api.z.ai/api/coding/paas/v4/chat/completions", keyEnv: "ZAI_API_KEY"}, true
	case strings.HasPrefix(key, "cerebras/"):
		// Only the prefixed spelling addresses this family, deliberately. The
		// models Cerebras serves are open-weight ones whose bare names are
		// served by OpenRouter and by the desktop too — `qwen-3.8-27b` is not
		// Cerebras's model, it is Cerebras's copy of it — so folding the bare
		// name here would capture a name that legitimately addresses three
		// different routes. `glm-` can fold because that slug names one plan.
		return nativeRoute{flavor: "cerebras", endpoint: "https://api.cerebras.ai/v1/chat/completions", keyEnv: "CEREBRAS_API_KEY"}, true
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
		if native.flavor == RemoteProviderName {
			// The policy is the only source for this route. SLBH_LOCAL_ENDPOINT
			// is deliberately not honoured here: it is one variable, and a
			// second tailnet engine would make it repoint every credential-free
			// route at once. SLBH_ENDPOINT still overrides a single route by
			// name, which is the escape hatch that already exists.
			route.Flavor, route.Endpoint = RemoteProviderName, strings.TrimSpace(entry.Endpoint)
			if route.Endpoint == "" {
				return Route{}, fmt.Errorf(
					"route %q states no endpoint in the routing policy: refusing rather than falling back to the desktop's, which is a different machine serving a different quant. A remote route has no compiled default by design",
					key)
			}
			// The pin is mandatory on this flavor, where it is merely advisable
			// on the others. A remote Ollama sends no credential and its
			// OpenAI-compatible catalog publishes no context length, so
			// discovery cannot answer for it; an unpinned route would silently
			// take the compiled fallback and size a window the engine does not
			// serve. The desktop had exactly this bug described in prose and
			// caught by a pin; here it refuses instead.
			if route.ContextWindow <= 0 {
				return Route{}, fmt.Errorf(
					"route %q states no contextWindow: refusing, because this flavor sends no credential and publishes no window, so nothing can discover one and the request would silently take the compiled fallback of %d",
					key, FallbackContextWindow)
			}
			return route, nil
		}
		if apiKey := os.Getenv(native.keyEnv); apiKey != "" {
			route.Flavor, route.Endpoint, route.APIKey = native.flavor, entry.Endpoint, apiKey
			// Decision 5 honours an explicit SLBH_ENDPOINT as a deliberate
			// override, and it has to be honoured here and not only on the
			// missing-key path below. The key being present is the normal case, so
			// overriding only when it is absent means the override never works on a
			// configured host — and the refusal below would be telling the operator
			// to set a variable this branch ignored.
			//
			// The native key and the policy's wire are both kept: the operator is
			// redirecting this provider, not swapping protocols. The context pin
			// goes, for the same reason as the fall-through below — a window
			// measured on the plan endpoint says nothing about wherever this now
			// points.
			if endpointExplicit && strings.TrimSpace(endpoint) != "" {
				route.Endpoint = strings.TrimSpace(endpoint)
				route.ContextWindow = 0
				// The catalog pointer goes with the pin. catalogEndpoint()
				// prefers the policy's value over the route's endpoint, so
				// leaving it set sends the first turn's metadata request to
				// Z.ai's catalog while inference goes to the override — sizing
				// the context window from a model on a host the operator has
				// just redirected away from, and calling out to Z.ai from a
				// route that may be offline or private.
				route.Policy.CatalogEndpoint = ""
			}
			return route, nil
		}
		if !endpointExplicit {
			return Route{}, fmt.Errorf(
				"model %q routes to the %s endpoint but %s is not set: refusing to fall through to OpenRouter, because a model reachable on a plan never runs through it (set %s, or set SLBH_ENDPOINT deliberately to override, which keeps this route's %s wire and sends no credential)",
				model, native.flavor, native.keyEnv, native.keyEnv, native.flavor)
		}
		// An explicit override on a native route with no native key. Where the
		// override points decides which arm takes it, because the endpoint is
		// what determines the protocol and the credential.
		//
		// Pointing at OpenRouter is a deliberate, sanctioned route through it,
		// and takes the OpenRouter flavor, credential and posture below.
		// Pointing anywhere else must not: stamping the OpenRouter flavor on a
		// private endpoint sent OPENROUTER_API_KEY to a host that never asked
		// for it, switched the wire under the operator, and demanded a
		// credential the refusal above never mentioned. Such a route keeps the
		// policy's wire — as the key-present arm does, and the two arms of one
		// escape hatch must not disagree about protocol — and carries no
		// credential, there being no plan key to carry. That is right for the
		// local or private endpoint this hatch exists to reach, and fails
		// loudly anywhere else rather than leaking a key to find out.
		if strings.TrimSpace(endpoint) != openRouterEndpoint {
			route.Flavor, route.Endpoint, route.APIKey = native.flavor, strings.TrimSpace(endpoint), ""
			route.ContextWindow = 0
			route.Policy.CatalogEndpoint = ""
			return route, nil
		}
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
		// As on the native path above: the catalog pointer belongs to the
		// endpoint the policy named, not to the one the operator substituted.
		route.Policy.CatalogEndpoint = ""
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

// LocalModelID is the leaf default and the fallback the catalog falls back to,
// never the list of what the desktop serves: the deployed quants are in the
// routing policy, which changes by converge, and this changes by rebuild. It
// is `UD-Q2_K_XL` at 64k, the one local tag the org serves since 2026-09-18:
// with the MTP head resident this arm tops out at 79k beside a working
// desktop, and the 96k tag it replaces was over that budget.
//
// It was `q27-IQ2_M-96k` until 2026-09-17, a tag deleted from the desktop on
// 2026-09-15, so every leaf that took the default routed to a model the server
// did not have. A compiled-in name is a claim about another machine and goes
// stale silently; the policy is the answer, and this is the floor under it.
const (
	LocalProviderName = "local"
	// RemoteProviderName is the second credential-free flavor: an Ollama the
	// fleet reaches over the tailnet that is not the desktop. It exists
	// because `local/` is not a spelling, it is an assertion — the Windows
	// converge fails if a `local/` route names a tag the desktop does not
	// serve — so a rented pod routed under that prefix would turn a converge
	// red to describe a machine Ansible does not manage. It carries no
	// credential for the same reason `local/` does not: the tailnet is the
	// boundary, and a route that sent a bearer token would be minting one for
	// an endpoint no key was issued for.
	RemoteProviderName = "remote"
	LocalModelID       = "local/q27-UD-Q2_K_XL-64k"
	localWireModelID   = "q27-UD-Q2_K_XL-64k"
	LocalContextWindow = 65536
	localDefaultURL    = "http://fractal.wyvern-temperature.ts.net:11434/api/chat"
)

// CredentialFreeFlavor reports whether a flavor reaches its endpoint over the
// tailnet with no credential at all. Both such flavors are Ollama servers the
// fleet owns the network path to, and neither was ever issued a key — so a
// guard that demanded one would refuse the two routes that are correct
// without it. It is a predicate rather than an equality test because the
// second flavor was added by widening exactly these call sites, and the next
// one must not be able to miss any.
func CredentialFreeFlavor(flavor string) bool {
	return flavor == LocalProviderName || flavor == RemoteProviderName
}

func localEndpoint() string {
	if endpoint := strings.TrimSpace(os.Getenv("SLBH_LOCAL_ENDPOINT")); endpoint != "" {
		return endpoint
	}
	return localDefaultURL
}
