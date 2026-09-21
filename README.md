# slbh

`slbh` is the Linux-first runtime for org v0.3, not a fourth managed harness.
It runs the Seat and Intern, launches managed Codex/Claude/native leaves, and
provides a Bubble Tea terminal UI, streaming provider responses, durable JSONL
transcripts, and managed shell jobs.

## Requirements

- Go 1.26 or newer.
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

The defaults are a `zai/glm-5.3-flash` Seat at `medium` effort and a local
`local/q27-UD-Q2_K_XL-64k` leaf/Intern route. Child delegation is depth-only:
native agents may launch one level deeper, and a depth-two child must receive
an explicit model string or managed short name. The optional harness selects
native slbh, Codex, or Claude Code; no named role catalogue is required. A new
configuration has no approved models, so the default native Seat model stays
disabled until models are selected.

After starting a fresh configuration, run `/models` and wait for the catalogs.
The Seat model can be selected with `/model NAME`; child launches choose their
model and optional harness directly. If the managed `$SLBH_HOME/models.toml`
file defines a short name such as `luna`, slbh expands it to the corresponding
real model string and uses its configured effort when no effort is supplied.
Press `Esc` to save and close the model menu.

## Providers and configuration

Provider routing is automatic:

- `local/` model names route to the desktop RTX 5080's Ollama server without
  an API key. The default local model is `local/q27-UD-Q2_K_XL-64k`, served as
  `q27-UD-Q2_K_XL-64k` on Ollama.
- `DEEPSEEK_API_KEY` routes `deepseek/` and `deepseek-` model names to
  DeepSeek.
- `ZAI_API_KEY` routes `zai/` and `glm-` model names to Z.ai.
- `CEREBRAS_API_KEY` routes `cerebras/` model names to Cerebras. Only the
  prefixed spelling routes there: the models it serves are open-weight ones
  OpenRouter and the desktop serve too, so a bare `qwen-3.8-27b` would be
  ambiguous where `glm-` is not.
- Other models use `SLBH_ENDPOINT` with `OPENROUTER_API_KEY`. The endpoint
  defaults to the OpenRouter chat-completions endpoint.

The synchronized Z.ai Coding Plan policy exposes both
`zai/glm-5.3-flash` and `zai/glm-5.3-flashx` on the Anthropic Messages wire.
FlashX is available when approved; it does not replace the Seat or Manager
default.

The Cerebras route is optional in the ordinary sense — no key, no branch in
`/models` and no route — and in one that is particular to it: its endpoint
accepts `reasoning_effort` at `low`, `medium` and `high` only, so the managed
route maps those three and leaves `max` and `xhigh` unmapped. Asking for one of
those refuses by name rather than quietly running at `high`. It also rejects
both reasoning round-trip fields (`include_reasoning` outbound,
`reasoning_content` on a replayed assistant turn), which slbh omits on this
flavor alone; reasoning still streams, but the model does not see its own
earlier thinking on a later turn. Its catalog publishes ids and no context
length, so the window comes from the policy pin — 131,072 tokens, the ceiling
the endpoint itself reports — and `/models` shows the pin rather than a blank.

A matching native route takes precedence over the configured endpoint,
including when the same model is also listed by OpenRouter. A custom
OpenAI-compatible endpoint uses `OPENROUTER_API_KEY` unless a native route
wins.

**Routing fails closed.** A model that belongs to a native route whose key is
absent is refused, naming the missing variable; it is not quietly redirected to
OpenRouter, because a model reachable on a plan never runs through OpenRouter.
Setting `SLBH_ENDPOINT` yourself is the deliberate override and is honoured even
for a native model — the endpoint's provenance is what routing consults, so the
same URL arriving as the built-in default overrides nothing. A model with no
native route and no `OPENROUTER_API_KEY` is refused when the route is resolved
rather than at its first request. The local route needs no credential and keeps
working with every provider key unset.

Every spelling that addresses one route resolves to a single authoritative route
key, so the routing table, the context pin and the per-route policy cannot
disagree about which route a request took. The `[1m]` model spellings are not
aliases of the plain slug and are never folded into it.

## Routing policy

Routing is governed by a policy document, and **a route with no policy entry
refuses**. What is compiled into the binary is the requirement that a policy
exist, and the mapping from a route to the environment variable holding its
credential — never the policy itself, which has to change without a rebuild.

