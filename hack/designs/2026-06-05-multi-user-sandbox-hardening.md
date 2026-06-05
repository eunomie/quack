# Multi-user sandbox hardening

Date: 2026-06-05

## Problem

quack today has a single, flat trust level. The Discord allowlist
(`internal/discord/bot.go` → `Bot.authorized`) checks `user AND guild AND
channel` and every authorized user is fully equal: they can launch sessions on
the **host**, in the owner's real checkouts (`~/dev/src/...`), in the shared
scratch dir (`~/dev/work`), in interactive `tmux` mode with
`--dangerously-skip-permissions`, and the agent runs as the **owner's OS user**
with the owner's full home — SSH keys, every other repo, the quack config (which
holds the Discord token), and the claude/codex credentials.

The owner wants to invite a few personally-trusted friends without handing them
that. Guests should be able to do real work — run an agent against a repo, build
and test it, even use Docker and push a branch — but must not be able to reach
the host, the owner's other data, or the owner's secrets beyond a narrow,
revocable set the owner deliberately shares.

## Goal

Two trust levels, decided per Discord user:

- **Owner** — identified by Discord user id. Exactly today's behavior, untouched.
- **Guest** — identified by membership in a configured Discord **role**. Every
  guest session runs inside a **Docker sandbox**: a fresh per-session container
  (plus a Docker-in-Docker sidecar) whose only contents are a clone of the
  target repo, a minimal injected credential set, and the agent CLI. Guests are
  forced headless, restricted to repo targets, given a curated tool/skill set,
  and their network egress is allow-listed. This holds for **both** claude and
  codex.

Anyone who is neither owner nor guest is rejected, as today.

### Threat model (what the sandbox does and does not protect)

The sandbox protects **the host and the owner's *other* data**: a guest agent —
even a misbehaving one — cannot read the owner's SSH keys, other repositories,
the quack config / Discord token, or anything else on the host filesystem,
because none of it exists inside the container.

It does **not** try to stop a guest from misusing the **credentials the owner
deliberately gives them** (the shared claude/codex auth and a fine-grained
GitHub PAT). Those live inside the guest container by necessity, and because
guests get real Docker (a privileged dind sidecar with registry egress), a guest
who actively wants to exfiltrate *their own* injected token can. The containment
strategy for those credentials is that they are **scoped and revocable**, not
that they are unreachable. This matches the stated context — a handful of
personally-trusted friends — and the owner's explicit choice of the dind-sidecar
(over the stronger, Fedora-unsupported Sysbox runtime).

## Design

### 1. Roles & authorization (`internal/discord`, `internal/config`)

New config under `[discord]`:

