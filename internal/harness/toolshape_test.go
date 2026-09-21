package harness

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/slbdotdev/slbh/internal/config"
	"github.com/slbdotdev/slbh/internal/provider"
)

func shapedRuntime(t *testing.T, shape string) (*Runtime, string) {
	t.Helper()
	r, err := New(config.Config{Home: t.TempDir(), SeatModel: "test", SeatEffort: "high", SubagentModel: "test-child", SubagentEffort: "high", ToolShape: shape}, Options{Provider: func(string) (provider.Provider, error) { return fakeProvider{}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r.workDir = dir
	r.seat().WorkDir = dir
	return r, dir
}

func call(t *testing.T, r *Runtime, name string, values map[string]any) (string, error) {
	t.Helper()
	raw, err := json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	return r.ExecuteTool(r.seat().ID, name, string(raw))
}

// shaped returns the text the model receives and whether it is flagged.
func shaped(t *testing.T, r *Runtime, name string, values map[string]any) (string, bool) {
	t.Helper()
	out, err := call(t, r, name, values)
	if err == nil {
		return out, false
	}
	var failure *shapedToolError
	if !errors.As(err, &failure) {
		t.Fatalf("%s returned an unshaped error: %v", name, err)
	}
	return failure.text, failure.flag
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestSlbhShapeIsByteIdenticalToMain(t *testing.T) {
	golden, err := os.ReadFile("testdata/toolshape/slbh-main-d5c712c.json")
	if err != nil {
		t.Fatal(err)
	}
	current, err := json.MarshalIndent(ToolDefinitions(), "", " ")
	if err != nil {
		t.Fatal(err)
	}
	if string(current) != string(golden) {
		t.Fatal("the slbh shape's tool definitions differ from main's; the default must not drift")
	}
	r, _ := shapedRuntime(t, "")
	if r.ToolShape() != config.ToolShapeSlbh {
		t.Fatalf("empty shape = %q, want slbh", r.ToolShape())
	}
}

func TestUnknownToolShapeIsRefused(t *testing.T) {
	_, err := New(config.Config{Home: t.TempDir(), SeatModel: "test", ToolShape: "claude"}, Options{Provider: func(string) (provider.Provider, error) { return fakeProvider{}, nil }})
	if err == nil || !strings.Contains(err.Error(), "unknown tool shape") {
		t.Fatalf("err = %v", err)
	}
}

type capture struct {
	Tools []map[string]any `json:"tools"`
	Calls []struct {
		Name    string `json:"name"`
		Result  string `json:"result"`
		IsError bool   `json:"is_error"`
	} `json:"calls"`
}

func loadCapture(t *testing.T, name string) capture {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata/toolshape", name))
	if err != nil {
		t.Fatal(err)
	}
	var c capture
	if err := json.Unmarshal(data, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

// Names and parameters are pinned against the captured harness; only the
// parameters listed as dropped (sandbox, approval and PTY controls) may be
// missing, and nothing may be added.
func TestShapeSchemasMatchCaptures(t *testing.T) {
	dropped := map[string]bool{"dangerouslyDisableSandbox": true, "pages": true, "justification": true, "login": true, "prefix_rule": true, "sandbox_permissions": true, "shell": true, "tty": true}
	for _, tc := range []struct {
		shape, file, schemaKey string
	}{{config.ToolShapeAnthropic, "claude_code-2.1.278.json", "input_schema"}, {config.ToolShapeCodex, "codex-0.155.1.json", "parameters"}} {
		captured := map[string]map[string]any{}
		for _, tool := range loadCapture(t, tc.file).Tools {
			captured[tool["name"].(string)] = tool
		}
		ours := ShapeToolDefinitions(tc.shape)
		primary := map[string]bool{}
		for _, name := range primaryToolNames[tc.shape] {
			primary[name] = true
		}
		seen := 0
		for _, tool := range ours {
			if !primary[tool.Name] {
				continue
			}
			seen++
			original, ok := captured[tool.Name]
			if !ok {
				t.Fatalf("%s: %s is not in the capture", tc.shape, tool.Name)
			}
			schema, ok := original[tc.schemaKey].(map[string]any)
			if !ok {
				// Codex's apply_patch is freeform: the grammar is the contract.
				if !strings.Contains(tool.Description, codexPatchGrammar) || !strings.Contains(original["format"].(map[string]any)["definition"].(string), "begin_patch hunk+ end_patch") {
					t.Fatalf("%s: %s does not carry the captured grammar", tc.shape, tool.Name)
				}
				continue
			}
			want := schema["properties"].(map[string]any)
			got := tool.Parameters["properties"].(map[string]any)
			for name := range got {
				if _, ok := want[name]; !ok {
					t.Fatalf("%s.%s: parameter %s is not in the capture", tc.shape, tool.Name, name)
				}
			}
			for name, property := range want {
				if dropped[name] {
					continue
				}
				mine, ok := got[name].(map[string]any)
				if !ok {
					t.Fatalf("%s.%s: captured parameter %s is missing", tc.shape, tool.Name, name)
				}
				if mine["type"] != property.(map[string]any)["type"] {
					t.Fatalf("%s.%s.%s: type %v, captured %v", tc.shape, tool.Name, name, mine["type"], property.(map[string]any)["type"])
				}
			}
			wantRequired, _ := json.Marshal(schema["required"])
			gotRequired, _ := json.Marshal(tool.Parameters["required"])
			if string(wantRequired) != string(gotRequired) {
				t.Fatalf("%s.%s: required %s, captured %s", tc.shape, tool.Name, gotRequired, wantRequired)
			}
		}
		if seen != len(primaryToolNames[tc.shape]) {
			t.Fatalf("%s: offered %d primary tools, want %d", tc.shape, seen, len(primaryToolNames[tc.shape]))
		}
	}
}

func TestShapesShareEverythingButPrimaryTools(t *testing.T) {
	shared := func(shape string) []string {
		var names []string
		primary := map[string]bool{}
		for _, name := range primaryToolNames[shape] {
			primary[name] = true
		}
		for _, tool := range ShapeToolDefinitions(shape) {
			if !primary[tool.Name] {
				b, _ := json.Marshal(tool)
				names = append(names, string(b))
			}
		}
		return names
	}
	base := strings.Join(shared(config.ToolShapeSlbh), "\n")
	for _, shape := range []string{config.ToolShapeAnthropic, config.ToolShapeCodex} {
		if got := strings.Join(shared(shape), "\n"); got != base {
			t.Fatalf("%s: shared tools differ from slbh's", shape)
		}
	}
}

func TestForeignPrimaryToolsAreRefused(t *testing.T) {
	r, dir := shapedRuntime(t, config.ToolShapeAnthropic)
	writeFile(t, filepath.Join(dir, "a.txt"), "a\n")
	if _, err := call(t, r, "read_file", map[string]any{"path": "a.txt"}); err == nil || err.Error() != "<tool_use_error>Error: No such tool available: read_file.</tool_use_error>" {
		t.Fatalf("read_file under anthropic: %v", err)
	}
	if _, err := call(t, r, "exec_command", map[string]any{"cmd": "true"}); err == nil {
		t.Fatal("exec_command accepted under anthropic")
	}
	c, _ := shapedRuntime(t, config.ToolShapeCodex)
	if out, _ := call(t, c, "Read", map[string]any{"file_path": "a.txt"}); out != "unsupported call: Read" {
		t.Fatalf("Read under codex = %q", out)
	}
	// apply_patch is in both slbh and codex; under codex it takes input.
	writeFile(t, filepath.Join(c.workDir, "p.txt"), "one\n")
	out, err := call(t, c, "apply_patch", map[string]any{"input": "*** Begin Patch\n*** Update File: p.txt\n@@\n-one\n+two\n*** End Patch"})
	if err != nil || !strings.Contains(out, "M p.txt") {
		t.Fatalf("codex apply_patch: %q %v", out, err)
	}
}

func TestBakedPromptNamesTheShapesCommandTool(t *testing.T) {
	for shape, want := range map[string]string{config.ToolShapeSlbh: "Your command tools are bash, python.", config.ToolShapeAnthropic: "Your command tools are Bash, python.", config.ToolShapeCodex: "Your command tools are exec_command, python."} {
		r, _ := shapedRuntime(t, shape)
		if prompt := bakedSystemPrompt(r.seat()); !strings.Contains(prompt, want) {
			t.Fatalf("%s prompt lacks %q", shape, want)
		}
	}
}

func TestAnthropicReadMatchesCapture(t *testing.T) {
	r, dir := shapedRuntime(t, config.ToolShapeAnthropic)
	a := filepath.Join(dir, "a.txt")
	writeFile(t, a, "alpha\nbeta\ngamma beta\n")
	writeFile(t, filepath.Join(dir, "empty.txt"), "")
	for _, tc := range []struct {
		values map[string]any
		want   string
		flag   bool
	}{
		{map[string]any{"file_path": a}, "1\talpha\n2\tbeta\n3\tgamma beta\n4\t", false},
		{map[string]any{"file_path": a, "offset": 2, "limit": 1}, "2\tbeta", false},
		{map[string]any{"file_path": "a.txt", "offset": 0, "limit": 2}, "1\talpha\n2\tbeta", false},
		{map[string]any{"file_path": a, "offset": 50}, "<system-reminder>Warning: the file exists but is shorter than the provided offset (50). The file has 4 lines.</system-reminder>", false},
		{map[string]any{"file_path": filepath.Join(dir, "empty.txt")}, "<system-reminder>Warning: the file exists but the contents are empty.</system-reminder>", false},
		{map[string]any{"file_path": filepath.Join(dir, "missing.txt")}, "File does not exist. Note: your current working directory is " + dir + ".", true},
		{map[string]any{"file_path": dir}, "EISDIR: illegal operation on a directory, read '" + dir + "'", true},
	} {
		got, flag := shaped(t, r, "Read", tc.values)
		if got != tc.want || flag != tc.flag {
			t.Fatalf("Read %v = %q (error %v), want %q (error %v)", tc.values, got, flag, tc.want, tc.flag)
		}
	}
	var many strings.Builder
	for i := 1; i <= 2500; i++ {
		many.WriteString("x\n")
	}
	writeFile(t, filepath.Join(dir, "many.txt"), many.String())
	got, _ := shaped(t, r, "Read", map[string]any{"file_path": "many.txt"})
	if lines := strings.Count(got, "\n") + 1; lines != 2000 {
		t.Fatalf("default Read returned %d lines, want 2000", lines)
	}
}

func TestAnthropicEditAndWriteMatchCapture(t *testing.T) {
	r, dir := shapedRuntime(t, config.ToolShapeAnthropic)
	a := filepath.Join(dir, "a.txt")
	writeFile(t, a, "alpha\nbeta\ngamma beta\n")
	state := " (file state is current in your context — no need to Read it back)"
	for _, tc := range []struct {
		values map[string]any
		want   string
		flag   bool
	}{
		{map[string]any{"file_path": a, "old_string": "alpha", "new_string": "ALPHA"}, "The file " + a + " has been updated successfully." + state, false},
		{map[string]any{"file_path": a, "old_string": "beta", "new_string": "B"}, "<tool_use_error>Found 2 matches of the string to replace, but replace_all is false. To replace all occurrences, set replace_all to true. To replace only one occurrence, please provide more context to uniquely identify the instance.\nString: beta</tool_use_error>", true},
		{map[string]any{"file_path": a, "old_string": "nothere", "new_string": "B"}, "<tool_use_error>String to replace not found in file.\nString: nothere</tool_use_error>", true},
		{map[string]any{"file_path": a, "old_string": "beta", "new_string": "B", "replace_all": true}, "The file " + a + " has been updated. All occurrences were successfully replaced." + state, false},
		{map[string]any{"file_path": a, "old_string": "B", "new_string": "B"}, "<tool_use_error>No changes to make: old_string and new_string are exactly the same.</tool_use_error>", true},
		{map[string]any{"file_path": filepath.Join(dir, "nofile.txt"), "old_string": "a", "new_string": "b"}, "<tool_use_error>File does not exist. Note: your current working directory is " + dir + ".</tool_use_error>", true},
		{map[string]any{"file_path": "made.txt", "old_string": "", "new_string": "made\n"}, "The file made.txt has been updated successfully." + state, false},
		{map[string]any{"file_path": "made.txt", "old_string": "", "new_string": "again\n"}, "<tool_use_error>Cannot create new file - file already exists.</tool_use_error>", true},
	} {
		got, flag := shaped(t, r, "Edit", tc.values)
		if got != tc.want || flag != tc.flag {
			t.Fatalf("Edit %v = %q (error %v), want %q", tc.values, got, flag, tc.want)
		}
	}
	if got := readFile(t, a); got != "ALPHA\nB\ngamma B\n" {
		t.Fatalf("a.txt = %q", got)
	}
	if got := readFile(t, filepath.Join(dir, "made.txt")); got != "made\n" {
		t.Fatalf("made.txt = %q", got)
	}

	// A file changed on disk after it was read still edits, with the note.
	r2 := filepath.Join(dir, "r.txt")
	writeFile(t, r2, "r\n")
	shaped(t, r, "Read", map[string]any{"file_path": r2})
	time.Sleep(10 * time.Millisecond)
	writeFile(t, r2, "r\nchanged\n")
	got, flag := shaped(t, r, "Edit", map[string]any{"file_path": r2, "old_string": "r\n", "new_string": "R\n"})
	if flag || !strings.HasSuffix(got, anthropicStaleNote) {
		t.Fatalf("stale edit = %q", got)
	}

	got, flag = shaped(t, r, "Write", map[string]any{"file_path": filepath.Join(dir, "sub", "new.txt"), "content": "fresh\n"})
	if flag || got != "File created successfully at: "+filepath.Join(dir, "sub", "new.txt")+state {
		t.Fatalf("Write new = %q", got)
	}
	got, _ = shaped(t, r, "Write", map[string]any{"file_path": a, "content": "over\n"})
	if got != "The file "+a+" has been updated successfully."+state || readFile(t, a) != "over\n" {
		t.Fatalf("Write overwrite = %q", got)
	}
}

func TestAnthropicBashMatchesCapture(t *testing.T) {
	r, dir := shapedRuntime(t, config.ToolShapeAnthropic)
	for _, tc := range []struct {
		command string
		want    string
		flag    bool
	}{
		{"echo hi; echo err >&2", "hi\nerr", false},
		{"echo out; exit 3", "Exit code 3\nout", true},
		{"echo only-err >&2; exit 2", "Exit code 2\nonly-err", true},
		{"true", "(Bash completed with no output)", false},
		{"mkdir -p sub; cd sub", "(Bash completed with no output)", false},
		{"pwd", filepath.Join(dir, "sub"), false},
		{"cd / && pwd", "/\nShell cwd was reset to " + dir, false},
		{"pwd", dir, false},
	} {
		got, flag := shaped(t, r, "Bash", map[string]any{"command": tc.command, "description": "x"})
		if got != tc.want || flag != tc.flag {
			t.Fatalf("Bash %q = %q (error %v), want %q (error %v)", tc.command, got, flag, tc.want, tc.flag)
		}
	}
	got, flag := shaped(t, r, "Bash", map[string]any{"command": "sleep 5", "timeout": 300})
	if !flag || got != "Exit code 143\nCommand timed out after 0.3s" {
		t.Fatalf("timeout = %q (error %v)", got, flag)
	}
	got, _ = shaped(t, r, "Bash", map[string]any{"command": "seq 1 30000"})
	if !strings.HasPrefix(got, "<persisted-output>\nOutput too large (164.9KB). Full output saved to: ") || !strings.HasSuffix(got, "\n...\n</persisted-output>") || !strings.Contains(got, "Preview (first 2KB):\n1\n2\n3\n") {
		t.Fatalf("large output = %q", got[:200])
	}
	got, flag = shaped(t, r, "Bash", map[string]any{"command": "echo bg", "run_in_background": true})
	if flag || !strings.HasPrefix(got, "Command running in background with ID: job-") {
		t.Fatalf("background = %q", got)
	}
}

var chunkHeader = regexp.MustCompile(`^Chunk ID: [0-9a-f]{6}\nWall time: \d+\.\d{4} seconds\n`)

func TestCodexExecMatchesCapture(t *testing.T) {
	r, _ := shapedRuntime(t, config.ToolShapeCodex)
	out, err := call(t, r, "exec_command", map[string]any{"cmd": "echo err >&2; echo hi"})
	if err != nil || !chunkHeader.MatchString(out) || !strings.HasSuffix(out, "Process exited with code 0\nOriginal token count: 2\nOutput:\nerr\nhi\n") {
		t.Fatalf("exec = %q %v", out, err)
	}
	out, err = call(t, r, "exec_command", map[string]any{"cmd": "echo out; exit 3"})
	if err != nil || !strings.HasSuffix(out, "Process exited with code 3\nOriginal token count: 1\nOutput:\nout\n") {
		t.Fatalf("non-zero exit must be ordinary output: %q %v", out, err)
	}
	out, _ = call(t, r, "exec_command", map[string]any{"cmd": "sleep 1; echo late", "yield_time_ms": 250})
	match := regexp.MustCompile(`Process running with session ID (\d+)\nOriginal token count: 0\nOutput:\n$`).FindStringSubmatch(out)
	if match == nil {
		t.Fatalf("yield = %q", out)
	}
	out, _ = call(t, r, "write_stdin", map[string]any{"session_id": json.Number(match[1]), "chars": "", "yield_time_ms": 5000})
	if !strings.HasSuffix(out, "Process exited with code 0\nOriginal token count: 2\nOutput:\nlate\n") {
		t.Fatalf("poll = %q", out)
	}
	out, _ = call(t, r, "write_stdin", map[string]any{"session_id": json.Number(match[1])})
	if !strings.HasPrefix(out, "write_stdin failed: Unknown process id") {
		t.Fatalf("finished session still open: %q", out)
	}
	out, _ = call(t, r, "exec_command", map[string]any{"cmd": "seq 1 30000"})
	if !strings.Contains(out, "Original token count: 42224\nOutput:\nWarning: truncated output (original token count: 42224)\nTotal output lines: 30000\n\n1\n2\n") || !strings.Contains(out, "…32224 tokens truncated…") || !strings.HasSuffix(out, "29999\n30000\n") {
		t.Fatalf("truncation = %q", out[:300])
	}
	body := out[strings.Index(out, "\n\n")+2:]
	head := body[:strings.Index(body, "…")]
	tail := body[strings.LastIndex(body, "…")+len("…"):]
	if len(head) != 20000 || len(tail) != 20000 {
		t.Fatalf("head %d tail %d, want 20000 each", len(head), len(tail))
	}
}

func TestCodexApplyPatch(t *testing.T) {
	r, dir := shapedRuntime(t, config.ToolShapeCodex)
	a := filepath.Join(dir, "a.txt")
	writeFile(t, a, "alpha\nbeta\n")
	patch := func(body string) string {
		out, err := call(t, r, "apply_patch", map[string]any{"input": "*** Begin Patch\n" + body + "*** End Patch\n"})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	if out := patch("*** Add File: b.txt\n+one\n+two\n"); out != "Exit code: 0\nWall time: 0 seconds\nOutput:\nSuccess. Updated the following files:\nA b.txt\n" {
		t.Fatalf("add = %q", out)
	}
	if out := patch("*** Update File: a.txt\n@@\n alpha\n-beta\n+BETA\n"); !strings.HasSuffix(out, "M a.txt\n") || readFile(t, a) != "alpha\nBETA\n" {
		t.Fatalf("update = %q, file %q", out, readFile(t, a))
	}
	before := readFile(t, a)
	out := patch("*** Add File: c.txt\n+never\n*** Update File: a.txt\n@@\n-nothere\n+x\n")
	if out != "apply_patch verification failed: Failed to find expected lines in "+a+":\nnothere" {
		t.Fatalf("failed hunk = %q", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "c.txt")); !os.IsNotExist(err) || readFile(t, a) != before {
		t.Fatal("a failed patch changed the tree")
	}
	writeFile(t, filepath.Join(dir, "code.py"), "def f():\n    return 1\n\ndef g():\n    return 1\n")
	patch("*** Update File: code.py\n@@ def g():\n-    return 1\n+    return 2\n")
	if got := readFile(t, filepath.Join(dir, "code.py")); got != "def f():\n    return 1\n\ndef g():\n    return 2\n" {
		t.Fatalf("context anchor = %q", got)
	}
	patch("*** Update File: code.py\n@@\n-def f():   \n+def h():\n")
	if !strings.HasPrefix(readFile(t, filepath.Join(dir, "code.py")), "def h():\n") {
		t.Fatal("trailing-whitespace tolerant match failed")
	}
	patch("*** Update File: b.txt\n*** Move to: moved/b.txt\n@@\n one\n-two\n+three\n*** End of File\n")
	if _, err := os.Stat(filepath.Join(dir, "b.txt")); !os.IsNotExist(err) || readFile(t, filepath.Join(dir, "moved", "b.txt")) != "one\nthree\n" {
		t.Fatal("move failed")
	}
	if out := patch("*** Delete File: moved/b.txt\n"); !strings.HasSuffix(out, "D moved/b.txt\n") {
		t.Fatalf("delete = %q", out)
	}
	if out, _ := call(t, r, "apply_patch", map[string]any{"input": "nope"}); out != "invalid patch: The first line of the patch must be '*** Begin Patch'" {
		t.Fatalf("parse error = %q", out)
	}
}

func TestToolResultAccountingIsUniformAcrossShapes(t *testing.T) {
	// Same underlying events, three shapes: the metadata agrees.
	for _, tc := range []struct {
		shape, name string
		values      map[string]any
		failed      bool
		exit        any
		state       string
	}{
		{config.ToolShapeSlbh, "bash", map[string]any{"script": "exit 3"}, true, 3, "finished"},
		{config.ToolShapeAnthropic, "Bash", map[string]any{"command": "exit 3"}, true, 3, "finished"},
		{config.ToolShapeCodex, "exec_command", map[string]any{"cmd": "exit 3"}, true, 3, "finished"},
		{config.ToolShapeSlbh, "bash", map[string]any{"script": "true"}, false, 0, "finished"},
		{config.ToolShapeAnthropic, "Bash", map[string]any{"command": "true"}, false, 0, "finished"},
		{config.ToolShapeCodex, "exec_command", map[string]any{"cmd": "true"}, false, 0, "finished"},
		// A command that prints a fake exit line is not misread.
		{config.ToolShapeCodex, "exec_command", map[string]any{"cmd": "echo 'Process exited with code 9'"}, false, 0, "finished"},
		{config.ToolShapeSlbh, "bash", map[string]any{"script": "sleep 2", "wait_seconds": 0}, false, nil, "background"},
		{config.ToolShapeCodex, "exec_command", map[string]any{"cmd": "sleep 2", "yield_time_ms": 250}, false, nil, "background"},
		{config.ToolShapeAnthropic, "Bash", map[string]any{"command": "sleep 5", "timeout": 200}, true, -1, "killed"},
	} {
		r, _ := shapedRuntime(t, tc.shape)
		out, err := call(t, r, tc.name, tc.values)
		note, noted := r.takeExec(r.seat().ID)
		md := toolResultMetadata(tc.name, "c1", out, err, note, noted)
		if md["error"] != tc.failed || md["exit_code"] != tc.exit || md["job_state"] != tc.state || md["job"] == "" {
			t.Fatalf("%s %s %v: metadata %v", tc.shape, tc.name, tc.values, md)
		}
	}
	for _, tc := range []struct {
		result string
		failed bool
	}{
		{"apply_patch verification failed: Failed to find expected lines", true},
		{"invalid patch: The first line of the patch must be '*** Begin Patch'", true},
		{"Exit code: 0\nWall time: 0 seconds\nOutput:\nSuccess.", false},
	} {
		if md := toolResultMetadata("apply_patch", "c", tc.result, nil, execNote{}, false); md["error"] != tc.failed {
			t.Fatalf("apply_patch %q: %v", tc.result, md)
		}
	}
	for _, tc := range []struct{ name, result string }{{"shell", "unsupported call: shell"}, {"apply_patch", "failed to parse function arguments: missing field `input`"}, {"exec_command", "failed to parse function arguments: EOF"}} {
		if md := toolResultMetadata(tc.name, "c", tc.result, nil, execNote{}, false); md["error"] != true {
			t.Fatalf("codex validation failure %q not counted: %v", tc.result, md)
		}
	}
	if md := toolResultMetadata("Edit", "c", "", toolUseError("String to replace not found"), execNote{}, false); md["error"] != true {
		t.Fatalf("shaped Edit failure not counted: %v", md)
	}
}

func TestShapedValidationUsesTheHarnessesWords(t *testing.T) {
	a, _ := shapedRuntime(t, config.ToolShapeAnthropic)
	for name, want := range map[string]string{
		"Glob": "<tool_use_error>Error: No such tool available: Glob. Glob is not available in this session — find files with `find` via the Bash tool instead.</tool_use_error>",
		"Grep": "<tool_use_error>Error: No such tool available: Grep. Grep is not available in this session — search file contents with `grep` via the Bash tool instead.</tool_use_error>",
	} {
		if got, flag := shaped(t, a, name, map[string]any{"pattern": "x"}); got != want || !flag {
			t.Fatalf("%s = %q", name, got)
		}
	}
	if got, flag := shaped(t, a, "Read", map[string]any{}); !flag || got != "<tool_use_error>InputValidationError: Read failed due to the following issue:\nThe required parameter `file_path` is missing</tool_use_error>" {
		t.Fatalf("missing parameter = %q", got)
	}
	if _, err := a.ExecuteTool(a.seat().ID, "Read", "{not json"); err == nil || !strings.Contains(err.Error(), "InputValidationError") {
		t.Fatalf("bad JSON = %v", err)
	}
	c, _ := shapedRuntime(t, config.ToolShapeCodex)
	if out, err := call(t, c, "shell", map[string]any{"command": []string{"ls"}}); err != nil || out != "unsupported call: shell" {
		t.Fatalf("codex unknown = %q %v", out, err)
	}
	if out, _ := c.ExecuteTool(c.seat().ID, "exec_command", "{"); !strings.HasPrefix(out, "failed to parse function arguments") {
		t.Fatalf("codex bad JSON = %q", out)
	}
	// Shared tools keep slbh's handling in every shape.
	if _, err := call(t, c, "job", map[string]any{"action": "list"}); err != nil {
		t.Fatalf("shared job tool: %v", err)
	}
}

func TestQuiescentSeesRunningJobsAndPendingInboxes(t *testing.T) {
	r, _ := shapedRuntime(t, config.ToolShapeSlbh)
	if !r.Quiescent() {
		t.Fatal("a fresh runtime is not quiescent")
	}
	if _, err := call(t, r, "bash", map[string]any{"script": "sleep 1", "wait_seconds": 0}); err != nil {
		t.Fatal(err)
	}
	if r.Quiescent() {
		t.Fatal("quiescent with a job running")
	}
	seat := r.seat()
	seat.mu.Lock()
	seat.inbox = append(seat.inbox, agentMessage{prompt: "x", kind: "steer"})
	seat.mu.Unlock()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		seat.mu.RLock()
		pending := len(seat.inbox) > 0 || seat.busy || seat.turnCancel != nil
		seat.mu.RUnlock()
		if pending && r.Quiescent() {
			t.Fatal("quiescent with a message pending or a turn running")
		}
		if !pending && r.Quiescent() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}
