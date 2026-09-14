package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/slbdotdev/slbh/internal/provider"
)

// throwawayHome is a SLBH_HOME under the test's own temporary directory. No
// test here ever reads or writes a live config directory.
func throwawayHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("SLBH_HOME", home)
	for _, name := range []string{"SLBH_MODEL", "SLBH_EFFORT", "SLBH_SUBAGENT_MODEL", "SLBH_LEAF_MODEL", "SLBH_SUBAGENT_EFFORT", "SLBH_ENDPOINT"} {
		t.Setenv(name, "")
	}
	return home
}

func writeFile(t *testing.T, path string, body []byte) {
	t.Helper()
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
}

// artifact reads one of the committed schema examples. Using them rather than
// inline fixtures means these tests fail if the shipped documents and the
// loader drift apart.
func artifact(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func localPolicyFromArtifact(t *testing.T) provider.Policy {
	t.Helper()
	var doc struct {
		LocalPolicy provider.Policy `json:"local_policy"`
	}
	if err := json.Unmarshal(artifact(t, "config-with-local-policy.json"), &doc); err != nil {
		t.Fatal(err)
	}
	return doc.LocalPolicy
}

func TestPolicyPrecedenceManagedBeatsLocal(t *testing.T) {
	// Step one of the three-step rule. On a fleet host the managed file wins
	// outright, and the application cannot clobber it because the application
	// writes a different file.
	home := throwawayHome(t)
	writeFile(t, filepath.Join(home, "policy.json"), artifact(t, "policy.json"))
	writeFile(t, filepath.Join(home, "config.json"), artifact(t, "config-with-local-policy.json"))

	cfg := Load()
	if cfg.PolicySource.Kind != PolicyManaged {
		t.Fatalf("policy source = %#v, want managed", cfg.PolicySource)
	}
	if cfg.PolicySource.Path != filepath.Join(home, "policy.json") {
		t.Fatalf("managed path = %q", cfg.PolicySource.Path)
	}
	// The managed document routes the plan to the Anthropic wire; the local
	// one routes it to the coding wire. Which one is in force is therefore
	// observable on a field rather than only on the source label.
	plan, ok := cfg.Policy.Route("zai/glm-5.3-flash")
	if !ok {
		t.Fatal("managed policy has no plan route")
	}
	if plan.Wire != provider.WireAnthropicMessages {
		t.Fatalf("plan wire = %q, want the managed document's anthropic-messages", plan.Wire)
	}
	// The local block is still loaded and still persisted — it is simply not
	// in force.
	if cfg.LocalPolicy == nil {
		t.Fatal("the local policy block was dropped rather than carried")
	}
}

func TestPolicyPrecedenceLocalServesAnUnmanagedHost(t *testing.T) {
	// Step two. Without this the fail-closed refusal would be a brick on any
	// host ansible does not manage.
	home := throwawayHome(t)
	writeFile(t, filepath.Join(home, "config.json"), artifact(t, "config-with-local-policy.json"))

	cfg := Load()
	if cfg.PolicySource.Kind != PolicyLocal {
		t.Fatalf("policy source = %#v, want local", cfg.PolicySource)
	}
	plan, ok := cfg.Policy.Route("zai/glm-5.3-flash")
	if !ok {
		t.Fatal("local policy has no plan route")
	}
	if plan.Wire != provider.WireOpenAIChat {
		t.Fatalf("plan wire = %q, want the local document's openai-chat", plan.Wire)
	}
	if !strings.Contains(cfg.PolicySource.Describe(), "local") {
		t.Fatalf("source description does not say local: %q", cfg.PolicySource.Describe())
	}
}

func TestPolicyPrecedenceRefusesWhenBothSourcesAreAbsent(t *testing.T) {
	// Step three, and the suspenders. No policy from either source is a
	// refusal, not a default.
	home := throwawayHome(t)
	cfg := Load()
	if cfg.PolicySource.Kind != PolicyNone {
		t.Fatalf("policy source = %#v, want none", cfg.PolicySource)
	}
	if cfg.Policy.Defined() {
		t.Fatal("a policy was resolved from nothing")
	}
	if _, err := provider.ResolveRoute("zai/glm-5.3-flash", cfg.Endpoint, cfg.EndpointExplicit, cfg.Policy); err == nil {
		t.Fatal("a route resolved with no policy in force")
	}
	if _, err := os.Stat(filepath.Join(home, "policy.json")); !os.IsNotExist(err) {
		t.Fatal("the loader created a managed policy file; slbh only ever reads that file")
	}
}

func TestPolicyAbsentManagedFileIsNotAnError(t *testing.T) {
	// An unmanaged host has no policy.json at all, and that is an ordinary
	// state rather than a fault, so nothing is reported for it.
	home := throwawayHome(t)
	writeFile(t, filepath.Join(home, "config.json"), artifact(t, "config-with-local-policy.json"))
	cfg := Load()
	if cfg.PolicySource.Note != "" {
		t.Fatalf("a missing managed file produced a note: %q", cfg.PolicySource.Note)
	}
}

func TestPolicyInvalidManagedFileDegradesVisibly(t *testing.T) {
	// The precedence rule says "present and valid", so an invalid managed file
	// falls through — but silently substituting a weaker local policy for the
	// org's own is the failure this note exists to prevent.
	home := throwawayHome(t)
	writeFile(t, filepath.Join(home, "policy.json"), []byte(`{"version":1,"routes":{"zai/glm-5.3-flash":{"wire":"openai-chat"}}}`))
	writeFile(t, filepath.Join(home, "config.json"), artifact(t, "config-with-local-policy.json"))

	cfg := Load()
	if cfg.PolicySource.Kind != PolicyLocal {
		t.Fatalf("policy source = %#v, want local", cfg.PolicySource)
	}
	if !strings.Contains(cfg.PolicySource.Note, "invalid") {
		t.Fatalf("fallthrough is silent: note = %q", cfg.PolicySource.Note)
	}
	if !strings.Contains(cfg.PolicySource.Describe(), "invalid") {
		t.Fatalf("description hides the fallthrough: %q", cfg.PolicySource.Describe())
	}

	// Unparseable bytes take the same path and say so too.
	writeFile(t, filepath.Join(home, "policy.json"), []byte("{not json"))
	if note := Load().PolicySource.Note; !strings.Contains(note, "unreadable") {
		t.Fatalf("unparseable managed policy note = %q", note)
	}
}

func TestPolicyRoundTripsThroughSave(t *testing.T) {
	// A policy authored from /models has to survive a restart, and it has to
	// survive every other save: without LocalPolicy on the round trip, a
	// /effort save would delete it and the next launch would refuse
	// everything.
	home := throwawayHome(t)
	authored := provider.DefaultLocalPolicy([]string{"deepseek-v4-flash", provider.LocalModelID})

	cfg := Load()
	if err := cfg.ApplyLocalPolicy(authored); err != nil {
		t.Fatal(err)
	}
	if cfg.PolicySource.Kind != PolicyLocal {
		t.Fatalf("source after authoring = %#v, want local", cfg.PolicySource)
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	reloaded := Load()
	if reloaded.PolicySource.Kind != PolicyLocal {
		t.Fatalf("source after reload = %#v, want local", reloaded.PolicySource)
	}
	if len(reloaded.Policy.Routes) != len(authored.Routes) {
		t.Fatalf("reloaded %d routes, wrote %d", len(reloaded.Policy.Routes), len(authored.Routes))
	}
	for key, want := range authored.Routes {
		got, ok := reloaded.Policy.Route(key)
		if !ok {
			t.Fatalf("route %q did not survive the round trip", key)
		}
		if got.Endpoint != want.Endpoint || got.Wire != want.Wire || got.Effort.Field != want.Effort.Field {
			t.Fatalf("route %q changed across the round trip:\nwant %#v\n got %#v", key, want, got)
		}
	}

	// An unrelated save must not drop it.
	reloaded.SeatEffort = "medium"
	if err := reloaded.Save(); err != nil {
		t.Fatal(err)
	}
	if after := Load(); after.PolicySource.Kind != PolicyLocal || len(after.Policy.Routes) != len(authored.Routes) {
		t.Fatalf("an unrelated save dropped the local policy: %#v", after.PolicySource)
	}

	// And the managed file is untouched by any of it.
	if _, err := os.Stat(filepath.Join(home, "policy.json")); !os.IsNotExist(err) {
		t.Fatal("saving wrote into the managed policy file")
	}
}

func TestLocalWriteDoesNotAlterBehaviourWhileManagedIsPresent(t *testing.T) {
	// The property the split file exists for. A local write on a fleet host is
	// recorded and persisted, and changes nothing about how requests route.
	home := throwawayHome(t)
	writeFile(t, filepath.Join(home, "policy.json"), artifact(t, "policy.json"))

	before := Load()
	if before.PolicySource.Kind != PolicyManaged {
		t.Fatalf("source = %#v, want managed", before.PolicySource)
	}
	beforePlan, _ := before.Policy.Route("zai/glm-5.3-flash")

	cfg := before
	weaker := provider.DefaultLocalPolicy([]string{"zai/glm-5.3-flash"})
	if err := cfg.ApplyLocalPolicy(weaker); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	after := Load()
	if after.PolicySource.Kind != PolicyManaged {
		t.Fatalf("a local write changed the policy in force: %#v", after.PolicySource)
	}
	afterPlan, ok := after.Policy.Route("zai/glm-5.3-flash")
	if !ok {
		t.Fatal("the managed plan route disappeared")
	}
	// Asserted on fields, not on the source label: the managed document routes
	// the plan to the Anthropic wire with a measured pin, and the local one
	// would have routed it to the coding wire.
	if afterPlan.Wire != beforePlan.Wire || afterPlan.Endpoint != beforePlan.Endpoint || afterPlan.ContextWindow != beforePlan.ContextWindow {
		t.Fatalf("the managed route changed:\nbefore %#v\n after %#v", beforePlan, afterPlan)
	}
	if afterPlan.ContextWindow != 1000000 {
		t.Fatalf("managed pin = %d, want 1000000", afterPlan.ContextWindow)
	}
	// The local block was still written down, so moving the host off ansible
	// later leaves a working policy behind.
	if after.LocalPolicy == nil {
		t.Fatal("the local policy was not persisted")
	}
}

func TestApprovedModelsFailSafeSurvivesThePolicyWork(t *testing.T) {
	// nil means "a config written before approval existed: permissive"; a
	// non-nil empty slice means "nothing approved yet", which is the
	// cost-control fail-safe. Phase 1c must not have collapsed the two.
	home := throwawayHome(t)
	writeFile(t, filepath.Join(home, "config.json"), []byte(`{"seat_model":"deepseek-v4-flash"}`))
	if models := Load().ApprovedModels; models != nil {
		t.Fatalf("a legacy config without approved_models = %#v, want nil", models)
	}
	writeFile(t, filepath.Join(home, "config.json"), []byte(`{"seat_model":"deepseek-v4-flash","approved_models":[]}`))
	cfg := Load()
	if cfg.ApprovedModels == nil || len(cfg.ApprovedModels) != 0 {
		t.Fatalf("an explicit empty approved_models = %#v, want non-nil empty", cfg.ApprovedModels)
	}
	if cfg.ModelApproved("deepseek-v4-flash") {
		t.Fatal("an empty approval list approved a model")
	}
}
