package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/slbdotdev/slbh/internal/config"
	"github.com/slbdotdev/slbh/internal/job"
	"github.com/slbdotdev/slbh/internal/orgstore"
	"github.com/slbdotdev/slbh/internal/provider"
	"github.com/slbdotdev/slbh/internal/seam"
)

const (
	compactAtNumerator   = 7
	compactAtDenominator = 10
)

type agentMessage struct {
	prompt   string
	kind     string
	text     string
	metadata map[string]any
	// senderTitle identifies the agent that authored a forwarded message.
	// AgentID on the resulting event remains the recipient so the UI can show
	// the message in the recipient's viewport.
	senderTitle string
}

type Agent struct {
	runtime  *Runtime
	ID       string
	Title    string
	Role     string
	ParentID string
	Depth    int
	Model    string
	Effort   string
	Harness  string
	WorkDir  string

	mu            sync.RWMutex
	status        string
	history       []provider.Message
	inbox         []agentMessage
	wake          chan struct{}
	stopped       bool
	cancel        context.CancelFunc
	done          chan struct{}
	stopOnce      sync.Once
	contextModel  string
	contextWindow int
	contextUsed   int
	cacheHits     int
	cacheMisses   int
	historyEpoch  uint64
	codexMu       sync.RWMutex
	codex         *codexLeaf
	claudeMu      sync.RWMutex
	claude        *claudeLeaf
}

func newAgent(runtime *Runtime, agentID, title, role, parentID string, depth int, model, effort string) *Agent {
	return &Agent{runtime: runtime, ID: agentID, Title: title, Role: role, ParentID: parentID, Depth: depth, Model: model, Effort: effort, Harness: "native", WorkDir: runtime.workDir, status: "idle", wake: make(chan struct{}, 1), done: make(chan struct{})}
}

func (a *Agent) start() {
	a.startNative()
}

func (a *Agent) startNative() {
	ctx, cancel := context.WithCancel(a.runtime.ctx)
	a.cancel = cancel
	go a.loop(ctx)
}

func (a *Agent) Send(prompt string) error {
	if codex := a.codexBackend(); codex != nil {
		return codex.send(prompt, "user")
	}
	if claude := a.claudeBackend(); claude != nil {
		return claude.send(prompt, "user")
	}
	return a.deliver(agentMessage{prompt: prompt, kind: "user", text: prompt})
}

// Steer uses the same inbox as every other message, including child results.
// A busy agent consumes it in the active turn; an idle agent wakes immediately.
func (a *Agent) Steer(message string) error {
	if strings.TrimSpace(message) == "" {
		return fmt.Errorf("message is empty")
	}
	if codex := a.codexBackend(); codex != nil {
		return codex.send(message, "steer")
	}
	if claude := a.claudeBackend(); claude != nil {
		return claude.send(message, "steer")
	}
	return a.deliver(agentMessage{prompt: "[steer] " + message, kind: "steer", text: message})
}

// steerFrom delivers an explicit agent-to-agent message while retaining its
// sender for the event stream. The recipient still owns the inbox and event
// routing, but the sender is the author shown by the UI.
func (a *Agent) steerFrom(sender *Agent, message string) error {
	if sender == nil {
		return fmt.Errorf("sender agent is required")
	}
	if strings.TrimSpace(message) == "" {
		return fmt.Errorf("message is empty")
	}
	if codex := a.codexBackend(); codex != nil {
		return codex.send(fmt.Sprintf("[steer] [from %s (%s)] %s", sender.Title, sender.ID, message), "steer")
	}
	if claude := a.claudeBackend(); claude != nil {
		return claude.send(fmt.Sprintf("[steer] [from %s (%s)] %s", sender.Title, sender.ID, message), "steer")
	}
	return a.deliver(agentMessage{
		prompt:      fmt.Sprintf("[steer] [from %s (%s)] %s", sender.Title, sender.ID, message),
		kind:        "steer",
		text:        message,
		metadata:    map[string]any{"sender": sender.ID},
		senderTitle: sender.Title,
	})
}

