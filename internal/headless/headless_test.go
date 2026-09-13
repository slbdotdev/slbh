package headless

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/slbdotdev/slbh/internal/config"
	"github.com/slbdotdev/slbh/internal/harness"
	"github.com/slbdotdev/slbh/internal/provider"
)

// answering replies once and ends the turn, the shape every completed turn has.
type answering struct{}

func (answering) Stream(_ context.Context, _ provider.Request, sink provider.StreamSink) error {
	if err := sink(provider.Event{Kind: provider.EventReasoning, Text: "plan"}); err != nil {
		return err
	}
	if err := sink(provider.Event{Kind: provider.EventText, Text: "SLBH_HEADLESS_OK"}); err != nil {
		return err
	}
	return sink(provider.Event{Kind: provider.EventDone})
}

// stalling never returns until the context is cancelled, so the wall cap is what ends it.
type stalling struct{}

func (stalling) Stream(ctx context.Context, _ provider.Request, _ provider.StreamSink) error {
	<-ctx.Done()
	return ctx.Err()
}

// refusing fails every attempt outright, the shape of a provider that rejects the request
// itself (a 4xx/5xx) rather than one that is slow.
type refusing struct{}

func (refusing) Stream(_ context.Context, _ provider.Request, _ provider.StreamSink) error {
	return errors.New("provider returned 500 Internal Server Error")
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

func TestRunReturnsWhenTheSeatTurnCompletes(t *testing.T) {
	rt := newRuntime(t, answering{})
	var out bytes.Buffer
	res, err := Run(rt, Options{Prompt: "hello", Timeout: 30 * time.Second, Out: &out})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.StopReason != "done" {
		t.Fatalf("stop_reason = %q, want done", res.StopReason)
	}
	if res.Turns < 1 {
		t.Fatalf("turns = %d, want >= 1", res.Turns)
	}
	if res.AgentID == "" {
		t.Fatal("agent_id is empty")
	}
	if res.WallS <= 0 {
		t.Fatalf("wall_s = %v, want > 0", res.WallS)
	}
	if !strings.Contains(res.Final, "SLBH_HEADLESS_OK") {
		t.Fatalf("final = %q, want the assistant text", res.Final)
	}
	if got := out.String(); !strings.Contains(got, "SLBH_HEADLESS_OK") {
		t.Fatalf("event stream did not carry the reply: %q", got)
	}
}

func TestRunStopsAtTheWallCap(t *testing.T) {
	rt := newRuntime(t, stalling{})
	res, err := Run(rt, Options{Prompt: "hello", Timeout: 150 * time.Millisecond})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.StopReason != "wall_cap" {
		t.Fatalf("stop_reason = %q, want wall_cap", res.StopReason)
	}
}

// A provider that refuses ends the seat's turn without a turn_done. Run must return then,
// with the reason named, instead of sitting out the whole wall cap; a bench caller with a
// fifteen-minute cap would otherwise pay it in full for every refused request.
func TestRunStopsWhenTheSeatFails(t *testing.T) {
	rt := newRuntime(t, refusing{})
	res, err := Run(rt, Options{Prompt: "hello", Timeout: 30 * time.Second})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.StopReason != "error" {
		t.Fatalf("stop_reason = %q, want error", res.StopReason)
	}
	if res.Errors < 1 {
		t.Fatalf("errors = %d, want >= 1", res.Errors)
	}
	if res.WallS >= 10 {
		t.Fatalf("wall_s = %v, want well under the 30s cap", res.WallS)
	}
}

func TestQuietSuppressesTheEventStreamButNotTheResult(t *testing.T) {
	rt := newRuntime(t, answering{})
	var out bytes.Buffer
	res, err := Run(rt, Options{Prompt: "hello", Timeout: 30 * time.Second, Quiet: true, Out: &out})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("quiet wrote %d bytes, want 0", out.Len())
	}
	if res.Turns < 1 {
		t.Fatalf("turns = %d, want >= 1 even when quiet", res.Turns)
	}
}

func TestJSONStreamIsOnePerLine(t *testing.T) {
	rt := newRuntime(t, answering{})
	var out bytes.Buffer
	if _, err := Run(rt, Options{Prompt: "hello", Timeout: 30 * time.Second, JSON: true, Out: &out}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "{") || !strings.HasSuffix(line, "}") {
			t.Fatalf("not a JSON object per line: %q", line)
		}
	}
}

// Usage counts arrive as int in-process and as float64 when they have been through JSON.
// Both must count, or token totals silently read zero on one of the two paths.
func TestMetaIntAcceptsBothNumericShapes(t *testing.T) {
	for _, tc := range []struct {
		name string
		meta map[string]any
		want int
		ok   bool
	}{
		{"int", map[string]any{"prompt_tokens": 7}, 7, true},
		{"int64", map[string]any{"prompt_tokens": int64(7)}, 7, true},
		{"float64", map[string]any{"prompt_tokens": float64(7)}, 7, true},
		{"missing", map[string]any{}, 0, false},
		{"nil", nil, 0, false},
		{"wrong type", map[string]any{"prompt_tokens": "7"}, 0, false},
	} {
		got, ok := metaInt(tc.meta, "prompt_tokens")
		if got != tc.want || ok != tc.ok {
			t.Fatalf("%s: metaInt = (%d, %v), want (%d, %v)", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}

func TestRunRejectsARuntimeWithNoSeat(t *testing.T) {
	if _, err := Run(nil, Options{Prompt: "x"}); err == nil {
		t.Fatal("want an error for a nil runtime")
	}
}
