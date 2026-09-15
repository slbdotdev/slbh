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
	mu         sync.RWMutex
	id         string
	runtimeDir string
	workDir    string
	config     config.Config
	ctx        context.Context
	cancel     context.CancelFunc
	jobs       *job.Manager
	agents     map[string]*Agent
	seatID     string
	current    map[string]*agentSession
	// pending holds a session opened by Clear while the agent was still
	// mid-turn. It becomes current at that turn's end, so the turn that issued
	// a request keeps its own transcript through its last event.
	pending      map[string]*agentSession
	sessions     []*agentSession
	redactor     *secretRedactor
	events       chan Event
	eventMu      sync.Mutex
	eventQueue   []Event
	eventWake    chan struct{}
	provider     func(model string) (provider.Provider, error)
	codexCommand string
	catalog      []provider.Catalog
	closeOnce    sync.Once
}

type Options struct {
	Config       config.Config
	Provider     func(model string) (provider.Provider, error)
	Events       chan Event
	CodexCommand string
}

func New(cfg config.Config, options Options) (*Runtime, error) {
	if cfg.Home == "" {
		cfg = config.Load()
	}
	runtimeID := id.NewShort("run")
	dir := filepath.Join(cfg.Home, "runtimes", runtimeID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	workDir, _ := os.Getwd()
	r := &Runtime{id: runtimeID, runtimeDir: dir, workDir: workDir, config: cfg, ctx: ctx, cancel: cancel, agents: make(map[string]*Agent), current: make(map[string]*agentSession), pending: make(map[string]*agentSession), redactor: newSecretRedactor(os.Environ()), events: options.Events, eventWake: make(chan struct{}, 1), provider: options.Provider, codexCommand: options.CodexCommand}
	if r.events == nil {
		r.events = make(chan Event, 1024)
	}
	go r.dispatchEvents()
	if r.provider == nil {
		// The current config is read per call rather than captured, so a policy
		// authored from /models on an unmanaged host takes effect on the next
		// request instead of at the next launch. That is the whole point of the
		// escape hatch: a refusal the user cannot clear without restarting is
		// still a brick.
		r.provider = func(model string) (provider.Provider, error) {
			current := r.Config()
			return provider.ForModel(model, current.Endpoint, current.EndpointExplicit, current.Policy)
		}
	}
	r.jobs = job.NewManagerWithLogger(r.sessionLogger)
	r.jobs.SetWarningHandler(func(snapshot job.Snapshot) {
		r.deliverJobWarning(snapshot)
	})
	r.jobs.SetCompletionHandler(func(snapshot job.Snapshot, stdout, stderr string) {
		r.deliverJobResult(snapshot, stdout, stderr)
	})
	if err := r.writeMarker(); err != nil {
		cancel()
		return nil, err
	}
	seatModel := cfg.SeatModel
	if !cfg.ModelApproved(seatModel) {
		seatModel = ""
	}
	seat, err := r.newAgent("seat", "", 0, seatModel, cfg.SeatEffort)
	if err != nil {
		cancel()
		_ = os.Remove(filepath.Join(r.runtimeDir, "runtime.json"))
		r.closeSessions()
		return nil, err
	}
	r.mu.Lock()
	r.seatID = seat.ID
	r.mu.Unlock()
	seat.start()
	r.emit(Event{AgentID: seat.ID, AgentTitle: seat.Title, Kind: "runtime", Text: "runtime started"})
	return r, nil
}

func (r *Runtime) ID() string           { return r.id }
func (r *Runtime) Dir() string          { return r.runtimeDir }
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

// PolicySource reports which routing policy is in force and where it came
// from, so the TUI can say so rather than leaving a user to guess why a local
// edit changed nothing.
func (r *Runtime) PolicySource() config.PolicySource {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.config.PolicySource
}

// AuthorLocalPolicy writes a local routing policy into the app-owned
// config.json and re-resolves which policy is in force.
//
// On a managed host the managed file still wins, and the returned source says
// so: the write is honest but inert, which is exactly what the precedence rule
// promises and what the user must be told. On an unmanaged host this is what
// turns the fail-closed refusal back into a working harness.
func (r *Runtime) AuthorLocalPolicy(policy provider.Policy) (config.PolicySource, error) {
	// Resolution re-reads the managed file, so it happens on a copy with no
	// lock held: a request resolving its own route takes the same lock, and
	// blocking it behind a file read for a menu keypress would be a poor
	// trade. Only the three policy fields are then written back, so a model
	// change made in between is not clobbered by this copy.
	cfg := r.Config()
	if err := cfg.ApplyLocalPolicy(policy); err != nil {
		return config.PolicySource{}, err
	}
	r.mu.Lock()
	r.config.LocalPolicy = cfg.LocalPolicy
	r.config.Policy = cfg.Policy
	r.config.PolicySource = cfg.PolicySource
	saved := r.config
	r.mu.Unlock()
	if err := saved.Save(); err != nil {
		return saved.PolicySource, err
	}
	return saved.PolicySource, nil
}

// ConfigureModels preserves the older two-slot API while keeping the leaf
// default aligned with the level-one subagent model.
func (r *Runtime) ConfigureModels(seatModel, subagentModel string, approved []string) error {
	cfg := r.Config()
	return r.ConfigureModelSlots(seatModel, subagentModel, cfg.LeafModel, approved)
}

// ConfigureModelSlots persists the user's model choices and updates the seat
// agent immediately. An unapproved configured default is retained in the
// dotfile but resolves to no model until it is approved again.
func (r *Runtime) ConfigureModelSlots(seatModel, subagentModel, leafModel string, approved []string) error {
	r.mu.Lock()
	r.config.SeatModel = seatModel
	r.config.SubagentModel = subagentModel
	r.config.LeafModel = leafModel
	r.config.ApprovedModels = append([]string(nil), approved...)
	cfg := r.config
	seatID := r.seatID
	r.mu.Unlock()
	if err := cfg.Save(); err != nil {
		return err
	}
	if seat, ok := r.Agent(seatID); ok {
		model := seatModel
		if !cfg.ModelApproved(model) {
			model = ""
		}
		seat.SetModel(model)
	}
	return nil
}

// LayerInstructions returns the managed role document for an agent at the
// given depth, or the empty string when that layer has no deployed document.
//
// It is read through the runtime rather than from the agent's own state
// because the documents are org policy: one copy, resolved once at startup,
// shared by every agent the runtime owns.
func (r *Runtime) LayerInstructions(depth int) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.config.Instructions.For(depth)
}

