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

func TestContainerLauncherWrapsDockerExec(t *testing.T) {
	l := ContainerLauncher{Container: "q-agent", Workdir: "/work/repo", DockerCmd: "docker"}
	cmd := l.Command(context.Background(), "claude", []string{"-p", "hi"}, "/ignored/host/path", []string{"FOO=bar"})
	want := []string{"docker", "exec", "-i", "-w", "/work/repo", "-e", "FOO=bar", "q-agent", "claude", "-p", "hi"}
	if len(cmd.Args) != len(want) {
		t.Fatalf("argv = %v", cmd.Args)
	}
	for i := range want {
		if cmd.Args[i] != want[i] {
			t.Fatalf("argv[%d] = %q, want %q (full %v)", i, cmd.Args[i], want[i], cmd.Args)
		}
	}
}
