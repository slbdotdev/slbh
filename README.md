# slbh

`slbh` is a small, Linux-first agent harness for coding work. It provides a
Bubble Tea terminal UI, an independent seat agent and child agents, streaming
OpenAI-compatible provider responses, durable JSONL transcripts, and managed
shell jobs.

## Requirements

- Go 1.27 or newer.
- An API key for any native provider model you select. Codex leaves use an
  authenticated local `codex` executable instead.
- To launch Codex leaves, `codex` must be on `PATH` (or be supplied through
  `Options.CodexCommand` when embedding the runtime).
- Linux is the primary target. WSL2 is supported; non-Linux builds use a
  shell fallback and do not provide Linux process-group semantics.

## Quick start

From the repository checkout:

```sh
go run ./cmd/slbh
```

Or build a reusable binary:

```sh
go build -o slbh ./cmd/slbh
./slbh
```

The defaults are a `deepseek-v4-flash` seat agent at `xhigh` effort,
`zai/glm-5.3-flash` child agents at `high` effort, and the local 5080
workhorse `local/q27-IQ2_M-96k` for leaf agents. A new configuration has no
approved models, so the default native seat model stays disabled until models
are selected.

After starting a fresh configuration, run `/models`, wait for the catalogs,
and assign the seat, subagent, and leaf defaults with `r`, `s`, and `l`.
Press `Esc` to save and close the model menu. `/model NAME` is a shortcut that
approves and selects `NAME` for the seat agent only; child defaults still need
to be approved or supplied explicitly when a child is launched.

## Providers and configuration

Provider routing is automatic:

- `local/` model names route to the desktop RTX 5080's Ollama server without
  an API key. The default local model is `local/q27-IQ2_M-96k`, the campaign's
  best long-context quant, served as `q27-IQ2_M-96k` on Ollama.
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
| `SLBH_HOME` | `$HOME/.slbh` | Base directory for configuration, history, and runtime records. |
| `SLBH_MODEL` | `deepseek-v4-flash` | Seat agent model. |
| `SLBH_EFFORT` | `xhigh` | Seat reasoning effort. |
| `SLBH_SUBAGENT_MODEL` | `zai/glm-5.3-flash` | Default child-agent model. |
| `SLBH_LEAF_MODEL` | `local/q27-IQ2_M-96k` | Default depth-two child model. |
| `SLBH_SUBAGENT_EFFORT` | `high` | Default child-agent effort. |
| `SLBH_PYTHON` | managed `~/.local/share/slbh/python` interpreter | Python interpreter used by `quick_py` and `long_py`. |
| `SLBH_ENDPOINT` | OpenRouter chat-completions endpoint | Compatible provider endpoint. |
| `SLBH_LOCAL_ENDPOINT` | `http://fractal.wyvern-temperature.ts.net:11434/v1/chat/completions` | Desktop Ollama chat-completions endpoint. |

The model policy is persisted at `$SLBH_HOME/config.json`. `/models` always
shows the configured local Ollama model and refreshes catalogs for providers
whose keys are present. The OpenRouter catalog is limited to models created
within the last year. Model and effort settings from the environment take
precedence over their persisted counterparts.

For native provider agents, the harness estimates context usage before a
request and compacts history at 70% of the active model's discovered context
window. If model metadata is unavailable, it uses a 128,000-token fallback.
Automatic compaction keeps the most recent 24 messages and writes a durable
marker; `/compact` does the same on demand. Requests stream responses and
include tool definitions, reasoning options where supported, usage, and a
stable `prompt_cache_key`.

## TUI controls

The message viewport, multiline input bar, agent panel, and footer resize with
the terminal. Streaming stays anchored to the bottom unless the viewport has
been scrolled. Status, usage, and request telemetry remains in the event
stream and transcript and is omitted from the normal message viewport; other
non-chat activity renders in compact blocks. Thinking blocks show elapsed
seconds, and tool blocks show per-tool call tallies. Completed background jobs
arrive automatically at the next API boundary with their captured output.

| Key | Action |
| --- | --- |
| `Enter` | Send input or run a slash command. |
| `Ctrl-J` | Insert a newline into the input. |
| `Up` / `Down` | Recall input history; Down can move toward the agent list. |
| `Tab` | Complete an unambiguous slash command. |
| `Esc` | Return from the agent list or a child view toward the seat. |
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
| `/model NAME` | Approve and select a model for the seat agent. |
| `/effort LEVEL` | Change the seat agent's effort. |
| `/agents` | Record the current agent tree as a status event. |
| `/jobs` | Record current jobs as a status event. |
| `/compact` | Compact the selected agent's history. |

When a child agent is selected, submitted text is sent to that child as a
steering message. It does not create a new seat turn.

## Agents and tools

Native agents can create children through depth two. Each agent has its own
history, model, effort, working-directory setting, and message inbox. A turn
may use up to 100 provider/tool rounds. Child launch requests return
immediately; results use the same mandatory steering path as every other
message.

The parent names each subagent. Choose a title of three relevant words joined
by hyphens, such as `inspect-api-cache`. This is naming guidance only; slbh does
not enforce the format.

**Mid-turn delivery is mandatory for every agent and every message.** Messages
enter the recipient's context in FIFO order at the next API/tool call boundary,
within its current turn. An idle recipient wakes immediately. This applies to
user input, parent/child/sibling messages, follow-ups, and child results. There
is no separate next-turn message queue. Waiting for the end of an agentic turn
is a delivery failure, never an optional mode.

