// Package orgstore provides the durable file-backed channels between a Seat
// and its Secretary.
package orgstore

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	inboxLogName      = "inbox.jsonl"
	inboxCursorName   = "inbox.cursor"
	inboxLockName     = ".inbox.lock"
	requestsLogName   = "requests.jsonl"
	requestsLockName  = ".requests.lock"
	requestRecordType = "request"
	statusRecordType  = "status"
)

// Store is a durable org inbox and request queue rooted at home/org.
// Store contains paths only and is safe to use from multiple goroutines and
// processes.
type Store struct {
	dir          string
	inboxLog     string
	inboxCursor  string
	inboxLock    string
	requestsLog  string
	requestsLock string
}

// Report is one Seat report in the org inbox.
type Report struct {
	ID              uint64    `json:"id"`
	Time            time.Time `json:"time"`
	From            string    `json:"from"`
	Text            string    `json:"text"`
	Invalidates     []string  `json:"invalidates,omitempty"`
	InvalidatesNone bool      `json:"invalidates_none,omitempty"`
}

// Status is the current state of a request.
type Status string

const (
	StatusQueued   Status = "queued"
	StatusAccepted Status = "accepted"
	StatusDeclined Status = "declined"
	StatusDone     Status = "done"
)

// StatusChange is one state in a request's history. The initial queued entry
// is synthesized from the request creation record.
type StatusChange struct {
	Status Status    `json:"status"`
	Time   time.Time `json:"time"`
	Note   string    `json:"note,omitempty"`
}

// Request is one Secretary request with its folded current state and history.
type Request struct {
	ID      uint64         `json:"id"`
	Time    time.Time      `json:"time"`
	From    string         `json:"from"`
	Text    string         `json:"text"`
	Status  Status         `json:"status"`
	History []StatusChange `json:"history"`
}

type requestRecord struct {
	Type string    `json:"type"`
	ID   uint64    `json:"id"`
	Time time.Time `json:"time"`
	From string    `json:"from"`
	Text string    `json:"text"`
}

type statusRecord struct {
	Type   string    `json:"type"`
	ID     uint64    `json:"id"`
	Status Status    `json:"status"`
	Time   time.Time `json:"time"`
	Note   string    `json:"note,omitempty"`
}

type recordType struct {
	Type string `json:"type"`
}

type cursorRecord struct {
	AcknowledgedID uint64 `json:"acknowledged_id"`
}

// Open opens a store below home. It creates home/org when necessary.
func Open(home string) (*Store, error) {
	if strings.TrimSpace(home) == "" {
		return nil, fmt.Errorf("orgstore home is empty")
	}
	dir := filepath.Join(home, "org")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create org directory: %w", err)
	}
	return &Store{
		dir:          dir,
		inboxLog:     filepath.Join(dir, inboxLogName),
		inboxCursor:  filepath.Join(dir, inboxCursorName),
		inboxLock:    filepath.Join(dir, inboxLockName),
		requestsLog:  filepath.Join(dir, requestsLogName),
		requestsLock: filepath.Join(dir, requestsLockName),
	}, nil
}

