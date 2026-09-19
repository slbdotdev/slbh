//go:build linux

package job

import (
	"context"
	"fmt"
	"os/exec"
	"syscall"
)

func scriptCommand(ctx context.Context, interpreter, path, dir string) (*exec.Cmd, error) {
	var args []string
	switch interpreter {
	case "bash":
		args = []string{"-l", path}
	case "python":
		args = []string{"-X", "utf8", "-u", path}
	default:
		return nil, fmt.Errorf("interpreter %q is not available on linux", interpreter)
	}
	cmd := exec.CommandContext(ctx, map[string]string{"bash": "bash", "python": pythonExecutable()}[interpreter], args...)
	cmd.Dir = dir
	cmd.Stdin = nil
	return cmd, nil
}

func processStarted(*exec.Cmd) (func(), func() error, error) { return func() {}, nil, nil }

func setProcessGroup(cmd *exec.Cmd) {
	// Setpgid lets Kill terminate descendants; Pdeathsig covers a hard TUI
	// death where Runtime.Close cannot run.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
}

func killCommand(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}

func exitCode(err error) int {
	if exit, ok := err.(*exec.ExitError); ok {
		return exit.ExitCode()
	}
	return -1
}
