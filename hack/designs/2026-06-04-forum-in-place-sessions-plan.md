# Forum in-place sessions — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** When a mention lands inside an existing Discord thread (the common case being a forum post), run the session *in place* — drive that thread instead of opening a new one, use the post's own title as the Discord identity with a status emoji, and leave the user's post open when the session stops.

**Architecture:** Additive to the existing thread-opening flow. The Discord adapter detects "this mention is already inside a thread" (`Channel.IsThread()`), authorizes against the thread's parent channel, and carries two new fields on `session.Request`. `Service.run` skips `OpenThread` for in-place requests. The headless title machinery gains a "title base" override (the post name, verbatim, no `owner/repo` label) and an `inPlace` flag that suppresses archive-on-stop; both persist so they survive a quack restart.

**Tech Stack:** Go, `github.com/bwmarrin/discordgo`, the `internal/session` orchestrator (unit-tested with fakes), `internal/discord` adapter.

**Build/test commands** (Go lives at `/usr/local/go/bin`; export it on PATH first in each shell):

```sh
export PATH=$PATH:/usr/local/go/bin
go test ./internal/session/ ./internal/discord/   # the two packages this plan touches
go vet ./...
go build -o ~/.local/bin/quack ./cmd/quack
```

Version control is Stacked Git (stg). Each task ends with an `stg` patch carrying a
`Signed-off-by: Yves Brissaud <yves.brissaud@gmail.com>` trailer and **no** AI
`Co-Authored-By` lines. Create a patch with `stg new <name> -m "<msg>"` then `stg refresh`
after staging; refine an existing patch with `stg refresh`.

---

## File Structure

Files created/modified, each with one responsibility:

- **`internal/session/service.go`** — add `Request.InThread` / `Request.ThreadName`; `run` skips `OpenThread` in place and passes in-place options to `startHeadless`.
- **`internal/discord/bot.go`** — detect an in-thread mention (`resolveChannel`, `threadContext`), authorize against the parent (`authorizedParent`), populate the new `Request` fields in `onMessage`.
- **`internal/session/headless.go`** — `liveSession` gains `inPlace` + `titleBase`; `startHeadless` accepts optional in-place options (variadic, so existing call sites are untouched); `StopThread` skips `ArchiveThread` when in place.
- **`internal/session/persist.go`** — `sessionRecord` gains `InPlace` + `TitleBase`; `record()` snapshots them; `newSession` restores them and feeds the title machinery via a new `titleParts` helper.
- **Tests:** `internal/discord/bot_test.go`, `internal/session/service_test.go`, `internal/session/persist_test.go`.
- **`AGENTS.md`** — one-line note in the request-flow section that an in-thread mention runs in place.

Note on `titlestatus.go`: **unchanged**. The title override is achieved by feeding the existing `titleUpdater` the post name as its `name` and an empty `label`, so `threadTitle(emoji, "", base)` renders `"👀 <post name>"`. No change to that file is needed.

---

## Task 1: `run` skips `OpenThread` for an in-place mention

**Files:**
- Modify: `internal/session/service.go` — `Request` struct (~line 112), `run` `OpenThread` block (~line 210)
- Test: `internal/session/service_test.go`

- [ ] **Step 1: Add the two `Request` fields.**

In `internal/session/service.go`, the `Request` struct currently reads:

```go
// Request is one parsed-but-unprocessed Discord command.
type Request struct {
	Content     string       // mention-stripped
	Attachments []Attachment // files dropped on the command (e.g. screenshots)
	Origin      Origin       // guild/channel/message/author/createdAt set; thread/reply empty
}
```

Add the in-place fields:

```go
// Request is one parsed-but-unprocessed Discord command.
type Request struct {
	Content     string       // mention-stripped
	Attachments []Attachment // files dropped on the command (e.g. screenshots)
	Origin      Origin       // guild/channel/message/author/createdAt set; thread/reply empty

	// InThread is set when the mention arrived inside an existing thread (commonly
	// a forum post). The session then runs in place in that thread — Origin.ChannelID
	// is the thread id — instead of opening a new one. ThreadName is the thread's
	// current title, used verbatim as the Discord-facing session title.
	InThread   bool
	ThreadName string
}
```

