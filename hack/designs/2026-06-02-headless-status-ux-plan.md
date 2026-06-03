# Headless Status & Tool-Activity UX — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** In headless mode, collapse a turn's tool activity into one edited-in-place Discord message, and surface a live global session status on both the thread title and the top (triggering) message reaction.

**Architecture:** All changes are in `internal/session`. `runTurn` (in `headless.go`) is rewired so a burst of tool events posts **one** message and edits it (throttled) instead of posting per burst, and so turn-boundary status updates a **global** marker on the thread's root message plus a best-effort thread-title prefix driven by a small background `titleUpdater`. The `Replier` interface is unchanged — it already exposes `Edit` and `RenameThread`. Interactive (`no-headless`) mode is untouched.

**Tech Stack:** Go, standard library only (`context`, `strings`, `sync`, `time`). Unit-tested with the existing in-package fakes (`fakes_test.go`, `headless_test.go`). No external deps; `go test ./internal/session` runs everything.

**Spec:** `hack/designs/2026-06-02-headless-status-ux.md`

---

## File Structure

- **Create** `internal/session/titlestatus.go` — `titleUpdater`: a per-session background goroutine that applies best-effort, latest-wins thread renames (`"<emoji> <name>"`). One responsibility: serialize + coalesce title renames so a rate-limited rename never blocks a turn.
- **Create** `internal/session/titlestatus_test.go` — unit test for `titleUpdater` coalescing + stop/drain.
- **Modify** `internal/session/headless.go` — add `time` import + `toolEditInterval` const; add `liveSession` fields (`rootChannelID`, `rootMessageID`, `lastGlobalEmoji`, `title`); rewrite tool handling and status reactions in `runTurn`; add `setGlobalStatus` / `clearGlobalWorking`; capture root + start `titleUpdater` in `startHeadless`; stop it in `close`.
- **Modify** `internal/session/headless_test.go` — update `TestHeadless_SegmentPosting` for the post-then-edit split; add new tests for tool-message editing, global status across turns, and the title icon; add `lastReactOn` helper.

No changes to `service.go`, `render.go`, `replier.go`, or the `Replier` interface.

---

## Task 1: `titleUpdater` background goroutine

**Files:**
- Create: `internal/session/titlestatus.go`
- Test: `internal/session/titlestatus_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/session/titlestatus_test.go`:

