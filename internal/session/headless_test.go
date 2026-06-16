package session

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/eunomie/quack/internal/agentproc"
)

type fakeDriver struct {
	turns   []scripted
	seen    []agentproc.Turn
	suggest string // returned by SuggestName ("" => caller falls back)

	oneShot           string   // returned by OneShot
	oneShotErr        error    // returned by OneShot
	oneShotSeen       []string // prompts passed to OneShot
	oneShotEffortSeen []string // efforts passed to OneShot
}

func (f *fakeDriver) OneShot(ctx context.Context, prompt, effort string) (string, error) {
	f.oneShotSeen = append(f.oneShotSeen, prompt)
	f.oneShotEffortSeen = append(f.oneShotEffortSeen, effort)
	return f.oneShot, f.oneShotErr
}

func (f *fakeDriver) SuggestName(ctx context.Context, prompt string) (string, error) {
	return f.suggest, nil
}

type scripted struct {
	texts  []string
	tools  []string
	events []agentproc.Event // if set, emitted in this exact order (overrides texts/tools)
	ref    string
	err    error
}

func (f *fakeDriver) RunTurn(ctx context.Context, t agentproc.Turn, emit func(agentproc.Event)) agentproc.TurnDone {
	i := len(f.seen)
	f.seen = append(f.seen, t)
	s := f.turns[i]
	if len(s.events) > 0 {
		for _, e := range s.events {
			emit(e)
		}
	} else {
		for _, x := range s.tools {
			emit(agentproc.ToolActivity{Label: x})
		}
		for _, x := range s.texts {
			emit(agentproc.AssistantText{Text: x})
		}
	}
	return agentproc.TurnDone{SessionRef: s.ref, Err: s.err}
}

func newHeadlessService(d agentproc.Driver) (*Service, *fakeReplier) {
	svc, _, r, _ := newHeadlessServiceFakes(d)
	return svc, r
}

// newHeadlessServiceFakes is like newHeadlessService but also exposes the git
// and in-memory FS fakes, for tests that assert on persistence/rehydration. The
// FS funcs are routed through memFS so unit tests never touch the real disk.
func newHeadlessServiceFakes(d agentproc.Driver) (*Service, *fakeGit, *fakeReplier, *memFS) {
	g, r, fs := newFakeGit(), newFakeReplier(), newMemFS()
	svc := New(Config{StateDir: "/state"}, g, newFakeTmux(), r)
	svc.drivers = map[string]agentproc.Driver{"claude": d}
	svc.mkdirAll = fs.mkdirAll
	svc.writeFile = fs.writeFile
	svc.remove = fs.remove
	svc.readDir = fs.readDir
	svc.readFile = fs.readFile
	return svc, g, r, fs
}

func TestHeadless_FirstTurnAndResume(t *testing.T) {
	d := &fakeDriver{turns: []scripted{
		{texts: []string{"Hi there."}, tools: []string{"Bash git status"}, ref: "sess-1"},
		{texts: []string{"Did it."}, ref: "sess-1"},
	}}
	svc, r := newHeadlessService(d)

	svc.startHeadless(context.Background(), "claude", "thread-1", "/wt", "high", "sess-1", "",
		RoleOwner, nil, "owner", turnReq{channelID: "c", messageID: "m1", text: "<quack-context>...</quack-context>\n\nDo the thing."})
	svc.waitIdle("thread-1")

	if len(d.seen) != 1 || d.seen[0].SessionRef != "" || d.seen[0].Workdir != "/wt" {
		t.Fatalf("first turn = %+v", d.seen)
	}
	if !anyContains(r.posts, "Hi there.") {
		t.Fatalf("answer not posted: %v", r.posts)
	}
	if !anyContains(r.posts, "Bash git status") {
		t.Fatalf("tool activity not posted: %v", r.posts)
	}
	// 👀 while working, then ✅ on the user's message.
	if !hasStr(r.reacts, "c|m1|"+emojiWorking) || !hasStr(r.reacts, "c|m1|"+emojiDone) {
		t.Errorf("expected working+done reactions on the trigger message, got %v", r.reacts)
	}

	if !svc.FeedThread(context.Background(), "thread-1", "thread-1", "m4", "now commit", nil, Caller{Role: RoleOwner, UserID: "owner"}) {
		t.Fatalf("feed should report tracked thread")
	}
	svc.waitIdle("thread-1")
	if len(d.seen) != 2 || d.seen[1].SessionRef != "sess-1" || d.seen[1].Prompt != "now commit" {
		t.Fatalf("resume turn = %+v", d.seen)
	}
}

