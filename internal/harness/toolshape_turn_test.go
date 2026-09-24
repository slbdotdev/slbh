package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/slbdotdev/slbh/internal/config"
	"github.com/slbdotdev/slbh/internal/provider"
)

// shapeTurnProvider plays one model turn: it records the tools it was offered,
// calls one of them, and answers once the tool's result comes back.
type shapeTurnProvider struct {
	tool, input string

	mu      sync.Mutex
	offered []string
	result  string
}

func (p *shapeTurnProvider) Stream(_ context.Context, request provider.Request, sink provider.StreamSink) error {
	for _, m := range request.Messages {
		if m.Role == "tool" {
			p.mu.Lock()
			p.result = m.Content
			p.mu.Unlock()
			return sink(provider.Event{Kind: provider.EventText, Text: "done"})
		}
	}
	p.mu.Lock()
	for _, tool := range request.Tools {
		p.offered = append(p.offered, tool.Name)
	}
	p.mu.Unlock()
	return sink(provider.Event{Kind: provider.EventTool, ToolIndex: 0, ToolCallID: "call-1", ToolName: p.tool, Input: p.input})
}

// TestShapeAdvertisementAndDispatchAgreeInATurn is the provider-driven half of
// the shape tests. Those call handlers directly; this proves, for each shape's
// shell, that the name the model is offered is the name the agent loop
// dispatches, with the shape's own argument format, through to the result the
// model receives on the continuation.
func TestShapeAdvertisementAndDispatchAgreeInATurn(t *testing.T) {
	const marker = "SHAPE_TURN_MARKER"
	for _, tc := range []struct {
		shape, tool string
		args        map[string]any
	}{
		{config.ToolShapeLean, "bash", map[string]any{"script": "echo " + marker}},
		{config.ToolShapeAnthropic, "Bash", map[string]any{"command": "echo " + marker, "description": "echo"}},
		{config.ToolShapeCodex, "exec_command", map[string]any{"cmd": "echo " + marker}},
	} {
		t.Run(tc.shape, func(t *testing.T) {
			input, err := json.Marshal(tc.args)
			if err != nil {
				t.Fatal(err)
			}
			p := &shapeTurnProvider{tool: tc.tool, input: string(input)}
			r, err := New(
				config.Config{Home: t.TempDir(), SeatModel: "test", SeatEffort: "high", ToolShape: tc.shape},
				Options{Provider: func(string) (provider.Provider, error) { return p, nil }},
			)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()

			seat := r.seat()
			if err := seat.Send("run it"); err != nil {
				t.Fatal(err)
			}
			var kinds []string
			deadline := time.After(10 * time.Second)
			for done := false; !done; {
				select {
				case event := <-testEvents(r):
					if event.AgentID != seat.ID {
						continue
					}
					switch event.Kind {
					case "error":
						t.Fatalf("turn failed: %s", event.Text)
					case "tool_start", "tool_result":
						if name, _ := event.Metadata["name"].(string); name != tc.tool {
							t.Fatalf("%s names %q, want %q", event.Kind, name, tc.tool)
						}
						if event.Kind == "tool_result" {
							if event.Metadata["error"] != false || fmt.Sprint(event.Metadata["exit_code"]) != "0" {
								t.Fatalf("tool_result metadata = %#v, want a clean exit", event.Metadata)
							}
						}
					case "turn_done":
						done = true
					}
					kinds = append(kinds, event.Kind)
				case <-deadline:
					t.Fatalf("turn did not finish; events %v", kinds)
				}
			}

			p.mu.Lock()
			defer p.mu.Unlock()
			if !slices.Contains(p.offered, tc.tool) {
				t.Fatalf("the %s shape offered %v, not %s", tc.shape, p.offered, tc.tool)
			}
			if !strings.Contains(p.result, marker) {
				t.Fatalf("the model received %q, not the command's output", p.result)
			}
			start, result := slices.Index(kinds, "tool_start"), slices.Index(kinds, "tool_result")
			if start < 0 || result < start || !slices.Contains(kinds[result:], "assistant") {
				t.Fatalf("event order = %v, want tool_start, tool_result, then the answer", kinds)
			}
		})
	}
}
