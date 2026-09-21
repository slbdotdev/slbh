# slbh for org v0.3

Live on `main` since the org's v0.3 cutover on 2026-09-18; the owner's final
acceptance is pending. The companion is `SKILL.md` in
`slb-org`, which specifies the organization this serves; where the two
disagree, that one is about the org and this one is about the program.

State of the tree, with evidence, is section 10. Open questions are 11.

## 1. Role

slbh is the org's runtime. It is not a fourth managed harness and not a
skill: the fleet's harnesses are tools slbh drives.

It runs the Seat, it runs the Intern, and it launches every leaf. Under
v0.3 the org's controller lives here.

It runs in tmux on the devbox and is reached over ssh. It needs no remote
control and therefore no daemon.

## 2. Agents

The runtime has a root agent at depth 0 and may delegate through depth 1 to
depth 2. Native agents below depth 2 may launch one child; a depth-2 child
launches nothing. There is no named-role catalogue and no configured launcher
relationship.

Each child chooses an optional harness (`native`, `codex`, or `claude_code`)
and a model. Depth 1 may use the configured child default. Depth 2 must carry
an explicit real model string or a short name resolved by the managed
`$SLBH_HOME/models.toml` map. When effort is omitted, route `defaultEffort`,
the model alias's effort, and the configured child effort are considered in
that order.

`AgentSnapshot` reports title, depth, harness, model and effort; it does not
expose a role identity. The Intern remains outside the tree and uses its
read-only runtime frontend.

## 3. The seam

slbh has one runtime and several front ends. That is already its shape;
v0.3 makes the boundary between them explicit, because a new consumer — the
Intern's watcher — is a front end on the same runtime.

Three rules define the seam:

1. **Serializable payloads.** Every type crossing it marshals to JSON: no
   channels, no functions, and no live pointers into runtime state.
2. **A closed command set inbound.** Named commands, enumerable and
   loggable, rather than an open method surface.
3. **One event stream outbound.** A single ordered, typed stream that every
   front end consumes. There is not a second way to learn what happened.

Reads are the third shape and are neither a command nor an event. A query
returns a copy: `Agents() []AgentSnapshot`, `JobSnapshots() []JobSnapshot`.
`Jobs()` and `Seat()` are replaced by those, and by named commands for what
they were reached through to change. No caller receives a live pointer into
another subsystem.

The seam is in-process for the TUI and the Intern, and headless exposes the
same seam as a persistent JSONL app-runtime protocol over stdin/stdout. There
is still one Go binary and one runtime: the transport is only marshalling,
not a second harness or daemon. A headless process stays alive for multiple
commands and emits the runtime's ordered event stream as notifications.

The test of whether the seam is real: the TUI, headless and the Intern's
watcher can each be written against it while importing nothing else from
`internal/harness`.

Codex's app-server is the model for the *decomposition* and the persistent
frontend lifecycle, not a vocabulary requirement. slbh remains a client of
that protocol for Codex leaves and a server of its own JSONL runtime protocol;
the two do not have to share method names. slbh's own nouns — depth, layer
instructions, routing policy, local routes — have no Codex equivalent.

## 4. Leaves

A deeper child's model and effort are chosen **per launch**. The runtime's
depth-one child default is a fallback only at depth one, not the only value
available. A short name is only an alias for a real model string; it is not an
agent identity.

**A launch always states the model explicitly.** Codex's app-server
`thread/start` inherits a model when the caller omits one, so an omission
does not fail — it silently runs the wrong agent. slbh asserts the model on
every leaf launch rather than relying on the default.

Three leaf kinds:

- **Codex**, over the app-server, as today.
- **Claude Code**, headless, when the launch selects `claude_code`. It uses
  the host's Claude login and the model supplied by the launch.
- **slbh's own provider path**, when the launch selects `native`. The model
  is supplied by the launch or the depth-one configured default.

