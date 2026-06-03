package command

import "testing"

func TestParse_Full(t *testing.T) {
	in := "dagger/dagger agent=claude effort=high name=fix-cache base=main\nLine one.\nLine two."
	d, err := Parse(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Target != "dagger/dagger" || d.Agent != "claude" || d.Effort != "high" || d.Name != "fix-cache" || d.Base != "main" {
		t.Errorf("flags = %+v", d)
	}
	if !d.Headless {
		t.Errorf("headless should default true")
	}
	if d.Prompt != "Line one.\nLine two." {
		t.Errorf("Prompt = %q", d.Prompt)
	}
}

func TestParse_DefaultsHeadlessTrue(t *testing.T) {
	d, err := Parse("dagger/dagger\nGo.")
	if err != nil {
		t.Fatal(err)
	}
	if !d.Headless {
		t.Errorf("headless should default true")
	}
	if d.Agent != "" {
		t.Errorf("agent should be empty (config default), got %q", d.Agent)
	}
}

func TestParse_BareKeywords(t *testing.T) {
	d, err := Parse("dagger/dagger codex no-headless\nGo.")
	if err != nil {
		t.Fatal(err)
	}
	if d.Agent != "codex" || d.Headless || d.Target != "dagger/dagger" {
		t.Errorf("got %+v", d)
	}

	// order-independent + bare claude/headless keywords
	d2, err := Parse("claude headless dagger/dagger\nGo.")
	if err != nil {
		t.Fatal(err)
	}
	if d2.Agent != "claude" || !d2.Headless || d2.Target != "dagger/dagger" {
		t.Errorf("got %+v", d2)
	}
}

func TestParse_NoWorktree(t *testing.T) {
	d, err := Parse("repo/x no-wt\nGo.")
	if err != nil {
		t.Fatal(err)
	}
	if !d.NoWorktree || d.Target != "repo/x" {
		t.Errorf("got %+v", d)
	}
	if d2, _ := Parse("repo/x\nGo."); d2.NoWorktree {
		t.Errorf("NoWorktree should default false")
	}
}

func TestParse_HeadlessForms(t *testing.T) {
	cases := map[string]bool{
		"r/x\nP":                true,
		"r/x headless=true\nP":  true,
		"r/x headless=false\nP": false,
		"r/x no-headless\nP":    false,
	}
	for in, want := range cases {
		d, err := Parse(in)
		if err != nil {
			t.Fatalf("Parse(%q): %v", in, err)
		}
		if d.Headless != want {
			t.Errorf("Parse(%q) headless=%v, want %v", in, d.Headless, want)
		}
	}
	if _, err := Parse("r/x headless=maybe\nP"); err == nil {
		t.Errorf("expected error for non-bool headless")
	}
}

func TestParse_NoTarget(t *testing.T) {
	// empty directive line — prompt is on the next line
	d, err := Parse("\nDo the thing.")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Target != "" || !d.Headless || d.Prompt != "Do the thing." {
		t.Errorf("got %+v", d)
	}

	// keywords/flags only, still no target
	d2, err := Parse("codex name=x\nGo.")
	if err != nil {
		t.Fatal(err)
	}
	if d2.Target != "" || d2.Agent != "codex" || d2.Name != "x" {
		t.Errorf("got %+v", d2)
	}
}

func TestParse_SingleLineIsPrompt(t *testing.T) {
	// A single-line mention (no newline) is the whole prompt — no directive
	// parsing — so quick questions run in the scratch workspace.
	d, err := Parse("how do I rebase onto upstream?")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Target != "" || !d.Headless || d.Prompt != "how do I rebase onto upstream?" {
		t.Errorf("got %+v", d)
	}

	// Tokens that would otherwise look like a directive are part of the prompt
	// when there's no line break.
	d2, err := Parse("dagger/dagger codex effort=high")
	if err != nil {
		t.Fatal(err)
	}
	if d2.Target != "" || d2.Agent != "" || d2.Effort != "" || d2.Prompt != "dagger/dagger codex effort=high" {
		t.Errorf("got %+v", d2)
	}
}

func TestParse_BlankLineSeparator(t *testing.T) {
	d, err := Parse("repo/x\n\nprompt body")
	if err != nil {
		t.Fatal(err)
	}
	if d.Prompt != "prompt body" {
		t.Errorf("Prompt = %q", d.Prompt)
	}
}

func TestParse_Errors(t *testing.T) {
	cases := map[string]string{
		"empty":               "",
		"directive no prompt": "repo/x agent=claude\n",
		"unknown flag":        "repo/x bogus=1\nprompt",
		"two targets":         "repo/x repo/y\nprompt",
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse(in); err == nil {
				t.Fatalf("expected error for %q", in)
			}
		})
	}
}
