package harness

import (
	"strings"
	"testing"
	"time"

	"github.com/slbdotdev/slbh/internal/config"
	"github.com/slbdotdev/slbh/internal/provider"
	"github.com/slbdotdev/slbh/internal/seam"
)

type unknownCommand struct{ name string }

func (command unknownCommand) CommandName() string { return command.name }

type disguisedCommand struct{}

func (disguisedCommand) CommandName() string { return seam.CommandSendPrompt }

func TestCommandNamesMatchDo(t *testing.T) {
	r := testRuntime(t)
	for _, name := range seam.CommandNames() {
		command, ok := seam.NewCommand(name)
		if !ok {
			t.Fatalf("enumerated command %q has no concrete value", name)
		}
		_, err := r.Do(command)
		if err != nil && (strings.Contains(err.Error(), "unknown command") || strings.Contains(err.Error(), "unsupported command")) {
			t.Fatalf("Do rejected enumerated %q as outside the command set: %v", name, err)
		}
	}
	if _, err := r.Do(disguisedCommand{}); err == nil || !strings.Contains(err.Error(), seam.CommandSendPrompt) {
		t.Fatalf("Do accepted an unregistered concrete command or failed to name it: %v", err)
	}
}

func TestDoRejectsUnknownCommandByName(t *testing.T) {
	r := testRuntime(t)
	const name = "invented_command"
	if _, err := r.Do(unknownCommand{name: name}); err == nil || !strings.Contains(err.Error(), name) {
		t.Fatalf("Do unknown command error = %v, want it to name %q", err, name)
	}
}

func TestDoEmitsCommandEvent(t *testing.T) {
	r := testRuntime(t)
	cursor := r.PollEvents(seam.EventQuery{}).Cursor
	if _, err := r.Do(seam.EmitStatusCommand{Kind: "status", Text: "ready"}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		batch := r.PollEvents(seam.EventQuery{After: cursor, WaitMilliseconds: 100})
		cursor = batch.Cursor
		for _, event := range batch.Events {
			if event.Kind == "command" {
				if event.Text != seam.CommandEmitStatus {
					t.Fatalf("command event text = %q, want %q", event.Text, seam.CommandEmitStatus)
				}
				if event.Metadata["name"] != seam.CommandEmitStatus {
					t.Fatalf("command event metadata = %#v", event.Metadata)
				}
				return
			}
		}
	}
	t.Fatal("command event was not emitted")
}

func TestSeamQueriesReturnDeepCopies(t *testing.T) {
	r := testRuntime(t)
	zdr := true
	policy := provider.Policy{Version: 1, Routes: map[string]provider.RoutePolicy{
		"test": {
			Effort: provider.EffortDescriptor{Levels: map[string]string{"high": "high"}},
			Provider: &provider.ProviderPosture{
				ZDR:      &zdr,
				Ignore:   []string{"one"},
				MaxPrice: &provider.MaxPrice{Prompt: 1},
			},
		},
	}}
	r.mu.Lock()
	r.config.ApprovedModels = []string{"test"}
	r.config.Policy = policy
	r.config.LocalPolicy = &policy
	r.config.Instructions = config.Instructions{
		Layers: map[string]string{config.LayerSeat: "seat instructions"},
		Source: config.InstructionSource{Missing: []string{config.LayerLeaf}},
	}
	r.config.Skills = config.Skills{
		Layers: map[string][]config.Skill{config.LayerSeat: {{Name: "seat-skill", Path: "/skills/seat/SKILL.md"}}},
		Source: config.SkillSource{Missing: []string{config.LayerLeaf}},
	}
	r.config.Roster = config.Roster{
		Version: 3,
		Source:  config.RosterSource{Kind: config.RosterManaged, Path: "/managed/roster.toml"},
		Names: map[string]config.Role{
			"flex": {Name: "flex", ModelsApproved: []string{"deepseek-v4-flash"}},
		},
	}
	r.mu.Unlock()
	r.setModelCatalog([]provider.Catalog{{Name: "test", Models: []provider.ModelInfo{{ID: "test/model"}}}})

	firstConfig := r.Config()
	firstConfig.ApprovedModels[0] = "mutated"
	firstConfig.Policy.Routes["test"].Effort.Levels["high"] = "mutated"
	firstConfig.LocalPolicy.Routes["test"].Provider.Ignore[0] = "mutated"
	firstConfig.Instructions.Layers[config.LayerSeat] = "mutated"
	firstConfig.Instructions.Source.Missing[0] = "mutated"
	firstConfig.Skills.Layers[config.LayerSeat][0].Name = "mutated"
	firstConfig.Skills.Source.Missing[0] = "mutated"
	firstConfig.Roster.Names["flex"] = config.Role{Name: "mutated"}
	secondConfig := r.Config()
	if secondConfig.ApprovedModels[0] != "test" ||
		secondConfig.Policy.Routes["test"].Effort.Levels["high"] != "high" ||
		secondConfig.LocalPolicy.Routes["test"].Provider.Ignore[0] != "one" ||
		secondConfig.Instructions.Layers[config.LayerSeat] != "seat instructions" ||
		secondConfig.Instructions.Source.Missing[0] != config.LayerLeaf ||
		secondConfig.Skills.Layers[config.LayerSeat][0].Name != "seat-skill" ||
		secondConfig.Skills.Source.Missing[0] != config.LayerLeaf ||
		secondConfig.Roster.Names["flex"].Name != "flex" {
		t.Fatalf("mutating Config query changed runtime state: %#v", secondConfig)
	}

	firstCatalog := r.ModelCatalog()
	firstCatalog[0].Models[0].ID = "mutated"
	if got := r.ModelCatalog()[0].Models[0].ID; got != "test/model" {
		t.Fatalf("mutating ModelCatalog query changed runtime state to %q", got)
	}
}
