# Codex: permissive sandbox by default

Date: 2026-06-08

## Problem

Headless codex was invoked as `codex exec [--config model_reasoning_effort=…]
--json <prompt>` with **no `--sandbox` flag**, so codex fell back to its own
default sandbox. That sandbox confines writes to the working directory, which
fights two of quack's core workflows:

- **Worktrees.** A worktree's shared `.git` lives outside the workspace
  (`<clone>-worktrees/<name>`), so the sandbox blocks `git commit` and friends —
  the same reason claude's OS sandbox is deliberately left off (see the
  `config.example.toml` note).
- **Multi-repo tasks.** A task spanning several checkouts needs to touch
  directories outside the cwd, which the sandbox denies.

claude is invoked permissively (`permission_mode=auto`, and
`--dangerously-skip-permissions` interactively) and relies on quack-level
controls (the `!` explicit grammar, the allowlist) for restriction. Codex should
align: permissive by default, restrict at the quack layer rather than via codex's
implicit sandbox.

## Design

Mirror claude's existing per-agent knob pattern.

- **Config.** Add a codex-only `sandbox_mode` field to `agent.Agent`
  (`read-only | workspace-write | danger-full-access`). `Agent.Sandbox()`
  defaults it to `danger-full-access` when unset — permissive, claude parity —
  and is overridable to tighten.
- **Driver.** `agentproc.Codex` gains a `SandboxMode` field. When non-empty it
  emits `--sandbox <mode>` immediately after `exec`, **before** any `resume`
  subcommand (codex parses the option on the parent command), on every headless
  invocation: first turn, resume, and the `OneShot`/naming path. Empty means no
  flag (codex's own default) — used only by callers that don't set it.
- **Wiring.** `main.go`'s codex driver case passes `SandboxMode: a.Sandbox()`,
  so the default applies unless config overrides it.
- **Interactive (tmux).** `Agent.interactiveArgs` now defaults codex to
  `--dangerously-bypass-approvals-and-sandbox` (the claude case already defaults
  to `--dangerously-skip-permissions`), overridable via `interactive_args`. Same
  worktree/multi-repo reasoning applies to the live tmux path.

`codex exec` is already non-interactive, so it never prompts for approvals; the
sandbox was the only thing in the way. `danger-full-access` removes it.

## Restriction story

Restriction moves up a layer, to quack: the `!` explicit grammar and the
user/guild/channel allowlist gate who can launch what. A host that still wants
codex to self-restrict sets `sandbox_mode = "workspace-write"` (or `"read-only"`)
per agent.

## Tests

- `agent`: `Sandbox()` default + override; codex interactive argv now includes
  the bypass flag.
- `agentproc`: `--sandbox` placement on first turn and before `resume`; empty
  `SandboxMode` adds no flag.
