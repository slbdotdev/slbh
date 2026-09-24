package provider

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
)

// ollamaChatWire is Ollama's native `/api/chat` protocol.
//
// It exists because the OpenAI-shaped shim on the same server is lossy in the
// one place the local model needs fidelity. Measured 2026-09-17 by a seeded
// sampler A/B (org/v8-phase0-2026-09-17.md): `/v1` forces `top_p = 1.0` over
// the GGUF's own 0.95 even when the client sends nothing, and silently discards
// `repeat_penalty`, `top_k`, `min_p` and `draft_num_predict` under HTTP 200.
// `/api/chat` honours all of them, so the route's sampler guard can only be
// carried here.
//
// Three things differ from the other two wires and are specification, each
// measured against Ollama 0.34.1 on 2026-09-18:
//
//   - The stream is NDJSON — one JSON object per line — not SSE. There is no
//     `data:` prefix and no `[DONE]`; the terminal object carries `done: true`.
//   - Tool calls arrive whole, on one chunk, with `function.arguments` as a
//     JSON object rather than a string. slbh's ToolCall carries a string, so the
//     object is forwarded as its JSON text, and history sends it back as an
//     object.
//   - Effort is the top-level `think` field, as a string. `think: "low" |
//     "medium" | "high"` renders the same prompt as `/v1` `reasoning_effort`
//     at the same level (41 / 11 / 53 prompt tokens for "hi"), `true` and an
//     omitted key render the template's default (53, xhigh), and `false` turns
//     thinking off (13). Any other string is an HTTP 400 naming the accepted set,
//     so an unmappable value is loud on this wire rather than discarded.
type ollamaChatWire struct{}

// ollamaWireRequest is the `/api/chat` body. `options` is always present,
// because `num_predict` is always sent: an unbounded local generation is the
// failure the route's maxOutputTokens exists to end. That also makes the key
// the field RequestFromPayload detects this wire by.
type ollamaWireRequest struct {
	Model    string                     `json:"model"`
	Messages []ollamaMessage            `json:"messages"`
	Tools    []wireTool                 `json:"tools,omitempty"`
	Stream   bool                       `json:"stream"`
	Think    string                     `json:"think,omitempty"`
	Options  map[string]json.RawMessage `json:"options"`
}

// ollamaMessage is one history entry. An assistant turn replays its reasoning
// as `thinking` and its calls with object arguments; a tool result is role
// `tool` and names both the tool and the call it answers.
type ollamaMessage struct {
	Role       string           `json:"role"`
	Content    string           `json:"content"`
	Thinking   string           `json:"thinking,omitempty"`
	ToolCalls  []ollamaToolCall `json:"tool_calls,omitempty"`
	ToolName   string           `json:"tool_name,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
}

type ollamaToolCall struct {
	ID       string `json:"id,omitempty"`
	Function struct {
		Index     *int            `json:"index,omitempty"`
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function"`
}

func (ollamaChatWire) name() string { return WireOllamaChat }

// applyHeaders sends no credential on the local route, which is deliberate:
// the server is on the house network and answers without one. A key, if one
// is ever configured, goes as a bearer token, which is what a proxy in front
// of Ollama would expect.
func (ollamaChatWire) applyHeaders(header http.Header, apiKey string) {
	if apiKey != "" {
		header.Set("Authorization", "Bearer "+apiKey)
	}
	header.Set("Content-Type", "application/json")
	header.Set("Accept", "application/x-ndjson")
}

func (ollamaChatWire) payload(p *HTTPProvider, req Request) ([]byte, error) {
	effort, err := p.effortValue(req.Effort)
	if err != nil {
		return nil, err
	}
	body := ollamaWireRequest{
		Model:    p.modelID(req.Model),
		Messages: ollamaMessages(req.System, req.Messages),
		Stream:   true,
		Think:    effort,
		Options:  ollamaOptions(p, req),
	}
	body.Tools = toolsToWire(req.Tools)
	return json.Marshal(body)
}

