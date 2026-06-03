package session

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/eunomie/quack/internal/agent"
)

// maybeTrust pre-seeds workspace trust for a freshly-created claude worktree so
// the interactive (tmux) path doesn't stop on the "trust this folder?" dialog.
// It is a no-op for non-claude agents (codex auto-trusts its run dirs) and
// harmless for the headless path (claude -p skips the dialog anyway).
func (s *Service) maybeTrust(ag agent.Agent, dir string) {
	if s.trustDir == nil || filepath.Base(ag.Command) != "claude" {
		return
	}
	_ = s.trustDir(dir)
}

// ensureClaudeTrust marks dir as trusted in ~/.claude.json (the field the claude
// CLI checks: projects.<abs-path>.hasTrustDialogAccepted). Best-effort atomic
// read-modify-write; claude also writes this file, so a concurrent write could
// be lost — acceptable for a single-user bot, and we only write when a path is
// not already trusted (i.e. once per new worktree).
func ensureClaudeTrust(dir string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	path := filepath.Join(home, ".claude.json")

	doc := map[string]any{}
	mode := os.FileMode(0o600)
	if data, rerr := os.ReadFile(path); rerr == nil {
		_ = json.Unmarshal(data, &doc)
		if fi, serr := os.Stat(path); serr == nil {
			mode = fi.Mode().Perm()
		}
	}

	projects, _ := doc["projects"].(map[string]any)
	if projects == nil {
		projects = map[string]any{}
		doc["projects"] = projects
	}
	proj, _ := projects[abs].(map[string]any)
	if proj == nil {
		proj = map[string]any{}
		projects[abs] = proj
	}
	if trusted, _ := proj["hasTrustDialogAccepted"].(bool); trusted {
		return nil // already trusted; skip the rewrite (and its write race)
	}
	proj["hasTrustDialogAccepted"] = true

	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".quack.tmp"
	if err := os.WriteFile(tmp, out, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
