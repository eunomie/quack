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

// An owner who used `! sandbox` takes the sandbox path even though their Role is
// owner: prepare(..., sandboxed=true) provisions a sandbox just like a guest.
func TestPrepareSandboxedOwnerProvisions(t *testing.T) {
	s := New(Config{}, nil, nil, nil)
	fs := &fakeSandboxer{}
	s.UseSandbox(fs, GuestPolicy{GitHubPAT: "PAT", EgressAllow: []string{"github.com"}})
	dir := &command.Directive{Target: "owner/repo", Prompt: "x"}
	prep, err := s.prepare(context.Background(), dir, "prov", false, "tok", "name", true)
	if err != nil {
		t.Fatal(err)
	}
	if fs.gotSpec.RepoURL == "" {
		t.Fatal("sandboxed owner should provision a sandbox with a clone URL")
	}
	if prep.sandbox == nil || prep.launcher == nil {
		t.Fatalf("prep missing sandbox/launcher: %+v", prep)
	}
}

// An owner WITHOUT the keyword (sandboxed=false) targeting a repo takes the
// normal owner worktree path — never prepareGuest, so the sandboxer is untouched.
func TestPrepareUnsandboxedOwnerSkipsSandbox(t *testing.T) {
	s := New(Config{DevSrcRoot: t.TempDir()}, newFakeGit(), newFakeTmux(), newFakeReplier())
	fs := &fakeSandboxer{}
	s.UseSandbox(fs, GuestPolicy{GitHubPAT: "PAT"})
	dir := &command.Directive{Target: "owner/repo", Prompt: "x", Headless: true}
	prep, err := s.prepare(context.Background(), dir, "prov", false, "tok", "name", false)
	if err != nil {
		t.Fatal(err)
	}
	if fs.gotSpec.RepoURL != "" || fs.handle != nil {
		t.Fatal("unsandboxed owner must NOT provision a sandbox")
	}
	if prep.sandbox != nil {
		t.Fatalf("owner worktree path should yield no sandbox handle: %+v", prep)
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

func TestPrepareGuestNoTargetClonesDefaultRepo(t *testing.T) {
	s := New(Config{}, nil, nil, nil)
	fs := &fakeSandboxer{}
	s.UseSandbox(fs, GuestPolicy{DefaultRepo: "dagger/dagger"})
	prep, err := s.prepareGuest(context.Background(), &command.Directive{Prompt: "fix a bug"}, "name")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fs.gotSpec.RepoURL, "dagger/dagger") {
		t.Fatalf("no target + default_repo should clone dagger/dagger, got %q", fs.gotSpec.RepoURL)
	}
	if prep.label != "dagger/dagger" {
		t.Fatalf("label = %q, want dagger/dagger", prep.label)
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
