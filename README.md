# slbh

`slbh` is a small, Linux-first agent harness for coding work. It combines a
responsive Bubble Tea terminal UI with runtime-isolated agents, streaming
provider responses, durable JSONL transcripts, and non-blocking shell jobs.

The design goal is deliberately narrow: keep the orchestration, cleanup, and
tool plumbing in code so an agent can focus on the work.

## What it provides

- A streaming terminal REPL with a scrollable message viewport.
- Full-width, wrapped user and assistant message blocks, with thinking,
  tool-call, status, usage, and error events shown separately. Consecutive
  non-chat events share one rolling block while streaming. Each block has a
  fixed, bold yellow header showing the latest response type, such as
  thinking or the tool name, while its ten-line body shows only the newest
  visual lines on a gray background. User messages use green blocks, assistant
  messages use blue blocks, and the transcript remains lossless.
  Startup control events before the first user message are omitted from the
  viewport but remain in the transcript.
- A root agent plus child agents up to depth two. Each agent has its own
  history, model, effort, working directory, and steer queue.
- Foreground commands with a five-second timeout and background jobs that can
  be inspected or killed without blocking the conversation.
- File search, file reads, atomic edits, patch application, and file creation
  tools scoped to the active agent's working directory.
- Stable-prefix request construction and provider cache keys for input
  caching.
- Per-launch runtime directories with append-only transcripts split by agent
  and agent session, so concurrent launches do not share state.

## Requirements

- Go 1.27 or newer.
- Linux is the primary target. WSL2 is supported for local development and
  testing; non-Linux builds use the platform shell fallback but do not provide
  Linux process-group semantics.
- An API key for the provider/model you want to use.

## Quick start

From the repository root:

```sh
go run ./cmd/slbh
```

For a reusable binary:

```sh
go build -o slbh ./cmd/slbh
./slbh
```

The default root configuration is `deepseek-v4-flash` at `xhigh` effort. Child
agents default to `zai/glm-5.3-flash` at `high` effort. Without an API key, the
UI still starts and records the conversation, but provider turns finish with
a configuration error in the transcript.

## Provider setup

`slbh` uses OpenAI-compatible streaming requests. Provider selection is based
on the model name and available key:

- `DEEPSEEK_API_KEY` uses DeepSeek's native endpoint for models beginning with
  `deepseek/` or `deepseek-`.
- `ZAI_API_KEY` uses Z.ai's native endpoint for models beginning with `zai/`
  or `glm-`.
- `OPENROUTER_API_KEY` is the fallback for other models and the default
  OpenRouter endpoint.

A custom compatible endpoint can be supplied with `SLBH_ENDPOINT`. Native
DeepSeek and Z.ai endpoints take precedence when their matching key and model
are present.

Example using OpenRouter:

```sh
export OPENROUTER_API_KEY="..."
export SLBH_MODEL="deepseek-v4-flash"
export SLBH_EFFORT="xhigh"
go run ./cmd/slbh
```

Example using the native Z.ai endpoint for child agents:

```sh
export ZAI_API_KEY="..."
export SLBH_SUBAGENT_MODEL="zai/glm-5.3-flash"
go run ./cmd/slbh
```

Requests include streaming, reasoning effort where supported, tool
definitions, and a stable `prompt_cache_key`. The stable system prompt and
tool schema are kept separate from user turns so providers can reuse the
prefix cache.

## Configuration

All settings are read from environment variables when the process starts.

| Variable | Default | Purpose |
| --- | --- | --- |
| `SLBH_HOME` | `$HOME/.slbh` | Root directory for runtime records. |
| `SLBH_MODEL` | `deepseek-v4-flash` | Root agent model. |
| `SLBH_EFFORT` | `xhigh` | Root reasoning effort. |
| `SLBH_SUBAGENT_MODEL` | `zai/glm-5.3-flash` | Default child-agent model. |
| `SLBH_SUBAGENT_EFFORT` | `high` | Default child-agent reasoning effort. |
| `SLBH_ENDPOINT` | OpenRouter chat-completions endpoint | Custom compatible endpoint. |

Compaction is automatic and not configurable: before the first request for an
active model, `slbh` asks the provider's live model metadata endpoint for its
maximum context window and starts compaction at 70% of that value. The result
is cached for that model during the runtime. If the provider does not expose
usable metadata, the harness falls back to 128,000 tokens. Token usage is
estimated from the serialized system prompt, tool definitions, and message
history. Compaction keeps the most recent 24 messages plus a durable marker;
`/compact` remains available for an explicit compaction.

## TUI controls

The upper portion of the terminal is the scrollable event viewport. The input
bar stays at the bottom as a minimum three-row frame: a horizontal top rule,
the text entry area, and a horizontal bottom rule. The text entry has no pipe,
prompt glyph, or leading space. It grows to show wrapped and explicitly
multiline input, while the message viewport contracts so the agent list and
status footer remain visible inside the terminal. The footer follows the input
frame and shows the current agent's model and effort, followed by compact
`used/available · cache%` context and prompt-cache statistics. This unlabeled,
space-efficient layout is the intended UI/UX. The root-only runtime hides the
agent list; it appears when a child exists.

