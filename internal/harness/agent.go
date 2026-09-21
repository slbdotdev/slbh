package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/slbdotdev/slbh/internal/config"
	"github.com/slbdotdev/slbh/internal/job"
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
	busy          bool
	wake          chan struct{}
	stopped       bool
	cancel        context.CancelFunc
	turnCancel    context.CancelFunc
	done          chan struct{}
	stopOnce      sync.Once
	contextModel  string
	contextWindow int
	contextUsed   int
	contextAnchor contextAnchor
	pendingAnchor contextAnchor
	cacheHits     int
	cacheMisses   int
	historyEpoch  uint64
	codexMu       sync.RWMutex
	codex         *codexLeaf
	claudeMu      sync.RWMutex
	claude        *claudeLeaf
}

func newAgent(runtime *Runtime, agentID, title, parentID string, depth int, model, effort string) *Agent {
	return &Agent{runtime: runtime, ID: agentID, Title: title, ParentID: parentID, Depth: depth, Model: model, Effort: effort, Harness: "native", WorkDir: runtime.workDir, status: "idle", wake: make(chan struct{}, 1), done: make(chan struct{})}
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
	// Another model means another tokenizer; its counts do not carry over.
	a.contextAnchor = contextAnchor{}
	a.pendingAnchor = contextAnchor{}
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

func (a *Agent) hasActiveTurn() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.turnCancel != nil
}

// ClearHistory starts the next turn with no conversation messages. An active
// native provider request is cancelled so it cannot resume the cleared
// session; Codex and Claude Code use their harness-specific reset paths.
func (a *Agent) ClearHistory() {
	a.mu.Lock()
	a.history = nil
	a.contextUsed = 0
	a.contextAnchor = contextAnchor{}
	a.pendingAnchor = contextAnchor{}
	a.historyEpoch++
	turnCancel := a.turnCancel
	a.mu.Unlock()
	if turnCancel != nil {
		turnCancel()
	}
	if codex := a.codexBackend(); codex != nil {
		codex.clear()
		return
	}
	if claude := a.claudeBackend(); claude != nil {
		claude.clear()
		return
	}
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
			// busy is set under the same lock that empties the inbox, so there
			// is no instant at which a taken message is in neither the inbox
			// nor a turn: Quiescent relies on that.
			a.mu.Lock()
			messages := a.inbox
			a.inbox = nil
			a.busy = len(messages) > 0
			a.mu.Unlock()
			if len(messages) > 0 {
				a.handle(ctx, messages)
				a.mu.Lock()
				a.busy = false
				a.mu.Unlock()
			}
		}
	}
}

