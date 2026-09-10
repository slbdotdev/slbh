//go:build linux

package job

import (
	"context"
	"os/exec"
	"syscall"
)

func shellCommand(ctx context.Context, script, dir string) (*exec.Cmd, error) {
	cmd := exec.CommandContext(ctx, "bash", "-lc", script)
	cmd.Dir = dir
	return cmd, nil
}

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
