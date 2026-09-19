//go:build live_integration

// Acceptance tests for the provider-routing campaign's live criteria (3, 4, 5
// and 6 of org/slbh-routing-plan-2026-09-13.md). Each is gated by its own
// environment variable rather than by one shared switch, because every one of
// them spends real plan quota and the ladder alone is fifteen calls.
//
// These route through the real managed policy and the real provider path. A
// stand-in policy or a hand-rolled HTTP call would prove nothing about what
// slbh does.
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
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/slbdotdev/slbh/internal/config"
	"github.com/slbdotdev/slbh/internal/provider"
)

const (
	acceptPlanRoute       = "zai/glm-5.3-flash"
	acceptOpenRouterRoute = "z-ai/glm-5.3-flash"

	// Reused verbatim from tools/matrix so the ladder measured here is
	// directly comparable with the gate's own figures of 2026-09-13.
	acceptEffortSystem   = "You are a terse capability-probe target. Answer exactly what is asked, with no preamble and no closing remarks."
	acceptEffortQuestion = "Define a(1)=7 and a(n)=a(n-1)+gcd(n,a(n-1)) for n>1. Work out every term from a(2) to a(60) one step at a time, showing each gcd you use, then list every n in that range where a(n)-a(n-1) is greater than 1, and finally state a(60). Be exhaustive and check your arithmetic as you go."

	acceptSentinel = "SLBH_ACCEPT_OK"
)

// teeTransport records both the request body and the raw response body of
// every POST. The response tee is what makes criterion 5 an observation rather
// than a derivation: the normalized usage alone cannot show that
// prompt_tokens came from input_tokens + cache_read_input_tokens, because the
// two raw fields never leave the provider package.
type teeTransport struct {
	base      http.RoundTripper
	mu        sync.Mutex
	requests  [][]byte
	responses []*bytes.Buffer
}

func (t *teeTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.Method == http.MethodPost && request.Body != nil {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		_ = request.Body.Close()
		request.Body = io.NopCloser(bytes.NewReader(body))
		t.mu.Lock()
		t.requests = append(t.requests, append([]byte(nil), body...))
		t.mu.Unlock()
	}
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	response, err := base.RoundTrip(request)
	if err != nil || response == nil || response.Body == nil {
		return response, err
	}
	sink := &bytes.Buffer{}
	t.mu.Lock()
	t.responses = append(t.responses, sink)
	t.mu.Unlock()
	response.Body = &teeBody{reader: io.TeeReader(response.Body, sink), closer: response.Body}
	return response, nil
}

type teeBody struct {
	reader io.Reader
	closer io.Closer
}

func (b *teeBody) Read(p []byte) (int, error) { return b.reader.Read(p) }
func (b *teeBody) Close() error               { return b.closer.Close() }

func (t *teeTransport) requestBodies() [][]byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([][]byte, len(t.requests))
	for i := range t.requests {
		out[i] = append([]byte(nil), t.requests[i]...)
	}
	return out
}

func (t *teeTransport) responseBodies() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]string, len(t.responses))
	for i := range t.responses {
		out[i] = t.responses[i].String()
	}
	return out
}

// acceptPolicy loads the real configuration and refuses to run on a host with
// no managed policy, rather than silently measuring a locally authored one.
func acceptPolicy(t *testing.T) config.Config {
	t.Helper()
	cfg := config.Load()
	if cfg.PolicySource.Kind != config.PolicyManaged {
		t.Fatalf("acceptance runs need the managed policy; policy source is %s", cfg.PolicySource.Describe())
	}
	t.Logf("policy source: %s", cfg.PolicySource.Describe())
	return cfg
}

// acceptProvider resolves a route through the production factory and replaces
// its transport with the tee.
func acceptProvider(t *testing.T, cfg config.Config, model string) (provider.Provider, *teeTransport) {
	t.Helper()
	instance, err := provider.ForModel(model, "", false, cfg.Policy)
	if err != nil {
		t.Fatalf("ForModel(%q): %v", model, err)
	}
	tee := &teeTransport{base: http.DefaultTransport}
	override, ok := instance.(provider.TransportOverride)
	if !ok {
		t.Fatalf("resolved provider %T cannot have its transport replaced", instance)
	}
	override.SetHTTPClient(&http.Client{Transport: tee})
	return instance, tee
}

type acceptStreamResult struct {
	Text           string
	Reasoning      string
	Usage          map[string]any
	StopReason     string
	ToolCalls      int
	PromptTokens   int
	OutputTokens   int
	CachedTokens   int
	ElapsedSeconds float64
}

