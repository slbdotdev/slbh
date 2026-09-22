package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeDoc(t *testing.T, dir, name, text string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A repository's documents load from its root down to the working directory,
// outer first, and a document above the root never does: it belongs to
// whatever holds the checkout, not to the project.
func TestProjectDocsWalkFromGitRootToWorkDir(t *testing.T) {
	outside := t.TempDir()
	root := filepath.Join(outside, "repo")
	sub := filepath.Join(root, "pkg")
	work := filepath.Join(sub, "inner")
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	writeDoc(t, outside, "AGENTS.md", "OUTSIDE-DOC")
	writeDoc(t, root, "AGENTS.md", "ROOT-DOC")
	writeDoc(t, sub, "CLAUDE.md", "SUB-CLAUDE-DOC")

	docs := projectDocs(work)
	if len(docs) != 2 {
		t.Fatalf("got %d documents, want 2: %+v", len(docs), docs)
	}
	if docs[0].path != filepath.Join(root, "AGENTS.md") || docs[0].text != "ROOT-DOC" {
		t.Fatalf("first document = %+v, want the root AGENTS.md", docs[0])
	}
	if docs[1].path != filepath.Join(sub, "CLAUDE.md") || docs[1].text != "SUB-CLAUDE-DOC" {
		t.Fatalf("second document = %+v, want the subdirectory CLAUDE.md", docs[1])
	}
	if strings.Contains(projectDocPrompt(work), "OUTSIDE-DOC") {
		t.Fatal("a document above the git root reached the prompt")
	}
}

// Where a directory has both, AGENTS.md is the one read. The fleet keeps the
// two as near copies, and loading both would send the text twice.
func TestProjectDocsPreferAgentsOverClaude(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeDoc(t, root, "AGENTS.md", "AGENTS-DOC")
	writeDoc(t, root, "CLAUDE.md", "CLAUDE-DOC")
	prompt := projectDocPrompt(root)
	if !strings.Contains(prompt, "AGENTS-DOC") || strings.Contains(prompt, "CLAUDE-DOC") {
		t.Fatalf("prompt = %q, want AGENTS.md alone", prompt)
	}
}

// Outside a repository only the working directory counts, so a document in
// a parent such as the home directory does not follow every agent around.
func TestProjectDocsOutsideARepositoryReadOnlyTheWorkDir(t *testing.T) {
	parent := t.TempDir()
	work := filepath.Join(parent, "scratch")
	writeDoc(t, parent, "AGENTS.md", "PARENT-DOC")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	if got := projectDocPrompt(work); got != "" {
		t.Fatalf("prompt = %q, want nothing without a repository or a local document", got)
	}
	writeDoc(t, work, "AGENTS.md", "WORK-DOC")
	if got := projectDocPrompt(work); !strings.Contains(got, "WORK-DOC") || strings.Contains(got, "PARENT-DOC") {
		t.Fatalf("prompt = %q, want the working directory's document alone", got)
	}
}

// The bound holds across documents: the one that crosses it is cut and says
// so, and nothing after it is read.
func TestProjectDocsAreBoundedInTotal(t *testing.T) {
	root := t.TempDir()
	work := filepath.Join(root, "deep")
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeDoc(t, root, "AGENTS.md", strings.Repeat("r", projectDocMaxBytes-10))
	writeDoc(t, work, "AGENTS.md", "DEEP-DOC "+strings.Repeat("d", 100))
	docs := projectDocs(work)
	if len(docs) != 2 {
		t.Fatalf("got %d documents, want the root whole and the deep one cut", len(docs))
	}
	if !strings.HasPrefix(docs[1].text, "DEEP-DOC") || !strings.Contains(docs[1].text, "[cut: project instructions are limited to") {
		t.Fatalf("deep document = %q, want its first bytes and the cut notice", docs[1].text)
	}
	body := len(docs[0].text) + strings.Index(docs[1].text, "\n\n[cut:")
	if body != projectDocMaxBytes {
		t.Fatalf("documents carry %d bytes of text, want exactly %d", body, projectDocMaxBytes)
	}

	writeDoc(t, root, "AGENTS.md", strings.Repeat("r", projectDocMaxBytes+10))
	if docs := projectDocs(work); len(docs) != 1 {
		t.Fatalf("got %d documents, want the documents after the bound dropped", len(docs))
	}
}

// The prompt states where the agent is and when, and carries the project's
// instructions after the org's layer document.
func TestSystemPromptStatesWorkDirDateAndProjectInstructions(t *testing.T) {
	clock := promptClock
	promptClock = func() time.Time { return time.Date(2026, 9, 22, 23, 59, 0, 0, time.UTC) }
	t.Cleanup(func() { promptClock = clock })

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeDoc(t, root, "AGENTS.md", "PROJECT-DOC: run the suite before committing.")
	r := runtimeWithInstructions(t, allThreeDocs())
	seat := r.seat()
	seat.WorkDir = root

	prompt := systemPrompt(seat)
	for _, want := range []string{
		"Your working directory is " + root + "; relative paths and commands resolve against it.",
		"The local date is Tuesday 2026-09-22 (UTC).",
		"Project instructions from " + filepath.Join(root, "AGENTS.md") + ":\n\nPROJECT-DOC: run the suite before committing.",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q: %s", want, prompt)
		}
	}
	if strings.Index(prompt, seatDoc) > strings.Index(prompt, "PROJECT-DOC") {
		t.Fatal("project instructions precede the org layer document")
	}
}
