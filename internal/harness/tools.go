package harness

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/slbdotdev/slbh/internal/config"
	"github.com/slbdotdev/slbh/internal/job"
	"github.com/slbdotdev/slbh/internal/provider"
	"github.com/slbdotdev/slbh/internal/readtools"
	"github.com/slbdotdev/slbh/internal/seam"
)

const scientificPythonPackageNames = "numpy, scipy, pandas, matplotlib, sympy, rebound, astropy, skyfield, jplephem"

// ToolDefinitions is the stable tool prefix sent to every provider request.
// Keep ordering stable: provider prefix caching keys include this schema.
func ToolDefinitions() []provider.Tool {
	return buildToolDefinitions(config.ToolShapeSlbh)
}

// ShapeToolDefinitions is ToolDefinitions for a named tool shape.
func ShapeToolDefinitions(shape string) []provider.Tool {
	return buildToolDefinitions(shape)
}

func buildToolDefinitions(shape string) []provider.Tool {
	stringArg := func(name string) map[string]any {
		return map[string]any{"type": "object", "properties": map[string]any{name: map[string]any{"type": "string"}}, "required": []string{name}}
	}
	var all []provider.Tool
	switch shape {
	case config.ToolShapeAnthropic:
		all = anthropicPrimaryTools()
	case config.ToolShapeCodex:
		all = codexPrimaryTools()
	default:
		all = append(readtools.Definitions(), slbhPrimaryTools(stringArg)...)
	}
	all = append(all, []provider.Tool{
		{Name: "pwsh", Description: commandDescription("Run a PowerShell 7 (pwsh) script on Windows. exit code is the script's exit value, or the last native command's; 1 after a terminating error."), Parameters: commandParameters()},
		{Name: "python", Description: commandDescription(fmt.Sprintf("Run a script with the managed scientific Python environment (%s).", scientificPythonPackageNames)), Parameters: commandParameters()},
		{Name: "job", Description: "Manage jobs with action list, read, or kill. job_id is required for read and kill.", Parameters: map[string]any{"type": "object", "properties": map[string]any{"action": map[string]any{"type": "string", "enum": []string{"list", "read", "kill"}}, "job_id": map[string]any{"type": "string"}}, "required": []string{"action"}}},
		{Name: "list_subagents", Description: "List this runtime's agent tree.", Parameters: map[string]any{"type": "object", "properties": map[string]any{}}},
		{Name: "launch_subagent", Description: "Launch one child and return immediately. Native agents may launch one child at each of the first two depths; a depth-2 child cannot launch anything. Choose a relevant title made of three words joined by hyphens (for example inspect-api-cache); this is guidance only and is not enforced. A depth-1 child may use the configured child model. A depth-2 child must receive a non-empty real model string or managed short name. The optional harness selects native, Codex, or Claude Code. Do not wait or poll: results arrive as mandatory mid-turn steers at the next API/tool call boundary, or wake an idle parent. In-flight work finishes and its output is retained.", Parameters: map[string]any{"type": "object", "properties": map[string]any{
			"title":              map[string]any{"type": "string", "description": "A relevant three-word dashed title chosen by the parent, such as inspect-api-cache. Guidance only; not enforced."},
			"harness":            map[string]any{"type": "string", "enum": []string{"native", "codex", "claude_code"}, "description": "Harness for the child. Omit for the native slbh provider; use codex or claude_code for a native harness leaf."},
			"model":              map[string]any{"type": "string", "description": "Real provider model ID or a managed short name. Required and non-empty for every depth-2 child; depth 1 may use the configured child default."},
			"effort":             map[string]any{"type": "string"},
			"brief":              map[string]any{"type": "string"},
			"warn_after_seconds": map[string]any{"type": "integer", "description": "Seconds before the parent receives one warning that this child is still running; defaults to 5."},
			"working_dir":        map[string]any{"type": "string"},
		}, "required": []string{"title", "brief"}}},
		{Name: "msg_subagent", Description: "Send a mandatory mid-turn steer to any agent in this runtime, including your parent or siblings. FIFO delivery at the next API/tool call boundary; wakes idle recipients. Never waits for turn completion or cancels in-flight work.", Parameters: map[string]any{"type": "object", "properties": map[string]any{"agent_id": map[string]any{"type": "string"}, "message": map[string]any{"type": "string"}}, "required": []string{"agent_id", "message"}}},
		{Name: "end_subagent", Description: "Stop a child agent.", Parameters: stringArg("agent_id")},
	}...)
	allowed := map[string]bool{}
	for _, name := range job.AvailableInterpreters() {
		allowed[name] = true
	}
	filtered := all[:0]
	for _, tool := range all {
		interpreter := tool.Name
		switch tool.Name {
		case "Bash", "exec_command", "write_stdin":
			interpreter = "bash"
		}
		if interpreter == "bash" || interpreter == "pwsh" || interpreter == "python" {
			if !allowed[interpreter] {
				continue
			}
		}
		filtered = append(filtered, tool)
	}
	return filtered
}

