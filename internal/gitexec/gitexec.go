// Package gitexec implements session.Git using the git CLI.
package gitexec

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Git runs git commands on the host.
type Git struct{}

// New returns a Git adapter.
func New() *Git { return &Git{} }

func (Git) run(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	trimmed := strings.TrimSpace(string(out))
	if err != nil && trimmed != "" {
		// Surface git's own message (e.g. "fatal: 'origin' does not appear to
		// be a git repository") instead of a bare "exit status 1".
		return trimmed, fmt.Errorf("%w: %s", err, trimmed)
	}
	return trimmed, err
}

// PathExists reports whether path exists on disk.
func (Git) PathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Exists reports whether clonePath is a git working tree.
func (g Git) Exists(clonePath string) bool {
	return g.IsRepo(clonePath)
}

// IsRepo reports whether path is inside a git repository.
func (g Git) IsRepo(path string) bool {
	if !g.PathExists(path) {
		return false
	}
	out, err := g.run(context.Background(), path, "rev-parse", "--is-inside-work-tree")
	return err == nil && out == "true"
}

// PrimaryClone resolves path to the main worktree (parent of the common git dir).
func (g Git) PrimaryClone(path string) (string, error) {
	common, err := g.run(context.Background(), path, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", err
	}
	if filepath.Base(common) == ".git" {
		return filepath.Dir(common), nil
	}
	return common, nil
}

// Clone clones url into clonePath, creating parent directories.
func (g Git) Clone(ctx context.Context, url, clonePath string) error {
	if err := os.MkdirAll(filepath.Dir(clonePath), 0o755); err != nil {
		return err
	}
	_, err := g.run(ctx, "", "clone", url, clonePath)
	return err
}

// Fetch updates all remotes in clonePath (pruning deleted refs), so a worktree
// can be based on fresh state from any remote (e.g. origin or upstream).
func (g Git) Fetch(ctx context.Context, clonePath string) error {
	_, err := g.run(ctx, clonePath, "fetch", "--all", "--prune")
	return err
}

// DefaultBranch returns origin's default branch (e.g. "main").
func (g Git) DefaultBranch(ctx context.Context, clonePath string) (string, error) {
	out, err := g.run(ctx, clonePath, "rev-parse", "--abbrev-ref", "origin/HEAD")
	if err != nil {
		out, err = g.run(ctx, clonePath, "symbolic-ref", "refs/remotes/origin/HEAD")
		if err != nil {
			return "", err
		}
	}
	return strings.TrimPrefix(strings.TrimPrefix(out, "origin/"), "refs/remotes/origin/"), nil
}

// BranchExists reports whether a local branch exists.
func (g Git) BranchExists(ctx context.Context, clonePath, branch string) bool {
	_, err := g.run(ctx, clonePath, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	return err == nil
}

// RefExists reports whether ref resolves to a commit in clonePath. ref may be
// a local branch, a remote-tracking ref ("upstream/1.0-beta"), a tag, or a SHA.
func (g Git) RefExists(ctx context.Context, clonePath, ref string) bool {
	_, err := g.run(ctx, clonePath, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	return err == nil
}

// Remotes lists the configured remote names (e.g. "origin", "upstream").
func (g Git) Remotes(ctx context.Context, clonePath string) []string {
	out, err := g.run(ctx, clonePath, "remote")
	if err != nil || out == "" {
		return nil
	}
	return strings.Fields(out)
}

// AddWorktree creates a new worktree on a new branch from baseRef.
func (g Git) AddWorktree(ctx context.Context, clonePath, worktreePath, branch, baseRef string) error {
	_, err := g.run(ctx, clonePath, "worktree", "add", "-b", branch, worktreePath, baseRef)
	return err
}
