package provider

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// AnthropicVersion is the version header this wire sends. The Z.ai proxy does
// not enforce it — a request with no version header answered 200 in the
// capability matrix — which does not excuse sending the wrong thing. The wire
// is the contract, not what this proxy happens to tolerate today.
const AnthropicVersion = "2023-06-01"

// anthropicWireRequest is the Messages-API body.
//
// There is no `thinking` field and no `reasoning_effort` field, and their
// absence is the design rather than an omission: both were measured accepted
// and discarded on this endpoint, so neither is representable here. Effort is
// `output_config.effort` and nothing else.
type anthropicWireRequest struct {
	Model        string                `json:"model"`
	MaxTokens    int                   `json:"max_tokens"`
	Stream       bool                  `json:"stream"`
	System       []anthropicTextBlock  `json:"system,omitempty"`
	Messages     []anthropicMessage    `json:"messages"`
	Tools        []anthropicTool       `json:"tools,omitempty"`
	Temperature  *float64              `json:"temperature,omitempty"`
	OutputConfig *anthropicOutputCfg   `json:"output_config,omitempty"`
	Metadata     *anthropicRequestMeta `json:"metadata,omitempty"`
}

type anthropicOutputCfg struct {
	Effort string `json:"effort"`
}

// anthropicRequestMeta carries the stable prefix key. The cache on this plan
// is implicit and content-addressed, so this changes nothing about caching; it
// is carried so a transcript round trip reproduces the Request exactly, which
// the byte-replay contract requires on both wires.
type anthropicRequestMeta struct {
	UserID string `json:"user_id,omitempty"`
}

type anthropicTextBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type anthropicMessage struct {
	Role    string             `json:"role"`
	Content []anthropicContent `json:"content"`
}

// anthropicContent is every content block shape this wire uses, in one struct
// so a message's blocks stay a homogeneous slice.
type anthropicContent struct {
	Type string `json:"type"`
	// text
	Text string `json:"text,omitempty"`
	// thinking, replayed on assistant continuations
	Thinking string `json:"thinking,omitempty"`
	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	// tool_result
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
}

type anthropicTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema"`
}

// anthropicMaxTokens is the ceiling sent when a request does not set one. The
// fleet's pi entry for this model uses 128,000 and the matrix ran at 32,768;
// the field is required by the Messages API, so some value must be chosen.
const anthropicMaxTokens = 128000

// parseRFC3339Unix converts this catalog's `created_at` string into the Unix
// seconds `modelMetadata.Created` holds, which is what the OpenAI shape's
// `created` integer already is. Normalizing here keeps one representation in
// the catalog code rather than two.
func parseRFC3339Unix(value string) (int64, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, fmt.Errorf("empty timestamp")
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return 0, err
	}
	return parsed.Unix(), nil
}

type anthropicMessagesWire struct{}

func (anthropicMessagesWire) name() string { return WireAnthropicMessages }

// applyHeaders sends the Anthropic-canonical pair. Either auth header shape
// was accepted on either wire in the matrix and the version header was not
// enforced, so nothing here is forced by the endpoint — it is forced by the
// protocol this strategy claims to speak.
func (anthropicMessagesWire) applyHeaders(header http.Header, apiKey string) {
	if apiKey != "" {
		header.Set("x-api-key", apiKey)
	}
	header.Set("anthropic-version", AnthropicVersion)
	header.Set("Content-Type", "application/json")
	header.Set("Accept", "text/event-stream")
}

func (anthropicMessagesWire) payload(p *HTTPProvider, req Request) ([]byte, error) {
	effort, err := p.effortValue(req.Effort)
	if err != nil {
		return nil, err
	}
	body := anthropicWireRequest{
		Model:       p.modelID(req.Model),
		MaxTokens:   p.maxOutputTokens(req, anthropicMaxTokens),
		Stream:      true,
		Messages:    anthropicMessages(req.Messages),
		Temperature: req.Temperature,
	}
	if req.System != "" {
		body.System = []anthropicTextBlock{{Type: "text", Text: req.System}}
	}
	if effort != "" {
		body.OutputConfig = &anthropicOutputCfg{Effort: effort}
	}
	if req.CacheKey != "" {
		body.Metadata = &anthropicRequestMeta{UserID: req.CacheKey}
	}
	if len(req.Tools) > 0 {
		body.Tools = make([]anthropicTool, 0, len(req.Tools))
		for _, tool := range req.Tools {
			body.Tools = append(body.Tools, anthropicTool{
				Name:        tool.Name,
				Description: tool.Description,
				InputSchema: tool.Parameters,
			})
		}
	}
	return json.Marshal(body)
}

