package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeLayers deploys the named layer documents into a throwaway SLBH_HOME,
// the way ansible deploys them, and returns the home.
func writeLayers(t *testing.T, docs map[string]string) string {
	t.Helper()
	home := t.TempDir()
	if len(docs) == 0 {
		return home
	}
	dir := filepath.Join(home, InstructionsDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for layer, text := range docs {
		if err := os.WriteFile(filepath.Join(dir, layer+".md"), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

// The depth-to-layer mapping is the whole security-relevant surface of this
// feature: an off-by-one here hands a leaf the manager document, which is the
// one that says an agent may launch children.
func TestLayerForDepthMapsEachLevel(t *testing.T) {
	for _, tc := range []struct {
		depth int
		want  string
	}{
		{-1, LayerSeat},
		{0, LayerSeat},
		{1, LayerManager},
		{2, LayerLeaf},
		{3, LayerLeaf},
		{99, LayerLeaf},
	} {
		if got := LayerForDepth(tc.depth); got != tc.want {
			t.Fatalf("LayerForDepth(%d) = %q, want %q", tc.depth, got, tc.want)
		}
	}
}

func TestLoadInstructionsReadsEachLayerIntoItsOwnSlot(t *testing.T) {
	home := writeLayers(t, map[string]string{
		LayerSeat:    "seat doc: converge and commit",
		LayerManager: "manager doc: launch level-two leaves",
		LayerLeaf:    "leaf doc: launch nothing",
	})
	loaded := LoadInstructions(home)
	if loaded.Source.Kind != InstructionsManaged {
		t.Fatalf("source kind = %q, want %q", loaded.Source.Kind, InstructionsManaged)
	}
	if len(loaded.Source.Missing) != 0 {
		t.Fatalf("missing = %v, want none", loaded.Source.Missing)
	}
	// Assert each layer carries its OWN document, not merely that something
	// was loaded: a loader that put the same text in all three slots, or that
	// transposed two of them, would pass a non-empty check.
	for depth, want := range map[int]string{
		0: "seat doc: converge and commit",
		1: "manager doc: launch level-two leaves",
		2: "leaf doc: launch nothing",
	} {
		if got := loaded.For(depth); got != want {
			t.Fatalf("For(%d) = %q, want %q", depth, got, want)
		}
	}
}

// The fleet may deploy the leaf document before the others, or withdraw one.
// A partial deployment must serve what exists and say what does not.
func TestLoadInstructionsReportsPartialDeployment(t *testing.T) {
	home := writeLayers(t, map[string]string{LayerLeaf: "leaf doc"})
	loaded := LoadInstructions(home)
	if loaded.Source.Kind != InstructionsManaged {
		t.Fatalf("source kind = %q, want %q", loaded.Source.Kind, InstructionsManaged)
	}
	if got := loaded.For(2); got != "leaf doc" {
		t.Fatalf("leaf = %q, want %q", got, "leaf doc")
	}
	if got := loaded.For(0); got != "" {
		t.Fatalf("seat = %q, want empty", got)
	}
	if strings.Join(loaded.Source.Missing, ",") != LayerSeat+","+LayerManager {
		t.Fatalf("missing = %v, want [seat manager]", loaded.Source.Missing)
	}
	if !strings.Contains(loaded.Source.Describe(), "no seat, manager document") {
		t.Fatalf("Describe() = %q, want it to name the missing layers", loaded.Source.Describe())
	}
}

// An unmanaged host has no instructions directory at all. That is not an
// error: the agent runs on the baked system prompt alone.
func TestLoadInstructionsOnUnmanagedHostDegradesWithoutError(t *testing.T) {
	loaded := LoadInstructions(t.TempDir())
	if loaded.Source.Kind != InstructionsNone {
		t.Fatalf("source kind = %q, want %q", loaded.Source.Kind, InstructionsNone)
	}
	if loaded.Source.Note != "" {
		t.Fatalf("note = %q, want empty: a host that was never converged is not an error", loaded.Source.Note)
	}
	for _, depth := range []int{0, 1, 2} {
		if got := loaded.For(depth); got != "" {
			t.Fatalf("For(%d) = %q, want empty", depth, got)
		}
	}
	if !strings.Contains(loaded.Source.Describe(), "baked system prompt alone") {
		t.Fatalf("Describe() = %q, want it to say what the agent falls back to", loaded.Source.Describe())
	}
}

// An empty document is no instruction. Treating it as one would append a
// header announcing org policy with nothing under it, which reads to a model
// as an instruction that was cut off.
func TestLoadInstructionsTreatsBlankDocumentAsAbsent(t *testing.T) {
	home := writeLayers(t, map[string]string{LayerSeat: "   \n\t\n", LayerLeaf: "leaf doc"})
	loaded := LoadInstructions(home)
	if got := loaded.For(0); got != "" {
		t.Fatalf("blank seat doc = %q, want empty", got)
	}
	if _, present := loaded.Layers[LayerSeat]; present {
		t.Fatal("blank document was stored as a layer; absent and empty must be the same fact")
	}
}

// A file that exists but cannot be read is a different fact from one that was
// never deployed, and an operator has to be able to tell them apart.
func TestLoadInstructionsDistinguishesUnreadableFromAbsent(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read a 0000 file")
	}
	home := writeLayers(t, map[string]string{LayerManager: "manager doc"})
	path := filepath.Join(home, InstructionsDir, LayerManager+".md")
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })
	loaded := LoadInstructions(home)
	if loaded.For(1) != "" {
		t.Fatal("unreadable document was served")
	}
	if !strings.Contains(loaded.Source.Note, "unreadable") {
		t.Fatalf("note = %q, want it to report the unreadable file", loaded.Source.Note)
	}
}

// Instructions must not be a second way to configure routing. Load resolves
// both, and this pins that a host with instructions but no policy still
// refuses to route: degrading one must not degrade the other.
func TestInstructionsDoNotSubstituteForPolicy(t *testing.T) {
	home := writeLayers(t, map[string]string{LayerSeat: "seat doc"})
	t.Setenv("SLBH_HOME", home)
	cfg := Load()
	if cfg.Instructions.For(0) != "seat doc" {
		t.Fatal("instructions did not load through Load()")
	}
	if cfg.PolicySource.Kind != PolicyNone {
		t.Fatalf("policy source = %q, want %q", cfg.PolicySource.Kind, PolicyNone)
	}
	if len(cfg.Policy.Routes) != 0 {
		t.Fatalf("routes = %d, want 0: instructions must not supply a route", len(cfg.Policy.Routes))
	}
}
