package harness

// The Codex shape: its primary tools, exec_command and write_stdin over managed
// jobs, and the apply_patch parser and applier.

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/slbdotdev/slbh/internal/job"
	"github.com/slbdotdev/slbh/internal/provider"
)

type codexSession struct {
	agentID string
	job     *job.Job
	offset  int
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
