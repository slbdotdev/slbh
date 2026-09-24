***This project, and ALL of it's associated documentation, was produced entirely by a system of coordinating AI agents. I have never read any of the code, and that is the point. This is an experiment.***

# slbh

`slbh` is a Linux-first runtime for multi-agent LLM work. It runs a root
agent (the Seat), an optional read-only watcher (the Intern), and child
agents backed by its own native provider loop, Codex, or Claude Code. It ships
a terminal UI and a headless JSONL protocol, streams provider responses, and
records every session as an append-only JSONL transcript.

## Features

- **Two frontends, one runtime.** A Bubble Tea TUI and a persistent headless
  JSON-RPC protocol share the same agents, tools, and event stream.
- **Delegation to depth two.** Native agents launch children that run on
  slbh's own provider loop, a Codex `app-server` session, or Claude Code.
- **Mid-turn messaging.** Messages reach an agent at the next API or tool-call
  boundary without cancelling in-flight work.
- **Policy-driven routing.** Each provider route is declared in a policy file;
  a route with no policy entry is refused.
- **Three wire protocols.** OpenAI chat completions, Anthropic Messages, and
  Ollama `/api/chat`, normalized so nothing downstream depends on which served
  a request.
- **Managed shell jobs.** Commands run as jobs that can be backgrounded,
  inspected while running, and killed.
- **Automatic compaction.** History is summarized when context usage reaches
  70% of the model's window. While the summary is written the agent's status
  is `compacting` and a `compacting` event names how many messages it replaces;
  the `compact` event that follows carries the summary.

## Requirements

- Go 1.26 or newer.
- Linux. WSL2 is supported; other platforms use a shell fallback without
  Linux process-group semantics.