func TestHeadless_StopEndsSession(t *testing.T) {
	d := &fakeDriver{turns: []scripted{{texts: []string{"ok"}, ref: "s"}}}
	svc, r := newHeadlessService(d)
	svc.startHeadless(context.Background(), "claude", "thread-2", "/wt", "", "sess-2", "", RoleOwner, nil, "owner", turnReq{channelID: "c", messageID: "m2", text: "go"})
	svc.waitIdle("thread-2")
	if !svc.StopThread(context.Background(), "thread-2", Caller{Role: RoleOwner, UserID: "owner"}) {
		t.Fatalf("stop should report it ended a tracked session")
	}
	if svc.Tracked("thread-2") {
		t.Fatalf("session still tracked after stop")
	}
	if !hasStr(r.archived, "thread-2") {
		t.Fatalf("thread should be archived (closed) after stop, got %v", r.archived)
	}
	if svc.FeedThread(context.Background(), "thread-2", "thread-2", "m5", "hello", nil, Caller{Role: RoleOwner, UserID: "owner"}) {
		t.Fatalf("feed should report false for untracked thread")
	}
}

// A stop reaction inside the thread (channel == thread id) ends the session.
func TestHeadless_StopByMessageInThread(t *testing.T) {
	d := &fakeDriver{turns: []scripted{{texts: []string{"ok"}, ref: "s"}}}
	svc, _ := newHeadlessService(d)
	svc.startHeadless(context.Background(), "claude", "thread-2", "/wt", "", "sess-2", "", RoleOwner, nil, "owner", turnReq{channelID: "c", messageID: "m2", text: "go"})
	svc.waitIdle("thread-2")
	if !svc.StopByMessage(context.Background(), "thread-2", "any-thread-msg", Caller{Role: RoleOwner, UserID: "owner"}) {
		t.Fatalf("reaction in the thread should stop the session")
	}
	if svc.Tracked("thread-2") {
		t.Fatalf("session still tracked after stop")
	}
}

// A stop reaction on the original triggering message in the parent channel
// (matched by recorded root channel+message) ends the session too.
func TestHeadless_StopByMessageOnRoot(t *testing.T) {
	d := &fakeDriver{turns: []scripted{{texts: []string{"ok"}, ref: "s"}}}
	svc, _ := newHeadlessService(d)
	svc.startHeadless(context.Background(), "claude", "thread-2", "/wt", "", "sess-2", "", RoleOwner, nil, "owner", turnReq{channelID: "c", messageID: "m2", text: "go"})
	svc.waitIdle("thread-2")
	// Wrong message id in the right channel must not match.
	if svc.StopByMessage(context.Background(), "c", "other", Caller{Role: RoleOwner, UserID: "owner"}) {
		t.Fatalf("a different message in the root channel should not match")
	}
	if !svc.StopByMessage(context.Background(), "c", "m2", Caller{Role: RoleOwner, UserID: "owner"}) {
		t.Fatalf("reaction on the root trigger message should stop the session")
	}
	if svc.Tracked("thread-2") {
		t.Fatalf("session still tracked after stop")
	}
}

// A stop reaction on an unrelated message stops nothing.
func TestHeadless_StopByMessageNoMatch(t *testing.T) {
	d := &fakeDriver{turns: []scripted{{texts: []string{"ok"}, ref: "s"}}}
	svc, _ := newHeadlessService(d)
	svc.startHeadless(context.Background(), "claude", "thread-2", "/wt", "", "sess-2", "", RoleOwner, nil, "owner", turnReq{channelID: "c", messageID: "m2", text: "go"})
	svc.waitIdle("thread-2")
	if svc.StopByMessage(context.Background(), "elsewhere", "nope", Caller{Role: RoleOwner, UserID: "owner"}) {
		t.Fatalf("unrelated reaction should not stop any session")
	}
	if !svc.Tracked("thread-2") {
		t.Fatalf("session should still be tracked")
	}
}