// anthropicMessages converts OpenAI-shaped messages into content blocks. This
// is real work rather than a reserialization: `tool_calls` on an assistant
// message become `tool_use` blocks, and a `tool` role message becomes a user
// message carrying a `tool_result` block, which is how this API continues a
// tool round trip.
func anthropicMessages(messages []Message) []anthropicMessage {
	converted := make([]anthropicMessage, 0, len(messages))
	for _, message := range messages {
		switch message.Role {
		case "tool":
			converted = append(converted, anthropicMessage{
				Role: "user",
				Content: []anthropicContent{{
					Type:      "tool_result",
					ToolUseID: message.ToolCallID,
					Content:   message.Content,
					IsError:   message.IsError,
				}},
			})
		case "assistant":
			blocks := make([]anthropicContent, 0, len(message.ToolCalls)+2)
			if message.ReasoningContent != "" {
				blocks = append(blocks, anthropicContent{Type: "thinking", Thinking: message.ReasoningContent})
			}
			if message.Content != "" {
				blocks = append(blocks, anthropicContent{Type: "text", Text: message.Content})
			}
			for _, call := range message.ToolCalls {
				input := json.RawMessage(strings.TrimSpace(call.Function.Arguments))
				if len(input) == 0 || !json.Valid(input) {
					// An incomplete fragment must not be sent as a malformed
					// object: an empty input is a well-formed request the model
					// can answer, where invalid JSON is a 422 on this wire.
					input = json.RawMessage(`{}`)
				}
				blocks = append(blocks, anthropicContent{
					Type:  "tool_use",
					ID:    call.ID,
					Name:  call.Function.Name,
					Input: input,
				})
			}
			if len(blocks) == 0 {
				blocks = append(blocks, anthropicContent{Type: "text", Text: ""})
			}
			converted = append(converted, anthropicMessage{Role: "assistant", Content: blocks})
		default:
			converted = append(converted, anthropicMessage{
				Role:    message.Role,
				Content: []anthropicContent{{Type: "text", Text: message.Content}},
			})
		}
	}
	return converted
}

// requestFromPayload is the other half of the byte-replay contract. Settled
// 2026-09-13: both wires carry it, so a transcript stays replayable on the
// route the campaigns actually run on.
func (anthropicMessagesWire) requestFromPayload(payload []byte) (Request, error) {
	var body anthropicWireRequest
	if err := json.Unmarshal(payload, &body); err != nil {
		return Request{}, fmt.Errorf("decode request payload: %w", err)
	}
	request := Request{
		Model:       body.Model,
		Messages:    make([]Message, 0, len(body.Messages)),
		Temperature: body.Temperature,
	}
	if body.OutputConfig != nil {
		request.Effort = body.OutputConfig.Effort
	}
	if body.Metadata != nil {
		request.CacheKey = body.Metadata.UserID
	}
	for _, block := range body.System {
		request.System += block.Text
	}
	for _, message := range body.Messages {
		request.Messages = append(request.Messages, anthropicMessageToOpenAI(message)...)
	}
	if len(body.Tools) > 0 {
		request.Tools = make([]Tool, 0, len(body.Tools))
		for _, tool := range body.Tools {
			request.Tools = append(request.Tools, Tool{
				Name:        tool.Name,
				Description: tool.Description,
				Parameters:  tool.InputSchema,
			})
		}
	}
	return request, nil
}

