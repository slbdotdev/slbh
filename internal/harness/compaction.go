package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/slbdotdev/slbh/internal/provider"
	"github.com/slbdotdev/slbh/internal/seam"
)

// Compaction follows pi's default (pi-coding-agent 0.87.0,
// dist/core/compaction): keep the most recent work verbatim, about
// compactKeepTokens of it, and replace everything before it with a structured
// summary written by the agent's own model. Nothing is pinned. The first
// request is summarized with the rest, into the summary's Goal, so what
// survives is weighted to the work in progress rather than to how it began.
//
// A later compaction updates the previous summary in place instead of
// summarizing a summary. If the summary cannot be had -- the provider fails,
// the generation is cut off, the model reaches for a tool -- the old
// behaviour stands in: the prefix is dropped behind a marker. A provider
// outage must never block the agent on compaction.
const (
	// compactKeepTokens is pi's keepRecentTokens. A window smaller than four
	// times it keeps a quarter of the window instead, so a 48k local route
	// does not spend half its context on the verbatim tail.
	compactKeepTokens = 20000
	// compactSummaryMaxTokens is pi's summary bound: 0.8 of its 16,384-token
	// reserve.
	compactSummaryMaxTokens = 13107
	// compactResultMaxChars bounds each tool result, and each thinking block,
	// in the text the summarizer reads. pi bounds tool results alone; slbh
	// replays reasoning on every assistant message, so it is bounded too.
	compactResultMaxChars = 2000
)

const (
	compactSummaryPrefix = "The conversation history before this point was compacted into the following summary:\n\n<summary>\n"
	compactSummarySuffix = "\n</summary>"
)

const compactSystemPrompt = `You are a context summarization assistant. Your task is to read a conversation between a user and an AI assistant, then produce a structured summary following the exact format specified.

Do NOT continue the conversation. Do NOT respond to any questions in the conversation. ONLY output the structured summary.`

const compactPrompt = `The messages above are a conversation to summarize. Create a structured context checkpoint summary that another LLM will use to continue the work.

Use this EXACT format:

## Goal
[What is the user trying to accomplish? Can be multiple items if the session covers different tasks.]

## Constraints & Preferences
- [Any constraints, preferences, or requirements mentioned by user]
- [Or "(none)" if none were mentioned]

## Progress
### Done
- [x] [Completed tasks/changes]

### In Progress
- [ ] [Current work]

### Blocked
- [Issues preventing progress, if any]

## Key Decisions
- **[Decision]**: [Brief rationale]

## Next Steps
1. [Ordered list of what should happen next]

## Critical Context
- [Any data, examples, or references needed to continue]
- [Or "(none)" if not applicable]

Keep each section concise. Preserve exact file paths, function names, and error messages.`

const compactUpdatePrompt = `The messages above are NEW conversation messages to incorporate into the existing summary provided in <previous-summary> tags.

Update the existing structured summary with new information. RULES:
- PRESERVE all existing information from the previous summary
- ADD new progress, decisions, and context from the new messages
- UPDATE the Progress section: move items from "In Progress" to "Done" when completed
- UPDATE "Next Steps" based on what was accomplished
- PRESERVE exact file paths, function names, and error messages
- If something is no longer relevant, you may remove it

Use this EXACT format:

## Goal
[Preserve existing goals, add new ones if the task expanded]

## Constraints & Preferences
- [Preserve existing, add new ones discovered]

## Progress
### Done
- [x] [Include previously done items AND newly completed items]

### In Progress
- [ ] [Current work - update based on progress]

### Blocked
- [Current blockers - remove if resolved]

## Key Decisions
- **[Decision]**: [Brief rationale] (preserve all previous, add new)

## Next Steps
1. [Update based on current state]

## Critical Context
- [Preserve important context, add new if needed]

Keep each section concise. Preserve exact file paths, function names, and error messages.`

// compactKeepBudget is the verbatim tail for a window.
func compactKeepBudget(contextWindow int) int {
	if contextWindow <= 0 {
		contextWindow = provider.FallbackContextWindow
	}
	return min(compactKeepTokens, contextWindow/4)
}

