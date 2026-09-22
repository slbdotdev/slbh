package harness

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// projectDocMaxBytes bounds the project instructions carried into a prompt.
// It is Codex's default project_doc_max_bytes, so a repository written for
// Codex fits here the same way.
const projectDocMaxBytes = 32 * 1024

// projectDocNames are read in order and the first present one wins, per
// directory. Repositories in the fleet carry AGENTS.md and CLAUDE.md as near
// copies of each other (ansible-slb), so loading both would double the text
// for nothing; CLAUDE.md counts only where no AGENTS.md exists.
var projectDocNames = []string{"AGENTS.md", "CLAUDE.md"}

type projectDoc struct {
	path string
	text string
}

// projectDocs returns the project instructions that apply to workDir: one
// document per directory from the enclosing git root down to workDir, outer
// first, so a nested document reads after the one it refines. Outside a git
// repository only workDir itself is consulted; walking to the filesystem root
// would pick up a stray AGENTS.md in a home directory for every project.
//
// The total is bounded at projectDocMaxBytes. A document that crosses the
// bound is cut and says so, and later documents are dropped, the way Codex
// treats its own limit.
func projectDocs(workDir string) []projectDoc {
	if workDir == "" {
		return nil
	}
	dir, err := filepath.Abs(workDir)
	if err != nil {
		return nil
	}
	dirs := []string{dir}
	for candidate := dir; ; {
		if _, err := os.Stat(filepath.Join(candidate, ".git")); err == nil {
			dirs = dirs[:0]
			for d := dir; ; d = filepath.Dir(d) {
				dirs = append(dirs, d)
				if d == candidate {
					break
				}
			}
			break
		}
		parent := filepath.Dir(candidate)
		if parent == candidate {
			break
		}
		candidate = parent
	}

	var docs []projectDoc
	remaining := projectDocMaxBytes
	for i := len(dirs) - 1; i >= 0 && remaining > 0; i-- {
		for _, name := range projectDocNames {
			path := filepath.Join(dirs[i], name)
			data, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			text := strings.TrimSpace(string(data))
			if text == "" {
				break
			}
			if len(text) > remaining {
				text = strings.ToValidUTF8(text[:remaining], "") + fmt.Sprintf("\n\n[cut: project instructions are limited to %d bytes in total]", projectDocMaxBytes)
			}
			remaining -= len(text)
			docs = append(docs, projectDoc{path: path, text: text})
			break
		}
	}
	return docs
}

// projectDocPrompt renders the project instructions for workDir, or "" when
// none apply.
func projectDocPrompt(workDir string) string {
	var b strings.Builder
	for _, doc := range projectDocs(workDir) {
		fmt.Fprintf(&b, "\n\nProject instructions from %s:\n\n%s", doc.path, doc.text)
	}
	return b.String()
}