A leaf receives the managed leaf document appended to the harness-specific
mechanics slbh supplies, as the Codex leaf already does.

## 5. The Intern

The Intern reads the Seat's event stream turn by turn and asks the Seat a
question when it believes the Seat is making a mistake.

- It is a front end on the runtime, not an agent in the Seat's tree. The
  depth cap does not apply to it and it launches nothing.
- The owner starts it inside the Seat's slbh process. It cannot run as a
  separate invocation, because a separate invocation cannot see live
  runtime state (§7).
- Having no depth, it takes no layer document by depth. slbh loads
  `instructions/intern.md` by name.
- Its tools are **read-only**, supplied by slbh as a harness feature rather
  than assembled per run: `glob`, `grep` and the file reads, plus the
  runtime's own state — agent snapshots, job states, the event stream.
- **Nothing executes.** There is no shell, whitelisted or otherwise. A
  restricted shell is a convention wearing a mechanism's clothes on an
  unsandboxed fleet, and the Intern's one safety property is that it cannot
  act. Reaching the runtime's state directly is both better evidence than a
  command and no capability at all.
- It asks. It does not instruct, direct, correct, or assert. It has no
  authority, which is the property being bought and not a limitation.
- Its questions are not rationed and the Seat's answers are not bounded.
  The guard is the register rather than a counter.
- It never blocks. The Seat answers briefly and proceeds, and a question
  the Seat overrides is a question that worked.
- It runs on the local quant, so reading every turn costs nothing and the
  Seat's full transcript never leaves the estate.

The Seat's leaves are Codex, and the Codex leaf already pumps into the same
event stream. The Intern therefore reads Seat turns and leaf turns in one
vocabulary.

## 6. Messages

**The agent inbox** is the one inbox: per agent (`agent.go`), in memory,
carrying steers and child results, and `job/manager.go` uses it to wake an
idle agent. It is runtime-scoped and dies with the run.

The durable org inbox and request queue, which carried the Seat's reports
to the Secretary and the Secretary's requests back, were retired with the
Secretary: `internal/orgstore`, the `slbh inbox`, `ack`, `request` and
`requests` subcommands, the Codex wake, and the Seat's
`report_to_secretary`, `org_requests` and `update_request` tools are gone.
Files a past run left under `$SLBH_HOME/org` are inert.

## 7. Separate invocations

slbh has no subcommands: every invocation is a front end on its own
runtime. A separate invocation therefore cannot see another's live runtime
state, and nothing needs it to. The Intern observes from inside; the owner
observes at the tmux session. Should that ever change, §3's rules make a
transport a marshalling layer rather than a redesign — which is why they
hold even with no transport in sight.

## 8. Documents and ownership

- `$SLBH_HOME/policy.json` and
  `$SLBH_HOME/instructions/{seat,manager,leaf,intern}.md` are org content. They live in `slb-org` and reach hosts by its sync.
- `$SLBH_HOME/models.toml` is optional org content containing short names and
  their real model strings. It has no agent or launch-role data.
- `$SLBH_HOME/skills/{seat,manager,leaf,intern}/<skill>/` is org content
  too. The sync fills each depth layer's directory with the complete skill
  catalogue; slbh does no name filtering.
- `$SLBH_HOME/config.json` stays application-owned. slbh writes it.

**Skills.** An slbh agent's skills are the directory for its layer, chosen by
depth, and the Intern's directory by name. slbh does no filtering.

- At prompt assembly slbh lists each skill in that directory: its name, the
  `description` from the front matter of its `SKILL.md`, and the absolute
  path to it. It lists no bodies. An agent reads a skill with the file
  tools it already has when a task matches. There is no skill tool.
- The front matter is the `---` block at the head of `SKILL.md`. slbh reads
  `name` and `description` from it, and nothing else. A description may be
  one line or a folded `>-` block.
- A missing directory or an unreadable skill degrades exactly as a missing
  instruction document does. The agent runs without it, and the source
  record says why.
