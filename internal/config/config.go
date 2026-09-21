package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/slbdotdev/slbh/internal/provider"
)

// Policy source kinds, reported to the user so a local edit that changes
// nothing is never a mystery. Without this a user on a fleet host edits a
// local policy, sees no change, and has no way to tell why.
const (
	PolicyManaged = "managed"
	PolicyLocal   = "local"
	PolicyNone    = "none"
)

// PolicySource records which of the two files the routing policy in force came
// from, and why a higher-precedence one was passed over if it was.
type PolicySource struct {
	// Kind is PolicyManaged, PolicyLocal or PolicyNone.
	Kind string
	// Path is the file the policy was read from, empty when Kind is
	// PolicyNone.
	Path string
	// Note carries the reason a managed file present on disk was not used, so
	// a corrupt managed policy degrades visibly rather than silently handing
	// control to a weaker local one.
	Note string
}

// Describe renders the policy source for the TUI footer and for /models.
func (s PolicySource) Describe() string {
	var text string
	switch s.Kind {
	case PolicyManaged:
		text = "managed " + s.Path
	case PolicyLocal:
		text = "local (" + s.Path + ")"
	default:
		text = "none — routing refuses; author one from /models"
	}
	if s.Note != "" {
		text += " [" + s.Note + "]"
	}
	return text
}

type Config struct {
	Home           string
	SeatModel      string
	SeatEffort     string
	InternModel    string
	InternEffort   string
	SubagentModel  string
	LeafModel      string
	SubagentEffort string
	// ToolShape selects the native agents' primary tool set, one of
	// ToolShapes. Empty means lean. SLBH_TOOL_SHAPE
	// overrides the file; ValidateToolShape refuses anything else.
	ToolShape string
	// toolShapeFile is the persisted value, kept apart so that Save never
	// writes an environment or flag override back into config.json.
	toolShapeFile string
	Endpoint      string
	// EndpointExplicit records that Endpoint came from SLBH_ENDPOINT rather
	// than from the default. Routing needs the provenance, not just the value:
	// an endpoint the operator set deliberately overrides a native route,
	// where the identical value arrived at by default must not. Endpoint keeps
	// its default so catalog discovery still has a URL to read.
	EndpointExplicit bool
	// ApprovedModels is nil for programmatic legacy configs and non-nil for
	// persisted/user-facing configs. A non-nil empty slice deliberately means
	// no model is approved yet: this is the cost-control fail-safe.
	ApprovedModels []string
	// LocalPolicy is the app-owned policy block persisted in config.json,
	// authored by /models. It is carried on Config so that Save() writes it
	// back: without that, any /effort or /model save would silently delete the
	// policy the user authored to make an unmanaged host work at all.
	LocalPolicy *provider.Policy
	// Policy is the routing policy actually in force, resolved by precedence:
	// the managed file, else the local block, else nothing. It is derived, not
	// persisted.
	Policy provider.Policy
	// PolicySource says which file Policy came from, for the TUI.
	PolicySource PolicySource
	// Instructions are the per-layer role documents in force, read from
	// $SLBH_HOME/instructions. Derived like Policy, never persisted: slbh
	// reads these files and never writes them.
	Instructions Instructions
	// Skills are the per-layer skill metadata read from $SLBH_HOME/skills.
	// Only name, description and absolute SKILL.md path are retained; skill
	// bodies remain on disk until an agent chooses to read one.
	Skills Skills
	// Models is the optional managed short-name map read from
	// $SLBH_HOME/models.toml. It contains model identities only; launch
	// relationships belong to the runtime's depth mechanics, not this file.
	Models Models
}

