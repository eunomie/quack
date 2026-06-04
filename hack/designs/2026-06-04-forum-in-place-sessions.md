# Forum support: in-place sessions

Date: 2026-06-04

## Problem

A Discord **forum** post is a thread (`ChannelTypeGuildPublicThread`, type 11)
whose parent is a forum channel (`ChannelTypeGuildForum`, type 15). The post's
title is user-authored and meaningful — it *is* the conversation's identity.

quack's session model assumes a mention arrives in a normal text channel, where
it opens a fresh thread off the triggering message (`Replier.OpenThread` →
`MessageThreadStartComplex`) and drives that thread. When a mention instead lands
**inside an existing thread** (the common case: a forum post), this breaks down:

- `onMessage` only routes a thread message to the live-session path when the
  thread is already `Tracked`. A *fresh* mention in a forum post is not tracked,
  so it falls through to the normal mention path.
- `run` then calls `OpenThread` on a message that is already inside a thread.
  Discord can't nest threads, so the call **fails**; the code falls back to
  `threadID = req.Origin.ChannelID` and posts in the same post. It half-works,
  but: the failing API call is wasted, the post title is never used as the
  session identity, and the title is never updated with status.

We want first-class behavior for this case, **additive** to the existing
thread-opening flow — not a replacement.

## Goal

When a mention arrives inside an existing thread, run the session **in place**:

1. Use the existing thread/post as the session's Discord surface — do **not**
   attempt to open a sub-thread.
2. Use the **post title** as the Discord-facing session identity, and stamp the
   working/done status emoji onto that title (👀 → ✅/❌), exactly as quack does
   for an auto-created thread title.
3. Leave the user's thread/post **open** when the session stops — it's theirs,
   not quack-created.

This applies to **any** existing thread, not only forum posts: the underlying
constraint ("can't nest a thread, so operate in the current one") is identical,
and forum posts are simply the common case. Regular manually-created threads get
the same fix for free.

### Decisions (from brainstorming)

