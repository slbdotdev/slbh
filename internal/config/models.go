package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// ModelsFile is an optional managed map from a human short name to a real
// provider model string. It is deliberately separate from the retired roster:
// an alias identifies a model, not an agent or a launch relationship.
const ModelsFile = "models.toml"

const (
	ModelsManaged = "managed"
	ModelsNone    = "none"
)

type ModelAlias struct {
	Model  string `json:"model"`
	Effort string `json:"effort,omitempty"`
}

type ModelsSource struct {
	Kind string `json:"kind"`
	Path string `json:"path"`
	Note string `json:"note,omitempty"`
}

func (s ModelsSource) Describe() string {
	if s.Kind == ModelsManaged {
		return "managed " + s.Path
	}
	text := "none — full model strings remain valid"
	if s.Note != "" {
		text += " [" + s.Note + "]"
	}
	return text
}

// Models is the normalized short-name catalogue. Full provider model strings
// do not need an entry and are always passed through unchanged.
type Models struct {
	Aliases map[string]ModelAlias `json:"aliases"`
	Source  ModelsSource          `json:"source"`
}

// Resolve expands a short name. An unknown value is treated as an already
// real model string, which keeps slbh useful without managed org content.
func (m Models) Resolve(value string) (model, defaultEffort string) {
	value = strings.TrimSpace(value)
	if alias, ok := m.Aliases[strings.ToLower(value)]; ok {
		return alias.Model, alias.Effort
	}
	return value, ""
}

type modelsFile struct {
	Version int                        `toml:"version"`
	Models  map[string]modelsFileModel `toml:"models"`
}

type modelsFileModel struct {
	Model  string `toml:"model"`
	Effort string `toml:"effort"`
}

// LoadModels reads the optional managed model alias file. Invalid content is
// reported through Source and never prevents slbh from accepting full model
// strings.
func LoadModels(home string) Models {
	path := filepath.Join(home, ModelsFile)
	source := ModelsSource{Kind: ModelsNone, Path: path}
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			source.Note = fmt.Sprintf("managed models could not be read: %v", err)
		}
		return Models{Source: source}
	}
	var raw modelsFile
	if err := toml.Unmarshal(data, &raw); err != nil {
		source.Note = fmt.Sprintf("managed models are invalid TOML: %v", err)
		return Models{Source: source}
	}
	if raw.Version != 1 {
		source.Note = fmt.Sprintf("unsupported managed models version %d", raw.Version)
		return Models{Source: source}
	}
	aliases := make(map[string]ModelAlias, len(raw.Models))
	for name, declaration := range raw.Models {
		canonical := strings.ToLower(strings.TrimSpace(name))
		if canonical == "" || canonical != name {
			source.Note = fmt.Sprintf("managed model alias %q is not canonical lowercase", name)
			return Models{Source: source}
		}
		model := strings.TrimSpace(declaration.Model)
		if model == "" {
			source.Note = fmt.Sprintf("managed model alias %s has no model string", canonical)
			return Models{Source: source}
		}
		aliases[canonical] = ModelAlias{Model: model, Effort: strings.TrimSpace(declaration.Effort)}
	}
	source.Kind = ModelsManaged
	return Models{Aliases: aliases, Source: source}
}
