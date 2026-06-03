package session

import (
	"fmt"
	"strings"
)

const discordMax = 2000

func splitMessage(s string, max int) []string {
	if max <= 0 {
		max = discordMax
	}
	var out []string
	for len(s) > max {
		cut := max
		if nl := strings.LastIndexByte(s[:max], '\n'); nl > max/2 {
			cut = nl
		}
		out = append(out, strings.TrimRight(s[:cut], "\n"))
		s = strings.TrimLeft(s[cut:], "\n")
	}
	if s != "" {
		out = append(out, s)
	}
	if len(out) == 0 {
		return []string{""}
	}
	return out
}

func answerOrPlaceholder(s string) string {
	if strings.TrimSpace(s) == "" {
		return "_(no text response this turn)_"
	}
	return s
}

// mutedText renders assistant narration (text the agent emits right before it
// goes off to use tools, e.g. "Let me explore the code…") as Discord subtext
// ("-#") so it reads as a quiet aside like the tool summaries instead of an
// answer worth a notification. "-#" applies per line, so every non-blank line is
// prefixed; blank lines are left intact to preserve paragraph spacing.
func mutedText(s string) string {
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		if strings.TrimSpace(ln) != "" {
			lines[i] = "-# " + ln
		}
	}
	return strings.Join(lines, "\n")
}

// toolLine renders a tool-activity label with a leading emoji for readability.
func toolLine(label string) string {
	return toolEmoji(label) + " " + label
}

// toolCategory buckets a tool for both its emoji and its end-of-burst count.
type toolCategory int

const (
	catRead toolCategory = iota
	catEdit
	catRun
	catSearch
	catWeb
	catAgent
	catSkill
	catTodo
	catOther
	numToolCategories
)

// categorize maps a tool label (its first word) to a category.
func categorize(label string) toolCategory {
	name := label
	if i := strings.IndexByte(label, ' '); i > 0 {
		name = label[:i]
	}
	switch strings.ToLower(name) {
	case "bash", "shell":
		return catRun
	case "read", "notebookread":
		return catRead
	case "edit", "write", "multiedit", "notebookedit":
		return catEdit
	case "glob", "grep", "search", "toolsearch":
		return catSearch
	case "webfetch", "websearch":
		return catWeb
	case "task", "agent":
		return catAgent
	case "skill":
		return catSkill
	case "todowrite":
		return catTodo
	default:
		return catOther
	}
}

// toolEmoji maps a tool label to its category emoji.
func toolEmoji(label string) string {
	switch categorize(label) {
	case catRun:
		return "▶️"
	case catRead:
		return "📖"
	case catEdit:
		return "✏️"
	case catSearch:
		return "🔍"
	case catWeb:
		return "🌐"
	case catAgent:
		return "🤖"
	case catSkill:
		return "🧩"
	case catTodo:
		return "📋"
	default:
		return "🔧"
	}
}

// toolTally counts a turn's tool use by category so a finished burst collapses to
// one muted summary line instead of leaving a leftover raw command.
type toolTally struct {
	counts [numToolCategories]int
	total  int
}

func (t *toolTally) add(label string) {
	t.counts[categorize(label)]++
	t.total++
}

// summary renders the tally as a single muted (-#) subtext line, e.g.
// "-# 📖 read 2 files · ▶️ ran 3 commands · 🔍 1 search". Returns "" when empty.
func (t *toolTally) summary() string {
	if t.total == 0 {
		return ""
	}
	var parts []string
	if n := t.counts[catRead]; n > 0 {
		parts = append(parts, "📖 read "+plural(n, "file", "files"))
	}
	if n := t.counts[catEdit]; n > 0 {
		parts = append(parts, "✏️ edited "+plural(n, "file", "files"))
	}
	if n := t.counts[catRun]; n > 0 {
		parts = append(parts, "▶️ ran "+plural(n, "command", "commands"))
	}
	if n := t.counts[catSearch]; n > 0 {
		parts = append(parts, "🔍 "+plural(n, "search", "searches"))
	}
	if n := t.counts[catWeb]; n > 0 {
		parts = append(parts, "🌐 "+plural(n, "web request", "web requests"))
	}
	if n := t.counts[catAgent]; n > 0 {
		parts = append(parts, "🤖 "+plural(n, "subagent", "subagents"))
	}
	if n := t.counts[catSkill]; n > 0 {
		parts = append(parts, "🧩 used "+plural(n, "skill", "skills"))
	}
	if t.counts[catTodo] > 0 {
		parts = append(parts, "📋 updated todos")
	}
	if n := t.counts[catOther]; n > 0 {
		parts = append(parts, "🔧 "+plural(n, "tool call", "tool calls"))
	}
	return "-# " + strings.Join(parts, " · ")
}

// plural formats a count with its singular or plural noun.
func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}