// previousSummary returns the summary a history opens with, if it opens with
// one.
func previousSummary(history []provider.Message) (string, bool) {
	if len(history) == 0 || history[0].Role != "user" {
		return "", false
	}
	text, ok := strings.CutPrefix(history[0].Content, compactSummaryPrefix)
	if !ok {
		return "", false
	}
	text, ok = strings.CutSuffix(text, compactSummarySuffix)
	return text, ok
}

func messageTokens(message provider.Message) int {
	encoded, err := json.Marshal(message)
	if err != nil {
		return 0
	}
	return (len(encoded) + 3) / 4
}

// compactionCut returns the index of the first message kept verbatim, or 0
// when there is nothing to compact. keepMessages > 0 keeps that many
// messages, as /compact's keep field asks; otherwise the tail is keepTokens
// of estimated tokens.
//
// A cut falls on a user or an assistant message, never on a tool result, so
// every retained result keeps the call that asked for it. That lets a long
// turn -- one request, then dozens of call/result pairs -- be cut inside
// itself, which the old rule (walk back to a user message) could not do. The
// last message is always kept, and so is a summary the history opens with,
// which is updated rather than cut.
func compactionCut(history []provider.Message, keepTokens, keepMessages int) int {
	first := 0
	if _, ok := previousSummary(history); ok {
		first = 1
	}
	if len(history)-first < 2 {
		return 0
	}
	candidate := len(history) - 1
	if keepMessages > 0 {
		candidate = max(len(history)-keepMessages, first)
	} else {
		total := 0
		for i := len(history) - 1; i >= first; i-- {
			total += messageTokens(history[i])
			candidate = i
			if total >= keepTokens {
				break
			}
		}
	}
	valid := func(i int) bool { return history[i].Role != "tool" }
	cut := -1
	for i := candidate; i < len(history); i++ {
		if valid(i) {
			cut = i
			break
		}
	}
	if cut < 0 {
		for i := candidate - 1; i > first; i-- {
			if valid(i) {
				cut = i
				break
			}
		}
	}
	if cut <= first {
		return 0
	}
	return cut
}

func truncateForSummary(text string) string {
	if len(text) <= compactResultMaxChars {
		return text
	}
	return strings.ToValidUTF8(text[:compactResultMaxChars], "") + fmt.Sprintf("\n\n[... %d more characters truncated]", len(text)-compactResultMaxChars)
}

// serializeForSummary renders messages as text, so the summarizer reads a
// conversation instead of continuing one. It is pi's serializeConversation.
// A batch of tool calls is stored as one assistant message per call, each
// carrying the batch's reasoning, so a repeated thinking block is written
// once.
func serializeForSummary(messages []provider.Message) string {
	var parts []string
	lastThinking := ""
	for _, message := range messages {
		switch message.Role {
		case "user":
			if message.Content != "" {
				parts = append(parts, "[User]: "+message.Content)
			}
		case "assistant":
			if message.ReasoningContent != "" && message.ReasoningContent != lastThinking {
				parts = append(parts, "[Assistant thinking]: "+truncateForSummary(message.ReasoningContent))
			}
			lastThinking = message.ReasoningContent
			if message.Content != "" {
				parts = append(parts, "[Assistant]: "+message.Content)
			}
			if len(message.ToolCalls) > 0 {
				calls := make([]string, 0, len(message.ToolCalls))
				for _, call := range message.ToolCalls {
					calls = append(calls, call.Function.Name+"("+call.Function.Arguments+")")
				}
				parts = append(parts, "[Assistant tool calls]: "+strings.Join(calls, "; "))
			}
		case "tool":
			if message.Content != "" {
				parts = append(parts, "[Tool result]: "+truncateForSummary(message.Content))
			}
		}
	}
	return strings.Join(parts, "\n\n")
}

// summaryRequest builds the one request that writes or updates a summary. It
// carries no tools, and its system prompt is the summarizer's, not the agent's.
func summaryRequest(model, effort string, messages []provider.Message, previous string, hasPrevious bool) provider.Request {
	prompt := "<conversation>\n" + serializeForSummary(messages) + "\n</conversation>\n\n"
	if hasPrevious {
		prompt += "<previous-summary>\n" + previous + "\n</previous-summary>\n\n" + compactUpdatePrompt
	} else {
		prompt += compactPrompt
	}
	maxTokens := compactSummaryMaxTokens
	return provider.Request{Model: model, Effort: effort, System: compactSystemPrompt, Messages: []provider.Message{{Role: "user", Content: prompt}}, MaxTokens: &maxTokens}
}

