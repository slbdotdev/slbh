package logx

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Entry struct {
	Time     time.Time      `json:"time"`
	Runtime  string         `json:"runtime"`
	Agent    string         `json:"agent,omitempty"`
	Session  string         `json:"session,omitempty"`
	Kind     string         `json:"kind"`
	Text     string         `json:"text,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// JSONL is a process-local, append-only log for one agent session. Messages,
// tool calls, jobs, and lifecycle events for that session share the logger so
// the complete session can be replayed after a crash.
type JSONL struct {
	mu      sync.Mutex
	file    *os.File
	runtime string
	session string
}

func Open(path, runtime string) (*JSONL, error) {
	return OpenSession(path, runtime, "")
}

func OpenSession(path, runtime, session string) (*JSONL, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	return &JSONL{file: f, runtime: runtime, session: session}, nil
}

func (l *JSONL) Append(entry Entry) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return fmt.Errorf("log is closed")
	}
	if entry.Time.IsZero() {
		entry.Time = time.Now().UTC()
	}
	if entry.Runtime == "" {
		entry.Runtime = l.runtime
	}
	if entry.Session == "" {
		entry.Session = l.session
	}
	b, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	// Read caps one line at maxRecordBytes, and a producer is not bound by
	// that: a job retains 4 MiB of stdout and 4 MiB of stderr and both go into
	// a single job_result, which JSON escaping can grow further. Trimming here
	// rather than writing an oversized line keeps the invariant that anything
	// Append writes, Read can read — a record that is durable but unreadable is
	// the one outcome this format exists to prevent.
	if len(b) > maxRecordBytes {
		entry.Text = truncateText(entry.Text, len(b)-maxRecordBytes)
		if b, err = json.Marshal(entry); err != nil {
			return err
		}
	}
	if _, err = l.file.Write(append(b, '\n')); err != nil {
		return err
	}
	// Stream deltas arrive per token; syncing each one costs a disk flush per
	// token (53 ms on a spinning disk, measured on fox 2026-09-13) and was
	// 79% of a bench slot's wall time. Every other kind still syncs, and an
	// fsync flushes the whole file, so the deltas reach disk with the next
	// tool, usage or turn event.
	if entry.Kind == "thinking" || entry.Kind == "assistant" {
		return nil
	}
	return l.file.Sync()
}

func (l *JSONL) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	return err
}

func Read(path string) ([]Entry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 64*1024), maxRecordBytes)
	var out []Entry
	for s.Scan() {
		var e Entry
		if err := json.Unmarshal(s.Bytes(), &e); err != nil {
			// A crash or a power loss between write and flush leaves a partial
			// final line. Discarding every valid record before it would lose a
			// whole history to the one failure this format exists to survive,
			// so a torn tail costs its own record and nothing else. A malformed
			// line with anything after it is real corruption and still fails.
			if !s.Scan() {
				if scanErr := s.Err(); scanErr != nil {
					return nil, scanErr
				}
				return out, nil
			}
			return nil, fmt.Errorf("decode transcript: %w", err)
		}
		out = append(out, e)
	}
	return out, s.Err()
}

// maxRecordBytes bounds one JSONL line. Append's trim and Read's scanner use
// the same figure, so neither can produce a record the other refuses.
const maxRecordBytes = 16 * 1024 * 1024

// truncateText removes at least overflow bytes from the middle of text and says
// so in place of what it removed, keeping the head and the tail — the two parts
// that identify what the record was.
func truncateText(text string, overflow int) string {
	cut := overflow + 256
	if cut >= len(text) {
		cut = len(text)
	}
	keep := len(text) - cut
	head := keep / 2
	return text[:head] +
		fmt.Sprintf("\n... [%d bytes elided to fit one transcript record] ...\n", cut) +
		text[len(text)-(keep-head):]
}
