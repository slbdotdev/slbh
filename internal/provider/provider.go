package provider

import (
	"context"
	"strings"
)

type Message struct {
	Role string `json:"role"`
	// Content must remain present even for assistant messages that contain
	// only tool_calls. DeepSeek rejects those messages when content is omitted.
	Content string `json:"content"`
	// ReasoningContent is returned by thinking models and must be replayed on
	// assistant continuations, especially when the response contains tools.
	ReasoningContent string     `json:"reasoning_content,omitempty"`
	Name             string     `json:"name,omitempty"`
	ToolCallID       string     `json:"tool_call_id,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
	// IsError marks a tool result as a failure. Only the Anthropic wire
	// carries it (tool_result is_error); it never reaches the OpenAI-shaped
	// JSON, which has no such field.
	IsError bool `json:"-"`
}

type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

type Request struct {
	Model       string
	Effort      string
	System      string
	Messages    []Message
	Tools       []Tool
	CacheKey    string
	Temperature *float64
	// MaxTokens bounds this one generation, nil to take the route's policy
	// or the wire default. A caller sets it only to override those.
	MaxTokens *int
}

// RequestPayloadProvider exposes the exact JSON payload that a provider will
// send for an inference request. The harness uses this to make transcript
// request records byte-replayable without recording credentials or headers.
type RequestPayloadProvider interface {
	RequestPayload(Request) ([]byte, error)
}

type wireRequest struct {
	Model            string        `json:"model"`
	Stream           bool          `json:"stream"`
	Messages         []Message     `json:"messages"`
	ReasoningEffort  string        `json:"reasoning_effort,omitempty"`
	IncludeReasoning bool          `json:"include_reasoning,omitempty"`
	PromptCacheKey   string        `json:"prompt_cache_key"`
	MaxTokens        int           `json:"max_tokens,omitempty"`
	Temperature      *float64      `json:"temperature,omitempty"`
	StreamOptions    streamOptions `json:"stream_options"`
	Tools            []wireTool    `json:"tools,omitempty"`
	// Provider is OpenRouter's routing object, and it is populated for the
	// openrouter route and for no other. It is what decides which upstream
	// endpoint serves a request and on what terms, and since the pi-run
	// preflight was retired on 2026-09-10 it is the entire enforcement of that
	// posture — nothing checks it before a launch any more, so it holds
	// because it is in the body that goes out.
	Provider *wireProviderObject `json:"provider,omitempty"`
}

// wireProviderObject is the `provider` field OpenRouter reads. Its shape is
// OpenRouter's, not ours, so the JSON names are theirs.
type wireProviderObject struct {
	ZDR            bool          `json:"zdr"`
	DataCollection string        `json:"data_collection"`
	Sort           string        `json:"sort,omitempty"`
	Only           []string      `json:"only,omitempty"`
	AllowFallbacks *bool         `json:"allow_fallbacks,omitempty"`
	Ignore         []string      `json:"ignore,omitempty"`
	MaxPrice       *wireMaxPrice `json:"max_price,omitempty"`
}

type wireMaxPrice struct {
	Prompt     float64 `json:"prompt"`
	Completion float64 `json:"completion"`
}

// providerObjectFor renders a route's posture into the wire object, and
// returns nil for every route that is not the OpenRouter one.
//
// `zdr` and `data_collection` are OpenRouter concepts and say nothing about
// the Z.ai plan endpoint or a server on the house network, so sending them
// there would be noise at best and a false record of a guarantee at worst.
func providerObjectFor(route Route) *wireProviderObject {
	if route.Flavor != "openrouter" || route.Policy.Provider == nil {
		return nil
	}
	posture := route.Policy.Provider
	object := &wireProviderObject{
		DataCollection: posture.DataCollection,
		Sort:           posture.Sort,
	}
	if posture.ZDR != nil {
		object.ZDR = *posture.ZDR
	}
	if len(posture.Only) > 0 {
		object.Only = append([]string(nil), posture.Only...)
	}
	if posture.AllowFallbacks != nil {
		allow := *posture.AllowFallbacks
		object.AllowFallbacks = &allow
	}
	if len(posture.Ignore) > 0 {
		object.Ignore = append([]string(nil), posture.Ignore...)
	}
	if posture.MaxPrice != nil {
		object.MaxPrice = &wireMaxPrice{Prompt: posture.MaxPrice.Prompt, Completion: posture.MaxPrice.Completion}
	}
	return object
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type wireTool struct {
	Type     string           `json:"type"`
	Function wireToolFunction `json:"function"`
}

type wireToolFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// toolsToWire and toolsFromWire convert between Tool and the function-tool shape
// the OpenAI-compatible and Ollama wires share. An empty list converts to nil,
// so an absent `tools` field stays absent in both directions.
func toolsToWire(tools []Tool) []wireTool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]wireTool, 0, len(tools))
	for _, tool := range tools {
		out = append(out, wireTool{
			Type: "function",
			Function: wireToolFunction{
				Name:        tool.Name,
				Description: tool.Description,
				Parameters:  tool.Parameters,
			},
		})
	}
	return out
}

func toolsFromWire(tools []wireTool) []Tool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]Tool, 0, len(tools))
	for _, tool := range tools {
		out = append(out, Tool{
			Name:        tool.Function.Name,
			Description: tool.Function.Description,
			Parameters:  tool.Function.Parameters,
		})
	}
	return out
}

type EventKind string

const (
	EventText      EventKind = "text"
	EventReasoning EventKind = "reasoning"
	EventTool      EventKind = "tool"
	EventUsage     EventKind = "usage"
	EventError     EventKind = "error"
	EventDone      EventKind = "done"
)

type Event struct {
	Kind       EventKind
	Text       string
	ToolName   string
	ToolCallID string
	// ToolIndex is a dense tool ordinal from 0, normalized inside this
	// package. It is never the wire's own index: on the Anthropic wire that is
	// a content-block position offset by the thinking block, so tools start at
	// 1 there. Nothing downstream may treat it as an array position.
	ToolIndex int
	Input     string
	Usage     map[string]any
	// StopReason is the terminal reason in the coding wire's vocabulary —
	// `stop`, `tool_calls` — whichever wire served the request. It rides the
	// usage event, which is the last thing a stream emits.
	StopReason string
	Err        error
}

// IsOutputLimitStop reports whether a terminal reason means the generation hit
// its output bound rather than ending. `length` is the coding wire's and
// Ollama's spelling; `max_tokens` is the Anthropic wire's, which passes through
// unmapped. A generation that ends this way was cut off, and a caller that
// treated it as finished would commit a partial answer as a whole one.
func IsOutputLimitStop(reason string) bool {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "length", "max_tokens", "max_output_tokens", "model_length":
		return true
	default:
		return false
	}
}

type StreamSink func(Event) error

type Provider interface {
	Stream(context.Context, Request, StreamSink) error
}

// ContextWindowProvider is optional because not every OpenAI-compatible API
// exposes model metadata. Callers must use FallbackContextWindow when a
// provider does not implement this capability or returns no usable value.
type ContextWindowProvider interface {
	ContextWindow(context.Context, string) (int, error)
}

const FallbackContextWindow = 128000

// DefaultMaxOutputTokens bounds one generation on the OpenAI-shaped wire when
// the route's policy does not set its own.
//
// It is deliberately not the Anthropic wire's ceiling. That wire sends
// `anthropicMaxTokens` because the Messages API *requires* the field, so its
// value was chosen to mean "effectively no limit" and to leave behaviour
// unchanged. This one exists to *be* a limit, so it is sized to sit well above
// any single turn this fleet has produced while still capping a generation
// that will not stop. The two differ because they are for different things.
const DefaultMaxOutputTokens = 32768

// OpenRouterEndpoint is the stock chat-completions endpoint, and the default
// value of SLBH_ENDPOINT. It lives here rather than in config so the policy
// author and the route resolver cannot disagree about the URL.
const OpenRouterEndpoint = "https://openrouter.ai/api/v1/chat/completions"

// openRouterEndpoint is the unexported spelling used inside this package.
const openRouterEndpoint = OpenRouterEndpoint