func (a *Agent) deliver(message agentMessage) error {
	if strings.TrimSpace(message.prompt) == "" {
		return fmt.Errorf("message is empty")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stopped || a.runtime.ctx.Err() != nil {
		return fmt.Errorf("agent %q is stopped", a.ID)
	}
	// The mutex defines FIFO acceptance order. The wake channel is only a
	// notification: it never carries messages and cannot drop or block them.
	a.inbox = append(a.inbox, message)
	select {
	case a.wake <- struct{}{}:
	default:
	}
	return nil
}

func (a *Agent) takeMessages() []agentMessage {
	a.mu.Lock()
	defer a.mu.Unlock()
	messages := a.inbox
	a.inbox = nil
	return messages
}

func (a *Agent) appendMessages(history []provider.Message, messages []agentMessage) []provider.Message {
	for _, message := range messages {
		history = append(history, provider.Message{Role: "user", Content: message.prompt})
		title := a.Title
		if message.senderTitle != "" {
			title = message.senderTitle
		}
		a.runtime.emit(seam.Event{AgentID: a.ID, AgentTitle: title, Kind: message.kind, Text: message.text, Metadata: message.metadata})
	}
	return history
}

func (a *Agent) Snapshot() seam.AgentSnapshot {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return seam.AgentSnapshot{
		ID:              a.ID,
		Title:           a.Title,
		Role:            a.Role,
		ParentID:        a.ParentID,
		Depth:           a.Depth,
		Model:           a.Model,
		Effort:          a.Effort,
		Status:          a.status,
		Harness:         a.Harness,
		WorkDir:         a.WorkDir,
		ContextWindow:   a.contextWindow,
		ContextUsed:     a.contextUsed,
		CacheHitTokens:  a.cacheHits,
		CacheMissTokens: a.cacheMisses,
	}
}

func (a *Agent) SetModel(model string) {
	a.mu.Lock()
	a.Model = strings.TrimSpace(model)
	a.contextModel = ""
	a.contextWindow = 0
	a.mu.Unlock()
}

func (a *Agent) SetEffort(effort string) {
	a.mu.Lock()
	a.Effort = strings.TrimSpace(effort)
	a.mu.Unlock()
}

func (a *Agent) History() []provider.Message {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return append([]provider.Message(nil), a.history...)
}

// ClearHistory starts the next turn with no conversation messages. A running
// provider request keeps its local snapshot, but its result cannot restore the
// history that was cleared while it was in flight.
func (a *Agent) ClearHistory() {
	if codex := a.codexBackend(); codex != nil {
		a.mu.Lock()
		a.history = nil
		a.contextUsed = 0
		a.historyEpoch++
		a.mu.Unlock()
		codex.clear()
		return
	}
	if claude := a.claudeBackend(); claude != nil {
		a.mu.Lock()
		a.history = nil
		a.contextUsed = 0
		a.historyEpoch++
		a.mu.Unlock()
		claude.clear()
		return
	}
	a.mu.Lock()
	a.history = nil
	a.contextUsed = 0
	a.historyEpoch++
	a.mu.Unlock()
}

func (a *Agent) stop() {
	a.stopOnce.Do(func() {
		a.runtime.cancelSubagentWarning(a.ID)
		if codex := a.codexBackend(); codex != nil {
			codex.stop()
			return
		}
		if claude := a.claudeBackend(); claude != nil {
			claude.stop()
			return
		}
		a.mu.Lock()
		a.stopped = true
		a.mu.Unlock()
		if a.cancel != nil {
			a.cancel()
		}
		select {
		case <-a.done:
		case <-time.After(2 * time.Second):
		}
	})
}

func (a *Agent) codexBackend() *codexLeaf {
	a.codexMu.RLock()
	defer a.codexMu.RUnlock()
	return a.codex
}

func (a *Agent) claudeBackend() *claudeLeaf {
	a.claudeMu.RLock()
	defer a.claudeMu.RUnlock()
	return a.claude
}

func (a *Agent) loop(ctx context.Context) {
	defer close(a.done)
	for {
		select {
		case <-ctx.Done():
			a.setStatus("stopped")
			return
		case <-a.wake:
			if messages := a.takeMessages(); len(messages) > 0 {
				a.handle(ctx, messages)
			}
		}
	}
}

func (a *Agent) handle(ctx context.Context, messages []agentMessage) {
	a.setStatus("thinking")
	a.mu.Lock()
	epoch := a.historyEpoch
	history := append([]provider.Message(nil), a.history...)
	a.mu.Unlock()
	history = a.appendMessages(history, messages)
	// Preserve consumed input even when provider setup or inference fails.
	defer func() {
		a.mu.Lock()
		if a.historyEpoch == epoch {
			a.history = history
		}
		a.mu.Unlock()
	}()
	a.mu.RLock()
	model := a.Model
	effort := a.Effort
	a.mu.RUnlock()
	if strings.TrimSpace(model) == "" {
		a.fail(fmt.Errorf("no model selected; configure an approved model with /models or honor an explicit user model request"))
		return
	}
	p, err := a.runtime.provider(model)
	if err != nil {
		a.fail(err)
		return
	}
	contextWindow := a.resolveContextWindow(ctx, p)
	system := systemPrompt(a)
	tools := a.runtime.toolDefinitions(a.ID)
	for round := 0; ; {
		if ctx.Err() != nil {
			return
		}
		history = a.appendMessages(history, a.takeMessages())
		history = a.compactHistoryIfNeeded(history, contextWindow, system, tools)
		if round >= 100 {
			a.fail(fmt.Errorf("provider/tool round limit reached"))
			return
		}
		var answer strings.Builder
		var reasoning strings.Builder
		calls := make(map[int]*provider.ToolCall)
		err = provider.Retry(ctx, 3, func() error {
			// A failed API attempt is also a call boundary. Retain partial prose
			// and accept new input before retrying; incomplete tool fragments stay
			// in the transcript and cannot be executed as successful calls.
			if answer.Len() > 0 || reasoning.Len() > 0 {
				content := answer.String()
				if content != "" {
					content += "\n"
				}
				content += "[API attempt failed before completion]"
				history = append(history, provider.Message{Role: "assistant", Content: content, ReasoningContent: reasoning.String()})
			}
			answer.Reset()
			reasoning.Reset()
			calls = make(map[int]*provider.ToolCall)
			history = a.appendMessages(history, a.takeMessages())
			req := provider.Request{Model: model, Effort: effort, System: system, Messages: history, Tools: tools, CacheKey: provider.StablePrefixKey(provider.Request{Model: model, System: system, Tools: tools})}
			a.recordRequestContext(req, contextWindow)
			a.runtime.recordInferenceRequest(a, round, req, p)
			return p.Stream(ctx, req, func(event provider.Event) error {
				switch event.Kind {
				case provider.EventText:
					answer.WriteString(event.Text)
					a.runtime.emit(seam.Event{AgentID: a.ID, AgentTitle: a.Title, Kind: "assistant", Text: event.Text})
				case provider.EventReasoning:
					reasoning.WriteString(event.Text)
					a.runtime.emit(seam.Event{AgentID: a.ID, AgentTitle: a.Title, Kind: "thinking", Text: event.Text})
				case provider.EventTool:
					call := calls[event.ToolIndex]
					if call == nil {
						call = &provider.ToolCall{ID: event.ToolCallID, Type: "function"}
						calls[event.ToolIndex] = call
					}
					if event.ToolCallID != "" {
						call.ID = event.ToolCallID
					}
					if event.ToolName != "" {
						call.Function.Name = event.ToolName
					}
					call.Function.Arguments += event.Input
					a.runtime.emit(seam.Event{AgentID: a.ID, AgentTitle: a.Title, Kind: "tool", Text: event.Input, Metadata: map[string]any{"name": event.ToolName, "call_id": event.ToolCallID, "index": event.ToolIndex}})
				case provider.EventUsage:
					a.recordUsage(event.Usage)
					a.runtime.emit(seam.Event{AgentID: a.ID, AgentTitle: a.Title, Kind: "usage", Metadata: event.Usage})
				}
				return nil
			})
		})
		if ctx.Err() != nil {
			return
		}
		// Paid-for output is retained exactly once, even when new messages
		// arrived during this request. Delivery never cancels or restarts it.
		responseContent := answer.String()
		responseReasoning := reasoning.String()
		if err != nil {
			if responseContent != "" || responseReasoning != "" {
				history = append(history, provider.Message{Role: "assistant", Content: responseContent, ReasoningContent: responseReasoning})
			}
			history = a.appendMessages(history, a.takeMessages())
			a.fail(err)
			return
		}
		if len(calls) == 0 {
			if responseContent != "" || responseReasoning != "" {
				history = append(history, provider.Message{Role: "assistant", Content: responseContent, ReasoningContent: responseReasoning})
			}
			// Finish and message acceptance share one lock. Input accepted before
			// this point must be consumed in THIS turn, even after stream EOF.
			a.mu.Lock()
			if len(a.inbox) > 0 {
				a.mu.Unlock()
				continue
			}
			if a.historyEpoch == epoch {
				a.history = history
			}
			a.status = "idle"
			a.mu.Unlock()
			a.runtime.emit(seam.Event{AgentID: a.ID, AgentTitle: a.Title, Kind: "status", Text: "idle"})
			if a.ParentID != "" && answer.Len() > 0 {
				if parent, ok := a.runtime.lookupAgent(a.ParentID); ok {
					if err := parent.receiveChildResult(a, answer.String()); err != nil {
						a.runtime.emit(seam.Event{AgentID: a.ID, AgentTitle: a.Title, Kind: "delivery_error", Text: err.Error()})
					}
				}
			}
			a.runtime.emit(seam.Event{AgentID: a.ID, AgentTitle: a.Title, Kind: "turn_done"})
			return
		}
		ordered := make([]int, 0, len(calls))
		for index := range calls {
			ordered = append(ordered, index)
		}
		for i := 0; i < len(ordered); i++ {
			for j := i + 1; j < len(ordered); j++ {
				if ordered[j] < ordered[i] {
					ordered[i], ordered[j] = ordered[j], ordered[i]
				}
			}
		}
		for callNumber, index := range ordered {
			if ctx.Err() != nil {
				return
			}
			if callNumber > 0 {
				history = a.appendMessages(history, a.takeMessages())
			}
			call := calls[index]
			// Represent a returned batch as ordered call/result pairs. This
			// permits messages at EVERY tool boundary without orphaning a tool
			// result or inserting user input inside an unresolved tool batch.
			// Every already-produced tool call still executes exactly once.
			message := provider.Message{Role: "assistant", Content: "", ReasoningContent: responseReasoning, ToolCalls: []provider.ToolCall{*call}}
			if callNumber == 0 {
				message.Content = responseContent
				message.ReasoningContent = responseReasoning
			}
			history = append(history, message)
			result, toolErr := a.runtime.ExecuteTool(a.ID, call.Function.Name, call.Function.Arguments)
			if toolErr != nil {
				result = toolFailure(toolErr, result)
			}
			// Before it is emitted and before it enters history. A tool runs
			// with the parent environment by design, so `env` — or any script
			// with `set -x` — puts a provider key on stdout; from history it
			// would be marshalled into the next request, reaching both the
			// transcript and the provider itself.
			result = a.runtime.redactSecrets(result)
			a.runtime.emit(seam.Event{AgentID: a.ID, AgentTitle: a.Title, Kind: "tool_result", Text: result, Metadata: map[string]any{"name": call.Function.Name, "call_id": call.ID}})
			history = append(history, provider.Message{Role: "tool", ToolCallID: call.ID, Name: call.Function.Name, Content: result})
			history = a.appendMessages(history, a.takeMessages())
		}
		round++
	}
}

func (a *Agent) fail(err error) {
	a.setStatus("error")
	a.runtime.emit(seam.Event{AgentID: a.ID, AgentTitle: a.Title, Kind: "error", Text: err.Error()})
}

func (a *Agent) setStatus(status string) {
	a.mu.Lock()
	a.status = status
	a.mu.Unlock()
	a.runtime.emit(seam.Event{AgentID: a.ID, AgentTitle: a.Title, Kind: "status", Text: status})
}

func (a *Agent) recordRequestContext(req provider.Request, contextWindow int) {
	encoded, err := provider.ContextPayload(req)
	if err != nil {
		return
	}
	a.mu.Lock()
	a.contextWindow = contextWindow
	a.contextUsed = (len(encoded) + 3) / 4
	a.mu.Unlock()
}

func (a *Agent) recordUsage(usage map[string]any) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if promptTokens, ok := usageInt(usage, "prompt_tokens"); ok {
		a.contextUsed = promptTokens
	}
	hit, hitOK := usageInt(usage, "prompt_cache_hit_tokens")
	miss, missOK := usageInt(usage, "prompt_cache_miss_tokens")
	if !hitOK {
		hit, hitOK = usageNestedInt(usage, "prompt_tokens_details", "cached_tokens")
	}
	if hitOK {
		a.cacheHits += hit
	}
	if !missOK && hitOK {
		if promptTokens, ok := usageInt(usage, "prompt_tokens"); ok && promptTokens >= hit {
			miss = promptTokens - hit
			missOK = true
		}
	}
	if missOK {
		a.cacheMisses += miss
	}
}

