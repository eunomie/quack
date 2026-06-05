package agentproc

import (
	"context"
	"os/exec"
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

type fakeLauncher struct {
	gotProgram string
	gotArgs    []string
	gotDir     string
}

func (f *fakeLauncher) Command(ctx context.Context, program string, args []string, dir string, env []string) *exec.Cmd {
	f.gotProgram, f.gotArgs, f.gotDir = program, args, dir
	const stream = `{"type":"assistant","session_id":"s1","message":{"content":[{"type":"text","text":"hello"}]}}
{"type":"result","session_id":"s1","is_error":false,"total_cost_usd":0}`
	return exec.CommandContext(ctx, "printf", "%s", stream)
}

func TestClaudeRunTurnUsesLauncher(t *testing.T) {
	f := &fakeLauncher{}
	var texts []string
	done := Claude{}.RunTurn(context.Background(), Turn{Prompt: "hi", Workdir: "/work", Launcher: f}, func(e Event) {
		if a, ok := e.(AssistantText); ok {
			texts = append(texts, a.Text)
		}
	})
	if done.Err != nil {
		t.Fatalf("err: %v", done.Err)
	}
	if f.gotProgram != "claude" || f.gotDir != "/work" {
		t.Fatalf("program=%q dir=%q", f.gotProgram, f.gotDir)
	}
	if done.SessionRef != "s1" || len(texts) != 1 || texts[0] != "hello" {
		t.Fatalf("ref=%q texts=%v", done.SessionRef, texts)
	}
}

func TestDirectLauncherAppendsEnvWhenNonEmpty(t *testing.T) {
	cmd := DirectLauncher{}.Command(context.Background(), "echo", nil, "/work", []string{"FOO=bar"})
	if cmd.Env == nil {
		t.Fatal("Env should be set when a non-empty env is passed")
	}
	found := false
	for _, e := range cmd.Env {
		if e == "FOO=bar" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("FOO=bar not found in Env: %v", cmd.Env)
	}
	// And nil env must leave Env unset (inherit parent) — the prior behavior.
	plain := DirectLauncher{}.Command(context.Background(), "echo", nil, "/work", nil)
	if plain.Env != nil {
		t.Fatalf("nil env should leave cmd.Env unset, got %v", plain.Env)
	}
}

type fakeCodexLauncher struct {
	gotProgram string
	gotDir     string
}

func (f *fakeCodexLauncher) Command(ctx context.Context, program string, args []string, dir string, env []string) *exec.Cmd {
	f.gotProgram, f.gotDir = program, dir
	const stream = `{"type":"item.completed","thread_id":"t1","item":{"type":"agent_message","text":"hello"}}
`
	return exec.CommandContext(ctx, "printf", "%s", stream)
}

func TestCodexRunTurnUsesLauncher(t *testing.T) {
	f := &fakeCodexLauncher{}
	var texts []string
	done := Codex{}.RunTurn(context.Background(), Turn{Prompt: "hi", Workdir: "/work", Launcher: f}, func(e Event) {
		if a, ok := e.(AssistantText); ok {
			texts = append(texts, a.Text)
		}
	})
	if done.Err != nil {
		t.Fatalf("err: %v", done.Err)
	}
	if f.gotProgram != "codex" || f.gotDir != "/work" {
		t.Fatalf("program=%q dir=%q", f.gotProgram, f.gotDir)
	}
	if done.SessionRef != "t1" || len(texts) != 1 || texts[0] != "hello" {
		t.Fatalf("ref=%q texts=%v", done.SessionRef, texts)
	}
}
