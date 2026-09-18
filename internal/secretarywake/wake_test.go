package secretarywake

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if os.Getenv("SLBH_WAKE_HELPER") == "1" {
		runWakeHelper()
		return
	}
	os.Exit(m.Run())
}

func runWakeHelper() {
	if path := os.Getenv("SLBH_WAKE_ARGS_FILE"); path != "" {
		data, _ := json.Marshal(os.Args[1:])
		_ = os.WriteFile(path, data, 0o600)
	}
	if path := os.Getenv("SLBH_WAKE_ENV_FILE"); path != "" {
		_, present := os.LookupEnv("CODEX_API_KEY")
		_ = os.WriteFile(path, []byte(strconv.FormatBool(present)), 0o600)
	}
	if path := os.Getenv("SLBH_WAKE_PID_FILE"); path != "" {
		_ = os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0o600)
	}
	if os.Getenv("SLBH_WAKE_HANG") == "1" {
		time.Sleep(10 * time.Minute)
	}
	_, _ = os.Stdout.WriteString(os.Getenv("SLBH_WAKE_OUTPUT"))
	status, _ := strconv.Atoi(os.Getenv("SLBH_WAKE_STATUS"))
	os.Exit(status)
}

func TestWakeBuildsExactQueueCommandParsesSuccessAndScrubsAPIKey(t *testing.T) {
	fixture := readFixture(t, "testdata/success.txt")
	argsFile := t.TempDir() + "/args.json"
	envFile := t.TempDir() + "/env.txt"
	t.Setenv("SLBH_WAKE_HELPER", "1")
	t.Setenv("SLBH_WAKE_OUTPUT", fixture)
	t.Setenv("SLBH_WAKE_STATUS", "0")
	t.Setenv("SLBH_WAKE_ARGS_FILE", argsFile)
	t.Setenv("SLBH_WAKE_ENV_FILE", envFile)
	t.Setenv("CODEX_API_KEY", "must-not-reach-child")

	result, err := Wake(context.Background(), Options{
		CodexCommand: os.Args[0],
		SessionName:  "secretary-exact",
	}, "pointer message")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Success || result.MessageID != "01a0b235-1595-7340-926c-6703e0fdada8" || result.ThreadID != "01a0b234-5330-7763-ab8d-f8a3d229d83a" {
		t.Fatalf("result = %#v", result)
	}

	data, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	var args []string
	if err := json.Unmarshal(data, &args); err != nil {
		t.Fatal(err)
	}
	want := []string{"queue", "--thread", "secretary-exact", "--message", "pointer message"}
	if strings.Join(args, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("argv = %#v, want %#v", args, want)
	}
	present, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(present) != "false" {
		t.Fatalf("CODEX_API_KEY reached child: %s", present)
	}
}

func TestWakeReportsUnresolvedNameFromCapturedCLIOutput(t *testing.T) {
	configureFailedHelper(t, "testdata/unresolved-name.txt")
	_, err := Wake(context.Background(), Options{CodexCommand: os.Args[0], SessionName: "slbh-wake-unresolved-20260918"}, "probe")
	if err == nil || !strings.Contains(err.Error(), `session "slbh-wake-unresolved-20260918" did not resolve`) {
		t.Fatalf("error = %v", err)
	}
}

func TestWakeReportsDaemonDownFromCapturedCLIOutput(t *testing.T) {
	configureFailedHelper(t, "testdata/daemon-down.txt")
	_, err := Wake(context.Background(), Options{CodexCommand: os.Args[0]}, "probe")
	if err == nil || !strings.Contains(err.Error(), "app-server daemon is unavailable") {
		t.Fatalf("error = %v", err)
	}
}

func TestWakeTimeoutKillsProcess(t *testing.T) {
	pidFile := t.TempDir() + "/pid"
	t.Setenv("SLBH_WAKE_HELPER", "1")
	t.Setenv("SLBH_WAKE_HANG", "1")
	t.Setenv("SLBH_WAKE_PID_FILE", pidFile)

	_, err := Wake(context.Background(), Options{CodexCommand: os.Args[0], Timeout: 100 * time.Millisecond}, "probe")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want deadline exceeded", err)
	}
	data, readErr := os.ReadFile(pidFile)
	if readErr != nil {
		t.Fatal(readErr)
	}
	pid, parseErr := strconv.Atoi(string(data))
	if parseErr != nil {
		t.Fatal(parseErr)
	}
	process, findErr := os.FindProcess(pid)
	if findErr == nil && process.Signal(syscall.Signal(0)) == nil {
		t.Fatalf("timed-out helper process %d is still running", pid)
	}
}

func TestInboxMessageIsOnlyAPendingReportPointer(t *testing.T) {
	message := InboxMessage(3)
	for _, wanted := range []string{"3 reports", "slbh inbox", "slbh ack <id>"} {
		if !strings.Contains(message, wanted) {
			t.Fatalf("InboxMessage(3) = %q, missing %q", message, wanted)
		}
	}
}

func configureFailedHelper(t *testing.T, fixture string) {
	t.Helper()
	t.Setenv("SLBH_WAKE_HELPER", "1")
	t.Setenv("SLBH_WAKE_OUTPUT", readFixture(t, fixture))
	t.Setenv("SLBH_WAKE_STATUS", "1")
}

func readFixture(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
