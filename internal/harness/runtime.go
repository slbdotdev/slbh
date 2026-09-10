package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/slbdotdev/slbh/internal/config"
	"github.com/slbdotdev/slbh/internal/id"
	"github.com/slbdotdev/slbh/internal/job"
	"github.com/slbdotdev/slbh/internal/logx"
	"github.com/slbdotdev/slbh/internal/provider"
)

type Event struct {
	Time       time.Time      `json:"time"`
	RuntimeID  string         `json:"runtime"`
	AgentID    string         `json:"agent"`
	AgentTitle string         `json:"agent_title"`
	Kind       string         `json:"kind"`
	Text       string         `json:"text,omitempty"`
	Metadata   map[string]any `json:"metadata,omitempty"`
}

type AgentSnapshot struct {
	ID       string
	Title    string
	ParentID string
	Depth    int
	Model    string
	Effort   string
	Status   string
	Harness  string
	WorkDir  string
	SSH      string
}

type LaunchSpec struct {
	Title            string
	Harness          string
	Model            string
	Effort           string
	Brief            string
	WarnAfterSeconds int
	WorkingDir       string
	SSH              string
}

type Runtime struct {
	mu        sync.RWMutex
	id        string
	rootPath  string
	workDir   string
	config    config.Config
	ctx       context.Context
	cancel    context.CancelFunc
	log       *logx.JSONL
	jobs      *job.Manager
	agents    map[string]*Agent
	events    chan Event
	provider  func(model string) (provider.Provider, error)
	closeOnce sync.Once
}

type Options struct {
	Config   config.Config
	Provider func(model string) (provider.Provider, error)
	Events   chan Event
}

func New(cfg config.Config, options Options) (*Runtime, error) {
	if cfg.Home == "" {
		cfg = config.Load()
	}
	runtimeID := id.New("run")
	dir := filepath.Join(cfg.Home, "runtimes", runtimeID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	log, err := logx.Open(filepath.Join(dir, "transcript.jsonl"), runtimeID)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	workDir, _ := os.Getwd()
	r := &Runtime{id: runtimeID, rootPath: dir, workDir: workDir, config: cfg, ctx: ctx, cancel: cancel, log: log, agents: make(map[string]*Agent), events: options.Events, provider: options.Provider}
	if r.events == nil {
		r.events = make(chan Event, 1024)
	}
	if r.provider == nil {
		r.provider = func(model string) (provider.Provider, error) {
			return provider.ForModel(model, cfg.Endpoint)
		}
	}
	r.jobs = job.NewManager(log)
	r.jobs.SetWarningHandler(func(snapshot job.Snapshot) {
		r.emit(Event{AgentID: snapshot.Author, Kind: "job_warning", Text: "job is still running", Metadata: map[string]any{"job": snapshot.ID, "warn_after": snapshot.WarnAfter.String()}})
	})
	if err := r.writeMarker(); err != nil {
		_ = log.Close()
		cancel()
		return nil, err
	}
	root := r.newAgent("root", "", 0, cfg.RootModel, cfg.RootEffort)
	r.emit(Event{AgentID: root.ID, AgentTitle: root.Title, Kind: "runtime", Text: "runtime started"})
	return r, nil
}

func (r *Runtime) ID() string           { return r.id }
func (r *Runtime) Dir() string          { return r.rootPath }
func (r *Runtime) Events() <-chan Event { return r.events }
func (r *Runtime) Jobs() *job.Manager   { return r.jobs }
func (r *Runtime) Root() *Agent {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, agent := range r.agents {
		if agent.Depth == 0 {
			return agent
		}
	}
	return nil
}

func (r *Runtime) newAgent(title, parentID string, depth int, model, effort string) *Agent {
	agent := newAgent(r, id.New("agent"), title, parentID, depth, model, effort)
	r.mu.Lock()
	r.agents[agent.ID] = agent
	r.mu.Unlock()
	agent.start()
	return agent
}

func (r *Runtime) LaunchSubagent(parentID, title, brief string) (*Agent, error) {
	return r.LaunchSubagentSpec(parentID, LaunchSpec{Title: title, Brief: brief})
}

func (r *Runtime) LaunchSubagentSpec(parentID string, spec LaunchSpec) (*Agent, error) {
	r.mu.RLock()
	parent, ok := r.agents[parentID]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("parent agent %q not found", parentID)
	}
	if parent.Depth >= 2 {
		return nil, fmt.Errorf("agent depth limit is 2")
	}
	if spec.Title == "" {
		return nil, fmt.Errorf("subagent title is required")
	}
	model, effort := spec.Model, spec.Effort
	if model == "" {
		model = r.config.SubagentModel
	}
	if effort == "" {
		effort = r.config.SubagentEffort
	}
	workingDir := parent.WorkDir
	if spec.WorkingDir != "" {
		workingDir = spec.WorkingDir
		if !filepath.IsAbs(workingDir) {
			workingDir = filepath.Join(parent.WorkDir, workingDir)
		}
		var err error
		workingDir, err = filepath.Abs(workingDir)
		if err != nil {
			return nil, err
		}
		if info, statErr := os.Stat(workingDir); statErr != nil || !info.IsDir() {
			return nil, fmt.Errorf("working directory %q is not a directory", spec.WorkingDir)
		}
	}
	agent := r.newAgent(spec.Title, parentID, parent.Depth+1, model, effort)
	agent.Harness, agent.SSH = spec.Harness, spec.SSH
	if agent.Harness == "" {
		agent.Harness = "native"
	}
	agent.WorkDir = workingDir
	r.emit(Event{AgentID: agent.ID, AgentTitle: spec.Title, Kind: "status", Text: "subagent launched", Metadata: map[string]any{"parent": parentID, "harness": agent.Harness, "ssh": agent.SSH, "working_dir": agent.WorkDir}})
	if spec.Brief != "" {
		agent.Send(spec.Brief)
	}
	return agent, nil
}