// slbhPrimaryTools is the slbh shape's own editing and shell tools, after the
// read tools and in the order the provider prefix cache has always seen.
func slbhPrimaryTools(stringArg func(string) map[string]any) []provider.Tool {
	return []provider.Tool{
		{Name: "edit_file", Description: "Replace an exact string in a file atomically.", Parameters: map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}, "old": map[string]any{"type": "string"}, "new": map[string]any{"type": "string"}}, "required": []string{"path", "old", "new"}}},
		{Name: "apply_patch", Description: "Apply a unified patch to the working tree.", Parameters: stringArg("patch")},
		{Name: "write_file", Description: "Create a new file; refuse to overwrite an existing file.", Parameters: map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}, "content": map[string]any{"type": "string"}}, "required": []string{"path", "content"}}},
		{Name: "bash", Description: commandDescription(job.BashDescription()), Parameters: commandParameters()},
	}
}

func commandParameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{
		"script": map[string]any{"type": "string"}, "cwd": map[string]any{"type": "string"},
		"wait_seconds":       map[string]any{"type": "integer", "description": "Seconds to wait inline, clamped to 0..30; 0 backgrounds immediately. Defaults to 5."},
		"warn_after_seconds": map[string]any{"type": "integer", "description": "Seconds before one warning to the agent that started the job; defaults to 60. Disabled when not greater than wait_seconds."},
	}, "required": []string{"script"}}
}

func commandDescription(specific string) string {
	return specific + " The script is written to a file and run; the call waits up to wait_seconds (default 5, maximum 30) and returns its output if it finishes. Otherwise it keeps running as a background job, the call returns its job id and output so far, and the full result is delivered automatically when it finishes."
}

func (r *Runtime) toolDefinitions(agentID string) []provider.Tool {
	definitions := buildToolDefinitions(r.toolShape)
	agent, ok := r.lookupAgent(agentID)
	if !ok {
		return definitions
	}
	for index := range definitions {
		if definitions[index].Name != "launch_subagent" {
			continue
		}
		if !r.canLaunch(agent) {
			// Nothing to launch: a leaf. Offering a dead tool only costs context.
			definitions = append(definitions[:index], definitions[index+1:]...)
		}
		break
	}
	return definitions
}

type args struct{ Values map[string]any }

func parseArgs(raw string) (args, error) {
	var a args
	if strings.TrimSpace(raw) == "" {
		return a, nil
	}
	if err := json.Unmarshal([]byte(raw), &a.Values); err != nil {
		return a, fmt.Errorf("tool arguments must be JSON: %w", err)
	}
	return a, nil
}

