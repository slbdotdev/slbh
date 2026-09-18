package harness

import (
	"fmt"
	"testing"
	"time"

	"github.com/slbdotdev/slbh/internal/config"
	"github.com/slbdotdev/slbh/internal/provider"
	"github.com/slbdotdev/slbh/internal/seam"
)

func subscriptionRuntime(t *testing.T, primaryBuffer int) *Runtime {
	t.Helper()
	r, err := New(
		config.Config{Home: t.TempDir(), SeatModel: "test", SeatEffort: "high"},
		Options{
			Events:   make(chan seam.Event, primaryBuffer),
			Provider: func(string) (provider.Provider, error) { return fakeProvider{}, nil },
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

func TestRuntimeSubscribersSeeSameOrderedEvents(t *testing.T) {
	r := subscriptionRuntime(t, 64)
	first, unsubscribeFirst := r.Subscribe()
	defer unsubscribeFirst()
	second, unsubscribeSecond := r.Subscribe()
	defer unsubscribeSecond()

	const count = 32
	for i := 0; i < count; i++ {
		r.emit(seam.Event{Kind: "test", Text: fmt.Sprintf("event-%02d", i)})
	}
	read := func(events <-chan seam.Event) []string {
		t.Helper()
		got := make([]string, 0, count)
		deadline := time.After(2 * time.Second)
		for len(got) < count {
			select {
			case event := <-events:
				got = append(got, event.Text)
			case <-deadline:
				t.Fatalf("received %d/%d subscribed events", len(got), count)
			}
		}
		return got
	}

	firstEvents := read(first)
	secondEvents := read(second)
	for i := range firstEvents {
		want := fmt.Sprintf("event-%02d", i)
		if firstEvents[i] != want || secondEvents[i] != want {
			t.Fatalf("event %d = %q/%q, want %q", i, firstEvents[i], secondEvents[i], want)
		}
	}
}

func TestRuntimeStalledSubscriberDropsWithoutDelayingAnother(t *testing.T) {
	const extra = 100
	count := subscriberEventBuffer + extra
	r := subscriptionRuntime(t, count+16)
	stalled, unsubscribeStalled := r.Subscribe()
	defer unsubscribeStalled()
	active, unsubscribeActive := r.Subscribe()
	defer unsubscribeActive()

	received := make(chan []string, 1)
	go func() {
		got := make([]string, 0, count)
		for len(got) < count {
			got = append(got, (<-active).Text)
		}
		received <- got
	}()
	for i := 0; i < count; i++ {
		r.emit(seam.Event{Kind: "test", Text: fmt.Sprintf("event-%04d", i)})
	}

	select {
	case got := <-received:
		for i, text := range got {
			if want := fmt.Sprintf("event-%04d", i); text != want {
				t.Fatalf("active subscriber event %d = %q, want %q", i, text, want)
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stalled subscriber delayed active subscriber")
	}

	select {
	case marker := <-stalled:
		if marker.Kind != "dropped" {
			t.Fatalf("first stalled event = %#v, want dropped marker", marker)
		}
		wantDropped := count - (subscriberEventBuffer - 1)
		if got := marker.Metadata["count"]; got != wantDropped {
			t.Fatalf("dropped count = %#v, want %d", got, wantDropped)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stalled subscriber did not receive a dropped marker")
	}
}

func TestRuntimeUnsubscribeAndCloseCloseSubscriptionsOnce(t *testing.T) {
	r := subscriptionRuntime(t, 16)
	unsubscribed, unsubscribe := r.Subscribe()
	unsubscribe()
	unsubscribe()
	if _, ok := <-unsubscribed; ok {
		t.Fatal("unsubscribe did not close subscription")
	}

	closed, unsubscribeClosed := r.Subscribe()
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	unsubscribeClosed()
	for range closed {
	}
	if _, ok := <-closed; ok {
		t.Fatal("Close did not close subscription")
	}

	afterClose, unsubscribeAfterClose := r.Subscribe()
	defer unsubscribeAfterClose()
	if _, ok := <-afterClose; ok {
		t.Fatal("Subscribe after Close returned an open channel")
	}
	r.emit(seam.Event{Kind: "test", Text: "after close"})
}
