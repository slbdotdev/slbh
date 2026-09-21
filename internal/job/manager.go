package job

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/slbdotdev/slbh/internal/id"
	"github.com/slbdotdev/slbh/internal/logx"
)

type Status string

const (
	Running  Status = "running"
	Complete Status = "complete"
	Failed   Status = "failed"
	Killed   Status = "killed"
)

type Spec struct {
	Author      string
	Script      string
	Command     []string
	Interpreter string
	ScriptRoot  string
	Inline      bool
	ToolName    string
	Dir         string
	// WarnAfter is the one timer a job has, and it warns exactly one party:
	// the agent named in Author, the agent that started the job. When it
	// elapses with the job still running, the manager hands the warning
	// handler a snapshot taken while the job was running, and the runtime
	// delivers it into that agent's inbox, waking it if it has gone idle.
	// Delivery to an agent that has already been stopped fails, and is
	// recorded as a delivery_error rather than retried.
	//
	// What the agent does then is the agent's own decision: kill the job, keep
	// waiting for its result, or get on with other work. There is deliberately
	// no fallback behind that decision. The warning fires once and is never
	// repeated, nothing escalates it, no second timer follows it, and nothing
	// the job manager owns blocks on the outcome. Zero disables the timer.
	WarnAfter   time.Duration
	Environment []string
}

type Snapshot struct {
	ID          string
	Author      string
	Script      string
	ToolName    string
	Status      Status
	Started     time.Time
	Finished    time.Time
	ExitCode    int
	StdoutBytes int
	StderrBytes int
	// WarnAfter is the interval the warning was armed for, carried so the
	// message delivered to the authoring agent can name it.
	WarnAfter  time.Duration
	ScriptPath string
}

type Job struct {
	mu        sync.RWMutex
	id        string
	author    string
	script    string
	toolName  string
	status    Status
	started   time.Time
	finished  time.Time
	exitCode  int
	warnAfter time.Duration
	// stdout and stderr are the job's capture buffers, and they are the
	// command's own cmd.Stdout and cmd.Stderr rather than copies taken after
	// cmd.Wait. They carry their own mutex and are never guarded by mu: the
	// exec copy goroutine writes them while the job runs and Output reads them
	// at any time, including mid-run, which is the whole point of job read.
	// Each is a leaf lock, taken under mu by snapshotLocked and never the other
	// way round.
	stdout       *limitedBuffer
	stderr       *limitedBuffer
	cancel       context.CancelFunc
	done         chan struct{}
	exited       chan struct{}
	cmd          *exec.Cmd
	closeProcess func()
	killProcess  func() error
	inline       bool
	log          *logx.JSONL
	scriptPath   string
}

func (j *Job) Snapshot() Snapshot {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return j.snapshotLocked()
}

// runningSnapshot reports whether the job is still running and, if it is, the
// snapshot describing it — both read in one critical section.
//
// The two have to be sampled together. Testing the status and then taking a
// second snapshot lets the job finish in the gap, so the warning handler would
// be handed a finished job's snapshot under a message whose text says the job
// is still running: a message contradicting the state it carries. The snapshot
// this returns is the state the warning describes, and what is delivered is
// exactly what was captured here.
//
// What that guarantees, precisely: the snapshot is accurate as of the timer's
// decision, not as of delivery. The lock is released before the handler runs,
// so a job can finish in between and its completion can reach the agent first,
// leaving the agent with a completion followed by a warning describing the
// state at the decision point. That is deliberate and is not worth closing:
// re-checking at delivery would narrow the window without removing it — the
// job can finish immediately after any check — while implying a freshness
// guarantee no lock-free path can keep. The completion is what carries the
// truth, it arrives on its own path, and the warning says in its own text that
// a result already in hand supersedes it.
func (j *Job) runningSnapshot() (Snapshot, bool) {
	j.mu.RLock()
	defer j.mu.RUnlock()
	if j.status != Running {
		return Snapshot{}, false
	}
	return j.snapshotLocked(), true
}

