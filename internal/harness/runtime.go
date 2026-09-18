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
	"github.com/slbdotdev/slbh/internal/orgstore"
	"github.com/slbdotdev/slbh/internal/provider"
	"github.com/slbdotdev/slbh/internal/seam"
)

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

type queuedEvent struct {
	event     seam.Event
	delivered chan struct{}
	sequence  uint64
}

type eventSubscriber struct {
	events chan seam.Event
	after  uint64
}

const subscriberEventBuffer = 1024

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
	pending           map[string]*agentSession
	sessions          []*agentSession
	redactor          *secretRedactor
	events            chan seam.Event
	eventMu           sync.Mutex
	eventQueue        []queuedEvent
	eventWake         chan struct{}
	eventStop         chan struct{}
	eventDone         chan struct{}
	eventsClosed      bool
	eventSequence     uint64
	subscribers       map[uint64]eventSubscriber
	nextSubscriber    uint64
	subscribersClosed bool
	provider          func(model string) (provider.Provider, error)
	codexCommand      string
	claudeCommand     string
	catalog           []provider.Catalog
	closeOnce         sync.Once
	orgStore          *orgstore.Store
	requestWatchStop  chan struct{}
	requestWatchDone  chan struct{}
	requestPoll       time.Duration
}

type Options struct {
	Config        config.Config
	Provider      func(model string) (provider.Provider, error)
	Events        chan seam.Event
	CodexCommand  string
	ClaudeCommand string
	// RequestPollInterval defaults to two seconds. Tests and embedders may use
	// a shorter interval; production callers should leave it zero.
	RequestPollInterval time.Duration
}

var _ seam.Runtime = (*Runtime)(nil)

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
	store, err := orgstore.Open(cfg.Home)
	if err != nil {
		cancel()
		return nil, err
	}
	poll := options.RequestPollInterval
	if poll <= 0 {
		poll = 2 * time.Second
	}
	r := &Runtime{id: runtimeID, runtimeDir: dir, workDir: workDir, config: cfg, ctx: ctx, cancel: cancel, agents: make(map[string]*Agent), current: make(map[string]*agentSession), pending: make(map[string]*agentSession), redactor: newSecretRedactor(os.Environ()), events: options.Events, eventWake: make(chan struct{}, 1), eventStop: make(chan struct{}), eventDone: make(chan struct{}), subscribers: make(map[uint64]eventSubscriber), provider: options.Provider, codexCommand: options.CodexCommand, claudeCommand: options.ClaudeCommand, orgStore: store, requestWatchStop: make(chan struct{}), requestWatchDone: make(chan struct{}), requestPoll: poll}
	if r.events == nil {
		r.events = make(chan seam.Event, 1024)
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
	r.emit(seam.Event{AgentID: seat.ID, AgentTitle: seat.Title, Kind: "runtime", Text: "runtime started"})
	go r.watchOrgRequests()
	return r, nil
}

func (r *Runtime) ID() string                { return r.id }
func (r *Runtime) Dir() string               { return r.runtimeDir }
func (r *Runtime) Home() string              { return r.config.Home }
func (r *Runtime) Events() <-chan seam.Event { return r.events }

func (r *Runtime) Subscribe() (<-chan seam.Event, func()) {
	events := make(chan seam.Event, subscriberEventBuffer)
	r.eventMu.Lock()
	if r.subscribersClosed {
		close(events)
		r.eventMu.Unlock()
		return events, func() {}
	}
	r.nextSubscriber++
	id := r.nextSubscriber
	r.subscribers[id] = eventSubscriber{events: events, after: r.eventSequence}
	r.eventMu.Unlock()

	var once sync.Once
	return events, func() {
		once.Do(func() {
			r.eventMu.Lock()
			if subscriber, ok := r.subscribers[id]; ok {
				delete(r.subscribers, id)
				close(subscriber.events)
			}
			r.eventMu.Unlock()
		})
	}
}

func (r *Runtime) Config() config.Config {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return cloneConfig(r.config)
}

// setModelCatalog makes the live provider tree available to every agent's
// next context. Catalog discovery is intentionally initiated by the TUI, not
// during startup, so launching slbh never spends a network request merely to
// render the terminal.
func (r *Runtime) setModelCatalog(catalog []provider.Catalog) {
	r.mu.Lock()
	r.catalog = cloneCatalog(catalog)
	r.mu.Unlock()
}

