package config

import (
	"os"
	"path/filepath"
)

type Config struct {
	Home           string
	RootModel      string
	RootEffort     string
	SubagentModel  string
	SubagentEffort string
	Provider       string
	Endpoint       string
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
	return Config{
		Home:           home,
		RootModel:      getenv("SLBH_MODEL", "deepseek-v4-flash"),
		RootEffort:     getenv("SLBH_EFFORT", "xhigh"),
		SubagentModel:  getenv("SLBH_SUBAGENT_MODEL", "zai/glm-5.3-flash"),
		SubagentEffort: getenv("SLBH_SUBAGENT_EFFORT", "high"),
		Provider:       getenv("SLBH_PROVIDER", "auto"),
		Endpoint:       getenv("SLBH_ENDPOINT", "https://openrouter.ai/api/v1/chat/completions"),
	}
}

func getenv(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
