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

type turn struct {
	prompt      string
	steer       bool
	childResult bool
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
	SSH      string

	mu            sync.RWMutex
	status        string
	history       []provider.Message
	turns         chan turn
	steers        chan string
	cancel        context.CancelFunc
	done          chan struct{}
	stopOnce      sync.Once
	contextModel  string
	contextWindow int
	contextUsed   int
	cacheHits     int
	cacheMisses   int
	historyEpoch  uint64
}

func newAgent(runtime *Runtime, agentID, title, parentID string, depth int, model, effort string) *Agent {
	return &Agent{runtime: runtime, ID: agentID, Title: title, ParentID: parentID, Depth: depth, Model: model, Effort: effort, Harness: "native", WorkDir: runtime.workDir, status: "idle", turns: make(chan turn, 16), steers: make(chan string, 64), done: make(chan struct{})}
}

func (a *Agent) start() {
	ctx, cancel := context.WithCancel(a.runtime.ctx)
	a.cancel = cancel
	go a.loop(ctx)
}

func (a *Agent) Send(prompt string) {
	if strings.TrimSpace(prompt) == "" {
		return
	}
	select {
	case a.turns <- turn{prompt: prompt}:
	case <-a.runtime.ctx.Done():
	}
}

// Steer is deliberately a separate queue. It is visible to the agent during
// its current turn, but it cannot replace or reorder the next parent result.
func (a *Agent) Steer(message string) {
	if strings.TrimSpace(message) == "" {
		return
	}
	select {
	case a.steers <- message:
		a.runtime.emit(Event{AgentID: a.ID, AgentTitle: a.Title, Kind: "steer", Text: message})
	case <-a.runtime.ctx.Done():
	}
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
		SSH:             a.SSH,
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
	a.mu.Lock()
	a.history = nil
	a.contextUsed = 0
	a.historyEpoch++
	a.mu.Unlock()
}

func (a *Agent) stop() {
	a.stopOnce.Do(func() {
		if a.cancel != nil {
			a.cancel()
		}
		select {
		case <-a.done:
		case <-time.After(2 * time.Second):
		}
	})
}

func (a *Agent) loop(ctx context.Context) {
	defer close(a.done)
	for {
		select {
		case <-ctx.Done():
			a.setStatus("stopped")
			return
		case turn := <-a.turns:
			a.handle(ctx, turn)
		}
	}
}

func (a *Agent) handle(ctx context.Context, turn turn) {
	a.setStatus("thinking")
	if !turn.childResult {
		a.runtime.emit(Event{AgentID: a.ID, AgentTitle: a.Title, Kind: "user", Text: turn.prompt})
	}
	a.mu.Lock()
	epoch := a.historyEpoch
	a.history = append(a.history, provider.Message{Role: "user", Content: turn.prompt})
	history := append([]provider.Message(nil), a.history...)
	a.mu.Unlock()
	for {
		select {
		case steer := <-a.steers:
			a.runtime.emit(Event{AgentID: a.ID, AgentTitle: a.Title, Kind: "steer", Text: steer})
			history = append(history, provider.Message{Role: "user", Content: "[steer] " + steer})
		default:
			goto drained
		}
	}
drained:
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
	var finalAnswer strings.Builder
	system := systemPrompt(a)
	tools := ToolDefinitions()
	for round := 0; round < 100; round++ {
		history = a.compactHistoryIfNeeded(history, contextWindow, system, tools)
		req := provider.Request{Model: model, Effort: effort, System: system, Messages: history, Tools: tools, CacheKey: provider.StablePrefixKey(provider.Request{Model: model, System: system, Tools: tools})}
		var answer strings.Builder
		calls := make(map[int]*provider.ToolCall)
		err = provider.Retry(ctx, 3, func() error {
			a.recordRequestContext(req, contextWindow)
			a.runtime.recordInferenceRequest(a, round, req, p)
			return p.Stream(ctx, req, func(event provider.Event) error {
				switch event.Kind {
				case provider.EventText:
					answer.WriteString(event.Text)
					a.runtime.emit(Event{AgentID: a.ID, AgentTitle: a.Title, Kind: "assistant", Text: event.Text})
				case provider.EventReasoning:
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
		if err != nil {
			a.fail(err)
			return
		}
		if answer.Len() > 0 {
			finalAnswer.WriteString(answer.String())
		}
		if len(calls) == 0 {
			break
		}
		assistant := provider.Message{Role: "assistant", Content: answer.String()}
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
		for _, index := range ordered {
			assistant.ToolCalls = append(assistant.ToolCalls, *calls[index])
		}
		history = append(history, assistant)
		for _, index := range ordered {
			call := calls[index]
			result, toolErr := a.runtime.ExecuteTool(a.ID, call.Function.Name, call.Function.Arguments)
			if toolErr != nil {
				result = "tool error: " + toolErr.Error()
			}
			a.runtime.emit(Event{AgentID: a.ID, AgentTitle: a.Title, Kind: "tool_result", Text: result, Metadata: map[string]any{"name": call.Function.Name, "call_id": call.ID}})
			history = append(history, provider.Message{Role: "tool", ToolCallID: call.ID, Name: call.Function.Name, Content: result})
		}
	}
	a.mu.Lock()
	if a.historyEpoch == epoch {
		a.history = history
		if finalAnswer.Len() > 0 {
			a.history = append(a.history, provider.Message{Role: "assistant", Content: finalAnswer.String()})
		}
	}
	a.mu.Unlock()
	a.maybeCompact(contextWindow, system, tools)
	a.setStatus("idle")
	a.runtime.emit(Event{AgentID: a.ID, AgentTitle: a.Title, Kind: "turn_done"})
	if a.ParentID != "" && finalAnswer.Len() > 0 {
		if parent, ok := a.runtime.Agent(a.ParentID); ok {
			parent.receiveChildResult(a, finalAnswer.String())
		}
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

func (a *Agent) receiveChildResult(child *Agent, text string) {
	// Queue the result as an internal turn instead of mutating history directly.
	// The active turn owns a local history snapshot and would otherwise overwrite
	// a result that arrives before it commits.
	prompt := fmt.Sprintf("[result from %s] %s", child.Title, text)
	select {
	case a.turns <- turn{prompt: prompt, childResult: true}:
		a.runtime.emit(Event{AgentID: a.ID, AgentTitle: a.Title, Kind: "child_result", Text: text, Metadata: map[string]any{"child": child.ID}})
	case <-a.runtime.ctx.Done():
	}
}

// Compact keeps the most recent work and leaves a durable marker in the
// transcript. It is intentionally deterministic and local: a provider outage
// must never make compaction block the agent.
func (a *Agent) Compact(keep int) int {
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
	return fmt.Sprintf("You are %s, an agent in slbh runtime %s. Runtime depth is %d. Show reasoning and tool activity as events. Keep answers actionable and concise. Delegated work is asynchronous: launch_subagent returns immediately, so do not block this turn waiting for a child. Do not use quick_bash, long_job, sleep, polling, or shell wait loops to watch a child. Continue useful independent work if there is any; otherwise end your turn. The harness will deliver the child's result as a later [result from ...] message and wake you, and you should act on that result when it arrives. As a parent, you are responsible for ending each subagent with end_subagent when its task is fully complete; subagents stay alive indefinitely so they can receive follow-up work. %s", a.Title, a.runtime.ID(), a.Depth, a.runtime.ModelGuidance())
}
