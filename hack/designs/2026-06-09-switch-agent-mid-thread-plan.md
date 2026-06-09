# Switch Agent Mid-Thread Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a typed `/claude` / `/codex` in a tracked headless thread switch the session's agent in place, carrying the prior work across via a handoff summary the outgoing agent writes.

**Architecture:** A trigger `/<name>` (an agent block with `switchable = true`) is intercepted in the Discord tracked-thread branch before `FeedThread`. `SwitchAgent` validates and spawns `doSwitch`, which tears down the old run loop, runs one standalone resume-by-ref turn on the outgoing driver to produce a handoff summary, rebuilds the `liveSession` with the new driver via `newSession` (empty `SessionRef`, summary stored in `pendingHandoff`), and seeds it lazily — the handoff rides along, prepended once, on the next turn (an inline prompt now, or the next message). `SessionRef` is agent-native and non-portable, so the new agent always starts a fresh native session.

**Tech Stack:** Go, the quack `internal/session` orchestrator (consumer-defined interfaces + injected fakes), `internal/agentproc` driver abstraction, `internal/agent` config. Tests are table/unit tests with the existing in-package fakes (`fakeDriver`, `fakeStreamDriver`, `fakeReplier`, `memFS`). Version control is Stacked Git (`stg`): one patch per task, each signed off, **no** AI attribution.

**Reference spec:** `hack/designs/2026-06-09-switch-agent-mid-thread.md`.

**Conventions for every task:**
- Run a single package's tests with `go test ./internal/session/` (and `./internal/agent/` / `./internal/discord/` where touched).
- Commit each task as an stg patch:
  ```sh
  stg new <patch-name> -m "<subject>

  <body>

  Signed-off-by: Yves Brissaud <yves.brissaud@gmail.com>"
  stg refresh
  ```
  (Stage new files with `git add` before `stg refresh`, or use `stg refresh --index` after `git add`.)

---

### Task 1: `Switchable` agent field + `matchSwitch`

Adds the config opt-in flag and the pure matcher that recognizes a `/<name>` switch trigger. No behavior wired yet — just recognition, fully unit-testable.

**Files:**
- Modify: `internal/agent/agent.go` (add `Switchable` field)
- Create: `internal/session/switch.go`
- Create: `internal/session/switch_test.go`

- [ ] **Step 1: Add the `Switchable` field**

In `internal/agent/agent.go`, add the field to the `Agent` struct (place it after `Headless`):

```go
	Headless       bool   `toml:"headless"`        // has a headless driver
	Switchable     bool   `toml:"switchable"`      // opt in as a /<name> mid-thread switch target
```

- [ ] **Step 2: Write the failing matcher test**

Create `internal/session/switch_test.go`:

```go
package session

import (
	"testing"

	"github.com/eunomie/quack/internal/agent"
)

func newSwitchTestService() *Service {
	svc := New(Config{StateDir: "/state"}, newFakeGit(), newFakeTmux(), newFakeReplier())
	svc.cfg.Agents = map[string]agent.Agent{
		"claude": {Command: "claude", Headless: true, Switchable: true},
		"codex":  {Command: "codex", Headless: true, Switchable: true},
		"infer":  {Command: "claude", Headless: true},                  // not switchable
		"legacy": {Command: "legacy", Switchable: true},                // switchable but not headless
	}
	return svc
}

func TestMatchSwitch(t *testing.T) {
	svc := newSwitchTestService()
	cases := []struct {
		name        string
		text        string
		wantAgent   string
		wantPrompt  string
		wantOK      bool
	}{
		{"bare trigger", "/codex", "codex", "", true},
		{"trigger with prompt", "/codex fix the failing test", "codex", "fix the failing test", true},
		{"prompt keeps internal spacing", "/claude  run   it", "claude", "run   it", true},
		{"trailing spaces trimmed", "/codex   ", "codex", "", true},
		{"non-switchable agent", "/infer summarize", "", "", false},
		{"not headless", "/legacy go", "", "", false},
		{"unknown agent", "/gpt hello", "", "", false},
		{"trigger mid-sentence", "please /codex now", "", "", false},
		{"no slash", "codex do it", "", "", false},
		{"empty", "", "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotAgent, gotPrompt, gotOK := svc.matchSwitch(c.text)
			if gotOK != c.wantOK || gotAgent != c.wantAgent || gotPrompt != c.wantPrompt {
				t.Fatalf("matchSwitch(%q) = (%q, %q, %v), want (%q, %q, %v)",
					c.text, gotAgent, gotPrompt, gotOK, c.wantAgent, c.wantPrompt, c.wantOK)
			}
		})
	}
}
```

- [ ] **Step 3: Run the test, verify it fails**

Run: `go test ./internal/session/ -run TestMatchSwitch`
Expected: FAIL — `svc.matchSwitch undefined`.

- [ ] **Step 4: Implement `matchSwitch`**

Create `internal/session/switch.go`:

