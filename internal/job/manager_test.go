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

func TestManagerCompletionHandlerReceivesFinishedOutput(t *testing.T) {
	m := NewManager(nil)
	completed := make(chan struct{})
	var got Snapshot
	var stdout, stderr string
	m.SetCompletionHandler(func(snapshot Snapshot, out, errOut string) {
		got = snapshot
		stdout, stderr = out, errOut
		close(completed)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	job, err := m.Start(ctx, Spec{Author: "agent-test", Script: "echo completion-stdout & echo completion-stderr 1>&2", ToolName: "long_py"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-completed:
	case <-time.After(5 * time.Second):
		t.Fatal("completion handler was not called")
	}
	select {
	case <-job.Done():
	default:
		t.Fatal("completion handler ran before Done was closed")
	}
	if got.ID != job.Snapshot().ID || got.Status != Complete || got.ToolName != "long_py" {
		t.Fatalf("snapshot=%#v job=%#v", got, job.Snapshot())
	}
	if !strings.Contains(stdout, "completion-stdout") || !strings.Contains(stderr, "completion-stderr") {
		t.Fatalf("stdout=%q stderr=%q", stdout, stderr)
	}
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
