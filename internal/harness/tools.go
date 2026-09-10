package harness

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/slbdotdev/slbh/internal/job"
	"github.com/slbdotdev/slbh/internal/provider"
)

// ToolDefinitions is the stable tool prefix sent to every provider request.
// Keep ordering stable: provider prefix caching keys include this schema.
func ToolDefinitions() []provider.Tool {
	stringArg := func(name string) map[string]any {
		return map[string]any{"type": "object", "properties": map[string]any{name: map[string]any{"type": "string"}}, "required": []string{name}}
	}
	return []provider.Tool{
		{Name: "glob", Description: "Find files by a glob pattern under the working directory.", Parameters: stringArg("pattern")},
		{Name: "grep", Description: "Search text using a regular expression.", Parameters: map[string]any{"type": "object", "properties": map[string]any{"pattern": map[string]any{"type": "string"}, "path": map[string]any{"type": "string"}}, "required": []string{"pattern"}}},
		{Name: "read_file", Description: "Read a whole file up to 100k bytes.", Parameters: stringArg("path")},
		{Name: "read_bytes", Description: "Read an inclusive byte range from a file.", Parameters: map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}, "start": map[string]any{"type": "integer"}, "end": map[string]any{"type": "integer"}}, "required": []string{"path", "start", "end"}}},
		{Name: "read_lines", Description: "Read an inclusive line range from a file.", Parameters: map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}, "start": map[string]any{"type": "integer"}, "end": map[string]any{"type": "integer"}}, "required": []string{"path", "start", "end"}}},
		{Name: "edit_file", Description: "Replace an exact string in a file atomically.", Parameters: map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}, "old": map[string]any{"type": "string"}, "new": map[string]any{"type": "string"}}, "required": []string{"path", "old", "new"}}},
		{Name: "apply_patch", Description: "Apply a unified patch to the working tree.", Parameters: stringArg("patch")},
		{Name: "write_file", Description: "Create a new file; refuse to overwrite an existing file.", Parameters: map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}, "content": map[string]any{"type": "string"}}, "required": []string{"path", "content"}}},
		{Name: "quick_bash", Description: "Run a foreground shell command with a five second timeout.", Parameters: map[string]any{"type": "object", "properties": map[string]any{"script": map[string]any{"type": "string"}, "cwd": map[string]any{"type": "string"}}, "required": []string{"script"}}},
		{Name: "long_job", Description: "Start a non-blocking background shell job.", Parameters: map[string]any{"type": "object", "properties": map[string]any{"script": map[string]any{"type": "string"}, "cwd": map[string]any{"type": "string"}, "warn_after_seconds": map[string]any{"type": "integer"}}, "required": []string{"script"}}},
		{Name: "list_jobs", Description: "List all jobs in this runtime.", Parameters: map[string]any{"type": "object", "properties": map[string]any{}}},
		{Name: "read_job", Description: "Read current stdout and stderr for a job.", Parameters: stringArg("job_id")},
		{Name: "kill_job", Description: "Kill a job owned by the calling agent.", Parameters: stringArg("job_id")},
		{Name: "list_subagents", Description: "List this runtime's agent tree.", Parameters: map[string]any{"type": "object", "properties": map[string]any{}}},
		{Name: "launch_subagent", Description: "Launch a child agent up to depth two; returns immediately. Omit model to use the configured subagent default and choose from the approved model guidance. Honor an explicit user request for another model. Do not wait or poll: results arrive as mandatory mid-turn steers at the next API/tool call boundary, or wake an idle parent. In-flight work finishes and its output is retained.", Parameters: map[string]any{"type": "object", "properties": map[string]any{"title": map[string]any{"type": "string"}, "harness": map[string]any{"type": "string"}, "model": map[string]any{"type": "string", "description": "Optional model ID; use the approved model guidance unless the user explicitly requests another model."}, "effort": map[string]any{"type": "string"}, "brief": map[string]any{"type": "string"}, "warn_after_seconds": map[string]any{"type": "integer"}, "working_dir": map[string]any{"type": "string"}}, "required": []string{"title", "brief"}}},
		{Name: "msg_subagent", Description: "Send a mandatory mid-turn steer to any agent in this runtime, including your parent or siblings. FIFO delivery at the next API/tool call boundary; wakes idle recipients. Never waits for turn completion or cancels in-flight work.", Parameters: map[string]any{"type": "object", "properties": map[string]any{"agent_id": map[string]any{"type": "string"}, "message": map[string]any{"type": "string"}}, "required": []string{"agent_id", "message"}}},
		{Name: "end_subagent", Description: "Stop a child agent.", Parameters: stringArg("agent_id")},
	}
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
	if agent, ok := r.Agent(agentID); ok && agent.WorkDir != "" {
		base = agent.WorkDir
	}
	switch name {
	case "glob":
		return r.glob(base, value(a.Values, "pattern"))
	case "grep":
		return r.grep(base, value(a.Values, "pattern"), valueDefault(a.Values, "path", "."))
	case "read_file":
		return r.readFile(base, value(a.Values, "path"))
	case "read_bytes":
		return r.readBytes(base, value(a.Values, "path"), intValue(a.Values, "start"), intValue(a.Values, "end"))
	case "read_lines":
		return r.readLines(base, value(a.Values, "path"), intValue(a.Values, "start"), intValue(a.Values, "end"))
	case "edit_file":
		return r.editFile(base, value(a.Values, "path"), value(a.Values, "old"), value(a.Values, "new"))
	case "apply_patch":
		return r.applyPatch(base, value(a.Values, "patch"))
	case "write_file":
		return r.writeFile(base, value(a.Values, "path"), value(a.Values, "content"))
	case "quick_bash":
		return r.quickBash(agentID, base, value(a.Values, "script"), valueDefault(a.Values, "cwd", base))
	case "long_job":
		seconds := intValue(a.Values, "warn_after_seconds")
		if seconds == 0 {
			seconds = 5
		}
		workingDir := valueDefault(a.Values, "cwd", base)
		if workingDir != base {
			workingDir, err = r.resolveFrom(base, workingDir)
			if err != nil {
				return "", err
			}
		}
		j, err := r.jobs.Start(r.ctx, jobSpec(agentID, value(a.Values, "script"), time.Duration(seconds)*time.Second, workingDir))
		if err != nil {
			return "", err
		}
		return j.Snapshot().ID, nil
	case "list_jobs":
		return jsonString(r.jobs.List())
	case "read_job":
		j, ok := r.jobs.Get(value(a.Values, "job_id"))
		if !ok {
			return "", fmt.Errorf("job not found")
		}
		out, stderr := j.Output()
		return jsonString(map[string]string{"stdout": out, "stderr": stderr})
	case "kill_job":
		if err := r.jobs.Kill(value(a.Values, "job_id"), agentID); err != nil {
			return "", err
		}
		return "killed", nil
	case "list_subagents":
		return jsonString(r.Agents())
	case "launch_subagent":
		child, err := r.LaunchSubagentSpec(agentID, LaunchSpec{Title: value(a.Values, "title"), Harness: value(a.Values, "harness"), Model: value(a.Values, "model"), Effort: value(a.Values, "effort"), Brief: value(a.Values, "brief"), WarnAfterSeconds: intValue(a.Values, "warn_after_seconds"), WorkingDir: value(a.Values, "working_dir")})
		if err != nil {
			return "", err
		}
		return child.ID, nil
	case "msg_subagent":
		child, ok := r.Agent(value(a.Values, "agent_id"))
		if !ok {
			return "", fmt.Errorf("agent not found")
		}
		sender, ok := r.Agent(agentID)
		if !ok {
			return "", fmt.Errorf("sender agent not found")
		}
		message := value(a.Values, "message")
		if strings.TrimSpace(message) == "" {
			return "", fmt.Errorf("message is empty")
		}
		if err := child.Steer(fmt.Sprintf("[from %s (%s)] %s", sender.Title, sender.ID, message)); err != nil {
			return "", err
		}
		return "accepted for delivery at the next API/tool call boundary; idle recipients wake immediately", nil
	case "end_subagent":
		if err := r.EndSubagent(agentID, value(a.Values, "agent_id")); err != nil {
			return "", err
		}
		return "stopped", nil
	default:
		return "", fmt.Errorf("unknown tool %q", name)
	}
}

