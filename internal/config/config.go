package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type Config struct {
	Home           string
	RootModel      string
	RootEffort     string
	SubagentModel  string
	LeafModel      string
	SubagentEffort string
	Provider       string
	Endpoint       string
	// ApprovedModels is nil for programmatic legacy configs and non-nil for
	// persisted/user-facing configs. A non-nil empty slice deliberately means
	// no model is approved yet: this is the cost-control fail-safe.
	ApprovedModels []string
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
		Home:           home,
		RootModel:      getenv("SLBH_MODEL", "deepseek-v4-flash"),
		RootEffort:     getenv("SLBH_EFFORT", "xhigh"),
		SubagentModel:  getenv("SLBH_SUBAGENT_MODEL", "zai/glm-5.3-flash"),
		LeafModel:      getenv("SLBH_LEAF_MODEL", ""),
		SubagentEffort: getenv("SLBH_SUBAGENT_EFFORT", "high"),
		Provider:       getenv("SLBH_PROVIDER", "auto"),
		Endpoint:       getenv("SLBH_ENDPOINT", "https://openrouter.ai/api/v1/chat/completions"),
		ApprovedModels: []string{},
	}
	if persisted, ok := loadFile(home); ok {
		if os.Getenv("SLBH_MODEL") == "" && persisted.RootModel != "" {
			cfg.RootModel = persisted.RootModel
		}
		if os.Getenv("SLBH_EFFORT") == "" && persisted.RootEffort != "" {
			cfg.RootEffort = persisted.RootEffort
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
		}
	}
	if cfg.LeafModel == "" {
		cfg.LeafModel = cfg.SubagentModel
	}
	return cfg
}

type fileConfig struct {
	RootModel      string   `json:"root_model,omitempty"`
	RootEffort     string   `json:"root_effort,omitempty"`
	SubagentModel  string   `json:"subagent_model,omitempty"`
	LeafModel      string   `json:"leaf_model,omitempty"`
	SubagentEffort string   `json:"subagent_effort,omitempty"`
	ApprovedModels []string `json:"approved_models"`
}

func (c Config) Save() error {
	if c.Home == "" {
		return fmt.Errorf("config home is empty")
	}
	if err := os.MkdirAll(c.Home, 0o700); err != nil {
		return err
	}
	payload, err := json.MarshalIndent(fileConfig{
		RootModel: c.RootModel, RootEffort: c.RootEffort,
		SubagentModel: c.SubagentModel, LeafModel: c.LeafModel, SubagentEffort: c.SubagentEffort,
		ApprovedModels: unique(c.ApprovedModels),
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
