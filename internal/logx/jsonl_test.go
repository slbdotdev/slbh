package logx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAppendAndReadRoundTripARecordLargerThanTheOldScannerCap(t *testing.T) {
	// A job retains 4 MiB of stdout and 4 MiB of stderr and both go into one
	// job_result record, while Read capped a line at 2 MiB. The record was
	// written and fsynced, and the next read of the whole transcript then
	// failed with bufio.ErrTooLong — durable but unreadable, which is the one
	// outcome this format exists to prevent.
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	log, err := OpenSession(path, "run-test", "session-test")
	if err != nil {
		t.Fatal(err)
	}
	big := strings.Repeat("x", 6*1024*1024)
	if err := log.Append(Entry{Kind: "job_result", Text: big}); err != nil {
		t.Fatal(err)
	}
	if err := log.Append(Entry{Kind: "assistant", Text: "after the big one"}); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	entries, err := Read(path)
	if err != nil {
		t.Fatalf("a record this logger wrote could not be read back: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("read %d entries, want 2", len(entries))
	}
	if entries[1].Text != "after the big one" {
		t.Fatalf("the record after the big one was lost: %#v", entries[1])
	}
}

func TestAppendTrimsARecordTooLargeForTheReader(t *testing.T) {
	// The producer is not bound by the reader's cap, so the trim happens at
	// write time and says what it removed. The invariant is that anything
	// Append writes, Read can read.
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	log, err := OpenSession(path, "run-test", "session-test")
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Append(Entry{Kind: "job_result", Text: strings.Repeat("y", maxRecordBytes+(1<<20))}); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	entries, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("read %d entries, want 1", len(entries))
	}
	if !strings.Contains(entries[0].Text, "elided to fit one transcript record") {
		t.Fatal("an over-large record was trimmed without saying so")
	}
	if !strings.HasPrefix(entries[0].Text, "yyy") {
		t.Fatal("the trim did not keep the head of the record")
	}
}

func TestReadKeepsEveryValidRecordBeforeATornTail(t *testing.T) {
	// A crash or power loss between write and flush leaves a partial final
	// line. Read returned nil for the whole file, so one interrupted write lost
	// the entire history — the exact failure a durable append-only log exists
	// to survive.
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	log, err := OpenSession(path, "run-test", "session-test")
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Append(Entry{Kind: "assistant", Text: "first"}); err != nil {
		t.Fatal(err)
	}
	if err := log.Append(Entry{Kind: "assistant", Text: "second"}); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"kind":"assistant","text":"tor`); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	entries, err := Read(path)
	if err != nil {
		t.Fatalf("a torn tail made the whole transcript unreadable: %v", err)
	}
	if len(entries) != 2 || entries[0].Text != "first" || entries[1].Text != "second" {
		t.Fatalf("the records written before the tear were lost: %#v", entries)
	}
}

func TestReadStillRejectsCorruptionThatIsNotTheFinalRecord(t *testing.T) {
	// Tolerating a torn tail must not tolerate a damaged middle: a bad line
	// with valid records after it is real corruption, not an interrupted write.
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	body := `{"kind":"assistant","text":"first"}` + "\n" +
		`{"kind":"assistant","text":"tor` + "\n" +
		`{"kind":"assistant","text":"third"}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(path); err == nil {
		t.Fatal("a malformed record in the middle of the file was accepted")
	}
}

func TestAppendBoundsARecordWhoseBulkIsInMetadata(t *testing.T) {
	// Trimming Text does nothing when the payload sits in Metadata and Text is
	// empty, which is exactly the shape of an inference_request. Without a
	// second pass the record went over the cap and made the whole file
	// unreadable — the same durable-but-unopenable outcome the trim exists to
	// prevent, reached through the neighbouring field.
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	log, err := OpenSession(path, "run-test", "session-test")
	if err != nil {
		t.Fatal(err)
	}
	huge := make([]byte, maxRecordBytes+(2<<20))
	for i := range huge {
		huge[i] = 'z'
	}
	entry := Entry{Kind: "inference_request", Metadata: map[string]any{
		"round":   1,
		"payload": string(huge),
	}}
	if err := log.Append(entry); err != nil {
		t.Fatal(err)
	}
	if err := log.Append(Entry{Kind: "assistant", Text: "after the big one"}); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	entries, err := Read(path)
	if err != nil {
		t.Fatalf("an oversized metadata record made the transcript unreadable: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("read %d entries, want 2", len(entries))
	}
	if entries[0].Metadata["round"] != float64(1) {
		t.Fatalf("the small scalar beside the payload was dropped: %#v", entries[0].Metadata)
	}
	payload, _ := entries[0].Metadata["payload"].(string)
	if !strings.Contains(payload, "elided to fit one transcript record") {
		t.Fatalf("the oversized value was not elided: %.80s", payload)
	}
	if entries[1].Text != "after the big one" {
		t.Fatalf("the record after it was lost: %#v", entries[1])
	}
}