func anthropicMessageToOpenAI(message anthropicMessage) []Message {
	// A user message carrying tool_result blocks decodes back to one `tool`
	// message per block, which is the shape it was built from.
	var results []Message
	for _, block := range message.Content {
		if block.Type == "tool_result" {
			results = append(results, Message{Role: "tool", ToolCallID: block.ToolUseID, Content: block.Content, IsError: block.IsError})
		}
	}
	if len(results) > 0 {
		return results
	}
	out := Message{Role: message.Role}
	for _, block := range message.Content {
		switch block.Type {
		case "text":
			out.Content += block.Text
		case "thinking":
			out.ReasoningContent += block.Thinking
		case "tool_use":
			call := ToolCall{ID: block.ID, Type: "function"}
			call.Function.Name = block.Name
			call.Function.Arguments = string(block.Input)
			out.ToolCalls = append(out.ToolCalls, call)
		}
	}
	return []Message{out}
}

// classifyError parses the two envelopes this endpoint answers with: the
// Anthropic shape, and an HTTP 422 FastAPI `detail[]` body for a schema
// violation, which is neither wire's documented error shape and which a
// classifier that knew only the first would render as an empty message.
func (anthropicMessagesWire) classifyError(status int, body []byte) error {
	err := &StatusError{Status: status, Wire: WireAnthropicMessages}
	var envelope struct {
		Type  string `json:"type"`
		Error struct {
			Type    string          `json:"type"`
			Code    json.RawMessage `json:"code"`
			Message string          `json:"message"`
		} `json:"error"`
		RequestID string `json:"request_id"`
	}
	if json.Unmarshal(body, &envelope) == nil && envelope.Error.Message != "" {
		err.Code = rawString(envelope.Error.Code)
		if err.Code == "" {
			err.Code = envelope.Error.Type
		}
		err.Message = envelope.Error.Message
		err.RequestID = envelope.RequestID
		return err
	}
	var fastAPI struct {
		Detail []struct {
			Type string `json:"type"`
			Loc  []any  `json:"loc"`
			Msg  string `json:"msg"`
		} `json:"detail"`
	}
	if json.Unmarshal(body, &fastAPI) == nil && len(fastAPI.Detail) > 0 {
		parts := make([]string, 0, len(fastAPI.Detail))
		for _, detail := range fastAPI.Detail {
			where := make([]string, 0, len(detail.Loc))
			for _, item := range detail.Loc {
				where = append(where, fmt.Sprint(item))
			}
			parts = append(parts, fmt.Sprintf("%s at %s: %s", detail.Type, strings.Join(where, "."), detail.Msg))
		}
		err.Code = "validation_error"
		err.Message = strings.Join(parts, "; ")
		return err
	}
	err.Message = strings.TrimSpace(string(body))
	return err
}

// anthropicStatusOverloaded is Anthropic's own overload status. It is
// non-standard, so `net/http` has no constant for it, and it is 5xx, which is
// what makes an overload retryable under the rule below.
const anthropicStatusOverloaded = 529

// anthropicErrorStatus maps an in-stream error frame's `type` onto the HTTP
// status the same condition carries when the endpoint refuses before the
// stream opens.
//
// This exists because retry classification is a status-class decision, and an
// in-stream frame has no status of its own. Raising one with `Status: 0` made
// `StatusError.Retryable` false, so `provider.Retry` returned on the first
// attempt and a server-side transient failed the turn outright — a plain
// `fmt.Errorf` would have been retried, and the typed error opted out of the
// one behaviour retrying exists for. Mapping the type rather than making
// `Status: 0` blanket-retryable is deliberate: a blanket rule would also
// retry the in-stream 4xx equivalents, and `rate_limit_error` among them,
// which the ruling on `StatusError.Retryable` forbids.
//
// An unrecognised type defaults to 500, the **retryable** side. That matches
// what this package already does with an error carrying no classification at
// all, and it matches the nil-error fallback in the same `case "error":`
// branch, which returns a bare `fmt.Errorf` and is therefore retried. The one
// refusal class that must never be retried here is a quota refusal, and it
// arrives as `rate_limit_error`, which is mapped — so the default cannot
// swallow it.
func anthropicErrorStatus(errorType string) int {
	switch errorType {
	case "invalid_request_error":
		return http.StatusBadRequest
	case "authentication_error":
		return http.StatusUnauthorized
	case "billing_error", "permission_error":
		return http.StatusForbidden
	case "not_found_error":
		return http.StatusNotFound
	case "request_too_large":
		return http.StatusRequestEntityTooLarge
	case "rate_limit_error":
		return http.StatusTooManyRequests
	case "api_error":
		return http.StatusInternalServerError
	case "overloaded_error":
		return anthropicStatusOverloaded
	default:
		return http.StatusInternalServerError
	}
}

