# Switch agent mid-thread

Date: 2026-06-09

## Problem

A headless session is bound to one agent for its whole life. The agent is chosen
once — at the mention, by inference or an explicit `agent=` — and `liveSession`
carries a single `driver`/`agentName` from creation to `/stop`. To try the other
agent on the same work you must start a new thread and re-establish all the
context by hand.

We want to switch the agent **in place**, inside a live thread, with a typed
command — `/claude`, `/codex` — and carry the accumulated context across so the
new agent picks up where the old one left off.

## Key constraint: `SessionRef` is agent-native and non-portable

Each agent resumes its own conversation through an opaque token: claude's
`session_id`, codex's `thread_id` (`SessionRef` in quack). These are **not
interchangeable** — codex cannot resume a claude `session_id`, and each agent's
history lives in its own on-disk format (claude's session jsonl, codex's thread).

So `/codex` in a claude thread cannot *resume* claude's memory. It starts a
**fresh** codex session that we must **seed** with the prior context. The seeding
mechanism is the heart of this design.

## Approach: handoff summary from the outgoing agent

When you switch, quack runs **one final turn on the outgoing agent** asking it to
summarize the state of the work for another agent taking over, captures that
summary, and seeds the **new** agent with it as a context block on its first
turn.

Considered and rejected:

- **Discord transcript replay** — read the thread's own messages and feed them to
  the new agent. No extra turn, but lossy: it only sees what was posted, not the
  agent's reasoning, tool detail, or the decisions behind the visible output.
- **Native transcript translation** — parse claude jsonl / codex thread into a
  prompt. Brittle and agent-specific; breaks the dependency-inversion seam.
- **Minimal (no chat transfer)** — just swap and let the new agent read the
  worktree fresh. Loses everything that isn't yet committed to disk.

The outgoing agent is the one entity that actually knows what's been tried,
rejected, decided, and left to do. Asking it to write the handoff is both the
most faithful transfer and the cleanest seam — no transcript parsing, no
agent-native coupling. The cost is one extra turn at switch time, which is
acceptable: the user explicitly chose to switch, so a brief "handing off…" pause
reads as natural.

## Seed timing: lazy + inline prompt

The captured summary is **not** run through the new agent eagerly. quack stores
it and swaps the driver; the summary rides along as context on the **next** turn,
whenever that comes:

- Bare `/codex` → switch and wait. The only cost is the outgoing summary turn;
  nothing is wasted if you then go quiet.
- `/codex <text>` → switch, then run `<text>` immediately as the first seeded
  turn (the handoff block is prepended to `<text>`).

## Scope

- **Headless tracked threads only.** The typed-command path exists only inside a
  tracked headless session (same as `/stop`, `/attach`, fast commands). A
  `no-headless` (tmux) session runs detached with no Discord back-channel and is
  out of scope.
- **Opt-in targets only.** Only agents that opt in via `switchable = true` are
  switch targets. The infer/name helper agents are not interactive targets and
  stay out by omission.

## Design

### 1. Config (`internal/config`, `internal/agent`)

Add one optional bool to the agent block (`agent.Agent`):

```toml
[agents.claude]
switchable = true   # opt in as a /<name> mid-thread switch target

[agents.codex]
switchable = true
```

```go
// internal/agent/agent.go
Switchable bool `toml:"switchable"` // opt in as a /<name> mid-thread switch target
```

The trigger string is **derived** from the block name (`/` + name) — no separate
trigger field. A switch target is any agent with `Headless == true &&
Switchable == true`. (`Headless` because a non-headless agent has no headless
driver to run the seeded turns.)

The field is named `switchable`, not `interactive`, to avoid confusion with the
existing `InteractiveArgs` (flags for no-headless/`/attach` launches) and
`Headless` fields.

### 2. Matcher (`internal/session/switch.go`)

Mirror `matchFastCommand`. `matchSwitch(text)` reports whether the first
whitespace-delimited token is `/<name>` for a switchable agent; on a match it
returns the resolved agent name and the remaining text (the inline prompt,
possibly empty). A trigger appearing anywhere but first does not match.

```go
func (s *Service) matchSwitch(text string) (agentName, prompt string, ok bool)
```

