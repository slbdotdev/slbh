package intern

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/slbdotdev/slbh/internal/config"
	"github.com/slbdotdev/slbh/internal/provider"
	"github.com/slbdotdev/slbh/internal/seam"
)

type fakeRuntime struct {
	config   config.Config
	agents   []seam.AgentSnapshot
	jobs     []seam.JobSnapshot
	events   chan seam.Event
	commands chan seam.Command

	mu       sync.Mutex
	received []seam.Command
}

func newFakeRuntime(t *testing.T) *fakeRuntime {
	t.Helper()
	dir := t.TempDir()
	return &fakeRuntime{
		config: config.Config{
			InternModel: "local/test-intern",
			Instructions: config.Instructions{Layers: map[string]string{
				config.InstructionIntern: "Ask careful questions.",
			}},
		},
		agents: []seam.AgentSnapshot{
			{ID: "seat", Title: "Seat", Depth: 0, WorkDir: dir},
			{ID: "leaf", Title: "Leaf", ParentID: "seat", Depth: 1, WorkDir: dir},
		},
		events:   make(chan seam.Event, 64),
		commands: make(chan seam.Command, 64),
	}
}

func (r *fakeRuntime) Do(_ context.Context, command seam.Command) (seam.Reply, error) {
	r.mu.Lock()
	r.received = append(r.received, command)
	r.mu.Unlock()
	r.commands <- command
	if steer, ok := command.(seam.SteerAgentCommand); ok {
		r.events <- seam.Event{AgentID: steer.AgentID, Kind: "steer", Text: steer.Message}
	}
	return seam.Reply{Command: command.CommandName()}, nil
}

func (r *fakeRuntime) Events() <-chan seam.Event              { return r.events }
func (r *fakeRuntime) Subscribe() (<-chan seam.Event, func()) { return r.events, func() {} }
func (r *fakeRuntime) Agents() []seam.AgentSnapshot {
	return append([]seam.AgentSnapshot(nil), r.agents...)
}
func (r *fakeRuntime) JobSnapshots() []seam.JobSnapshot {
	return append([]seam.JobSnapshot(nil), r.jobs...)
}
func (r *fakeRuntime) Config() config.Config                 { return r.config }
func (r *fakeRuntime) ID() string                            { return "runtime" }
func (r *fakeRuntime) Home() string                          { return r.config.Home }
func (r *fakeRuntime) Dir() string                           { return r.agents[0].WorkDir }
func (r *fakeRuntime) TranscriptPath(string) (string, error) { return "", nil }
func (r *fakeRuntime) InstructionSource() config.InstructionSource {
	return r.config.Instructions.Source
}
func (r *fakeRuntime) PolicySource() config.PolicySource { return r.config.PolicySource }
func (r *fakeRuntime) ModelCatalog() []provider.Catalog  { return nil }
func (r *fakeRuntime) ModelGuidance() string             { return "" }

type scriptedProvider struct {
	mu       sync.Mutex
	requests []provider.Request
	calls    chan int
	stream   func(int, context.Context, provider.Request, provider.StreamSink) error
}

func newScriptedProvider(stream func(int, context.Context, provider.Request, provider.StreamSink) error) *scriptedProvider {
	return &scriptedProvider{calls: make(chan int, 32), stream: stream}
}

func (p *scriptedProvider) Stream(ctx context.Context, request provider.Request, sink provider.StreamSink) error {
	p.mu.Lock()
	index := len(p.requests)
	p.requests = append(p.requests, request)
	p.mu.Unlock()
	p.calls <- index
	if p.stream == nil {
		return nil
	}
	return p.stream(index, ctx, request, sink)
}

func (p *scriptedProvider) request(index int) provider.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.requests[index]
}

func startIntern(t *testing.T, rt *fakeRuntime, p provider.Provider) chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- Run(context.Background(), rt, Options{Provider: p}) }()
	return done
}

func finishIntern(t *testing.T, rt *fakeRuntime, done chan error) {
	t.Helper()
	close(rt.events)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Intern did not stop when its subscription closed")
	}
}