func usageInt(usage map[string]any, key string) (int, bool) {
	value, ok := usage[key]
	if !ok {
		return 0, false
	}
	switch number := value.(type) {
	case float64:
		return int(number), true
	case float32:
		return int(number), true
	case int:
		return number, true
	case int64:
		return int(number), true
	case json.Number:
		parsed, err := number.Int64()
		return int(parsed), err == nil
	default:
		return 0, false
	}
}

func usageNestedInt(usage map[string]any, parent, key string) (int, bool) {
	nested, ok := usage[parent].(map[string]any)
	if !ok {
		return 0, false
	}
	return usageInt(nested, key)
}

func (a *Agent) receiveChildResult(child *Agent, text string) error {
	a.runtime.cancelSubagentWarning(child.ID)
	return a.deliver(agentMessage{prompt: fmt.Sprintf("[result from %s] %s", child.Title, text), kind: "child_result", text: text, metadata: map[string]any{"child": child.ID}, senderTitle: child.Title})
}

func (a *Agent) receiveSubagentWarning(child *Agent, after time.Duration) error {
	text := fmt.Sprintf("%s %s is still running after %s. This is the only warning for this child: inspect it with list_subagents, message it with msg_subagent, end it with end_subagent, or continue other work and wait for its result.", child.Title, child.ID, after)
	return a.deliver(agentMessage{
		prompt:      "[warning from subagent " + child.Title + "]\n" + text,
		kind:        "subagent_warning",
		text:        text,
		metadata:    map[string]any{"child": child.ID, "warn_after": after.String()},
		senderTitle: child.Title,
	})
}

