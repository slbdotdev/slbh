package harness

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/slbdotdev/slbh/internal/config"
	"github.com/slbdotdev/slbh/internal/provider"
)

type claudeHelperCapture struct {
	Args        []string `json:"args"`
	Environment []string `json:"environment"`
}

func runClaudeTestHelper() {
	if path := os.Getenv("SLBH_CLAUDE_CAPTURE_FILE"); path != "" {
		data, _ := json.Marshal(claudeHelperCapture{Args: os.Args[1:], Environment: os.Environ()})
		_ = os.WriteFile(path, data, 0o600)
	}
	if path := os.Getenv("SLBH_CLAUDE_START_FILE"); path != "" {
		file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err == nil {
			_, _ = fmt.Fprintln(file, os.Getpid())
			_ = file.Close()
		}
	}
	if path := os.Getenv("SLBH_CLAUDE_CHILD_PID_FILE"); path != "" {
		child := exec.Command("sleep", "300")
		if child.Start() == nil {
			_ = os.WriteFile(path, []byte(strconv.Itoa(child.Process.Pid)), 0o600)
		}
	}
	encoder := json.NewEncoder(os.Stdout)
	_ = encoder.Encode(map[string]any{"type": "system", "subtype": "init", "session_id": "fake-claude-session"})
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var input struct {
			Type    string `json:"type"`
			Message struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(scanner.Bytes(), &input) != nil || input.Type != "user" || len(input.Message.Content) == 0 {
			continue
		}
		answer := "fake: " + input.Message.Content[0].Text
		_ = encoder.Encode(map[string]any{
			"type": "assistant", "session_id": "fake-claude-session", "uuid": "fake-message",
			"message": map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": answer}}},
		})
		_ = encoder.Encode(map[string]any{
			"type": "result", "subtype": "success", "session_id": "fake-claude-session", "result": answer, "is_error": false,
		})
	}
}

