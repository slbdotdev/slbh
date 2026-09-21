package harness

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/slbdotdev/slbh/internal/config"
	"github.com/slbdotdev/slbh/internal/job"
	"github.com/slbdotdev/slbh/internal/provider"
)

// Tool shapes replace the native agents' primary tools — read, edit, create
// and shell — with the set another harness gives its model. Names, parameter
// schemas and result text follow the harness as captured from its installed
// build (testdata/toolshape); descriptions are condensed where the original
// carries protocol that means nothing inside slbh. Everything that is not a
// primary tool — python, pwsh, job and the subagent tools — is the same in
// every shape.

// shapedToolError is a failed call whose text is the shape's own and reaches
// the model verbatim, without slbh's "tool error:" rendering. flag marks the
// result as an error on wires that carry one (Anthropic's is_error).
type shapedToolError struct {
	text string
	flag bool
}

func (e *shapedToolError) Error() string { return e.text }

func anthropicFailure(text string) error { return &shapedToolError{text: text, flag: true} }

func toolUseError(text string) error {
	return anthropicFailure("<tool_use_error>" + text + "</tool_use_error>")
}

// shapeState is the per-runtime state a shape keeps between calls: the
// Anthropic shell's working directory and read times per agent, and the Codex
// exec sessions.
type shapeState struct {
	mu          sync.Mutex
	cwd         map[string]string
	reads       map[string]map[string]time.Time
	sessions    map[int]*codexSession
	nextSession int
	notes       map[string]execNote
}

type codexSession struct {
	agentID string
	job     *job.Job
	offset  int
}

func newShapeState() *shapeState {
	return &shapeState{cwd: map[string]string{}, reads: map[string]map[string]time.Time{}, sessions: map[int]*codexSession{}, nextSession: 1000 + rand.IntN(60000)}
}

var primaryToolNames = map[string][]string{
	config.ToolShapeLean:      {"apply_patch", "bash"},
	config.ToolShapeMid:       {"read_file", "apply_patch", "bash"},
	config.ToolShapeFull:      {"glob", "grep", "read_file", "read_bytes", "read_lines", "edit_file", "apply_patch", "write_file", "bash"},
	config.ToolShapeAnthropic: {"Bash", "Read", "Edit", "Write"},
	config.ToolShapeCodex:     {"exec_command", "write_stdin", "apply_patch"},
}

// shellToolNames are the shape's own command tools, as the baked prompt names them.
func shellToolNames(shape string) []string {
	switch shape {
	case config.ToolShapeAnthropic:
		return []string{"Bash"}
	case config.ToolShapeCodex:
		return []string{"exec_command"}
	}
	return []string{"bash"}
}

// commandToolNames lists what the baked prompt calls the agent's command
// tools: the shape's shell plus the shared interpreters.
func commandToolNames(shape string) []string {
	available := job.AvailableInterpreters()
	var names []string
	for _, name := range available {
		if name == "bash" {
			names = append(names, shellToolNames(shape)...)
			continue
		}
		names = append(names, name)
	}
	return names
}

func objectSchema(properties map[string]any, required ...string) map[string]any {
	return map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}
}

func prop(kind, description string) map[string]any {
	property := map[string]any{"type": kind}
	if description != "" {
		property["description"] = description
	}
	return property
}

const anthropicBashDescription = "Executes a given bash command and returns its output.\n\n" +
	"The working directory persists between commands, but shell state does not.\n\n" +
	"IMPORTANT: Avoid using this tool to run `cat`, `head`, `tail`, `sed`, `awk`, or `echo` commands, unless explicitly instructed or after you have verified that a dedicated tool cannot accomplish your task. Instead, use the appropriate dedicated tool:\n\n" +
	" - Read files: Use Read (NOT cat/head/tail)\n" +
	" - Edit files: Use Edit (NOT sed/awk)\n" +
	" - Write files: Use Write (NOT echo >/cat <<EOF)\n" +
	" - Communication: Output text directly (NOT echo/printf)\n\n" +
	"# Instructions\n" +
	" - If your command will create new directories or files, first use this tool to run `ls` to verify the parent directory exists and is the correct location.\n" +
	" - Always quote file paths that contain spaces with double quotes in your command (e.g., cd \"path with spaces/file.txt\")\n" +
	" - Try to maintain your current working directory throughout the session by using absolute paths and avoiding usage of `cd`.\n" +
	" - You may specify an optional timeout in milliseconds (up to 600000ms / 10 minutes). By default, your command will timeout after 120000ms (2 minutes).\n" +
	" - You can use the `run_in_background` parameter to run the command in the background. Only use this if you don't need the result immediately and are OK being notified when the command completes later. You do not need to check the output right away - you'll be notified when it finishes. You do not need to use '&' at the end of the command when using this parameter.\n" +
	" - Avoid unnecessary `sleep` commands: do not sleep between commands that can run immediately, and do not retry failing commands in a sleep loop — diagnose the root cause.\n" +
	" - When running `find`, search from `.` (or a specific path), not `/` — scanning the full filesystem can exhaust system resources on large trees."

const anthropicReadDescription = "Reads a file from the local filesystem. You can access any file directly by using this tool.\n" +
	"Assume this tool is able to read all files on the machine. If the User provides a path to a file assume that path is valid. It is okay to read a file that does not exist; an error will be returned.\n\n" +
	"Usage:\n" +
	"- The file_path parameter must be an absolute path, not a relative path\n" +
	"- By default, it reads up to 2000 lines starting from the beginning of the file\n" +
	"- When you already know which part of the file you need, only read that part. This can be important for larger files.\n" +
	"- Results are returned using cat -n format, with line numbers starting at 1\n" +
	"- This tool can only read files, not directories. To list files in a directory, use the registered shell tool.\n" +
	"- If you read a file that exists but has empty contents you will receive a system reminder warning in place of file contents.\n" +
	"- Do NOT re-read a file you just edited to verify — Edit/Write would have errored if the change failed, and the harness tracks file state for you."