func (a *Agent) receiveOrgRequest(request orgstore.Request) error {
	text := fmt.Sprintf("[request %d from Secretary]\n%s\n\nThis is a proposal to be judged against the tree before dispatching; only the owner's word is an order.", request.ID, request.Text)
	return a.deliver(agentMessage{
		prompt:   text,
		kind:     "org_request",
		text:     text,
		metadata: map[string]any{"request": request.ID},
	})
}

func (a *Agent) receiveJobResult(snapshot job.Snapshot, stdout, stderr string) error {
	// Same reason as the foreground tools: a job's captured output reaches
	// history through deliver(), and from there the next request payload.
	text := a.runtime.redactSecrets(formatJobResult(snapshot, stdout, stderr))
	toolName := snapshot.ToolName
	if toolName == "" {
		toolName = "long_job"
	}
	return a.deliver(agentMessage{
		prompt:   fmt.Sprintf("[result from %s %s]\n%s", toolName, snapshot.ID, text),
		kind:     "job_result",
		text:     text,
		metadata: map[string]any{"job": snapshot.ID, "tool": toolName, "status": string(snapshot.Status), "exit_code": snapshot.ExitCode},
	})
}

// receiveJobWarning puts a job's one warning into the agent's own context.
//
// It is deliberately the same mechanism as receiveJobResult: deliver() appends
// under the agent mutex and pokes the buffered wake channel, so the message is
// consumed at the next API/tool call boundary of a busy agent and wakes an idle
// one immediately. A warning that only reached the event stream reached nobody
// who could act on it.
//
// The message is a decision point and says so. It names the three things the
// agent can do about the job and points at read_job, which since 2026-09-15
// answers a running job with what it has captured so far — the evidence the
// decision wants. Until then the text said the opposite, because the buffers
// really were unreadable mid-run and the call would have come back empty.
func (a *Agent) receiveJobWarning(snapshot job.Snapshot) error {
	toolName := snapshot.ToolName
	if toolName == "" {
		toolName = "long_job"
	}
	// Same reason as a job result: the script is the agent's own text and can
	// name a credential, and from history it would be marshalled into the next
	// request payload.
	text := a.runtime.redactSecrets(formatJobWarning(snapshot, toolName))
	return a.deliver(agentMessage{
		prompt:   fmt.Sprintf("[warning from %s %s]\n%s", toolName, snapshot.ID, text),
		kind:     "job_warning",
		text:     text,
		metadata: map[string]any{"job": snapshot.ID, "tool": toolName, "status": string(snapshot.Status), "warn_after": snapshot.WarnAfter.String()},
	})
}