func waitCall(t *testing.T, p *scriptedProvider) int {
	t.Helper()
	select {
	case index := <-p.calls:
		return index
	case <-time.After(2 * time.Second):
		t.Fatal("provider was not called")
		return -1
	}
}

func requireNoCall(t *testing.T, p *scriptedProvider) {
	t.Helper()
	select {
	case index := <-p.calls:
		t.Fatalf("unexpected provider call %d", index)
	case <-time.After(75 * time.Millisecond):
	}
}

func TestInferenceRunsOnlyAtSeatTurnBoundaries(t *testing.T) {
	rt := newFakeRuntime(t)
	p := newScriptedProvider(nil)
	done := startIntern(t, rt, p)

	rt.events <- seam.Event{AgentID: "seat", Kind: "assistant", Text: "working"}
	rt.events <- seam.Event{AgentID: "leaf", Kind: "turn_done", Text: "leaf finished"}
	requireNoCall(t, p)
	rt.events <- seam.Event{AgentID: "seat", Kind: "turn_done"}
	if got := waitCall(t, p); got != 0 {
		t.Fatalf("provider call = %d, want 0", got)
	}

	request := p.request(0)
	if len(request.Messages) != 1 || !strings.Contains(request.Messages[0].Content, "working") || !strings.Contains(request.Messages[0].Content, "leaf finished") {
		t.Fatalf("inference did not receive accumulated Seat-tree events: %#v", request.Messages)
	}
	finishIntern(t, rt, done)
}

