// Package seam defines the in-process boundary between the runtime and its
// front ends. It deliberately contains no transport or harness implementation.
package seam

import (
	"context"
	"time"

	"github.com/slbdotdev/slbh/internal/config"
	"github.com/slbdotdev/slbh/internal/provider"
)

// Event is one ordered record emitted by a runtime.
type Event struct {
	Time       time.Time      `json:"time"`
	RuntimeID  string         `json:"runtime"`
	AgentID    string         `json:"agent"`
	AgentTitle string         `json:"agent_title"`
	Kind       string         `json:"kind"`
	Text       string         `json:"text,omitempty"`
	Metadata   map[string]any `json:"metadata,omitempty"`
}

// AgentSnapshot is a copy of an agent's externally observable state.
type AgentSnapshot struct {
	ID              string `json:"id"`
	Title           string `json:"title"`
	ParentID        string `json:"parent_id"`
	Depth           int    `json:"depth"`
	Model           string `json:"model"`
	Effort          string `json:"effort"`
	Status          string `json:"status"`
	Harness         string `json:"harness"`
	WorkDir         string `json:"work_dir"`
	ContextWindow   int    `json:"context_window"`
	ContextUsed     int    `json:"context_used"`
	CacheHitTokens  int    `json:"cache_hit_tokens"`
	CacheMissTokens int    `json:"cache_miss_tokens"`
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
	Do(context.Context, Command) (Reply, error)
	// Events returns the ordered event stream. The channel is closed when the
	// runtime stops; closure is the definitive end-of-stream signal.
	Events() <-chan Event
	// Subscribe returns an independent ordered event stream and an idempotent
	// unsubscribe function. The stream contains events emitted after Subscribe.
	Subscribe() (<-chan Event, func())
	Agents() []AgentSnapshot
	JobSnapshots() []JobSnapshot
	Config() config.Config
	ID() string
	Home() string
	Dir() string
	TranscriptPath(agentID string) (string, error)
	InstructionSource() config.InstructionSource
	PolicySource() config.PolicySource
	ModelCatalog() []provider.Catalog
	ModelGuidance() string
}
