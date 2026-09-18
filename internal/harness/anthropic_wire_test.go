package harness

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/slbdotdev/slbh/internal/config"
	"github.com/slbdotdev/slbh/internal/provider"
)

// anthropicCachedStream is a Messages stream on a warm prefix: 46 input tokens
// net of cache beside 5,504 read from it, which is the measured shape of the
// trap. The gross context is 5,550.
const anthropicCachedStream = `event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":46,"cache_read_input_tokens":5504}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"done"}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":83}}

event: message_stop
data: {"type":"message_stop"}
`

// TestAnthropicWireUsageReachesTheHarness is the offline half of the plan's
// acceptance criterion 5. The provider package's own tests prove the usage
// object is normalized; this proves the normalized object actually reaches
// recordUsage and lands on the agent, which is the thing compaction reads.
//
// It runs a real agent turn against a real HTTP server over the real wire
// strategy — only the endpoint is a fake — so nothing between the SSE bytes
// and the agent's counters is stubbed out.
func TestAnthropicWireUsageReachesTheHarness(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Assert the wire on the way past: a bearer token here would mean the
		// request went out on the wrong strategy.
		if r.Header.Get("x-api-key") == "" {
			t.Errorf("request reached the Anthropic endpoint without x-api-key: %v", r.Header)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, anthropicCachedStream)
	}))
	defer server.Close()

	runtime, err := New(config.Config{
		Home:      t.TempDir(),
		SeatModel: "zai/glm-5.3-flash",
	}, Options{Provider: func(string) (provider.Provider, error) {
		return provider.NewHTTPOnWire(server.URL+"/v1/messages", "test-key-not-a-credential", provider.WireAnthropicMessages)
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	seat := runtime.seat()
	if err := seat.Send("hello"); err != nil {
		t.Fatal(err)
	}
	waitAgentTurn(t, runtime, seat.ID)

	snapshot := seat.Snapshot()
	// The gross figure, not the net one. Had input_tokens been mapped straight
	// across, this would read 46 and automatic compaction would never fire on
	// a cached conversation while everything still appeared to run.
	if snapshot.ContextUsed != 46+5504 {
		t.Fatalf("contextUsed = %d, want %d (gross prompt tokens)", snapshot.ContextUsed, 46+5504)
	}
	if snapshot.CacheHitTokens != 5504 {
		t.Fatalf("cache hits = %d, want 5504", snapshot.CacheHitTokens)
	}
	// Misses are derived, because this wire has no cache_creation field at all.
	if snapshot.CacheMissTokens != 46 {
		t.Fatalf("cache misses = %d, want 46 derived from gross minus hits", snapshot.CacheMissTokens)
	}
}
