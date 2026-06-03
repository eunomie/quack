# quack — Discord bot to start Agent sessions

- **Date:** 2026-05-31
- **Status:** Approved design, pre-implementation
- **Author:** Yves Brissaud

## Summary

`quack` is a small Go daemon that runs on a personal home server and lets its
owner start new coding-agent sessions (Claude Code or Codex) from Discord. On a
mention, it resolves (or clones) a target repository, creates a git worktree for
the session, launches the agent inside a named `tmux` session with the prompt as
its initial argument, and reports back in a Discord thread. Each session is also
handed structured context about its Discord origin so a future skill can read and
post messages back.

The name is a nod to ducks (lgtd.io) and in the casual spirit of dagger's `dawg`.

## Goal (v1)

From Discord, the owner asks quack to **start** a new agent session, providing:

- the target **directory or git repository** to start from,
- an optional **effort** level,
- an optional **session name**,
- which **agent** to use (claude or codex),
- a **prompt**.

quack then prepares an isolated working directory and launches the agent there
with the prompt. Discussing with the running session afterward is out of scope:
Claude sessions are reachable through Claude's own remote/web surface, and any
session is reachable by attaching to its `tmux` session.

## Non-goals (v1)

- No session lifecycle commands (`list` / `stop` / `resume` / `archive`). Naming
  is deterministic enough that v1 does not need a persistent registry.
- No mechanism to converse with a running agent from Discord (the *context* to
  build that later is captured, but the message-reading/posting skill itself is
  not built here).
- No multi-machine split: the bot and the executor are the same process on one
  host.
- No inbound network exposure.

## Feasibility notes (candid)

- **No tailscale / dynamic DNS / port forwarding is required.** A Discord
  Gateway bot opens a persistent *outbound* WebSocket to Discord and receives
  commands over it. NAT/UDM is irrelevant. Inbound exposure would only be needed
  for Discord's "Interactions Endpoint URL" (HTTP webhook) model, which quack
  does not use.
- Reading freeform message text requires Discord's **Message Content Intent**, a
  *privileged* intent. For a single private server (far under the 100-server
  verification threshold) it is a one-checkbox toggle in the Developer Portal.
- Because the bot runs on the same machine as the agents, there is no "bot vs.
  server component" split — it is one process that directly performs the git,
  tmux, and launch work locally.

## Architecture

A single Go binary (`quack`) using `bwmarrin/discordgo`.

```
Discord  ──outbound WS (Gateway)──►  quack daemon (home server)
                                       │
                                       ├─ command parse + auth
                                       ├─ repo resolve / clone
                                       ├─ git worktree add
                                       ├─ tmux new-session → agent + prompt
                                       └─ reply in Discord thread
```

- Connects with intents `Guilds`, `GuildMessages`, and the privileged
  `MessageContent`.
- Runs as a **systemd user service** (`Restart=always`, logs to journald).
  `discordgo` auto-reconnects on gateway drops, so a flaky home connection
  self-heals.
- Git/tmux work runs in a goroutine so the gateway stays responsive. A
  **per-clone mutex** serializes concurrent operations on the same repository to
  avoid clone/worktree races.

### Components (Go packages)

```
quack/
  cmd/quack/main.go      # wiring: config, discord session, handler
  internal/config/       # config.toml load
  internal/discord/      # gateway connection, mention handling, threads, replies
  internal/command/      # directive grammar parse + validation
  internal/repo/         # repo-ref resolution, clone, path mapping
  internal/worktree/     # worktree creation, naming, collisions
  internal/agent/        # agent registry, effort mapping, argv build
  internal/launch/       # tmux session creation + env injection
  config.example.toml
  quack.service          # systemd unit example
  README.md
```

Side effects sit behind small interfaces (`Git`, `Tmux`, `Replier`) so the
orchestration logic can be tested against fakes.

## Security boundary

- Hard allowlist: quack responds **only** to the owner's Discord user ID, **only**
  in one configured guild (optionally one channel). Everything else is ignored,
  with a single terse "not authorized" reply so the owner is never left guessing.
- Because the process shells out to git / tmux / agents, the allowlist check runs
  **before any parsing or side effect**.
- The bot **token** comes from env (`DISCORD_BOT_TOKEN`) or a `0600` secret file
  and is **never committed**.

## Command grammar (freeform mention)

The first line after the mention is the **directive line**; everything after is
the **prompt** (verbatim, multiline).

```
@quack dagger/dagger agent=claude effort=high name=fix-cache-pin
Investigate the directory cache pin bug. Start by reading
internal/core/… and reproduce with a failing test first.
```