// ollamaOptions merges the route's sampler options with the two values slbh
// derives itself, in a fixed precedence: the policy's `options`, then
// `num_ctx` from contextWindow and `num_predict` from the generation bound, then
// a request's own temperature.
//
// `num_ctx` is sent because an `/api/chat` request whose window differs from
// the loaded runner's makes the server reload the model, and it is sent from
// the pin so the two can never disagree. Policy validation refuses `num_ctx`
// and `num_predict` inside `options` for the same reason: one number, one
// place.
func ollamaOptions(p *HTTPProvider, req Request) map[string]json.RawMessage {
	options := make(map[string]json.RawMessage, len(p.Route.Policy.Options)+3)
	for key, value := range p.Route.Policy.Options {
		options[key] = append(json.RawMessage(nil), value...)
	}
	if p.Route.ContextWindow > 0 {
		options["num_ctx"] = rawInt(p.Route.ContextWindow)
	}
	options["num_predict"] = rawInt(p.maxOutputTokens(req, DefaultMaxOutputTokens))
	if req.Temperature != nil {
		if encoded, err := json.Marshal(*req.Temperature); err == nil {
			options["temperature"] = encoded
		}
	}
	return options
}

func rawInt(value int) json.RawMessage { return json.RawMessage(fmt.Sprintf("%d", value)) }

// ollamaMessages converts OpenAI-shaped history. The system prompt leads as a
// system message when there is one; an empty one is not sent, because the
// template renders an empty system block as tokens.
func ollamaMessages(system string, messages []Message) []ollamaMessage {
	converted := make([]ollamaMessage, 0, len(messages)+1)
	if system != "" {
		converted = append(converted, ollamaMessage{Role: "system", Content: system})
	}
	for _, message := range messages {
		out := ollamaMessage{Role: message.Role, Content: message.Content, Thinking: message.ReasoningContent}
		switch message.Role {
		case "tool":
			out.ToolName = message.Name
			out.ToolCallID = message.ToolCallID
		case "assistant":
			for _, call := range message.ToolCalls {
				arguments := json.RawMessage(strings.TrimSpace(call.Function.Arguments))
				if len(arguments) == 0 || !json.Valid(arguments) {
					// As on the Anthropic wire: an incomplete fragment goes as an
					// empty object the model can answer, never as malformed JSON.
					arguments = json.RawMessage(`{}`)
				}
				var wire ollamaToolCall
				wire.ID = call.ID
				wire.Function.Name = call.Function.Name
				wire.Function.Arguments = arguments
				out.ToolCalls = append(out.ToolCalls, wire)
			}
		}
		converted = append(converted, out)
	}
	return converted
}

// requestFromPayload is this wire's half of the byte-replay contract.
//
// Two fields of a Request have no home on this wire and are said so rather than
// smuggled: CacheKey, because Ollama's prompt cache is automatic prefix reuse
// with no key (it reports `prompt_eval_cached_count` instead), and a request's
// own temperature, which rides `options.temperature` beside any the policy set.
// A replayed request therefore carries the effective temperature, which
// re-encodes to the same bytes.
func (ollamaChatWire) requestFromPayload(payload []byte) (Request, error) {
	var body ollamaWireRequest
	if err := json.Unmarshal(payload, &body); err != nil {
		return Request{}, fmt.Errorf("decode request payload: %w", err)
	}
	request := Request{Model: body.Model, Effort: body.Think}
	messages := body.Messages
	if len(messages) > 0 && messages[0].Role == "system" {
		request.System = messages[0].Content
		messages = messages[1:]
	}
	request.Messages = make([]Message, 0, len(messages))
	for _, message := range messages {
		out := Message{Role: message.Role, Content: message.Content, ReasoningContent: message.Thinking}
		if message.Role == "tool" {
			out.Name = message.ToolName
			out.ToolCallID = message.ToolCallID
		}
		for _, call := range message.ToolCalls {
			converted := ToolCall{ID: call.ID, Type: "function"}
			converted.Function.Name = call.Function.Name
			converted.Function.Arguments = string(call.Function.Arguments)
			out.ToolCalls = append(out.ToolCalls, converted)
		}
		request.Messages = append(request.Messages, out)
	}
	if raw, ok := body.Options["temperature"]; ok {
		var temperature float64
		if err := json.Unmarshal(raw, &temperature); err == nil {
			request.Temperature = &temperature
		}
	}
	request.Tools = toolsFromWire(body.Tools)
	return request, nil
}

// classifyError reads Ollama's envelope, `{"error": "..."}` — a bare string,
// not an object. A missing model is a 404, an invalid `think` value a 400, and
// a template exception a 500 whose string is itself a JSON document; all of
// them keep the status class that governs retry.
func (ollamaChatWire) classifyError(status int, body []byte) error {
	err := &StatusError{Status: status, Wire: WireOllamaChat}
	err.Message = ollamaErrorMessage(body)
	return err
}

