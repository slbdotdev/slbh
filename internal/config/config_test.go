package config

import (
	"os"
	"testing"
)

func TestSaveAndLoadModelPolicy(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SLBH_HOME", home)
	for _, name := range []string{"SLBH_MODEL", "SLBH_EFFORT", "SLBH_SUBAGENT_MODEL", "SLBH_LEAF_MODEL", "SLBH_SUBAGENT_EFFORT", "SLBH_PROVIDER", "SLBH_ENDPOINT"} {
		t.Setenv(name, "")
	}
	want := Config{
		Home: home, RootModel: "deepseek/deepseek-chat", RootEffort: "high",
		SubagentModel: "zai/glm-5.3-flash", LeafModel: "deepseek/deepseek-chat", SubagentEffort: "medium",
		ApprovedModels: []string{"deepseek/deepseek-chat", "zai/glm-5.3-flash", "deepseek/deepseek-chat"},
	}
	if err := want.Save(); err != nil {
		t.Fatal(err)
	}
	got := Load()
	if got.RootModel != want.RootModel || got.SubagentModel != want.SubagentModel || got.LeafModel != want.LeafModel || len(got.ApprovedModels) != 2 {
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
