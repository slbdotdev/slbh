# slbh

`slbh` is a small, Linux-first agent harness for coding work. It provides a
Bubble Tea terminal UI, independent root and child agents, streaming
OpenAI-compatible provider responses, durable JSONL transcripts, and managed
shell jobs.

## Requirements

- Go 1.27 or newer.
- An API key for the provider used by the selected model.
- Linux is the primary target. WSL2 is supported; non-Linux builds use a
  shell fallback and do not provide Linux process-group semantics.

## Quick start

From the repository root:

```sh
go run ./cmd/slbh
```

Or build a reusable binary:

```sh
go build -o slbh ./cmd/slbh
./slbh
```

The defaults are a `deepseek-v4-flash` root agent at `xhigh` effort and
`zai/glm-5.3-flash` child agents at `high` effort. A new configuration has no
approved models, so the harness fails closed until models are selected.

After starting a fresh configuration, run `/models`, wait for the catalogs,
and assign the root, subagent, and leaf defaults with `r`, `s`, and `l`.
Press `Esc` to save and close the model menu. `/model NAME` is a shortcut that
approves and selects `NAME` for the root agent only; child defaults still need
to be approved or supplied explicitly when a child is launched.

## Providers and configuration

Provider routing is automatic:

- `DEEPSEEK_API_KEY` routes `deepseek/` and `deepseek-` model names to
  DeepSeek.
- `ZAI_API_KEY` routes `zai/` and `glm-` model names to Z.ai.
- Other models use `SLBH_ENDPOINT` with `OPENROUTER_API_KEY`. The endpoint
  defaults to the OpenRouter chat-completions endpoint.

A matching native route takes precedence over the configured endpoint,
including when the same model is also listed by OpenRouter. A custom
OpenAI-compatible endpoint uses `OPENROUTER_API_KEY` unless a native route
wins.

These environment variables are read at startup:

| Variable | Default | Purpose |
| --- | --- | --- |
| `SLBH_HOME` | `$HOME/.slbh` | Root directory for configuration, history, and runtime records. |
| `SLBH_MODEL` | `deepseek-v4-flash` | Root agent model. |
| `SLBH_EFFORT` | `xhigh` | Root reasoning effort. |
| `SLBH_SUBAGENT_MODEL` | `zai/glm-5.3-flash` | Default child-agent model. |
| `SLBH_LEAF_MODEL` | Same as `SLBH_SUBAGENT_MODEL` | Default depth-two child model. |
| `SLBH_SUBAGENT_EFFORT` | `high` | Default child-agent effort. |
| `SLBH_ENDPOINT` | OpenRouter chat-completions endpoint | Compatible provider endpoint. |

The model policy is persisted at `$SLBH_HOME/config.json`. `/models` refreshes
catalogs for providers whose keys are present. The OpenRouter catalog is
limited to models created within the last year. Model and effort settings from
the environment take precedence over their persisted counterparts.

Before a request, the harness estimates context usage and compacts history at
70% of the active model's discovered context window. If model metadata is
unavailable, it uses a 128,000-token fallback. Automatic compaction keeps the
most recent 24 messages and writes a durable marker; `/compact` does the same
on demand. Requests stream responses and include tool definitions, reasoning
options where supported, usage, and a stable `prompt_cache_key`.

## TUI controls

The message viewport, multiline input bar, agent panel, and footer resize with
the terminal. Streaming stays anchored to the bottom unless the viewport has
been scrolled. Status, usage, and lifecycle telemetry remains in the event
stream and transcript but is hidden from the normal message viewport.

| Key | Action |
| --- | --- |
| `Enter` | Send input or run a slash command. |
| `Ctrl-J` | Insert a newline into the input. |
| `Up` / `Down` | Recall input history; Down can move toward the agent list. |
| `Tab` | Complete an unambiguous slash command. |
| `Esc` | Return from the agent list or a child view toward the root. |
| `Enter` in agent list | View that agent without interrupting it. |
| `PgUp` / `PgDn` | Scroll the viewport by five lines. |
| `Ctrl-U` / `Ctrl-D` | Scroll the viewport up or down. |
| `Ctrl-C` / `Ctrl-Q` | Shut down and exit. |

Input history is stored as JSON lines in `$SLBH_HOME/history`, normally
`~/.slbh/history`, and is shared by launches using the same home directory.

## Slash commands

| Command | Action |
| --- | --- |
| `/exit`, `/quit`, `/q` | Stop agents and jobs, close transcripts, and exit. |
| `/clear` | Clear the selected agent and start a new session. |
| `/models` | Refresh provider catalogs and open the model menu. |
| `/model NAME` | Approve and select a model for the root agent. |
| `/effort LEVEL` | Change the root agent's effort. |
| `/agents` | Record the current agent tree as a status event. |
| `/jobs` | Record current jobs as a status event. |
| `/compact` | Compact the selected agent's history. |

When a child agent is selected, submitted text is sent to that child as a
steering message. It does not create a new root turn.

## Agents and tools

The root agent can create children through depth two. Agents have independent
histories, models, efforts, working directories, and steering queues. A turn
may use up to 100 provider/tool rounds. Child launch requests return
immediately; later results are delivered to the requesting agent.

Subagents currently execute through this local runtime. `working_dir`,
`harness`, and `ssh` values in a launch request are recorded as agent metadata;
the current harness does not use them to execute a remote process.

Available tools are:

- Files: `glob`, `grep`, `read_file`, `read_bytes`, `read_lines`, `edit_file`,
  `apply_patch`, and `write_file`.
- Jobs: `quick_bash`, `long_job`, `list_jobs`, `read_job`, and `kill_job`.
- Agents: `list_subagents`, `launch_subagent`, `msg_subagent`, and
  `end_subagent`.

File operations are scoped to the active agent's working directory. Reads and
job output are bounded. `quick_bash` is for short foreground commands with a
five-second direct-command timeout; `long_job` is the asynchronous option for
work that may take longer. Jobs and agent activity are recorded in the active
session transcript.

## Runtime data

Each launch gets a unique runtime directory with per-agent, per-session
transcripts:

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

Transcripts are append-only JSONL and retain messages, reasoning, tool calls
and results, usage, status, lifecycle, and job records. `/clear` creates a new
session for the selected agent while preserving its agent ID. `runtime.json`
is a live marker removed during normal shutdown.

Shutdown cancels provider requests, stops agents, kills and drains jobs, and
closes session transcripts. The same cleanup path handles slash-command exit,
`Ctrl-C`, `Ctrl-Q`, and termination signals.

## Development

Run these checks from the repository root:

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

An optional live test makes real provider requests and requires a configured
key:

```sh
SLBH_RUN_LIVE_TESTS=1 go test -tags live_integration ./internal/harness \
  -run TestLiveTranscriptReplayAndCache -count=1 -timeout 10m
```
