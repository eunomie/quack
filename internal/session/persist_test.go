package session

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/eunomie/quack/internal/agent"
)

// seedRecord writes a persisted session record into the fake FS, as a previous
// quack run would have, so Rehydrate can pick it up.
func seedRecord(fs *memFS, rec sessionRecord) {
	data, _ := json.MarshalIndent(rec, "", "  ")
	fs.files[filepath.Join("/state", "sessions", rec.Name, "session.json")] = data
}

func readRecord(t *testing.T, fs *memFS, name string) (sessionRecord, bool) {
	t.Helper()
	data, ok := fs.files[filepath.Join("/state", "sessions", name, "session.json")]
	if !ok {
		return sessionRecord{}, false
	}
	var rec sessionRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatalf("unmarshal record: %v", err)
	}
	return rec, true
}

func TestHeadless_PersistsRecordWithSessionRef(t *testing.T) {
	d := &fakeDriver{turns: []scripted{{texts: []string{"ok"}, ref: "sess-ref-1"}}}
	svc, _, _, fs := newHeadlessServiceFakes(d)

	svc.startHeadless(context.Background(), "claude", "thread-1", "/wt", "high", "demo", "acme/widget",
		RoleOwner, nil, "owner", turnReq{channelID: "c", messageID: "m1", text: "go"})
	svc.waitIdle("thread-1")

	rec, ok := readRecord(t, fs, "demo")
	if !ok {
		t.Fatalf("no session record persisted; files=%v", keys(fs))
	}
	if rec.SessionRef != "sess-ref-1" {
		t.Errorf("SessionRef = %q, want the turn's ref %q", rec.SessionRef, "sess-ref-1")
	}
	if rec.ThreadID != "thread-1" || rec.Workdir != "/wt" || rec.AgentName != "claude" {
		t.Errorf("record = %+v, missing thread/workdir/agent", rec)
	}
	// The workspace label must persist so a restored thread keeps its title prefix.
	if rec.Label != "acme/widget" {
		t.Errorf("Label = %q, want %q", rec.Label, "acme/widget")
	}
	if rec.RootChannelID != "c" || rec.RootMessageID != "m1" {
		t.Errorf("record = %+v, missing root channel/message", rec)
	}
}

func TestHeadless_StopRemovesRecord(t *testing.T) {
	d := &fakeDriver{turns: []scripted{{texts: []string{"ok"}, ref: "s"}}}
	svc, _, _, fs := newHeadlessServiceFakes(d)

	svc.startHeadless(context.Background(), "claude", "thread-1", "/wt", "", "demo", "",
		RoleOwner, nil, "owner", turnReq{channelID: "c", messageID: "m1", text: "go"})
	svc.waitIdle("thread-1")
	if _, ok := readRecord(t, fs, "demo"); !ok {
		t.Fatalf("record should exist before stop")
	}

	svc.StopThread(context.Background(), "thread-1", Caller{Role: RoleOwner})
	if _, ok := readRecord(t, fs, "demo"); ok {
		t.Errorf("record should be removed after /stop")
	}
}

func TestHeadless_PromoteRemovesRecord(t *testing.T) {
	d := &fakeDriver{turns: []scripted{{texts: []string{"ok"}, ref: "live-ref"}}}
	svc, _, _, fs := newHeadlessServiceFakes(d)
	svc.cfg.Agents = map[string]agent.Agent{
		"claude": {Command: "claude", ResumeTemplate: "--resume {session}"},
	}

	svc.startHeadless(context.Background(), "claude", "thread-1", "/wt", "", "demo", "",
		RoleOwner, nil, "owner", turnReq{channelID: "c", messageID: "m1", text: "go"})
	svc.waitIdle("thread-1")
	if _, ok := readRecord(t, fs, "demo"); !ok {
		t.Fatalf("record should exist before promote")
	}

	svc.PromoteThread(context.Background(), "thread-1")
	if _, ok := readRecord(t, fs, "demo"); ok {
		t.Errorf("record should be removed after /attach (promote to tmux)")
	}
}

