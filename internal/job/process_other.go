//go:build !linux

package job

import (
	"context"
	"os/exec"
)

func shellCommand(ctx context.Context, script, dir string) (*exec.Cmd, error) {
	if isWindows() {
		cmd := exec.CommandContext(ctx, "cmd.exe", "/d", "/c", script)
		cmd.Dir = dir
		return cmd, nil
	}
	cmd := exec.CommandContext(ctx, "sh", "-c", script)
	cmd.Dir = dir
	return cmd, nil
}

func setProcessGroup(*exec.Cmd) {}

func killCommand(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}

func exitCode(err error) int {
	if exit, ok := err.(*exec.ExitError); ok {
		return exit.ExitCode()
	}
	return -1
}