```go
package session

import "strings"

// matchSwitch reports whether text's first whitespace-delimited token is a
// switch trigger — "/<name>" for a configured agent that is headless and opted
// in with switchable=true. On a match it returns the agent name and the rest of
// the line (the inline prompt, internal spacing preserved, empty for a bare
// switch). A trigger anywhere but first does not match. /stop, /attach and fast
// commands are matched earlier in the bot, so they take precedence over a
// same-named agent.
func (s *Service) matchSwitch(text string) (agentName, prompt string, ok bool) {
	trimmed := strings.TrimSpace(text)
	fields := strings.Fields(trimmed)
	if len(fields) == 0 {
		return "", "", false
	}
	tok := fields[0]
	if !strings.HasPrefix(tok, "/") {
		return "", "", false
	}
	name := strings.TrimPrefix(tok, "/")
	ag, exists := s.cfg.Agents[name]
	if !exists || !ag.Headless || !ag.Switchable {
		return "", "", false
	}
	return name, strings.TrimSpace(trimmed[len(tok):]), true
}
```

- [ ] **Step 5: Run the test, verify it passes**

Run: `go test ./internal/session/ -run TestMatchSwitch && go test ./internal/agent/`
Expected: PASS for both.

- [ ] **Step 6: Commit**

```sh
git add internal/agent/agent.go internal/session/switch.go internal/session/switch_test.go
stg new switch-matcher -m "session: add switchable agent flag and /<name> matcher

A new switchable bool on agent.Agent opts an agent in as a mid-thread
switch target. matchSwitch recognizes a leading /<name> for such an agent
and splits off the inline prompt.

Signed-off-by: Yves Brissaud <yves.brissaud@gmail.com>"
stg refresh
```

---

### Task 2: `pendingHandoff` plumbing (field, record, consume in both loops)

Carries the handoff summary onto the next turn, consumed exactly once, on both the per-turn (codex) and stream (claude) paths, and persists it across a restart.

**Files:**
- Modify: `internal/session/headless.go` (field on `liveSession`, `consumeHandoff`, apply in `runTurn`)
- Modify: `internal/session/headless_stream.go` (apply in `streamBegin`)
- Modify: `internal/session/persist.go` (`PendingHandoff` on `sessionRecord`, in `record()`, copy in `newSession`)
- Modify: `internal/session/switch_test.go` (unit test)
- Modify: `internal/session/persist_test.go` (round-trip + consume-once tests)

- [ ] **Step 1: Write the failing `consumeHandoff` unit test**

Append to `internal/session/switch_test.go`:

```go
func TestConsumeHandoff(t *testing.T) {
	ls := &liveSession{}
	ls.pendingHandoff = "HANDOFF"

	if got := ls.consumeHandoff("hello"); got != "HANDOFF\n\nhello" {
		t.Fatalf("first consume = %q, want prepend", got)
	}
	if got := ls.consumeHandoff("again"); got != "again" {
		t.Fatalf("second consume = %q, want unchanged (cleared)", got)
	}

	ls2 := &liveSession{}
	ls2.pendingHandoff = "ONLY"
	if got := ls2.consumeHandoff(""); got != "ONLY" {
		t.Fatalf("empty text consume = %q, want the handoff alone", got)
	}
}
```

- [ ] **Step 2: Run the test, verify it fails**

Run: `go test ./internal/session/ -run TestConsumeHandoff`
Expected: FAIL — `ls.pendingHandoff undefined` / `ls.consumeHandoff undefined`.

- [ ] **Step 3: Add the field and helper**

In `internal/session/headless.go`, add to the `liveSession` struct, right after the `sessionRef` field (it shares `mu`):

```go
	sessionRef string // guarded by mu (read by PromoteThread from another goroutine)

	// pendingHandoff is a <quack-handoff> block from an agent switch, prepended to
	// the next turn's prompt and cleared. Guarded by mu. Empty when no switch is
	// pending.
	pendingHandoff string
```

Add the helper (place it next to `ref`/`setRef` in `headless.go`):

```go
// consumeHandoff prepends the pending handoff block to text the first time it's
// called after a switch, then clears it; returns text unchanged when none is
// pending. Mirrors how origin.go prepends <quack-context>.
func (ls *liveSession) consumeHandoff(text string) string {
	ls.mu.Lock()
	h := ls.pendingHandoff
	ls.pendingHandoff = ""
	ls.mu.Unlock()
	if h == "" {
		return text
	}
	if text == "" {
		return h
	}
	return h + "\n\n" + text
}
```

- [ ] **Step 4: Apply it at both turn-send sites**

In `internal/session/headless.go`, `runTurn`, change the `Prompt` field:

```go
	done := ls.driver.RunTurn(ctx, agentproc.Turn{
		SessionRef: ls.ref(),
		Prompt:     ls.consumeHandoff(tr.text),
		Workdir:    ls.workdir,
		Effort:     ls.effort,
		Name:       ls.name,
		Launcher:   ls.launcher, // nil for owners ⇒ DirectLauncher in the driver
	}, func(e agentproc.Event) {
```

In `internal/session/headless_stream.go`, `streamBegin`, change the send:

```go
	_ = ls.sess.Send(ls.consumeHandoff(tr.text))
```

- [ ] **Step 5: Add `PendingHandoff` to the record**

In `internal/session/persist.go`, add to `sessionRecord` (after `SessionRef`):

```go
	SessionRef    string `json:"session_ref"`
	PendingHandoff string `json:"pending_handoff,omitempty"` // <quack-handoff> block awaiting the next turn (set on an agent switch)
```

In `record()`, add the field:

```go
		SessionRef:    ls.sessionRef,
		PendingHandoff: ls.pendingHandoff,
```

In `newSession`, copy it onto the `liveSession` (add to the struct literal, near `sessionRef: rec.SessionRef`):

