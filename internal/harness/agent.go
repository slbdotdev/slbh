package harness

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/slbdotdev/slbh/internal/provider"
)

type turn struct {
	prompt string
	steer  bool
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

	mu       sync.RWMutex
	status   string
	history  []provider.Message
	turns    chan turn
	steers   chan string
	cancel   context.CancelFunc
	done     chan struct{}
	stopOnce sync.Once
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
	return AgentSnapshot{ID: a.ID, Title: a.Title, ParentID: a.ParentID, Depth: a.Depth, Model: a.Model, Effort: a.Effort, Status: a.status, Harness: a.Harness, WorkDir: a.WorkDir, SSH: a.SSH}
}

func (a *Agent) History() []provider.Message {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return append([]provider.Message(nil), a.history...)
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
	a.runtime.emit(Event{AgentID: a.ID, AgentTitle: a.Title, Kind: "user", Text: turn.prompt})
	a.mu.Lock()
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
	p, err := a.runtime.provider(a.Model)
	if err != nil {
		a.fail(err)
		return
	}
	var finalAnswer strings.Builder
	for round := 0; round < 8; round++ {
		req := provider.Request{Model: a.Model, Effort: a.Effort, System: systemPrompt(a), Messages: history, Tools: ToolDefinitions(), CacheKey: provider.StablePrefixKey(provider.Request{Model: a.Model, System: systemPrompt(a), Tools: ToolDefinitions()})}
		var answer strings.Builder
		calls := make(map[int]*provider.ToolCall)
		err = provider.Retry(ctx, 3, func() error {
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
	a.history = history
	if finalAnswer.Len() > 0 {
		a.history = append(a.history, provider.Message{Role: "assistant", Content: finalAnswer.String()})
	}
	a.mu.Unlock()
	a.maybeCompact()
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

func (a *Agent) receiveChildResult(child *Agent, text string) {
	a.mu.Lock()
	a.history = append(a.history, provider.Message{Role: "user", Content: fmt.Sprintf("[result from %s] %s", child.Title, text)})
	a.mu.Unlock()
	a.runtime.emit(Event{AgentID: a.ID, AgentTitle: a.Title, Kind: "child_result", Text: text, Metadata: map[string]any{"child": child.ID}})
}

// Compact keeps the most recent work and leaves a durable marker in the
// transcript. It is intentionally deterministic and local: a provider outage
// must never make compaction block the agent.
func (a *Agent) Compact(keep int) int {
	if keep < 4 {
		keep = 4
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.history) <= keep {
		return 0
	}
	dropped := len(a.history) - keep
	recent := append([]provider.Message(nil), a.history[len(a.history)-keep:]...)
	a.history = append([]provider.Message{{Role: "user", Content: fmt.Sprintf("[compacted %d earlier messages; preserve their conclusions]", dropped)}}, recent...)
	a.runtime.emit(Event{AgentID: a.ID, AgentTitle: a.Title, Kind: "compact", Text: fmt.Sprintf("compacted %d earlier messages", dropped)})
	return dropped
}

func (a *Agent) maybeCompact() {
	limit := 120000
	if configured, err := strconv.Atoi(os.Getenv("SLBH_CONTEXT_BYTES")); err == nil && configured > 1000 {
		limit = configured
	}
	a.mu.RLock()
	bytes := 0
	for _, message := range a.history {
		bytes += len(message.Role) + len(message.Content) + len(message.Name) + len(message.ToolCallID)
		for _, call := range message.ToolCalls {
			bytes += len(call.ID) + len(call.Function.Name) + len(call.Function.Arguments)
		}
	}
	a.mu.RUnlock()
	if bytes >= limit*7/10 {
		a.Compact(24)
	}
}

func systemPrompt(a *Agent) string {
	return fmt.Sprintf("You are %s, an agent in slbh runtime %s. Runtime depth is %d. Show reasoning and tool activity as events. Keep answers actionable and concise.", a.Title, a.runtime.ID(), a.Depth)
}
