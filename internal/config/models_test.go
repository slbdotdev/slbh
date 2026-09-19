package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testModelsTOML = `version = 1

[models.astra]
model = "gpt-6-astra"
effort = "low"

[models.luna]
model = "gpt-5.6-luna"
effort = "xhigh"
`

func TestLoadModelsResolvesShortNames(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ModelsFile)
	if err := os.WriteFile(path, []byte(testModelsTOML), 0o600); err != nil {
		t.Fatal(err)
	}
	models := LoadModels(home)
	if models.Source.Kind != ModelsManaged || models.Source.Path != path {
		t.Fatalf("models source = %#v", models.Source)
	}
	if model, effort := models.Resolve(" Luna "); model != "gpt-5.6-luna" || effort != "xhigh" {
		t.Fatalf("luna = %q/%q", model, effort)
	}
	if model, effort := models.Resolve("vendor/model"); model != "vendor/model" || effort != "" {
		t.Fatalf("full model = %q/%q", model, effort)
	}
}

func TestLoadModelsDegradesToFullStrings(t *testing.T) {
	missing := LoadModels(t.TempDir())
	if missing.Source.Kind != ModelsNone || !strings.Contains(missing.Source.Describe(), "full model strings remain valid") {
		t.Fatalf("missing source = %#v", missing.Source)
	}

	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ModelsFile), []byte("version = ["), 0o600); err != nil {
		t.Fatal(err)
	}
	got := LoadModels(home)
	if got.Source.Kind != ModelsNone || got.Source.Note == "" {
		t.Fatalf("invalid source = %#v", got.Source)
	}
	if model, _ := got.Resolve("vendor/model"); model != "vendor/model" {
		t.Fatalf("invalid catalogue changed full model to %q", model)
	}
}

func TestConfigLoadReadsManagedModels(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SLBH_HOME", home)
	if err := os.WriteFile(filepath.Join(home, ModelsFile), []byte(testModelsTOML), 0o600); err != nil {
		t.Fatal(err)
	}
	got := Load()
	if model, effort := got.Models.Resolve("astra"); model != "gpt-6-astra" || effort != "low" {
		t.Fatalf("loaded models = %q/%q", model, effort)
	}
}
