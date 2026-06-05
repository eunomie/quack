# quack — mid-turn interjection & owner-answered questions

- **Date:** 2026-06-05
- **Status:** Implemented
- **Author:** Yves Brissaud
- **Builds on:** `hack/designs/2026-06-01-quack-headless-bidirectional-design.md`

> **Implemented as designed**, with the three decisions resolved to their
> recommendations: (1) hand-rolled minimal MCP HTTP server, no new dependency
> (`internal/askmcp`); (2) full streaming-driver model for interjection; (3)
> claude-first, codex unchanged. Interjection is implemented as **interrupt +
> resend** on the live process: a verified `control_request{interrupt}` cuts off
> the in-flight turn (which ends as `result subtype=error_during_execution`) so
> the agent reads the interjected message next — the agent genuinely sees the new
> message rather than queuing it behind the whole turn. Verified end-to-end
> against the real claude CLI (streaming session + MCP `ask_user` round-trip,
> integration tests gated on `QUACK_INTEGRATION`).

## Summary

Two related improvements to how a headless session and its owner interact in a
Discord thread:

1. **Mid-turn interjection (#2).** Today a message the owner posts while the
   agent is working is queued and only seen *after* the current turn finishes.
   We want the agent to see it *while* it works, so the owner can steer ("no,
   not that file", "stop and do X first") without aborting.

2. **Owner-answered questions (#1).** When the agent needs a decision it calls
   `AskUserQuestion`. In headless mode that tool has no UI, so it stalls briefly
   then the model self-resolves — the owner's answer (if they tab over) is never
   seen. We want the question surfaced in the thread and the agent to *block on
   the owner's real answer*.

`#3` (the 🛑 stop reaction) is intentionally left as-is: a full session
terminator. Once interjection works, "stop" no longer needs to double as
"redirect".

## Background: the current model & why it blocks both

Headless turns run **one process per turn** (`internal/agentproc/claude.go`):

```
claude -p <prompt> --output-format stream-json --verbose --resume <ref>
```

stdout-only, no stdin. `runLoop` (`internal/session/headless.go:269`) is a single
goroutine draining a buffered `queue chan turnReq`; it calls `runTurn`, which
blocks in `driver.RunTurn` until the child exits, then dequeues the next message.
So:

- A mid-turn message sits in `queue` until the child exits — the agent can't see
  it (#2).
- An `AskUserQuestion` `tool_use` is surfaced as a generic `🔧 AskUserQuestion`
  line (`render.go` → `catOther`) and never answered; the model proceeds on its
  own (#1).

Neither is fixable inside one-shot-per-turn. Both want a **persistent process we
can write to mid-flight**.

### Verified CLI behavior (claude 2.1.162)

- `claude -p --input-format stream-json --output-format stream-json` keeps **one
  process alive across many user messages**; each user message yields its own
  `result`. (Verified: two messages, one process, two answers.)
- `--replay-user-messages` echoes stdin user messages back on stdout for
  acknowledgment — lets us confirm an injected message was consumed.
- There is **no `--permission-prompt-tool`** flag in this version, and the
  harness cannot supply a `tool_result` for a *built-in* tool. So `AskUserQuestion`
  cannot be intercepted; quack must expose its **own** question tool via MCP and
  steer the model to it.

User-message stdin frame:

```json
{"type":"user","message":{"role":"user","content":[{"type":"text","text":"…"}]}}
```

## Feature #2 — mid-turn interjection (streaming driver)

### Driver: persistent streaming session

Add a streaming variant to the `claude` driver. Instead of one process per turn,
a session owns **one long-lived child**:

```
claude -p --input-format stream-json --output-format stream-json --verbose \
       --permission-mode <mode> [--allowedTools …] [--settings …] [--model …] \
       [--session-id <uuid>]   # first start; resume uses --resume <ref>
```

- stdin stays open for the session's life; quack writes one `user` frame per
  owner message.
- stdout is parsed continuously (reuse `parseClaudeStream`'s decoding) into the
  same `AssistantText` / `ToolActivity` events, plus a new **`TurnComplete`**
  event emitted on each `result` (so the orchestrator knows one answer finished
  without the process exiting).
- The `session_id` from the stream is still captured and persisted per `result`,
  so a quack restart resumes via `--resume`.

### Driver interface change

`agentproc.Driver` gains a streaming-session abstraction. Sketch:

```go
// Session is a live, resumable agent process accepting interleaved input.
type Session interface {
    Send(text string) error          // write a user frame to stdin
    Events() <-chan Event            // AssistantText | ToolActivity | TurnComplete
    Close() error                    // close stdin, wait, kill on ctx cancel
}

type StreamDriver interface {
    Open(ctx context.Context, o OpenOpts) (Session, error)
}
```

`RunTurn`/`OneShot`/`SuggestName` stay for codex and for one-shot uses (naming,
infer). A driver may implement `StreamDriver` (claude) or not (codex, which keeps
resume-per-turn).

### Orchestrator: `runLoop` over a streaming session

`liveSession` (claude path) opens a `Session` once. `runLoop` becomes:

- On a queued `turnReq` → `sess.Send(text)` and mark that message "working".
- Drain `sess.Events()`: render `AssistantText` / `ToolActivity` exactly as today
  (`flushPending`, tool summaries), and on `TurnComplete` finalize the current
  answer + set the per-message done status.
- A message that arrives mid-turn is `Send`-ed immediately; the agent sees it in
  the same process. (Claude addresses it after the in-flight step — already a
  large UX win over "after the whole turn".)

Status reactions (`emojiWorking`/`emojiDone`) now key off `TurnComplete`
boundaries rather than process exit.

Codex sessions keep the existing one-shot-per-turn `runTurn` unchanged.

### Stop (#3) unchanged

`close()` cancels ctx → closes stdin / kills the child. 🛑 and `/stop` and thread
archive all still terminate. No behavior change.

## Feature #1 — owner-answered questions (quack MCP `ask_user`)

### Mechanism

quack hosts a tiny **localhost HTTP MCP server** in-process, exposing one tool:

```
mcp__quack__ask_user(question, options[], multiple?) -> { answers[] }
```

Each headless `claude` is launched with:

- `--mcp-config '{"mcpServers":{"quack":{"type":"http","url":"http://127.0.0.1:<port>/mcp?s=<token>"}}}'`
- `--disallowedTools AskUserQuestion` (remove the dead native tool)
- `--append-system-prompt` nudging: "When you need a decision only the owner can
  make, call `mcp__quack__ask_user` and wait — do not guess."

The `?s=<token>` ties a tool call to the calling `liveSession` (random per-session
token; the server rejects unknown/again-used-after-stop tokens).

### Blocking on the owner's answer

The MCP `tools/call` handler:

1. Resolves the session from the token.
2. Registers a `pendingAsk{ questions, reply chan answer }` on the `liveSession`.
3. Posts the question to the thread: the prompt text + numbered options, and adds
   1️⃣2️⃣3️⃣… reactions for one-tap answering. (A typed reply also works.)
4. Blocks on `reply` (also selecting on session-ctx cancel and a configurable
   timeout).
5. Returns the chosen option(s) as the tool result; clears `pendingAsk`.

Because the tool call blocks inside the agent process, the agent genuinely waits —
no self-resolve. This works in **either** driver model, but composes naturally
with the streaming session.

### Routing the answer (bot)

While a session has a `pendingAsk`, the owner's next signal is the *answer*, not a
new turn:

- `onReaction`: a number emoji on the question message → `svc.AnswerAsk(threadID,
  index)`. (Stop reaction still terminates.)
- `onMessage`: in a tracked thread with a pending ask → route the text to
  `AnswerAsk` (free-form / "option 2") instead of `FeedThread`.

`Service` gains `HasPendingAsk(threadID)` and `AnswerAsk(threadID, …)`. The
`Replier` interface is unchanged (we already have `Post`/`React`).

### Timeout fallback (loud)

If no answer within `ask_timeout` (config, default e.g. 10m), the tool returns a
sentinel result: "owner unavailable — proceed using your best judgement and say
what you chose." This preserves today's "don't hang forever" property but makes
the autonomous fallback explicit instead of silent.

## Decisions to confirm

1. **#1 transport: hand-rolled minimal MCP HTTP vs. an MCP Go SDK dependency.**
   go.mod is deliberately tiny (2 direct deps). A single-tool Streamable-HTTP
   server is ~150–200 lines of `net/http`+`encoding/json` with no new dep.
   *Recommendation: hand-roll, keep go.mod minimal.*
2. **#2 scope: full streaming-driver model (verified) vs. cheaper
   interrupt-and-resend.** Streaming is the "correct" interjection (no lost work);
   interrupt-and-resend kills the in-flight turn and re-sends — lossy.
   *Recommendation: streaming.*
3. **Codex:** keep resume-per-turn (no streaming, no `ask_user`) for now, or also
   wire `ask_user`? *Recommendation: claude-first; codex unchanged this pass.*

## Implementation plan (phased)

Each phase is a self-contained commit (plain `git` — stg stack is broken in this
repo), with tests alongside.

**Phase A — streaming driver (#2)**
- A1 `agentproc`: `Session`/`StreamDriver`, `TurnComplete` event; `claudeStream`
  built on `parseClaudeStream` decoding. Unit-test the frame encode + stream
  decode with a fake stdout/stdin pair.
- A2 `session`: claude `liveSession` opens a `Session`; `runLoop` consumes
  `Events()`; status keyed on `TurnComplete`. Keep codex path. Update
  `service_test`/`headless_test` fakes.
- A3 `cmd/quack`: build the streaming driver for claude.

**Phase B — owner-answered questions (#1)**
- B1 `internal/askmcp`: minimal MCP HTTP server, one `ask_user` tool, per-session
  token. Unit-test the JSON-RPC handshake + tools/call.
- B2 `session`: `pendingAsk`, `HasPendingAsk`, `AnswerAsk`; post question +
  number reactions; block with timeout + ctx.
- B3 `discord/bot`: route reactions/messages to `AnswerAsk` when a pending ask
  exists.
- B4 `cmd/quack` + `config`: start the server, pass `--mcp-config`,
  `--disallowedTools AskUserQuestion`, `--append-system-prompt`; add `ask_timeout`.

**Phase C — docs**
- README + config.example.toml notes; this doc → Status: Implemented.

## Testing

- Driver: table tests over recorded stream-json lines incl. multi-`result`
  streams and an `ask_user` tool round-trip; assert one process, two answers.
- MCP server: handshake + `tools/call` blocks until `AnswerAsk`, honors timeout.
- Bot routing: pending-ask present → message/number-reaction routes to answer;
  absent → normal `FeedThread`; stop reaction always terminates.
- Manual: a real thread — interject mid-turn; trigger `ask_user`, answer via a
  number reaction and via a typed reply; let one time out.

## Risks

- **Streaming "turn" boundaries.** A `result` per user message defines a turn;
  back-to-back sends could interleave answers. Mitigate by tagging each `Send`
  and correlating `TurnComplete` ordering; render conservatively.
- **MCP model adoption.** The model must prefer `mcp__quack__ask_user`.
  `--disallowedTools AskUserQuestion` + system-prompt nudge should suffice; verify.
- **Restart during a pending ask.** The blocked tool call dies with the child; on
  rehydrate the agent simply re-asks. Acceptable.
