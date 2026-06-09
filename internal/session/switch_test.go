package session

import (
	"context"
	"testing"

	"github.com/eunomie/quack/internal/agent"
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
