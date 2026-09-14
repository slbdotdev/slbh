//go:build live_integration

package harness

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/slbdotdev/slbh/internal/config"
	"github.com/slbdotdev/slbh/internal/provider"
)

const liveTranscriptPrompt = "This is a live slbh transcript replay check. Reply with exactly SLBH_LIVE_REPLAY_OK and nothing else. Do not call tools."

type capturedTransport struct {
	base http.RoundTripper
	mu   sync.Mutex
	post [][]byte
}

func (t *capturedTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.Method == http.MethodPost && request.Body != nil {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		_ = request.Body.Close()
		request.Body = io.NopCloser(bytes.NewReader(body))
		t.mu.Lock()
		t.post = append(t.post, append([]byte(nil), body...))
		t.mu.Unlock()
	}
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(request)
}

func (t *capturedTransport) posts() [][]byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	posts := make([][]byte, len(t.post))
	for i := range t.post {
		posts[i] = append([]byte(nil), t.post[i]...)
	}
	return posts
}

type liveProvider struct {
	inner *provider.HTTPProvider
	mu    sync.Mutex
	usage []map[string]any
}

func (p *liveProvider) ContextWindow(ctx context.Context, model string) (int, error) {
	return p.inner.ContextWindow(ctx, model)
}

// PinnedContextWindow forwards the wrapped route's policy. A wrapper that
// swallowed it would silently put the live test back on the 128,000-token
// fallback while the real path used the pin.
func (p *liveProvider) PinnedContextWindow() (int, bool) {
	return p.inner.PinnedContextWindow()
}

func (p *liveProvider) RequestPayload(request provider.Request) ([]byte, error) {
	return p.inner.RequestPayload(request)
}

func (p *liveProvider) Stream(ctx context.Context, request provider.Request, sink provider.StreamSink) error {
	return p.inner.Stream(ctx, request, func(event provider.Event) error {
		if event.Kind == provider.EventUsage {
			p.mu.Lock()
			p.usage = append(p.usage, cloneMap(event.Usage))
			p.mu.Unlock()
		}
		return sink(event)
	})
}

func (p *liveProvider) usages() []map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	result := make([]map[string]any, len(p.usage))
	for i := range p.usage {
		result[i] = cloneMap(p.usage[i])
	}
	return result
}

type transcriptEntry struct {
	AgentID  string          `json:"agent"`
	Kind     string          `json:"kind"`
	Text     string          `json:"text"`
	Metadata json.RawMessage `json:"metadata"`
}