func (j *Job) snapshotLocked() Snapshot {
	return Snapshot{ID: j.id, Author: j.author, Script: j.script, ToolName: j.toolName, Status: j.status, Started: j.started, Finished: j.finished, ExitCode: j.exitCode, StdoutBytes: j.stdout.Len(), StderrBytes: j.stderr.Len(), WarnAfter: j.warnAfter, ScriptPath: j.scriptPath}
}

// Output returns everything captured from the job's two streams so far. It is
// valid while the job is still running and returns what has been captured up to
// that instant, which is what job read answers with; it does not wait for the
// job to finish. Until 2026-09-15 the buffers it reads were filled only after
// cmd.Wait returned, so a live job answered with two empty strings.
//
// It takes no job lock. The buffers guard themselves, and taking mu here would
// serialise a read of a running job behind whatever else holds it without
// making the answer any fresher.
func (j *Job) Output() (string, string) {
	return j.stdout.String(), j.stderr.String()
}

func (j *Job) Done() <-chan struct{}   { return j.done }
func (j *Job) Exited() <-chan struct{} { return j.exited }

func (j *Job) Kill() error {
	j.mu.Lock()
	if j.status != Running {
		j.mu.Unlock()
		return nil
	}
	cancel := j.cancel
	cmd := j.cmd
	killProcess := j.killProcess
	j.status = Killed
	j.mu.Unlock()
	// The tree kill goes first: cancel kills only the top process, and once it
	// is dead the wait goroutine may release the platform process handle.
	if killProcess != nil {
		err := killProcess()
		if cancel != nil {
			cancel()
		}
		if err != nil && cmd != nil {
			return killCommand(cmd)
		}
		return err
	}
	if cancel != nil {
		cancel()
	}
	if cmd != nil {
		return killCommand(cmd)
	}
	return nil
}

type Manager struct {
	mu         sync.RWMutex
	jobs       map[string]*Job
	logger     func(string) *logx.JSONL
	onWarning  func(Snapshot)
	onComplete func(Snapshot, string, string)
	// warnings counts the per-job warning timers. Close joins them under a
	// two-second cap, and nothing else constrains one once its timer has
	// fired: at that point the goroutine is committed to calling the handler,
	// and the handler writes into the owning runtime's session logs. See
	// Close.
	warnings sync.WaitGroup
	// closed is set by Close, under mu, before it kills anything or waits.
	// Start reads it and arms nothing afterwards, in the same acquisition that
	// registers the job and counts its timer: a check in one acquisition and
	// an Add in another would leave the window open rather than close it.
	closed bool
}

func NewManager(log *logx.JSONL) *Manager {
	if log == nil {
		return NewManagerWithLogger(nil)
	}
	return NewManagerWithLogger(func(string) *logx.JSONL { return log })
}

func NewManagerWithLogger(logger func(string) *logx.JSONL) *Manager {
	return &Manager{jobs: make(map[string]*Job), logger: logger}
}

// SetWarningHandler registers the callback that routes a long-running job's
// single warning to the agent that started it.
//
// The snapshot it receives was taken while the job was still running, in the
// critical section that decided so. The callback runs on the job's warning
// timer goroutine. It is not entered for a job that has already finished, nor
// for a timer that is still pending when shutdown cancels the job's context —
// but once that timer has elapsed the goroutine is committed, and no later
// cancellation can call it back. Close is what bounds it, and Close's bound is
// a two-second cap rather than a guarantee; a handler must therefore tolerate
// running while the runtime that registered it is shutting down.
func (m *Manager) SetWarningHandler(handler func(Snapshot)) {
	m.mu.Lock()
	m.onWarning = handler
	m.mu.Unlock()
}

