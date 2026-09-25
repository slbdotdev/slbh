// Package seam defines the in-process boundary between the runtime and its
// front ends. It deliberately contains no transport or harness implementation.
package seam

import (
	"time"

	"github.com/slbdotdev/slbh/internal/config"
	"github.com/slbdotdev/slbh/internal/provider"
)

// Event is one ordered record emitted by a runtime.
type Event struct {
	Cursor     EventCursor    `json:"cursor"`
	Time       time.Time      `json:"time"`
	RuntimeID  string         `json:"runtime"`
	AgentID    string         `json:"agent"`
	AgentTitle string         `json:"agent_title"`
	Kind       string         `json:"kind"`
	Text       string         `json:"text,omitempty"`
	Metadata   map[string]any `json:"metadata,omitempty"`
}

// EventCursor is a consumer's position in the runtime's ordered event stream.
// Zero names the position before the first event.
type EventCursor uint64

// EventQuery asks for events emitted after After. Limit zero means no limit.
// WaitMilliseconds enables a bounded long poll when no event is immediately
// available; it is ignored after the runtime reaches end of stream.
type EventQuery struct {
	After            EventCursor `json:"after"`
	Limit            int         `json:"limit,omitempty"`
	WaitMilliseconds int         `json:"wait_ms,omitempty"`
}

// EventBatch is a copied, serializable view of one position in the event
// stream. Cursor is the last returned event cursor (or After when Events is
// empty). End is true only when no event remains after Cursor.
type EventBatch struct {
	Events []Event     `json:"events"`
	Cursor EventCursor `json:"cursor"`
	End    bool        `json:"end"`
}

// AgentSnapshot is a copy of an agent's externally observable state.
type AgentSnapshot struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	ParentID string `json:"parent_id"`
	Depth    int    `json:"depth"`
	Model    string `json:"model"`
	Effort   string `json:"effort"`
	Status   string `json:"status"`
	// StatusSince is when Status last changed.
	StatusSince     time.Time `json:"status_since"`
	Harness         string    `json:"harness"`
	WorkDir         string    `json:"work_dir"`
	ContextWindow   int       `json:"context_window"`
	ContextUsed     int       `json:"context_used"`
	CacheHitTokens  int       `json:"cache_hit_tokens"`
	CacheMissTokens int       `json:"cache_miss_tokens"`
}

// JobSnapshot is a copy of a background job's externally observable state.
type JobSnapshot struct {
	ID          string        `json:"id"`
	Author      string        `json:"author"`
	Script      string        `json:"script"`
	ToolName    string        `json:"tool_name"`
	Status      string        `json:"status"`
	Started     time.Time     `json:"started"`
	Finished    time.Time     `json:"finished"`
	ExitCode    int           `json:"exit_code"`
	StdoutBytes int           `json:"stdout_bytes"`
	StderrBytes int           `json:"stderr_bytes"`
	WarnAfter   time.Duration `json:"warn_after"`
}

// Reply is the serializable result of a command. Most commands need only the
// command name; fields are populated for commands with a value result.
type Reply struct {
	Command      string              `json:"command"`
	Dropped      int                 `json:"dropped,omitempty"`
	PolicySource config.PolicySource `json:"policy_source,omitempty"`
}

// Runtime is the in-process boundary consumed by front ends.
type Runtime interface {
	Do(Command) (Reply, error)
	// PollEvents returns copied events emitted after the supplied cursor. Each
	// front end advances its own cursor, so consumers are independent.
	PollEvents(EventQuery) EventBatch
	Agents() []AgentSnapshot
	JobSnapshots() []JobSnapshot
	Config() config.Config
	ID() string
	Home() string
	Dir() string
	TranscriptPath(agentID string) (string, error)
	InstructionSource() config.InstructionSource
	SkillSource() config.SkillSource
	PolicySource() config.PolicySource
	ModelCatalog() []provider.Catalog
	ModelGuidance() string
}
