package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
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
	ID              string
	Title           string
	ParentID        string
	Depth           int
	Model           string
	Effort          string
	Status          string
	Harness         string
	WorkDir         string
	ContextWindow   int
	ContextUsed     int
	CacheHitTokens  int
	CacheMissTokens int
}

type LaunchSpec struct {
	Title            string
	Harness          string
	Model            string
	Effort           string
	Brief            string
	WarnAfterSeconds int
	WorkingDir       string
}

type agentSession struct {
	id   string
	path string
	log  *logx.JSONL
}

type Runtime struct {
	mu        sync.RWMutex
	id        string
	rootPath  string
	workDir   string
	config    config.Config
	ctx       context.Context
	cancel    context.CancelFunc
	jobs      *job.Manager
	agents    map[string]*Agent
	rootID    string
	current   map[string]*agentSession
	sessions  []*agentSession
	events    chan Event
	provider  func(model string) (provider.Provider, error)
	catalog   []provider.Catalog
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
	ctx, cancel := context.WithCancel(context.Background())
	workDir, _ := os.Getwd()
	r := &Runtime{id: runtimeID, rootPath: dir, workDir: workDir, config: cfg, ctx: ctx, cancel: cancel, agents: make(map[string]*Agent), current: make(map[string]*agentSession), events: options.Events, provider: options.Provider}
	if r.events == nil {
		r.events = make(chan Event, 1024)
	}
	if r.provider == nil {
		r.provider = func(model string) (provider.Provider, error) {
			return provider.ForModel(model, cfg.Endpoint)
		}
	}
	r.jobs = job.NewManagerWithLogger(r.sessionLogger)
	r.jobs.SetWarningHandler(func(snapshot job.Snapshot) {
		r.emit(Event{AgentID: snapshot.Author, Kind: "job_warning", Text: "job is still running", Metadata: map[string]any{"job": snapshot.ID, "warn_after": snapshot.WarnAfter.String()}})
	})
	if err := r.writeMarker(); err != nil {
		cancel()
		return nil, err
	}
	rootModel := cfg.RootModel
	if !cfg.ModelApproved(rootModel) {
		rootModel = ""
	}
	root, err := r.newAgent("root", "", 0, rootModel, cfg.RootEffort)
	if err != nil {
		cancel()
		_ = os.Remove(filepath.Join(r.rootPath, "runtime.json"))
		r.closeSessions()
		return nil, err
	}
	r.mu.Lock()
	r.rootID = root.ID
	r.mu.Unlock()
	r.emit(Event{AgentID: root.ID, AgentTitle: root.Title, Kind: "runtime", Text: "runtime started"})
	return r, nil
}

func (r *Runtime) ID() string           { return r.id }
func (r *Runtime) Dir() string          { return r.rootPath }
func (r *Runtime) Home() string         { return r.config.Home }
func (r *Runtime) Events() <-chan Event { return r.events }
func (r *Runtime) Jobs() *job.Manager   { return r.jobs }

func (r *Runtime) Config() config.Config {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.config
}

// SetModelCatalog makes the live provider tree available to every agent's
// next context. Catalog discovery is intentionally initiated by the TUI, not
// during startup, so launching slbh never spends a network request merely to
// render the terminal.
func (r *Runtime) SetModelCatalog(catalog []provider.Catalog) {
	r.mu.Lock()
	r.catalog = append([]provider.Catalog(nil), catalog...)
	r.mu.Unlock()
}

func (r *Runtime) ModelCatalog() []provider.Catalog {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]provider.Catalog(nil), r.catalog...)
}

// ConfigureModels preserves the older two-slot API while keeping the leaf
// default aligned with the level-one subagent model.
func (r *Runtime) ConfigureModels(rootModel, subagentModel string, approved []string) error {
	cfg := r.Config()
	return r.ConfigureModelSlots(rootModel, subagentModel, cfg.LeafModel, approved)
}

// ConfigureModelSlots persists the user's model choices and updates the root
// agent immediately. An unapproved configured default is retained in the
// dotfile but resolves to no model until it is approved again.
func (r *Runtime) ConfigureModelSlots(rootModel, subagentModel, leafModel string, approved []string) error {
	r.mu.Lock()
	r.config.RootModel = rootModel
	r.config.SubagentModel = subagentModel
	r.config.LeafModel = leafModel
	r.config.ApprovedModels = append([]string(nil), approved...)
	cfg := r.config
	rootID := r.rootID
	r.mu.Unlock()
	if err := cfg.Save(); err != nil {
		return err
	}
	if root, ok := r.Agent(rootID); ok {
		model := rootModel
		if !cfg.ModelApproved(model) {
			model = ""
		}
		root.SetModel(model)
	}
	return nil
}

func (r *Runtime) ModelGuidance() string {
	r.mu.RLock()
	cfg := r.config
	catalog := append([]provider.Catalog(nil), r.catalog...)
	r.mu.RUnlock()

	approved := "none"
	if len(cfg.ApprovedModels) > 0 {
		approved = fmt.Sprintf("%v", cfg.ApprovedModels)
	}
	var branches []string
	for _, branch := range catalog {
		if branch.Err != "" {
			branches = append(branches, branch.Name+" (unavailable: "+branch.Err+")")
			continue
		}
		branches = append(branches, fmt.Sprintf("%s (%d models; choose in /models)", branch.Name, len(branch.Models)))
	}
	if len(branches) == 0 {
		branches = append(branches, "no provider catalog loaded; use the configured default or honor an explicit user model request")
	}
	defaults := fmt.Sprintf("defaults are root=%q, subagent=%q, leaf=%q", cfg.RootModel, cfg.SubagentModel, cfg.LeafModel)
	return "Model guidance: approved models are " + approved + ". " + defaults + ". Available provider models: " + strings.Join(branches, "; ") + ". Use the configured subagent default for level-one children and the leaf default for level-two children when no model is requested. A model explicitly requested by the user may override the approved list; do not invent model IDs."
}
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

