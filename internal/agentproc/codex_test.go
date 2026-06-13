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
	// The prompt is the last positional arg; codex has no system-prompt flag, so
	// the Discord-format nudge is prepended to it (hence substring, not equality).
	first := d.args(Turn{Prompt: "hi", Effort: "high"})
	if first[0] != "exec" || !contains(first, "--json") || !contains(first, "model_reasoning_effort=high") {
		t.Errorf("first = %v", first)
	}
	if p := first[len(first)-1]; !strings.Contains(p, "hi") || !strings.Contains(p, discordFormatNudge) {
		t.Errorf("first prompt missing task or nudge: %q", p)
	}
	next := d.args(Turn{Prompt: "again", SessionRef: "th-xyz", Effort: "high"})
	if next[0] != "exec" || next[1] != "resume" || !contains(next, "th-xyz") || !contains(next, "--json") {
		t.Errorf("resume = %v", next)
	}
	// The nudge rides on resume turns too, so it reaches sessions that predate it.
	if p := next[len(next)-1]; !strings.Contains(p, "again") || !strings.Contains(p, discordFormatNudge) {
		t.Errorf("resume prompt missing task or nudge: %q", p)
	}
	if contains(next, "model_reasoning_effort=high") {
		t.Errorf("effort must apply only on first turn: %v", next)
	}
}

func TestCodexSandboxArgs(t *testing.T) {
	d := Codex{Command: "codex", SandboxMode: "danger-full-access"}

	// First turn: --sandbox sits right after exec, before --json.
	first := d.args(Turn{Prompt: "hi"})
	if first[0] != "exec" || first[1] != "--sandbox" || first[2] != "danger-full-access" {
		t.Errorf("first = %v, want exec --sandbox danger-full-access ...", first)
	}

	// Resume: --sandbox must precede the resume subcommand so codex parses it.
	next := d.args(Turn{Prompt: "again", SessionRef: "th-xyz"})
	if next[0] != "exec" || next[1] != "--sandbox" || next[2] != "danger-full-access" || next[3] != "resume" {
		t.Errorf("resume = %v, want exec --sandbox danger-full-access resume ...", next)
	}

	// Empty SandboxMode adds no flag (codex's own default applies).
	bare := Codex{Command: "codex"}.args(Turn{Prompt: "hi"})
	if contains(bare, "--sandbox") {
		t.Errorf("empty SandboxMode must add no --sandbox flag: %v", bare)
	}
}