const anthropicEditDescription = "Performs exact string replacements in files.\n\n" +
	"Usage:\n" +
	"- Read the file at least once in the conversation before editing it.\n" +
	"- When editing text from Read tool output, ensure you preserve the exact indentation (tabs/spaces) as it appears AFTER the line number prefix. The line number prefix format is: line number + tab. Everything after that is the actual file content to match. Never include any part of the line number prefix in the old_string or new_string.\n" +
	"- ALWAYS prefer editing existing files in the codebase. NEVER write new files unless explicitly required.\n" +
	"- Only use emojis if the user explicitly requests it. Avoid adding emojis to files unless asked.\n" +
	"- The edit will FAIL if `old_string` is not unique in the file. Either provide a larger string with more surrounding context to make it unique or use `replace_all` to change every instance of `old_string`.\n" +
	"- Use `replace_all` for replacing and renaming strings across the file. This parameter is useful if you want to rename a variable for instance."

const anthropicWriteDescription = "Writes a file to the local filesystem.\n\n" +
	"Usage:\n" +
	"- This tool will overwrite the existing file if there is one at the provided path.\n" +
	"- If this is an existing file, Read it first.\n" +
	"- Prefer the Edit tool for modifying existing files — it only sends the diff. Only use this tool to create new files or for complete rewrites.\n" +
	"- NEVER create documentation files (*.md) or README files unless explicitly requested by the User.\n" +
	"- Only use emojis if the user explicitly requests it. Avoid writing emojis to files unless asked."

func anthropicPrimaryTools() []provider.Tool {
	return []provider.Tool{
		{Name: "Bash", Description: anthropicBashDescription, Parameters: objectSchema(map[string]any{
			"command":           prop("string", "The command to execute"),
			"timeout":           prop("number", "Optional timeout in milliseconds (max 600000)"),
			"description":       prop("string", "Clear, concise description of what this command does in active voice."),
			"run_in_background": prop("boolean", "Set to true to run this command in the background."),
		}, "command")},
		{Name: "Read", Description: anthropicReadDescription, Parameters: objectSchema(map[string]any{
			"file_path": prop("string", "The absolute path to the file to read"),
			"offset":    map[string]any{"type": "integer", "minimum": 0, "description": "The line number to start reading from. Only provide if the file is too large to read at once"},
			"limit":     map[string]any{"type": "integer", "exclusiveMinimum": 0, "description": "The number of lines to read. Only provide if the file is too large to read at once."},
		}, "file_path")},
		{Name: "Edit", Description: anthropicEditDescription, Parameters: objectSchema(map[string]any{
			"file_path":   prop("string", "The absolute path to the file to modify"),
			"old_string":  prop("string", "The text to replace"),
			"new_string":  prop("string", "The text to replace it with (must be different from old_string)"),
			"replace_all": map[string]any{"type": "boolean", "default": false, "description": "Replace all occurrences of old_string (default false)"},
		}, "file_path", "old_string", "new_string")},
		{Name: "Write", Description: anthropicWriteDescription, Parameters: objectSchema(map[string]any{
			"file_path": prop("string", "The absolute path to the file to write (must be absolute, not relative)"),
			"content":   prop("string", "The content to write to the file"),
		}, "file_path", "content")},
	}
}

// codexPatchGrammar is the Lark grammar Codex attaches to its freeform
// apply_patch tool. GLM speaks JSON functions, so the Codex shape carries the
// grammar in the description of a one-string function instead.
const codexPatchGrammar = "start: begin_patch hunk+ end_patch\n" +
	"begin_patch: \"*** Begin Patch\" LF\n" +
	"end_patch: \"*** End Patch\" LF?\n\n" +
	"hunk: add_hunk | delete_hunk | update_hunk\n" +
	"add_hunk: \"*** Add File: \" filename LF add_line+\n" +
	"delete_hunk: \"*** Delete File: \" filename LF\n" +
	"update_hunk: \"*** Update File: \" filename LF change_move? change?\n\n" +
	"filename: /(.+)/\n" +
	"add_line: \"+\" /(.*)/ LF -> line\n\n" +
	"change_move: \"*** Move to: \" filename LF\n" +
	"change: (change_context | change_line)+ eof_line?\n" +
	"change_context: (\"@@\" | \"@@ \" /(.+)/) LF\n" +
	"change_line: (\"+\" | \"-\" | \" \") /(.*)/ LF\n" +
	"eof_line: \"*** End of File\" LF\n\n" +
	"%import common.LF\n"

func codexPrimaryTools() []provider.Tool {
	budget := prop("number", "Output token budget. Defaults to 10000 tokens; larger requests may be capped by policy.")
	return []provider.Tool{
		{Name: "exec_command", Description: "Runs a command, returning output or a session ID for ongoing interaction.", Parameters: objectSchema(map[string]any{
			"cmd":               prop("string", "Shell command to execute."),
			"workdir":           prop("string", "Working directory for the command. Defaults to the turn cwd."),
			"yield_time_ms":     prop("number", "Wait before yielding output. Defaults to 10000 ms; effective range is 250-30000 ms."),
			"max_output_tokens": budget,
		}, "cmd")},
		{Name: "write_stdin", Description: "Writes characters to an existing unified exec session and returns recent output.", Parameters: objectSchema(map[string]any{
			"session_id":        prop("number", "Identifier of the running unified exec session."),
			"chars":             prop("string", "Bytes to write to stdin. Defaults to empty, which polls without writing."),
			"yield_time_ms":     prop("number", "Wait before yielding output. Non-empty writes default to 250 ms and cap at 30000 ms; empty polls wait 5000-300000 ms by default."),
			"max_output_tokens": budget,
		}, "session_id")},
		{Name: "apply_patch", Description: "The `apply_patch` tool can be used to edit files. Pass the whole patch as `input`; it must match this Lark grammar:\n\n" + codexPatchGrammar, Parameters: objectSchema(map[string]any{
			"input": prop("string", "The entire patch, from *** Begin Patch to *** End Patch."),
		}, "input")},
	}
}

