# quack — headless bidirectional sessions (Discord ↔ agent)

- **Date:** 2026-06-01
- **Status:** Approved design, pre-implementation
- **Author:** Yves Brissaud
- **Builds on:** `hack/designs/2026-05-31-quack-design.md`

## Summary

Add an opt-in **`headless=true`** directive flag that makes a quack session
**conversational from Discord**: the agent's answers stream back into the
session thread, and any message the owner posts in that thread is forwarded to
the agent as its next turn.

The mechanism is a **unified "resume-per-turn" model**: each turn is one
non-interactive (`headless`) invocation of the agent CLI that emits a
machine-readable JSON event stream and **resumes** the prior turn's session by
id. Both supported agents expose this:

- **claude** — `claude -p … --output-format stream-json --verbose`, resume via `--resume <session_id>`.
- **codex** — `codex exec … --json`, resume via `codex exec resume <thread_id> …`.

Without the flag, quack behaves exactly as today: an interactive `tmux` session
you `tmux attach` locally, with no Discord back-channel. **The default path is
unchanged — zero regression.**

## Goals

- A per-command `headless=true` flag selecting the conversational mode.
- Per-turn agent answers posted to the thread, plus **light progress lines**
  for tool/command activity.
- Owner messages in the thread routed to the agent as the next turn.
- Session end via **`/stop`** in the thread **and** **archiving the thread**.
- One agent-agnostic turn abstraction; **claude and codex both supported**.
- No bot token ever handed to the agent (quack mediates all I/O and posting).

## Non-goals (this iteration)

- True token-by-token streaming (Discord edit rate limits ≈ 5/5s per channel →
  we post per turn, optionally with throttled progress lines).
- Surviving a quack restart (registry is in-memory; see *Lifecycle*). Cheap to
  add later thanks to resume-per-turn — deferred, not designed-out.
