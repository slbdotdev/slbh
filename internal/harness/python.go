package harness

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

func pythonCommand(script string) []string {
	return []string{pythonExecutable(), "-c", script}
}

func pythonExecutable() string {
	if configured := strings.TrimSpace(os.Getenv("SLBH_PYTHON")); configured != "" {
		return configured
	}
	if home, err := os.UserHomeDir(); err == nil {
		venv := filepath.Join(home, ".local", "share", "slbh", "python")
		if runtime.GOOS == "windows" {
			venv = filepath.Join(venv, "Scripts", "python.exe")
		} else {
			venv = filepath.Join(venv, "bin", "python")
		}
		if _, err := os.Stat(venv); err == nil {
			return venv
		}
	}
	if runtime.GOOS == "windows" {
		if _, err := exec.LookPath("python.exe"); err == nil {
			return "python.exe"
		}
		return "python"
	}
	if _, err := exec.LookPath("python3"); err == nil {
		return "python3"
	}
	return "python"
}
