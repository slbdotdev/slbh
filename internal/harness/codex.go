package harness

// Codex leaf workers are driven through the app-server JSON-RPC protocol.
// The transport deliberately lives outside Agent.loop: a Codex process owns
// its own history and tools, while Runtime continues to own identity,
// lifecycle, transcripts, and parent delivery.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const codexParentTool = "slbh_message_parent"

const codexCallTimeout = 30 * time.Second

type codexWire struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *codexRPCError  `json:"error,omitempty"`
}

type codexRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type codexRPCResponse struct {
	result json.RawMessage
	err    error
}

type codexRPC struct {
	in        io.Writer
	out       io.Reader
	closeFn   func() error
	writeMu   sync.Mutex
	pendingMu sync.Mutex
	pending   map[string]chan codexRPCResponse
	nextID    atomic.Int64
	events    chan codexWire
	errors    chan error
	done      chan struct{}
	closeOnce sync.Once
}

func newCodexRPC(in io.Writer, out io.Reader, closeFn func() error) *codexRPC {
	rpc := &codexRPC{
		in: in, out: out, closeFn: closeFn, pending: make(map[string]chan codexRPCResponse),
		events: make(chan codexWire, 256), errors: make(chan error, 1), done: make(chan struct{}),
	}
	go rpc.readLoop()
	return rpc
}

func (r *codexRPC) readLoop() {
	scanner := bufio.NewScanner(r.out)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		var message codexWire
		if err := json.Unmarshal(scanner.Bytes(), &message); err != nil {
			r.reportError(fmt.Errorf("codex app-server protocol: %w", err))
			return
		}
		if len(message.ID) > 0 && message.Method == "" {
			key := string(message.ID)
			r.pendingMu.Lock()
			waiter := r.pending[key]
			delete(r.pending, key)
			r.pendingMu.Unlock()
			if waiter == nil {
				continue
			}
			if message.Error != nil {
				waiter <- codexRPCResponse{err: fmt.Errorf("codex %d: %s", message.Error.Code, message.Error.Message)}
			} else {
				waiter <- codexRPCResponse{result: message.Result}
			}
			continue
		}
		select {
		case r.events <- message:
		case <-r.done:
			return
		}
	}
	if err := scanner.Err(); err != nil {
		r.reportError(fmt.Errorf("read codex app-server: %w", err))
	} else {
		r.reportError(io.EOF)
	}
}

func (r *codexRPC) reportError(err error) {
	select {
	case r.errors <- err:
	default:
	}
	r.close()
}

func (r *codexRPC) write(message any) error {
	payload, err := json.Marshal(message)
	if err != nil {
		return err
	}
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	if _, err := r.in.Write(append(payload, '\n')); err != nil {
		return err
	}
	return nil
}

func (r *codexRPC) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := r.nextID.Add(1)
	key := fmt.Sprintf("%d", id)
	waiter := make(chan codexRPCResponse, 1)
	r.pendingMu.Lock()
	r.pending[key] = waiter
	r.pendingMu.Unlock()
	if err := r.write(map[string]any{"id": id, "method": method, "params": params}); err != nil {
		r.pendingMu.Lock()
		delete(r.pending, key)
		r.pendingMu.Unlock()
		return nil, err
	}
	select {
	case response := <-waiter:
		return response.result, response.err
	case <-ctx.Done():
		r.pendingMu.Lock()
		delete(r.pending, key)
		r.pendingMu.Unlock()
		return nil, ctx.Err()
	case <-r.done:
		return nil, io.EOF
	}
}

func (r *codexRPC) respond(id json.RawMessage, result any) error {
	var raw json.RawMessage
	var err error
	if result != nil {
		raw, err = json.Marshal(result)
		if err != nil {
			return err
		}
	}
	message := map[string]any{"id": json.RawMessage(id), "result": json.RawMessage(raw)}
	return r.write(message)
}

