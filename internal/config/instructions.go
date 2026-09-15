package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// The three instruction layers. These are the org's own role documents, not
// harness mechanics: what a seat, a manager and a leaf are each permitted to
// do. Everything that describes how *this build* behaves stays baked into the
// binary, because a managed file that disagreed with the binary would simply
// be wrong and only the binary knows.
const (
	LayerSeat    = "seat"
	LayerManager = "manager"
	LayerLeaf    = "leaf"
)

// InstructionsDir is the directory under $SLBH_HOME that ansible deploys the
// three documents into. It sits beside policy.json and is read the same way:
// wholly managed, never written by slbh.
const InstructionsDir = "instructions"

// Instruction source kinds, mirroring PolicySource so the TUI can report both
// the same way.
const (
	InstructionsManaged = "managed"
	InstructionsNone    = "none"
)

// LayerForDepth maps an agent's runtime depth to its instruction layer.
//
// The mapping is by depth rather than by a configured role name because depth
// is the one fact the runtime cannot be wrong about: it assigns it when the
// agent is created and nothing can edit it afterwards. A role name in a config
// file is a second source of truth for something already known.
//
// Depths beyond 2 are leaves rather than an error. The runtime caps delegation
// at depth 2 today (a parent at depth >= 2 may not launch), so depth 3 cannot
// currently occur — but if that cap is ever raised, a deeper agent is more
// leaf-like, not less, and the alternative is an agent that silently receives
// no instructions at all.
func LayerForDepth(depth int) string {
	switch {
	case depth <= 0:
		return LayerSeat
	case depth == 1:
		return LayerManager
	default:
		return LayerLeaf
	}
}

// InstructionSource records where the layer documents came from and why any of
// them are missing, so degradation is visible rather than silent — the same
// reason PolicySource carries a Note.
type InstructionSource struct {
	// Kind is InstructionsManaged when at least one layer document was read,
	// InstructionsNone when none was.
	Kind string
	// Dir is the directory the documents were looked for in.
	Dir string
	// Missing names the layers that had no readable document, in seat,
	// manager, leaf order.
	Missing []string
	// Note carries read errors other than a plain absence. A file that exists
	// but cannot be read is a different fact from a file that was never
	// deployed, and an operator needs to be able to tell them apart.
	Note string
}

// Describe renders the instruction source for the TUI.
func (s InstructionSource) Describe() string {
	var text string
	if s.Kind == InstructionsManaged {
		text = "managed " + s.Dir
		if len(s.Missing) > 0 {
			text += " (no " + strings.Join(s.Missing, ", ") + " document)"
		}
	} else {
		text = "none — agents run on the baked system prompt alone"
	}
	if s.Note != "" {
		text += " [" + s.Note + "]"
	}
	return text
}

// Instructions holds the per-layer managed documents in force.
type Instructions struct {
	// Layers maps a layer name to its document text. A layer with no deployed
	// document is absent from the map rather than present and empty, so
	// "nothing was deployed" and "an empty document was deployed" are the same
	// fact — which they are, because an empty instruction is no instruction.
	Layers map[string]string
	// Source describes where the documents came from.
	Source InstructionSource
}

// For returns the instruction document for an agent at the given depth, or the
// empty string when that layer has no document.
//
// Absence deliberately behaves differently here from a missing routing policy.
// A missing policy refuses the request, because routing is a security posture
// and guessing at one is worse than declining. A missing instruction document
// is not: the agent runs on the baked system prompt alone. Degrade, never
// brick — the same principle that lets /models author a local policy on a host
// ansible has never touched.
func (i Instructions) For(depth int) string {
	return i.Layers[LayerForDepth(depth)]
}

// LoadInstructions reads the three layer documents from $SLBH_HOME/instructions.
//
// It never returns an error. Every failure mode — no directory, no file, an
// unreadable file — degrades to a layer with no document and is recorded on
// the source, because an agent that cannot start because its role document is
// missing is a worse outcome than an agent running on baked mechanics alone.
func LoadInstructions(home string) Instructions {
	dir := filepath.Join(home, InstructionsDir)
	loaded := Instructions{
		Layers: map[string]string{},
		Source: InstructionSource{Kind: InstructionsNone, Dir: dir},
	}
	var notes []string
	for _, layer := range []string{LayerSeat, LayerManager, LayerLeaf} {
		path := filepath.Join(dir, layer+".md")
		data, err := os.ReadFile(path)
		if err != nil {
			if !os.IsNotExist(err) {
				notes = append(notes, fmt.Sprintf("%s is unreadable: %v", path, err))
			}
			loaded.Source.Missing = append(loaded.Source.Missing, layer)
			continue
		}
		text := strings.TrimSpace(string(data))
		if text == "" {
			loaded.Source.Missing = append(loaded.Source.Missing, layer)
			continue
		}
		loaded.Layers[layer] = text
		loaded.Source.Kind = InstructionsManaged
	}
	loaded.Source.Note = strings.Join(notes, "; ")
	return loaded
}
