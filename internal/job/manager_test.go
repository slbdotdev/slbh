package job

import (
	"context"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/slbdotdev/slbh/internal/logx"
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
	// done closes last, so it means finished *and* recorded *and* delivered.
	// It used to close first, which let Manager.Close return — and Runtime.Close
	// shut the session loggers — while this goroutine still had its job_end
	// Append and this handler to run.
	select {
	case <-job.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("Done did not close after the completion handler ran")
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

func TestJobDoneMeansTheEndRecordIsAlreadyWritten(t *testing.T) {
	// Manager.Close waits on done and nothing else, so if done can close before
	// the job_end Append, Runtime.Close is free to shut the logger underneath
	// it. The Append then fails with "log is closed", the job goroutine ignores
	// the error, and the transcript silently loses the job's terminal record.
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")
	log, err := logx.OpenSession(path, "run-test", "session-test")
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(log)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	job, err := m.Start(ctx, Spec{Author: "agent-test", Script: "echo done-ordering", ToolName: "long_py"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-job.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("job never finished")
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	entries, err := logx.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Kind == "job_end" {
			return
		}
	}
	t.Fatalf("no job_end record was written before Done closed: %#v", entries)
}