func (r *Runtime) EndSubagent(requester, target string) error {
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
	r.emit(Event{AgentID: child.ID, AgentTitle: child.Title, Kind: "status", Text: "stopped"})
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
func jsonString(value any) (string, error) {
	b, err := json.MarshalIndent(value, "", "  ")
	return string(b), err
}

func (r *Runtime) resolve(path string) (string, error) {
	return r.resolveFrom(r.workDir, path)
}

func (r *Runtime) resolveFrom(base, path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("path is required")
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(base, path)
	}
	clean, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	root, err := filepath.Abs(base)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, clean)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path is outside working directory")
	}
	return clean, nil
}

func (r *Runtime) glob(base, pattern string) (string, error) {
	if pattern == "" {
		return "", fmt.Errorf("pattern is required")
	}
	pattern = filepath.Join(base, pattern)
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return "", err
	}
	for i := range matches {
		matches[i], _ = filepath.Rel(base, matches[i])
	}
	sort.Strings(matches)
	return strings.Join(matches, "\n"), nil
}

func (r *Runtime) grep(base, pattern, path string) (string, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return "", err
	}
	root, err := r.resolveFrom(base, path)
	if err != nil {
		return "", err
	}
	var out strings.Builder
	err = filepath.Walk(root, func(file string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			return nil
		}
		if info.Size() > 2*1024*1024 {
			return nil
		}
		f, err := os.Open(file)
		if err != nil {
			return nil
		}
		defer f.Close()
		scanner := bufio.NewScanner(f)
		line := 0
		for scanner.Scan() {
			line++
			if re.MatchString(scanner.Text()) {
				rel, _ := filepath.Rel(base, file)
				fmt.Fprintf(&out, "%s:%d:%s\n", rel, line, scanner.Text())
			}
		}
		return scanner.Err()
	})
	return out.String(), err
}