// InstructionSource reports where the layer documents came from, for the TUI.
func (r *Runtime) InstructionSource() config.InstructionSource {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.config.Instructions.Source
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
	defaults := fmt.Sprintf("defaults are seat=%q, subagent=%q, leaf=%q", cfg.SeatModel, cfg.SubagentModel, cfg.LeafModel)
	return "Model guidance: approved models are " + approved + ". " + defaults + ". Available provider models: " + strings.Join(branches, "; ") + ". Use the configured subagent default for level-one children and the leaf default for level-two children when no model is requested. A model explicitly requested by the user may override the approved list; do not invent model IDs. Codex leaves use the headless Codex app-server and ChatGPT model slugs, independent of the native approval list. To launch one from a native seat or level-one agent, set harness to \"codex\" and pass the exact ChatGPT model slug in model; never substitute a native default for a Codex leaf."
}
func (r *Runtime) Seat() *Agent {
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
	agent := newAgent(r, id.NewShort("agent"), title, parentID, depth, model, effort)
	session, err := r.openAgentSession(agent.ID)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	r.agents[agent.ID] = agent
	r.current[agent.ID] = session
	r.sessions = append(r.sessions, session)
	r.mu.Unlock()
	return agent, nil
}

func (r *Runtime) openAgentSession(agentID string) (*agentSession, error) {
	sessionID := id.New("session")
	path := filepath.Join(r.runtimeDir, "agents", agentID, "sessions", sessionID, "transcript.jsonl")
	log, err := logx.OpenSession(path, r.id, sessionID)
	if err != nil {
		return nil, err
	}
	return &agentSession{id: sessionID, path: path, log: log}, nil
}