func Load() Config {
	home := os.Getenv("SLBH_HOME")
	if home == "" {
		if h, err := os.UserHomeDir(); err == nil {
			home = filepath.Join(h, ".slbh")
		} else {
			home = ".slbh"
		}
	}
	cfg := Config{
		Home: home,
		// The seat default is `medium`, and deliberately not `high` or
		// `xhigh`. Two separate reasons, either of which is enough.
		//
		// It is a level the local Ollama route can actually serve.
		// Ollama's OpenAI-compatible layer rewrites the level before the
		// model's chat template runs: `high` becomes the template's top rung,
		// `xhigh` becomes `max`, and the template raises on `max` because it
		// has no such rung. A seat on a `local/` model at `xhigh` therefore
		// fails its first request with a 500 out of the template, which is
		// what a default must never do. Measured 2026-09-12 and again
		// 2026-09-17 against the deployed tags: low, medium and high return
		// 200; xhigh and max return 500.
		//
		// And on the plan routes, where both levels work, `xhigh` was not
		// worth its cost: pooled across the two live output_config.effort
		// ladders, its median is +16.1% over `high` but it wins only 67% of
		// turn-pairs and costs about 48% more wall clock.
		SeatModel:      getenv("SLBH_MODEL", "zai/glm-5.3-flash"),
		SeatEffort:     getenv("SLBH_EFFORT", "medium"),
		InternModel:    getenv("SLBH_INTERN_MODEL", provider.LocalModelID),
		InternEffort:   getenv("SLBH_INTERN_EFFORT", "low"),
		SubagentModel:  getenv("SLBH_SUBAGENT_MODEL", "zai/glm-5.3-flash"),
		LeafModel:      getenv("SLBH_LEAF_MODEL", provider.LocalModelID),
		SubagentEffort: getenv("SLBH_SUBAGENT_EFFORT", "medium"),
		Endpoint:       getenv("SLBH_ENDPOINT", provider.OpenRouterEndpoint),
		ApprovedModels: []string{},
	}
	cfg.EndpointExplicit = strings.TrimSpace(os.Getenv("SLBH_ENDPOINT")) != ""
	if persisted, ok := loadFile(home); ok {
		cfg.toolShapeFile = persisted.ToolShape
		cfg.ToolShape = persisted.ToolShape
		if os.Getenv("SLBH_MODEL") == "" {
			seatModel := persisted.SeatModel
			if seatModel == "" {
				seatModel = persisted.RootModel
			}
			if seatModel != "" {
				cfg.SeatModel = seatModel
			}
		}
		if os.Getenv("SLBH_EFFORT") == "" {
			seatEffort := persisted.SeatEffort
			if seatEffort == "" {
				seatEffort = persisted.RootEffort
			}
			if seatEffort != "" {
				cfg.SeatEffort = seatEffort
			}
		}
		if os.Getenv("SLBH_INTERN_MODEL") == "" && persisted.InternModel != "" {
			cfg.InternModel = persisted.InternModel
		}
		if os.Getenv("SLBH_INTERN_EFFORT") == "" && persisted.InternEffort != "" {
			cfg.InternEffort = persisted.InternEffort
		}
		if os.Getenv("SLBH_SUBAGENT_MODEL") == "" && persisted.SubagentModel != "" {
			cfg.SubagentModel = persisted.SubagentModel
		}
		if os.Getenv("SLBH_LEAF_MODEL") == "" && persisted.LeafModel != "" {
			cfg.LeafModel = persisted.LeafModel
		}
		if os.Getenv("SLBH_SUBAGENT_EFFORT") == "" && persisted.SubagentEffort != "" {
			cfg.SubagentEffort = persisted.SubagentEffort
		}
		if persisted.ApprovedModels != nil {
			cfg.ApprovedModels = unique(persisted.ApprovedModels)
		} else {
			// Config files written before model approval was introduced did not
			// have an approved_models field. Preserve their permissive behavior.
			cfg.ApprovedModels = nil
		}
		cfg.LocalPolicy = persisted.LocalPolicy
	}
	if shape := strings.TrimSpace(os.Getenv("SLBH_TOOL_SHAPE")); shape != "" {
		cfg.ToolShape = shape
	}
	if cfg.LeafModel == "" {
		cfg.LeafModel = cfg.SubagentModel
	}
	cfg.Policy, cfg.PolicySource = ResolvePolicy(home, cfg.LocalPolicy)
	cfg.Instructions = LoadInstructions(home)
	cfg.Skills = LoadSkills(home)
	cfg.Models = LoadModels(home)
	return cfg
}

