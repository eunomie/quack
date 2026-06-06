package session

import (
	"context"
	"fmt"

	"github.com/eunomie/quack/internal/agentproc"
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
func (s *Service) prepareGuest(ctx context.Context, dir *command.Directive, name string) (prepResult, error) {
	spec := SandboxSpec{
		SessionName:  name,
		GitHubPAT:    s.guest.GitHubPAT,
		GitUserName:  s.guest.GitUserName,
		GitUserEmail: s.guest.GitUserEmail,
		EgressAllow:  s.guest.EgressAllow,
		CredFiles:    s.guest.CredFiles,
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
		CredFiles:    s.guest.CredFiles,
	}
}

// guestDriver returns the agent driver for guest sessions: the base driver with
// guest tool/skill restrictions applied. Only claude supports tool filtering;
// other agents (codex) are returned unchanged (codex has no skills).
func (s *Service) guestDriver(agentName string) agentproc.Driver {
	base := s.drivers[agentName]
	c, ok := base.(agentproc.Claude)
	if !ok {
		return base
	}
	allowed, disallowed := claudeGuestToolFlags(s.guest)
	c.AllowedTools = allowed
	c.DisallowedTools = disallowed
	return c
}

// claudeGuestToolFlags encodes the guest tool/skill policy into claude
// --allowedTools/--disallowedTools values. Individual skills are matched as
// Skill(<name>) (exact matcher token confirmed in host-verification spike P3).
//
// NOTE: AllowedSkills is carried in GuestPolicy but the default guest
// restriction is expressed via DisallowedSkills. The allow-list mechanism for
// skills is deferred to P3 once the exact claude matcher semantics are
// confirmed — do not add it prematurely.
func claudeGuestToolFlags(p GuestPolicy) (allowed, disallowed string) {
	allowed = p.AllowedTools
	disallowed = p.DisallowedTools
	for _, sk := range p.DisallowedSkills {
		disallowed = appendCSV(disallowed, "Skill("+sk+")")
	}
	return allowed, disallowed
}

// appendCSV appends item to a comma-separated list, or returns item if the
// list is empty.
func appendCSV(csv, item string) string {
	if csv == "" {
		return item
	}
	return csv + "," + item
}