func newClaudeTestRuntime(t *testing.T, captureFile string) *Runtime {
	t.Helper()
	t.Setenv("SLBH_CLAUDE_HELPER", "1")
	if captureFile != "" {
		t.Setenv("SLBH_CLAUDE_CAPTURE_FILE", captureFile)
	}
	helperCommand := filepath.Join(t.TempDir(), "claude-helper")
	helperScript := "#!/bin/sh\nexec " + strconv.Quote(os.Args[0]) + " claude-helper \"$@\"\n"
	if err := os.WriteFile(helperCommand, []byte(helperScript), 0o700); err != nil {
		t.Fatal(err)
	}
	r, err := New(config.Config{
		Home: t.TempDir(), SeatModel: "test", ApprovedModels: []string{"test"},
		Instructions: config.Instructions{Layers: map[string]string{
			config.LayerManager: "MANAGED CLAUDE LAYER",
		}},
	}, Options{
		Provider:      func(string) (provider.Provider, error) { return fakeProvider{}, nil },
		ClaudeCommand: helperCommand,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

func launchFakeClaude(t *testing.T, r *Runtime, effort, brief string) *Agent {
	t.Helper()
	child, err := r.launchSubagentSpec(r.seat().ID, LaunchSpec{
		Title: "claude-test", Harness: "claude_code", Model: "claude-opus-5", Effort: effort, Brief: brief,
	})
	if err != nil {
		t.Fatal(err)
	}
	return child
}

func waitForClaudeEvent(t *testing.T, r *Runtime, agentID, kind string) string {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case event := <-r.Events():
			if event.AgentID == agentID && event.Kind == "error" {
				t.Fatalf("Claude leaf failed: %s", event.Text)
			}
			if event.AgentID == agentID && event.Kind == kind {
				return event.Text
			}
		case <-deadline.C:
			t.Fatalf("Claude leaf %s did not emit %s", agentID, kind)
		}
	}
}

func readClaudeCapture(t *testing.T, path string) claudeHelperCapture {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			var capture claudeHelperCapture
			if json.Unmarshal(data, &capture) == nil {
				return capture
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("Claude helper did not write capture %s", path)
	return claudeHelperCapture{}
}

func TestClaudeLeafArgvEnvironmentAndParentDelivery(t *testing.T) {
	captureFile := filepath.Join(t.TempDir(), "capture.json")
	t.Setenv("ANTHROPIC_API_KEY", "must-not-reach-child")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "must-not-reach-child")
	t.Setenv("CLAUDE_CODE_USE_BEDROCK", "1")
	t.Setenv("CLAUDE_CODE_USE_VERTEX", "1")
	r := newClaudeTestRuntime(t, captureFile)
	child := launchFakeClaude(t, r, "xhigh", "do the tiny task")
	if got := waitForClaudeEvent(t, r, child.ID, "turn_done"); got != "fake: do the tiny task" {
		t.Fatalf("Claude result = %q", got)
	}
	parentDeadline := time.After(3 * time.Second)
	for {
		select {
		case event := <-r.Events():
			if event.AgentID == r.seat().ID && event.Kind == "child_result" {
				if event.Text != "fake: do the tiny task" {
					t.Fatalf("parent received Claude result %q", event.Text)
				}
				goto parentReceived
			}
		case <-parentDeadline:
			t.Fatal("Claude turn result did not reach the parent")
		}
	}
parentReceived:

	capture := readClaudeCapture(t, captureFile)
	joined := strings.Join(capture.Args, "\x00")
	for _, want := range []string{
		"-p", "--input-format\x00stream-json", "--output-format\x00stream-json", "--verbose",
		"--model\x00claude-opus-5", "--effort\x00xhigh", "--dangerously-skip-permissions",
		"--disallowed-tools\x00Task",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("Claude argv %q is missing %q", capture.Args, want)
		}
	}
	promptIndex := -1
	for i, argument := range capture.Args {
		if argument == "--append-system-prompt" {
			promptIndex = i + 1
			break
		}
	}
	if promptIndex >= len(capture.Args) || promptIndex < 0 {
		t.Fatalf("Claude argv has no appended system prompt: %q", capture.Args)
	}
	wantArgs := []string{
		"claude-helper", "-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--model", "claude-opus-5",
		"--effort", "xhigh",
		"--append-system-prompt", capture.Args[promptIndex],
		"--dangerously-skip-permissions",
		"--disallowed-tools", "Task",
	}
	if !reflect.DeepEqual(capture.Args, wantArgs) {
		t.Fatalf("Claude argv = %#v, want %#v", capture.Args, wantArgs)
	}
	for _, want := range []string{claudeLeafMechanics, "MANAGED CLAUDE LAYER", "your layer (manager)"} {
		if !strings.Contains(capture.Args[promptIndex], want) {
			t.Fatalf("Claude system prompt is missing %q: %q", want, capture.Args[promptIndex])
		}
	}
	for _, entry := range capture.Environment {
		name, _, _ := strings.Cut(entry, "=")
		switch name {
		case "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX":
			t.Fatalf("Claude child environment retained %s", name)
		}
	}
}

func TestClaudeLeafOmitsEmptyEffort(t *testing.T) {
	captureFile := filepath.Join(t.TempDir(), "capture.json")
	r := newClaudeTestRuntime(t, captureFile)
	child := launchFakeClaude(t, r, "", "brief")
	_ = waitForClaudeEvent(t, r, child.ID, "turn_done")
	for _, argument := range readClaudeCapture(t, captureFile).Args {
		if argument == "--effort" {
			t.Fatal("Claude argv includes --effort for an empty effort")
		}
	}
}

func TestClaudeLeafRequiresExplicitModel(t *testing.T) {
	r := newClaudeTestRuntime(t, "")
	input := `{"title":"missing","harness":"claude_code","brief":""}`
	if _, err := r.ExecuteTool(r.seat().ID, "launch_subagent", input); err == nil || !strings.Contains(err.Error(), "explicit Claude model") {
		t.Fatalf("missing Claude model error = %v", err)
	}
}

