package orgcli

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/slbdotdev/slbh/internal/orgstore"
)

func TestInboxHumanAndJSON(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SLBH_HOME", home)
	store := openStore(t, home)
	first, err := store.AppendReport("seat", "nothing invalidated", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.AppendReport("seat", "pages changed", []string{"org/a.md", "org/b.md"}, false)
	if err != nil {
		t.Fatal(err)
	}

	code, output, errors := invoke([]string{"inbox"}, "")
	if code != 0 || errors != "" {
		t.Fatalf("inbox: code=%d stderr=%q", code, errors)
	}
	for _, want := range []string{
		"report 1\n", "from: seat\n", "text: nothing invalidated\n", "invalidates: none\n",
		"report 2\n", "text: pages changed\n", "invalidates: org/a.md, org/b.md\n",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("human inbox missing %q:\n%s", want, output)
		}
	}
	if !strings.Contains(output, "time: "+first.Time.Format("2006-01-02")) ||
		!strings.Contains(output, "time: "+second.Time.Format("2006-01-02")) {
		t.Errorf("human inbox did not include report times:\n%s", output)
	}

	code, output, errors = invoke([]string{"inbox", "--json"}, "")
	if code != 0 || errors != "" {
		t.Fatalf("inbox --json: code=%d stderr=%q", code, errors)
	}
	var reports []orgstore.Report
	if err := json.Unmarshal([]byte(output), &reports); err != nil {
		t.Fatalf("decode inbox JSON: %v\n%s", err, output)
	}
	if len(reports) != 2 || reports[0].ID != first.ID || reports[1].ID != second.ID || !reports[0].InvalidatesNone {
		t.Fatalf("unexpected inbox JSON: %+v", reports)
	}
}

func TestAckOutputErrorsAndExitCodes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SLBH_HOME", home)
	store := openStore(t, home)
	for range 2 {
		if _, err := store.AppendReport("seat", "report", nil, true); err != nil {
			t.Fatal(err)
		}
	}

	code, output, errors := invoke([]string{"ack", "2"}, "")
	if code != 0 || output != "acknowledged report 2\n" || errors != "" {
		t.Fatalf("ack: code=%d stdout=%q stderr=%q", code, output, errors)
	}
	code, output, errors = invoke([]string{"ack", "2", "--json"}, "")
	if code != 0 || output != "{\"acknowledged_id\":2}\n" || errors != "" {
		t.Fatalf("ack --json: code=%d stdout=%q stderr=%q", code, output, errors)
	}

	code, _, errors = invoke([]string{"ack", "1"}, "")
	if code != 1 || !strings.Contains(errors, "cannot move org inbox cursor backwards") {
		t.Fatalf("backwards ack: code=%d stderr=%q", code, errors)
	}
	code, _, errors = invoke([]string{"ack", "99"}, "")
	if code != 1 || !strings.Contains(errors, "cannot acknowledge unknown report 99") {
		t.Fatalf("unknown ack: code=%d stderr=%q", code, errors)
	}
}

