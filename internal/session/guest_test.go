package session

import (
	"context"
	"strings"
	"testing"

	"github.com/eunomie/quack/internal/agentproc"
	"github.com/eunomie/quack/internal/command"
)

type fakeSandboxer struct {
	gotSpec SandboxSpec
	handle  *SandboxHandle

	teardowns    int         // Teardown call count
	reattaches   int         // Reattach call count
	reattachSpec SandboxSpec // spec passed to the last Reattach
}

func (f *fakeSandboxer) Provision(ctx context.Context, spec SandboxSpec) (*SandboxHandle, error) {
	f.gotSpec = spec
	f.handle = &SandboxHandle{AgentContainer: "q-agent", Workdir: "/work/r", Name: spec.SessionName}
	return f.handle, nil
}
func (f *fakeSandboxer) Teardown(context.Context, *SandboxHandle) error { f.teardowns++; return nil }
func (f *fakeSandboxer) Reattach(_ context.Context, _ *SandboxHandle, spec SandboxSpec) error {
	f.reattaches++
	f.reattachSpec = spec
	return nil
}
func (f *fakeSandboxer) Launcher(h *SandboxHandle) agentproc.Launcher {
	return agentproc.ContainerLauncher{Container: h.AgentContainer, Workdir: h.Workdir}
}

func TestClampGuestDirective(t *testing.T) {
	d := &command.Directive{Headless: false, Prompt: "x", Target: "o/r"}
	note := clampGuestDirective(d)
	if !d.Headless {
		t.Fatal("guest must be forced headless")
	}
	if note == "" {
		t.Fatal("expected a note explaining the clamp")
	}
	d2 := &command.Directive{Headless: true, NoWorktree: true, Target: "o/r", Prompt: "x"}
	if note2 := clampGuestDirective(d2); note2 != "" {
		t.Fatalf("no clamp note expected when already headless, got %q", note2)
	}
	if d2.NoWorktree {
		t.Fatal("guest no-wt must be cleared")
	}
}

func TestPrepareGuestProvisionsSandboxForRepo(t *testing.T) {
	s := New(Config{}, nil, nil, nil) // git/tmux/replier unused on this path
	fs := &fakeSandboxer{}
	s.UseSandbox(fs, GuestPolicy{GitHubPAT: "PAT", GitUserName: "O", GitUserEmail: "o@e", EgressAllow: []string{"github.com"}})
	dir := &command.Directive{Target: "owner/repo", Prompt: "x", Base: "main"}
	prep, err := s.prepareGuest(context.Background(), dir, "name")
	if err != nil {
		t.Fatal(err)
	}
	if fs.gotSpec.RepoURL == "" {
		t.Fatal("expected a clone URL in the spec")
	}
	if fs.gotSpec.GitHubPAT != "PAT" {
		t.Fatalf("spec should carry the PAT from policy, got %q", fs.gotSpec.GitHubPAT)
	}
	if prep.launcher == nil || prep.sandbox == nil {
		t.Fatalf("prep missing sandbox/launcher: %+v", prep)
	}
	if prep.label != "owner/repo" {
		t.Fatalf("label = %q, want owner/repo", prep.label)
	}
}

func TestPrepareGuestEmptySandboxNoTarget(t *testing.T) {
	s := New(Config{}, nil, nil, nil)
	fs := &fakeSandboxer{}
	s.UseSandbox(fs, GuestPolicy{})
	prep, err := s.prepareGuest(context.Background(), &command.Directive{Prompt: "hi"}, "name")
	if err != nil {
		t.Fatal(err)
	}
	if fs.gotSpec.RepoURL != "" {
		t.Fatal("no target => empty sandbox, no RepoURL")
	}
	if prep.sandbox == nil || prep.launcher == nil {
		t.Fatal("empty sandbox still gets a handle + launcher")
	}
}

func TestGuestDriverAppliesToolPolicy(t *testing.T) {
	s := New(Config{}, nil, nil, nil)
	s.UseDrivers(map[string]agentproc.Driver{"claude": agentproc.Claude{Command: "claude", PermissionMode: "auto"}})
	s.UseSandbox(&fakeSandboxer{}, GuestPolicy{
		DisallowedSkills: []string{"open-zed"},
		AllowedSkills:    []string{"revue"},
	})
	d := s.guestDriver("claude")
	c, ok := d.(agentproc.Claude)
	if !ok {
		t.Fatalf("expected claude driver, got %T", d)
	}
	if !strings.Contains(c.DisallowedTools, "open-zed") {
		t.Fatalf("guest claude must disallow open-zed: %q", c.DisallowedTools)
	}
	// non-claude driver is returned unchanged
	s.UseDrivers(map[string]agentproc.Driver{"codex": agentproc.Codex{Command: "codex"}})
	if _, ok := s.guestDriver("codex").(agentproc.Codex); !ok {
		t.Fatal("codex guest driver should be unchanged codex")
	}
}

func TestGuestTargetRejectsHostPaths(t *testing.T) {
	for _, tgt := range []string{"/abs/path", "~/x", "./rel", "temp-dir"} {
		if err := guestTargetAllowed(tgt); err == nil {
			t.Fatalf("target %q should be rejected for guests", tgt)
		}
	}
	for _, tgt := range []string{"", "owner/repo"} {
		if err := guestTargetAllowed(tgt); err != nil {
			t.Fatalf("target %q should be allowed for guests: %v", tgt, err)
		}
	}
}