- A Codex or Claude Code leaf takes no slbh skills. Its own harness loads
  the skill directory the sync gave it.
- The managed policy wins over local policy. With neither, slbh refuses to
  route.
- The **binary** is deployed by `ansible-slb`, cross-built on the
  controller. Binary deployment is what remains Ansible's.

## 9. Constraints carried from the org

- No key, token or vault content enters a prompt, a log, or a transcript.
- Codex uses the host's own login. `~/.codex/auth.json` is never copied and
  `CODEX_API_KEY` is never deployed.
- A model reachable on a plan runs through its native harness.
- A review does not go to its author's own model family where another
  family can do it.
- Commits are the owner's, signed, one line, one to five lowercase words.
  No agent is a contributor.

## 10. State of the tree

Measured at `e672775` on `main`, into which `v0.3-draft` merged at
`ac58fab`. The code-bearing delta is built, one unit per commit, each checked with `gofmt`, build, vet, the full suite and the
race detector. The current tip also records the depth-aware TUI fixture
migration used by the runtime's delegation contract:

| unit | commit | what it delivers |
| --- | --- | --- |
| Codex leaf effort | `f6a29b8` | effort reaches a Codex leaf on every `turn/start` |
| queries return copies | `6cb7af1` | JSON-tagged snapshots, `JobSnapshots()`, no `Jobs()`/`Seat()` |
| the closed command set | `c511112` | `internal/seam`, eleven named commands through `Do`, persistent headless protocol on the seam; the event stream closes on Close |
| TUI on the seam | `3265a3a` | the TUI imports only the seam; duplicate `Runtime` methods unexported |
| Claude Code leaf | `adcec0e` | `claude_code` harness for Opus, plan-billed with the API-key variables scrubbed; live round trip passed |
| durable org store | `0aa7b5f` | `internal/orgstore`: append-only inbox and request queue, flock, fsync, atomic cursor, torn-line repair |
| subscribers and read tools | `246ac1e` | `Subscribe()` fan-out; `internal/readtools`; `intern.md` loaded by name |
| the Intern | `7a06ec0` | `internal/intern` behind the shared runtime frontend lifecycle and `--intern`, local routes only, nine tools with `ask_seat` its one move |
| Secretary subcommands | `ed29397` | `slbh inbox`, `ack`, `request`, `requests`, `help`, with `--json`, no runtime constructed |
| the one local tag | `4297deb`, `8820bc5` | leaf and Intern default `local/q27-UD-Q2_K_XL-64k` |
| Secretary wake | `be09a05` | `internal/secretarywake` over `codex queue --thread <name>`; live wake of a real TUI passed |
| wiring | `75cd5bb` | Seat-only `report_to_secretary`, `org_requests`, `update_request`; request watcher; Intern effort `medium`, output bound, token-budgeted prompt; end-to-end test |
| the seat runs glm | `c1fe614` | the Seat's compiled default is `zai/glm-5.3-flash` |
| skill loading | `cc030fc` | layer skill metadata loads from `$SLBH_HOME`; native agents and the Intern receive names, descriptions and absolute `SKILL.md` paths without bodies |
| JSON-safe polling seam | `5d64de6` | `PollEvents(EventQuery) EventBatch` replaces live subscription channels; TUI, headless and Intern consume cursor-based event batches |
| role-aware runtime | `c70e239` | historical identity implementation, retired by the depth-only launch contract |
| runtime documentation | `d7042f1` | historical v0.3 identity documentation, superseded by model aliases and depth-only delegation |
| role-aware test fixtures | `7989857` | historical fixtures, superseded by role-free launch tests |
| subagent stall warnings | `ce9c1ca` | `launch_subagent.warn_after_seconds` defaults to five seconds, warns the parent once through its inbox, and cancels on child completion, error, stop or runtime shutdown |
| persistent headless | `a397d17` | `slbh --headless` is a long-lived JSONL protocol over the seam: initialize, prompts, polled events and `close`, with the Intern inside the same runtime |
| job output cap | `0e0bd76` | each job stream keeps at most 4 MiB and consumes the rest, so the cap holds past the first full write and the child is never blocked on a short write |
| live headless test | `de7174d` | the live check judges the streamed reply whole at `turn_done`; passed on `zai/glm-5.3-flash` and `local/q27-UD-Q2_K_XL-64k` |
| Windows test build | `4a3089f` | the POSIX process-group test builds on POSIX alone, so `GOOS=windows go vet ./...` passes |
| Codex final answer | `89101df` | a Codex leaf returns its turn's `final_answer` message, read with the turn id the app-server sends beside the item; commentary no longer runs into the result |
| native Ollama wire | `e672775` | `ollama-chat` speaks `/api/chat`: NDJSON stream, effort as the `think` string, a policy `options` sampler block sent verbatim and refused on other wires or for unknown keys, `num_ctx` and `num_predict` from `contextWindow` and `maxOutputTokens`; the compiled local default moves to it. An output-limit stop on any wire raises a `warning` event into the transcript. Live against Ollama 0.34.1: reply ends `stop`, tool call round-trips, a 16-token bound ends `length`, and a seeded A/B shows `presence_penalty` reaches the sampler |