func (r *codexRPC) respondError(id json.RawMessage, code int, message string) error {
	return r.write(map[string]any{"id": json.RawMessage(id), "error": map[string]any{"code": code, "message": message}})
}

func (r *codexRPC) close() {
	r.closeOnce.Do(func() {
		close(r.done)
		if r.closeFn != nil {
			_ = r.closeFn()
		}
		r.pendingMu.Lock()
		for key, waiter := range r.pending {
			delete(r.pending, key)
			waiter <- codexRPCResponse{err: io.EOF}
		}
		r.pendingMu.Unlock()
	})
}

type codexLeaf struct {
	agent   *Agent
	runtime *Runtime
	rpc     *codexRPC
	cmd     *exec.Cmd
	cancel  context.CancelFunc

	mu             sync.Mutex
	ready          bool
	stopped        bool
	threadID       string
	activeTurn     string
	pending        []string
	answers        map[string]string
	completed      map[string]bool
	wake           chan struct{}
	done           chan struct{}
	stopOnce       sync.Once
	processOnce    sync.Once
	resetting      bool
	compactPending bool
}

func (a *Agent) startCodex(command string) error {
	if strings.TrimSpace(command) == "" {
		command = "codex"
	}
	ctx, cancel := context.WithCancel(a.runtime.ctx)
	cmd := exec.CommandContext(ctx, command, "app-server", "--stdio")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return err
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		cancel()
		return fmt.Errorf("start codex app-server: %w", err)
	}
	rpc := newCodexRPC(stdin, stdout, func() error {
		_ = stdin.Close()
		return stdout.Close()
	})
	leaf := &codexLeaf{agent: a, runtime: a.runtime, rpc: rpc, cmd: cmd, cancel: cancel, answers: make(map[string]string), completed: make(map[string]bool), wake: make(chan struct{}, 1), done: make(chan struct{})}
	a.codexMu.Lock()
	a.codex = leaf
	a.codexMu.Unlock()
	go leaf.run()
	return nil
}

func (c *codexLeaf) send(message, kind string) error {
	if strings.TrimSpace(message) == "" {
		return fmt.Errorf("message is empty")
	}
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return fmt.Errorf("agent %q is stopped", c.agent.ID)
	}
	c.pending = append(c.pending, message)
	c.mu.Unlock()
	c.runtime.emit(Event{AgentID: c.agent.ID, AgentTitle: c.agent.Title, Kind: kind, Text: message, Metadata: map[string]any{"harness": "codex"}})
	select {
	case c.wake <- struct{}{}:
	default:
	}
	return nil
}

func (c *codexLeaf) clear() {
	c.mu.Lock()
	if !c.stopped {
		c.resetting = true
		c.pending = nil
		c.compactPending = false
	}
	c.mu.Unlock()
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// compact asks Codex to compact its own thread. The app-server call is
// bounded and runs asynchronously so the slbh API remains responsive even if
// the child process is slow or unavailable.
func (c *codexLeaf) compact() {
	c.mu.Lock()
	thread := c.threadID
	ready := c.ready
	stopped := c.stopped
	active := c.activeTurn != ""
	if !stopped && (!ready || thread == "" || active) {
		c.compactPending = true
	}
	c.mu.Unlock()
	if stopped {
		c.runtime.emit(Event{AgentID: c.agent.ID, AgentTitle: c.agent.Title, Kind: "error", Text: "Codex compaction requested after the leaf stopped", Metadata: map[string]any{"harness": "codex"}})
		return
	}
	if !ready || thread == "" || active {
		select {
		case c.wake <- struct{}{}:
		default:
		}
		return
	}
	c.requestCompact(thread)
}

func (c *codexLeaf) requestCompact(thread string) {
	go func() {
		ctx, cancel := context.WithTimeout(c.runtime.ctx, 5*time.Second)
		defer cancel()
		if _, err := c.rpc.call(ctx, "thread/compact/start", map[string]any{"threadId": thread}); err != nil {
			if !c.isStopped() {
				c.runtime.emit(Event{AgentID: c.agent.ID, AgentTitle: c.agent.Title, Kind: "error", Text: fmt.Sprintf("Codex compaction: %v", err), Metadata: map[string]any{"thread": thread, "harness": "codex"}})
			}
			return
		}
		c.runtime.emit(Event{AgentID: c.agent.ID, AgentTitle: c.agent.Title, Kind: "compact", Text: "Codex compaction requested", Metadata: map[string]any{"thread": thread, "harness": "codex"}})
	}()
}

func (c *codexLeaf) maybeCompact() {
	c.mu.Lock()
	if !c.compactPending || c.stopped || !c.ready || c.threadID == "" || c.activeTurn != "" {
		c.mu.Unlock()
		return
	}
	c.compactPending = false
	thread := c.threadID
	c.mu.Unlock()
	c.requestCompact(thread)
}

func (c *codexLeaf) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	requestCtx, cancel := context.WithTimeout(ctx, codexCallTimeout)
	defer cancel()
	return c.rpc.call(requestCtx, method, params)
}

