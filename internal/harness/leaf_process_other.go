//go:build !linux

package harness

import "os/exec"

func configureLeafProcess(*exec.Cmd) {}

func killLeafProcess(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