func (r *Runtime) newAgent(title, parentID string, depth int, model, effort string) (*Agent, error) {
	agent := newAgent(r, id.New("agent"), title, parentID, depth, model, effort)
	session, err := r.openAgentSession(agent.ID)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	r.agents[agent.ID] = agent
	r.current[agent.ID] = session
	r.sessions = append(r.sessions, session)
	r.mu.Unlock()
	agent.start()
	return agent, nil
}

func (r *Runtime) openAgentSession(agentID string) (*agentSession, error) {
	sessionID := id.New("session")
	path := filepath.Join(r.rootPath, "agents", agentID, "sessions", sessionID, "transcript.jsonl")
	log, err := logx.OpenSession(path, r.id, sessionID)
	if err != nil {
		return nil, err
	}
	return &agentSession{id: sessionID, path: path, log: log}, nil
}

func (r *Runtime) sessionLogger(agentID string) *logx.JSONL {
	r.mu.RLock()
	if agentID == "" {
		agentID = r.rootID
	}
	session := r.current[agentID]
	r.mu.RUnlock()
	if session == nil {
		return nil
	}
	return session.log
}

func (r *Runtime) currentSession(agentID string) (*agentSession, error) {
	r.mu.RLock()
	session := r.current[agentID]
	r.mu.RUnlock()
	if session == nil {
		return nil, fmt.Errorf("agent %q not found", agentID)
	}
	return session, nil
}

func (r *Runtime) TranscriptPath(agentID string) (string, error) {
	session, err := r.currentSession(agentID)
	if err != nil {
		return "", err
	}
	return session.path, nil
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
	r.mu.RLock()
	cfg := r.config
	r.mu.RUnlock()
	if model == "" {
		model = cfg.SubagentModel
		if parent.Depth >= 1 && cfg.LeafModel != "" {
			model = cfg.LeafModel
		}
		if !cfg.ModelApproved(model) {
			model = ""
		}
	}
	if effort == "" {
		effort = cfg.SubagentEffort
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
	agent, err := r.newAgent(spec.Title, parentID, parent.Depth+1, model, effort)
	if err != nil {
		return nil, err
	}
	agent.Harness = spec.Harness
	if agent.Harness == "" {
		agent.Harness = "native"
	}
	agent.WorkDir = workingDir
	r.emit(Event{AgentID: agent.ID, AgentTitle: spec.Title, Kind: "status", Text: "subagent launched", Metadata: map[string]any{"parent": parentID, "harness": agent.Harness, "working_dir": agent.WorkDir}})
	if spec.Brief != "" {
		if err := agent.Send(spec.Brief); err != nil {
			return nil, err
		}
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

func (r *Runtime) Clear(agentID string) error {
	agent, ok := r.Agent(agentID)
	if !ok {
		return fmt.Errorf("agent %q not found", agentID)
	}
	session, err := r.openAgentSession(agentID)
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.current[agentID] = session
	r.sessions = append(r.sessions, session)
	agent.ClearHistory()
	r.mu.Unlock()
	return nil
}

func (r *Runtime) emit(event Event) {
	if event.Time.IsZero() {
		event.Time = time.Now().UTC()
	}
	event.RuntimeID = r.id
	r.mu.RLock()
	agentID := event.AgentID
	if agentID == "" {
		agentID = r.rootID
	}
	session := r.current[agentID]
	r.mu.RUnlock()
	if session != nil {
		_ = session.log.Append(logx.Entry{Time: event.Time, Agent: event.AgentID, Session: session.id, Kind: event.Kind, Text: event.Text, Metadata: event.Metadata})
	}
	select {
	case r.events <- event:
	default:
		// The transcript is lossless; a slow UI must not stall token streaming.
	}
}

func (r *Runtime) recordInferenceRequest(agent *Agent, round int, req provider.Request, p provider.Provider) {
	metadata := map[string]any{"round": round}
	if payloadProvider, ok := p.(provider.RequestPayloadProvider); ok {
		payload, err := payloadProvider.RequestPayload(req)
		if err != nil {
			metadata["payload_error"] = err.Error()
		} else {
			metadata["payload"] = json.RawMessage(payload)
			metadata["payload_sha256"] = provider.PayloadSHA256(payload)
		}
	} else if context, err := provider.ContextPayload(req); err != nil {
		metadata["context_error"] = err.Error()
	} else {
		metadata["context"] = json.RawMessage(context)
		metadata["context_sha256"] = provider.PayloadSHA256(context)
	}
	r.emit(Event{AgentID: agent.ID, AgentTitle: agent.Title, Kind: "inference_request", Metadata: metadata})
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
		r.mu.RLock()
		sessions := append([]*agentSession(nil), r.sessions...)
		r.mu.RUnlock()
		for _, session := range sessions {
			if closeErr := session.log.Close(); err == nil {
				err = closeErr
			}
		}
	})
	return err
}

func (r *Runtime) closeSessions() {
	r.mu.RLock()
	sessions := append([]*agentSession(nil), r.sessions...)
	r.mu.RUnlock()
	for _, session := range sessions {
		_ = session.log.Close()
	}
}

func (r *Runtime) writeMarker() error {
	data, err := json.MarshalIndent(map[string]any{"runtime": r.id, "pid": os.Getpid(), "started": time.Now().UTC()}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(r.rootPath, "runtime.json"), append(data, '\n'), 0o600)
}