func (c *codexLeaf) stop() {
	c.stopOnce.Do(func() {
		c.mu.Lock()
		c.stopped = true
		c.ready = false
		c.mu.Unlock()
		c.agent.setStatus("stopped")
		c.terminateProcess()
		select {
		case <-c.done:
		case <-time.After(2 * time.Second):
		}
	})
}

func (c *codexLeaf) terminateProcess() {
	c.processOnce.Do(func() {
		if c.cancel != nil {
			c.cancel()
		}
		if c.rpc != nil {
			c.rpc.close()
		}
		if c.cmd != nil {
			if c.cmd.Process != nil {
				_ = c.cmd.Process.Kill()
			}
			_ = c.cmd.Wait()
		}
	})
}

func (c *codexLeaf) run() {
	defer close(c.done)
	defer c.terminateProcess()
	ctx, cancel := context.WithCancel(c.agent.runtime.ctx)
	defer cancel()
	if err := c.initialize(ctx); err != nil {
		c.fail(err)
		return
	}
	c.mu.Lock()
	c.ready = true
	c.mu.Unlock()
	c.agent.setStatus("idle")
	c.runtime.emit(Event{AgentID: c.agent.ID, AgentTitle: c.agent.Title, Kind: "codex_ready", Metadata: map[string]any{"thread": c.threadID}})
	c.pump(ctx)
	c.maybeCompact()
	for {
		select {
		case <-ctx.Done():
			return
		case err := <-c.rpc.errors:
			if err != nil && !c.isStopped() {
				c.fail(err)
			}
			return
		case message := <-c.rpc.events:
			c.handle(message)
		case <-c.wake:
			c.mu.Lock()
			reset := c.resetting
			c.resetting = false
			c.mu.Unlock()
			if reset {
				c.reset(ctx)
			} else {
				c.pump(ctx)
			}
			c.maybeCompact()
		}
	}
}

func (c *codexLeaf) reset(ctx context.Context) {
	c.mu.Lock()
	oldThread, oldTurn := c.threadID, c.activeTurn
	c.ready = false
	c.threadID = ""
	c.activeTurn = ""
	c.answers = make(map[string]string)
	c.completed = make(map[string]bool)
	c.mu.Unlock()
	if oldThread != "" && oldTurn != "" {
		_, _ = c.call(ctx, "turn/interrupt", map[string]any{"threadId": oldThread, "turnId": oldTurn})
	}
	if err := c.startThread(ctx); err != nil {
		c.fail(err)
		return
	}
	c.mu.Lock()
	c.ready = true
	c.mu.Unlock()
	c.agent.setStatus("idle")
	c.runtime.emit(Event{AgentID: c.agent.ID, AgentTitle: c.agent.Title, Kind: "codex_ready", Metadata: map[string]any{"thread": c.threadID, "reset": true}})
	c.pump(ctx)
}