The policy resolves in a fixed order:

1. `$SLBH_HOME/policy.json`, if present and valid. This file is wholly managed
   by the `slb-org` sync on the fleet, and slbh only ever reads it.
2. otherwise a `local_policy` block in the app-owned `$SLBH_HOME/config.json`,
   which `/models` can author.
3. otherwise slbh refuses to route.

Provenance is the filename, which is what lets a managed value beat a local one
without the application ever opening the managed file. On a host nobody manages,
open `/models` and press `p`: slbh authors a working policy for the models you
have selected, writes it into `config.json`, and routes on it from the next
request. On a managed host that write still happens and is still inert, because
the managed file wins — `/models` says so rather than leaving you to wonder why
an edit changed nothing.

Per route the policy carries the endpoint, the wire protocol, a separate
catalog endpoint where one cannot be derived from the other, a context window
where the provider's catalog cannot report one, an optional `maxOutputTokens`
bound on one generation, an effort descriptor, an optional `defaultEffort`, on an `ollama-chat` route an
`options` block of sampler settings, and — for
an OpenRouter route — the routing posture (`zdr`, `data_collection`, `sort`,
`ignore`, `max_price`). No credential appears in either file.

`defaultEffort` is the effort an agent takes when it is put on the route — a
Seat started on it or switched to it, the Intern, a leaf launched without an
explicit effort. It outranks the model-agnostic `SLBH_EFFORT`,
`SLBH_INTERN_EFFORT` and managed model-alias defaults, yields to an effort passed at
launch or set with `/effort`, and must be a level the route's effort map sends.

`maxOutputTokens` is per route rather than global because the routes differ by
more than an order of magnitude in what an unbounded generation costs: a cloud
route runs away in seconds, while a local 27B decoding at about 76 tokens per
second against a 196,608-token window is some forty minutes of held GPU before
anything stops it. Unset, a route takes its wire's default —
`DefaultMaxOutputTokens` on the OpenAI-shaped and Ollama wires, and on the
Messages wire the larger ceiling that exists only because that API requires the field. A
request may override both.

Bounding is also what makes the terminal telemetry mean anything. An unbounded
request can only ever come back `stop`, so `finish_reason` carries no signal at
all; with a bound, a generation that will not end reports `length` and is
visible as what it is rather than as a hang. When one does, on any wire, the
runtime emits a `warning` event naming the model, the stop reason and the
output token count, and it is written to the agent's transcript — the usage
record alone does not carry the stop reason, so without it a truncated reply
would look like any finished turn.

That posture is sent as OpenRouter's `provider` routing object on the
OpenRouter route and on no other, because `zdr` and `data_collection` are
OpenRouter concepts that say nothing about a plan endpoint or a server on your
own network. **An OpenRouter route whose policy states no posture refuses**:
routing there without it would pick an upstream on price and availability
alone, and a request carrying no `provider` object looks exactly like a normal
one, so nothing downstream would ever report the absence. `zdr` is required to
be *stated* rather than required to be true — a deliberate `false` is
expressible, an omission is not, because the two are indistinguishable once
decoded.

The one exception is an explicit `SLBH_ENDPOINT` override of a native route.
That route's policy entry describes its own endpoint and says nothing about
wherever you have pointed it, so no posture is demanded and none is invented.
An override is stepping outside the managed path deliberately, and it is the
one hole in this guarantee that you sign for by hand.

### Wires

A route names the protocol it speaks, and slbh implements three.

- `openai-chat` — the OpenAI-shaped chat-completions protocol: OpenRouter,
  DeepSeek and Z.ai's coding endpoint. Effort is `reasoning_effort`.
- `anthropic-messages` — the Anthropic-shaped Messages protocol, which the
  Z.ai coding plan also exposes. Effort is `output_config.effort`, sent with
  `x-api-key` and `anthropic-version`.
- `ollama-chat` — Ollama's native `/api/chat`, which the local route speaks.
  Effort is the top-level `think` string. The stream is NDJSON rather than
  SSE, tool calls arrive whole with object arguments, and the route's
  `options` block is sent as Ollama's `options` verbatim, beside `num_ctx`
  (from `contextWindow`) and `num_predict` (from `maxOutputTokens`).

