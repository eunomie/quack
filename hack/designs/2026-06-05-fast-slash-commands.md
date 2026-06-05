# Fast slash commands

Date: 2026-06-05

## Problem

Some Discord commands — notably `/revue` and `/open-zed` — are pure
side-effect launchers: they resolve a bit of context and exec a binary that
daemonizes and prints a URL. Today each one travels the full path:

```
Discord message → quack → headless agent turn → skill → script → binary
```

Spinning up an agent turn for what is, on a shell, just `open-zed` adds seconds
of latency for zero added value — the agent makes no decisions worth making.

We want these to be **damn fast**: recognize the command in quack and exec the
underlying script directly, skipping the agent entirely.

## Key insight: the working directory is unambiguous

The original worry was "the agent may have changed directory since the session
started, so quack won't know where to run the script." In quack's headless
model this concern is moot:

- A `liveSession`'s `workdir` is fixed at creation (the worktree path) and
  persisted to `session.json`; it never changes during the session.
- Every turn is a fresh child process launched with `Workdir: ls.workdir`, so
  the agent's cwd resets to the worktree at the start of each turn. There is no
  hidden "current directory" that quack fails to track.

So inside a tracked session thread, `ls.workdir` *is* the correct directory.
Targeting a subdirectory is only ever done by passing an explicit argument.

quack runs as the user's systemd **user** unit — same user, same environment
(`XDG_RUNTIME_DIR`, systemd access) the skills rely on — so it can exec
`revue` / `open-zed.sh` directly with the same results the skill would get.

## Scope

Fast commands fire **only inside an already-tracked session thread**, where the
directory is unambiguous. Brand-new mentions (`@quack /open-zed` with no
session) are out of scope: with no session there is no unambiguous directory,
which reintroduces exactly the ambiguity the thread case avoids. The skills
remain available for every other context.

## Design

### 1. Config (`internal/config`)

A new repeatable block maps a trigger to an argv to exec:

```toml
[[fast_commands]]
trigger = "/revue"
argv    = ["/home/yves/.claude/skills/revue/revue-start.sh"]

[[fast_commands]]
trigger = "/open-zed"
argv    = ["/home/yves/.claude/skills/open-zed/open-zed.sh"]
```

quack execs `argv` with the user's trailing text appended (whitespace-split
into tokens), `cwd = session workdir`. No placeholders, no shell — direct
exec, matching the existing "tmux argv is exec'd directly, no escaping needed"
convention. quack owns no resolution logic (base branch, port, …); whatever
smarts a command needs live in the script that `argv` points at.

### 2. Matching & interception

In `discord.Bot.onMessage`, the tracked-thread branch already routes to
`FeedThread` / `/stop` / `/attach`. Fast commands slot in there, **before**
`FeedThread`:

- Trim the message text.
- If its **first whitespace-delimited token** exactly equals a configured
  `trigger`, it is a fast command; the rest of the line is the argument string.
- Otherwise fall through to the agent as today (a normal message, or the
  command appearing mid-sentence, is *not* intercepted).

### 3. Execution (preserving the DI seam)

Matching and exec live in the `session` service, where `liveSession.workdir` is
known — a new method:

```go
func (s *Service) RunFastCommand(ctx context.Context, threadID, text string) (handled bool)
```

To exec without `session` importing `os/exec`, add one consumer-side interface
and inject an adapter from `main.go`, mirroring `Git`/`Tmux`/`Replier`:

```go
type Runner interface {
    Run(ctx context.Context, dir string, argv []string) (output []byte, err error)
}
```

The adapter runs the argv via `os/exec` with `Dir = dir`, returning combined
stdout+stderr. `RunFastCommand`:

1. Looks up the `liveSession` by `threadID`; if none, returns `handled=false`.
2. Matches `text` against configured triggers; no match → `handled=false`.
3. On a match: builds `argv = command.argv + splitArgs(rest)`, runs it in a
   goroutine with a ~30s timeout (`context.WithTimeout`), `dir = ls.workdir`.
4. Bypasses the turn queue and `sessionRef` entirely — it never touches the
   agent.

### 4. Output back to Discord

- React 👀 on the user's message when the command starts.
- Post the combined output to the thread, reusing the existing `discordMax`
  splitter in `render.go`. The `revue` / `open-zed` URL lines land in the
  thread exactly as the skill prints them today.
- React ✅ on a zero exit, ❌ on a non-zero exit or timeout — with the output
  still posted so the error is visible.

### 5. `revue-start.sh` extraction

`open-zed` already ships a standalone `open-zed.sh`. `revue`'s prep (base-branch
resolution, port selection, per-repo pidfile, `setsid` of the binary) currently
lives inline in its `SKILL.md`. Extract it into
`~/.claude/skills/revue/revue-start.sh` and have the skill call it, so the
script is the single source of truth shared by the skill and the fast path.

## Testing

Add a fake `Runner` to `internal/session/fakes_test.go` and unit-test
`RunFastCommand`:

- trigger match vs. no-match (no-match → `handled=false`, falls through);
- argument splitting (`/revue main` → argv tail `["main"]`);
- `cwd` passed to the runner equals the session `workdir`;
- success → ✅ reaction + posted output;
- non-zero exit / timeout → ❌ reaction + posted output;
- unknown trigger → `handled=false`.

The `session` package stays unit-testable with fakes; no real git/tmux/Discord
or external binary needed.

## Non-goals

- Fast commands for brand-new mentions (no session → ambiguous directory).
- quack owning any command-specific resolution logic.
- Removing or changing the skills' agent-driven behavior; the fast path is a
  shortcut, not a replacement.