```go
package session

import "testing"

func TestTitleUpdater_AppliesLatestStatus(t *testing.T) {
	r := newFakeReplier()
	tu := newTitleUpdater(r, "thread-1", "demo")

	tu.set(emojiWorking)
	tu.set(emojiDone)
	tu.stop()
	<-tu.done

	if len(r.renames) == 0 {
		t.Fatalf("expected at least one thread rename, got none")
	}
	last := r.renames[len(r.renames)-1]
	if want := "thread-1|" + emojiDone + " demo"; last != want {
		t.Fatalf("final title = %q, want %q (renames=%v)", last, want, r.renames)
	}
}

func TestTitleUpdater_StopIsIdempotent(t *testing.T) {
	r := newFakeReplier()
	tu := newTitleUpdater(r, "thread-1", "demo")
	tu.set(emojiWorking)
	tu.stop()
	tu.stop() // must not panic (double close)
	<-tu.done
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/session -run TestTitleUpdater -v`
Expected: FAIL — compile error `undefined: newTitleUpdater` (the type doesn't exist yet).

- [ ] **Step 3: Write the implementation**

Create `internal/session/titlestatus.go`:

```go
package session

import (
	"context"
	"sync"
)

// titleUpdater applies thread-title status changes on a background goroutine.
// Discord rate-limits thread renames heavily (~2 per 10 minutes), so updates are
// best-effort and coalesced latest-wins: a newer status replaces any still-pending
// one, and a rename that blocks on the rate limiter blocks only this goroutine,
// never a turn. The title is always reconstructed as "<emoji> <name>" so status
// icons never stack.
type titleUpdater struct {
	reply    Replier
	threadID string
	name     string

	ch       chan string
	stopCh   chan struct{}
	stopOnce sync.Once
	done     chan struct{}
}

func newTitleUpdater(reply Replier, threadID, name string) *titleUpdater {
	tu := &titleUpdater{
		reply:    reply,
		threadID: threadID,
		name:     name,
		ch:       make(chan string, 1),
		stopCh:   make(chan struct{}),
		done:     make(chan struct{}),
	}
	go tu.run()
	return tu
}

// set requests that the title show emoji as its status prefix. Non-blocking; if a
// prior request is still pending it is replaced (latest wins).
func (tu *titleUpdater) set(emoji string) {
	title := emoji + " " + tu.name
	for {
		select {
		case tu.ch <- title:
			return
		default:
			select {
			case <-tu.ch: // drop the stale pending value, then retry the send
			default:
			}
		}
	}
}

// stop signals the updater to apply any pending title and exit. Idempotent and
// non-blocking; the goroutine closes done when it has exited.
func (tu *titleUpdater) stop() {
	tu.stopOnce.Do(func() { close(tu.stopCh) })
}

func (tu *titleUpdater) run() {
	defer close(tu.done)
	for {
		select {
		case title := <-tu.ch:
			_ = tu.reply.RenameThread(context.Background(), tu.threadID, title)
		case <-tu.stopCh:
			select {
			case title := <-tu.ch: // final drain: apply the latest pending title
				_ = tu.reply.RenameThread(context.Background(), tu.threadID, title)
			default:
			}
			return
		}
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/session -run TestTitleUpdater -v`
Expected: PASS (both tests).

- [ ] **Step 5: Commit**

```bash
stg new titlestatus-updater -m "feat(session): background thread-title status updater

Coalescing, best-effort titleUpdater so rate-limited thread renames never
block a turn.

Signed-off-by: Yves Brissaud <yves.brissaud@gmail.com>"
git add internal/session/titlestatus.go internal/session/titlestatus_test.go
stg refresh
```

---

## Task 2: Single refreshing tool message per turn

Rewrites the tool-activity handling in `runTurn`: the first tool of a burst **posts** one message; subsequent tools **edit** it (throttled to `toolEditInterval`); the message is **finalized** (one last edit, then reset) when an answer arrives or the turn ends. Status reactions are left exactly as they are today in this task (changed in Task 3).

**Files:**
- Modify: `internal/session/headless.go` (imports + `toolEditInterval` const + `runTurn`)
- Modify: `internal/session/headless_test.go` (update `TestHeadless_SegmentPosting`, add `TestHeadless_ToolMessageEditedNotReposted`, add `lastEditTo` helper)

- [ ] **Step 1: Update the existing segment test and add the new one (failing)**

In `internal/session/headless_test.go`, **replace** `TestHeadless_SegmentPosting` (currently asserting the tools are coalesced into `posts[0]`) with the version below, which expects the tool message to be posted once and then edited to its final content:

```go
func TestHeadless_SegmentPosting(t *testing.T) {
	// fakeDriver emits all tools, then all texts.
	d := &fakeDriver{turns: []scripted{{
		tools: []string{"WebFetch", "Grep"},
		texts: []string{"First segment.", "Second segment."},
		ref:   "s",
	}}}
	svc, r := newHeadlessService(d)

	svc.startHeadless(context.Background(), "claude", "t", "/wt", "", "n",
		turnReq{channelID: "c", messageID: "m", text: "go"})
	svc.waitIdle("t")

	// One tool message + each text segment as its own message.
	if len(r.posts) != 3 {
		t.Fatalf("want 3 posts (tool message + 2 segments), got %d: %v", len(r.posts), r.posts)
	}
	if r.posts[1].content != "First segment." || r.posts[2].content != "Second segment." {
		t.Errorf("segments = %q, %q", r.posts[1].content, r.posts[2].content)
	}
	// The tool message (posts[0], id "msg-1") is edited to hold every tool line,
	// not re-posted per tool.
	final := lastEditTo(r.edits, "msg-1")
	if !strings.Contains(final, "WebFetch") || !strings.Contains(final, "Grep") {
		t.Errorf("tool message not finalized with all tools: post=%q finalEdit=%q", r.posts[0].content, final)
	}
}

func TestHeadless_ToolMessageEditedNotReposted(t *testing.T) {
	d := &fakeDriver{turns: []scripted{{
		tools: []string{"Read a", "Read b", "Read c"},
		texts: []string{"done"},
		ref:   "s",
	}}}
	svc, r := newHeadlessService(d)

	svc.startHeadless(context.Background(), "claude", "t", "/wt", "", "n",
		turnReq{channelID: "c", messageID: "m", text: "go"})
	svc.waitIdle("t")

	// Exactly two posts: the single tool message, then the answer.
	if len(r.posts) != 2 {
		t.Fatalf("want 2 posts (one tool message + answer), got %d: %v", len(r.posts), r.posts)
	}
	if r.posts[1].content != "done" {
		t.Errorf("answer post = %q, want %q", r.posts[1].content, "done")
	}
	// All three tools land in the one edited tool message.
	final := lastEditTo(r.edits, "msg-1")
	for _, want := range []string{"Read a", "Read b", "Read c"} {
		if !strings.Contains(final, want) {
			t.Errorf("tool message missing %q; final edit = %q", want, final)
		}
	}
}
```

Add this helper at the bottom of `internal/session/headless_test.go` (next to `anyContains` / `hasStr`):

```go
// lastEditTo returns the content of the last edit targeting messageID, or "".
func lastEditTo(edits []postedMsg, messageID string) string {
	out := ""
	for _, e := range edits {
		if e.channel == messageID { // fakeReplier.Edit stores messageID in .channel
			out = e.content
		}
	}
	return out
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/session -run 'TestHeadless_SegmentPosting|TestHeadless_ToolMessageEditedNotReposted' -v`
Expected: FAIL — `TestHeadless_SegmentPosting` fails its new edit assertion and `TestHeadless_ToolMessageEditedNotReposted` fails on `len(r.posts) != 2` (today every tool burst is re-posted, and tools are coalesced into a single *post* rather than edited).

- [ ] **Step 3: Add the `time` import and `toolEditInterval` const**

In `internal/session/headless.go`, change the import block from:

```go
import (
	"context"
	"strings"
	"sync"

	"github.com/eunomie/quack/internal/agentproc"
)
```

to:

```go
import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/eunomie/quack/internal/agentproc"
)

// toolEditInterval throttles in-place edits of the per-turn tool message so a
// fast tool stream stays well under Discord's edit rate limit (~5/5s per channel).
const toolEditInterval = time.Second
```

- [ ] **Step 4: Rewrite `runTurn`'s tool handling**

In `internal/session/headless.go`, **replace** the body of `runTurn` (the function starting `func (s *Service) runTurn(...)`) with:

```go
func (s *Service) runTurn(ctx context.Context, ls *liveSession, tr turnReq) {
	// 👀 on the user's message while the agent works.
	_ = s.reply.React(ctx, tr.channelID, tr.messageID, emojiWorking)

	// Tool activity within a turn is shown in ONE message, edited in place as new
	// tools run (no new notification per tool). The message is finalized when an
	// answer arrives; any later tools start a fresh tool message.
	var toolMsgID string
	var toolLines []string
	var lastEdit time.Time
	finalizeTools := func() {
		if toolMsgID == "" {
			return
		}
		_ = s.reply.Edit(ctx, ls.threadID, toolMsgID, strings.Join(toolLines, "\n"))
		toolMsgID = ""
		toolLines = toolLines[:0]
	}
	posted := false

	done := ls.driver.RunTurn(ctx, agentproc.Turn{
		SessionRef: ls.ref(),
		Prompt:     tr.text,
		Workdir:    ls.workdir,
		Effort:     ls.effort,
		Name:       ls.name,
	}, func(e agentproc.Event) {
		switch ev := e.(type) {
		case agentproc.AssistantText:
			if strings.TrimSpace(ev.Text) == "" {
				return
			}
			finalizeTools()
			for _, chunk := range splitMessage(ev.Text, discordMax) {
				_, _ = s.reply.Post(ctx, ls.threadID, chunk)
			}
			posted = true
		case agentproc.ToolActivity:
			if strings.TrimSpace(ev.Label) == "" || len(toolLines) >= 25 {
				return
			}
			toolLines = append(toolLines, toolLine(ev.Label))
			content := strings.Join(toolLines, "\n")
			if toolMsgID == "" {
				toolMsgID, _ = s.reply.Post(ctx, ls.threadID, content)
				lastEdit = time.Now()
				return
			}
			if time.Since(lastEdit) >= toolEditInterval {
				_ = s.reply.Edit(ctx, ls.threadID, toolMsgID, content)
				lastEdit = time.Now()
			}
		}
	})

	// Session was stopped/archived mid-turn: cancelled on purpose, so clear the
	// working marker and stay quiet — StopThread posts the closing note.
	if ctx.Err() != nil {
		_ = s.reply.Unreact(ctx, tr.channelID, tr.messageID, emojiWorking)
		return
	}

	if done.SessionRef != "" {
		ls.setRef(done.SessionRef)
	}
	finalizeTools() // any trailing tool steps

	_ = s.reply.Unreact(ctx, tr.channelID, tr.messageID, emojiWorking)
	if done.Err != nil {
		_ = s.reply.React(ctx, tr.channelID, tr.messageID, emojiError)
		_, _ = s.reply.Post(ctx, ls.threadID, "error: "+done.Err.Error())
		return
	}
	if !posted {
		_, _ = s.reply.Post(ctx, ls.threadID, answerOrPlaceholder(""))
	}
	_ = s.reply.React(ctx, tr.channelID, tr.messageID, emojiDone)
}
```

- [ ] **Step 5: Run the full session test suite to verify pass**

Run: `go test ./internal/session -v`
Expected: PASS — including the updated `TestHeadless_SegmentPosting`, the new `TestHeadless_ToolMessageEditedNotReposted`, and the unchanged `TestHeadless_FirstTurnAndResume` (its single tool `Bash git status` is now the content of the first post, so `anyContains(r.posts, "Bash git status")` still holds).

- [ ] **Step 6: Commit**

```bash
stg new headless-single-tool-message -m "feat(session): one refreshing tool message per turn

Post a single tool-activity message and edit it in place (throttled) as
tools run, instead of re-posting per burst. Finalize on answer text.

Signed-off-by: Yves Brissaud <yves.brissaud@gmail.com>"
git add internal/session/headless.go internal/session/headless_test.go
stg refresh
```

---

## Task 3: Global session status on the top (triggering) message

Adds a global status marker on the thread's **root** message (the mention that opened the thread) that tracks the latest turn, replacing the prior status emoji. Per-turn reactions remain on in-thread follow-up messages (turns ≥ 2); for turn 1 the trigger *is* the root, so only the global marker applies.

**Files:**
- Modify: `internal/session/headless.go` (`liveSession` fields, `startHeadless`, `runTurn` reactions, new `setGlobalStatus` / `clearGlobalWorking`)
- Modify: `internal/session/headless_test.go` (add `TestHeadless_GlobalStatusTracksLatestTurn`, add `lastReactOn` helper)

- [ ] **Step 1: Write the failing test**

In `internal/session/headless_test.go`, add:

```go
func TestHeadless_GlobalStatusTracksLatestTurn(t *testing.T) {
	d := &fakeDriver{turns: []scripted{
		{texts: []string{"one"}, ref: "s"},
		{err: errors.New("boom"), ref: "s"},
	}}
	svc, r := newHeadlessService(d)

	svc.startHeadless(context.Background(), "claude", "thread-1", "/wt", "", "demo",
		turnReq{channelID: "c", messageID: "m1", text: "go"})
	svc.waitIdle("thread-1")

	if !svc.FeedThread(context.Background(), "thread-1", "thread-1", "m2", "again") {
		t.Fatalf("feed should report tracked thread")
	}
	svc.waitIdle("thread-1")

	// The root message (c|m1) must reflect the LATEST turn's status (error), not
	// stay stuck on the first turn's ✅.
	if last := lastReactOn(r.reacts, "c|m1"); last != emojiError {
		t.Fatalf("root message should show latest status %q, got %q (reacts=%v)", emojiError, last, r.reacts)
	}
	// The stale ✅ from turn 1 must have been cleared from the root message.
	if !hasStr(r.unreacts, "c|m1|"+emojiDone) {
		t.Errorf("stale done reaction not cleared from root message: %v", r.unreacts)
	}
	// Turn 2's in-thread message carries its own per-turn reactions.
	if !hasStr(r.reacts, "thread-1|m2|"+emojiWorking) || !hasStr(r.reacts, "thread-1|m2|"+emojiError) {
		t.Errorf("expected per-turn reactions on the in-thread message, got %v", r.reacts)
	}
}
```

Add this helper at the bottom of `internal/session/headless_test.go`:

```go
// lastReactOn returns the emoji of the last reaction recorded on channel|message
// (e.g. prefix "c|m1"), or "" if none.
func lastReactOn(reacts []string, chanMsg string) string {
	want := chanMsg + "|"
	last := ""
	for _, e := range reacts {
		if strings.HasPrefix(e, want) {
			last = strings.TrimPrefix(e, want)
		}
	}
	return last
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/session -run TestHeadless_GlobalStatusTracksLatestTurn -v`
Expected: FAIL — today the root message `c|m1` only gets reactions from turn 1, so its last reaction is `✅` (not `❌`) and `c|m1|✅` is never unreacted.

- [ ] **Step 3: Add `liveSession` fields and capture the root message**

In `internal/session/headless.go`, add three fields to the `liveSession` struct (after `sessionRef`):

```go
	sessionRef string // guarded by mu (read by PromoteThread from another goroutine)

	// Root (triggering) message + the status emoji currently shown on it. The
	// global status tracks the latest turn so the channel view stays current.
	// Touched only by the single runLoop goroutine.
	rootChannelID   string
	rootMessageID   string
	lastGlobalEmoji string
```

In `startHeadless`, set them from the first turn in the struct literal (add after `threadID: threadID,`):

```go
		threadID:      threadID,
		rootChannelID: first.channelID,
		rootMessageID: first.messageID,
```

- [ ] **Step 4: Add the status helpers and rewire `runTurn` reactions**

In `internal/session/headless.go`, add these two methods (e.g. just above `runTurn`):

```go
// setGlobalStatus reflects the session's current status on the thread's root
// (triggering) message so the channel view tracks the latest turn, replacing the
// previously-shown status emoji. Called only from the single runLoop goroutine.
func (s *Service) setGlobalStatus(ctx context.Context, ls *liveSession, emoji string) {
	prev := ls.lastGlobalEmoji
	if prev == emoji {
		return
	}
	if prev != "" {
		_ = s.reply.Unreact(ctx, ls.rootChannelID, ls.rootMessageID, prev)
	}
	_ = s.reply.React(ctx, ls.rootChannelID, ls.rootMessageID, emoji)
	ls.lastGlobalEmoji = emoji
}

// clearGlobalWorking removes the working marker from the root message when a turn
// is cancelled mid-flight, without claiming success or failure.
func (s *Service) clearGlobalWorking(ctx context.Context, ls *liveSession) {
	if ls.lastGlobalEmoji == emojiWorking {
		_ = s.reply.Unreact(ctx, ls.rootChannelID, ls.rootMessageID, emojiWorking)
		ls.lastGlobalEmoji = ""
	}
}
```

Then update `runTurn` (the version from Task 2) in three places.

Replace the opening reaction line:

```go
	// 👀 on the user's message while the agent works.
	_ = s.reply.React(ctx, tr.channelID, tr.messageID, emojiWorking)
```

with:

```go
	// Track the latest status globally on the root message (visible in the
	// channel). Turn 1's trigger IS the root, so it needs no separate per-turn
	// marker; in-thread follow-ups (turns ≥ 2) get their own.
	isRoot := tr.channelID == ls.rootChannelID && tr.messageID == ls.rootMessageID
	s.setGlobalStatus(ctx, ls, emojiWorking)
	if !isRoot {
		_ = s.reply.React(ctx, tr.channelID, tr.messageID, emojiWorking)
	}
```

Replace the cancelled-mid-turn block:

```go
	if ctx.Err() != nil {
		_ = s.reply.Unreact(ctx, tr.channelID, tr.messageID, emojiWorking)
		return
	}
```

with:

```go
	if ctx.Err() != nil {
		if !isRoot {
			_ = s.reply.Unreact(ctx, tr.channelID, tr.messageID, emojiWorking)
		}
		s.clearGlobalWorking(ctx, ls)
		return
	}
```

Replace the turn-end reaction block:

```go
	_ = s.reply.Unreact(ctx, tr.channelID, tr.messageID, emojiWorking)
	if done.Err != nil {
		_ = s.reply.React(ctx, tr.channelID, tr.messageID, emojiError)
		_, _ = s.reply.Post(ctx, ls.threadID, "error: "+done.Err.Error())
		return
	}
	if !posted {
		_, _ = s.reply.Post(ctx, ls.threadID, answerOrPlaceholder(""))
	}
	_ = s.reply.React(ctx, tr.channelID, tr.messageID, emojiDone)
```

with:

```go
	if !isRoot {
		_ = s.reply.Unreact(ctx, tr.channelID, tr.messageID, emojiWorking)
	}
	if done.Err != nil {
		if !isRoot {
			_ = s.reply.React(ctx, tr.channelID, tr.messageID, emojiError)
		}
		s.setGlobalStatus(ctx, ls, emojiError)
		_, _ = s.reply.Post(ctx, ls.threadID, "error: "+done.Err.Error())
		return
	}
	if !posted {
		_, _ = s.reply.Post(ctx, ls.threadID, answerOrPlaceholder(""))
	}
	if !isRoot {
		_ = s.reply.React(ctx, tr.channelID, tr.messageID, emojiDone)
	}
	s.setGlobalStatus(ctx, ls, emojiDone)
```

- [ ] **Step 5: Run the full session suite to verify pass**

Run: `go test ./internal/session -v`
Expected: PASS — the new `TestHeadless_GlobalStatusTracksLatestTurn`, plus unchanged tests. (`TestHeadless_FirstTurnAndResume`: turn 1 is the root, so `setGlobalStatus` reacts `c|m1|👀` then replaces it with `c|m1|✅` — both still present in `r.reacts`. `TestHeadless_ErrorKeepsSessionOpen`: `c|m3|👀` then `c|m3|❌` — `c|m3|❌` present.)

- [ ] **Step 6: Commit**

```bash
stg new headless-global-status -m "feat(session): live global status on the top message

Track the latest turn's status (working/done/error) on the thread's root
message so the channel view isn't stuck on the first turn's outcome.

Signed-off-by: Yves Brissaud <yves.brissaud@gmail.com>"
git add internal/session/headless.go internal/session/headless_test.go
stg refresh
```

---

## Task 4: Thread-title status icon

Wires the `titleUpdater` from Task 1 into the live session and the global-status path so the thread title is prefixed with the current status icon, best-effort.

**Files:**
- Modify: `internal/session/headless.go` (`liveSession.title` field, `startHeadless`, `close`, `setGlobalStatus`)
- Modify: `internal/session/headless_test.go` (add `TestHeadless_ThreadTitleStatusIcon`)

- [ ] **Step 1: Write the failing test**

In `internal/session/headless_test.go`, add:

```go
func TestHeadless_ThreadTitleStatusIcon(t *testing.T) {
	d := &fakeDriver{turns: []scripted{{texts: []string{"done"}, ref: "s"}}}
	svc, r := newHeadlessService(d)

	svc.startHeadless(context.Background(), "claude", "thread-1", "/wt", "", "demo",
		turnReq{channelID: "c", messageID: "m1", text: "go"})
	svc.waitIdle("thread-1")

	ls := svc.sessions["thread-1"]
	svc.StopThread(context.Background(), "thread-1") // close() stops the title updater
	<-ls.title.done                                  // wait for its final drain

	if len(r.renames) == 0 {
		t.Fatalf("expected at least one thread rename for the status icon")
	}
	last := r.renames[len(r.renames)-1]
	if want := "thread-1|" + emojiDone + " demo"; last != want {
		t.Fatalf("final title = %q, want %q (renames=%v)", last, want, r.renames)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/session -run TestHeadless_ThreadTitleStatusIcon -v`
Expected: FAIL — compile error `ls.title undefined` (no `title` field on `liveSession` yet).

- [ ] **Step 3: Add the `title` field and wire start/stop**

In `internal/session/headless.go`, add the field to `liveSession` (after `cancel`):

```go
	cancel context.CancelFunc
	title  *titleUpdater
```

In `startHeadless`, start it in the struct literal (add after the `cancel: cancel,` line):

```go
		cancel:        cancel,
		title:         newTitleUpdater(s.reply, threadID, name),
```

Replace the `close` method:

```go
func (ls *liveSession) close() {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	if !ls.closed {
		ls.closed = true
		ls.cancel()
		close(ls.stop)
	}
}
```

with:

```go
func (ls *liveSession) close() {
	ls.mu.Lock()
	first := !ls.closed
	if first {
		ls.closed = true
		ls.cancel()
		close(ls.stop)
	}
	ls.mu.Unlock()
	if first {
		ls.title.stop()
	}
}
```

- [ ] **Step 4: Drive the title from `setGlobalStatus`**

In `internal/session/headless.go`, add the title update to `setGlobalStatus` (last line before the closing brace):

```go
	_ = s.reply.React(ctx, ls.rootChannelID, ls.rootMessageID, emoji)
	ls.lastGlobalEmoji = emoji
	ls.title.set(emoji)
}
```

- [ ] **Step 5: Run the full session suite to verify pass**

Run: `go test ./internal/session -v`
Expected: PASS — including `TestHeadless_ThreadTitleStatusIcon` (turn enqueues `👀 demo` then `✅ demo`; `StopThread`→`close`→`title.stop` drains the latest, so the final applied rename is `thread-1|✅ demo`).

- [ ] **Step 6: Commit**

```bash
stg new headless-thread-title-icon -m "feat(session): status icon prefix on the thread title

Prefix the thread title with 👀/✅/❌ via the best-effort titleUpdater, so
session status is visible at a glance in the threads list.

Signed-off-by: Yves Brissaud <yves.brissaud@gmail.com>"
git add internal/session/headless.go internal/session/headless_test.go
stg refresh
```

---

## Task 5: Full verification

**Files:** none (verification only).

- [ ] **Step 1: Race detector on the session package**

Run: `go test -race ./internal/session`
Expected: PASS, no race warnings. (The title goroutine only ever touches `f.renames` on the fake; `runTurn`/`Post`/`React`/`Edit` touch disjoint fake fields; tests read `r.renames` only after `<-ls.title.done`.)

- [ ] **Step 2: Vet and full unit suite**

Run: `go vet ./... && go test ./...`
Expected: PASS (integration tests stay skipped without `QUACK_INTEGRATION=1`).

- [ ] **Step 3: Build the binary**

Run: `go build -o /tmp/quack-build ./cmd/quack`
Expected: builds with no errors.

- [ ] **Step 4: Confirm the patch series**

Run: `stg series`
Expected: the five patches stacked on `design-headless-status-ux`:
`titlestatus-updater`, `headless-single-tool-message`, `headless-global-status`, `headless-thread-title-icon` (Task 5 adds no patch).

---

## Self-Review

**Spec coverage:**
- "Single refreshing tool message" → Task 2 (post-then-edit, throttle, finalize on answer). ✔
- "No new notification per tool" → editing instead of posting (Task 2). ✔
- "Global status on top message, tracks latest turn" → Task 3 (`setGlobalStatus` on root, replaces prior emoji). ✔
- "Keep per-turn reaction on in-thread messages" → Task 3 (`!isRoot` per-turn React/Unreact). ✔
- "Status icon prefixed to thread title, best-effort, non-blocking, coalescing" → Task 1 (`titleUpdater`) + Task 4 (wiring). ✔
- "Headless-only; interactive untouched" → all changes are in `runTurn`/`startHeadless`/`close`/`liveSession`, none on the interactive path in `service.go`. ✔
- "Stay within rate limits without blocking a turn" → tool-edit throttle (Task 2) + background title goroutine (Task 1/4). ✔
- "25-line cap retained" → `len(toolLines) >= 25` guard (Task 2). ✔
- Edge cases: edit/rename best-effort (`_ =`), cancelled mid-turn clears the global working marker (`clearGlobalWorking`, Task 3), title coalescing latest-wins + drain-on-stop (Task 1). ✔

**Placeholder scan:** No TBD/TODO; every code step is complete and compiles in sequence.

**Type consistency:** `titleUpdater`/`newTitleUpdater`/`set`/`stop`/`done` are used identically in Tasks 1 and 4. `setGlobalStatus`/`clearGlobalWorking` signatures match their call sites. `lastEditTo`/`lastReactOn`/`anyContains`/`hasStr` test helpers are each defined once. `toolEditInterval`, `toolLines`, `toolMsgID`, `rootChannelID`, `rootMessageID`, `lastGlobalEmoji` are referenced consistently across tasks.
