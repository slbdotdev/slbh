package provider

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// identityLevels is the effort map every fleet route carries: the level is
// sent as written, and nothing is clamped.
func identityLevels() map[string]string {
	return map[string]string{"low": "low", "medium": "medium", "high": "high", "xhigh": "xhigh", "max": "max"}
}

// testPolicy is a policy on wires this build can speak. The plan route is on
// the coding endpoint deliberately: the managed fleet document puts it on
// anthropic-messages, which phase 2 implements, and a fixture that used it
// would be testing the refusal rather than the routing.
func testPolicy() Policy {
	openai := func(endpoint string, window int) RoutePolicy {
		catalog, _ := modelsEndpoint(endpoint)
		return RoutePolicy{
			Endpoint:        endpoint,
			Wire:            WireOpenAIChat,
			CatalogEndpoint: catalog,
			ContextWindow:   window,
			Effort:          EffortDescriptor{Field: "reasoning_effort", Levels: identityLevels()},
		}
	}
	orr := openai(OpenRouterEndpoint, 0)
	orr.Provider = &ProviderPosture{ZDR: true, DataCollection: "deny", Sort: "throughput"}
	return Policy{Version: PolicyVersion, Routes: map[string]RoutePolicy{
		"zai/glm-5.3-flash": openai("https://api.z.ai/api/coding/paas/v4/chat/completions", 1000000),
		"zai/glm-5.3":       openai("https://api.z.ai/api/coding/paas/v4/chat/completions", 1000000),
		"deepseek-v4-flash": openai("https://api.deepseek.com/chat/completions", 0),
		LocalModelID:        openai("http://fractal.wyvern-temperature.ts.net:11434/v1/chat/completions", 0),
		"vendor/model":      orr,
	}}
}

func TestPolicyRefusesEveryRouteWhenNoPolicyIsInForce(t *testing.T) {
	// The suspenders. An absent policy from both sources is not "route with
	// defaults": it is a refusal, because routing without a posture is the
	// thing the whole document exists to prevent.
	t.Setenv("ZAI_API_KEY", "test-key-not-a-credential")
	t.Setenv("DEEPSEEK_API_KEY", "test-key-not-a-credential")
	t.Setenv("OPENROUTER_API_KEY", "test-key-not-a-credential")
	for _, model := range []string{"zai/glm-5.3-flash", "deepseek-v4-flash", LocalModelID, "vendor/model"} {
		_, err := ResolveRoute(model, OpenRouterEndpoint, false, Policy{})
		if err == nil {
			t.Fatalf("%q routed with no policy in force", model)
		}
		if !strings.Contains(err.Error(), "no routing policy is in force") {
			t.Fatalf("refusal for %q does not say why: %v", model, err)
		}
	}
}

func TestPolicyRefusesARouteItDoesNotDescribe(t *testing.T) {
	// Phase 3's total refusal, in its phase 1c half: an unknown model with no
	// entry refuses rather than falling back to a bare endpoint.
	t.Setenv("OPENROUTER_API_KEY", "test-key-not-a-credential")
	_, err := ResolveRoute("someone/unlisted-model", OpenRouterEndpoint, false, testPolicy())
	if err == nil {
		t.Fatal("a model absent from the policy was routed")
	}
	if !strings.Contains(err.Error(), "no entry in the routing policy") {
		t.Fatalf("refusal does not name the cause: %v", err)
	}
}

