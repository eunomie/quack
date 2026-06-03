package session

import (
	"strings"
	"testing"
)

func TestSplitMessage(t *testing.T) {
	if got := splitMessage("short", 2000); len(got) != 1 || got[0] != "short" {
		t.Fatalf("got %v", got)
	}
	big := strings.Repeat("a", 4500)
	parts := splitMessage(big, 2000)
	if len(parts) != 3 {
		t.Fatalf("want 3 parts, got %d", len(parts))
	}
	for _, p := range parts {
		if len(p) > 2000 {
			t.Errorf("part too long: %d", len(p))
		}
	}
	nl := strings.Repeat("x", 1990) + "\n" + strings.Repeat("y", 100)
	if p := splitMessage(nl, 2000); !strings.HasSuffix(p[0], "x") {
		t.Errorf("did not break on newline: %q", p[0][len(p[0])-5:])
	}
}

func TestEmptyAnswerPlaceholder(t *testing.T) {
	if got := answerOrPlaceholder(""); got == "" {
		t.Errorf("empty answer should produce a placeholder")
	}
}

func TestMutedText(t *testing.T) {
	// Single line gets the subtext prefix.
	if got := mutedText("Let me explore the code."); got != "-# Let me explore the code." {
		t.Errorf("mutedText single line = %q", got)
	}
	// Every non-blank line is prefixed; blank lines (paragraph gaps) are kept as-is.
	got := mutedText("First line.\n\nSecond line.")
	if want := "-# First line.\n\n-# Second line."; got != want {
		t.Errorf("mutedText multi-line = %q, want %q", got, want)
	}
}

func TestToolEmoji(t *testing.T) {
	cases := map[string]string{
		"WebFetch":         "🌐",
		"ToolSearch":       "🔍",
		"Grep":             "🔍",
		"Read":             "📖",
		"Edit":             "✏️",
		"Bash git status":  "▶️",
		"SomethingUnknown": "🔧",
	}
	for label, want := range cases {
		if got := toolEmoji(label); got != want {
			t.Errorf("toolEmoji(%q) = %q, want %q", label, got, want)
		}
	}
	if got := toolLine("WebFetch"); got != "🌐 WebFetch" {
		t.Errorf("toolLine = %q", got)
	}
}

func TestToolTallySummary(t *testing.T) {
	// Empty tally renders nothing.
	var empty toolTally
	if got := empty.summary(); got != "" {
		t.Errorf("empty tally summary = %q, want empty", got)
	}

	cases := []struct {
		name   string
		labels []string
		want   string
	}{
		{
			name:   "mixed categories in canonical order",
			labels: []string{"Bash cat x", "Read a", "Read b", "Grep foo"},
			want:   "-# 📖 read 2 files · ▶️ ran 1 command · 🔍 1 search",
		},
		{
			name:   "singular vs plural nouns",
			labels: []string{"Read a"},
			want:   "-# 📖 read 1 file",
		},
		{
			name:   "search pluralizes irregularly",
			labels: []string{"Grep a", "Glob b"},
			want:   "-# 🔍 2 searches",
		},
		{
			name:   "todos are not counted",
			labels: []string{"TodoWrite", "TodoWrite"},
			want:   "-# 📋 updated todos",
		},
		{
			name:   "skills",
			labels: []string{"Skill superpowers:brainstorming", "Skill stg"},
			want:   "-# 🧩 used 2 skills",
		},
		{
			name:   "unknown tools fall back",
			labels: []string{"Frobnicate x", "Edit f"},
			want:   "-# ✏️ edited 1 file · 🔧 1 tool call",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var tally toolTally
			for _, l := range tc.labels {
				tally.add(l)
			}
			if got := tally.summary(); got != tc.want {
				t.Errorf("summary() = %q, want %q", got, tc.want)
			}
		})
	}
}