```go
		sessionRef:    rec.SessionRef,
		pendingHandoff: rec.PendingHandoff,
```

- [ ] **Step 6: Run the unit test, verify it passes**

Run: `go test ./internal/session/ -run TestConsumeHandoff`
Expected: PASS.

- [ ] **Step 7: Write the failing per-turn + stream consume-once tests**

Append to `internal/session/persist_test.go`:

```go
func TestHeadless_RehydrateConsumesHandoffOncePerTurn(t *testing.T) {
	d := &fakeDriver{turns: []scripted{
		{texts: []string{"a"}, ref: "sess-2"},
		{texts: []string{"b"}, ref: "sess-3"},
	}}
	svc, g, _, fs := newHeadlessServiceFakes(d)
	g.pathExists["/wt"] = true
	seedRecord(fs, sessionRecord{
		Name: "demo", AgentName: "claude", Workdir: "/wt", Effort: "high",
		ThreadID: "thread-x", RootChannelID: "c", RootMessageID: "m1", SessionRef: "sess-1",
		PendingHandoff: "<quack-handoff>SUMMARY</quack-handoff>",
	})
	if n := svc.Rehydrate(context.Background()); n != 1 {
		t.Fatalf("Rehydrate restored %d, want 1", n)
	}

	svc.FeedThread(context.Background(), "thread-x", "thread-x", "m2", "first", nil, Caller{Role: RoleOwner})
	svc.waitIdle("thread-x")
	svc.FeedThread(context.Background(), "thread-x", "thread-x", "m3", "second", nil, Caller{Role: RoleOwner})
	svc.waitIdle("thread-x")

	if len(d.seen) != 2 {
		t.Fatalf("driver saw %d turns, want 2", len(d.seen))
	}
	if !strings.Contains(d.seen[0].Prompt, "<quack-handoff>SUMMARY</quack-handoff>") ||
		!strings.Contains(d.seen[0].Prompt, "first") {
		t.Errorf("turn 1 prompt = %q, want handoff + 'first'", d.seen[0].Prompt)
	}
	if strings.Contains(d.seen[1].Prompt, "quack-handoff") {
		t.Errorf("turn 2 prompt = %q, handoff must be consumed once", d.seen[1].Prompt)
	}
}

func TestHeadless_RehydrateConsumesHandoffOnStreamPath(t *testing.T) {
	sess := newFakeStreamSession("sess-1")
	d := newFakeStreamDriver(sess)
	g, r, fs := newFakeGit(), newFakeReplier(), newMemFS()
	svc := New(Config{StateDir: "/state"}, g, newFakeTmux(), r)
	svc.drivers = map[string]agentproc.Driver{"claude": d}
	svc.mkdirAll, svc.writeFile, svc.remove = fs.mkdirAll, fs.writeFile, fs.remove
	svc.readDir, svc.readFile = fs.readDir, fs.readFile
	g.pathExists["/wt"] = true
	seedRecord(fs, sessionRecord{
		Name: "demo", AgentName: "claude", Workdir: "/wt",
		ThreadID: "thread-x", RootChannelID: "c", RootMessageID: "m1", SessionRef: "sess-1",
		PendingHandoff: "<quack-handoff>SUMMARY</quack-handoff>",
	})
	svc.Rehydrate(context.Background())

	svc.FeedThread(context.Background(), "thread-x", "thread-x", "m2", "first", nil, Caller{Role: RoleOwner})
	waitFor(t, "first send", func() bool { return sess.sentCount() >= 1 })

	sess.mu.Lock()
	first := sess.sent[0]
	sess.mu.Unlock()
	if !strings.Contains(first, "<quack-handoff>SUMMARY</quack-handoff>") || !strings.Contains(first, "first") {
		t.Errorf("stream send = %q, want handoff + 'first'", first)
	}
}
```

(`persist_test.go` already imports `context`; add `"strings"` and ensure `agentproc` is imported — it is used by `svc.drivers` map type, so add `"github.com/eunomie/quack/internal/agentproc"` to the import block if not already present.)

- [ ] **Step 8: Run, verify it passes**

Run: `go test ./internal/session/ -run 'TestHeadless_RehydrateConsumesHandoff|TestConsumeHandoff'`
Expected: PASS.

- [ ] **Step 9: Run the whole package to catch regressions**

Run: `go test ./internal/session/`
Expected: PASS (existing persist/rehydrate tests still green — `PendingHandoff` is `omitempty` and defaults empty).

- [ ] **Step 10: Commit**

```sh
git add internal/session/headless.go internal/session/headless_stream.go internal/session/persist.go internal/session/switch_test.go internal/session/persist_test.go
stg new switch-handoff-plumbing -m "session: carry a pending handoff onto the next turn

liveSession gains a pendingHandoff block, prepended once to the next turn's
prompt (consumeHandoff) on both the per-turn and stream paths, and persisted
in the session record so it survives a restart.

Signed-off-by: Yves Brissaud <yves.brissaud@gmail.com>"
stg refresh
```

---

### Task 3: `runSummaryTurn`

Runs one standalone resume-by-ref turn on the outgoing driver to capture the handoff summary, streaming it to the thread.

**Files:**
- Modify: `internal/session/switch.go` (constants + `runSummaryTurn`)
- Modify: `internal/session/switch_test.go` (test)

- [ ] **Step 1: Write the failing test**

