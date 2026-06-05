package agentproc

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// nextTurn drains events until a TurnComplete, returning the concatenated
// assistant text and the completion.
func nextTurn(t *testing.T, sess Session) (string, TurnComplete) {
	t.Helper()
	var b strings.Builder
	timeout := time.After(60 * time.Second)
	for {
		select {
		case ev, ok := <-sess.Events():
			if !ok {
				t.Fatalf("event stream closed before TurnComplete")
			}
			switch e := ev.(type) {
			case AssistantText:
				b.WriteString(e.Text)
			case TurnComplete:
				return b.String(), e
			}
		case <-timeout:
			t.Fatalf("timed out waiting for a turn")
		}
	}
}

// TestClaudeStreamSession_Integration drives the real claude CLI as a persistent
// streaming session: two sequential turns in one process, with context carried
// across them (it must recall the first answer).
func TestClaudeStreamSession_Integration(t *testing.T) {
	if os.Getenv("QUACK_INTEGRATION") == "" {
		t.Skip("set QUACK_INTEGRATION=1 to run (needs an authenticated claude CLI)")
	}
	d := Claude{Command: "claude", PermissionMode: "acceptEdits", Model: "claude-haiku-4-5-20251001"}
	sess, err := d.OpenSession(context.Background(), OpenOpts{Workdir: t.TempDir()})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer sess.Close()

	if err := sess.Send("Reply with exactly the word PINEAPPLE and nothing else."); err != nil {
		t.Fatalf("send 1: %v", err)
	}
	a1, c1 := nextTurn(t, sess)
	if c1.Err != nil || c1.Interrupted {
		t.Fatalf("turn 1 completion = %+v", c1)
	}
	if !strings.Contains(strings.ToUpper(a1), "PINEAPPLE") {
		t.Fatalf("turn 1 answer = %q", a1)
	}
	if sess.SessionRef() == "" {
		t.Fatalf("no session ref captured")
	}

	if err := sess.Send("What word did you just say? Reply with only that word."); err != nil {
		t.Fatalf("send 2: %v", err)
	}
	a2, c2 := nextTurn(t, sess)
	if c2.Err != nil {
		t.Fatalf("turn 2 completion = %+v", c2)
	}
	if !strings.Contains(strings.ToUpper(a2), "PINEAPPLE") {
		t.Fatalf("turn 2 should recall context across the streaming session, got %q", a2)
	}
}