The plan route runs on `anthropic-messages` because that is the only route on
which an effort setting is honoured. On the coding endpoint the effort enum is
applied but is not monotone at the top — `xhigh` and `max` both produce less
deliberation than `high` — while `output_config.effort` produces an ordered
ladder. On that same Anthropic endpoint `thinking.budget_tokens` and a bare
`reasoning_effort` are both accepted with HTTP 200 and then discarded, so
neither is representable in the policy schema at all.

The local route left `openai-chat` for `ollama-chat` on 2026-09-18 because
Ollama's `/v1` shim is lossy: it forces `top_p` to 1.0 over the model's own
0.95 and silently discards `repeat_penalty`, `top_k`, `min_p` and
`draft_num_predict` under HTTP 200, where `/api/chat` honours all of them
(measured by a seeded sampler A/B). A sampler guard can therefore only be
carried on the native wire, and the policy refuses an `options` block on any
other wire rather than let it be written down and never applied. The block
admits the sampler keys only — `temperature`, `top_k`, `top_p`, `min_p`,
`typical_p`, `repeat_penalty`, `repeat_last_n`, `presence_penalty`,
`frequency_penalty`, `seed`, `stop` — and refuses anything else by name:
Ollama ignores an option it does not know, and a runner option such as
`num_gpu` or `draft_num_predict` would reload the shared model. `num_ctx` and
`num_predict` are refused inside it because slbh derives them, so each is
stated once. A request's own temperature beats the block's.

On `/api/chat`, `think: "low" | "medium" | "high"` renders the same prompt as
`/v1`'s `reasoning_effort` at that level; an omitted `think` takes the model
template's default, which on the served family is its top rung. The template
has no `xhigh` or `max` rung — Ollama refuses `"xhigh"` with a 400 and the
template raises on `"max"` with a 500 — so the managed policy maps both onto
`high`, and a policy that does not is loud rather than silently clamped.
Replayed transcripts on this wire carry no cache key, because Ollama has no
field for one: its prompt cache is automatic prefix reuse, reported as
`cached_tokens` in usage.

Everything a wire changes is normalized before it leaves the provider package,
so nothing downstream knows or cares which one served a request. Three of those
normalizations are worth naming because the naive version of each is silently
wrong:

- **Usage.** `input_tokens` on the Messages wire is net of cache where
  `prompt_tokens` is gross, so `prompt_tokens` is reported as
  `input_tokens + cache_read_input_tokens`. Mapping it straight across would
  have told the harness a 5,550-token context was 46 tokens, and automatic
  compaction would never have fired on a cached conversation while everything
  still appeared to run. `reasoning_tokens` is *absent* on that wire rather
  than zero, because the wire reports no equivalent and a synthesized zero
  would hide exactly what the record exists to reveal.
- **Tool indexes.** The Messages wire's index is a content-block ordinal, so
  tools begin at 1 behind the thinking block. They are renumbered to a dense
  ordinal from 0, and nothing downstream may treat the wire index as an array
  position.
- **Tool arguments** arrive fragmented on the Messages wire and whole on the
  other two. Both are handled by accumulation, so no test asserts one chunk
  per call. On `ollama-chat` they arrive as a JSON object and are forwarded as
  its JSON text, and history sends them back as an object.
- **Stop reasons** are reported in the coding wire's vocabulary. Ollama
  reports `done_reason: "stop"` for a turn that ended in tool calls, so on that
  wire the calls decide it and the turn reports `tool_calls`; `length` passes
  through. Its usage maps `prompt_eval_count`, which is gross, onto
  `prompt_tokens`, and `prompt_eval_cached_count` onto `cached_tokens`.

Errors are classified by status class over four envelopes — the coding wire's
`{"error":{...}}`, the Messages wire's `{"type":"error",...,"request_id"}`,
an HTTP 422 FastAPI `{"detail":[...]}` for a schema violation on that same
wire, and Ollama's bare-string `{"error":"..."}`. A 4xx is not retried and a 5xx is. A 429 is inside the 4xx rule
deliberately: a quota refusal should surface at once rather than be spent three
times over. `request_id` is preserved in the error text, since it is the only
handle the provider gives for a support question.