Append to `internal/session/switch_test.go`:

```go
func TestRunSummaryTurn(t *testing.T) {
	d := &fakeDriver{turns: []scripted{
		{texts: []string{"Here is the handoff. "}, tools: []string{"Read main.go"}, ref: "ignored"},
	}}
	svc, r := newHeadlessService(d)
	ls := &liveSession{driver: d, workdir: "/wt", effort: "high", name: "demo", threadID: "thread-1"}

	summary := svc.runSummaryTurn(context.Background(), ls, "sess-1")

	if len(d.seen) != 1 {
		t.Fatalf("driver saw %d turns, want 1", len(d.seen))
	}
	if d.seen[0].SessionRef != "sess-1" || d.seen[0].Workdir != "/wt" {
		t.Errorf("summary turn = %+v, want resume sess-1 in /wt", d.seen[0])
	}
	if d.seen[0].Prompt != handoffPrompt {
		t.Errorf("summary prompt = %q, want the handoff prompt", d.seen[0].Prompt)
	}
	if summary != "Here is the handoff." {
		t.Errorf("summary = %q, want the trimmed assistant text", summary)
	}
	if !anyContains(r.posts, "Here is the handoff.") {
		t.Errorf("summary not streamed to the thread: %v", r.posts)
	}
}
```

- [ ] **Step 2: Run, verify it fails**

Run: `go test ./internal/session/ -run TestRunSummaryTurn`
Expected: FAIL — `handoffPrompt undefined` / `svc.runSummaryTurn undefined`.

- [ ] **Step 3: Implement constants and `runSummaryTurn`**

Append to `internal/session/switch.go` (add imports `context`, `strings`, `time`, and `github.com/eunomie/quack/internal/agentproc`):

```go
// summaryTimeout bounds the outgoing agent's handoff-summary turn so a wedged
// agent can't hang a switch; on timeout the switch proceeds with whatever
// streamed (possibly empty).
const summaryTimeout = 3 * time.Minute

// handoffPrompt asks the outgoing agent to summarize the session for a different
// agent that has no access to its history.
const handoffPrompt = "You are about to hand this session off to a DIFFERENT coding agent that has no access to your conversation history or memory. Write a thorough handoff so it can continue seamlessly. Cover: the goal of this session, what you've done so far, key decisions and why, approaches you tried and rejected, the current state of the working tree (which files changed and what is committed), and the concrete next steps. Be specific with file paths and identifiers. Output ONLY the handoff text."

// runSummaryTurn runs one resume-by-ref turn on the outgoing driver asking for a
// handoff summary, streams it to the thread (a fresh renderer, so it reads like a
// normal answer), and returns the assistant text. The session's run loop must
// already be stopped: this is a standalone process (claude --resume / a codex
// resume turn), independent of any live ls.sess.
func (s *Service) runSummaryTurn(ctx context.Context, ls *liveSession, ref string) string {
	rend := newTurnRender(s, ls)
	var sb strings.Builder
	done := ls.driver.RunTurn(ctx, agentproc.Turn{
		SessionRef: ref,
		Prompt:     handoffPrompt,
		Workdir:    ls.workdir,
		Effort:     ls.effort,
		Name:       ls.name,
		Launcher:   ls.launcher, // guest sessions: summarize inside the container
	}, func(e agentproc.Event) {
		switch ev := e.(type) {
		case agentproc.AssistantText:
			sb.WriteString(ev.Text)
			rend.handle(ctx, ev.Text, false)
		case agentproc.ToolActivity:
			rend.handle(ctx, ev.Label, true)
		}
	})
	rend.finalizeTools(ctx)
	rend.flushPending(ctx, true)
	if done.Err != nil {
		_, _ = s.reply.Post(ctx, ls.threadID, "⚠️ handoff summary failed: "+done.Err.Error())
	}
	return strings.TrimSpace(sb.String())
}
```

- [ ] **Step 4: Run, verify it passes**

Run: `go test ./internal/session/ -run TestRunSummaryTurn`
Expected: PASS.

- [ ] **Step 5: Commit**

```sh
git add internal/session/switch.go internal/session/switch_test.go
stg new switch-summary-turn -m "session: capture an outgoing agent's handoff summary

runSummaryTurn runs one resume-by-ref turn asking the outgoing driver for a
handoff summary, streams it to the thread, and returns the text. Standalone
(no live session), so it needs no run-loop changes.

Signed-off-by: Yves Brissaud <yves.brissaud@gmail.com>"
stg refresh
```

---

### Task 4: `SwitchAgent` + `doSwitch`

The full operation: validate, tear down, summarize, rebuild with the new driver, seed lazily.

**Files:**
- Modify: `internal/session/switch.go` (`wrapHandoff`, `SwitchAgent`, `doSwitch`)
- Modify: `internal/session/switch_test.go` (tests)

- [ ] **Step 1: Write the failing tests**

Append to `internal/session/switch_test.go` (add imports `context`, `strings`):