func TestHeadless_RehydrateRestoresAndResumes(t *testing.T) {
	// Driver is scripted only for the resume turn — rehydration must NOT replay
	// any past turn, only restore the mapping and resume on the next message.
	d := &fakeDriver{turns: []scripted{{texts: []string{"resumed answer"}, ref: "sess-2"}}}
	svc, g, r, fs := newHeadlessServiceFakes(d)
	g.pathExists["/wt"] = true
	seedRecord(fs, sessionRecord{
		Name: "demo", Label: "eunomie/quack", AgentName: "claude", Workdir: "/wt", Effort: "high",
		ThreadID: "thread-x", RootChannelID: "c", RootMessageID: "m1", SessionRef: "sess-1",
	})

	if n := svc.Rehydrate(context.Background()); n != 1 {
		t.Fatalf("Rehydrate restored %d sessions, want 1", n)
	}
	if !svc.Tracked("thread-x") {
		t.Fatalf("thread should be tracked after rehydration")
	}
	// The restored session keeps its workspace label so the title prefix survives.
	if got := svc.sessions["thread-x"].label; got != "eunomie/quack" {
		t.Errorf("restored session label = %q, want %q", got, "eunomie/quack")
	}

	if !svc.FeedThread(context.Background(), "thread-x", "thread-x", "m9", "continue", nil, Caller{Role: RoleOwner}) {
		t.Fatalf("feed should report the rehydrated thread as tracked")
	}
	svc.waitIdle("thread-x")

	if len(d.seen) != 1 {
		t.Fatalf("driver saw %d turns, want 1 (resume only)", len(d.seen))
	}
	if d.seen[0].SessionRef != "sess-1" || d.seen[0].Prompt != "continue" || d.seen[0].Workdir != "/wt" {
		t.Errorf("resume turn = %+v, want SessionRef sess-1 / Prompt continue / Workdir /wt", d.seen[0])
	}
	if !anyContains(r.posts, "resumed answer") {
		t.Errorf("resumed answer not posted: %v", r.posts)
	}
	// The rotated ref from the resumed turn must be persisted back.
	if rec, _ := readRecord(t, fs, "demo"); rec.SessionRef != "sess-2" {
		t.Errorf("persisted SessionRef = %q after resume, want sess-2", rec.SessionRef)
	}
}

func TestHeadless_RehydratePostsBackGreeting(t *testing.T) {
	d := &fakeDriver{}
	svc, g, r, fs := newHeadlessServiceFakes(d)
	g.pathExists["/wt"] = true
	seedRecord(fs, sessionRecord{
		Name: "demo", Label: "eunomie/quack", AgentName: "claude", Workdir: "/wt",
		ThreadID: "thread-x", RootChannelID: "c", RootMessageID: "m1", SessionRef: "sess-1",
	})

	if n := svc.Rehydrate(context.Background()); n != 1 {
		t.Fatalf("Rehydrate restored %d sessions, want 1", n)
	}

	// A greeting from backMessages must land in the thread, notifying (not
	// silent) so the user sees the session came back alive.
	known := make(map[string]bool, len(backMessages))
	for _, m := range backMessages {
		known[m] = true
	}
	var greeted bool
	for _, p := range r.posts {
		if p.channel == "thread-x" && !p.silent && known[p.content] {
			greeted = true
		}
	}
	if !greeted {
		t.Errorf("no back greeting posted to thread on rehydrate: %v", r.posts)
	}
}

func TestHeadless_RehydrateSkipsMissingWorkdir(t *testing.T) {
	d := &fakeDriver{}
	svc, _, _, fs := newHeadlessServiceFakes(d)
	// Workdir /gone is never registered in the fake git, so it "doesn't exist".
	seedRecord(fs, sessionRecord{
		Name: "dead", AgentName: "claude", Workdir: "/gone",
		ThreadID: "thread-dead", SessionRef: "sess-1",
	})

	if n := svc.Rehydrate(context.Background()); n != 0 {
		t.Fatalf("Rehydrate restored %d, want 0 for a missing worktree", n)
	}
	if svc.Tracked("thread-dead") {
		t.Errorf("session with a missing worktree should not be tracked")
	}
}

func TestHeadless_RehydrateSkipsUnknownAgent(t *testing.T) {
	d := &fakeDriver{}
	svc, g, _, fs := newHeadlessServiceFakes(d)
	g.pathExists["/wt"] = true
	seedRecord(fs, sessionRecord{
		Name: "ghost", AgentName: "no-such-agent", Workdir: "/wt",
		ThreadID: "thread-ghost", SessionRef: "sess-1",
	})

	if n := svc.Rehydrate(context.Background()); n != 0 {
		t.Fatalf("Rehydrate restored %d, want 0 for an unconfigured agent", n)
	}
	if svc.Tracked("thread-ghost") {
		t.Errorf("session for an unknown agent should not be tracked")
	}
}

func keys(fs *memFS) []string {
	out := make([]string, 0, len(fs.files))
	for k := range fs.files {
		out = append(out, k)
	}
	return out
}

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
		RoleOwner, nil, "owner", turnReq{channelID: "post1", messageID: "m1", text: "go"},
		inPlaceOpts{inPlace: true, titleBase: "Help with login"})
	svc.waitIdle("post1")

	rec, ok := readRecord(t, fs, "demo")
	if !ok {
		t.Fatalf("no record persisted; files=%v", keys(fs))
	}
	if rec.TitleBase != "Help with login" || !rec.InPlace {
		t.Errorf("record TitleBase=%q InPlace=%v, want \"Help with login\"/true", rec.TitleBase, rec.InPlace)
	}

	svc.StopThread(context.Background(), "post1", Caller{Role: RoleOwner})
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

	svc.StopThread(context.Background(), "post1", Caller{Role: RoleOwner})
	for _, id := range r.archived {
		if id == "post1" {
			t.Errorf("rehydrated in-place thread must not be archived on stop; archived=%v", r.archived)
		}
	}
}