func (c *codexLeaf) initialize(ctx context.Context) error {
	if _, err := c.call(ctx, "initialize", map[string]any{
		"clientInfo":   map[string]string{"name": "slbh", "title": "slbh Codex leaf", "version": "0.1.0"},
		"capabilities": map[string]any{"experimentalApi": true},
	}); err != nil {
		return err
	}
	if err := c.rpc.write(map[string]any{"method": "initialized", "params": map[string]any{}}); err != nil {
		return err
	}
	return c.startThread(ctx)
}

func (c *codexLeaf) startThread(ctx context.Context) error {
	c.agent.mu.RLock()
	model, workDir := c.agent.Model, c.agent.WorkDir
	c.agent.mu.RUnlock()
	if strings.TrimSpace(model) == "" {
		return fmt.Errorf("no model selected; configure an approved model with /models or honor an explicit user model request")
	}
	params := map[string]any{
		"model":                 model,
		"cwd":                   workDir,
		"approvalPolicy":        "never",
		"sandbox":               "danger-full-access",
		"serviceName":           "slbh",
		"config":                map[string]any{"agents": map[string]any{"enabled": false}},
		"developerInstructions": "You are a leaf worker managed by slbh. You may not launch, delegate to, or create other agents. You may send progress to your slbh parent with slbh_message_parent. Keep foreground commands bounded and use background work only when the Codex policy provides it.",
		"dynamicTools": []any{map[string]any{
			"type": "function", "name": codexParentTool,
			"description": "Send a concise progress or result message to the slbh parent agent.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"message": map[string]any{"type": "string"}}, "required": []string{"message"}, "additionalProperties": false},
		}},
	}
	result, err := c.call(ctx, "thread/start", params)
	if err != nil {
		return err
	}
	var decoded struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(result, &decoded); err != nil {
		return fmt.Errorf("decode thread/start: %w", err)
	}
	if decoded.Thread.ID == "" {
		return fmt.Errorf("thread/start returned no thread id")
	}
	c.mu.Lock()
	c.threadID = decoded.Thread.ID
	c.mu.Unlock()
	return nil
}

func (c *codexLeaf) pump(ctx context.Context) {
	for {
		c.mu.Lock()
		if !c.ready || c.stopped || len(c.pending) == 0 || c.threadID == "" {
			c.mu.Unlock()
			return
		}
		message := c.pending[0]
		turn := c.activeTurn
		c.pending = c.pending[1:]
		c.mu.Unlock()

		var err error
		var result json.RawMessage
		if turn != "" {
			result, err = c.call(ctx, "turn/steer", map[string]any{"threadId": c.threadID, "input": []any{map[string]any{"type": "text", "text": message}}, "expectedTurnId": turn})
		} else {
			result, err = c.call(ctx, "turn/start", map[string]any{"threadId": c.threadID, "input": []any{map[string]any{"type": "text", "text": message}}})
		}
		if err != nil {
			c.mu.Lock()
			c.pending = append([]string{message}, c.pending...)
			c.mu.Unlock()
			if strings.Contains(err.Error(), "no active turn") || strings.Contains(err.Error(), "turn") {
				c.mu.Lock()
				c.activeTurn = ""
				c.mu.Unlock()
				continue
			}
			c.fail(err)
			return
		}
		if turn == "" {
			var started struct {
				Turn struct {
					ID string `json:"id"`
				} `json:"turn"`
			}
			if json.Unmarshal(result, &started) == nil && started.Turn.ID != "" {
				c.mu.Lock()
				c.activeTurn = started.Turn.ID
				c.mu.Unlock()
				c.agent.setStatus("thinking")
			}
		}
		// One turn/start or steer is in flight at a time. The next message is
		// submitted after the corresponding notification updates activeTurn.
		return
	}
}