```go
// newSwitchService builds a service with two per-turn drivers, both switchable.
func newSwitchService(claude, codex agentproc.Driver) (*Service, *fakeReplier, *memFS) {
	g, r, fs := newFakeGit(), newFakeReplier(), newMemFS()
	svc := New(Config{StateDir: "/state"}, g, newFakeTmux(), r)
	svc.drivers = map[string]agentproc.Driver{"claude": claude, "codex": codex}
	svc.cfg.Agents = map[string]agent.Agent{
		"claude": {Command: "claude", Headless: true, Switchable: true},
		"codex":  {Command: "codex", Headless: true, Switchable: true},
	}
	svc.mkdirAll, svc.writeFile, svc.remove = fs.mkdirAll, fs.writeFile, fs.remove
	svc.readDir, svc.readFile = fs.readDir, fs.readFile
	return svc, r, fs
}

func TestDoSwitch_LazyHandoff(t *testing.T) {
	claude := &fakeDriver{turns: []scripted{
		{texts: []string{"working on it"}, ref: "claude-1"}, // first real turn
		{texts: []string{"HANDOFF SUMMARY"}, ref: "claude-2"}, // summary turn
	}}
	codex := &fakeDriver{turns: []scripted{{texts: []string{"continuing"}, ref: "codex-1"}}}
	svc, r, fs := newSwitchService(claude, codex)

	svc.startHeadless(context.Background(), "claude", "thread-1", "/wt", "high", "demo", "",
		RoleOwner, nil, "owner", turnReq{channelID: "c", messageID: "m1", text: "go"})
	svc.waitIdle("thread-1")

	ls := svc.sessions["thread-1"]
	svc.doSwitch(context.Background(), ls, "codex", "", "c", "m2", nil, Caller{Role: RoleOwner})

	// Outgoing claude ran the initial turn + the summary turn (resuming its ref).
	if len(claude.seen) != 2 || claude.seen[1].SessionRef != "claude-1" || claude.seen[1].Prompt != handoffPrompt {
		t.Fatalf("claude turns = %+v, want a summary turn resuming claude-1", claude.seen)
	}
	// The session is now codex, fresh ref, with the handoff queued (lazy: no turn yet).
	newls := svc.sessions["thread-1"]
	if newls.agentName != "codex" {
		t.Fatalf("agentName = %q, want codex", newls.agentName)
	}
	if newls.ref() != "" {
		t.Errorf("SessionRef = %q, want empty (fresh native session)", newls.ref())
	}
	if !strings.Contains(newls.pendingHandoff, "HANDOFF SUMMARY") {
		t.Errorf("pendingHandoff = %q, want the summary wrapped", newls.pendingHandoff)
	}
	if len(codex.seen) != 0 {
		t.Errorf("codex ran %d turns, want 0 (lazy seed)", len(codex.seen))
	}
	// Persisted record reflects the new agent + handoff + empty ref.
	rec, ok := readRecord(t, fs, "demo")
	if !ok || rec.AgentName != "codex" || rec.SessionRef != "" || !strings.Contains(rec.PendingHandoff, "HANDOFF SUMMARY") {
		t.Errorf("record = %+v, want codex / empty ref / handoff", rec)
	}
	if !anyContains(r.posts, "switched to codex") {
		t.Errorf("no switch confirmation posted: %v", r.posts)
	}
}

func TestDoSwitch_InlinePromptSeedsImmediately(t *testing.T) {
	claude := &fakeDriver{turns: []scripted{
		{texts: []string{"working"}, ref: "claude-1"},
		{texts: []string{"SUMMARY"}, ref: "claude-2"},
	}}
	codex := &fakeDriver{turns: []scripted{{texts: []string{"done"}, ref: "codex-1"}}}
	svc, _, _ := newSwitchService(claude, codex)
	svc.startHeadless(context.Background(), "claude", "thread-1", "/wt", "high", "demo", "",
		RoleOwner, nil, "owner", turnReq{channelID: "c", messageID: "m1", text: "go"})
	svc.waitIdle("thread-1")

	ls := svc.sessions["thread-1"]
	svc.doSwitch(context.Background(), ls, "codex", "fix the test", "c", "m2", nil, Caller{Role: RoleOwner})
	svc.waitIdle("thread-1")

	if len(codex.seen) != 1 {
		t.Fatalf("codex ran %d turns, want 1 (inline prompt)", len(codex.seen))
	}
	if codex.seen[0].SessionRef != "" {
		t.Errorf("codex first turn ref = %q, want empty", codex.seen[0].SessionRef)
	}
	if !strings.Contains(codex.seen[0].Prompt, "SUMMARY") || !strings.Contains(codex.seen[0].Prompt, "fix the test") {
		t.Errorf("codex prompt = %q, want handoff + inline prompt", codex.seen[0].Prompt)
	}
}

func TestDoSwitch_SameAgentNoOp(t *testing.T) {
	claude := &fakeDriver{turns: []scripted{
		{texts: []string{"working"}, ref: "claude-1"},
		{texts: []string{"second"}, ref: "claude-2"},
	}}
	codex := &fakeDriver{}
	svc, r, _ := newSwitchService(claude, codex)
	svc.startHeadless(context.Background(), "claude", "thread-1", "/wt", "high", "demo", "",
		RoleOwner, nil, "owner", turnReq{channelID: "c", messageID: "m1", text: "go"})
	svc.waitIdle("thread-1")

	ls := svc.sessions["thread-1"]
	svc.doSwitch(context.Background(), ls, "claude", "keep going", "c", "m2", nil, Caller{Role: RoleOwner})
	svc.waitIdle("thread-1")

	// Same liveSession (no rebuild), no summary turn; the inline prompt ran as a
	// normal turn resuming the existing ref.
	if svc.sessions["thread-1"] != ls {
		t.Errorf("session was rebuilt on a same-agent switch")
	}
	if len(claude.seen) != 2 || claude.seen[1].Prompt != "keep going" || claude.seen[1].SessionRef != "claude-1" {
		t.Fatalf("claude turns = %+v, want a normal resume of 'keep going'", claude.seen)
	}
	if anyContains(r.posts, "switched to") {
		t.Errorf("same-agent switch should not post a switch confirmation")
	}
}

func TestDoSwitch_EmptyRefSkipsSummary(t *testing.T) {
	claude := &fakeDriver{} // no turns scripted: must not be asked to summarize
	codex := &fakeDriver{}
	svc, _, _ := newSwitchService(claude, codex)
	ls := svc.newSession(context.Background(), sessionRecord{
		Name: "demo", AgentName: "claude", Workdir: "/wt", ThreadID: "thread-1",
		RootChannelID: "c", RootMessageID: "m1", SessionRef: "", // never produced a reply
	})

	svc.doSwitch(context.Background(), ls, "codex", "", "c", "m2", nil, Caller{Role: RoleOwner})

	if len(claude.seen) != 0 {
		t.Errorf("claude ran %d summary turns, want 0 (no ref to resume)", len(claude.seen))
	}
	if svc.sessions["thread-1"].agentName != "codex" {
		t.Errorf("switch did not complete to codex")
	}
	if svc.sessions["thread-1"].pendingHandoff != "" {
		t.Errorf("pendingHandoff = %q, want empty (no summary)", svc.sessions["thread-1"].pendingHandoff)
	}
}

func TestDoSwitch_UnknownAgentDoesNotTearDown(t *testing.T) {
	claude := &fakeDriver{turns: []scripted{{texts: []string{"working"}, ref: "claude-1"}}}
	codex := &fakeDriver{}
	svc, r, _ := newSwitchService(claude, codex)
	svc.startHeadless(context.Background(), "claude", "thread-1", "/wt", "high", "demo", "",
		RoleOwner, nil, "owner", turnReq{channelID: "c", messageID: "m1", text: "go"})
	svc.waitIdle("thread-1")
	ls := svc.sessions["thread-1"]

	svc.doSwitch(context.Background(), ls, "ghost", "", "c", "m2", nil, Caller{Role: RoleOwner})

	if svc.sessions["thread-1"] != ls || ls.agentName != "claude" {
		t.Errorf("a bad switch must leave the live session untouched")
	}
	if !anyContains(r.posts, "unknown agent") {
		t.Errorf("expected an unknown-agent error, got %v", r.posts)
	}
}

func TestSwitchAgent_GuestCannotSwitchOthersSession(t *testing.T) {
	claude := &fakeDriver{turns: []scripted{{texts: []string{"working"}, ref: "claude-1"}}}
	codex := &fakeDriver{}
	svc, _, _ := newSwitchService(claude, codex)
	svc.startHeadless(context.Background(), "claude", "thread-1", "/wt", "high", "demo", "",
		RoleOwner, nil, "owner", turnReq{channelID: "c", messageID: "m1", text: "go"})
	svc.waitIdle("thread-1")

	// A guest who didn't start the session: matched as a switch (returns true, so it
	// isn't fed as text) but dropped — the session is untouched.
	guest := Caller{Role: RoleGuest, UserID: "intruder"}
	if !svc.SwitchAgent(context.Background(), "thread-1", "c", "m2", "/codex", nil, guest) {
		t.Fatalf("a switch trigger must report handled even when authz drops it")
	}
	if svc.sessions["thread-1"].agentName != "claude" {
		t.Errorf("guest must not switch a session it didn't start")
	}
}

func TestSwitchAgent_NotASwitchFallsThrough(t *testing.T) {
	svc, _, _ := newSwitchService(&fakeDriver{}, &fakeDriver{})
	if svc.SwitchAgent(context.Background(), "thread-1", "c", "m1", "just a message", nil, Caller{Role: RoleOwner}) {
		t.Errorf("non-trigger text must return false so the bot falls through to FeedThread")
	}
}
```

