package harness

// Claude Code leaf workers are driven through the CLI's bidirectional
// stream-json print mode. One process owns the leaf's conversation history;
// Runtime continues to own identity, lifecycle, transcripts, and delivery to
// the parent.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/slbdotdev/slbh/internal/config"
	"github.com/slbdotdev/slbh/internal/seam"
)

const claudeLeafMechanics = "You are a leaf worker managed by slbh. You may not launch, delegate to, or create other agents. Claude Code's Task tool is disabled. Complete assigned work yourself and put the result for your slbh parent in your final response. Keep foreground commands bounded."

type claudeStreamEvent struct {
	Type      string          `json:"type"`
	Subtype   string          `json:"subtype,omitempty"`
	SessionID string          `json:"session_id,omitempty"`
	UUID      string          `json:"uuid,omitempty"`
	Message   json.RawMessage `json:"message,omitempty"`
	Result    string          `json:"result,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
	Error     json.RawMessage `json:"error,omitempty"`
}

type claudeMessage struct {
	Role    string               `json:"role"`
	Content []claudeContentBlock `json:"content"`
}

type claudeContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

type claudeLeaf struct {
	agent   *Agent
	runtime *Runtime
	command string
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	cancel  context.CancelFunc
	stderr  bytes.Buffer

	mu          sync.Mutex
	writeMu     sync.Mutex
	stopped     bool
	ready       bool
	sessionID   string
	answer      strings.Builder
	turn        int
	done        chan struct{}
	stopOnce    sync.Once
	processOnce sync.Once
}

func (a *Agent) startClaude(command string) error {
	if strings.TrimSpace(command) == "" {
		command = "claude"
	}
	a.mu.RLock()
	model, effort, workDir := strings.TrimSpace(a.Model), strings.TrimSpace(a.Effort), a.WorkDir
	a.mu.RUnlock()
	if model == "" {
		return fmt.Errorf("Claude Code leaves require an explicit Claude model in launch_subagent.model")
	}

	args := claudeArgs(model, effort, claudeDeveloperInstructions(a))
	ctx, cancel := context.WithCancel(a.runtime.ctx)
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir = workDir
	cmd.Env = scrubClaudeEnvironment(os.Environ())
	configureLeafProcess(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		_ = stdin.Close()
		return err
	}
	leaf := &claudeLeaf{
		agent: a, runtime: a.runtime, command: command, cmd: cmd, stdin: stdin,
		cancel: cancel, done: make(chan struct{}),
	}
	cmd.Stderr = &leaf.stderr
	if err := cmd.Start(); err != nil {
		cancel()
		_ = stdin.Close()
		return fmt.Errorf("start Claude Code: %w", err)
	}
	a.claudeMu.Lock()
	a.claude = leaf
	a.claudeMu.Unlock()
	go leaf.run(stdout)
	return nil
}

func claudeArgs(model, effort, instructions string) []string {
	args := []string{
		"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--model", model,
	}
	if effort = strings.TrimSpace(effort); effort != "" {
		args = append(args, "--effort", effort)
	}
	return append(args,
		"--append-system-prompt", instructions,
		"--dangerously-skip-permissions",
		"--disallowed-tools", "Task",
	)
}

func claudeDeveloperInstructions(a *Agent) string {
	instructions := claudeLeafMechanics
	layer := a.runtime.LayerInstructions(a.Depth)
	if strings.TrimSpace(layer) == "" {
		return instructions
	}
	return instructions + "\n\nOrg instructions for your layer (" + config.LayerForDepth(a.Depth) + "). These are managed by the fleet and define what an agent at this layer may and may not do. Where they appear to contradict the runtime mechanics above, the mechanics are facts about this build and stand; the role policy governs everything else.\n\n" + layer
}

func scrubClaudeEnvironment(environment []string) []string {
	blocked := map[string]bool{
		"ANTHROPIC_API_KEY":       true,
		"ANTHROPIC_AUTH_TOKEN":    true,
		"CLAUDE_CODE_USE_BEDROCK": true,
		"CLAUDE_CODE_USE_VERTEX":  true,
	}
	clean := make([]string, 0, len(environment))
	for _, entry := range environment {
		name, _, _ := strings.Cut(entry, "=")
		if !blocked[name] {
			clean = append(clean, entry)
		}
	}
	return clean
}

func (c *claudeLeaf) send(message, kind string) error {
	if strings.TrimSpace(message) == "" {
		return fmt.Errorf("message is empty")
	}
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return fmt.Errorf("agent %q is stopped", c.agent.ID)
	}
	c.mu.Unlock()
	payload, err := json.Marshal(map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": []any{map[string]any{"type": "text", "text": message}},
		},
	})
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	_, err = c.stdin.Write(append(payload, '\n'))
	c.writeMu.Unlock()
	if err != nil {
		return fmt.Errorf("write Claude Code turn: %w", err)
	}
	c.agent.setStatus("thinking")
	c.runtime.emit(seam.Event{AgentID: c.agent.ID, AgentTitle: c.agent.Title, Kind: kind, Text: message, Metadata: map[string]any{"harness": "claude_code"}})
	return nil
}

func (c *claudeLeaf) run(stdout io.ReadCloser) {
	defer close(c.done)
	defer stdout.Close()
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		if err := c.handleLine(scanner.Bytes()); err != nil {
			c.fail(err)
			break
		}
	}
	scanErr := scanner.Err()
	waitErr := c.cmd.Wait()
	if c.isStopped() || c.runtime.ctx.Err() != nil {
		return
	}
	if scanErr != nil {
		c.fail(fmt.Errorf("read Claude Code stream: %w", scanErr))
		return
	}
	if waitErr != nil {
		message := strings.TrimSpace(c.stderr.String())
		if message != "" {
			c.fail(fmt.Errorf("Claude Code exited: %w: %s", waitErr, message))
		} else {
			c.fail(fmt.Errorf("Claude Code exited: %w", waitErr))
		}
		return
	}
	c.fail(fmt.Errorf("Claude Code stream closed unexpectedly"))
}

func (c *claudeLeaf) handleLine(line []byte) error {
	var event claudeStreamEvent
	if err := json.Unmarshal(line, &event); err != nil {
		return fmt.Errorf("Claude Code stream-json: %w", err)
	}
	switch event.Type {
	case "system":
		if event.Subtype == "init" {
			c.mu.Lock()
			c.ready = true
			c.sessionID = event.SessionID
			c.mu.Unlock()
			c.agent.setStatus("idle")
			c.runtime.emit(seam.Event{AgentID: c.agent.ID, AgentTitle: c.agent.Title, Kind: "claude_ready", Metadata: map[string]any{"session": event.SessionID, "harness": "claude_code"}})
		}
	case "assistant":
		return c.handleAssistant(event)
	case "user":
		return c.handleToolResults(event)
	case "result":
		c.handleResult(event)
	case "error":
		text := event.Result
		if text == "" && len(event.Error) > 0 {
			text = string(event.Error)
		}
		if text == "" {
			text = string(line)
		}
		c.runtime.emit(seam.Event{AgentID: c.agent.ID, AgentTitle: c.agent.Title, Kind: "error", Text: text, Metadata: map[string]any{"session": event.SessionID, "harness": "claude_code"}})
	}
	return nil
}

func (c *claudeLeaf) handleAssistant(event claudeStreamEvent) error {
	var message claudeMessage
	if err := json.Unmarshal(event.Message, &message); err != nil {
		return fmt.Errorf("decode Claude Code assistant event: %w", err)
	}
	for _, block := range message.Content {
		switch block.Type {
		case "text":
			c.mu.Lock()
			c.answer.WriteString(block.Text)
			c.mu.Unlock()
			c.runtime.emit(seam.Event{AgentID: c.agent.ID, AgentTitle: c.agent.Title, Kind: "assistant", Text: block.Text, Metadata: map[string]any{"session": event.SessionID, "message": event.UUID, "harness": "claude_code"}})
		case "thinking":
			if block.Thinking != "" {
				c.runtime.emit(seam.Event{AgentID: c.agent.ID, AgentTitle: c.agent.Title, Kind: "thinking", Text: block.Thinking, Metadata: map[string]any{"session": event.SessionID, "message": event.UUID, "harness": "claude_code"}})
			}
		case "tool_use":
			c.runtime.emit(seam.Event{AgentID: c.agent.ID, AgentTitle: c.agent.Title, Kind: "tool", Text: string(block.Input), Metadata: map[string]any{"name": block.Name, "call_id": block.ID, "session": event.SessionID, "harness": "claude_code"}})
		}
	}
	return nil
}

func (c *claudeLeaf) handleToolResults(event claudeStreamEvent) error {
	var message claudeMessage
	if err := json.Unmarshal(event.Message, &message); err != nil {
		return fmt.Errorf("decode Claude Code user event: %w", err)
	}
	for _, block := range message.Content {
		if block.Type != "tool_result" {
			continue
		}
		c.runtime.emit(seam.Event{AgentID: c.agent.ID, AgentTitle: c.agent.Title, Kind: "tool_result", Text: claudeContentText(block.Content), Metadata: map[string]any{"call_id": block.ToolUseID, "is_error": block.IsError, "session": event.SessionID, "harness": "claude_code"}})
	}
	return nil
}

func claudeContentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		var joined strings.Builder
		for _, block := range blocks {
			if block.Type == "text" {
				joined.WriteString(block.Text)
			}
		}
		if joined.Len() > 0 {
			return joined.String()
		}
	}
	return string(raw)
}

func (c *claudeLeaf) handleResult(event claudeStreamEvent) {
	c.mu.Lock()
	answer := event.Result
	if answer == "" {
		answer = c.answer.String()
	}
	c.answer.Reset()
	c.turn++
	turn := c.turn
	c.mu.Unlock()
	c.agent.setStatus("idle")
	metadata := map[string]any{"session": event.SessionID, "turn": turn, "status": event.Subtype, "harness": "claude_code"}
	if event.IsError || event.Subtype == "error" {
		if answer == "" {
			answer = "Claude Code turn failed"
		}
		c.runtime.emit(seam.Event{AgentID: c.agent.ID, AgentTitle: c.agent.Title, Kind: "error", Text: answer, Metadata: metadata})
		return
	}
	if answer != "" {
		if parent, ok := c.runtime.lookupAgent(c.agent.ParentID); ok {
			if err := parent.receiveChildResult(c.agent, answer); err != nil {
				c.runtime.emit(seam.Event{AgentID: c.agent.ID, AgentTitle: c.agent.Title, Kind: "delivery_error", Text: err.Error(), Metadata: metadata})
			}
		}
	}
	c.runtime.emit(seam.Event{AgentID: c.agent.ID, AgentTitle: c.agent.Title, Kind: "turn_done", Text: answer, Metadata: metadata})
}

func (c *claudeLeaf) clear() {
	command := c.command
	c.shutdown(false)
	c.runtime.promotePending(c.agent.ID)
	if err := c.agent.startClaude(command); err != nil {
		c.agent.setStatus("error")
		c.runtime.emit(seam.Event{AgentID: c.agent.ID, AgentTitle: c.agent.Title, Kind: "error", Text: err.Error(), Metadata: map[string]any{"harness": "claude_code"}})
	}
}

func (c *claudeLeaf) compact() {
	c.runtime.emit(seam.Event{AgentID: c.agent.ID, AgentTitle: c.agent.Title, Kind: "compact", Text: "Claude Code manages its own context", Metadata: map[string]any{"harness": "claude_code"}})
}

func (c *claudeLeaf) stop() {
	c.shutdown(true)
}

func (c *claudeLeaf) shutdown(setStoppedStatus bool) {
	c.stopOnce.Do(func() {
		c.mu.Lock()
		c.stopped = true
		c.ready = false
		c.mu.Unlock()
		if setStoppedStatus {
			c.agent.setStatus("stopped")
		}
		c.terminateProcess()
		select {
		case <-c.done:
		case <-time.After(2 * time.Second):
		}
	})
}

func (c *claudeLeaf) terminateProcess() {
	c.processOnce.Do(func() {
		c.writeMu.Lock()
		if c.stdin != nil {
			_ = c.stdin.Close()
		}
		c.writeMu.Unlock()
		if c.cmd != nil && c.cmd.Process != nil {
			_ = killLeafProcess(c.cmd)
		}
		if c.cancel != nil {
			c.cancel()
		}
	})
}

func (c *claudeLeaf) fail(err error) {
	if c.isStopped() || c.runtime.ctx.Err() != nil {
		return
	}
	c.mu.Lock()
	c.stopped = true
	c.ready = false
	c.mu.Unlock()
	c.agent.setStatus("error")
	c.runtime.emit(seam.Event{AgentID: c.agent.ID, AgentTitle: c.agent.Title, Kind: "error", Text: err.Error(), Metadata: map[string]any{"harness": "claude_code"}})
	c.terminateProcess()
}

func (c *claudeLeaf) isStopped() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stopped
}
