//go:build live_integration

package secretarywake

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestLiveWakeStartsTurnInNamedInteractiveTUI(t *testing.T) {
	if os.Getenv("SLBH_RUN_WAKE_TESTS") != "1" {
		t.Skip("set SLBH_RUN_WAKE_TESTS=1 to run the billed Secretary wake acceptance test")
	}
	codex, err := exec.LookPath("codex")
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}

	// Listing first is a safety boundary: every later tmux mutation uses only
	// the unique name constructed below, never an owner's existing session.
	sessions, listErr := tmuxOutput("list-sessions", "-F", "#{session_name}")
	if listErr != nil && !strings.Contains(strings.ToLower(listErr.Error()), "no server running") {
		t.Fatalf("list tmux sessions: %v", listErr)
	}
	t.Logf("tmux sessions before wake test: %q", strings.Fields(sessions))
	tmuxSession := fmt.Sprintf("slbh-wake-%d-%d", os.Getpid(), time.Now().UnixNano())
	for _, existing := range strings.Fields(sessions) {
		if existing == tmuxSession {
			t.Fatalf("refusing to reuse existing tmux session %q", tmuxSession)
		}
	}
	threadName := tmuxSession + "-thread"

	startedDaemon, updaterPID, err := ensureRemoteControl(codex)
	if startedDaemon {
		t.Cleanup(func() { stopTrialDaemon(t, codex, updaterPID) })
	}
	if err != nil {
		t.Fatal(err)
	}

	const readyMarker = "WAKE-LIVE-READY"
	firstPrompt := "Reply with exactly " + readyMarker + " and then wait for more input. Do not use tools."
	commandLine := strings.Join([]string{
		shellQuote(codex), "-C", shellQuote(root), "-m", "gpt-5.6-luna",
		"-s", "read-only", "--no-alt-screen", shellQuote(firstPrompt),
	}, " ")
	start := exec.Command("tmux", "new-session", "-d", "-s", tmuxSession, "-x", "200", "-y", "60", "-c", root, commandLine)
	start.Env = scrubEnvironment(os.Environ())
	if output, err := start.CombinedOutput(); err != nil {
		t.Fatalf("start throwaway TUI: %v\n%s", err, output)
	}
	t.Cleanup(func() {
		command := exec.Command("tmux", "kill-session", "-t", tmuxSession)
		command.Env = scrubEnvironment(os.Environ())
		if output, err := command.CombinedOutput(); err != nil && !strings.Contains(strings.ToLower(string(output)), "can't find session") {
			t.Errorf("clean up tmux session %q: %v: %s", tmuxSession, err, output)
		}
	})

	waitForPane(t, tmuxSession, 5*time.Minute, func(pane string) bool {
		return strings.Count(pane, readyMarker) >= 2
	}, "first TUI turn to answer")
	if _, err := tmuxOutput("send-keys", "-t", tmuxSession, "-l", "--", "/rename "+threadName); err != nil {
		t.Fatalf("type /rename in throwaway TUI: %v", err)
	}
	if _, err := tmuxOutput("send-keys", "-t", tmuxSession, "Enter"); err != nil {
		t.Fatalf("submit /rename in throwaway TUI: %v", err)
	}
	// Codex's multiline composer may use the first Enter to accept the text
	// without submitting it. The measured trial likewise needed the follow-up
	// Enter; a blank submission is harmless if the first one already ran.
	time.Sleep(300 * time.Millisecond)
	if _, err := tmuxOutput("send-keys", "-t", tmuxSession, "Enter"); err != nil {
		t.Fatalf("confirm /rename in throwaway TUI: %v", err)
	}
	wantThreadID := waitForSessionName(t, threadName, time.Minute)

	const deliveredMarker = "WAKE-LIVE-DELIVERED"
	wakeMessage := "Reply with exactly " + deliveredMarker + " and nothing else. Do not use tools."
	result, err := Wake(context.Background(), Options{
		CodexCommand: codex,
		SessionName:  threadName,
		Timeout:      30 * time.Second,
	}, wakeMessage)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Success || result.ThreadID != wantThreadID {
		t.Fatalf("wake result = %#v", result)
	}
	waitForPane(t, tmuxSession, 5*time.Minute, func(pane string) bool {
		// The prompt contributes one marker; the assistant's answer is the
		// second, proving the queued message became and completed a new turn.
		return strings.Count(pane, deliveredMarker) >= 2
	}, "queued wake turn to answer")
	t.Logf("live wake started and answered on thread %s", result.ThreadID)
}

