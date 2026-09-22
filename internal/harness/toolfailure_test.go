package harness

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/slbdotdev/slbh/internal/config"
	"github.com/slbdotdev/slbh/internal/provider"
)

func TestToolFailureKeepsTheOutputTheToolReturned(t *testing.T) {
	got := toolFailure(errAsError("exit status 1"), "grep: nothing matched\n")
	if !strings.Contains(got, "exit status 1") {
		t.Fatalf("the error was lost: %q", got)
	}
	if !strings.Contains(got, "grep: nothing matched") {
		t.Fatalf("the output was discarded, which is the bug this guards: %q", got)
	}
	if !strings.HasPrefix(got, "tool error: ") {
		t.Fatalf("the error must lead so a long output cannot bury it: %q", got)
	}
}

func TestToolFailureWithNoOutputIsJustTheError(t *testing.T) {
	for _, out := range []string{"", "   ", "\n\t\n"} {
		got := toolFailure(errAsError("boom"), out)
		if got != "tool error: boom" {
			t.Fatalf("output %q: got %q, want the bare error", out, got)
		}
	}
}

func TestToolFailureTruncatesRunawayOutput(t *testing.T) {
	limit := commandOutputTokens * 4
	got := toolFailure(errAsError("exit status 2"), "start"+strings.Repeat("x", limit*2)+"end")
	if len(got) > limit {
		t.Fatalf("result is %d bytes, want at most %d", len(got), limit)
	}
	if !strings.Contains(got, "over the 20000-token limit") || !strings.Contains(got, "read a range instead") {
		t.Fatal("truncation must be visible to the model, not silent")
	}
	if !strings.HasPrefix(got, "tool error: exit status 2\noutput cut: ") || !strings.Contains(got, "start") || !strings.HasSuffix(got, "end") {
		t.Fatalf("the error, the head and the tail must survive truncation: %q", got[:60])
	}
	if again := boundedOutput(strings.TrimPrefix(got, "tool error: exit status 2\n")); again != strings.TrimPrefix(got, "tool error: exit status 2\n") {
		t.Fatal("cutting an already cut result must not cut it again")
	}
}

// errAsError keeps the table tests readable without pulling in errors.New everywhere.
type stringError string

func (e stringError) Error() string { return string(e) }

func errAsError(s string) error { return stringError(s) }

// failingShellProvider asks for one bash call that exits non-zero with output on stderr,
// then ends the turn. It is the end-to-end regression: the model must receive the command's
// own diagnostic, not just the exit code.
type failingShellProvider struct {
	mu      sync.Mutex
	results []string
}

func (p *failingShellProvider) Stream(_ context.Context, request provider.Request, sink provider.StreamSink) error {
	for _, m := range request.Messages {
		if m.Role == "tool" {
			p.mu.Lock()
			p.results = append(p.results, m.Content)
			p.mu.Unlock()
			return sink(provider.Event{Kind: provider.EventText, Text: "seen"})
		}
	}
	return sink(provider.Event{
		Kind: provider.EventTool, ToolIndex: 0, ToolCallID: "call-1", ToolName: "bash",
		Input: `{"script":"echo SENTINEL_STDERR 1>&2; exit 3"}`,
	})
}

func TestFailingShellCommandReturnsItsOutputToTheModel(t *testing.T) {
	p := &failingShellProvider{}
	r, err := New(
		config.Config{Home: t.TempDir(), SeatModel: "test", SeatEffort: "high"},
		Options{Provider: func(string) (provider.Provider, error) { return p, nil }},
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer r.Close()

	if err := r.seat().Send("run it"); err != nil {
		t.Fatalf("Send: %v", err)
	}

	deadline := time.After(20 * time.Second)
	for {
		p.mu.Lock()
		n := len(p.results)
		var last string
		if n > 0 {
			last = p.results[n-1]
		}
		p.mu.Unlock()
		if n > 0 {
			if !strings.Contains(last, "exit status 3") {
				t.Fatalf("the exit status was lost: %q", last)
			}
			if !strings.Contains(last, "SENTINEL_STDERR") {
				t.Fatalf("the command's own stderr never reached the model: %q", last)
			}
			return
		}
		select {
		case <-deadline:
			t.Fatal("no tool result reached the provider within the deadline")
		case <-time.After(50 * time.Millisecond):
		}
	}
}
