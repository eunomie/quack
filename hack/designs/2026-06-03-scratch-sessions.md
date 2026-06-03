# Scratch sessions for quick questions

Date: 2026-06-03

## Problem

To ask quack a quick, repo-less question you had to put the prompt on the line
*after* the mention (`@quack ⏎ <question>`), because the first line is always
parsed as the directive. A natural one-liner like `@quack how do I rebase?`
errored — `how` was read as the target, `do` as an unexpected token. And the
no-target case dropped the agent into a throwaway `os.MkdirTemp` dir, so nothing
written there survived or was easy to find later.

## Goal

Make "quick question" the path of least resistance: a bare one-line mention just
works, runs in a stable scratch workspace, and uses a moderate effort by default.

## Design

### Parsing (`internal/command/directive.go`)

- **Single-line mention** (no newline anywhere): the entire message is the
  prompt, with no directive parsing. This is purely additive — a single line
  previously always errored with "missing prompt" (the prompt lived after the
  first newline), so no working invocation changes meaning.
- **Multi-line mention**: unchanged — line 1 is the directive, the rest is the
  prompt.

Consequence: directives (repo/path, flags, `temp-dir`) require the multi-line
form. That's the simple, teachable rule — *one line = a question; multiple lines
= a command plus a prompt.*

### Workspace routing (`prepare` in `internal/session/service.go`)

The no-target branch splits into two:

| Target | Workspace | Effort |
|--------|-----------|--------|
| `""` (no target) | `scratch_dir` (`~/dev/work`), non-isolated, mkdir-on-miss | `medium` (default; overridable via `effort=`) |
| `temp-dir` (reserved) | fresh `os.MkdirTemp`, non-isolated | agent default — a faithful "old behavior" escape hatch |
| path / repo ref | as before | as before |

`temp-dir` is intercepted in `prepare` before path/ref routing (it isn't a path
and would otherwise fail `ParseRef`). It naturally yields the bare-token
provisional name, exactly like the old temp dir.

### Effort default

`scratchEffort = "medium"` is applied in `Handle` only when the target is empty
*and* no `effort=` was given, so the multi-line form can still say `effort=high`.
A single-line question always gets `medium` (there's nowhere to put a flag).

### Config (`internal/config`)

New `scratch_dir` field, `expandHome`-d, defaulting to `~/dev/work` (mirrors the
existing `dev_src_root` default pattern). Plumbed through `session.Config` and
`main.go`.

## Trade-offs

- **Shared scratch dir.** Concurrent scratch sessions share `~/dev/work` — fine
  for read-only Q&A, but two sessions writing files there can step on each other.
  Documented; if it bites, a later change can give each scratch session its own
  subdir.
- **Single line = always a prompt.** You can no longer pass a directive on one
  line. Acceptable: that form never worked anyway (it errored), and the
  two-line form covers every directive case.
