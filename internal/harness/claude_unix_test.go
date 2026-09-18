//go:build !windows

package harness

// Process-group teardown is a POSIX contract: the Claude Code leaf runs on the
// devbox, and Windows has neither process groups nor signal 0.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestClaudeLeafStopKillsProcessGroup(t *testing.T) {
	childPIDFile := filepath.Join(t.TempDir(), "child.pid")
	t.Setenv("SLBH_CLAUDE_CHILD_PID_FILE", childPIDFile)
	r := newClaudeTestRuntime(t, "")
	child := launchFakeClaude(t, r, "low", "")
	_ = waitForClaudeEvent(t, r, child.ID, "claude_ready")
	leaf := child.claudeBackend()
	processPID := leaf.cmd.Process.Pid
	data := waitForFile(t, childPIDFile)
	descendantPID, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.endSubagent(child.ParentID, child.ID); err != nil {
		t.Fatal(err)
	}
	for _, pid := range []int{processPID, descendantPID} {
		deadline := time.Now().Add(3 * time.Second)
		for processExists(pid) && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if processExists(pid) {
			t.Fatalf("stopping Claude leaf left process %d alive", pid)
		}
	}
}

func processExists(pid int) bool {
	if stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil {
		fields := strings.Fields(string(stat))
		if len(fields) > 2 && fields[2] == "Z" {
			return false
		}
	}
	err := syscall.Kill(pid, 0)
	return err == nil || !errors.Is(err, syscall.ESRCH)
}