func (r *Runtime) ModelCatalog() []provider.Catalog {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return cloneCatalog(r.catalog)
}

// PolicySource reports which routing policy is in force and where it came
// from, so the TUI can say so rather than leaving a user to guess why a local
// edit changed nothing.
func (r *Runtime) PolicySource() config.PolicySource {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.config.PolicySource
}

// authorLocalPolicy writes a local routing policy into the app-owned
// config.json and re-resolves which policy is in force.
//
// On a managed host the managed file still wins, and the returned source says
// so: the write is honest but inert, which is exactly what the precedence rule
// promises and what the user must be told. On an unmanaged host this is what
// turns the fail-closed refusal back into a working harness.
func (r *Runtime) authorLocalPolicy(policy provider.Policy) (config.PolicySource, error) {
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

// configureModels preserves the older two-slot API while keeping the leaf
// default aligned with the level-one subagent model.
func (r *Runtime) configureModels(seatModel, subagentModel string, approved []string) error {
	cfg := r.Config()
	return r.configureModelSlots(seatModel, subagentModel, cfg.LeafModel, approved)
}

// configureModelSlots persists the user's model choices and updates the seat
// agent immediately. An unapproved configured default is retained in the
// dotfile but resolves to no model until it is approved again.
func (r *Runtime) configureModelSlots(seatModel, subagentModel, leafModel string, approved []string) error {
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
	if seat, ok := r.lookupAgent(seatID); ok {
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
	source := r.config.Instructions.Source
	source.Missing = append([]string(nil), source.Missing...)
	return source
}

// SkillSource reports where the managed skill metadata came from and why any
// layer or skill was omitted.
func (r *Runtime) SkillSource() config.SkillSource {
	r.mu.RLock()
	defer r.mu.RUnlock()
	source := r.config.Skills.Source
	source.Missing = append([]string(nil), source.Missing...)
	return source
}

// LayerSkillPrompt returns the metadata-only skill section for a native agent
// at depth. Codex and Claude Code leaves use separate prompt paths and do not
// call it because their own harnesses load skills.
func (r *Runtime) LayerSkillPrompt(depth int) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.config.Skills.PromptFor(depth)
}

func (r *Runtime) ModelGuidance() string {
	r.mu.RLock()
	cfg := r.config
	catalog := cloneCatalog(r.catalog)
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
	return "Model guidance: approved models are " + approved + ". " + defaults + ". Available provider models: " + strings.Join(branches, "; ") + ". The depth-0 Seat may launch only a native depth-1 Manager; only a native depth-1 Manager may launch a depth-2 leaf. A Manager may use any supported leaf harness: native for Flex, codex for Codex, or claude_code for Claude Code. Every depth-2 launch must pass a non-empty model explicitly; neither the subagent default nor the leaf default is substituted for a leaf. The configured subagent default applies only to a native depth-1 Manager whose launch omits model. A model explicitly requested by the user may override the approved list; do not invent model IDs. Codex leaves use the headless Codex app-server and exact ChatGPT model slugs, independent of the native approval list. Claude Code leaves use the host's claude.ai login and exact Claude model slugs; never substitute a native provider route."
}
func (r *Runtime) seat() *Agent {
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

func (r *Runtime) launchSubagent(parentID, title, brief string) (*Agent, error) {
	return r.launchSubagentSpec(parentID, LaunchSpec{Title: title, Brief: brief})
}

func (r *Runtime) launchSubagentSpec(parentID string, spec LaunchSpec) (*Agent, error) {
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
	harness := strings.TrimSpace(spec.Harness)
	if harness == "" {
		harness = "native"
	}
	if harness != "native" && harness != "codex" && harness != "claude_code" {
		return nil, fmt.Errorf("unsupported harness %q", harness)
	}
	if parent.Depth == 0 && harness != "native" {
		return nil, fmt.Errorf("the depth-0 Seat may launch only a native depth-1 Manager; %s leaves must be launched by a native depth-1 Manager", leafHarnessName(harness))
	}
	model, effort := strings.TrimSpace(spec.Model), strings.TrimSpace(spec.Effort)
	r.mu.RLock()
	cfg := r.config
	r.mu.RUnlock()
	if model == "" {
		if parent.Depth == 1 {
			return nil, fmt.Errorf("a depth-1 Manager launching a depth-2 %s leaf must pass a non-empty explicit model in launch_subagent.model; subagent and leaf defaults are not used for leaves", leafHarnessName(harness))
		}
		model = cfg.SubagentModel
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
	agent.Harness = harness
	agent.WorkDir = workingDir
	if agent.Harness == "codex" {
		if err := agent.startCodex(r.codexCommand); err != nil {
			r.discardAgent(agent.ID)
			return nil, err
		}
	} else if agent.Harness == "claude_code" {
		if err := agent.startClaude(r.claudeCommand); err != nil {
			r.discardAgent(agent.ID)
			return nil, err
		}
	} else {
		agent.start()
	}
	r.emit(seam.Event{AgentID: agent.ID, AgentTitle: spec.Title, Kind: "status", Text: "subagent launched", Metadata: map[string]any{"parent": parentID, "harness": agent.Harness, "working_dir": agent.WorkDir}})
	if spec.Brief != "" {
		if err := agent.Send(spec.Brief); err != nil {
			agent.stop()
			r.discardAgent(agent.ID)
			return nil, err
		}
	}
	return agent, nil
}

func leafHarnessName(harness string) string {
	switch harness {
	case "codex":
		return "Codex"
	case "claude_code":
		return "Claude Code"
	default:
		return "native/Flex"
	}
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

func (r *Runtime) Agents() []seam.AgentSnapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]seam.AgentSnapshot, 0, len(r.agents))
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

// JobSnapshots returns copies of all jobs owned by the runtime.
func (r *Runtime) JobSnapshots() []seam.JobSnapshot {
	jobs := r.jobs.List()
	result := make([]seam.JobSnapshot, len(jobs))
	for i, snapshot := range jobs {
		result[i] = seam.JobSnapshot{
			ID:          snapshot.ID,
			Author:      snapshot.Author,
			Script:      snapshot.Script,
			ToolName:    snapshot.ToolName,
			Status:      string(snapshot.Status),
			Started:     snapshot.Started,
			Finished:    snapshot.Finished,
			ExitCode:    snapshot.ExitCode,
			StdoutBytes: snapshot.StdoutBytes,
			StderrBytes: snapshot.StderrBytes,
			WarnAfter:   snapshot.WarnAfter,
		}
	}
	return result
}

// sendPrompt delivers a user prompt to an agent.
func (r *Runtime) sendPrompt(agentID, prompt string) error {
	agent, ok := r.lookupAgent(agentID)
	if !ok {
		return fmt.Errorf("agent %q not found", agentID)
	}
	return agent.Send(prompt)
}

// steerAgent delivers a steering message to an agent.
func (r *Runtime) steerAgent(agentID, message string) error {
	agent, ok := r.lookupAgent(agentID)
	if !ok {
		return fmt.Errorf("agent %q not found", agentID)
	}
	return agent.Steer(message)
}

// setAgentEffort updates an agent's inference effort.
func (r *Runtime) setAgentEffort(agentID, effort string) error {
	agent, ok := r.lookupAgent(agentID)
	if !ok {
		return fmt.Errorf("agent %q not found", agentID)
	}
	agent.SetEffort(effort)
	return nil
}

func (r *Runtime) lookupAgent(agentID string) (*Agent, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.agents[agentID]
	return a, ok
}

func (r *Runtime) compact(agentID string, keep int) (int, error) {
	agent, ok := r.lookupAgent(agentID)
	if !ok {
		return 0, fmt.Errorf("agent %q not found", agentID)
	}
	return agent.Compact(keep), nil
}

func (r *Runtime) clear(agentID string) error {
	agent, ok := r.lookupAgent(agentID)
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
	// Native and Codex defer. An earlier version excepted Codex on the grounds
	// that its clear interrupts the turn, but clear() only sets `resetting` and
	// signals `wake`: the turn/interrupt RPC happens later, in reset(), when
	// the leaf's run loop next services that signal. In the gap the
	// app-server's already-queued deltas still pass the threadID guard and
	// would land in the new transcript — the very defect this defers to avoid.
	// reset() promotes explicitly once the interrupt has returned. Claude Code
	// clear synchronously stops its old stream, promotes at that boundary, and
	// starts a fresh process with fresh conversation history.
	idle := agent.codexBackend() == nil && agent.claudeBackend() == nil && agent.Snapshot().Status != "thinking"
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

func (r *Runtime) emit(event seam.Event) {
	r.emitQueued(event, nil)
}

func (r *Runtime) emitQueued(event seam.Event, delivered chan struct{}) {
	r.queueEvent(event, delivered, false)
}

func (r *Runtime) queueEvent(event seam.Event, delivered chan struct{}, closeEvents bool) {
	r.eventMu.Lock()
	if r.eventsClosed {
		r.eventMu.Unlock()
		return
	}
	r.eventMu.Unlock()

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
	if r.eventsClosed {
		r.eventMu.Unlock()
		return
	}
	if closeEvents {
		r.eventsClosed = true
	}
	r.eventSequence++
	r.eventQueue = append(r.eventQueue, queuedEvent{event: event, delivered: delivered, sequence: r.eventSequence})
	r.eventMu.Unlock()
	select {
	case r.eventWake <- struct{}{}:
	default:
		// The wake channel is only a notification. Events stay in the FIFO
		// queue until the dispatcher hands them to the UI.
	}
}

func (r *Runtime) dispatchEvents() {
	defer close(r.eventDone)
	for {
		select {
		case <-r.eventWake:
			r.flushEvents()
		case <-r.eventStop:
			return
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
		queued := r.eventQueue[0]
		r.eventQueue[0] = queuedEvent{}
		r.eventQueue = r.eventQueue[1:]
		r.eventMu.Unlock()
		r.dispatchSubscribers(queued)
		select {
		case r.events <- queued.event:
			if queued.delivered != nil {
				close(queued.delivered)
			}
		case <-r.eventStop:
			return
		case <-r.ctx.Done():
			return
		}
	}
}

func (r *Runtime) dispatchSubscribers(queued queuedEvent) {
	r.eventMu.Lock()
	defer r.eventMu.Unlock()
	for _, subscriber := range r.subscribers {
		if queued.sequence <= subscriber.after {
			continue
		}
		deliverSubscriberEvent(subscriber.events, queued.event)
	}
}

func deliverSubscriberEvent(events chan seam.Event, event seam.Event) {
	select {
	case events <- event:
		return
	default:
	}

	buffered := make([]seam.Event, 0, cap(events))
	for {
		select {
		case existing := <-events:
			buffered = append(buffered, existing)
		default:
			goto drained
		}
	}

drained:
	markerCount, hasMarker := subscriberDropCount(buffered)
	if hasMarker {
		payload := append(buffered[1:], event)
		dropped := len(payload) - (cap(events) - 1)
		if dropped < 0 {
			dropped = 0
		}
		markerCount += dropped
		events <- seam.Event{Kind: "dropped", Metadata: map[string]any{"count": markerCount}}
		for _, retained := range payload[dropped:] {
			events <- retained
		}
		return
	}

	payload := append(buffered, event)
	if len(payload) <= cap(events) {
		for _, retained := range payload {
			events <- retained
		}
		return
	}
	dropped := len(payload) - (cap(events) - 1)
	events <- seam.Event{Kind: "dropped", Metadata: map[string]any{"count": dropped}}
	for _, retained := range payload[dropped:] {
		events <- retained
	}
}

func subscriberDropCount(events []seam.Event) (int, bool) {
	if len(events) == 0 || events[0].Kind != "dropped" || !events[0].Time.IsZero() || events[0].RuntimeID != "" || events[0].AgentID != "" || events[0].Text != "" {
		return 0, false
	}
	count, ok := events[0].Metadata["count"].(int)
	return count, ok
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
	r.emit(seam.Event{AgentID: agent.ID, AgentTitle: agent.Title, Kind: "inference_request", Metadata: metadata})
}

// emitStatus lets front ends record local control-plane events without
// fabricating a provider turn.
func (r *Runtime) emitStatus(kind, text string) {
	r.emit(seam.Event{Kind: kind, Text: text})
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
// The TUI event is still emitted, and first, so the firing is on the record and
// on screen without that depending on delivery succeeding — in particular when
// the agent has since been stopped. It is a record, not the delivery. Nothing
// is built on it.
//
// The snapshot arrives captured under the job's own lock and is passed through
// unchanged: what the agent is told and what the manager observed are the same
// reading.
func (r *Runtime) deliverJobWarning(snapshot job.Snapshot) {
	r.emit(seam.Event{AgentID: snapshot.Author, Kind: "job_warning", Text: "job is still running", Metadata: map[string]any{"job": snapshot.ID, "warn_after": snapshot.WarnAfter.String()}})
	agent, ok := r.lookupAgent(snapshot.Author)
	if !ok {
		return
	}
	if err := agent.receiveJobWarning(snapshot); err != nil {
		r.emit(seam.Event{AgentID: snapshot.Author, AgentTitle: agent.Title, Kind: "delivery_error", Text: err.Error(), Metadata: map[string]any{"job": snapshot.ID}})
	}
}

func (r *Runtime) deliverJobResult(snapshot job.Snapshot, stdout, stderr string) {
	agent, ok := r.lookupAgent(snapshot.Author)
	if !ok {
		return
	}
	if err := agent.receiveJobResult(snapshot, stdout, stderr); err != nil {
		r.emit(seam.Event{AgentID: snapshot.Author, AgentTitle: agent.Title, Kind: "delivery_error", Text: err.Error(), Metadata: map[string]any{"job": snapshot.ID}})
	}
}

func (r *Runtime) watchOrgRequests() {
	defer close(r.requestWatchDone)
	watchRequestQueue(r.requestWatchStop, r.requestPoll, r.orgStore, func(request orgstore.Request) bool {
		seat, ok := r.lookupAgent(r.seatID)
		if !ok {
			return false
		}
		if err := seat.receiveOrgRequest(request); err != nil {
			r.emit(seam.Event{AgentID: seat.ID, AgentTitle: seat.Title, Kind: "delivery_error", Text: err.Error(), Metadata: map[string]any{"request": request.ID}})
			return false
		}
		return true
	}, func(err error) {
		r.emitStatus("org_requests", err.Error())
	})
}

type requestQueueStore interface {
	Requests() ([]orgstore.Request, error)
	RequestsState() (orgstore.RequestLogState, error)
}

func watchRequestQueue(stop <-chan struct{}, poll time.Duration, store requestQueueStore, handle func(orgstore.Request) bool, reportError func(error)) {
	seen := make(map[uint64]struct{})
	deliver := func() {
		requests, err := store.Requests()
		if err != nil {
			reportError(err)
			return
		}
		for _, request := range requests {
			if request.Status != orgstore.StatusQueued {
				continue
			}
			if _, delivered := seen[request.ID]; delivered {
				continue
			}
			if !handle(request) {
				continue
			}
			seen[request.ID] = struct{}{}
		}
	}

	// Establish the baseline before the first scan. If an external append lands
	// between these operations, the scan sees it now; if it lands after the
	// scan, the next state check differs from this baseline. Scanning first can
	// lose an append that lands before the baseline is sampled forever.
	state, err := store.RequestsState()
	if err != nil {
		reportError(err)
	}
	deliver()
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			next, statErr := store.RequestsState()
			if statErr != nil {
				reportError(statErr)
				continue
			}
			if next.Size == state.Size && next.ModTime.Equal(state.ModTime) {
				continue
			}
			state = next
			deliver()
		}
	}
}

func (r *Runtime) Close() error {
	var err error
	r.closeOnce.Do(func() {
		close(r.requestWatchStop)
		<-r.requestWatchDone
		delivered := make(chan struct{})
		r.queueEvent(seam.Event{Kind: "runtime", Text: "runtime stopping"}, delivered, true)
		// Shutdown is learned from the event stream, not from a second lifecycle
		// channel. Give the dispatcher a bounded opportunity to hand off this
		// marker and every earlier event. If the consumer is not reading, stopping
		// the dispatcher drops the remaining backlog so Close still returns.
		select {
		case <-delivered:
		case <-time.After(250 * time.Millisecond):
		}
		close(r.eventStop)
		<-r.eventDone
		r.eventMu.Lock()
		for id, subscriber := range r.subscribers {
			close(subscriber.events)
			delete(r.subscribers, id)
		}
		r.subscribersClosed = true
		r.eventMu.Unlock()
		close(r.events)
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