An in-stream `error` frame is classified the same way. The Messages dialect can
raise one after the stream has already opened — observed live under overload on
2026-09-14 — and such a frame carries no status of its own, so its `type` is
mapped onto the status the same condition carries as a pre-stream refusal:
`overloaded_error` and `api_error` are retried, `rate_limit_error` is not, and
an unrecognised type defaults to the retryable side, which is what an error
carrying no classification at all already gets.

Two refusals are deliberate and worth knowing about:

- **An effort level the route cannot express refuses the request**, naming the
  route, the level asked for and what the route supports. It is never dropped
  and never walked down to the nearest supported level, because either would
  let a benchmark record results at an effort nobody configured.
- **A route configured for a wire this build does not implement refuses**,
  naming the wire. A policy is deployed by one mechanism and a binary by
  another, so the two can arrive in either order; refusing is the only safe
  reading, since sending one wire's conversation down another's encoder is
  something these endpoints will answer 200 to.

These environment variables are read at startup:

| Variable | Default | Purpose |
| --- | --- | --- |
| `SLBH_HOME` | `$HOME/.slbh` | Base directory for configuration, history, and runtime records. |
| `SLBH_MODEL` | `zai/glm-5.3-flash` | Seat agent model. |
| `SLBH_EFFORT` | `medium` | Seat reasoning effort. |
| `SLBH_INTERN_MODEL` | `local/q27-UD-Q2_K_XL-64k` | Intern model. |
| `SLBH_INTERN_EFFORT` | `low` | Intern reasoning effort. A route's policy `defaultEffort` takes precedence. |
| `SLBH_SUBAGENT_MODEL` | `zai/glm-5.3-flash` | Default child-agent model. |
| `SLBH_LEAF_MODEL` | `local/q27-UD-Q2_K_XL-64k` | Legacy child setting retained for config compatibility; depth-two launches require an explicit model. |
| `SLBH_SUBAGENT_EFFORT` | `medium` | Default child-agent effort. |
| `SLBH_PYTHON` | managed `~/.local/share/slbh/python` interpreter | Python interpreter used by the `python` tool. |
| `SLBH_BASH` | host Git Bash detection | Optional Windows Git Bash executable override. |
| `SLBH_ENDPOINT` | OpenRouter chat-completions endpoint | Compatible provider endpoint. |
| `SLBH_LOCAL_ENDPOINT` | `http://fractal.wyvern-temperature.ts.net:11434/api/chat` | Desktop Ollama endpoint. It must match the route's wire: an `ollama-chat` route needs an `/api/chat` URL. |
| `SLBH_FRAME_PROFILE` | unset | Optional CSV path for TUI `Update` and `View` durations, frame gaps, message types, and retained event counts. |

The model policy is persisted at `$SLBH_HOME/config.json`. `/models` always
shows the configured local Ollama model and refreshes catalogs for providers
whose keys are present. The OpenRouter catalog is limited to models created
within the last year. Model and effort settings from the environment take
precedence over their persisted counterparts.

For native provider agents, the harness estimates context usage before a
request and compacts history at 70% of the active model's context window. That
window is resolved in three steps: a pinned window for the route if it has one,
otherwise the model's discovered context length, otherwise a 128,000-token
fallback. A route is pinned when its provider catalog cannot report a length —
`zai/glm-5.3-flash` and `zai/glm-5.3-flashx` are pinned at 1,000,000 tokens by
the synchronized policy because neither Z.ai catalog publishes a context
length. A pin therefore beats discovery as well as the fallback. The pin comes
from the routing policy above, and from the resolved provider instance rather
than from a lookup by model name: only the instance knows which endpoint the
request will really reach, so a name-keyed pin could size the window for a
route this request is not taking.
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
Agent output in chat blocks renders as markdown; the owner's own input and
non-chat blocks stay literal, because tool results and payloads are not
markdown. A streamed message re-renders at most once every 100ms, so a long
response styles itself as it arrives without the render cost growing with its
length. The viewport keeps a bounded recent display window for responsiveness;
the complete transcript remains available through the runtime transcript.

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

Mouse reporting is off, so the terminal keeps click and drag and text in the
TUI can be selected and copied the usual way. `/mouse` turns capture on when
wheel scrolling is wanted; the status line then shows `mouse`, and selecting
text takes Shift-drag until it is turned back off.