func TestPolicyRefusesAWireThisBuildCannotSpeak(t *testing.T) {
	// The ordering trap made loud. The managed fleet policy puts the plan
	// route on anthropic-messages, which phase 2 implements. Until it does, a
	// binary handed that policy must refuse by name rather than encode an
	// Anthropic conversation as an OpenAI one — which these endpoints would
	// answer 200 to, since an invalid parameter is accepted silently on both
	// (fact 7).
	t.Setenv("ZAI_API_KEY", "test-key-not-a-credential")
	policy := testPolicy()
	entry := policy.Routes["zai/glm-5.3-flash"]
	entry.Wire = WireAnthropicMessages
	entry.Effort = EffortDescriptor{Field: "output_config.effort", Levels: identityLevels()}
	policy.Routes["zai/glm-5.3-flash"] = entry

	// It is a valid document: the schema admits the wire, the build does not.
	if err := policy.Validate(); err != nil {
		t.Fatalf("anthropic-messages must be a valid schema value: %v", err)
	}
	if WireSupported(WireAnthropicMessages) {
		t.Fatal("this build claims to speak anthropic-messages; phase 2 must update this test with the implementation")
	}
	_, err := ResolveRoute("zai/glm-5.3-flash", OpenRouterEndpoint, false, policy)
	if err == nil {
		t.Fatal("a route on an unimplemented wire was resolved")
	}
	if !strings.Contains(err.Error(), "cannot speak") || !strings.Contains(err.Error(), WireAnthropicMessages) {
		t.Fatalf("refusal does not name the wire: %v", err)
	}
}

func TestPolicyRejectsTheInertEffortSpellings(t *testing.T) {
	// Measured inert on Z.ai's Anthropic endpoint: both return 200 and change
	// nothing (fact 10). They are excluded structurally, so a policy naming
	// one is refused when it is read rather than discovered by a sweep that
	// recorded results at an effort nobody got.
	for _, field := range []string{"thinking.budget_tokens", "reasoning_effort", "thinking", "effort"} {
		policy := Policy{Version: PolicyVersion, Routes: map[string]RoutePolicy{
			"zai/glm-5.3-flash": {
				Endpoint: "https://api.z.ai/api/anthropic/v1/messages",
				Wire:     WireAnthropicMessages,
				Effort:   EffortDescriptor{Field: field, Levels: identityLevels()},
			},
		}}
		err := policy.Validate()
		if err == nil {
			t.Fatalf("anthropic route accepted effort field %q", field)
		}
		if !strings.Contains(err.Error(), "output_config.effort") {
			t.Fatalf("rejection of %q does not name the working spelling: %v", field, err)
		}
	}

	// And the converse: the coding wire may not borrow the Anthropic field.
	policy := Policy{Version: PolicyVersion, Routes: map[string]RoutePolicy{
		"zai/glm-5.3-flash": {
			Endpoint: "https://api.z.ai/api/coding/paas/v4/chat/completions",
			Wire:     WireOpenAIChat,
			Effort:   EffortDescriptor{Field: "output_config.effort", Levels: identityLevels()},
		},
	}}
	if err := policy.Validate(); err == nil {
		t.Fatal("the coding wire accepted output_config.effort")
	}
}

func TestPolicyValidateRejectsAPartialDocument(t *testing.T) {
	good := testPolicy()
	for name, mutate := range map[string]func(*Policy){
		"wrong version": func(p *Policy) { p.Version = 2 },
		"no version":    func(p *Policy) { p.Version = 0 },
		"no routes":     func(p *Policy) { p.Routes = map[string]RoutePolicy{} },
		"no endpoint": func(p *Policy) {
			r := p.Routes["deepseek-v4-flash"]
			r.Endpoint = ""
			p.Routes["deepseek-v4-flash"] = r
		},
		"relative endpoint": func(p *Policy) {
			r := p.Routes["deepseek-v4-flash"]
			r.Endpoint = "/v1/chat"
			p.Routes["deepseek-v4-flash"] = r
		},
		"no wire": func(p *Policy) { r := p.Routes["deepseek-v4-flash"]; r.Wire = ""; p.Routes["deepseek-v4-flash"] = r },
		"unknown wire": func(p *Policy) {
			r := p.Routes["deepseek-v4-flash"]
			r.Wire = "grpc"
			p.Routes["deepseek-v4-flash"] = r
		},
		"no effort field": func(p *Policy) {
			r := p.Routes["deepseek-v4-flash"]
			r.Effort.Field = ""
			p.Routes["deepseek-v4-flash"] = r
		},
		"empty effort map": func(p *Policy) {
			r := p.Routes["deepseek-v4-flash"]
			r.Effort.Levels = map[string]string{}
			p.Routes["deepseek-v4-flash"] = r
		},
		"negative window": func(p *Policy) {
			r := p.Routes["deepseek-v4-flash"]
			r.ContextWindow = -1
			p.Routes["deepseek-v4-flash"] = r
		},
	} {
		policy := Policy{Version: good.Version, Routes: map[string]RoutePolicy{}}
		for key, route := range good.Routes {
			policy.Routes[key] = route
		}
		mutate(&policy)
		if err := policy.Validate(); err == nil {
			t.Fatalf("a policy with %s validated", name)
		}
	}
	if err := good.Validate(); err != nil {
		t.Fatalf("the unmutated fixture must validate: %v", err)
	}
}