// maxJobWarningScript bounds the script echoed back in a warning. It is there
// to identify which job this is, not to reproduce the job; an agent that wrote
// a long here-doc does not need it quoted back at the cost of its context.
const maxJobWarningScript = 400

func formatJobWarning(snapshot job.Snapshot, toolName string) string {
	script := snapshot.Script
	if len(script) > maxJobWarningScript {
		script = script[:maxJobWarningScript] + "\n[script truncated]"
	}
	return fmt.Sprintf("%s %s is still running after %s.\nscript:\n%s\n\nThat is its state as of when this warning was raised; if the job's result has already reached you, the result is the truth and this warning is stale. This is the only warning you get for this job: nothing will send it again and nothing will act for you. Decide now, and you may decide to do nothing. read_job returns what this job has captured so far, so you can look at its output before deciding. Kill it with kill_job if it is stuck or no longer worth waiting for; otherwise leave it and its captured output will be delivered to you automatically when it finishes, or carry on with other work in the meantime.",
		toolName, snapshot.ID, snapshot.WarnAfter, script)
}

func formatJobResult(snapshot job.Snapshot, stdout, stderr string) string {
	return fmt.Sprintf("status: %s\nexit_code: %d\nstdout:\n%s\nstderr:\n%s", snapshot.Status, snapshot.ExitCode, stdout, stderr)
}

