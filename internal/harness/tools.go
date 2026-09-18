package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/slbdotdev/slbh/internal/job"
	"github.com/slbdotdev/slbh/internal/orgstore"
	"github.com/slbdotdev/slbh/internal/provider"
	"github.com/slbdotdev/slbh/internal/readtools"
	"github.com/slbdotdev/slbh/internal/seam"
	"github.com/slbdotdev/slbh/internal/secretarywake"
)

// ToolDefinitions is the stable tool prefix sent to every provider request.
// Keep ordering stable: provider prefix caching keys include this schema.
func ToolDefinitions() []provider.Tool {
	stringArg := func(name string) map[string]any {
		return map[string]any{"type": "object", "properties": map[string]any{name: map[string]any{"type": "string"}}, "required": []string{name}}
	}
	definitions := readtools.Definitions()
	return append(definitions, []provider.Tool{
		{Name: "edit_file", Description: "Replace an exact string in a file atomically.", Parameters: map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}, "old": map[string]any{"type": "string"}, "new": map[string]any{"type": "string"}}, "required": []string{"path", "old", "new"}}},
		{Name: "apply_patch", Description: "Apply a unified patch to the working tree.", Parameters: stringArg("patch")},
		{Name: "write_file", Description: "Create a new file; refuse to overwrite an existing file.", Parameters: map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}, "content": map[string]any{"type": "string"}}, "required": []string{"path", "content"}}},
		{Name: "quick_bash", Description: "Run a foreground shell command with a five second timeout.", Parameters: map[string]any{"type": "object", "properties": map[string]any{"script": map[string]any{"type": "string"}, "cwd": map[string]any{"type": "string"}}, "required": []string{"script"}}},
		{Name: "long_job", Description: "Start a non-blocking background shell job. Returns its job id immediately; the job's captured output is delivered to you automatically when it finishes, and read_job returns what it has captured so far at any point while it runs.", Parameters: map[string]any{"type": "object", "properties": map[string]any{"script": map[string]any{"type": "string"}, "cwd": map[string]any{"type": "string"}, "warn_after_seconds": map[string]any{"type": "integer", "description": "Seconds to wait before the harness sends YOU, the agent starting this job, one message saying the job is still running. That message is delivered into your context at the next API/tool call boundary and wakes you if you have gone idle. You are the audience: no human is asked to act on it. It is sent once and never repeated, and what to do about it is your decision: kill_job, keep waiting for the job's result, or carry on with other work. Defaults to 5 seconds."}}, "required": []string{"script"}}},
		{Name: "quick_py", Description: "Run Python code with the managed scientific environment and a five second timeout.", Parameters: map[string]any{"type": "object", "properties": map[string]any{"script": map[string]any{"type": "string"}, "cwd": map[string]any{"type": "string"}}, "required": []string{"script"}}},
		{Name: "long_py", Description: "Start a non-blocking background Python job in the managed scientific environment. Returns its job id immediately; the job's captured output is delivered to you automatically when it finishes, and read_job returns what it has captured so far at any point while it runs.", Parameters: map[string]any{"type": "object", "properties": map[string]any{"script": map[string]any{"type": "string"}, "cwd": map[string]any{"type": "string"}, "warn_after_seconds": map[string]any{"type": "integer", "description": "Seconds to wait before the harness sends YOU, the agent starting this job, one message saying the job is still running. That message is delivered into your context at the next API/tool call boundary and wakes you if you have gone idle. You are the audience: no human is asked to act on it. It is sent once and never repeated, and what to do about it is your decision: kill_job, keep waiting for the job's result, or carry on with other work. Defaults to 5 seconds."}}, "required": []string{"script"}}},
		{Name: "list_jobs", Description: "List all jobs in this runtime.", Parameters: map[string]any{"type": "object", "properties": map[string]any{}}},
		{Name: "read_job", Description: "Read current stdout and stderr for a job.", Parameters: stringArg("job_id")},
		{Name: "kill_job", Description: "Kill a job owned by the calling agent.", Parameters: stringArg("job_id")},
		{Name: "list_subagents", Description: "List this runtime's agent tree.", Parameters: map[string]any{"type": "object", "properties": map[string]any{}}},
		{Name: "launch_subagent", Description: "Launch a child agent up to depth two; returns immediately. The parent chooses a relevant title made of three words joined by hyphens (for example inspect-api-cache); this is guidance only and is not enforced. Omit model for a native child to use its configured default. For a Codex leaf, set harness to codex and pass the exact ChatGPT model slug in model. For an Opus leaf, set harness to claude_code and pass the exact Claude model slug in model. Native harness leaves alone use native defaults. Honor an explicit user model request. Do not wait or poll: results arrive as mandatory mid-turn steers at the next API/tool call boundary, or wake an idle parent. In-flight work finishes and its output is retained.", Parameters: map[string]any{"type": "object", "properties": map[string]any{
			"title":              map[string]any{"type": "string", "description": "A relevant three-word dashed title chosen by the parent, such as inspect-api-cache. Guidance only; not enforced."},
			"harness":            map[string]any{"type": "string", "enum": []string{"native", "codex", "claude_code"}, "description": "Harness for the child. Omit for native; use codex for a headless Codex ChatGPT leaf or claude_code for a headless Claude Code leaf."},
			"model":              map[string]any{"type": "string", "description": "Model ID. For codex or claude_code, pass the exact model slug available to that harness; do not use a native default."},
			"effort":             map[string]any{"type": "string"},
			"brief":              map[string]any{"type": "string"},
			"warn_after_seconds": map[string]any{"type": "integer"},
			"working_dir":        map[string]any{"type": "string"},
		}, "required": []string{"title", "brief"}}},
		{Name: "msg_subagent", Description: "Send a mandatory mid-turn steer to any agent in this runtime, including your parent or siblings. FIFO delivery at the next API/tool call boundary; wakes idle recipients. Never waits for turn completion or cancels in-flight work.", Parameters: map[string]any{"type": "object", "properties": map[string]any{"agent_id": map[string]any{"type": "string"}, "message": map[string]any{"type": "string"}}, "required": []string{"agent_id", "message"}}},
		{Name: "end_subagent", Description: "Stop a child agent.", Parameters: stringArg("agent_id")},
	}...)
}

