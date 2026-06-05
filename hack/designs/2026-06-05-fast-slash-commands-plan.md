# Fast slash commands — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let quack exec a configured binary directly (skipping the agent) when a message in a tracked session thread starts with a configured trigger like `/revue` or `/open-zed`.

**Architecture:** A new consumer-side `Runner` interface in `internal/session` (os/exec adapter in `internal/cmdexec`) plus a `Service.RunFastCommand` method. The Discord bot intercepts tracked-thread messages whose first token matches a configured `FastCommand` trigger, runs the argv in the session's `workdir`, and posts the output back — never touching the agent turn queue or resume token. A `revue.sh` script becomes the single source of truth shared by the `/revue` skill and the fast path.

**Tech Stack:** Go (stdlib `os/exec`), BurntSushi/toml config, bash for the extracted script.

**Spec:** `hack/designs/2026-06-05-fast-slash-commands.md`

**Version control:** This repo uses Stacked Git. Each task ends with a patch carrying a `Signed-off-by: Yves Brissaud <yves.brissaud@gmail.com>` trailer and **no** AI `Co-Authored-By` line. Use the `stg` skill if unsure; the literal commands below are a guide.

---

## File Structure

In-repo (Go, committed via stg):

- **Create** `internal/cmdexec/cmdexec.go` — os/exec adapter implementing `session.Runner`. One responsibility: exec an argv in a dir, return combined output.
- **Create** `internal/session/fastcmd.go` — `FastCommand` type, `Runner` interface, `UseRunner`, `matchFastCommand`, `RunFastCommand`, `execFastCommand`.
- **Create** `internal/session/fastcmd_test.go` — unit tests.
- **Modify** `internal/session/service.go` — add `runner Runner` and `FastCommands []FastCommand` to the structs.
- **Modify** `internal/session/fakes_test.go` — add `fakeRunner`.
- **Modify** `internal/discord/bot.go` — intercept in the tracked-thread branch.
- **Modify** `internal/config/config.go` — `FastCommand` toml struct + `FastCommands` field.
- **Modify** `cmd/quack/main.go` — map config → session, wire the runner.
- **Modify** `config.example.toml` — commented `[[fast_commands]]` example.
- **Modify** `AGENTS.md` — document the fast-command path.

Outside the repo (NOT committed via stg — these live under `~/.claude/skills`):

- **Create** `~/.claude/skills/revue/revue.sh` — extracted start/stop/status logic.
- **Modify** `~/.claude/skills/revue/SKILL.md` — delegate to `revue.sh`.

---

## Task 1: `cmdexec` adapter

**Files:**
- Create: `internal/cmdexec/cmdexec.go`

- [ ] **Step 1: Write the adapter**

```go
// Package cmdexec runs configured fast-command binaries via os/exec. It is the
// concrete adapter for session.Runner, injected by main.go, keeping the session
// core free of os/exec — mirroring gitexec/tmuxexec.
package cmdexec

import (
	"context"
	"errors"
	"os/exec"
)

// Runner execs commands in a working directory, returning combined output.
type Runner struct{}

// New builds a Runner.
func New() *Runner { return &Runner{} }

// Run execs argv with cwd=dir and returns its combined stdout+stderr. The
// context bounds the run (callers apply a timeout); an empty argv is an error.
func (r *Runner) Run(ctx context.Context, dir string, argv []string) ([]byte, error) {
	if len(argv) == 0 {
		return nil, errors.New("empty argv")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	return cmd.CombinedOutput()
}
```

- [ ] **Step 2: Verify it builds**

Run: `go build ./internal/cmdexec/`
Expected: no output (success).

- [ ] **Step 3: Commit**

```sh
stg new fastcmd-cmdexec -m "cmdexec: add os/exec runner adapter

Signed-off-by: Yves Brissaud <yves.brissaud@gmail.com>"
stg refresh
```

---

## Task 2: `FastCommand` type, `Runner` interface, and matching

This task adds the core types and the pure matching helper, test-first. `RunFastCommand`/`execFastCommand` come in Task 3.