// AppendReport appends a report and returns the stored value. Exactly one of
// invalidates (a non-empty list of non-empty paths) and invalidatesNone must be
// supplied.
func (s *Store) AppendReport(from, text string, invalidates []string, invalidatesNone bool) (report Report, err error) {
	if err := validateInvalidation(invalidates, invalidatesNone); err != nil {
		return Report{}, err
	}
	unlock, err := acquireLock(s.inboxLock)
	if err != nil {
		return Report{}, err
	}
	defer func() { err = errors.Join(err, unlock()) }()

	file, err := openAppendLog(s.inboxLog)
	if err != nil {
		return Report{}, err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	if err := repairTrailingLine(file); err != nil {
		return Report{}, fmt.Errorf("repair org inbox: %w", err)
	}
	reports, err := readReportsFile(file)
	if err != nil {
		return Report{}, err
	}
	var lastID uint64
	if len(reports) != 0 {
		lastID = reports[len(reports)-1].ID
	}
	if lastID == ^uint64(0) {
		return Report{}, fmt.Errorf("org inbox id space exhausted")
	}
	report = Report{
		ID:              lastID + 1,
		Time:            time.Now().UTC(),
		From:            from,
		Text:            text,
		Invalidates:     append([]string(nil), invalidates...),
		InvalidatesNone: invalidatesNone,
	}
	if err := appendJSONLine(file, report); err != nil {
		return Report{}, fmt.Errorf("append org inbox: %w", err)
	}
	return report, nil
}

// Pending returns reports after the durable acknowledgement cursor.
func (s *Store) Pending() ([]Report, error) {
	reports, err := readReports(s.inboxLog)
	if err != nil {
		return nil, err
	}
	cursor, err := readCursor(s.inboxCursor)
	if err != nil {
		return nil, err
	}
	if cursor != 0 && !hasReport(reports, cursor) {
		return nil, fmt.Errorf("org inbox cursor references unknown report %d", cursor)
	}
	pending := make([]Report, 0, len(reports))
	for _, report := range reports {
		if report.ID > cursor {
			report.Invalidates = append([]string(nil), report.Invalidates...)
			pending = append(pending, report)
		}
	}
	return pending, nil
}

// Ack advances the durable inbox cursor to id. It rejects an unknown report
// and a move behind the current cursor. A repeated acknowledgement is a no-op.
func (s *Store) Ack(id uint64) (err error) {
	unlock, err := acquireLock(s.inboxLock)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, unlock()) }()

	reports, err := readReports(s.inboxLog)
	if err != nil {
		return err
	}
	if !hasReport(reports, id) {
		return fmt.Errorf("cannot acknowledge unknown report %d", id)
	}
	cursor, err := readCursor(s.inboxCursor)
	if err != nil {
		return err
	}
	if id < cursor {
		return fmt.Errorf("cannot move org inbox cursor backwards from %d to %d", cursor, id)
	}
	if id == cursor {
		return nil
	}
	return replaceCursor(s.dir, s.inboxCursor, id)
}

