//go:build live_integration

package harness

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/slbdotdev/slbh/internal/config"
	"github.com/slbdotdev/slbh/internal/provider"
)

func TestLiveClaudeCodeLeafRoundTrip(t *testing.T) {
	if os.Getenv("SLBH_RUN_CLAUDE_TESTS") != "1" {
		t.Skip("set SLBH_RUN_CLAUDE_TESTS=1 to run the billed Claude Code leaf acceptance test")
	}
	model := os.Getenv("SLBH_CLAUDE_TEST_MODEL")
	if model == "" {
		model = "claude-opus-5"
	}
	r, err := New(config.Config{Home: t.TempDir()}, Options{Provider: func(string) (provider.Provider, error) { return fakeProvider{}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	child, err := r.launchSubagentSpec(r.seat().ID, LaunchSpec{
		Title: "claude-live", Harness: "claude_code", Model: model, Effort: "low",
		Brief: "Reply with exactly CLAUDE_LEAF_OK and nothing else. Do not call tools.",
	})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.NewTimer(8 * time.Minute)
	defer deadline.Stop()
	for {
		select {
		case event := <-r.Events():
			if event.AgentID == child.ID && event.Kind == "error" {
				t.Fatal(event.Text)
			}
			if event.AgentID == child.ID && event.Kind == "turn_done" {
				if !strings.Contains(event.Text, "CLAUDE_LEAF_OK") {
					t.Fatalf("Claude Code leaf answer = %q", event.Text)
				}
				return
			}
		case <-deadline.C:
			t.Fatal("Claude Code leaf did not finish")
		}
	}
}