func (c *codexLeaf) handle(message codexWire) {
	if message.Method == "item/tool/call" {
		c.handleToolCall(message)
		return
	}
	if len(message.ID) > 0 && message.Method != "" {
		_ = c.rpc.respondError(message.ID, -32601, "unsupported leaf server request")
		c.runtime.emit(Event{AgentID: c.agent.ID, AgentTitle: c.agent.Title, Kind: "codex_request_rejected", Text: message.Method})
		return
	}
	switch message.Method {
	case "item/started":
		var params struct {
			ThreadID string                     `json:"threadId"`
			TurnID   string                     `json:"turnId"`
			Item     map[string]json.RawMessage `json:"item"`
		}
		if json.Unmarshal(message.Params, &params) != nil || params.ThreadID != c.threadID {
			return
		}
		var typ, itemID string
		_ = json.Unmarshal(params.Item["type"], &typ)
		_ = json.Unmarshal(params.Item["id"], &itemID)
		if typ == "collabToolCall" {
			c.runtime.emit(Event{AgentID: c.agent.ID, AgentTitle: c.agent.Title, Kind: "codex_request_rejected", Text: "Codex collaboration is disabled for leaf workers", Metadata: map[string]any{"thread": params.ThreadID, "turn": params.TurnID, "item": itemID, "harness": "codex"}})
		}
		c.runtime.emit(Event{AgentID: c.agent.ID, AgentTitle: c.agent.Title, Kind: "codex_item_started", Text: typ, Metadata: map[string]any{"thread": params.ThreadID, "turn": params.TurnID, "item": itemID, "harness": "codex"}})
	case "turn/started":
		var params struct {
			ThreadID string `json:"threadId"`
			Turn     struct {
				ID string `json:"id"`
			} `json:"turn"`
		}
		if json.Unmarshal(message.Params, &params) == nil && params.ThreadID == c.threadID {
			c.mu.Lock()
			if c.activeTurn != "" && c.activeTurn != params.Turn.ID {
				c.mu.Unlock()
				return
			}
			c.activeTurn = params.Turn.ID
			c.mu.Unlock()
			c.agent.setStatus("thinking")
		}
	case "item/agentMessage/delta":
		var params struct{ ThreadID, TurnID, ItemID, Delta string }
		var raw map[string]json.RawMessage
		if json.Unmarshal(message.Params, &raw) != nil {
			return
		}
		_ = json.Unmarshal(raw["threadId"], &params.ThreadID)
		_ = json.Unmarshal(raw["turnId"], &params.TurnID)
		_ = json.Unmarshal(raw["itemId"], &params.ItemID)
		_ = json.Unmarshal(raw["delta"], &params.Delta)
		if params.ThreadID != c.threadID {
			return
		}
		c.mu.Lock()
		c.answers[params.TurnID] += params.Delta
		c.mu.Unlock()
		c.runtime.emit(Event{AgentID: c.agent.ID, AgentTitle: c.agent.Title, Kind: "assistant", Text: params.Delta, Metadata: map[string]any{"thread": params.ThreadID, "turn": params.TurnID, "item": params.ItemID, "harness": "codex"}})
	case "item/completed":
		var params struct {
			Item map[string]json.RawMessage `json:"item"`
		}
		if json.Unmarshal(message.Params, &params) != nil {
			return
		}
		var typ, turnID, text string
		_ = json.Unmarshal(params.Item["type"], &typ)
		_ = json.Unmarshal(params.Item["text"], &text)
		_ = json.Unmarshal(params.Item["turnId"], &turnID)
		if typ == "agentMessage" && text != "" {
			c.mu.Lock()
			c.answers[turnID] = text
			c.mu.Unlock()
		}
	case "turn/completed":
		var params struct {
			ThreadID string `json:"threadId"`
			Turn     struct {
				ID     string `json:"id"`
				Status string `json:"status"`
			} `json:"turn"`
		}
		if json.Unmarshal(message.Params, &params) != nil || params.ThreadID != c.threadID {
			return
		}
		c.mu.Lock()
		answer := c.answers[params.Turn.ID]
		if c.completed[params.Turn.ID] {
			c.mu.Unlock()
			return
		}
		c.completed[params.Turn.ID] = true
		if c.activeTurn == params.Turn.ID {
			c.activeTurn = ""
		}
		c.mu.Unlock()
		c.agent.setStatus("idle")
		if answer != "" {
			if parent, ok := c.agent.runtime.Agent(c.agent.ParentID); ok {
				if err := parent.receiveChildResult(c.agent, answer); err != nil {
					c.runtime.emit(Event{AgentID: c.agent.ID, AgentTitle: c.agent.Title, Kind: "delivery_error", Text: err.Error()})
				}
			}
		}
		c.runtime.emit(Event{AgentID: c.agent.ID, AgentTitle: c.agent.Title, Kind: "turn_done", Text: answer, Metadata: map[string]any{"thread": params.ThreadID, "turn": params.Turn.ID, "status": params.Turn.Status, "harness": "codex"}})
		select {
		case c.wake <- struct{}{}:
		default:
		}
	case "error":
		c.runtime.emit(Event{AgentID: c.agent.ID, AgentTitle: c.agent.Title, Kind: "error", Text: string(message.Params)})
	}
}

