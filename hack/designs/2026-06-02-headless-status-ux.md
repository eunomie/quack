# quack — headless status & tool-activity UX

- **Date:** 2026-06-02
- **Status:** Approved design, pre-implementation
- **Author:** Yves Brissaud
- **Builds on:** `hack/designs/2026-06-01-quack-headless-bidirectional-design.md`

## Summary

Two UX refinements to the **headless** Discord conversation, driven by owner
feedback that a busy turn is noisy and that session status is hard to see at a
glance:

1. **Single refreshing tool message.** Within a turn, tool/command activity is
   shown in **one** message that is **edited in place** as new tools run, instead
   of posting a fresh message per tool burst. Editing avoids a new Discord
   notification per tool, and collapses the wall of `Grep`/`Read`/`Bash` lines
   into a single, quiet line that **updates to show the latest tool** (it does not
   accumulate a list). The agent's answer text still posts as its own separate
   message(s).

2. **Glanceable session status.** A single **global status** (working / done /
   error) for the whole session drives two new surfaces in addition to the
   existing per-turn reaction: a **status icon prefixed to the thread title**
   (`👀/✅/❌ <name>`) and the **reaction on the top/triggering message** kept
   current with the latest status — so the channel view reflects the *current*
   session state, not the outcome of the first turn forever.

Both changes are **headless-only**. The interactive (`no-headless`) path has no
turn loop and no Discord back-channel, so it keeps its single launch-time
reaction unchanged. **No regression to the default interactive path.**

## Motivation

- A single agent turn can emit dozens of tool calls. Today each *burst* of tools
  (the run up to the next answer-text block) is posted as a **new** message
  (`flushTools` in `runTurn`), so a turn becomes a long scroll of separate
  messages — each one pinging a notification.
- The status reaction on the **top** message (the mention that spawned the
  thread, visible in the channel) only ever reflects the **first** turn: after
  the first answer it shows `✅` and never changes, even while later turns run or
  fail. There is no at-a-glance, channel-level signal of the *current* state.

## Goals

- One **edited-in-place** tool-activity message per tool burst within a turn;
  answer text remains separate.
- No new notification per tool (achieved for free by editing instead of posting).
- A **global session status** (working / done / error) reflected on:
  - the **thread title** (icon prefix), best-effort, and
  - the **top/triggering message** reaction, kept current.
- Keep the existing per-turn reaction on each in-thread follow-up message.
- Stay within Discord rate limits (edits ≈ 5/5s per channel; thread renames
  ≈ 2/10min) without ever blocking a turn.

## Non-goals (this iteration)

- Token-by-token streaming of answer text (still one post per text block).
- Status surfaces for the interactive `no-headless` path.
- A perfectly live thread title — renames are heavily rate-limited, so the title
  is explicitly **best-effort** and may lag; the top-message reaction is the
  faster channel-level signal.
- Editing/merging answer messages (only the *tool* message is edited).

## Design

### Status model

A session has one **global status** at any moment:

| Status | Emoji | Meaning |
|--------|-------|---------|
| working | `👀` | a turn is currently running |
| done | `✅` | idle; last turn completed without error |
| error | `❌` | last turn ended in error |

The same three constants already exist (`emojiWorking/Done/Error` in
`service.go`). The global status transitions on turn boundaries inside
`runTurn`: → working at turn start, → done/error at turn end.

Two surfaces are driven from this global status, plus the existing per-turn
reaction:

| Surface | Scope | Where it shows | Mechanism |
|---------|-------|----------------|-----------|
| Thread **title** prefix | global (latest) | channel thread badge + threads sidebar | best-effort coalescing goroutine |
| **Top message** reaction | global (latest) | the channel where the thread was spawned | React/Unreact on turn boundaries |
| **In-thread** message reaction | per turn (turns ≥ 2) | inside the thread | existing React/Unreact |

**Turn 1 vs later turns.** For the first turn the triggering message *is* the
top message, so the global tracker already reacts on it — no separate per-turn
reaction is needed. For turns ≥ 2 the trigger is an in-thread message: it gets
its own per-turn reaction **and** the global status updates the top message +
title. This avoids double-reacting the same message.

### Top-message reaction (global)

