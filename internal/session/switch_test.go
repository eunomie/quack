package session

import (
	"context"
	"strings"
	"testing"

	"github.com/eunomie/quack/internal/agent"
	"github.com/eunomie/quack/internal/agentproc"
)

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

func newSwitchTestService() *Service {
	svc := New(Config{StateDir: "/state"}, newFakeGit(), newFakeTmux(), newFakeReplier())
	svc.cfg.Agents = map[string]agent.Agent{
		"claude": {Command: "claude", Headless: true, Switchable: true},
		"codex":  {Command: "codex", Headless: true, Switchable: true},
		"infer":  {Command: "claude", Headless: true},   // not switchable
		"legacy": {Command: "legacy", Switchable: true}, // switchable but not headless
	}
	return svc
}

func TestMatchSwitch(t *testing.T) {
	svc := newSwitchTestService()
	cases := []struct {
		name       string
		text       string
		wantAgent  string
		wantPrompt string
		wantOK     bool
	}{
		{"bare trigger", "/codex", "codex", "", true},
		{"trigger with prompt", "/codex fix the failing test", "codex", "fix the failing test", true},
		{"prompt keeps internal spacing", "/claude  run   it", "claude", "run   it", true},
		{"trailing spaces trimmed", "/codex   ", "codex", "", true},
		{"leading spaces tolerated", "  /codex fix it", "codex", "fix it", true},
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
		{texts: []string{"working on it"}, ref: "claude-1"},   // first real turn
		{texts: []string{"HANDOFF SUMMARY"}, ref: "claude-2"}, // summary turn
	}}
	codex := &fakeDriver{turns: []scripted{{texts: []string{"continuing"}, ref: "codex-1"}}}
	svc, r, fs := newSwitchService(claude, codex)

	svc.startHeadless(context.Background(), "claude", "thread-1", "/wt", "high", "demo", "",
		RoleOwner, nil, "owner", turnReq{channelID: "c", messageID: "m1", text: "go"})
	svc.waitIdle("thread-1")

	ls := svc.sessions["thread-1"]
	svc.doSwitch(context.Background(), ls, "codex", "", "c", "m2", nil, Caller{Role: RoleOwner, UserID: "owner"})

	if len(claude.seen) != 2 || claude.seen[1].SessionRef != "claude-1" || claude.seen[1].Prompt != handoffPrompt {
		t.Fatalf("claude turns = %+v, want a summary turn resuming claude-1", claude.seen)
	}
	newls := svc.sessions["thread-1"]
	if newls.agentName != "codex" {
		t.Fatalf("agentName = %q, want codex", newls.agentName)
	}
	if newls.ref() != "" {
		t.Errorf("SessionRef = %q, want empty (fresh native session)", newls.ref())
	}
	if h := newls.record().PendingHandoff; !strings.Contains(h, "HANDOFF SUMMARY") {
		t.Errorf("pendingHandoff = %q, want the summary wrapped", h)
	}
	if len(codex.seen) != 0 {
		t.Errorf("codex ran %d turns, want 0 (lazy seed)", len(codex.seen))
	}
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
	svc.doSwitch(context.Background(), ls, "codex", "fix the test", "c", "m2", nil, Caller{Role: RoleOwner, UserID: "owner"})
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
	svc.doSwitch(context.Background(), ls, "claude", "keep going", "c", "m2", nil, Caller{Role: RoleOwner, UserID: "owner"})
	svc.waitIdle("thread-1")

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

	svc.doSwitch(context.Background(), ls, "codex", "", "c", "m2", nil, Caller{Role: RoleOwner, UserID: "owner"})

	if len(claude.seen) != 0 {
		t.Errorf("claude ran %d summary turns, want 0 (no ref to resume)", len(claude.seen))
	}
	if svc.sessions["thread-1"].agentName != "codex" {
		t.Errorf("switch did not complete to codex")
	}
	if h := svc.sessions["thread-1"].record().PendingHandoff; h != "" {
		t.Errorf("pendingHandoff = %q, want empty (no summary)", h)
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

	svc.doSwitch(context.Background(), ls, "ghost", "", "c", "m2", nil, Caller{Role: RoleOwner, UserID: "owner"})

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
	if svc.SwitchAgent(context.Background(), "thread-1", "c", "m1", "just a message", nil, Caller{Role: RoleOwner, UserID: "owner"}) {
		t.Errorf("non-trigger text must return false so the bot falls through to FeedThread")
	}
}

// A second switch while one is already in flight must be dropped, not race the
// first into an orphaned rebuild. With the claim already set, SwitchAgent reports
// handled but starts no new switch.
func TestSwitchAgent_DropsSecondConcurrentSwitch(t *testing.T) {
	claude := &fakeDriver{turns: []scripted{{texts: []string{"working"}, ref: "claude-1"}}}
	codex := &fakeDriver{}
	svc, _, _ := newSwitchService(claude, codex)
	svc.startHeadless(context.Background(), "claude", "thread-1", "/wt", "high", "demo", "",
		RoleOwner, nil, "owner", turnReq{channelID: "c", messageID: "m1", text: "go"})
	svc.waitIdle("thread-1")

	// Simulate a switch already claimed/in-flight on this session.
	ls := svc.sessions["thread-1"]
	svc.hmu.Lock()
	ls.switching = true
	svc.hmu.Unlock()

	if !svc.SwitchAgent(context.Background(), "thread-1", "c", "m2", "/codex", nil, Caller{Role: RoleOwner, UserID: "owner"}) {
		t.Fatalf("a switch trigger must report handled")
	}
	// No new switch started: still claude, and no summary turn ran.
	if svc.sessions["thread-1"].agentName != "claude" {
		t.Errorf("second concurrent switch must be dropped, agent = %q", svc.sessions["thread-1"].agentName)
	}
	if len(claude.seen) != 1 {
		t.Errorf("dropped switch must not run a summary turn, claude turns = %d", len(claude.seen))
	}
}