func (r *Runtime) Agents() []AgentSnapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]AgentSnapshot, 0, len(r.agents))
	for _, a := range r.agents {
		result = append(result, a.Snapshot())
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Depth != result[j].Depth {
			return result[i].Depth < result[j].Depth
		}
		return result[i].Title < result[j].Title
	})
	return result
}

func (r *Runtime) Agent(agentID string) (*Agent, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.agents[agentID]
	return a, ok
}

func (r *Runtime) Compact(agentID string, keep int) (int, error) {
	agent, ok := r.Agent(agentID)
	if !ok {
		return 0, fmt.Errorf("agent %q not found", agentID)
	}
	return agent.Compact(keep), nil
}

func (r *Runtime) emit(event Event) {
	if event.Time.IsZero() {
		event.Time = time.Now().UTC()
	}
	event.RuntimeID = r.id
	if r.log != nil {
		_ = r.log.Append(logx.Entry{Time: event.Time, Agent: event.AgentID, Kind: event.Kind, Text: event.Text, Metadata: event.Metadata})
	}
	select {
	case r.events <- event:
	default:
		// The transcript is lossless; a slow UI must not stall token streaming.
	}
}

// EmitStatus lets front ends record local control-plane events without
// fabricating a provider turn.
func (r *Runtime) EmitStatus(kind, text string) {
	r.emit(Event{Kind: kind, Text: text})
}

func (r *Runtime) Close() error {
	var err error
	r.closeOnce.Do(func() {
		r.emit(Event{Kind: "runtime", Text: "runtime stopping"})
		r.cancel()
		r.mu.RLock()
		agents := make([]*Agent, 0, len(r.agents))
		for _, a := range r.agents {
			agents = append(agents, a)
		}
		r.mu.RUnlock()
		for _, a := range agents {
			a.stop()
		}
		r.jobs.Close()
		if removeErr := os.Remove(filepath.Join(r.rootPath, "runtime.json")); removeErr != nil && !os.IsNotExist(removeErr) {
			err = removeErr
		}
		if closeErr := r.log.Close(); err == nil {
			err = closeErr
		}
	})
	return err
}

func (r *Runtime) writeMarker() error {
	data, err := json.MarshalIndent(map[string]any{"runtime": r.id, "pid": os.Getpid(), "started": time.Now().UTC()}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(r.rootPath, "runtime.json"), append(data, '\n'), 0o600)
}