func (r *Runtime) ExecuteTool(agentID, name, raw string) (string, error) {
	a, err := parseArgs(raw)
	if err != nil {
		return "", err
	}
	base := r.workDir
	if agent, ok := r.lookupAgent(agentID); ok && agent.WorkDir != "" {
		base = agent.WorkDir
	}
	if shaped, foreign := r.shapedDispatch(name); foreign {
		return "", fmt.Errorf("unknown tool %q: this runtime uses the %s tool shape", name, r.toolShape)
	} else if shaped {
		return r.executeShapedTool(agentID, base, name, a.Values)
	}
	switch name {
	case "glob", "grep", "read_file", "read_bytes", "read_lines":
		return readtools.Execute(base, name, raw)
	case "edit_file":
		return r.editFile(base, value(a.Values, "path"), value(a.Values, "old"), value(a.Values, "new"))
	case "apply_patch":
		return r.applyPatch(base, value(a.Values, "patch"))
	case "write_file":
		return r.writeFile(base, value(a.Values, "path"), value(a.Values, "content"))
	case "bash", "pwsh", "python":
		return r.executeCommandTool(agentID, base, name, a.Values)
	case "job":
		action := value(a.Values, "action")
		if action == "list" {
			return jsonString(r.jobs.List())
		}
		j, ok := r.jobs.Get(value(a.Values, "job_id"))
		if !ok {
			return "", fmt.Errorf("job not found")
		}
		if action == "read" {
			out, stderr := j.Output()
			return jsonString(map[string]string{"stdout": out, "stderr": stderr})
		}
		if action == "kill" {
			if err := r.jobs.Kill(value(a.Values, "job_id"), agentID); err != nil {
				return "", err
			}
			return "killed", nil
		}
		return "", fmt.Errorf("unknown job action %q", action)
	case "list_subagents":
		return jsonString(r.Agents())
	case "launch_subagent":
		child, err := r.launchSubagentSpec(agentID, LaunchSpec{Title: value(a.Values, "title"), Harness: value(a.Values, "harness"), Model: value(a.Values, "model"), Effort: value(a.Values, "effort"), Brief: value(a.Values, "brief"), WarnAfterSeconds: intValue(a.Values, "warn_after_seconds"), WorkingDir: value(a.Values, "working_dir")})
		if err != nil {
			return "", err
		}
		return child.ID, nil
	case "msg_subagent":
		child, ok := r.lookupAgent(value(a.Values, "agent_id"))
		if !ok {
			return "", fmt.Errorf("agent not found")
		}
		sender, ok := r.lookupAgent(agentID)
		if !ok {
			return "", fmt.Errorf("sender agent not found")
		}
		message := value(a.Values, "message")
		if strings.TrimSpace(message) == "" {
			return "", fmt.Errorf("message is empty")
		}
		if err := child.steerFrom(sender, message); err != nil {
			return "", err
		}
		return "accepted for delivery at the next API/tool call boundary; idle recipients wake immediately", nil
	case "end_subagent":
		if err := r.endSubagent(agentID, value(a.Values, "agent_id")); err != nil {
			return "", err
		}
		return "stopped", nil
	default:
		return "", fmt.Errorf("unknown tool %q", name)
	}
}

func (r *Runtime) endSubagent(requester, target string) error {
	r.mu.RLock()
	child, ok := r.agents[target]
	requesterAgent, requesterOK := r.agents[requester]
	r.mu.RUnlock()
	if !ok || !requesterOK {
		return fmt.Errorf("agent not found")
	}
	if child.ParentID != requesterAgent.ID {
		return fmt.Errorf("agent is not your child")
	}
	child.stop()
	r.emit(seam.Event{AgentID: child.ID, AgentTitle: child.Title, Kind: "status", Text: "stopped"})
	return nil
}

func value(values map[string]any, key string) string { return valueDefault(values, key, "") }
func valueDefault(values map[string]any, key, fallback string) string {
	if v, ok := values[key].(string); ok {
		return v
	}
	return fallback
}
func intValue(values map[string]any, key string) int {
	if v, ok := values[key].(float64); ok {
		return int(v)
	}
	if v, ok := values[key].(int); ok {
		return v
	}
	return 0
}

func boolValue(values map[string]any, key string) bool {
	value, _ := values[key].(bool)
	return value
}

func uint64Value(values map[string]any, key string) (uint64, error) {
	value, ok := values[key].(float64)
	if !ok || value < 1 || value != float64(uint64(value)) {
		return 0, fmt.Errorf("%s must be a positive integer", key)
	}
	return uint64(value), nil
}

func stringSliceValue(values map[string]any, key string) ([]string, error) {
	raw, present := values[key]
	if !present {
		return nil, nil
	}
	items, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("%s must be an array of strings", key)
	}
	result := make([]string, len(items))
	for index, item := range items {
		text, ok := item.(string)
		if !ok {
			return nil, fmt.Errorf("%s must be an array of strings", key)
		}
		result[index] = text
	}
	return result, nil
}
func jsonString(value any) (string, error) {
	b, err := json.MarshalIndent(value, "", "  ")
	return string(b), err
}

func (r *Runtime) resolvePath(base, path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("path is required")
	}
	if runtime.GOOS == "windows" && filepath.IsAbs(path) && filepath.VolumeName(path) == "" {
		return "", fmt.Errorf("rooted path %q has no drive; use a drive-letter path or a relative path", path)
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(base, path)
	}
	clean, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return clean, nil
}

