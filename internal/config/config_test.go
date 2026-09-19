package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/slbdotdev/slbh/internal/provider"
)

func TestSaveAndLoadModelPolicy(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SLBH_HOME", home)
	for _, name := range []string{"SLBH_SECRETARY_SESSION", "SLBH_MODEL", "SLBH_EFFORT", "SLBH_SUBAGENT_MODEL", "SLBH_LEAF_MODEL", "SLBH_SUBAGENT_EFFORT", "SLBH_ENDPOINT"} {
		t.Setenv(name, "")
	}
	want := Config{
		Home: home, SecretarySession: "named-secretary", SeatModel: "deepseek/deepseek-chat", SeatEffort: "high",
		SubagentModel: "zai/glm-5.3-flash", LeafModel: "deepseek/deepseek-chat", SubagentEffort: "medium",
		ApprovedModels: []string{"deepseek/deepseek-chat", "zai/glm-5.3-flash", "deepseek/deepseek-chat"},
	}
	if err := want.Save(); err != nil {
		t.Fatal(err)
	}
	got := Load()
	if got.SecretarySession != want.SecretarySession || got.SeatModel != want.SeatModel || got.SubagentModel != want.SubagentModel || got.LeafModel != want.LeafModel || len(got.ApprovedModels) != 2 {
		t.Fatalf("loaded config = %#v", got)
	}
	if got.ModelApproved("zai/glm-5.3-flash") == false || got.ModelApproved("unknown") {
		t.Fatalf("loaded approval policy is wrong: %#v", got.ApprovedModels)
	}
	if _, err := os.Stat(home + string(os.PathSeparator) + "config.json"); err != nil {
		t.Fatal(err)
	}
}

func TestSecretarySessionDefaultEnvironmentAndPersistence(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SLBH_HOME", home)
	t.Setenv("SLBH_SECRETARY_SESSION", "")
	if got := Load().SecretarySession; got != "secretary" {
		t.Fatalf("default SecretarySession = %q, want secretary", got)
	}

	t.Setenv("SLBH_SECRETARY_SESSION", "env-secretary")
	if got := Load().SecretarySession; got != "env-secretary" {
		t.Fatalf("environment SecretarySession = %q", got)
	}

	t.Setenv("SLBH_SECRETARY_SESSION", "")
	cfg := Load()
	cfg.SecretarySession = "persisted-secretary"
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	if got := Load().SecretarySession; got != "persisted-secretary" {
		t.Fatalf("persisted SecretarySession = %q", got)
	}
}

func TestEmptyApprovedListFailsClosed(t *testing.T) {
	if (Config{ApprovedModels: []string{}}).ModelApproved("any/model") {
		t.Fatal("empty approval list must reject models")
	}
	if !(Config{}).ModelApproved("legacy/model") {
		t.Fatal("nil approval list should preserve programmatic compatibility")
	}
}

func TestLoadMigratesLegacyRootModelConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SLBH_HOME", home)
	for _, name := range []string{"SLBH_MODEL", "SLBH_EFFORT", "SLBH_SUBAGENT_MODEL", "SLBH_LEAF_MODEL", "SLBH_SUBAGENT_EFFORT"} {
		t.Setenv(name, "")
	}
	legacy := map[string]string{
		"root_model":     "deepseek/deepseek-chat",
		"root_effort":    "high",
		"subagent_model": "zai/glm-5.3-flash",
	}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	got := Load()
	if got.SeatModel != legacy["root_model"] || got.SeatEffort != legacy["root_effort"] {
		t.Fatalf("legacy seat settings were not migrated: %#v", got)
	}
	if got.SubagentModel != legacy["subagent_model"] {
		t.Fatalf("subagent model = %q, want %q", got.SubagentModel, legacy["subagent_model"])
	}
	if got.ApprovedModels != nil || !got.ModelApproved(got.SeatModel) {
		t.Fatalf("legacy approval policy = %#v, want permissive nil policy", got.ApprovedModels)
	}
}

