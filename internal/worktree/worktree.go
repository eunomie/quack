// Package worktree derives session slugs and worktree paths.
package worktree

import (
	"regexp"
	"strings"
)

const maxSlugLen = 40

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// Slugify turns a prompt into a short, filesystem/branch-safe slug.
// Returns "session" when the prompt has no usable characters.
func Slugify(prompt string) string {
	s := nonSlug.ReplaceAllString(strings.ToLower(prompt), "-")
	s = strings.Trim(s, "-")
	if s == "" {
		return "session"
	}
	if len(s) > maxSlugLen {
		s = s[:maxSlugLen]
		if i := strings.LastIndex(s, "-"); i > 0 {
			s = s[:i]
		}
		s = strings.Trim(s, "-")
	}
	if s == "" {
		return "session"
	}
	return s
}

// Path returns the worktree directory for a clone, following the
// "<clone>-worktrees/<name>" convention observed in dagger/dagger.
func Path(clonePath, name string) string {
	return clonePath + "-worktrees/" + name
}
