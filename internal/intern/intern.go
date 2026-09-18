// Package intern implements the read-only Intern front end on the runtime.
package intern

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/slbdotdev/slbh/internal/config"
	"github.com/slbdotdev/slbh/internal/provider"
	"github.com/slbdotdev/slbh/internal/readtools"
	"github.com/slbdotdev/slbh/internal/seam"
)

const (
	maxToolRounds     = 8
	historyBudget     = 64 * 1024
	maxBufferedEvents = 4096
)

// Options configures the Intern. Model defaults to the runtime's InternModel.
// Provider is an optional transport override, primarily useful to embedders
// and tests; when nil, Run resolves Model through provider.ForModel.
type Options struct {
	Model    string
	Provider provider.Provider
}

// Run watches the Seat's tree until ctx is canceled or the runtime closes.
// It returns only startup errors; inference errors are emitted as intern status
// events so a failed turn never takes down the watcher.
func Run(ctx context.Context, rt seam.Runtime, opts Options) error {
	if rt == nil {
		return fmt.Errorf("intern: runtime is required")
	}
	cfg := rt.Config()
	system := strings.TrimSpace(cfg.Instructions.ForName(config.InstructionIntern))
	if system == "" {
		return fmt.Errorf("intern: instructions/%s.md is missing or empty", config.InstructionIntern)
	}
	seat, err := findSeat(rt.Agents())
	if err != nil {
		return err
	}
	model := strings.TrimSpace(opts.Model)
	if model == "" {
		model = strings.TrimSpace(cfg.InternModel)
	}
	// The Intern reads the Seat's whole transcript. It runs on a local route
	// so that transcript never leaves the estate (SPEC §5); refuse any other.
	if model != "" && !strings.HasPrefix(model, "local/") {
		return fmt.Errorf("intern: model %q is not a local route; the Intern runs only on local/ models so the Seat's transcript never leaves the estate", model)
	}

	events, unsubscribe := rt.Subscribe()
	defer unsubscribe()
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	watcher := &watcher{
		rt:       rt,
		seatID:   seat.ID,
		model:    model,
		system:   system,
		provider: opts.Provider,
		buffer:   newEventBuffer(maxBufferedEvents),
		history:  newHistory(system, historyBudget),
	}
	done := make(chan error, 1)
	var pending []seam.Event
	running := false
	pendingBoundary := false

	start := func(batch []seam.Event) {
		running = true
		copied := append([]seam.Event(nil), batch...)
		go func() { done <- watcher.infer(runCtx, copied) }()
	}
	accumulate := func(event seam.Event) {
		if !watcher.belongsToSeatTree(event.AgentID) || isOwnSteer(event) {
			return
		}
		watcher.buffer.add(event)
		pending = append(pending, event)
		if event.AgentID == seat.ID && event.Kind == "turn_done" {
			if running {
				pendingBoundary = true
			} else {
				batch := pending
				pending = nil
				start(batch)
			}
		}
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case event, ok := <-events:
			if !ok {
				return nil
			}
			accumulate(event)
		case inferErr := <-done:
			// Events already waiting in the subscription arrived before this
			// inference finished. Consume them while it is still marked running
			// so all of their boundaries coalesce into the same follow-up.
			draining := true
			for draining {
				select {
				case event, ok := <-events:
					if !ok {
						return nil
					}
					accumulate(event)
				default:
					draining = false
				}
			}
			running = false
			if inferErr != nil && runCtx.Err() == nil {
				watcher.emitError(runCtx, inferErr)
			}
			if pendingBoundary && runCtx.Err() == nil {
				batch := pending
				pending = nil
				pendingBoundary = false
				start(batch)
			}
		}
	}
}

type watcher struct {
	rt       seam.Runtime
	seatID   string
	model    string
	system   string
	provider provider.Provider
	buffer   *eventBuffer
	history  *conversationHistory
}

func findSeat(agents []seam.AgentSnapshot) (seam.AgentSnapshot, error) {
	for _, agent := range agents {
		if agent.Depth == 0 {
			return agent, nil
		}
	}
	return seam.AgentSnapshot{}, fmt.Errorf("intern: runtime has no depth-0 Seat")
}