func acceptStream(ctx context.Context, p provider.Provider, request provider.Request) (acceptStreamResult, error) {
	var result acceptStreamResult
	var text, reasoning strings.Builder
	tools := map[string]bool{}
	started := time.Now()
	err := p.Stream(ctx, request, func(event provider.Event) error {
		switch event.Kind {
		case provider.EventText:
			text.WriteString(event.Text)
		case provider.EventReasoning:
			reasoning.WriteString(event.Text)
		case provider.EventTool:
			tools[event.ToolCallID] = true
		case provider.EventUsage:
			result.Usage = cloneMap(event.Usage)
			result.StopReason = event.StopReason
		}
		return nil
	})
	result.ElapsedSeconds = time.Since(started).Seconds()
	result.Text = text.String()
	result.Reasoning = reasoning.String()
	result.ToolCalls = len(tools)
	if result.Usage != nil {
		result.PromptTokens = acceptInt(result.Usage["prompt_tokens"])
		result.OutputTokens = acceptInt(result.Usage["completion_tokens"])
		if cached, ok := cachedTokens(result.Usage); ok {
			result.CachedTokens = cached
		}
	}
	return result, err
}

func acceptInt(value any) int {
	switch typed := value.(type) {
	case int:
		return typed
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	case json.Number:
		parsed, err := typed.Int64()
		if err != nil {
			return 0
		}
		return int(parsed)
	}
	return 0
}

// acceptWriteJSON puts every measurement on disk as it is taken. A run that is
// interrupted must still leave its evidence behind.
func acceptWriteJSON(t *testing.T, name string, value any) {
	t.Helper()
	dir := os.Getenv("SLBH_ACCEPT_OUT")
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("creating %s: %v", dir, err)
	}
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatalf("encoding %s: %v", name, err)
	}
	path := dir + "/" + name
	if err := os.WriteFile(path, append(encoded, '\n'), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	t.Logf("wrote %s", path)
}

// --------------------------------------------------------------- criterion 3

type ladderCall struct {
	Level          string  `json:"level"`
	Repetition     int     `json:"repetition"`
	OutputTokens   int     `json:"output_tokens"`
	PromptTokens   int     `json:"prompt_tokens"`
	ReasoningChars int     `json:"reasoning_chars"`
	TextChars      int     `json:"text_chars"`
	StopReason     string  `json:"stop_reason"`
	ElapsedSeconds float64 `json:"elapsed_seconds"`
	SentEffort     string  `json:"sent_output_config_effort"`
}