// shapedDispatch reports whether name is a primary tool of the runtime's
// shape that executeShapedTool runs, and whether it is another shape's
// primary tool, which this runtime does not offer and refuses.
func (r *Runtime) shapedDispatch(name string) (shaped, foreign bool) {
	for _, known := range primaryToolNames[r.toolShape] {
		if known == name {
			return !config.NativeToolShape(r.toolShape), false
		}
	}
	for shape, names := range primaryToolNames {
		if shape == r.toolShape {
			continue
		}
		for _, known := range names {
			if known == name {
				return false, true
			}
		}
	}
	return false, false
}

var sharedToolNames = map[string]bool{"python": true, "pwsh": true, "job": true, "list_subagents": true, "launch_subagent": true, "msg_subagent": true, "end_subagent": true}

// shapedValidation renders, in the shape's own words, a call its harness
// would reject before running anything: a tool the shape does not offer,
// arguments that are not JSON, or a missing required parameter. Shared tools
// and the slbh shape keep slbh's handling.
func (r *Runtime) shapedValidation(name, raw string) (string, error, bool) {
	if config.NativeToolShape(r.toolShape) || sharedToolNames[name] {
		return "", nil, false
	}
	var definition *provider.Tool
	for _, tool := range buildToolDefinitions(r.toolShape) {
		if tool.Name == name {
			tool := tool
			definition = &tool
			break
		}
	}
	anthropic := r.toolShape == config.ToolShapeAnthropic
	if definition == nil {
		if anthropic {
			hint := ""
			switch name {
			case "Glob":
				hint = " Glob is not available in this session — find files with `find` via the Bash tool instead."
			case "Grep":
				hint = " Grep is not available in this session — search file contents with `grep` via the Bash tool instead."
			}
			return "", toolUseError("Error: No such tool available: " + name + "." + hint), true
		}
		return "unsupported call: " + name, nil, true
	}
	values := map[string]any{}
	if strings.TrimSpace(raw) != "" {
		if err := json.Unmarshal([]byte(raw), &values); err != nil {
			if anthropic {
				return "", toolUseError(fmt.Sprintf("InputValidationError: %s failed due to the following issue:\nThe tool input is not valid JSON: %v", name, err)), true
			}
			return fmt.Sprintf("failed to parse function arguments: %v", err), nil, true
		}
	}
	required, _ := definition.Parameters["required"].([]string)
	for _, key := range required {
		if _, ok := values[key]; !ok {
			if anthropic {
				return "", toolUseError(fmt.Sprintf("InputValidationError: %s failed due to the following issue:\nThe required parameter `%s` is missing", name, key)), true
			}
			return fmt.Sprintf("failed to parse function arguments: missing field `%s`", key), nil, true
		}
	}
	return "", nil, false
}

// executeShapedTool runs a primary tool of the anthropic or codex shape.
func (r *Runtime) executeShapedTool(agentID, base, name string, values map[string]any) (string, error) {
	switch name {
	case "Read":
		return r.anthropicRead(agentID, base, values)
	case "Edit":
		return r.anthropicEdit(agentID, base, values)
	case "Write":
		return r.anthropicWrite(agentID, base, values)
	case "Bash":
		return r.anthropicBash(agentID, base, values)
	case "exec_command":
		return r.codexExec(agentID, base, values)
	case "write_stdin":
		return r.codexWriteStdin(agentID, values)
	case "apply_patch":
		return r.codexApplyPatch(base, value(values, "input")), nil
	}
	return "", fmt.Errorf("unknown tool %q", name)
}

// ---- Anthropic shape ----

func (r *Runtime) shellCwd(agentID, base string) string {
	r.shape.mu.Lock()
	defer r.shape.mu.Unlock()
	if cwd, ok := r.shape.cwd[agentID]; ok {
		if info, err := os.Stat(cwd); err == nil && info.IsDir() {
			return cwd
		}
	}
	return base
}

func (r *Runtime) recordRead(agentID, file string) {
	info, err := os.Stat(file)
	if err != nil {
		return
	}
	r.shape.mu.Lock()
	defer r.shape.mu.Unlock()
	if r.shape.reads[agentID] == nil {
		r.shape.reads[agentID] = map[string]time.Time{}
	}
	r.shape.reads[agentID][file] = info.ModTime()
}

// changedSinceRead reports a file the agent has read that has since changed
// on disk. A file it never read reports false: Claude Code does not enforce
// read-before-edit, and neither does this shape.
func (r *Runtime) changedSinceRead(agentID, file string) bool {
	r.shape.mu.Lock()
	seen, ok := r.shape.reads[agentID][file]
	r.shape.mu.Unlock()
	if !ok {
		return false
	}
	info, err := os.Stat(file)
	return err == nil && !info.ModTime().Equal(seen)
}

const anthropicStateNote = " (file state is current in your context — no need to Read it back)"
const anthropicStaleNote = " (note: the file had been modified on disk since you last read it — the edit applied cleanly, but the file contains other changes not in your context. Read it before edits that depend on surrounding content.)"