// ResolvePolicy applies the three-step precedence: the managed policy.json if
// it is present and valid, otherwise a local policy from the app-owned
// config.json, otherwise none — and none refuses every route.
//
// The split is by writer, and provenance is the filename. On a fleet host the
// managed file wins and the application cannot clobber it, because the
// application writes a different file that is ignored while the managed one is
// present. On an unmanaged host the local block serves, so the fail-closed
// refusal is escapable from the TUI rather than a brick.
//
// A managed file that is present but unreadable or invalid falls through to
// the local block, which is what the precedence rule says — but the reason is
// carried in the source's Note and shown, so that degradation is visible
// instead of silently substituting a weaker policy for the org's own.
func ResolvePolicy(home string, local *provider.Policy) (provider.Policy, PolicySource) {
	managedPath := filepath.Join(home, "policy.json")
	localPath := filepath.Join(home, "config.json")
	note := ""

	data, err := os.ReadFile(managedPath)
	switch {
	case err == nil:
		var managed provider.Policy
		if decodeErr := json.Unmarshal(data, &managed); decodeErr != nil {
			note = fmt.Sprintf("managed policy at %s is unreadable: %v", managedPath, decodeErr)
		} else if validateErr := managed.Validate(); validateErr != nil {
			note = fmt.Sprintf("managed policy at %s is invalid: %v", managedPath, validateErr)
		} else {
			return managed, PolicySource{Kind: PolicyManaged, Path: managedPath}
		}
	case !os.IsNotExist(err):
		note = fmt.Sprintf("managed policy at %s could not be read: %v", managedPath, err)
	}

	if local != nil {
		if validateErr := local.Validate(); validateErr == nil {
			return *local, PolicySource{Kind: PolicyLocal, Path: localPath, Note: note}
		} else if note == "" {
			note = fmt.Sprintf("local policy in %s is invalid: %v", localPath, validateErr)
		}
	}
	return provider.Policy{}, PolicySource{Kind: PolicyNone, Note: note}
}

// ApplyLocalPolicy validates a policy, records it as the app-owned local
// block, and re-resolves which policy is in force. It deliberately does not
// save: the caller decides when to persist.
//
// Note what it does not do on a managed host: the local block is stored and
// persisted, but ResolvePolicy still prefers the managed file, so the returned
// source keeps saying "managed". That is the behaviour the TUI has to surface
// — a user who edits a local policy on a fleet host and sees nothing change
// must be told why.
func (c *Config) ApplyLocalPolicy(policy provider.Policy) error {
	if err := policy.Validate(); err != nil {
		return err
	}
	stored := policy
	c.LocalPolicy = &stored
	c.Policy, c.PolicySource = ResolvePolicy(c.Home, c.LocalPolicy)
	return nil
}

type fileConfig struct {
	SeatModel      string   `json:"seat_model,omitempty"`
	SeatEffort     string   `json:"seat_effort,omitempty"`
	InternModel    string   `json:"intern_model,omitempty"`
	InternEffort   string   `json:"intern_effort,omitempty"`
	RootModel      string   `json:"root_model,omitempty"`
	RootEffort     string   `json:"root_effort,omitempty"`
	SubagentModel  string   `json:"subagent_model,omitempty"`
	LeafModel      string   `json:"leaf_model,omitempty"`
	SubagentEffort string   `json:"subagent_effort,omitempty"`
	ApprovedModels []string `json:"approved_models"`
	ToolShape      string   `json:"tool_shape,omitempty"`
	// LocalPolicy is the app-owned half of the split. slbh writes it here and
	// never into the managed policy.json, which it only ever reads.
	LocalPolicy *provider.Policy `json:"local_policy,omitempty"`
}