func TestLiveTranscriptReplayAndCache(t *testing.T) {
	if os.Getenv("SLBH_RUN_LIVE_TESTS") != "1" {
		t.Skip("set SLBH_RUN_LIVE_TESTS=1 to run the billed live integration test")
	}

	cfg := config.Load()
	if model := os.Getenv("SLBH_LIVE_TEST_MODEL"); model != "" {
		cfg.SeatModel = model
	}
	if effort := os.Getenv("SLBH_LIVE_TEST_EFFORT"); effort != "" {
		cfg.SeatEffort = effort
	}
	inner, err := provider.ForModel(cfg.SeatModel, cfg.Endpoint, cfg.EndpointExplicit)
	if err != nil {
		t.Fatal(err)
	}
	capture := &capturedTransport{base: http.DefaultTransport}
	inner.Client = &http.Client{Transport: capture}
	live := &liveProvider{inner: inner}
	runtime, err := New(cfg, Options{Provider: func(string) (provider.Provider, error) { return live, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	seat := runtime.Seat()
	transcriptPath, err := runtime.TranscriptPath(seat.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("live runtime transcript: %s", transcriptPath)
	seat.Send(liveTranscriptPrompt)
	waitLiveTurn(t, runtime, 1)

	entries, err := readLiveTranscript(transcriptPath)
	if err != nil {
		t.Fatal(err)
	}
	requestPayloads, err := liveRequestPayloads(entries, seat.ID)
	if err != nil {
		t.Fatal(err)
	}
	posts := capture.posts()
	if len(requestPayloads) != 1 || len(posts) != 1 {
		t.Fatalf("first chat made %d transcript requests and %d HTTP POSTs; the replay fixture must not call tools", len(requestPayloads), len(posts))
	}
	if !bytes.Equal(requestPayloads[0], posts[0]) {
		t.Fatalf("transcript request payload is not byte-identical to the HTTP inference payload")
	}

	firstRequest, err := provider.RequestFromPayload(requestPayloads[0])
	if err != nil {
		t.Fatal(err)
	}
	answer := transcriptAssistantText(entries, seat.ID)
	if answer == "" {
		t.Fatal("first transcript has no assistant response to reload")
	}
	loadedHistory := append([]provider.Message(nil), firstRequest.Messages...)
	loadedHistory = append(loadedHistory, provider.Message{Role: "assistant", Content: answer})

	// Drop the live in-memory history, then rebuild it solely from the durable
	// request frame and streamed assistant events in the agent session transcript.
	seat.mu.Lock()
	seat.history = nil
	seat.mu.Unlock()
	seat.mu.Lock()
	seat.history = loadedHistory
	seat.mu.Unlock()
	if got := seat.History(); len(got) != len(loadedHistory) {
		t.Fatalf("reloaded history length = %d, want %d", len(got), len(loadedHistory))
	}

	seat.Send(liveTranscriptPrompt)
	waitLiveTurn(t, runtime, 1)
	entries, err = readLiveTranscript(transcriptPath)
	if err != nil {
		t.Fatal(err)
	}
	requestPayloads, err = liveRequestPayloads(entries, seat.ID)
	if err != nil {
		t.Fatal(err)
	}
	posts = capture.posts()
	if len(requestPayloads) != 2 || len(posts) != 2 {
		t.Fatalf("replayed chat made %d transcript requests and %d HTTP POSTs; the fixture must not call tools", len(requestPayloads), len(posts))
	}
	if !bytes.Equal(requestPayloads[1], posts[1]) {
		t.Fatalf("replayed transcript request payload is not byte-identical to the HTTP inference payload")
	}

	secondRequest, err := provider.RequestFromPayload(requestPayloads[1])
	if err != nil {
		t.Fatal(err)
	}
	if firstRequest.CacheKey == "" || firstRequest.CacheKey != secondRequest.CacheKey {
		t.Fatalf("cache key changed across transcript reload: first=%q second=%q", firstRequest.CacheKey, secondRequest.CacheKey)
	}
	if firstRequest.System != secondRequest.System {
		t.Fatal("system context changed across transcript reload")
	}
	if len(secondRequest.Messages) != len(firstRequest.Messages)+2 {
		t.Fatalf("replayed message count = %d, want %d", len(secondRequest.Messages), len(firstRequest.Messages)+2)
	}
	if len(live.usages()) < 2 {
		t.Fatalf("provider returned fewer than two usage records: %v", live.usages())
	}
	if cached, ok := cachedTokens(live.usages()[1]); !ok || cached <= 0 {
		t.Fatalf("second inference did not report a cache hit: %v", live.usages()[1])
	}
	t.Logf("second inference reported cached input tokens: %d", mustCachedTokens(t, live.usages()[1]))
}

func TestLiveNativeSeatCodexLeafContinuation(t *testing.T) {
	if os.Getenv("SLBH_RUN_NATIVE_CODEX_TESTS") != "1" {
		t.Skip("set SLBH_RUN_NATIVE_CODEX_TESTS=1 to run the billed native-seat/Codex-leaf acceptance test")
	}
	cfg := config.Load()
	if model := os.Getenv("SLBH_LIVE_TEST_MODEL"); model != "" {
		cfg.SeatModel = model
	}
	if cfg.SeatModel == "" {
		cfg.SeatModel = "deepseek-v4-flash"
	}
	if effort := os.Getenv("SLBH_LIVE_TEST_EFFORT"); effort != "" {
		cfg.SeatEffort = effort
	}
	if cfg.SeatEffort == "" {
		cfg.SeatEffort = "xhigh"
	}
	// This test deliberately selects the seat model for a temporary runtime;
	// it must not depend on the user's persisted approval list.
	cfg.ApprovedModels = []string{cfg.SeatModel}
	codexModel := os.Getenv("SLBH_CODEX_TEST_MODEL")
	if codexModel == "" {
		codexModel = "gpt-5.6-luna"
	}
	runtime, err := New(cfg, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	seat := runtime.Seat()
	prompt := fmt.Sprintf("Launch one Codex leaf using harness codex and model %s. Give it this brief: reply with exactly NATIVE_CODEX_LEAF_OK and nothing else. After the leaf reports back, reply with exactly NATIVE_CODEX_LEAF_OK and nothing else.", codexModel)
	if err := seat.Send(prompt); err != nil {
		t.Fatal(err)
	}
	deadline := time.NewTimer(8 * time.Minute)
	defer deadline.Stop()
	for {
		select {
		case event := <-runtime.Events():
			if event.Kind == "error" {
				t.Fatalf("native seat/Codex leaf inference failed: %s", event.Text)
			}
			if event.AgentID == seat.ID && event.Kind == "turn_done" {
				for _, message := range seat.History() {
					if strings.Contains(message.Content, "NATIVE_CODEX_LEAF_OK") {
						return
					}
				}
			}
		case <-deadline.C:
			t.Fatal("native seat/Codex leaf continuation did not finish")
		}
	}
}

func waitLiveTurn(t *testing.T, runtime *Runtime, count int) {
	t.Helper()
	deadline := time.NewTimer(8 * time.Minute)
	defer deadline.Stop()
	for count > 0 {
		select {
		case event := <-runtime.Events():
			if event.Kind == "error" {
				t.Fatalf("live inference failed: %s", event.Text)
			}
			if event.Kind == "turn_done" {
				count--
			}
		case <-deadline.C:
			t.Fatal("live inference did not finish before timeout")
		}
	}
}

func readLiveTranscript(path string) ([]transcriptEntry, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	var entries []transcriptEntry
	for scanner.Scan() {
		var entry transcriptEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			return nil, fmt.Errorf("decode transcript: %w", err)
		}
		entries = append(entries, entry)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return entries, nil
}

func liveRequestPayloads(entries []transcriptEntry, agentID string) ([][]byte, error) {
	var result [][]byte
	for _, entry := range entries {
		if entry.AgentID != agentID || entry.Kind != "inference_request" {
			continue
		}
		var metadata struct {
			Payload       json.RawMessage `json:"payload"`
			PayloadSHA256 string          `json:"payload_sha256"`
		}
		if err := json.Unmarshal(entry.Metadata, &metadata); err != nil {
			return nil, fmt.Errorf("decode inference metadata: %w", err)
		}
		if len(metadata.Payload) == 0 {
			return nil, fmt.Errorf("inference request has no wire payload")
		}
		if got := provider.PayloadSHA256(metadata.Payload); got != metadata.PayloadSHA256 {
			return nil, fmt.Errorf("inference payload hash mismatch: got %s, recorded %s", got, metadata.PayloadSHA256)
		}
		result = append(result, append([]byte(nil), metadata.Payload...))
	}
	return result, nil
}

func transcriptAssistantText(entries []transcriptEntry, agentID string) string {
	var answer strings.Builder
	for _, entry := range entries {
		if entry.AgentID == agentID && entry.Kind == "assistant" {
			answer.WriteString(entry.Text)
		}
	}
	return answer.String()
}

func cachedTokens(value any) (int, bool) {
	switch current := value.(type) {
	case map[string]any:
		for key, nested := range current {
			name := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(key, "-", "_"), " ", "_"))
			if strings.Contains(name, "cache") && (strings.Contains(name, "hit") || strings.Contains(name, "cached")) {
				if tokens, ok := numberValue(nested); ok {
					return tokens, true
				}
			}
			if tokens, ok := cachedTokens(nested); ok {
				return tokens, true
			}
		}
	case []any:
		for _, nested := range current {
			if tokens, ok := cachedTokens(nested); ok {
				return tokens, true
			}
		}
	}
	return 0, false
}

func numberValue(value any) (int, bool) {
	switch number := value.(type) {
	case float64:
		return int(number), true
	case int:
		return number, true
	case json.Number:
		parsed, err := number.Int64()
		return int(parsed), err == nil
	default:
		return 0, false
	}
}

func mustCachedTokens(t *testing.T, usage map[string]any) int {
	t.Helper()
	value, ok := cachedTokens(usage)
	if !ok {
		t.Fatalf("usage has no cache token field: %v", usage)
	}
	return value
}

func cloneMap(input map[string]any) map[string]any {
	if input == nil {
		return nil
	}
	output := make(map[string]any, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}
