package harness

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
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

type reasoningToolProvider struct {
	mu       sync.Mutex
	requests []provider.Request
}

func (p *reasoningToolProvider) Stream(_ context.Context, request provider.Request, sink provider.StreamSink) error {
	p.mu.Lock()
	call := len(p.requests)
	request.Messages = append([]provider.Message(nil), request.Messages...)
	p.requests = append(p.requests, request)
	p.mu.Unlock()
	if call == 0 {
		if err := sink(provider.Event{Kind: provider.EventReasoning, Text: "thought"}); err != nil {
			return err
		}
		if err := sink(provider.Event{Kind: provider.EventText, Text: "before tool"}); err != nil {
			return err
		}
		if err := sink(provider.Event{Kind: provider.EventTool, ToolIndex: 0, ToolCallID: "call-1", ToolName: "list_subagents", Input: "{}"}); err != nil {
			return err
		}
		return sink(provider.Event{Kind: provider.EventTool, ToolIndex: 1, ToolCallID: "call-2", ToolName: "list_subagents", Input: "{}"})
	}
	return sink(provider.Event{Kind: provider.EventText, Text: "done"})
}

func (p *reasoningToolProvider) snapshot() []provider.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]provider.Request(nil), p.requests...)
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

type childResultProvider struct{}

func (childResultProvider) Stream(_ context.Context, request provider.Request, sink provider.StreamSink) error {
	if request.Model == "test-child" {
		return sink(provider.Event{Kind: provider.EventText, Text: "child answer"})
	}
	for _, message := range request.Messages {
		if message.Role == "user" && strings.HasPrefix(message.Content, "[result from child]") {
			return sink(provider.Event{Kind: provider.EventText, Text: "parent saw child"})
		}
	}
	for _, message := range request.Messages {
		if message.Role == "tool" {
			return sink(provider.Event{Kind: provider.EventText, Text: "parent waiting"})
		}
	}
	return sink(provider.Event{Kind: provider.EventTool, ToolIndex: 0, ToolCallID: "launch-1", ToolName: "launch_subagent", Input: `{"title":"child","brief":"child brief"}`})
}