| Field | Meaning |
|-------|---------|
| `owner_user_ids` | Discord user ids with full (today's) access. |
| `guest_role_ids` | Discord **role** ids whose members get the sandbox. |

Back-compat: the existing `allowed_user_id(s)` continue to mean **owner** — an
existing single-user config keeps full access with no edits. `guild`/`channel`
allowlists remain an outer gate applied to everyone.

A new `Role` (an enum: `RoleOwner` / `RoleGuest`) is resolved **in the bot**,
where both the allowlist and the author's guild roles are available, and carried
on `session.Request` (a plain enum so `session` stays free of discord types).
Resolution per inbound message:

1. author ∈ owners → `RoleOwner`.
2. else author has a role ∈ `guest_role_ids` **and** passes the guild/channel
   gate → `RoleGuest`.
3. else → rejected (`🦆 not authorized`, as today).

Member roles ride along on the gateway events — `m.Member.Roles` on
`MessageCreate`, `r.Member.Roles` on `MessageReactionAdd` — so no extra REST
lookups are needed. `mentionsBot`, `allows`, and the role-resolution helper sit
together in `bot.go`.

**Own-session-only for guests.** Today any authorized user can feed
(`authorizedThread`) or stop (`authorizedReaction`) **any** tracked session. The
session record already stores `Origin.AuthorID`. Guests are restricted to
sessions whose recorded author is themselves; owners retain access to anything.
This prevents guests from interfering with each other's or the owner's sessions.

### 2. The guest sandbox (`internal/sandbox` — new)

A guest session is **two containers on a private per-session Docker network**:

- **Agent container** (unprivileged). Holds the clone (rw), the injected
  credentials (see §4), and the agent CLI; this is where `claude`/`codex` runs,
  one `docker exec` per turn. Joined to an **`internal: true`** Docker network so
  it has **no direct route to the internet** — its only reachable peers are the
  egress proxy and the dind sidecar.
- **dind sidecar** (`docker:dind`, `--privileged`). Runs an inner dockerd. The
  agent container sets `DOCKER_HOST=tcp://<sidecar>:2376` (TLS) so a guest's
  `docker build` / `docker run` hits the **inner** daemon, never the host's
  socket. The sidecar holds no secrets and no host bind-mounts, so it is allowed
  broader egress (registry pulls for builds).

Lifecycle: the container pair + network + a named volume are created when a guest
session starts; each turn is a `docker exec` into the agent container (so claude
`--resume` / codex thread state persists across turns on the volume). `/stop`,
thread archive, or `StopByMessage` tears the whole set down. Owner sessions
create **none** of this — they run exactly as today.

The image is a project artifact (`hack/sandbox/Dockerfile`): a small base with
`git`, `gh`, the Docker CLI, a JS runtime, common build tools, and the `claude` +
`codex` CLIs. Building/refreshing it is a documented setup step.

### 3. Network egress (allow-listed)

Egress for the **agent container** is forced through a small allow-listing
forward proxy (the agent container has no other route out, being on the internal
network). The agent CLIs honor `HTTPS_PROXY`/`HTTP_PROXY`, so this is
agent-agnostic. The proxy permits only:

- the model API hosts (Anthropic / OpenAI auth + inference),
- the GitHub hosts needed to push and use `gh`.

Exact host list is tuned during implementation (claude/codex each touch a couple
of auxiliary domains). The dind sidecar uses a separate, broader egress so
`docker build` can pull base images; it is acceptable because the sidecar carries
no secrets. (Per the threat model, a guest *can* route their own injected token
out via a sidecar-run container; the defense for those credentials is revocation,
not egress.)

### 4. Credentials (injected, minimal — not bulk-mounted)

Nothing from the owner's home is bulk-mounted. The sandbox is given exactly:

- **Model auth** — the claude/codex credential, mounted **read-only as the single
  credential file** (e.g. `~/.codex/auth.json`, claude's credential file), *not*
  the whole `~/.claude.json` (which carries the owner's project list/history).
  Exact minimal paths confirmed during implementation.
- **Git identity** — `GIT_AUTHOR_*` / `GIT_COMMITTER_*` env (or a generated
  minimal gitconfig), not the owner's `~/.gitconfig`.
- **Push credential** — a **fine-grained GitHub PAT** supplied to quack via
  config/env and written into the container's git credential store (HTTPS) plus
  `GH_TOKEN` for `gh`, at create time. Per-session, scoped, revocable; the
  owner's SSH private key never enters any container.

### 5. Workspace rules for guests (`internal/session` `prepare`)

`prepare`'s target handling is gated on role:

| Target | Owner | Guest |
|--------|-------|-------|
| repo ref (`owner/repo`) | clone + worktree (today) | **fresh clone into the sandbox volume** |
| _(no target)_ | shared scratch dir | **empty sandbox container** (isolated Q&A, no repo) |
| filesystem path | direct/worktree | **rejected** (host filesystem) |
| `temp-dir` | host temp dir | **rejected** (host filesystem escape) |
| `no-wt` flag | honored | **ignored** (guests are always isolated) |

So a guest can target a repo (cloned fresh inside the jail) or ask a quick
question in an empty jail; they can never name a host path or the host temp/scratch
dirs. The clone is fetched fresh from the remote so no owner local-only branches
leak in.

### 6. Forced headless

Guests can never reach the interactive `tmux` branch (`service.go:294`). The
clamp is applied **after** inference, not trusted from it — the fluent infer step
can set `headless:false` purely from a user *phrasing* "give me a tmux session",
so guest directives are normalized (`Headless = true`) right before launch, and
an explicit guest `no-headless` gets a muted "interactive mode is owner-only"
note rather than a host session.

### 7. Tools & skills

The agent already accepts `--permission-mode` / `--allowed-tools` /
`--settings` (claude). A **guest policy block** in config supplies guest-specific
`allowed_tools` / `disallowed_tools` and a skills allow/deny list, layered on top
of the agent config. Default guest policy blocks host-escaping skills (e.g.
`zed`, which opens an editor on the host) and allows safe ones (e.g. `revue`).
The exact claude matcher syntax for per-skill allow/deny is confirmed during
implementation; codex exposes no skills, so this is claude-only. (Inside the jail
`zed` is largely inert anyway, but it is still withheld.)

### 8. Code seam: a per-session launcher (`internal/agentproc`, `internal/session`)

The single technical seam that makes guest turns run in a container without
touching the streaming/resume logic: a **`Launcher`** that turns a (program,
args, workdir, env) into the `*exec.Cmd` the driver runs.

- `direct` launcher → `exec.CommandContext(ctx, program, args...)` with
  `cmd.Dir = workdir` (today's behavior verbatim).
- `container` launcher → `docker exec -w <container-path> -i <agent-container>
  program args...`.

The two drivers (`Claude.RunTurn`, `Codex.RunTurn`) replace their direct
`exec.CommandContext` call with `t.launcher.Command(...)`; everything else
(argv construction, stream parsing, `TurnDone`) is unchanged. The launcher is
carried on the headless `liveSession` (owner → direct, guest → container) and
defaults to `direct` when unset, so owner and naming/infer one-shots are
byte-for-byte as today. **Only guest `RunTurn`s are containerized**; the
read-only infer/naming one-shots stay on the host (they are quack's routing
brain, run with owner creds, plan-mode).

### 9. Restart resilience (`internal/session/persist.go`)

The persisted `session.json` gains: `role`, launcher kind, and the
container/sidecar/network/volume names. On `Rehydrate`, a guest session
reconstructs its container launcher; if the containers are gone (host reboot) but
the named volume survives, the pair is recreated and bound to the existing
volume (which holds the clone + agent session state), so the conversation
resumes on the next reply exactly as the owner path does. Records whose volume is
also gone are skipped (as today for missing worktrees). `/stop` removes the
volume too.

### 10. Wiring (`cmd/quack/main.go`)

`main.go` builds a launcher factory from config (image name, proxy address,
credential paths, PAT, dind settings, guest tool/skill policy) and injects it
into `session.Service`. With no `guest_role_ids` configured, the factory is never
exercised and quack behaves exactly as before — the whole feature is inert until
the owner opts in.

## Config (new)

```toml
[discord]
owner_user_ids = ["OWNER_ID"]          # full access; allowed_user_id(s) also = owner
guest_role_ids = ["GUEST_ROLE_ID"]     # members get the sandbox

[guest]                                 # only consulted for guest sessions
image          = "quack-sandbox:latest"
github_pat     = "github_pat_..."       # fine-grained PAT for push/gh (or via env)
egress_allow   = ["api.anthropic.com", "api.openai.com", "github.com", "api.github.com"]
allowed_tools  = ""                     # optional claude tool allow-list for guests
disallowed_skills = ["zed"]
allowed_skills    = ["revue"]
# dind/proxy/credential-path knobs with sensible defaults
```

Documented in `config.example.toml` and `AGENTS.md`.

## Testing

Consistent with the existing fake-based `session` tests (no Docker, no network):

- **Role resolution** (`internal/discord`): owner id → owner; guest role → guest;
  neither → rejected; guild/channel gate still applies.
- **Guest clamps** (`internal/session`, fake launcher): `no-headless` forced to
  headless; filesystem-path / `temp-dir` targets rejected; `no-wt` ignored;
  repo target → container-prepare path; no target → empty-sandbox path.
- **Own-session-only**: a guest cannot feed/stop another author's session; owner
  can.
- **Launcher seam** (`internal/agentproc`): a fake launcher records the argv and
  workdir; the `direct` path is unchanged; the `container` path wraps with
  `docker exec`.
- **Persistence/rehydrate**: a guest record round-trips role + container/volume
  names and rebuilds the launcher.
- **Integration** (`QUACK_INTEGRATION=1`, needs Docker): real create → exec →
  push (against a throwaway remote) → teardown of the container pair + volume.

## Trade-offs & open items

- **dind sidecar is privileged.** The owner chose it over Sysbox (unsupported on
  Fedora). The agent container stays unprivileged and the host socket is never
  exposed, but the sidecar's `--privileged` is the residual host-boundary risk;
  documented, accepted.
- **Shared credentials are exfiltratable by a determined guest** (see threat
  model). Mitigation is scoping + revocability (fine-grained PAT, revocable model
  auth), not unreachability.
- **Per-turn `docker exec` latency** and image upkeep are the operational costs of
  isolation; a long-lived per-session container (vs per-turn) keeps the inner
  dockerd warm.
- **Items to confirm during implementation:** minimal claude/codex credential
  file paths; exact egress host list per agent; claude per-skill allow/deny
  matcher syntax; dind TLS wiring; image contents.