func (r *Runtime) anthropicRead(agentID, base string, values map[string]any) (string, error) {
	path := value(values, "file_path")
	file, err := r.resolvePath(base, path)
	if err != nil {
		return "", toolUseError(err.Error())
	}
	info, err := os.Stat(file)
	if err != nil {
		return "", anthropicFailure("File does not exist. Note: your current working directory is " + r.shellCwd(agentID, base) + ".")
	}
	if info.IsDir() {
		return "", anthropicFailure(fmt.Sprintf("EISDIR: illegal operation on a directory, read '%s'", file))
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return "", anthropicFailure(err.Error())
	}
	r.recordRead(agentID, file)
	if len(data) == 0 {
		return "<system-reminder>Warning: the file exists but the contents are empty.</system-reminder>", nil
	}
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	start := intValue(values, "offset")
	if start < 1 {
		start = 1
	}
	limit := intValue(values, "limit")
	if limit < 1 {
		limit = 2000
	}
	if start > len(lines) {
		return fmt.Sprintf("<system-reminder>Warning: the file exists but is shorter than the provided offset (%d). The file has %d lines.</system-reminder>", start, len(lines)), nil
	}
	end := start - 1 + limit
	if end > len(lines) {
		end = len(lines)
	}
	var out strings.Builder
	for number := start; number <= end; number++ {
		if number > start {
			out.WriteByte('\n')
		}
		fmt.Fprintf(&out, "%d\t%s", number, lines[number-1])
	}
	return out.String(), nil
}

func (r *Runtime) anthropicEdit(agentID, base string, values map[string]any) (string, error) {
	path := value(values, "file_path")
	old, replacement := value(values, "old_string"), value(values, "new_string")
	replaceAll := boolValue(values, "replace_all")
	if old == replacement {
		return "", toolUseError("No changes to make: old_string and new_string are exactly the same.")
	}
	file, err := r.resolvePath(base, path)
	if err != nil {
		return "", toolUseError(err.Error())
	}
	data, err := os.ReadFile(file)
	if os.IsNotExist(err) {
		if old != "" {
			return "", toolUseError("File does not exist. Note: your current working directory is " + r.shellCwd(agentID, base) + ".")
		}
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			return "", toolUseError(err.Error())
		}
		if err := os.WriteFile(file, []byte(replacement), 0o644); err != nil {
			return "", toolUseError(err.Error())
		}
		r.recordRead(agentID, file)
		return "The file " + path + " has been updated successfully." + anthropicStateNote, nil
	}
	if err != nil {
		return "", toolUseError(err.Error())
	}
	if old == "" {
		return "", toolUseError("Cannot create new file - file already exists.")
	}
	text := string(data)
	count := strings.Count(text, old)
	if count == 0 {
		return "", toolUseError("String to replace not found in file.\nString: " + old)
	}
	if count > 1 && !replaceAll {
		return "", toolUseError(fmt.Sprintf("Found %d matches of the string to replace, but replace_all is false. To replace all occurrences, set replace_all to true. To replace only one occurrence, please provide more context to uniquely identify the instance.\nString: %s", count, old))
	}
	stale := r.changedSinceRead(agentID, file)
	if replaceAll {
		text = strings.ReplaceAll(text, old, replacement)
	} else {
		text = strings.Replace(text, old, replacement, 1)
	}
	if err := atomicReplace(file, []byte(text), infoMode(file)); err != nil {
		return "", toolUseError(err.Error())
	}
	r.recordRead(agentID, file)
	message := "The file " + path + " has been updated successfully."
	if replaceAll {
		message = "The file " + path + " has been updated. All occurrences were successfully replaced."
	}
	if stale {
		return message + anthropicStaleNote, nil
	}
	return message + anthropicStateNote, nil
}

func (r *Runtime) anthropicWrite(agentID, base string, values map[string]any) (string, error) {
	path := value(values, "file_path")
	file, err := r.resolvePath(base, path)
	if err != nil {
		return "", toolUseError(err.Error())
	}
	content := []byte(value(values, "content"))
	if info, err := os.Stat(file); err == nil {
		if info.IsDir() {
			return "", toolUseError(fmt.Sprintf("EISDIR: illegal operation on a directory, open '%s'", file))
		}
		if err := atomicReplace(file, content, infoMode(file)); err != nil {
			return "", toolUseError(err.Error())
		}
		r.recordRead(agentID, file)
		return "The file " + path + " has been updated successfully." + anthropicStateNote, nil
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return "", toolUseError(err.Error())
	}
	if err := os.WriteFile(file, content, 0o644); err != nil {
		return "", toolUseError(err.Error())
	}
	r.recordRead(agentID, file)
	return "File created successfully at: " + path + anthropicStateNote, nil
}

const anthropicMaxOutput = 30000

