package provider

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// fleetPolicy is the managed fleet document's shape for the OpenRouter route:
// pi's own values for z-ai/glm-5.3-flash, which the two must mirror because
// the ignore list and the context pin were built against the same figure and
// move together.
func fleetPolicy() Policy {
	policy := testPolicy()
	policy.Routes["z-ai/glm-5.3-flash"] = RoutePolicy{
		Endpoint:        OpenRouterEndpoint,
		Wire:            WireOpenAIChat,
		CatalogEndpoint: "https://openrouter.ai/api/v1/models",
		ContextWindow:   1048576,
		Effort:          EffortDescriptor{Field: "reasoning_effort", Levels: identityLevels()},
		Provider: &ProviderPosture{
			ZDR:            BoolPtr(true),
			DataCollection: "deny",
			Sort:           "throughput",
			Ignore:         []string{"reka", "io-net"},
			MaxPrice:       &MaxPrice{Prompt: 0.19, Completion: 0.5},
		},
	}
	return policy
}

// providerObjectIn pulls the `provider` routing object out of an encoded body,
// reporting whether it was present at all. Absence and an empty object are
// different things and the tests below turn on the difference.
func providerObjectIn(t *testing.T, payload []byte) (map[string]any, bool) {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(payload, &body); err != nil {
		t.Fatal(err)
	}
	raw, present := body["provider"]
	if !present {
		return nil, false
	}
	object, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("provider field is %T, want an object", raw)
	}
	return object, true
}

func TestProviderObjectIsExactForOpenRouter(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "test-key-not-a-credential")
	p := forModelHTTP(t, "z-ai/glm-5.3-flash", OpenRouterEndpoint, false, fleetPolicy())
	payload, err := p.RequestPayload(Request{Model: "z-ai/glm-5.3-flash", System: "s", Effort: "high"})
	if err != nil {
		t.Fatal(err)
	}
	object, present := providerObjectIn(t, payload)
	if !present {
		t.Fatal("the OpenRouter route sent no provider object; the posture is the entire enforcement since the pi-run preflight was retired")
	}

	// Asserted field by field rather than as a blob, because a silently
	// dropped field is the failure mode here: a request missing `zdr` looks
	// exactly like a normal one on the wire.
	if object["zdr"] != true {
		t.Fatalf("zdr = %v, want true", object["zdr"])
	}
	if object["data_collection"] != "deny" {
		t.Fatalf("data_collection = %v, want deny", object["data_collection"])
	}
	if object["sort"] != "throughput" {
		t.Fatalf("sort = %v, want throughput", object["sort"])
	}
	ignore, _ := object["ignore"].([]any)
	if !reflect.DeepEqual(ignore, []any{"reka", "io-net"}) {
		t.Fatalf("ignore = %v, want reka, io-net", object["ignore"])
	}
	maxPrice, ok := object["max_price"].(map[string]any)
	if !ok {
		t.Fatalf("max_price = %T, want an object", object["max_price"])
	}
	if maxPrice["prompt"] != 0.19 || maxPrice["completion"] != 0.5 {
		t.Fatalf("max_price = %v, want prompt 0.19 / completion 0.5", maxPrice)
	}

	// The ignore list and the pin move together: those two endpoints are
	// dropped precisely because they cannot serve the pinned window.
	if window, pinned := p.PinnedContextWindow(); !pinned || window != 1048576 {
		t.Fatalf("pinned window = %d (pinned=%v), want 1048576 beside the ignore list", window, pinned)
	}
}