(Confirm `RoleGuest` is the guest role constant — grep `internal/session/*.go` for `RoleGuest`/`RoleOwner`; use the actual identifiers.)

- [ ] **Step 2: Run, verify it fails**

Run: `go test ./internal/session/ -run 'TestDoSwitch|TestSwitchAgent'`
Expected: FAIL — `svc.doSwitch undefined` / `svc.SwitchAgent undefined`.

- [ ] **Step 3: Implement `wrapHandoff`, `SwitchAgent`, `doSwitch`**

Append to `internal/session/switch.go`:

```go
// wrapHandoff frames a captured summary as the <quack-handoff> block seeded onto
// the new agent's next turn. Empty summary ⇒ empty block (nothing to seed).
func wrapHandoff(fromAgent, summary string) string {
	if strings.TrimSpace(summary) == "" {
		return ""
	}
	return "<quack-handoff>\nA previous agent (" + fromAgent + ") worked on this session and left this handoff for you. You do not share its memory or conversation history; treat the following as your context for continuing the work.\n\n" + summary + "\n</quack-handoff>"
}

// SwitchAgent handles a /<name> switch command in a tracked thread. It returns
// true when the first token is a switch trigger (so the bot does not fall through
// to feed the text to the agent), false when it isn't (fall through). The slow
// work — summarize, tear down, rebuild — runs in a goroutine, like a fast command.
func (s *Service) SwitchAgent(ctx context.Context, threadID, channelID, messageID, text string, atts []Attachment, caller Caller) bool {
	target, prompt, ok := s.matchSwitch(text)
	if !ok {
		return false
	}
	s.hmu.Lock()
	ls, tracked := s.sessions[threadID]
	s.hmu.Unlock()
	if !tracked {
		return false
	}
	// A guest may only switch a session it started; drop (but report handled, so
	// "/codex …" is never fed as a prompt to a session the guest can't touch).
	if !ls.canModify(caller) {
		return true
	}
	go s.doSwitch(context.Background(), ls, target, prompt, channelID, messageID, atts, caller)
	return true
}

// doSwitch performs the switch: guard, tear down the old loop, summarize the
// outgoing agent, rebuild the session with the new driver, then seed lazily. All
// guards that can reject the switch run before any teardown, so a rejected switch
// never disturbs the live session.
func (s *Service) doSwitch(ctx context.Context, ls *liveSession, target, prompt, channelID, messageID string, atts []Attachment, caller Caller) {
	if s.drivers[target] == nil {
		_, _ = s.reply.Post(ctx, ls.threadID, "❌ unknown agent: "+target)
		return
	}
	if target == ls.agentName {
		_, _ = s.reply.Post(ctx, ls.threadID, "already on "+target)
		if strings.TrimSpace(prompt) != "" {
			s.FeedThread(ctx, ls.threadID, channelID, messageID, prompt, atts, caller)
		}
		return
	}

	oldRef := ls.ref()
	oldAgent := ls.agentName
	threadID := ls.threadID

	// Tear down the old loop. cancelPending/close cut off any in-flight turn — the
	// user asked to switch now.
	s.hmu.Lock()
	delete(s.sessions, threadID)
	delete(s.askByToken, ls.askToken)
	s.hmu.Unlock()
	ls.cancelPending()
	ls.close()
	<-ls.done

	// Summarize, if the outgoing agent ever produced a resumable ref.
	summary := ""
	if oldRef != "" {
		_, _ = s.reply.Post(ctx, threadID, "🔄 switching to "+target+" — summarizing the handoff…")
		sctx, cancel := context.WithTimeout(ctx, summaryTimeout)
		summary = s.runSummaryTurn(sctx, ls, oldRef)
		cancel()
	}

	// Rebuild in place with the new driver. record() carries label/role/sandbox/
	// askToken forward; newSession picks the right loop type and (for a sandbox)
	// rebuilds the guest launcher + driver.
	rec := ls.record()
	rec.AgentName = target
	rec.SessionRef = ""
	rec.PendingHandoff = wrapHandoff(oldAgent, summary)
	newls := s.newSession(context.Background(), rec)
	s.persistRecord(newls.record())

	_, _ = s.reply.Post(ctx, threadID, "🔄 switched to "+target)
	if strings.TrimSpace(prompt) != "" {
		s.FeedThread(ctx, threadID, channelID, messageID, prompt, atts, caller)
	}
}
```

