# Channel-scoped sandbox defaults & multi-user sessions

Date: 2026-06-16

## Goal

Let the owner keep unsandboxed access in a chosen set of channels while everyone
else (and the owner, everywhere else) runs in a sandbox, and allow multiple
authorized users to interact with a single sandboxed session.

Three behavior changes:

1. On a **non-sandboxed** session, only the **creator** of the thread may
   interact. Messages from other users are ignored.
2. On a **sandboxed** session, **any authorized user** may interact (multi-user).
3. Add a channel-scoped sandbox default: a list of "trusted" channels where the
   owner runs unsandboxed; every other channel the bot operates in is sandboxed
   for everyone, always.

## Background — current model

- `resolveRole` (`internal/discord/bot.go`) decides a user's trust level:
  owner-id match → `RoleOwner` (today this **bypasses** the guild/channel
  allowlist); otherwise allowed-guild **+** allowed-channel **+** a configured
  guest role → `RoleGuest`; else rejected.
- Sandbox decision (`internal/session/service.go:332`):
  `sandboxed := req.Role.IsGuest() || dir.Sandbox`.
- `prepare` (`service.go:431`) and the runtime already key on the `sandboxed`
  bool, **not** on role: a sandboxed session always routes through
  `prepareGuest` (container + HTTPS/PAT clone), and `liveSession.sandbox`
  (`*SandboxHandle`, nil for unsandboxed) drives per-turn launching.
- `canModify` (`internal/session/headless.go:101`) gates feed/stop/switch and is
  today **role**-keyed: guests may act only on their own session
  (`authorID == caller.UserID`), owners on any.
- Guest *tool/skill* restrictions are applied via `guestDriver`
  (`persist.go:150`), selected by **role**.

## Two-level channel model

- **L1 = `allowed_channel_ids`** (existing): the channels the bot answers in.
  - empty → all channels in an allowed guild
  - non-empty → only these; all other channels are ignored — **including for the
    owner** (behavior change: owners no longer bypass the channel allowlist).
- **L2 = `trusted_channel_ids`** (new): the channels where the **owner is
  unsandboxed by default**. Expected to be a subset of L1; a channel listed in L2
  but not in L1 (when L1 is non-empty) is ignored, because L1 gates first.

Resulting per-channel behavior (within an allowed guild):

| Channel | Owner | Guest (holds guest role) |
|---|---|---|
| **In L2** | **unsandboxed** by default; may opt into a sandbox with the existing `sandbox` keyword | admitted, **sandboxed** |
| **In L1, not in L2** ("public") | **sandboxed, always** — no way to opt out | admitted, **sandboxed** |
| **Not in L1** (when L1 is set) | ignored | ignored |

### Backward compatibility

The trusted-channel feature is **inert until `trusted_channel_ids` is
non-empty**, mirroring how the guest feature stays off until `guest_role` is
set. When L2 is empty, the owner's sandbox default is `false` everywhere (today's
behavior); only the `sandbox` keyword or a guest role produces a sandbox. This
prevents an existing single-user config from silently sandboxing the owner after
upgrade.

## Design

### 1. Config (`internal/config`)

Add to the `Discord` struct (config.go), following the existing
singular+plural merge pattern:

```go
TrustedChannelID  string   `toml:"trusted_channel_id"`
TrustedChannelIDs []string `toml:"trusted_channel_ids"`
```

Add accessor (named `TrustedChannels()` to avoid colliding with the
`TrustedChannelIDs` field — mirroring the existing `AllowedChannelIDs` field vs
`ChannelIDs()` method):

```go
func (d Discord) TrustedChannels() []string {
    return mergeIDs(d.TrustedChannelID, d.TrustedChannelIDs)
}
```

### 2. Authorization + sandbox default (`internal/discord/bot.go`)

Extend `Allow`:

```go
TrustedChannelIDs []string // channels where the owner runs unsandboxed
```

Change `resolveRole` so the channel/guild allowlist gates **everyone**,
including the owner:

```go
func (b *Bot) resolveRole(userID, guildID, channelID string, memberRoles []string) (session.Role, bool) {
    if !allows(b.allowed.GuildIDs, guildID) || !allows(b.allowed.ChannelIDs, channelID) {
        return 0, false
    }
    for _, id := range b.allowed.OwnerUserIDs {
        if id == userID {
            return session.RoleOwner, true
        }
    }
    for _, want := range b.allowed.GuestRoleIDs {
        for _, have := range memberRoles {
            if want == have {
                return session.RoleGuest, true
            }
        }
    }
    return 0, false
}
```

Compute the **sandbox default** for owners alongside role resolution, against the
same channel used for authorization (`authChannel` — the parent channel for an
in-thread mention). The trusted-channel feature is active only when the list is
non-empty:

```go
// ownerSandboxDefault reports whether an owner request in channelID must default
// to a sandbox. Inert (false) until trusted channels are configured; once
// configured, only the trusted channels grant the owner an unsandboxed session.
func (b *Bot) ownerSandboxDefault(channelID string) bool {
    if len(b.allowed.TrustedChannelIDs) == 0 {
        return false
    }
    return !contains(b.allowed.TrustedChannelIDs, channelID)
}
```