// Compact keeps the most recent work and leaves a durable marker in the
// transcript. It is intentionally deterministic and local: a provider outage
// must never make compaction block the agent.
func (a *Agent) Compact(keep int) int {
	if codex := a.codexBackend(); codex != nil {
		codex.compact()
		return 0
	}
	if claude := a.claudeBackend(); claude != nil {
		claude.compact()
		return 0
	}
	if keep < 4 {
		keep = 4
	}
	a.mu.Lock()
	compacted, dropped := compactMessages(a.history, keep)
	if dropped == 0 {
		a.mu.Unlock()
		return 0
	}
	a.history = compacted
	a.mu.Unlock()
	a.runtime.emit(seam.Event{AgentID: a.ID, AgentTitle: a.Title, Kind: "compact", Text: fmt.Sprintf("compacted %d earlier messages", dropped)})
	return dropped
}

func (a *Agent) maybeCompact(contextWindow int, system string, tools []provider.Tool) {
	a.mu.RLock()
	history := append([]provider.Message(nil), a.history...)
	a.mu.RUnlock()
	compacted, dropped := compactHistory(history, contextWindow, system, tools, 24)
	if dropped == 0 {
		return
	}
	a.mu.Lock()
	a.history = compacted
	a.mu.Unlock()
	a.runtime.emit(seam.Event{AgentID: a.ID, AgentTitle: a.Title, Kind: "compact", Text: fmt.Sprintf("compacted %d earlier messages", dropped)})
}