func TestClaudeLeafClearStartsFreshProcess(t *testing.T) {
	startFile := filepath.Join(t.TempDir(), "starts")
	t.Setenv("SLBH_CLAUDE_START_FILE", startFile)
	r := newClaudeTestRuntime(t, "")
	child := launchFakeClaude(t, r, "low", "first")
	_ = waitForClaudeEvent(t, r, child.ID, "turn_done")
	oldTranscript, err := r.TranscriptPath(child.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.clear(child.ID); err != nil {
		t.Fatal(err)
	}
	_ = waitForClaudeEvent(t, r, child.ID, "claude_ready")
	newTranscript, err := r.TranscriptPath(child.ID)
	if err != nil {
		t.Fatal(err)
	}
	if newTranscript == oldTranscript {
		t.Fatalf("Claude clear reused transcript %q", oldTranscript)
	}
	if err := child.Send("second"); err != nil {
		t.Fatal(err)
	}
	if got := waitForClaudeEvent(t, r, child.ID, "turn_done"); got != "fake: second" {
		t.Fatalf("post-clear Claude result = %q", got)
	}
	data, err := os.ReadFile(startFile)
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Fields(string(data)); len(lines) != 2 {
		t.Fatalf("Claude helper starts = %q, want two processes", lines)
	}
}

func TestClaudeLeafStopKillsProcessGroup(t *testing.T) {
	childPIDFile := filepath.Join(t.TempDir(), "child.pid")
	t.Setenv("SLBH_CLAUDE_CHILD_PID_FILE", childPIDFile)
	r := newClaudeTestRuntime(t, "")
	child := launchFakeClaude(t, r, "low", "")
	_ = waitForClaudeEvent(t, r, child.ID, "claude_ready")
	leaf := child.claudeBackend()
	processPID := leaf.cmd.Process.Pid
	data := waitForFile(t, childPIDFile)
	descendantPID, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.endSubagent(r.seat().ID, child.ID); err != nil {
		t.Fatal(err)
	}
	for _, pid := range []int{processPID, descendantPID} {
		deadline := time.Now().Add(3 * time.Second)
		for processExists(pid) && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if processExists(pid) {
			t.Fatalf("stopping Claude leaf left process %d alive", pid)
		}
	}
}

func waitForFile(t *testing.T, path string) []byte {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil && len(data) > 0 {
			return data
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("file %s was not created", path)
	return nil
}

func processExists(pid int) bool {
	if stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil {
		fields := strings.Fields(string(stat))
		if len(fields) > 2 && fields[2] == "Z" {
			return false
		}
	}
	err := syscall.Kill(pid, 0)
	return err == nil || !errors.Is(err, syscall.ESRCH)
}

func TestClaudeStreamFixtureMapsRuntimeEvents(t *testing.T) {
	r := newClaudeTestRuntime(t, "")
	agent, err := r.newAgent("fixture", r.seat().ID, 1, "claude-opus-5", "low")
	if err != nil {
		t.Fatal(err)
	}
	agent.Harness = "claude_code"
	done := make(chan struct{})
	close(done)
	leaf := &claudeLeaf{agent: agent, runtime: r, done: done}
	agent.claudeMu.Lock()
	agent.claude = leaf
	agent.claudeMu.Unlock()
	file, err := os.Open(filepath.Join("testdata", "claude_stream.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		if err := leaf.handleLine(scanner.Bytes()); err != nil {
			t.Fatal(err)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"claude_ready": false, "tool": false, "tool_result": false, "assistant": false, "turn_done": false}
	deadline := time.After(3 * time.Second)
	for {
		complete := true
		for _, seen := range want {
			complete = complete && seen
		}
		if complete {
			return
		}
		select {
		case event := <-r.Events():
			if event.AgentID == agent.ID {
				if _, ok := want[event.Kind]; ok {
					want[event.Kind] = true
				}
			}
		case <-deadline:
			t.Fatalf("fixture event mapping = %#v", want)
		}
	}
}
