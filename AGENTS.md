# AGENTS.md

Guidance for coding agents (Claude Code, Codex) working in this repository.
`CLAUDE.md` imports this file, so both tools read the same source.

quack is a small Go daemon that launches coding-agent sessions (Claude Code or
Codex) from Discord. It runs on a personal machine, opens an **outbound**
WebSocket to the Discord Gateway, and on a mention resolves a repo, creates a
git worktree, and runs the agent — either headless (a two-way Discord
conversation) or in a detached `tmux` session. No inbound networking.

## Commands

```sh
go build -o ~/.local/bin/quack ./cmd/quack   # build
go test ./...                                  # unit tests (no external deps)
go test ./internal/session -run TestHandle     # a single package / test
go vet ./...                                    # vet
QUACK_INTEGRATION=1 go test ./...               # integration tests (see below)
quack --config ~/.config/quack/config.toml      # run locally (or via the systemd user unit)
systemctl --user restart quack.service          # restart the unit (picks up a fresh build)
```

Integration tests are skipped unless `QUACK_INTEGRATION=1` is set. They shell out
to real tools: `gitexec`/`tmuxexec` tests need `git` and `tmux`; the `agentproc`
tests additionally need an authenticated `claude` / `codex` CLI on `PATH`.

### Build & restart

quack runs as the `quack.service` systemd **user** unit (`Restart=always`). To
deploy a change, rebuild the binary onto `PATH` and restart the unit:

```sh
go build -o ~/.local/bin/quack ./cmd/quack
systemctl --user restart quack.service
systemctl --user status quack.service           # confirm active + new main PID
```

**Caveat — restarting from inside a headless session:** if the agent issuing the
restart is itself a quack-spawned headless session, it runs inside the
`quack.service` cgroup, so a direct `systemctl --user restart` kills its own
process mid-turn. Schedule a detached restart instead — `systemd-run` runs it as
a transient unit outside that cgroup, so it fires after the turn ends:

```sh
systemd-run --user --on-active=10 systemctl --user restart quack.service
```

A session running in a plain `tmux`/shell scope (not under `quack.service`) can
restart directly without this dance.

## Architecture

A single binary (`cmd/quack`) wiring a layered design. The **orchestrator core**
lives in `internal/session` and depends only on interfaces it defines itself;
concrete adapters are injected by `main.go`. This is why `session` is unit-tested
with fakes (`internal/session/fakes_test.go`) and needs no real git/tmux/Discord.

Interfaces (defined consumer-side in `internal/session/service.go`) and their adapters:

| Interface | Adapter | Role |
|-----------|---------|------|
| `Git` | `internal/gitexec` | clone/fetch/worktree via the `git` CLI |
| `Tmux` | `internal/tmuxexec` | detached `tmux` sessions (note: adapter imports `session` for `NewSessionOpts`) |
| `Replier` | `internal/discord` (`replier`) | post/edit messages, threads, reactions |
| `agentproc.Driver` | `internal/agentproc` (`Claude`, `Codex`) | run one headless turn, stream events |

### Request flow

1. `discord.Bot.onMessage` — Gateway event → recognize a mention (a direct user
   mention **or** a mention of the bot's managed integration role, since Discord
   autocompletes both as `@quack`; `mentionsBot` + `botRoleIDs` resolve the role
   set per guild) → authorize against the `Allow` allowlist (user/guild/optional
   channel) → build `session.Request` (carrying any `m.Attachments`) → dispatch
   `Service.Handle` in its own goroutine. A message **in a tracked thread** is
   routed instead to `FeedThread` / `/stop` (`StopThread`) / `/attach`
   (`PromoteThread`); a screenshot-only thread reply (empty text) still feeds.
2. `Service.Handle` (`service.go`) — the spine: route the message (fluent infer by
   default, or the explicit `!` grammar) into a directive → resolve agent → open a
   Discord thread + post an ack → (if no explicit `name=`) ask the agent to suggest
   a name → `prepare` the workspace → write context → launch.

### Routing: fluent (default) vs. explicit grammar (`!`)

`Service.Handle` picks one of two paths via `directivePrefix` (`infer.go`):

