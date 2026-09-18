package harness

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/slbdotdev/slbh/internal/config"
	"github.com/slbdotdev/slbh/internal/logx"
	"github.com/slbdotdev/slbh/internal/provider"
)

// gatedProvider holds its first turn open until Clear cancels it, then lets
// later turns finish. It records requests so the post-clear context can be
// checked directly at the provider boundary.
type gatedProvider struct {
	started  chan struct{}
	canceled chan struct{}
	mu       sync.Mutex
	calls    []provider.Request
}

func (p *gatedProvider) Stream(ctx context.Context, request provider.Request, sink provider.StreamSink) error {
	p.mu.Lock()
	call := len(p.calls)
	p.calls = append(p.calls, request)
	p.mu.Unlock()
	if call == 0 {
		if err := sink(provider.Event{Kind: provider.EventText, Text: "before-clear "}); err != nil {
			return err
		}
		close(p.started)
		<-ctx.Done()
		close(p.canceled)
		return ctx.Err()
	}
	if err := sink(provider.Event{Kind: provider.EventText, Text: "fresh"}); err != nil {
		return err
	}
	return sink(provider.Event{Kind: provider.EventDone})
}

func TestClearCancelsAnInFlightTurnAndStartsWithEmptyHistory(t *testing.T) {
	p := &gatedProvider{started: make(chan struct{}), canceled: make(chan struct{})}
	r, err := New(
		config.Config{Home: t.TempDir(), SeatModel: "test"},
		Options{Provider: func(string) (provider.Provider, error) { return p, nil }},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	seat := r.seat()
	oldPath, err := r.TranscriptPath(seat.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := seat.Send("hello"); err != nil {
		t.Fatal(err)
	}
	<-p.started

	if err := r.clear(seat.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("clear did not cancel the in-flight provider call")
	}

	deadline := time.Now().Add(5 * time.Second)
	var newPath string
	for time.Now().Before(deadline) {
		newPath, err = r.TranscriptPath(seat.ID)
		if err != nil {
			t.Fatal(err)
		}
		if newPath != oldPath {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if newPath == oldPath {
		t.Fatal("the cleared session never became the log target")
	}
	if err := seat.Send("second"); err != nil {
		t.Fatal(err)
	}
	waitForKind(t, r, "turn_done")

	p.mu.Lock()
	calls := append([]provider.Request(nil), p.calls...)
	p.mu.Unlock()
	if len(calls) != 2 {
		t.Fatalf("provider calls = %d, want initial and post-clear calls", len(calls))
	}
	if len(calls[1].Messages) != 1 || calls[1].Messages[0].Role != "user" || calls[1].Messages[0].Content != "second" {
		t.Fatalf("post-clear request reused old history: %#v", calls[1].Messages)
	}
	if transcriptHasText(t, newPath, "hello") {
		t.Fatal("new transcript retained the cleared prompt")
	}
}

func waitForKind(t *testing.T, r *Runtime, kind string) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case event := <-testEvents(r):
			if event.Kind == kind {
				return
			}
		case <-deadline:
			t.Fatalf("no %q event arrived", kind)
		}
	}
}

// transcriptHasText reads a transcript off disk and defers the matching to the
// package's existing helper.
func transcriptHasText(t *testing.T, path, want string) bool {
	t.Helper()
	entries, err := logx.Read(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return transcriptContains(entries, want)
}