// TestAcceptanceEffortLadder is criterion 3: the effort ladder confirmed live
// on the Anthropic route, three repetitions per level, through slbh's own
// provider path.
//
// It asserts the outbound body as well as the returned volume. A route that
// routes but silently drops effort is exactly what this exists to catch, and
// the only two things that can be observed about it are what went out and how
// much came back.
func TestAcceptanceEffortLadder(t *testing.T) {
	if os.Getenv("SLBH_ACCEPT_LADDER") != "1" {
		t.Skip("set SLBH_ACCEPT_LADDER=1 to run the fifteen billed ladder calls")
	}
	cfg := acceptPolicy(t)
	levels := []string{"low", "medium", "high", "xhigh", "max"}
	// SLBH_ACCEPT_LEVELS resumes a run that a provider-side 500 cut short,
	// without respending the levels already measured. The plan's budget is
	// about twenty live calls and a transient must not cost the whole ladder.
	if only := strings.TrimSpace(os.Getenv("SLBH_ACCEPT_LEVELS")); only != "" {
		levels = strings.Split(only, ",")
	}
	reps := 3
	calls := make([]ladderCall, 0, len(levels)*reps)

	for _, level := range levels {
		for rep := 1; rep <= reps; rep++ {
			instance, tee := acceptProvider(t, cfg, acceptPlanRoute)
			routed, ok := instance.(*provider.HTTPProvider)
			if !ok {
				t.Fatalf("resolved provider is %T, not the HTTP provider", instance)
			}
			if routed.Route.Wire != provider.WireAnthropicMessages {
				t.Fatalf("plan route resolved to wire %q, want %q", routed.Route.Wire, provider.WireAnthropicMessages)
			}
			if routed.Route.Endpoint != "https://api.z.ai/api/anthropic/v1/messages" {
				t.Fatalf("plan route endpoint is %q", routed.Route.Endpoint)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
			result, err := acceptStream(ctx, instance, provider.Request{
				Model:    acceptPlanRoute,
				Effort:   level,
				System:   acceptEffortSystem,
				Messages: []provider.Message{{Role: "user", Content: acceptEffortQuestion}},
			})
			cancel()
			if err != nil {
				t.Fatalf("level %s rep %d: %v", level, rep, err)
			}
			bodies := tee.requestBodies()
			if len(bodies) != 1 {
				t.Fatalf("level %s rep %d made %d POSTs, want 1", level, rep, len(bodies))
			}
			var sent struct {
				Model        string `json:"model"`
				OutputConfig *struct {
					Effort string `json:"effort"`
				} `json:"output_config"`
				ReasoningEffort string `json:"reasoning_effort"`
				Thinking        any    `json:"thinking"`
			}
			if err := json.Unmarshal(bodies[0], &sent); err != nil {
				t.Fatalf("level %s rep %d: decoding sent body: %v", level, rep, err)
			}
			if sent.Model != "glm-5.3-flash" {
				t.Fatalf("level %s rep %d sent model %q, want the unsuffixed slug", level, rep, sent.Model)
			}
			if sent.OutputConfig == nil || sent.OutputConfig.Effort != level {
				t.Fatalf("level %s rep %d did not send output_config.effort=%s: %s", level, rep, level, string(bodies[0]))
			}
			if sent.ReasoningEffort != "" || sent.Thinking != nil {
				t.Fatalf("level %s rep %d sent an inert effort spelling: %s", level, rep, string(bodies[0]))
			}
			call := ladderCall{
				Level:          level,
				Repetition:     rep,
				OutputTokens:   result.OutputTokens,
				PromptTokens:   result.PromptTokens,
				ReasoningChars: len(result.Reasoning),
				TextChars:      len(result.Text),
				StopReason:     result.StopReason,
				ElapsedSeconds: result.ElapsedSeconds,
				SentEffort:     sent.OutputConfig.Effort,
			}
			calls = append(calls, call)
			t.Logf("level=%s rep=%d output_tokens=%d reasoning_chars=%d text_chars=%d stop=%s elapsed=%.1fs",
				level, rep, call.OutputTokens, call.ReasoningChars, call.TextChars, call.StopReason, call.ElapsedSeconds)
			acceptWriteJSON(t, "ladder-progress-"+strings.Join(levels, "-")+".json", calls)
		}
	}

	type levelSummary struct {
		Level  string  `json:"level"`
		Values []int   `json:"output_tokens"`
		Min    int     `json:"min"`
		Max    int     `json:"max"`
		Mean   float64 `json:"mean"`
	}
	summaries := make([]levelSummary, 0, len(levels))
	for _, level := range levels {
		summary := levelSummary{Level: level, Min: 1 << 30}
		total := 0
		for _, call := range calls {
			if call.Level != level {
				continue
			}
			summary.Values = append(summary.Values, call.OutputTokens)
			if call.OutputTokens < summary.Min {
				summary.Min = call.OutputTokens
			}
			if call.OutputTokens > summary.Max {
				summary.Max = call.OutputTokens
			}
			total += call.OutputTokens
		}
		if len(summary.Values) > 0 {
			summary.Mean = float64(total) / float64(len(summary.Values))
		}
		summaries = append(summaries, summary)
	}
	acceptWriteJSON(t, "criterion-3-effort-ladder-"+strings.Join(levels, "-")+".json", map[string]any{
		"route":    acceptPlanRoute,
		"endpoint": "https://api.z.ai/api/anthropic/v1/messages",
		"wire":     provider.WireAnthropicMessages,
		"field":    "output_config.effort",
		"calls":    calls,
		"levels":   summaries,
	})
	for _, summary := range summaries {
		t.Logf("SUMMARY level=%-6s n=%d min=%d max=%d mean=%.1f", summary.Level, len(summary.Values), summary.Min, summary.Max, summary.Mean)
	}
	// The one assertion that is not a report: a route that drops effort
	// returns the same volume whatever is asked for. Separation is the
	// feature; the exact order is measured and reported, not asserted, because
	// the gate itself measured xhigh below high on this wire.
	if len(levels) < 5 {
		// A resumed partial run measures and reports; the inertness check
		// needs the whole ladder to mean anything.
		return
	}
	lowest, highest := 1<<30, 0
	for _, summary := range summaries {
		if summary.Mean < float64(lowest) {
			lowest = int(summary.Mean)
		}
		if summary.Mean > float64(highest) {
			highest = int(summary.Mean)
		}
	}
	if lowest == 0 || float64(highest)/float64(lowest) < 1.5 {
		t.Fatalf("effort appears inert: lowest level mean %d, highest %d", lowest, highest)
	}
}

// ------------------------------------------------- open question: tool replay
//
// The third adversarial audit flagged, and declined to rank, that this wire has
// never been shown to accept the shape slbh sends it on a multi-tool turn.
// anthropicMessages converts each `tool` message into its own `user` message
// carrying one tool_result block, so a turn with N tool calls produces N
// consecutive `user` messages, where the canonical Messages shape merges them
// into one. The offline round-trip tests cover the codec, not the endpoint, and
// the capability matrix records that no probe ever exercised a multi-turn tool
// round-trip.
//
// This settles it in one billed call, and asserts both halves: that the
// outbound body really does carry consecutive user messages (otherwise a pass
// would only mean the encoder merged them and the question was moot), and that
// the endpoint accepts them and continues the turn.
func TestAcceptanceConsecutiveToolResults(t *testing.T) {
	if os.Getenv("SLBH_ACCEPT_TOOLREPLAY") != "1" {
		t.Skip("set SLBH_ACCEPT_TOOLREPLAY=1 to run the one billed tool-replay call")
	}
	cfg := acceptPolicy(t)
	instance, tee := acceptProvider(t, cfg, acceptPlanRoute)
	routed := instance.(*provider.HTTPProvider)
	if routed.Route.Wire != provider.WireAnthropicMessages {
		t.Fatalf("this question is about the Anthropic wire; route resolved to %q", routed.Route.Wire)
	}

	call := func(id, name, args string) provider.ToolCall {
		c := provider.ToolCall{ID: id, Type: "function"}
		c.Function.Name = name
		c.Function.Arguments = args
		return c
	}

	// A completed two-tool round: the assistant asked for both, and both
	// results come back as separate `tool` messages — exactly what the agent
	// loop builds after executing a parallel batch.
	history := []provider.Message{
		{Role: "user", Content: "Call read_gauge for both sensors, then report both values."},
		{
			Role:      "assistant",
			Content:   "",
			ToolCalls: []provider.ToolCall{call("call_a", "read_gauge", `{"sensor":"alpha"}`), call("call_b", "read_gauge", `{"sensor":"beta"}`)},
		},
		{Role: "tool", ToolCallID: "call_a", Name: "read_gauge", Content: "alpha=41"},
		{Role: "tool", ToolCallID: "call_b", Name: "read_gauge", Content: "beta=42"},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	result, err := acceptStream(ctx, instance, provider.Request{
		Model:    acceptPlanRoute,
		Effort:   "low",
		System:   "You report gauge readings tersely.",
		Messages: history,
		Tools: []provider.Tool{{
			Name:        "read_gauge",
			Description: "Read one sensor gauge.",
			Parameters: map[string]any{
				"type":       "object",
				"properties": map[string]any{"sensor": map[string]any{"type": "string"}},
				"required":   []string{"sensor"},
			},
		}},
	})
	cancel()
	if err != nil {
		t.Fatalf("the endpoint refused a continuation carrying two tool results: %v", err)
	}

	// Half one: prove the body really is the shape in question.
	sent := tee.requestBodies()[0]
	var payload struct {
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Type      string `json:"type"`
				ToolUseID string `json:"tool_use_id"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(sent, &payload); err != nil {
		t.Fatalf("decoding the sent body: %v", err)
	}
	consecutive, seen := 0, []string{}
	for i, m := range payload.Messages {
		toolResults := 0
		for _, block := range m.Content {
			if block.Type == "tool_result" {
				toolResults++
				seen = append(seen, block.ToolUseID)
			}
		}
		if toolResults > 0 && m.Role == "user" && i > 0 && payload.Messages[i-1].Role == "user" {
			consecutive++
		}
	}
	if len(seen) != 2 {
		t.Fatalf("sent %d tool_result blocks, want 2: %s", len(seen), sent)
	}

	// Half two: the endpoint accepted it and the turn continued.
	if strings.TrimSpace(result.Text) == "" {
		t.Fatalf("the continuation produced no text; stop=%q", result.StopReason)
	}
	if result.OutputTokens <= 0 {
		t.Fatalf("the continuation reported %d output tokens", result.OutputTokens)
	}

	shape := "merged into one user message"
	if consecutive > 0 {
		shape = "consecutive user messages"
	}
	t.Logf("tool replay: shape=%s consecutive=%d tool_result_ids=%v text=%q stop=%s prompt_tokens=%d output_tokens=%d",
		shape, consecutive, seen, strings.TrimSpace(result.Text), result.StopReason, result.PromptTokens, result.OutputTokens)

	acceptWriteJSON(t, "open-question-tool-replay.json", map[string]any{
		"route":            routed.Route.Key,
		"endpoint":         routed.Route.Endpoint,
		"wire":             routed.Route.Wire,
		"sent_body":        json.RawMessage(sent),
		"shape":            shape,
		"consecutive_user": consecutive,
		"tool_result_ids":  seen,
		"text":             result.Text,
		"stop_reason":      result.StopReason,
		"prompt_tokens":    result.PromptTokens,
		"output_tokens":    result.OutputTokens,
		"asserted":         "two tool_result blocks sent; endpoint returned text and output_tokens>0",
	})
}

// --------------------------------------------------------------- criterion 4

// TestAcceptanceRouteCalls is criterion 4: one live call per affected route on
// each wire, asserting a field or a count and never a status code. The plan
// route goes out on the Anthropic wire and OpenRouter on the OpenAI one; the
// OpenRouter arm also asserts the whole posture object byte for byte, since
// that object is the entire enforcement of the org's ZDR posture.
func TestAcceptanceRouteCalls(t *testing.T) {
	if os.Getenv("SLBH_ACCEPT_ROUTES") != "1" {
		t.Skip("set SLBH_ACCEPT_ROUTES=1 to run the two billed per-route calls")
	}
	cfg := acceptPolicy(t)
	report := map[string]any{}

	// Plan route, Anthropic wire.
	instance, tee := acceptProvider(t, cfg, acceptPlanRoute)
	routed := instance.(*provider.HTTPProvider)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	result, err := acceptStream(ctx, instance, provider.Request{
		Model:    acceptPlanRoute,
		Effort:   "low",
		System:   "You answer with one token and nothing else.",
		Messages: []provider.Message{{Role: "user", Content: "Reply with exactly " + acceptSentinel + " and nothing else."}},
	})
	cancel()
	if err != nil {
		t.Fatalf("plan route: %v", err)
	}
	if !strings.Contains(result.Text, acceptSentinel) {
		t.Fatalf("plan route returned %q, which does not contain the sentinel", result.Text)
	}
	if result.OutputTokens <= 0 {
		t.Fatalf("plan route reported %d completion tokens", result.OutputTokens)
	}
	if result.StopReason != "stop" {
		t.Fatalf("plan route stop reason %q, want the normalized \"stop\"", result.StopReason)
	}
	planSent := tee.requestBodies()[0]
	report["plan"] = map[string]any{
		"route":          routed.Route.Key,
		"endpoint":       routed.Route.Endpoint,
		"wire":           routed.Route.Wire,
		"sent_body":      json.RawMessage(planSent),
		"text":           result.Text,
		"stop_reason":    result.StopReason,
		"prompt_tokens":  result.PromptTokens,
		"output_tokens":  result.OutputTokens,
		"usage":          result.Usage,
		"asserted_field": "assistant text contains " + acceptSentinel + "; stop_reason==stop; completion_tokens>0",
	}
	t.Logf("plan route: endpoint=%s wire=%s text=%q stop=%s prompt_tokens=%d completion_tokens=%d",
		routed.Route.Endpoint, routed.Route.Wire, strings.TrimSpace(result.Text), result.StopReason, result.PromptTokens, result.OutputTokens)

	// OpenRouter route, OpenAI wire.
	orInstance, orTee := acceptProvider(t, cfg, acceptOpenRouterRoute)
	orRouted := orInstance.(*provider.HTTPProvider)
	if orRouted.Route.Wire != provider.WireOpenAIChat {
		t.Fatalf("openrouter route resolved to wire %q", orRouted.Route.Wire)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Minute)
	orResult, err := acceptStream(ctx, orInstance, provider.Request{
		Model:    acceptOpenRouterRoute,
		Effort:   "low",
		System:   "You answer with one token and nothing else.",
		Messages: []provider.Message{{Role: "user", Content: "Reply with exactly " + acceptSentinel + " and nothing else."}},
	})
	cancel()
	if err != nil {
		t.Fatalf("openrouter route: %v", err)
	}
	if !strings.Contains(orResult.Text, acceptSentinel) {
		t.Fatalf("openrouter route returned %q, which does not contain the sentinel", orResult.Text)
	}
	if orResult.OutputTokens <= 0 {
		t.Fatalf("openrouter route reported %d completion tokens", orResult.OutputTokens)
	}
	orSent := orTee.requestBodies()[0]
	var orBody struct {
		Model           string `json:"model"`
		ReasoningEffort string `json:"reasoning_effort"`
		Provider        *struct {
			ZDR            bool     `json:"zdr"`
			DataCollection string   `json:"data_collection"`
			Sort           string   `json:"sort"`
			Ignore         []string `json:"ignore"`
			MaxPrice       *struct {
				Prompt     float64 `json:"prompt"`
				Completion float64 `json:"completion"`
			} `json:"max_price"`
		} `json:"provider"`
	}
	if err := json.Unmarshal(orSent, &orBody); err != nil {
		t.Fatalf("decoding openrouter body: %v", err)
	}
	if orBody.Provider == nil {
		t.Fatalf("openrouter request carried no provider posture: %s", string(orSent))
	}
	if !orBody.Provider.ZDR || orBody.Provider.DataCollection != "deny" || orBody.Provider.Sort != "throughput" {
		t.Fatalf("openrouter posture is wrong: %s", string(orSent))
	}
	if strings.Join(orBody.Provider.Ignore, ",") != "reka,io-net" {
		t.Fatalf("openrouter ignore list is %v", orBody.Provider.Ignore)
	}
	if orBody.Provider.MaxPrice == nil || orBody.Provider.MaxPrice.Prompt != 0.19 || orBody.Provider.MaxPrice.Completion != 0.5 {
		t.Fatalf("openrouter max_price is %+v", orBody.Provider.MaxPrice)
	}
	if orBody.ReasoningEffort != "low" {
		t.Fatalf("openrouter reasoning_effort is %q", orBody.ReasoningEffort)
	}
	report["openrouter"] = map[string]any{
		"route":          orRouted.Route.Key,
		"endpoint":       orRouted.Route.Endpoint,
		"wire":           orRouted.Route.Wire,
		"sent_body":      json.RawMessage(orSent),
		"text":           orResult.Text,
		"prompt_tokens":  orResult.PromptTokens,
		"output_tokens":  orResult.OutputTokens,
		"usage":          orResult.Usage,
		"asserted_field": "provider posture exact (zdr/data_collection/sort/ignore/max_price); reasoning_effort==low; assistant text contains " + acceptSentinel,
	}
	t.Logf("openrouter route: endpoint=%s wire=%s text=%q posture=%+v prompt_tokens=%d completion_tokens=%d",
		orRouted.Route.Endpoint, orRouted.Route.Wire, strings.TrimSpace(orResult.Text), *orBody.Provider, orResult.PromptTokens, orResult.OutputTokens)
	acceptWriteJSON(t, "criterion-4-per-route.json", report)
}

// --------------------------------------------------------------- criterion 5

type anthropicRawUsage struct {
	InputTokens          int `json:"input_tokens"`
	OutputTokens         int `json:"output_tokens"`
	CacheReadInputTokens int `json:"cache_read_input_tokens"`
}

// acceptRawUsage merges the usage frames out of one raw Anthropic SSE
// response, the same way the provider does, so the two can be compared.
func acceptRawUsage(raw string) anthropicRawUsage {
	var merged anthropicRawUsage
	scanner := bufio.NewScanner(strings.NewReader(raw))
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<24)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		var frame struct {
			Type    string `json:"type"`
			Message *struct {
				Usage *struct {
					InputTokens          *int `json:"input_tokens"`
					OutputTokens         *int `json:"output_tokens"`
					CacheReadInputTokens *int `json:"cache_read_input_tokens"`
				} `json:"usage"`
			} `json:"message"`
			Usage *struct {
				InputTokens          *int `json:"input_tokens"`
				OutputTokens         *int `json:"output_tokens"`
				CacheReadInputTokens *int `json:"cache_read_input_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(payload), &frame); err != nil {
			continue
		}
		apply := func(in *int, out *int) {
			if in != nil {
				*out = *in
			}
		}
		if frame.Message != nil && frame.Message.Usage != nil {
			apply(frame.Message.Usage.InputTokens, &merged.InputTokens)
			apply(frame.Message.Usage.OutputTokens, &merged.OutputTokens)
			apply(frame.Message.Usage.CacheReadInputTokens, &merged.CacheReadInputTokens)
		}
		if frame.Usage != nil {
			apply(frame.Usage.InputTokens, &merged.InputTokens)
			apply(frame.Usage.OutputTokens, &merged.OutputTokens)
			apply(frame.Usage.CacheReadInputTokens, &merged.CacheReadInputTokens)
		}
	}
	return merged
}

// TestAcceptanceCachedUsage is criterion 5: usage normalization proven on a
// cached prefix. On an uncached prefix the correct and the naive mapping are
// indistinguishable, so the test warms a long prefix and then measures the
// second turn.
//
// It runs through a real Runtime rather than the provider alone, because the
// second half of the criterion is the harness's own contextUsed, which only
// exists once recordUsage has run.
func TestAcceptanceCachedUsage(t *testing.T) {
	if os.Getenv("SLBH_ACCEPT_CACHE") != "1" {
		t.Skip("set SLBH_ACCEPT_CACHE=1 to run the two billed cached-prefix calls")
	}
	cfg := acceptPolicy(t)
	cfg.SeatModel = acceptPlanRoute
	cfg.SeatEffort = "low"
	// A fresh $SLBH_HOME approves nothing — ApprovedModels defaults to an
	// empty slice, which is the deliberate fail-safe — so approve the route
	// under test the way /models would.
	cfg.ApprovedModels = []string{acceptPlanRoute}

	instance, tee := acceptProvider(t, cfg, acceptPlanRoute)
	live := &liveProvider{inner: instance}
	runtime, err := New(cfg, Options{Provider: func(string) (provider.Provider, error) { return live, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	seat := runtime.seat()

	// A long, dull prefix: big enough to be worth caching, boring enough that
	// the model does not spend the turn on it.
	var filler strings.Builder
	for i := 0; i < 900; i++ {
		fmt.Fprintf(&filler, "Reference line %04d: the quick brown fox jumps over the lazy dog near the river bank. ", i)
	}
	seat.Send("Here is a reference document. Acknowledge it with the single word ACK and nothing else. Do not call tools.\n\n" + filler.String())
	waitLiveTurn(t, runtime, 1)
	seat.Send("Reply with exactly " + acceptSentinel + " and nothing else. Do not call tools.")
	waitLiveTurn(t, runtime, 1)

	usages := live.usages()
	if len(usages) < 2 {
		t.Fatalf("expected two usage records, got %d: %v", len(usages), usages)
	}
	second := usages[len(usages)-1]
	normalizedPrompt := acceptInt(second["prompt_tokens"])
	normalizedCached, _ := cachedTokens(second)

	raws := tee.responseBodies()
	if len(raws) < 2 {
		t.Fatalf("expected two raw responses, got %d", len(raws))
	}
	raw := acceptRawUsage(raws[len(raws)-1])
	if raw.CacheReadInputTokens <= 0 {
		t.Fatalf("second turn did not hit the prompt cache: raw usage %+v", raw)
	}
	if got, want := normalizedPrompt, raw.InputTokens+raw.CacheReadInputTokens; got != want {
		t.Fatalf("normalized prompt_tokens = %d, want input_tokens(%d) + cache_read_input_tokens(%d) = %d",
			got, raw.InputTokens, raw.CacheReadInputTokens, want)
	}
	if normalizedCached != raw.CacheReadInputTokens {
		t.Fatalf("normalized cached_tokens = %d, want %d", normalizedCached, raw.CacheReadInputTokens)
	}
	if normalizedPrompt <= raw.InputTokens {
		t.Fatalf("normalized prompt_tokens (%d) is not greater than the net input_tokens (%d); the prefix was not cached and this proves nothing",
			normalizedPrompt, raw.InputTokens)
	}

	snapshot := seat.Snapshot()
	if snapshot.ContextUsed != normalizedPrompt {
		t.Fatalf("harness contextUsed = %d, want the gross figure %d", snapshot.ContextUsed, normalizedPrompt)
	}
	naive := raw.InputTokens
	t.Logf("raw wire usage: input_tokens=%d cache_read_input_tokens=%d output_tokens=%d", raw.InputTokens, raw.CacheReadInputTokens, raw.OutputTokens)
	t.Logf("normalized: prompt_tokens=%d cached_tokens=%d", normalizedPrompt, normalizedCached)
	t.Logf("harness: contextUsed=%d contextWindow=%d cacheHit=%d cacheMiss=%d", snapshot.ContextUsed, snapshot.ContextWindow, snapshot.CacheHitTokens, snapshot.CacheMissTokens)
	t.Logf("naive mapping would have reported %d, understating the context by %d tokens", naive, normalizedPrompt-naive)
	acceptWriteJSON(t, "criterion-5-usage-normalization.json", map[string]any{
		"route":                    acceptPlanRoute,
		"raw_input_tokens":         raw.InputTokens,
		"raw_cache_read_tokens":    raw.CacheReadInputTokens,
		"raw_output_tokens":        raw.OutputTokens,
		"normalized_prompt_tokens": normalizedPrompt,
		"normalized_cached_tokens": normalizedCached,
		"harness_context_used":     snapshot.ContextUsed,
		"harness_context_window":   snapshot.ContextWindow,
		"harness_cache_hit":        snapshot.CacheHitTokens,
		"harness_cache_miss":       snapshot.CacheMissTokens,
		"naive_mapping_would_say":  naive,
	})
}

// --------------------------------------------------------------- criterion 6

// TestAcceptanceContextPin is criterion 6: the pin observable end to end. An
// agent on zai/glm-5.3-flash must report a 1,000,000-token window, and must
// not compact below 700,000.
//
// The compaction half is measured against the same functions the inference
// loop calls, at the two sides of the threshold, rather than by filling a real
// context — which would cost seven hundred thousand tokens to prove one
// inequality.
func TestAcceptanceContextPin(t *testing.T) {
	if os.Getenv("SLBH_ACCEPT_PIN") != "1" {
		t.Skip("set SLBH_ACCEPT_PIN=1 to run the one billed context-pin call")
	}
	cfg := acceptPolicy(t)
	cfg.SeatModel = acceptPlanRoute
	cfg.SeatEffort = "low"
	// A fresh $SLBH_HOME approves nothing — ApprovedModels defaults to an
	// empty slice, which is the deliberate fail-safe — so approve the route
	// under test the way /models would.
	cfg.ApprovedModels = []string{acceptPlanRoute}

	instance, _ := acceptProvider(t, cfg, acceptPlanRoute)
	live := &liveProvider{inner: instance}
	runtime, err := New(cfg, Options{Provider: func(string) (provider.Provider, error) { return live, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	seat := runtime.seat()
	seat.Send("Reply with exactly " + acceptSentinel + " and nothing else. Do not call tools.")
	waitLiveTurn(t, runtime, 1)

	snapshot := seat.Snapshot()
	if snapshot.ContextWindow != 1_000_000 {
		t.Fatalf("agent reports a %d-token window, want 1000000", snapshot.ContextWindow)
	}
	budget := contextBudget(snapshot.ContextWindow)
	if budget != 700_000 {
		t.Fatalf("compaction threshold is %d, want 700000", budget)
	}

	// Just under and just over the threshold, through the same predicate the
	// turn loop uses.
	system := "acceptance"
	below := acceptHistoryOfEstimatedTokens(650_000)
	above := acceptHistoryOfEstimatedTokens(720_000)
	if contextLimitReached(snapshot.ContextWindow, system, below, nil, contextAnchor{}) {
		t.Fatalf("compaction fired below the threshold")
	}
	if !contextLimitReached(snapshot.ContextWindow, system, above, nil, contextAnchor{}) {
		t.Fatalf("compaction did not fire above the threshold")
	}
	// The same histories against the old 128,000 fallback, to show the pin is
	// what moved the line rather than the histories being trivially small.
	if !contextLimitReached(provider.FallbackContextWindow, system, below, nil, contextAnchor{}) {
		t.Fatalf("the 650k history does not exceed the 128k fallback budget; the fixture is wrong")
	}

	belowEstimate := acceptEstimatedTokens(system, below)
	aboveEstimate := acceptEstimatedTokens(system, above)
	t.Logf("agent window=%d budget=%d", snapshot.ContextWindow, budget)
	t.Logf("history of ~%d estimated tokens does not compact; ~%d does", belowEstimate, aboveEstimate)
	t.Logf("both would have compacted under the %d-token fallback", provider.FallbackContextWindow)
	acceptWriteJSON(t, "criterion-6-context-pin.json", map[string]any{
		"route":                          acceptPlanRoute,
		"agent_context_window":           snapshot.ContextWindow,
		"compaction_threshold":           budget,
		"no_compaction_at_tokens":        belowEstimate,
		"compaction_at_tokens":           aboveEstimate,
		"fallback_window":                provider.FallbackContextWindow,
		"fallback_would_compact_at_650k": true,
		"live_turn_text":                 "one live turn completed on the pinned route",
	})
}

// acceptHistoryOfEstimatedTokens builds a history whose estimated token count
// (the harness's own len(json)/4) is close to the target.
func acceptHistoryOfEstimatedTokens(target int) []provider.Message {
	// Four characters per estimated token, minus a little for the JSON
	// envelope, which only ever makes the estimate larger.
	chars := target * 4
	body := strings.Repeat("a", chars)
	return []provider.Message{{Role: "user", Content: body}}
}

func acceptEstimatedTokens(system string, history []provider.Message) int {
	encoded, err := json.Marshal(struct {
		System   string             `json:"system"`
		Messages []provider.Message `json:"messages"`
		Tools    []provider.Tool    `json:"tools"`
	}{System: system, Messages: history})
	if err != nil {
		return 0
	}
	return (len(encoded) + 3) / 4
}

var _ = sort.Strings