A **fluent mention** is the **default** (no prefix): the whole message is a
natural-language request handed to a quick read-only one-shot (the configured
`infer_agent`, default `name_agent`) that — given the request plus recent channel
messages — emits the directive fields (target, base, worktree, agent, effort,
name, headless) as JSON. quack maps that to a `command.Directive` and launches via
the shared `run`, so everything downstream is identical. The infer step also yields
a resolved-context blurb (for references like "this feature") prepended to the
working agent's prompt, and replaces the separate naming call. On any infer
failure it falls back to a scratch-dir run of the raw request. See
`internal/session/infer.go` and
`hack/designs/2026-06-03-fluent-directive-inference.md`.

An **explicit mention** opts out of inference: if the mention starts with `!`
(with or without a following space), `directivePrefix` strips the marker and the
rest is parsed by the **`internal/command` grammar**. There the **first line** is
the directive (may be empty) and **everything after the first newline is the
verbatim prompt** — so a single-line `!` mention is just a literal prompt run in
the scratch workspace, no inference. `stripMention` trims only spaces/tabs, never
newlines, because that first newline is the directive/prompt boundary. Directive
tokens (any order): an optional target (repo ref, path, or the literal
`temp-dir`), bare keywords (`codex`/`claude`, `no-headless`/`headless`, `no-wt`),
and `key=value` flags (`agent=`, `effort=`, `name=`, `base=`, `headless=`).
Headless is the default.

### Workspace preparation (`prepare` in `service.go`)

Four cases: no target → the configured `scratch_dir` (`~/dev/work`, not isolated,
created if missing, effort defaults to `medium`) for quick questions; the literal
`temp-dir` → a fresh throwaway temp dir (not isolated, agent default effort) — the
escape hatch back to the old no-target behavior; a filesystem path; a repo ref →
clone-on-miss / `git fetch --all --prune` then add a worktree. Key points:

- **Scratch dir** (`scratch_dir`, default `~/dev/work`): shared across concurrent
  scratch sessions, so it suits read-only Q&A; two sessions writing files there
  can clash. The `medium` effort default is still overridable with `effort=` on a
  multi-line directive.
- **Worktree layout:** `<clone>-worktrees/<name>` (`internal/worktree`).
- **Base ref resolution** (`resolveBaseRef`): prefers remote-tracking refs in
  order origin → upstream → any other remote → local, so fork checkouts work
  (a branch may exist only on `upstream`). Default branch falls back to `main`
  when `origin/HEAD` isn't set.
- **`no-wt`** runs directly in the checkout (no isolation) — parallel sessions on
  the same repo can clash.
- **Concurrency:** a `keyedMutex` keyed on clone path serializes clone/fetch/
  worktree for the same repo across concurrent commands.

### Two execution modes

- **Headless (default, `internal/session/headless.go`):** one `liveSession` per
  Discord thread holding a buffered turn `queue`; a single `runLoop` goroutine
  serializes turns. Each turn is one `Driver.RunTurn` child process that resumes
  the previous turn via its `SessionRef`. Assistant text and tool activity stream
  back as Discord posts; status shows as a reaction on the user's message
  (👀→✅/❌). `/attach` promotes the session to a resumable `tmux` session;
  `/stop` ends it and archives the thread, and archiving the thread ends it. The
  session's resume state is persisted so it survives a quack restart (see
  *Restart resilience*).
- **Interactive (`no-headless`):** a single detached `tmux new-session` runs the
  agent with the prompt as argv; no Discord back-channel. Inherently restart-safe
  — tmux runs detached, independent of the quack process.

### Agent process normalization (`internal/agentproc`)

`Driver` abstracts an agent CLI. Each driver runs the agent in headless,
resume-per-turn mode and parses its line-delimited JSON stream (`claude
--output-format stream-json`, `codex exec --json`) into a common event set:
`AssistantText`, `ToolActivity`, and a terminal `TurnDone{SessionRef, …}`.
`SessionRef` is claude's `session_id` / codex's `thread_id`. **To add an agent:**
implement `Driver` (+ `SuggestName`), register it in the `main.go` driver switch,
and add an `[agents.<name>]` block to config.

### Session naming (`internal/session/names.go`)

