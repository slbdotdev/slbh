//go:build !linux

package harness

import "runtime"

func shellName() string {
	if runtime.GOOS == "windows" {
		return "cmd.exe"
	}
	return "sh"
}

func shellArgs(script string) []string {
	if runtime.GOOS == "windows" {
		return []string{"/d", "/c", script}
	}
	return []string{"-c", script}
}