It consults the configured agents (filtering on `Headless && Switchable`), so no
extra config plumbing beyond the new field.

### 3. Interception (`internal/discord/bot.go`)

In `onMessage`'s tracked-thread branch, slot the switch **after**
`RunFastCommand` and **before** the ask-answer / `FeedThread` fall-through — it's
an explicit command, same precedence class as `/stop`, `/attach`, and fast
commands, so it takes precedence over treating the text as an ask_user answer:

```go
if b.svc.RunFastCommand(...) { return }
if b.svc.SwitchAgent(context.Background(), m.ChannelID, m.ChannelID, m.ID, content, atts, caller) { return }
if content != "" && b.svc.AnswerAskText(...) { return }
b.svc.FeedThread(...)
```

`SwitchAgent` returns `true` when the first token is a switch trigger (it owns
the message, even when authz then drops it), `false` when the text isn't a switch
trigger (caller falls through to `FeedThread`, so an unknown `/foo` still reaches
the agent as ordinary text). `caller` carries through so a guest can only switch a
session it started, matching `FeedThread`/`StopThread`.

### 4. The switch operation (`Service.SwitchAgent`)

The synchronous part (deciding the bool the bot routes on) is small; the slow
work runs in a goroutine, exactly like `RunFastCommand` → `execFastCommand`.

**`SwitchAgent` (synchronous):**

1. `matchSwitch(text)` → not a trigger ⇒ return false (fall through).
2. Look up the tracked session; not tracked ⇒ return false (defensive — the bot
   only calls this in a tracked thread).
