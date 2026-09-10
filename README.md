# slbh

`slbh` is a small, Linux-first agent harness for coding work. It provides a
responsive Bubble Tea terminal UI, isolated agents, streaming provider
responses, durable JSONL transcripts, and non-blocking shell jobs.

## Features

- A streaming terminal REPL with a scrollable message viewport.
- User and assistant messages use standalone bold-yellow bullet headers, with
  green and blue full-width message bodies.
- Thinking, tool, steer, error, and tool-result output share a gray rolling
  context block with a bold-yellow bullet header. The body keeps the newest
  ten visual lines. Status and usage telemetry is retained in the event data
  and transcript but hidden from the message viewport.
- Events before the first user message for the selected agent are omitted from
  the viewport but remain available in the transcript.
- A root agent with children up to depth two. Agents have independent history,
  model, effort, working directory, and steer queues.
- Five-second foreground shell commands and inspectable, killable background
  jobs.
- File search, reads, atomic edits, patch application, and new-file tools
  scoped to the active agent's working directory.
- Stable-prefix request construction and provider cache keys for input caching.
- Per-launch runtime directories with separate agent and session transcripts.

## Requirements

- Go 1.27 or newer.
- Linux is the primary target. WSL2 is supported for development and testing;
  non-Linux builds use a shell fallback and do not provide Linux process-group
  semantics.
- An API key for the provider used by the selected model.

## Quick start

From the repository root:

```sh
go run ./cmd/slbh
```

To build a reusable binary:

```sh
go build -o slbh ./cmd/slbh
./slbh
```

The default root model is `deepseek-v4-flash` at `xhigh` effort. Child agents
default to `zai/glm-5.3-flash` at `high` effort. The UI starts without an API
key, but inference turns finish with a provider configuration error.

## Providers

`slbh` sends OpenAI-compatible streaming requests. It selects a native
endpoint when the model prefix and matching key are present:

- `DEEPSEEK_API_KEY` routes `deepseek/` and `deepseek-` models to DeepSeek.
- `ZAI_API_KEY` routes `zai/` and `glm-` models to Z.ai.
- Other models use `SLBH_ENDPOINT`, which defaults to the OpenRouter chat
  completions endpoint, with `OPENROUTER_API_KEY`.

Native routes take precedence over the configured endpoint when their matching
key is available. A custom compatible endpoint can be set with
`SLBH_ENDPOINT`; it uses `OPENROUTER_API_KEY` unless a native route wins.

Example using OpenRouter:

```sh
export OPENROUTER_API_KEY="..."
export SLBH_MODEL="deepseek-v4-flash"
export SLBH_EFFORT="xhigh"
go run ./cmd/slbh
```

Example using native Z.ai for child agents:

```sh
export ZAI_API_KEY="..."
export SLBH_SUBAGENT_MODEL="zai/glm-5.3-flash"
go run ./cmd/slbh
```

Requests include streaming, reasoning effort where supported, tool
definitions, and a stable `prompt_cache_key`. The system prompt and tool
schema stay in the stable request prefix so providers can reuse their cache.

## Configuration

These environment variables are read at startup:

| Variable | Default | Purpose |
| --- | --- | --- |
| `SLBH_HOME` | `$HOME/.slbh` | Root directory for runtime records. |
| `SLBH_MODEL` | `deepseek-v4-flash` | Root agent model. |
| `SLBH_EFFORT` | `xhigh` | Root reasoning effort. |
| `SLBH_SUBAGENT_MODEL` | `zai/glm-5.3-flash` | Default child-agent model. |
| `SLBH_SUBAGENT_EFFORT` | `high` | Default child-agent effort. |
| `SLBH_ENDPOINT` | OpenRouter chat-completions endpoint | Compatible provider endpoint. |

Before a request, the harness estimates context usage and compacts history at
70% of the active model's discovered context window. If model metadata is
unavailable, it uses a 128,000-token fallback. Automatic compaction keeps the
most recent 24 messages and a durable marker; `/compact` performs the same
operation on demand.