func (r *Runtime) anthropicBash(agentID, base string, values map[string]any) (string, error) {
	command := value(values, "command")
	if strings.TrimSpace(command) == "" {
		return "", toolUseError("command is required")
	}
	timeout := 120 * time.Second
	if ms := intValue(values, "timeout"); ms > 0 {
		if ms > 600000 {
			ms = 600000
		}
		timeout = time.Duration(ms) * time.Millisecond
	}
	scratch := r.agentScratch(agentID)
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		return "", toolUseError(err.Error())
	}
	cwd := r.shellCwd(agentID, base)
	cwdFile := filepath.Join(scratch, ".bash-cwd")
	_ = os.Remove(cwdFile)
	// The trap records where the command left the shell, including after an
	// explicit exit, so the next call starts there as Claude Code's does.
	script := "trap 'pwd -P > " + shellQuote(cwdFile) + " 2>/dev/null' EXIT\n" + command
	background := boolValue(values, "run_in_background")
	j, err := r.jobs.Start(r.ctx, job.Spec{Author: agentID, Script: script, Interpreter: "bash", ScriptRoot: filepath.Join(r.runtimeDir, "agents", agentID, "jobs"), ToolName: "Bash", Dir: cwd, Environment: r.scratchEnvironment(agentID), Inline: !background})
	if err != nil {
		return "", anthropicFailure(err.Error())
	}
	if background {
		snap, _, _, _ := r.jobs.Wait(j, 0)
		r.noteExec(agentID, noteFromSnapshot(snap, true))
		return fmt.Sprintf("Command running in background with ID: %s. You will be notified when it completes. To check interim output, use the job tool with action read.", j.Snapshot().ID), nil
	}
	snap, stdout, stderr, timedOut := r.jobs.WaitOrKill(j, timeout)
	r.noteExec(agentID, noteFromSnapshot(snap, false))
	output := joinOutput(stdout, stderr)
	output = r.persistLargeOutput(agentID, snap.ID, output)
	note := r.updateShellCwd(agentID, base, cwdFile)
	if timedOut {
		text := "Exit code 143"
		if output != "" {
			text += "\n" + output
		}
		return "", anthropicFailure(text + "\nCommand timed out after " + formatSeconds(timeout))
	}
	if snap.ExitCode != 0 || snap.Status == job.Failed {
		text := fmt.Sprintf("Exit code %d", snap.ExitCode)
		if output != "" {
			text += "\n" + output
		}
		return "", anthropicFailure(text + note)
	}
	if output == "" {
		output = "(Bash completed with no output)"
	}
	return output + note, nil
}

// updateShellCwd keeps the directory the command ended in when it is inside
// the agent's working tree, and otherwise resets to it with the note Claude
// Code appends.
func (r *Runtime) updateShellCwd(agentID, base, cwdFile string) string {
	data, err := os.ReadFile(cwdFile)
	if err != nil {
		return ""
	}
	cwd := strings.TrimSpace(string(data))
	root, _ := filepath.EvalSymlinks(base)
	if root == "" {
		root = base
	}
	r.shape.mu.Lock()
	defer r.shape.mu.Unlock()
	if cwd == root || strings.HasPrefix(cwd, root+string(os.PathSeparator)) {
		r.shape.cwd[agentID] = cwd
		return ""
	}
	delete(r.shape.cwd, agentID)
	return "\nShell cwd was reset to " + base
}

func (r *Runtime) persistLargeOutput(agentID, id, output string) string {
	if len(output) <= anthropicMaxOutput {
		return output
	}
	dir := filepath.Join(r.agentScratch(agentID), "tool-results")
	path := filepath.Join(dir, id+".txt")
	if err := os.MkdirAll(dir, 0o700); err != nil || os.WriteFile(path, []byte(output), 0o600) != nil {
		return output[:anthropicMaxOutput] + "\n[output truncated]"
	}
	preview := output[:2000]
	if cut := strings.LastIndexByte(preview, '\n'); cut > 0 {
		preview = preview[:cut]
	}
	return fmt.Sprintf("<persisted-output>\nOutput too large (%.1fKB). Full output saved to: %s\n\nPreview (first 2KB):\n%s\n...\n</persisted-output>", float64(len(output))/1024, path, preview)
}

func joinOutput(stdout, stderr string) string {
	var parts []string
	if text := strings.TrimRight(stdout, "\n"); text != "" {
		parts = append(parts, text)
	}
	if text := strings.TrimRight(stderr, "\n"); text != "" {
		parts = append(parts, text)
	}
	return strings.Join(parts, "\n")
}

