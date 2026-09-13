// Command matrix runs the slbh provider-routing capability matrix against the
// two wires the Z.ai coding plan exposes: the OpenAI-shaped chat-completions
// endpoint slbh uses today, and the Anthropic-shaped Messages endpoint phase 2
// of org/slbh-routing-plan-2026-09-13.md would move `zai` to.
//
// It is deliberately outside cmd/ so the fleet artifact build never picks it
// up, and deliberately self-contained: it speaks both wires directly rather
// than through internal/provider, because the point is to measure the
// endpoints rather than the harness's current view of them.
//
// No credential is ever written to a result file. Authorization and x-api-key
// header values are replaced with "REDACTED" before a record is serialized.
//
// Usage:
//
//	go run ./tools/matrix                       # every probe, both endpoints
//	go run ./tools/matrix -probe cache,text     # a subset
//	go run ./tools/matrix -endpoint anthropic   # one wire
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	openAIURL    = "https://api.z.ai/api/coding/paas/v4/chat/completions"
	openAIModels = "https://api.z.ai/api/coding/paas/v4/models"
	anthropicURL = "https://api.z.ai/api/anthropic/v1/messages"
	anthropicMod = "https://api.z.ai/api/anthropic/v1/models"

	modelOpenAI    = "glm-5.3-flash"
	modelAnthropic = "glm-5.3-flash"
	// Z.ai's own Claude Code guide names the [1m] suffixed slugs on the
	// Anthropic wire, so both spellings are probed and both recorded.
	modelAnthropic1M = "glm-5.3-flash[1m]"

	anthropicVersion = "2023-06-01"

	requestTimeout = 900 * time.Second
	systemPrompt   = "You are a terse capability-probe target. Answer exactly what is asked, with no preamble and no closing remarks."
)

var (
	flagProbe    = flag.String("probe", "", "comma-separated probe names to run (default: all)")
	flagEndpoint = flag.String("endpoint", "", "comma-separated endpoint names to run (default: all)")
	flagOut      = flag.String("out", "tools/matrix/results/2026-09-13", "results directory")
	flagReps     = flag.Int("reps", 2, "repetitions for stochastic probes")
)

// ---------------------------------------------------------------- endpoints

type endpoint struct {
	Name  string
	Wire  string // "openai" or "anthropic"
	URL   string
	Model string
}

var endpoints = []endpoint{
	{Name: "openai-coding", Wire: "openai", URL: openAIURL, Model: modelOpenAI},
	{Name: "anthropic", Wire: "anthropic", URL: anthropicURL, Model: modelAnthropic},
}

// ------------------------------------------------------------------ records

type headerSet map[string]string

type callRecord struct {
	Label      string          `json:"label"`
	Method     string          `json:"method"`
	URL        string          `json:"url"`
	Headers    headerSet       `json:"headers_redacted"`
	Body       json.RawMessage `json:"request_body,omitempty"`
	Status     int             `json:"http_status"`
	StatusText string          `json:"http_status_text"`
	DurationMS int64           `json:"duration_ms"`
	RawEvents  []string        `json:"raw_sse_events,omitempty"`
	RawBody    string          `json:"raw_body,omitempty"`
	Obs        *observations   `json:"observations,omitempty"`
	Transport  string          `json:"transport_error,omitempty"`
}

type toolObs struct {
	Index int    `json:"index"`
	ID    string `json:"id"`
	Name  string `json:"name"`
	Args  string `json:"accumulated_arguments"`
	Valid bool   `json:"arguments_parse_ok"`
}

type observations struct {
	EventTypes      map[string]int `json:"event_types"`
	TextChars       int            `json:"text_chars"`
	TextSample      string         `json:"text_sample"`
	ReasoningChars  int            `json:"reasoning_chars"`
	ReasoningSample string         `json:"reasoning_sample"`
	ToolCalls       []toolObs      `json:"tool_calls,omitempty"`
	Usage           map[string]any `json:"usage,omitempty"`
	StopReason      string         `json:"stop_reason,omitempty"`
	Terminal        string         `json:"terminal"`
	ErrorEvents     []string       `json:"error_events,omitempty"`
}

type probeResult struct {
	Probe      string       `json:"probe"`
	Endpoint   string       `json:"endpoint"`
	Wire       string       `json:"wire"`
	Model      string       `json:"model"`
	Rep        int          `json:"repetition"`
	StartedUTC string       `json:"started_utc"`
	Calls      []callRecord `json:"calls"`
	Verdict    string       `json:"verdict"`
	Notes      []string     `json:"notes"`
}

func (p *probeResult) note(format string, args ...any) {
	p.Notes = append(p.Notes, fmt.Sprintf(format, args...))
}

// ---------------------------------------------------------------- HTTP layer

var client = &http.Client{Timeout: requestTimeout}

func apiKey() string { return strings.TrimSpace(os.Getenv("ZAI_API_KEY")) }

func redact(h http.Header) headerSet {
	out := headerSet{}
	for name, values := range h {
		lower := strings.ToLower(name)
		if lower == "authorization" || lower == "x-api-key" {
			out[name] = "REDACTED"
			continue
		}
		out[name] = strings.Join(values, ", ")
	}
	return out
}

type callOpts struct {
	Label   string
	Method  string
	URL     string
	Wire    string
	Body    any
	Headers map[string]string // extra or overriding headers
	Stream  bool
}