- An API key for each cloud provider you use (see [Providers](#providers)).
- Optional: an authenticated `codex` or `claude` executable on `PATH` for
  Codex or Claude Code children.

## Quick start

```sh
go build -o slbh ./cmd/slbh
./slbh
```

A new configuration has no approved models. Run `/models`, select the models
you want, and press `Esc` to save. On a host without a policy file, press `p`
in the model menu to generate a local routing policy for the selected models.

## Usage

```text
slbh [flags]

  --headless          Run the JSONL runtime protocol instead of the TUI
  -p, --prompt TEXT   Initial Seat prompt (implies --headless)
  --prompt-file PATH  Read the initial prompt from a file (implies --headless)
  --workdir DIR       Working directory for the Seat
  --model NAME        Seat model
  --effort LEVEL      Seat reasoning effort
  --tool-shape SHAPE  lean (default), mid, full, anthropic, or codex
  --intern            Run the read-only Intern watcher
```

### TUI

| Key | Action |
| --- | --- |
| `Enter` | Send input or run a slash command |
| `Ctrl-J` | Insert a newline |
| `Up` / `Down` | Recall input history; `Down` moves toward the agent list |
| `Tab` | Complete a slash command |
| `Esc` | Return from the agent list or a child view to the Seat |
| `PgUp` / `PgDn`, `Ctrl-U` / `Ctrl-D` | Scroll the viewport |
| `Ctrl-C` / `Ctrl-Q` | Shut down and exit |

| Command | Action |
| --- | --- |
| `/models` | Refresh provider catalogs and open the model menu |
| `/model NAME` | Approve and select the Seat model |
| `/effort LEVEL` | Set the Seat's reasoning effort |
| `/agents`, `/jobs` | Record the agent tree or job list as a status event |
| `/compact` | Summarize the selected idle agent's history |
| `/clear` | Start a new session for the selected agent |
| `/mouse` | Toggle mouse capture (wheel scrolling vs. text selection) |
| `/exit`, `/quit`, `/q` | Stop agents and jobs and exit |

When a child agent is selected, input is sent to it as a steering message.
Input history is stored in `$SLBH_HOME/history`.

### Headless

```sh
slbh --headless --intern
slbh -p "Summarize the open issues" --model zai/glm-5.3-flash
slbh --prompt-file brief.md --workdir /srv/project
```

Requests are JSON-RPC 2.0 lines on stdin; responses and asynchronous `event`
notifications are written to stdout. The process serves multiple turns until
it receives `close`, stdin closes, or it is signalled.

```json
{"jsonrpc":"2.0","id":1,"method":"send_prompt","params":{"agent_id":"agent-...","prompt":"continue"}}
{"jsonrpc":"2.0","id":2,"method":"agents","params":{}}
{"jsonrpc":"2.0","id":3,"method":"close","params":{}}
```

Queries expose agent and job snapshots, runtime metadata, transcript paths,
the model catalog, and cursor-based event polling. `quiescent` reports whether
all jobs are delivered and no agent has pending work. Embedders can call
`headless.Serve` directly.

## Providers

The route is chosen from the model name:

| Model name | Provider | Credential |
| --- | --- | --- |
| `local/...` | Ollama (`SLBH_LOCAL_ENDPOINT`) | none |
| `zai/...`, `glm-...` | Z.ai | `ZAI_API_KEY` |
| `cerebras/...` | Cerebras | `CEREBRAS_API_KEY` |
| anything else | OpenRouter or `SLBH_ENDPOINT` | `OPENROUTER_API_KEY` |

Routing fails closed: a native model whose key is missing is refused rather
than redirected to OpenRouter. Setting `SLBH_ENDPOINT` explicitly overrides
any route.

### Routing policy

Every route must have a policy entry. slbh reads, in order:

1. `$SLBH_HOME/policy.json`, if present (treated as externally managed and
   never written by slbh).
2. The `local_policy` block in `$SLBH_HOME/config.json`, which `/models` can
   generate.

Per route, the policy defines the endpoint, wire protocol, catalog endpoint,
context window, `maxOutputTokens`, effort mapping, `defaultEffort`, Ollama
sampler `options`, and, for OpenRouter, the provider posture (`zdr`,
`data_collection`, `sort`, `ignore`, `max_price`). An OpenRouter route with no
stated posture is refused. Credentials never appear in policy files.

Two requests are refused rather than silently adjusted: an effort level the
route cannot express, and a wire protocol this build does not implement.

### Wire protocols

| Wire | Used by | Effort parameter |
| --- | --- | --- |
| `openai-chat` | OpenRouter, Cerebras | `reasoning_effort` |
| `anthropic-messages` | Z.ai coding plan | `output_config.effort` |
| `ollama-chat` | Local Ollama | `think` |

Usage, tool-call indexes, tool arguments, and stop reasons are normalized
across wires. 4xx errors are not retried (including 429); 5xx errors and
retryable in-stream errors are.

## Agents and tools

Each agent has its own history, model, effort, working directory, and inbox.
A turn may run up to 500 provider/tool rounds.

**Message delivery.** Messages from the user, parents, children, and siblings
enter the recipient's context in FIFO order at the next call boundary, and an
idle agent wakes immediately. Delivery never cancels in-flight requests or
tool calls, and completed output is never discarded.

**Children.** `launch_subagent` returns immediately; results arrive as
messages. The optional `harness` field selects `native`, `codex`, or
`claude_code`. Depth-two children require an explicit model. Codex and Claude
Code children receive the model name unchanged:

```json
{"title":"codex-worker","harness":"codex","model":"gpt-6-luna","brief":"..."}
```

**Project instructions.** A native agent's prompt includes one `AGENTS.md`
(or `CLAUDE.md`) per directory from the git root down to its working
directory, capped at 32 KiB.

### Tool shapes

The primary file and shell tools are selected by a tool shape. All shapes
share `python`, `job`, and the agent tools (`list_subagents`,
`launch_subagent`, `msg_subagent`, `end_subagent`).

| Shape | Primary tools |
| --- | --- |
| `lean` (default) | `apply_patch`, `bash` |
| `mid` | `read_file`, `apply_patch`, `bash` |
| `full` | `glob`, `grep`, `read_file`, `read_bytes`, `read_lines`, `edit_file`, `apply_patch`, `write_file`, `bash` |
| `anthropic` | Claude Code's `Bash`, `Read`, `Edit`, `Write` |
| `codex` | Codex CLI's `exec_command`, `write_stdin`, `apply_patch` |

Set the shape with `tool_shape` in `config.json`, `SLBH_TOOL_SHAPE`, or
`--tool-shape` (later sources win). An unknown shape exits with status 2.

`apply_patch` accepts unified diffs or Codex `*** Begin Patch` format and
applies only if every hunk matches.

### Jobs

`bash`, `pwsh`, and `python` wait up to `wait_seconds` (default 10) and then
background the command. Output is capped at 20,000 tokens, keeping the head
and tail. `job` lists, reads (including while running), and kills jobs. An
optional `warn_after_seconds` sends a single message to the agent that
started the job. Failed tool calls return both the error and any output.

## Configuration

| Variable | Default | Purpose |
| --- | --- | --- |
| `SLBH_HOME` | `~/.slbh` | Configuration, history, and runtime data |
| `SLBH_MODEL` | `zai/glm-5.3-flash` | Seat model |
| `SLBH_EFFORT` | `medium` | Seat effort |
| `SLBH_SUBAGENT_MODEL` | `zai/glm-5.3-flash` | Default child model |
| `SLBH_SUBAGENT_EFFORT` | `medium` | Default child effort |
| `SLBH_INTERN_MODEL` | `local/q27-UD-Q2_K_XL-64k` | Intern model |
| `SLBH_INTERN_EFFORT` | `low` | Intern effort |
| `SLBH_ENDPOINT` | OpenRouter | OpenAI-compatible endpoint override |
| `SLBH_LOCAL_ENDPOINT` | from policy | Ollama `/api/chat` URL |
| `SLBH_TOOL_SHAPE` | `lean` | Tool shape |
| `SLBH_PYTHON` | managed interpreter | Interpreter for the `python` tool |
| `SLBH_BASH` | auto-detected | Git Bash path on Windows |
| `SLBH_FRAME_PROFILE` | unset | CSV path for TUI frame timing |

Environment variables override `config.json`. A route's `defaultEffort`
overrides the effort variables; an effort given at launch or with `/effort`
overrides both.

## Runtime data

```text
$SLBH_HOME/runtimes/run-.../
├── runtime.json
└── agents/agent-.../
    ├── sessions/session-.../transcript.jsonl
    ├── jobs/job-.../
    └── scratch/
```

Transcripts record messages, reasoning, tool calls and results, usage,
status, and job events. `runtime.json` is removed on clean shutdown, which
cancels requests, stops agents, drains jobs, and closes transcripts.

## Development

```sh
gofmt -l .
go vet ./...
go test ./...
go test -race ./...   # Linux / WSL2
```

Live tests make real provider requests and are gated by environment
variables:

```sh
SLBH_RUN_LIVE_TESTS=1 go test -tags live_integration ./internal/harness \
  -run TestLiveTranscriptReplayAndCache -count=1 -timeout 10m

SLBH_RUN_CODEX_TESTS=1 go test -tags live_integration ./internal/harness \
  -run '^TestLiveCodexLeaf' -count=1 -timeout 10m

# Compaction end to end on a real route with its window shrunk to 16,384;
# SLBH_ACCEPT_COMPACT_ROUTE picks the route (default the rented 5090).
SLBH_ACCEPT_COMPACT=1 go test -tags live_integration ./internal/harness \
  -run TestAcceptanceCompaction -count=1 -timeout 10m
```

Messaging changes must be tested for model-visible delivery during active
turns, idle wakeup, FIFO ordering, and preservation of in-flight work.
