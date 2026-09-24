package provider

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

func (p *HTTPProvider) Stream(ctx context.Context, req Request, sink StreamSink) error {
	if !CredentialFreeFlavor(p.Flavor) && p.APIKey == "" {
		return fmt.Errorf("provider API key is not configured (set OPENROUTER_API_KEY or ZAI_API_KEY)")
	}
	// OpenRouter accepts prompt_cache_key; native providers safely ignore the
	// extra metadata in their compatible endpoint. The prefix itself is kept
	// stable by Runtime and is never mixed with user turns.
	payload, err := p.RequestPayload(req)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, p.Endpoint, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	// Headers belong to the wire strategy, not to the transport: the Anthropic
	// shape needs x-api-key and a version header where the OpenAI one needs a
	// bearer token, and a seam that only owned payload and parsing could not
	// express that.
	p.strategy().applyHeaders(request.Header, p.APIKey)
	resp, err := p.Client.Do(request)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 32*1024))
		return p.strategy().classifyError(resp.StatusCode, data)
	}
	if err := p.strategy().parseStream(resp.Body, sink); err != nil {
		return err
	}
	return sink(Event{Kind: EventDone})
}

func StablePrefixKey(req Request) string {
	// Hash the complete stable prefix, including tool descriptions. User turns
	// are deliberately absent so the key remains reusable across a session.
	encoded, _ := json.Marshal(struct {
		Model  string `json:"model"`
		System string `json:"system"`
		Tools  []Tool `json:"tools"`
	}{Model: req.Model, System: req.System, Tools: req.Tools})
	sum := sha256.Sum256(encoded)
	// Half the digest, so the key is 37 characters. OpenAI's API caps
	// prompt_cache_key at 64 and NInfer enforces the cap with a 400; the full
	// digest made 69, which OpenRouter and Z.ai tolerated and a rented
	// ninfer-serve refused on the first request. 128 bits still separates
	// every prefix one host will ever send.
	return "slbh-" + hex.EncodeToString(sum[:16])
}

// errTruncatedStream reports a response that ended before its wire's terminal
// marker and without a finish reason. It deliberately carries no status, so
// Retry's rule that "an error that does not classify itself is still retried"
// applies: a connection cut mid-response is exactly the transport failure
// retrying exists for.
var errTruncatedStream = errors.New("provider stream ended before its terminal marker and without a finish reason: the response was truncated")

func parseSSE(reader io.Reader, sink StreamSink) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 16*1024), 2*1024*1024)
	// The terminal reason is carried on the usage event so both wires report it
	// the same way. On this wire it arrives on the last content chunk, ahead of
	// the usage-only chunk that follows it.
	stopReason := ""
	// A stream that ends with neither `[DONE]` nor a finish reason was cut
	// short. Without this the transport closing cleanly mid-response is
	// indistinguishable from a complete turn, and the harness commits partial
	// content — or executes a partial tool-call batch — as a successful round.
	// Either marker suffices: an endpoint that drops `[DONE]` after delivering
	// a finish reason has still delivered the whole message.
	sawTerminal := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			sawTerminal = true
			continue
		}
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
			Usage map[string]any `json:"usage"`
			Error *struct {
				Message string `json:"message"`
				Type    string `json:"type"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return fmt.Errorf("decode provider event: %w", err)
		}
		if chunk.Error != nil {
			// Parity with the Anthropic arm. An unclassified error is retried,
			// which is right for a transient but wrong for a rate limit — and
			// this wire had no classification at all, so an in-stream
			// rate_limit_error would have been retried against the ruling that
			// keeps it non-retryable. Where the endpoint names a type, it is
			// mapped the same way; where it does not, the plain error stands
			// and today's behaviour is unchanged.
			if kind := strings.TrimSpace(chunk.Error.Type); kind != "" {
				return &StatusError{
					Status:  anthropicErrorStatus(kind),
					Wire:    WireOpenAIChat,
					Code:    kind,
					Message: chunk.Error.Message,
				}
			}
			return fmt.Errorf("provider stream error: %s", chunk.Error.Message)
		}
		for _, choice := range chunk.Choices {
			if choice.FinishReason != "" {
				stopReason = choice.FinishReason
			}
			if choice.Delta.Content != "" {
				if err := sink(Event{Kind: EventText, Text: choice.Delta.Content}); err != nil {
					return err
				}
			}
			reasoning := choice.Delta.ReasoningContent
			if reasoning == "" {
				reasoning = choice.Delta.Reasoning
			}
			if reasoning != "" {
				if err := sink(Event{Kind: EventReasoning, Text: reasoning}); err != nil {
					return err
				}
			}
			for _, call := range choice.Delta.ToolCalls {
				if err := sink(Event{Kind: EventTool, ToolName: call.Function.Name, ToolCallID: call.ID, ToolIndex: call.Index, Input: call.Function.Arguments}); err != nil {
					return err
				}
			}
		}
		if len(chunk.Usage) > 0 {
			if err := sink(Event{Kind: EventUsage, Usage: chunk.Usage, StopReason: stopReason}); err != nil {
				return err
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if !sawTerminal && stopReason == "" {
		return errTruncatedStream
	}
	return nil
}

// Retry executes an operation with short stepped delays. It intentionally
// leaves cancellation to the caller and never retries context cancellation.
//
// It used to retry every stream error three times. A refusal on these
// endpoints is a status-class decision instead: a 4xx will say the same thing
// three times, and retrying it wastes quota and delays the report. An error
// that does not classify itself is still retried, which keeps transport
// failures — the case retrying exists for — behaving as before.
//
// Most refusals are an HTTP status before the stream opens, which is all the
// capability matrix produced. An in-stream `error` frame was observed live
// under overload on 2026-09-14, so the Anthropic wire maps that frame's type
// onto the status its pre-stream equivalent would have carried and both halves
// classify under the one rule.
func Retry(ctx context.Context, attempts int, fn func() error) error {
	if attempts < 1 {
		attempts = 1
	}
	var err error
	for attempt := 0; attempt < attempts; attempt++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		err = fn()
		if err == nil {
			return nil
		}
		if classified, ok := err.(retryable); ok && !classified.Retryable() {
			return err
		}
		if attempt+1 < attempts {
			delay := time.Duration(attempt+1) * 300 * time.Millisecond
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
	}
	return err
}
