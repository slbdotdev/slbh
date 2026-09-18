package readtools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func toolArgs(t *testing.T, values map[string]any) string {
	t.Helper()
	data, err := json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestDefinitionsExposeOnlyReadToolsInStableOrder(t *testing.T) {
	definitions := Definitions()
	got := make([]string, len(definitions))
	for i, definition := range definitions {
		got[i] = definition.Name
		if definition.Description == "" || definition.Parameters == nil {
			t.Fatalf("definition %q is incomplete: %#v", definition.Name, definition)
		}
	}
	want := []string{"glob", "grep", "read_file", "read_bytes", "read_lines"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("definition names = %v, want %v", got, want)
	}
}

func TestExecuteReadTools(t *testing.T) {
	work := t.TempDir()
	if err := os.MkdirAll(filepath.Join(work, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(work, "nested", "sample.txt")
	if err := os.WriteFile(path, []byte("alpha\nbeta\ngamma\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	matches, err := Execute(work, "glob", toolArgs(t, map[string]any{"pattern": "**/*.txt"}))
	if err != nil || matches != filepath.Join("nested", "sample.txt") {
		t.Fatalf("glob = %q, %v", matches, err)
	}
	grepResult, err := Execute(work, "grep", toolArgs(t, map[string]any{"pattern": "^beta$"}))
	if err != nil || grepResult != filepath.Join("nested", "sample.txt")+":2:beta\n" {
		t.Fatalf("grep = %q, %v", grepResult, err)
	}
	whole, err := Execute(work, "read_file", toolArgs(t, map[string]any{"path": "nested/sample.txt"}))
	if err != nil || whole != "alpha\nbeta\ngamma\n" {
		t.Fatalf("read_file = %q, %v", whole, err)
	}
	byteRange, err := Execute(work, "read_bytes", toolArgs(t, map[string]any{"path": "nested/sample.txt", "start": 6, "end": 9}))
	if err != nil || byteRange != "beta" {
		t.Fatalf("read_bytes = %q, %v", byteRange, err)
	}
	lineRange, err := Execute(work, "read_lines", toolArgs(t, map[string]any{"path": "nested/sample.txt", "start": 2, "end": 3}))
	if err != nil || lineRange != "2:beta\n3:gamma\n" {
		t.Fatalf("read_lines = %q, %v", lineRange, err)
	}
}

func TestExecuteHasNoWriteOrExecutionTools(t *testing.T) {
	work := t.TempDir()
	target := filepath.Join(work, "created.txt")
	_, err := Execute(work, "write_file", toolArgs(t, map[string]any{"path": target, "content": "created"}))
	if err == nil || !strings.Contains(err.Error(), "unknown read tool") {
		t.Fatalf("write_file error = %v, want unknown read tool", err)
	}
	if _, statErr := os.Stat(target); !os.IsNotExist(statErr) {
		t.Fatalf("write_file changed filesystem: %v", statErr)
	}
	if _, err := Execute(work, "quick_bash", `{"script":"touch created.txt"}`); err == nil || !strings.Contains(err.Error(), "unknown read tool") {
		t.Fatalf("quick_bash error = %v, want unknown read tool", err)
	}
}