Input history is stored as JSON lines in `$SLBH_HOME/history`, normally
`~/.slbh/history`, and is shared by launches using the same home directory.

## Slash commands

| Command | Action |
| --- | --- |
| `/exit`, `/quit`, `/q` | Stop agents and jobs, close transcripts, and exit. |
| `/clear` | Clear the selected agent and start a new session. |
| `/models` | Refresh provider catalogs and open the model menu. Shows which routing policy is in force and from where; `p` authors a local one. |
| `/model NAME` | Approve and select a model for the seat agent. |
| `/effort LEVEL` | Change the seat agent's effort. |
| `/agents` | Record the current agent tree as a status event. |
| `/jobs` | Record current jobs as a status event. |
| `/compact` | Compact the selected agent's history. |
| `/mouse` | Toggle mouse capture: off leaves text selectable, on adds wheel scrolling. |

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
- Jobs: `bash` and, on Windows, `pwsh`; `python` uses the managed scientific
  environment. `job` lists, reads, or kills jobs with its `action` field.
- Agents: `list_subagents`, `launch_subagent`, `msg_subagent`, and
  `end_subagent`.

### Tool shapes

The files and shell tools above are the default `slbh` **tool shape**. Two
other shapes replace just that primary set with the tools another harness
gives its model, and leave everything else unchanged: `python`, `pwsh`, `job`,
the agent tools and the prompt, apart from the sentence naming the command
tools.

| shape | primary tools | reproduces |
| --- | --- | --- |
| `slbh` (default) | `glob`, `grep`, `read_file`, `read_bytes`, `read_lines`, `edit_file`, `apply_patch`, `write_file`, `bash` | this harness |
| `anthropic` | `Bash`, `Read`, `Edit`, `Write` | Claude Code 2.1.278 |
| `codex` | `exec_command`, `write_stdin`, `apply_patch` | Codex CLI 0.155.1 |

Select a shape with `tool_shape` in `config.json`, the `SLBH_TOOL_SHAPE`
environment variable, or `--tool-shape`. Each one overrides the one before
it, and `Save` never writes an override back. An unknown value stops slbh at
startup with exit status 2; there is no fallback. The shape is fixed for the
runtime's life and applies to the Seat and every native child. Codex and
Claude Code leaves bring their own tools, and the Intern keeps its read tools.

A shape reproduces its harness's names, parameter schemas and result text, as
captured from the installed builds in `internal/harness/testdata/toolshape/`
and pinned by tests. Descriptions are condensed where the original carries
protocol that means nothing here, such as Claude Code's git and PR
instructions. Where each shape departs from its harness:

- `anthropic`: `Read` applies the 2000-line default its description states,
  and `offset: 0` numbers from 1. Read-before-edit is not enforced, as in
  2.1.278. An edit to a file that changed since it was read notes that it
  did. `Bash` keeps its working directory inside the agent's tree, and a
  timeout kills the command with `Exit code 143`. Background commands are slbh
  jobs: read them with `job` and receive their results automatically.
  Failures carry `is_error` on the Anthropic wire.
- `codex`: stderr is merged into stdout; output is truncated at
  `max_output_tokens`, keeping head and tail. A command still running after
  `yield_time_ms` returns a session id. `write_stdin` polls that session and
  cannot write input, because slbh jobs have no stdin. GLM speaks JSON
  functions, so `apply_patch` is a one-string function carrying Codex's
  patch grammar in its description, where Codex uses a freeform grammar tool.
  A patch is verified whole before any file is written.

Every `tool_result` event records the tool `name` and an `error` flag. A
command tool also records its `job`, a `job_state` (`finished`, `background`
or `killed`) and, once the command ends, its `exit_code`. The handlers record
these from the process, not from rendered text. `error` uses one definition
in every shape: the tool failed, a command it ran finished non-zero or was
killed, or a patch or session call did not succeed. Codex itself reports a
non-zero exit as ordinary output, so without the shared definition, error
counts would not compare across shapes. A backgrounded command's outcome
arrives later as a `job_result` event with the same `job` id, so an analysis
counts each command's outcome once per job. Every execution is preceded by a
`tool_start` event, so a call killed before it returns is still on record. A
completed request emits `request_done` and a failed attempt emits
`request_error`. A stream that breaks after reporting usage still emits that
usage, and any usage lacking its input or output count is marked
`incomplete`.

