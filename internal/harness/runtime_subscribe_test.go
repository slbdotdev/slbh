package harness

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/slbdotdev/slbh/internal/config"
	"github.com/slbdotdev/slbh/internal/provider"
	"github.com/slbdotdev/slbh/internal/seam"
)

func subscriptionRuntime(t *testing.T) *Runtime {
	t.Helper()
	r, err := New(
		config.Config{Home: t.TempDir(), SeatModel: "test", SeatEffort: "high"},
		Options{Provider: func(string) (provider.Provider, error) { return fakeProvider{}, nil }},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

func TestRuntimeConsumersSeeSameOrderedEventsAfterTheirCursors(t *testing.T) {
	r := subscriptionRuntime(t)
	baseline := r.PollEvents(seam.EventQuery{}).Cursor
	firstCursor, secondCursor := baseline, baseline

	const count = 32
	for i := 0; i < count; i++ {
		r.emit(seam.Event{Kind: "test", Text: fmt.Sprintf("event-%02d", i)})
	}
	first := r.PollEvents(seam.EventQuery{After: firstCursor})
	second := r.PollEvents(seam.EventQuery{After: secondCursor})
	if len(first.Events) != count || len(second.Events) != count {
		t.Fatalf("consumer event counts = %d/%d, want %d", len(first.Events), len(second.Events), count)
	}
	for i := range first.Events {
		want := fmt.Sprintf("event-%02d", i)
		if first.Events[i].Text != want || second.Events[i].Text != want {
			t.Fatalf("event %d = %q/%q, want %q", i, first.Events[i].Text, second.Events[i].Text, want)
		}
		if first.Events[i].Cursor != second.Events[i].Cursor || first.Events[i].Cursor <= baseline {
			t.Fatalf("event %d cursors = %d/%d after %d", i, first.Events[i].Cursor, second.Events[i].Cursor, baseline)
		}
	}
}

func TestRuntimeSlowConsumerLosesNoEventsAndDelaysNobody(t *testing.T) {
	r := subscriptionRuntime(t)
	baseline := r.PollEvents(seam.EventQuery{}).Cursor
	const count = 4096
	for i := 0; i < count; i++ {
		r.emit(seam.Event{Kind: "test", Text: fmt.Sprintf("event-%04d", i)})
	}

	active := r.PollEvents(seam.EventQuery{After: baseline})
	if len(active.Events) != count {
		t.Fatalf("active consumer received %d/%d events", len(active.Events), count)
	}
	stalled := r.PollEvents(seam.EventQuery{After: baseline})
	if len(stalled.Events) != count {
		t.Fatalf("stalled consumer later received %d/%d events", len(stalled.Events), count)
	}
}

func TestRuntimePollWakesAndCloseEndsAfterFinalEvent(t *testing.T) {
	r := subscriptionRuntime(t)
	baseline := r.PollEvents(seam.EventQuery{}).Cursor
	woke := make(chan seam.EventBatch, 1)
	go func() {
		woke <- r.PollEvents(seam.EventQuery{After: baseline, WaitMilliseconds: 2000})
	}()
	r.emit(seam.Event{Kind: "test", Text: "wake"})
	select {
	case batch := <-woke:
		if len(batch.Events) != 1 || batch.Events[0].Text != "wake" || batch.End {
			t.Fatalf("woken batch = %#v", batch)
		}
		baseline = batch.Cursor
	case <-time.After(2 * time.Second):
		t.Fatal("event poll did not wake")
	}

	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	final := r.PollEvents(seam.EventQuery{After: baseline})
	if len(final.Events) != 1 || final.Events[0].Kind != "runtime" || final.Events[0].Text != "runtime stopping" || !final.End {
		t.Fatalf("final batch = %#v", final)
	}
	after := r.PollEvents(seam.EventQuery{After: final.Cursor})
	if len(after.Events) != 0 || !after.End {
		t.Fatalf("poll after end = %#v", after)
	}
	r.emit(seam.Event{Kind: "test", Text: "after close"})
	if got := r.PollEvents(seam.EventQuery{After: final.Cursor}); len(got.Events) != 0 || !got.End {
		t.Fatalf("emit after close changed stream: %#v", got)
	}
}

func TestRuntimeEventBatchesAreSerializableIndependentCopies(t *testing.T) {
	r := subscriptionRuntime(t)
	baseline := r.PollEvents(seam.EventQuery{}).Cursor
	metadata := map[string]any{"nested": map[string]any{"value": "original"}}
	r.emit(seam.Event{Kind: "test", Metadata: metadata})
	metadata["nested"].(map[string]any)["value"] = "producer mutation"

	first := r.PollEvents(seam.EventQuery{After: baseline})
	if _, err := json.Marshal(first); err != nil {
		t.Fatalf("marshal event batch: %v", err)
	}
	first.Events[0].Metadata["nested"].(map[string]any)["value"] = "consumer mutation"
	second := r.PollEvents(seam.EventQuery{After: baseline})
	if got := second.Events[0].Metadata["nested"].(map[string]any)["value"]; got != "original" {
		t.Fatalf("event metadata was not copied: %#v", got)
	}

	live := &struct {
		Value string `json:"value"`
	}{Value: "before"}
	r.emit(seam.Event{Kind: "test", Metadata: map[string]any{"pointer": live}})
	live.Value = "after"
	pointer := r.PollEvents(seam.EventQuery{After: second.Cursor})
	if got := pointer.Events[0].Metadata["pointer"].(map[string]any)["value"]; got != "before" {
		t.Fatalf("live pointer crossed seam: %#v", got)
	}

	for name, value := range map[string]any{
		"channel":  make(chan struct{}),
		"function": func() {},
	} {
		r.emit(seam.Event{Kind: "test", Metadata: map[string]any{"bad": value}})
		bad := r.PollEvents(seam.EventQuery{After: pointer.Cursor})
		pointer.Cursor = bad.Cursor
		if _, err := json.Marshal(bad); err != nil {
			t.Fatalf("%s producer value crossed seam: %v", name, err)
		}
		if bad.Events[0].Metadata["serialization_error"] == nil {
			t.Fatalf("%s metadata was not replaced: %#v", name, bad.Events[0])
		}
	}
}