func TestHeadless_StopMarksRootMessageStopped(t *testing.T) {
	d := &fakeDriver{turns: []scripted{{texts: []string{"ok"}, ref: "s"}}}
	svc, r := newHeadlessService(d)
	svc.startHeadless(context.Background(), "claude", "thread-2", "/wt", "", "sess-2", "", RoleOwner, nil, "owner", turnReq{channelID: "c", messageID: "m2", text: "go"})
	svc.waitIdle("thread-2")

	if !svc.StopThread(context.Background(), "thread-2", Caller{Role: RoleOwner, UserID: "owner"}) {
		t.Fatalf("stop should report it ended a tracked session")
	}

	// The root (triggering) message must show the stopped marker so the channel
	// view makes clear the session is no longer running.
	if last := lastReactOn(r.reacts, "c|m2"); last != emojiStopped {
		t.Fatalf("root message should show stopped status %q, got %q (reacts=%v)", emojiStopped, last, r.reacts)
	}
	// The prior status emoji (✅ from the completed turn) is replaced, not stacked.
	if !hasStr(r.unreacts, "c|m2|"+emojiDone) {
		t.Errorf("stale done reaction not cleared from root message: %v", r.unreacts)
	}
}

func TestHeadless_ErrorKeepsSessionOpen(t *testing.T) {
	d := &fakeDriver{turns: []scripted{{err: errors.New("try again"), ref: "s"}}}
	svc, r := newHeadlessService(d)
	svc.startHeadless(context.Background(), "claude", "thread-3", "/wt", "", "sess-3", "", RoleOwner, nil, "owner", turnReq{channelID: "c", messageID: "m3", text: "go"})
	svc.waitIdle("thread-3")
	if !svc.Tracked("thread-3") {
		t.Fatalf("session should stay tracked after turn error")
	}
	if !anyContains(r.posts, "try again") {
		t.Fatalf("error not posted: %v", r.posts)
	}
	if !hasStr(r.reacts, "c|m3|"+emojiError) {
		t.Errorf("expected error reaction on the trigger message, got %v", r.reacts)
	}
}

func TestHeadless_SegmentPosting(t *testing.T) {
	// fakeDriver emits all tools, then all texts.
	d := &fakeDriver{turns: []scripted{{
		tools: []string{"WebFetch", "Grep"},
		texts: []string{"First segment.", "Second segment."},
		ref:   "s",
	}}}
	svc, r := newHeadlessService(d)

	svc.startHeadless(context.Background(), "claude", "t", "/wt", "", "n", "",
		RoleOwner, nil, "owner", turnReq{channelID: "c", messageID: "m", text: "go"})
	svc.waitIdle("t")

	// Live tool message + reposted summary + each text segment as its own message.
	if len(r.posts) != 4 {
		t.Fatalf("want 4 posts (tool message + summary + 2 segments), got %d: %v", len(r.posts), r.posts)
	}
	if r.posts[2].content != "First segment." || r.posts[3].content != "Second segment." {
		t.Errorf("segments = %q, %q", r.posts[2].content, r.posts[3].content)
	}
	// The live tool message (posts[0], id "msg-1") is finalized by DELETING it and
	// reposting the summary (posts[1]) as a fresh, silent message — not by editing
	// it in place, which would wrap Discord's "(edited)" marker onto a second line.
	if !hasStr(r.deletes, "t|msg-1") {
		t.Errorf("live tool message should be deleted on finalize, got deletes=%v", r.deletes)
	}
	summary := r.posts[1]
	if !summary.silent {
		t.Errorf("summary should be posted silently, got silent=%v", summary.silent)
	}
	if strings.Contains(summary.content, "\n") {
		t.Errorf("tool summary must be a single line, got %q", summary.content)
	}
	if want := "-# 🔍 1 search · 🌐 1 web request"; summary.content != want {
		t.Errorf("tool summary = %q, want %q", summary.content, want)
	}
	if strings.Contains(summary.content, "Grep") || strings.Contains(summary.content, "WebFetch") {
		t.Errorf("finalized summary should not contain raw tool labels: %q", summary.content)
	}
}

