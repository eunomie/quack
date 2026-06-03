package gitexec

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
)

func TestWorktreeAdd_Integration(t *testing.T) {
	if os.Getenv("QUACK_INTEGRATION") == "" {
		t.Skip("set QUACK_INTEGRATION=1 to run (needs git)")
	}
	dir := t.TempDir()
	clone := filepath.Join(dir, "repo")

	mustRun(t, "", "git", "init", "-b", "main", clone)
	mustRun(t, clone, "git", "config", "user.email", "t@example.com")
	mustRun(t, clone, "git", "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(clone, "f.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, clone, "git", "add", "-A")
	mustRun(t, clone, "git", "commit", "-m", "init")

	g := New()
	if !g.IsRepo(clone) {
		t.Fatal("IsRepo = false")
	}
	wt := filepath.Join(dir, "repo-worktrees", "feature")
	if err := g.AddWorktree(context.Background(), clone, wt, "feature", "main"); err != nil {
		t.Fatalf("AddWorktree: %v", err)
	}
	if !g.PathExists(filepath.Join(wt, "f.txt")) {
		t.Fatal("worktree file missing")
	}
	if !g.BranchExists(context.Background(), clone, "feature") {
		t.Fatal("branch not created")
	}
}

// TestForkRemotes_Integration reproduces the fork layout where the requested
// branch lives only on upstream (origin = your fork lacks it), the exact case
// that failed with "create worktree: ... invalid reference: origin/1.0-beta".
func TestForkRemotes_Integration(t *testing.T) {
	if os.Getenv("QUACK_INTEGRATION") == "" {
		t.Skip("set QUACK_INTEGRATION=1 to run (needs git)")
	}
	dir := t.TempDir()
	g := New()
	ctx := context.Background()

	// upstream (canonical): main + 1.0-beta (1.0-beta carries beta.txt).
	upstream := filepath.Join(dir, "upstream")
	mustRun(t, "", "git", "init", "-b", "main", upstream)
	gitID(t, upstream)
	writeCommit(t, upstream, "f.txt", "main", "init")
	mustRun(t, upstream, "git", "checkout", "-b", "1.0-beta")
	writeCommit(t, upstream, "beta.txt", "beta", "beta")
	mustRun(t, upstream, "git", "checkout", "main")

	// origin (the fork): main only, no 1.0-beta.
	origin := filepath.Join(dir, "origin")
	mustRun(t, "", "git", "init", "-b", "main", origin)
	gitID(t, origin)
	writeCommit(t, origin, "f.txt", "main", "init")

	// clone (local checkout) with both remotes wired.
	clone := filepath.Join(dir, "clone")
	mustRun(t, "", "git", "init", "-b", "main", clone)
	gitID(t, clone)
	writeCommit(t, clone, "f.txt", "main", "init")
	mustRun(t, clone, "git", "remote", "add", "origin", origin)
	mustRun(t, clone, "git", "remote", "add", "upstream", upstream)

	if err := g.Fetch(ctx, clone); err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	if names := g.Remotes(ctx, clone); !slices.Contains(names, "origin") || !slices.Contains(names, "upstream") {
		t.Fatalf("Remotes = %v, want origin+upstream", names)
	}
	if g.RefExists(ctx, clone, "origin/1.0-beta") {
		t.Fatal("origin/1.0-beta should not exist (fork lacks the branch)")
	}
	if !g.RefExists(ctx, clone, "upstream/1.0-beta") {
		t.Fatal("upstream/1.0-beta should exist after fetch --all")
	}

	// The previously failing operation now succeeds.
	wt := filepath.Join(dir, "clone-worktrees", "ship")
	if err := g.AddWorktree(ctx, clone, wt, "ship", "upstream/1.0-beta"); err != nil {
		t.Fatalf("AddWorktree from upstream/1.0-beta: %v", err)
	}
	if !g.PathExists(filepath.Join(wt, "beta.txt")) {
		t.Fatal("worktree should contain beta.txt from upstream/1.0-beta")
	}
}

func gitID(t *testing.T, dir string) {
	t.Helper()
	mustRun(t, dir, "git", "config", "user.email", "t@example.com")
	mustRun(t, dir, "git", "config", "user.name", "t")
}

func writeCommit(t *testing.T, dir, file, content, msg string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, dir, "git", "add", "-A")
	mustRun(t, dir, "git", "commit", "-m", msg)
}

func mustRun(t *testing.T, dir, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
}