- [ ] **Step 2: Skip `OpenThread` in place.**

In `run`, replace this block:

```go
	threadID, err := s.reply.OpenThread(ctx, req.Origin.ChannelID, req.Origin.MessageID, provisional, s.cfg.ThreadAutoArchiveMin)
	if err != nil {
		threadID = req.Origin.ChannelID
	}
	req.Origin.ThreadID = threadID
```

with:

```go
	// A mention already inside a thread (a forum post) runs in place: drive that
	// thread directly. Discord can't nest threads anyway, so OpenThread would only
	// fail here. Otherwise open a fresh thread off the triggering message.
	threadID := req.Origin.ChannelID
	if !req.InThread {
		if id, err := s.reply.OpenThread(ctx, req.Origin.ChannelID, req.Origin.MessageID, provisional, s.cfg.ThreadAutoArchiveMin); err == nil {
			threadID = id
		}
	}
	req.Origin.ThreadID = threadID
```

- [ ] **Step 3: Write the failing test.**

Add to `internal/session/service_test.go`:

```go
// A mention already inside a thread (a forum post) runs in place: quack drives
// that thread (Origin.ChannelID) and never opens a new one.
func TestHandle_InThread_RunsInPlace(t *testing.T) {
	svc, _, tx, r, _ := newTestService()

	svc.Handle(context.Background(), Request{
		Content:    "! no-headless\nhi",
		InThread:   true,
		ThreadName: "Help with login",
		Origin:     Origin{GuildID: "g", ChannelID: "post1", MessageID: "m", AuthorID: "u", Author: "alice", CreatedAt: "2026-06-04T17:00:00Z"},
	})

	if len(r.threads) != 0 {
		t.Fatalf("OpenThread must be skipped in place; threads = %v", r.threads)
	}
	if len(tx.created) != 1 {
		t.Fatalf("expected one tmux session, got %d", len(tx.created))
	}
	// Ack/progress posts go into the post itself, not a new thread.
	for _, p := range r.posts {
		if p.channel != "post1" {
			t.Errorf("post to %q, want the in-place thread post1: %+v", p.channel, p)
		}
	}
	// The start-rename is skipped in place (threadID == channelID), so the user's
	// post title is left untouched on the interactive path.
	if len(r.renames) != 0 {
		t.Errorf("no rename expected in place on the no-headless path; renames = %v", r.renames)
	}
}
```

- [ ] **Step 4: Run it; expect FAIL before Step 2 is applied, PASS after.**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/session/ -run TestHandle_InThread_RunsInPlace -v`
Expected: PASS (with Steps 1–2 applied). If you wrote the test first against unmodified code, it FAILs because `OpenThread` records a thread.

- [ ] **Step 5: Full package test + vet.**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/session/ && go vet ./internal/session/`
Expected: ok.

- [ ] **Step 6: Commit (stg).**

```sh
git add internal/session/service.go internal/session/service_test.go
stg new forum-run-inplace -m "session: run in place when mentioned inside a thread

When a mention arrives inside an existing thread (commonly a forum post),
skip OpenThread and drive that thread directly. Carry the in-thread marker
and the thread's title on session.Request.

Signed-off-by: Yves Brissaud <yves.brissaud@gmail.com>"
stg refresh
```

---

## Task 2: Detect the in-thread mention in the Discord adapter

**Files:**
- Modify: `internal/discord/bot.go` — `onMessage` mention path (~lines 95–125), new helpers near the other `authorized*` helpers
- Test: `internal/discord/bot_test.go`

- [ ] **Step 1: Add the detection + auth helpers.**

In `internal/discord/bot.go`, add these near the existing `authorized*` helpers (after `authorizedReaction`, before `allows`):