func TestHeadless_ToolBurstFinalizedAsRepostedSummary(t *testing.T) {
	d := &fakeDriver{turns: []scripted{{
		tools: []string{"Read a", "Read b", "Read c"},
		texts: []string{"done"},
		ref:   "s",
	}}}
	svc, r := newHeadlessService(d)

	svc.startHeadless(context.Background(), "claude", "t", "/wt", "", "n", "",
		RoleOwner, nil, "owner", turnReq{channelID: "c", messageID: "m", text: "go"})
	svc.waitIdle("t")

	// Three Reads collapse to ONE live tool message (not a message per tool); on
	// finalize that message is deleted and the summary reposted. So three posts:
	// live tool message, reposted summary, then the answer.
	if len(r.posts) != 3 {
		t.Fatalf("want 3 posts (tool message + summary + answer), got %d: %v", len(r.posts), r.posts)
	}
	if r.posts[2].content != "done" {
		t.Errorf("answer post = %q, want %q", r.posts[2].content, "done")
	}
	// The live tool message and the reposted summary are both silent (no
	// notification); only the agent's answer notifies.
	if !r.posts[0].silent {
		t.Errorf("tool message should be posted silently, got silent=%v", r.posts[0].silent)
	}
	// The live tool line is muted subtext too, so it reads in the same quiet style
	// as the narration and the end-of-burst summary.
	if !strings.HasPrefix(r.posts[0].content, "-# ") {
		t.Errorf("live tool message should be subtext-styled (muted), got %q", r.posts[0].content)
	}
	if !r.posts[1].silent {
		t.Errorf("summary should be posted silently, got silent=%v", r.posts[1].silent)
	}
	if r.posts[2].silent {
		t.Errorf("answer should notify (not silent), got silent=%v", r.posts[2].silent)
	}
	// The burst is finalized by delete + repost, never edited into the summary: an
	// edited `-#` subtext line makes Discord render "(edited)" on a second line.
	if !hasStr(r.deletes, "t|msg-1") {
		t.Errorf("live tool message should be deleted on finalize, got deletes=%v", r.deletes)
	}
	if len(r.edits) != 0 {
		t.Errorf("summary should be reposted, not produced by an edit, got edits=%v", r.edits)
	}
	// The finalized summary is a single muted line counting the burst ("read 3
	// files") — not any raw "Read a/b/c" line, and not an accumulated list.
	summary := r.posts[1].content
	if strings.Contains(summary, "\n") {
		t.Errorf("tool summary must be a single line, got %q", summary)
	}
	if want := "-# 📖 read 3 files"; summary != want {
		t.Errorf("tool summary = %q, want %q", summary, want)
	}
	for _, gone := range []string{"Read a", "Read b", "Read c"} {
		if strings.Contains(summary, gone) {
			t.Errorf("finalized summary should not contain raw tool label %q: %q", gone, summary)
		}
	}
}

func TestHeadless_NarrationMutedAnswerNotifies(t *testing.T) {
	// Interleaved within one turn: narration → tool → more narration → tool →
	// answer. Each text run that is immediately followed by tool activity is the
	// agent saying what it's about to do — muted (silent + subtext). Only the
	// trailing run, with no tool after it, is the answer and notifies.
	d := &fakeDriver{turns: []scripted{{
		events: []agentproc.Event{
			agentproc.AssistantText{Text: "I'll use the brainstorming skill."},
			agentproc.ToolActivity{Label: "Skill superpowers:brainstorming"},
			agentproc.AssistantText{Text: "Let me explore the command-parsing code."},
			agentproc.ToolActivity{Label: "Read directive.go"},
			agentproc.ToolActivity{Label: "Grep parse"},
			agentproc.AssistantText{Text: "Here's what I found in the parser."},
		},
		ref: "s",
	}}}
	svc, r := newHeadlessService(d)

	svc.startHeadless(context.Background(), "claude", "t", "/wt", "", "n", "",
		RoleOwner, nil, "owner", turnReq{channelID: "c", messageID: "m", text: "go"})
	svc.waitIdle("t")

	find := func(sub string) *postedMsg {
		for i := range r.posts {
			if strings.Contains(r.posts[i].content, sub) {
				return &r.posts[i]
			}
		}
		return nil
	}

	// Both narration runs are muted: silent (no notification) and subtext-styled.
	for _, narr := range []string{"I'll use the brainstorming skill.", "Let me explore the command-parsing code."} {
		p := find(narr)
		if p == nil {
			t.Fatalf("narration %q not posted: %v", narr, r.posts)
		}
		if !p.silent {
			t.Errorf("narration %q should be silent (no notification), got silent=%v", narr, p.silent)
		}
		if !strings.HasPrefix(p.content, "-# ") {
			t.Errorf("narration %q should be subtext-styled, got %q", narr, p.content)
		}
	}

	// The trailing run is the real answer: posted normally so it notifies, and not
	// muted to subtext.
	ans := find("Here's what I found in the parser.")
	if ans == nil {
		t.Fatalf("answer not posted: %v", r.posts)
	}
	if ans.silent {
		t.Errorf("answer should notify (not silent), got silent=%v", ans.silent)
	}
	if strings.HasPrefix(ans.content, "-#") {
		t.Errorf("answer should not be subtext-styled, got %q", ans.content)
	}
}