`liveSession` records the **root** channel/message (the mention that opened the
thread — already available as the first turn's `turnReq`). A small helper sets
the global status by **clearing the previously-set global emoji** and adding the
new one on the root message, so the channel shows exactly one of `👀/✅/❌` and
it tracks the current turn:

- turn start → clear prior global emoji, add `👀`
- turn done → clear `👀`, add `✅`
- turn error → clear `👀`, add `❌`

The previously-set global emoji is tracked on `liveSession` so the helper knows
what to `Unreact`. Reactions are not meaningfully rate-limited at this volume
(a handful per turn), so these calls stay inline.

### Thread title (global, best-effort)

The title is set to `"<emoji> <name>"` where `<name>` is the session slug
already held on `liveSession` (`ls.name`). The title is **reconstructed** from
`ls.name` each time rather than read-modified, so status icons never stack.

Because Discord caps thread renames at ≈ 2 per 10 minutes, title updates run on
a **dedicated background goroutine** with **latest-wins coalescing**:

- A status change submits the desired emoji to the updater via a buffered,
  size-1 channel; if a prior pending value is present it is replaced (drained)
  so only the most recent target is applied.
- The goroutine calls `RenameThread` best-effort. If discordgo blocks on the
  rate limiter, it blocks **only this goroutine**, never the turn loop. Stale
  intermediate states are skipped by the coalescing.
- The updater is owned by the `liveSession` and shut down when the session
  closes; it is observable in tests (see Testing).

This makes the title a low-frequency, eventually-current hint; the top-message
reaction is the higher-fidelity channel signal.

### Single refreshing tool message

`runTurn` currently buffers tool labels and, on each answer-text block (and at
turn end), **posts** the buffer as a new message via `flushTools`. The change:

- The first tool of a burst **posts** a tool message and remembers its message
  id; each subsequent tool **edits** that message to show **only the latest
  tool** — a single line that replaces the previous one, never a growing list.
- **Edit throttling:** edits are coalesced to at most ~once/sec using a
  monotonic timestamp on the session; intermediate tool events update the
  in-memory line but only call `Edit` when the throttle interval has elapsed.
  A **guaranteed final edit** runs when the burst ends (answer arrives or turn
  ends) so the last tool always shows.
- **Burst boundary = answer text.** When an answer-text block arrives, the
  current tool message is **finalized** (its id/line cleared) and the answer is
  posted separately. Subsequent tools begin a **new** single tool message. In
  the common case (tools → final answer) this yields exactly one tool message per
  turn; when the agent interleaves tools and text, ordering stays correct.
- Because the message is a **single line**, it stays trivially under the
  2000-char limit — no multi-line buffer to cap.

The answer-text path is otherwise unchanged: each text block is split at
`discordMax` and posted as separate messages.

### Affected components

- `internal/session/headless.go`
  - `liveSession`: new fields — root channel/message id, current tool message id,
    current tool-line buffer, last global emoji, last tool-edit timestamp, and
    the title-status updater handle.
  - `startHeadless`: capture root channel/message from the first `turnReq`; start
    the title updater.
  - `runTurn`: rewired tool handling (post-then-edit + throttle + finalize on
    answer) and global-status transitions (start/done/error) on the root message
    + title; per-turn reactions retained for turns ≥ 2.
  - `close`: stop the title updater.
- `internal/session/render.go`: helper(s) for the title string (`"<emoji>
  <name>"`) and any tool-buffer join used by both post and edit.
- `internal/session/service.go`: no interface change required — `Replier`
  already exposes `Edit` and `RenameThread`.
- `internal/discord/replier.go`: unchanged (`Edit`, `RenameThread` exist).

No `Replier` interface additions; no new adapter surface.

## Error handling & edge cases

- **Edit fails / message gone:** edits are best-effort (`_ = ...`), matching the
  existing post-and-ignore-error style; a failed edit just leaves the last good
  state.
- **Turn cancelled mid-flight** (thread archived/`/stop`): existing behavior —
  clear the working marker and stay quiet — extended to also clear the global
  `👀` on the root message and leave the title as-is (the updater is torn down).
- **Rate-limited title rename:** absorbed by the background goroutine; the turn
  is never blocked; only the most recent status is eventually applied.
- **Rapid tool stream:** throttling bounds edit frequency; the final edit
  guarantees the last state is shown.
- **Title coalescing race:** size-1 latest-wins channel; a status submitted
  while one is pending replaces it, so the goroutine always converges to the
  newest target.

## Testing

Unit-tested in `internal/session` with the existing fakes
(`fakes_test.go`), which record `Post` / `Edit` / `React` / `Unreact` /
`RenameThread` calls. New/extended tests assert:

- A multi-tool burst produces **one `Post` + edits** for the tool message
  (not N posts), and the finalized tool message is a **single line** showing only
  the latest tool — not an accumulated list.
- An answer between tool bursts **finalizes** the first tool message and starts a
  **new** one for later tools.
- Global status transitions on the **root** message: `👀` on start, replaced by
  `✅`/`❌` on completion, across **multiple turns** (the top message is not stuck
  on the first turn's outcome).
- Turns ≥ 2 also carry a per-turn reaction on their in-thread message.
- The title updater applies `RenameThread("<emoji> <name>")`; with coalescing,
  the final applied title matches the last status. The updater is drained/closed
  deterministically so the test isn't timing-flaky (e.g. close the session and
  wait for the updater goroutine before asserting).

`go test ./internal/session` covers all of the above with no external deps.

## Rollout

Pure UX change to an existing opt-in mode; no config, no new flags, no migration.
Interactive path untouched. Shippable as a single change set.