Carry the decision on the `Request` as a single bool so the session layer needs
no channel knowledge. The decision is made only on **session creation**
(the mention path → `svc.Handle`); the tracked-thread feed path does not
re-decide.

Add to `session.Request`:

```go
// DefaultSandbox is the owner's channel-derived sandbox default, resolved at the
// gateway (true on every channel except the trusted ones; always false until
// trusted channels are configured). Guests are sandboxed independently of this,
// by role, in the session layer.
DefaultSandbox bool
```

In `onMessage` (mention path), set it to the *owner channel policy only* — the
guest case is handled defensively in the session layer (below):

```go
req.DefaultSandbox = b.ownerSandboxDefault(authChannel)
```

### 3. Sandbox computation (`internal/session/service.go`)

Extend the computation at service.go:332 to add the carried owner default, while
**keeping `req.Role.IsGuest()`** so "a guest is always sandboxed" stays a hard
invariant of the session layer (defense in depth — it does not depend on the
gateway computing a bool correctly):

```go
sandboxed := req.Role.IsGuest() || req.DefaultSandbox || dir.Sandbox
```

- Guest anywhere → `Role.IsGuest()` → sandboxed (independent of `DefaultSandbox`).
- Owner in a "public" L1 channel → `DefaultSandbox` true → sandboxed, and the
  `sandbox` keyword is moot (already true), matching "no way to opt out".
- Owner in an L2 channel → `DefaultSandbox` false → unsandboxed unless `sandbox`.
- L2 empty (feature off) → owner `DefaultSandbox` false → today's behavior.

No other changes here: `prepare`/`prepareGuest`, the `clampGuestDirective`
envelope, and `guestTargetAllowed` already key on the `sandboxed` bool, so a
sandboxed owner is correctly confined (headless, repo-only target, no host path).

### 4. Multi-user interaction (`internal/session/headless.go`)

Re-key `canModify` on the **session's sandbox state** instead of the caller's
role:

```go
// canModify reports whether caller may feed/stop/switch this session.
// Sandboxed sessions are shared: any authorized caller may act on them.
// Unsandboxed sessions are private to their creator.
func (ls *liveSession) canModify(caller Caller) bool {
    if ls.sandbox != nil {
        return true // sandboxed: any authorized user (multi-user)
    }
    return ls.authorID == caller.UserID // unsandboxed: creator only
}
```

This satisfies asks 1 and 2 directly:

- Non-sandboxed session (only ever an owner in an L2 channel, or an existing
  single-user setup) → only its creator interacts; others are ignored.
- Sandboxed session → any authorized user (owner + role-holding guests) may feed,
  stop, switch.

`canModify` already guards `FeedThread` (headless.go:156), `StopThread`
(headless.go:176), and `SwitchAgent` (switch.go:78), so the gateway paths that
call them inherit the new rule with no further change. The reaction-stop and
ask-answer paths also flow through these.

### 5. Wiring (`cmd/quack/main.go`)

Populate the new `Allow.TrustedChannelIDs` from `cfg.Discord.TrustedChannels()`
alongside the existing `OwnerUserIDs`/`GuestRoleIDs` wiring (~main.go:112).

## Decisions held constant (explicit)

- **No escape-hatch keyword.** Being in an L2 channel is the only way for the
  owner to get an unsandboxed session; there is no `no-sandbox`/`unsafe`
  directive. (Owners may still opt *into* a sandbox with `sandbox`.)
- **Guest-role requirement kept.** A non-owner must still hold a configured guest
  role to be admitted; the channel lists do not relax this.
- **Tool/skill restrictions stay role-keyed.** A *sandboxed owner* keeps full
  tools/skills — only filesystem/network *isolation* is sandbox-keyed, the
  tool/skill *restriction* (`guestDriver`) remains tied to `RoleGuest`.

## Components & boundaries

- `config`: parse + merge the new list. No behavior.
- `discord/bot.go`: the only place that knows about channels — resolves role and
  the owner sandbox default, then hands the session layer a finished
  `DefaultSandbox` bool. No channel logic leaks into the session package.
- `session`: consumes `DefaultSandbox`; `canModify` keys on the live sandbox
  handle. Unchanged sandbox execution path.

## Testing

- `config`: trusted singular+plural merge; empty → nil.
- `bot.go` `resolveRole`: owner now rejected outside L1; guest unchanged.
- `bot.go` `ownerSandboxDefault`: empty L2 → always false; non-empty L2 → false
  in a trusted channel, true elsewhere.
- `service` sandbox decision: guest always sandboxed; owner in trusted →
  unsandboxed; owner in public → sandboxed even without keyword; owner+`sandbox`
  in trusted → sandboxed; L2 empty → owner unsandboxed (regression guard).
- `headless` `canModify`: sandboxed session → non-creator allowed; unsandboxed
  session → non-creator rejected, creator allowed.

## Out of scope

- Per-channel guest-role overrides.
- Softening the "guests"/"owner-only" wording in the sandbox clamp messages
  (`clampGuestDirective`, promotion notice) now that owners can be sandboxed — a
  cosmetic follow-up, noted but not required.