// anthropicCatalog is this endpoint's own catalog shape. `modelMetadata`
// cannot read it: `created_at` is an RFC3339 string where the OpenAI shape has
// a Unix integer `created`, and there is no context length on either wire, so
// this decoder exists for the /models menu and never for sizing.
type anthropicCatalog struct {
	Data []struct {
		ID          string `json:"id"`
		Type        string `json:"type"`
		DisplayName string `json:"display_name"`
		CreatedAt   string `json:"created_at"`
	} `json:"data"`
	HasMore bool `json:"hasMore"`
}

func (anthropicMessagesWire) decodeCatalog(data []byte) ([]modelMetadata, error) {
	var catalog anthropicCatalog
	if err := json.Unmarshal(data, &catalog); err != nil {
		return nil, fmt.Errorf("decode anthropic model catalog: %w", err)
	}
	models := make([]modelMetadata, 0, len(catalog.Data))
	for _, entry := range catalog.Data {
		metadata := modelMetadata{ID: entry.ID}
		if parsed, err := parseRFC3339Unix(entry.CreatedAt); err == nil {
			metadata.Created = parsed
		}
		models = append(models, metadata)
	}
	return models, nil
}

// parseStream normalizes the Messages SSE dialect.
//
// Four things here are specification rather than judgement, each measured:
// `ping` carries nothing and must not be treated as content; the terminal
// marker is `message_stop` and there is no `[DONE]`; usage arrives split
// across `message_start` and `message_delta` and must be merged rather than
// overwritten; and the block index is a content-block ordinal, so tools start
// at 1 behind the thinking block and must be renumbered before they leave
// this package.
func (anthropicMessagesWire) parseStream(body io.Reader, sink StreamSink) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 16*1024), 2*1024*1024)

	state := newAnthropicStream()
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		// `event:` lines are redundant with the `type` field inside each data
		// payload, so the payload is the single source and these are skipped.
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" {
			continue
		}
		if err := state.consume([]byte(data), sink); err != nil {
			return state.flushIncomplete(sink, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return state.flushIncomplete(sink, err)
	}
	if !state.sawStop && state.stopReason == "" {
		return state.flushIncomplete(sink, errTruncatedStream)
	}
	return state.flush(sink)
}

// anthropicStream holds the per-stream state the dialect requires: the
// block-index to tool-ordinal map, and the usage object being merged.
type anthropicStream struct {
	// toolOrdinal maps a content-block index onto a dense tool ordinal from 0.
	toolOrdinal map[int]int
	// nextOrdinal is the next dense ordinal to hand out.
	nextOrdinal int
	// toolCallID maps a content-block index onto the tool call's id, so an
	// argument fragment arriving later can still be attributed.
	toolCallID map[int]string
	// usage accumulates across message_start and message_delta.
	inputTokens     int
	outputTokens    int
	cacheReadTokens int
	sawUsage        bool
	// sawInput and sawOutput record each half of the usage separately: a
	// stream that ends without its output count reports input alone, and
	// that total is a lower bound, not a measurement.
	sawInput   bool
	sawOutput  bool
	stopReason string
	// sawStop records this wire's terminal marker, `message_stop`. It was
	// parsed into the default arm and discarded before 2026-09-14, which left a
	// truncated stream looking exactly like a complete one.
	sawStop bool
}

func newAnthropicStream() *anthropicStream {
	return &anthropicStream{toolOrdinal: map[int]int{}, toolCallID: map[int]string{}}
}

