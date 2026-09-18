package secretarywake

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

const defaultTimeout = 30 * time.Second

var queuedResult = regexp.MustCompile(`(?m)^Queued message ([^[:space:]]+) for thread ([^[:space:].]+)\.[[:space:]]*$`)

// Options configures one wake. Empty fields use the codex executable, the
// "secretary" session name, and a 30-second timeout.
type Options struct {
	CodexCommand string
	SessionName  string
	Timeout      time.Duration
}

// Result describes a successfully queued wake.
type Result struct {
	Success   bool
	ThreadID  string
	MessageID string
	Output    string
}

// Wake queues message to the exact named Secretary session.
func Wake(ctx context.Context, opts Options, message string) (Result, error) {
	command := strings.TrimSpace(opts.CodexCommand)
	if command == "" {
		command = "codex"
	}
	session := strings.TrimSpace(opts.SessionName)
	if session == "" {
		session = "secretary"
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	if strings.TrimSpace(message) == "" {
		return Result{}, fmt.Errorf("secretary wake: message is empty")
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	args := []string{"queue", "--thread", session, "--message", message}
	cmd := exec.CommandContext(runCtx, command, args...)
	cmd.Env = scrubEnvironment(os.Environ())
	configureProcess(cmd)
	cmd.Cancel = func() error { return killProcess(cmd) }
	output, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(output))
	result := Result{Output: text}

	if runCtx.Err() != nil {
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			return result, fmt.Errorf("secretary wake: codex queue timed out after %s: %w", timeout, runCtx.Err())
		}
		return result, fmt.Errorf("secretary wake: codex queue canceled: %w", runCtx.Err())
	}
	if err != nil {
		return result, commandError(session, text, err)
	}

	result.Success = true
	if match := queuedResult.FindStringSubmatch(text); len(match) == 3 {
		result.MessageID = match[1]
		result.ThreadID = match[2]
	}
	return result, nil
}

// InboxMessage returns the small durable-store pointer used for a wake. The
// reports themselves are deliberately not copied into the queued message.
func InboxMessage(pending int) string {
	if pending == 1 {
		return "1 report is pending. Run slbh inbox to review it and acknowledge it with slbh ack <id>."
	}
	return fmt.Sprintf("%d reports are pending. Run slbh inbox to review them and acknowledge each with slbh ack <id>.", pending)
}

func scrubEnvironment(environment []string) []string {
	clean := make([]string, 0, len(environment))
	for _, entry := range environment {
		name, _, _ := strings.Cut(entry, "=")
		if strings.EqualFold(name, "CODEX_API_KEY") {
			continue
		}
		clean = append(clean, entry)
	}
	return clean
}

func commandError(session, output string, runErr error) error {
	lower := strings.ToLower(output)
	switch {
	case strings.Contains(lower, "no active session found matching"):
		return fmt.Errorf("secretary wake: session %q did not resolve: %s", session, output)
	case strings.Contains(lower, "failed to connect to remote app server"),
		strings.Contains(lower, "connection refused"),
		strings.Contains(lower, "daemon is not running"):
		return fmt.Errorf("secretary wake: Codex app-server daemon is unavailable: %s", output)
	case output != "":
		return fmt.Errorf("secretary wake: codex queue failed: %w: %s", runErr, output)
	default:
		return fmt.Errorf("secretary wake: codex queue failed: %w", runErr)
	}
}
