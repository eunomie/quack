package session

import (
	"fmt"

	"github.com/eunomie/quack/internal/command"
	"github.com/eunomie/quack/internal/repo"
)

// clampGuestDirective normalizes a guest's directive to the safe envelope:
// always headless, never no-worktree (guests are always isolated). Returns a
// muted note when it had to override an explicit choice, "" otherwise.
func clampGuestDirective(d *command.Directive) string {
	note := ""
	if !d.Headless {
		d.Headless = true
		note = "interactive (no-headless) mode is owner-only — running headless in a sandbox instead."
	}
	d.NoWorktree = false
	return note
}

// guestTargetAllowed permits only a repo ref or no target (empty sandbox).
// Filesystem paths, ~ paths, and the temp-dir host escape are rejected — they
// would point at the host filesystem.
func guestTargetAllowed(target string) error {
	if target == "" {
		return nil
	}
	if target == targetTempDir || repo.IsPath(target) {
		return fmt.Errorf("guests can only target a repository (e.g. owner/repo), not host paths")
	}
	return nil
}