```go
// resolveChannel returns the channel for channelID, preferring the gateway state
// cache and falling back to a REST fetch. Returns nil when it can't be resolved —
// the caller treats that as "not a thread", so the normal open-a-thread path runs.
func resolveChannel(s *discordgo.Session, channelID string) *discordgo.Channel {
	if s.State != nil {
		if ch, err := s.State.Channel(channelID); err == nil && ch != nil {
			return ch
		}
	}
	ch, err := s.Channel(channelID)
	if err != nil {
		return nil
	}
	return ch
}

// threadContext reports whether a mention's channel is an existing thread — a
// forum post or a regular thread — that quack should run in place rather than
// opening a sub-thread (Discord can't nest threads). When it is, it returns the
// thread's display name and its parent channel id; the parent is what a channel
// allowlist is checked against, since the thread id itself is never listed.
func threadContext(ch *discordgo.Channel) (inThread bool, name, parentID string) {
	if ch == nil || !ch.IsThread() {
		return false, "", ""
	}
	return true, ch.Name, ch.ParentID
}

// authorizedParent gates a mention that arrived inside a thread: user+guild as
// usual, with the channel dimension checked against the thread's PARENT channel.
func (b *Bot) authorizedParent(m *discordgo.MessageCreate, parentID string) bool {
	return allows(b.allowed.UserIDs, m.Author.ID) &&
		allows(b.allowed.GuildIDs, m.GuildID) &&
		allows(b.allowed.ChannelIDs, parentID)
}
```

- [ ] **Step 2: Wire detection into `onMessage`.**

In `onMessage`, replace this block (it currently sits right after the `mentionsBot` check):

```go
	if !b.authorized(m) {
		_, _ = s.ChannelMessageSend(m.ChannelID, "🦆 not authorized")
		return
	}

	content := stripMention(m.Content, botID, botRoles)
```

with:

```go
	// A mention inside an existing thread (commonly a forum post) runs in place:
	// quack drives that thread rather than opening a new one. Resolve the channel
	// to detect it and to authorize against the thread's parent — a thread/post id
	// is never itself in a channel allowlist.
	ch := resolveChannel(s, m.ChannelID)
	inThread, threadName, parentID := threadContext(ch)
	authed := b.authorized(m)
	if inThread {
		authed = b.authorizedParent(m, parentID)
	}
	if !authed {
		_, _ = s.ChannelMessageSend(m.ChannelID, "🦆 not authorized")
		return
	}

	content := stripMention(m.Content, botID, botRoles)
```

- [ ] **Step 3: Populate the new `Request` fields.**

Still in `onMessage`, the request is built as:

```go
	req := session.Request{
		Content:     content,
		Attachments: toAttachments(m.Attachments),
		Origin:      origin,
	}
	go b.svc.Handle(context.Background(), req)
```

Change it to:

```go
	req := session.Request{
		Content:     content,
		Attachments: toAttachments(m.Attachments),
		Origin:      origin,
		InThread:    inThread,
		ThreadName:  threadName,
	}
	go b.svc.Handle(context.Background(), req)
```

- [ ] **Step 4: Write the failing tests.**

Add to `internal/discord/bot_test.go`:

```go
// threadContext recognizes a thread (forum post or regular thread) by its channel
// type and surfaces the post name + parent id; a normal text channel is not in place.
func TestThreadContext(t *testing.T) {
	forumPost := &discordgo.Channel{
		Type:     discordgo.ChannelTypeGuildPublicThread,
		Name:     "Help with login",
		ParentID: "forum1",
	}
	in, name, parent := threadContext(forumPost)
	if !in || name != "Help with login" || parent != "forum1" {
		t.Errorf("threadContext(forum post) = %v,%q,%q; want true,\"Help with login\",\"forum1\"", in, name, parent)
	}

	textChan := &discordgo.Channel{Type: discordgo.ChannelTypeGuildText, Name: "general"}
	if in, _, _ := threadContext(textChan); in {
		t.Error("a normal text channel is not an in-place thread")
	}

	if in, _, _ := threadContext(nil); in {
		t.Error("an unresolved channel (nil) is not an in-place thread")
	}
}

// An in-thread mention is authorized against the thread's PARENT channel, since a
// thread/post id is never itself in a channel allowlist.
func TestAuthorizedParent(t *testing.T) {
	open := &Bot{}
	if !open.authorizedParent(authMsg("u9", "g9", "post1"), "forum1") {
		t.Error("empty allowlist should authorize any in-thread mention")
	}

	b := &Bot{allowed: Allow{ChannelIDs: []string{"forum1"}}}
	if !b.authorizedParent(authMsg("u", "g", "post1"), "forum1") {
		t.Error("parent channel forum1 is allowlisted, should authorize")
	}
	if b.authorizedParent(authMsg("u", "g", "post1"), "forum2") {
		t.Error("parent channel forum2 not in allowlist should be rejected")
	}
	if b.authorized(authMsg("u", "g", "post1")) {
		t.Error("guard: the post id itself is not in the channel allowlist (proves why parent-based auth is needed)")
	}
}
```

