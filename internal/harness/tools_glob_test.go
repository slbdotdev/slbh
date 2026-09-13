package harness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestGlobRecursesAndSpeaksOnNoMatch pins the two things filepath.Glob does not do.
//
// It fails on the old implementation twice over: `**/*.py` returned nothing at all for a
// file two directories down, because filepath.Glob treats `**` as a single-segment `*`;
// and a pattern that matched nothing returned the empty string, which a model reads as a
// tool that did not work rather than as a directory that holds no such file.
func TestGlobRecursesAndSpeaksOnNoMatch(t *testing.T) {
	r := testRuntime(t)
	work := t.TempDir()
	r.workDir = work
	r.Seat().WorkDir = work
	if err := os.MkdirAll(filepath.Join(work, "src", "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(work, ".git", "objects"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		filepath.Join(work, "src", "pkg", "deep.py"),
		filepath.Join(work, "top.py"),
		filepath.Join(work, ".git", "objects", "buried.py"),
	} {
		if err := os.WriteFile(path, []byte("x = 1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	run := func(pattern string) string {
		args, err := json.Marshal(map[string]string{"pattern": pattern})
		if err != nil {
			t.Fatal(err)
		}
		out, err := r.ExecuteTool(r.Seat().ID, "glob", string(args))
		if err != nil {
			t.Fatalf("glob %q: %v", pattern, err)
		}
		return out
	}

	deep := run("**/*.py")
	if !strings.Contains(deep, filepath.Join("src", "pkg", "deep.py")) {
		t.Fatalf("** did not reach a file two directories down: %q", deep)
	}
	if !strings.Contains(deep, "top.py") {
		t.Fatalf("** did not match zero directories: %q", deep)
	}
	if strings.Contains(deep, "buried.py") {
		t.Fatalf("** descended into .git: %q", deep)
	}

	empty := run("**/*.rs")
	if strings.TrimSpace(empty) == "" {
		t.Fatal("a pattern matching nothing returned the empty string")
	}
	if !strings.Contains(empty, "no files match") {
		t.Fatalf("no-match result does not say so: %q", empty)
	}

	dirs := run("*")
	if !strings.Contains(dirs, "src"+string(filepath.Separator)) {
		t.Fatalf("directory carries no trailing separator: %q", dirs)
	}
}
