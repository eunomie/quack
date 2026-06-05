package agentproc

import (
	"context"
	"testing"
)

func TestDirectLauncherSetsProgramArgsDir(t *testing.T) {
	cmd := DirectLauncher{}.Command(context.Background(), "claude", []string{"-p", "hi"}, "/work", nil)
	if cmd.Args[0] != "claude" || cmd.Args[1] != "-p" || cmd.Args[2] != "hi" {
		t.Fatalf("argv = %v", cmd.Args)
	}
	if cmd.Dir != "/work" {
		t.Fatalf("dir = %q", cmd.Dir)
	}
}
