# slbh for org v0.3

**Draft.** On `v0.3-draft`. Nothing here is implemented by this document
being written, and `main` remains what runs. The companion is `SPEC.md` in
`slb-org`, which specifies the organization this serves; where the two
disagree, that one is about the org and this one is about the program.

State of the tree, with evidence, is section 11. Open questions are 12.

## 1. Role

slbh is the org's runtime. It is not a fourth managed harness and not a
skill: the fleet's harnesses are tools slbh drives.

It runs the Seat, it runs the Intern, and it launches every leaf. Under
v0.3 the org's controller lives here.

It runs in tmux on the devbox and is reached over ssh. It needs no remote
control and therefore no daemon.

## 2. Agents

| Agent | Depth | Harness under it | Model |
| --- | --- | --- | --- |
| Seat | 0 | native provider | `zai/glm-5.3-flash` |
| Manager | 1 | native provider | `zai/glm-5.3-flash` |
| Luna | 2 | Codex leaf | `gpt-5.6-luna` |
| Sol | 2 | Codex leaf | `gpt-5.6-sol` |
| Opus | 2 | Claude Code leaf, headless | `claude-opus-5` |
| Flex | 2 | native provider | named at dispatch |
| Intern | outside the tree | native provider | `local/q27-…` |

All three rungs are occupied. `ConfigureModelSlots`' seat, subagent and
leaf slots map onto them exactly, and `instructions/manager.md` has a
reader.

A leaf launches nothing; a Manager may. The existing cap at
`parent.Depth >= 2` is already correct and is not relaxed.

Several Managers run concurrently against the Z.ai plan alongside the Seat.
That concurrency has been tested and holds.

## 3. The seam

slbh has one runtime and several front ends. That is already its shape;
v0.3 makes the boundary between them explicit, because new consumers — the
Intern's watcher, and the org inbox's write side — are front ends on the
same runtime.

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

The seam stays **in-process**. No listener, no wire protocol, no daemon.
The rules exist so that adding a transport later is a marshalling layer
rather than a redesign — not because a transport is planned.

The test of whether the seam is real: the TUI, headless and the Intern's
watcher can each be written against it while importing nothing else from
`internal/harness`.

Codex's app-server is the model for the *decomposition* and not for the
vocabulary. Its method names are not copied. slbh is a client of that
protocol and a server of its own; the two do not have to look alike, and
slbh's own nouns — depth, layer instructions, routing policy, local routes
— have no Codex equivalent.

## 4. Leaves

A leaf's model and effort are chosen **per launch**. The runtime's leaf
default is a fallback for a launch that names none, not the only value
available. Luna and Sol are the same harness at different models, so the
roster does not exist unless the launch can say which.

**A launch always states the model explicitly.** Codex's app-server
`thread/start` inherits a model when the caller omits one, so an omission
does not fail — it silently runs the wrong agent. slbh asserts the model on
every leaf launch rather than relying on the default.

Three leaf kinds:

- **Codex**, over the app-server, as today.
- **Claude Code**, headless, for Opus. `ANTHROPIC_API_KEY` is not exported
  on this fleet, so a headless Claude Code leaf runs on the host's
  claude.ai login and is plan-billed. It must stay that way.
- **slbh's own provider path**, for Flex. This needs no integration: the
  routes are already in `policy.json`. Flex names its model at dispatch
  rather than carrying one, and that model is never the author's family.

A leaf receives the managed leaf document appended to the harness-specific
mechanics slbh supplies, as the Codex leaf already does.

## 5. The Intern

The Intern reads the Seat's event stream turn by turn and asks the Seat a
question when it believes the Seat is making a mistake.

- It is a front end on the runtime, not an agent in the Seat's tree. The
  depth cap does not apply to it and it launches nothing.
- The owner starts it inside the Seat's slbh process. It cannot run as a
  separate invocation, because a separate invocation cannot see live
  runtime state (§8).
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

Two different things are called an inbox and must not be conflated.

**The agent inbox** exists: `agent.go:46`, per agent, in memory, carrying
steers and child results, and `job/manager.go:34` uses it to wake an idle
agent. It is runtime-scoped and dies with the run. Nothing about it
changes.

**The org inbox** is new. It is durable, survives the process, and carries
the Seat's reports to the Secretary. It:

- accrues while the Secretary is detached and is intact when it returns,
- is append-only,
- is drained by **acknowledgement, not by reading**, through a cursor, so a
  Secretary that dies mid-drain loses nothing and repeats nothing.

A Seat report names the org page its change invalidates, or states that
none is.

Both the org inbox and the Secretary's request queue are files under
`$SLBH_HOME`. slbh owns their format and their cursors; nothing else parses
the shape on disk. The running Seat and a separate `slbh` invocation both
touch them, so append-only writes and an atomic cursor update are load
bearing rather than incidental.

Request status is written back into the queue as the Seat works it. That is
deliberate: anything the Secretary needs to know is made durable rather than
made reachable in memory.

## 7. Reaching the Secretary

The Secretary is a Codex session, not an slbh agent. MCP cannot start its
turn: notifications reach the Codex process and stop at its logging
handler, and elicitation reaches the owner rather than the model. Both were
measured on 2026-09-17.

So slbh wakes the Secretary as a **client of Codex's app-server**, through
the queue method, addressing the session by a stable name. This is the same
connection kind slbh already makes for its Codex leaves and adds no new
dependency class.

The wake is measured, not assumed. On 2026-09-18 a queued message started a
turn in an idle session **43 ms** after the queue command returned. Queued
mid-turn it did not interrupt: it waited for the active response and was
taken up as soon as that turn completed.

