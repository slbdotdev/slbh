package job

import (
	"context"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
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

// TestJobOutputIsReadableWhileRunning is the regression test for the defect
// read_job carried until 2026-09-15: Job.Output read buffers that the wait
// goroutine filled only after cmd.Wait returned, so an agent reading a live job
// got {"stdout":"","stderr":""} and decided on nothing. The test reads the job
// mid-run on purpose — any test that reads after Done() passes against the
// broken code — and the suite is run under -race because the capture buffers
// are being written by the exec copy goroutine at the same instant.
func TestJobOutputIsReadableWhileRunning(t *testing.T) {
	m := NewManager(nil)
	defer m.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	script := "echo live-stdout; echo live-stderr 1>&2; sleep 30"
	if runtime.GOOS == "windows" {
		script = "echo live-stdout & echo live-stderr 1>&2 & ping 127.0.0.1 -n 30 > nul"
	}
	job, err := m.Start(ctx, Spec{Author: "agent-test", Script: script})
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr string
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		stdout, stderr = job.Output()
		if strings.Contains(stdout, "live-stdout") && strings.Contains(stderr, "live-stderr") {
			break
		}
		select {
		case <-job.Done():
			t.Fatal("job finished before its output could be read mid-run")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if !strings.Contains(stdout, "live-stdout") || !strings.Contains(stderr, "live-stderr") {
		t.Fatalf("live job output not readable: stdout=%q stderr=%q", stdout, stderr)
	}
	snapshot := job.Snapshot()
	if snapshot.Status != Running {
		t.Fatalf("status=%s, want %s", snapshot.Status, Running)
	}
	if snapshot.StdoutBytes == 0 || snapshot.StderrBytes == 0 {
		t.Fatalf("snapshot counted stdout=%d stderr=%d bytes for a job with output", snapshot.StdoutBytes, snapshot.StderrBytes)
	}
	if err := job.Kill(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-job.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("killed job did not finish")
	}
}

// TestJobOutputRacesTheWriter points the race detector at the access the fix
// introduces: many readers calling Output and Snapshot while the exec copy
// goroutine is still writing both capture buffers. Without the buffers' own
// mutex this reports a data race; the assertions alone would not.
func TestJobOutputRacesTheWriter(t *testing.T) {
	m := NewManager(nil)
	defer m.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	script := "i=1; while [ $i -le 400 ]; do echo line-$i; echo err-$i 1>&2; i=$((i+1)); done"
	if runtime.GOOS == "windows" {
		script = "for /l %i in (1,1,400) do @(echo line-%i & echo err-%i 1>&2)"
	}
	job, err := m.Start(ctx, Spec{Author: "agent-test", Script: script})
	if err != nil {
		t.Fatal(err)
	}
	var readers sync.WaitGroup
	for range 4 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-job.Done():
					return
				default:
				}
				job.Output()
				job.Snapshot()
			}
		}()
	}
	select {
	case <-job.Done():
	case <-time.After(30 * time.Second):
		t.Fatal("job did not finish")
	}
	readers.Wait()
	stdout, stderr := job.Output()
	if !strings.Contains(stdout, "line-400") || !strings.Contains(stderr, "err-400") {
		t.Fatalf("final output truncated: stdout=%d bytes stderr=%d bytes", len(stdout), len(stderr))
	}
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
		// The delivered snapshot is captured in the same critical section that
		// decides the job is still running, so a warning can never describe a
		// job that had already finished when it was taken.
		if warning.Status != Running {
			t.Fatalf("warning snapshot status = %q, want %q", warning.Status, Running)
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

// TestManagerCloseJoinsAWarningAlreadyInFlight is the shutdown proof.
//
// Once the timer has been selected the goroutine is committed to calling the
// handler: no channel can call it back, so the job's context arm — cancelled
// by Runtime.Close and by the kill in Close below — is unreachable from there.
// Runtime.Close calls Manager.Close and only then closes the session logs the
// handler writes into, so if Close does not join the goroutine, the warning
// can resume after shutdown, append into a closed log, have the error
// discarded, and be lost.
//
// The handler blocks here to hold the goroutine in exactly that state while
// Close runs. Before the join, Close returned immediately and this failed. The
// hold is well inside Close's two-second cap, so what this exercises is the
// join and not the cap.
func TestManagerCloseJoinsAWarningAlreadyInFlight(t *testing.T) {
	m := NewManager(nil)
	entered := make(chan struct{})
	release := make(chan struct{})
	delivered := make(chan struct{})
	m.SetWarningHandler(func(Snapshot) {
		close(entered)
		<-release
		close(delivered)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	script := "sleep 5"
	if runtime.GOOS == "windows" {
		script = "ping 127.0.0.1 -n 6 > nul"
	}
	if _, err := m.Start(ctx, Spec{Author: "agent-test", Script: script, WarnAfter: 10 * time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the warning handler was never entered")
	}
	closed := make(chan struct{})
	go func() {
		m.Close()
		close(closed)
	}()
	select {
	case <-closed:
		t.Fatal("Close returned while a warning was still being delivered; the handler can still write into a session log the runtime has since closed")
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return once the warning had been delivered")
	}
	select {
	case <-delivered:
	default:
		t.Fatal("Close returned without the warning having been delivered")
	}
}

// The join must not turn shutdown into a wait for a timer that has not fired.
// Close kills every job first, which cancels each job's context and so releases
// the pre-fire arm of every warning select; an hour-long warning must therefore
// cost shutdown nothing.
func TestManagerCloseDoesNotWaitForAnUnfiredWarning(t *testing.T) {
	m := NewManager(nil)
	m.SetWarningHandler(func(Snapshot) { t.Error("an unfired warning was delivered at shutdown") })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	script := "sleep 30"
	if runtime.GOOS == "windows" {
		script = "ping 127.0.0.1 -n 30 > nul"
	}
	if _, err := m.Start(ctx, Spec{Author: "agent-test", Script: script, WarnAfter: time.Hour}); err != nil {
		t.Fatal(err)
	}
	closed := make(chan struct{})
	go func() {
		m.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close blocked on a warning timer that had not fired")
	}
}

// TestStartAfterCloseIsRefusedAndArmsNoWarning is item 1's proof.
//
// Nothing serialised Start against Close before this: a Start landing after
// Close had begun waiting could Add to a WaitGroup already at zero — misuse —
// and arm a timer that fires into session logs Runtime.Close has since closed.
// The closed flag is read in the same acquisition that registers the job and
// counts its timer, so there is no window between the check and the Add.
func TestStartAfterCloseIsRefusedAndArmsNoWarning(t *testing.T) {
	m := NewManager(nil)
	m.SetWarningHandler(func(Snapshot) { t.Error("a warning was armed after Close") })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Close()
	j, err := m.Start(ctx, Spec{Author: "agent-test", Script: "echo late", WarnAfter: 10 * time.Millisecond})
	if err == nil {
		_ = j.Kill()
		t.Fatal("Start was accepted after Close")
	}
	if j != nil {
		t.Fatalf("refused Start returned a job: %#v", j.Snapshot())
	}
	if !strings.Contains(err.Error(), "closed") {
		t.Fatalf("Start after Close failed with %q, which does not name the closed manager", err)
	}
	// Past the interval the refused job asked for: nothing may fire, and the
	// manager must still shut down.
	time.Sleep(50 * time.Millisecond)
	closed := make(chan struct{})
	go func() {
		m.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return after a refused Start")
	}
}

// TestLimitedBufferCapsAcrossWrites is the regression test for the cap that
// held only for the write that crossed it: once the buffer was full the guard
// was skipped and every later write was kept whole, so a 5,000,000-byte job
// captured all of it. It also pins the reported count, which must be the whole
// write or exec's copy goroutine stops with io.ErrShortWrite.
func TestLimitedBufferCapsAcrossWrites(t *testing.T) {
	var b limitedBuffer
	chunk := make([]byte, 1024*1024+7)
	for i := 0; i < 6; i++ {
		n, err := b.Write(chunk)
		if err != nil || n != len(chunk) {
			t.Fatalf("write %d: n=%d err=%v, want n=%d", i, n, err, len(chunk))
		}
	}
	if b.Len() != limitedBufferMax {
		t.Fatalf("len=%d, want %d", b.Len(), limitedBufferMax)
	}
}

func TestJobOutputIsCappedAndTheJobCompletes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a POSIX shell pipeline")
	}
	m := NewManager(nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	job, err := m.Start(ctx, Spec{Author: "agent-test", Script: "head -c 5000000 /dev/zero; echo done >&2"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-job.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("job did not finish")
	}
	stdout, stderr := job.Output()
	if len(stdout) != limitedBufferMax {
		t.Fatalf("stdout captured %d bytes, want %d", len(stdout), limitedBufferMax)
	}
	if !strings.Contains(stderr, "done") {
		t.Fatalf("stderr=%q", stderr)
	}
	if snap := job.Snapshot(); snap.Status != Complete || snap.StdoutBytes != limitedBufferMax {
		t.Fatalf("snapshot=%+v", snap)
	}
}
