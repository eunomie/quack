package agentproc

import (
	"os"
	"strings"
	"testing"
)

func TestParseClaudeStream(t *testing.T) {
	f, err := os.Open("testdata/claude-turn.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	var texts, tools []string
	done := parseClaudeStream(f, func(e Event) {
		switch ev := e.(type) {
		case AssistantText:
			texts = append(texts, ev.Text)
		case ToolActivity:
			tools = append(tools, ev.Label)
		}
	})

	if done.SessionRef != "sess-abc" {
		t.Errorf("SessionRef = %q", done.SessionRef)
	}
	if done.Err != nil {
		t.Errorf("Err = %v", done.Err)
	}
	if len(texts) != 1 || !strings.Contains(texts[0], "Hello") {
		t.Errorf("texts = %v", texts)
	}
	if len(tools) != 1 || !strings.Contains(tools[0], "Bash") {
		t.Errorf("tools = %v", tools)
	}
}

func TestClaudeArgs(t *testing.T) {
	d := Claude{Command: "claude", EffortTemplate: "--effort {effort}", NameTemplate: "-n {name}", PermissionMode: "auto", AllowedTools: "Read,Edit"}
	first := d.args(Turn{Prompt: "hi", Effort: "xhigh", Name: "dagger-main-7k2p"})
	if !contains(first, "-p") || !contains(first, "hi") || !contains(first, "--effort") || !contains(first, "xhigh") {
		t.Errorf("first args = %v", first)
	}
	if !contains(first, "-n") || !contains(first, "dagger-main-7k2p") {
		t.Errorf("first turn should set the display name: %v", first)
	}
	if contains(first, "--resume") {
		t.Errorf("first turn must not resume: %v", first)
	}
	next := d.args(Turn{Prompt: "again", SessionRef: "sess-abc", Effort: "xhigh", Name: "dagger-main-7k2p"})
	if !contains(next, "--resume") || !contains(next, "sess-abc") {
		t.Errorf("resume args = %v", next)
	}
	if contains(next, "--effort") || contains(next, "-n") {
		t.Errorf("effort/name must apply only on the first turn: %v", next)
	}
}

func TestClaudeArgs_Settings(t *testing.T) {
	d := Claude{Command: "claude", PermissionMode: "auto", Settings: `{"sandbox":{"enabled":true}}`}
	first := d.args(Turn{Prompt: "hi"})
	if !contains(first, "--settings") || !contains(first, `{"sandbox":{"enabled":true}}`) {
		t.Errorf("first turn missing --settings: %v", first)
	}
	if !contains(first, "auto") {
		t.Errorf("permission-mode auto not passed: %v", first)
	}
	// settings must apply on resume turns too (sandbox is per-process)
	next := d.args(Turn{Prompt: "again", SessionRef: "sess-1"})
	if !contains(next, "--settings") {
		t.Errorf("resume turn missing --settings: %v", next)
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
