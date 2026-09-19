//go:build windows

package job

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

func scriptCommand(ctx context.Context, interpreter, path, dir string) (*exec.Cmd, error) {
	var name string
	var args []string
	switch interpreter {
	case "bash":
		name = cachedBashExecutable()
		if name == "" {
			return nil, fmt.Errorf("Git Bash was not found")
		}
		args = []string{"-l", filepath.ToSlash(path)}
	case "pwsh":
		if strings.Contains(path, "'") {
			return nil, fmt.Errorf("PowerShell script path contains unsupported quote")
		}
		name = cachedPwshExecutable()
		if name == "" {
			return nil, fmt.Errorf("pwsh was not found")
		}
		args = []string{"-NoProfile", "-NonInteractive", "-Command", pwshWrapper(path)}
	case "python":
		name = pythonExecutable()
		args = []string{"-X", "utf8", "-u", path}
	default:
		return nil, fmt.Errorf("unknown job interpreter %q", interpreter)
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Stdin = nil
	return cmd, nil
}

var cachedBashExecutable = sync.OnceValue(detectWindowsBashExecutable)
var cachedPwshExecutable = sync.OnceValue(func() string {
	path, _ := exec.LookPath("pwsh")
	return path
})

func pwshWrapper(path string) string {
	return "[Console]::OutputEncoding=[Text.UTF8Encoding]::new($false); $PSStyle.OutputRendering='PlainText'; $global:LASTEXITCODE=0; & '" + path + "'; if (-not $?) { if ($LASTEXITCODE) { exit $LASTEXITCODE } else { exit 1 } }; exit $LASTEXITCODE"
}

func windowsBashExecutable() string { return cachedBashExecutable() }

func detectWindowsBashExecutable() string {
	candidates := []string{}
	if configured := strings.TrimSpace(os.Getenv("SLBH_BASH")); configured != "" {
		candidates = append(candidates, configured)
	}
	if out, err := exec.Command("git", "--exec-path").Output(); err == nil {
		candidates = append(candidates, gitBashCandidate(string(out)))
	}
	if pf := os.Getenv("ProgramFiles"); pf != "" {
		candidates = append(candidates, filepath.Join(pf, "Git", "bin", "bash.exe"))
	}
	for _, candidate := range candidates {
		if confirmGitBash(candidate) {
			return candidate
		}
	}
	return ""
}

func gitBashCandidate(execPath string) string {
	root := strings.TrimSpace(execPath)
	for i := 0; i < 3; i++ {
		root = filepath.Dir(root)
	}
	return filepath.Join(root, "bin", "bash.exe")
}

func AvailableInterpreters() []string {
	result := []string{}
	if cachedBashExecutable() != "" {
		result = append(result, "bash")
	}
	if cachedPwshExecutable() != "" {
		result = append(result, "pwsh")
	}
	return append(result, "python")
}
func BashDescription() string {
	return "Run a Bash script with Git Bash on Windows; Windows paths appear as /c/Users/... ."
}

func confirmGitBash(path string) bool {
	out, err := exec.Command(path, "-c", "uname -s").Output()
	if err != nil {
		return false
	}
	s := strings.TrimSpace(string(out))
	return strings.HasPrefix(s, "MINGW") || strings.HasPrefix(s, "MSYS")
}

type windowsJob struct {
	mu     sync.Mutex
	handle windows.Handle
}

func processStarted(cmd *exec.Cmd) (func(), func() error, error) {
	h, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, nil, err
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err = windows.SetInformationJobObject(h, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(h)
		return nil, nil, err
	}
	// A child can escape in the unavoidable window between Start and assignment.
	process, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err != nil {
		windows.CloseHandle(h)
		return nil, nil, err
	}
	defer windows.CloseHandle(process)
	if err = windows.AssignProcessToJobObject(h, process); err != nil {
		windows.CloseHandle(h)
		return nil, nil, err
	}
	j := &windowsJob{handle: h}
	return j.close, j.terminate, nil
}

// close and terminate share a lock because Kill and the job's wait goroutine
// can reach them concurrently, and a closed handle value can be reused by the
// next CreateJobObject: terminating through a stale value could kill another
// job's tree.
func (j *windowsJob) close() {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.handle != 0 {
		windows.CloseHandle(j.handle)
		j.handle = 0
	}
}

func (j *windowsJob) terminate() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.handle == 0 {
		return nil
	}
	return windows.TerminateJobObject(j.handle, 1)
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