A *provisional* name is used for the early ack/thread (`<repo>-<token>`, or an
explicit `name=`). With no explicit name, the configured `name_agent` is asked
(low-effort one-shot) for a slug; on any failure it falls back to
`<repo>-<base>-<token>`. Collisions (existing worktree/branch/tmux session) get
`-2`, `-3`, … appended; an explicit name that collides is an error.

### Discord context propagation (`internal/session/origin.go`)

Each session receives its Discord origin three ways so the agent can post back
later: a `<quack-context>` block prepended to the prompt, `QUACK_*` env vars on
the tmux session, and a `context.json` under `state_dir/sessions/<name>/`.

### Attachment passthrough (`internal/session/attachments.go`)

Files dropped on a command (e.g. screenshots) ride along on `Request.Attachments`
and on thread turns. `saveAttachments` downloads each (`fetchURL`, injectable;
default `httpFetch`) into `state_dir/sessions/<name>/attachments/` and appends a
`<quack-attachments>` block to the prompt listing the **absolute local paths** —
agents read images from files, not from ephemeral Discord CDN URLs. A single
download failure is noted inline rather than failing the turn.

### Trust seeding (`internal/session/trust.go`)

For the interactive claude path, `maybeTrust` pre-seeds
`~/.claude.json` (`projects.<abs>.hasTrustDialogAccepted=true`) so the new
worktree doesn't stall on the trust dialog. No-op for codex / headless.

### Restart resilience (`internal/session/persist.go`)

Headless sessions live only in memory (`s.sessions`), so a quack restart would
otherwise orphan every live thread. To survive it, each session's resume state
is written to a `session.json` under `state_dir/sessions/<name>/`: agent, workdir,
effort, thread + root-message IDs, and the latest `SessionRef`. It's written at
launch (with an empty ref) and rewritten after every turn, since the agent's
resume token can rotate per turn. `/stop` and `/attach` remove it.

On startup, `Service.Rehydrate` (called from `main.go` **before** the gateway
opens, so no message races the rebuild) scans those records and rebuilds each
`liveSession` without replaying any turn — the conversation resumes on the next
Discord reply via `--resume <SessionRef>`. Records whose worktree is gone or
whose agent driver is no longer configured are skipped. The agent's own
conversation history (claude's session jsonl, codex's thread) persists
independently; the record just carries the token that points back into it.

An in-flight turn at restart time is lost (its child process was killed); the
session itself still resumes on the next message.

## Config

TOML at `~/.config/quack/config.toml` (`internal/config`, loaded by `main.go`).
The Discord token is read from `DISCORD_BOT_TOKEN` (preferred) over `[discord].token`.
`[agents.<name>]` entries use placeholder templates — `{effort}`, `{name}`,
`{session}` — expanded into argv. See `config.example.toml` for the full shape and
the inline note on why the OS sandbox is left off (it breaks `git commit` in
worktrees whose shared `.git` lives outside the workspace).

Two optional levers tune the fluent infer step without touching the working
agent: `infer_guidance` (a free-text string folded into the infer prompt as
resolution hints — repo shorthands, where clones live; it never rewrites the
request and is bounded by the same JSON validation) and a per-agent `model`
field (claude-only; codex ignores it). Point `infer_agent` at a dedicated
`[agents.infer]` entry with `model = …` and `headless = true` to run the infer
one-shot (and naming) on a faster model — drivers are now selected by the
agent's `command`, so the entry's name is a free-form label.

## Conventions

- Keep the dependency-inversion seam intact: `session` should depend on its own
  interfaces, not on `gitexec`/`tmuxexec`/`discordgo` directly.
- tmux argv is exec'd directly (no shell), so prompts need no escaping; Discord
  messages are split at `discordMax` (2000) on newline boundaries (`render.go`).
- Design docs live in `hack/designs/` (dated `YYYY-MM-DD-*.md`) — read these for
  rationale before larger changes; put new design/spec docs there, not under `docs/`.
- Version control here uses Stacked Git (stg) patches, each with a
  `Signed-off-by: Yves Brissaud <yves.brissaud@gmail.com>` trailer and **no** AI
  `Co-Authored-By` lines. Never `git push` without explicit approval.
