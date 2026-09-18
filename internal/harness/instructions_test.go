package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/slbdotdev/slbh/internal/config"
	"github.com/slbdotdev/slbh/internal/provider"
)

// The three documents carry text that is unmistakably wrong for the other two
// layers, so a test cannot pass by delivering any document at all. "launch
// level-two leaves" reaching a leaf is the failure this whole feature has to
// make impossible.
const (
	seatDoc    = "SEAT-DOC: converge hosts, commit for the fleet, close the session."
	managerDoc = "MANAGER-DOC: launch level-two leaves and own a branch; never converge a host."
	leafDoc    = "LEAF-DOC: do the work and report; launch nothing."
)

// runtimeWithInstructions builds a runtime whose SLBH_HOME carries the named
// layer documents, deployed the way ansible deploys them.
func runtimeWithInstructions(t *testing.T, docs map[string]string) *Runtime {
	return runtimeWithInstructionsAndSkills(t, docs, nil)
}

// runtimeWithInstructionsAndSkills adds managed skill files before config is
// loaded so prompt tests exercise the same $SLBH_HOME layout as production.
func runtimeWithInstructionsAndSkills(t *testing.T, docs map[string]string, skills map[string]map[string]string) *Runtime {
	t.Helper()
	home := t.TempDir()
	if len(docs) > 0 {
		dir := filepath.Join(home, config.InstructionsDir)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		for layer, text := range docs {
			if err := os.WriteFile(filepath.Join(dir, layer+".md"), []byte(text), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	for layer, layerSkills := range skills {
		for directory, document := range layerSkills {
			dir := filepath.Join(home, config.SkillsDir, layer, directory)
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(document), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	cfg := config.Config{
		Home: home, SeatModel: "test", SeatEffort: "high",
		SubagentModel: "test-child", SubagentEffort: "high",
		Instructions: config.LoadInstructions(home),
		Skills:       config.LoadSkills(home),
	}
	r, err := New(cfg, Options{Provider: func(string) (provider.Provider, error) { return fakeProvider{}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

func allThreeDocs() map[string]string {
	return map[string]string{
		config.LayerSeat:    seatDoc,
		config.LayerManager: managerDoc,
		config.LayerLeaf:    leafDoc,
	}
}

// agentAtDepth returns the seat, a level-one manager, or a level-two leaf.
func agentAtDepth(t *testing.T, r *Runtime, depth int) *Agent {
	t.Helper()
	agent := r.seat()
	for i := 0; i < depth; i++ {
		child, err := r.launchSubagent(agent.ID, "child-agent-here", "")
		if err != nil {
			t.Fatal(err)
		}
		agent = child
	}
	if agent.Depth != depth {
		t.Fatalf("agent depth = %d, want %d", agent.Depth, depth)
	}
	return agent
}

// Each layer gets its own document AND is proved not to get the other two.
// Asserting only that the right document is present would pass a prompt that
// appended all three.
func TestSystemPromptDeliversOnlyThisAgentsLayer(t *testing.T) {
	r := runtimeWithInstructions(t, allThreeDocs())
	for _, tc := range []struct {
		depth   int
		want    string
		forbid  []string
		layerID string
	}{
		{0, seatDoc, []string{managerDoc, leafDoc}, "seat"},
		{1, managerDoc, []string{seatDoc, leafDoc}, "manager"},
		{2, leafDoc, []string{seatDoc, managerDoc}, "leaf"},
	} {
		prompt := systemPrompt(agentAtDepth(t, r, tc.depth))
		if !strings.Contains(prompt, tc.want) {
			t.Fatalf("depth %d prompt missing its own document %q", tc.depth, tc.want)
		}
		for _, forbidden := range tc.forbid {
			if strings.Contains(prompt, forbidden) {
				t.Fatalf("depth %d prompt carries another layer's document %q", tc.depth, forbidden)
			}
		}
		if !strings.Contains(prompt, "your layer ("+tc.layerID+")") {
			t.Fatalf("depth %d prompt does not name its layer as %q", tc.depth, tc.layerID)
		}
	}
}

// The managed document is appended to the mechanics, never substituted for
// them. A leaf that received only org policy would lose the async delegation
// contract and the tool prohibitions.
func TestSystemPromptKeepsBakedMechanicsAlongsideTheLayerDocument(t *testing.T) {
	r := runtimeWithInstructions(t, allThreeDocs())
	for _, depth := range []int{0, 1, 2} {
		agent := agentAtDepth(t, r, depth)
		prompt := systemPrompt(agent)
		for _, mechanic := range []string{
			"launch_subagent returns immediately",
			"Do not use quick_bash, long_job, quick_py, long_py, sleep, polling, or shell wait loops",
			"mandatory mid-turn steer",
			"Model guidance: approved models are",
		} {
			if !strings.Contains(prompt, mechanic) {
				t.Fatalf("depth %d prompt lost baked mechanic %q", depth, mechanic)
			}
		}
		if !strings.HasPrefix(prompt, bakedSystemPrompt(agent)) {
			t.Fatalf("depth %d prompt does not begin with the baked mechanics", depth)
		}
	}
}

// Absence degrades, it does not brick. A host ansible has never touched runs
// on the baked prompt alone, with no dangling header announcing instructions
// that are not there.
func TestSystemPromptWithoutManagedInstructionsIsBakedPromptAlone(t *testing.T) {
	r := runtimeWithInstructions(t, nil)
	for _, depth := range []int{0, 1, 2} {
		agent := agentAtDepth(t, r, depth)
		prompt := systemPrompt(agent)
		if prompt != bakedSystemPrompt(agent) {
			t.Fatalf("depth %d prompt differs from the baked prompt with no documents deployed", depth)
		}
		if strings.Contains(prompt, "Org instructions for your layer") {
			t.Fatalf("depth %d prompt announces org instructions that were never deployed", depth)
		}
	}
}

// A partial deployment serves the layers that exist without inventing one for
// the layer that does not.
func TestSystemPromptWithOnlyOneLayerDeployed(t *testing.T) {
	r := runtimeWithInstructions(t, map[string]string{config.LayerLeaf: leafDoc})
	leaf := agentAtDepth(t, r, 2)
	if !strings.Contains(systemPrompt(leaf), leafDoc) {
		t.Fatal("leaf did not receive the one deployed document")
	}
	seat := r.seat()
	if systemPrompt(seat) != bakedSystemPrompt(seat) {
		t.Fatal("seat received a document when only the leaf layer was deployed")
	}
}

func TestSeatSystemPromptListsSkillsWithAbsolutePaths(t *testing.T) {
	r := runtimeWithInstructionsAndSkills(t, allThreeDocs(), map[string]map[string]string{
		config.LayerSeat: {
			"review": "---\nname: review-change\ndescription: Review a change before handoff.\n---\nSECRET BODY MUST NOT APPEAR",
		},
	})
	prompt := systemPrompt(r.seat())
	path := filepath.Join(r.Home(), config.SkillsDir, config.LayerSeat, "review", "SKILL.md")
	for _, want := range []string{
		"Skills for your layer (seat).",
		"When a task matches a skill below, read its SKILL.md at the listed path with your file tools before acting.",
		"- review-change: Review a change before handoff.",
		"SKILL.md: " + path,
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("seat prompt missing %q: %s", want, prompt)
		}
	}
	if !filepath.IsAbs(path) {
		t.Fatalf("test path = %q, want absolute", path)
	}
	if strings.Contains(prompt, "SECRET BODY MUST NOT APPEAR") {
		t.Fatal("seat prompt includes a SKILL.md body")
	}
}

// The Codex leaf is where per-layer instruction already existed as a hardcoded
// literal. It keeps its mechanics and now also receives the managed leaf
// document — and must never receive the manager one.
func TestCodexLeafReceivesLeafDocumentBesideItsMechanics(t *testing.T) {
	r := runtimeWithInstructions(t, allThreeDocs())
	leaf := agentAtDepth(t, r, 2)
	instructions := (&codexLeaf{agent: leaf}).developerInstructions()
	if !strings.Contains(instructions, codexLeafMechanics) {
		t.Fatal("Codex leaf lost its baked mechanics")
	}
	// The depth, the layer it resolves to and where the documents came from
	// are the three things that would explain a miss here, and none of them
	// is recoverable from the assembled string alone — so this assertion
	// reports them rather than failing bare.
	if !strings.Contains(instructions, leafDoc) {
		t.Fatalf("Codex leaf did not receive the managed leaf document: depth=%d layer=%q source=%+v instructions=%q",
			leaf.Depth, config.LayerForDepth(leaf.Depth), r.InstructionSource(), instructions)
	}
	if strings.Contains(instructions, managerDoc) || strings.Contains(instructions, seatDoc) {
		t.Fatalf("Codex leaf received another layer's document: %q", instructions)
	}
	if !strings.HasPrefix(instructions, codexLeafMechanics) {
		t.Fatal("Codex leaf instructions do not lead with the mechanics")
	}
}

func TestCodexLeafWithoutManagedInstructionsKeepsMechanicsOnly(t *testing.T) {
	r := runtimeWithInstructions(t, nil)
	leaf := agentAtDepth(t, r, 2)
	instructions := (&codexLeaf{agent: leaf}).developerInstructions()
	if instructions != codexLeafMechanics {
		t.Fatalf("Codex leaf instructions = %q, want the mechanics alone", instructions)
	}
}

func TestCodexLeafDeveloperInstructionsCarryNoSLBHSkillList(t *testing.T) {
	r := runtimeWithInstructionsAndSkills(t, allThreeDocs(), map[string]map[string]string{
		config.LayerLeaf: {
			"implement": "---\nname: codex-forbidden-skill\ndescription: This metadata must stay out of Codex developer instructions.\n---\n",
		},
	})
	leaf := agentAtDepth(t, r, 2)
	instructions := (&codexLeaf{agent: leaf}).developerInstructions()
	for _, forbidden := range []string{"Skills for your layer", "codex-forbidden-skill", config.SkillsDir + string(filepath.Separator) + config.LayerLeaf} {
		if strings.Contains(instructions, forbidden) {
			t.Fatalf("Codex leaf developer instructions contain slbh skill data %q: %q", forbidden, instructions)
		}
	}
}

func TestClaudeLeafDeveloperInstructionsCarryNoSLBHSkillList(t *testing.T) {
	r := runtimeWithInstructionsAndSkills(t, allThreeDocs(), map[string]map[string]string{
		config.LayerLeaf: {
			"implement": "---\nname: claude-forbidden-skill\ndescription: This metadata must stay out of Claude developer instructions.\n---\n",
		},
	})
	leaf := agentAtDepth(t, r, 2)
	instructions := claudeDeveloperInstructions(leaf)
	for _, forbidden := range []string{"Skills for your layer", "claude-forbidden-skill", config.SkillsDir + string(filepath.Separator) + config.LayerLeaf} {
		if strings.Contains(instructions, forbidden) {
			t.Fatalf("Claude leaf developer instructions contain slbh skill data %q: %q", forbidden, instructions)
		}
	}
}

// The runtime reports where the documents came from, so a converge that did
// not land is visible rather than silent.
func TestRuntimeReportsInstructionSource(t *testing.T) {
	r := runtimeWithInstructions(t, allThreeDocs())
	if got := r.InstructionSource().Kind; got != config.InstructionsManaged {
		t.Fatalf("source kind = %q, want managed", got)
	}
	bare := runtimeWithInstructions(t, nil)
	if got := bare.InstructionSource().Kind; got != config.InstructionsNone {
		t.Fatalf("source kind = %q, want none", got)
	}
}
