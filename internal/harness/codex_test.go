package harness

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/slbdotdev/slbh/internal/config"
	"github.com/slbdotdev/slbh/internal/logx"
	"github.com/slbdotdev/slbh/internal/provider"
)

func TestMain(m *testing.M) {
	if os.Getenv("SLBH_CODEX_HELPER") == "1" {
		runCodexTestHelper()
		return
	}
	if os.Getenv("SLBH_CLAUDE_HELPER") == "1" {
		runClaudeTestHelper()
		return
	}
	os.Exit(m.Run())
}

func runCodexTestHelper() {
	decoder := json.NewDecoder(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	for {
		var request map[string]any
		if err := decoder.Decode(&request); err != nil {
			return
		}
		id, hasID := request["id"]
		method, _ := request["method"].(string)
		if !hasID {
			continue
		}
		switch method {
		case "initialize":
			_ = encoder.Encode(map[string]any{"id": id, "result": map[string]any{}})
		case "thread/start":
			params, _ := request["params"].(map[string]any)
			if path := os.Getenv("SLBH_CODEX_MODEL_FILE"); path != "" {
				model, _ := json.Marshal(params["model"])
				_ = os.WriteFile(path, model, 0o600)
			}
			_ = encoder.Encode(map[string]any{"id": id, "result": map[string]any{"thread": map[string]any{"id": "test-thread"}}})
		default:
			_ = encoder.Encode(map[string]any{"id": id, "result": map[string]any{}})
		}
	}
}

func fakeCodexLeaf(t *testing.T, r *Runtime, parent *Agent) (*Agent, net.Conn) {
	return fakeCodexLeafWithEffort(t, r, parent, "high")
}

func fakeCodexLeafWithEffort(t *testing.T, r *Runtime, parent *Agent, effort string) (*Agent, net.Conn) {
	t.Helper()
	if parent.Depth != 1 || parent.Harness != "native" {
		t.Fatalf("fake Codex leaf parent = depth %d harness %q, want native depth-1 Manager", parent.Depth, parent.Harness)
	}
	agent, err := r.newAgent("codex", parent.ID, parent.Depth+1, "gpt-test", effort)
	if err != nil {
		t.Fatal(err)
	}
	agent.Harness = "codex"
	agent.WorkDir = r.workDir
	client, server := net.Pipe()
	rpc := newCodexRPC(client, client, func() error {
		_ = client.Close()
		return nil
	})
	leaf := &codexLeaf{agent: agent, runtime: r, rpc: rpc, answers: make(map[string]string), finals: make(map[string]bool), completed: make(map[string]bool), wake: make(chan struct{}, 1), done: make(chan struct{})}
	agent.codexMu.Lock()
	agent.codex = leaf
	agent.codexMu.Unlock()
	go leaf.run()
	t.Cleanup(func() {
		leaf.stop()
		_ = server.Close()
	})
	return agent, server
}

func TestCodexLeafTurnStartEffort(t *testing.T) {
	tests := []struct {
		name       string
		effort     string
		want       string
		wantEffort bool
	}{
		{name: "mapped", effort: "max", want: "max", wantEffort: true},
		{name: "empty", effort: "", wantEffort: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			r, err := New(config.Config{Home: t.TempDir()}, Options{Provider: func(string) (provider.Provider, error) { return fakeProvider{}, nil }})
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			manager := launchTestManager(t, r)
			child, server := fakeCodexLeafWithEffort(t, r, manager, test.effort)
			reader, writer := bufio.NewReader(server), bufio.NewWriter(server)
			if err := child.Send("brief"); err != nil {
				t.Fatal(err)
			}
			initialize := readCodexWire(t, reader)
			respondCodex(t, writer, initialize, map[string]any{})
			_ = readCodexWire(t, reader) // initialized
			thread := readCodexWire(t, reader)
			respondCodex(t, writer, thread, map[string]any{"thread": map[string]any{"id": "effort-thread"}})
			turn := readCodexWire(t, reader)
			if turn.Method != "turn/start" {
				t.Fatalf("request = %q, want turn/start", turn.Method)
			}
			var params map[string]any
			if err := json.Unmarshal(turn.Params, &params); err != nil {
				t.Fatal(err)
			}
			got, present := params["effort"]
			if present != test.wantEffort || (present && got != test.want) {
				t.Fatalf("turn/start effort = %#v (present %v), want %q (present %v)", got, present, test.want, test.wantEffort)
			}
		})
	}
}