**Files:**
- Create: `internal/session/fastcmd.go`
- Modify: `internal/session/service.go` (struct fields)
- Modify: `internal/session/fakes_test.go` (fakeRunner)
- Test: `internal/session/fastcmd_test.go`

- [ ] **Step 1: Add struct fields in `service.go`**

In the `Config` struct (after `Agents map[string]agent.Agent`), add:

```go
	// FastCommands are trigger→argv launchers run directly (bypassing the agent)
	// when their trigger is the first word of a tracked-thread message.
	FastCommands []FastCommand
```

In the `Service` struct (after `history History`), add:

```go
	runner  Runner
```

- [ ] **Step 2: Create `internal/session/fastcmd.go` with types + match only**

```go
package session

import (
	"context"
	"strings"
	"time"
)

// FastCommand maps a trigger token (e.g. "/revue") to an argv that quack execs
// directly — bypassing the agent — when the trigger is the first word of a
// message in a tracked session thread. The user's trailing tokens are appended
// to Argv as arguments; the command runs with cwd = the session's workdir.
type FastCommand struct {
	Trigger string
	Argv    []string
}

// Runner execs a command in a directory and returns its combined output. The
// os/exec adapter lives in internal/cmdexec; session depends only on this
// interface so it stays unit-testable with a fake.
type Runner interface {
	Run(ctx context.Context, dir string, argv []string) ([]byte, error)
}

// UseRunner injects the command runner used for fast commands.
func (s *Service) UseRunner(r Runner) { s.runner = r }

// fastCommandTimeout bounds a fast command. revue/open-zed daemonize and return
// within a few seconds; the ceiling just stops a wedged binary from hanging.
const fastCommandTimeout = 30 * time.Second

// matchFastCommand reports whether text's first whitespace-delimited token is a
// configured trigger. On a match it returns the command and the remaining tokens
// as its arguments. A trigger appearing anywhere but first does not match.
func (s *Service) matchFastCommand(text string) (FastCommand, []string, bool) {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return FastCommand{}, nil, false
	}
	for _, fc := range s.cfg.FastCommands {
		if fields[0] == fc.Trigger {
			return fc, fields[1:], true
		}
	}
	return FastCommand{}, nil, false
}
```

- [ ] **Step 3: Add `fakeRunner` to `internal/session/fakes_test.go`**

Append at the end of the file:

```go
type fakeRunner struct {
	dir  string
	argv []string
	out  []byte
	err  error
	runs int
}

func (f *fakeRunner) Run(ctx context.Context, dir string, argv []string) ([]byte, error) {
	f.runs++
	f.dir = dir
	f.argv = argv
	return f.out, f.err
}
```

- [ ] **Step 4: Write the matching test in `internal/session/fastcmd_test.go`**

```go
package session

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fastService builds a minimal Service with one configured fast command and one
// tracked live session ("thread-1", workdir "/work"), plus the given runner
// behaviour. Constructed as a struct literal (in-package) so no real
// git/tmux/driver is needed.
func fastService(out []byte, runErr error) (*Service, *fakeReplier, *fakeRunner) {
	r := newFakeReplier()
	run := &fakeRunner{out: out, err: runErr}
	svc := &Service{
		cfg: Config{
			FastCommands: []FastCommand{
				{Trigger: "/revue", Argv: []string{"/bin/revue.sh"}},
			},
		},
		reply:  r,
		runner: run,
		sessions: map[string]*liveSession{
			"thread-1": {threadID: "thread-1", workdir: "/work", name: "sess"},
		},
	}
	return svc, r, run
}

func has(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func TestMatchFastCommand(t *testing.T) {
	svc, _, _ := fastService(nil, nil)
	cases := []struct {
		text     string
		want     bool
		wantArgs string // comma-joined
	}{
		{"/revue", true, ""},
		{"/revue main", true, "main"},
		{"/revue from workspace branch", true, "from,workspace,branch"},
		{"please /revue", false, ""},
		{"/open-zed", false, ""},
		{"", false, ""},
	}
	for _, c := range cases {
		fc, args, ok := svc.matchFastCommand(c.text)
		if ok != c.want {
			t.Fatalf("%q: ok=%v want %v", c.text, ok, c.want)
		}
		if !ok {
			continue
		}
		if fc.Trigger != "/revue" {
			t.Fatalf("%q: trigger=%q", c.text, fc.Trigger)
		}
		if got := strings.Join(args, ","); got != c.wantArgs {
			t.Fatalf("%q: args=%q want %q", c.text, got, c.wantArgs)
		}
	}
}
```