func (r *Runtime) editFile(base, path, old, replacement string) (string, error) {
	file, err := r.resolvePath(base, path)
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return "", err
	}
	text := string(b)
	count := strings.Count(text, old)
	if count != 1 {
		return "", fmt.Errorf("replacement matched %d locations; expected exactly one", count)
	}
	text = strings.Replace(text, old, replacement, 1)
	return "updated", atomicReplace(file, []byte(text), infoMode(file))
}

func (r *Runtime) writeFile(base, path, content string) (string, error) {
	file, err := r.resolvePath(base, path)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(file); err == nil {
		return "", fmt.Errorf("file already exists: %s; use edit_file or apply_patch to change it", file)
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return "", err
	}
	f, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return "", fmt.Errorf("file already exists: %s; use edit_file or apply_patch to change it", file)
		}
		return "", err
	}
	if _, err = f.WriteString(content); err != nil {
		_ = f.Close()
		return "", err
	}
	return "created " + file, f.Close()
}

func infoMode(path string) os.FileMode {
	if info, err := os.Stat(path); err == nil {
		return info.Mode().Perm()
	}
	return 0o600
}

func atomicReplace(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".slbh-edit-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err == nil {
		return nil
	}
	// Windows cannot replace an existing file with Rename. The content is
	// already durable before this small platform-specific fallback.
	if err := os.Remove(path); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func (r *Runtime) applyPatch(base, patch string) (string, error) {
	if strings.TrimSpace(patch) == "" {
		return "", fmt.Errorf("patch is empty")
	}
	if strings.HasPrefix(strings.TrimSpace(patch), "*** Begin Patch") {
		if err := r.applyAnthropicPatch(base, patch); err != nil {
			return "", err
		}
		return "applied", nil
	}
	cmd := exec.Command("git", "apply", "--unsafe-paths", "--whitespace=nowarn", "-")
	cmd.Dir = base
	cmd.Stdin = strings.NewReader(patch)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git apply: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return "applied", nil
}