- [ ] **Step 4: Run, verify it passes**

Run: `go test ./internal/session/ -run 'TestDoSwitch|TestSwitchAgent'`
Expected: PASS.

- [ ] **Step 5: Run the whole package**

Run: `go test ./internal/session/`
Expected: PASS.

- [ ] **Step 6: Commit**

```sh
git add internal/session/switch.go internal/session/switch_test.go
stg new switch-operation -m "session: switch a thread's agent in place

SwitchAgent intercepts a /<name> trigger; doSwitch tears down the old loop,
captures a handoff summary from the outgoing agent, rebuilds the session with
the new driver (fresh native session), and seeds the handoff lazily — now for
an inline prompt, otherwise on the next message.

Signed-off-by: Yves Brissaud <yves.brissaud@gmail.com>"
stg refresh
```

---

### Task 5: Wire interception into the Discord bot

**Files:**
- Modify: `internal/discord/bot.go` (one line in the tracked-thread branch)

- [ ] **Step 1: Add the interception line**

In `internal/discord/bot.go`, `onMessage`, in the tracked-thread branch, after the `RunFastCommand` block and before the `AnswerAskText` block:

```go
		if b.svc.RunFastCommand(context.Background(), m.ChannelID, m.ID, content) {
			return
		}
		// A configured /<name> (an agent with switchable=true) switches the session's
		// agent in place, carrying a handoff summary across. Explicit command, so it
		// takes precedence over treating the text as an ask_user answer.
		if b.svc.SwitchAgent(context.Background(), m.ChannelID, m.ChannelID, m.ID, content, atts, caller) {
			return
		}
		// While the agent is blocked on an ask_user question, a text reply is the
		// answer, not a new turn. Empty content (an attachment-only message) falls
		// through to FeedThread so it can still interject.
		if content != "" && b.svc.AnswerAskText(m.ChannelID, content) {
			return
		}
```

(`caller` and `atts` are already in scope in this branch — they are passed to `StopThread`/`FeedThread` on the surrounding lines. Confirm the exact variable names by reading the branch; match them.)

- [ ] **Step 2: Build and vet**

Run: `go build ./... && go vet ./...`
Expected: no errors.

- [ ] **Step 3: Run the discord package tests (if any) + full suite**