// SetCompletionHandler registers the callback used to hand a finished job's
// captured output back to its owning runtime. The callback runs after the job
// is marked finished and its Done channel is closed, so Output and Snapshot
// are stable when it is invoked.
func (m *Manager) SetCompletionHandler(handler func(Snapshot, string, string)) {
	m.mu.Lock()
	m.onComplete = handler
	m.mu.Unlock()
}

func (m *Manager) Start(parent context.Context, spec Spec) (*Job, error) {
	if spec.Script == "" {
		return nil, fmt.Errorf("job script is empty")
	}
	if spec.Interpreter == "" && len(spec.Command) == 0 {
		spec.Interpreter = "bash"
	}
	ctx, cancel := context.WithCancel(parent)
	var cmd *exec.Cmd
	var err error
	var scriptPath, tempRoot string
	jobID := id.New("job")
	if len(spec.Command) > 0 {
		if spec.Command[0] == "" {
			cancel()
			return nil, fmt.Errorf("job command is empty")
		}
		cmd = exec.CommandContext(ctx, spec.Command[0], spec.Command[1:]...)
		cmd.Dir = spec.Dir
	} else {
		var root string
		root = spec.ScriptRoot
		if root == "" {
			tempRoot, err = os.MkdirTemp("", "slbh-job-")
			if err != nil {
				cancel()
				return nil, err
			}
			root = tempRoot
		}
		scriptPath, err = writeScript(root, jobID, spec.Interpreter, spec.Script)
		if err == nil {
			cmd, err = scriptCommand(ctx, spec.Interpreter, scriptPath, spec.Dir)
		}
	}
	if err != nil {
		if tempRoot != "" {
			_ = os.RemoveAll(tempRoot)
		}
		cancel()
		return nil, err
	}
	cmd.Env = append(cmd.Environ(), spec.Environment...)
	setProcessGroup(cmd)
	var log *logx.JSONL
	if m.logger != nil {
		log = m.logger(spec.Author)
	}
	job := &Job{id: jobID, author: spec.Author, script: spec.Script, toolName: spec.ToolName, status: Running, started: time.Now().UTC(), warnAfter: spec.WarnAfter, stdout: &limitedBuffer{}, stderr: &limitedBuffer{}, cancel: cancel, done: make(chan struct{}), exited: make(chan struct{}), cmd: cmd, log: log, scriptPath: scriptPath, inline: spec.Inline}
	cmd.Stdout, cmd.Stderr = job.stdout, job.stderr
	// Registration, the closed check and the warning count are one acquisition.
	// A Start that lands after Close has begun would otherwise Add to a
	// WaitGroup already at zero and being waited on — WaitGroup misuse — and
	// arm a timer that could fire into session logs Runtime.Close has since
	// closed. The whole job is refused rather than merely its warning: Close
	// has already taken its list of jobs to kill, so a job accepted now would
	// never be killed or drained, and its completion would be delivered to an
	// agent the runtime has stopped. Refusing is the honest answer, and no
	// process is spawned because cmd.Start has not run yet.
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		cancel()
		return nil, fmt.Errorf("job manager is closed")
	}
	m.jobs[job.id] = job
	warn := spec.WarnAfter > 0
	if warn {
		m.warnings.Add(1)
	}
	m.mu.Unlock()
	if err := cmd.Start(); err != nil {
		cancel()
		if warn {
			// The goroutine below never runs, so its Done must happen here or
			// Close would wait out its cap on a timer that does not exist.
			m.warnings.Done()
		}
		m.mu.Lock()
		delete(m.jobs, job.id)
		m.mu.Unlock()
		if tempRoot != "" {
			_ = os.RemoveAll(tempRoot)
		}
		return nil, err
	}
	// The job is already registered, so Kill can reach it from another
	// goroutine; the process hooks are published under the job lock that Kill
	// reads them under.
	closeProcess, killProcess, err := processStarted(cmd)
	if err != nil {
		// The process is already registered and must still be reaped. Keep the
		// job alive and use the platform fallback kill path if needed.
		closeProcess, killProcess = nil, nil
	}
	job.mu.Lock()
	job.closeProcess, job.killProcess = closeProcess, killProcess
	job.mu.Unlock()
	if job.log != nil {
		_ = job.log.Append(logx.Entry{Agent: spec.Author, Kind: "job_start", Text: spec.Script, Metadata: map[string]any{"job": job.id, "script_path": scriptPath}})
	}
	if warn {
		go func() {
			// The count was taken above, with the closed check, and is released
			// only when this goroutine returns, so Close can join it. Nothing
			// else bounds this goroutine: the select below decides whether the
			// warning fires, but once it has fired no channel can call it back.
			defer m.warnings.Done()
			timer := time.NewTimer(spec.WarnAfter)
			defer timer.Stop()
			select {
			case <-timer.C:
				snapshot, running := job.runningSnapshot()
				if !running {
					return
				}
				// A last look at the job's context before delivering. This
				// narrows the window in which a warning is handed to a runtime
				// that has begun shutting down; it does not close it, and it
				// is not what bounds delivery. Close's capped join is.
				if ctx.Err() != nil {
					return
				}
				m.mu.RLock()
				handler := m.onWarning
				m.mu.RUnlock()
				if handler != nil {
					handler(snapshot)
				}
			case <-job.done:
			case <-ctx.Done():
				// This arm covers the PRE-fire window only. ctx is the job's
				// own context, cancelled when the job ends and when the
				// runtime that owns the manager shuts down, so a job killed at
				// shutdown before its timer elapses gets no warning — right,
				// because there is no longer anyone to act on one. Once
				// timer.C has been selected this arm is unreachable, and it
				// guarantees nothing about what happens after that; the claim
				// that it did was wrong and is what Close's join now provides.
			}
		}()
	}
	go func() {
		err := cmd.Wait()
		job.mu.Lock()
		wasKilled := job.status == Killed
		// Nothing is copied into the capture buffers here. They are the
		// command's own sinks, already filled by the exec copy goroutines that
		// cmd.Wait has just joined.
		if !wasKilled {
			if err == nil {
				job.status = Complete
				job.exitCode = 0
			} else {
				job.status = Failed
				job.exitCode = exitCode(err)
			}
		}
		job.finished = time.Now().UTC()
		inline := job.inline
		close(job.exited)
		job.mu.Unlock()
		cancel()
		if job.closeProcess != nil {
			job.closeProcess()
		}
		if tempRoot != "" {
			_ = os.RemoveAll(tempRoot)
		}
		if job.log != nil {
			_ = job.log.Append(logx.Entry{Agent: spec.Author, Kind: "job_end", Metadata: map[string]any{"job": job.id, "status": job.Snapshot().Status, "exit_code": job.Snapshot().ExitCode}})
		}
		m.mu.RLock()
		handler := m.onComplete
		m.mu.RUnlock()
		if handler != nil && !inline {
			stdoutText, stderrText := job.Output()
			handler(job.Snapshot(), stdoutText, stderrText)
		}
		// done closes last, so that it means the job is finished *and* recorded
		// *and* delivered. Manager.Close waits on it before Runtime.Close shuts
		// the session loggers; closing it before these two steps let Close
		// return while this goroutine still had an Append to make, which then
		// failed with "log is closed" and dropped both the job_end record and
		// the job's result.
		close(job.done)
	}()
	return job, nil
}