func (c Config) Save() error {
	if c.Home == "" {
		return fmt.Errorf("config home is empty")
	}
	if err := os.MkdirAll(c.Home, 0o700); err != nil {
		return err
	}
	// LocalPolicy is written back on every save. Without it a /effort or
	// /model save would drop the policy the user authored to make an unmanaged
	// host work, and the next launch would refuse every route.
	payload, err := json.MarshalIndent(fileConfig{
		SeatModel: c.SeatModel, SeatEffort: c.SeatEffort,
		InternModel:   c.InternModel,
		InternEffort:  c.InternEffort,
		SubagentModel: c.SubagentModel, LeafModel: c.LeafModel, SubagentEffort: c.SubagentEffort,
		ApprovedModels: unique(c.ApprovedModels),
		ToolShape:      c.toolShapeFile,
		LocalPolicy:    c.LocalPolicy,
	}, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(c.Home, ".config.json.tmp")
	path := filepath.Join(c.Home, "config.json")
	if err := os.WriteFile(tmp, append(payload, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func loadFile(home string) (fileConfig, bool) {
	data, err := os.ReadFile(filepath.Join(home, "config.json"))
	if err != nil {
		return fileConfig{}, false
	}
	var persisted fileConfig
	if json.Unmarshal(data, &persisted) != nil {
		return fileConfig{}, false
	}
	return persisted, true
}

func (c Config) ModelApproved(model string) bool {
	model = strings.TrimSpace(model)
	if model == "" {
		return false
	}
	if c.ApprovedModels == nil {
		return true
	}
	for _, approved := range c.ApprovedModels {
		if strings.EqualFold(strings.TrimSpace(approved), model) {
			return true
		}
	}
	return false
}

func unique(models []string) []string {
	seen := make(map[string]struct{}, len(models))
	result := make([]string, 0, len(models))
	for _, model := range models {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		key := strings.ToLower(model)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, model)
	}
	return result
}

func getenv(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

// Tool shapes. lean is slbh's default: bash and apply_patch as the primary
// tools. mid adds read_file and full is slbh's original set; both keep slbh's
// own tools and texts. anthropic and codex reproduce the primary tools of
// Claude Code and the Codex CLI. On GLM, lean and mid cost ~0.72x full's
// tokens at equal correctness (slb-org org/tool-lean-2026-09-21.md).
const (
	ToolShapeLean      = "lean"
	ToolShapeMid       = "mid"
	ToolShapeFull      = "full"
	ToolShapeAnthropic = "anthropic"
	ToolShapeCodex     = "codex"
)

// ToolShapes lists the accepted tool_shape values in documentation order.
var ToolShapes = []string{ToolShapeLean, ToolShapeMid, ToolShapeFull, ToolShapeAnthropic, ToolShapeCodex}

// NativeToolShape reports whether shape is one of slbh's own tool sets,
// executed by slbh's own handlers.
func NativeToolShape(shape string) bool {
	return shape == ToolShapeLean || shape == ToolShapeMid || shape == ToolShapeFull
}

// NormalizeToolShape maps empty to lean and refuses an unknown value. There is
// no fallback: a run on the wrong tool set looks exactly like a run on the
// right one, so a typo has to stop the program rather than quietly select the
// default.
func NormalizeToolShape(shape string) (string, error) {
	shape = strings.TrimSpace(shape)
	if shape == "" {
		return ToolShapeLean, nil
	}
	for _, known := range ToolShapes {
		if shape == known {
			return shape, nil
		}
	}
	return "", fmt.Errorf("unknown tool shape %q; expected one of %s", shape, strings.Join(ToolShapes, ", "))
}
