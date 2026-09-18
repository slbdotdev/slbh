//go:build live_integration

package headless

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/slbdotdev/slbh/internal/config"
	"github.com/slbdotdev/slbh/internal/harness"
)

// The live counterpart of the unit tests: a real provider, a persistent runtime,
// no TUI. Gated behind the live_integration tag because it consumes a real
// endpoint. Run it with SLBH_LIVE_MODEL set to an approved local or plan route.
func TestLiveHeadlessRuntimeCompletes(t *testing.T) {
	model := os.Getenv("SLBH_LIVE_MODEL")
	if model == "" {
		t.Skip("set SLBH_LIVE_MODEL to run the live headless check")
	}
	cfg := config.Load()
	cfg.SeatModel = model
	if !cfg.ModelApproved(model) {
		cfg.ApprovedModels = append(cfg.ApprovedModels, model)
	}
	if effort := os.Getenv("SLBH_LIVE_EFFORT"); effort != "" {
		cfg.SeatEffort = effort
	}
	rt, err := harness.New(cfg, harness.Options{Config: cfg})
	if err != nil {
		t.Fatalf("harness.New: %v", err)
	}
	defer rt.Close()

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- Serve(context.Background(), rt, inR, outW, Options{
			InitialPrompt: "Reply with exactly SLBH_HEADLESS_LIVE_OK and nothing else. Do not call tools.",
		})
	}()
	decoder := json.NewDecoder(bufio.NewReader(outR))
	// Assistant text arrives as streamed fragments — a Codex delta or a native
	// provider chunk can split the marker anywhere — so the reply is judged
	// whole, once the turn is done, and never fragment by fragment.
	var reply strings.Builder
	for done := false; !done; {
		var message map[string]json.RawMessage
		if err := decoder.Decode(&message); err != nil {
			t.Fatalf("decode live event: %v", err)
		}
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
		switch event.Kind {
		case "assistant":
			reply.WriteString(event.Text)
		case "error":
			t.Fatalf("live turn failed: %s", event.Text)
		case "turn_done":
			done = true
		}
	}
	if !strings.Contains(reply.String(), "SLBH_HEADLESS_LIVE_OK") {
		t.Fatalf("reply %q does not carry the marker", reply.String())
	}
	writeRequest(t, inW, 1, "close", map[string]any{})
	readUntilID(t, decoder, 1)
	_ = inW.Close()
	if err := <-done; err != nil {
		t.Fatalf("Serve: %v", err)
	}
}
