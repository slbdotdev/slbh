package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// SkillsDir is the managed skill tree under $SLBH_HOME. The org sync owns
// everything below it; slbh only reads metadata from each SKILL.md.
const SkillsDir = "skills"

const (
	SkillsManaged = "managed"
	SkillsNone    = "none"
)

// Skill is the prompt-visible metadata for one managed skill. Path is always
// the absolute path to its SKILL.md; the body is deliberately never loaded
// into a prompt.
type Skill struct {
	Name        string
	Description string
	Path        string
}

// SkillSource records where skills were sought and why any part of the
// managed tree could not be loaded. Missing contains layer directories that
// were absent. Note contains per-skill read and parse failures.
type SkillSource struct {
	Kind    string
	Dir     string
	Missing []string
	Note    string
}

// Describe renders the skill source for operator-facing status.
func (s SkillSource) Describe() string {
	var text string
	if s.Kind == SkillsManaged {
		text = "managed " + s.Dir
		if len(s.Missing) > 0 {
			text += " (no " + strings.Join(s.Missing, ", ") + " directory)"
		}
	} else {
		text = "none — agents run without slbh skills"
	}
	if s.Note != "" {
		text += " [" + s.Note + "]"
	}
	return text
}

// Skills holds the managed skill metadata in force, keyed by layer name.
type Skills struct {
	Layers map[string][]Skill
	Source SkillSource
}

// For returns the skills for an agent at depth. The returned slice is a copy.
func (s Skills) For(depth int) []Skill {
	return s.ForName(LayerForDepth(depth))
}

// ForName returns the skills for a named layer, including the Intern.
func (s Skills) ForName(name string) []Skill {
	return append([]Skill(nil), s.Layers[name]...)
}

// PromptFor returns the prompt section for a tree agent at depth.
func (s Skills) PromptFor(depth int) string {
	return s.PromptForName(LayerForDepth(depth))
}

// PromptForName renders only skill metadata, never SKILL.md bodies.
func (s Skills) PromptForName(layer string) string {
	skills := s.Layers[layer]
	if len(skills) == 0 {
		return ""
	}
	var prompt strings.Builder
	fmt.Fprintf(&prompt, "Skills for your layer (%s). When a task matches a skill below, read its SKILL.md at the listed path with your file tools before acting. Skill bodies are not included here.", layer)
	for _, skill := range skills {
		fmt.Fprintf(&prompt, "\n\n- %s: %s\n  SKILL.md: %s", skill.Name, skill.Description, skill.Path)
	}
	return prompt.String()
}

// LoadSkills reads skill metadata from $SLBH_HOME/skills. It never returns an
// error: missing directories, unreadable files and invalid front matter omit
// only the affected skills and remain visible through Source.
func LoadSkills(home string) Skills {
	layers := []string{LayerSeat, LayerManager, LayerLeaf, InstructionIntern}
	requestedDir := filepath.Join(home, SkillsDir)
	dir, err := filepath.Abs(requestedDir)
	if err != nil {
		return Skills{
			Layers: map[string][]Skill{},
			Source: SkillSource{
				Kind:    SkillsNone,
				Dir:     requestedDir,
				Missing: append([]string(nil), layers...),
				Note:    fmt.Sprintf("skills directory %s has no absolute path: %v", requestedDir, err),
			},
		}
	}
	loaded := Skills{
		Layers: map[string][]Skill{},
		Source: SkillSource{Kind: SkillsNone, Dir: dir},
	}
	if _, err := os.ReadDir(dir); err != nil {
		loaded.Source.Missing = append(loaded.Source.Missing, layers...)
		if os.IsNotExist(err) {
			loaded.Source.Note = fmt.Sprintf("skills directory %s is missing", dir)
		} else {
			loaded.Source.Note = fmt.Sprintf("skills directory %s is unreadable: %v", dir, err)
		}
		return loaded
	}
	loaded.Source.Kind = SkillsManaged

	var notes []string
	for _, layer := range layers {
		layerDir := filepath.Join(dir, layer)
		entries, err := os.ReadDir(layerDir)
		if err != nil {
			loaded.Source.Missing = append(loaded.Source.Missing, layer)
			if os.IsNotExist(err) {
				notes = append(notes, fmt.Sprintf("skill layer directory %s is missing", layerDir))
			} else {
				notes = append(notes, fmt.Sprintf("skill layer directory %s is unreadable: %v", layerDir, err))
			}
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			path := filepath.Join(layerDir, entry.Name(), "SKILL.md")
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				if os.IsNotExist(readErr) {
					notes = append(notes, fmt.Sprintf("skill document %s is missing", path))
				} else {
					notes = append(notes, fmt.Sprintf("skill document %s is unreadable: %v", path, readErr))
				}
				continue
			}
			skill, parseErr := parseSkillFrontMatter(data)
			if parseErr != nil {
				notes = append(notes, fmt.Sprintf("skill document %s has unparseable front matter: %v", path, parseErr))
				continue
			}
			skill.Path = path
			loaded.Layers[layer] = append(loaded.Layers[layer], skill)
		}
	}
	loaded.Source.Note = strings.Join(notes, "; ")
	return loaded
}

// parseSkillFrontMatter implements the deliberately small accepted subset of
// YAML: a leading --- block with name and either a one-line description or a
// folded >- description. No other field is interpreted.
func parseSkillFrontMatter(data []byte) (Skill, error) {
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	if len(lines) == 0 || strings.TrimSuffix(lines[0], "\r") != "---" {
		return Skill{}, fmt.Errorf("missing opening --- block")
	}
	end := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSuffix(lines[i], "\r") == "---" {
			end = i
			break
		}
	}
	if end == -1 {
		return Skill{}, fmt.Errorf("missing closing --- block")
	}

	var skill Skill
	nameSeen, descriptionSeen := false, false
	for i := 1; i < end; i++ {
		line := strings.TrimSuffix(lines[i], "\r")
		if line == "" || line[0] == ' ' || line[0] == '\t' {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		switch key {
		case "name":
			if nameSeen {
				return Skill{}, fmt.Errorf("duplicate name")
			}
			nameSeen = true
			skill.Name = value
		case "description":
			if descriptionSeen {
				return Skill{}, fmt.Errorf("duplicate description")
			}
			descriptionSeen = true
			if value != ">-" {
				if strings.HasPrefix(value, ">") || strings.HasPrefix(value, "|") {
					return Skill{}, fmt.Errorf("unsupported description block style %q", value)
				}
				skill.Description = value
				continue
			}
			var folded []string
			for i+1 < end {
				next := strings.TrimSuffix(lines[i+1], "\r")
				if next == "" {
					i++
					continue
				}
				if next[0] != ' ' && next[0] != '\t' {
					break
				}
				i++
				if text := strings.TrimSpace(next); text != "" {
					folded = append(folded, text)
				}
			}
			skill.Description = strings.Join(folded, " ")
		}
	}
	if !nameSeen || strings.TrimSpace(skill.Name) == "" {
		return Skill{}, fmt.Errorf("missing name")
	}
	if !descriptionSeen || strings.TrimSpace(skill.Description) == "" {
		return Skill{}, fmt.Errorf("missing description")
	}
	return skill, nil
}