// ordinalFor renumbers a content-block index into a dense tool ordinal,
// assigned in order of first appearance. Nothing downstream may treat the wire
// index as an array position, and nothing may subtract a constant from it: the
// offset is conditional on a thinking block being emitted, which is not
// guaranteed. Measured 2026-09-14: a single-tool turn produced no thinking
// block and put tool_use at index 0, while a parallel-tool turn on the same
// endpoint put thinking at 0 and tools at 1..n. Order of appearance is correct
// under both, which is why it is used.
func (s *anthropicStream) ordinalFor(blockIndex int) int {
	if ordinal, ok := s.toolOrdinal[blockIndex]; ok {
		return ordinal
	}
	ordinal := s.nextOrdinal
	s.toolOrdinal[blockIndex] = ordinal
	s.nextOrdinal++
	return ordinal
}

func (s *anthropicStream) consume(data []byte, sink StreamSink) error {
	var frame struct {
		Type  string `json:"type"`
		Index int    `json:"index"`
		Delta struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
			Thinking    string `json:"thinking"`
			PartialJSON string `json:"partial_json"`
			StopReason  string `json:"stop_reason"`
		} `json:"delta"`
		ContentBlock struct {
			Type string `json:"type"`
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"content_block"`
		Message struct {
			Usage anthropicUsage `json:"usage"`
		} `json:"message"`
		Usage anthropicUsage `json:"usage"`
		Error *struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
		// RequestID sits beside `error`, not inside it. It is the only handle
		// Z.ai gives for a failed request, the pre-stream classifier already
		// preserves it, and the plan requires it in the error text — the
		// in-stream path was the one place it was dropped.
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(data, &frame); err != nil {
		return fmt.Errorf("decode provider event: %w", err)
	}

	switch frame.Type {
	case "ping":
		// Carries nothing. Never content.
		return nil
	case "error":
		// The capability matrix produced no in-stream error frame from either
		// endpoint, but this endpoint produced one live under overload on
		// 2026-09-14 — `overloaded_error`, mid-stream, after the stream had
		// already opened. A stream that produced it must not be read as a
		// successful end, and it must carry the same status class the same
		// condition carries when it arrives as a pre-stream refusal.
		if frame.Error != nil {
			return &StatusError{
				Status:  anthropicErrorStatus(frame.Error.Type),
				Wire:    WireAnthropicMessages,
				Code:    frame.Error.Type,
				Message: frame.Error.Message,
				// The frame's own numeric `code` is deliberately not consulted
				// for Status: the type mapping is the settled classifier, and
				// rate_limit_error is kept non-retryable there on purpose.
				RequestID: frame.RequestID,
			}
		}
		return fmt.Errorf("provider stream error")
	case "message_start":
		s.mergeUsage(frame.Message.Usage)
		return nil
	case "content_block_start":
		if frame.ContentBlock.Type == "tool_use" {
			s.toolCallID[frame.Index] = frame.ContentBlock.ID
			return sink(Event{
				Kind:       EventTool,
				ToolName:   frame.ContentBlock.Name,
				ToolCallID: frame.ContentBlock.ID,
				ToolIndex:  s.ordinalFor(frame.Index),
			})
		}
		return nil
	case "content_block_delta":
		switch frame.Delta.Type {
		case "text_delta":
			if frame.Delta.Text == "" {
				return nil
			}
			return sink(Event{Kind: EventText, Text: frame.Delta.Text})
		case "thinking_delta":
			if frame.Delta.Thinking == "" {
				return nil
			}
			return sink(Event{Kind: EventReasoning, Text: frame.Delta.Thinking})
		case "input_json_delta":
			// Arguments are fragmented on this wire and whole on the other.
			// Both are handled by accumulation downstream, so the fragment is
			// forwarded as-is under its dense ordinal.
			if frame.Delta.PartialJSON == "" {
				return nil
			}
			return sink(Event{
				Kind:       EventTool,
				ToolCallID: s.toolCallID[frame.Index],
				ToolIndex:  s.ordinalFor(frame.Index),
				Input:      frame.Delta.PartialJSON,
			})
		}
		return nil
	case "message_delta":
		s.mergeUsage(frame.Usage)
		if frame.Delta.StopReason != "" {
			s.stopReason = frame.Delta.StopReason
		}
		return nil
	case "message_stop":
		s.sawStop = true
		return nil
	}
	return nil
}

// flush emits the merged usage once, at the end.
//
// It is emitted once and not per frame because the harness *accumulates* cache
// counters on every usage event it sees, so emitting the partial object from
// message_start and the fuller one from message_delta would double-count the
// same tokens.
func (s *anthropicStream) flush(sink StreamSink) error {
	if !s.sawUsage {
		return nil
	}
	usage := s.normalizedUsage()
	if !s.sawInput || !s.sawOutput {
		usage["incomplete"] = true
	}
	return sink(Event{Kind: EventUsage, Usage: usage, StopReason: mapAnthropicStopReason(s.stopReason)})
}

// flushIncomplete reports the usage a failed stream had already received,
// marked incomplete, before returning err. A request that billed input and
// produced output before disconnecting still consumed those tokens; dropping
// its usage would make a retried request look cheaper than it was. The harness
// records an incomplete usage in the transcript but does not anchor context
// on it.
func (s *anthropicStream) flushIncomplete(sink StreamSink, err error) error {
	if s.sawUsage {
		usage := s.normalizedUsage()
		usage["incomplete"] = true
		if sinkErr := sink(Event{Kind: EventUsage, Usage: usage}); sinkErr != nil {
			return sinkErr
		}
	}
	return err
}

type anthropicUsage struct {
	InputTokens          *int `json:"input_tokens"`
	OutputTokens         *int `json:"output_tokens"`
	CacheReadInputTokens *int `json:"cache_read_input_tokens"`
}

// mergeUsage merges rather than overwrites. message_start carries the input
// side and message_delta the output side, and a later frame that omits a field
// must not zero what an earlier one reported.
func (s *anthropicStream) mergeUsage(usage anthropicUsage) {
	if usage.InputTokens != nil {
		s.inputTokens = *usage.InputTokens
		s.sawUsage = true
		s.sawInput = true
	}
	if usage.OutputTokens != nil {
		s.outputTokens = *usage.OutputTokens
		s.sawUsage = true
		s.sawOutput = true
	}
	if usage.CacheReadInputTokens != nil {
		s.cacheReadTokens = *usage.CacheReadInputTokens
		s.sawUsage = true
	}
}

// normalizedUsage renders this wire's usage in the coding wire's shape, so
// recordUsage derives cache misses in one place and nothing downstream has to
// know which wire served the request.
//
// The load-bearing line is prompt_tokens. `input_tokens` on this wire is net
// of cache and `prompt_tokens` on the other is gross: measured on one identical
// 5,550-token prefix, the coding wire reported prompt_tokens 5551 with
// cached_tokens 5504, and this wire reported input_tokens 46 with
// cache_read_input_tokens 5504. Mapping input_tokens straight across would
// tell the harness a 5,550-token context was 46 tokens, and automatic
// compaction — which fires at 70% of the window — would never fire on a cached
// conversation while everything still appeared to run.
//
// `completion_tokens_details.reasoning_tokens` is deliberately **absent**
// rather than zero. The coding wire reports it and this one reports no
// equivalent, and on a wire that can silently discard an effort setting that
// record is the only way the inertness would ever be noticed after the fact —
// so a synthesized zero would be a lie in exactly the place it matters.
func (s *anthropicStream) normalizedUsage() map[string]any {
	promptTokens := s.inputTokens + s.cacheReadTokens
	return map[string]any{
		"prompt_tokens":         promptTokens,
		"completion_tokens":     s.outputTokens,
		"total_tokens":          promptTokens + s.outputTokens,
		"prompt_tokens_details": map[string]any{"cached_tokens": s.cacheReadTokens},
	}
}

// mapAnthropicStopReason normalizes the terminal reason onto the coding wire's
// vocabulary, so a consumer reads one set of values whichever wire served it.
func mapAnthropicStopReason(reason string) string {
	switch reason {
	case "end_turn":
		return "stop"
	case "tool_use":
		return "tool_calls"
	case "":
		return ""
	default:
		return reason
	}
}
