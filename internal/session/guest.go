package session

import (
	"context"
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

// prepareGuest provisions an isolated Docker sandbox for a guest. A repo target
// is cloned fresh inside the container; no target yields an empty sandbox. The
// clone URL is HTTPS (the injected PAT authenticates; no SSH key in the jail).
func (s *Service) prepareGuest(ctx context.Context, dir *command.Directive, provisional, name string) (prepResult, error) {
	spec := SandboxSpec{
		SessionName:  name,
		GitHubPAT:    s.guest.GitHubPAT,
		GitUserName:  s.guest.GitUserName,
		GitUserEmail: s.guest.GitUserEmail,
		EgressAllow:  s.guest.EgressAllow,
		ModelMounts:  s.guest.ModelMounts,
	}
	label := ""
	if dir.Target != "" {
		ref, err := repo.ParseRef(dir.Target)
		if err != nil {
			return prepResult{}, err
		}
		spec.RepoURL = ref.CloneURL("https")
		spec.CloneRef = dir.Base
		spec.RepoDir = ref.Repo
		label = ref.Owner + "/" + ref.Repo
	}
	h, err := s.sandbox.Provision(ctx, spec)
	if err != nil {
		return prepResult{}, fmt.Errorf("provision sandbox: %w", err)
	}
	return prepResult{
		workdir:  h.Workdir,
		name:     name,
		isolated: true,
		label:    label,
		sandbox:  h,
		launcher: s.sandbox.Launcher(h),
	}, nil
}

// guestReattachSpec rebuilds the SandboxSpec needed to bring a guest's sandbox
// back after a restart. Secrets/egress come from current config (GuestPolicy),
// never from the persisted record. No repo fields are needed — Reattach's rebuild
// path does not re-clone (the work volume persists the clone).
func (s *Service) guestReattachSpec(rec sessionRecord) SandboxSpec {
	return SandboxSpec{
		SessionName:  rec.Name,
		GitHubPAT:    s.guest.GitHubPAT,
		GitUserName:  s.guest.GitUserName,
		GitUserEmail: s.guest.GitUserEmail,
		EgressAllow:  s.guest.EgressAllow,
		ModelMounts:  s.guest.ModelMounts,
	}
}