func doCall(ctx context.Context, o callOpts) callRecord {
	rec := callRecord{Label: o.Label, Method: o.Method, URL: o.URL}
	var reader io.Reader
	if o.Body != nil {
		encoded, err := json.Marshal(o.Body)
		if err != nil {
			rec.Transport = "marshal: " + err.Error()
			return rec
		}
		rec.Body = encoded
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, o.Method, o.URL, reader)
	if err != nil {
		rec.Transport = err.Error()
		return rec
	}
	if o.Body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if o.Stream {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	switch o.Wire {
	case "anthropic":
		req.Header.Set("x-api-key", apiKey())
		req.Header.Set("anthropic-version", anthropicVersion)
	default:
		req.Header.Set("Authorization", "Bearer "+apiKey())
	}
	for name, value := range o.Headers {
		if value == "" {
			req.Header.Del(name)
			continue
		}
		req.Header.Set(name, value)
	}
	rec.Headers = redact(req.Header)

	started := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		rec.DurationMS = time.Since(started).Milliseconds()
		rec.Transport = err.Error()
		return rec
	}
	defer resp.Body.Close()
	rec.Status = resp.StatusCode
	rec.StatusText = resp.Status

	contentType := resp.Header.Get("Content-Type")
	if o.Stream && resp.StatusCode >= 200 && resp.StatusCode < 300 && strings.Contains(contentType, "event-stream") {
		events, obs, err := readSSE(resp.Body, o.Wire)
		rec.RawEvents = events
		rec.Obs = obs
		if err != nil {
			rec.Transport = "sse: " + err.Error()
		}
	} else {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
		rec.RawBody = strings.TrimSpace(string(data))
	}
	rec.DurationMS = time.Since(started).Milliseconds()
	return rec
}

// ------------------------------------------------------------- SSE decoding

func readSSE(body io.Reader, wire string) ([]string, *observations, error) {
	obs := &observations{EventTypes: map[string]int{}}
	raw := []string{}
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)

	var text, reasoning strings.Builder
	toolByIndex := map[int]*toolObs{}
	order := []int{}
	currentEvent := ""

	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimRight(line, "\r")
		if trimmed != "" {
			raw = append(raw, trimmed)
		}
		switch {
		case strings.HasPrefix(trimmed, "event:"):
			currentEvent = strings.TrimSpace(strings.TrimPrefix(trimmed, "event:"))
			obs.EventTypes[currentEvent]++
		case strings.HasPrefix(trimmed, "data:"):
			data := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
			if data == "[DONE]" {
				obs.EventTypes["[DONE]"]++
				obs.Terminal = "[DONE]"
				continue
			}
			if wire == "anthropic" {
				decodeAnthropicEvent(currentEvent, data, obs, &text, &reasoning, toolByIndex, &order)
			} else {
				decodeOpenAIChunk(data, obs, &text, &reasoning, toolByIndex, &order)
			}
		}
	}
	if obs.Terminal == "" {
		if obs.EventTypes["message_stop"] > 0 {
			obs.Terminal = "message_stop"
		} else {
			obs.Terminal = "eof-without-terminal-marker"
		}
	}
	obs.TextChars = text.Len()
	obs.TextSample = sample(text.String(), 600)
	obs.ReasoningChars = reasoning.Len()
	obs.ReasoningSample = sample(reasoning.String(), 600)
	for _, index := range order {
		call := toolByIndex[index]
		var probe any
		call.Valid = json.Unmarshal([]byte(call.Args), &probe) == nil
		obs.ToolCalls = append(obs.ToolCalls, *call)
	}
	return raw, obs, scanner.Err()
}

func tool(toolByIndex map[int]*toolObs, order *[]int, index int) *toolObs {
	if existing, ok := toolByIndex[index]; ok {
		return existing
	}
	created := &toolObs{Index: index}
	toolByIndex[index] = created
	*order = append(*order, index)
	return created
}

func decodeOpenAIChunk(data string, obs *observations, text, reasoning *strings.Builder, toolByIndex map[int]*toolObs, order *[]int) {
	obs.EventTypes["data"]++
	var chunk struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Delta        struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				Reasoning        string `json:"reasoning"`
				ToolCalls        []struct {
					Index    int    `json:"index"`
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
		} `json:"choices"`
		Usage map[string]any  `json:"usage"`
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		obs.ErrorEvents = append(obs.ErrorEvents, "undecodable chunk: "+sample(data, 400))
		return
	}
	if len(chunk.Error) > 0 && string(chunk.Error) != "null" {
		obs.ErrorEvents = append(obs.ErrorEvents, sample(string(chunk.Error), 600))
	}
	for _, choice := range chunk.Choices {
		if choice.FinishReason != "" {
			obs.StopReason = choice.FinishReason
		}
		text.WriteString(choice.Delta.Content)
		if choice.Delta.ReasoningContent != "" {
			reasoning.WriteString(choice.Delta.ReasoningContent)
		} else {
			reasoning.WriteString(choice.Delta.Reasoning)
		}
		for _, call := range choice.Delta.ToolCalls {
			entry := tool(toolByIndex, order, call.Index)
			if call.ID != "" {
				entry.ID = call.ID
			}
			if call.Function.Name != "" {
				entry.Name = call.Function.Name
			}
			entry.Args += call.Function.Arguments
		}
	}
	if len(chunk.Usage) > 0 {
		obs.Usage = chunk.Usage
	}
}

