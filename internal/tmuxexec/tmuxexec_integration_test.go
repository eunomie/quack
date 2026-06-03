package tmuxexec

import (
	"context"
	"os"
	"os/exec"
	"testing"

	"github.com/eunomie/quack/internal/session"
)

func TestNewSession_Integration(t *testing.T) {
	if os.Getenv("QUACK_INTEGRATION") == "" {
		t.Skip("set QUACK_INTEGRATION=1 to run (needs tmux)")
	}
	sock := "quacktest"
	tx := &Tmux{Socket: sock}
	defer exec.Command("tmux", "-L", sock, "kill-server").Run()

	name := "quack/itest"
	if tx.SessionExists(name) {
		t.Fatal("session should not exist yet")
	}
	err := tx.NewSession(context.Background(), session.NewSessionOpts{
		Name: name,
		Dir:  t.TempDir(),
		Env:  []string{"QUACK_SESSION_NAME=itest"},
		Argv: []string{"sleep", "30"},
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if !tx.SessionExists(name) {
		t.Fatal("session should exist after creation")
	}
}
