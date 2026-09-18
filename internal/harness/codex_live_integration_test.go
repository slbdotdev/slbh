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

func TestLiveCodexLeafRoundTrip(t *testing.T) {
	if os.Getenv("SLBH_RUN_CODEX_TESTS") != "1" {
		t.Skip("set SLBH_RUN_CODEX_TESTS=1 to run the billed Codex leaf acceptance test")
	}
	model := os.Getenv("SLBH_CODEX_TEST_MODEL")
	if model == "" {
		model = "gpt-5.6-luna"
	}
	r, err := New(config.Config{Home: t.TempDir()}, Options{Provider: func(string) (provider.Provider, error) { return fakeProvider{}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	manager := launchTestManager(t, r)
	child, err := r.launchSubagentSpec(manager.ID, LaunchSpec{
		Title: "codex-live", Role: "luna", Harness: "codex", Model: model,
		Brief: "Reply with exactly CODEX_LEAF_OK and nothing else. Do not call tools.",
	})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.NewTimer(8 * time.Minute)
	defer deadline.Stop()
	var answer string
	for {
		select {
		case event := <-testEvents(r):
			if event.AgentID == child.ID && event.Kind == "error" {
				t.Fatal(event.Text)
			}
			if event.AgentID == child.ID && event.Kind == "turn_done" {
				answer = event.Text
				if !strings.Contains(answer, "CODEX_LEAF_OK") {
					t.Fatalf("Codex leaf answer = %q", answer)
				}
				return
			}
		case <-deadline.C:
			t.Fatal("Codex leaf did not finish")
		}
	}
}

func TestLiveCodexLeafBidirectionalSteer(t *testing.T) {
	if os.Getenv("SLBH_RUN_CODEX_TESTS") != "1" {
		t.Skip("set SLBH_RUN_CODEX_TESTS=1 to run the billed Codex steering acceptance test")
	}
	model := os.Getenv("SLBH_CODEX_TEST_MODEL")
	if model == "" {
		model = "gpt-5.6-luna"
	}
	r, err := New(config.Config{Home: t.TempDir()}, Options{Provider: func(string) (provider.Provider, error) { return fakeProvider{}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	manager := launchTestManager(t, r)
	child, err := r.launchSubagentSpec(manager.ID, LaunchSpec{
		Title: "codex-steer-live", Role: "luna", Harness: "codex", Model: model,
		Brief: "First run exactly `python -c \"import time; time.sleep(3)\"`. While it runs, a slbh steer will arrive. After that, call slbh_message_parent with message CODEX_PARENT_OK, then reply exactly CODEX_STEER_OK and nothing else.",
	})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.NewTimer(8 * time.Minute)
	defer deadline.Stop()
	steered := false
	parentMessage := false
	for {
		select {
		case event := <-testEvents(r):
			if event.AgentID == child.ID && event.Kind == "error" {
				t.Fatal(event.Text)
			}
			if event.AgentID == child.ID && event.Kind == "codex_item_started" && event.Text == "commandExecution" && !steered {
				if err := child.Steer("STEER_NOW: continue with the parent-message step and final answer."); err != nil {
					t.Fatal(err)
				}
				steered = true
			}
			if event.AgentID == child.ID && event.Kind == "child_message" && strings.Contains(event.Text, "CODEX_PARENT_OK") {
				parentMessage = true
			}
			if event.AgentID == child.ID && event.Kind == "turn_done" {
				if !steered {
					t.Fatal("Codex leaf finished before the active-turn steer")
				}
				if !parentMessage {
					t.Fatal("Codex leaf never called slbh_message_parent")
				}
				if !strings.Contains(event.Text, "CODEX_STEER_OK") {
					t.Fatalf("Codex leaf answer = %q", event.Text)
				}
				return
			}
		case <-deadline.C:
			t.Fatalf("Codex steering acceptance timed out; steered=%v parentMessage=%v", steered, parentMessage)
		}
	}
}
