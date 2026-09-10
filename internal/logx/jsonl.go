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
	if _, err = l.file.Write(append(b, '\n')); err != nil {
		return err
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
	s.Buffer(make([]byte, 64*1024), 2*1024*1024)
	var out []Entry
	for s.Scan() {
		var e Entry
		if err := json.Unmarshal(s.Bytes(), &e); err != nil {
			return nil, fmt.Errorf("decode transcript: %w", err)
		}
		out = append(out, e)
	}
	return out, s.Err()
}