func (a *Agent) compactHistoryIfNeeded(history []provider.Message, contextWindow int, system string, tools []provider.Tool) []provider.Message {
	compacted, dropped := compactHistory(history, contextWindow, system, tools, 24)
	if dropped > 0 {
		a.runtime.emit(seam.Event{AgentID: a.ID, AgentTitle: a.Title, Kind: "compact", Text: fmt.Sprintf("compacted %d earlier messages", dropped)})
		return compacted
	}
	return history
}

func compactHistory(history []provider.Message, contextWindow int, system string, tools []provider.Tool, keep int) ([]provider.Message, int) {
	if !contextLimitReached(contextWindow, system, history, tools) {
		return history, 0
	}
	return compactMessages(history, keep)
}

func contextBudget(contextWindow int) int {
	return contextWindow * compactAtNumerator / compactAtDenominator
}

func contextLimitReached(contextWindow int, system string, history []provider.Message, tools []provider.Tool) bool {
	encoded, err := json.Marshal(struct {
		System   string             `json:"system"`
		Messages []provider.Message `json:"messages"`
		Tools    []provider.Tool    `json:"tools"`
	}{System: system, Messages: history, Tools: tools})
	if err != nil {
		return false
	}
	estimatedTokens := (len(encoded) + 3) / 4
	return estimatedTokens >= contextBudget(contextWindow)
}

func (a *Agent) resolveContextWindow(ctx context.Context, p provider.Provider) int {
	a.mu.RLock()
	model := a.Model
	if a.contextModel == model && a.contextWindow > 0 {
		window := a.contextWindow
		a.mu.RUnlock()
		return window
	}
	a.mu.RUnlock()

	// A pinned route window is consulted first and wins outright. It beats
	// discovery as well as the fallback: a route carries a pin precisely
	// because its catalog cannot answer, so asking the catalog first would
	// either fail and waste a round trip or return a figure the pin exists to
	// override. A pin that nothing consults changes nothing, which is why this
	// resolution path is the substance of the change and the table is not.
	//
	// The pin comes from the provider instance, which carries its own route's
	// policy, and never from a lookup by model name here. Only the instance
	// knows the endpoint the request will really reach: under an explicit
	// endpoint override the same model name goes to OpenRouter instead of the
	// plan, and a name-keyed pin would size the window for a route this
	// request is not taking.
	window, pinned := 0, false
	if routed, ok := p.(provider.RoutePolicyProvider); ok {
		window, pinned = routed.PinnedContextWindow()
	}
	if !pinned {
		window = provider.FallbackContextWindow
		if metadata, ok := p.(provider.ContextWindowProvider); ok {
			metadataCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			if discovered, err := metadata.ContextWindow(metadataCtx, model); err == nil && discovered > 0 {
				window = discovered
			}
			cancel()
		}
	}
	a.mu.Lock()
	a.contextModel = model
	a.contextWindow = window
	a.mu.Unlock()
	return window
}

func compactMessages(history []provider.Message, keep int) ([]provider.Message, int) {
	if keep < 4 {
		keep = 4
	}
	if len(history) <= keep {
		return history, 0
	}
	start := len(history) - keep
	// Never begin the retained suffix with an assistant/tool message. Walking
	// back to a user message keeps tool calls and their results structurally
	// attached to the turn that requested them.
	for start > 0 && history[start].Role != "user" {
		start--
	}
	if start == 0 {
		return history, 0
	}
	recent := append([]provider.Message(nil), history[start:]...)
	marker := provider.Message{Role: "user", Content: fmt.Sprintf("[compacted %d earlier messages; preserve their conclusions]", start)}
	return append([]provider.Message{marker}, recent...), start
}

