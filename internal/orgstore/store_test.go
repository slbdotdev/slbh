package orgstore

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
)

const processHelperEnv = "SLBH_ORGSTORE_HELPER"

func TestMain(m *testing.M) {
	if os.Getenv(processHelperEnv) == "1" {
		runAppendReportHelper()
		return
	}
	os.Exit(m.Run())
}

func runAppendReportHelper() {
	home := os.Getenv("SLBH_ORGSTORE_HOME")
	count, err := strconv.Atoi(os.Getenv("SLBH_ORGSTORE_COUNT"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	store, err := Open(home)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	for index := 0; index < count; index++ {
		if _, err := store.AppendReport("helper", fmt.Sprintf("report %d", index), nil, true); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
	}
}

func TestReportRoundTripValidationAndAck(t *testing.T) {
	home := t.TempDir()
	store, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendReport("seat", "neither", nil, false); err == nil {
		t.Fatal("report with neither invalidation form was accepted")
	}
	if _, err := store.AppendReport("seat", "both", []string{"org/charter.md"}, true); err == nil {
		t.Fatal("report with both invalidation forms was accepted")
	}
	if _, err := store.AppendReport("seat", "empty path", []string{" "}, false); err == nil {
		t.Fatal("report with an empty invalidation path was accepted")
	}
	first, err := store.AppendReport("seat", "changed charter", []string{"org/charter.md"}, false)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.AppendReport("seat", "no page change", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	third, err := store.AppendReport("seat", "changed roles", []string{"org/roles.md"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != 1 || second.ID != 2 || third.ID != 3 {
		t.Fatalf("report ids = %d, %d, %d", first.ID, second.ID, third.ID)
	}
	pending, err := store.Pending()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 3 || pending[0].Text != "changed charter" || pending[1].InvalidatesNone != true {
		t.Fatalf("pending reports = %#v", pending)
	}
	if err := store.Ack(second.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.Ack(first.ID); err == nil {
		t.Fatal("backwards acknowledgement was accepted")
	}
	if err := store.Ack(99); err == nil {
		t.Fatal("unknown acknowledgement was accepted")
	}
	if err := store.Ack(second.ID); err != nil {
		t.Fatalf("repeated acknowledgement: %v", err)
	}

	reopened, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	pending, err = reopened.Pending()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ID != third.ID {
		t.Fatalf("pending after reopen = %#v", pending)
	}
	if err := reopened.Ack(third.ID); err != nil {
		t.Fatal(err)
	}
	pending, err = store.Pending()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending after final acknowledgement = %#v", pending)
	}
}

func TestClearAdvancesCursorAndRetainsAppendOnlyLog(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := store.AppendReport("seat", "report", nil, true); err != nil {
			t.Fatal(err)
		}
	}

	cleared, err := store.Clear()
	if err != nil {
		t.Fatal(err)
	}
	if cleared != 3 {
		t.Fatalf("cleared reports = %d, want 3", cleared)
	}
	pending, err := store.Pending()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending after clear = %#v", pending)
	}

	if _, err := store.AppendReport("seat", "after clear", nil, true); err != nil {
		t.Fatal(err)
	}
	pending, err = store.Pending()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ID != 4 {
		t.Fatalf("pending after append = %#v", pending)
	}
	data, err := os.ReadFile(store.inboxLog)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Count(data, []byte{'\n'}) != 4 {
		t.Fatalf("clear removed append-only records: %q", data)
	}
}

func TestRequestRoundTripAndStatusRules(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.AppendRequest("secretary", "write the page")
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != 1 || first.Status != StatusQueued || len(first.History) != 1 || first.History[0].Status != StatusQueued {
		t.Fatalf("new request = %#v", first)
	}
	if _, err := store.UpdateRequestStatus(first.ID, StatusQueued, ""); err == nil {
		t.Fatal("explicit queued status was accepted")
	}
	if _, err := store.UpdateRequestStatus(99, StatusAccepted, ""); err == nil {
		t.Fatal("status for unknown request was accepted")
	}
	accepted, err := store.UpdateRequestStatus(first.ID, StatusAccepted, "starting")
	if err != nil {
		t.Fatal(err)
	}
	if accepted.Status != StatusAccepted || accepted.Note != "starting" {
		t.Fatalf("accepted change = %#v", accepted)
	}
	if _, err := store.UpdateRequestStatus(first.ID, StatusDone, "finished"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateRequestStatus(first.ID, StatusAccepted, "again"); err == nil {
		t.Fatal("terminal done request was changed")
	}
	second, err := store.AppendRequest("secretary", "do not do this")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateRequestStatus(second.ID, StatusDeclined, "out of scope"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateRequestStatus(second.ID, StatusDone, "changed mind"); err == nil {
		t.Fatal("terminal declined request was changed")
	}
	requests, err := store.Requests()
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 {
		t.Fatalf("requests = %#v", requests)
	}
	if requests[0].Status != StatusDone || len(requests[0].History) != 3 || requests[0].History[1].Note != "starting" {
		t.Fatalf("first folded request = %#v", requests[0])
	}
	if requests[1].Status != StatusDeclined || len(requests[1].History) != 2 {
		t.Fatalf("second folded request = %#v", requests[1])
	}
}

func TestConcurrentReportAppendsAcrossProcesses(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("cross-process flock behavior is Linux-specific")
	}
	const processes = 6
	const perProcess = 30
	home := t.TempDir()
	store, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	commands := make([]*exec.Cmd, processes)
	for index := range commands {
		command := exec.Command(os.Args[0], "-test.run=^$")
		command.Env = append(os.Environ(),
			processHelperEnv+"=1",
			"SLBH_ORGSTORE_HOME="+home,
			"SLBH_ORGSTORE_COUNT="+strconv.Itoa(perProcess),
		)
		command.Stdout = os.Stdout
		command.Stderr = os.Stderr
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		commands[index] = command
	}
	for _, command := range commands {
		if err := command.Wait(); err != nil {
			t.Fatal(err)
		}
	}
	assertReportLog(t, store, processes*perProcess)
}

func TestConcurrentReportAppendsAcrossGoroutines(t *testing.T) {
	const goroutines = 8
	const perGoroutine = 40
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var wait sync.WaitGroup
	errors := make(chan error, goroutines)
	for worker := 0; worker < goroutines; worker++ {
		wait.Add(1)
		go func(worker int) {
			defer wait.Done()
			for index := 0; index < perGoroutine; index++ {
				if _, err := store.AppendReport(fmt.Sprintf("worker-%d", worker), fmt.Sprintf("report-%d", index), nil, true); err != nil {
					errors <- err
					return
				}
			}
		}(worker)
	}
	wait.Wait()
	close(errors)
	for err := range errors {
		t.Error(err)
	}
	assertReportLog(t, store, goroutines*perGoroutine)
}

func assertReportLog(t *testing.T, store *Store, want int) {
	t.Helper()
	reports, err := store.Pending()
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != want {
		t.Fatalf("report count = %d, want %d", len(reports), want)
	}
	seen := make(map[uint64]bool, want)
	for index, report := range reports {
		wantID := uint64(index + 1)
		if report.ID != wantID {
			t.Fatalf("report %d id = %d, want %d", index, report.ID, wantID)
		}
		if seen[report.ID] {
			t.Fatalf("duplicate report id %d", report.ID)
		}
		seen[report.ID] = true
	}
	data, err := os.ReadFile(store.inboxLog)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasSuffix(data, []byte{'\n'}) || bytes.Count(data, []byte{'\n'}) != want {
		t.Fatalf("report log is not %d complete lines", want)
	}
	for number, line := range bytes.Split(bytes.TrimSuffix(data, []byte{'\n'}), []byte{'\n'}) {
		var record Report
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatalf("line %d is torn: %v", number+1, err)
		}
	}
}

func TestTornTrailingLinesAreSkippedAndRepaired(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendReport("seat", "complete", nil, true); err != nil {
		t.Fatal(err)
	}
	appendTorn(t, store.inboxLog, `{"id":2,"time":"torn`)
	reports, err := store.Pending()
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != 1 {
		t.Fatalf("reports with torn tail = %#v", reports)
	}
	if _, err := store.AppendReport("seat", "after repair", nil, true); err != nil {
		t.Fatal(err)
	}
	assertReportLog(t, store, 2)

	request, err := store.AppendRequest("secretary", "complete")
	if err != nil {
		t.Fatal(err)
	}
	appendTorn(t, store.requestsLog, `{"type":"status","id":1`)
	requests, err := store.Requests()
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 1 || requests[0].Status != StatusQueued {
		t.Fatalf("requests with torn tail = %#v", requests)
	}
	if _, err := store.UpdateRequestStatus(request.ID, StatusDone, "repaired"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(store.requestsLog)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasSuffix(data, []byte{'\n'}) || bytes.Contains(data, []byte(`{"type":"status","id":1{"type"`)) {
		t.Fatalf("request log was not repaired: %q", data)
	}
	requests, err = store.Requests()
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 1 || requests[0].Status != StatusDone {
		t.Fatalf("requests after repair = %#v", requests)
	}
}

func appendTorn(t *testing.T, path, fragment string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(fragment); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPackageDoesNotDependOnHarness(t *testing.T) {
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not locate test source")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(source), "..", ".."))
	command := exec.Command("go", "list", "-deps", "./internal/orgstore")
	command.Dir = root
	command.Env = withEnvironment(os.Environ(), "GOTOOLCHAIN", "go1.27.0")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps: %v\n%s", err, output)
	}
	for _, dependency := range strings.Fields(string(output)) {
		if dependency == "github.com/slbdotdev/slbh/internal/harness" {
			t.Fatal("internal/orgstore depends on internal/harness")
		}
	}
}

func withEnvironment(environment []string, name, value string) []string {
	prefix := name + "="
	out := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		if !strings.HasPrefix(entry, prefix) {
			out = append(out, entry)
		}
	}
	return append(out, prefix+value)
}