On branch `tool-shape`, not yet merged: the **tool shape** toggle. `tool_shape`,
`SLBH_TOOL_SHAPE` or `--tool-shape` selects `slbh` (the default, whose
definitions are pinned byte for byte against `main`), `anthropic` (Claude
Code 2.1.278's `Bash`/`Read`/`Edit`/`Write`) or `codex` (Codex 0.155.1's
`exec_command`/`write_stdin`/`apply_patch`). A shape replaces only the primary
tools. Captures of both harnesses, and the pins against them, live in
`internal/harness/testdata/toolshape/`. An unknown shape refuses to start.
Tool results carry a uniform `error` flag and `exit_code`. See the README's
*Tool shapes*.

`internal/tui`, `headless`, `seam`, `readtools` and `intern` import
nothing from `internal/harness`. The org store, the Secretary subcommands,
the wake and the Seat's org tools in the table above have since been
removed with the Secretary (§6).

**Run as the org** on 2026-09-18, recorded in `slb-org`
`org/v03-cutover-readiness-2026-09-18.md`: a headless GLM Seat launched a
native Manager and a Sol Codex leaf, reported to a live named Secretary that
was woken, drained its inbox once and filed a request the Seat carried to
done, and the Intern watched that Seat and asked it a live question.

## 11. Open

No design question is open. One operational risk is unmeasured rather than
unanswered, and what remains of it is the model's, not slbh's:

- Local termination. slbh's half is closed at `e672775`: the local route
  speaks Ollama's native `/api/chat`, so its sampler guard — the GGUF's own
  `temperature 1.0`, `top_k 20`, `top_p 0.95` plus `presence_penalty 1.5` —
  reaches the sampler instead of being dropped by the `/v1` shim; every
  generation is bounded at 32,768 tokens by `num_predict`; and a generation
  that hits the bound raises a `warning` into the transcript. Whether the
  served tag *terminates* under that guard is not measured: as served over
  `/v1` it failed to close its thinking block in 3 of 10 runs at `high`, the
  guard has never been benched on `/api/chat`, and the termination bench is an
  owner-ruled v8 campaign item that stays open. Nothing detects a loop before
  the bound; the warning makes a runaway visible, it does not end it sooner.

Codex auto-compaction on the app-server path slbh uses was measured on
2026-09-18 with codex-cli 0.155.0: with `model_auto_compact_token_limit` set
to 15,000 on the thread, two `contextCompaction` items fired mid-turn and the
context fell back each time, and every turn completed. The production
threshold is 700,000 and no leaf has yet run that deep.