func (c *codexLeaf) handleToolCall(message codexWire) {
	var params struct{ ThreadID, TurnID, Tool, Arguments string }
	var raw map[string]json.RawMessage
	if json.Unmarshal(message.Params, &raw) != nil {
		_ = c.rpc.respond(message.ID, map[string]any{"success": false, "contentItems": []any{}})
		return
	}
	_ = json.Unmarshal(raw["threadId"], &params.ThreadID)
	_ = json.Unmarshal(raw["turnId"], &params.TurnID)
	_ = json.Unmarshal(raw["tool"], &params.Tool)
	var arguments json.RawMessage = raw["arguments"]
	if params.Tool != codexParentTool {
		_ = c.rpc.respond(message.ID, map[string]any{"success": false, "contentItems": []any{map[string]any{"type": "inputText", "text": "tool unavailable"}}})
		return
	}
	var args struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(arguments, &args) != nil {
		var encoded string
		if json.Unmarshal(arguments, &encoded) == nil {
			_ = json.Unmarshal([]byte(encoded), &args)
		}
	}
	if strings.TrimSpace(args.Message) == "" {
		_ = c.rpc.respond(message.ID, map[string]any{"success": false, "contentItems": []any{map[string]any{"type": "inputText", "text": "message is required"}}})
		return
	}
	parent, ok := c.runtime.Agent(c.agent.ParentID)
	if !ok {
		_ = c.rpc.respond(message.ID, map[string]any{"success": false, "contentItems": []any{map[string]any{"type": "inputText", "text": "parent is unavailable"}}})
		return
	}
	err := parent.steerFrom(c.agent, args.Message)
	if err != nil {
		_ = c.rpc.respond(message.ID, map[string]any{"success": false, "contentItems": []any{map[string]any{"type": "inputText", "text": err.Error()}}})
		return
	}
	c.runtime.emit(Event{AgentID: c.agent.ID, AgentTitle: c.agent.Title, Kind: "child_message", Text: args.Message, Metadata: map[string]any{"parent": parent.ID, "thread": params.ThreadID, "turn": params.TurnID, "harness": "codex"}})
	_ = c.rpc.respond(message.ID, map[string]any{"success": true, "contentItems": []any{map[string]any{"type": "inputText", "text": "accepted"}}})
}

func (c *codexLeaf) fail(err error) {
	if c.isStopped() {
		return
	}
	c.mu.Lock()
	c.stopped = true
	c.ready = false
	c.mu.Unlock()
	c.agent.setStatus("error")
	c.runtime.emit(Event{AgentID: c.agent.ID, AgentTitle: c.agent.Title, Kind: "error", Text: err.Error(), Metadata: map[string]any{"harness": "codex"}})
	c.terminateProcess()
}

func (c *codexLeaf) isStopped() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stopped
}