- [ ] **Step 5: Run; expect FAIL before Steps 1–3, PASS after.**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/discord/ -run 'TestThreadContext|TestAuthorizedParent' -v`
Expected: PASS.

- [ ] **Step 6: Full package test + vet.**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/discord/ && go vet ./internal/discord/`
Expected: ok. (`onMessage`/`resolveChannel` need a live session and are exercised manually, not in unit tests — consistent with the existing `bot_test.go`, which tests helper functions only.)

- [ ] **Step 7: Commit (stg).**

```sh
git add internal/discord/bot.go internal/discord/bot_test.go
stg new forum-detect-thread -m "discord: detect a mention inside a thread as in-place

Resolve the mention's channel; if it is a thread (forum post or regular
thread), mark the request in-place, carry the thread title, and authorize
the channel allowlist against the thread's parent.

Signed-off-by: Yves Brissaud <yves.brissaud@gmail.com>"
stg refresh
```

---

## Task 3: Post-name title + leave the post open on stop (headless)

**Files:**
- Modify: `internal/session/headless.go` — `liveSession` struct (~line 23), `startHeadless` (~line 56), `StopThread` archive (~line 117)
- Modify: `internal/session/persist.go` — `sessionRecord` (~line 20), `record()` (~line 34), `newSession` (~line 72), new `titleParts` helper
- Modify: `internal/session/service.go` — `run` headless branch passes in-place options (~line 289)
- Test: `internal/session/persist_test.go`

- [ ] **Step 1: Add the in-place options type and `liveSession` fields.**

In `internal/session/headless.go`, add this type just above `type liveSession struct` (after the `turnReq` type):

```go
// inPlaceOpts carries the extras for a session that runs inside a user-owned
// thread (a forum post): titleBase is the post's own name, used verbatim as the
// Discord title (no owner/repo label); inPlace leaves the thread open on stop
// instead of archiving it. The zero value is an ordinary auto-created thread.
type inPlaceOpts struct {
	inPlace   bool
	titleBase string
}
```

Then add two fields to `liveSession` (next to `name` / `label`):

```go
	name       string
	label      string // workspace label shown in the thread title (owner/repo or dir)
	titleBase  string // verbatim Discord title (the post name); empty => name+label
	inPlace    bool   // session runs in a user-owned thread; don't archive on stop
	threadID   string
```

- [ ] **Step 2: Accept the options in `startHeadless` (variadic, so existing call sites are untouched).**

Change the signature and record construction. Current:

```go
func (s *Service) startHeadless(ctx context.Context, agentName, threadID, workdir, effort, name, label string, first turnReq) {
	ls := s.newSession(ctx, sessionRecord{
		Name:          name,
		Label:         label,
		AgentName:     agentName,
		Workdir:       workdir,
		Effort:        effort,
		ThreadID:      threadID,
		RootChannelID: first.channelID,
		RootMessageID: first.messageID,
	})
```

Change to:

```go
func (s *Service) startHeadless(ctx context.Context, agentName, threadID, workdir, effort, name, label string, first turnReq, opts ...inPlaceOpts) {
	var ip inPlaceOpts
	if len(opts) > 0 {
		ip = opts[0]
	}
	ls := s.newSession(ctx, sessionRecord{
		Name:          name,
		Label:         label,
		TitleBase:     ip.titleBase,
		InPlace:       ip.inPlace,
		AgentName:     agentName,
		Workdir:       workdir,
		Effort:        effort,
		ThreadID:      threadID,
		RootChannelID: first.channelID,
		RootMessageID: first.messageID,
	})
```

