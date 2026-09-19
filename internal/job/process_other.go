//go:build !linux && !windows

package job

import (
	"context"
	"fmt"
	"os/exec"
)

func scriptCommand(ctx context.Context, interpreter, path, dir string) (*exec.Cmd, error) {
	var name string
	var args []string
	switch interpreter {
	case "bash":
		name, args = "bash", []string{"-l", path}
	case "python":
		name, args = pythonExecutable(), []string{"-X", "utf8", "-u", path}
	default:
		return nil, fmt.Errorf("interpreter %q is not available", interpreter)
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Stdin = nil
	return cmd, nil
}
func setProcessGroup(*exec.Cmd)                              {}
func processStarted(*exec.Cmd) (func(), func() error, error) { return func() {}, nil, nil }
func killCommand(cmd *exec.Cmd) error {
	if cmd.Process != nil {
		return cmd.Process.Kill()
	}
	return nil
}
func exitCode(err error) int {
	if exit, ok := err.(*exec.ExitError); ok {
		return exit.ExitCode()
	}
	return -1
}
