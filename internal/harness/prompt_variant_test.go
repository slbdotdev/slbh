package harness

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/slbdotdev/slbh/internal/config"
	"github.com/slbdotdev/slbh/internal/provider"
)

func variantRuntime(t *testing.T, variant string) *Runtime {
	t.Helper()
	r, err := New(config.Config{Home: t.TempDir(), SeatModel: "test", SeatEffort: "high", SubagentModel: "test-child", SubagentEffort: "high", ToolShape: config.ToolShapeLean, PromptVariant: variant}, Options{Provider: func(string) (provider.Provider, error) { return fakeProvider{}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

func TestUnknownPromptVariantIsRefused(t *testing.T) {
	_, err := New(config.Config{Home: t.TempDir(), SeatModel: "test", PromptVariant: "mian"}, Options{Provider: func(string) (provider.Provider, error) { return fakeProvider{}, nil }})
	if err == nil || !strings.Contains(err.Error(), `unknown prompt variant "mian"`) {
		t.Fatalf("mistyped variant was not refused: %v", err)
	}
	if got, err := config.NormalizePromptVariant(""); err != nil || got != config.PromptVariantInfo {
		t.Fatalf("empty variant = %q, %v; want info", got, err)
	}
}

// The main variant is the comparison's baseline, so it must be the prompt as
// it stood on main: the thinking paragraph and the behavioural rules included.
func TestMainPromptVariantKeepsTheOldPrompt(t *testing.T) {
	r := variantRuntime(t, config.PromptVariantMain)
	prompt := bakedSystemPrompt(r.seat())
	for _, phrase := range []string{
		"Keep your thinking brief and focused",
		"use it for temporary files instead of /tmp, your working directory, or your home directory.",
		"Act on each in the current turn; never defer one to the end.",
		"Decide then: kill or end it, keep waiting for its result, or do other work.",
		"Never sleep, poll, or run wait loops to watch one",
		"Launch one child at the next depth with a relevant title.",
	} {
		if !strings.Contains(prompt, phrase) {
			t.Fatalf("main variant lost %q: %s", phrase, prompt)
		}
	}
	if !reflect.DeepEqual(r.toolDefinitions(r.seat().ID), variantRuntime(t, config.PromptVariantInfo).toolDefinitions(r.seat().ID)) {
		t.Fatal("main and info variants must offer identical tool definitions")
	}
}

// facts changes exactly four descriptions and nothing else:
// the arm it serves has to differ from lean only in those words.
func TestFactsPromptVariantOnlyExtendsCommandAndPatchDescriptions(t *testing.T) {
	info, facts := variantRuntime(t, config.PromptVariantInfo), variantRuntime(t, config.PromptVariantFacts)
	if normalizedPrompt(facts) != normalizedPrompt(info) {
		t.Fatal("facts and info variants must share one system prompt")
	}
	base := buildToolDefinitions(config.ToolShapeLean, config.PromptVariantInfo)
	extended := buildToolDefinitions(config.ToolShapeLean, config.PromptVariantFacts)
	if len(base) != len(extended) {
		t.Fatalf("facts changed the tool count: %d vs %d", len(base), len(extended))
	}
	changed := map[string]bool{}
	for i := range base {
		if base[i].Name != extended[i].Name || !reflect.DeepEqual(base[i].Parameters, extended[i].Parameters) {
			t.Fatalf("facts changed tool %s beyond its description", base[i].Name)
		}
		if base[i].Description == extended[i].Description {
			continue
		}
		changed[base[i].Name] = true
		want := base[i].Description + commandOutputFacts
		if base[i].Name == "apply_patch" {
			// The placement sentence is replaced, not contradicted.
			want = strings.TrimSuffix(base[i].Description, applyPatchPlacement) + applyPatchFacts
			if strings.Contains(extended[i].Description, "not line numbers") {
				t.Fatal("facts apply_patch still says hunks ignore line numbers")
			}
		}
		if extended[i].Description != want {
			t.Fatalf("%s description is not its fact variant: %q", base[i].Name, extended[i].Description)
		}
	}
	for _, name := range []string{"apply_patch", "bash", "python"} {
		if !changed[name] {
			t.Fatalf("facts variant left %s unchanged", name)
		}
	}
	for name := range changed {
		if name != "apply_patch" && name != "bash" && name != "python" && name != "pwsh" {
			t.Fatalf("facts variant changed %s", name)
		}
	}
}

// normalizedPrompt is the seat's baked prompt with the per-runtime names
// replaced, so prompts from two runtimes compare by their text alone.
func normalizedPrompt(r *Runtime) string {
	seat := r.seat()
	return strings.NewReplacer(r.runtimeDir, "DIR", r.ID(), "RUN", seat.ID, "AGENT").Replace(bakedSystemPrompt(seat))
}

// The runner asserts the variant from the runtime-start event, so it must be
// there for every variant, the default included.
func TestRuntimeStartEventRecordsPromptVariant(t *testing.T) {
	for _, variant := range []string{"", config.PromptVariantMain, config.PromptVariantFacts} {
		want := variant
		if want == "" {
			want = config.PromptVariantInfo
		}
		r := variantRuntime(t, variant)
		deadline := time.After(2 * time.Second)
		events := testEvents(r)
	wait:
		for {
			select {
			case event := <-events:
				if event.Kind == "runtime" {
					if event.Metadata["prompt_variant"] != want {
						t.Fatalf("variant %q: runtime event metadata %v", variant, event.Metadata)
					}
					break wait
				}
			case <-deadline:
				t.Fatalf("variant %q: no runtime event", variant)
			}
		}
	}
}