func (w *watcher) belongsToSeatTree(agentID string) bool {
	if agentID == "" {
		return false
	}
	agents := w.rt.Agents()
	byID := make(map[string]seam.AgentSnapshot, len(agents))
	for _, agent := range agents {
		byID[agent.ID] = agent
	}
	for id := agentID; id != ""; {
		if id == w.seatID {
			return true
		}
		agent, ok := byID[id]
		if !ok || agent.ParentID == id {
			return false
		}
		id = agent.ParentID
	}
	return false
}

func isOwnSteer(event seam.Event) bool {
	return event.Kind == "steer" && strings.HasPrefix(event.Text, "[intern] ")
}

func (w *watcher) providerForInference() (provider.Provider, error) {
	if strings.TrimSpace(w.model) == "" {
		return nil, fmt.Errorf("intern: no model selected; set SLBH_INTERN_MODEL or Config.InternModel")
	}
	if w.provider != nil {
		return w.provider, nil
	}
	cfg := w.rt.Config()
	return provider.ForModel(w.model, cfg.Endpoint, cfg.EndpointExplicit, cfg.Policy)
}

func (w *watcher) infer(ctx context.Context, events []seam.Event) error {
	p, err := w.providerForInference()
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(events)
	if err != nil {
		return fmt.Errorf("intern: encode events: %w", err)
	}
	turn := []provider.Message{{Role: "user", Content: "Events since your previous inference:\n" + string(encoded)}}
	tools := toolDefinitions()

	for round := 0; round < maxToolRounds; round++ {
		messages := append(w.history.messages(), turn...)
		request := provider.Request{
			Model:    w.model,
			System:   w.system,
			Messages: messages,
			Tools:    tools,
		}
		request.CacheKey = provider.StablePrefixKey(request)

		var answer strings.Builder
		var reasoning strings.Builder
		calls := make(map[int]*provider.ToolCall)
		err := p.Stream(ctx, request, func(event provider.Event) error {
			switch event.Kind {
			case provider.EventText:
				answer.WriteString(event.Text)
			case provider.EventReasoning:
				reasoning.WriteString(event.Text)
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
			case provider.EventError:
				if event.Err != nil {
					return event.Err
				}
				return fmt.Errorf("intern: provider error: %s", event.Text)
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("intern: provider inference: %w", err)
		}

		content := answer.String()
		reasoningContent := reasoning.String()
		if len(calls) == 0 {
			turn = append(turn, provider.Message{Role: "assistant", Content: content, ReasoningContent: reasoningContent})
			w.history.add(turn)
			return nil
		}

		indexes := make([]int, 0, len(calls))
		for index := range calls {
			indexes = append(indexes, index)
		}
		sort.Ints(indexes)
		for number, index := range indexes {
			call := calls[index]
			if call.ID == "" {
				call.ID = fmt.Sprintf("intern-call-%d-%d", round, number)
			}
			assistant := provider.Message{Role: "assistant", ToolCalls: []provider.ToolCall{*call}}
			if number == 0 {
				assistant.Content = content
				assistant.ReasoningContent = reasoningContent
			}
			turn = append(turn, assistant)
			result, toolErr := w.executeTool(ctx, call.Function.Name, call.Function.Arguments)
			if toolErr != nil {
				result = "error: " + toolErr.Error()
			}
			turn = append(turn, provider.Message{Role: "tool", ToolCallID: call.ID, Name: call.Function.Name, Content: result})
		}
	}
	w.history.add(turn)
	return fmt.Errorf("intern: provider/tool round limit reached (%d)", maxToolRounds)
}

func (w *watcher) emitError(ctx context.Context, err error) {
	_, _ = w.rt.Do(ctx, seam.EmitStatusCommand{Kind: "intern", Text: err.Error()})
}

func toolDefinitions() []provider.Tool {
	tools := append([]provider.Tool(nil), readtools.Definitions()...)
	tools = append(tools,
		provider.Tool{Name: "agent_snapshots", Description: "Return current runtime agent snapshots as JSON.", Parameters: emptyObjectSchema()},
		provider.Tool{Name: "job_snapshots", Description: "Return current runtime job snapshots as JSON.", Parameters: emptyObjectSchema()},
		provider.Tool{Name: "event_stream", Description: "Return buffered Seat-tree events as JSON. since is a zero-based event sequence number; limit selects at most that many events.", Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"since": map[string]any{"type": "integer", "minimum": 0},
				"limit": map[string]any{"type": "integer", "minimum": 1},
			},
		}},
		provider.Tool{Name: "ask_seat", Description: "Ask the Seat one signed, non-blocking question.", Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{"question": map[string]any{"type": "string"}},
			"required":   []string{"question"},
		}},
	)
	return tools
}

func emptyObjectSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

func (w *watcher) executeTool(ctx context.Context, name, raw string) (string, error) {
	for _, definition := range readtools.Definitions() {
		if definition.Name == name {
			seat, err := findSeat(w.rt.Agents())
			if err != nil {
				return "", err
			}
			return readtools.Execute(seat.WorkDir, name, raw)
		}
	}
	switch name {
	case "agent_snapshots":
		return marshalToolResult(w.rt.Agents())
	case "job_snapshots":
		return marshalToolResult(w.rt.JobSnapshots())
	case "event_stream":
		var args struct {
			Since *int `json:"since"`
			Limit *int `json:"limit"`
		}
		if err := decodeArgs(raw, &args); err != nil {
			return "", err
		}
		if args.Since != nil && *args.Since < 0 {
			return "", fmt.Errorf("since must be non-negative")
		}
		if args.Limit != nil && *args.Limit < 1 {
			return "", fmt.Errorf("limit must be positive")
		}
		return marshalToolResult(w.buffer.slice(args.Since, args.Limit))
	case "ask_seat":
		var args struct {
			Question string `json:"question"`
		}
		if err := decodeArgs(raw, &args); err != nil {
			return "", err
		}
		question := strings.TrimSpace(args.Question)
		if question == "" {
			return "", fmt.Errorf("question is required")
		}
		_, err := w.rt.Do(ctx, seam.SteerAgentCommand{AgentID: w.seatID, Message: "[intern] " + question})
		if err != nil {
			return "", err
		}
		return "question sent", nil
	default:
		return "", fmt.Errorf("unknown intern tool %q", name)
	}
}

func decodeArgs(raw string, value any) error {
	if strings.TrimSpace(raw) == "" {
		raw = "{}"
	}
	if err := json.Unmarshal([]byte(raw), value); err != nil {
		return fmt.Errorf("tool arguments must be JSON: %w", err)
	}
	return nil
}

func marshalToolResult(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

type bufferedEvent struct {
	Sequence int `json:"sequence"`
	seam.Event
}

type eventBuffer struct {
	mu      sync.RWMutex
	next    int
	maximum int
	events  []bufferedEvent
}

func newEventBuffer(maximum int) *eventBuffer {
	return &eventBuffer{maximum: maximum}
}

func (b *eventBuffer) add(event seam.Event) {
	b.mu.Lock()
	b.events = append(b.events, bufferedEvent{Sequence: b.next, Event: event})
	b.next++
	if len(b.events) > b.maximum {
		b.events = append([]bufferedEvent(nil), b.events[len(b.events)-b.maximum:]...)
	}
	b.mu.Unlock()
}

func (b *eventBuffer) slice(since, limit *int) []bufferedEvent {
	b.mu.RLock()
	defer b.mu.RUnlock()
	start := 0
	if since != nil {
		start = sort.Search(len(b.events), func(i int) bool { return b.events[i].Sequence >= *since })
	}
	end := len(b.events)
	if limit != nil && *limit < end-start {
		end = start + *limit
	}
	return append([]bufferedEvent(nil), b.events[start:end]...)
}

type conversationHistory struct {
	system string
	budget int
	turns  [][]provider.Message
}

func newHistory(system string, budget int) *conversationHistory {
	return &conversationHistory{system: system, budget: budget}
}

func (h *conversationHistory) messages() []provider.Message {
	var messages []provider.Message
	for _, turn := range h.turns {
		messages = append(messages, turn...)
	}
	return messages
}

func (h *conversationHistory) add(turn []provider.Message) {
	h.turns = append(h.turns, append([]provider.Message(nil), turn...))
	for len(h.turns) > 0 && h.size() > h.budget {
		h.turns = h.turns[1:]
	}
}

func (h *conversationHistory) size() int {
	encoded, _ := json.Marshal(h.messages())
	return len(h.system) + len(encoded)
}