- [ ] **Step 5: Run the test**

Run: `go test ./internal/session/ -run TestMatchFastCommand -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```sh
stg new fastcmd-types -m "session: add FastCommand type, Runner seam, and matcher

Signed-off-by: Yves Brissaud <yves.brissaud@gmail.com>"
stg refresh
```

---

## Task 3: `RunFastCommand` + `execFastCommand`

**Files:**
- Modify: `internal/session/fastcmd.go`
- Test: `internal/session/fastcmd_test.go`

- [ ] **Step 1: Write the execution tests** (append to `fastcmd_test.go`)

```go
func TestRunFastCommand_NoMatch(t *testing.T) {
	svc, _, run := fastService(nil, nil)
	if svc.RunFastCommand(context.Background(), "thread-1", "msg-1", "hello there") {
		t.Fatal("non-trigger text should return false (fall through to the agent)")
	}
	if run.runs != 0 {
		t.Fatalf("runner should not run on a non-match: runs=%d", run.runs)
	}
}

func TestRunFastCommand_NoSession(t *testing.T) {
	svc, _, _ := fastService(nil, nil)
	if svc.RunFastCommand(context.Background(), "thread-unknown", "msg-1", "/revue") {
		t.Fatal("untracked thread should return false")
	}
}

func TestExecFastCommand_Success(t *testing.T) {
	svc, r, run := fastService([]byte("http://host:8080\n"), nil)
	ls := svc.sessions["thread-1"]

	svc.execFastCommand(context.Background(), ls, "msg-1",
		FastCommand{Argv: []string{"/bin/revue.sh"}}, []string{"main"})

	if run.dir != "/work" {
		t.Fatalf("cwd = %q, want /work", run.dir)
	}
	if got := strings.Join(run.argv, " "); got != "/bin/revue.sh main" {
		t.Fatalf("argv = %q", got)
	}
	if len(r.posts) != 1 || r.posts[0].content != "http://host:8080" {
		t.Fatalf("posts = %v", r.posts)
	}
	if !has(r.reacts, "thread-1|msg-1|👀") || !has(r.reacts, "thread-1|msg-1|✅") {
		t.Fatalf("reacts = %v", r.reacts)
	}
	if !has(r.unreacts, "thread-1|msg-1|👀") {
		t.Fatalf("unreacts = %v", r.unreacts)
	}
}

func TestExecFastCommand_Failure_PostsError(t *testing.T) {
	svc, r, _ := fastService(nil, errors.New("boom"))
	ls := svc.sessions["thread-1"]

	svc.execFastCommand(context.Background(), ls, "msg-1",
		FastCommand{Argv: []string{"/bin/revue.sh"}}, nil)

	if len(r.posts) != 1 || !strings.Contains(r.posts[0].content, "boom") {
		t.Fatalf("posts = %v", r.posts)
	}
	if !has(r.reacts, "thread-1|msg-1|❌") {
		t.Fatalf("reacts = %v", r.reacts)
	}
}

