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
	"github.com/slbdotdev/slbh/internal/job"
	"github.com/slbdotdev/slbh/internal/logx"
	"github.com/slbdotdev/slbh/internal/provider"
	"github.com/slbdotdev/slbh/internal/seam"
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

type blockingProvider struct {
	started chan struct{}
	once    sync.Once
}

func (p *blockingProvider) Stream(ctx context.Context, _ provider.Request, _ provider.StreamSink) error {
	p.once.Do(func() { close(p.started) })
	<-ctx.Done()
	return ctx.Err()
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
	if request.Model == "test-manager" {
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

type requestCaptureProvider struct{ requests chan provider.Request }

func (p requestCaptureProvider) Stream(_ context.Context, request provider.Request, sink provider.StreamSink) error {
	p.requests <- request
	if err := sink(provider.Event{Kind: provider.EventText, Text: "captured"}); err != nil {
		return err
	}
	return sink(provider.Event{Kind: provider.EventDone})
}

// testRuntime runs the full tool shape, so the tests below can reach every
// native tool; the default shape is a subset of it.
func testRuntime(t *testing.T) *Runtime {
	t.Helper()
	r, err := New(config.Config{Home: t.TempDir(), SeatModel: "test", SeatEffort: "high", SubagentModel: "test-child", SubagentEffort: "high", ToolShape: config.ToolShapeFull}, Options{Provider: func(string) (provider.Provider, error) { return fakeProvider{}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

var testEventStreams sync.Map

// testEvents adapts the serializable polling contract for older behavioral
// tests whose assertion shape is naturally a select on a local test channel.
// The channel belongs to the test process; only EventQuery and EventBatch
// cross the seam.
func testEvents(runtime seam.Runtime) <-chan seam.Event {
	created := make(chan seam.Event, 1024)
	actual, loaded := testEventStreams.LoadOrStore(runtime.ID(), created)
	if loaded {
		return actual.(chan seam.Event)
	}
	go func() {
		defer close(created)
		defer testEventStreams.Delete(runtime.ID())
		var cursor seam.EventCursor
		for {
			batch := runtime.PollEvents(seam.EventQuery{After: cursor, Limit: 128, WaitMilliseconds: 100})
			cursor = batch.Cursor
			for _, event := range batch.Events {
				created <- event
			}
			if batch.End {
				return
			}
		}
	}()
	return created
}

func launchTestManager(t *testing.T, r *Runtime) *Agent {
	t.Helper()
	manager, err := r.launchSubagentSpec(r.seat().ID, LaunchSpec{Title: "native-manager"})
	if err != nil {
		t.Fatal(err)
	}
	return manager
}

func TestAgentSnapshotJSONRoundTrip(t *testing.T) {
	want := seam.AgentSnapshot{
		ID:              "agent-1234",
		Title:           "reviewer",
		ParentID:        "agent-parent",
		Depth:           2,
		Model:           "gpt-test",
		Effort:          "high",
		Status:          "thinking",
		Harness:         "codex",
		WorkDir:         "/tmp/work",
		ContextWindow:   128000,
		ContextUsed:     12000,
		CacheHitTokens:  7000,
		CacheMissTokens: 5000,
	}
	data, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var got seam.AgentSnapshot
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("seam.AgentSnapshot round trip = %#v, want %#v", got, want)
	}
	for _, key := range []string{"id", "title", "parent_id", "depth", "model", "effort", "status", "harness", "work_dir", "context_window", "context_used", "cache_hit_tokens", "cache_miss_tokens"} {
		if !strings.Contains(string(data), `"`+key+`"`) {
			t.Fatalf("seam.AgentSnapshot JSON missing %q: %s", key, data)
		}
	}
}

func TestJobSnapshotJSONRoundTrip(t *testing.T) {
	want := seam.JobSnapshot{
		ID:          "job-1234",
		Author:      "agent-1234",
		Script:      "printf output",
		ToolName:    "bash",
		Status:      "complete",
		Started:     time.Date(2026, 9, 18, 1, 2, 3, 0, time.UTC),
		Finished:    time.Date(2026, 9, 18, 1, 2, 4, 0, time.UTC),
		ExitCode:    0,
		StdoutBytes: 6,
		StderrBytes: 2,
		WarnAfter:   30 * time.Second,
	}
	data, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var got seam.JobSnapshot
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("seam.JobSnapshot round trip = %#v, want %#v", got, want)
	}
	for _, key := range []string{"id", "author", "script", "tool_name", "status", "started", "finished", "exit_code", "stdout_bytes", "stderr_bytes", "warn_after"} {
		if !strings.Contains(string(data), `"`+key+`"`) {
			t.Fatalf("seam.JobSnapshot JSON missing %q: %s", key, data)
		}
	}
}

func TestJobSnapshotsReturnsCopies(t *testing.T) {
	r := testRuntime(t)
	seat := r.seat()
	started, err := r.jobs.Start(context.Background(), job.Spec{
		Author:   seat.ID,
		Script:   "printf output",
		Command:  []string{"sh", "-c", "printf output"},
		ToolName: "bash",
	})
	if err != nil {
		t.Fatal(err)
	}
	<-started.Done()

	first := r.JobSnapshots()
	if len(first) != 1 {
		t.Fatalf("JobSnapshots length = %d, want 1", len(first))
	}
	original := first[0]
	first[0] = seam.JobSnapshot{ID: "mutated"}
	first = append(first, seam.JobSnapshot{ID: "invented"})

	second := r.JobSnapshots()
	if len(second) != 1 || second[0] != original {
		t.Fatalf("mutating returned snapshots changed runtime jobs: %#v", second)
	}
}

func TestRuntimeSetAgentEffort(t *testing.T) {
	r := testRuntime(t)
	seat := r.seat()

	if err := r.setAgentEffort(seat.ID, "medium"); err != nil {
		t.Fatal(err)
	}
	if got := seat.Snapshot().Effort; got != "medium" {
		t.Fatalf("SetAgentEffort set effort %q, want medium", got)
	}
}

func TestRuntimeSendPrompt(t *testing.T) {
	r := testRuntime(t)
	seat := r.seat()

	if err := r.sendPrompt(seat.ID, "first prompt"); err != nil {
		t.Fatal(err)
	}
	waitAgentTurn(t, r, seat.ID)
	history := seat.History()
	for _, message := range history {
		if message.Content == "first prompt" {
			return
		}
	}
	t.Fatalf("SendPrompt did not deliver the prompt: %#v", history)
}

func TestRuntimeSteerAgent(t *testing.T) {
	r := testRuntime(t)
	seat := r.seat()

	if err := r.steerAgent(seat.ID, "new direction"); err != nil {
		t.Fatal(err)
	}
	waitAgentTurn(t, r, seat.ID)
	history := seat.History()
	for _, message := range history {
		if message.Content == "[steer] new direction" {
			return
		}
	}
	t.Fatalf("SteerAgent did not deliver the steer: %#v", history)
}

func TestRuntimeStreamsAndLogs(t *testing.T) {
	r := testRuntime(t)
	if got := len(strings.TrimPrefix(r.ID(), "run-")); got != 8 {
		t.Fatalf("runtime ID suffix length = %d, want 8", got)
	}
	if got := len(strings.TrimPrefix(r.seat().ID, "agent-")); got != 8 {
		t.Fatalf("agent ID suffix length = %d, want 8", got)
	}
	r.seat().Send("hello")
	deadline := time.After(5 * time.Second)
	var sawDone bool
	for !sawDone {
		select {
		case event := <-testEvents(r):
			if event.Kind == "turn_done" {
				sawDone = true
			}
		case <-deadline:
			t.Fatal("agent turn did not finish")
		}
	}
	transcriptPath, err := r.TranscriptPath(r.seat().ID)
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

func TestRuntimeDoesNotDropStreamEventsWhenUIFallsBehind(t *testing.T) {
	r := testRuntime(t)
	baseline := r.PollEvents(seam.EventQuery{}).Cursor
	const count = 4096
	for i := 0; i < count; i++ {
		r.emit(seam.Event{AgentID: r.seat().ID, AgentTitle: "seat", Kind: "assistant", Text: "stream-event"})
	}
	batch := r.PollEvents(seam.EventQuery{After: baseline})
	if len(batch.Events) != count {
		t.Fatalf("received %d/%d stream events", len(batch.Events), count)
	}
}

func TestRuntimeCloseDeliversStoppingEventThenClosesStream(t *testing.T) {
	r := testRuntime(t)
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	batch := r.PollEvents(seam.EventQuery{})
	if !batch.End || len(batch.Events) == 0 {
		t.Fatalf("closed stream batch = %#v", batch)
	}
	last := batch.Events[len(batch.Events)-1]
	if last.Kind != "runtime" || last.Text != "runtime stopping" {
		t.Fatalf("last event = %#v, want runtime stopping", last)
	}
}

func TestRuntimeCloseReturnsAndClosesFullUnreadStream(t *testing.T) {
	r, err := New(
		config.Config{Home: t.TempDir(), SeatModel: "test", SeatEffort: "high"},
		Options{Provider: func(string) (provider.Provider, error) { return fakeProvider{}, nil }},
	)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4096; i++ {
		r.emit(seam.Event{Kind: "status", Text: "fill"})
	}

	started := time.Now()
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed >= 2*time.Second {
		t.Fatalf("Close took %v with a full unread stream", elapsed)
	}
	batch := r.PollEvents(seam.EventQuery{})
	if !batch.End || len(batch.Events) != 4098 {
		t.Fatalf("closed unread stream = %d events, end=%v; want 4098 and true", len(batch.Events), batch.End)
	}
}

func TestRuntimeEmitAfterCloseIsNoOp(t *testing.T) {
	r := testRuntime(t)
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	final := r.PollEvents(seam.EventQuery{})
	r.emit(seam.Event{Kind: "status", Text: "after close"})
	if after := r.PollEvents(seam.EventQuery{After: final.Cursor}); len(after.Events) != 0 || !after.End {
		t.Fatalf("event stream changed after Close: %#v", after)
	}
}

func TestAgentSessionsHaveSeparateTranscriptsAndClearRotatesSelectedAgent(t *testing.T) {
	r := testRuntime(t)
	seat := r.seat()
	firstPath, err := r.TranscriptPath(seat.ID)
	if err != nil {
		t.Fatal(err)
	}
	child, err := r.launchSubagentSpec(seat.ID, LaunchSpec{Title: "child"})
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

	if err := r.clear(seat.ID); err != nil {
		t.Fatal(err)
	}
	secondPath, err := r.TranscriptPath(seat.ID)
	if err != nil {
		t.Fatal(err)
	}
	if firstPath == secondPath {
		t.Fatalf("clear reused transcript path %q", secondPath)
	}
	if got := r.seat().ID; got != seat.ID {
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
		case event := <-testEvents(r):
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
	r.seat().Send("hello")
	deadline := time.After(5 * time.Second)
	for {
		select {
		case event := <-testEvents(r):
			if event.Kind == "turn_done" {
				snapshot := r.seat().Snapshot()
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
	child := launchTestManager(t, r)
	grandchild, err := r.launchSubagentSpec(child.ID, LaunchSpec{Title: "grandchild", Model: "test-leaf", Brief: "brief"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.launchSubagentSpec(grandchild.ID, LaunchSpec{Title: "too-deep", Model: "test-leaf", Brief: "brief"}); err == nil {
		t.Fatal("expected depth error")
	}
	child.Steer("change emphasis")
	deadline := time.After(5 * time.Second)
	for {
		select {
		case event := <-testEvents(r):
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
	r.seat().WorkDir = workDir
	path := filepath.Join(outsideDir, "nested", "file.txt")
	writeArgs, err := json.Marshal(map[string]string{"path": path, "content": "one\ntwo\n"})
	if _, err := r.ExecuteTool(r.seat().ID, "write_file", string(writeArgs)); err != nil {
		t.Fatal(err)
	}
	globArgs, err := json.Marshal(map[string]string{"pattern": filepath.Join(outsideDir, "nested", "*.txt")})
	if err != nil {
		t.Fatal(err)
	}
	matches, err := r.ExecuteTool(r.seat().ID, "glob", string(globArgs))
	if err != nil || !strings.Contains(matches, filepath.Clean(path)) {
		t.Fatalf("glob=%q err=%v", matches, err)
	}
	grepArgs, err := json.Marshal(map[string]string{"pattern": "two", "path": path})
	if err != nil {
		t.Fatal(err)
	}
	grep, err := r.ExecuteTool(r.seat().ID, "grep", string(grepArgs))
	if err != nil || !strings.Contains(grep, filepath.Clean(path)+":2:two") {
		t.Fatalf("grep=%q err=%v", grep, err)
	}
	readLinesArgs, err := json.Marshal(map[string]any{"path": path, "start": 2, "end": 2})
	if err != nil {
		t.Fatal(err)
	}
	lines, err := r.ExecuteTool(r.seat().ID, "read_lines", string(readLinesArgs))
	if err != nil || !strings.Contains(lines, "2:two") {
		t.Fatalf("lines=%q err=%v", lines, err)
	}
	editArgs, err := json.Marshal(map[string]string{"path": path, "old": "one", "new": "ONE"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.ExecuteTool(r.seat().ID, "edit_file", string(editArgs)); err != nil {
		t.Fatal(err)
	}
	readArgs, err := json.Marshal(map[string]string{"path": path})
	if err != nil {
		t.Fatal(err)
	}
	content, err := r.ExecuteTool(r.seat().ID, "read_file", string(readArgs))
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
	if relativeContent, err := r.ExecuteTool(r.seat().ID, "read_file", string(relativeReadArgs)); err != nil || relativeContent != content {
		t.Fatalf("relative content=%q err=%v", relativeContent, err)
	}
	bytesArgs, err := json.Marshal(map[string]any{"path": path, "start": 0, "end": 2})
	if err != nil {
		t.Fatal(err)
	}
	bytes, err := r.ExecuteTool(r.seat().ID, "read_bytes", string(bytesArgs))
	if err != nil || bytes != "ONE" {
		t.Fatalf("bytes=%q err=%v", bytes, err)
	}
	patch := "*** Begin Patch\n*** Update File: " + filepath.ToSlash(path) + "\n@@\n-two\n+TWO\n*** End Patch"
	patchArgs, err := json.Marshal(map[string]string{"patch": patch})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.ExecuteTool(r.seat().ID, "apply_patch", string(patchArgs)); err != nil {
		t.Fatal(err)
	}
	if content, err := os.ReadFile(path); err != nil || string(content) != "ONE\nTWO\n" {
		t.Fatalf("outside file content=%q err=%v", content, err)
	}
	cwdScript := "pwd"
	if runtime.GOOS == "windows" {
		cwdScript = "pwd -W"
	}
	cwdArgs, err := json.Marshal(map[string]string{"script": cwdScript, "cwd": outsideDir})
	if err != nil {
		t.Fatal(err)
	}
	cwdOutput, err := r.ExecuteTool(r.seat().ID, "bash", string(cwdArgs))
	if err != nil || !strings.Contains(strings.ReplaceAll(cwdOutput, "\\", "/"), strings.ReplaceAll(filepath.Clean(outsideDir), "\\", "/")) {
		t.Fatalf("bash cwd=%q err=%v", cwdOutput, err)
	}
	jobArgs, err := json.Marshal(map[string]any{"script": cwdScript, "cwd": outsideDir, "wait_seconds": 0, "warn_after_seconds": 1})
	if err != nil {
		t.Fatal(err)
	}
	jobResult, err := r.ExecuteTool(r.seat().ID, "bash", string(jobArgs))
	if err != nil {
		t.Fatal(err)
	}
	jobID := strings.Fields(jobResult)[1]
	job, ok := r.jobs.Get(jobID)
	if !ok {
		t.Fatalf("bash %q was not registered", jobID)
	}
	select {
	case <-job.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("bash did not finish")
	}
	jobOutput, _ := job.Output()
	if !strings.Contains(strings.ReplaceAll(jobOutput, "\\", "/"), strings.ReplaceAll(filepath.Clean(outsideDir), "\\", "/")) {
		t.Fatalf("bash cwd=%q", jobOutput)
	}
	pythonScript := `import os; print(os.getcwd())`
	pythonArgs, err := json.Marshal(map[string]string{"script": pythonScript, "cwd": outsideDir})
	if err != nil {
		t.Fatal(err)
	}
	pythonOutput, err := r.ExecuteTool(r.seat().ID, "python", string(pythonArgs))
	if err != nil || !strings.Contains(strings.ReplaceAll(pythonOutput, "\\", "/"), strings.ReplaceAll(filepath.Clean(outsideDir), "\\", "/")) {
		t.Fatalf("python cwd=%q err=%v", pythonOutput, err)
	}
	pythonJobArgs, err := json.Marshal(map[string]any{"script": `print("python job")`, "cwd": outsideDir, "wait_seconds": 0, "warn_after_seconds": 1})
	if err != nil {
		t.Fatal(err)
	}
	pythonJobResult, err := r.ExecuteTool(r.seat().ID, "python", string(pythonJobArgs))
	if err != nil {
		t.Fatal(err)
	}
	pythonJobID := strings.Fields(pythonJobResult)[1]
	pythonJob, ok := r.jobs.Get(pythonJobID)
	if !ok {
		t.Fatalf("python %q was not registered", pythonJobID)
	}
	select {
	case <-pythonJob.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("python did not finish")
	}
	pythonJobOutput, _ := pythonJob.Output()
	if !strings.Contains(pythonJobOutput, "python job") {
		t.Fatalf("python output=%q", pythonJobOutput)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}

// TestJobReadReturnsOutputOfRunningJob walks the tool path the defect was
// reported on rather than the package under it: bash, then job read while
// the job is still running. Until 2026-09-15 that answered
// {"stdout":"","stderr":""}, because Job.Output read buffers the wait goroutine
// filled only once the job had ended — so the one tool an agent has for looking
// at a stalled job showed it nothing.
func TestJobReadReturnsOutputOfRunningJob(t *testing.T) {
	r := testRuntime(t)
	script := "echo live-marker; sleep 30"
	// An hour of warn_after keeps the warning timer out of this test; the
	// warning is a separate mechanism with its own tests.
	jobArgs, err := json.Marshal(map[string]any{"script": script, "wait_seconds": 0, "warn_after_seconds": 3600})
	if err != nil {
		t.Fatal(err)
	}
	jobResult, err := r.ExecuteTool(r.seat().ID, "bash", string(jobArgs))
	if err != nil {
		t.Fatal(err)
	}
	jobID := strings.Fields(jobResult)[1]
	job, ok := r.jobs.Get(jobID)
	if !ok {
		t.Fatalf("bash %q was not registered", jobID)
	}
	var payload struct {
		Stdout string `json:"stdout"`
		Stderr string `json:"stderr"`
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		raw, err := r.ExecuteTool(r.seat().ID, "job", string(`{"action":"read","job_id":"`+jobID+`"}`))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(raw), &payload); err != nil {
			t.Fatalf("job read returned %q: %v", raw, err)
		}
		if strings.Contains(payload.Stdout, "live-marker") {
			break
		}
		select {
		case <-job.Done():
			t.Fatal("job finished before job read could see it running")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if !strings.Contains(payload.Stdout, "live-marker") {
		t.Fatalf("job read on a running job returned stdout=%q stderr=%q", payload.Stdout, payload.Stderr)
	}
	select {
	case <-job.Done():
		t.Fatal("job was no longer running when job read answered")
	default:
	}
	if _, err := r.ExecuteTool(r.seat().ID, "job", string(`{"action":"kill","job_id":"`+jobID+`"}`)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-job.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("killed job did not finish")
	}
}

func TestProviderToolCallsExecuteAndContinue(t *testing.T) {
	r, err := New(config.Config{Home: t.TempDir(), SeatModel: "test", SeatEffort: "high"}, Options{Provider: func(string) (provider.Provider, error) { return toolProvider{}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	r.seat().Send("use a tool")
	deadline := time.After(5 * time.Second)
	var sawResult, sawDone bool
	for !sawDone {
		select {
		case event := <-testEvents(r):
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
	if err := r.seat().Send("use a tool"); err != nil {
		t.Fatal(err)
	}
	waitAgentTurn(t, r, r.seat().ID)

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

func TestDepthOneModelDoesNotUseApplicationApprovalDefaults(t *testing.T) {
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
	if got := r.seat().Snapshot().Model; got != "" {
		t.Fatalf("seat model = %q, want fail-closed empty model", got)
	}
	child, err := r.launchSubagentSpec(r.seat().ID, LaunchSpec{Title: "manager"})
	if err != nil {
		t.Fatal(err)
	}
	if got := child.Snapshot().Model; got != "default-child" {
		t.Fatalf("child model = %q, want configured default-child", got)
	}
	if providerCalls != 0 {
		t.Fatalf("provider calls before any turn = %d, want zero", providerCalls)
	}
}

func TestNativeLeafRequiresAndUsesExplicitLaunchModel(t *testing.T) {
	requests := make(chan provider.Request, 1)
	r, err := New(config.Config{
		Home: t.TempDir(), SeatModel: "seat", SeatEffort: "high",
		SubagentModel: "level-one", LeafModel: "level-two", SubagentEffort: "high",
	}, Options{Provider: func(model string) (provider.Provider, error) {
		if model == "explicit-flex" {
			return requestCaptureProvider{requests: requests}, nil
		}
		return fakeProvider{}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	levelOne, err := r.launchSubagentSpec(r.seat().ID, LaunchSpec{Title: "level one"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.launchSubagentSpec(levelOne.ID, LaunchSpec{Title: "missing model"}); err == nil || !strings.Contains(err.Error(), "non-empty explicit model") || !strings.Contains(err.Error(), "defaults are not used") {
		t.Fatalf("native leaf launch error = %v, want explicit-model refusal", err)
	}
	levelTwo, err := r.launchSubagentSpec(levelOne.ID, LaunchSpec{Title: "level two", Model: "explicit-flex", Effort: "xhigh", Brief: "capture the request"})
	if err != nil {
		t.Fatal(err)
	}
	if got := levelOne.Snapshot().Model; got != "level-one" {
		t.Fatalf("level-one model = %q, want configured level-one", got)
	}
	if got := levelTwo.Snapshot().Model; got != "explicit-flex" {
		t.Fatalf("level-two model = %q, want explicit-flex", got)
	}
	if got := levelTwo.Snapshot().Effort; got != "xhigh" {
		t.Fatalf("level-two effort = %q, want per-launch xhigh", got)
	}
	var recorded bool
	for _, event := range r.PollEvents(seam.EventQuery{}).Events {
		if event.AgentID == levelTwo.ID && event.Kind == "status" && event.Text == "subagent launched" {
			recorded = event.Metadata["depth"] == float64(2) && event.Metadata["model"] == "explicit-flex" && event.Metadata["effort"] == "xhigh"
		}
	}
	if !recorded {
		t.Fatal("launch event did not record depth, model, and effort")
	}
	select {
	case request := <-requests:
		if request.Model != "explicit-flex" {
			t.Fatalf("native provider request model = %q, want explicit-flex", request.Model)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("native leaf did not reach its provider")
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
	manager := launchTestManager(t, r)
	if _, err := r.launchSubagentSpec(manager.ID, LaunchSpec{Title: "broken codex", Harness: "codex", Model: "gpt-5.6-luna"}); err == nil {
		t.Fatal("missing Codex executable unexpectedly launched")
	}
	agents := r.Agents()
	if len(agents) != 2 || agents[0].Depth != 0 || agents[1].Depth != 1 {
		t.Fatalf("failed Codex launch left agents behind: %#v", agents)
	}
}

func TestSeatCanLaunchWithoutNamedRole(t *testing.T) {
	r := testRuntime(t)
	input, err := json.Marshal(map[string]string{"title": "direct-child", "harness": "native", "model": "custom-child", "brief": "work"})
	if err != nil {
		t.Fatal(err)
	}
	id, err := r.ExecuteTool(r.seat().ID, "launch_subagent", string(input))
	if err != nil {
		t.Fatal(err)
	}
	for _, agent := range r.Agents() {
		if agent.ID == id && agent.Depth == 1 && agent.Model == "custom-child" && agent.Harness == "native" {
			return
		}
	}
	t.Fatalf("direct child %q not found in %#v", id, r.Agents())
}

func TestManagerLeafLaunchRequiresExplicitModelForEveryHarness(t *testing.T) {
	r := testRuntime(t)
	manager := launchTestManager(t, r)
	for _, harness := range []string{
		"native", "codex", "claude_code",
	} {
		input, err := json.Marshal(map[string]string{"title": "missing model", "harness": harness, "brief": "work"})
		if err != nil {
			t.Fatal(err)
		}
		if _, launchErr := r.ExecuteTool(manager.ID, "launch_subagent", string(input)); launchErr == nil || !strings.Contains(launchErr.Error(), "non-empty explicit model") || !strings.Contains(launchErr.Error(), "defaults are not used") {
			t.Fatalf("%s leaf launch error = %v, want explicit-model refusal", harness, launchErr)
		}
	}
	if got := len(r.Agents()); got != 2 {
		t.Fatalf("failed leaf validation left an agent behind: %d agents", got)
	}
}

func TestRoleFreeLaunchValidation(t *testing.T) {
	r := testRuntime(t)
	seat := r.seat()
	for _, test := range []struct {
		name string
		spec LaunchSpec
		want string
	}{
		{name: "unsupported harness", spec: LaunchSpec{Title: "unknown", Harness: "other"}, want: "unsupported child harness"},
		{name: "negative warning", spec: LaunchSpec{Title: "child", WarnAfterSeconds: -1}, want: "warn_after_seconds must not be negative"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := r.launchSubagentSpec(seat.ID, test.spec); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("launch error = %v, want %q", err, test.want)
			}
		})
	}

	manager := launchTestManager(t, r)
	for _, test := range []struct {
		name string
		spec LaunchSpec
		want string
	}{
		{name: "unsupported harness", spec: LaunchSpec{Title: "child", Harness: "other", Model: "vendor/model"}, want: "unsupported child harness"},
		{name: "missing leaf model", spec: LaunchSpec{Title: "child"}, want: "non-empty explicit model"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := r.launchSubagentSpec(manager.ID, test.spec); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("launch error = %v, want %q", err, test.want)
			}
		})
	}
	if got := len(r.Agents()); got != 2 {
		t.Fatalf("rejected launches left %d agents, want Seat and Manager", got)
	}
}

func TestSubagentWarningReachesParentOnce(t *testing.T) {
	blocker := &blockingProvider{started: make(chan struct{})}
	r, err := New(config.Config{Home: t.TempDir(), SeatModel: "test", SeatEffort: "high", SubagentModel: "test-manager"}, Options{
		Provider: func(model string) (provider.Provider, error) {
			if model == "test-manager" {
				return blocker, nil
			}
			return fakeProvider{}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	parent := r.seat()
	child, err := r.launchSubagentSpec(parent.ID, LaunchSpec{Title: "slow-manager", Brief: "work", WarnAfterSeconds: 1})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-blocker.started:
	case <-time.After(2 * time.Second):
		t.Fatal("child provider did not start")
	}
	cursor := r.PollEvents(seam.EventQuery{}).Cursor
	deadline := time.Now().Add(3 * time.Second)
	warnings := 0
	for time.Now().Before(deadline) && warnings == 0 {
		batch := r.PollEvents(seam.EventQuery{After: cursor, WaitMilliseconds: 100})
		cursor = batch.Cursor
		for _, event := range batch.Events {
			if event.AgentID == parent.ID && event.Kind == "subagent_warning" {
				warnings++
				if !strings.Contains(event.Text, child.ID) || !strings.Contains(event.Text, "end_subagent") {
					t.Fatalf("warning = %q, want child id and end_subagent guidance", event.Text)
				}
			}
		}
	}
	if warnings != 1 {
		t.Fatalf("subagent warnings = %d, want one", warnings)
	}
	time.Sleep(1200 * time.Millisecond)
	batch := r.PollEvents(seam.EventQuery{After: cursor})
	for _, event := range batch.Events {
		if event.AgentID == parent.ID && event.Kind == "subagent_warning" {
			t.Fatal("subagent warning repeated")
		}
	}
}

func TestSubagentWarningCancelsAfterChildResult(t *testing.T) {
	r := testRuntime(t)
	parent := r.seat()
	child, err := r.launchSubagentSpec(parent.ID, LaunchSpec{Title: "fast-manager", Brief: "work", WarnAfterSeconds: 1})
	if err != nil {
		t.Fatal(err)
	}
	cursor := r.PollEvents(seam.EventQuery{}).Cursor
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		batch := r.PollEvents(seam.EventQuery{After: cursor, WaitMilliseconds: 100})
		cursor = batch.Cursor
		for _, event := range batch.Events {
			if event.AgentID == child.ID && event.Kind == "turn_done" {
				time.Sleep(1200 * time.Millisecond)
				for _, later := range r.PollEvents(seam.EventQuery{After: cursor}).Events {
					if later.Kind == "subagent_warning" {
						t.Fatal("warning fired after child result")
					}
				}
				return
			}
		}
	}
	t.Fatal("child did not finish before warning interval")
}

func TestLaunchWorksWithoutNamedCatalogue(t *testing.T) {
	r := testRuntime(t)
	child, err := r.launchSubagentSpec(r.seat().ID, LaunchSpec{Title: "child"})
	if err != nil {
		t.Fatal(err)
	}
	if child.Depth != 1 || child.Model != "test-child" {
		t.Fatalf("child = %#v, want depth 1 and configured model", child.Snapshot())
	}
}

func TestModelGuidanceDescribesExternalLeafModelSelection(t *testing.T) {
	r := testRuntime(t)
	guidance := r.ModelGuidance()
	for _, want := range []string{"do not use named roles", "depth-limited to two levels", "depth-2 launch must pass a non-empty model", "configured child default"} {
		if !strings.Contains(guidance, want) {
			t.Fatalf("model guidance missing %q: %s", want, guidance)
		}
	}
}

func TestLaunchSubagentToolAdvertisesLeafHarnessModelFields(t *testing.T) {
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
	required, ok := launch.Parameters["required"].([]string)
	if !ok || !containsString(required, "title") || !containsString(required, "brief") || containsString(required, "role") {
		t.Fatalf("launch required fields = %#v", launch.Parameters["required"])
	}
	harness, ok := properties["harness"].(map[string]any)
	if !ok {
		t.Fatalf("harness schema = %#v", properties["harness"])
	}
	enum, ok := harness["enum"].([]string)
	if !ok || len(enum) != 3 || enum[0] != "native" || enum[1] != "codex" || enum[2] != "claude_code" {
		t.Fatalf("harness enum = %#v", harness["enum"])
	}
	model, ok := properties["model"].(map[string]any)
	description, _ := model["description"].(string)
	if !ok || !strings.Contains(description, "Real provider model ID") || !strings.Contains(description, "depth-2 child") {
		t.Fatalf("model schema = %#v", properties["model"])
	}
	for _, want := range []string{"depths", "depth-2 child", "optional harness", "Do not wait or poll"} {
		if !strings.Contains(launch.Description, want) {
			t.Fatalf("launch_subagent description missing %q: %s", want, launch.Description)
		}
	}
	r := testRuntime(t)
	if len(r.toolDefinitions(r.seat().ID)) == 0 {
		t.Fatal("seat lost launch_subagent tool")
	}
	manager := launchTestManager(t, r)
	if len(r.toolDefinitions(manager.ID)) == 0 {
		t.Fatal("depth-1 child lost launch_subagent tool")
	}
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func TestChildResultReachesParent(t *testing.T) {
	r, err := New(config.Config{Home: t.TempDir(), SeatModel: "test", SeatEffort: "high", SubagentModel: "test-manager", SubagentEffort: "high"}, Options{Provider: func(string) (provider.Provider, error) { return childResultProvider{}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	r.seat().Send("ask a child")
	deadline := time.After(5 * time.Second)
	var sawParentResult bool
	var childResultTitle string
	for !sawParentResult {
		select {
		case event := <-testEvents(r):
			if event.AgentID == r.seat().ID && event.Kind == "child_result" {
				childResultTitle = event.AgentTitle
			}
			if event.AgentID == r.seat().ID && event.Kind == "assistant" && event.Text == "parent saw child" {
				sawParentResult = true
			}
		case <-deadline:
			t.Fatal("parent never received the child result")
		}
	}
	if childResultTitle != "child" {
		t.Fatalf("child result event title = %q, want child", childResultTitle)
	}

	waitAgentTurn(t, r, r.seat().ID)
	var resultCount int
	for _, message := range r.seat().History() {
		if strings.HasPrefix(message.Content, "[result from child] child answer") {
			resultCount++
		}
	}
	if resultCount != 1 {
		t.Fatalf("parent history has %d child results, want one: %#v", resultCount, r.seat().History())
	}
	transcriptPath, err := r.TranscriptPath(r.seat().ID)
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

// lengthProvider ends its generation at the output bound, as a runaway local
// generation does when maxOutputTokens stops it.
type lengthProvider struct{ reason string }

func (p lengthProvider) Stream(_ context.Context, _ provider.Request, sink provider.StreamSink) error {
	if err := sink(provider.Event{Kind: provider.EventText, Text: "partial"}); err != nil {
		return err
	}
	return sink(provider.Event{Kind: provider.EventUsage, StopReason: p.reason, Usage: map[string]any{"completion_tokens": 32768}})
}

func TestOutputLimitStopIsAWarningInTheTranscript(t *testing.T) {
	for _, tc := range []struct {
		reason string
		warn   bool
	}{{"length", true}, {"max_tokens", true}, {"stop", false}} {
		t.Run(tc.reason, func(t *testing.T) {
			r, err := New(config.Config{Home: t.TempDir(), SeatModel: "test", SeatEffort: "high"}, Options{Provider: func(string) (provider.Provider, error) { return lengthProvider{reason: tc.reason}, nil }})
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			seat := r.seat()
			seat.Send("hello")
			warned := false
			deadline := time.After(5 * time.Second)
		wait:
			for {
				select {
				case event := <-testEvents(r):
					if event.Kind == "warning" {
						warned = true
						if !strings.Contains(event.Text, "output limit") || !strings.Contains(event.Text, "32768") || event.Metadata["stop_reason"] != tc.reason {
							t.Fatalf("warning = %#v", event)
						}
					}
					if event.Kind == "turn_done" {
						break wait
					}
				case <-deadline:
					t.Fatal("agent turn did not finish")
				}
			}
			if warned != tc.warn {
				t.Fatalf("stop reason %q warned = %v, want %v", tc.reason, warned, tc.warn)
			}
			path, err := r.TranscriptPath(seat.ID)
			if err != nil {
				t.Fatal(err)
			}
			entries, err := logx.Read(path)
			if err != nil {
				t.Fatal(err)
			}
			inTranscript := false
			for _, entry := range entries {
				if entry.Kind == "warning" {
					inTranscript = true
				}
			}
			if inTranscript != tc.warn {
				t.Fatalf("stop reason %q transcript warning = %v, want %v", tc.reason, inTranscript, tc.warn)
			}
		})
	}
}

// A route's defaultEffort outranks the model-agnostic seat and child defaults
// wherever an agent is put on that route, and yields to an effort named for
// the placement.
func TestRouteDefaultEffortAppliesOnPlacement(t *testing.T) {
	policy := provider.Policy{Version: provider.PolicyVersion, Routes: map[string]provider.RoutePolicy{
		"test-leaf": {DefaultEffort: "low"},
	}}
	r, err := New(config.Config{Home: t.TempDir(), SeatModel: "test", SeatEffort: "high", SubagentModel: "test-child", SubagentEffort: "high", Policy: policy},
		Options{Provider: func(string) (provider.Provider, error) { return fakeProvider{}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	seat := r.seat()
	if got := seat.Snapshot().Effort; got != "high" {
		t.Fatalf("seat on a route without defaultEffort = %q, want the seat default high", got)
	}

	manager, err := r.launchSubagentSpec(seat.ID, LaunchSpec{Title: "manager", Brief: "inspect"})
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := r.launchSubagentSpec(manager.ID, LaunchSpec{Title: "leaf", Model: "test-leaf", Brief: "inspect"})
	if err != nil {
		t.Fatal(err)
	}
	if got := leaf.Snapshot().Effort; got != "low" {
		t.Fatalf("leaf on the route = %q, want defaultEffort low over the role's high", got)
	}
	explicit, err := r.launchSubagentSpec(manager.ID, LaunchSpec{Title: "explicit", Model: "test-leaf", Effort: "high", Brief: "inspect"})
	if err != nil {
		t.Fatal(err)
	}
	if got := explicit.Snapshot().Effort; got != "high" {
		t.Fatalf("leaf launched with an explicit effort = %q, want high", got)
	}

	if err := r.configureModelSlots("test-leaf", "test-child", "test-leaf", []string{"test", "test-leaf", "test-child"}); err != nil {
		t.Fatal(err)
	}
	if got := seat.Snapshot(); got.Model != "test-leaf" || got.Effort != "low" {
		t.Fatalf("seat switched onto the route = %s at %q, want test-leaf at low", got.Model, got.Effort)
	}
}
