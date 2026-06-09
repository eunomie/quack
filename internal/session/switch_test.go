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