func seatToolDefinitions() []provider.Tool {
	return []provider.Tool{
		{Name: "report_to_secretary", Description: "Append a durable Seat report to the org inbox. Supply exactly one of invalidates or invalidates_none.", Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"text":             map[string]any{"type": "string"},
				"invalidates":      map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				"invalidates_none": map[string]any{"type": "boolean"},
			},
			"required": []string{"text"},
		}},
		{Name: "org_requests", Description: "Return the Secretary request queue with current status and full history as JSON.", Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"open_only": map[string]any{"type": "boolean"},
			},
		}},
		{Name: "update_request", Description: "Append an accepted, declined, or done status to a Secretary request.", Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id":     map[string]any{"type": "integer", "minimum": 1},
				"status": map[string]any{"type": "string", "enum": []string{"accepted", "declined", "done"}},
				"note":   map[string]any{"type": "string"},
			},
			"required": []string{"id", "status"},
		}},
	}
}

func (r *Runtime) toolDefinitions(agentID string) []provider.Tool {
	definitions := ToolDefinitions()
	agent, ok := r.lookupAgent(agentID)
	if !ok || agent.Depth != 0 {
		return definitions
	}
	return append(definitions, seatToolDefinitions()...)
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
	switch name {
	case "report_to_secretary", "org_requests", "update_request":
		agent, ok := r.lookupAgent(agentID)
		if !ok || agent.Depth != 0 {
			return "", fmt.Errorf("tool %q is available only to the depth-0 Seat", name)
		}
		return r.executeSeatTool(name, a.Values)
	case "glob", "grep", "read_file", "read_bytes", "read_lines":
		return readtools.Execute(base, name, raw)
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
		workingDir, err := r.resolvePath(base, valueDefault(a.Values, "cwd", base))
		if err != nil {
			return "", err
		}
		j, err := r.jobs.Start(r.ctx, jobSpec(agentID, value(a.Values, "script"), time.Duration(seconds)*time.Second, workingDir))
		if err != nil {
			return "", err
		}
		return j.Snapshot().ID, nil
	case "quick_py":
		return r.quickPy(agentID, base, value(a.Values, "script"), valueDefault(a.Values, "cwd", base))
	case "long_py":
		seconds := intValue(a.Values, "warn_after_seconds")
		if seconds == 0 {
			seconds = 5
		}
		workingDir, err := r.resolvePath(base, valueDefault(a.Values, "cwd", base))
		if err != nil {
			return "", err
		}
		j, err := r.jobs.Start(r.ctx, pythonJobSpec(agentID, value(a.Values, "script"), time.Duration(seconds)*time.Second, workingDir))
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

func (r *Runtime) executeSeatTool(name string, values map[string]any) (string, error) {
	if r.orgStore == nil {
		return "", fmt.Errorf("org store is unavailable")
	}
	switch name {
	case "report_to_secretary":
		text := strings.TrimSpace(value(values, "text"))
		if text == "" {
			return "", fmt.Errorf("text is required")
		}
		invalidates, err := stringSliceValue(values, "invalidates")
		if err != nil {
			return "", err
		}
		report, err := r.orgStore.AppendReport("seat", text, invalidates, boolValue(values, "invalidates_none"))
		if err != nil {
			return "", err
		}
		pending, err := r.orgStore.Pending()
		if err != nil {
			return "", err
		}
		if r.Config().SecretaryWake {
			message := secretarywake.InboxMessage(len(pending))
			go func() {
				_, wakeErr := secretarywake.Wake(r.ctx, secretarywake.Options{CodexCommand: r.codexCommand, SessionName: r.Config().SecretarySession}, message)
				if wakeErr != nil {
					r.emitStatus("secretary_wake", wakeErr.Error())
				}
			}()
		}
		return jsonString(report)
	case "org_requests":
		requests, err := r.orgStore.Requests()
		if err != nil {
			return "", err
		}
		if boolValue(values, "open_only") {
			open := requests[:0]
			for _, request := range requests {
				if request.Status == orgstore.StatusQueued || request.Status == orgstore.StatusAccepted {
					open = append(open, request)
				}
			}
			requests = open
		}
		if requests == nil {
			requests = []orgstore.Request{}
		}
		return jsonString(requests)
	case "update_request":
		id, err := uint64Value(values, "id")
		if err != nil {
			return "", err
		}
		change, err := r.orgStore.UpdateRequestStatus(id, orgstore.Status(value(values, "status")), value(values, "note"))
		if err != nil {
			return "", err
		}
		return jsonString(change)
	default:
		return "", fmt.Errorf("unknown Seat tool %q", name)
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

func (r *Runtime) quickBash(agentID, base, script, cwd string) (string, error) {
	if script == "" {
		return "", fmt.Errorf("script is required")
	}
	if cwd == "" {
		cwd = base
	} else if cwd != base {
		var err error
		cwd, err = r.resolvePath(base, cwd)
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
	return stdout.String(), nil
}

func (r *Runtime) quickPy(agentID, base, script, cwd string) (string, error) {
	if script == "" {
		return "", fmt.Errorf("script is required")
	}
	if cwd == "" {
		cwd = base
	} else if cwd != base {
		var err error
		cwd, err = r.resolvePath(base, cwd)
		if err != nil {
			return "", err
		}
	}
	ctx, cancel := context.WithTimeout(r.ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, pythonExecutable(), "-c", script)
	cmd.Dir = cwd
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		return "", fmt.Errorf("quick_py timed out")
	}
	if err != nil {
		return stdout.String() + stderr.String(), err
	}
	return stdout.String(), nil
}

func jobSpec(agentID, script string, warn time.Duration, dir string) job.Spec {
	return job.Spec{Author: agentID, Script: script, ToolName: "long_job", WarnAfter: warn, Dir: dir}
}

func pythonJobSpec(agentID, script string, warn time.Duration, dir string) job.Spec {
	return job.Spec{Author: agentID, Script: script, Command: pythonCommand(script), ToolName: "long_py", WarnAfter: warn, Dir: dir}
}
