package agentproc

import (
	"os"
	"strings"
	"testing"
)

func TestParseCodexStream(t *testing.T) {
	f, err := os.Open("testdata/codex-turn.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	var texts, tools []string
	done := parseCodexStream(f, func(e Event) {
		switch ev := e.(type) {
		case AssistantText:
			texts = append(texts, ev.Text)
		case ToolActivity:
			tools = append(tools, ev.Label)
		}
	})
	if done.SessionRef != "th-xyz" {
		t.Errorf("SessionRef = %q", done.SessionRef)
	}
	if len(texts) != 1 || !strings.Contains(texts[0], "Hello") {
		t.Errorf("texts = %v", texts)
	}
	if len(tools) != 1 || !strings.Contains(tools[0], "git status") {
		t.Errorf("tools = %v", tools)
	}
}

func TestCodexArgs(t *testing.T) {
	d := Codex{Command: "codex", EffortTemplate: "--config model_reasoning_effort={effort}"}
	first := d.args(Turn{Prompt: "hi", Effort: "high"})
	if first[0] != "exec" || !contains(first, "hi") || !contains(first, "--json") || !contains(first, "model_reasoning_effort=high") {
		t.Errorf("first = %v", first)
	}
	next := d.args(Turn{Prompt: "again", SessionRef: "th-xyz", Effort: "high"})
	if next[0] != "exec" || next[1] != "resume" || !contains(next, "th-xyz") || !contains(next, "again") || !contains(next, "--json") {
		t.Errorf("resume = %v", next)
	}
	if contains(next, "model_reasoning_effort=high") {
		t.Errorf("effort must apply only on first turn: %v", next)
	}
}
