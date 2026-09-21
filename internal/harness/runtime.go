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
	pending            map[string]*agentSession
	sessions           []*agentSession
	redactor           *secretRedactor
	eventMu            sync.Mutex
	eventLog           []seam.Event
	eventNotify        chan struct{}
	eventsClosed       bool
	eventSequence      seam.EventCursor
	provider           func(model string) (provider.Provider, error)
	codexCommand       string
	claudeCommand      string
	catalog            []provider.Catalog
	closeOnce          sync.Once
	subagentWarningsMu sync.Mutex
	subagentWarnings   map[string]*time.Timer
	// toolShape is fixed for the runtime's life: changing the primary tools
	// mid-run would strand every call already in history.
	toolShape string
	// promptVariant is fixed for the same reason.
	promptVariant string
	shape         *shapeState
}

type Options struct {
	Config        config.Config
	Provider      func(model string) (provider.Provider, error)
	CodexCommand  string
	ClaudeCommand string
}

var _ seam.Runtime = (*Runtime)(nil)

func New(cfg config.Config, options Options) (*Runtime, error) {
	if cfg.Home == "" {
		cfg = config.Load()
	}
	toolShape, err := config.NormalizeToolShape(cfg.ToolShape)
	if err != nil {
		return nil, err
	}
	promptVariant, err := config.NormalizePromptVariant(cfg.PromptVariant)
	if err != nil {
		return nil, err
	}
	runtimeID := id.NewShort("run")
	dir := filepath.Join(cfg.Home, "runtimes", runtimeID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	workDir, _ := os.Getwd()
	r := &Runtime{id: runtimeID, runtimeDir: dir, workDir: workDir, config: cfg, ctx: ctx, cancel: cancel, agents: make(map[string]*Agent), current: make(map[string]*agentSession), pending: make(map[string]*agentSession), redactor: newSecretRedactor(os.Environ()), eventNotify: make(chan struct{}), provider: options.Provider, codexCommand: options.CodexCommand, claudeCommand: options.ClaudeCommand, subagentWarnings: make(map[string]*time.Timer), toolShape: toolShape, promptVariant: promptVariant, shape: newShapeState()}
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
	seatEffort := cfg.SeatEffort
	if routeEffort := routeDefaultEffort(cfg.Policy, seatModel); routeEffort != "" {
		seatEffort = routeEffort
	}
	seat, err := r.newAgent("seat", "", 0, seatModel, seatEffort)
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
	r.emit(seam.Event{AgentID: seat.ID, AgentTitle: seat.Title, Kind: "runtime", Text: "runtime started", Metadata: map[string]any{"tool_shape": r.toolShape, "prompt_variant": r.promptVariant}})
	return r, nil
}

func (r *Runtime) ID() string   { return r.id }
func (r *Runtime) Dir() string  { return r.runtimeDir }
func (r *Runtime) Home() string { return r.config.Home }

// Quiescent reports whether nothing in the runtime is running or pending:
// every job has finished and been delivered, and no agent has a turn in
// progress, an undelivered message, or a non-idle status. Jobs are checked
// before agents, so a job that finished before the check has already put its
// result in an inbox the agent check will see. Agents are checked one at a
// time, so a caller that must be certain asks twice, apart, and requires both
// answers true with no event between them.
func (r *Runtime) Quiescent() bool {
	for _, snapshot := range r.jobs.List() {
		j, ok := r.jobs.Get(snapshot.ID)
		if !ok {
			continue
		}
		select {
		case <-j.Done():
		default:
			return false
		}
	}
	for _, agent := range r.Agents() {
		a, ok := r.lookupAgent(agent.ID)
		if !ok {
			continue
		}
		a.mu.RLock()
		busy := a.busy || len(a.inbox) > 0 || a.turnCancel != nil
		status := a.status
		a.mu.RUnlock()
		if busy || (status != "idle" && status != "stopped" && status != "error") {
			return false
		}
	}
	return true
}

// LastEventCursor is the cursor of the most recent event emitted.
func (r *Runtime) LastEventCursor() seam.EventCursor {
	r.eventMu.Lock()
	defer r.eventMu.Unlock()
	return r.eventSequence
}

// ToolShape is the runtime's primary tool shape.
func (r *Runtime) ToolShape() string { return r.toolShape }

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
		if routeEffort := routeDefaultEffort(cfg.Policy, model); routeEffort != "" {
			seat.SetEffort(routeEffort)
		}
	}
	return nil
}

