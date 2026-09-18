package harness

import (
	"context"
	"testing"
	"time"

	"github.com/slbdotdev/slbh/internal/config"
	"github.com/slbdotdev/slbh/internal/logx"
	"github.com/slbdotdev/slbh/internal/provider"
)

// gatedProvider streams one delta, waits to be released, then streams a second
// and finishes. It holds a turn open across a Clear, which is the only way to
// reach the case this tests.
type gatedProvider struct {
	started chan struct{}
	release chan struct{}
}

func (p *gatedProvider) Stream(ctx context.Context, _ provider.Request, sink provider.StreamSink) error {
	if err := sink(provider.Event{Kind: provider.EventText, Text: "before-clear "}); err != nil {
		return err
	}
	close(p.started)
	select {
	case <-p.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := sink(provider.Event{Kind: provider.EventText, Text: "after-clear"}); err != nil {
		return err
	}
	return sink(provider.Event{Kind: provider.EventDone})
}

func TestClearDefersTheLogSwapUntilAnInFlightTurnEnds(t *testing.T) {
	// Clear used to replace the log target immediately while the provider call
	// kept streaming. emit resolves the session per event, so every later
	// delta, the usage and the turn_done were written into a transcript that
	// never issued the request: the new file opened mid-answer to a question it
	// did not contain, and the old one lost its own turn's ending.
	p := &gatedProvider{started: make(chan struct{}), release: make(chan struct{})}
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
	during, err := r.TranscriptPath(seat.ID)
	if err != nil {
		t.Fatal(err)
	}
	if during != oldPath {
		t.Fatalf("the log target swapped while a turn was in flight: %q", during)
	}

	close(p.release)
	waitForKind(t, r, "turn_done")

	newPath, err := r.TranscriptPath(seat.ID)
	if err != nil {
		t.Fatal(err)
	}
	if newPath == oldPath {
		t.Fatal("the cleared session never became the log target")
	}
	if !transcriptHasText(t, oldPath, "after-clear") {
		t.Fatal("the turn that issued the request lost its own tail")
	}
	if transcriptHasText(t, newPath, "after-clear") {
		t.Fatal("the new transcript opens with output from a turn it never issued")
	}
}

func waitForKind(t *testing.T, r *Runtime, kind string) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case event := <-r.Events():
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