func waitForSessionName(t *testing.T, name string, timeout time.Duration) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	indexPath := filepath.Join(home, ".codex", "session_index.jsonl")
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		data, readErr := os.ReadFile(indexPath)
		if readErr == nil {
			for _, line := range strings.Split(string(data), "\n") {
				var entry struct {
					ID   string `json:"id"`
					Name string `json:"thread_name"`
				}
				if json.Unmarshal([]byte(line), &entry) == nil && entry.Name == name && entry.ID != "" {
					return entry.ID
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for Codex to persist session name %q", name)
	return ""
}

func tmuxOutput(args ...string) (string, error) {
	command := exec.Command("tmux", args...)
	command.Env = scrubEnvironment(os.Environ())
	output, err := command.CombinedOutput()
	if err != nil {
		return string(output), fmt.Errorf("tmux %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return string(output), nil
}

func waitForPane(t *testing.T, session string, timeout time.Duration, ready func(string) bool, description string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		pane, err := tmuxOutput("capture-pane", "-p", "-t", session, "-S", "-200")
		if err == nil {
			last = pane
			if ready(pane) {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s; final pane:\n%s", description, last)
}

func ensureRemoteControl(codex string) (bool, int, error) {
	command := exec.Command(codex, "remote-control", "start", "--json")
	command.Env = scrubEnvironment(os.Environ())
	output, err := command.CombinedOutput()
	if err != nil {
		return false, 0, fmt.Errorf("start Codex remote control: %w: %s", err, output)
	}
	var response struct {
		Daemon struct {
			Status string `json:"status"`
		} `json:"daemon"`
	}
	if err := json.Unmarshal(output, &response); err != nil {
		return false, 0, fmt.Errorf("decode Codex remote-control start: %w: %s", err, output)
	}
	started := response.Daemon.Status == "bootstrapped"
	if !started {
		return false, 0, nil
	}
	pid, err := daemonUpdaterPID()
	if err != nil {
		return true, 0, err
	}
	return true, pid, nil
}

func daemonUpdaterPID() (int, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return 0, err
	}
	path := filepath.Join(home, ".codex", "app-server-daemon", "app-server-updater.pid")
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("read trial daemon updater PID: %w", err)
	}
	var record struct {
		PID int `json:"pid"`
	}
	if err := json.Unmarshal(data, &record); err != nil {
		return 0, fmt.Errorf("decode trial daemon updater PID: %w", err)
	}
	if record.PID <= 0 {
		return 0, fmt.Errorf("trial daemon updater PID is invalid: %d", record.PID)
	}
	return record.PID, nil
}

func stopTrialDaemon(t *testing.T, codex string, updaterPID int) {
	t.Helper()
	command := exec.Command(codex, "remote-control", "stop", "--json")
	command.Env = scrubEnvironment(os.Environ())
	if output, err := command.CombinedOutput(); err != nil {
		t.Errorf("stop trial Codex daemon: %v: %s", err, output)
	}
	time.Sleep(300 * time.Millisecond)
	if updaterPID <= 0 {
		return
	}
	cmdline, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(updaterPID), "cmdline"))
	if err != nil {
		if !os.IsNotExist(err) {
			t.Errorf("inspect trial updater %d: %v", updaterPID, err)
		}
		return
	}
	if !strings.Contains(strings.ReplaceAll(string(cmdline), "\x00", " "), "app-server daemon pid-update-loop") {
		t.Errorf("refusing to kill PID %d because it is no longer the trial updater", updaterPID)
		return
	}
	process, err := os.FindProcess(updaterPID)
	if err != nil {
		t.Errorf("find trial updater %d: %v", updaterPID, err)
		return
	}
	if err := process.Kill(); err != nil && !strings.Contains(strings.ToLower(err.Error()), "process already finished") {
		t.Errorf("kill residual trial updater %d: %v", updaterPID, err)
	}
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
