package headless

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/slbdotdev/slbh/internal/config"
	"github.com/slbdotdev/slbh/internal/harness"
	"github.com/slbdotdev/slbh/internal/provider"
)

type answering struct{}

func (answering) Stream(_ context.Context, _ provider.Request, sink provider.StreamSink) error {
	if err := sink(provider.Event{Kind: provider.EventText, Text: "SLBH_HEADLESS_OK"}); err != nil {
		return err
	}
	return sink(provider.Event{Kind: provider.EventDone})
}

type stalling struct{}

func (stalling) Stream(ctx context.Context, _ provider.Request, _ provider.StreamSink) error {
	<-ctx.Done()
	return ctx.Err()
}

func newRuntime(t *testing.T, p provider.Provider) *harness.Runtime {
	t.Helper()
	rt, err := harness.New(
		config.Config{Home: t.TempDir(), SeatModel: "test", SeatEffort: "high"},
		harness.Options{Provider: func(string) (provider.Provider, error) { return p, nil }},
	)
	if err != nil {
		t.Fatalf("harness.New: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	return rt
}

func TestServeIsPersistentAndCarriesTheRuntimeEventStream(t *testing.T) {
	rt := newRuntime(t, answering{})
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- Serve(context.Background(), rt, inR, outW, Options{})
		_ = outW.Close()
	}()
	decoder := json.NewDecoder(outR)
	seatID := rt.Agents()[0].ID

	// Each prompt runs to turn_done before the next is sent, so both are
	// separate turns on one protocol session rather than one merged turn.
	var messages []map[string]json.RawMessage
	for id, prompt := range []string{"first", "second"} {
		writeRequest(t, inW, id+1, "send_prompt", map[string]any{"agent_id": seatID, "prompt": prompt})
		messages = append(messages, readUntilEvent(t, decoder, "turn_done")...)
	}
	writeRequest(t, inW, 3, "close", map[string]any{})
	messages = append(messages, readUntilID(t, decoder, 3)...)
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
	if !hasResponseID(messages, 1) || !hasResponseID(messages, 2) || !hasResponseID(messages, 3) {
		t.Fatalf("protocol responses missing: %v", messages)
	}
	if got := countAssistantEvents(messages); got != 2 {
		t.Fatalf("assistant events = %d, want one per prompt: %v", got, messages)
	}
}

func TestServeInitialPromptAndQueries(t *testing.T) {
	rt := newRuntime(t, answering{})
	inR, inW := io.Pipe()
	var output bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- Serve(context.Background(), rt, inR, &output, Options{InitialPrompt: "initial"})
	}()
	writeRequest(t, inW, 1, "initialize", map[string]any{})
	writeRequest(t, inW, 2, "agents", map[string]any{})
	writeRequest(t, inW, 3, "close", map[string]any{})
	_ = inW.Close()
	if err := <-done; err != nil {
		t.Fatalf("Serve: %v", err)
	}
	messages := decodeMessages(t, output.Bytes())
	if !hasResponseID(messages, 1) || !hasResponseID(messages, 2) || !hasResponseID(messages, 3) {
		t.Fatalf("query responses missing: %v", messages)
	}
}

func TestServeReportsProtocolErrorsWithoutEndingTheRuntime(t *testing.T) {
	rt := newRuntime(t, stalling{})
	input := strings.NewReader(`{"id":1,"method":"not_a_method","params":{}}
{"id":2,"method":"close","params":{}}
`)
	var output strings.Builder
	if err := Serve(context.Background(), rt, input, &output, Options{}); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if !strings.Contains(output.String(), `"code":"request_failed"`) || !strings.Contains(output.String(), `"id":2`) {
		t.Fatalf("protocol output = %q", output.String())
	}
}

func TestServeRejectsNilRuntime(t *testing.T) {
	if err := Serve(context.Background(), nil, strings.NewReader("{}\n"), io.Discard, Options{}); err == nil {
		t.Fatal("Serve(nil) succeeded")
	}
}

func writeRequest(t *testing.T, w *io.PipeWriter, id int, method string, params map[string]any) {
	t.Helper()
	request := map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}
	if err := json.NewEncoder(w).Encode(request); err != nil {
		t.Fatalf("write request: %v", err)
	}
}

func readUntilID(t *testing.T, decoder *json.Decoder, id int) []map[string]json.RawMessage {
	t.Helper()
	var messages []map[string]json.RawMessage
	want := json.RawMessage(strconv.Itoa(id))
	for {
		var message map[string]json.RawMessage
		if err := decoder.Decode(&message); err != nil {
			t.Fatalf("decode protocol message: %v", err)
		}
		messages = append(messages, message)
		if string(message["id"]) == string(want) {
			return messages
		}
	}
}

func countAssistantEvents(messages []map[string]json.RawMessage) int {
	count := 0
	for _, message := range messages {
		if string(message["method"]) != `"event"` {
			continue
		}
		var event struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal(message["params"], &event); err == nil && event.Kind == "assistant" {
			count++
		}
	}
	return count
}

func decodeMessages(t *testing.T, data []byte) []map[string]json.RawMessage {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(data))
	var messages []map[string]json.RawMessage
	for {
		var message map[string]json.RawMessage
		err := decoder.Decode(&message)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("decode protocol output: %v", err)
		}
		messages = append(messages, message)
	}
	return messages
}

func hasResponseID(messages []map[string]json.RawMessage, id int) bool {
	want := strconv.Itoa(id)
	for _, message := range messages {
		if string(message["id"]) == want {
			return true
		}
	}
	return false
}

func readUntilEvent(t *testing.T, decoder *json.Decoder, kind string) []map[string]json.RawMessage {
	t.Helper()
	var messages []map[string]json.RawMessage
	for {
		var message map[string]json.RawMessage
		if err := decoder.Decode(&message); err != nil {
			t.Fatalf("decode event message: %v", err)
		}
		messages = append(messages, message)
		if string(message["method"]) != `"event"` {
			continue
		}
		var event struct {
			Kind string `json:"kind"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(message["params"], &event); err != nil {
			continue
		}
		if event.Kind == kind {
			return messages
		}
		// A failed turn never reaches turn_done; stop rather than wait forever.
		if event.Kind == "error" {
			t.Fatalf("error event while waiting for %s: %s", kind, event.Text)
		}
	}
}