func ollamaErrorMessage(body []byte) string {
	var envelope struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(body, &envelope) == nil && len(envelope.Error) > 0 {
		var asString string
		if json.Unmarshal(envelope.Error, &asString) == nil {
			return asString
		}
		var asObject struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(envelope.Error, &asObject) == nil && asObject.Message != "" {
			return asObject.Message
		}
		return strings.TrimSpace(string(envelope.Error))
	}
	return strings.TrimSpace(string(body))
}

// decodeCatalog reads `/api/tags`. The local route lists its models from the
// policy rather than fetching them (localCatalog), so this exists for a route
// that names a catalogEndpoint on this wire, and it carries no window: the tags
// document states none.
func (ollamaChatWire) decodeCatalog(data []byte) ([]modelMetadata, error) {
	var catalog struct {
		Models []struct {
			Name  string `json:"name"`
			Model string `json:"model"`
		} `json:"models"`
	}
	if err := json.Unmarshal(data, &catalog); err != nil {
		return nil, fmt.Errorf("decode ollama model catalog: %w", err)
	}
	models := make([]modelMetadata, 0, len(catalog.Models))
	for _, entry := range catalog.Models {
		id := entry.Model
		if id == "" {
			id = entry.Name
		}
		models = append(models, modelMetadata{ID: id})
	}
	return models, nil
}

// ollamaChunk is one NDJSON line.
type ollamaChunk struct {
	Message struct {
		Content   string           `json:"content"`
		Thinking  string           `json:"thinking"`
		ToolCalls []ollamaToolCall `json:"tool_calls"`
	} `json:"message"`
	Done                  bool            `json:"done"`
	DoneReason            string          `json:"done_reason"`
	PromptEvalCount       *int            `json:"prompt_eval_count"`
	PromptEvalCachedCount *int            `json:"prompt_eval_cached_count"`
	EvalCount             *int            `json:"eval_count"`
	Error                 json.RawMessage `json:"error"`
}

// parseStream normalizes the NDJSON dialect.
//
// A stream that ends without a `done: true` object was cut short and reports
// errTruncatedStream, which is retried, exactly as the SSE wires do. An
// `{"error": ...}` line mid-stream is how the server reports a failure after
// it has already answered 200; it is a plain error, and so retried, because
// the envelope carries no class to say otherwise.
func (ollamaChatWire) parseStream(body io.Reader, sink StreamSink) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 16*1024), 2*1024*1024)
	toolCalls := 0
	sawDone := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var chunk ollamaChunk
		if err := json.Unmarshal([]byte(line), &chunk); err != nil {
			return fmt.Errorf("decode provider event: %w", err)
		}
		if len(chunk.Error) > 0 && string(chunk.Error) != "null" {
			return fmt.Errorf("provider stream error on the %s wire: %s", WireOllamaChat, ollamaErrorMessage([]byte(line)))
		}
		if chunk.Message.Thinking != "" {
			if err := sink(Event{Kind: EventReasoning, Text: chunk.Message.Thinking}); err != nil {
				return err
			}
		}
		if chunk.Message.Content != "" {
			if err := sink(Event{Kind: EventText, Text: chunk.Message.Content}); err != nil {
				return err
			}
		}
		for _, call := range chunk.Message.ToolCalls {
			// The ordinal is slbh's own, in order of arrival across the whole
			// stream, as on the Anthropic wire; the wire's `function.index` is
			// not read, because nothing downstream may treat a wire index as a
			// position.
			id := call.ID
			if id == "" {
				id = fmt.Sprintf("call_%d", toolCalls)
			}
			arguments := strings.TrimSpace(string(call.Function.Arguments))
			if arguments == "" || arguments == "null" {
				arguments = "{}"
			}
			if err := sink(Event{Kind: EventTool, ToolName: call.Function.Name, ToolCallID: id, ToolIndex: toolCalls, Input: arguments}); err != nil {
				return err
			}
			toolCalls++
		}
		if chunk.Done {
			sawDone = true
			if err := sink(Event{Kind: EventUsage, Usage: ollamaUsage(chunk), StopReason: mapOllamaDoneReason(chunk.DoneReason, toolCalls > 0)}); err != nil {
				return err
			}
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if !sawDone {
		return errTruncatedStream
	}
	return nil
}