func TestRequestHumanJSONFileAndStdin(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SLBH_HOME", home)

	code, output, errors := invoke([]string{"request", "first request"}, "")
	if code != 0 || output != "1\n" || errors != "" {
		t.Fatalf("request: code=%d stdout=%q stderr=%q", code, output, errors)
	}
	code, output, errors = invoke([]string{"request", "-f", "-", "--json"}, "from stdin")
	if code != 0 || errors != "" {
		t.Fatalf("request -f - --json: code=%d stderr=%q", code, errors)
	}
	var request orgstore.Request
	if err := json.Unmarshal([]byte(output), &request); err != nil {
		t.Fatalf("decode request JSON: %v\n%s", err, output)
	}
	if request.ID != 2 || request.From != "secretary" || request.Text != "from stdin" || request.Status != orgstore.StatusQueued {
		t.Fatalf("unexpected request JSON: %+v", request)
	}

	path := filepath.Join(t.TempDir(), "request.txt")
	if err := os.WriteFile(path, []byte("from file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, output, errors = invoke([]string{"request", "-f", path}, "")
	if code != 0 || output != "3\n" || errors != "" {
		t.Fatalf("request -f file: code=%d stdout=%q stderr=%q", code, output, errors)
	}
}

func TestRequestsHumanJSONAndOpenFilter(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SLBH_HOME", home)
	store := openStore(t, home)
	queued := appendRequest(t, store, "queued work")
	accepted := appendRequest(t, store, "accepted work")
	if _, err := store.UpdateRequestStatus(accepted.ID, orgstore.StatusAccepted, "started"); err != nil {
		t.Fatal(err)
	}
	done := appendRequest(t, store, "done work")
	if _, err := store.UpdateRequestStatus(done.ID, orgstore.StatusAccepted, "starting"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateRequestStatus(done.ID, orgstore.StatusDone, "finished"); err != nil {
		t.Fatal(err)
	}
	declined := appendRequest(t, store, "declined work")
	if _, err := store.UpdateRequestStatus(declined.ID, orgstore.StatusDeclined, "not now"); err != nil {
		t.Fatal(err)
	}

	code, output, errors := invoke([]string{"requests"}, "")
	if code != 0 || errors != "" {
		t.Fatalf("requests: code=%d stderr=%q", code, errors)
	}
	for _, want := range []string{
		"request 1\n", "text: queued work\n", "status: queued\n", "request 2\n",
		"status: accepted\n", "accepted: started\n", "request 3\n", "done: finished\n",
		"request 4\n", "declined: not now\n",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("human requests missing %q:\n%s", want, output)
		}
	}

	code, output, errors = invoke([]string{"requests", "--json"}, "")
	if code != 0 || errors != "" {
		t.Fatalf("requests --json: code=%d stderr=%q", code, errors)
	}
	all := decodeRequests(t, output)
	if len(all) != 4 || len(all[2].History) != 3 {
		t.Fatalf("unexpected requests JSON: %+v", all)
	}

	code, output, errors = invoke([]string{"requests", "--open"}, "")
	if code != 0 || errors != "" || !strings.Contains(output, "request 1\n") || !strings.Contains(output, "request 2\n") || strings.Contains(output, "request 3\n") || strings.Contains(output, "request 4\n") {
		t.Fatalf("requests --open: code=%d stdout=%q stderr=%q", code, output, errors)
	}
	code, output, errors = invoke([]string{"requests", "--json", "--open"}, "")
	if code != 0 || errors != "" {
		t.Fatalf("requests --open --json: code=%d stderr=%q", code, errors)
	}
	open := decodeRequests(t, output)
	if len(open) != 2 || open[0].ID != queued.ID || open[1].ID != accepted.ID {
		t.Fatalf("unexpected open requests: %+v", open)
	}
}

func TestHelpHumanAndJSON(t *testing.T) {
	code, output, errors := invoke([]string{"help"}, "")
	if code != 0 || output != Usage || errors != "" {
		t.Fatalf("help: code=%d stdout=%q stderr=%q", code, output, errors)
	}
	code, output, errors = invoke([]string{"help", "--json"}, "")
	if code != 0 || errors != "" {
		t.Fatalf("help --json: code=%d stderr=%q", code, errors)
	}
	var result map[string]string
	if err := json.Unmarshal([]byte(output), &result); err != nil || result["usage"] != Usage {
		t.Fatalf("help JSON: err=%v result=%q", err, result)
	}
}

func TestUnknownCommandAndBadUsageExitTwo(t *testing.T) {
	t.Setenv("SLBH_HOME", t.TempDir())
	tests := [][]string{
		{"unknown"},
		{"inbox", "extra"},
		{"ack"},
		{"ack", "zero"},
		{"request"},
		{"request", "-f"},
		{"requests", "--bad"},
		{"help", "extra"},
	}
	for _, args := range tests {
		code, _, errors := invoke(args, "")
		if code != 2 || !strings.Contains(errors, Usage) {
			t.Errorf("%v: code=%d stderr=%q", args, code, errors)
		}
	}

	code, output, errors := invoke([]string{"unknown", "--json"}, "")
	if code != 2 || output != "" {
		t.Fatalf("unknown --json: code=%d stdout=%q", code, output)
	}
	var result map[string]string
	if err := json.Unmarshal([]byte(errors), &result); err != nil || !strings.Contains(result["error"], "unknown command") {
		t.Fatalf("unknown JSON error: err=%v result=%q", err, result)
	}
}

func TestPackageDoesNotDependOnHarness(t *testing.T) {
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not locate test source")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(source), "..", ".."))
	command := exec.Command("go", "list", "-deps", "./internal/orgcli")
	command.Dir = root
	command.Env = replaceEnvironment(os.Environ(), "GOTOOLCHAIN", "go1.27.0")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps: %v\n%s", err, output)
	}
	for _, dependency := range strings.Fields(string(output)) {
		if dependency == "github.com/slbdotdev/slbh/internal/harness" {
			t.Fatal("internal/orgcli depends on internal/harness")
		}
	}
}

func invoke(args []string, input string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := Run(args, strings.NewReader(input), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func openStore(t *testing.T, home string) *orgstore.Store {
	t.Helper()
	store, err := orgstore.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func appendRequest(t *testing.T, store *orgstore.Store, text string) orgstore.Request {
	t.Helper()
	request, err := store.AppendRequest("secretary", text)
	if err != nil {
		t.Fatal(err)
	}
	return request
}

func decodeRequests(t *testing.T, output string) []orgstore.Request {
	t.Helper()
	var requests []orgstore.Request
	if err := json.Unmarshal([]byte(output), &requests); err != nil {
		t.Fatalf("decode requests JSON: %v\n%s", err, output)
	}
	return requests
}

func replaceEnvironment(environment []string, name, value string) []string {
	prefix := name + "="
	out := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		if !strings.HasPrefix(entry, prefix) {
			out = append(out, entry)
		}
	}
	return append(out, prefix+value)
}
