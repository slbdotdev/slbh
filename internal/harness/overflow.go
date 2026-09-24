package harness

import (
	"context"
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
//     so that no message can hold the agent over the window;
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

// cutOversized replaces the content of every message that eligible accepts
// and that is over overflowMessageChars with its first and last lines. The
// history is copied, never edited in place: a turn's history shares its
// backing array with the one it was read from.
func cutOversized(history []provider.Message, eligible func(provider.Message) bool) ([]provider.Message, int) {
	out := append([]provider.Message(nil), history...)
	changed := 0
	for i, message := range out {
		if len(message.Content) <= overflowMessageChars || !eligible(message) {
			continue
		}
		lines := strings.Count(strings.TrimSuffix(message.Content, "\n"), "\n") + 1
		head, tail := edgeLines(message.Content)
		out[i].Content = fmt.Sprintf("content cut after the provider refused the request as over its context window: %d bytes, %d lines; the first and last %d lines are shown\n%s\n…\n%s", len(message.Content), lines, edgeLineCount, head, tail)
		changed++
	}
	return out, changed
}