- **First token** of the directive line = repo-or-path (required).
- **Remaining tokens** = optional `key=value` flags: `agent=`, `effort=`,
  `name=`, `base=`.
- **Prompt** = everything from line 2 onward, trimmed. A blank line after the
  directives is optional (just nicer to read).
- Unknown flag or missing repo → quack replies with a one-line usage hint instead
  of guessing.

### Defaults

| Input    | Default                                                                 |
|----------|-------------------------------------------------------------------------|
| `agent`  | `claude` (configurable)                                                 |
| `effort` | omitted → no effort flag (agent default); otherwise passed verbatim     |
| `base`   | the repo's detected default branch (`origin/HEAD`), freshly fetched     |
| `name`   | generated readable slug (prompt-derived, e.g. `fix-cache-pin`)          |

## Repo resolution

The first directive token is classified as a **path** if it starts with `/`,
`~`, or `.`, otherwise a **repo reference**.

- **Repo ref forms** all normalize to `<dev_src_root>/<host>/<owner>/<repo>`
  (default root `~/dev/src`):
  - `dagger/dagger` → `…/github.com/dagger/dagger` (host defaults to github.com)
  - `gitlab.com/foo/bar` → `…/gitlab.com/foo/bar`
  - `https://github.com/owner/repo(.git)` or `git@github.com:owner/repo.git`
  - **Exists** → used as the main clone (and `git fetch`-ed).
  - **Missing** → cloned there (default `git@…` SSH, configurable to HTTPS),
    creating parent directories.
- **Path forms:** the directory must exist.
  - A **git repo** → treated as the main clone and worktreed.
  - A **plain directory** → the agent runs directly in it (no worktree/branch),
    with a note in the reply that there is no isolation.
- If the path/clone is itself a worktree, quack resolves to the **primary clone**
  (`git rev-parse --git-common-dir`) so new worktrees stay siblings of the main
  clone — matching the observed `dagger-worktrees/` layout.

## Worktree creation

Observed convention on the host (from `dagger/dagger`):

- Main clone: `~/dev/src/github.com/dagger/dagger`
- Worktrees: `~/dev/src/github.com/dagger/dagger-worktrees/<name>`

So the rule is `<clone-path>-worktrees/<session-name>`, on a fresh branch:

```
git -C <clone> fetch
git -C <clone> worktree add -b <session-name> \
    <clone>-worktrees/<session-name> origin/<base>
```

- **Branch name = worktree dir name = session name** (keeps the dagger
  convention).
- **Base ref** is `origin/<base>` after a fetch, so the worktree is current.
  `base` defaults to the repo's detected default branch.
- **Collisions:**
  - A *generated* name auto-bumps (`-2`, `-3`, …).
  - An *explicit* `name=` that collides → quack **refuses and asks for a
    rename**, rather than silently changing what the owner named.

## Agent launch (tmux)

```
tmux new-session -d -s quack/<name> -c <worktree> \
     -e QUACK_CHANNEL_ID=… -e QUACK_THREAD_ID=… (…env…) \
     -- <agent> <effort-flags> <prompt>
```

- Every part is passed as a **separate argv from Go (no shell string)**, so the
  prompt needs zero escaping and can contain arbitrary characters.
- The prompt passed to the agent is the **context header + the user's prompt**
  (see *Discord context injection*).
- `remain-on-exit on` is set so the pane stays inspectable after the agent
  finishes; interactive Claude/Codex sessions stay live regardless.
- **Ops requirement (candid):** under systemd the service must have `claude` /
  `codex` on `PATH` and access to their credentials (`~/.claude`, codex auth).
  The unit documents the required `Environment=` / `PATH`. Claude on the host is
  already configured with remote control access enabled everywhere, so a plain
  interactive `claude` in tmux is automatically reachable from Claude's
  web/remote surface — no special launch flag is needed.

## Effort (pass-through string)

`effort` is a **free-form string** (e.g. `high`, `xhigh`, `max`) passed straight
through to the agent — quack does not interpret or validate it. Each agent
declares an `effort_template` with an `{effort}` placeholder; quack substitutes
the string and splits the result into argv. When `effort` is omitted, no effort
flag is added (the agent uses its own default).

```toml
[agents.codex]
command         = "codex"
effort_template = "--config model_reasoning_effort={effort}"

[agents.claude]
command         = "claude"
effort_template = "--effort {effort}"   # exact claude flag confirmed at impl time
```

This keeps quack agnostic to each agent's effort vocabulary: new tiers like
`xhigh` / `max` work without any code or config change — only the value the owner
types changes.

## Discord context injection + threads

### Threads

