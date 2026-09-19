package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/slbdotdev/slbh/internal/config"
	"github.com/slbdotdev/slbh/internal/job"
	"github.com/slbdotdev/slbh/internal/logx"
	"github.com/slbdotdev/slbh/internal/provider"
)

// Each inference stays in flight until the test releases it. Tests inspect
// the NEXT request while the original turn is still active, not just an event
// saying that a message has been queued.
type pendingInference struct {
	ctx     context.Context
	request provider.Request
	reply   chan []provider.Event
}

type boundaryProvider struct{ calls chan pendingInference }

func (p *boundaryProvider) Stream(ctx context.Context, request provider.Request, sink provider.StreamSink) error {
	call := pendingInference{ctx: ctx, request: request, reply: make(chan []provider.Event, 1)}
	select {
	case p.calls <- call:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case events := <-call.reply:
		for _, event := range events {
			if err := sink(event); err != nil {
				return err
			}
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *boundaryProvider) next(t *testing.T) pendingInference {
	t.Helper()
	select {
	case call := <-p.calls:
		return call
	case <-time.After(3 * time.Second):
		t.Fatal("no inference at the next call boundary")
		return pendingInference{}
	}
}

func (c pendingInference) finish(t *testing.T, events ...provider.Event) {
	t.Helper()
	if err := c.ctx.Err(); err != nil {
		t.Fatalf("in-flight inference was cancelled by delivery: %v", err)
	}
	c.reply <- events
}

func textEvent(text string) provider.Event {
	return provider.Event{Kind: provider.EventText, Text: text}
}

func toolEvent(index int, id, name, args string) provider.Event {
	return provider.Event{Kind: provider.EventTool, ToolIndex: index, ToolCallID: id, ToolName: name, Input: args}
}

func messagingRuntime(t *testing.T) (*Runtime, *boundaryProvider) {
	t.Helper()
	p := &boundaryProvider{calls: make(chan pendingInference, 16)}
	r, err := New(config.Config{Home: t.TempDir(), SeatModel: "passive", SubagentModel: "passive", LeafModel: "passive"}, Options{Provider: func(model string) (provider.Provider, error) {
		if model == "active" {
			return p, nil
		}
		return fakeProvider{}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r, p
}

func requireMessage(t *testing.T, request provider.Request, role, text string) int {
	t.Helper()
	count, at := 0, -1
	for i, m := range request.Messages {
		if m.Role == role && m.Content == text {
			count++
			at = i
		}
	}
	if count != 1 {
		t.Fatalf("want exactly one %s %q, got %d in %#v", role, text, count, request.Messages)
	}
	return at
}

func requireActiveTurn(t *testing.T, r *Runtime, a *Agent) {
	t.Helper()
	path, err := r.TranscriptPath(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := logx.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Kind == "turn_done" {
			t.Fatal("message was deferred past turn completion")
		}
	}
}

func requireToolPairs(t *testing.T, messages []provider.Message) {
	t.Helper()
	for i, m := range messages {
		if len(m.ToolCalls) > 0 {
			if len(m.ToolCalls) != 1 || i+1 == len(messages) || messages[i+1].Role != "tool" || messages[i+1].ToolCallID != m.ToolCalls[0].ID {
				t.Fatalf("unresolved tool call at %d: %#v", i, messages)
			}
		}
		if m.Role == "tool" && (i == 0 || len(messages[i-1].ToolCalls) != 1 || messages[i-1].ToolCalls[0].ID != m.ToolCallID) {
			t.Fatalf("orphaned tool result at %d: %#v", i, messages)
		}
	}
}

func TestMessagesReachEveryAgentAtInferenceBoundary(t *testing.T) {
	for _, direction := range []string{"parent-to-child", "child-to-parent", "parent-to-leaf", "leaf-to-parent", "sibling-to-sibling"} {
		t.Run(direction, func(t *testing.T) {
			r, p := messagingRuntime(t)
			seat := r.seat()
			child, err := r.launchSubagentSpec(seat.ID, LaunchSpec{Title: "child"})
			if err != nil {
				t.Fatal(err)
			}
			leaf, err := r.launchSubagentSpec(child.ID, LaunchSpec{Title: "leaf", Model: "passive"})
			if err != nil {
				t.Fatal(err)
			}
			sibling, err := r.launchSubagentSpec(seat.ID, LaunchSpec{Title: "sibling"})
			if err != nil {
				t.Fatal(err)
			}
			sender, recipient := seat, child
			switch direction {
			case "child-to-parent":
				sender, recipient = child, seat
			case "parent-to-leaf":
				sender, recipient = child, leaf
			case "leaf-to-parent":
				sender, recipient = leaf, child
			case "sibling-to-sibling":
				sender, recipient = child, sibling
			}
			recipient.SetModel("active")
			if err := recipient.Send("original task"); err != nil {
				t.Fatal(err)
			}
			first := p.next(t)
			args, _ := json.Marshal(map[string]string{"agent_id": recipient.ID, "message": "new direction"})
			if _, err := r.ExecuteTool(sender.ID, "msg_subagent", string(args)); err != nil {
				t.Fatal(err)
			}
			// Even a response without tools must preserve its work and continue
			// THIS turn with the message, rather than report completion first.
			first.finish(t, textEvent("paid-for answer"))
			next := p.next(t)
			answerAt := requireMessage(t, next.request, "assistant", "paid-for answer")
			steerAt := requireMessage(t, next.request, "user", fmt.Sprintf("[steer] [from %s (%s)] new direction", sender.Title, sender.ID))
			if steerAt <= answerAt {
				t.Fatal("message displaced the completed inference output")
			}
			requireActiveTurn(t, r, recipient)
			next.finish(t, textEvent("revised answer"))
			waitAgentTurn(t, r, recipient.ID)
		})
	}
}

func TestIdleSteerWakesWithoutAnotherMessage(t *testing.T) {
	r, p := messagingRuntime(t)
	a := r.seat()
	a.SetModel("active")
	if err := a.Steer("wake now"); err != nil {
		t.Fatal(err)
	}
	call := p.next(t)
	requireMessage(t, call.request, "user", "[steer] wake now")
	call.finish(t, textEvent("awake"))
	waitAgentTurn(t, r, a.ID)
}

func TestOneInboxPreservesFIFOBurstAndDoesNotBlockOnBusyAgentOrUI(t *testing.T) {
	r, p := messagingRuntime(t)
	a := r.seat()
	a.SetModel("active")
	child, err := r.launchSubagentSpec(a.ID, LaunchSpec{Title: "child"})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Send("start"); err != nil {
		t.Fatal(err)
	}
	first := p.next(t)
	accepted := make(chan error, 1)
	go func() {
		for i := 0; i < 200; i++ {
			var err error
			text := fmt.Sprintf("message %03d", i)
			switch i % 3 {
			case 0:
				err = a.Send(text)
			case 1:
				err = a.Steer(text)
			case 2:
				err = a.receiveChildResult(child, text)
			}
			if err != nil {
				accepted <- err
				return
			}
		}
		accepted <- nil
	}()
	select {
	case err := <-accepted:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("message submission blocked on a busy recipient")
	}
	// Saturate telemetry without reading Events; message transport must work.
	for i := 0; i < 1100; i++ {
		r.emitStatus("status", "UI backpressure")
	}
	first.finish(t, textEvent("retained"))
	next := p.next(t)
	last := requireMessage(t, next.request, "assistant", "retained")
	for i := 0; i < 200; i++ {
		text := fmt.Sprintf("message %03d", i)
		switch i % 3 {
		case 1:
			text = "[steer] " + text
		case 2:
			text = "[result from child] " + text
		}
		at := requireMessage(t, next.request, "user", text)
		if at <= last {
			t.Fatalf("FIFO order broken at message %d", i)
		}
		last = at
	}
	requireActiveTurn(t, r, a)
	// Close terminates the deliberately held call; no UI event is required.
}

func TestAutomaticChildResultEntersBusyParentTurn(t *testing.T) {
	r, p := messagingRuntime(t)
	seat := r.seat()
	seat.SetModel("active")
	if err := seat.Send("working while child runs"); err != nil {
		t.Fatal(err)
	}
	first := p.next(t)
	child, err := r.launchSubagentSpec(seat.ID, LaunchSpec{Title: "child", Brief: "do the work"})
	if err != nil {
		t.Fatal(err)
	}
	waitAgentTurn(t, r, child.ID)
	first.finish(t, textEvent("parent work"))
	next := p.next(t)
	requireMessage(t, next.request, "assistant", "parent work")
	requireMessage(t, next.request, "user", "[result from child] done")
	requireActiveTurn(t, r, seat)
	next.finish(t, textEvent("used child result"))
	waitAgentTurn(t, r, seat.ID)
}

func TestInferenceToolBatchIsPreservedWhenMessagesArrive(t *testing.T) {
	r, p := messagingRuntime(t)
	a := r.seat()
	a.SetModel("active")
	a.WorkDir = t.TempDir()
	if err := a.Send("write two files"); err != nil {
		t.Fatal(err)
	}
	first := p.next(t)
	if err := a.Steer("keep this context"); err != nil {
		t.Fatal(err)
	}
	first.finish(t, textEvent("completed reasoning output"),
		toolEvent(0, "write-1", "write_file", `{"path":"one","content":"first"}`),
		toolEvent(1, "write-2", "write_file", `{"path":"two","content":"second"}`))
	next := p.next(t)
	requireMessage(t, next.request, "assistant", "completed reasoning output")
	requireMessage(t, next.request, "user", "[steer] keep this context")
	requireToolPairs(t, next.request.Messages)
	for name, want := range map[string]string{"one": "first", "two": "second"} {
		data, err := os.ReadFile(filepath.Join(a.WorkDir, name))
		if err != nil || string(data) != want {
			t.Fatalf("paid-for tool %s was lost: %q, %v", name, data, err)
		}
	}
	for _, id := range []string{"write-1", "write-2"} {
		count := 0
		for _, m := range next.request.Messages {
			if m.Role == "tool" && m.ToolCallID == id {
				count++
				if !strings.HasPrefix(m.Content, "created ") {
					t.Fatalf("tool was skipped or replayed: %#v", m)
				}
			}
		}
		if count != 1 {
			t.Fatalf("tool %s returned %d results", id, count)
		}
	}
	requireActiveTurn(t, r, a)
	next.finish(t, textEvent("done"))
	waitAgentTurn(t, r, a.ID)
}

// waitJobResultQueued blocks until the finished job's completion has actually
// been enqueued into the agent's inbox. The job manager closes Done before its
// completion handler delivers the result, so waiting on Done alone races the
// turn-end inbox check: the agent can find an empty inbox, close the turn, and
// only then receive the completion. The inbox is the thing the turn boundary
// reads, so it is the thing this test waits on.
func waitJobResultQueued(t *testing.T, a *Agent, jobID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		a.mu.RLock()
		queued := false
		for _, message := range a.inbox {
			if message.kind == "job_result" && message.metadata["job"] == jobID {
				queued = true
				break
			}
		}
		a.mu.RUnlock()
		if queued {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("bash %q completion never reached the inbox", jobID)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestBackgroundJobCompletionIsDeliveredAtNextBoundary(t *testing.T) {
	r, p := messagingRuntime(t)
	a := r.seat()
	a.SetModel("active")
	a.WorkDir = t.TempDir()
	release := filepath.Join(a.WorkDir, "release")
	t.Cleanup(func() { _ = os.WriteFile(release, nil, 0600) })
	if err := a.Send("start background work"); err != nil {
		t.Fatal(err)
	}
	first := p.next(t)
	script := "sleep 1; printf 'job-stdout\\n'; printf 'job-stderr\\n' >&2"
	args, err := json.Marshal(map[string]any{"script": script, "wait_seconds": 0, "warn_after_seconds": 10})
	if err != nil {
		t.Fatal(err)
	}
	first.finish(t, toolEvent(0, "job-call", "bash", string(args)))
	second := p.next(t)
	var jobID string
	for _, message := range second.request.Messages {
		if message.Role == "tool" && message.Name == "bash" {
			jobID = strings.Fields(message.Content)[1]
			break
		}
	}
	if jobID == "" {
		t.Fatalf("continuation did not retain bash result: %#v", second.request.Messages)
	}
	if _, ok := r.jobs.Get(jobID); !ok {
		t.Fatalf("bash %q was not registered", jobID)
	}
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	waitJobResultQueued(t, a, jobID)
	second.finish(t, textEvent("continued while job ran"))
	third := p.next(t)
	continuedAt := requireMessage(t, third.request, "assistant", "continued while job ran")
	jobResultAt := -1
	for i, message := range third.request.Messages {
		if message.Role == "user" && strings.Contains(message.Content, "[result from bash "+jobID+"]") {
			jobResultAt = i
			if !strings.Contains(message.Content, "job-stdout") || !strings.Contains(message.Content, "job-stderr") {
				t.Fatalf("bash result lost output: %q", message.Content)
			}
			break
		}
	}
	if jobResultAt != continuedAt+1 {
		t.Fatalf("bash result index=%d, want immediately after assistant index=%d; messages=%#v", jobResultAt, continuedAt, third.request.Messages)
	}
	requireActiveTurn(t, r, a)
	third.finish(t, textEvent("done"))
	waitAgentTurn(t, r, a.ID)
}

func TestToolInFlightFinishesAndDeliversBeforeNextTool(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux/WSL bash boundary test")
	}
	r, p := messagingRuntime(t)
	a := r.seat()
	a.SetModel("active")
	a.WorkDir = t.TempDir()
	release := filepath.Join(a.WorkDir, "release")
	// Release a blocked tool before runtime cleanup, even on assertion failure.
	t.Cleanup(func() { _ = os.WriteFile(release, nil, 0600) })
	if err := a.Send("run a tool batch"); err != nil {
		t.Fatal(err)
	}
	first := p.next(t)
	args, _ := json.Marshal(map[string]string{"script": "touch started; while [ ! -f release ]; do sleep 0.01; done; printf paid-tool-output"})
	first.finish(t, toolEvent(0, "blocking", "bash", string(args)), toolEvent(1, "following", "write_file", `{"path":"after","content":"retained"}`))
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(a.WorkDir, "started")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("tool did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := a.Steer("arrived inside tool"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	next := p.next(t)
	outputAt := requireMessage(t, next.request, "tool", "paid-tool-output")
	steerAt := requireMessage(t, next.request, "user", "[steer] arrived inside tool")
	if steerAt != outputAt+1 {
		t.Fatal("message missed the first completed tool boundary")
	}
	if steerAt+1 >= len(next.request.Messages) || len(next.request.Messages[steerAt+1].ToolCalls) != 1 || next.request.Messages[steerAt+1].ToolCalls[0].ID != "following" {
		t.Fatal("remaining tool call was discarded or message was delayed behind it")
	}
	requireToolPairs(t, next.request.Messages)
	data, err := os.ReadFile(filepath.Join(a.WorkDir, "after"))
	if err != nil || string(data) != "retained" {
		t.Fatalf("following tool did not finish: %q %v", data, err)
	}
	requireActiveTurn(t, r, a)
	next.finish(t, textEvent("done"))
	waitAgentTurn(t, r, a.ID)
}

func TestStoppedRecipientsAndEmptyMessagesFailExplicitly(t *testing.T) {
	r, _ := messagingRuntime(t)
	child, err := r.launchSubagentSpec(r.seat().ID, LaunchSpec{Title: "child"})
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Steer("  "); err == nil {
		t.Fatal("empty message accepted")
	}
	if err := r.endSubagent(r.seat().ID, child.ID); err != nil {
		t.Fatal(err)
	}
	if err := child.Send("late"); err == nil {
		t.Fatal("stopped recipient accepted input")
	}
	args, _ := json.Marshal(map[string]string{"agent_id": child.ID, "message": "late"})
	if _, err := r.ExecuteTool(r.seat().ID, "msg_subagent", string(args)); err == nil || !strings.Contains(err.Error(), "stopped") {
		t.Fatalf("stopped recipient reported success: %v", err)
	}
}

func TestHTTPMessageWaitsForResponseCompletionWithinSameTurn(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	requests := make(chan provider.Request, 2)
	var cancelled atomic.Bool
	var posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"data":[{"id":"test","context_length":128000}]}`)
			return
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Error(err)
			return
		}
		parsed, err := provider.RequestFromPayload(body)
		if err != nil {
			t.Error(err)
			return
		}
		requests <- parsed
		w.Header().Set("Content-Type", "text/event-stream")
		if posts.Add(1) == 1 {
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"paid \"}}]}\n\n")
			w.(http.Flusher).Flush()
			close(started)
			select {
			case <-release:
			case <-request.Context().Done():
				cancelled.Store(true)
				return
			}
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"output\"}}]}\n\n")
		} else {
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"received\"}}]}\n\n")
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(server.Close)
	p := provider.NewHTTP(server.URL+"/chat/completions", "local-test-key")
	r, err := New(config.Config{Home: t.TempDir(), SeatModel: "test"}, Options{Provider: func(string) (provider.Provider, error) { return p, nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	t.Cleanup(func() { close(release) })
	if err := r.seat().Send("initial"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("HTTP stream never started")
	}
	<-requests
	if err := r.seat().Steer("during HTTP stream"); err != nil {
		t.Fatal(err)
	}
	// Send a token instead of closing: cleanup still releases the handler on
	// failure, and no message arrival is permitted to cancel this HTTP call.
	select {
	case release <- struct{}{}:
	case <-time.After(3 * time.Second):
		t.Fatal("in-flight HTTP request was cancelled")
	}
	var next provider.Request
	select {
	case next = <-requests:
	case <-time.After(3 * time.Second):
		t.Fatal("steer did not reach the next HTTP request")
	}
	if cancelled.Load() {
		t.Fatal("delivery cancelled a paid-for HTTP response")
	}
	requireMessage(t, next, "assistant", "paid output")
	requireMessage(t, next, "user", "[steer] during HTTP stream")
	waitAgentTurn(t, r, r.seat().ID)
	path, _ := r.TranscriptPath(r.seat().ID)
	entries, err := logx.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	completed := 0
	for _, entry := range entries {
		if entry.Kind == "turn_done" {
			completed++
		}
	}
	if completed != 1 {
		t.Fatalf("steering crossed a turn boundary: %d completed turns", completed)
	}
}

// TestJobWarningWakesIdleAuthoringAgent is the routing contract for
// warn_after_seconds.
//
// The timer exists for exactly one party: the agent that started the job. That
// agent is usually idle when it fires, because starting a long job and ending
// the turn is the documented way to use one — the alternative, blocking the
// turn on the job, is what background execution exists to avoid. So the warning has to
// reach the agent's inbox and wake it, by the same path a completion takes.
//
// Before the fix the timer's handler only emitted a job_warning event to the
// TUI, so the one party the timer exists to inform never heard it, and this
// test failed at the third boundary wait below: nothing ever woke the agent.
func TestJobWarningWakesIdleAuthoringAgent(t *testing.T) {
	r, p := messagingRuntime(t)
	a := r.seat()
	a.SetModel("active")
	a.WorkDir = t.TempDir()
	if err := a.Send("start background work"); err != nil {
		t.Fatal(err)
	}
	first := p.next(t)
	// The job must outlive the whole test: what is under test is the warning
	// for a job that has NOT finished, so its completion must never be the
	// thing that wakes the agent.
	script := "sleep 30"
	args, err := json.Marshal(map[string]any{"script": script, "wait_seconds": 0, "warn_after_seconds": 2})
	if err != nil {
		t.Fatal(err)
	}
	first.finish(t, toolEvent(0, "job-call", "bash", string(args)))
	second := p.next(t)
	jobID := ""
	for _, message := range second.request.Messages {
		if message.Role == "tool" && message.Name == "bash" {
			jobID = strings.Fields(message.Content)[1]
			break
		}
	}
	if jobID == "" {
		t.Fatalf("continuation did not retain the bash id: %#v", second.request.Messages)
	}
	started, ok := r.jobs.Get(jobID)
	if !ok {
		t.Fatalf("bash %q was not registered", jobID)
	}
	t.Cleanup(func() { _ = started.Kill() })

	// End the turn while the job runs. The agent goes idle, which is precisely
	// the state the warning has to break.
	second.finish(t, textEvent("job started; ending the turn"))
	waitAgentTurn(t, r, a.ID)
	if status := a.Snapshot().Status; status != "idle" {
		t.Fatalf("agent status = %q, want idle before the warning fires", status)
	}
	if status := started.Snapshot().Status; status != job.Running {
		t.Fatalf("job status = %q, want the job still running", status)
	}

	// The warning wakes the idle agent: a new inference, carrying the warning
	// as a user message in the agent's own context.
	third := p.next(t)
	warning := ""
	for _, message := range third.request.Messages {
		if message.Role == "user" && strings.Contains(message.Content, "[warning from bash "+jobID+"]") {
			warning = message.Content
			break
		}
	}
	if warning == "" {
		t.Fatalf("the warning never reached the agent's context: %#v", third.request.Messages)
	}
	if !strings.Contains(warning, "still running") {
		t.Fatalf("warning does not say the job is still running: %q", warning)
	}
	if !strings.Contains(warning, "action kill") {
		t.Fatalf("warning does not name the agent's options: %q", warning)
	}
	if status := started.Snapshot().Status; status != job.Running {
		t.Fatalf("job status = %q; the warning must describe a job that is still running", status)
	}

	third.finish(t, textEvent("noted; leaving the job to run"))
	waitAgentTurn(t, r, a.ID)

	// One warning, the agent's call, done. Nothing reminds it again and nothing
	// escalates, so no further inference may start on the job's account.
	select {
	case again := <-p.calls:
		t.Fatalf("the warning recurred or escalated: %#v", again.request.Messages)
	case <-time.After(time.Second):
	}
}

// TestJobWarningNamesTheToolThatStartedTheJob covers python.
//
// python and bash share the whole warning path. The specs differ only in
// the command the job runs and the ToolName they carry (jobSpec vs
// pythonJobSpec), and the warning reads ToolName and nothing else, so this
// drives the routing with a python snapshot rather than spawning a Python
// interpreter that a development checkout may not have.
func TestJobWarningNamesTheToolThatStartedTheJob(t *testing.T) {
	r, p := messagingRuntime(t)
	a := r.seat()
	a.SetModel("active")
	r.deliverJobWarning(job.Snapshot{
		ID:        "job-python",
		Author:    a.ID,
		Script:    "print('work')",
		ToolName:  "python",
		Status:    job.Running,
		WarnAfter: 3 * time.Second,
	})
	call := p.next(t)
	warning := ""
	for _, message := range call.request.Messages {
		if message.Role == "user" && strings.Contains(message.Content, "[warning from python job-python]") {
			warning = message.Content
			break
		}
	}
	if warning == "" {
		t.Fatalf("a python warning did not reach the agent's context: %#v", call.request.Messages)
	}
	if !strings.Contains(warning, "python job-python is still running after 3s") {
		t.Fatalf("warning does not name the tool, the job and the interval: %q", warning)
	}
	call.finish(t, textEvent("noted"))
	waitAgentTurn(t, r, a.ID)
}
