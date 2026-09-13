//go:build live_integration

package headless

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/slbdotdev/slbh/internal/config"
	"github.com/slbdotdev/slbh/internal/harness"
)

// The live counterpart of the unit tests: a real provider, a real turn, no TUI. Gated behind
// the live_integration tag because it consumes a real endpoint. Run it with:
//
//	SLBH_LIVE_MODEL=local/q27-IQ2_M-64k \
//	  go test -tags live_integration ./internal/headless/ -run TestLive -v
//
// SLBH_LIVE_MODEL is required; the test skips without it rather than guessing a model and
// spending someone's quota or GPU on a default.
func TestLiveHeadlessTurnCompletes(t *testing.T) {
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

	var out bytes.Buffer
	res, err := Run(rt, Options{
		Prompt:  "Reply with exactly SLBH_HEADLESS_LIVE_OK and nothing else. Do not call tools.",
		Timeout: 5 * time.Minute,
		Out:     &out,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.StopReason != "done" {
		t.Fatalf("stop_reason = %q, want done (wall_cap means the turn never finished)", res.StopReason)
	}
	if res.Turns < 1 {
		t.Fatalf("turns = %d, want >= 1", res.Turns)
	}
	if !strings.Contains(res.Final+out.String(), "SLBH_HEADLESS_LIVE_OK") {
		t.Fatalf("the model's reply never arrived; final=%q", res.Final)
	}
	t.Logf("live headless: turns=%d tool_calls=%d prompt_tokens=%d output_tokens=%d wall=%.1fs",
		res.Turns, res.ToolCalls, res.PromptTokens, res.OutputTokens, res.WallS)
}