func (r *Runtime) applyAnthropicPatch(base, patch string) error {
	lines := strings.Split(strings.ReplaceAll(patch, "\r\n", "\n"), "\n")
	if len(lines) < 2 || strings.TrimSpace(lines[0]) != "*** Begin Patch" {
		return fmt.Errorf("patch must start with *** Begin Patch")
	}
	for i := 1; i < len(lines); {
		if lines[i] == "" || lines[i] == "*** End Patch" {
			i++
			continue
		}
		header := lines[i]
		if strings.HasPrefix(header, "*** Add File: ") {
			path := strings.TrimSpace(strings.TrimPrefix(header, "*** Add File: "))
			i++
			var content []string
			for i < len(lines) && !strings.HasPrefix(lines[i], "*** ") {
				if !strings.HasPrefix(lines[i], "+") {
					return fmt.Errorf("add file %q contains a non-add line", path)
				}
				content = append(content, strings.TrimPrefix(lines[i], "+"))
				i++
			}
			file, err := r.resolvePath(base, path)
			if err != nil {
				return err
			}
			if _, err := os.Stat(file); err == nil {
				return fmt.Errorf("add file %q already exists", path)
			}
			if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(file, []byte(strings.Join(content, "\n")), 0o600); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(header, "*** Delete File: ") {
			path := strings.TrimSpace(strings.TrimPrefix(header, "*** Delete File: "))
			file, err := r.resolvePath(base, path)
			if err != nil {
				return err
			}
			if err := os.Remove(file); err != nil {
				return fmt.Errorf("delete file %q: %w", path, err)
			}
			i++
			continue
		}
		if strings.HasPrefix(header, "*** Update File: ") {
			path := strings.TrimSpace(strings.TrimPrefix(header, "*** Update File: "))
			file, err := r.resolvePath(base, path)
			if err != nil {
				return err
			}
			original, err := os.ReadFile(file)
			if err != nil {
				return err
			}
			hadNewline := strings.HasSuffix(string(original), "\n")
			fileLines := strings.Split(strings.TrimSuffix(string(original), "\n"), "\n")
			if len(fileLines) == 1 && fileLines[0] == "" && !hadNewline {
				fileLines = nil
			}
			i++
			for i < len(lines) && !strings.HasPrefix(lines[i], "*** ") {
				if !strings.HasPrefix(lines[i], "@@") {
					i++
					continue
				}
				i++
				var oldLines, newLines []string
				for i < len(lines) && !strings.HasPrefix(lines[i], "@@") && !strings.HasPrefix(lines[i], "*** ") {
					line := lines[i]
					if line == "" {
						oldLines = append(oldLines, "")
						newLines = append(newLines, "")
						i++
						continue
					}
					switch line[0] {
					case ' ':
						oldLines = append(oldLines, line[1:])
						newLines = append(newLines, line[1:])
					case '-':
						oldLines = append(oldLines, line[1:])
					case '+':
						newLines = append(newLines, line[1:])
					default:
						return fmt.Errorf("invalid update line %q", line)
					}
					i++
				}
				at := findLines(fileLines, oldLines)
				if at < 0 {
					return fmt.Errorf("hunk for %q did not match", path)
				}
				replaced := append([]string{}, fileLines[:at]...)
				replaced = append(replaced, newLines...)
				replaced = append(replaced, fileLines[at+len(oldLines):]...)
				fileLines = replaced
			}
			content := strings.Join(fileLines, "\n")
			if hadNewline {
				content += "\n"
			}
			if err := atomicReplace(file, []byte(content), infoMode(file)); err != nil {
				return err
			}
			continue
		}
		return fmt.Errorf("unknown patch header %q", header)
	}
	return nil
}

func findLines(haystack, needle []string) int {
	if len(needle) == 0 {
		return len(haystack)
	}
	found := -1
	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j := range needle {
			if haystack[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			if found >= 0 {
				return -2
			}
			found = i
		}
	}
	return found
}

func (r *Runtime) executeCommandTool(agentID, base, name string, values map[string]any) (string, error) {
	script := value(values, "script")
	if script == "" {
		return "", fmt.Errorf("script is required")
	}
	cwd := valueDefault(values, "cwd", base)
	if cwd == "" {
		cwd = base
	}
	cwd, err := r.resolvePath(base, cwd)
	if err != nil {
		return "", err
	}
	wait := intValue(values, "wait_seconds")
	if _, ok := values["wait_seconds"]; !ok {
		wait = 5
	}
	if wait < 0 {
		wait = 0
	}
	if wait > 30 {
		wait = 30
	}
	warn := intValue(values, "warn_after_seconds")
	if _, ok := values["warn_after_seconds"]; !ok {
		warn = 60
	}
	if warn <= wait {
		warn = 0
	}
	env := []string{}
	scratch := filepath.Join(r.runtimeDir, "agents", agentID, "scratch")
	env = append(env, "TMPDIR="+scratch, "TMP="+scratch, "TEMP="+scratch)
	if name == "python" {
		env = append(env, "PYTHONPATH="+cwd+string(os.PathListSeparator)+os.Getenv("PYTHONPATH"))
	}
	root := filepath.Join(r.runtimeDir, "agents", agentID, "jobs")
	j, err := r.jobs.Start(r.ctx, job.Spec{Author: agentID, Script: script, Interpreter: name, ScriptRoot: root, ToolName: name, Dir: cwd, WarnAfter: time.Duration(warn) * time.Second, Environment: env, Inline: wait > 0})
	if err != nil {
		return "", err
	}
	snap, stdout, stderr, background := r.jobs.Wait(j, time.Duration(wait)*time.Second)
	if background {
		return backgroundOutput(snap, stdout, stderr, time.Duration(wait)*time.Second), nil
	}
	output := stdout
	if stderr != "" {
		output += "\nstderr:\n" + stderr
	}
	if snap.Status == job.Killed {
		if output == "" {
			return fmt.Sprintf("job %s killed", snap.ID), nil
		}
		return fmt.Sprintf("job %s killed\n%s", snap.ID, output), nil
	}
	if snap.Status == job.Failed {
		return output, fmt.Errorf("exit status %d", snap.ExitCode)
	}
	return output, nil
}

func backgroundOutput(s job.Snapshot, stdout, stderr string, waited time.Duration) string {
	return fmt.Sprintf("job %s is still running after %s; its result will be delivered automatically when it finishes\nstdout:\n%s\nstderr:\n%s", s.ID, waited, tailOutput(stdout), tailOutput(stderr))
}
func tailOutput(s string) string {
	if len(s) <= 8000 {
		return s
	}
	return "[output truncated]\n" + s[len(s)-8000:]
}
