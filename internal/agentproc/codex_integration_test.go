package agentproc

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestCodexRunTurn_Integration(t *testing.T) {
	if os.Getenv("QUACK_INTEGRATION") == "" {
		t.Skip("set QUACK_INTEGRATION=1 to run (needs an authenticated codex CLI)")
	}
	if _, err := exec.LookPath("codex"); err != nil {
		t.Skip("codex not installed")
	}
	dir := t.TempDir()
	run(t, dir, "git", "init", "-q", "-b", "main", ".")
	run(t, dir, "git", "commit", "-q", "--allow-empty", "-m", "init")

	d := Codex{Command: "codex"}
	var ans1 strings.Builder
	done1 := d.RunTurn(context.Background(), Turn{
		Prompt:  "Reply with exactly the word PONG and nothing else.",
		Workdir: dir,
	}, func(e Event) {
		if a, ok := e.(AssistantText); ok {
			ans1.WriteString(a.Text)
		}
	})
	if done1.Err != nil || done1.SessionRef == "" {
		t.Fatalf("turn1: err=%v ref=%q", done1.Err, done1.SessionRef)
	}
	if !strings.Contains(strings.ToUpper(ans1.String()), "PONG") {
		t.Fatalf("turn1 answer = %q", ans1.String())
	}

	// resume must continue the SAME thread (exercises `exec resume --json <id> <prompt>`)
	done2 := d.RunTurn(context.Background(), Turn{
		SessionRef: done1.SessionRef,
		Prompt:     "What single word did you just say? Reply with only that word.",
		Workdir:    dir,
	}, func(e Event) {})
	if done2.Err != nil {
		t.Fatalf("turn2 err: %v", done2.Err)
	}
	if done2.SessionRef != done1.SessionRef {
		t.Fatalf("thread ref changed on resume: %q -> %q", done1.SessionRef, done2.SessionRef)
	}
}