func (a *Agent) handle(ctx context.Context, messages []agentMessage) {
	turnCtx, cancelTurn := context.WithCancel(ctx)
	a.mu.Lock()
	a.turnCancel = cancelTurn
	epoch := a.historyEpoch
	history := append([]provider.Message(nil), a.history...)
	a.mu.Unlock()
	defer func() {
		cleared := turnCtx.Err() != nil && ctx.Err() == nil
		cancelTurn()
		a.mu.Lock()
		a.turnCancel = nil
		a.mu.Unlock()
		if cleared {
			a.setStatus("idle")
			a.runtime.promotePending(a.ID)
		}
	}()
	a.setStatus("thinking")
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
	contextWindow := a.resolveContextWindow(turnCtx, p)
	system := systemPrompt(a)
	tools := a.runtime.toolDefinitions(a.ID)
	for round := 0; ; {
		if turnCtx.Err() != nil {
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
		attempt := 0
		err = provider.Retry(turnCtx, 3, func() (streamErr error) {
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
			attempt++
			defer func() {
				// Every failed attempt is its own event, so a recovered 5xx or a
				// dropped stream is visible in the transcript rather than
				// inferred from a retried request.
				if streamErr != nil && turnCtx.Err() == nil {
					a.runtime.emit(seam.Event{AgentID: a.ID, AgentTitle: a.Title, Kind: "request_error", Text: streamErr.Error(), Metadata: map[string]any{"round": round, "attempt": attempt}})
				}
				if streamErr == nil {
					// Completion is its own record: a response without usage
					// still completed, and usage completeness is judged apart.
					a.runtime.emit(seam.Event{AgentID: a.ID, AgentTitle: a.Title, Kind: "request_done", Metadata: map[string]any{"round": round, "attempt": attempt}})
				}
			}()
			streamErr = p.Stream(turnCtx, req, func(event provider.Event) error {
				if turnCtx.Err() != nil {
					return turnCtx.Err()
				}
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
					if incomplete, _ := event.Usage["incomplete"].(bool); !incomplete {
						a.recordUsage(event.Usage)
					}
					a.runtime.emit(seam.Event{AgentID: a.ID, AgentTitle: a.Title, Kind: "usage", Metadata: event.Usage})
					if provider.IsOutputLimitStop(event.StopReason) {
						// A generation cut off by its output bound otherwise looks
						// like any other finished turn: the usage record carries no
						// stop reason. On the local route that bound is the only
						// thing that ends a runaway, so its firing must be seen, on
						// screen and in the transcript, not inferred.
						a.runtime.emit(seam.Event{AgentID: a.ID, AgentTitle: a.Title, Kind: "warning",
							Text:     outputLimitWarning(model, event.StopReason, event.Usage),
							Metadata: map[string]any{"stop_reason": event.StopReason, "model": model, "round": round}})
					}
				}
				return nil
			})
			return streamErr
		})
		if turnCtx.Err() != nil {
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
			if turnCtx.Err() != nil {
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
			// Every execution is on record before it runs, so a call killed
			// before it returns is still a call.
			a.runtime.emit(seam.Event{AgentID: a.ID, AgentTitle: a.Title, Kind: "tool_start", Metadata: map[string]any{"name": call.Function.Name, "call_id": call.ID}})
			result, toolErr := a.runtime.ExecuteTool(a.ID, call.Function.Name, call.Function.Arguments)
			flagged := false
			var shaped *shapedToolError
			if errors.As(toolErr, &shaped) {
				// A shaped tool's failure is already in its harness's own words.
				result, flagged = shaped.text, shaped.flag
			} else if toolErr != nil {
				result = toolFailure(toolErr, result)
			}
			note, noted := a.runtime.takeExec(a.ID)
			resultMetadata := toolResultMetadata(call.Function.Name, call.ID, result, toolErr, note, noted)
			// Before it is emitted and before it enters history. A tool runs
			// with the parent environment by design, so `env` — or any script
			// with `set -x` — puts a provider key on stdout; from history it
			// would be marshalled into the next request, reaching both the
			// transcript and the provider itself.
			result = a.runtime.redactSecrets(result)
			a.runtime.emit(seam.Event{AgentID: a.ID, AgentTitle: a.Title, Kind: "tool_result", Text: result, Metadata: resultMetadata})
			history = append(history, provider.Message{Role: "tool", ToolCallID: call.ID, Name: call.Function.Name, Content: result, IsError: flagged})
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
	prefix, err := contextPrefixKey(req.System, req.Tools, req.Messages)
	if err != nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.contextWindow = contextWindow
	a.contextUsed = estimateContextTokens(req.System, req.Tools, req.Messages, a.contextAnchor)
	// The usage this request reports measures exactly this prompt.
	a.pendingAnchor = contextAnchor{messages: len(req.Messages), prefix: prefix}
}

func (a *Agent) recordUsage(usage map[string]any) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if promptTokens, ok := usageInt(usage, "prompt_tokens"); ok {
		a.contextUsed = promptTokens
		if a.pendingAnchor.prefix != "" && promptTokens > 0 {
			a.contextAnchor = a.pendingAnchor
			a.contextAnchor.tokens = promptTokens
		}
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

// outputLimitWarning is the text of the warning a truncated generation raises.
func outputLimitWarning(model, reason string, usage map[string]any) string {
	text := fmt.Sprintf("generation on %s hit its output limit (stop reason %q)", model, reason)
	if tokens, ok := usageInt(usage, "completion_tokens"); ok {
		text += fmt.Sprintf(" after %d output tokens", tokens)
	}
	return text + "; the reply is truncated"
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

func (a *Agent) receiveJobResult(snapshot job.Snapshot, stdout, stderr string) error {
	// Same reason as the foreground tools: a job's captured output reaches
	// history through deliver(), and from there the next request payload.
	text := a.runtime.redactSecrets(formatJobResult(snapshot, stdout, stderr))
	toolName := snapshot.ToolName
	if toolName == "" {
		toolName = "bash"
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
// agent can do about the job and points at job read, which since 2026-09-15
// answers a running job with what it has captured so far — the evidence the
// decision wants. Until then the text said the opposite, because the buffers
// really were unreadable mid-run and the call would have come back empty.
func (a *Agent) receiveJobWarning(snapshot job.Snapshot) error {
	toolName := snapshot.ToolName
	if toolName == "" {
		toolName = "bash"
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
	return fmt.Sprintf("%s %s is still running after %s.\nscript:\n%s\n\nThat is its state as of when this warning was raised; if the job's result has already reached you, the result is the truth and this warning is stale. This is the only warning you get for this job: nothing will send it again and nothing will act for you. Decide now, and you may decide to do nothing. job with action read returns what this job has captured so far. Use job with action kill if it is stuck or no longer worth waiting for; otherwise leave it and its captured output will be delivered to you automatically when it finishes, or carry on with other work in the meantime.",
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
	anchor := a.contextAnchor
	a.mu.RUnlock()
	compacted, dropped := compactHistory(history, contextWindow, system, tools, 24, anchor)
	if dropped == 0 {
		return
	}
	a.mu.Lock()
	a.history = compacted
	a.mu.Unlock()
	a.runtime.emit(seam.Event{AgentID: a.ID, AgentTitle: a.Title, Kind: "compact", Text: fmt.Sprintf("compacted %d earlier messages", dropped)})
}

func (a *Agent) compactHistoryIfNeeded(history []provider.Message, contextWindow int, system string, tools []provider.Tool) []provider.Message {
	a.mu.RLock()
	anchor := a.contextAnchor
	a.mu.RUnlock()
	compacted, dropped := compactHistory(history, contextWindow, system, tools, 24, anchor)
	if dropped > 0 {
		a.runtime.emit(seam.Event{AgentID: a.ID, AgentTitle: a.Title, Kind: "compact", Text: fmt.Sprintf("compacted %d earlier messages", dropped)})
		return compacted
	}
	return history
}

func compactHistory(history []provider.Message, contextWindow int, system string, tools []provider.Tool, keep int, anchor contextAnchor) ([]provider.Message, int) {
	if !contextLimitReached(contextWindow, system, history, tools, anchor) {
		return history, 0
	}
	return compactMessages(history, keep)
}

func contextBudget(contextWindow int) int {
	return contextWindow * compactAtNumerator / compactAtDenominator
}

func contextLimitReached(contextWindow int, system string, history []provider.Message, tools []provider.Tool, anchor contextAnchor) bool {
	return estimateContextTokens(system, tools, history, anchor) >= contextBudget(contextWindow)
}

// contextAnchor is the provider's own prompt_tokens for one request, together
// with the exact prompt it measured. Counting bytes overstates a thinking
// model badly: replayed reasoning_content is serialized into every request,
// but chat templates drop it from earlier turns, so it never reaches the
// model. One run on the local route estimated 82k tokens against a real
// 13.5k and would have compacted for nothing. The real count is the truth
// for the prefix it measured; only messages added since are estimated.
type contextAnchor struct {
	tokens   int
	messages int
	prefix   string
}

// estimateContextTokens returns the anchored count plus an estimate of the
// messages appended after it. Without an anchor that still matches the
// history — none yet, compaction, a cleared session — it falls back to
// estimating the whole prompt at four bytes per token.
func estimateContextTokens(system string, tools []provider.Tool, history []provider.Message, anchor contextAnchor) int {
	if anchor.tokens > 0 && anchor.messages <= len(history) {
		if prefix, err := contextPrefixKey(system, tools, history[:anchor.messages]); err == nil && prefix == anchor.prefix {
			if anchor.messages == len(history) {
				return anchor.tokens
			}
			if tail, err := json.Marshal(history[anchor.messages:]); err == nil {
				return anchor.tokens + (len(tail)+3)/4
			}
		}
	}
	encoded, err := provider.ContextPayload(provider.Request{System: system, Messages: history, Tools: tools})
	if err != nil {
		return 0
	}
	return (len(encoded) + 3) / 4
}

func contextPrefixKey(system string, tools []provider.Tool, messages []provider.Message) (string, error) {
	encoded, err := provider.ContextPayload(provider.Request{System: system, Messages: messages, Tools: tools})
	if err != nil {
		return "", err
	}
	return provider.PayloadSHA256(encoded), nil
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
		prompt += fmt.Sprintf("\n\nOrg instructions for your layer (%s). These are managed by the fleet and define what an agent at this depth may and may not do. Where they appear to contradict the runtime mechanics above, the mechanics are facts about this build and stand; the layer policy governs everything else.\n\n%s", config.LayerForDepth(a.Depth), layer)
	}
	if skills := a.runtime.LayerSkillPrompt(a.Depth); skills != "" {
		prompt += "\n\n" + skills
	}
	// Missing documents and skills both degrade independently. A native agent
	// always retains the baked mechanics even on an unmanaged host.
	return prompt
}

// bakedSystemPrompt is the harness-mechanics half: identity, how to think,
// how messages arrive, and for a launcher how subagents behave. It states
// only what the tool descriptions do not; launch rules, message addressing
// and job-warning details live on the tools themselves.
func bakedSystemPrompt(a *Agent) string {
	prompt := fmt.Sprintf("You are %s, a native agent in slbh runtime %s at depth %d.", a.Title, a.runtime.ID(), a.Depth) +
		"\n\nKeep your thinking brief and focused, moving directly to the next action or conclusion without unnecessary elaboration. When a question can be settled by looking — reading a file, running a command, checking a result — use a tool to find out rather than reasoning at length about what is likely true." +
		fmt.Sprintf("\n\nThis host is %s. Your command tools are %s. TMPDIR, TMP and TEMP point at your agent scratch directory, %s; use it for temporary files instead of /tmp, your working directory, or your home directory.", runtime.GOOS, strings.Join(commandToolNames(a.runtime.toolShape), ", "), filepath.Join(a.runtime.runtimeDir, "agents", a.ID, "scratch")) +
		"\n\nMessages reach you at your next tool or API call boundary, or wake you if you are idle: steers from the owner or other agents, subagent results, and background job output. Act on each in the current turn; never defer one to the end. A warning that a job or subagent is still running is sent once and never repeated. Decide then: kill or end it, keep waiting for its result, or do other work."
	// Launch guidance only for an agent that can launch a child. A leaf
	// cannot launch anything, and the mechanics themselves are in the
	// launch_subagent description and enforced at launch.
	if a.runtime.canLaunch(a) {
		prompt += "\n\nSubagents run asynchronously and report back as messages. Never sleep, poll, or run wait loops to watch one; if there is no other useful work, end your turn and you will be woken. End each subagent with end_subagent once its work is complete. Launch one child at the next depth with a relevant title. Depth-1 children may use the configured child model; deeper children must name a real model or short name explicitly and may select native, Codex, or Claude Code with the harness field."
	}
	return prompt
}

// toolFailure renders a failed tool call for the model.
//
// It exists because the obvious version -- replacing the result with the error -- discards
// output the tool deliberately returned. quickBash and quickPy both return
// stdout+stderr ALONGSIDE their error precisely so a model can see why a command failed;
// throwing that away leaves "tool error: exit status 1" as the entire diagnostic. Shell
// exit codes are routinely informative rather than fatal -- grep exits 1 when it matches
// nothing, test exits 1 on false, a failing suite exits 1 -- so a model that sees only the
// code cannot tell "no matches" from "command not found", and retries blind. Measured on
// the v7.5 benchmark: 120 of 147 bash calls failed, and 104 of those returned exactly
// "tool error: exit status 1" with no other content.
//
// The error leads so that a long output cannot bury the fact that the call failed.
func toolFailure(err error, output string) string {
	head := "tool error: " + err.Error()
	if strings.TrimSpace(output) == "" {
		return head
	}
	return head + "\n" + boundedOutput(output)
}
