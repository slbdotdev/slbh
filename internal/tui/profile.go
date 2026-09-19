package tui

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// frameProfiler is deliberately opt-in. It records the time spent in the two
// Bubble Tea callbacks that determine input and paint latency, without adding
// a dependency on a particular terminal or profiling backend.
//
// The output is CSV so a long interactive run can be analyzed after the
// process exits, even if the process is terminated by the terminal or shell.
type frameProfiler struct {
	file     *os.File
	lastView time.Time
}

func newFrameProfiler() *frameProfiler {
	path := strings.TrimSpace(os.Getenv("SLBH_FRAME_PROFILE"))
	if path == "" {
		return nil
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil
	}
	if _, err := fmt.Fprintln(file, "time_unix_ns,kind,message,duration_ns,frame_gap_ns,event_count"); err != nil {
		_ = file.Close()
		return nil
	}
	return &frameProfiler{file: file}
}

func (p *frameProfiler) record(kind, message string, started, finished time.Time, eventCount int) {
	if p == nil || p.file == nil {
		return
	}
	gap := time.Duration(0)
	if kind == "view" && !p.lastView.IsZero() {
		gap = finished.Sub(p.lastView)
	}
	if kind == "view" {
		p.lastView = finished
	}
	_, _ = fmt.Fprintf(p.file, "%d,%s,%s,%d,%d,%d\n", finished.UnixNano(), kind, message, finished.Sub(started).Nanoseconds(), gap.Nanoseconds(), eventCount)
}