func TestExecFastCommand_Failure_PostsOutputNotError(t *testing.T) {
	svc, r, _ := fastService([]byte("port in use\n"), errors.New("exit 1"))
	ls := svc.sessions["thread-1"]

	svc.execFastCommand(context.Background(), ls, "msg-1",
		FastCommand{Argv: []string{"/bin/revue.sh"}}, nil)

	// When the command produced output, that output is shown (the error string is
	// redundant), still flagged with ❌.
	if len(r.posts) != 1 || r.posts[0].content != "port in use" {
		t.Fatalf("posts = %v", r.posts)
	}
	if !has(r.reacts, "thread-1|msg-1|❌") {
		t.Fatalf("reacts = %v", r.reacts)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/session/ -run TestRunFastCommand_NoMatch -v`
Expected: FAIL — `svc.RunFastCommand` undefined (compile error). (`execFastCommand` is also undefined.)

- [ ] **Step 3: Implement `RunFastCommand` and `execFastCommand`** (append to `fastcmd.go`)

```go
// RunFastCommand intercepts a tracked-thread message that names a fast command,
// running it directly instead of feeding it to the agent. It returns false when
// the message isn't a fast command (caller falls through to FeedThread) or the
// thread isn't a tracked session. The command runs in its own goroutine so the
// Discord gateway handler isn't blocked by a slow binary.
func (s *Service) RunFastCommand(ctx context.Context, threadID, messageID, text string) bool {
	fc, args, ok := s.matchFastCommand(text)
	if !ok {
		return false
	}
	s.hmu.Lock()
	ls, ok := s.sessions[threadID]
	s.hmu.Unlock()
	if !ok {
		return false
	}
	go s.execFastCommand(context.Background(), ls, messageID, fc, args)
	return true
}

// execFastCommand runs one fast command in the session's workdir and reports the
// result to the thread: 👀 while it runs, the command's combined output posted,
// then ✅ on success or ❌ on a non-zero exit / timeout. It touches neither the
// turn queue nor the agent's resume token. On a non-zero exit it shows the
// command's output when there is any, otherwise the error string.
func (s *Service) execFastCommand(ctx context.Context, ls *liveSession, messageID string, fc FastCommand, args []string) {
	_ = s.reply.React(ctx, ls.threadID, messageID, emojiWorking)

	argv := append(append([]string{}, fc.Argv...), args...)
	runCtx, cancel := context.WithTimeout(ctx, fastCommandTimeout)
	defer cancel()
	out, err := s.runner.Run(runCtx, ls.workdir, argv)

	_ = s.reply.Unreact(ctx, ls.threadID, messageID, emojiWorking)

	body := strings.TrimSpace(string(out))
	if body != "" {
		for _, chunk := range splitMessage(body, discordMax) {
			_, _ = s.reply.Post(ctx, ls.threadID, chunk)
		}
	}
	if err != nil {
		if body == "" {
			_, _ = s.reply.Post(ctx, ls.threadID, "❌ "+err.Error())
		}
		_ = s.reply.React(ctx, ls.threadID, messageID, emojiError)
		return
	}
	_ = s.reply.React(ctx, ls.threadID, messageID, emojiDone)
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/session/ -run 'TestRunFastCommand|TestExecFastCommand' -v`
Expected: PASS (all four).

- [ ] **Step 5: Run the full session package + vet**

Run: `go test ./internal/session/ && go vet ./internal/session/`
Expected: PASS, no vet warnings.

- [ ] **Step 6: Commit**

```sh
stg new fastcmd-run -m "session: run fast commands directly in the session workdir

Signed-off-by: Yves Brissaud <yves.brissaud@gmail.com>"
stg refresh
```

---

## Task 4: Discord interception

**Files:**
- Modify: `internal/discord/bot.go:79-83`

- [ ] **Step 1: Add the interception in the tracked-thread branch**

In `onMessage`, the tracked branch currently reads (around lines 79–83):

```go
		if content == "/attach" || strings.HasPrefix(content, "/attach ") {
			b.svc.PromoteThread(context.Background(), m.ChannelID)
			return
		}
		b.svc.FeedThread(context.Background(), m.ChannelID, m.ChannelID, m.ID, content, atts)
		return
```

Insert the fast-command check between the `/attach` block and the `FeedThread` call:

```go
		if content == "/attach" || strings.HasPrefix(content, "/attach ") {
			b.svc.PromoteThread(context.Background(), m.ChannelID)
			return
		}
		// Fast path: a configured trigger (e.g. /revue) runs its binary directly in
		// the session's workdir, skipping the agent. Falls through when the message
		// isn't a fast command.
		if b.svc.RunFastCommand(context.Background(), m.ChannelID, m.ID, content) {
			return
		}
		b.svc.FeedThread(context.Background(), m.ChannelID, m.ChannelID, m.ID, content, atts)
		return
```

- [ ] **Step 2: Verify the package builds**

Run: `go build ./internal/discord/`
Expected: no output (success).

- [ ] **Step 3: Commit**

```sh
stg new fastcmd-bot -m "discord: intercept fast commands in tracked threads

Signed-off-by: Yves Brissaud <yves.brissaud@gmail.com>"
stg refresh
```

---

## Task 5: Config plumbing + main wiring

**Files:**
- Modify: `internal/config/config.go`
- Modify: `cmd/quack/main.go`

- [ ] **Step 1: Add the config struct + field in `config.go`**

After the `Config` struct (before `Discord`), add the type:

```go
// FastCommand maps a trigger token to an argv quack execs directly in a tracked
// session thread, skipping the agent. Mirrors session.FastCommand; mapped to it
// in main.go.
type FastCommand struct {
	Trigger string   `toml:"trigger"`
	Argv    []string `toml:"argv"`
}
```

In the `Config` struct, add a field (after `Agents`):

```go
	FastCommands      []FastCommand          `toml:"fast_commands"`
```

- [ ] **Step 2: Map and wire in `main.go`**

Add the import (with the other `internal/*` imports):

```go
	"github.com/eunomie/quack/internal/cmdexec"
```

In the `scfg := session.Config{...}` literal, the `FastCommands` slice can't be a one-liner, so map it right after the literal:

```go
	for _, fc := range cfg.FastCommands {
		scfg.FastCommands = append(scfg.FastCommands, session.FastCommand{
			Trigger: fc.Trigger,
			Argv:    fc.Argv,
		})
	}
```

In the `svcFor` closure, after `svc.UseDrivers(drivers)`, wire the runner:

```go
		svc.UseRunner(cmdexec.New())
```

- [ ] **Step 3: Build the whole module**

Run: `go build ./...`
Expected: no output (success).

- [ ] **Step 4: Full test + vet**

Run: `go test ./... && go vet ./...`
Expected: PASS, no warnings.

- [ ] **Step 5: Commit**

```sh
stg new fastcmd-config -m "config: load fast_commands and wire the runner

Signed-off-by: Yves Brissaud <yves.brissaud@gmail.com>"
stg refresh
```

---

## Task 6: Docs — config example + AGENTS.md

**Files:**
- Modify: `config.example.toml`
- Modify: `AGENTS.md`

- [ ] **Step 1: Add a commented example to `config.example.toml`**

Append at the end of the file:

```toml

# Fast commands: a trigger token quack execs DIRECTLY in a tracked session
# thread, skipping the agent — for pure launchers like revue/open-zed where the
# agent round-trip adds latency for no decision. The user's trailing words become
# arguments; the command runs in the session's worktree. Only fires inside an
# existing session thread, where the directory is unambiguous.
# [[fast_commands]]
# trigger = "/revue"
# argv    = ["/home/you/.claude/skills/revue/revue.sh"]
#
# [[fast_commands]]
# trigger = "/open-zed"
# argv    = ["/home/you/.claude/skills/open-zed/open-zed.sh"]
```

- [ ] **Step 2: Document the path in `AGENTS.md`**

In the "Request flow" section, after the numbered list item 1 (the bullet that ends with the 🛑 reaction routing to `StopByMessage`), add a paragraph:

```markdown
   A message in a tracked thread whose **first word is a configured fast command**
   (`[[fast_commands]]`, e.g. `/revue`, `/open-zed`) is intercepted before
   `FeedThread`: quack execs the command's argv directly in the session's
   `workdir` (`Service.RunFastCommand` → `Runner`/`internal/cmdexec`), posts the
   output, and never starts an agent turn. The directory is unambiguous because a
   headless session's `workdir` is fixed and each turn resets the agent's cwd to
   it. See `hack/designs/2026-06-05-fast-slash-commands.md`.
```

- [ ] **Step 3: Commit**

```sh
stg new fastcmd-docs -m "docs: document fast_commands in config example and AGENTS.md

Signed-off-by: Yves Brissaud <yves.brissaud@gmail.com>"
stg refresh
```

---

## Task 7: `revue.sh` extraction (outside the repo — not stg)

This task touches `~/.claude/skills/revue/`, which is **not** part of this git repo. Do not create an stg patch for it; just write the files. `open-zed` already ships `open-zed.sh`, so it needs no change.

**Files:**
- Create: `~/.claude/skills/revue/revue.sh`
- Modify: `~/.claude/skills/revue/SKILL.md`

- [ ] **Step 1: Read the current skill** to confirm the logic being extracted

Run: `cat ~/.claude/skills/revue/SKILL.md`
Confirm the State / Start / Stop / Status / URL sections match what's ported below.

- [ ] **Step 2: Write `~/.claude/skills/revue/revue.sh`**

```sh
#!/usr/bin/env bash
# revue.sh — start/stop/status the revue diff-review server for the current git
# repo, printing the clickable URL. Single source of truth shared by the /revue
# skill and quack's fast path. State is keyed by repo toplevel so instances in
# different repos run in parallel without colliding.
#
# Usage:
#   revue.sh                    # start, auto-detect base
#   revue.sh <base> [port <n>]  # start against <base> (filler words ignored)
#   revue.sh stop
#   revue.sh status
set -u

STATE="${XDG_RUNTIME_DIR:-$HOME/.cache}/revue"
mkdir -p "$STATE"

REPO=$(git rev-parse --show-toplevel 2>/dev/null)
SLUG=$(printf '%s' "$(basename "$REPO")" | tr -c 'A-Za-z0-9._-' '-')
HASH=$(printf '%s' "$REPO" | sha256sum | cut -c1-12)
KEY="${SLUG}-${HASH}"
PIDFILE="$STATE/$KEY.pid"
LOGFILE="$STATE/$KEY.log"

REVUE=$(command -v revue || echo "$HOME/dev/bin/revue")

running() { [ -f "$PIDFILE" ] && kill -0 "$(cat "$PIDFILE")" 2>/dev/null; }

# url prints the clickable URL derived from the actual listening port in the log,
# preferring the Tailscale MagicDNS name so it works from other devices.
url() {
	local port ts
	port=$(grep -oE 'listening on http://localhost:[0-9]+' "$LOGFILE" | grep -oE '[0-9]+$' | head -1)
	[ -n "$port" ] || return 0
	ts=$(tailscale status --json 2>/dev/null | jq -r '.Self.DNSName // empty' | sed 's/\.$//')
	if [ -n "$ts" ]; then echo "http://$ts:$port"; else echo "http://localhost:$port"; fi
}

case "${1:-}" in
stop)
	if [ -z "$REPO" ] || ! running; then rm -f "$PIDFILE"; echo "not running"; exit 0; fi
	pid=$(cat "$PIDFILE"); kill "$pid" 2>/dev/null; sleep 1
	kill -0 "$pid" 2>/dev/null && kill -9 "$pid" 2>/dev/null
	rm -f "$PIDFILE"; echo "stopped"; exit 0 ;;
status)
	if [ -z "$REPO" ] || ! running; then echo "not running"; exit 0; fi
	url; exit 0 ;;
esac

# --- start ---
if [ -z "$REPO" ]; then echo "not in a git repo — revue reviews a git branch"; exit 1; fi
[ -x "$REVUE" ] || { echo "revue binary not found (looked on PATH and at $HOME/dev/bin/revue)"; exit 1; }
if running; then url; exit 0; fi
rm -f "$PIDFILE"

# Parse base + optional port, dropping filler words and leading dashes.
base=""; port=""; prev=""
for tok in "$@"; do
	case "$tok" in
	from|against|base|branch|on) prev=""; continue ;;
	port|--port) prev="port"; continue ;;
	esac
	if [ "$prev" = "port" ]; then port="${tok#--}"; prev=""; continue; fi
	t="${tok##-}"
	[ -n "$t" ] && [ -z "$base" ] && base="$t"
done

if [ -n "$base" ] && ! printf '%s' "$base" | grep -qE '^[A-Za-z0-9._/-]+$'; then
	echo "invalid base branch: $base"; exit 1
fi

args=()
[ -n "$base" ] && args+=("--base=$base")
[ -n "$port" ] && args+=("--port=$port")

setsid "$REVUE" "${args[@]}" >"$LOGFILE" 2>&1 </dev/null &
echo $! > "$PIDFILE"

for _ in $(seq 1 30); do
	kill -0 "$(cat "$PIDFILE")" 2>/dev/null || break
	grep -q "listening" "$LOGFILE" && break
	sleep 0.1
done

if ! running; then cat "$LOGFILE"; rm -f "$PIDFILE"; exit 1; fi
url
```

- [ ] **Step 3: Make it executable**

Run: `chmod +x ~/.claude/skills/revue/revue.sh`
Expected: no output.

- [ ] **Step 4: Smoke-test the script** from inside this repo

Run: `~/.claude/skills/revue/revue.sh status`
Expected: prints `not running` (no instance started here yet). If it instead errors about the `revue` binary, that's fine for this check — it means arg handling reached the start path; only the binary is absent.

- [ ] **Step 5: Update `SKILL.md` to delegate to the script**

Replace the **State**, **Start**, **Stop**, **Status**, and **URL / Tailscale** sections with a single instruction (keep the frontmatter and the **Inputs** intro). The skill now only normalizes the natural-language arguments and calls the script:

```markdown
## Run

Normalize the user's request to the script's argument form, then run the script
and print its output verbatim (it prints just the URL, or `not running` /
`stopped`):

- no args → `bash ~/.claude/skills/revue/revue.sh`
- stop → `bash ~/.claude/skills/revue/revue.sh stop`
- status → `bash ~/.claude/skills/revue/revue.sh status`
- a base branch → `bash ~/.claude/skills/revue/revue.sh <base>` (the script drops
  filler words `from/against/base/branch/on` and leading dashes itself, and
  validates the base, so passing the user's words through is safe)
- explicit port → append `port <n>`

The script is the single source of truth for revue's start/stop/status logic; it
is also what quack's `[[fast_commands]]` fast path execs directly, so both routes
behave identically.
```

- [ ] **Step 6: No commit** (these files are outside the repo). Verify the repo is otherwise clean:

Run: `git -C /home/yves/dev/src/github.com/eunomie/quack-worktrees/fast-slash-commands status --short`
Expected: clean (all in-repo changes already committed in Tasks 1–6).

---

## Final verification

- [ ] `go build ./...` — succeeds
- [ ] `go test ./...` — all pass
- [ ] `go vet ./...` — no warnings
- [ ] `stg series` — shows the six fastcmd patches, each with a Signed-off-by trailer
- [ ] Manual: add `[[fast_commands]]` for `/revue` to the local config, rebuild (`go build -o ~/.local/bin/quack ./cmd/quack`), restart via `systemd-run --user --on-active=10 systemctl --user restart quack.service`, then in a live session thread type `/revue` and confirm the URL comes back without an agent turn.

---

## Self-review notes

- **Spec coverage:** config block (Task 5), thread-only interception (Task 4), `RunFastCommand`/workdir exec via the `Runner` DI seam (Tasks 2–3), 👀/output/✅-❌ output handling (Task 3), `revue.sh` extraction (Task 7), tests (Tasks 2–3). All spec sections map to a task.
- **Type consistency:** `Runner.Run(ctx, dir, argv) ([]byte, error)` is identical in the interface (`fastcmd.go`), the adapter (`cmdexec.go`), and the fake (`fakes_test.go`). `FastCommand{Trigger, Argv}` matches across `session`, `config`, and the main.go mapping. `RunFastCommand(ctx, threadID, messageID, text) bool` is called exactly that way in `bot.go`.
- **No placeholders:** every code step shows complete code; every run step shows the command and expected result.