- **Naming is display-only.** The post title drives the *Discord title* only. The
  worktree / branch / tmux name / `prep.name` continue to come from the
  agent-suggested slug (or an explicit `name=`). Arbitrary human titles ("Help
  with login!") are never forced through git-ref slugification. The title shown
  is the post name **verbatim** with a status emoji — no `owner/repo` label
  prefix, so the human title is preserved exactly.
- **Status emoji on the title:** yes — reuse the existing `titleUpdater`.
- **Scope:** any existing thread (forum post or regular thread), detected by
  `Channel.IsThread()`.
- **Stop does not archive** a user-owned in-place thread.

### Non-goals

- Forum **tags** (`applied_tags`) as a status surface. Nice and forum-native, but
  requires the forum to have matching tags configured; deferred.
- Reading the post's starter message as extra prompt context. The infer step
  already reads the thread's recent messages via `RecentMessages(channelID=…)`,
  which for a forum post returns the post's own messages — so this is largely
  captured already. No change.
- Creating forum posts from quack. Out of scope.

## Design

### 1. Detection (`internal/discord/bot.go`)

In `onMessage`, in the non-`Tracked` mention branch (after `mentionsBot`,
before/around `authorized`), resolve the channel:

```
ch, err := s.State.Channel(m.ChannelID)   // cached; REST s.Channel fallback
```

If `ch != nil && ch.IsThread()`, the mention is **in place**:

- Authorize the **channel** allowlist dimension against `ch.ParentID` (the parent
  forum/text channel), not `m.ChannelID`. A forum post's id is never in a
  channel-restricted `Allow.ChannelIDs`, so the current full `authorized()` would
  wrongly reject it. (Tracked-thread messages already get this leniency via
  `authorizedThread`, which ignores the channel dimension; in-place mentions need
  the equivalent.)
- Carry the in-place context on the `Request`:
  - `req.Origin.ThreadID = m.ChannelID`
  - `req.InThread = true`
  - `req.ThreadName = ch.Name`

A non-thread mention is unchanged: `ch.IsThread()` is false, no new behavior.

Channel resolution happens only for non-tracked mentions; for a normal text
channel it's a cached lookup. One REST call worst-case per new mention.

### 2. Request / Origin plumbing (`internal/session/service.go`)

Add to `Request`:

```go
InThread   bool   // mention arrived inside an existing thread (forum post or thread)
ThreadName string // that thread's current title, used as the Discord display title
```

`Origin.ThreadID` is already a field (today set inside `run`). For an in-place
mention it is pre-set by the bot to the thread id.

Both entry paths (`Handle` explicit grammar and `handleFluent`) converge on
`run` with the same `req`, so the in-place fields flow through unchanged.

### 3. In-place launch (`run` in `service.go`)

Where `run` currently calls `OpenThread`:

```go
threadID, err := s.reply.OpenThread(ctx, req.Origin.ChannelID, req.Origin.MessageID, provisional, …)
if err != nil { threadID = req.Origin.ChannelID }
req.Origin.ThreadID = threadID
```

Branch on `req.InThread`: when set, **skip** `OpenThread` and use
`req.Origin.ThreadID` directly (no API call, no failure path). Otherwise behave
as today.

Everything downstream already works with `threadID == req.Origin.ChannelID`:

- The ack, streamed output, and the success message post into the thread.
- The status reaction goes on `req.Origin.MessageID` in `req.Origin.ChannelID`
  (the thread) — fine.
- `Tracked()` keys live sessions on the thread id, so follow-up messages in the
  post route to `FeedThread` / `/stop` / `/attach` naturally.
- The start-rename at `service.go:262` is guarded by
  `threadID != req.Origin.ChannelID`, which is false in place — so the
  interactive (`no-headless`) path already leaves the post title untouched, for
  free. No change needed there.

Internal naming is unchanged: `prep.name` (worktree/branch/tmux/state dir) still
comes from the suggested slug or explicit `name=`.

### 4. Title = post name + status emoji (`titlestatus.go`, headless wiring)

The `titleUpdater` builds `threadTitle(emoji, label, name)` →
`"👀 owner/repo slug"`. For an in-place session the title base must be the **post
name verbatim** (no label), so it reads `"👀 Help with login"`.

Decouple the title's base text from the internal `name`. The in-place session
carries a **title base** string (= `req.ThreadName`); the `titleUpdater` stamps
`<emoji> <titleBase>` when present, falling back to the current
`threadTitle(emoji, label, name)` when empty. Existing rate-limit-aware,
latest-wins coalescing is reused unchanged.

`startHeadless` gains the title base (threaded through from `run`), and stores it
on the `liveSession` / `sessionRecord`. Status stamping stays headless-only, as
today.

### 5. Don't archive a user-owned post on stop (`headless.go` `StopThread`)

`StopThread` currently ends the session and then `ArchiveThread`s the thread. For
an in-place session the thread is the user's — end + untrack + post "session
stopped", but **skip the archive**. Gated on an `inPlace` flag on the
`liveSession`.

`onThreadUpdate` is unchanged: if the user archives their own post, stopping the
session remains correct.

### 6. Persistence (`internal/session/persist.go`)

Add to `sessionRecord` (both `omitempty`, so old records and normal sessions are
unaffected):

- `InPlace bool` — so a rehydrated session still skips archive-on-stop.
- the **title base** — so a rehydrated session still stamps the post title rather
  than the internal slug.

`record()` snapshots them; `newSession` rebuilds the `titleUpdater` from them.

## Data flow (forum post, headless)

```
user @quack in forum post "Help with login"
  → onMessage: not tracked, mentionsBot ✓
  → s.State.Channel(postID).IsThread() == true
  → authorize channel dim against ParentID (the forum channel)
  → Request{InThread:true, ThreadName:"Help with login", Origin.ThreadID:postID}
  → Handle → run:
      InThread ⇒ skip OpenThread, threadID = postID
      prep.name = suggested slug (worktree/branch)
      startHeadless(titleBase="Help with login", inPlace=true)
  → titleUpdater stamps "👀 Help with login" … "✅ Help with login"
  → follow-up posts in the post: Tracked(postID) ⇒ FeedThread
  → /stop: end + untrack + "session stopped", post left OPEN
```

## Testing

`internal/session` is unit-tested with fakes (`fakes_test.go`), no real Discord:

- **In-place run skips OpenThread:** a `Request{InThread:true, Origin.ThreadID:…}`
  drives `run`/`startHeadless` without an `OpenThread` call on the fake Replier;
  threadID is the provided id.
- **Title base:** the `titleUpdater` for an in-place session stamps
  `"👀 <ThreadName>"` and `"✅ <ThreadName>"` — no `owner/repo` label, internal
  `name` not shown. Extend `titlestatus_test.go`.
- **Stop leaves thread open:** `StopThread` on an in-place session posts "session
  stopped" and does **not** call `ArchiveThread` on the fake.
- **Persistence round-trip:** `InPlace` + title base survive
  `record()` → `persistRecord` → `Rehydrate`, and a rehydrated in-place session
  still skips archive and stamps the post title. Extend `persist_test.go`.

`internal/discord` (`bot_test.go`): detection — a mention whose channel
`IsThread()` produces a `Request` with `InThread`/`ThreadName`/`Origin.ThreadID`
set and authorizes against the parent channel; a normal-channel mention is
unchanged. The Discord session lookup (`s.State.Channel` / `s.Channel`) is
behind discordgo; factor the channel-resolve into a small seam (a function field
or tiny interface) so `bot_test.go` can fake it, consistent with how the bot is
already tested.

## Files touched

- `internal/discord/bot.go` — detect in-thread mention, parent-based channel
  auth, populate `Request` in-place fields; channel-resolve seam.
- `internal/session/service.go` — `Request.InThread`/`ThreadName`; `run` skips
  `OpenThread` in place; thread title base.
- `internal/session/headless.go` — `startHeadless` carries title base + inPlace;
  `StopThread` skips archive when in place; `liveSession` fields.
- `internal/session/titlestatus.go` — title base override in `titleUpdater`.
- `internal/session/persist.go` — `sessionRecord.InPlace` + title base;
  `record()` / `newSession` wiring.
- Tests: `internal/discord/bot_test.go`, `internal/session/titlestatus_test.go`,
  `internal/session/persist_test.go`, plus a `run`/`startHeadless` in-place test.
```
