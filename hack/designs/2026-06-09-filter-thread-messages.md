# Filtering side-chat in quack threads

**Date:** 2026-06-09
**Status:** approved (design)

## Problem

A tracked headless thread forwards every non-command user message to the agent
via `FeedThread`. There is no way to talk *inside* the thread — leave a note,
discuss with another person, react to a message — without that text becoming the
next agent turn. Users want to hold side-conversations in a quack thread that
quack quietly ignores.

## Behavior

In a tracked thread, a message is **dropped** (never reaches `FeedThread`, no
agent turn, no reaction) when **either** condition holds:

1. **Reply to a non-bot user.** The message is a Discord reply whose referenced
   message was authored by a real (non-bot) user. Replies to quack's *own*
   messages are the normal way to talk to the agent, so they still feed — only
   replies to a human are treated as side-chat.
2. **Configured prefix.** The message content starts with one of a configured
   list of ignore-prefixes. Default: `"_ "` (underscore + space), chosen to avoid
   colliding with Markdown italics (`_word_`). Configurable so the owner can try
   other markers or add several.

"Dropped" is fully silent: no reply, no reaction. quack stamps a 👀 reaction when
it *handles* a message, so the **absence of any reaction** is itself the signal
that the message was not forwarded. This matches existing UX and needs no new
marker.

The two rules are independent — either one drops the message. A message that
matches neither feeds the agent exactly as today.

## Design

The filter is a `discord` gateway-layer concern only; the `session` orchestrator
core is untouched.

### Placement

In `Bot.onMessage` (`internal/discord/bot.go`), inside the tracked-thread block,
immediately after `authorizedThread(m)` passes and **before** the empty-content
check — so even an attachment-only reply to a human is treated as side-chat and
dropped.

```go
if b.svc.Tracked(m.ChannelID) {
    if !b.authorizedThread(m) {
        return
    }
    if b.ignoredInThread(s, m) {
        return
    }
    // ... existing empty-check, /stop, /attach, fast command, ask, FeedThread
}
```

### Helper

```go
// ignoredInThread reports whether a tracked-thread message is side-chat that
// must not reach the agent: a reply to a non-bot user, or content starting with
// a configured ignore prefix. Dropped messages get no reaction — the absence of
// quack's 👀 marker is the signal it wasn't forwarded.
func (b *Bot) ignoredInThread(s *discordgo.Session, m *discordgo.MessageCreate) bool {
    if ref := referencedMessage(s, m); ref != nil && ref.Author != nil && !ref.Author.Bot {
        return true
    }
    content := strings.TrimSpace(m.Content)
    for _, p := range b.ignorePrefixes {
        if p != "" && strings.HasPrefix(content, p) {
            return true
        }
    }
    return false
}
```

`referencedMessage` is the existing helper (`bot.go`) that resolves a reply's
target, preferring the gateway-inlined `ReferencedMessage` and falling back to a
REST fetch — the same call the mention path already uses. An empty `ignorePrefixes`
list disables prefix matching entirely (the reply rule still applies).

### Config wiring

New field on the `[discord]` config block:

```toml
[discord]
ignore_prefixes = ["_ "]   # messages starting with these are kept out of the agent
```

- `internal/config`: add `IgnorePrefixes []string `toml:"ignore_prefixes"`` to the
  `Discord` struct. When unset (nil), default to `["_ "]` at load time, alongside
  the other config defaults, so it works out of the box; an explicit empty list
  (`ignore_prefixes = []`) disables prefix matching while keeping the reply rule.
- `internal/discord`: add an `ignorePrefixes []string` field to `Bot`, set via a
  new parameter on `discord.New`.
- `cmd/quack/main.go`: pass `cfg.Discord.IgnorePrefixes` into `discord.New`.

## Testing

Table-driven tests for `ignoredInThread` in `internal/discord/bot_test.go`. Reply
cases set `m.ReferencedMessage` inline so no network/REST is needed:

- reply to a non-bot user → dropped
- reply to the bot (`Author.Bot == true`) → fed
- not a reply, plain text → fed
- `"_ note"` with default prefixes → dropped
- `"_italic_"` with default `["_ "]` → fed (no space after `_`)
- custom prefix list (e.g. `["//"]`) respected
- empty prefixes list → prefix matching disabled, reply rule still drops a
  reply-to-human

Config default test in `internal/config/config_test.go`: unset → `["_ "]`;
explicit `[]` preserved.

## Out of scope

- Per-thread or per-user filtering toggles.
- Editing/unsending a message to retroactively feed or unfeed it.
- Any visible marker on dropped messages (deliberately none).

## Touched files

- `internal/discord/bot.go` — `ignoredInThread`, `Bot.ignorePrefixes`, `New` signature
- `internal/discord/bot_test.go` — filter tests
- `internal/config/config.go` — `Discord.IgnorePrefixes` + default
- `internal/config/config_test.go` — default test
- `cmd/quack/main.go` — wire config → `discord.New`
- `AGENTS.md` — document the tracked-thread filter
- `config.example.toml` — document `ignore_prefixes`