func TestProviderObjectIsAbsentOnEveryOtherRoute(t *testing.T) {
	// zdr and data_collection are OpenRouter concepts. Sending them to the
	// Z.ai plan endpoint or to a server on the house network would be a false
	// record of a guarantee that concept does not carry there.
	t.Setenv("OPENROUTER_API_KEY", "test-key-not-a-credential")
	t.Setenv("ZAI_API_KEY", "test-key-not-a-credential")
	t.Setenv("DEEPSEEK_API_KEY", "test-key-not-a-credential")
	t.Setenv("SLBH_LOCAL_ENDPOINT", "")

	// Every non-OpenRouter route is given a posture block it has no business
	// carrying, which is the realistic misconfiguration: someone copies the
	// block onto the plan entry. What must stop it is the route's flavor, not
	// the happy accident that those entries are usually empty — and a policy
	// with no posture on them cannot tell the two apart.
	misconfigured := fleetPolicy()
	for _, key := range []string{"zai/glm-5.3-flash", "deepseek-v4-flash", LocalModelID} {
		route := misconfigured.Routes[key]
		route.Provider = &ProviderPosture{
			ZDR:            BoolPtr(true),
			DataCollection: "deny",
			Sort:           "throughput",
			Ignore:         []string{"reka"},
			MaxPrice:       &MaxPrice{Prompt: 0.19, Completion: 0.5},
		}
		misconfigured.Routes[key] = route
	}

	for _, model := range []string{"zai/glm-5.3-flash", "deepseek-v4-flash", LocalModelID} {
		for name, policy := range map[string]Policy{"no posture in policy": fleetPolicy(), "posture wrongly present": misconfigured} {
			p := forModelHTTP(t, model, OpenRouterEndpoint, false, policy)
			payload, err := p.RequestPayload(Request{Model: model, System: "s"})
			if err != nil {
				t.Fatalf("%s (%s): %v", model, name, err)
			}
			if _, present := providerObjectIn(t, payload); present {
				t.Fatalf("%s carried an OpenRouter provider object (%s)", model, name)
			}
		}
	}
}