func TestHeadless_ThreadTitleStatusIcon(t *testing.T) {
	d := &fakeDriver{turns: []scripted{{texts: []string{"done"}, ref: "s"}}}
	svc, r := newHeadlessService(d)

	svc.startHeadless(context.Background(), "claude", "thread-1", "/wt", "", "demo", "acme/widget",
		RoleOwner, nil, "owner", turnReq{channelID: "c", messageID: "m1", text: "go"})
	svc.waitIdle("thread-1")

	ls := svc.sessions["thread-1"]
	svc.StopThread(context.Background(), "thread-1", Caller{Role: RoleOwner, UserID: "owner"}) // close() stops the title updater
	<-ls.title.done                                                                            // wait for its final drain

	if len(r.renames) == 0 {
		t.Fatalf("expected at least one thread rename for the status icon")
	}
	// Title carries both the status icon and the repo/dir label: "<icon> <label> <name>".
	last := r.renames[len(r.renames)-1]
	if want := "thread-1|" + emojiDone + " acme/widget demo"; last != want {
		t.Fatalf("final title = %q, want %q (renames=%v)", last, want, r.renames)
	}
}

func TestHeadless_GlobalStatusTracksLatestTurn(t *testing.T) {
	d := &fakeDriver{turns: []scripted{
		{texts: []string{"one"}, ref: "s"},
		{err: errors.New("boom"), ref: "s"},
	}}
	svc, r := newHeadlessService(d)

	svc.startHeadless(context.Background(), "claude", "thread-1", "/wt", "", "demo", "",
		RoleOwner, nil, "owner", turnReq{channelID: "c", messageID: "m1", text: "go"})
	svc.waitIdle("thread-1")

	if !svc.FeedThread(context.Background(), "thread-1", "thread-1", "m2", "again", nil, Caller{Role: RoleOwner, UserID: "owner"}) {
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

// canModify keys on the session's sandbox state, not the caller's role:
// a sandboxed session is shared (any authorized user may act), an unsandboxed
// session is private to its creator (even another owner is turned away).
func TestCanModify(t *testing.T) {
	creator := Caller{Role: RoleGuest, UserID: "alice"}
	otherGuest := Caller{Role: RoleGuest, UserID: "bob"}
	otherOwner := Caller{Role: RoleOwner, UserID: "carol"}

	sandboxed := &liveSession{authorID: "alice", sandbox: &SandboxHandle{}}
	for _, c := range []Caller{creator, otherGuest, otherOwner} {
		if !sandboxed.canModify(c) {
			t.Errorf("sandboxed session: %+v should be allowed (multi-user)", c)
		}
	}

	unsandboxed := &liveSession{authorID: "alice", sandbox: nil}
	if !unsandboxed.canModify(creator) {
		t.Error("unsandboxed session: the creator should be allowed")
	}
	if unsandboxed.canModify(otherGuest) {
		t.Error("unsandboxed session: a non-creator guest must be rejected")
	}
	if unsandboxed.canModify(otherOwner) {
		t.Error("unsandboxed session: a non-creator owner must be rejected")
	}
	// A System caller (e.g. a thread-archive stop) bypasses the creator gate.
	if !unsandboxed.canModify(Caller{System: true}) {
		t.Error("unsandboxed session: a System caller must be allowed")
	}
}

func anyContains(ms []postedMsg, sub string) bool {
	for _, m := range ms {
		if strings.Contains(m.content, sub) {
			return true
		}
	}
	return false
}

func hasStr(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

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