func readCodexWire(t *testing.T, reader *bufio.Reader) codexWire {
	t.Helper()
	_ = reader
	var wire codexWire
	if err := json.NewDecoder(reader).Decode(&wire); err != nil {
		t.Fatal(err)
	}
	return wire
}

func writeCodexWire(t *testing.T, writer *bufio.Writer, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(append(data, '\n')); err != nil {
		t.Fatal(err)
	}
	if err := writer.Flush(); err != nil {
		t.Fatal(err)
	}
}

func waitForCodexReady(t *testing.T, r *Runtime, agentID string) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case event := <-testEvents(r):
			if event.AgentID == agentID && event.Kind == "codex_ready" {
				return
			}
			if event.AgentID == agentID && event.Kind == "error" {
				t.Fatalf("Codex leaf failed to start: %s", event.Text)
			}
		case <-deadline.C:
			t.Fatalf("Codex leaf %s did not become ready", agentID)
		}
	}
}

func TestManagerCodexLaunchPreservesExplicitChatGPTModel(t *testing.T) {
	modelFile := filepath.Join(t.TempDir(), "model.json")
	t.Setenv("SLBH_CODEX_HELPER", "1")
	t.Setenv("SLBH_CODEX_MODEL_FILE", modelFile)
	r, err := New(config.Config{
		Home: t.TempDir(), SeatModel: "native-seat", SubagentModel: "native-child", LeafModel: "native-leaf",
		ApprovedModels: []string{"native-seat", "native-child", "native-leaf"},
	}, Options{
		Provider:     func(string) (provider.Provider, error) { return fakeProvider{}, nil },
		CodexCommand: os.Args[0],
	})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	manager := launchTestManager(t, r)
	models := []struct {
		name  string
		model string
	}{
		{name: "luna", model: "gpt-5.6-luna"},
		{name: "sol", model: "gpt-5.6-sol"},
	}

	for _, test := range models {
		t.Run(test.name, func(t *testing.T) {
			if err := os.Remove(modelFile); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			input, err := json.Marshal(map[string]string{
				"title": "codex " + test.name, "harness": "codex", "model": test.model,
			})
			if err != nil {
				t.Fatal(err)
			}
			childID, err := r.ExecuteTool(manager.ID, "launch_subagent", string(input))
			if err != nil {
				t.Fatal(err)
			}
			child, ok := r.lookupAgent(childID)
			if !ok {
				t.Fatalf("launch_subagent returned unknown agent %q", childID)
			}
			waitForCodexReady(t, r, child.ID)
			data, err := os.ReadFile(modelFile)
			if err != nil {
				t.Fatal(err)
			}
			var got string
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatal(err)
			}
			if got != test.model {
				t.Fatalf("Codex model = %q, want explicit ChatGPT model %q", got, test.model)
			}
			if snapshot := child.Snapshot(); snapshot.Harness != "codex" || snapshot.Model != test.model {
				t.Fatalf("Codex snapshot = %#v", snapshot)
			}
			r.endSubagent(manager.ID, child.ID)
		})
	}
}

func respondCodex(t *testing.T, writer *bufio.Writer, request codexWire, result any) {
	t.Helper()
	writeCodexWire(t, writer, map[string]any{"id": json.RawMessage(request.ID), "result": result})
}