// Wait returns a finished result, or marks a still-running job for exactly-once
// background delivery when the deadline expires.
func (m *Manager) Wait(j *Job, d time.Duration) (Snapshot, string, string, bool) {
	if d > 0 {
		timer := time.NewTimer(d)
		select {
		case <-j.exited:
			timer.Stop()
		case <-timer.C:
		}
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.status == Running {
		j.inline = false
		return j.snapshotLocked(), j.stdout.String(), j.stderr.String(), true
	}
	return j.snapshotLocked(), j.stdout.String(), j.stderr.String(), false
}

// WaitOrKill is Wait with a hard deadline: a job still running when d expires
// is killed rather than backgrounded. The job stays inline throughout, so its
// result is returned here and is never delivered a second time. The final
// value reports whether the deadline killed it.
func (m *Manager) WaitOrKill(j *Job, d time.Duration) (Snapshot, string, string, bool) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-j.exited:
	case <-timer.C:
		_ = j.Kill()
		<-j.exited
	}
	j.mu.RLock()
	defer j.mu.RUnlock()
	return j.snapshotLocked(), j.stdout.String(), j.stderr.String(), j.status == Killed
}

func (m *Manager) List() []Snapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]Snapshot, 0, len(m.jobs))
	for _, j := range m.jobs {
		result = append(result, j.Snapshot())
	}
	return result
}