## TUI controls

The message viewport occupies the upper portion of the terminal. The input
bar stays at the bottom with a top rule, text-entry area, and bottom rule. It
grows for wrapped or explicitly multiline input while the viewport contracts
to keep the agent panel and footer visible.

The footer shows the selected agent's model and effort, context used and
available, cache-hit percentage, and nonzero job or agent counts. The agent
panel is hidden until a child agent exists. Mouse-wheel scrolling is enabled,
and streaming remains anchored to the bottom unless the user scrolls away.

| Key | Action |
| --- | --- |
| `Enter` | Send input or run a slash command. |
| `Ctrl-J` | Insert a newline into the input. |
| `Up` / `Down` | Recall input history; Down moves toward the agent list. |
| `Tab` | Complete an unambiguous slash command. |
| `Esc` | Return from the agent list or a child view toward the root. |
| `Enter` in agent list | View that agent without interrupting it. |
| `PgUp` / `PgDn` | Scroll the message viewport by five lines. |
| `Ctrl-U` / `Ctrl-D` | Scroll the message viewport up or down. |
| `Ctrl-C` / `Ctrl-Q` | Shut down and exit. |

Sent user messages are stored as JSON-lines in the machine-global
`$SLBH_HOME/history` file, normally `~/.slbh/history`, and are shared by
launches from every working directory.

## Slash commands

| Command | Action |
| --- | --- |
| `/exit`, `/quit`, `/q` | Stop agents and jobs, close transcripts, and exit. |
| `/clear` | Clear the selected agent and start a new session. |
| `/model NAME` | Change the root agent's model for later turns. |
| `/effort LEVEL` | Change the root agent's effort for later turns. |
| `/agents` | Record the current agent tree as a status event; status output is hidden. |
| `/jobs` | Record current jobs as a status event; status output is hidden. |
| `/compact` | Compact the selected agent's history. |

## Agent tools

The root and child agents share these tools:

- Files: `glob`, `grep`, `read_file`, `read_bytes`, `read_lines`,
  `edit_file`, `apply_patch`, and `write_file`.
- Jobs: `quick_bash`, `long_job`, `list_jobs`, `read_job`, and `kill_job`.
- Agents: `list_subagents`, `launch_subagent`, `msg_subagent`, and
  `end_subagent`.

File operations are scoped to the active agent's working directory. Reads are
bounded where noted by the tool schema; `quick_bash` has a five-second
timeout, while `long_job` runs asynchronously and captures bounded output.
Jobs and agent activity are recorded in the active session transcript.

## Runtime data and cleanup

Each launch gets a unique runtime directory:

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

Transcripts are append-only and retain messages, reasoning, tool calls and
results, usage, status, lifecycle, and job records. `/clear` creates a new
session for only the selected agent while preserving its agent ID.
`runtime.json` is a live marker removed during normal shutdown.

Shutdown cancels provider requests, stops agents, kills and drains jobs, and
closes session transcripts. The same cleanup path handles slash-command exit,
`Ctrl-C`, `Ctrl-Q`, and termination signals.

An optional live provider test verifies transcript request replay and cache
hits. It makes real provider requests, so run it only with a configured key:

```sh
SLBH_RUN_LIVE_TESTS=1 go test -tags live_integration ./internal/harness \
  -run TestLiveTranscriptReplayAndCache -count=1 -timeout 10m
```

## Development

Run the local checks from the repository root:

```sh
gofmt -w cmd/slbh/main.go internal/config/*.go internal/harness/*.go internal/id/*.go internal/job/*.go internal/logx/*.go internal/provider/*.go internal/tui/*.go
go test ./...
go vet ./...
go build ./...
```

On Linux or WSL2, also run:

```sh
go test -race ./...
```

The test suite covers provider request construction and caching, model routing,
agent delivery and compaction, tool and path behavior, job lifecycle, cleanup,
and TUI rendering and interaction.