// summaryRefusal is a failure the same request would repeat, so it is not
// retried: provider.Retry stops on an error whose Retryable reports false.
type summaryRefusal struct{ error }

func (summaryRefusal) Retryable() bool { return false }

var errSummaryToolCall = summaryRefusal{errors.New("the summarizer called a tool")}

// summarize runs the summary request on the agent's own model and route. Its
// request and usage are recorded like any other, marked as compaction, so
// token accounting sees what compaction costs; its usage never becomes the
// agent's context reading, which measures the conversation, not this.
func (a *Agent) summarize(ctx context.Context, p provider.Provider, req provider.Request) (string, error) {
	var text strings.Builder
	attempt := 0
	err := provider.Retry(ctx, 3, func() (streamErr error) {
		text.Reset()
		attempt++
		a.runtime.recordRequest(a, map[string]any{"purpose": "compaction", "attempt": attempt}, req, p)
		defer func() {
			if streamErr != nil && ctx.Err() == nil {
				a.runtime.emit(seam.Event{AgentID: a.ID, AgentTitle: a.Title, Kind: "request_error", Text: streamErr.Error(), Metadata: map[string]any{"purpose": "compaction", "attempt": attempt}})
			}
			if streamErr == nil {
				a.runtime.emit(seam.Event{AgentID: a.ID, AgentTitle: a.Title, Kind: "request_done", Metadata: map[string]any{"purpose": "compaction", "attempt": attempt}})
			}
		}()
		var stopReason string
		streamErr = p.Stream(ctx, req, func(event provider.Event) error {
			switch event.Kind {
			case provider.EventText:
				text.WriteString(event.Text)
			case provider.EventTool:
				return errSummaryToolCall
			case provider.EventUsage:
				stopReason = event.StopReason
				usage := make(map[string]any, len(event.Usage)+1)
				for key, value := range event.Usage {
					usage[key] = value
				}
				usage["purpose"] = "compaction"
				a.runtime.emit(seam.Event{AgentID: a.ID, AgentTitle: a.Title, Kind: "usage", Metadata: usage})
			}
			return nil
		})
		if streamErr == nil && provider.IsOutputLimitStop(stopReason) {
			// A partial summary is worse than none: it reads as complete.
			streamErr = summaryRefusal{fmt.Errorf("summary cut off at its output bound (%s)", stopReason)}
		}
		return streamErr
	})
	if err != nil {
		return "", err
	}
	summary := strings.TrimSpace(text.String())
	if summary == "" {
		return "", errors.New("the summarizer returned no text")
	}
	return summary, nil
}

// compacted replaces history[:cut] with a summary, or when the summary cannot
// be had, with the old marker. It returns the new history and the number of
// messages replaced, or the history unchanged and 0 when ctx ended first.
func (a *Agent) compacted(ctx context.Context, p provider.Provider, model, effort string, history []provider.Message, cut int) ([]provider.Message, int) {
	previous, hasPrevious := previousSummary(history)
	first := 0
	if hasPrevious {
		first = 1
	}
	replaced := cut - first
	recent := history[cut:]
	defer a.beginCompacting(replaced)()
	summary, err := a.summarize(ctx, p, summaryRequest(model, effort, history[first:cut], previous, hasPrevious))
	if ctx.Err() != nil {
		return history, 0
	}
	if err != nil {
		a.runtime.emit(seam.Event{AgentID: a.ID, AgentTitle: a.Title, Kind: "warning", Text: "compaction summary failed; dropping the earlier messages instead: " + err.Error(), Metadata: map[string]any{"purpose": "compaction"}})
		kept := append([]provider.Message(nil), history[:first]...)
		kept = append(kept, provider.Message{Role: "user", Content: fmt.Sprintf("[compacted %d earlier messages; preserve their conclusions]", replaced)})
		a.runtime.emit(seam.Event{AgentID: a.ID, AgentTitle: a.Title, Kind: "compact", Text: fmt.Sprintf("compacted %d earlier messages", replaced), Metadata: map[string]any{"mode": "drop", "replaced": replaced}})
		return append(kept, recent...), replaced
	}
	a.runtime.emit(seam.Event{AgentID: a.ID, AgentTitle: a.Title, Kind: "compact", Text: summary, Metadata: map[string]any{"mode": "summary", "replaced": replaced, "updated": hasPrevious}})
	message := provider.Message{Role: "user", Content: compactSummaryPrefix + summary + compactSummarySuffix}
	return append([]provider.Message{message}, recent...), replaced
}

