package job

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestManagerRunsAndCapturesOutput(t *testing.T) {
	m := NewManager(nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	job, err := m.Start(ctx, Spec{Author: "agent-test", Script: "echo hello"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-job.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("job did not finish")
	}
	stdout, stderr := job.Output()
	if !strings.Contains(stdout, "hello") || stderr != "" {
		t.Fatalf("stdout=%q stderr=%q", stdout, stderr)
	}
	if job.Snapshot().Status != Complete {
		t.Fatalf("status=%s", job.Snapshot().Status)
	}
	_ = runtime.GOOS // document that the manager uses a platform shell.
}

func TestManagerKill(t *testing.T) {
	m := NewManager(nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	script := "sleep 30"
	if runtime.GOOS == "windows" {
		script = "ping 127.0.0.1 -n 30 > nul"
	}
	job, err := m.Start(ctx, Spec{Author: "agent-test", Script: script})
	if err != nil {
		t.Fatal(err)
	}
	if err := job.Kill(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-job.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("killed job did not finish")
	}
	if job.Snapshot().Status != Killed {
		t.Fatalf("status=%s", job.Snapshot().Status)
	}
}

func TestManagerWarning(t *testing.T) {
	m := NewManager(nil)
	warnings := make(chan Snapshot, 1)
	m.SetWarningHandler(func(snapshot Snapshot) { warnings <- snapshot })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	script := "sleep 2"
	if runtime.GOOS == "windows" {
		script = "ping 127.0.0.1 -n 3 > nul"
	}
	j, err := m.Start(ctx, Spec{Author: "agent-test", Script: script, WarnAfter: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case warning := <-warnings:
		if warning.ID != j.Snapshot().ID {
			t.Fatalf("warning for %q, want %q", warning.ID, j.Snapshot().ID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("warning was not emitted")
	}
	_ = j.Kill()
}