func (m *Manager) Get(jobID string) (*Job, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	j, ok := m.jobs[jobID]
	return j, ok
}

func (m *Manager) Kill(jobID, author string) error {
	j, ok := m.Get(jobID)
	if !ok {
		return fmt.Errorf("job %q not found", jobID)
	}
	if j.author != author {
		return fmt.Errorf("job %q belongs to %q", jobID, j.author)
	}
	return j.Kill()
}

// Close marks the manager closed, kills every job, waits for each to report
// finished, and then joins the warning timers.
//
// The join matters because a warning goroutine whose timer has already fired
// is committed: it will call the warning handler whatever any channel does
// afterwards, and that handler delivers into the owning runtime — an agent's
// inbox and a session log. Runtime.Close calls this before it closes those
// logs, so without the join a warning could resume after shutdown and append
// into a closed log, where the error is discarded and the warning is silently
// lost.
//
// The join is capped at two seconds, the same idiom and the same cap as the
// per-job wait above, so shutdown has a ceiling whatever a handler does. That
// is a bound and not a guarantee, and the difference is worth stating: a
// handler still running at the cap is abandoned, and can still append into a
// log that is about to close. The runtime's own handler does a synchronous
// session-log append before it enqueues, so "the handler only enqueues" would
// have been the wrong reason to believe this cannot hang. A timer that has not
// fired costs nothing: the kill loop above cancels every job's context, which
// is the pre-fire arm of each timer's select.
func (m *Manager) Close() {
	m.mu.Lock()
	m.closed = true
	jobs := make([]*Job, 0, len(m.jobs))
	for _, j := range m.jobs {
		jobs = append(jobs, j)
	}
	m.mu.Unlock()
	for _, j := range jobs {
		_ = j.Kill()
	}
	for _, j := range jobs {
		select {
		case <-j.done:
		case <-time.After(2 * time.Second):
		}
	}
	joined := make(chan struct{})
	go func() {
		m.warnings.Wait()
		close(joined)
	}()
	select {
	case <-joined:
	case <-time.After(2 * time.Second):
	}
}

// limitedBuffer is one stream's capture buffer, written by the exec copy
// goroutine while the job runs and read concurrently by Job.Output and
// Job.Snapshot, so every access goes through mu.
//
// bytes.Buffer is a field and deliberately not embedded: embedding promotes
// every one of its unsynchronised methods, and a single promoted call from a
// reader would be a data race against the copy goroutine with nothing in the
// type to show it.
type limitedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

const limitedBufferMax = 4 * 1024 * 1024

// Write keeps at most limitedBufferMax bytes and discards the rest. It always
// reports the whole of p as written: a short count would make exec's copy
// goroutine stop with io.ErrShortWrite and leave the child blocked on a full
// pipe, so the overflow is consumed and dropped rather than refused.
func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	if remaining := limitedBufferMax - b.buf.Len(); remaining < n {
		p = p[:max(remaining, 0)]
	}
	_, _ = b.buf.Write(p)
	return n, nil
}

func (b *limitedBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Len()
}

func (b *limitedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
