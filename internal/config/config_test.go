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
	for _, name := range []string{"SLBH_MODEL", "SLBH_EFFORT", "SLBH_SUBAGENT_MODEL", "SLBH_LEAF_MODEL", "SLBH_SUBAGENT_EFFORT", "SLBH_ENDPOINT"} {
		t.Setenv(name, "")
	}
	want := Config{
		Home: home, SeatModel: "deepseek/deepseek-chat", SeatEffort: "high",
		SubagentModel: "zai/glm-5.3-flash", LeafModel: "deepseek/deepseek-chat", SubagentEffort: "medium",
		ApprovedModels: []string{"deepseek/deepseek-chat", "zai/glm-5.3-flash", "deepseek/deepseek-chat"},
	}
	if err := want.Save(); err != nil {
		t.Fatal(err)
	}
	got := Load()
	if got.SeatModel != want.SeatModel || got.SubagentModel != want.SubagentModel || got.LeafModel != want.LeafModel || len(got.ApprovedModels) != 2 {
		t.Fatalf("loaded config = %#v", got)
	}
	if got.ModelApproved("zai/glm-5.3-flash") == false || got.ModelApproved("unknown") {
		t.Fatalf("loaded approval policy is wrong: %#v", got.ApprovedModels)
	}
	if _, err := os.Stat(home + string(os.PathSeparator) + "config.json"); err != nil {
		t.Fatal(err)
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

// TestDefaultSeatEffortIsServableLocally pins the built-in seat effort. The
// default has to be a level every route slbh ships with can actually serve,
// and the local Ollama route is the narrow one: Ollama rewrites `xhigh` to
// `max` before the model's chat template runs, and the template raises on
// `max`, so a seat defaulting to `xhigh` fails its first local request with a
// 500. `high` is the top level that route serves.
func TestDefaultSeatEffortIsServableLocally(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SLBH_HOME", home)
	for _, name := range []string{"SLBH_MODEL", "SLBH_EFFORT", "SLBH_SUBAGENT_MODEL", "SLBH_LEAF_MODEL", "SLBH_SUBAGENT_EFFORT", "SLBH_ENDPOINT"} {
		t.Setenv(name, "")
	}
	got := Load()
	if got.SeatEffort != "high" {
		t.Fatalf("default seat effort = %q, want high", got.SeatEffort)
	}
	if got.SeatEffort == "xhigh" || got.SeatEffort == "max" {
		t.Fatalf("default seat effort %q cannot be served by the local route", got.SeatEffort)
	}
}