The Secretary's session runs on the devbox, beside the Seat.

A session is addressed by UUID or by an exact name, and a name is assigned
with `/rename` **after the session's first turn** — there is no launch-time
flag for it. So the Secretary's session is named once at startup, and the
Seat addresses that name rather than hunting for the newest session, which
is not an identity test.

## 8. The Secretary's interface

`slbh` subcommands, invoked from the Secretary's shell. Read the inbox,
acknowledge it, request work. There is no MCP server and no IPC.

The Secretary is a Codex session with full shell access, and slbh is a
binary on the same machine. MCP's only remaining job would have been pull,
which a command does — at the price of a server, a transport, a lifecycle
and a capability surface, for an agent whose instruction document can simply
name the commands.

**A subcommand does not construct a `harness.Runtime`.** It is file work
against `$SLBH_HOME`, so the inbox and queue code must be usable without
`internal/harness`. That is a package-dependency rule, not a process
boundary, and it is what keeps the TUI and the CLI one binary.

A separate invocation therefore cannot see live runtime state, and nothing
needs it to. The Intern observes from inside; the owner observes at the tmux
session. Should that ever change, §3's rules make a transport a marshalling
layer rather than a redesign — which is why they hold even with no
transport in sight.

## 9. Documents and ownership

- `$SLBH_HOME/policy.json` and
  `$SLBH_HOME/instructions/{seat,manager,leaf,intern}.md` are org content. They live in `slb-org` and reach hosts by its sync.
- `$SLBH_HOME/config.json` stays application-owned. slbh writes it.
- The managed policy wins over local policy. With neither, slbh refuses to
  route.
- The **binary** is deployed by `ansible-slb`, cross-built on the
  controller. Binary deployment is what remains Ansible's.

## 10. Constraints carried from the org

- No key, token or vault content enters a prompt, a log, or a transcript.
- Codex uses the host's own login. `~/.codex/auth.json` is never copied and
  `CODEX_API_KEY` is never deployed.
- A model reachable on a plan runs through its native harness.
- A review does not go to its author's own model family where another
  family can do it.
- Commits are the owner's, signed, one line, one to five lowercase words.
  No agent is a contributor.

## 11. State of the tree

Measured at `f6a29b8` on `v0.3-draft`. Non-test Go lines: `internal/harness`
3,256; `provider` 2,597; `tui` 1,794; `config` 477; `job` 555; `headless`
190. With tests: 7,713; 4,835; 2,895; 1,035; 949; 501.

**Already true:**

- One runtime, two front ends, in-process. `tui.go:140` takes
  `*harness.Runtime`; `headless.go:68` takes the same. `headless.go:8` says
  it outright: "a different front end, not a second harness."
- The outbound stream is already protocol-shaped. `runtime.go:21` defines
  `Event` with complete JSON tags; `runtime.go:158` exposes
  `Events() <-chan Event`. `Agents()` already returns snapshots.
- The Codex leaf exists and drives the app-server.
- A leaf's harness, model and effort are chosen per launch: `LaunchSpec`
  (`runtime.go:47`) carries all three, and a Codex launch with no model is
  refused. Effort reaches a Codex leaf on every `turn/start` since
  `f6a29b8`. `ConfigureModelSlots` sets only the fallbacks.
- Layer instructions resolve by depth: `config/instructions.go` loads
  seat, manager and leaf.
- An idle agent can be woken by delivery: `job/manager.go:34`.
- A flag-driven headless mode exists (`cmd/slbh/main.go`); it is not the
  subcommand path §8 needs.

**Not yet true:**

- `AgentSnapshot` (`runtime.go:31`) carries the right fields — `Depth`,
  `Model`, `Effort`, `Status`, `Harness`, `ContextWindow`, `ContextUsed` —
  and **no JSON tags**. It is the type the Intern wants. There is no
  `JobSnapshots()`.
- `Jobs() *job.Manager` (`runtime.go:159`) and `Seat() *Agent`
  (`runtime.go:296`) hand out live pointers into other subsystems. These
  are holes through the seam and cannot cross a transport.
- The inbound surface is not a closed command set. `Runtime` exports 28
  methods mixing accessors, tool execution and config mutation, and the TUI
  and headless front ends import `internal/harness` directly.
- The Seat's default model is `deepseek-v4-flash` (`config/config.go`), not
  the roster's `zai/glm-5.3-flash`.
- No Claude Code leaf. `internal/provider` mentions Claude Code only as a
  wire client, not as a harness slbh drives.
- No `slbh` subcommand path, and no inbox or queue code separable from
  `internal/harness`.
- No durable org inbox or request queue.
- No Codex app-server client for waking the Secretary by session name.
- No Intern, and no read-only tool set to give one: the general tool set
  mixes reads with edits, writes, shell and jobs. Nothing loads
  `instructions/intern.md`.

**Implementation order.** Each unit builds and tests on its own and lands
before the next starts:

1. The seam: JSON-tagged snapshots, `JobSnapshots()`, and named commands in
   place of `Jobs()` and `Seat()`; then the closed command set, with the
   front ends written against it.
2. The Claude Code headless leaf, for Opus.
3. The durable org inbox and request queue, a package independent of
   `internal/harness`.
4. The Intern: its read-only tools, `intern.md` loaded by name, and its
   watcher on the event stream.
5. The `slbh` subcommands over the store.
6. The Secretary wake over Codex's app-server.
7. End-to-end wiring: Seat reports, Intern questions, request status and the
   wake together.

## 12. Open

Nothing. Every design question this specification raised has been answered;
what remains is the delta in section 11, which is work rather than
uncertainty.