func TestEffortValueRefusesAnUnmappableLevel(t *testing.T) {
	// The ruling: an unmappable level rejects the request, naming the route,
	// the level asked for and what the route supports. Neither omitting the
	// field nor downgrading to the nearest level is acceptable — that is
	// exactly what pi's clampThinkingLevel did, walking max down to high
	// twice on 2026-09-08 before an explicit map was written.
	route := RoutePolicy{
		Endpoint: "https://api.deepseek.com/chat/completions",
		Wire:     WireOpenAIChat,
		Effort: EffortDescriptor{Field: "reasoning_effort", Levels: map[string]string{
			"low": "low", "medium": "medium", "high": "high",
		}},
	}
	_, err := route.EffortValue("deepseek-v4-flash", "xhigh")
	if err == nil {
		t.Fatal("an unmappable level was accepted")
	}
	for _, want := range []string{"deepseek-v4-flash", "xhigh", "high, low, medium"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal does not name %q: %v", want, err)
		}
	}

	// A level present but empty means "this route does not support it", which
	// is how a route drops one level without dropping the map.
	route.Effort.Levels["max"] = ""
	if _, err := route.EffortValue("deepseek-v4-flash", "max"); err == nil {
		t.Fatal("a level mapped to an empty value was accepted")
	}

	// A mappable level passes through, and no level asks for nothing.
	if value, err := route.EffortValue("deepseek-v4-flash", "high"); err != nil || value != "high" {
		t.Fatalf("mappable level = %q, err = %v", value, err)
	}
	if value, err := route.EffortValue("deepseek-v4-flash", ""); err != nil || value != "" {
		t.Fatalf("empty level = %q, err = %v", value, err)
	}
}