func TestDefaultLeafModelIsLocalWorkhorse(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SLBH_HOME", home)
	for _, name := range []string{"SLBH_MODEL", "SLBH_EFFORT", "SLBH_SUBAGENT_MODEL", "SLBH_LEAF_MODEL", "SLBH_SUBAGENT_EFFORT"} {
		t.Setenv(name, "")
	}
	got := Load()
	if got.LeafModel != provider.LocalModelID {
		t.Fatalf("leaf model = %q, want %q", got.LeafModel, provider.LocalModelID)
	}
}

func TestInternModelDefaultEnvironmentAndPersistence(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SLBH_HOME", home)
	t.Setenv("SLBH_INTERN_MODEL", "")
	if got := Load().InternModel; got != "local/q27-UD-Q2_K_XL-64k" {
		t.Fatalf("default InternModel = %q", got)
	}

	t.Setenv("SLBH_INTERN_MODEL", "local/override")
	if got := Load().InternModel; got != "local/override" {
		t.Fatalf("environment InternModel = %q", got)
	}

	t.Setenv("SLBH_INTERN_MODEL", "")
	cfg := Load()
	cfg.InternModel = "local/persisted"
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	if got := Load().InternModel; got != "local/persisted" {
		t.Fatalf("persisted InternModel = %q", got)
	}
}

func TestSecretaryWakeAndInternEffortDefaultsEnvironmentAndPersistence(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SLBH_HOME", home)
	t.Setenv("SLBH_SECRETARY_WAKE", "")
	t.Setenv("SLBH_INTERN_EFFORT", "")
	got := Load()
	if !got.SecretaryWake || got.InternEffort != "medium" {
		t.Fatalf("defaults = SecretaryWake %v, InternEffort %q", got.SecretaryWake, got.InternEffort)
	}

	t.Setenv("SLBH_SECRETARY_WAKE", "false")
	t.Setenv("SLBH_INTERN_EFFORT", "high")
	got = Load()
	if got.SecretaryWake || got.InternEffort != "high" {
		t.Fatalf("environment = SecretaryWake %v, InternEffort %q", got.SecretaryWake, got.InternEffort)
	}

	t.Setenv("SLBH_SECRETARY_WAKE", "")
	t.Setenv("SLBH_INTERN_EFFORT", "")
	got.SecretaryWake = false
	got.InternEffort = "low"
	if err := got.Save(); err != nil {
		t.Fatal(err)
	}
	got = Load()
	if got.SecretaryWake || got.InternEffort != "low" {
		t.Fatalf("persisted = SecretaryWake %v, InternEffort %q", got.SecretaryWake, got.InternEffort)
	}
}

// TestDefaultEffortsAreServableLocally pins the built-in effort defaults. The
// local Ollama route is the narrow one: Ollama rewrites `xhigh` to `max` before
// the model's chat template runs, and the template raises on `max`, so a
// defaulting agent must stay at a directly servable level.
func TestDefaultEffortsAreServableLocally(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SLBH_HOME", home)
	for _, name := range []string{"SLBH_MODEL", "SLBH_EFFORT", "SLBH_SUBAGENT_MODEL", "SLBH_LEAF_MODEL", "SLBH_SUBAGENT_EFFORT", "SLBH_ENDPOINT"} {
		t.Setenv(name, "")
	}
	got := Load()
	if got.SeatEffort != "medium" || got.InternEffort != "medium" || got.SubagentEffort != "medium" {
		t.Fatalf("default efforts = seat %q, intern %q, subagent %q; want medium everywhere", got.SeatEffort, got.InternEffort, got.SubagentEffort)
	}
	if got.SeatEffort == "xhigh" || got.SeatEffort == "max" {
		t.Fatalf("default seat effort %q cannot be served by the local route", got.SeatEffort)
	}
}
