# Read-only Discord broker for sandboxes

Date: 2026-06-18

## Problem

A sandboxed session (a guest, or an owner dogfooding with `! sandbox`) has no
way to read Discord. The agent container sits on an `--internal` network whose
only egress is the allow-listing CONNECT proxy (`hack/sandbox/proxy`), and the
only Discord-reading skill on the host (`discord-reader`) authenticates with the
owner's **personal Discord user token** — the single credential most corrosive
to the sandbox's purpose (it is the owner's whole identity: every server, every
DM, read and write).

The owner wants a sandboxed agent to gather information from Discord **by
itself** during a session — primarily to recover a session's own thread after a
restart, and to read related discussion in the server — without a Discord
credential ever entering the agent's box.

## Goal

Give a sandboxed agent **ongoing, self-service, read-only** access to **public
channels of one configured guild** (the dagger server), while:

- no Discord credential (user token *or* bot token) is ever placed in the
  **agent** container;
- access is read-only by construction and provably scoped (one guild, public
  channels only);
- the design holds for a real, untrusted guest — not just owner dogfooding.

Non-goals: posting/reacting/managing (read-only only); private channels or DMs;
cross-server access; full-text search (see *Limitations*).

## Why a broker, and why a sidecar

A *broker* — quack-controlled code that holds the credential and exposes only
narrow read operations — is the threat-model-correct way to give the agent
self-service access without handing it a secret. Two realizations were weighed:

- **In-process broker** (an HTTP handler inside the quack host process, reusing
  the already-loaded `discordgo` session): keeps the bot token in exactly one
  place, but the agent's only egress is the CONNECT-only proxy, so reaching a
  quack-on-host listener needs a host-gateway route through the proxy **plus**
  TLS certs the agent trusts **plus** opening a host port to sandboxes —
  piercing the "sandboxes only talk to other containers" boundary.

- **Sidecar broker** (*chosen*): a third per-session container,
  `quack-<n>-discord`, on **both** the internal and external networks, exactly
  like the egress proxy. The agent reaches it directly by hostname over the
  internal network — **no TLS, no proxy change, no host port** — and the bot
  token lives only in this quack-controlled, read-only container, never in the
  agent box. It mirrors the existing dind/proxy sidecar pattern.

Trade-off accepted: quack's bot token is copied into one additional
quack-controlled container. Its blast radius is bounded by the broker's surface
(read-only, one guild, public channels); the agent cannot `exec` into the
broker, only make HTTP requests to it. This is strictly better than the token in
the agent container and far better than the user-token skill.

## Architecture

```
agent container ──http──> quack-<n>-discord ──https──> discord.com
 (internal net,           (internal + external          (bot token,
  no token)                nets, holds bot token)        read-only API)
```

- The broker is on the **internal** network (agent reaches it at
  `http://quack-discord:PORT`) and the **external** network (it reaches
  `discord.com`). This is the proxy's exact two-network shape.
- The broker hostname is added to the agent's **`NO_PROXY`** (alongside
  `docker`), so the agent's curl hits it directly instead of routing through the
  egress proxy (which is CONNECT-only and would reject a plain-HTTP forward).
- No `egress_allow` entry is needed for the broker: it sits on the external
  (NAT) network directly, like the proxy and dind sidecars.

### Broker binary

A small Go binary in a new image `quack-discord-broker`, built like
`hack/sandbox/proxy` (single dependency-free `main.go`, so no module fetch in the
image build and a minimal attack surface). It talks to Discord's REST API
directly with `net/http` (`Authorization: Bot <token>`) — the handful of GETs it
needs don't warrant a Discord SDK — and serves a read-only HTTP API. It takes via
env: `DISCORD_BOT_TOKEN`, `GUILD_ID` (the one allowed guild), `ADDR` (listen
address, default `:8080`).

### API (read-only)

- `GET /channels` — public text/forum channels of the configured guild
  `[{id,name,type,parent}]` (the guild is fixed by config, not a parameter)