// routeDefaultEffort is the policy's defaultEffort for the route a model
// addresses, or empty when the route has none.
func routeDefaultEffort(policy provider.Policy, model string) string {
	key, ok := provider.RouteKey(model)
	if !ok {
		return ""
	}
	route, found := policy.Route(key)
	if !found {
		return ""
	}
	return strings.TrimSpace(route.DefaultEffort)
}

// LayerInstructions returns the managed depth document for an agent at the
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
	defaults := fmt.Sprintf("configured application defaults are seat=%q, subagent=%q, leaf=%q", cfg.SeatModel, cfg.SubagentModel, cfg.LeafModel)
	return "Model guidance: approved native models are " + approved + ". " + defaults + ". Available provider models: " + strings.Join(branches, "; ") + ". Child launches are depth-limited to two levels and do not use named roles. A depth-1 child may use the configured child default; every depth-2 launch must pass a non-empty model or short name explicitly. The optional harness selects native, Codex, or Claude Code; omission selects native. Per-launch effort is honored; omission uses the model route's defaultEffort, then the managed model alias's effort, then the configured subagent effort. Configured subagent and leaf defaults are never substituted for an explicit deeper-child model."
}

// canLaunch is the runtime's only delegation topology: a native agent may
// launch one child at each of the first two depths. No external identity is
// consulted or required.
func (r *Runtime) canLaunch(agent *Agent) bool {
	return agent != nil && agent.Harness == "native" && agent.Depth < 2
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
	if err := os.MkdirAll(filepath.Join(r.runtimeDir, "agents", agent.ID, "scratch"), 0o700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(r.runtimeDir, "agents", agent.ID, "jobs"), 0o700); err != nil {
		return nil, err
	}
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
	if spec.WarnAfterSeconds < 0 {
		return nil, fmt.Errorf("launch_subagent.warn_after_seconds must not be negative")
	}
	warnAfterSeconds := spec.WarnAfterSeconds
	if warnAfterSeconds == 0 {
		warnAfterSeconds = defaultSubagentWarnAfterSeconds
	}
	r.mu.RLock()
	cfg := r.config
	r.mu.RUnlock()
	childDepth := parent.Depth + 1
	harness := strings.TrimSpace(spec.Harness)
	if harness == "" {
		harness = "native"
	} else if _, err := launchHarness(harness); err != nil {
		return nil, err
	}
	model, effort := strings.TrimSpace(spec.Model), strings.TrimSpace(spec.Effort)
	if childDepth == 2 && model == "" {
		return nil, fmt.Errorf("a depth-1 child launching depth 2 must pass a non-empty explicit model or short name in launch_subagent.model; configured defaults are not used for deeper children")
	}
	aliasEffort := ""
	if model == "" {
		model = cfg.SubagentModel
	}
	model, aliasEffort = cfg.Models.Resolve(model)
	if effort == "" {
		r.mu.RLock()
		effort = routeDefaultEffort(r.config.Policy, model)
		r.mu.RUnlock()
	}
	if effort == "" {
		effort = aliasEffort
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
	agent, err := r.newAgent(spec.Title, parentID, childDepth, model, effort)
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
	r.emit(seam.Event{AgentID: agent.ID, AgentTitle: spec.Title, Kind: "status", Text: "subagent launched", Metadata: map[string]any{"parent": parentID, "depth": agent.Depth, "harness": agent.Harness, "model": agent.Model, "effort": agent.Effort, "working_dir": agent.WorkDir}})
	if spec.Brief != "" {
		r.armSubagentWarning(agent.ID, parentID, time.Duration(warnAfterSeconds)*time.Second)
		if err := agent.Send(spec.Brief); err != nil {
			agent.stop()
			r.discardAgent(agent.ID)
			return nil, err
		}
	}
	return agent, nil
}

func launchHarness(harness string) (string, error) {
	switch harness {
	case "native":
		return harness, nil
	case "codex", "claude_code":
		return harness, nil
	default:
		return "", fmt.Errorf("unsupported child harness %q", harness)
	}
}

func (r *Runtime) discardAgent(agentID string) {
	r.cancelSubagentWarning(agentID)
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

const defaultSubagentWarnAfterSeconds = 5

func (r *Runtime) armSubagentWarning(childID, parentID string, after time.Duration) {
	r.subagentWarningsMu.Lock()
	if previous := r.subagentWarnings[childID]; previous != nil {
		previous.Stop()
	}
	var timer *time.Timer
	timer = time.AfterFunc(after, func() {
		r.subagentWarningsMu.Lock()
		if current := r.subagentWarnings[childID]; current != timer {
			r.subagentWarningsMu.Unlock()
			return
		}
		delete(r.subagentWarnings, childID)
		r.subagentWarningsMu.Unlock()
		if r.ctx.Err() != nil {
			return
		}
		child, childOK := r.lookupAgent(childID)
		parent, parentOK := r.lookupAgent(parentID)
		if !childOK || !parentOK {
			return
		}
		if err := parent.receiveSubagentWarning(child, after); err != nil {
			r.emit(seam.Event{AgentID: child.ID, AgentTitle: child.Title, Kind: "delivery_error", Text: err.Error(), Metadata: map[string]any{"parent": parent.ID, "warning": "subagent"}})
		}
	})
	r.subagentWarnings[childID] = timer
	r.subagentWarningsMu.Unlock()
}

func (r *Runtime) cancelSubagentWarning(childID string) {
	r.subagentWarningsMu.Lock()
	if timer := r.subagentWarnings[childID]; timer != nil {
		timer.Stop()
		delete(r.subagentWarnings, childID)
	}
	r.subagentWarningsMu.Unlock()
}

func (r *Runtime) cancelAllSubagentWarnings() {
	r.subagentWarningsMu.Lock()
	for childID, timer := range r.subagentWarnings {
		timer.Stop()
		delete(r.subagentWarnings, childID)
	}
	r.subagentWarningsMu.Unlock()
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
	// ClearHistory cancels an active native turn, and the agent promotes the
	// pending session when that cancellation reaches its turn boundary. Codex
	// and Claude Code promote from their harness-specific reset paths. An idle
	// agent has no turn in flight, so its boundary is now.
	idle := agent.codexBackend() == nil && agent.claudeBackend() == nil && !agent.hasActiveTurn()
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
	r.queueEvent(event, false)
}

func (r *Runtime) queueEvent(event seam.Event, closeEvents bool) {
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
	if event.Kind == "turn_done" || event.Kind == "error" {
		r.cancelSubagentWarning(event.AgentID)
	}
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
	event.Cursor = r.eventSequence
	event = serializableEvent(event)
	r.eventLog = append(r.eventLog, event)
	close(r.eventNotify)
	r.eventNotify = make(chan struct{})
	r.eventMu.Unlock()
}

// PollEvents implements the seam's cursor-based stream. The runtime retains
// the ordered log for its lifetime; a slow consumer therefore delays nobody
// and loses nothing. eventNotify is private wake-up machinery only and never
// crosses the seam.
func (r *Runtime) PollEvents(query seam.EventQuery) seam.EventBatch {
	wait := time.Duration(query.WaitMilliseconds) * time.Millisecond
	if wait < 0 {
		wait = 0
	}
	deadline := time.Now().Add(wait)
	for {
		r.eventMu.Lock()
		start := sort.Search(len(r.eventLog), func(i int) bool {
			return r.eventLog[i].Cursor > query.After
		})
		end := len(r.eventLog)
		if query.Limit > 0 && start+query.Limit < end {
			end = start + query.Limit
		}
		batch := seam.EventBatch{Cursor: query.After}
		if start < end {
			batch.Events = make([]seam.Event, end-start)
			for i, event := range r.eventLog[start:end] {
				batch.Events[i] = serializableEvent(event)
			}
			batch.Cursor = batch.Events[len(batch.Events)-1].Cursor
		}
		batch.End = r.eventsClosed && end == len(r.eventLog)
		notify := r.eventNotify
		r.eventMu.Unlock()
		if len(batch.Events) > 0 || batch.End || wait == 0 {
			return batch
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return batch
		}
		timer := time.NewTimer(remaining)
		select {
		case <-notify:
			if !timer.Stop() {
				<-timer.C
			}
		case <-timer.C:
			return batch
		}
	}
}

// serializableEvent makes the event independent of its producer and confines
// metadata to JSON values. An invalid metadata value is replaced with a
// serializable diagnostic instead of allowing a channel, function, or live
// pointer to cross the seam.
func serializableEvent(event seam.Event) seam.Event {
	encoded, err := json.Marshal(event)
	if err != nil {
		event.Metadata = map[string]any{"serialization_error": err.Error()}
		encoded, _ = json.Marshal(event)
	}
	var copied seam.Event
	if err := json.Unmarshal(encoded, &copied); err != nil {
		return seam.Event{
			Cursor:    event.Cursor,
			Time:      event.Time,
			RuntimeID: event.RuntimeID,
			AgentID:   event.AgentID,
			Kind:      "serialization_error",
			Text:      err.Error(),
		}
	}
	return copied
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

func (r *Runtime) Close() error {
	var err error
	r.closeOnce.Do(func() {
		r.queueEvent(seam.Event{Kind: "runtime", Text: "runtime stopping"}, true)
		r.cancel()
		r.cancelAllSubagentWarnings()
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
