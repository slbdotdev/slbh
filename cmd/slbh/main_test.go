package main

import (
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/slbdotdev/slbh/internal/orgstore"
)

func TestSecretaryCommandsFromBuiltBinaryDoNotConstructRuntime(t *testing.T) {
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not locate test source")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(source), "..", ".."))
	bin := filepath.Join(t.TempDir(), "slbh")
	build := exec.Command("go", "build", "-o", bin, "./cmd/slbh")
	build.Dir = root
	build.Env = commandEnvironment(os.Environ(), "GOTOOLCHAIN", "go1.27.0")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build slbh: %v\n%s", err, output)
	}

	home := t.TempDir()
	store, err := orgstore.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	report, err := store.AppendReport("seat", "binary report", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	environment := commandEnvironment(os.Environ(), "SLBH_HOME", home)

	output := runBinary(t, bin, environment, "request", "binary request")
	if output != "1\n" {
		t.Fatalf("request output = %q", output)
	}

	output = runBinary(t, bin, environment, "requests", "--json")
	var requests []orgstore.Request
	if err := json.Unmarshal([]byte(output), &requests); err != nil {
		t.Fatalf("decode requests output: %v\n%s", err, output)
	}
	if len(requests) != 1 || requests[0].Text != "binary request" || requests[0].From != "secretary" {
		t.Fatalf("unexpected requests: %+v", requests)
	}

	output = runBinary(t, bin, environment, "inbox", "--json")
	var reports []orgstore.Report
	if err := json.Unmarshal([]byte(output), &reports); err != nil {
		t.Fatalf("decode inbox output: %v\n%s", err, output)
	}
	if len(reports) != 1 || reports[0].ID != report.ID {
		t.Fatalf("unexpected reports: %+v", reports)
	}

	output = runBinary(t, bin, environment, "ack", "1")
	if output != "acknowledged report 1\n" {
		t.Fatalf("ack output = %q", output)
	}
	pending, err := store.Pending()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending reports after ack: %+v", pending)
	}

	if _, err := os.Stat(filepath.Join(home, "runtimes")); !os.IsNotExist(err) {
		t.Fatalf("runtime directory exists or could not be checked: %v", err)
	}
	if err := filepath.WalkDir(home, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Name() == "runtime.json" {
			t.Errorf("runtime marker unexpectedly created at %s", path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func runBinary(t *testing.T, bin string, environment []string, args ...string) string {
	t.Helper()
	command := exec.Command(bin, args...)
	command.Env = environment
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", bin, strings.Join(args, " "), err, output)
	}
	return string(output)
}

func commandEnvironment(environment []string, name, value string) []string {
	prefix := name + "="
	out := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		if !strings.HasPrefix(entry, prefix) {
			out = append(out, entry)
		}
	}
	return append(out, prefix+value)
}