// systemPrompt assembles an agent's system prompt from its two layers.
//
// The split is by what owns the truth. Everything bakedSystemPrompt returns is
// harness mechanics — how *this build* behaves — and only the binary knows it,
// so a managed file that disagreed would simply be wrong. Everything the
// managed layer document adds is org policy about what an agent at this layer
// may do, and it has to be able to change without a rebuild.
//
// slbh can do this structurally because it owns the agent tree and knows each
// agent's Depth at the moment it builds a prompt. The other harnesses can only
// approximate per-layer delivery by convention.
func systemPrompt(a *Agent) string {
	prompt := bakedSystemPrompt(a)
	layer := a.runtime.LayerInstructions(a.Depth)
	if strings.TrimSpace(layer) != "" {
		prompt += fmt.Sprintf("\n\nOrg instructions for your layer (%s). These are managed by the fleet and define what an agent at this layer may and may not do. Where they appear to contradict the runtime mechanics above, the mechanics are facts about this build and stand; the role policy governs everything else.\n\n%s", config.LayerForDepth(a.Depth), layer)
	}
	if skills := a.runtime.LayerSkillPrompt(a.Depth); skills != "" {
		prompt += "\n\n" + skills
	}
	// Missing documents and skills both degrade independently. A native agent
	// always retains the baked mechanics even on an unmanaged host.
	return prompt
}

// bakedSystemPrompt is the harness-mechanics half, authored as one string.
//
// It used to be a legacy literal patched by two strings.Replace calls to
// insert quick_py and long_py. The patches are folded in here: a prompt
// assembled by search-and-replace has no single readable source, and layering
// a managed tier on top of a patched string would have made that permanent.
// The fold was verified byte-identical to the patched output before the
// legacy form was removed.
func bakedSystemPrompt(a *Agent) string {
	return fmt.Sprintf("You are %s, an agent in slbh runtime %s. Your frozen roster role is %s and runtime depth is %d. Show reasoning and tool activity as events. Keep answers actionable and concise. Delegated work is asynchronous: launch_subagent returns immediately, so do not block this turn waiting for a child. Do not use quick_bash, long_job, quick_py, long_py, sleep, polling, or shell wait loops to watch a child. Continue useful independent work if there is any; otherwise end your turn. Every message, including every [result from ...] message and completed long_job/long_py output, is a mandatory mid-turn steer: read and act on it during your current work. A [warning from long_job ...] or [warning from long_py ...] message means a background job you started has passed its warn_after_seconds and is still running; it is a decision point for you alone. Kill it with kill_job, leave it running and take its result when it finishes, or carry on with other work. It is the only warning that job will send, nothing escalates it, and deciding to keep waiting is a valid decision. Messages enter context in FIFO order at the next API/tool call boundary; idle agents wake immediately. In-flight API and tool calls finish normally. Preserve all inference output and tool results; already-produced tool calls execute in order. Deferring a message until the end of a turn is a failure, never a delivery mode. Use msg_subagent to message any agent by ID, including your parent or siblings. As a parent, choose each subagent's title: use three relevant words joined by hyphens, such as inspect-api-cache. This is guidance, not a validation rule. As a parent, you are responsible for ending each subagent with end_subagent when its task is fully complete; subagents stay alive indefinitely so they can receive follow-up work. %s", a.Title, a.runtime.ID(), a.Role, a.Depth, a.runtime.ModelGuidance())
}

// maxToolErrorOutput bounds the output carried back with a failing tool call. A five-second
// command can emit a great deal, and this fleet's local models run in 48k-96k windows, so an
// unbounded dump could cost more context than the diagnostic is worth.
const maxToolErrorOutput = 8000

// toolFailure renders a failed tool call for the model.
//
// It exists because the obvious version -- replacing the result with the error -- discards
// output the tool deliberately returned. quickBash and quickPy both return
// stdout+stderr ALONGSIDE their error precisely so a model can see why a command failed;
// throwing that away leaves "tool error: exit status 1" as the entire diagnostic. Shell
// exit codes are routinely informative rather than fatal -- grep exits 1 when it matches
// nothing, test exits 1 on false, a failing suite exits 1 -- so a model that sees only the
// code cannot tell "no matches" from "command not found", and retries blind. Measured on
// the v7.5 benchmark: 120 of 147 quick_bash calls failed, and 104 of those returned exactly
// "tool error: exit status 1" with no other content.
//
// The error leads so that a long output cannot bury the fact that the call failed.
func toolFailure(err error, output string) string {
	head := "tool error: " + err.Error()
	if strings.TrimSpace(output) == "" {
		return head
	}
	if len(output) > maxToolErrorOutput {
		output = output[:maxToolErrorOutput] + "\n[output truncated]"
	}
	return head + "\n" + output
}
