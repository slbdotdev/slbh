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
	Author      string
	Script      string
	Command     []string
	Dir         string
	WarnAfter   time.Duration
	Environment []string
}

type Snapshot struct {
	ID          string
	Author      string
	Script      string
	Status      Status
	Started     time.Time
	Finished    time.Time
	ExitCode    int
	StdoutBytes int
	StderrBytes int
	WarnAfter   time.Duration
}

type Job struct {
	mu        sync.RWMutex
	id        string
	author    string
	script    string
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
	return Snapshot{ID: j.id, Author: j.author, Script: j.script, Status: j.status, Started: j.started, Finished: j.finished, ExitCode: j.exitCode, StdoutBytes: j.stdout.Len(), StderrBytes: j.stderr.Len(), WarnAfter: j.warnAfter}
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
	mu        sync.RWMutex
	jobs      map[string]*Job
	logger    func(string) *logx.JSONL
	onWarning func(Snapshot)
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

func (m *Manager) SetWarningHandler(handler func(Snapshot)) {
	m.mu.Lock()
	m.onWarning = handler
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
	job := &Job{id: id.New("job"), author: spec.Author, script: spec.Script, status: Running, started: time.Now().UTC(), warnAfter: spec.WarnAfter, cancel: cancel, done: make(chan struct{}), cmd: cmd, log: log}
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
		go func() {
			timer := time.NewTimer(spec.WarnAfter)
			defer timer.Stop()
			select {
			case <-timer.C:
				if job.Snapshot().Status != Running {
					return
				}
				m.mu.RLock()
				handler := m.onWarning
				m.mu.RUnlock()
				if handler != nil {
					handler(job.Snapshot())
				}
			case <-job.done:
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
		close(job.done)
		cancel()
		if job.log != nil {
			_ = job.log.Append(logx.Entry{Agent: spec.Author, Kind: "job_end", Metadata: map[string]any{"job": job.id, "status": job.Snapshot().Status, "exit_code": job.Snapshot().ExitCode}})
		}
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