func (r *Runtime) sessionLogger(agentID string) *logx.JSONL {
	r.mu.RLock()
	if agentID == "" {
		agentID = r.seatID
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
	if parent.Harness != "native" {
		return nil, fmt.Errorf("only native slbh agents may launch subagents")
	}
	if spec.Title == "" {
		return nil, fmt.Errorf("subagent title is required")
	}
	if spec.Harness != "" && spec.Harness != "native" && spec.Harness != "codex" {
		return nil, fmt.Errorf("unsupported harness %q", spec.Harness)
	}
	model, effort := strings.TrimSpace(spec.Model), strings.TrimSpace(spec.Effort)
	r.mu.RLock()
	cfg := r.config
	r.mu.RUnlock()
	if model == "" {
		if spec.Harness == "codex" {
			return nil, fmt.Errorf("Codex leaves require an explicit ChatGPT model in launch_subagent.model")
		}
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
	if agent.Harness == "codex" {
		if err := agent.startCodex(r.codexCommand); err != nil {
			r.discardAgent(agent.ID)
			return nil, err
		}
	} else {
		agent.start()
	}
	r.emit(Event{AgentID: agent.ID, AgentTitle: spec.Title, Kind: "status", Text: "subagent launched", Metadata: map[string]any{"parent": parentID, "harness": agent.Harness, "working_dir": agent.WorkDir}})
	if spec.Brief != "" {
		if err := agent.Send(spec.Brief); err != nil {
			agent.stop()
			r.discardAgent(agent.ID)
			return nil, err
		}
	}
	return agent, nil
}

func (r *Runtime) discardAgent(agentID string) {
	r.mu.Lock()
	delete(r.agents, agentID)
	session := r.current[agentID]
	delete(r.current, agentID)
	for i, candidate := range r.sessions {
		if candidate == session {
			r.sessions = append(r.sessions[:i], r.sessions[i+1:]...)
			break
		}
	}
	r.mu.Unlock()
	if session != nil {
		_ = session.log.Close()
	}
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
	// ClearHistory deliberately lets an in-flight provider request run to
	// completion, and emit resolves the session per event, so swapping the log
	// target here would write the tail of the old turn — its deltas, its usage,
	// its turn_done — into a transcript that never issued the request. The new
	// file would open mid-answer to a question it does not contain. The swap
	// therefore waits for the turn boundary; an idle agent has no turn in
	// flight, so for it the boundary is now.
	// Both backends defer. An earlier version excepted Codex on the grounds
	// that its clear interrupts the turn, but clear() only sets `resetting` and
	// signals `wake`: the turn/interrupt RPC happens later, in reset(), when
	// the leaf's run loop next services that signal. In the gap the
	// app-server's already-queued deltas still pass the threadID guard and
	// would land in the new transcript — the very defect this defers to avoid.
	// reset() promotes explicitly once the interrupt has returned.
	idle := agent.codexBackend() == nil && agent.Snapshot().Status != "thinking"
	r.mu.Lock()
	if superseded, ok := r.pending[agentID]; ok {
		_ = superseded.log.Close()
	}
	r.pending[agentID] = session
	r.sessions = append(r.sessions, session)
	r.mu.Unlock()
	agent.ClearHistory()
	if idle {
		r.promotePending(agentID)
	}
	return nil
}

// redactSecrets strips credential values from text that is about to enter
// durable state or the conversation. Redacting at emit alone was not enough:
// the raw tool result also entered `history`, so the next round marshalled the
// credential into the request payload — storing it *and sending it to the
// provider*. Capture-time redaction closes both, and is the only one of the
// two that prevents transmission.
func (r *Runtime) redactSecrets(text string) string {
	r.mu.RLock()
	redactor := r.redactor
	r.mu.RUnlock()
	return redactor.redact(text)
}

// promotePending makes a session opened by Clear the log target. It runs at a
// turn boundary, the first moment at which no earlier turn can still be
// streaming into the transcript it was issued against.
func (r *Runtime) promotePending(agentID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if session, ok := r.pending[agentID]; ok {
		r.current[agentID] = session
		delete(r.pending, agentID)
	}
}

// Done closes when the runtime is shutting down. A headless run selects on it
// so that a signal arriving during an in-flight turn ends the run, instead of
// blocking forever on an events channel that Close never closes.
func (r *Runtime) Done() <-chan struct{} { return r.ctx.Done() }

func (r *Runtime) emit(event Event) {
	if event.Time.IsZero() {
		event.Time = time.Now().UTC()
	}
	event.RuntimeID = r.id
	r.mu.RLock()
	agentID := event.AgentID
	if agentID == "" {
		agentID = r.seatID
	}
	session := r.current[agentID]
	redactor := r.redactor
	r.mu.RUnlock()
	// The tools hand a child the parent environment on purpose, so an agent
	// that runs `env`, or a prompt-injected call that names one variable, would
	// otherwise put a vaulted credential into durable JSONL and onto the
	// screen. Redacting here covers every producer at once, and covers the
	// stored copy and the rendered one identically.
	event.Text = redactor.redact(event.Text)
	event.Metadata = redactor.redactMetadata(event.Metadata)
	if session != nil {
		_ = session.log.Append(logx.Entry{Time: event.Time, Agent: event.AgentID, Session: session.id, Kind: event.Kind, Text: event.Text, Metadata: event.Metadata})
	}
	// A session Clear opened mid-turn becomes the log target only now, at the
	// end of the turn that was in flight. "error" ends a turn too: fail() emits
	// it with no turn_done to follow, so promoting on turn_done alone would
	// strand the new session forever behind a turn that ended badly.
	if event.Kind == "turn_done" || event.Kind == "error" {
		r.promotePending(agentID)
	}
	r.eventMu.Lock()
	r.eventQueue = append(r.eventQueue, event)
	r.eventMu.Unlock()
	select {
	case r.eventWake <- struct{}{}:
	default:
		// The wake channel is only a notification. Events stay in the FIFO
		// queue until the dispatcher hands them to the UI.
	}
}

func (r *Runtime) dispatchEvents() {
	for {
		select {
		case <-r.eventWake:
			r.flushEvents()
		case <-r.ctx.Done():
			return
		}
	}
}

func (r *Runtime) flushEvents() {
	for {
		r.eventMu.Lock()
		if len(r.eventQueue) == 0 {
			r.eventMu.Unlock()
			return
		}
		event := r.eventQueue[0]
		r.eventQueue[0] = Event{}
		r.eventQueue = r.eventQueue[1:]
		r.eventMu.Unlock()
		select {
		case r.events <- event:
		case <-r.ctx.Done():
			return
		}
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

// deliverJobWarning routes a job's single warn_after_seconds warning.
//
// The audience is the agent that started the job and nobody else. The human
// operator does not create these jobs, never sees the ones a subagent runs,
// and telling the human is the control agent's duty rather than the job
// manager's — so the warning goes into the authoring agent's inbox by the same
// path a completion takes, and wakes it if it has gone idle. It used to reach
// the TUI event stream alone, which meant the one party the timer exists to
// inform was the one party that never heard it.
//
// The TUI event is still emitted, and first, so the warning is recorded in the
// transcript and shown even when the agent has since been stopped. It is a
// record, not the delivery. Nothing is built on it.
//
// The snapshot arrives captured under the job's own lock and is passed through
// unchanged: what the agent is told and what the manager observed are the same
// reading.
func (r *Runtime) deliverJobWarning(snapshot job.Snapshot) {
	r.emit(Event{AgentID: snapshot.Author, Kind: "job_warning", Text: "job is still running", Metadata: map[string]any{"job": snapshot.ID, "warn_after": snapshot.WarnAfter.String()}})
	agent, ok := r.Agent(snapshot.Author)
	if !ok {
		return
	}
	if err := agent.receiveJobWarning(snapshot); err != nil {
		r.emit(Event{AgentID: snapshot.Author, AgentTitle: agent.Title, Kind: "delivery_error", Text: err.Error(), Metadata: map[string]any{"job": snapshot.ID}})
	}
}

func (r *Runtime) deliverJobResult(snapshot job.Snapshot, stdout, stderr string) {
	agent, ok := r.Agent(snapshot.Author)
	if !ok {
		return
	}
	if err := agent.receiveJobResult(snapshot, stdout, stderr); err != nil {
		r.emit(Event{AgentID: snapshot.Author, AgentTitle: agent.Title, Kind: "delivery_error", Text: err.Error(), Metadata: map[string]any{"job": snapshot.ID}})
	}
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
		if removeErr := os.Remove(filepath.Join(r.runtimeDir, "runtime.json")); removeErr != nil && !os.IsNotExist(removeErr) {
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
	return os.WriteFile(filepath.Join(r.runtimeDir, "runtime.json"), append(data, '\n'), 0o600)
}
