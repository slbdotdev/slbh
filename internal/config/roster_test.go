package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const testRosterTOML = `version = 3

[names.intern]
harness = "slbh"
model = "local/q27-UD-Q2_K_XL-64k"
effort = "low"
depth = "outside"
launched_by = "owner"

[names.seat]
harness = "slbh"
model = "zai/glm-5.3-flash"
effort = "high"
depth = 0
launched_by = "owner"

[names.manager]
harness = "slbh"
model = "zai/glm-5.3-flash"
effort = "high"
depth = 1
launched_by = "seat"

[names.luna]
harness = "codex"
model = "gpt-5.6-luna"
effort = "high"
depth = 2
launched_by = "manager"

[names.flex]
harness = "slbh"
model = "at_dispatch"
models_approved = ["deepseek-v4-flash", "z-ai/glm-5.3-flash"]
effort = "high"
depth = 2
launched_by = "manager"
`

func TestLoadRosterNormalizesLaunchData(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, RosterFile)
	if err := os.WriteFile(path, []byte(testRosterTOML), 0o600); err != nil {
		t.Fatal(err)
	}
	roster := LoadRoster(home)
	if roster.Source.Kind != RosterManaged || roster.Source.Path != path || roster.Version != 3 {
		t.Fatalf("roster source = %#v", roster)
	}
	manager, ok := roster.Role("Manager")
	if !ok || manager.Name != "manager" || manager.Depth != 1 || manager.Harness != "slbh" || manager.Model != "zai/glm-5.3-flash" {
		t.Fatalf("manager role = %#v, present=%v", manager, ok)
	}
	intern, ok := roster.Role("intern")
	if !ok || intern.Depth != -1 {
		t.Fatalf("outside role = %#v, present=%v", intern, ok)
	}
	children := roster.ChildRoles("manager", 2)
	if names := []string{children[0].Name, children[1].Name}; !reflect.DeepEqual(names, []string{"flex", "luna"}) {
		t.Fatalf("manager child roles = %v", names)
	}
	children[0].ModelsApproved[0] = "mutated"
	flex, _ := roster.Role("flex")
	if flex.ModelsApproved[0] != "deepseek-v4-flash" {
		t.Fatal("returned role mutated roster data")
	}
}

func TestLoadRosterFailureRefusesManagedSource(t *testing.T) {
	missing := LoadRoster(t.TempDir())
	if missing.Source.Kind != RosterNone || !strings.Contains(missing.Source.Describe(), "child launches refuse") {
		t.Fatalf("missing source = %#v", missing.Source)
	}

	for name, content := range map[string]string{
		"syntax":  "version = [",
		"version": strings.Replace(testRosterTOML, "version = 3", "version = 2", 1),
		"depth":   strings.Replace(testRosterTOML, "depth = 1", "depth = \"nearby\"", 1),
	} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			if err := os.WriteFile(filepath.Join(home, RosterFile), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			got := LoadRoster(home)
			if got.Source.Kind != RosterNone || got.Source.Note == "" {
				t.Fatalf("invalid roster source = %#v", got.Source)
			}
		})
	}
}

func TestConfigLoadReadsManagedRoster(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SLBH_HOME", home)
	if err := os.WriteFile(filepath.Join(home, RosterFile), []byte(testRosterTOML), 0o600); err != nil {
		t.Fatal(err)
	}
	got := Load()
	if role, ok := got.Roster.Role("luna"); !ok || role.Model != "gpt-5.6-luna" {
		t.Fatalf("loaded roster role = %#v, present=%v", role, ok)
	}
}