Headless also answers `quiescent`: `true` when every job has finished and been
delivered and no agent has a turn running, an undelivered message, or a
non-idle status. It also returns the last event cursor. A caller that must be
sure the runtime is done asks twice, apart, and requires both answers true
with the same cursor.

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
error first and bounded. This matters most for `bash` and `python`: a non-zero exit
is routinely informative rather than fatal — `grep` exits 1 when it matches nothing, `test`
exits 1 on false, a failing suite exits 1 — so an agent given only the exit code cannot tell
"no matches" from "command not found" and retries blind.

File arguments may be absolute or relative; relative paths resolve against the
active agent's working directory. slbh does not add a filesystem permission
boundary, so the operating system determines whether a requested path or
working directory is usable. Reads and job output are bounded. `bash`, `pwsh`,
and `python` start a job, wait up to `wait_seconds` (default five seconds), and
background it if it is still running; `wait_seconds: 0` backgrounds immediately.
Jobs and agent activity are recorded in the
active session transcript.

A background job has exactly one timer, `warn_after_seconds`, and it warns
exactly one party: the agent that started the job. When it elapses with the job
still running, that agent receives a message by the same mandatory mid-turn
path as every other message, which wakes it if it has gone idle. The agent then
decides — kill the job, keep waiting for its result, or get on with other work
— and there is deliberately no fallback behind that decision. The warning is
never repeated, nothing escalates it, and no turn blocks on an outstanding job.
The human operator is not the audience: the operator does not create these jobs
and never sees the ones a subagent runs, so telling a person is the control
agent's duty rather than the harness's. The TUI still shows a `job_warning`
event, as a record of the firing and not as the delivery.

A running job's output is readable. `job` with `action: "read"` returns whatever the job has
captured on each stream at the instant it is asked, finished or not, so a warned
agent can look at the job before deciding what to do about it. The capture
buffers are the command's own sinks and carry their own mutex; until
2026-09-15 they were filled only after the process exited, and `job read`
answered a live job with two empty strings.

## Headless

Headless is the persistent runtime frontend. It creates the same runtime, Seat, tools,
event stream and child lifecycle as the TUI, then speaks a JSONL request/notification
protocol over stdin/stdout. The process remains alive for multiple turns until the client
sends `close`, stdin closes, or it receives a termination signal. The TUI is only a
presentation frontend; no runtime capability depends on it.

```sh
slbh --headless --intern
slbh -p "start with this Seat prompt" --model zai/glm-5.3-flash
slbh --headless --prompt-file brief.md --workdir /srv/project
```

Requests are JSON-RPC-shaped. Commands use the seam command names and fields:

```json
{"jsonrpc":"2.0","id":1,"method":"send_prompt","params":{"agent_id":"agent-...","prompt":"continue"}}
{"jsonrpc":"2.0","id":2,"method":"agents","params":{}}
{"jsonrpc":"2.0","id":3,"method":"close","params":{}}
```

Responses carry `result` or `error`; runtime events are asynchronous
`{"jsonrpc":"2.0","method":"event","params":{...}}` notifications. The available
commands are the closed seam command set (`send_prompt`, `steer_agent`, `clear`, `compact`,
model configuration, local-policy authoring, status and `close`). Queries expose agent/job
snapshots, runtime metadata, transcript paths, instruction/skill/policy sources, model
catalog/guidance and cursor-based event polling.

`-p` and `--prompt-file` provide an optional initial Seat prompt and imply headless mode;
later turns use `send_prompt`. `--intern` works with either frontend and starts the same
runtime watcher. Running with no frontend flags starts the TUI.

Embedders call `headless.Serve(ctx, runtime, stdin, stdout, headless.Options{...})` and retain
the same seam rather than selecting a separate one-turn implementation.


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
                ├── jobs/
                │   └── job-.../script.sh (or script.ps1/script.py)
                └── scratch/
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
gofmt -w cmd/slbh/main.go internal/config/*.go internal/harness/*.go internal/headless/*.go internal/id/*.go internal/job/*.go internal/logx/*.go internal/provider/*.go internal/runtimeapp/*.go internal/tui/*.go
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