func formatSeconds(d time.Duration) string {
	if d%time.Second == 0 {
		return fmt.Sprintf("%ds", int(d/time.Second))
	}
	return fmt.Sprintf("%.1fs", d.Seconds())
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// ---- Codex shape ----

func clampMillis(values map[string]any, key string, fallback, low, high int) time.Duration {
	ms := fallback
	if _, ok := values[key]; ok {
		ms = intValue(values, key)
	}
	if ms < low {
		ms = low
	}
	if ms > high {
		ms = high
	}
	return time.Duration(ms) * time.Millisecond
}

func outputBudget(values map[string]any) int {
	if budget := intValue(values, "max_output_tokens"); budget > 0 {
		return budget
	}
	return 10000
}

func (r *Runtime) codexExec(agentID, base string, values map[string]any) (string, error) {
	command := value(values, "cmd")
	if strings.TrimSpace(command) == "" {
		return "failed to parse function arguments: missing field `cmd`", nil
	}
	cwd, err := r.resolvePath(base, valueDefault(values, "workdir", base))
	if err != nil || valueDefault(values, "workdir", "") == "" {
		cwd = base
	}
	yield := clampMillis(values, "yield_time_ms", 10000, 250, 30000)
	started := time.Now()
	// stderr joins stdout in order, as it does on Codex's terminal.
	j, err := r.jobs.Start(r.ctx, job.Spec{Author: agentID, Script: "exec 2>&1\n" + command, Interpreter: "bash", ScriptRoot: filepath.Join(r.runtimeDir, "agents", agentID, "jobs"), ToolName: "exec_command", Dir: cwd, Environment: r.scratchEnvironment(agentID), Inline: true})
	if err != nil {
		return "exec_command failed: " + err.Error(), nil
	}
	snap, stdout, _, running := r.jobs.Wait(j, yield)
	r.noteExec(agentID, noteFromSnapshot(snap, running))
	session := 0
	if running {
		r.shape.mu.Lock()
		session = r.shape.nextSession
		r.shape.nextSession++
		r.shape.sessions[session] = &codexSession{agentID: agentID, job: j, offset: len(stdout)}
		r.shape.mu.Unlock()
	}
	return codexChunk(time.Since(started), snap, running, session, stdout, outputBudget(values)), nil
}

func (r *Runtime) codexWriteStdin(agentID string, values map[string]any) (string, error) {
	id := intValue(values, "session_id")
	r.shape.mu.Lock()
	session, ok := r.shape.sessions[id]
	r.shape.mu.Unlock()
	if !ok || session.agentID != agentID {
		return fmt.Sprintf("write_stdin failed: Unknown process id %d", id), nil
	}
	if value(values, "chars") != "" {
		return "write_stdin failed: stdin is not available for this session; call write_stdin with empty chars to poll it", nil
	}
	yield := clampMillis(values, "yield_time_ms", 5000, 250, 300000)
	started := time.Now()
	timer := time.NewTimer(yield)
	select {
	case <-session.job.Exited():
	case <-timer.C:
	}
	timer.Stop()
	snap := session.job.Snapshot()
	stdout, _ := session.job.Output()
	running := snap.Status == job.Running
	r.noteExec(agentID, noteFromSnapshot(snap, running))
	r.shape.mu.Lock()
	fresh := stdout
	if session.offset <= len(stdout) {
		fresh = stdout[session.offset:]
	}
	session.offset = len(stdout)
	if !running {
		delete(r.shape.sessions, id)
	}
	r.shape.mu.Unlock()
	return codexChunk(time.Since(started), snap, running, id, fresh, outputBudget(values)), nil
}

func approxTokens(text string) int { return (len(text) + 3) / 4 }

// codexChunk renders an exec result the way Codex's unified exec does, down
// to its head-and-tail truncation at max_output_tokens.
func codexChunk(wall time.Duration, snap job.Snapshot, running bool, session int, output string, budget int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Chunk ID: %06x\nWall time: %.4f seconds\n", rand.IntN(1<<24), wall.Seconds())
	if running {
		fmt.Fprintf(&b, "Process running with session ID %d\n", session)
	} else {
		code := snap.ExitCode
		if snap.Status == job.Killed && code == 0 {
			code = -1
		}
		fmt.Fprintf(&b, "Process exited with code %d\n", code)
	}
	fmt.Fprintf(&b, "Original token count: %d\nOutput:\n", approxTokens(output))
	b.WriteString(truncateMiddle(output, budget, budget*4/2))
	return b.String()
}

// truncateMiddle is Codex's head-and-tail truncation: output over budget
// tokens keeps its first and last keep bytes and says what it dropped.
func truncateMiddle(output string, budget, keep int) string {
	tokens := approxTokens(output)
	if tokens <= budget {
		return output
	}
	head, tail := output[:keep], output[len(output)-keep:]
	return fmt.Sprintf("Warning: truncated output (original token count: %d)\nTotal output lines: %d\n\n%s…%d tokens truncated…%s", tokens, strings.Count(strings.TrimSuffix(output, "\n"), "\n")+1, head, tokens-budget, tail)
}

type codexHunk struct {
	context string
	old     []string
	new     []string
	eof     bool
}

type codexFileOp struct {
	kind   string // "A", "D" or "M"
	path   string
	moveTo string
	lines  []string
	hunks  []codexHunk
}

func parseCodexPatch(patch string) ([]codexFileOp, error) {
	lines := strings.Split(strings.ReplaceAll(strings.TrimSpace(patch), "\r\n", "\n"), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "*** Begin Patch" {
		return nil, fmt.Errorf("invalid patch: The first line of the patch must be '*** Begin Patch'")
	}
	if strings.TrimSpace(lines[len(lines)-1]) != "*** End Patch" {
		return nil, fmt.Errorf("invalid patch: The last line of the patch must be '*** End Patch'")
	}
	body := lines[1 : len(lines)-1]
	var ops []codexFileOp
	for i := 0; i < len(body); {
		header := strings.TrimRight(body[i], " ")
		lineNo := i + 2
		switch {
		case strings.HasPrefix(header, "*** Add File: "):
			op := codexFileOp{kind: "A", path: strings.TrimSpace(strings.TrimPrefix(header, "*** Add File: "))}
			i++
			for i < len(body) && strings.HasPrefix(body[i], "+") {
				op.lines = append(op.lines, body[i][1:])
				i++
			}
			ops = append(ops, op)
		case strings.HasPrefix(header, "*** Delete File: "):
			ops = append(ops, codexFileOp{kind: "D", path: strings.TrimSpace(strings.TrimPrefix(header, "*** Delete File: "))})
			i++
		case strings.HasPrefix(header, "*** Update File: "):
			op := codexFileOp{kind: "M", path: strings.TrimSpace(strings.TrimPrefix(header, "*** Update File: "))}
			i++
			if i < len(body) && strings.HasPrefix(body[i], "*** Move to: ") {
				op.moveTo = strings.TrimSpace(strings.TrimPrefix(body[i], "*** Move to: "))
				i++
			}
			var current *codexHunk
			for i < len(body) {
				line := body[i]
				if line == "*** End of File" {
					if current != nil {
						current.eof = true
					}
					i++
					continue
				}
				if strings.HasPrefix(line, "*** ") {
					break
				}
				if strings.HasPrefix(line, "@@") {
					op.hunks = append(op.hunks, codexHunk{context: strings.TrimSpace(strings.TrimPrefix(line, "@@"))})
					current = &op.hunks[len(op.hunks)-1]
					i++
					continue
				}
				if current == nil {
					op.hunks = append(op.hunks, codexHunk{})
					current = &op.hunks[len(op.hunks)-1]
				}
				switch {
				case line == "":
					current.old = append(current.old, "")
					current.new = append(current.new, "")
				case line[0] == ' ':
					current.old = append(current.old, line[1:])
					current.new = append(current.new, line[1:])
				case line[0] == '-':
					current.old = append(current.old, line[1:])
				case line[0] == '+':
					current.new = append(current.new, line[1:])
				default:
					return nil, fmt.Errorf("invalid patch: Invalid patch hunk on line %d: Unexpected line found in update hunk: '%s'. Every line should start with ' ' (context line), '+' (added line), or '-' (removed line)", i+2, line)
				}
				i++
			}
			if len(op.hunks) == 0 && op.moveTo == "" {
				return nil, fmt.Errorf("invalid patch: Invalid patch hunk on line %d: Update file hunk for path '%s' is empty", lineNo, op.path)
			}
			ops = append(ops, op)
		case strings.TrimSpace(header) == "":
			i++
		default:
			return nil, fmt.Errorf("invalid patch: Invalid patch hunk on line %d: '%s' is not a valid hunk header. Valid hunk headers: '*** Add File: {path}', '*** Delete File: {path}', '*** Update File: {path}'", lineNo, header)
		}
	}
	if len(ops) == 0 {
		return nil, fmt.Errorf("invalid patch: No files were modified.")
	}
	return ops, nil
}

// seekLines finds needle in haystack at or after start, trying an exact match,
// then one ignoring trailing whitespace, then one ignoring both ends, as
// Codex's seek_sequence does.
func seekLines(haystack, needle []string, start int, eof bool) int {
	if len(needle) == 0 {
		return start
	}
	if len(needle) > len(haystack) {
		return -1
	}
	normalizers := []func(string) string{
		func(s string) string { return s },
		func(s string) string { return strings.TrimRight(s, " \t") },
		strings.TrimSpace,
	}
	first := start
	if eof {
		first = len(haystack) - len(needle)
	}
	for _, norm := range normalizers {
		for _, from := range []int{first, start} {
			for i := from; i+len(needle) <= len(haystack); i++ {
				if i < 0 {
					continue
				}
				match := true
				for j := range needle {
					if norm(haystack[i+j]) != norm(needle[j]) {
						match = false
						break
					}
				}
				if match {
					return i
				}
			}
		}
	}
	return -1
}

// applyCodexHunks applies an Update's hunks as Codex does: each hunk's @@
// line is found first, and its old lines after it; a hunk with no old lines
// is appended at the end of the file. native changes two things: a pure
// insertion goes directly after its @@ line, and old lines may start at the
// @@ line itself, since models routinely repeat it as the first context line.
func applyCodexHunks(file string, original string, hunks []codexHunk, native bool) (string, error) {
	lines := strings.Split(original, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	type replacement struct {
		at, remove int
		insert     []string
	}
	var replacements []replacement
	cursor := 0
	for _, hunk := range hunks {
		anchor := -1
		if hunk.context != "" {
			anchor = seekLines(lines, []string{hunk.context}, cursor, false)
			if anchor < 0 {
				return "", fmt.Errorf("Failed to find context '%s' in %s", hunk.context, file)
			}
			cursor = anchor + 1
		}
		if len(hunk.old) == 0 {
			at := len(lines)
			if native && anchor >= 0 {
				at = anchor + 1
			}
			replacements = append(replacements, replacement{at: at, insert: hunk.new})
			continue
		}
		old, replaced := hunk.old, hunk.new
		seek := func() int {
			at := seekLines(lines, old, cursor, hunk.eof)
			if at < 0 && native && anchor >= 0 {
				at = seekLines(lines, old, anchor, hunk.eof)
			}
			return at
		}
		at := seek()
		if at < 0 && len(old) > 0 && old[len(old)-1] == "" {
			old = old[:len(old)-1]
			if len(replaced) > 0 && replaced[len(replaced)-1] == "" {
				replaced = replaced[:len(replaced)-1]
			}
			at = seek()
		}
		if at < 0 {
			return "", fmt.Errorf("Failed to find expected lines in %s:\n%s", file, strings.Join(hunk.old, "\n"))
		}
		replacements = append(replacements, replacement{at: at, remove: len(old), insert: replaced})
		cursor = at + len(old)
	}
	for i := len(replacements) - 1; i >= 0; i-- {
		rep := replacements[i]
		next := append([]string{}, lines[:rep.at]...)
		next = append(next, rep.insert...)
		lines = append(next, lines[rep.at+rep.remove:]...)
	}
	return strings.Join(lines, "\n") + "\n", nil
}

// codexApplyPatch applies a patch in Codex's format and reports as Codex does.
func (r *Runtime) codexApplyPatch(base, patch string) string {
	summary, err := r.applyCodexPatch(base, patch, false)
	if err != nil {
		return err.Error()
	}
	return "Exit code: 0\nWall time: 0 seconds\nOutput:\nSuccess. Updated the following files:\n" + strings.Join(summary, "\n") + "\n"
}

// applyCodexPatch applies a patch in Codex's format, verifying every file
// operation before writing any of them so a failed hunk leaves the tree
// untouched. Operations apply in order to a staged view of the tree, as Codex
// applies them to the disk one after another, so a second Update of one file
// sees the first's result and a Move onto its own path is an Update. It
// returns one "A path", "D path" or "M path" line per operation, and errors
// carry Codex's own text.
//
// native selects slbh's own semantics where Codex's lose work: an Add onto an
// existing file is refused rather than overwriting it, and a hunk's @@ line
// places a pure insertion after it and may itself be the hunk's first line
// (applyCodexHunks). The codex shape passes false and keeps Codex's behaviour.
func (r *Runtime) applyCodexPatch(base, patch string, native bool) ([]string, error) {
	ops, err := parseCodexPatch(patch)
	if err != nil {
		return nil, err
	}
	// staged maps a path to its content after the operations so far; a nil
	// entry is a path the patch deletes.
	staged := map[string][]byte{}
	var order []string
	stage := func(path string, content []byte) {
		if _, seen := staged[path]; !seen {
			order = append(order, path)
		}
		staged[path] = content
	}
	read := func(path string) ([]byte, bool) {
		if content, ok := staged[path]; ok {
			return content, content != nil
		}
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			return nil, false
		}
		data, err := os.ReadFile(path)
		return data, err == nil
	}
	var summary []string
	for _, op := range ops {
		file, err := r.resolvePath(base, op.path)
		if err != nil {
			return nil, errors.New("apply_patch verification failed: " + err.Error())
		}
		switch op.kind {
		case "A":
			if _, exists := read(file); exists && native {
				return nil, fmt.Errorf("apply_patch verification failed: Add File %s: the file already exists; use Update File to change it", op.path)
			}
			stage(file, []byte(strings.Join(op.lines, "\n")+"\n"))
			summary = append(summary, "A "+op.path)
		case "D":
			if _, exists := read(file); !exists {
				return nil, fmt.Errorf("apply_patch verification failed: Failed to read %s: No such file or directory (os error 2)", file)
			}
			stage(file, nil)
			summary = append(summary, "D "+op.path)
		case "M":
			data, exists := read(file)
			if !exists {
				return nil, fmt.Errorf("apply_patch verification failed: Failed to read file to update %s: No such file or directory (os error 2)", file)
			}
			content, err := applyCodexHunks(file, string(data), op.hunks, native)
			if err != nil {
				return nil, errors.New("apply_patch verification failed: " + err.Error())
			}
			target, shown := file, op.path
			if op.moveTo != "" {
				if target, err = r.resolvePath(base, op.moveTo); err != nil {
					return nil, errors.New("apply_patch verification failed: " + err.Error())
				}
				shown = op.moveTo
			}
			if target != file {
				stage(file, nil)
			}
			stage(target, []byte(content))
			summary = append(summary, "M "+shown)
		}
	}
	for _, path := range order {
		content := staged[path]
		if content == nil {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return nil, errors.New("apply_patch failed: " + err.Error())
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, errors.New("apply_patch failed: " + err.Error())
		}
		mode := os.FileMode(0o644)
		if info, err := os.Stat(path); err == nil {
			mode = info.Mode().Perm()
		}
		if err := atomicReplace(path, content, mode); err != nil {
			return nil, errors.New("apply_patch failed: " + err.Error())
		}
	}
	return summary, nil
}

func (r *Runtime) agentScratch(agentID string) string {
	return filepath.Join(r.runtimeDir, "agents", agentID, "scratch")
}

func (r *Runtime) scratchEnvironment(agentID string) []string {
	scratch := r.agentScratch(agentID)
	return []string{"TMPDIR=" + scratch, "TMP=" + scratch, "TEMP=" + scratch}
}

// ---- Uniform accounting ----
//
// Each shape reports failure differently: slbh and Anthropic return an error
// for a non-zero exit, while Codex returns the exit code in ordinary output
// and its patch failures as plain text. So that counts compare across shapes,
// the command handlers record what actually happened to the process as
// structured metadata, and the transcript's tool_result carries it. Nothing is
// parsed back out of rendered text, which a command's own output could fake.

// execNote is one command call's execution outcome.
type execNote struct {
	job      string
	state    string // "finished", "background" or "killed"
	exitCode int
}

func (r *Runtime) noteExec(agentID string, note execNote) {
	r.shape.mu.Lock()
	defer r.shape.mu.Unlock()
	if r.shape.notes == nil {
		r.shape.notes = map[string]execNote{}
	}
	r.shape.notes[agentID] = note
}

// takeExec returns and clears the note left by the agent's last tool call.
// An agent runs its tool calls one at a time, so the note is that call's.
func (r *Runtime) takeExec(agentID string) (execNote, bool) {
	r.shape.mu.Lock()
	defer r.shape.mu.Unlock()
	note, ok := r.shape.notes[agentID]
	delete(r.shape.notes, agentID)
	return note, ok
}

func noteFromSnapshot(snap job.Snapshot, running bool) execNote {
	switch {
	case running:
		return execNote{job: snap.ID, state: "background"}
	case snap.Status == job.Killed:
		return execNote{job: snap.ID, state: "killed", exitCode: -1}
	}
	return execNote{job: snap.ID, state: "finished", exitCode: snap.ExitCode}
}

// toolResultMetadata is the uniform record of one tool call. error means the
// call failed: the tool errored, a command it ran finished non-zero or was
// killed, or a Codex patch or session call did not succeed. A command that is
// still running is not yet a failure; its outcome arrives later as a
// job_result event, or through a write_stdin poll, carrying the same job id.
func toolResultMetadata(name, callID, result string, toolErr error, note execNote, noted bool) map[string]any {
	metadata := map[string]any{"name": name, "call_id": callID}
	failed := false
	if noted {
		metadata["job"] = note.job
		metadata["job_state"] = note.state
		if note.state != "background" {
			metadata["exit_code"] = note.exitCode
			failed = note.exitCode != 0
		}
		if name == "write_stdin" && note.state == "background" {
			// A poll of a command still running has not failed. The poll that
			// sees it end carries its outcome, once for the job.
			failed = false
		}
	} else {
		failed = toolErr != nil || codexCallFailed(name, result)
	}
	metadata["error"] = failed
	return metadata
}

// codexCallFailed reads the Codex shape's own non-command results, whose text
// is produced entirely by this file.
func codexCallFailed(name, result string) bool {
	// Validation failures, whatever the tool: shapedValidation's own text.
	if strings.HasPrefix(result, "unsupported call: ") || strings.HasPrefix(result, "failed to parse function arguments") {
		return true
	}
	switch name {
	case "apply_patch":
		return strings.HasPrefix(result, "apply_patch verification failed") || strings.HasPrefix(result, "invalid patch") || strings.HasPrefix(result, "apply_patch failed")
	case "exec_command", "write_stdin":
		return strings.HasPrefix(result, "exec_command failed") || strings.HasPrefix(result, "write_stdin failed") || strings.HasPrefix(result, "failed to parse")
	}
	return false
}