// AppendRequest appends a queued request and returns the folded value.
func (s *Store) AppendRequest(from, text string) (request Request, err error) {
	unlock, err := acquireLock(s.requestsLock)
	if err != nil {
		return Request{}, err
	}
	defer func() { err = errors.Join(err, unlock()) }()

	file, err := openAppendLog(s.requestsLog)
	if err != nil {
		return Request{}, err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	if err := repairTrailingLine(file); err != nil {
		return Request{}, fmt.Errorf("repair request queue: %w", err)
	}
	requests, err := readRequestsFile(file)
	if err != nil {
		return Request{}, err
	}
	var lastID uint64
	for _, existing := range requests {
		if existing.ID > lastID {
			lastID = existing.ID
		}
	}
	if lastID == ^uint64(0) {
		return Request{}, fmt.Errorf("request id space exhausted")
	}
	now := time.Now().UTC()
	record := requestRecord{Type: requestRecordType, ID: lastID + 1, Time: now, From: from, Text: text}
	if err := appendJSONLine(file, record); err != nil {
		return Request{}, fmt.Errorf("append request queue: %w", err)
	}
	return Request{
		ID: record.ID, Time: now, From: from, Text: text, Status: StatusQueued,
		History: []StatusChange{{Status: StatusQueued, Time: now}},
	}, nil
}

// UpdateRequestStatus appends a request status change. queued is implicit and
// cannot be appended. Terminal requests cannot be changed.
func (s *Store) UpdateRequestStatus(id uint64, status Status, note string) (change StatusChange, err error) {
	if status != StatusAccepted && status != StatusDeclined && status != StatusDone {
		return StatusChange{}, fmt.Errorf("invalid request status %q", status)
	}
	unlock, err := acquireLock(s.requestsLock)
	if err != nil {
		return StatusChange{}, err
	}
	defer func() { err = errors.Join(err, unlock()) }()

	file, err := openAppendLog(s.requestsLog)
	if err != nil {
		return StatusChange{}, err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	if err := repairTrailingLine(file); err != nil {
		return StatusChange{}, fmt.Errorf("repair request queue: %w", err)
	}
	requests, err := readRequestsFile(file)
	if err != nil {
		return StatusChange{}, err
	}
	var request *Request
	for i := range requests {
		if requests[i].ID == id {
			request = &requests[i]
			break
		}
	}
	if request == nil {
		return StatusChange{}, fmt.Errorf("cannot update unknown request %d", id)
	}
	if request.Status == StatusDeclined || request.Status == StatusDone {
		return StatusChange{}, fmt.Errorf("cannot update terminal request %d in status %q", id, request.Status)
	}
	change = StatusChange{Status: status, Time: time.Now().UTC(), Note: note}
	record := statusRecord{Type: statusRecordType, ID: id, Status: status, Time: change.Time, Note: note}
	if err := appendJSONLine(file, record); err != nil {
		return StatusChange{}, fmt.Errorf("append request status: %w", err)
	}
	return change, nil
}

// Requests returns all requests in creation order with status records folded
// into their current status and history.
func (s *Store) Requests() ([]Request, error) {
	requests, err := readRequests(s.requestsLog)
	if err != nil {
		return nil, err
	}
	return requests, nil
}

func validateInvalidation(invalidates []string, invalidatesNone bool) error {
	if (len(invalidates) > 0) == invalidatesNone {
		return fmt.Errorf("report must provide exactly one of invalidates or invalidates_none")
	}
	for _, path := range invalidates {
		if strings.TrimSpace(path) == "" {
			return fmt.Errorf("report invalidates contains an empty path")
		}
	}
	return nil
}

func openAppendLog(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", filepath.Base(path), err)
	}
	return file, nil
}

func appendJSONLine(file *os.File, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	written, err := file.Write(data)
	if err != nil {
		return err
	}
	if written != len(data) {
		return io.ErrShortWrite
	}
	return file.Sync()
}

func repairTrailingLine(file *os.File) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	size := info.Size()
	if size == 0 {
		return nil
	}
	last := []byte{0}
	if _, err := file.ReadAt(last, size-1); err != nil {
		return err
	}
	if last[0] == '\n' {
		return nil
	}
	const blockSize = 32 * 1024
	buf := make([]byte, blockSize)
	for end := size; end > 0; {
		start := end - blockSize
		if start < 0 {
			start = 0
		}
		chunk := buf[:end-start]
		if _, err := file.ReadAt(chunk, start); err != nil {
			return err
		}
		if index := bytes.LastIndexByte(chunk, '\n'); index >= 0 {
			return file.Truncate(start + int64(index) + 1)
		}
		end = start
	}
	return file.Truncate(0)
}

func readReports(path string) ([]Report, error) {
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open org inbox: %w", err)
	}
	defer file.Close()
	return readReportsFile(file)
}

func readReportsFile(file *os.File) ([]Report, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	var reports []Report
	err := readCompleteLines(file, func(line []byte, lineNumber int) error {
		var report Report
		if err := json.Unmarshal(line, &report); err != nil {
			return fmt.Errorf("decode org inbox line %d: %w", lineNumber, err)
		}
		if report.ID == 0 || report.Time.IsZero() {
			return fmt.Errorf("invalid org inbox line %d", lineNumber)
		}
		if err := validateInvalidation(report.Invalidates, report.InvalidatesNone); err != nil {
			return fmt.Errorf("invalid org inbox line %d: %w", lineNumber, err)
		}
		if len(reports) != 0 && report.ID <= reports[len(reports)-1].ID {
			return fmt.Errorf("org inbox ids are not strictly increasing at line %d", lineNumber)
		}
		reports = append(reports, report)
		return nil
	})
	return reports, err
}

func hasReport(reports []Report, id uint64) bool {
	for _, report := range reports {
		if report.ID == id {
			return true
		}
	}
	return false
}

func readCursor(path string) (uint64, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read org inbox cursor: %w", err)
	}
	var cursor cursorRecord
	if err := json.Unmarshal(data, &cursor); err != nil {
		return 0, fmt.Errorf("decode org inbox cursor: %w", err)
	}
	return cursor.AcknowledgedID, nil
}