- [ ] **Step 3: Skip archive-on-stop for an in-place session.**

In `StopThread`, the tail currently reads:

```go
	s.markGlobalStopped(ctx, ls)
	_, _ = s.reply.Post(ctx, threadID, "session stopped")
	// Close the thread now the session is gone. It's already removed from the
	// tracking map, so the resulting archive event no-ops in onThreadUpdate.
	_ = s.reply.ArchiveThread(ctx, threadID)
	return true
```

Change to:

```go
	s.markGlobalStopped(ctx, ls)
	_, _ = s.reply.Post(ctx, threadID, "session stopped")
	// Close an auto-created thread now the session is gone (it's already removed
	// from the tracking map, so the archive event no-ops in onThreadUpdate). An
	// in-place thread is the user's own (a forum post) — leave it open.
	if !ls.inPlace {
		_ = s.reply.ArchiveThread(ctx, threadID)
	}
	return true
```

- [ ] **Step 4: Persist the new fields and feed the title machinery.**

In `internal/session/persist.go`, add two fields to `sessionRecord` (after `Label`):

```go
	Name          string `json:"name"`
	Label         string `json:"label"` // workspace label for the thread title (owner/repo or dir)
	TitleBase     string `json:"title_base,omitempty"` // verbatim Discord title (post name); empty => name+label
	InPlace       bool   `json:"in_place,omitempty"`   // user-owned thread; don't archive on stop
	AgentName     string `json:"agent_name"`
```

In `record()`, snapshot them (after `Label: ls.label,`):

```go
		Name:          ls.name,
		Label:         ls.label,
		TitleBase:     ls.titleBase,
		InPlace:       ls.inPlace,
		AgentName:     ls.agentName,
```

Add the `titleParts` helper (place it just above `newSession`):

```go
// titleParts chooses what the thread title is built from. An in-place session
// (a forum post) uses the post's own name verbatim with no workspace label, so
// threadTitle renders "<emoji> <post name>". An ordinary session uses the
// workspace label + session name, as before.
func titleParts(rec sessionRecord) (name, label string) {
	if rec.TitleBase != "" {
		return rec.TitleBase, ""
	}
	return rec.Name, rec.Label
}
```

In `newSession`, restore the fields onto the `liveSession` and build the title updater from `titleParts`. Current:

```go
		name:      rec.Name,
		label:     rec.Label,
		threadID:  rec.ThreadID,

		sessionRef:    rec.SessionRef,
		rootChannelID: rec.RootChannelID,
		rootMessageID: rec.RootMessageID,

		queue:  make(chan turnReq, 32),
		done:   make(chan struct{}),
		stop:   make(chan struct{}),
		cancel: cancel,
		title:  newTitleUpdater(s.reply, rec.ThreadID, rec.Name, rec.Label),
	}
```

Change to:

```go
		name:      rec.Name,
		label:     rec.Label,
		titleBase: rec.TitleBase,
		inPlace:   rec.InPlace,
		threadID:  rec.ThreadID,

		sessionRef:    rec.SessionRef,
		rootChannelID: rec.RootChannelID,
		rootMessageID: rec.RootMessageID,

		queue:  make(chan turnReq, 32),
		done:   make(chan struct{}),
		stop:   make(chan struct{}),
		cancel: cancel,
		title:  newTitleUpdater(s.reply, rec.ThreadID, titleName, titleLabel),
	}
```

and immediately before the `ls := &liveSession{` line, compute the title parts:

```go
	titleName, titleLabel := titleParts(rec)
	turnCtx, cancel := context.WithCancel(ctx)
	ls := &liveSession{
```

(Move/keep the existing `turnCtx, cancel := context.WithCancel(ctx)` line right after the `titleParts` call — there must be exactly one such line.)

- [ ] **Step 5: Pass the options from `run`.**

In `internal/session/service.go`, the headless branch calls `startHeadless`:

