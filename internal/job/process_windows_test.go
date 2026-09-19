//go:build windows

package job

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGitBashCandidateAndDetection(t *testing.T) {
	got := gitBashCandidate(`C:\Users\slb\scoop\apps\git\2.55.0.3\mingw64\libexec\git-core`)
	if got != filepath.Clean(`C:\Users\slb\scoop\apps\git\2.55.0.3\bin\bash.exe`) {
		t.Fatalf("scoop candidate=%q", got)
	}
	got = gitBashCandidate(`C:/Program Files/Git/mingw64/libexec/git-core`)
	if got != filepath.Clean(`C:\Program Files\Git\bin\bash.exe`) {
		t.Fatalf("standard candidate=%q", got)
	}
	if path := cachedBashExecutable(); strings.Contains(strings.ToLower(path), "windowsapps") {
		t.Fatalf("WSL launcher selected: %q", path)
	}
}

func TestPowerShellWrapper(t *testing.T) {
	want := "[Console]::OutputEncoding=[Text.UTF8Encoding]::new($false); $PSStyle.OutputRendering='PlainText'; $global:LASTEXITCODE=0; & 'C:/tmp/script.ps1'; if (-not $?) { if ($LASTEXITCODE) { exit $LASTEXITCODE } else { exit 1 } }; exit $LASTEXITCODE"
	if got := pwshWrapper("C:/tmp/script.ps1"); got != want {
		t.Fatalf("wrapper=%q", got)
	}
}

func TestPowerShellJobs(t *testing.T) {
	if cachedPwshExecutable() == "" {
		t.Skip("pwsh not installed")
	}
	for _, test := range []struct {
		name, script, want string
		code               int
	}{
		{"unicode", "Write-Output 'café ✓'", "café ✓", 0},
		{"exit", "exit 7", "", 7},
		{"throw", "throw 'boom'", "", 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			m := NewManager(nil)
			j, err := m.Start(context.Background(), Spec{Author: "test", Script: test.script, Interpreter: "pwsh", ScriptRoot: t.TempDir(), Dir: t.TempDir(), Inline: true})
			if err != nil {
				t.Fatal(err)
			}
			snapshot, stdout, stderr, background := m.Wait(j, 10*time.Second)
			if background || snapshot.ExitCode != test.code {
				t.Fatalf("snapshot=%#v stdout=%q stderr=%q", snapshot, stdout, stderr)
			}
			stdout = strings.ReplaceAll(stdout, "\r\n", "\n")
			stderr = strings.ReplaceAll(stderr, "\r\n", "\n")
			if test.want != "" && strings.TrimSpace(stdout) != test.want {
				t.Fatalf("stdout=%q", stdout)
			}
			if test.name == "unicode" && strings.Contains(stdout+stderr, "\x1b[") {
				t.Fatalf("ANSI escape in output: %q", stdout+stderr)
			}
		})
	}
}

func TestPythonUnicodeOnWindows(t *testing.T) {
	m := NewManager(nil)
	j, err := m.Start(context.Background(), Spec{Author: "test", Script: "print('✓')", Interpreter: "python", ScriptRoot: t.TempDir(), Dir: t.TempDir(), Inline: true})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, stdout, stderr, background := m.Wait(j, 10*time.Second)
	stdout = strings.ReplaceAll(stdout, "\r\n", "\n")
	stderr = strings.ReplaceAll(stderr, "\r\n", "\n")
	if background || snapshot.Status != Complete || strings.TrimSpace(stdout) != "✓" || stderr != "" {
		t.Fatalf("snapshot=%#v stdout=%q stderr=%q", snapshot, stdout, stderr)
	}
}

func TestWindowsBashGrandchildDies(t *testing.T) {
	if cachedBashExecutable() == "" {
		t.Skip("Git Bash not installed")
	}
	m := NewManager(nil)
	j, err := m.Start(context.Background(), Spec{Author: "test", Script: "sleep 60 & wait", Interpreter: "bash", ScriptRoot: t.TempDir(), Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Kill(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-j.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("job did not finish")
	}
	_ = exec.Command("tasklist").Run()
}