func (r *Runtime) readFile(base, path string) (string, error) {
	file, err := r.resolveFrom(base, path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(file)
	if err != nil {
		return "", err
	}
	if info.Size() > 100*1024 {
		data, _ := os.ReadFile(file)
		return "", fmt.Errorf("file is %d bytes, %d lines, type %s; use read_lines or read_bytes instead", info.Size(), bytes.Count(data, []byte{'\n'})+1, detectType(file))
	}
	b, err := os.ReadFile(file)
	return string(b), err
}

func (r *Runtime) readBytes(base, path string, start, end int) (string, error) {
	if start < 0 || end < start || end-start+1 > 100*1024 {
		return "", fmt.Errorf("byte range must be zero-based, inclusive, and at most 100k bytes")
	}
	file, err := r.resolveFrom(base, path)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return "", err
	}
	if start >= len(data) {
		return "", fmt.Errorf("byte start %d is past file size %d", start, len(data))
	}
	if end >= len(data) {
		end = len(data) - 1
	}
	return string(data[start : end+1]), nil
}

func detectType(path string) string {
	file, err := os.Open(path)
	if err != nil {
		return "unknown"
	}
	defer file.Close()
	var header [512]byte
	n, _ := file.Read(header[:])
	return http.DetectContentType(header[:n])
}

func (r *Runtime) readLines(base, path string, start, end int) (string, error) {
	if start < 1 || end < start {
		return "", fmt.Errorf("line range must be one-based and inclusive")
	}
	file, err := r.resolveFrom(base, path)
	if err != nil {
		return "", err
	}
	f, err := os.Open(file)
	if err != nil {
		return "", err
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	var out strings.Builder
	line := 0
	for s.Scan() {
		line++
		if line >= start && line <= end {
			fmt.Fprintf(&out, "%d:%s\n", line, s.Text())
		}
		if line > end {
			break
		}
	}
	return out.String(), s.Err()
}

func (r *Runtime) editFile(base, path, old, replacement string) (string, error) {
	file, err := r.resolveFrom(base, path)
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
	file, err := r.resolveFrom(base, path)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(file); err == nil {
		return "", fmt.Errorf("file already exists")
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return "", err
	}
	f, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	if _, err = f.WriteString(content); err != nil {
		_ = f.Close()
		return "", err
	}
	return "created", f.Close()
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
	cmd := exec.Command("git", "apply", "--whitespace=nowarn", "-")
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
			file, err := r.resolveFrom(base, path)
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
			file, err := r.resolveFrom(base, path)
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
			file, err := r.resolveFrom(base, path)
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

func (r *Runtime) quickBash(agentID, base, script, cwd string) (string, error) {
	if script == "" {
		return "", fmt.Errorf("script is required")
	}
	if cwd == "" {
		cwd = base
	} else if cwd != base {
		var err error
		cwd, err = r.resolveFrom(base, cwd)
		if err != nil {
			return "", err
		}
	}
	ctx, cancel := context.WithTimeout(r.ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, shellName(), shellArgs(script)...)
	cmd.Dir = cwd
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		return "", fmt.Errorf("quick_bash timed out")
	}
	if err != nil {
		return stdout.String() + stderr.String(), err
	}
	r.emit(Event{AgentID: agentID, Kind: "tool_result", Text: stdout.String()})
	return stdout.String(), nil
}

func jobSpec(agentID, script string, warn time.Duration, dir string) job.Spec {
	return job.Spec{Author: agentID, Script: script, WarnAfter: warn, Dir: dir}
}
