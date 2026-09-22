package harness

import (
	"encoding/json"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func executeCommandTest(t *testing.T, r *Runtime, name string, values map[string]any) (string, error) {
	t.Helper()
	raw, err := json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	return r.ExecuteTool(r.seat().ID, name, string(raw))
}

func TestCommandToolsRunMultilineAndSpecialScripts(t *testing.T) {
	r := testRuntime(t)
	body := strings.Repeat("# quote \" apostrophe ' dollar $ slash \\\n", 3000)
	out, err := executeCommandTest(t, r, "bash", map[string]any{"script": body + "printf 'bash café ✓\\n'"})
	if err != nil || !strings.Contains(out, "bash café ✓") {
		t.Fatalf("bash output=%q err=%v", out, err)
	}
	python := "print('one')\nprint(\"python café ✓\")\n"
	out, err = executeCommandTest(t, r, "python", map[string]any{"script": python})
	out = strings.ReplaceAll(out, "\r\n", "\n")
	if err != nil || out != "one\npython café ✓\n" {
		t.Fatalf("python output=%q err=%v", out, err)
	}
	if len(body) <= 40000 {
		t.Fatal("body did not exceed 40k")
	}
}

func TestCommandToolAcceptsCommandAlias(t *testing.T) {
	r := testRuntime(t)
	out, err := executeCommandTest(t, r, "bash", map[string]any{"command": "printf via-command"})
	if err != nil || out != "via-command" {
		t.Fatalf("output=%q err=%v", out, err)
	}
	out, err = executeCommandTest(t, r, "bash", map[string]any{"script": "printf via-script", "command": "printf ignored"})
	if err != nil || out != "via-script" {
		t.Fatalf("script should win: output=%q err=%v", out, err)
	}
	if _, err := executeCommandTest(t, r, "bash", map[string]any{}); err == nil || !strings.Contains(err.Error(), "script is required") {
		t.Fatalf("err=%v", err)
	}
}

func TestCommandToolFailureIncludesOutputAndExitCode(t *testing.T) {
	r := testRuntime(t)
	out, err := executeCommandTest(t, r, "bash", map[string]any{"script": "echo before-failure; echo diagnostic >&2; exit 7"})
	if err == nil || !strings.Contains(err.Error(), "exit status 7") || !strings.Contains(out, "before-failure") || !strings.Contains(out, "diagnostic") {
		t.Fatalf("output=%q err=%v", out, err)
	}
}

func TestCommandToolCwdAndScratchEnvironment(t *testing.T) {
	r := testRuntime(t)
	seat := r.seat()
	seat.WorkDir = t.TempDir()
	script := `printf '%s\n' "$PWD" "$TMPDIR" "$TMP" "$TEMP"`
	if runtime.GOOS == "windows" {
		script = `pwd -W; cygpath -w "$TMPDIR"; cygpath -w "$TMP"; cygpath -w "$TEMP"`
	}
	out, err := executeCommandTest(t, r, "bash", map[string]any{"script": script})
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	scratch := filepath.Join(r.runtimeDir, "agents", seat.ID, "scratch")
	if runtime.GOOS == "windows" {
		for i, line := range lines {
			lines[i] = filepath.Clean(strings.ReplaceAll(line, "\\", string(filepath.Separator)))
		}
		scratch = filepath.Clean(scratch)
	}
	want := []string{seat.WorkDir, scratch, scratch, scratch}
	if len(lines) != 4 || strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("output=%q scratch=%q", out, scratch)
	}
}

func TestCommandToolWaitZeroReturnsJobAndDeliversResult(t *testing.T) {
	r := testRuntime(t)
	out, err := executeCommandTest(t, r, "bash", map[string]any{"script": "printf partial; sleep 1; printf complete", "wait_seconds": 0, "warn_after_seconds": 0})
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(out)
	if len(fields) < 2 || fields[0] != "job" {
		t.Fatalf("background result=%q", out)
	}
	jobID := fields[1]
	job, ok := r.jobs.Get(jobID)
	if !ok {
		t.Fatalf("job %q not found", jobID)
	}
	select {
	case <-job.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("job did not finish")
	}
}