func TestAskSeatSendsSignedSteerWithoutWaitingForAnswer(t *testing.T) {
	rt := newFakeRuntime(t)
	p := newScriptedProvider(func(index int, _ context.Context, _ provider.Request, sink provider.StreamSink) error {
		if index == 0 {
			return sink(provider.Event{Kind: provider.EventTool, ToolIndex: 0, ToolCallID: "ask-1", ToolName: "ask_seat", Input: `{"question":"Could the boundary be stale?"}`})
		}
		return nil
	})
	done := startIntern(t, rt, p)
	rt.events <- seam.Event{AgentID: "seat", Kind: "turn_done"}
	waitCall(t, p)
	waitCall(t, p)

	select {
	case command := <-rt.commands:
		steer, ok := command.(seam.SteerAgentCommand)
		if !ok {
			t.Fatalf("command = %T, want SteerAgentCommand", command)
		}
		if steer.AgentID != "seat" || steer.Message != "[intern] Could the boundary be stale?" {
			t.Fatalf("steer = %#v", steer)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ask_seat did not send a steer")
	}
	finishIntern(t, rt, done)
}

func TestBoundariesCoalesceWhileInferenceRuns(t *testing.T) {
	rt := newFakeRuntime(t)
	release := make(chan struct{})
	p := newScriptedProvider(func(index int, ctx context.Context, _ provider.Request, _ provider.StreamSink) error {
		if index != 0 {
			return nil
		}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	done := startIntern(t, rt, p)
	rt.events <- seam.Event{AgentID: "seat", Kind: "turn_done", Text: "first"}
	waitCall(t, p)
	rt.events <- seam.Event{AgentID: "seat", Kind: "turn_done", Text: "second"}
	rt.events <- seam.Event{AgentID: "seat", Kind: "turn_done", Text: "third"}
	close(release)
	if got := waitCall(t, p); got != 1 {
		t.Fatalf("follow-up call = %d, want 1", got)
	}
	requireNoCall(t, p)

	followUp := p.request(1)
	if len(followUp.Messages) == 0 {
		t.Fatal("follow-up has no messages")
	}
	last := followUp.Messages[len(followUp.Messages)-1].Content
	if !strings.Contains(last, "second") || !strings.Contains(last, "third") {
		t.Fatalf("coalesced inference did not cover both pending boundaries: %q", last)
	}
	finishIntern(t, rt, done)
}

func TestOwnSteersAreIgnored(t *testing.T) {
	rt := newFakeRuntime(t)
	p := newScriptedProvider(nil)
	done := startIntern(t, rt, p)
	rt.events <- seam.Event{AgentID: "seat", Kind: "steer", Text: "[intern] Is this recursive?"}
	rt.events <- seam.Event{AgentID: "seat", Kind: "turn_done"}
	waitCall(t, p)
	request := p.request(0)
	if strings.Contains(request.Messages[0].Content, "Is this recursive?") {
		t.Fatalf("Intern accumulated its own steer: %q", request.Messages[0].Content)
	}
	finishIntern(t, rt, done)
}

func TestExactToolListAndUnknownToolRefusal(t *testing.T) {
	rt := newFakeRuntime(t)
	p := newScriptedProvider(func(index int, _ context.Context, request provider.Request, sink provider.StreamSink) error {
		if index == 0 {
			return sink(provider.Event{Kind: provider.EventTool, ToolIndex: 0, ToolCallID: "bad-1", ToolName: "quick_bash", Input: `{}`})
		}
		last := request.Messages[len(request.Messages)-1]
		if last.Role != "tool" || !strings.Contains(last.Content, `unknown intern tool "quick_bash"`) {
			return errors.New("unknown tool was not refused in the tool result")
		}
		return nil
	})
	done := startIntern(t, rt, p)
	rt.events <- seam.Event{AgentID: "seat", Kind: "turn_done"}
	waitCall(t, p)
	waitCall(t, p)
	second := p.request(1)
	last := second.Messages[len(second.Messages)-1]
	if last.Role != "tool" || !strings.Contains(last.Content, `unknown intern tool "quick_bash"`) {
		t.Fatalf("unknown tool result = %#v", last)
	}

	request := p.request(0)
	var names []string
	for _, tool := range request.Tools {
		names = append(names, tool.Name)
	}
	want := []string{"glob", "grep", "read_file", "read_bytes", "read_lines", "agent_snapshots", "job_snapshots", "event_stream", "ask_seat"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("tools = %v, want %v", names, want)
	}
	for _, tool := range request.Tools {
		if tool.Parameters["type"] != "object" {
			t.Fatalf("tool %s schema = %#v", tool.Name, tool.Parameters)
		}
	}
	finishIntern(t, rt, done)
}

func TestReadToolsNeverCreateFiles(t *testing.T) {
	rt := newFakeRuntime(t)
	w := &watcher{rt: rt, seatID: "seat", buffer: newEventBuffer(8)}
	dir := rt.agents[0].WorkDir
	calls := []struct {
		name string
		args string
	}{
		{"glob", `{"pattern":"missing/**/*"}`},
		{"grep", `{"pattern":"needle","path":"missing"}`},
		{"read_file", `{"path":"missing/file"}`},
		{"read_bytes", `{"path":"missing/file","start":0,"end":1}`},
		{"read_lines", `{"path":"missing/file","start":1,"end":2}`},
	}
	for _, call := range calls {
		_, _ = w.executeTool(context.Background(), call.name, call.args)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("read tools created entries in %s: %v", dir, entries)
	}
}

func TestMissingInternInstructionsRefusesStart(t *testing.T) {
	rt := newFakeRuntime(t)
	rt.config.Instructions = config.Instructions{Layers: map[string]string{}}
	err := Run(context.Background(), rt, Options{Provider: newScriptedProvider(nil)})
	if err == nil || !strings.Contains(err.Error(), "instructions/intern.md is missing or empty") {
		t.Fatalf("Run error = %v", err)
	}
}

func TestNonLocalModelRefusesStart(t *testing.T) {
	for _, model := range []string{"zai/glm-5.3-flash", "deepseek-v4-flash", "gpt-5.6-sol"} {
		rt := newFakeRuntime(t)
		err := Run(context.Background(), rt, Options{Model: model, Provider: newScriptedProvider(nil)})
		if err == nil || !strings.Contains(err.Error(), "not a local route") {
			t.Fatalf("Run(%q) error = %v, want refusal", model, err)
		}
	}
}

func TestProviderErrorEmitsStatusAndLoopContinues(t *testing.T) {
	rt := newFakeRuntime(t)
	p := newScriptedProvider(func(index int, _ context.Context, _ provider.Request, _ provider.StreamSink) error {
		if index == 0 {
			return errors.New("provider unavailable")
		}
		return nil
	})
	done := startIntern(t, rt, p)
	rt.events <- seam.Event{AgentID: "seat", Kind: "turn_done", Text: "first"}
	waitCall(t, p)
	select {
	case command := <-rt.commands:
		status, ok := command.(seam.EmitStatusCommand)
		if !ok || status.Kind != "intern" || !strings.Contains(status.Text, "provider unavailable") {
			t.Fatalf("status command = %#v", command)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("provider error did not emit intern status")
	}
	rt.events <- seam.Event{AgentID: "seat", Kind: "turn_done", Text: "second"}
	if got := waitCall(t, p); got != 1 {
		t.Fatalf("call after provider error = %d, want 1", got)
	}
	finishIntern(t, rt, done)
}

func TestMissingModelEmitsStatusAndLoopContinues(t *testing.T) {
	rt := newFakeRuntime(t)
	rt.config.InternModel = ""
	p := newScriptedProvider(nil)
	done := startIntern(t, rt, p)
	rt.events <- seam.Event{AgentID: "seat", Kind: "turn_done", Text: "first"}
	select {
	case command := <-rt.commands:
		status, ok := command.(seam.EmitStatusCommand)
		if !ok || status.Kind != "intern" || !strings.Contains(status.Text, "no model selected") {
			t.Fatalf("status command = %#v", command)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("missing model did not emit intern status")
	}
	rt.events <- seam.Event{AgentID: "seat", Kind: "turn_done", Text: "second"}
	select {
	case command := <-rt.commands:
		status, ok := command.(seam.EmitStatusCommand)
		if !ok || status.Kind != "intern" {
			t.Fatalf("second status command = %#v", command)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Intern did not continue after missing model")
	}
	requireNoCall(t, p)
	finishIntern(t, rt, done)
}

func TestRuntimeStateToolsReturnJSON(t *testing.T) {
	rt := newFakeRuntime(t)
	rt.jobs = []seam.JobSnapshot{{ID: "job-1", Status: "running"}}
	w := &watcher{rt: rt, seatID: "seat", buffer: newEventBuffer(8)}
	w.buffer.add(seam.Event{AgentID: "seat", Kind: "assistant", Text: "zero"})
	w.buffer.add(seam.Event{AgentID: "seat", Kind: "assistant", Text: "one"})
	for _, call := range []struct {
		name string
		args string
	}{
		{"agent_snapshots", `{}`},
		{"job_snapshots", `{}`},
		{"event_stream", `{"since":1,"limit":1}`},
	} {
		result, err := w.executeTool(context.Background(), call.name, call.args)
		if err != nil {
			t.Fatalf("%s: %v", call.name, err)
		}
		var decoded any
		if err := json.Unmarshal([]byte(result), &decoded); err != nil {
			t.Fatalf("%s returned invalid JSON %q: %v", call.name, result, err)
		}
		if call.name == "event_stream" && (!strings.Contains(result, `"sequence":1`) || strings.Contains(result, `"sequence":0`)) {
			t.Fatalf("event_stream since/limit result = %s", result)
		}
	}
}

func TestConversationHistoryDropsOldestTurnsWithinBudget(t *testing.T) {
	history := newHistory("system", 140)
	history.add([]provider.Message{{Role: "user", Content: strings.Repeat("old", 20)}})
	history.add([]provider.Message{{Role: "user", Content: strings.Repeat("new", 20)}})
	if history.size() > 140 {
		t.Fatalf("history size = %d, budget 140", history.size())
	}
	messages := history.messages()
	if len(messages) != 1 || !strings.Contains(messages[0].Content, "new") {
		t.Fatalf("history retained wrong turns: %#v", messages)
	}
}

func TestSeatWorkDirIsUsedForReadTools(t *testing.T) {
	rt := newFakeRuntime(t)
	path := filepath.Join(rt.agents[0].WorkDir, "evidence.txt")
	if err := os.WriteFile(path, []byte("evidence"), 0o600); err != nil {
		t.Fatal(err)
	}
	w := &watcher{rt: rt, seatID: "seat", buffer: newEventBuffer(8)}
	result, err := w.executeTool(context.Background(), "read_file", `{"path":"evidence.txt"}`)
	if err != nil || result != "evidence" {
		t.Fatalf("read_file = %q, %v", result, err)
	}
}