// beginCompacting shows that a summary is being written, which on a long
// history takes as long as a turn and otherwise looks like a stalled agent.
// The returned func restores the prior status unless something else, such as
// a turn that started during /compact, has set one since.
func (a *Agent) beginCompacting(replaced int) func() {
	a.mu.Lock()
	prior := a.status
	a.status = "compacting"
	a.mu.Unlock()
	a.runtime.emit(seam.Event{AgentID: a.ID, AgentTitle: a.Title, Kind: "status", Text: "compacting"})
	a.runtime.emit(seam.Event{AgentID: a.ID, AgentTitle: a.Title, Kind: "compacting", Text: fmt.Sprintf("compacting %d earlier messages", replaced), Metadata: map[string]any{"purpose": "compaction", "replaced": replaced}})
	return func() {
		a.mu.Lock()
		restore := a.status == "compacting"
		if restore {
			a.status = prior
		}
		a.mu.Unlock()
		if restore {
			a.runtime.emit(seam.Event{AgentID: a.ID, AgentTitle: a.Title, Kind: "status", Text: prior})
		}
	}
}

// compactHistoryIfNeeded is the turn loop's compaction: at 70% of the window,
// summarize all but the recent tail.
func (a *Agent) compactHistoryIfNeeded(ctx context.Context, p provider.Provider, model, effort string, history []provider.Message, contextWindow int, system string, tools []provider.Tool) []provider.Message {
	a.mu.RLock()
	anchor := a.contextAnchor
	a.mu.RUnlock()
	if !contextLimitReached(contextWindow, system, history, tools, anchor) {
		return history
	}
	cut := compactionCut(history, compactKeepBudget(contextWindow), 0)
	if cut == 0 {
		return history
	}
	compacted, _ := a.compacted(ctx, p, model, effort, history, cut)
	return compacted
}

// Compact is /compact. keep > 0 keeps that many recent messages verbatim;
// keep <= 0 keeps the same token tail automatic compaction does. It returns
// the number of messages the summary (or the fallback marker) replaced.
//
// A busy agent is refused: its turn works on its own copy of the history and
// would write it back over the compaction when it ends, and it compacts
// itself when it needs to.
func (a *Agent) Compact(keep int) (int, error) {
	if codex := a.codexBackend(); codex != nil {
		codex.compact()
		return 0, nil
	}
	if claude := a.claudeBackend(); claude != nil {
		claude.compact()
		return 0, nil
	}
	a.mu.RLock()
	busy, epoch := a.busy, a.historyEpoch
	history := append([]provider.Message(nil), a.history...)
	model, effort, window := a.Model, a.Effort, a.contextWindow
	a.mu.RUnlock()
	if busy {
		return 0, errors.New("agent is mid-turn; it compacts itself at 70% of its context window")
	}
	cut := compactionCut(history, compactKeepBudget(window), keep)
	if cut == 0 {
		return 0, nil
	}
	if strings.TrimSpace(model) == "" {
		return 0, errors.New("no model selected to write the summary")
	}
	p, err := a.runtime.provider(model)
	if err != nil {
		return 0, err
	}
	compacted, replaced := a.compacted(a.runtime.ctx, p, model, effort, history, cut)
	if replaced == 0 {
		return 0, a.runtime.ctx.Err()
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	// The summary call takes time. Apply it only over the history it read;
	// anything appended since is carried after it.
	if a.historyEpoch != epoch || a.busy || len(a.history) < len(history) || !reflect.DeepEqual(a.history[:len(history)], history) {
		err := errors.New("history changed while the summary was written; compaction not applied")
		a.runtime.emit(seam.Event{AgentID: a.ID, AgentTitle: a.Title, Kind: "warning", Text: err.Error(), Metadata: map[string]any{"purpose": "compaction"}})
		return 0, err
	}
	a.history = append(compacted, a.history[len(history):]...)
	return replaced, nil
}