// ollamaUsage renders the terminal counts in the coding wire's shape.
// `prompt_eval_count` is gross — measured, a 363-token prompt reported 363 with
// 301 of them in `prompt_eval_cached_count` — so it maps straight onto
// prompt_tokens, and the cached count onto the field recordUsage reads.
func ollamaUsage(chunk ollamaChunk) map[string]any {
	prompt, completion := 0, 0
	if chunk.PromptEvalCount != nil {
		prompt = *chunk.PromptEvalCount
	}
	if chunk.EvalCount != nil {
		completion = *chunk.EvalCount
	}
	usage := map[string]any{
		"prompt_tokens":     prompt,
		"completion_tokens": completion,
		"total_tokens":      prompt + completion,
	}
	if chunk.PromptEvalCachedCount != nil {
		usage["prompt_tokens_details"] = map[string]any{"cached_tokens": *chunk.PromptEvalCachedCount}
	}
	return usage
}

// mapOllamaDoneReason normalizes onto the coding wire's vocabulary. Ollama
// reports `stop` for a turn that ended in tool calls — measured: a native tool
// call arrived with `done_reason: "stop"` — so the calls decide it, as they do
// on the other wires. `length` and anything else pass through unchanged, so an
// unfamiliar reason is visible rather than folded into success.
func mapOllamaDoneReason(reason string, madeToolCalls bool) string {
	if madeToolCalls && (reason == "stop" || reason == "") {
		return "tool_calls"
	}
	return reason
}

// ollamaSamplerOptions are the `options` keys a route may carry, with whether
// each must be an integer. It is the sampler set and nothing else.
//
// Runner options — `num_gpu`, `num_batch`, `num_thread`, `use_mmap`,
// `draft_num_predict` — are deliberately absent: they are properties of the
// loaded model, a request that changes one reloads it for every client of the
// shared GPU, and they belong in the tag's Modelfile. `num_ctx` and
// `num_predict` are absent because slbh derives them from contextWindow and
// maxOutputTokens.
var ollamaSamplerOptions = map[string]bool{
	"temperature":       false,
	"top_k":             true,
	"top_p":             false,
	"min_p":             false,
	"typical_p":         false,
	"repeat_penalty":    false,
	"repeat_last_n":     true,
	"presence_penalty":  false,
	"frequency_penalty": false,
	"seed":              true,
	"stop":              false,
}

// validateOllamaOptions checks a route's `options` block. Unknown keys refuse,
// by name, for the same reason an unknown effort spelling does: Ollama
// ignores an option it does not know, so a typo would be a guard that is
// written down and never applied.
func validateOllamaOptions(key string, options map[string]json.RawMessage) error {
	for _, name := range sortedOptionNames(options) {
		raw := options[name]
		switch name {
		case "num_ctx":
			return fmt.Errorf("route %q sets options.num_ctx; the window is contextWindow and slbh sends it as num_ctx, so it is stated once", key)
		case "num_predict":
			return fmt.Errorf("route %q sets options.num_predict; the generation bound is maxOutputTokens and slbh sends it as num_predict, so it is stated once", key)
		}
		integer, known := ollamaSamplerOptions[name]
		if !known {
			return fmt.Errorf("route %q sets options.%s, which is not a sampler option slbh sends (%s); Ollama ignores an option it does not know, and a runner option would reload the shared model",
				key, name, strings.Join(sortedOptionNames(ollamaSamplerOptions), ", "))
		}
		if name == "stop" {
			var stops []string
			if err := json.Unmarshal(raw, &stops); err != nil {
				return fmt.Errorf("route %q options.stop must be an array of strings", key)
			}
			continue
		}
		var value float64
		if err := json.Unmarshal(raw, &value); err != nil {
			return fmt.Errorf("route %q options.%s must be a number", key, name)
		}
		if integer && value != float64(int64(value)) {
			return fmt.Errorf("route %q options.%s must be an integer", key, name)
		}
		if value < 0 && name != "seed" && name != "repeat_last_n" {
			return fmt.Errorf("route %q options.%s is negative", key, name)
		}
		if (name == "top_p" || name == "min_p" || name == "typical_p") && value > 1 {
			return fmt.Errorf("route %q options.%s is above 1, which is not a probability", key, name)
		}
	}
	return nil
}

func sortedOptionNames[V any](options map[string]V) []string {
	names := make([]string, 0, len(options))
	for name := range options {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
