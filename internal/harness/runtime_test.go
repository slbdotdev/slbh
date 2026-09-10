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
	"github.com/slbdotdev/slbh/internal/logx"
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

type usageProvider struct{}

func (usageProvider) Stream(ctx context.Context, _ provider.Request, sink provider.StreamSink) error {
	if err := sink(provider.Event{Kind: provider.EventUsage, Usage: map[string]any{
		"prompt_tokens":            float64(100),
		"prompt_cache_hit_tokens":  float64(60),
		"prompt_cache_miss_tokens": float64(40),
	}}); err != nil {
		return err
	}
	return sink(provider.Event{Kind: provider.EventText, Text: "done"})
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
	transcriptPath, err := r.TranscriptPath(r.Root().ID)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := readTranscript(transcriptPath)
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
	loggedEntries, err := logx.Read(transcriptPath)
	if err != nil {
		t.Fatal(err)
	}
	var requestRecorded bool
	for _, entry := range loggedEntries {
		if entry.Kind != "inference_request" {
			continue
		}
		requestRecorded = true
		if _, ok := entry.Metadata["context"].(map[string]any); !ok {
			t.Fatalf("generic provider request did not record replayable context: %#v", entry.Metadata)
		}
	}
	if !requestRecorded {
		t.Fatal("transcript missing inference request context")
	}
}

func TestAgentSessionsHaveSeparateTranscriptsAndClearRotatesSelectedAgent(t *testing.T) {
	r := testRuntime(t)
	root := r.Root()
	firstPath, err := r.TranscriptPath(root.ID)
	if err != nil {
		t.Fatal(err)
	}
	child, err := r.LaunchSubagent(root.ID, "child", "")
	if err != nil {
		t.Fatal(err)
	}
	childPath, err := r.TranscriptPath(child.ID)
	if err != nil {
		t.Fatal(err)
	}
	if firstPath == childPath {
		t.Fatalf("root and child share transcript path %q", firstPath)
	}

	root.Send("first session")
	waitAgentTurn(t, r, root.ID)
	firstEntries, err := logx.Read(firstPath)
	if err != nil {
		t.Fatal(err)
	}
	if !transcriptContains(firstEntries, "first session") {
		t.Fatalf("first session transcript lacks prompt: %#v", firstEntries)
	}

	if err := r.Clear(root.ID); err != nil {
		t.Fatal(err)
	}
	secondPath, err := r.TranscriptPath(root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if firstPath == secondPath {
		t.Fatalf("clear reused transcript path %q", secondPath)
	}
	if got := r.Root().ID; got != root.ID {
		t.Fatalf("clear changed root ID from %q to %q", root.ID, got)
	}
	if got, err := r.TranscriptPath(child.ID); err != nil || got != childPath {
		t.Fatalf("clear changed child transcript path to %q (err=%v)", got, err)
	}

	root.Send("second session")
	waitAgentTurn(t, r, root.ID)
	firstEntries, err = logx.Read(firstPath)
	if err != nil {
		t.Fatal(err)
	}
	secondEntries, err := logx.Read(secondPath)
	if err != nil {
		t.Fatal(err)
	}
	if !transcriptContains(firstEntries, "first session") || transcriptContains(firstEntries, "second session") {
		t.Fatalf("old transcript changed across clear: %#v", firstEntries)
	}
	if transcriptContains(secondEntries, "first session") || !transcriptContains(secondEntries, "second session") {
		t.Fatalf("new transcript has incorrect session contents: %#v", secondEntries)
	}
	for _, entry := range secondEntries {
		if entry.Session == "" {
			t.Fatalf("new transcript entry lacks session ID: %#v", entry)
		}
	}
}

func waitAgentTurn(t *testing.T, r *Runtime, agentID string) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case event := <-r.Events():
			if event.AgentID == agentID && event.Kind == "turn_done" {
				return
			}
		case <-deadline:
			t.Fatalf("agent %q turn did not finish", agentID)
		}
	}
}

func transcriptContains(entries []logx.Entry, text string) bool {
	for _, entry := range entries {
		if strings.Contains(entry.Text, text) {
			return true
		}
	}
	return false
}

func TestAgentTracksContextAndCacheStats(t *testing.T) {
	r, err := New(config.Config{Home: t.TempDir(), RootModel: "test", RootEffort: "high"}, Options{Provider: func(string) (provider.Provider, error) { return usageProvider{}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	r.Root().Send("hello")
	deadline := time.After(5 * time.Second)
	for {
		select {
		case event := <-r.Events():
			if event.Kind == "turn_done" {
				snapshot := r.Root().Snapshot()
				if snapshot.ContextWindow != provider.FallbackContextWindow {
					t.Fatalf("context window = %d, want %d", snapshot.ContextWindow, provider.FallbackContextWindow)
				}
				if snapshot.ContextUsed != 100 {
					t.Fatalf("context used = %d, want 100", snapshot.ContextUsed)
				}
				if snapshot.CacheHitTokens != 60 || snapshot.CacheMissTokens != 40 {
					t.Fatalf("cache tokens = %d/%d, want 60/40", snapshot.CacheHitTokens, snapshot.CacheMissTokens)
				}
				return
			}
		case <-deadline:
			t.Fatal("agent turn did not finish")
		}
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

func readTranscript(path string) ([]logEntry, error) {
	data, err := os.ReadFile(path)
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