func TestRequestPayloadRefusesAnUnmappableLevelOnARoutedProvider(t *testing.T) {
	// The refusal has to reach the request, not merely exist on the type.
	t.Setenv("DEEPSEEK_API_KEY", "test-key-not-a-credential")
	policy := testPolicy()
	entry := policy.Routes["deepseek-v4-flash"]
	entry.Effort.Levels = map[string]string{"low": "low", "medium": "medium", "high": "high"}
	policy.Routes["deepseek-v4-flash"] = entry

	p, err := ForModel("deepseek-v4-flash", OpenRouterEndpoint, false, policy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.RequestPayload(Request{Model: "deepseek-v4-flash", Effort: "max", System: "s"}); err == nil {
		t.Fatal("a request at an unsupported effort was encoded")
	}

	// A supported level is rendered into the wire field, and the value sent is
	// the mapped one rather than the level name.
	payload, err := p.RequestPayload(Request{Model: "deepseek-v4-flash", Effort: "high", System: "s"})
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(payload, &body); err != nil {
		t.Fatal(err)
	}
	if body["reasoning_effort"] != "high" {
		t.Fatalf("reasoning_effort = %v, want high", body["reasoning_effort"])
	}
}

func TestDefaultLocalPolicyAuthorsRoutesThisBuildCanSpeak(t *testing.T) {
	// The escape hatch must produce a policy that works, not merely one that
	// parses: a generated policy on an unimplemented wire would refuse on its
	// own routes and leave the user exactly as stuck.
	t.Setenv("SLBH_LOCAL_ENDPOINT", "")
	policy := DefaultLocalPolicy([]string{"zai/glm-5.3-flash", "deepseek-v4-flash", LocalModelID, "vendor/model", "", "  "})
	if err := policy.Validate(); err != nil {
		t.Fatalf("an authored policy must validate: %v", err)
	}
	if len(policy.Routes) != 4 {
		t.Fatalf("authored %d routes, want 4: %#v", len(policy.Routes), policy.Routes)
	}
	for key, route := range policy.Routes {
		if !WireSupported(route.Wire) {
			t.Fatalf("route %q authored on unsupported wire %q", key, route.Wire)
		}
		if route.Endpoint == "" {
			t.Fatalf("route %q authored with no endpoint", key)
		}
	}
	// The plan route is authored on the coding endpoint, which is the wire
	// this build speaks, rather than on the Anthropic one the managed policy
	// uses.
	if got := policy.Routes["zai/glm-5.3-flash"].Endpoint; !strings.Contains(got, "/api/coding/") {
		t.Fatalf("plan route authored at %q, want the coding endpoint", got)
	}
	// An OpenRouter route is authored with the posture rather than without it:
	// a locally authored policy may be weaker than the managed one, but not by
	// omitting the guarantee entirely.
	posture := policy.Routes["vendor/model"].Provider
	if posture == nil || !posture.ZDR || posture.DataCollection != "deny" {
		t.Fatalf("openrouter route authored without posture: %#v", posture)
	}
	// And the local route keeps the identity effort map: the known local
	// effort defect (org/pending.md) stays visible rather than clamped.
	if got := policy.Routes[LocalModelID].Effort.Levels["max"]; got != "max" {
		t.Fatalf("local route clamps max to %q; it must pass through", got)
	}
}

func TestCommittedPolicyArtifactsValidate(t *testing.T) {
	// The two artifacts phase 1a committed are the schema's worked examples
	// and the fleet's target shape. If either stops validating, the document
	// and the code that reads it have drifted apart.
	managed := filepath.Join("..", "config", "testdata", "policy.json")
	data, err := os.ReadFile(managed)
	if err != nil {
		t.Fatal(err)
	}
	var policy Policy
	if err := json.Unmarshal(data, &policy); err != nil {
		t.Fatalf("decode %s: %v", managed, err)
	}
	if err := policy.Validate(); err != nil {
		t.Fatalf("%s does not validate: %v", managed, err)
	}
	// The managed shape is the one phase 2 is required for: it routes the plan
	// to anthropic-messages with output_config.effort and a measured pin.
	plan := policy.Routes["zai/glm-5.3-flash"]
	if plan.Wire != WireAnthropicMessages || plan.Effort.Field != "output_config.effort" || plan.ContextWindow != 1000000 {
		t.Fatalf("managed plan route = %#v", plan)
	}
	if plan.CatalogEndpoint == "" {
		t.Fatal("managed plan route has no catalogEndpoint; modelsEndpoint cannot derive it on that wire")
	}
	orr := policy.Routes["z-ai/glm-5.3-flash"]
	if orr.Provider == nil || !orr.Provider.ZDR || orr.Provider.DataCollection != "deny" {
		t.Fatalf("managed openrouter route posture = %#v", orr.Provider)
	}
	if orr.ContextWindow != 1048576 {
		t.Fatalf("managed openrouter pin = %d, want 1048576", orr.ContextWindow)
	}
	// Posture is scoped to the OpenRouter route alone: zdr and data_collection
	// are OpenRouter concepts and say nothing about the plan endpoint.
	for key, route := range policy.Routes {
		if key != "z-ai/glm-5.3-flash" && route.Provider != nil {
			t.Fatalf("route %q carries an OpenRouter posture block", key)
		}
	}
}
