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
		turnReq{channelID: "c", messageID: "m1", text: "go"})
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
		turnReq{channelID: "c", messageID: "m1", text: "go"})
	svc.waitIdle("thread-1")
	if _, ok := readRecord(t, fs, "demo"); !ok {
		t.Fatalf("record should exist before stop")
	}

	svc.StopThread(context.Background(), "thread-1")
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
		turnReq{channelID: "c", messageID: "m1", text: "go"})
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

	if !svc.FeedThread(context.Background(), "thread-x", "thread-x", "m9", "continue", nil) {
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
