package harness

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/slbdotdev/slbh/internal/config"
	"github.com/slbdotdev/slbh/internal/provider"
)

type fakeProvider struct{}

func (fakeProvider) Stream(ctx context.Context, _ provider.Request, sink provider.StreamSink) error {
	if err := sink(provider.Event{Kind: provider.EventReasoning, Text: "plan"}); err != nil {
		return err
	}
	if err := sink(provider.Event{Kind: provider.EventText, Text: "done"}); err != nil {
		return err
	}
	return sink(provider.Event{Kind: provider.EventDone})
}

type toolProvider struct{}

func (toolProvider) Stream(ctx context.Context, request provider.Request, sink provider.StreamSink) error {
	for _, message := range request.Messages {
		if message.Role == "tool" {
			return sink(provider.Event{Kind: provider.EventText, Text: "tool complete"})
		}
	}
	return sink(provider.Event{Kind: provider.EventTool, ToolIndex: 0, ToolCallID: "call-1", ToolName: "list_subagents", Input: "{}"})
}

func testRuntime(t *testing.T) *Runtime {
	t.Helper()
	r, err := New(config.Config{Home: t.TempDir(), RootModel: "test", RootEffort: "high", SubagentModel: "test-child", SubagentEffort: "high"}, Options{Provider: func(string) (provider.Provider, error) { return fakeProvider{}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

func TestRuntimeStreamsAndLogs(t *testing.T) {
	r := testRuntime(t)
	r.Root().Send("hello")
	deadline := time.After(5 * time.Second)
	var sawDone bool
	for !sawDone {
		select {
		case event := <-r.Events():
			if event.Kind == "turn_done" {
				sawDone = true
			}
		case <-deadline:
			t.Fatal("agent turn did not finish")
		}
	}
	entries, err := readTranscript(r)
	if err != nil {
		t.Fatal(err)
	}
	var text string
	for _, entry := range entries {
		text += entry.Text
	}
	if !strings.Contains(text, "done") {
		t.Fatalf("transcript missing response: %q", text)
	}
}

func TestDepthAndSteerIsolation(t *testing.T) {
	r := testRuntime(t)
	root := r.Root()
	child, err := r.LaunchSubagent(root.ID, "child", "brief")
	if err != nil {
		t.Fatal(err)
	}
	grandchild, err := r.LaunchSubagent(child.ID, "grandchild", "brief")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.LaunchSubagent(grandchild.ID, "too-deep", "brief"); err == nil {
		t.Fatal("expected depth error")
	}
	child.Steer("change emphasis")
	deadline := time.After(5 * time.Second)
	for {
		select {
		case event := <-r.Events():
			if event.AgentID == child.ID && event.Kind == "steer" {
				return
			}
		case <-deadline:
			t.Fatal("steer was not emitted")
		}
	}
}

func TestToolsStayInWorkingDirectory(t *testing.T) {
	r := testRuntime(t)
	dir := t.TempDir()
	r.workDir = dir
	r.Root().WorkDir = dir
	path := filepath.Join("nested", "file.txt")
	if _, err := r.ExecuteTool(r.Root().ID, "write_file", `{"path":"nested/file.txt","content":"one\ntwo\n"}`); err != nil {
		t.Fatal(err)
	}
	lines, err := r.ExecuteTool(r.Root().ID, "read_lines", `{"path":"nested/file.txt","start":2,"end":2}`)
	if err != nil || !strings.Contains(lines, "2:two") {
		t.Fatalf("lines=%q err=%v", lines, err)
	}
	if _, err := r.ExecuteTool(r.Root().ID, "edit_file", `{"path":"nested/file.txt","old":"one","new":"ONE"}`); err != nil {
		t.Fatal(err)
	}
	patch := "*** Begin Patch\n*** Update File: nested/file.txt\n@@\n-two\n+TWO\n*** End Patch"
	if _, err := r.ExecuteTool(r.Root().ID, "apply_patch", `{"patch":`+strconv.Quote(patch)+`}`); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ExecuteTool(r.Root().ID, "read_file", `{"path":"../outside.txt"}`); err == nil {
		t.Fatal("expected path traversal error")
	}
	if _, err := os.Stat(filepath.Join(dir, path)); err != nil {
		t.Fatal(err)
	}
}

func TestProviderToolCallsExecuteAndContinue(t *testing.T) {
	r, err := New(config.Config{Home: t.TempDir(), RootModel: "test", RootEffort: "high"}, Options{Provider: func(string) (provider.Provider, error) { return toolProvider{}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	r.Root().Send("use a tool")
	deadline := time.After(5 * time.Second)
	var sawResult, sawDone bool
	for !sawDone {
		select {
		case event := <-r.Events():
			if event.Kind == "tool_result" {
				sawResult = true
			}
			if event.Kind == "turn_done" {
				sawDone = true
			}
		case <-deadline:
			t.Fatal("tool turn did not finish")
		}
	}
	if !sawResult {
		t.Fatal("tool result event was not emitted")
	}
}

func readTranscript(r *Runtime) ([]logEntry, error) {
	data, err := os.ReadFile(filepath.Join(r.Dir(), "transcript.jsonl"))
	if err != nil {
		return nil, err
	}
	var entries []logEntry
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		entries = append(entries, logEntry{Text: line})
	}
	return entries, nil
}

type logEntry struct{ Text string }