Run: `go test ./...`
Expected: PASS.

- [ ] **Step 4: Commit**

```sh
git add internal/discord/bot.go
stg new switch-bot-wiring -m "discord: intercept /<name> agent switches in tracked threads

Route a tracked-thread message whose first token is a configured switchable
agent trigger to SwitchAgent, before the ask-answer/feed fall-through.

Signed-off-by: Yves Brissaud <yves.brissaud@gmail.com>"
stg refresh
```

---

### Task 6: Documentation

**Files:**
- Modify: `config.example.toml` (document `switchable`)
- Modify: `AGENTS.md` (document the switch in the request-flow section)

- [ ] **Step 1: Document `switchable` in the config example**

In `config.example.toml`, on the `[agents.claude]` and `[agents.codex]` blocks (or wherever agents are shown), add the flag with a comment. Example for each real agent:

```toml
# switchable = true opts this agent in as a /<name> mid-thread switch target,
# so you can type /claude or /codex in a tracked thread to hand the session
# off to it (the outgoing agent writes a handoff summary first).
switchable = true
```

- [ ] **Step 2: Document the switch in AGENTS.md**

In `AGENTS.md`, in the request-flow section (step 1, near the fast-command paragraph), add a paragraph:

```markdown
   A message in a tracked thread whose **first word is a `/<name>` switch
   trigger** — an `[agents.<name>]` block with `switchable = true` (e.g.
   `/claude`, `/codex`) — switches the session's agent **in place**
   (`Service.SwitchAgent` → `doSwitch`). The outgoing agent writes a handoff
   summary (a standalone resume-by-ref turn), quack rebuilds the session with the
   new driver and a fresh `SessionRef`, and seeds the summary lazily as a
   `<quack-handoff>` block on the next turn (now if `/codex <prompt>` carried one,
   else the next message). `SessionRef` is agent-native, so the new agent always
   starts fresh. See `hack/designs/2026-06-09-switch-agent-mid-thread.md`.
```

- [ ] **Step 3: Commit**

```sh
git add config.example.toml AGENTS.md
stg new switch-docs -m "docs: document mid-thread agent switching

Note the switchable agent flag in the config example and the /<name> switch
flow in AGENTS.md.

Signed-off-by: Yves Brissaud <yves.brissaud@gmail.com>"
stg refresh
```

---

### Task 7: Final verification

- [ ] **Step 1: Full build, vet, and test**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: all PASS.

- [ ] **Step 2: Review the patch series**

Run: `stg series` and `stg show switch-matcher switch-handoff-plumbing switch-summary-turn switch-operation switch-bot-wiring switch-docs`
Expected: six clean patches (plus the `switch-agent-design` spec patch), each signed off, no AI attribution.

- [ ] **Step 3 (optional, owner): live smoke test**

Build onto PATH and restart the unit, then in a real thread try `/codex`, `/codex fix X`, `/claude`, a bogus `/ghost`, and a same-agent `/claude`. (Per AGENTS.md, schedule a detached restart if running this from inside a headless session: `systemd-run --user --on-active=10 systemctl --user restart quack.service`.)

---

## Self-Review

**Spec coverage:**
- §1 Config (`switchable`) → Task 1 + Task 6. ✓
- §2 `matchSwitch` → Task 1. ✓
- §3 Bot interception → Task 5. ✓
- §4 `SwitchAgent`/`doSwitch` (guards, teardown, summarize, rebuild, lazy seed) → Task 4. ✓
- §5 `consumeHandoff` (both loops) → Task 2. ✓
- §6 `runSummaryTurn` → Task 3. ✓
- §7 Handoff prompt + `<quack-handoff>` wrapping → Task 3 (`handoffPrompt`) + Task 4 (`wrapHandoff`). ✓
- §8 Persistence (`PendingHandoff`) → Task 2. ✓
- §9 Edge cases: in-flight cutoff (close-first, Task 4), same-agent (Task 4 test), empty-ref (Task 4 test), unknown agent (Task 4 test), non-trigger fall-through (Task 4 test), guest gate (Task 4 test), summary timeout (Task 3 const, used in Task 4). ✓
- Testing list → Tasks 1–4 tests. ✓

**Placeholder scan:** No TBD/TODO; every code step has complete code; every test has assertions. ✓

**Type consistency:** `matchSwitch(text) (agentName, prompt string, ok bool)`, `consumeHandoff(text) string`, `runSummaryTurn(ctx, ls, ref) string`, `wrapHandoff(fromAgent, summary) string`, `SwitchAgent(ctx, threadID, channelID, messageID, text, atts, caller) bool`, `doSwitch(ctx, ls, target, prompt, channelID, messageID, atts, caller)`, `sessionRecord.PendingHandoff`, `liveSession.pendingHandoff`, `handoffPrompt`, `summaryTimeout` — used consistently across tasks. ✓

**Verification notes for the implementer (do not skip):**
- Confirm the guest role constant name (`RoleGuest` vs other) and `canModify`'s semantics by reading `internal/session/headless.go` before writing Task 4's guest test.
- Confirm `caller`/`atts` variable names in `internal/discord/bot.go`'s tracked-thread branch (Task 5) — match what `FeedThread`/`StopThread` are already passed there.
- Confirm `persist_test.go`'s import block gains `"strings"` and `"github.com/eunomie/quack/internal/agentproc"` (Task 2).
