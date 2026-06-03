package session

import (
	crand "crypto/rand"
	"encoding/hex"
	"path/filepath"
	"strings"

	"github.com/eunomie/quack/internal/command"
	"github.com/eunomie/quack/internal/repo"
	"github.com/eunomie/quack/internal/worktree"
)

// defaultToken returns a short random token for auto-generated session names.
func defaultToken() string {
	b := make([]byte, 3)
	if _, err := crand.Read(b); err != nil {
		return "session"
	}
	return hex.EncodeToString(b) // 6 hex chars
}

// provisionalName is the name used for the early ack/thread, before the base
// branch is known. Explicit name wins; otherwise it's "<repo>-<token>" for a
// target, or the bare token (pure random) when there's no target.
func (s *Service) provisionalName(dir *command.Directive, token string) string {
	if dir.Name != "" {
		return dir.Name
	}
	base := repoBaseName(dir.Target)
	if base == "" {
		return token
	}
	return base + "-" + token
}

// repoBaseName extracts the repo/dir name from a target, or "" when there's no
// usable name (empty target, or an unparseable ref).
func repoBaseName(target string) string {
	if target == "" {
		return ""
	}
	var raw string
	if repo.IsPath(target) {
		raw = filepath.Base(strings.TrimRight(expandHome(target), "/"))
	} else if ref, err := repo.ParseRef(target); err == nil {
		raw = ref.Repo
	}
	slug := worktree.Slugify(raw)
	if slug == "session" { // Slugify's empty sentinel
		return ""
	}
	return slug
}

// worktreeName builds the auto name for a repo session once the base branch is
// known: "<repo>-<base>-<token>".
func worktreeName(clonePath, base, token string) string {
	repoSlug := worktree.Slugify(filepath.Base(clonePath))
	baseSlug := worktree.Slugify(base)
	parts := repoSlug
	if baseSlug != "" && baseSlug != "session" {
		parts += "-" + baseSlug
	}
	return parts + "-" + token
}