- On a mention in a channel, quack creates a **public thread anchored to the
  triggering message**, named after the session, and posts the ack + all status
  updates **inside the thread** (keeps the parent channel clean).
- `auto_archive_duration` is set high (default 7 days / 10080 min, configurable)
  so a long-running session's thread does not archive.
- The thread is the agent's **back-channel** for the future message skill.
- If the session name collision-bumps, the thread is **renamed** to match.
- **Fallback:** if threads aren't available (DM, or already inside a thread),
  quack replies in place and sets `thread_id = channel_id`.

### Captured context

`guild_id`, `channel_id`, `thread_id`, `message_id` (the triggering message),
`reply_message_id` (quack's ack message *inside the thread* — the anchor an agent
can later edit/reply to), `author_id` + username, `created_at`, and a ready-made
**permalink** `https://discord.com/channels/<guild>/<channel>/<message>`.

Delivered three overlapping ways — prompt for immediate awareness, structured
data for a future skill:

1. **Prompt header** prepended to the prompt:
   ```
   <quack-context>
   channel_id: 123…   thread_id: 234…   message_id: 456…   reply_message_id: 789…
   author: yves (id 111…)
   permalink: https://discord.com/channels/…/…/…
   </quack-context>

   <user prompt, verbatim>
   ```
2. **Context file** `context.json` written to a **state dir**
   (`~/.local/state/quack/sessions/<name>/context.json`), *not* in the worktree,
   so it never dirties the git tree. A skill reads it via `$QUACK_CONTEXT_FILE`.
3. **Env vars** in the tmux session (`tmux new-session -e …`):
   `QUACK_GUILD_ID`, `QUACK_CHANNEL_ID`, `QUACK_THREAD_ID`, `QUACK_MESSAGE_ID`,
   `QUACK_REPLY_MESSAGE_ID`, `QUACK_PERMALINK`, `QUACK_CONTEXT_FILE`,
   `QUACK_SESSION_NAME`.

### Sequencing

1. Parse the directive line and compute a provisional session name.
2. Create the thread off the triggering message; post the ack inside it.
3. Resolve / clone the repo, create the worktree (rename the thread if the name
   had to bump).
4. Launch the agent with the full context (thread + reply IDs included).
5. Edit the ack with the final success details.

### Future-skill scope/security (candid)

- This design makes the context *available*; the actual "read/post Discord
  messages" **skill is out of scope for v1**. These identifiers are exactly what
  it needs, so it is cleanly buildable later.
- That future skill needs **Discord API credentials** to call back. quack does
  **not** auto-inject the bot token into the agent's environment (handing an
  agent the bot's full credentials is a real risk). When the skill is built, the
  choice is made deliberately: a scoped read token, or a tiny local quack
  endpoint the skill calls so the token never leaves the bot.

## Reply UX & error handling

- Immediate ack inside the thread (`🦆 on it — cloning dagger/dagger…`), later
  **edited** with the final result. Cloning can take minutes; mention-replies
  have no 3-second deadline (unlike slash-command interactions).
- **Success** reply: session name, worktree path, branch, `tmux attach -t …`,
  agent + effort, and the Claude web link when available.
- **No silent failures:** every stage (clone, worktree, tmux, missing agent
  binary) posts a clear error *and* logs detail to journald.

## Configuration reference (`~/.config/quack/config.toml`)

- `discord.token` (or `DISCORD_BOT_TOKEN` env) — bot token.
- `discord.allowed_user_id` — the only authorized user.
- `discord.allowed_guild_id`, `discord.allowed_channel_id` (optional).
- `discord.thread_auto_archive_minutes` (default `10080`).
- `dev_src_root` (default `~/dev/src`).
- `clone_protocol` (`ssh` | `https`, default `ssh`).
- `default_agent` (default `claude`).
- `[agents.<name>]` — `command`, `effort_template` (with `{effort}` placeholder).

## Testing

- Unit-tested pure logic: directive parsing, repo-ref → path mapping (every
  form), name slug + collision bump, effort → flag mapping, tmux argv
  construction, context-header rendering.
- Side effects behind `Git` / `Tmux` / `Replier` interfaces → faked in
  orchestration tests.
- One opt-in integration smoke test: real `git worktree add` in a temp dir plus a
  throwaway `tmux -L quacktest` server, gated by env / build tag (needs git +
  tmux installed). No live Discord in tests.

## Deferred / future work

- Session lifecycle commands + persistent registry (SQLite) — Approach 2.
- The Discord message read/post **skill** for running agents.
- Per-session Discord thread back-channel posting from the agent.
- Possible richer input UX (slash commands / modal) if freeform proves limiting.