func TestCodexLeafSteersActiveTurnAndReturnsResult(t *testing.T) {
	r, err := New(config.Config{Home: t.TempDir()}, Options{Provider: func(string) (provider.Provider, error) { return fakeProvider{}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	manager := launchTestManager(t, r)
	child, server := fakeCodexLeaf(t, r, manager)
	reader, writer := bufio.NewReader(server), bufio.NewWriter(server)

	if err := child.Send("initial brief"); err != nil {
		t.Fatal(err)
	}
	initialize := readCodexWire(t, reader)
	if initialize.Method != "initialize" {
		t.Fatalf("first request = %q, want initialize", initialize.Method)
	}
	respondCodex(t, writer, initialize, map[string]any{"userAgent": "fake"})
	initialized := readCodexWire(t, reader)
	if initialized.Method != "initialized" {
		t.Fatalf("second message = %q, want initialized", initialized.Method)
	}
	startThread := readCodexWire(t, reader)
	if startThread.Method != "thread/start" {
		t.Fatalf("third request = %q, want thread/start", startThread.Method)
	}
	var startParams map[string]any
	if err := json.Unmarshal(startThread.Params, &startParams); err != nil {
		t.Fatal(err)
	}
	if startParams["approvalPolicy"] != "never" || startParams["sandbox"] != "danger-full-access" {
		t.Fatalf("unexpected thread settings: %#v", startParams)
	}
	configValue, configOK := startParams["config"].(map[string]any)
	agentsValue, agentsOK := configValue["agents"].(map[string]any)
	if !configOK || !agentsOK || agentsValue["enabled"] != false {
		t.Fatalf("Codex leaf did not disable native delegation: %#v", startParams["config"])
	}
	respondCodex(t, writer, startThread, map[string]any{"thread": map[string]any{"id": "thread-1"}})
	turnStart := readCodexWire(t, reader)
	if turnStart.Method != "turn/start" {
		t.Fatalf("first turn request = %q, want turn/start", turnStart.Method)
	}
	respondCodex(t, writer, turnStart, map[string]any{"turn": map[string]any{"id": "turn-1", "status": "inProgress"}})
	writeCodexWire(t, writer, map[string]any{"method": "turn/started", "params": map[string]any{"threadId": "thread-1", "turn": map[string]any{"id": "turn-1"}}})

	if err := child.Steer("change direction"); err != nil {
		t.Fatal(err)
	}
	steer := readCodexWire(t, reader)
	if steer.Method != "turn/steer" {
		t.Fatalf("active message request = %q, want turn/steer", steer.Method)
	}
	var steerParams map[string]any
	if err := json.Unmarshal(steer.Params, &steerParams); err != nil {
		t.Fatal(err)
	}
	if steerParams["expectedTurnId"] != "turn-1" {
		t.Fatalf("steer used wrong turn: %#v", steerParams)
	}
	respondCodex(t, writer, steer, map[string]any{"turnId": "turn-1"})
	writeCodexWire(t, writer, map[string]any{"method": "item/agentMessage/delta", "params": map[string]any{"threadId": "thread-1", "turnId": "turn-1", "itemId": "item-1", "delta": "answer"}})
	writeCodexWire(t, writer, map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": "thread-1", "turn": map[string]any{"id": "turn-1", "status": "completed"}}})

	deadline := time.After(3 * time.Second)
	var sawParent, sawDone bool
	for !sawParent || !sawDone {
		select {
		case event := <-testEvents(r):
			if event.AgentID == child.ID && event.Kind == "turn_done" {
				sawDone = true
			}
			if event.AgentID == manager.ID && event.Kind == "child_result" && strings.Contains(event.Text, "answer") {
				sawParent = true
			}
		case <-deadline:
			t.Fatalf("leaf result state parent=%v done=%v", sawParent, sawDone)
		}
	}
	path, err := r.TranscriptPath(child.ID)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := logx.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	var sawSteer bool
	for _, entry := range entries {
		if entry.Kind == "steer" && entry.Text == "change direction" {
			sawSteer = true
		}
	}
	if !sawSteer {
		t.Fatalf("transcript did not record Codex steer: %#v", entries)
	}
	if err := child.Send("idle follow-up"); err != nil {
		t.Fatal(err)
	}
	nextStart := readCodexWire(t, reader)
	if nextStart.Method != "turn/start" {
		t.Fatalf("idle message request = %q, want turn/start", nextStart.Method)
	}
	respondCodex(t, writer, nextStart, map[string]any{"turn": map[string]any{"id": "turn-2", "status": "inProgress"}})
	writeCodexWire(t, writer, map[string]any{"method": "turn/started", "params": map[string]any{"threadId": "thread-1", "turn": map[string]any{"id": "turn-2"}}})
	writeCodexWire(t, writer, map[string]any{"method": "item/agentMessage/delta", "params": map[string]any{"threadId": "thread-1", "turnId": "turn-2", "itemId": "item-2", "delta": "followed"}})
	writeCodexWire(t, writer, map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": "thread-1", "turn": map[string]any{"id": "turn-2", "status": "completed"}}})
	deadline = time.After(3 * time.Second)
	for {
		select {
		case event := <-testEvents(r):
			if event.AgentID == child.ID && event.Kind == "turn_done" && strings.Contains(event.Text, "followed") {
				return
			}
		case <-deadline:
			t.Fatal("idle follow-up did not complete")
		}
	}
}

func TestCodexLeafParentToolAndNoDelegation(t *testing.T) {
	r, err := New(config.Config{Home: t.TempDir()}, Options{Provider: func(string) (provider.Provider, error) { return fakeProvider{}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	manager := launchTestManager(t, r)
	child, server := fakeCodexLeaf(t, r, manager)
	reader, writer := bufio.NewReader(server), bufio.NewWriter(server)

	if err := child.Send("brief"); err != nil {
		t.Fatal(err)
	}
	initialize := readCodexWire(t, reader)
	respondCodex(t, writer, initialize, map[string]any{})
	_ = readCodexWire(t, reader) // initialized
	startThread := readCodexWire(t, reader)
	var params map[string]any
	if err := json.Unmarshal(startThread.Params, &params); err != nil {
		t.Fatal(err)
	}
	dynamic, ok := params["dynamicTools"].([]any)
	if !ok || len(dynamic) != 1 {
		t.Fatalf("Codex leaf did not receive exactly one parent tool: %#v", params["dynamicTools"])
	}
	tool := dynamic[0].(map[string]any)
	if tool["name"] != codexParentTool || strings.Contains(fmt.Sprint(params["developerInstructions"]), "launch") == false {
		t.Fatalf("leaf capability boundary missing: %#v", params)
	}
	respondCodex(t, writer, startThread, map[string]any{"thread": map[string]any{"id": "thread-2"}})
	turnStart := readCodexWire(t, reader)
	respondCodex(t, writer, turnStart, map[string]any{"turn": map[string]any{"id": "turn-2", "status": "inProgress"}})
	writeCodexWire(t, writer, map[string]any{"method": "turn/started", "params": map[string]any{"threadId": "thread-2", "turn": map[string]any{"id": "turn-2"}}})

	writeCodexWire(t, writer, map[string]any{"id": 99, "method": "item/tool/call", "params": map[string]any{"threadId": "thread-2", "turnId": "turn-2", "callId": "call-1", "tool": codexParentTool, "arguments": map[string]any{"message": "progress"}}})
	response := readCodexWire(t, reader)
	if string(response.ID) != "99" || response.Error != nil {
		t.Fatalf("parent tool response = %#v", response)
	}
	var responseResult map[string]any
	if err := json.Unmarshal(response.Result, &responseResult); err != nil {
		t.Fatal(err)
	}
	if responseResult["success"] != true {
		t.Fatalf("parent tool was not accepted: %#v", responseResult)
	}
	deadline := time.After(3 * time.Second)
	for {
		select {
		case event := <-testEvents(r):
			if event.Kind == "child_message" {
				goto childMessageRecorded
			}
		case <-deadline:
			t.Fatal("parent tool event not recorded")
		}
	}
childMessageRecorded:
	if _, err := r.launchSubagentSpec(child.ID, LaunchSpec{Title: "nested", Harness: "codex", Model: "gpt-5.6-luna"}); err == nil {
		t.Fatal("Codex leaf was allowed to launch another leaf")
	}
}

func TestCodexLeafSteerRaceIsRequeuedAfterTurnCompletion(t *testing.T) {
	r, err := New(config.Config{Home: t.TempDir()}, Options{Provider: func(string) (provider.Provider, error) { return fakeProvider{}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	manager := launchTestManager(t, r)
	child, server := fakeCodexLeaf(t, r, manager)
	reader, writer := bufio.NewReader(server), bufio.NewWriter(server)
	if err := child.Send("first"); err != nil {
		t.Fatal(err)
	}
	initialize := readCodexWire(t, reader)
	respondCodex(t, writer, initialize, map[string]any{})
	_ = readCodexWire(t, reader)
	thread := readCodexWire(t, reader)
	respondCodex(t, writer, thread, map[string]any{"thread": map[string]any{"id": "race-thread"}})
	first := readCodexWire(t, reader)
	respondCodex(t, writer, first, map[string]any{"turn": map[string]any{"id": "race-turn-1", "status": "inProgress"}})
	writeCodexWire(t, writer, map[string]any{"method": "turn/started", "params": map[string]any{"threadId": "race-thread", "turn": map[string]any{"id": "race-turn-1"}}})

	if err := child.Steer("must survive race"); err != nil {
		t.Fatal(err)
	}
	steer := readCodexWire(t, reader)
	if steer.Method != "turn/steer" {
		t.Fatalf("request = %q, want turn/steer", steer.Method)
	}
	// Model the server deciding that the turn completed just before it
	// processed the steer. The adapter must retry the exact input on a new turn.
	writeCodexWire(t, writer, map[string]any{"id": json.RawMessage(steer.ID), "error": map[string]any{"code": -32000, "message": "no active turn"}})
	writeCodexWire(t, writer, map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": "race-thread", "turn": map[string]any{"id": "race-turn-1", "status": "completed"}}})
	next := readCodexWire(t, reader)
	if next.Method != "turn/start" {
		t.Fatalf("requeued request = %q, want turn/start", next.Method)
	}
	var nextParams map[string]any
	if err := json.Unmarshal(next.Params, &nextParams); err != nil {
		t.Fatal(err)
	}
	input := nextParams["input"].([]any)[0].(map[string]any)
	if input["text"] != "must survive race" {
		t.Fatalf("requeued text = %#v", input["text"])
	}
	respondCodex(t, writer, next, map[string]any{"turn": map[string]any{"id": "race-turn-2", "status": "inProgress"}})
	writeCodexWire(t, writer, map[string]any{"method": "turn/started", "params": map[string]any{"threadId": "race-thread", "turn": map[string]any{"id": "race-turn-2"}}})
	writeCodexWire(t, writer, map[string]any{"method": "item/agentMessage/delta", "params": map[string]any{"threadId": "race-thread", "turnId": "race-turn-2", "itemId": "race-item", "delta": "recovered"}})
	writeCodexWire(t, writer, map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": "race-thread", "turn": map[string]any{"id": "race-turn-2", "status": "completed"}}})
	deadline := time.After(3 * time.Second)
	for {
		select {
		case event := <-testEvents(r):
			if event.AgentID == child.ID && event.Kind == "turn_done" && strings.Contains(event.Text, "recovered") {
				return
			}
		case <-deadline:
			t.Fatal("requeued turn did not finish")
		}
	}
}

// TestCodexLeafReturnsTheFinalAnswerNotTheStream replays the wire shape
// codex-cli 0.155.0's app-server sends, read on 2026-09-18: item/completed
// carries threadId and turnId beside the item rather than inside it, and a
// turn can complete a commentary message before its final answer. The leaf
// used to read turnId from inside the item, file the completed text under an
// empty key, and hand the parent the deltas of both messages run together.
func TestCodexLeafReturnsTheFinalAnswerNotTheStream(t *testing.T) {
	r, err := New(config.Config{Home: t.TempDir()}, Options{Provider: func(string) (provider.Provider, error) { return fakeProvider{}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	manager := launchTestManager(t, r)
	child, server := fakeCodexLeaf(t, r, manager)
	reader, writer := bufio.NewReader(server), bufio.NewWriter(server)
	if err := child.Send("brief"); err != nil {
		t.Fatal(err)
	}
	initialize := readCodexWire(t, reader)
	respondCodex(t, writer, initialize, map[string]any{})
	_ = readCodexWire(t, reader)
	thread := readCodexWire(t, reader)
	respondCodex(t, writer, thread, map[string]any{"thread": map[string]any{"id": "t"}})
	start := readCodexWire(t, reader)
	respondCodex(t, writer, start, map[string]any{"turn": map[string]any{"id": "u", "status": "inProgress"}})
	writeCodexWire(t, writer, map[string]any{"method": "turn/started", "params": map[string]any{"threadId": "t", "turn": map[string]any{"id": "u"}}})
	message := func(item, phase, text string) {
		writeCodexWire(t, writer, map[string]any{"method": "item/agentMessage/delta", "params": map[string]any{"threadId": "t", "turnId": "u", "itemId": item, "delta": text}})
		writeCodexWire(t, writer, map[string]any{"method": "item/completed", "params": map[string]any{"threadId": "t", "turnId": "u", "item": map[string]any{"type": "agentMessage", "id": item, "phase": phase, "text": text}}})
	}
	message("c", "commentary", "Checking the file first.")
	message("f", "final_answer", "delta624 0")
	writeCodexWire(t, writer, map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": "t", "turn": map[string]any{"id": "u", "status": "completed"}}})
	deadline := time.After(3 * time.Second)
	for {
		select {
		case event := <-testEvents(r):
			if event.AgentID == child.ID && event.Kind == "turn_done" {
				if event.Text != "delta624 0" {
					t.Fatalf("turn_done text = %q, want only the final answer", event.Text)
				}
				return
			}
		case <-deadline:
			t.Fatal("turn did not finish")
		}
	}
}

func TestCodexLeafClearStartsFreshThread(t *testing.T) {
	r, err := New(config.Config{Home: t.TempDir()}, Options{Provider: func(string) (provider.Provider, error) { return fakeProvider{}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	manager := launchTestManager(t, r)
	child, server := fakeCodexLeaf(t, r, manager)
	reader, writer := bufio.NewReader(server), bufio.NewWriter(server)
	if err := child.Send("initial"); err != nil {
		t.Fatal(err)
	}
	initialize := readCodexWire(t, reader)
	respondCodex(t, writer, initialize, map[string]any{})
	_ = readCodexWire(t, reader)
	thread := readCodexWire(t, reader)
	respondCodex(t, writer, thread, map[string]any{"thread": map[string]any{"id": "clear-thread-1"}})
	turn := readCodexWire(t, reader)
	respondCodex(t, writer, turn, map[string]any{"turn": map[string]any{"id": "clear-turn", "status": "inProgress"}})
	oldPath, err := r.TranscriptPath(child.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.clear(child.ID); err != nil {
		t.Fatal(err)
	}
	interrupt := readCodexWire(t, reader)
	if interrupt.Method != "turn/interrupt" {
		t.Fatalf("clear request = %q, want turn/interrupt", interrupt.Method)
	}
	respondCodex(t, writer, interrupt, map[string]any{})
	newThread := readCodexWire(t, reader)
	if newThread.Method != "thread/start" {
		t.Fatalf("clear restart request = %q, want thread/start", newThread.Method)
	}
	respondCodex(t, writer, newThread, map[string]any{"thread": map[string]any{"id": "clear-thread-2"}})
	newPath, err := r.TranscriptPath(child.ID)
	if err != nil {
		t.Fatal(err)
	}
	if newPath == oldPath {
		t.Fatalf("clear reused transcript %q", newPath)
	}
	if _, err := r.compact(child.ID, 4); err != nil {
		t.Fatal(err)
	}
	compact := readCodexWire(t, reader)
	if compact.Method != "thread/compact/start" {
		t.Fatalf("compact request = %q, want thread/compact/start", compact.Method)
	}
	var compactParams map[string]any
	if err := json.Unmarshal(compact.Params, &compactParams); err != nil {
		t.Fatal(err)
	}
	if compactParams["threadId"] != "clear-thread-2" {
		t.Fatalf("compact used wrong thread: %#v", compactParams)
	}
	respondCodex(t, writer, compact, map[string]any{"threadId": "clear-thread-2"})
	deadline := time.After(3 * time.Second)
	for {
		select {
		case event := <-testEvents(r):
			if event.AgentID == child.ID && event.Kind == "compact" {
				return
			}
		case <-deadline:
			t.Fatal("Codex compaction did not complete")
		}
	}
}

func TestCodexItemStatus(t *testing.T) {
	for _, tc := range []struct {
		typ, tool, want string
	}{
		{"reasoning", "", "thinking"},
		{"agentMessage", "", "output"},
		{"commandExecution", "", "tool:exec"},
		{"fileChange", "", "tool:apply_patch"},
		{"mcpToolCall", "search", "tool:search"},
		{"mcpToolCall", "", "tool:mcpToolCall"},
		{"userMessage", "", ""},
	} {
		item := map[string]json.RawMessage{}
		if tc.tool != "" {
			item["tool"] = json.RawMessage(`"` + tc.tool + `"`)
		}
		if got := codexItemStatus(tc.typ, item); got != tc.want {
			t.Fatalf("codexItemStatus(%q, tool %q) = %q, want %q", tc.typ, tc.tool, got, tc.want)
		}
	}
}
