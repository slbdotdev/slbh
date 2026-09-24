package headless

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/slbdotdev/slbh/internal/config"
	"github.com/slbdotdev/slbh/internal/harness"
	"github.com/slbdotdev/slbh/internal/logx"
	"github.com/slbdotdev/slbh/internal/provider"
	"github.com/slbdotdev/slbh/internal/seam"
)

// TestServeDrivesAToolTurnOverAnHTTPWire joins every layer a headless client
// relies on, with only the model server faked: JSONL requests reach the
// runtime, the Seat's turn goes out over the real Ollama wire, the model's
// bash call runs as a managed job, its result goes back on the continuation
// request, and the whole turn is on the protocol stream, in the jobs query and
// in the JSONL transcript.
func TestServeDrivesAToolTurnOverAnHTTPWire(t *testing.T) {
	const marker = "SLBH_TOOL_MARKER"
	var mu sync.Mutex
	var bodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Context-window discovery reaches the same host; it is not a turn.
		if r.URL.Path != "/api/chat" {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(body))
		call := len(bodies)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/x-ndjson")
		if call == 1 {
			_, _ = io.WriteString(w, `{"message":{"role":"assistant","content":"","tool_calls":[{"id":"call-1","function":{"index":0,"name":"bash","arguments":{"script":"echo `+marker+`"}}}]},"done":true,"done_reason":"stop","prompt_eval_count":10,"eval_count":5}`+"\n")
			return
		}
		_, _ = io.WriteString(w, `{"message":{"role":"assistant","content":"all done"},"done":false}`+"\n")
		_, _ = io.WriteString(w, `{"message":{"role":"assistant","content":""},"done":true,"done_reason":"stop","prompt_eval_count":20,"eval_count":2}`+"\n")
	}))
	defer server.Close()

	rt, err := harness.New(
		config.Config{Home: t.TempDir(), SeatModel: "test", SeatEffort: "high"},
		harness.Options{Provider: func(string) (provider.Provider, error) {
			return provider.NewHTTPOnWire(server.URL+"/api/chat", "test-key-not-a-credential", provider.WireOllamaChat)
		}},
	)
	if err != nil {
		t.Fatalf("harness.New: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- Serve(context.Background(), rt, inR, outW, Options{})
		_ = outW.Close()
	}()
	decoder := json.NewDecoder(outR)
	seatID := rt.Agents()[0].ID

	writeRequest(t, inW, 1, "send_prompt", map[string]any{"agent_id": seatID, "prompt": "run it"})
	messages := readUntilEvent(t, decoder, "turn_done")
	writeRequest(t, inW, 2, "jobs", map[string]any{})
	jobsReply := readUntilID(t, decoder, 2)
	writeRequest(t, inW, 3, "transcript_path", map[string]any{"agent_id": seatID})
	pathReply := readUntilID(t, decoder, 3)
	writeRequest(t, inW, 4, "close", map[string]any{})
	readUntilID(t, decoder, 4)
	_ = inW.Close()
	go func() { _, _ = io.Copy(io.Discard, outR) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not stop after close")
	}

	// The protocol stream carries the turn in order, and nothing failed.
	var kinds []string
	for _, message := range messages {
		if string(message["method"]) != `"event"` {
			continue
		}
		var event seam.Event
		if err := json.Unmarshal(message["params"], &event); err != nil {
			t.Fatalf("decode event: %v", err)
		}
		if event.Kind == "error" || event.Kind == "request_error" {
			t.Fatalf("turn reported %s: %q", event.Kind, event.Text)
		}
		if event.Kind == "tool_result" && !strings.Contains(event.Text, marker) {
			t.Fatalf("tool_result = %q, want the command's output", event.Text)
		}
		kinds = append(kinds, event.Kind)
	}
	if !inOrder(kinds, "tool_start", "tool_result", "assistant", "turn_done") {
		t.Fatalf("event kinds = %v, want tool_start, tool_result, assistant, turn_done in order", kinds)
	}

	// The model server saw the call and then its result on the continuation.
	mu.Lock()
	requests := append([]string(nil), bodies...)
	mu.Unlock()
	if len(requests) != 2 {
		t.Fatalf("provider requests = %d, want 2", len(requests))
	}
	if !strings.Contains(requests[0], `"name":"bash"`) {
		t.Fatalf("first request did not advertise bash: %s", requests[0])
	}
	if !strings.Contains(requests[1], `"role":"tool"`) || !strings.Contains(requests[1], marker) {
		t.Fatalf("continuation request did not carry the tool result: %s", requests[1])
	}

	var jobs []seam.JobSnapshot
	if err := json.Unmarshal(jobsReply[len(jobsReply)-1]["result"], &jobs); err != nil {
		t.Fatalf("decode jobs reply: %v", err)
	}
	if len(jobs) != 1 || jobs[0].ToolName != "bash" || jobs[0].Status != "complete" || jobs[0].ExitCode != 0 {
		t.Fatalf("jobs = %#v, want one completed bash job", jobs)
	}

	var path struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(pathReply[len(pathReply)-1]["result"], &path); err != nil || path.Path == "" {
		t.Fatalf("transcript_path reply = %s (%v)", pathReply[len(pathReply)-1]["result"], err)
	}
	entries, err := logx.Read(path.Path)
	if err != nil {
		t.Fatalf("read transcript: %v", err)
	}
	var logged []string
	for _, entry := range entries {
		logged = append(logged, entry.Kind)
	}
	if !inOrder(logged, "inference_request", "tool_start", "tool_result", "inference_request", "assistant", "turn_done") {
		t.Fatalf("transcript kinds = %v", logged)
	}
}

// inOrder reports whether want appears in got as a subsequence.
func inOrder(got []string, want ...string) bool {
	for _, kind := range got {
		if len(want) > 0 && kind == want[0] {
			want = want[1:]
		}
	}
	return len(want) == 0
}