func TestProviderObjectAbsentOnTheAnthropicWire(t *testing.T) {
	// The plan route is on the Messages wire, which has no such field at all,
	// so the object must not appear there under any encoding.
	t.Setenv("ZAI_API_KEY", "test-key-not-a-credential")
	p := forModelHTTP(t, "zai/glm-5.3-flash", OpenRouterEndpoint, false, anthropicPolicy())
	payload, err := p.RequestPayload(Request{Model: "zai/glm-5.3-flash", System: "s"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(payload), "data_collection") || strings.Contains(string(payload), `"zdr"`) {
		t.Fatalf("the Anthropic payload carries OpenRouter posture fields: %s", payload)
	}
}

func TestOpenRouterRefusesOnIncompletePosture(t *testing.T) {
	// The refusal is total. Not merely "refuse when the block is missing": a
	// block that forgot one of the two fields that *are* the guarantee refuses
	// too, because a posture with no zdr is indistinguishable on the wire from
	// no posture at all.
	t.Setenv("OPENROUTER_API_KEY", "test-key-not-a-credential")

	for name, mutate := range map[string]func(*RoutePolicy){
		"no provider block": func(r *RoutePolicy) { r.Provider = nil },
		"no zdr": func(r *RoutePolicy) {
			r.Provider = &ProviderPosture{DataCollection: "deny", Sort: "throughput"}
		},
		"no data_collection": func(r *RoutePolicy) {
			r.Provider = &ProviderPosture{ZDR: BoolPtr(true), Sort: "throughput"}
		},
		"blank data_collection": func(r *RoutePolicy) {
			r.Provider = &ProviderPosture{ZDR: BoolPtr(true), DataCollection: "   "}
		},
	} {
		policy := fleetPolicy()
		route := policy.Routes["z-ai/glm-5.3-flash"]
		mutate(&route)
		policy.Routes["z-ai/glm-5.3-flash"] = route

		_, err := ResolveRoute("z-ai/glm-5.3-flash", OpenRouterEndpoint, false, policy)
		if err == nil {
			t.Fatalf("a policy with %s routed to OpenRouter", name)
		}
		if !strings.Contains(err.Error(), "posture") {
			t.Fatalf("refusal for %s does not name the cause: %v", name, err)
		}
	}

	// A deliberate `zdr: false` is expressible and is not a refusal: the field
	// is stated, so nothing was forgotten. Only omission refuses.
	policy := fleetPolicy()
	route := policy.Routes["z-ai/glm-5.3-flash"]
	route.Provider = &ProviderPosture{ZDR: BoolPtr(false), DataCollection: "allow"}
	policy.Routes["z-ai/glm-5.3-flash"] = route
	resolved, err := ResolveRoute("z-ai/glm-5.3-flash", OpenRouterEndpoint, false, policy)
	if err != nil {
		t.Fatalf("a stated posture must route even when it is weak: %v", err)
	}
	if object := providerObjectFor(resolved); object == nil || object.ZDR {
		t.Fatalf("a stated zdr:false did not reach the wire object: %#v", object)
	}
}

func TestOpenRouterRefusesAnUnknownModel(t *testing.T) {
	// The other half of "total": a model the policy does not describe refuses
	// rather than going out with no posture at all.
	t.Setenv("OPENROUTER_API_KEY", "test-key-not-a-credential")
	if _, err := ResolveRoute("someone/unlisted-model", OpenRouterEndpoint, false, fleetPolicy()); err == nil {
		t.Fatal("an unknown model routed to OpenRouter")
	}
}

func TestExplicitEndpointOverrideKeepsItsEscapeHatch(t *testing.T) {
	// The posture requirement attaches to routes the policy describes as
	// OpenRouter routes, not to a native route the operator redirected by
	// hand. The plan route's entry describes the Z.ai endpoint and carries no
	// posture, so requiring it would be demanding the wrong document's answer
	// and would make the sanctioned override unusable for exactly the native
	// routes it exists for.
	t.Setenv("ZAI_API_KEY", "")
	t.Setenv("OPENROUTER_API_KEY", "test-key-not-a-credential")

	route, err := ResolveRoute("zai/glm-5.3-flash", "https://example.invalid/v1/chat/completions", true, fleetPolicy())
	if err != nil {
		t.Fatalf("an explicit override must still resolve: %v", err)
	}
	if route.Endpoint != "https://example.invalid/v1/chat/completions" {
		t.Fatalf("override endpoint = %q", route.Endpoint)
	}
	// And no posture is invented for a destination nothing describes.
	if object := providerObjectFor(route); object != nil {
		t.Fatalf("an override invented a posture for an undescribed endpoint: %#v", object)
	}
	// The default endpoint is still not an override, so the plan route still
	// refuses without its key.
	if _, err := ResolveRoute("zai/glm-5.3-flash", OpenRouterEndpoint, false, fleetPolicy()); err == nil {
		t.Fatal("a defaulted endpoint overrode a native route")
	}
}

func TestManagedPolicyArtifactCarriesPiValues(t *testing.T) {
	// The committed managed artifact must still mirror pi's numbers. If these
	// drift apart, two harnesses on one account disagree about which upstreams
	// may serve a request and at what price.
	policy := committedManagedPolicy(t)
	posture := policy.Routes["z-ai/glm-5.3-flash"].Provider
	if err := posture.Complete(); err != nil {
		t.Fatalf("managed openrouter posture is incomplete: %v", err)
	}
	if !*posture.ZDR || posture.DataCollection != "deny" || posture.Sort != "throughput" {
		t.Fatalf("posture = %#v", posture)
	}
	if !reflect.DeepEqual(posture.Ignore, []string{"reka", "io-net"}) {
		t.Fatalf("ignore = %v, want reka, io-net", posture.Ignore)
	}
	if posture.MaxPrice == nil || posture.MaxPrice.Prompt != 0.19 || posture.MaxPrice.Completion != 0.5 {
		t.Fatalf("max_price = %#v", posture.MaxPrice)
	}
}