func testRuntime(t *testing.T) *Runtime {
	t.Helper()
	r, err := New(config.Config{Home: t.TempDir(), SeatModel: "test", SeatEffort: "high", SubagentModel: "test-child", SubagentEffort: "high"}, Options{Provider: func(string) (provider.Provider, error) { return fakeProvider{}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

func TestRuntimeStreamsAndLogs(t *testing.T) {
	r := testRuntime(t)
	if got := len(strings.TrimPrefix(r.ID(), "run-")); got != 8 {
		t.Fatalf("runtime ID suffix length = %d, want 8", got)
	}
	if got := len(strings.TrimPrefix(r.Seat().ID, "agent-")); got != 8 {
		t.Fatalf("agent ID suffix length = %d, want 8", got)
	}
	r.Seat().Send("hello")
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
	transcriptPath, err := r.TranscriptPath(r.Seat().ID)
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
	seat := r.Seat()
	firstPath, err := r.TranscriptPath(seat.ID)
	if err != nil {
		t.Fatal(err)
	}
	child, err := r.LaunchSubagent(seat.ID, "child", "")
	if err != nil {
		t.Fatal(err)
	}
	childPath, err := r.TranscriptPath(child.ID)
	if err != nil {
		t.Fatal(err)
	}
	if firstPath == childPath {
		t.Fatalf("seat and child share transcript path %q", firstPath)
	}

	seat.Send("first session")
	waitAgentTurn(t, r, seat.ID)
	firstEntries, err := logx.Read(firstPath)
	if err != nil {
		t.Fatal(err)
	}
	if !transcriptContains(firstEntries, "first session") {
		t.Fatalf("first session transcript lacks prompt: %#v", firstEntries)
	}

	if err := r.Clear(seat.ID); err != nil {
		t.Fatal(err)
	}
	secondPath, err := r.TranscriptPath(seat.ID)
	if err != nil {
		t.Fatal(err)
	}
	if firstPath == secondPath {
		t.Fatalf("clear reused transcript path %q", secondPath)
	}
	if got := r.Seat().ID; got != seat.ID {
		t.Fatalf("clear changed seat ID from %q to %q", seat.ID, got)
	}
	if got, err := r.TranscriptPath(child.ID); err != nil || got != childPath {
		t.Fatalf("clear changed child transcript path to %q (err=%v)", got, err)
	}

	seat.Send("second session")
	waitAgentTurn(t, r, seat.ID)
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
	r, err := New(config.Config{Home: t.TempDir(), SeatModel: "test", SeatEffort: "high"}, Options{Provider: func(string) (provider.Provider, error) { return usageProvider{}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	r.Seat().Send("hello")
	deadline := time.After(5 * time.Second)
	for {
		select {
		case event := <-r.Events():
			if event.Kind == "turn_done" {
				snapshot := r.Seat().Snapshot()
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
	seat := r.Seat()
	child, err := r.LaunchSubagent(seat.ID, "child", "brief")
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

func TestToolsAllowPathsOutsideWorkingDirectory(t *testing.T) {
	r := testRuntime(t)
	workDir := t.TempDir()
	outsideDir := t.TempDir()
	r.workDir = workDir
	r.Seat().WorkDir = workDir
	path := filepath.Join(outsideDir, "nested", "file.txt")
	writeArgs, err := json.Marshal(map[string]string{"path": path, "content": "one\ntwo\n"})
	if _, err := r.ExecuteTool(r.Seat().ID, "write_file", string(writeArgs)); err != nil {
		t.Fatal(err)
	}
	globArgs, err := json.Marshal(map[string]string{"pattern": filepath.Join(outsideDir, "nested", "*.txt")})
	if err != nil {
		t.Fatal(err)
	}
	matches, err := r.ExecuteTool(r.Seat().ID, "glob", string(globArgs))
	if err != nil || !strings.Contains(matches, filepath.Clean(path)) {
		t.Fatalf("glob=%q err=%v", matches, err)
	}
	grepArgs, err := json.Marshal(map[string]string{"pattern": "two", "path": path})
	if err != nil {
		t.Fatal(err)
	}
	grep, err := r.ExecuteTool(r.Seat().ID, "grep", string(grepArgs))
	if err != nil || !strings.Contains(grep, filepath.Clean(path)+":2:two") {
		t.Fatalf("grep=%q err=%v", grep, err)
	}
	readLinesArgs, err := json.Marshal(map[string]any{"path": path, "start": 2, "end": 2})
	if err != nil {
		t.Fatal(err)
	}
	lines, err := r.ExecuteTool(r.Seat().ID, "read_lines", string(readLinesArgs))
	if err != nil || !strings.Contains(lines, "2:two") {
		t.Fatalf("lines=%q err=%v", lines, err)
	}
	editArgs, err := json.Marshal(map[string]string{"path": path, "old": "one", "new": "ONE"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.ExecuteTool(r.Seat().ID, "edit_file", string(editArgs)); err != nil {
		t.Fatal(err)
	}
	readArgs, err := json.Marshal(map[string]string{"path": path})
	if err != nil {
		t.Fatal(err)
	}
	content, err := r.ExecuteTool(r.Seat().ID, "read_file", string(readArgs))
	if err != nil || content != "ONE\ntwo\n" {
		t.Fatalf("content=%q err=%v", content, err)
	}
	relativePath, err := filepath.Rel(workDir, path)
	if err != nil {
		t.Fatal(err)
	}
	relativeReadArgs, err := json.Marshal(map[string]string{"path": relativePath})
	if err != nil {
		t.Fatal(err)
	}
	if relativeContent, err := r.ExecuteTool(r.Seat().ID, "read_file", string(relativeReadArgs)); err != nil || relativeContent != content {
		t.Fatalf("relative content=%q err=%v", relativeContent, err)
	}
	bytesArgs, err := json.Marshal(map[string]any{"path": path, "start": 0, "end": 2})
	if err != nil {
		t.Fatal(err)
	}
	bytes, err := r.ExecuteTool(r.Seat().ID, "read_bytes", string(bytesArgs))
	if err != nil || bytes != "ONE" {
		t.Fatalf("bytes=%q err=%v", bytes, err)
	}
	patch := "*** Begin Patch\n*** Update File: " + filepath.ToSlash(path) + "\n@@\n-two\n+TWO\n*** End Patch"
	patchArgs, err := json.Marshal(map[string]string{"patch": patch})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.ExecuteTool(r.Seat().ID, "apply_patch", string(patchArgs)); err != nil {
		t.Fatal(err)
	}
	if content, err := os.ReadFile(path); err != nil || string(content) != "ONE\nTWO\n" {
		t.Fatalf("outside file content=%q err=%v", content, err)
	}
	cwdScript := "pwd"
	if runtime.GOOS == "windows" {
		cwdScript = "cd"
	}
	cwdArgs, err := json.Marshal(map[string]string{"script": cwdScript, "cwd": outsideDir})
	if err != nil {
		t.Fatal(err)
	}
	cwdOutput, err := r.ExecuteTool(r.Seat().ID, "quick_bash", string(cwdArgs))
	if err != nil || !strings.Contains(strings.ReplaceAll(cwdOutput, "\\", "/"), strings.ReplaceAll(filepath.Clean(outsideDir), "\\", "/")) {
		t.Fatalf("quick_bash cwd=%q err=%v", cwdOutput, err)
	}
	jobArgs, err := json.Marshal(map[string]any{"script": cwdScript, "cwd": outsideDir, "warn_after_seconds": 1})
	if err != nil {
		t.Fatal(err)
	}
	jobID, err := r.ExecuteTool(r.Seat().ID, "long_job", string(jobArgs))
	if err != nil {
		t.Fatal(err)
	}
	job, ok := r.Jobs().Get(jobID)
	if !ok {
		t.Fatalf("long_job %q was not registered", jobID)
	}
	select {
	case <-job.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("long_job did not finish")
	}
	jobOutput, _ := job.Output()
	if !strings.Contains(strings.ReplaceAll(jobOutput, "\\", "/"), strings.ReplaceAll(filepath.Clean(outsideDir), "\\", "/")) {
		t.Fatalf("long_job cwd=%q", jobOutput)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}

func TestProviderToolCallsExecuteAndContinue(t *testing.T) {
	r, err := New(config.Config{Home: t.TempDir(), SeatModel: "test", SeatEffort: "high"}, Options{Provider: func(string) (provider.Provider, error) { return toolProvider{}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	r.Seat().Send("use a tool")
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

func TestReasoningContentIsReplayedForToolContinuation(t *testing.T) {
	p := &reasoningToolProvider{}
	r, err := New(config.Config{Home: t.TempDir(), SeatModel: "test", SeatEffort: "high"}, Options{Provider: func(string) (provider.Provider, error) { return p, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := r.Seat().Send("use a tool"); err != nil {
		t.Fatal(err)
	}
	waitAgentTurn(t, r, r.Seat().ID)

	requests := p.snapshot()
	if len(requests) != 2 {
		t.Fatalf("provider requests = %d, want 2", len(requests))
	}
	var toolMessages []*provider.Message
	for i := range requests[1].Messages {
		message := &requests[1].Messages[i]
		if len(message.ToolCalls) > 0 {
			toolMessages = append(toolMessages, message)
		}
	}
	if len(toolMessages) != 2 {
		t.Fatalf("continuation omitted the assistant tool call: %#v", requests[1].Messages)
	}
	if toolMessages[0].Content != "before tool" || toolMessages[0].ReasoningContent != "thought" || toolMessages[1].ReasoningContent != "thought" {
		t.Fatalf("continuation lost assistant response state: %#v", toolMessages)
	}
}

func TestEmptyApprovalPolicyDoesNotCallProviderButExplicitLaunchModelWorks(t *testing.T) {
	providerCalls := 0
	r, err := New(config.Config{
		Home: t.TempDir(), SeatModel: "seat-model", SeatEffort: "high",
		SubagentModel: "default-child", SubagentEffort: "high", ApprovedModels: []string{},
	}, Options{Provider: func(string) (provider.Provider, error) {
		providerCalls++
		return fakeProvider{}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if got := r.Seat().Snapshot().Model; got != "" {
		t.Fatalf("seat model = %q, want fail-closed empty model", got)
	}
	child, err := r.LaunchSubagentSpec(r.Seat().ID, LaunchSpec{Title: "explicit", Model: "user-requested"})
	if err != nil {
		t.Fatal(err)
	}
	if got := child.Snapshot().Model; got != "user-requested" {
		t.Fatalf("explicit child model = %q, want user-requested", got)
	}
	if providerCalls != 0 {
		t.Fatalf("provider calls before any turn = %d, want zero", providerCalls)
	}
}

func TestLeafSubagentUsesLeafDefault(t *testing.T) {
	r, err := New(config.Config{
		Home: t.TempDir(), SeatModel: "seat", SeatEffort: "high",
		SubagentModel: "level-one", LeafModel: "level-two", SubagentEffort: "high",
	}, Options{Provider: func(string) (provider.Provider, error) { return fakeProvider{}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	levelOne, err := r.LaunchSubagentSpec(r.Seat().ID, LaunchSpec{Title: "level one"})
	if err != nil {
		t.Fatal(err)
	}
	levelTwo, err := r.LaunchSubagentSpec(levelOne.ID, LaunchSpec{Title: "level two"})
	if err != nil {
		t.Fatal(err)
	}
	if got := levelOne.Snapshot().Model; got != "level-one" {
		t.Fatalf("level-one model = %q, want level-one", got)
	}
	if got := levelTwo.Snapshot().Model; got != "level-two" {
		t.Fatalf("level-two model = %q, want level-two", got)
	}
}

func TestCodexLeafStartFailureDoesNotLeaveOrphanAgent(t *testing.T) {
	r, err := New(config.Config{Home: t.TempDir(), SeatModel: "seat"}, Options{
		Provider:     func(string) (provider.Provider, error) { return fakeProvider{}, nil },
		CodexCommand: filepath.Join(t.TempDir(), "missing-codex"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := r.LaunchSubagentSpec(r.Seat().ID, LaunchSpec{Title: "broken codex", Harness: "codex", Model: "gpt-test"}); err == nil {
		t.Fatal("missing Codex executable unexpectedly launched")
	}
	agents := r.Agents()
	if len(agents) != 1 || agents[0].Depth != 0 {
		t.Fatalf("failed Codex launch left agents behind: %#v", agents)
	}
}

func TestCodexLeafRequiresExplicitChatGPTModel(t *testing.T) {
	r, err := New(config.Config{
		Home: t.TempDir(), SeatModel: "native-seat", SubagentModel: "native-child", LeafModel: "native-leaf",
		ApprovedModels: []string{"native-seat", "native-child", "native-leaf"},
	}, Options{
		Provider:     func(string) (provider.Provider, error) { return fakeProvider{}, nil },
		CodexCommand: filepath.Join(t.TempDir(), "missing-codex"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := r.LaunchSubagentSpec(r.Seat().ID, LaunchSpec{Title: "missing model", Harness: "codex"}); err == nil || !strings.Contains(err.Error(), "explicit ChatGPT model") {
		t.Fatalf("Codex launch error = %v, want explicit-model validation", err)
	}
	levelOne, err := r.LaunchSubagentSpec(r.Seat().ID, LaunchSpec{Title: "native parent", Model: "native-child"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.LaunchSubagentSpec(levelOne.ID, LaunchSpec{Title: "missing model", Harness: "codex"}); err == nil || !strings.Contains(err.Error(), "explicit ChatGPT model") {
		t.Fatalf("level-one Codex launch error = %v, want explicit-model validation", err)
	}
	if got := len(r.Agents()); got != 2 {
		t.Fatalf("failed Codex validation left an agent behind: %d agents", got)
	}
}

func TestModelGuidanceDescribesCodexModelSelection(t *testing.T) {
	r := testRuntime(t)
	guidance := r.ModelGuidance()
	for _, want := range []string{"headless Codex app-server", `harness to "codex"`, "exact ChatGPT model slug", "independent of the native approval list"} {
		if !strings.Contains(guidance, want) {
			t.Fatalf("model guidance missing %q: %s", want, guidance)
		}
	}
}

func TestLaunchSubagentToolAdvertisesCodexModelFields(t *testing.T) {
	var launch provider.Tool
	for _, tool := range ToolDefinitions() {
		if tool.Name == "launch_subagent" {
			launch = tool
			break
		}
	}
	if launch.Name == "" {
		t.Fatal("launch_subagent tool is missing")
	}
	properties, ok := launch.Parameters["properties"].(map[string]any)
	if !ok {
		t.Fatalf("launch_subagent properties = %#v", launch.Parameters["properties"])
	}
	harness, ok := properties["harness"].(map[string]any)
	if !ok {
		t.Fatalf("harness schema = %#v", properties["harness"])
	}
	enum, ok := harness["enum"].([]string)
	if !ok || len(enum) != 2 || enum[0] != "native" || enum[1] != "codex" {
		t.Fatalf("harness enum = %#v", harness["enum"])
	}
	model, ok := properties["model"].(map[string]any)
	description, _ := model["description"].(string)
	if !ok || !strings.Contains(description, "exact ChatGPT model slug") {
		t.Fatalf("model schema = %#v", properties["model"])
	}
}

func TestChildResultReachesParent(t *testing.T) {
	r, err := New(config.Config{Home: t.TempDir(), SeatModel: "test", SeatEffort: "high", SubagentModel: "test-child", SubagentEffort: "high"}, Options{Provider: func(string) (provider.Provider, error) { return childResultProvider{}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	r.Seat().Send("ask a child")
	deadline := time.After(5 * time.Second)
	var sawParentResult bool
	var childResultTitle string
	for !sawParentResult {
		select {
		case event := <-r.Events():
			if event.AgentID == r.Seat().ID && event.Kind == "child_result" {
				childResultTitle = event.AgentTitle
			}
			if event.AgentID == r.Seat().ID && event.Kind == "assistant" && event.Text == "parent saw child" {
				sawParentResult = true
			}
		case <-deadline:
			t.Fatal("parent never received the child result")
		}
	}
	if childResultTitle != "child" {
		t.Fatalf("child result event title = %q, want child", childResultTitle)
	}

	waitAgentTurn(t, r, r.Seat().ID)
	var resultCount int
	for _, message := range r.Seat().History() {
		if strings.HasPrefix(message.Content, "[result from child] child answer") {
			resultCount++
		}
	}
	if resultCount != 1 {
		t.Fatalf("parent history has %d child results, want one: %#v", resultCount, r.Seat().History())
	}
	transcriptPath, err := r.TranscriptPath(r.Seat().ID)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := logx.Read(transcriptPath)
	if err != nil {
		t.Fatal(err)
	}
	var sawChildResult bool
	for _, entry := range entries {
		if entry.Kind == "child_result" && entry.Text == "child answer" {
			sawChildResult = true
			break
		}
	}
	if !sawChildResult {
		t.Fatalf("parent transcript missing child result: %#v", entries)
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