```go
		s.startHeadless(ctx, agentName, threadID, prep.workdir, effort, prep.name, prep.label,
			turnReq{channelID: req.Origin.ChannelID, messageID: req.Origin.MessageID, text: fullPrompt})
```

Change to:

```go
		s.startHeadless(ctx, agentName, threadID, prep.workdir, effort, prep.name, prep.label,
			turnReq{channelID: req.Origin.ChannelID, messageID: req.Origin.MessageID, text: fullPrompt},
			inPlaceOpts{inPlace: req.InThread, titleBase: req.ThreadName})
```

- [ ] **Step 6: Write the failing tests.**

Add to `internal/session/persist_test.go`:

```go
// titleParts: an in-place session titles the thread with the post name verbatim
// (no label); an ordinary session keeps the workspace label + session name.
func TestTitleParts(t *testing.T) {
	name, label := titleParts(sessionRecord{Name: "demo", Label: "acme/widget"})
	if name != "demo" || label != "acme/widget" {
		t.Errorf("ordinary: got %q,%q want demo,acme/widget", name, label)
	}
	name, label = titleParts(sessionRecord{Name: "demo", Label: "acme/widget", TitleBase: "Help with login"})
	if name != "Help with login" || label != "" {
		t.Errorf("in-place: got %q,%q want \"Help with login\",\"\"", name, label)
	}
}

// An in-place headless session persists its title base + inPlace flag, and /stop
// leaves the user's thread OPEN (no archive).
func TestHeadless_InPlace_PersistsAndKeepsThreadOpen(t *testing.T) {
	d := &fakeDriver{turns: []scripted{{texts: []string{"ok"}, ref: "s"}}}
	svc, _, r, fs := newHeadlessServiceFakes(d)

	svc.startHeadless(context.Background(), "claude", "post1", "/wt", "high", "demo", "acme/widget",
		turnReq{channelID: "post1", messageID: "m1", text: "go"},
		inPlaceOpts{inPlace: true, titleBase: "Help with login"})
	svc.waitIdle("post1")

	rec, ok := readRecord(t, fs, "demo")
	if !ok {
		t.Fatalf("no record persisted; files=%v", keys(fs))
	}
	if rec.TitleBase != "Help with login" || !rec.InPlace {
		t.Errorf("record TitleBase=%q InPlace=%v, want \"Help with login\"/true", rec.TitleBase, rec.InPlace)
	}

	svc.StopThread(context.Background(), "post1")
	for _, id := range r.archived {
		if id == "post1" {
			t.Errorf("in-place thread post1 must not be archived on stop; archived=%v", r.archived)
		}
	}
	if !anyContains(r.posts, "session stopped") {
		t.Errorf("expected a 'session stopped' post; posts=%v", r.posts)
	}
}

// A rehydrated in-place session keeps its inPlace flag, so a restart-then-stop
// still leaves the user's thread open.
func TestHeadless_RehydrateInPlaceKeepsThreadOpen(t *testing.T) {
	d := &fakeDriver{}
	svc, g, r, fs := newHeadlessServiceFakes(d)
	g.pathExists["/wt"] = true
	seedRecord(fs, sessionRecord{
		Name: "demo", AgentName: "claude", Workdir: "/wt",
		ThreadID: "post1", RootChannelID: "post1", RootMessageID: "m1", SessionRef: "s1",
		TitleBase: "Help with login", InPlace: true,
	})

	if n := svc.Rehydrate(context.Background()); n != 1 {
		t.Fatalf("Rehydrate restored %d, want 1", n)
	}
	if got := svc.sessions["post1"]; got == nil || !got.inPlace || got.titleBase != "Help with login" {
		t.Fatalf("restored session lost in-place state: %+v", got)
	}

	svc.StopThread(context.Background(), "post1")
	for _, id := range r.archived {
		if id == "post1" {
			t.Errorf("rehydrated in-place thread must not be archived on stop; archived=%v", r.archived)
		}
	}
}
```

