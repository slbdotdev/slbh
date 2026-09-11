package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/slbdotdev/slbh/internal/provider"
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
	return a.deliver(agentMessage{prompt: "[steer] " + message, kind: "steer", text: message})
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
		a.runtime.emit(Event{AgentID: a.ID, AgentTitle: a.Title, Kind: message.kind, Text: message.text, Metadata: message.metadata})
	}
	return history
}

func (a *Agent) Snapshot() AgentSnapshot {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return AgentSnapshot{
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
	a.mu.Lock()
	a.history = nil
	a.contextUsed = 0
	a.historyEpoch++
	a.mu.Unlock()
}

func (a *Agent) stop() {
	a.stopOnce.Do(func() {
		if codex := a.codexBackend(); codex != nil {
			codex.stop()
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
	tools := ToolDefinitions()
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
					a.runtime.emit(Event{AgentID: a.ID, AgentTitle: a.Title, Kind: "assistant", Text: event.Text})
				case provider.EventReasoning:
					reasoning.WriteString(event.Text)
					a.runtime.emit(Event{AgentID: a.ID, AgentTitle: a.Title, Kind: "thinking", Text: event.Text})
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
					a.runtime.emit(Event{AgentID: a.ID, AgentTitle: a.Title, Kind: "tool", Text: event.Input, Metadata: map[string]any{"name": event.ToolName, "call_id": event.ToolCallID, "index": event.ToolIndex}})
				case provider.EventUsage:
					a.recordUsage(event.Usage)
					a.runtime.emit(Event{AgentID: a.ID, AgentTitle: a.Title, Kind: "usage", Metadata: event.Usage})
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
			a.runtime.emit(Event{AgentID: a.ID, AgentTitle: a.Title, Kind: "status", Text: "idle"})
			if a.ParentID != "" && answer.Len() > 0 {
				if parent, ok := a.runtime.Agent(a.ParentID); ok {
					if err := parent.receiveChildResult(a, answer.String()); err != nil {
						a.runtime.emit(Event{AgentID: a.ID, AgentTitle: a.Title, Kind: "delivery_error", Text: err.Error()})
					}
				}
			}
			a.runtime.emit(Event{AgentID: a.ID, AgentTitle: a.Title, Kind: "turn_done"})
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
				result = "tool error: " + toolErr.Error()
			}
			a.runtime.emit(Event{AgentID: a.ID, AgentTitle: a.Title, Kind: "tool_result", Text: result, Metadata: map[string]any{"name": call.Function.Name, "call_id": call.ID}})
			history = append(history, provider.Message{Role: "tool", ToolCallID: call.ID, Name: call.Function.Name, Content: result})
			history = a.appendMessages(history, a.takeMessages())
		}
		round++
	}
}

func (a *Agent) fail(err error) {
	a.setStatus("error")
	a.runtime.emit(Event{AgentID: a.ID, AgentTitle: a.Title, Kind: "error", Text: err.Error()})
}

func (a *Agent) setStatus(status string) {
	a.mu.Lock()
	a.status = status
	a.mu.Unlock()
	a.runtime.emit(Event{AgentID: a.ID, AgentTitle: a.Title, Kind: "status", Text: status})
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
	return a.deliver(agentMessage{prompt: fmt.Sprintf("[result from %s] %s", child.Title, text), kind: "child_result", text: text, metadata: map[string]any{"child": child.ID}})
}

// Compact keeps the most recent work and leaves a durable marker in the
// transcript. It is intentionally deterministic and local: a provider outage
// must never make compaction block the agent.
func (a *Agent) Compact(keep int) int {
	if codex := a.codexBackend(); codex != nil {
		codex.compact()
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
	a.runtime.emit(Event{AgentID: a.ID, AgentTitle: a.Title, Kind: "compact", Text: fmt.Sprintf("compacted %d earlier messages", dropped)})
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
	a.runtime.emit(Event{AgentID: a.ID, AgentTitle: a.Title, Kind: "compact", Text: fmt.Sprintf("compacted %d earlier messages", dropped)})
}

func (a *Agent) compactHistoryIfNeeded(history []provider.Message, contextWindow int, system string, tools []provider.Tool) []provider.Message {
	compacted, dropped := compactHistory(history, contextWindow, system, tools, 24)
	if dropped > 0 {
		a.runtime.emit(Event{AgentID: a.ID, AgentTitle: a.Title, Kind: "compact", Text: fmt.Sprintf("compacted %d earlier messages", dropped)})
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

	window := provider.FallbackContextWindow
	if metadata, ok := p.(provider.ContextWindowProvider); ok {
		metadataCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		if discovered, err := metadata.ContextWindow(metadataCtx, model); err == nil && discovered > 0 {
			window = discovered
		}
		cancel()
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

func systemPrompt(a *Agent) string {
	return fmt.Sprintf("You are %s, an agent in slbh runtime %s. Runtime depth is %d. Show reasoning and tool activity as events. Keep answers actionable and concise. Delegated work is asynchronous: launch_subagent returns immediately, so do not block this turn waiting for a child. Do not use quick_bash, long_job, sleep, polling, or shell wait loops to watch a child. Continue useful independent work if there is any; otherwise end your turn. Every message, including every [result from ...] message, is a mandatory mid-turn steer: read and act on it during your current work. Messages enter context in FIFO order at the next API/tool call boundary; idle agents wake immediately. In-flight API and tool calls finish normally. Preserve all inference output and tool results; already-produced tool calls execute in order. Deferring a message until the end of a turn is a failure, never a delivery mode. Use msg_subagent to message any agent by ID, including your parent or siblings. As a parent, you are responsible for ending each subagent with end_subagent when its task is fully complete; subagents stay alive indefinitely so they can receive follow-up work. %s", a.Title, a.runtime.ID(), a.Depth, a.runtime.ModelGuidance())
}