| Key | Action |
| --- | --- |
| `Enter` | Send the input or run a slash command. |
| `Ctrl-J` | Insert a newline into the input. |
| `Up` / `Down` | Recall input history; Down from a fresh single-line input enters the agent list. |
| `Tab` | Complete an unambiguous slash command. |
| `Esc` | Move back toward the input from the agent list; from a child view, return to root. |
| `Enter` in agent list | View that agent's transcript without interrupting it. |
| `PgUp` / `PgDn` | Scroll the message viewport by five lines. |
| `Ctrl-U` / `Ctrl-D` | Scroll the viewport by a smaller page step. |
| `Ctrl-C` / `Ctrl-Q` | Shut down the runtime and exit. |

Mouse-wheel scrolling is enabled for the message viewport. The UI keeps token
streaming anchored to the bottom unless the user has scrolled away.

Sent user messages are stored as JSON-lines in the machine-global
`$SLBH_HOME/history` file (normally `~/.slbh/history`) and are shared by
launches from every working directory.

## Slash commands

| Command | Action |
| --- | --- |
| `/exit`, `/quit`, `/q` | Stop agents and jobs, close all session transcripts, and exit. |
| `/clear` | Clear the selected agent view and start a new agent session. |
| `/model NAME` | Change the root agent's model for subsequent turns. |
| `/effort LEVEL` | Change the root agent's reasoning effort. |
| `/agents` | Print the current agent tree as a status event. |
| `/jobs` | Print the current runtime jobs as a status event. |
| `/compact` | Compact the selected agent's history, keeping recent work. |

## Agent tools

The root and child agents share the following tool surface.

### Files and search

- `glob` finds files by pattern.
- `grep` searches file content with a regular expression.
- `read_file` reads up to 100,000 bytes.
- `read_bytes` and `read_lines` read bounded ranges from large files.
- `edit_file` replaces one exact string atomically.
- `apply_patch` applies a unified or coordinated patch.
- `write_file` creates a new file and refuses to overwrite an existing one.

Paths are resolved from the calling agent's working directory and are checked
so file tools cannot escape that directory.

### Jobs

- `quick_bash` runs a foreground Bash command with a five-second timeout.
- `long_job` starts a non-blocking background Bash command and returns a job ID.
- `list_jobs` lists every job in the current runtime.
- `read_job` returns the job's captured stdout and stderr.
- `kill_job` stops a job owned by the calling agent.

Jobs capture bounded stdout and stderr, emit lifecycle events to the transcript,
and can emit a warning when they exceed `warn_after_seconds`. On Linux, jobs
run in their own process group; runtime shutdown kills the group, and the
parent-death signal covers hard process termination.

### Agent coordination

- `list_subagents` returns the complete runtime agent tree.
- `launch_subagent` creates a child up to the depth-two limit and can specify a
  title, brief, model, effort, working directory, harness label, and SSH
  metadata.
- `msg_subagent` steers an existing child without reordering its parent result.
- `end_subagent` stops a child agent.

## Runtime records and cleanup

Every launch receives a random ID such as `run-abc123...` and writes to its own
directory:

```text
$SLBH_HOME/
└── runtimes/
    └── run-.../
        ├── runtime.json
        └── agents/
            └── agent-.../
                └── sessions/
                    └── session-.../
                        └── transcript.jsonl
```

Each `transcript.jsonl` contains the user and assistant messages, streamed
reasoning, tool calls and results, usage, agent status, lifecycle, and job
records for one agent session. It is append-only and can be replayed after the
UI exits. `/clear` starts a new session directory for only the selected agent;
the agent ID remains stable for the lifetime of the TUI. `runtime.json` exists
as a live marker and is removed during normal shutdown.

The billed transcript/cache integration test is opt-in and is excluded from
normal builds and test runs. Run it manually from WSL only when a live provider
key is configured:

```sh
SLBH_RUN_LIVE_TESTS=1 go test -tags live_integration ./internal/harness \
  -run TestLiveTranscriptReplayAndCache -count=1 -timeout 10m
```

It records the exact wire request in the agent session transcript, reloads the
conversation from that record after clearing in-memory history, sends a second
turn, and requires the provider's usage response to report cached input tokens.
Session transcripts are retained under `$SLBH_HOME` (or `$HOME/.slbh`).

Closing the runtime cancels provider requests, stops all agents, kills and
drains all jobs, and closes every session transcript. The same cleanup path is
used by the UI exit commands, `Ctrl-C`, `Ctrl-Q`, and termination signals.

## Development

Run the complete local checks from the repository root:

```sh
gofmt -w cmd/slbh/main.go internal/config/*.go internal/harness/*.go internal/id/*.go internal/job/*.go internal/logx/*.go internal/provider/*.go internal/tui/*.go
go test ./...
go vet ./...
go build ./...
```

On Linux or WSL2, run the race-enabled suite as well:

```sh
go test -race ./...
```

The unit tests cover provider request construction and caching, model routing,
agent delivery and compaction, tool/path behavior, job lifecycle and cleanup,
and TUI wrapping, spacing, selection, and message-block rendering.
