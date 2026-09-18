package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

const (
	// RosterFile is the verbatim roster deployed from slb-org. slbh reads it
	// but never writes it; agent_config/roster.toml remains the authority.
	RosterFile = "roster.toml"

	RosterManaged = "managed"
	RosterNone    = "none"
)

// RosterSource records whether the managed roster was loaded. Child launches
// fail closed when Kind is not RosterManaged, so a missing sync cannot turn
// role identity into a default hidden in the binary.
type RosterSource struct {
	Kind string `json:"kind"`
	Path string `json:"path"`
	Note string `json:"note,omitempty"`
}

func (s RosterSource) Describe() string {
	if s.Kind == RosterManaged {
		return "managed " + s.Path
	}
	text := "none — child launches refuse"
	if s.Note != "" {
		text += " [" + s.Note + "]"
	}
	return text
}

// Role is the launch-relevant portion of one frozen roster name. Depth -1
// represents a name outside the delegation tree.
type Role struct {
	Name           string   `json:"name"`
	Harness        string   `json:"harness"`
	Model          string   `json:"model"`
	ModelsApproved []string `json:"models_approved,omitempty"`
	Effort         string   `json:"effort"`
	Depth          int      `json:"depth"`
	LaunchedBy     string   `json:"launched_by"`
}

// Roster is the normalized, serializable launch policy loaded from the
// managed TOML file.
type Roster struct {
	Version int             `json:"version"`
	Names   map[string]Role `json:"names"`
	Source  RosterSource    `json:"source"`
}

// Role returns a copy of the canonical role named by name.
func (r Roster) Role(name string) (Role, bool) {
	if r.Source.Kind != RosterManaged {
		return Role{}, false
	}
	role, ok := r.Names[strings.ToLower(strings.TrimSpace(name))]
	if !ok {
		return Role{}, false
	}
	role.ModelsApproved = append([]string(nil), role.ModelsApproved...)
	return role, true
}

// ChildRoles returns the roster names launchable by parentRole at depth in a
// stable order. The result is derived from the roster rather than a compiled
// list of the current names.
func (r Roster) ChildRoles(parentRole string, depth int) []Role {
	if r.Source.Kind != RosterManaged {
		return nil
	}
	parentRole = strings.ToLower(strings.TrimSpace(parentRole))
	var roles []Role
	for _, role := range r.Names {
		if role.Depth != depth || role.LaunchedBy != parentRole {
			continue
		}
		role.ModelsApproved = append([]string(nil), role.ModelsApproved...)
		roles = append(roles, role)
	}
	sort.Slice(roles, func(i, j int) bool { return roles[i].Name < roles[j].Name })
	return roles
}

type rosterFile struct {
	Version int                       `toml:"version"`
	Names   map[string]rosterFileRole `toml:"names"`
}

type rosterFileRole struct {
	Harness        string   `toml:"harness"`
	Model          string   `toml:"model"`
	ModelsApproved []string `toml:"models_approved"`
	Effort         string   `toml:"effort"`
	Depth          any      `toml:"depth"`
	LaunchedBy     string   `toml:"launched_by"`
}

// LoadRoster reads the verbatim roster deployed into $SLBH_HOME. It records
// failures instead of returning an error so the Seat can still start and
// report the broken sync; launch validation consults Source and refuses.
func LoadRoster(home string) Roster {
	path := filepath.Join(home, RosterFile)
	source := RosterSource{Kind: RosterNone, Path: path}
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			source.Note = fmt.Sprintf("managed roster could not be read: %v", err)
		}
		return Roster{Source: source}
	}

	var raw rosterFile
	if err := toml.Unmarshal(data, &raw); err != nil {
		source.Note = fmt.Sprintf("managed roster is invalid TOML: %v", err)
		return Roster{Source: source}
	}
	if raw.Version != 3 {
		source.Note = fmt.Sprintf("unsupported managed roster version %d", raw.Version)
		return Roster{Version: raw.Version, Source: source}
	}
	if len(raw.Names) == 0 {
		source.Note = "managed roster has no names"
		return Roster{Version: raw.Version, Source: source}
	}

	names := make(map[string]Role, len(raw.Names))
	for name, declaration := range raw.Names {
		canonical := strings.ToLower(strings.TrimSpace(name))
		if canonical == "" || canonical != name {
			source.Note = fmt.Sprintf("managed roster name %q is not canonical lowercase", name)
			return Roster{Version: raw.Version, Source: source}
		}
		if _, duplicate := names[canonical]; duplicate {
			source.Note = fmt.Sprintf("managed roster repeats name %q", canonical)
			return Roster{Version: raw.Version, Source: source}
		}
		depth, depthErr := rosterDepth(declaration.Depth)
		if depthErr != nil {
			source.Note = fmt.Sprintf("managed roster name %s.depth: %v", canonical, depthErr)
			return Roster{Version: raw.Version, Source: source}
		}
		role := Role{
			Name:           canonical,
			Harness:        strings.TrimSpace(declaration.Harness),
			Model:          strings.TrimSpace(declaration.Model),
			ModelsApproved: trimmedStrings(declaration.ModelsApproved),
			Effort:         strings.TrimSpace(declaration.Effort),
			Depth:          depth,
			LaunchedBy:     strings.ToLower(strings.TrimSpace(declaration.LaunchedBy)),
		}
		if role.Harness == "" || role.Model == "" || role.Effort == "" || role.LaunchedBy == "" {
			source.Note = fmt.Sprintf("managed roster name %s lacks launch fields", canonical)
			return Roster{Version: raw.Version, Source: source}
		}
		if role.Model == "at_dispatch" && len(role.ModelsApproved) == 0 {
			source.Note = fmt.Sprintf("managed roster name %s has no approved dispatch models", canonical)
			return Roster{Version: raw.Version, Source: source}
		}
		names[canonical] = role
	}

	source.Kind = RosterManaged
	return Roster{Version: raw.Version, Names: names, Source: source}
}

func rosterDepth(value any) (int, error) {
	switch depth := value.(type) {
	case int64:
		if depth < 0 || depth > int64(^uint(0)>>1) {
			return 0, fmt.Errorf("integer %d is out of range", depth)
		}
		return int(depth), nil
	case int:
		if depth < 0 {
			return 0, fmt.Errorf("integer %d is negative", depth)
		}
		return depth, nil
	case string:
		if depth == "outside" {
			return -1, nil
		}
		return 0, fmt.Errorf("string %q is not 'outside'", depth)
	default:
		return 0, fmt.Errorf("must be an integer or 'outside', got %T", value)
	}
}

func trimmedStrings(values []string) []string {
	result := make([]string, 0, len(values))
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	return result
}
