package job

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func runScriptTest(t *testing.T, interpreter, script string, env ...string) (Snapshot, string, string) {
	t.Helper()
	root := t.TempDir()
	m := NewManager(nil)
	j, err := m.Start(context.Background(), Spec{Author: "test", Script: script, Interpreter: interpreter, ScriptRoot: root, Dir: t.TempDir(), Environment: env, Inline: true})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, stdout, stderr, background := m.Wait(j, 10*time.Second)
	if background {
		t.Fatal("test job unexpectedly backgrounded")
	}
	return snapshot, stdout, stderr
}

func TestBashMultilineSpecialCharactersUnicodeAndLargeBody(t *testing.T) {
	body := strings.Repeat(": # \" ' $ \\ café ✓\n", 3000)
	snapshot, stdout, stderr := runScriptTest(t, "bash", body+"printf '%s\\n' 'bash café ✓'")
	if snapshot.Status != Complete || stdout != "bash café ✓\n" || stderr != "" {
		t.Fatalf("status=%s stdout=%q stderr=%q", snapshot.Status, stdout, stderr)
	}
	if len(body) <= 40000 {
		t.Fatal("test body is not larger than 40k")
	}
}

func TestPythonMultilineUnicodeAndErrorPath(t *testing.T) {
	root := t.TempDir()
	m := NewManager(nil)
	script := "print('one')\nprint(\"café ✓\")\nraise RuntimeError('boom')\n"
	j, err := m.Start(context.Background(), Spec{Author: "test", Script: script, Interpreter: "python", ScriptRoot: root, Dir: t.TempDir(), Inline: true})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, stdout, stderr, background := m.Wait(j, 10*time.Second)
	stdout = strings.ReplaceAll(stdout, "\r\n", "\n")
	stderr = strings.ReplaceAll(stderr, "\r\n", "\n")
	if background || snapshot.Status != Failed || !strings.Contains(stdout, "one\ncafé ✓\n") {
		t.Fatalf("snapshot=%#v stdout=%q", snapshot, stdout)
	}
	if !strings.Contains(stderr, snapshot.ScriptPath+`", line 3`) {
		t.Fatalf("traceback does not name script line: path=%q stderr=%q", snapshot.ScriptPath, stderr)
	}
}

func TestJobCwdAndTemporaryEnvironment(t *testing.T) {
	cwd := t.TempDir()
	script := `printf '%s\n' "$PWD"; printf '%s\n' "$TMPDIR"; printf '%s\n' "$TMP"; printf '%s\n' "$TEMP"`
	if runtime.GOOS == "windows" {
		script = `pwd -W; cygpath -w "$TMPDIR"; cygpath -w "$TMP"; cygpath -w "$TEMP"`
	}
	m := NewManager(nil)
	j, err := m.Start(context.Background(), Spec{Author: "test", Script: script, Interpreter: "bash", ScriptRoot: t.TempDir(), Dir: cwd, Environment: []string{"TMPDIR=" + cwd, "TMP=" + cwd, "TEMP=" + cwd}, Inline: true})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, stdout, _, _ := m.Wait(j, 10*time.Second)
	if snapshot.Status != Complete {
		t.Fatalf("status=%s", snapshot.Status)
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if runtime.GOOS == "windows" {
		for i, line := range lines {
			lines[i] = filepath.Clean(strings.ReplaceAll(line, "\\", string(filepath.Separator)))
		}
	}
	want := []string{cwd, cwd, cwd, cwd}
	if len(lines) != 4 || strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("cwd/environment=%q", stdout)
	}
}

func TestBackgroundWaitAndExactlyOnceCompletion(t *testing.T) {
	m := NewManager(nil)
	completed := make(chan Snapshot, 2)
	m.SetCompletionHandler(func(snapshot Snapshot, _, _ string) { completed <- snapshot })
	j, err := m.Start(context.Background(), Spec{Author: "test", Script: "printf partial; sleep 5; printf done", Interpreter: "bash", ScriptRoot: t.TempDir(), Dir: t.TempDir(), Inline: true})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, stdout, _, background := m.Wait(j, 2*time.Second)
	if !background || snapshot.Status != Running || !strings.Contains(stdout, "partial") {
		t.Fatalf("snapshot=%#v stdout=%q background=%v", snapshot, stdout, background)
	}
	select {
	case got := <-completed:
		if got.ID != snapshot.ID {
			t.Fatalf("completion id=%q want %q", got.ID, snapshot.ID)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("completion not delivered")
	}
	select {
	case duplicate := <-completed:
		t.Fatalf("duplicate completion: %#v", duplicate)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestZeroWaitBackgroundsEvenForFastJob(t *testing.T) {
	m := NewManager(nil)
	completed := make(chan struct{}, 1)
	m.SetCompletionHandler(func(Snapshot, string, string) { completed <- struct{}{} })
	j, err := m.Start(context.Background(), Spec{Author: "test", Script: "printf fast", Interpreter: "bash", ScriptRoot: t.TempDir(), Dir: t.TempDir(), Inline: false})
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, background := m.Wait(j, 0)
	if !background {
		t.Fatal("zero wait did not background")
	}
	select {
	case <-completed:
	case <-time.After(3 * time.Second):
		t.Fatal("completion not delivered")
	}
}

func TestKillRemovesDescendant(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux process-group contract")
	}
	m := NewManager(nil)
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	j, err := m.Start(context.Background(), Spec{Author: "test", Script: "sleep 60 & child=$!; echo $child > '" + pidFile + "'; wait", Interpreter: "bash", ScriptRoot: t.TempDir(), Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(pidFile); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("child pid was not written")
		}
		time.Sleep(10 * time.Millisecond)
	}
	data, _ := os.ReadFile(pidFile)
	child := strings.TrimSpace(string(data))
	if err := j.Kill(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-j.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("job did not finish")
	}
	if err := exec.Command("kill", "-0", child).Run(); err == nil {
		t.Fatalf("child %s is still alive", child)
	}
}
