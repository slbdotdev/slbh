package job

import (
	"bytes"
	"context"
	"fmt"
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
	Author   string
	Script   string
	Command  []string
	ToolName string
	Dir      string
	// WarnAfter is the one timer a job has, and it warns exactly one party:
	// the agent named in Author, the agent that started the job. When it
	// elapses with the job still running, the manager hands the warning
	// handler a snapshot taken while the job was running, and the runtime
	// delivers it into that agent's inbox, waking it if it has gone idle.
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
	WarnAfter time.Duration
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
	stdout    bytes.Buffer
	stderr    bytes.Buffer
	cancel    context.CancelFunc
	done      chan struct{}
	cmd       *exec.Cmd
	log       *logx.JSONL
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
	return Snapshot{ID: j.id, Author: j.author, Script: j.script, ToolName: j.toolName, Status: j.status, Started: j.started, Finished: j.finished, ExitCode: j.exitCode, StdoutBytes: j.stdout.Len(), StderrBytes: j.stderr.Len(), WarnAfter: j.warnAfter}
}

func (j *Job) Output() (string, string) {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return j.stdout.String(), j.stderr.String()
}

func (j *Job) Done() <-chan struct{} { return j.done }

func (j *Job) Kill() error {
	j.mu.Lock()
	if j.status != Running {
		j.mu.Unlock()
		return nil
	}
	cancel := j.cancel
	cmd := j.cmd
	j.status = Killed
	j.mu.Unlock()
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
	// warnings counts the per-job warning timers. Close joins them, which is
	// the only thing that bounds a warning's lifetime once its timer has
	// fired: at that point the goroutine is committed to calling the handler,
	// and the handler writes into the owning runtime's session logs. See
	// Close.
	warnings sync.WaitGroup
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
// single warning to the agent that started it. The snapshot it receives was
// taken while the job was still running; the callback runs on the timer
// goroutine, which is bound to the job's context, so it cannot be entered once
// the job has ended or the runtime has begun shutting down.
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
	ctx, cancel := context.WithCancel(parent)
	var cmd *exec.Cmd
	var err error
	if len(spec.Command) > 0 {
		if spec.Command[0] == "" {
			cancel()
			return nil, fmt.Errorf("job command is empty")
		}
		cmd = exec.CommandContext(ctx, spec.Command[0], spec.Command[1:]...)
		cmd.Dir = spec.Dir
	} else {
		cmd, err = shellCommand(ctx, spec.Script, spec.Dir)
	}
	if err != nil {
		cancel()
		return nil, err
	}
	cmd.Env = append(cmd.Environ(), spec.Environment...)
	setProcessGroup(cmd)
	var log *logx.JSONL
	if m.logger != nil {
		log = m.logger(spec.Author)
	}
	job := &Job{id: id.New("job"), author: spec.Author, script: spec.Script, toolName: spec.ToolName, status: Running, started: time.Now().UTC(), warnAfter: spec.WarnAfter, cancel: cancel, done: make(chan struct{}), cmd: cmd, log: log}
	stdout, stderr := &limitedBuffer{}, &limitedBuffer{}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	m.mu.Lock()
	m.jobs[job.id] = job
	m.mu.Unlock()
	if err := cmd.Start(); err != nil {
		cancel()
		m.mu.Lock()
		delete(m.jobs, job.id)
		m.mu.Unlock()
		return nil, err
	}
	if job.log != nil {
		_ = job.log.Append(logx.Entry{Agent: spec.Author, Kind: "job_start", Text: spec.Script, Metadata: map[string]any{"job": job.id}})
	}
	if spec.WarnAfter > 0 {
		m.warnings.Add(1)
		go func() {
			// Counted before the goroutine starts and released only when it
			// returns, so Close can join it. Nothing else bounds this
			// goroutine: the select below decides whether the warning fires,
			// but once it has fired no channel can call it back.
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
				// is not what makes delivery safe. Close's join is.
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
		_, wasKilled := job.status, job.status == Killed
		job.stdout.Write(stdout.Bytes())
		job.stderr.Write(stderr.Bytes())
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
		job.mu.Unlock()
		cancel()
		if job.log != nil {
			_ = job.log.Append(logx.Entry{Agent: spec.Author, Kind: "job_end", Metadata: map[string]any{"job": job.id, "status": job.Snapshot().Status, "exit_code": job.Snapshot().ExitCode}})
		}
		m.mu.RLock()
		handler := m.onComplete
		m.mu.RUnlock()
		if handler != nil {
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

// Close kills every job, waits for each to report finished, and then joins the
// warning timers.
//
// The join is the last step and is unbounded on purpose. A warning goroutine
// whose timer has already fired is committed: it will call the warning handler
// whatever any channel does afterwards, and that handler delivers into the
// owning runtime — an agent's inbox and a session log. Runtime.Close calls
// this before it closes those logs, so without the join a warning could resume
// after shutdown and append into a closed log, where the error is discarded
// and the warning is silently lost.
//
// It cannot hang. The kill loop above cancels every job's context, which is
// the pre-fire arm of each timer's select, so a timer that has not fired
// returns at once; one that has fired runs a handler that only enqueues.
func (m *Manager) Close() {
	m.mu.RLock()
	jobs := make([]*Job, 0, len(m.jobs))
	for _, j := range m.jobs {
		jobs = append(jobs, j)
	}
	m.mu.RUnlock()
	for _, j := range jobs {
		_ = j.Kill()
	}
	for _, j := range jobs {
		select {
		case <-j.done:
		case <-time.After(2 * time.Second):
		}
	}
	m.warnings.Wait()
}

type limitedBuffer struct{ bytes.Buffer }

func (b *limitedBuffer) Write(p []byte) (int, error) {
	const max = 4 * 1024 * 1024
	if b.Len() < max {
		remaining := max - b.Len()
		if len(p) > remaining {
			p = p[:remaining]
		}
	}
	return b.Buffer.Write(p)
}
