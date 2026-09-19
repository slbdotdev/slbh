package job

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

func writeScript(root, jobID, interpreter, script string) (string, error) {
	name := map[string]string{"bash": "script.sh", "pwsh": "script.ps1", "python": "script.py"}[interpreter]
	if name == "" {
		return "", fmt.Errorf("unknown job interpreter %q", interpreter)
	}
	dir := filepath.Join(root, jobID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if interpreter == "bash" {
		script = strings.ReplaceAll(script, "\r\n", "\n")
	}
	path := filepath.Join(dir, name)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	if _, err = f.WriteString(script); err != nil {
		_ = f.Close()
		return "", err
	}
	if err = f.Close(); err != nil {
		return "", err
	}
	return path, nil
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
