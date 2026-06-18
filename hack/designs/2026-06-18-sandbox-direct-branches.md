# Sandbox: direct branches on the source repo (drop the fork workflow)

Date: 2026-06-18
Status: accepted

Supersedes the fork-remote portion of
[`2026-06-17-sandbox-dagger-defaults.md`](2026-06-17-sandbox-dagger-defaults.md)
(section 2). Everything else in that doc — the dagger-dedicated default repo,
the latest-dagger/stg image, the agent guidance file, dagger Cloud auth — stands.

## Problem

The original guest-sandbox design pushed guest work through a fork
(`eunomie-quack/dagger`): after cloning `dagger/dagger`, the provisioner renamed
`origin`→`upstream` and added `eunomie-quack/<repo>` as the new `origin`, so guest
pushes landed on the fork. That mirrored a fork-PR contribution flow.

The injected GitHub token has since been replaced with a PAT carrying
`public_repo` scope whose owner can push to `dagger/dagger` directly. With that
token a guest can do exactly what the owner does by hand: create a branch on the
source repo and open a PR from it. The fork indirection is now pure friction.

## Change

Guest sandboxes branch **directly on the source repo**; there is no fork.

- **`internal/sandbox/sandbox.go`** — `Spec.ForkOwner` removed. `Provision` no
  longer rewrites remotes after a clone: `origin` stays pointed at the clone
  source (e.g. `dagger/dagger`). The injected PAT authenticates pushes to it.
- **Agent guidance** (`guidanceText`) — replaced the "origin is your fork,
  upstream is the source" paragraph with: `origin` is the source itself, push a
  branch there and open a PR from it (no fork), and pick a unique branch name to
  avoid clashing with other branches on the shared repo.
- **Config** (`internal/config/guest.go`, `config.example.toml`) — `fork_owner`
  removed (field + the `eunomie-quack` default). `default_repo` is unchanged.
- **Plumbing** — `ForkOwner` dropped from `session.SandboxSpec`,
  `session.GuestPolicy`, `sandbox.Spec`, the adapter, and `main.go` wiring.

All changes are guest-sandbox only; owner sessions are unaffected. The host-side
`resolveBaseRef` origin→upstream resolution (`internal/session/service.go`) is
untouched — it serves owner fork checkouts on the host, independent of the
sandbox.

## Trade-off

Every guest now pushes branches into the one shared `dagger/dagger`, so branch
names can collide between sessions; the guidance asks agents to use descriptive,
unique names. This is the same exposure the owner already accepts pushing
branches by hand, and it is the explicit goal: guests contribute the way the
owner does. Containment of the shared PAT is unchanged — it is scoped (a
`public_repo` PAT) and revocable, not unreachable (see the hardening threat
model).

## Testing

- `sandbox_test.go`: `TestProvisionKeepsOriginAtSource` asserts no
  `remote rename`/`remote add` after a clone; the guidance test still checks the
  file mentions `dagger/dagger`, `stg`, and the no-AI-credit rule.
- `config` test: `default_repo` default; explicit `default_repo` preserved.
- `session` guest test: no target + `default_repo` clones `dagger/dagger`.