- [ ] **Step 7: Run; expect FAIL before Steps 1–5, PASS after.**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/session/ -run 'TestTitleParts|TestHeadless_InPlace_PersistsAndKeepsThreadOpen|TestHeadless_RehydrateInPlaceKeepsThreadOpen' -v`
Expected: PASS.

- [ ] **Step 8: Full session package test + vet.**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/session/ && go vet ./internal/session/`
Expected: ok. (All existing `startHeadless` call sites compile unchanged because the new options are variadic.)

- [ ] **Step 9: Commit (stg).**

```sh
git add internal/session/headless.go internal/session/persist.go internal/session/service.go internal/session/persist_test.go
stg new forum-title-and-keep-open -m "session: title in-place sessions with the post name, keep the post open

A session running inside a forum post titles the thread with the post's own
name plus the status emoji (no owner/repo label), and leaves the post open
on /stop instead of archiving it. Both survive a restart via the session
record.

Signed-off-by: Yves Brissaud <yves.brissaud@gmail.com>"
stg refresh
```

---

## Task 4: Docs + whole-suite verification

**Files:**
- Modify: `AGENTS.md` — request-flow note
- No test; this task verifies the full build/test/vet.

- [ ] **Step 1: Note in-place handling in `AGENTS.md`.**

In `AGENTS.md`, in the **Request flow** section, step 1 ends with "dispatch `Service.Handle` in its own goroutine. A message **in a tracked thread** is routed instead to `FeedThread` / `/stop` …". Append a sentence to that step:

```
A fresh mention **already inside a thread** (the common case: a Discord forum
post) is detected via the channel type and runs **in place** — quack drives that
thread instead of opening a new one, titles it with the post's own name plus the
status emoji, authorizes the channel allowlist against the thread's **parent**,
and leaves the post open on `/stop` (it's the user's). `Request.InThread` /
`Request.ThreadName` carry this; see
`hack/designs/2026-06-04-forum-in-place-sessions.md`.
```

- [ ] **Step 2: Whole-suite test + vet + build.**

Run:

```sh
export PATH=$PATH:/usr/local/go/bin
go test ./...
go vet ./...
go build -o ~/.local/bin/quack ./cmd/quack
```

Expected: all packages `ok`, vet clean, build succeeds.

- [ ] **Step 3: Commit (stg).**

```sh
git add AGENTS.md
stg new forum-docs -m "docs: note in-place forum handling in the request flow

Signed-off-by: Yves Brissaud <yves.brissaud@gmail.com>"
stg refresh
```

- [ ] **Step 4 (optional, on request): deploy + smoke test.**

`AGENTS.md` deploy: `go build -o ~/.local/bin/quack ./cmd/quack` then restart the unit. If running from inside a quack headless session, use the detached form so the restart doesn't kill the current turn:

```sh
systemd-run --user --on-active=10 systemctl --user restart quack.service
```

Manual smoke test: in a Discord **forum** channel, create a post and `@quack` inside it. Expect: no new thread; the post title becomes `👀 <post name>` while working and `✅ <post name>` when done; replies in the post feed the session; `/stop` ends it and leaves the post open.

---

## Self-Review

**Spec coverage** — every design section maps to a task:
- Detection (`IsThread`, parent auth, `Request` fields) → Task 2 (+ fields in Task 1).
- In-place launch (skip `OpenThread`) → Task 1.
- Title = post name + status emoji → Task 3 (`titleParts` + `newSession` wiring; `titlestatus.go` reused unchanged, as the design's note allows).
- Don't archive a user-owned post on stop → Task 3 (`StopThread`).
- Persistence (`InPlace` + title base) → Task 3 (`sessionRecord`, `record`, `newSession`, rehydrate test).
- Non-goals (forum tags, starter-message context) → intentionally absent.

**Placeholder scan** — no TBD/TODO; every code step shows complete code; every test step shows full test code and the exact command + expected result.

**Type/name consistency** — `Request.InThread`/`Request.ThreadName`, `inPlaceOpts{inPlace,titleBase}`, `sessionRecord.InPlace`/`TitleBase`, `liveSession.inPlace`/`titleBase`, `titleParts(rec)`, `threadContext`, `authorizedParent`, `resolveChannel` are used identically across all tasks. `startHeadless` stays source-compatible with all existing call sites (variadic options), so no other call site needs editing.