3. `ls.canModify(caller)` false (a guest on someone else's session) ⇒ return true
   and stop — it was a switch command, just not an authorized one; we don't want
   `/codex …` falling through to be fed as text to a session the guest can't
   touch.
4. Spawn `go s.doSwitch(context.Background(), ls, target, prompt, channelID, messageID, atts, caller)` and return true.

**`doSwitch` (goroutine):** ordering matters — all the can-this-even-happen
guards run **before** any teardown, so a rejected switch never disturbs the live
session.

1. **Driver guard.** `s.drivers[target] == nil` ⇒ post `❌ unknown agent: <name>`
   and return. No teardown.
2. **Same-agent guard.** `target == ls.agentName` ⇒ post a brief note; if `prompt`
   is non-empty, `s.FeedThread(ctx, ls.threadID, channelID, messageID, prompt, atts, caller)`
   to run it as an ordinary turn; return. No teardown, no summary.
3. **Snapshot + tear down.** Read `oldRef := ls.ref()` and `oldAgent := ls.agentName`.
   Remove the session from `s.sessions` / `s.askByToken`, `ls.cancelPending()`,
   `ls.close()`, then `<-ls.done`. The old loop is now stopped and (for claude)
   `ls.sess` is closed. `ls`'s fields stay readable.
4. **Summarize (standalone).** If `oldRef != ""`, post `🔄 switching to <target> — summarizing…`,
   then `summary := s.runSummaryTurn(ctx, ls, oldRef)` (§6). This is a one-off
   `Driver.RunTurn` resuming `oldRef`, *not* a turn through the (now-stopped)
   loop. If `oldRef == ""` (switched before any reply), skip — summary is empty.
5. **Rebuild in place.** `rec := ls.record()`; set `rec.AgentName = target`,
   `rec.SessionRef = ""`, `rec.PendingHandoff = wrapHandoff(oldAgent, summary)`
   (empty when summary is empty). `newls := s.newSession(context.Background(), rec)`
   re-registers under the same thread id and starts the right loop type for the
   new driver (stream vs per-turn, chosen automatically); for a guest/sandbox
   record it rebuilds the launcher and selects `guestDriver(target)`, same as
   rehydrate. `s.persistRecord(newls.record())`.
6. **Confirm + seed lazily.** Post `🔄 switched to <target>`. For bare `/codex`,
   enqueue nothing — the handoff waits on `newls.pendingHandoff` for the next
   message. For `/codex <text>`, `s.FeedThread(ctx, ls.threadID, channelID, messageID, prompt, atts, caller)`
   runs `<text>` now, and `consumeHandoff` (§5) prepends the handoff to it.

Why close-first instead of summarizing through the live loop: closing first means
the summary is a standalone `RunTurn` resuming the persisted ref — for claude a
separate `claude --resume` process, independent of the now-closed streaming
`ls.sess`; for codex identical to a normal turn. This needs **zero** changes to
either run loop (no `capture` channel, no terminal-path plumbing). The cost is
that a turn in flight when you switch is cut off by `ls.close()` — acceptable and
arguably intended: you asked to switch now, and claude's stream loop would
interrupt an in-flight turn for any new message anyway. The summary then reflects
the last completed turn (the only state a ref points at).

### 5. Lazy seed consumption (`liveSession` + turn build)

`liveSession` gains `pendingHandoff string` (guarded by `mu`, like `sessionRef`).
A helper consumes it once:

```go
// consumeHandoff prepends the stored handoff block to text the first time it's
// called after a switch, then clears it. Returns text unchanged when none is
// pending.
func (ls *liveSession) consumeHandoff(text string) string
```

The two run loops send the turn text at different points and there is **no**
shared prompt-assembly function — the per-turn loop builds
`agentproc.Turn{Prompt: tr.text}` in `runTurn`, the stream loop calls
`ls.sess.Send(tr.text)` in `streamBegin`. So the helper is applied at **both**
call sites (`Prompt: ls.consumeHandoff(tr.text)` and
`ls.sess.Send(ls.consumeHandoff(tr.text))`). Guarding+clearing inside the helper
keeps it consumed exactly once regardless of which loop runs, by whichever turn
comes first (the inline prompt or the user's next message). This mirrors how
`origin.go` prepends `<quack-context>` and `attachments.go` appends
`<quack-attachments>`.

### 6. Capturing the summary (`runSummaryTurn`)

The one genuinely new mechanism, and — because §4 closes the loop first — a small
one. After teardown the agent process is gone, so the summary is a **standalone
one-off turn**: call `Driver.RunTurn` directly with the saved ref and the handoff
prompt, with an `emit` closure that both renders to the thread (a fresh
`turnRender`, so it streams identically to a normal answer) and accumulates
`AssistantText` into a `strings.Builder`. Return the accumulated text.

```go
// runSummaryTurn runs one resume-by-ref turn on the outgoing driver asking for a
// handoff summary, streams it to the thread, and returns the assistant text. The
// loop is already stopped, so this is a separate process (claude --resume / codex
// resume), not a turn through ls.sess.
func (s *Service) runSummaryTurn(ctx context.Context, ls *liveSession, ref string) string {
    rend := newTurnRender(s, ls)
    var sb strings.Builder
    done := ls.driver.RunTurn(ctx, agentproc.Turn{
        SessionRef: ref,
        Prompt:     handoffPrompt,
        Workdir:    ls.workdir,
        Effort:     ls.effort,
        Name:       ls.name,
        Launcher:   ls.launcher, // guest sessions: summarize inside the container
    }, func(e agentproc.Event) {
        switch ev := e.(type) {
        case agentproc.AssistantText:
            sb.WriteString(ev.Text)
            rend.handle(ctx, ev.Text, false)
        case agentproc.ToolActivity:
            rend.handle(ctx, ev.Label, true)
        }
    })
    rend.finalizeTools(ctx)
    rend.flushPending(ctx, true)
    if done.Err != nil {
        _, _ = s.reply.Post(ctx, ls.threadID, "⚠️ handoff summary failed: "+done.Err.Error())
    }
    return strings.TrimSpace(sb.String())
}
```

`ctx` is bounded by a timeout (`summaryTimeout`, a couple of minutes) so a wedged
agent can't hang the switch — on timeout the summary is whatever streamed so far
(possibly empty), and the switch proceeds. No `turnReq` changes, no loop
terminal-path plumbing, no concurrent use of `ls.sess`.

### 7. Handoff prompt

A hardcoded constant (no config — YAGNI). Roughly:

> You are handing this session off to a different coding agent that has **no
> access** to your conversation history. Write a concise but complete handoff so
> it can continue seamlessly. Cover: the goal/task, what you've done so far, key
> decisions and why, approaches tried and rejected, the current state of the
> code, and the immediate next steps. Be specific about file paths and
> identifiers. Output only the handoff.

The new agent receives it wrapped as:

```
<quack-handoff>
A previous agent (claude) worked on this session and produced this handoff:
<the summary>
</quack-handoff>
```

### 8. Persistence / restart resilience (`internal/session/persist.go`)

`sessionRecord` gains `PendingHandoff string` (`json:"pending_handoff,omitempty"`).
Written at switch time, restored by `Rehydrate`, and consumed on the first
post-restart turn — so a quack restart in the gap between switching and the next
message does not drop the handoff. `newSession` already copies record fields onto
the `liveSession`; it copies `PendingHandoff` too. `/stop` and `/attach` remove
the record as today, so the pending handoff is cleaned up with it.

The record's `AgentName` is the **new** agent after a switch, so a restart
resumes the new agent (empty `SessionRef` ⇒ a fresh native session, seeded by the
persisted handoff on the next message). This is already the existing contract;
the switch just rewrites the record.

### 9. Edge cases

| Case | Behavior |
|------|----------|
| Switch with a turn in flight | `ls.close()` cancels it; the summary resumes the last completed ref. The user asked to switch now. |
| Same-agent switch (`/claude` on claude) | No-op note; run inline prompt as a normal turn if present. No summary, no teardown. |
| Switch before any reply (`SessionRef == ""`) | Skip the summary turn; switch with an empty handoff. |
| Unknown agent name in driver map | Post `❌ unknown agent`; **no** teardown — the live session is untouched. |
| Unknown / non-switchable `/foo` | Not matched by `matchSwitch`; falls through to `FeedThread` as ordinary text. |
| Guest switches a session it didn't start | `canModify` false ⇒ dropped (returns true, does nothing). |
| Summary agent hangs | `summaryTimeout` bounds it; switch proceeds with whatever streamed (maybe empty). |
| `/attach` after a switch | Promotes the **new** agent (record already updated to it). |
| Inline prompt is whitespace only | Treated as bare switch (no first turn enqueued). |

## Testing

Unit tests with the existing `fakes_test.go` drivers — no real git/agent needed:

- `matchSwitch` table test: trigger-first matches and resolves the agent;
  mid-sentence trigger doesn't; non-switchable / unknown name doesn't; inline
  prompt is split off correctly.
- `SwitchAgent` swaps `driver`/`agentName`, clears `SessionRef`, stores the
  captured handoff (fake outgoing driver returns a known summary).
- Same-agent switch is a no-op and still runs an inline prompt.
- Empty-`SessionRef` switch skips the summary turn (fake driver records zero
  summary turns).
- `pendingHandoff` is prepended to the first turn's prompt exactly once, then
  cleared (assert the fake driver sees the `<quack-handoff>` block on turn 1 and
  not turn 2).
- `sessionRecord` round-trips `pending_handoff` through persist → `Rehydrate`,
  and the rehydrated session consumes it on its next turn.

## Files touched

- `internal/agent/agent.go` — `Switchable` field.
- `internal/config/*` — (only if the agent struct needs surfacing; the field
  rides on `agent.Agent` which config already embeds).
- `internal/agent/agent.go` — `Switchable` field (rides the `map[string]agent.Agent`
  that `config.Config` and `session.Config` already share, so no extra plumbing
  to surface it in `session`).
- `internal/session/switch.go` — **new**: `matchSwitch`, `SwitchAgent`,
  `doSwitch`, `runSummaryTurn`, `summaryTimeout`, `handoffPrompt` constant,
  `wrapHandoff`, `<quack-handoff>` wrapping.
- `internal/session/headless.go` — `pendingHandoff` field on `liveSession`;
  `consumeHandoff` helper; apply it at the per-turn call site (`runTurn`).
- `internal/session/headless_stream.go` — apply `consumeHandoff` at the stream
  call site (`streamBegin`’s `ls.sess.Send`).
- `internal/session/persist.go` — `PendingHandoff` on `sessionRecord`; set in
  `record()` and copy in `newSession`.
- `internal/discord/bot.go` — interception line in the tracked-thread branch.
- `config.example.toml`, `AGENTS.md` — document `switchable` and the
  `/claude`/`/codex` switch.
- Tests: `internal/session/switch_test.go`, plus additions to
  `persist_test.go`.

No change to `turnReq` or either run loop's terminal handling — the close-first
summary keeps the loops untouched.