func decodeAnthropicEvent(eventName, data string, obs *observations, text, reasoning *strings.Builder, toolByIndex map[int]*toolObs, order *[]int) {
	var event struct {
		Type    string `json:"type"`
		Index   int    `json:"index"`
		Message struct {
			Usage      map[string]any `json:"usage"`
			StopReason string         `json:"stop_reason"`
		} `json:"message"`
		ContentBlock struct {
			Type string `json:"type"`
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"content_block"`
		Delta struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
			Thinking    string `json:"thinking"`
			PartialJSON string `json:"partial_json"`
			StopReason  string `json:"stop_reason"`
		} `json:"delta"`
		Usage map[string]any  `json:"usage"`
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal([]byte(data), &event); err != nil {
		obs.ErrorEvents = append(obs.ErrorEvents, "undecodable event: "+sample(data, 400))
		return
	}
	kind := event.Type
	if kind == "" {
		kind = eventName
	}
	switch kind {
	case "message_start":
		if len(event.Message.Usage) > 0 {
			obs.Usage = mergeUsage(obs.Usage, event.Message.Usage)
		}
	case "content_block_start":
		if event.ContentBlock.Type == "tool_use" {
			entry := tool(toolByIndex, order, event.Index)
			entry.ID = event.ContentBlock.ID
			entry.Name = event.ContentBlock.Name
		}
	case "content_block_delta":
		switch event.Delta.Type {
		case "text_delta":
			text.WriteString(event.Delta.Text)
		case "thinking_delta":
			reasoning.WriteString(event.Delta.Thinking)
		case "input_json_delta":
			entry := tool(toolByIndex, order, event.Index)
			entry.Args += event.Delta.PartialJSON
		}
	case "message_delta":
		if event.Delta.StopReason != "" {
			obs.StopReason = event.Delta.StopReason
		}
		if len(event.Usage) > 0 {
			obs.Usage = mergeUsage(obs.Usage, event.Usage)
		}
	case "message_stop":
		obs.Terminal = "message_stop"
	case "error":
		obs.ErrorEvents = append(obs.ErrorEvents, sample(data, 600))
	}
}

func mergeUsage(into, from map[string]any) map[string]any {
	if into == nil {
		into = map[string]any{}
	}
	for key, value := range from {
		into[key] = value
	}
	return into
}

func sample(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + fmt.Sprintf("… (+%d chars)", len(s)-n)
}

// ------------------------------------------------------------ body builders

type toolDef struct {
	Name        string
	Description string
	Schema      map[string]any
}

func probeTools() []toolDef {
	return []toolDef{
		{
			Name:        "get_weather",
			Description: "Return the current weather for one city. Call once per city.",
			Schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"city": map[string]any{"type": "string", "description": "City name"},
					"unit": map[string]any{"type": "string", "enum": []string{"c", "f"}},
				},
				"required":             []string{"city", "unit"},
				"additionalProperties": false,
			},
		},
		{
			Name:        "get_time",
			Description: "Return the local time for one city.",
			Schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"city": map[string]any{"type": "string", "description": "City name"},
				},
				"required":             []string{"city"},
				"additionalProperties": false,
			},
		},
	}
}

type bodyOpts struct {
	Model        string
	System       string
	User         string
	Effort       string // openai reasoning_effort, or passthrough on anthropic
	Budget       int    // anthropic thinking budget_tokens; 0 disables the field
	MaxTokens    int
	Tools        []toolDef
	CacheKey     string
	CacheControl bool // anthropic: mark the system block ephemeral
	Temperature  *float64
	Extra        map[string]any
}

func zero() *float64 { v := 0.0; return &v }

func openAIBody(o bodyOpts) map[string]any {
	messages := []map[string]any{
		{"role": "system", "content": o.System},
		{"role": "user", "content": o.User},
	}
	body := map[string]any{
		"model":          o.Model,
		"stream":         true,
		"messages":       messages,
		"stream_options": map[string]any{"include_usage": true},
	}
	if o.MaxTokens > 0 {
		body["max_tokens"] = o.MaxTokens
	}
	if o.Effort != "" {
		body["reasoning_effort"] = o.Effort
	}
	if o.Temperature != nil {
		body["temperature"] = *o.Temperature
	}
	if o.CacheKey != "" {
		body["prompt_cache_key"] = o.CacheKey
	}
	if len(o.Tools) > 0 {
		tools := make([]map[string]any, 0, len(o.Tools))
		for _, t := range o.Tools {
			tools = append(tools, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        t.Name,
					"description": t.Description,
					"parameters":  t.Schema,
				},
			})
		}
		body["tools"] = tools
		body["tool_choice"] = "auto"
	}
	for key, value := range o.Extra {
		body[key] = value
	}
	return body
}

func anthropicBody(o bodyOpts) map[string]any {
	systemBlock := map[string]any{"type": "text", "text": o.System}
	if o.CacheControl {
		systemBlock["cache_control"] = map[string]any{"type": "ephemeral"}
	}
	maxTokens := o.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 2048
	}
	if o.Budget > 0 && maxTokens <= o.Budget {
		maxTokens = o.Budget + 1024
	}
	body := map[string]any{
		"model":      o.Model,
		"stream":     true,
		"max_tokens": maxTokens,
		"system":     []map[string]any{systemBlock},
		"messages": []map[string]any{
			{"role": "user", "content": []map[string]any{{"type": "text", "text": o.User}}},
		},
	}
	if o.Budget > 0 {
		body["thinking"] = map[string]any{"type": "enabled", "budget_tokens": o.Budget}
	}
	if o.Effort != "" {
		body["reasoning_effort"] = o.Effort
	}
	if o.Temperature != nil {
		body["temperature"] = *o.Temperature
	}
	if len(o.Tools) > 0 {
		tools := make([]map[string]any, 0, len(o.Tools))
		for _, t := range o.Tools {
			tools = append(tools, map[string]any{
				"name":         t.Name,
				"description":  t.Description,
				"input_schema": t.Schema,
			})
		}
		body["tools"] = tools
	}
	for key, value := range o.Extra {
		body[key] = value
	}
	return body
}

func buildBody(ep endpoint, o bodyOpts) map[string]any {
	if o.Model == "" {
		o.Model = ep.Model
	}
	if ep.Wire == "anthropic" {
		return anthropicBody(o)
	}
	return openAIBody(o)
}

// ------------------------------------------------------------------- probes

type probe struct {
	Name string
	Reps func() int
	Run  func(ctx context.Context, ep endpoint, rep int, out *probeResult)
}

// effortBudget maps slbh's effort names onto an Anthropic thinking budget.
// The mapping is written down here rather than derived, because the plan
// requires an unmappable level to be a recorded fact rather than a guess.
var effortBudget = map[string]int{
	"low":    1024,
	"medium": 4096,
	"high":   16384,
	"xhigh":  32768,
	"max":    65536,
}

var effortLevels = []string{"low", "medium", "high", "xhigh", "max"}

func fixedReps() int { return *flagReps }
func onceRep() int   { return 1 }

// effortReps is three rather than two: a first pass at two repetitions
// produced reasoning volumes whose ranges overlapped between adjacent levels,
// so the plan's allowance for a third repetition where the evidence needs one
// is spent here.
func effortReps() int { return 3 }

func probes() []probe {
	list := []probe{
		{Name: "auth-headers", Reps: onceRep, Run: probeAuthHeaders},
		{Name: "catalog", Reps: onceRep, Run: probeCatalog},
		{Name: "model-slugs", Reps: onceRep, Run: probeModelSlugs},
		{Name: "text", Reps: fixedReps, Run: probeText},
		{Name: "usage", Reps: fixedReps, Run: probeUsage},
		{Name: "tool-single", Reps: fixedReps, Run: probeToolSingle},
		{Name: "tool-parallel", Reps: fixedReps, Run: probeToolParallel},
		{Name: "error-events", Reps: onceRep, Run: probeErrorEvents},
		{Name: "cache", Reps: fixedReps, Run: probeCache},
		{Name: "context-limit", Reps: onceRep, Run: probeContextLimit},
		{Name: "context-ceiling", Reps: onceRep, Run: probeContextCeiling},
	}
	for _, level := range effortLevels {
		list = append(list, probe{Name: "effort-" + level, Reps: effortReps, Run: effortProbe(level)})
	}
	return list
}

func probeAuthHeaders(ctx context.Context, ep endpoint, rep int, out *probeResult) {
	body := buildBody(ep, bodyOpts{User: "Reply with the single word: ok", System: systemPrompt, MaxTokens: 64, Temperature: zero()})
	variants := []struct {
		label   string
		headers map[string]string
	}{}
	if ep.Wire == "anthropic" {
		variants = append(variants,
			struct {
				label   string
				headers map[string]string
			}{"x-api-key + anthropic-version", nil},
			struct {
				label   string
				headers map[string]string
			}{"authorization-bearer only", map[string]string{"x-api-key": "", "Authorization": "Bearer " + apiKey()}},
			struct {
				label   string
				headers map[string]string
			}{"x-api-key without anthropic-version", map[string]string{"anthropic-version": ""}},
		)
	} else {
		variants = append(variants,
			struct {
				label   string
				headers map[string]string
			}{"authorization-bearer", nil},
			struct {
				label   string
				headers map[string]string
			}{"x-api-key only", map[string]string{"Authorization": "", "x-api-key": apiKey()}},
		)
	}
	accepted := []string{}
	for _, variant := range variants {
		rec := doCall(ctx, callOpts{Label: variant.label, Method: http.MethodPost, URL: ep.URL, Wire: ep.Wire, Body: body, Headers: variant.headers, Stream: true})
		out.Calls = append(out.Calls, rec)
		if rec.Status == 200 {
			accepted = append(accepted, variant.label)
		}
		out.note("%s -> HTTP %d", variant.label, rec.Status)
	}
	if len(accepted) > 0 {
		out.Verdict = "pass"
		out.note("accepted auth shapes: %s", strings.Join(accepted, "; "))
	} else {
		out.Verdict = "fail"
		out.note("no auth header shape was accepted")
	}
}

func probeCatalog(ctx context.Context, ep endpoint, rep int, out *probeResult) {
	url := openAIModels
	if ep.Wire == "anthropic" {
		url = anthropicMod
	}
	rec := doCall(ctx, callOpts{Label: "GET models", Method: http.MethodGet, URL: url, Wire: ep.Wire})
	out.Calls = append(out.Calls, rec)
	out.note("%s -> HTTP %d", url, rec.Status)
	if rec.Status == 200 {
		out.Verdict = "pass"
		out.note("catalog body: %s", sample(rec.RawBody, 1500))
	} else {
		out.Verdict = "fail"
		out.note("catalog body: %s", sample(rec.RawBody, 800))
	}
}

func probeModelSlugs(ctx context.Context, ep endpoint, rep int, out *probeResult) {
	slugs := []string{modelOpenAI, modelAnthropic1M, "glm-5.3", "glm-5.3[1m]"}
	ok := []string{}
	for _, slug := range slugs {
		body := buildBody(ep, bodyOpts{Model: slug, System: systemPrompt, User: "Reply with the single word: ok", MaxTokens: 64, Temperature: zero()})
		rec := doCall(ctx, callOpts{Label: "slug " + slug, Method: http.MethodPost, URL: ep.URL, Wire: ep.Wire, Body: body, Stream: true})
		out.Calls = append(out.Calls, rec)
		if rec.Status == 200 {
			ok = append(ok, slug)
			out.note("slug %q accepted (HTTP 200)", slug)
		} else {
			out.note("slug %q rejected: HTTP %d %s", slug, rec.Status, sample(rec.RawBody, 300))
		}
	}
	if len(ok) > 0 {
		out.Verdict = "pass"
		out.note("accepted slugs: %s", strings.Join(ok, ", "))
	} else {
		out.Verdict = "fail"
	}
}

func probeText(ctx context.Context, ep endpoint, rep int, out *probeResult) {
	body := buildBody(ep, bodyOpts{
		System:      systemPrompt,
		User:        "Reply with exactly these five words and nothing else: the quick brown fox jumps",
		MaxTokens:   1024,
		Temperature: zero(),
		CacheKey:    "slbh-matrix-text",
	})
	rec := doCall(ctx, callOpts{Label: "plain text", Method: http.MethodPost, URL: ep.URL, Wire: ep.Wire, Body: body, Stream: true})
	out.Calls = append(out.Calls, rec)
	if rec.Status != 200 || rec.Obs == nil {
		out.Verdict = "fail"
		out.note("HTTP %d %s", rec.Status, sample(rec.RawBody, 400))
		return
	}
	out.note("terminal=%s stop_reason=%s text_chars=%d reasoning_chars=%d", rec.Obs.Terminal, rec.Obs.StopReason, rec.Obs.TextChars, rec.Obs.ReasoningChars)
	out.note("text: %q", strings.TrimSpace(rec.Obs.TextSample))
	if rec.Obs.TextChars > 0 && strings.Contains(strings.ToLower(rec.Obs.TextSample), "quick brown fox") {
		out.Verdict = "pass"
	} else {
		out.Verdict = "fail"
	}
}

func probeUsage(ctx context.Context, ep endpoint, rep int, out *probeResult) {
	body := buildBody(ep, bodyOpts{
		System:      systemPrompt,
		User:        "Name three primary colours, comma separated.",
		MaxTokens:   512,
		Temperature: zero(),
		CacheKey:    "slbh-matrix-usage",
	})
	rec := doCall(ctx, callOpts{Label: "usage", Method: http.MethodPost, URL: ep.URL, Wire: ep.Wire, Body: body, Stream: true})
	out.Calls = append(out.Calls, rec)
	if rec.Status != 200 || rec.Obs == nil {
		out.Verdict = "fail"
		out.note("HTTP %d %s", rec.Status, sample(rec.RawBody, 400))
		return
	}
	usage := rec.Obs.Usage
	encoded, _ := json.Marshal(usage)
	out.note("usage object: %s", string(encoded))
	promptField := ""
	for _, candidate := range []string{"prompt_tokens", "input_tokens"} {
		if _, ok := usage[candidate]; ok {
			promptField = candidate
			break
		}
	}
	completionField := ""
	for _, candidate := range []string{"completion_tokens", "output_tokens"} {
		if _, ok := usage[candidate]; ok {
			completionField = candidate
			break
		}
	}
	out.note("prompt-token field=%q completion-token field=%q", promptField, completionField)
	if promptField != "" && completionField != "" {
		out.Verdict = "pass"
	} else {
		out.Verdict = "fail"
	}
}

func probeToolSingle(ctx context.Context, ep endpoint, rep int, out *probeResult) {
	body := buildBody(ep, bodyOpts{
		System:      systemPrompt,
		User:        "What is the weather in Denver in celsius? Call the tool; do not answer in prose.",
		MaxTokens:   2048,
		Temperature: zero(),
		Tools:       probeTools(),
		CacheKey:    "slbh-matrix-tools",
	})
	rec := doCall(ctx, callOpts{Label: "single tool call", Method: http.MethodPost, URL: ep.URL, Wire: ep.Wire, Body: body, Stream: true})
	out.Calls = append(out.Calls, rec)
	if rec.Status != 200 || rec.Obs == nil {
		out.Verdict = "fail"
		out.note("HTTP %d %s", rec.Status, sample(rec.RawBody, 400))
		return
	}
	calls := rec.Obs.ToolCalls
	out.note("stop_reason=%s tool_calls=%d", rec.Obs.StopReason, len(calls))
	for _, call := range calls {
		out.note("index=%d id=%q name=%q args=%s parse_ok=%v", call.Index, call.ID, call.Name, call.Args, call.Valid)
	}
	if len(calls) == 1 && calls[0].Name == "get_weather" && calls[0].Valid && strings.Contains(strings.ToLower(calls[0].Args), "denver") {
		out.Verdict = "pass"
	} else {
		out.Verdict = "fail"
	}
}

func probeToolParallel(ctx context.Context, ep endpoint, rep int, out *probeResult) {
	body := buildBody(ep, bodyOpts{
		System:      systemPrompt,
		User:        "Call get_weather once for each of Denver, Osaka and Reykjavik, all in the same response, with unit \"c\". Then call get_time for Denver. Do not answer in prose.",
		MaxTokens:   4096,
		Temperature: zero(),
		Tools:       probeTools(),
		CacheKey:    "slbh-matrix-tools",
	})
	rec := doCall(ctx, callOpts{Label: "parallel tool calls", Method: http.MethodPost, URL: ep.URL, Wire: ep.Wire, Body: body, Stream: true})
	out.Calls = append(out.Calls, rec)
	if rec.Status != 200 || rec.Obs == nil {
		out.Verdict = "fail"
		out.note("HTTP %d %s", rec.Status, sample(rec.RawBody, 400))
		return
	}
	calls := rec.Obs.ToolCalls
	indexes := map[int]bool{}
	allValid := true
	for _, call := range calls {
		indexes[call.Index] = true
		if !call.Valid {
			allValid = false
		}
		out.note("index=%d id=%q name=%q args=%s parse_ok=%v", call.Index, call.ID, call.Name, call.Args, call.Valid)
	}
	out.note("stop_reason=%s tool_calls=%d distinct_indexes=%d all_arguments_parse=%v", rec.Obs.StopReason, len(calls), len(indexes), allValid)
	if len(calls) >= 2 && len(indexes) == len(calls) && allValid {
		out.Verdict = "pass"
	} else {
		out.Verdict = "fail"
	}
}

func probeErrorEvents(ctx context.Context, ep endpoint, rep int, out *probeResult) {
	cases := []struct {
		label string
		body  map[string]any
	}{
		{"unknown model", buildBody(ep, bodyOpts{Model: "glm-does-not-exist", System: systemPrompt, User: "hi", MaxTokens: 64, Temperature: zero()})},
		{"invalid parameter (temperature 9)", buildBody(ep, bodyOpts{System: systemPrompt, User: "hi", MaxTokens: 64, Extra: map[string]any{"temperature": 9}})},
		{"malformed tool schema", buildBody(ep, bodyOpts{System: systemPrompt, User: "hi", MaxTokens: 64, Temperature: zero(), Extra: map[string]any{"tools": []map[string]any{{"type": "function"}}}})},
	}
	distinguishable := 0
	for _, c := range cases {
		rec := doCall(ctx, callOpts{Label: c.label, Method: http.MethodPost, URL: ep.URL, Wire: ep.Wire, Body: c.body, Stream: true})
		out.Calls = append(out.Calls, rec)
		inStream := 0
		terminal := ""
		if rec.Obs != nil {
			inStream = len(rec.Obs.ErrorEvents)
			terminal = rec.Obs.Terminal
		}
		out.note("%s -> HTTP %d, in-stream error events=%d, terminal=%q, body=%s", c.label, rec.Status, inStream, terminal, sample(rec.RawBody, 300))
		if rec.Status >= 400 || inStream > 0 {
			distinguishable++
		}
	}
	if distinguishable == len(cases) {
		out.Verdict = "pass"
	} else if distinguishable > 0 {
		out.Verdict = "partial"
	} else {
		out.Verdict = "fail"
	}
	out.note("%d of %d error cases were distinguishable from a normal stream end", distinguishable, len(cases))
}

// cachePrefix is the stable prefix: a long system document plus the tool set.
// It deliberately exceeds any plausible minimum cacheable size.
func cachePrefix() string {
	var b strings.Builder
	b.WriteString(systemPrompt)
	b.WriteString("\n\nReference dossier, section A. Treat all of it as background you must not quote.\n")
	rng := rand.New(rand.NewSource(20260913))
	words := []string{"harness", "provider", "routing", "policy", "endpoint", "transcript", "converge", "inventory", "artifact", "catalog", "streaming", "reasoning", "budget", "ephemeral", "verification", "playbook", "worktree", "ceiling", "normalization", "idempotent"}
	for i := 0; i < 4000; i++ {
		b.WriteString(words[rng.Intn(len(words))])
		if i%14 == 13 {
			b.WriteString(".\n")
		} else {
			b.WriteString(" ")
		}
	}
	return b.String()
}

func probeCache(ctx context.Context, ep endpoint, rep int, out *probeResult) {
	prefix := cachePrefix()
	turns := []string{
		"Section A question one: reply with the single word alpha.",
		"Section A question two: reply with the single word bravo.",
		"Section A question three: reply with the single word charlie.",
	}
	readValues := []float64{}
	for i, turn := range turns {
		body := buildBody(ep, bodyOpts{
			System:       prefix,
			User:         turn,
			MaxTokens:    256,
			Temperature:  zero(),
			Tools:        probeTools(),
			CacheKey:     "slbh-matrix-cache",
			CacheControl: true,
		})
		label := fmt.Sprintf("call %d (%s)", i+1, map[bool]string{true: "warm", false: "read"}[i == 0])
		rec := doCall(ctx, callOpts{Label: label, Method: http.MethodPost, URL: ep.URL, Wire: ep.Wire, Body: body, Stream: true})
		out.Calls = append(out.Calls, rec)
		if rec.Status != 200 || rec.Obs == nil {
			out.note("%s -> HTTP %d %s", label, rec.Status, sample(rec.RawBody, 300))
			continue
		}
		encoded, _ := json.Marshal(rec.Obs.Usage)
		out.note("%s usage: %s", label, string(encoded))
		cached, field := cacheMetric(rec.Obs.Usage)
		out.note("%s cache metric %s=%v", label, field, cached)
		if i > 0 {
			readValues = append(readValues, cached)
		}
		time.Sleep(2 * time.Second)
	}
	hit := false
	for _, value := range readValues {
		if value > 0 {
			hit = true
		}
	}
	if hit {
		out.Verdict = "pass"
	} else {
		out.Verdict = "fail"
	}
	out.note("cache reads after warm-up: %v (hit=%v)", readValues, hit)
}

func cacheMetric(usage map[string]any) (float64, string) {
	if usage == nil {
		return 0, "none"
	}
	for _, field := range []string{"cache_read_input_tokens", "cached_tokens"} {
		if value, ok := usage[field]; ok {
			if number, ok := value.(float64); ok {
				return number, field
			}
		}
	}
	if details, ok := usage["prompt_tokens_details"].(map[string]any); ok {
		if value, ok := details["cached_tokens"]; ok {
			if number, ok := value.(float64); ok {
				return number, "prompt_tokens_details.cached_tokens"
			}
		}
	}
	return 0, "absent"
}

func padding(tokens int) string {
	// Roughly three characters per token for this vocabulary; the exact ratio
	// does not matter because the probe reads the provider's own rejection.
	var b strings.Builder
	rng := rand.New(rand.NewSource(int64(tokens)))
	words := []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf", "hotel", "india", "juliet"}
	for i := 0; i < tokens; i++ {
		b.WriteString(words[rng.Intn(len(words))])
		b.WriteString(" ")
	}
	return b.String()
}

func probeContextLimit(ctx context.Context, ep endpoint, rep int, out *probeResult) {
	sizes := []int{150000, 220000, 1100000}
	for _, size := range sizes {
		body := buildBody(ep, bodyOpts{
			System:      systemPrompt,
			User:        padding(size) + "\nReply with the single word: ok",
			MaxTokens:   32,
			Temperature: zero(),
		})
		label := fmt.Sprintf("%d padding words", size)
		rec := doCall(ctx, callOpts{Label: label, Method: http.MethodPost, URL: ep.URL, Wire: ep.Wire, Body: body, Stream: true})
		// The padding itself is enormous and already described by its size, so
		// it is replaced in the saved record rather than stored three times.
		rec.Body = json.RawMessage(fmt.Sprintf("{\"_elided\":\"body omitted; user turn is %d filler words plus the instruction\",\"max_tokens\":32,\"model\":%q}", size, ep.Model))
		promptTokens := ""
		if rec.Obs != nil && rec.Obs.Usage != nil {
			encoded, _ := json.Marshal(rec.Obs.Usage)
			promptTokens = string(encoded)
		}
		out.Calls = append(out.Calls, rec)
		out.note("%s -> HTTP %d usage=%s body=%s", label, rec.Status, promptTokens, sample(rec.RawBody, 400))
	}
	out.Verdict = "recorded"
}

// probeContextCeiling narrows the real ceiling from above. A rejected request
// is never prefilled and so costs nothing, while an accepted one costs its
// whole prompt in plan quota, so the walk starts high and stops at the first
// acceptance: the last rejection and the first acceptance bracket the ceiling.
// The measured ratio on this vocabulary is 1.5015 prompt tokens per filler
// word (150,000 words -> 225,287 prompt tokens on the coding wire).
func probeContextCeiling(ctx context.Context, ep endpoint, rep int, out *probeResult) {
	targets := []int{1_400_000, 1_150_000, 1_050_000, 1_000_000, 900_000, 700_000, 500_000}
	lastRejected := 0
	for _, target := range targets {
		words := int(float64(target) / 1.5015)
		body := buildBody(ep, bodyOpts{
			System:      systemPrompt,
			User:        padding(words) + "\nReply with the single word: ok",
			MaxTokens:   32,
			Temperature: zero(),
		})
		label := fmt.Sprintf("~%d prompt tokens (%d filler words)", target, words)
		rec := doCall(ctx, callOpts{Label: label, Method: http.MethodPost, URL: ep.URL, Wire: ep.Wire, Body: body, Stream: true})
		rec.Body = json.RawMessage(fmt.Sprintf("{\"_elided\":\"body omitted; user turn is %d filler words plus the instruction\",\"max_tokens\":32,\"model\":%q}", words, ep.Model))
		usage := ""
		if rec.Obs != nil && rec.Obs.Usage != nil {
			encoded, _ := json.Marshal(rec.Obs.Usage)
			usage = string(encoded)
		}
		out.Calls = append(out.Calls, rec)
		out.note("%s -> HTTP %d usage=%s body=%s", label, rec.Status, usage, sample(rec.RawBody, 300))
		if rec.Status == 200 {
			out.note("first acceptance at ~%d prompt tokens; last rejection at ~%d: the ceiling is between them", target, lastRejected)
			out.Verdict = "recorded"
			return
		}
		lastRejected = target
	}
	out.note("every probed size was rejected down to ~%d prompt tokens", targets[len(targets)-1])
	out.Verdict = "recorded"
}

func effortProbe(level string) func(context.Context, endpoint, int, *probeResult) {
	return func(ctx context.Context, ep endpoint, rep int, out *probeResult) {
		question := "Define a(1)=7 and a(n)=a(n-1)+gcd(n,a(n-1)) for n>1. Work out every term from a(2) to a(60) one step at a time, showing each gcd you use, then list every n in that range where a(n)-a(n-1) is greater than 1, and finally state a(60). Be exhaustive and check your arithmetic as you go."
		opts := bodyOpts{
			System:      systemPrompt,
			User:        question,
			MaxTokens:   32768,
			Temperature: zero(),
			CacheKey:    "slbh-matrix-effort",
		}
		if ep.Wire == "anthropic" {
			opts.Budget = effortBudget[level]
		} else {
			opts.Effort = level
		}
		body := buildBody(ep, opts)
		rec := doCall(ctx, callOpts{Label: "effort " + level, Method: http.MethodPost, URL: ep.URL, Wire: ep.Wire, Body: body, Stream: true})
		out.Calls = append(out.Calls, rec)

		if ep.Wire == "anthropic" {
			// Second arm: does the proxy also accept the OpenAI-shaped
			// reasoning_effort string on this wire?
			passthrough := buildBody(ep, bodyOpts{
				System: systemPrompt, User: question, MaxTokens: 32768, Temperature: zero(), Effort: level,
			})
			alt := doCall(ctx, callOpts{Label: "reasoning_effort passthrough " + level, Method: http.MethodPost, URL: ep.URL, Wire: ep.Wire, Body: passthrough, Stream: true})
			out.Calls = append(out.Calls, alt)
			reasoning := 0
			if alt.Obs != nil {
				reasoning = alt.Obs.ReasoningChars
			}
			out.note("reasoning_effort passthrough -> HTTP %d reasoning_chars=%d %s", alt.Status, reasoning, sample(alt.RawBody, 250))
		}

		if rec.Status != 200 || rec.Obs == nil {
			out.Verdict = "refused"
			out.note("HTTP %d %s", rec.Status, sample(rec.RawBody, 400))
			return
		}
		encoded, _ := json.Marshal(rec.Obs.Usage)
		out.note("level=%s reasoning_chars=%d text_chars=%d usage=%s stop_reason=%s", level, rec.Obs.ReasoningChars, rec.Obs.TextChars, string(encoded), rec.Obs.StopReason)
		out.note("answer: %q", strings.TrimSpace(rec.Obs.TextSample))
		out.Verdict = "recorded"
	}
}

// --------------------------------------------------------------------- main

func main() {
	flag.Parse()
	if apiKey() == "" {
		fmt.Fprintln(os.Stderr, "ZAI_API_KEY is not set")
		os.Exit(2)
	}
	if err := os.MkdirAll(*flagOut, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	probeFilter := splitFilter(*flagProbe)
	endpointFilter := splitFilter(*flagEndpoint)

	type summaryRow struct {
		Probe    string `json:"probe"`
		Endpoint string `json:"endpoint"`
		Rep      int    `json:"repetition"`
		Verdict  string `json:"verdict"`
		File     string `json:"file"`
	}
	summary := []summaryRow{}

	ctx := context.Background()
	for _, ep := range endpoints {
		if len(endpointFilter) > 0 && !endpointFilter[ep.Name] {
			continue
		}
		for _, p := range probes() {
			if len(probeFilter) > 0 && !probeFilter[p.Name] {
				continue
			}
			for rep := 1; rep <= p.Reps(); rep++ {
				result := &probeResult{
					Probe:      p.Name,
					Endpoint:   ep.Name,
					Wire:       ep.Wire,
					Model:      ep.Model,
					Rep:        rep,
					StartedUTC: time.Now().UTC().Format(time.RFC3339),
					Verdict:    "unset",
				}
				func() {
					defer func() {
						if r := recover(); r != nil {
							result.Verdict = "error"
							result.note("panic: %v", r)
						}
					}()
					p.Run(ctx, ep, rep, result)
				}()
				name := fmt.Sprintf("%s__%s__rep%d.json", ep.Name, p.Name, rep)
				path := filepath.Join(*flagOut, name)
				encoded, err := json.MarshalIndent(result, "", "  ")
				if err != nil {
					fmt.Fprintln(os.Stderr, err)
					continue
				}
				if err := os.WriteFile(path, append(encoded, '\n'), 0o644); err != nil {
					fmt.Fprintln(os.Stderr, err)
					continue
				}
				summary = append(summary, summaryRow{Probe: p.Name, Endpoint: ep.Name, Rep: rep, Verdict: result.Verdict, File: name})
				fmt.Printf("%-14s %-16s rep%d  %-9s  %s\n", ep.Name, p.Name, rep, result.Verdict, name)
			}
		}
	}
	// The summary indexes every result file in the directory, not only the
	// probes this invocation ran, so a partial re-run leaves a complete index.
	existing, _ := filepath.Glob(filepath.Join(*flagOut, "*__*__rep*.json"))
	seen := map[string]bool{}
	for _, row := range summary {
		seen[row.File] = true
	}
	for _, path := range existing {
		name := filepath.Base(path)
		if seen[name] {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var prior probeResult
		if json.Unmarshal(data, &prior) != nil {
			continue
		}
		summary = append(summary, summaryRow{Probe: prior.Probe, Endpoint: prior.Endpoint, Rep: prior.Rep, Verdict: prior.Verdict, File: name})
	}
	sort.Slice(summary, func(i, j int) bool {
		if summary[i].Endpoint != summary[j].Endpoint {
			return summary[i].Endpoint < summary[j].Endpoint
		}
		if summary[i].Probe != summary[j].Probe {
			return summary[i].Probe < summary[j].Probe
		}
		return summary[i].Rep < summary[j].Rep
	})
	encoded, _ := json.MarshalIndent(summary, "", "  ")
	_ = os.WriteFile(filepath.Join(*flagOut, "summary.json"), append(encoded, '\n'), 0o644)
	fmt.Printf("\n%d probe results written to %s\n", len(summary), *flagOut)
}

func splitFilter(value string) map[string]bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	out := map[string]bool{}
	for _, part := range strings.Split(value, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out[trimmed] = true
		}
	}
	return out
}
