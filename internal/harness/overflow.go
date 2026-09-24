package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/slbdotdev/slbh/internal/provider"
	"github.com/slbdotdev/slbh/internal/seam"
)

// A request the provider refuses as over its context window is not retried
// unchanged: the history that caused it is kept, so every later turn would be
// refused the same way and the agent could never recover. The 70% check does
// not prevent this, because it estimates at four bytes per token and dense
// output such as numbered rows runs at about 1.2, so one result that passed
// the command cap can be larger than a 64k window by itself.
//
// Recovery escalates, one retry per stage, and resets after any request that
// succeeds:
//
//  1. cut oversized tool results to their first and last lines, the cut an
//     over-limit command output already gets;
//  2. compact now, whatever the estimate says; the summary request cuts every
//     tool result to compactResultMaxChars, so it cannot overflow on the
//     result that caused this;
//  3. cut any other oversized message, pasted input or a delivered result,
//     and any oversized reasoning or tool-call arguments, so that no message
//     can hold the agent over the window; the summary is left whole, since
//     its own output bound (compactSummaryMaxTokens) already limits it;
//  4. compact again keeping only the latest message or call, for a tail of
//     many small messages that is dense enough to overflow on its own.
//
// Only when all four are spent does the turn fail as before.
const (
	overflowCutResults = iota + 1
	overflowCompact
	overflowCutMessages
	overflowCompactAll
)

// overflowMessageChars is the size above which a message is cut in an
// overflow: comfortably more than the edge cut it becomes.
const overflowMessageChars = 4 * edgeChars

// recoverFromOverflow applies the next stage that changes the history and
// returns it, the stage reached, and whether anything changed.
func (a *Agent) recoverFromOverflow(ctx context.Context, p provider.Provider, model, effort string, history []provider.Message, contextWindow, stage int, cause error) ([]provider.Message, int, bool) {
	for stage < overflowCompactAll {
		stage++
		var changed int
		switch stage {
		case overflowCutResults:
			history, changed = cutOversized(history, func(m provider.Message) bool { return m.Role == "tool" })
			if changed > 0 {
				a.warnOverflow(cause, fmt.Sprintf("cut %d oversized tool result(s) to their first and last lines", changed))
			}
		case overflowCompact:
			cut := compactionCut(history, compactKeepBudget(contextWindow), 0)
			if cut > 0 {
				a.warnOverflow(cause, "compacting now")
				history, changed = a.compacted(ctx, p, model, effort, history, cut)
			}
		case overflowCutMessages:
			history, changed = cutOversized(history, func(m provider.Message) bool {
				_, summary := previousSummary([]provider.Message{m})
				return !summary
			})
			if changed > 0 {
				a.warnOverflow(cause, fmt.Sprintf("cut %d oversized message(s) to their first and last lines", changed))
			}
		case overflowCompactAll:
			cut := compactionCut(history, 0, 1)
			if cut > 0 {
				a.warnOverflow(cause, "compacting all but the latest message")
				history, changed = a.compacted(ctx, p, model, effort, history, cut)
			}
		}
		if changed > 0 {
			return history, stage, true
		}
	}
	return history, stage, false
}

func (a *Agent) warnOverflow(cause error, action string) {
	a.runtime.emit(seam.Event{AgentID: a.ID, AgentTitle: a.Title, Kind: "warning", Text: "the provider refused the request as over the context window; " + action + " and retrying: " + cause.Error(), Metadata: map[string]any{"purpose": "overflow"}})
}

// cutOversized cuts every message that eligible accepts: its content and
// its reasoning, each when over overflowMessageChars, to their first and last
// lines, and its tool-call arguments, when over it, to a valid JSON stub, so
// the call and its result stay paired. It returns the number of messages
// changed. The history is copied, never edited in place: a turn's history
// shares its backing array with the one it was read from.
func cutOversized(history []provider.Message, eligible func(provider.Message) bool) ([]provider.Message, int) {
	out := append([]provider.Message(nil), history...)
	changed := 0
	for i, message := range out {
		if !eligible(message) {
			continue
		}
		cut := false
		if len(message.Content) > overflowMessageChars {
			out[i].Content = overflowCut(message.Content)
			cut = true
		}
		if len(message.ReasoningContent) > overflowMessageChars {
			out[i].ReasoningContent = overflowCut(message.ReasoningContent)
			cut = true
		}
		copied := false
		for j, call := range message.ToolCalls {
			if len(call.Function.Arguments) <= overflowMessageChars {
				continue
			}
			if !copied {
				out[i].ToolCalls = append([]provider.ToolCall(nil), message.ToolCalls...)
				copied = true
			}
			stub, _ := json.Marshal(map[string]string{"cut": fmt.Sprintf("%d bytes of arguments cut after the provider refused the request as over its context window", len(call.Function.Arguments))})
			out[i].ToolCalls[j].Function.Arguments = string(stub)
			cut = true
		}
		if cut {
			changed++
		}
	}
	return out, changed
}

func overflowCut(text string) string {
	lines := strings.Count(strings.TrimSuffix(text, "\n"), "\n") + 1
	head, tail := edgeLines(text)
	return fmt.Sprintf("content cut after the provider refused the request as over its context window: %d bytes, %d lines; the first and last %d lines are shown\n%s\n…\n%s", len(text), lines, edgeLineCount, head, tail)
}
