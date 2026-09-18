// Package headless runs one slbh turn without the Bubble Tea UI.
//
// slbh's Codex leaves have always been headless: a native parent launches one with
// harness: "codex" and it runs a persistent app-server session with no UI involvement.
// The native side had no equivalent -- cmd/slbh took no flags and handed the runtime
// straight to the TUI -- so a native agent could only be driven by a human at a terminal.
// This package closes that gap using the same runtime, the same Agent, the same tools and
// the same system prompt the TUI drives. It is a different front end, not a second harness.
package headless

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/slbdotdev/slbh/internal/harness"
)

// Options configures a single headless turn.
type Options struct {
	Prompt  string        // the user message; required
	Timeout time.Duration // wall cap for the turn; zero means no cap
	JSON    bool          // emit one JSON object per event instead of prose
	Quiet   bool          // suppress per-event output; the summary is still returned
	Out     io.Writer     // event stream destination; nil means io.Discard
}

// Result is what the turn did. Counts come from the runtime's own event stream, so they
// are the same numbers the TUI renders rather than a parallel accounting.
type Result struct {
	AgentID      string  `json:"agent_id"`
	Model        string  `json:"model"`
	Runtime      string  `json:"runtime"`              // runtime id; its directory under the home holds every transcript
	Transcript   string  `json:"transcript,omitempty"` // the seat's transcript.jsonl, the full record of the turn
	StopReason   string  `json:"stop_reason"`          // "done", "error" or "wall_cap"
	Turns        int     `json:"turns"`
	ToolCalls    int     `json:"tool_calls"`
	ToolResults  int     `json:"tool_results"`
	Errors       int     `json:"errors"`
	PromptTokens int     `json:"prompt_tokens"`
	OutputTokens int     `json:"output_tokens"`
	WallS        float64 `json:"wall_s"`
	Final        string  `json:"final,omitempty"`
}

// metaInt reads a count from an event's metadata, tolerating the numeric types JSON and Go
// both produce: a value that arrived over the wire is float64, one set in-process is int.
func metaInt(meta map[string]any, key string) (int, bool) {
	if meta == nil {
		return 0, false
	}
	switch v := meta[key].(type) {
	case int:
		return v, true
	case int64:
		return int(v), true
	case float64:
		return int(v), true
	}
	return 0, false
}

// Run sends one prompt to the runtime's seat agent and returns when that agent's turn
// completes or the wall cap expires. The runtime is caller-owned: Run neither creates nor
// closes it, so an embedder can drive several turns or inspect state afterwards.
func Run(rt *harness.Runtime, opts Options) (Result, error) {
	out := opts.Out
	if out == nil || opts.Quiet {
		out = io.Discard
	}
	if rt == nil {
		return Result{}, fmt.Errorf("headless: nil runtime")
	}
	var seat harness.AgentSnapshot
	var found bool
	for _, agent := range rt.Agents() {
		if agent.Depth == 0 {
			seat = agent
			found = true
			break
		}
	}
	if !found {
		return Result{}, fmt.Errorf("headless: runtime has no seat agent")
	}
	res := Result{AgentID: seat.ID, Model: seat.Model, StopReason: "done", Runtime: rt.ID()}
	if path, err := rt.TranscriptPath(seat.ID); err == nil {
		res.Transcript = path
	}

	start := time.Now()
	if err := rt.SendPrompt(seat.ID, opts.Prompt); err != nil {
		return res, fmt.Errorf("headless: send: %w", err)
	}

	var deadline <-chan time.Time
	if opts.Timeout > 0 {
		timer := time.NewTimer(opts.Timeout)
		defer timer.Stop()
		deadline = timer.C
	}

	// The seat's reply streams as deltas; Final is the whole of the last one.
	var final strings.Builder
	finish := func() (Result, error) {
		res.WallS = time.Since(start).Seconds()
		res.Final = final.String()
		return res, nil
	}

	events := rt.Events()
	for {
		select {
		case ev := <-events:
			emit(out, opts.JSON, ev)
			switch ev.Kind {
			case "inference_request":
				// One API round is one turn. A retry re-emits the same round number, so the
				// count is the highest round seen, not the number of requests; the agent
				// drops its partial answer when it retries, and so does Final.
				if ev.AgentID == seat.ID {
					if round, ok := metaInt(ev.Metadata, "round"); ok && round+1 > res.Turns {
						res.Turns = round + 1
					}
					final.Reset()
				}
			case "assistant":
				if ev.AgentID == seat.ID {
					final.WriteString(ev.Text)
				}
			case "tool":
				res.ToolCalls++
			case "tool_result":
				res.ToolResults++
			case "error", "delivery_error":
				res.Errors++
				// A native agent emits "error" only from fail(), which ends its turn with no
				// turn_done to follow. For the seat that is the end of the run: waiting on
				// would only burn the wall cap against a provider that has already refused.
				if ev.Kind == "error" && ev.AgentID == seat.ID {
					res.StopReason = "error"
					return finish()
				}
			case "usage":
				// The metadata is the provider's own usage object on the OpenAI wire shape:
				// prompt_tokens and completion_tokens. Summed over rounds.
				if v, ok := metaInt(ev.Metadata, "prompt_tokens"); ok {
					res.PromptTokens += v
				}
				if v, ok := metaInt(ev.Metadata, "completion_tokens"); ok {
					res.OutputTokens += v
				} else if v, ok := metaInt(ev.Metadata, "output_tokens"); ok {
					res.OutputTokens += v
				}
			case "turn_done":
				// Only the seat's own turn ends this run. A subagent finishing is not the
				// seat finishing, and treating it as such would cut the turn short.
				if ev.AgentID == seat.ID {
					return finish()
				}
			}
		case <-deadline:
			res.StopReason = "wall_cap"
			return finish()
		case <-rt.Done():
			// Close cancels the context and stops the dispatcher but never
			// closes the events channel, and a cancelled turn returns without
			// emitting turn_done. Without this case a SIGTERM mid-turn parks
			// the process on <-events until someone sends SIGKILL, so the CLI
			// can never report its status and no service manager can observe a
			// clean shutdown.
			res.StopReason = "shutdown"
			return finish()
		}
	}
}

func emit(out io.Writer, asJSON bool, ev harness.Event) {
	if out == io.Discard {
		return
	}
	if asJSON {
		if b, err := json.Marshal(ev); err == nil {
			fmt.Fprintln(out, string(b))
		}
		return
	}
	switch ev.Kind {
	case "assistant", "thinking", "status", "error", "delivery_error":
		if ev.Text != "" {
			fmt.Fprintf(out, "[%s] %s\n", ev.Kind, ev.Text)
		}
	case "tool", "tool_result":
		fmt.Fprintf(out, "[%s] %s\n", ev.Kind, ev.Text)
	}
}
