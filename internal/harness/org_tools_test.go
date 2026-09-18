package harness

import (
	"bytes"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/slbdotdev/slbh/internal/config"
	"github.com/slbdotdev/slbh/internal/orgcli"
	"github.com/slbdotdev/slbh/internal/orgstore"
	"github.com/slbdotdev/slbh/internal/provider"
)

func runHarnessWakeHelper() {
	if path := os.Getenv("SLBH_WAKE_ARGS_FILE"); path != "" {
		data, _ := json.Marshal(os.Args[1:])
		_ = os.WriteFile(path, data, 0o600)
	}
	if milliseconds, _ := strconv.Atoi(os.Getenv("SLBH_WAKE_SLEEP_MS")); milliseconds > 0 {
		time.Sleep(time.Duration(milliseconds) * time.Millisecond)
	}
	_, _ = os.Stdout.WriteString(os.Getenv("SLBH_WAKE_OUTPUT"))
	status, _ := strconv.Atoi(os.Getenv("SLBH_WAKE_STATUS"))
	os.Exit(status)
}

func newOrgRuntime(t *testing.T, cfg config.Config) *Runtime {
	t.Helper()
	if cfg.Home == "" {
		cfg.Home = t.TempDir()
	}
	if cfg.SeatModel == "" {
		cfg.SeatModel = "test"
	}
	if cfg.Roster.Source.Kind != config.RosterManaged {
		cfg.Roster = testRoster()
	}
	r, err := New(cfg, Options{
		Provider:            func(string) (provider.Provider, error) { return fakeProvider{}, nil },
		CodexCommand:        os.Args[0],
		RequestPollInterval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

func TestSeatOrgToolSchemasAreExclusive(t *testing.T) {
	r := newOrgRuntime(t, config.Config{SecretaryWake: false})
	seat := r.seat()
	child, err := r.launchSubagentSpec(seat.ID, LaunchSpec{Title: "manager", Role: "manager", Brief: "inspect"})
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := r.launchSubagentSpec(child.ID, LaunchSpec{Title: "leaf", Role: "flex", Model: "test-leaf", Brief: "inspect more"})
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]bool{"report_to_secretary": true, "org_requests": true, "update_request": true}
	for _, tool := range r.toolDefinitions(seat.ID) {
		delete(want, tool.Name)
	}
	if len(want) != 0 {
		t.Fatalf("Seat is missing org tools: %v", want)
	}
	for _, agent := range []*Agent{child, leaf} {
		for _, tool := range r.toolDefinitions(agent.ID) {
			if tool.Name == "report_to_secretary" || tool.Name == "org_requests" || tool.Name == "update_request" {
				t.Fatalf("depth-%d agent was offered Seat tool %q", agent.Depth, tool.Name)
			}
		}
		for _, name := range []string{"report_to_secretary", "org_requests", "update_request"} {
			if _, err := r.ExecuteTool(agent.ID, name, `{}`); err == nil || !strings.Contains(err.Error(), "depth-0 Seat") {
				t.Fatalf("depth-%d %s refusal = %v", agent.Depth, name, err)
			}
		}
	}
}

func TestReportIsDurableBeforeAsynchronousSecretaryWake(t *testing.T) {
	argsFile := t.TempDir() + "/args.json"
	t.Setenv("SLBH_WAKE_HELPER", "1")
	t.Setenv("SLBH_WAKE_ARGS_FILE", argsFile)
	t.Setenv("SLBH_WAKE_SLEEP_MS", "500")
	t.Setenv("SLBH_WAKE_OUTPUT", "Queued message message-id for thread thread-id.\n")
	t.Setenv("SLBH_WAKE_STATUS", "0")
	r := newOrgRuntime(t, config.Config{SecretaryWake: true, SecretarySession: "named-secretary"})

	started := time.Now()
	if _, err := r.ExecuteTool(r.seat().ID, "report_to_secretary", `{"text":"changed","invalidates_none":true}`); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed >= 400*time.Millisecond {
		t.Fatalf("report tool waited for Secretary wake: %s", elapsed)
	}
	pending, err := r.orgStore.Pending()
	if err != nil || len(pending) != 1 || pending[0].From != "seat" || pending[0].Text != "changed" {
		t.Fatalf("pending reports = %#v, %v", pending, err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		data, readErr := os.ReadFile(argsFile)
		if readErr == nil {
			var args []string
			if err := json.Unmarshal(data, &args); err != nil {
				t.Fatal(err)
			}
			want := []string{"queue", "--thread", "named-secretary", "--message", "1 report is pending. Run slbh inbox to review it and acknowledge it with slbh ack <id>."}
			if strings.Join(args, "\x00") != strings.Join(want, "\x00") {
				t.Fatalf("wake argv = %#v, want %#v", args, want)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Secretary wake helper was not called")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestFailedAndDisabledSecretaryWake(t *testing.T) {
	t.Run("failed", func(t *testing.T) {
		t.Setenv("SLBH_WAKE_HELPER", "1")
		t.Setenv("SLBH_WAKE_OUTPUT", "queue failed")
		t.Setenv("SLBH_WAKE_STATUS", "1")
		r := newOrgRuntime(t, config.Config{SecretaryWake: true})
		if _, err := r.ExecuteTool(r.seat().ID, "report_to_secretary", `{"text":"durable","invalidates_none":true}`); err != nil {
			t.Fatal(err)
		}
		deadline := time.After(2 * time.Second)
		for {
			select {
			case event := <-testEvents(r):
				if event.Kind == "secretary_wake" {
					if !strings.Contains(event.Text, "codex queue failed") {
						t.Fatalf("wake failure event = %#v", event)
					}
					pending, err := r.orgStore.Pending()
					if err != nil || len(pending) != 1 {
						t.Fatalf("failed wake lost report: %#v, %v", pending, err)
					}
					return
				}
			case <-deadline:
				t.Fatal("failed wake did not emit a status")
			}
		}
	})

	t.Run("disabled", func(t *testing.T) {
		argsFile := t.TempDir() + "/args.json"
		t.Setenv("SLBH_WAKE_HELPER", "1")
		t.Setenv("SLBH_WAKE_ARGS_FILE", argsFile)
		r := newOrgRuntime(t, config.Config{SecretaryWake: false})
		if _, err := r.ExecuteTool(r.seat().ID, "report_to_secretary", `{"text":"quiet","invalidates_none":true}`); err != nil {
			t.Fatal(err)
		}
		time.Sleep(75 * time.Millisecond)
		if _, err := os.Stat(argsFile); !os.IsNotExist(err) {
			t.Fatalf("SecretaryWake=false called codex: %v", err)
		}
	})
}

func TestSeatRequestToolsUseStoreRules(t *testing.T) {
	r := newOrgRuntime(t, config.Config{SecretaryWake: false})
	request, err := r.orgStore.AppendRequest("secretary", "proposal")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.ExecuteTool(r.seat().ID, "update_request", `{"id":1,"status":"accepted","note":"checking"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ExecuteTool(r.seat().ID, "update_request", `{"id":1,"status":"done"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ExecuteTool(r.seat().ID, "update_request", `{"id":1,"status":"accepted"}`); err == nil || !strings.Contains(err.Error(), "terminal") {
		t.Fatalf("terminal update error = %v", err)
	}
	if request.ID != 1 {
		t.Fatalf("request id = %d", request.ID)
	}
}

type startupAppendStore struct {
	stateCalls int
	requests   []orgstore.Request
}

func (s *startupAppendStore) RequestsState() (orgstore.RequestLogState, error) {
	if s.stateCalls == 0 {
		s.requests = append(s.requests, orgstore.Request{
			ID: 1, From: "secretary", Text: "arrived during startup", Status: orgstore.StatusQueued,
		})
	}
	s.stateCalls++
	return orgstore.RequestLogState{Size: int64(len(s.requests)), ModTime: time.Unix(int64(len(s.requests)), 0)}, nil
}

func (s *startupAppendStore) Requests() ([]orgstore.Request, error) {
	return append([]orgstore.Request(nil), s.requests...), nil
}

func TestRequestWatcherBaselinesBeforeInitialScan(t *testing.T) {
	// The fake publishes its request during the first state sample. A watcher
	// that scans first sees an empty queue, then records the new log as its
	// baseline and never notices the request. Sampling the baseline first makes
	// the following scan deliver it, while still covering appends after the scan
	// on the next poll.
	store := &startupAppendStore{}
	stop := make(chan struct{})
	done := make(chan struct{})
	delivered := make(chan orgstore.Request, 1)
	errors := make(chan error, 1)
	go func() {
		defer close(done)
		watchRequestQueue(stop, time.Hour, store, func(request orgstore.Request) bool {
			delivered <- request
			return true
		}, func(err error) {
			errors <- err
		})
	}()
	t.Cleanup(func() {
		close(stop)
		<-done
	})

	select {
	case request := <-delivered:
		if request.ID != 1 || request.Text != "arrived during startup" {
			t.Fatalf("delivered request = %#v", request)
		}
	case err := <-errors:
		t.Fatalf("request watcher error: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("request appended during watcher startup was not delivered")
	}
}

func TestRequestWatcherDeliversStartupAndExternalAppendsOnceAndStops(t *testing.T) {
	home := t.TempDir()
	store, err := orgstore.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	startup, err := store.AppendRequest("secretary", "already queued")
	if err != nil {
		t.Fatal(err)
	}
	r := newOrgRuntime(t, config.Config{Home: home, SecretaryWake: false})
	external, err := store.AppendRequest("secretary", "new proposal")
	if err != nil {
		t.Fatal(err)
	}

	counts := map[uint64]int{}
	deadline := time.After(2 * time.Second)
	for len(counts) < 2 {
		select {
		case event := <-testEvents(r):
			if event.Kind == "org_request" {
				id := uint64(event.Metadata["request"].(float64))
				counts[id]++
				if !strings.Contains(event.Text, "proposal to be judged against the tree") || !strings.Contains(event.Text, strconv.FormatUint(id, 10)) {
					t.Fatalf("delivery text = %q", event.Text)
				}
			}
		case <-deadline:
			t.Fatalf("request deliveries = %v", counts)
		}
	}
	if counts[startup.ID] != 1 || counts[external.ID] != 1 {
		t.Fatalf("request delivery counts = %v", counts)
	}
	time.Sleep(50 * time.Millisecond)
	for {
		select {
		case event := <-testEvents(r):
			if event.Kind == "org_request" {
				t.Fatalf("duplicate request delivery: %#v", event)
			}
		default:
			goto drained
		}
	}

drained:
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-r.requestWatchDone:
	default:
		t.Fatal("request watcher did not stop on Close")
	}
}

func TestOrgEndToEndThroughCLI(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SLBH_HOME", home)
	t.Setenv("SLBH_SECRETARY_WAKE", "false")
	r := newOrgRuntime(t, config.Config{Home: home, SecretaryWake: false})
	if _, err := r.ExecuteTool(r.seat().ID, "report_to_secretary", `{"text":"e2e report","invalidates_none":true}`); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := orgcli.Run([]string{"inbox", "--json"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("inbox code %d: %s", code, stderr.String())
	}
	var reports []orgstore.Report
	if err := json.Unmarshal(stdout.Bytes(), &reports); err != nil || len(reports) != 1 {
		t.Fatalf("inbox = %s, %v", stdout.String(), err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := orgcli.Run([]string{"ack", "1"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("ack code %d: %s", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := orgcli.Run([]string{"request", "judge this"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("request code %d: %s", code, stderr.String())
	}

	deadline := time.After(2 * time.Second)
	for {
		select {
		case event := <-testEvents(r):
			if event.Kind == "org_request" && strings.Contains(event.Text, "judge this") {
				goto delivered
			}
		case <-deadline:
			t.Fatal("Seat did not receive CLI request")
		}
	}

delivered:
	if _, err := r.ExecuteTool(r.seat().ID, "update_request", `{"id":1,"status":"done"}`); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := orgcli.Run([]string{"requests", "--json"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("requests code %d: %s", code, stderr.String())
	}
	var requests []orgstore.Request
	if err := json.Unmarshal(stdout.Bytes(), &requests); err != nil || len(requests) != 1 || requests[0].Status != orgstore.StatusDone {
		t.Fatalf("requests = %s, %v", stdout.String(), err)
	}
}