A call boundary is the completion of an individual inference request or tool
call, not the end of the agent's overall task. **Messages never cancel in-flight
API or tool calls or discard paid-for output.** Completed inference text,
already-produced tool calls, and tool results are retained. Tool calls from a
completed response execute in order, with messages inserted between complete
call/result pairs; the next inference receives all of that work and the new
messages. A response without tools cannot end the turn while messages are
pending. Message transport is independent of the lossy UI event channel.

Available tools are:

- Files: `glob`, `grep`, `read_file`, `read_bytes`, `read_lines`, `edit_file`,
  `apply_patch`, and `write_file`.
- Jobs: `quick_bash`, `long_job`, `list_jobs`, `read_job`, and `kill_job`.
- Python jobs: `quick_py`, `long_py`, `list_jobs`, `read_job`, and `kill_job`.
- Agents: `list_subagents`, `launch_subagent`, `msg_subagent`, and
  `end_subagent`.

`launch_subagent` accepts `harness: "codex"` for a Codex leaf launched by a
native parent (at either supported child depth). The leaf runs a persistent
`codex app-server --stdio` session in the requested working directory. Parent
messages use Codex `turn/steer` while a turn is active and start a new turn
when it is idle; Codex can send progress back with the private
`slbh_message_parent` tool. Codex leaves cannot launch agents. This path is
headless and does not depend on the Bubble Tea UI. Set
`Options.CodexCommand` when embedding the runtime to use a particular Codex
executable; the default is `codex` from `PATH`.

Codex leaves must receive an explicit ChatGPT model slug because the native
subagent and leaf defaults are provider-specific. A native seat or level-one
agent launches one with `harness: "codex"` and a model such as
`gpt-5.6-luna`; the name is passed unchanged to Codex and is not limited by
slbh's native approved-model list. For example:

```json
{"title":"codex worker","harness":"codex","model":"gpt-5.6-luna","brief":"..."}
```

`msg_subagent` addresses any agent in the runtime by ID, including a parent or
sibling. Empty messages and delivery to stopped agents return errors. Successful
submission acknowledges acceptance; the transcript records context insertion
as `steer` or `child_result` at the call boundary.

When a tool fails, the model receives the error **and** whatever output the tool produced,
error first and bounded. This matters most for `quick_bash` and `quick_py`: a non-zero exit
is routinely informative rather than fatal — `grep` exits 1 when it matches nothing, `test`
exits 1 on false, a failing suite exits 1 — so an agent given only the exit code cannot tell
"no matches" from "command not found" and retries blind.

File arguments may be absolute or relative; relative paths resolve against the
active agent's working directory. slbh does not add a filesystem permission
boundary, so the operating system determines whether a requested path or
working directory is usable. Reads and job output are bounded. `quick_bash` is
for short foreground commands with a five-second direct-command timeout;
`long_job` is the asynchronous option for work that may take longer. `quick_py`
and `long_py` provide the same two shapes through the Ansible-managed
scientific Python environment, with the system Python fallback retained for
unmanaged development checkouts. Jobs and agent activity are recorded in the
active session transcript.

## Headless

`slbh` with a prompt runs one turn without the TUI and exits. It is the same runtime, the
same seat agent, the same tools and the same system prompt the TUI drives — only the front
end differs. Codex leaves have always been headless; this is the native equivalent.

```sh
slbh -p "summarise the failing tests in ./logs"
slbh --prompt-file brief.md --workdir /srv/project --timeout 15m
echo "what changed today?" | slbh --prompt-file -
```

| flag | meaning |
| --- | --- |
| `-p`, `--prompt` | prompt text; supplying one is what selects headless |
| `--prompt-file` | read the prompt from a file, or `-` for stdin |
| `--workdir` | directory the agent works in (default: the current one) |
| `--model` | model for this turn; naming one also approves it for the turn |
| `--effort` | thinking effort for this turn |
| `--timeout` | wall cap such as `15m`; zero means none |
| `--json` | one JSON object per runtime event on stdout |
| `-q`, `--quiet` | no event stream; print only the summary |
| `--summary` | print the run summary as JSON on exit |

Exit status is `0` when the turn completed, `1` on error, `2` on a usage problem, and `124`
when the wall cap was reached — so a caller can tell a finished turn from a truncated one
without parsing output.

Running with no flags starts the TUI exactly as before; the prompt is the only thing that
selects headless.

Embedders can skip the CLI and call `headless.Run(runtime, headless.Options{...})`, which
neither creates nor closes the runtime, so several turns can be driven in one process.


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

Run these checks from the repository checkout:

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

Messaging regression tests must verify model-visible delivery during an active
turn, including a message arriving during the final inference response, idle
wakeup, FIFO bursts, tool boundaries, and both directions of parent/child
communication. They must also verify that in-flight calls finish, their output
is preserved, and completed tool calls are not discarded or replayed. A queued
message or UI event alone is not evidence of successful delivery.

An optional live test makes real provider requests and requires a configured
key:

```sh
SLBH_RUN_LIVE_TESTS=1 go test -tags live_integration ./internal/harness \
  -run TestLiveTranscriptReplayAndCache -count=1 -timeout 10m
```

The Codex leaf acceptance tests use the host's Codex login and are separately
gated because they consume plan quota. Run the direct Codex-leaf tests with:

```sh
SLBH_RUN_CODEX_TESTS=1 go test -tags live_integration ./internal/harness \
  -run '^TestLiveCodexLeaf' -count=1 -timeout 10m
```

The native-seat continuation test also requires a native provider key:

```sh
SLBH_RUN_NATIVE_CODEX_TESTS=1 go test -tags live_integration ./internal/harness \
  -run '^TestLiveNativeSeatCodexLeafContinuation$' -count=1 -timeout 10m
```
