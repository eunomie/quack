# Sandbox improvements: dagger-dedicated guest environments

Date: 2026-06-17
Status: accepted

## Problem

The guest sandbox (the unprivileged container a guest's claude/codex turns are
`docker exec`'d into) is a generic clone-any-repo jail. In practice these
sandboxes are dedicated to working on `dagger/dagger`, via the bot's fork
(`eunomie-quack/dagger`), and agents inside them should follow the same
contribution conventions the owner uses (stg patches, sign-off, no AI credit).

Today none of that is wired up: dagger is pinned to an old version, `stg` is
absent, a fresh clone keeps `origin` pointed at the upstream source (so a guest
push would target `dagger/dagger` directly), there is no default repo, and the
agents get no contribution guidance.

All changes are **guest-sandbox only**; owner sessions are unaffected.

## Changes

### 1. Image (`hack/sandbox/Dockerfile`)

- **dagger**: drop the `DAGGER_VERSION=v0.21.4` pin; install the latest release
  (the install script defaults to latest when `DAGGER_VERSION` is empty). The
  build arg stays so a specific version can still be forced.
- **gh**: already installed — unchanged.
- **stg**: add `stgit` to the base `apt-get install` line.

### 2. Fork remote setup on provision (`internal/sandbox/sandbox.go`)

> **Superseded (2026-06-18):** the fork workflow was dropped in favour of
> branching directly on the source repo — see
> [`2026-06-18-sandbox-direct-branches.md`](2026-06-18-sandbox-direct-branches.md).
> The section below is kept for historical rationale.

New `Spec.ForkOwner` field (sourced from config, default `eunomie-quack`). After
a successful clone of `<owner>/<repo>`, when `ForkOwner` is set:

```
git remote rename origin upstream
git remote add origin https://github.com/<ForkOwner>/<repo>.git
```

Result for dagger: `origin = eunomie-quack/dagger` (where the guest pushes,
authenticated by the injected PAT), `upstream = dagger/dagger` (the source). The
repo name is the clone's basename; the fork URL reuses the standard
`https://github.com/` host. Skipped for empty sandboxes (nothing cloned).

### 3. Default repo = `dagger/dagger` (`internal/session/guest.go`, config)

New `[guest] default_repo` (default `dagger/dagger`). When a guest starts a
sandbox with **no target**, `prepareGuest` clones `default_repo` instead of
standing up an empty sandbox. Explicit targets (`owner/repo`) still win, and a
guest can still get an empty sandbox by being unable to target host paths (that
rejection is unchanged).

The owner-shared infer one-shot is **not** modified — biasing it would leak the
dagger default into owner routing. The "dedicated to dagger/dagger" steering is
realized by (a) this clone default and (b) the guidance file below, which the
in-sandbox agent reads as system-level context.

### 4. Agent contribution guidance (`internal/sandbox/sandbox.go`)

At provision time, write a guidance document into the sandbox home so every
in-sandbox turn picks it up without clobbering the cloned repo's own
`AGENTS.md`:

- `/root/.claude/CLAUDE.md` (claude's global user instructions)
- `/root/.codex/AGENTS.md` (codex's global guidance)

Both hold the same adapted content:

- This sandbox is dedicated to working on `dagger/dagger` by default (pushes go
  to the `origin` fork `eunomie-quack/dagger`; `upstream` is `dagger/dagger`).
- Use **stg** (Stacked Git) for patches; the `stg` CLI is installed.
- **Always** add `Signed-off-by: <git_user_name> <<git_user_email>>` to every
  patch/commit. The identity is **templated from the configured guest identity**
  (`git_user_name`/`git_user_email`); no identity is hard-coded in the sources, so
  a different deployment signs off as its own identity without code changes.
- **Never** add `Co-Authored-By`, "Generated with Claude", or any AI-agent
  attribution to commits, patches, PR titles, or PR bodies.
- **Never** `git push` or open a PR without explicit approval.

The content is a Go template filled with the guest identity at provision time,
written via a quoted heredoc (no shell interpolation of the file body itself).

### 5. Dagger Cloud authentication (already wired — documented for completeness)

No code change. The existing `[guest] cred_files` mechanism already copies the
host's `~/.config/dagger/credentials.json` and `~/.config/dagger/org` into the
sandbox's `/root/.config/dagger/`, and the egress allow-list already permits
`dagger.cloud`/`api.dagger.cloud`/`auth.dagger.cloud`. So in-sandbox `dagger`
runs authenticate to dagger Cloud with the host's credentials and land in the
host's org. Verified present in the live config; the `config.example.toml`
comment block documents it.

## Config surface (`internal/config/guest.go`, `config.example.toml`)

```toml
[guest]
fork_owner   = "eunomie-quack"   # origin fork owner; clones rewrite origin->upstream, fork->origin
default_repo = "dagger/dagger"   # cloned when a guest gives no target
```

Defaults applied in `Guest.WithDefaults`. Threaded through
`session.GuestPolicy` → `SandboxSpec` → `sandbox.Spec` like the existing guest
fields.

## Testing

- `sandbox_test.go`: provision with `ForkOwner` rewrites remotes
  (`remote rename origin upstream`, `remote add origin .../eunomie-quack/<repo>`);
  empty sandbox does neither; provision writes the guidance files to
  `/root/.claude/CLAUDE.md` and `/root/.codex/AGENTS.md`.
- `guest` (session) test: no target + `default_repo` set clones the default repo
  and derives the fork; explicit target unchanged.
- `config` test: defaults for `fork_owner` / `default_repo`.
