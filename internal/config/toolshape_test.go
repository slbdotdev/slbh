package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestToolShapePrecedenceAndPersistence(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SLBH_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "config.json"), []byte(`{"approved_models":[],"tool_shape":"codex"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := Load().ToolShape; got != ToolShapeCodex {
		t.Fatalf("file shape = %q", got)
	}
	t.Setenv("SLBH_TOOL_SHAPE", "anthropic")
	cfg := Load()
	if cfg.ToolShape != ToolShapeAnthropic {
		t.Fatalf("env shape = %q", cfg.ToolShape)
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(home, "config.json"))
	if !strings.Contains(string(data), `"tool_shape": "codex"`) || strings.Contains(string(data), "anthropic") {
		t.Fatalf("Save wrote the override back: %s", data)
	}
}

func TestNormalizeToolShape(t *testing.T) {
	for input, want := range map[string]string{"": ToolShapeLean, " codex ": ToolShapeCodex, "anthropic": ToolShapeAnthropic, "full": ToolShapeFull, "mid": ToolShapeMid, "lean": ToolShapeLean} {
		if got, err := NormalizeToolShape(input); err != nil || got != want {
			t.Fatalf("%q = %q, %v", input, got, err)
		}
	}
	if _, err := NormalizeToolShape("slbh"); err == nil {
		t.Fatal("the retired name slbh was accepted")
	}
	if _, err := NormalizeToolShape("Anthropic"); err == nil {
		t.Fatal("a near miss was accepted")
	}
}