func replaceCursor(dir, path string, id uint64) (err error) {
	data, err := json.Marshal(cursorRecord{AcknowledgedID: id})
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(dir, ".inbox.cursor.tmp-")
	if err != nil {
		return fmt.Errorf("create org inbox cursor temporary file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		if tmp != nil {
			err = errors.Join(err, tmp.Close())
		}
		if tmpPath != "" {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("write org inbox cursor: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync org inbox cursor: %w", err)
	}
	if err := tmp.Close(); err != nil {
		tmp = nil
		return fmt.Errorf("close org inbox cursor: %w", err)
	}
	tmp = nil
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace org inbox cursor: %w", err)
	}
	tmpPath = ""
	directory, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open org directory for sync: %w", err)
	}
	defer func() { err = errors.Join(err, directory.Close()) }()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("sync org directory: %w", err)
	}
	return nil
}

func readRequests(path string) ([]Request, error) {
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open request queue: %w", err)
	}
	defer file.Close()
	return readRequestsFile(file)
}

func readRequestsFile(file *os.File) ([]Request, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	var requests []Request
	byID := make(map[uint64]int)
	err := readCompleteLines(file, func(line []byte, lineNumber int) error {
		var kind recordType
		if err := json.Unmarshal(line, &kind); err != nil {
			return fmt.Errorf("decode request queue line %d: %w", lineNumber, err)
		}
		switch kind.Type {
		case requestRecordType:
			var record requestRecord
			if err := json.Unmarshal(line, &record); err != nil {
				return fmt.Errorf("decode request line %d: %w", lineNumber, err)
			}
			if record.ID == 0 || record.Time.IsZero() {
				return fmt.Errorf("invalid request line %d", lineNumber)
			}
			if _, exists := byID[record.ID]; exists {
				return fmt.Errorf("duplicate request id %d at line %d", record.ID, lineNumber)
			}
			if len(requests) != 0 && record.ID <= requests[len(requests)-1].ID {
				return fmt.Errorf("request ids are not strictly increasing at line %d", lineNumber)
			}
			byID[record.ID] = len(requests)
			requests = append(requests, Request{
				ID: record.ID, Time: record.Time, From: record.From, Text: record.Text, Status: StatusQueued,
				History: []StatusChange{{Status: StatusQueued, Time: record.Time}},
			})
		case statusRecordType:
			var record statusRecord
			if err := json.Unmarshal(line, &record); err != nil {
				return fmt.Errorf("decode request status line %d: %w", lineNumber, err)
			}
			if record.Time.IsZero() || (record.Status != StatusAccepted && record.Status != StatusDeclined && record.Status != StatusDone) {
				return fmt.Errorf("invalid request status line %d", lineNumber)
			}
			index, exists := byID[record.ID]
			if !exists {
				return fmt.Errorf("request status line %d references unknown request %d", lineNumber, record.ID)
			}
			request := &requests[index]
			if request.Status == StatusDeclined || request.Status == StatusDone {
				return fmt.Errorf("request status line %d changes terminal request %d", lineNumber, record.ID)
			}
			change := StatusChange{Status: record.Status, Time: record.Time, Note: record.Note}
			request.Status = record.Status
			request.History = append(request.History, change)
		default:
			return fmt.Errorf("unknown request queue record type %q at line %d", kind.Type, lineNumber)
		}
		return nil
	})
	return requests, err
}

func readCompleteLines(reader io.Reader, consume func([]byte, int) error) error {
	buffered := bufio.NewReader(reader)
	for lineNumber := 1; ; lineNumber++ {
		line, err := buffered.ReadBytes('\n')
		switch {
		case err == nil:
			line = bytes.TrimSuffix(line, []byte{'\n'})
			if len(line) != 0 && line[len(line)-1] == '\r' {
				line = line[:len(line)-1]
			}
			if len(line) == 0 {
				return fmt.Errorf("empty JSONL record at line %d", lineNumber)
			}
			if err := consume(line, lineNumber); err != nil {
				return err
			}
		case errors.Is(err, io.EOF):
			// A final token without a newline is the only shape an interrupted
			// append can leave. It is deliberately ignored, even if it happens
			// to contain valid JSON.
			return nil
		default:
			return err
		}
	}
}