- `GET /channels/{id}` — channel metadata (name, parent, type)
- `GET /channels/{id}/messages?limit=&before=&after=&contains=` — message
  history (author, content, timestamp); `contains=` is a server-side substring
  filter applied over the fetched page (a convenience over the no-search
  limitation, not a search index)
- `GET /channels/{id}/threads` — active public threads under a channel

Only `GET` is routed; any other method → `405`.

## Scope enforcement (security core)

Two hard gates run on **every** request, default-deny:

1. **Guild allow-list.** The channel (or its parent, for a thread) must resolve
   to the configured `GUILD_ID`. Anything else → `403`.
2. **Public-only.** Compute whether `@everyone` (role id == guild id) has
   effective `VIEW_CHANNEL` on the channel: start from the guild `@everyone`
   role's base permissions, then apply the channel's (and, for inheritance, the
   parent category's) `@everyone` deny/allow overwrites. If `VIEW_CHANNEL` is not
   provably set → `403`. Private thread type (`GUILD_PRIVATE_THREAD`, 12) is
   always rejected; a public thread inherits its parent channel's visibility. Any
   ambiguity is treated as private.

Because the only endpoints are `GET` history/metadata reads, the exposed
capability — even though the broker holds the full bot token — is exactly: read
history of public channels in one guild. Nothing writes.

## Wiring into quack

- `sandbox.Spec`: add `DiscordBotToken` and `DiscordReadGuildID`. Secret, like
  `GitHubPAT`: **not** serialized onto the `Handle`, re-sourced from current
  config on `Reattach`.
- `handleFor`: add `DiscordContainer: "quack-" + n + "-discord"`.
- `bringUp`: `Run` the broker on the internal net, `ConnectNetwork` it to the
  external net (the proxy's two-step), and append the broker alias to the
  agent's `NO_PROXY` env.
- `Teardown`: include `DiscordContainer` in the removed set.
- `Reattach`: include it in the restart set; the rebuild path recreates it via
  `bringUp`.
- `main.go`: wire the already-loaded Discord bot token and the new config field
  into the `Spec`.
- `internal/config` + `config.example.toml`: add `[guest].discord_broker_image`
  and `[guest].discord_read_guild_id`. The broker is enabled only when both a
  guild id is configured and the bot token is available; otherwise the sidecar
  is simply not started (the feature is inert, like the rest of the sandbox).
- **Discoverability** (`internal/session/origin.go`): for sandboxed sessions,
  add a `<quack-discord>` block to the injected context — the broker base URL,
  the read-only endpoints, and the session's own `thread_id` — so the agent
  knows how to read its own thread and public channels. Host sessions are
  unaffected.

## Limitations

- **No full-text search.** Discord's search endpoint is user-token-only; bots
  cannot use it. "Search" is reading channel/thread history (paginated) and
  filtering client-side or via `contains=`. Recovering a session's own thread
  and reading referenced channels works well; "find every mention of X across
  the server" does not.
- **Public channels only, one guild.** By design. Private channels, DMs, and
  other guilds are never reachable, even though the bot token could technically
  see some of them.

## Testing

- **Broker unit tests** with a fake Discord transport: guild mismatch → `403`,
  private channel → `403`, private thread type → `403`, public channel →
  messages, non-`GET` → `405`, `contains=` filters the page. The publicness
  computation gets its own table-driven test — it is the security-critical bit,
  so it is covered in isolation (base perms × overwrites × parent inheritance).
- **`sandbox` package**: `handleFor` includes the new container; Teardown and
  Reattach cover it (extend the existing fakes in `internal/sandbox`).
- Integration (gated by `QUACK_INTEGRATION=1`): provision a sandbox, curl the
  broker from inside the agent container for a known public channel, assert a
  private channel id is refused.

## Out of scope / future

- A shared (rather than per-session) broker container to avoid N copies of the
  token — deferred; per-session matches the current model and keeps teardown
  trivial.
- Write/participation capability — explicitly excluded here.
