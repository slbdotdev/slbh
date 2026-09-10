# slbh

`slbh` is slb's small, Linux-first agent harness. It provides a responsive Bubble Tea REPL, runtime-isolated agents and jobs, provider streaming, stable-prefix inference caching, and a tool surface designed for coding agents.

## Run

```sh
go run ./cmd/slbh
```

The default root model is `deepseek/deepseek-v4.1-flash` with `xhigh` effort. Subagents default to `zai/glm-5.3-flash` with `high` effort. Set `SLBH_MODEL`, `SLBH_EFFORT`, and a provider API key (`OPENROUTER_API_KEY`, `DEEPSEEK_API_KEY`, or `ZAI_API_KEY`) to change it. `SLBH_HOME` can be used to place runtime logs somewhere explicit.

The app remains useful without a key: messages are recorded and the provider returns a clear configuration error in the transcript.

## Slash commands

`/exit` and `/quit` cleanly stop all agents and jobs. `/clear` clears the visible chat, `/model [name]` changes the model, `/effort [level]` changes reasoning effort, `/agents` and `/jobs` print runtime state, and `/compact` writes a compact summary marker to the transcript.

## Design notes

Each launch gets a cryptographically random runtime ID and a runtime directory under `.slbh/runtimes/<id>`. Transcript and job records are JSONL and are never shared between launches. Child agents are goroutines with bounded depth (root 0, children 1, grandchildren 2); their steer inboxes are independent from parent delivery. Jobs use cancellable process contexts and are always drained during runtime shutdown.