- The agent reading/posting Discord *itself* (the deferred "Discord read/post
  skill" — a separate, later feature).
- Codex parity with every claude nuance; codex support targets the same
  resume-per-turn contract.

## Mode selection

`internal/command` gains one optional flag on the directive line:

```
@quack dagger/dagger headless=true effort=high name=triage
Investigate the flaky cache test; reproduce it first.
```

- `headless` parses as a bool (`true`/`false`, default `false`).
- `headless=true` with an agent that has no headless driver (initially
  everything except the two below, but the registry is by agent name) →
  immediate usage error, no side effects.
- All other directive parsing/validation is unchanged.

## Architecture

```
Discord thread  ──message──►  quack (bot)
      ▲                          │  route by thread_id
      │ per-turn answer          ▼
      │ + progress lines    session registry (in-mem: thread_id → live session)
      │                          │  RunTurn(prompt)
      └──────── posts ───────  agent driver  ──spawn──►  `claude`/`codex` (headless, one process per turn)
                                   ▲   parse JSON events (assistant text, tool activity, result+session_id)
                                   └── stdin/args: prompt (+ --resume <ref>) , cwd = worktree
```

A **live session** is a small record, not a long-lived process: between turns
no agent process runs. A turn spawns one short-lived child, streams its events,
captures the new session ref, and exits.

### Components (new / changed)

```
internal/command/     directive.go      # + Headless bool
internal/agentproc/   driver.go         # Driver interface + Event types (pure contract)
                      claude.go         # claude resume-per-turn driver + JSON parse
                      codex.go          # codex resume-per-turn driver + JSON parse
                      *_test.go         # parse fixtures (recorded JSON), table tests
internal/session/     headless.go       # headless orchestration: registry, turn loop, event→Discord mapping
                      headless_test.go  # faked Driver + Replier
                      service.go        # branch: headless vs tmux
internal/discord/     bot.go            # route thread messages, /stop, ThreadUpdate(archive)
```

The pure pieces (directive flag, JSON→Event parsing, Event→message mapping) are
unit-tested; the child-process spawn sits behind the `Driver` interface and is
faked in `session` tests; an opt-in integration test exercises one real
`claude` turn.

## The unified turn model

### Driver contract (`internal/agentproc`)

```go
// Turn is one request to the agent.
type Turn struct {
    SessionRef string // agent-opaque resume token; "" = first turn
    Prompt     string // user text (first turn includes the quack-context header)
    Workdir    string // the worktree (child cwd)
    Effort     string // pass-through, first turn only
}

// Event is what a turn emits as it runs, normalized across agents.
type Event interface{ isEvent() }
type AssistantText struct{ Text string }       // a chunk/block of the agent's answer
type ToolActivity  struct{ Label string }       // compact line, e.g. "▶ go test ./..."

// TurnDone is the RETURN VALUE of a turn (not an emitted Event): the terminal
// state captured when the child exits.
type TurnDone struct {
    SessionRef string                            // ref to resume next turn
    CostUSD    float64
    Err        error                             // non-nil if the turn failed
}

// Driver runs one turn as a headless child process, emitting in-flight Events
// via emit and returning the terminal TurnDone.
type Driver interface {
    RunTurn(ctx context.Context, t Turn, emit func(Event)) TurnDone
}
```

quack stores the returned `SessionRef` on the live session and passes it as
`Turn.SessionRef` next time. The first turn's `Prompt` carries the existing
`<quack-context>` header (so the agent still knows its Discord origin).

### claude driver

- **First turn:** `claude -p <prompt> --output-format stream-json --verbose
  --permission-mode acceptEdits --allowedTools <list>` `<effort flags>`, child
  `cwd = worktree`.
- **Resume:** same, plus `--resume <session_ref>` (and no effort/`-p` prompt
  carries the new message).
- **Parse (NDJSON):** `type:"assistant"` → `AssistantText` from
  `message.content[].text`; `tool_use` content blocks → `ToolActivity`;
  `type:"result"` → `TurnDone{SessionRef: .session_id, CostUSD: .total_cost_usd,
  Err: .is_error ? .result : nil}`.
- **Permissions:** `acceptEdits` + an `allowedTools` allowlist by default
  (configurable); the worktree provides isolation. `bypassPermissions` available
  via config for fully sandboxed hosts.

### codex driver

- **First turn:** `codex exec <prompt> --json` (effort applied via the agent's
  effort template), child `cwd = worktree`.
- **Resume:** `codex exec resume <session_ref> <prompt> --json` *(exact `resume`
  invocation to be confirmed at implementation).*
- **Parse (JSONL):** agent-message items → `AssistantText`; command/file-change/
  MCP-tool items → `ToolActivity`; `turn.completed`/thread id → `TurnDone`
  (`SessionRef` = the codex thread id).

Drivers are keyed by agent name; `headless=true` for an agent without a driver
is a usage error.

## Output: events → Discord

A pure mapper turns the `Event` stream of one turn into thread posts:

- **`AssistantText`** accumulates into the turn's answer; on `TurnDone` the full
  answer is posted as one message (split if > Discord's 2000-char limit).
- **`ToolActivity`** → a compact progress line, **coalesced/throttled** (≤ ~1
  line/sec; collapse runs) so the thread isn't spammed. Rendered as a single
  edited "status" message during the turn, finalized on `TurnDone`.
- **`TurnDone.Err`** → an error post (`❌ …`); the session stays open so the
  owner can retry or `/stop`.
- Cost/tokens optionally appended as a faint footer (config toggle).

All posting goes through the existing `Replier`. The mapper is unit-tested with
synthetic event sequences.

## Input: thread messages → agent

`internal/discord` extends `onMessage`:

1. Drop bot/self messages (existing) — this keeps the loop safe (quack's own
   posts are never fed back).
2. If the message's channel is a **tracked headless thread**:
   - enforce the allowlist (owner only);
   - `/stop` (exact match / leading token) → end the session;
   - otherwise → enqueue the text as the next **turn** for that session.
3. Otherwise → the existing mention/new-session path.

Turns for a session are **serialized**: while a turn is running, further
messages queue (FIFO) and are delivered when it completes. A new
`ThreadUpdate` handler ends a session when its thread is archived.

## Session lifecycle & registry

- **Registry:** in-memory `map[threadID]*liveSession`, mutex-guarded.
  `liveSession` holds: agent driver, `sessionRef`, workdir, effort, a turn queue,
  a `running` flag, and a `cancel` for the in-flight child.
- **Create:** on a successful `headless=true` launch (after thread+worktree as
  today), register the session and run the first turn.
- **End conditions:** `/stop` command, or thread archived. (There is no
  "self-exit": resume-per-turn keeps no process between turns.) Ending cancels
  any in-flight child, posts a closing note, and deregisters.
- **Restart:** the in-memory registry is lost on a quack restart. A message in a
  now-untracked thread gets a one-line "session isn't active (bot restarted) —
  mention me to start fresh" reply. *(Because each CLI persists its own session
  on disk, persisting a tiny `threadID → {agent, sessionRef, workdir}` index to
  the state dir would make conversations survive restarts — deferred.)*

## Security

Unchanged from the base design and reinforced: in headless mode quack is the
parent process and performs **all** Discord posting, so the **bot token is never
exposed to the agent**. The agent still receives only the `QUACK_*` context
(channel/thread/message ids, permalink) — exactly what a future read/post skill
needs, with no credentials.

## Configuration changes

Per-agent config gains optional headless settings (with sane defaults), e.g.:

```toml
[agents.claude]
command          = "claude"
effort_template  = "--effort {effort}"
headless         = true                 # has a headless driver
permission_mode  = "acceptEdits"        # or "bypassPermissions"
allowed_tools    = "Bash(git *),Read,Edit,Write"

[agents.codex]
command          = "codex"
effort_template  = "--config model_reasoning_effort={effort}"
headless         = true
```

No new top-level config. Absent settings fall back to safe defaults
(`acceptEdits`, a conservative tool allowlist).

## Error handling

- Unknown/headless-unsupported agent, or bad flag → usage error, no side effects.
- Spawn failure → `❌` edited onto the ack; session not created.
- Turn failure (`TurnDone.Err`, non-zero exit, crash) → `❌` post with the agent's
  message; session stays open. No silent failures; details to journald.
- quack shutdown → cancel in-flight children, best-effort closing note per session.

## Testing

- **Pure unit:** directive `headless` parsing; JSON→`Event` parsing per agent
  using **recorded fixture streams** (committed sample `stream-json`/JSONL);
  `Event`→message mapping (answer assembly, splitting, progress coalescing).
- **Faked orchestration:** `session` headless turn-loop tested with a fake
  `Driver` + `Replier` + registry: first turn, resume passing the captured ref,
  queued messages, `/stop`, archive, turn error.
- **Opt-in integration** (gated like `QUACK_INTEGRATION`): one real `claude`
  headless turn in a temp git repo — assert a `session_id` is captured and a
  resume second turn succeeds. (Codex integration similarly gated, skipped if
  `codex` absent.)
- Existing tmux-path tests unchanged.

## Verify at implementation (open items)

1. **claude auth in headless on this host** — confirm `-p` reuses the existing
   `~/.claude` login (no `ANTHROPIC_API_KEY` needed); adjust the unit's env if it
   does. (`--bare` / API-key claims from research were unverified.)
2. **claude tool-activity JSON** — confirm which events/fields carry tool name +
   command for progress lines; parse defensively (best-effort labels).
3. **codex `resume` syntax** — confirm `codex exec resume <id> <prompt> --json`
   (or the current equivalent) and its event names for agent-message vs activity.
4. **Discord 2000-char split** behavior and thread-message edit cadence under the
   rate limit.

## Deferred / future

- Restart-survival via a persisted thread→session index (now cheap).
- Throttled live-edit "typing" output (vs per-turn) if desired.
- The Discord **read/post skill** for the agent (the planned next feature).
- `list`/lifecycle commands and a richer registry.
