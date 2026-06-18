package sandbox

import (
	"context"

	"github.com/eunomie/quack/internal/agentproc"
	"github.com/eunomie/quack/internal/session"
)

// Adapter implements session.Sandboxer over a DockerProvisioner, converting
// between session's plain types and this package's Spec/Handle.
type Adapter struct{ P *DockerProvisioner }

func (a Adapter) Provision(ctx context.Context, s session.SandboxSpec) (*session.SandboxHandle, error) {
	h, err := a.P.Provision(ctx, toSpec(s))
	if err != nil {
		return nil, err
	}
	return toSessionHandle(h), nil
}

func (a Adapter) Teardown(ctx context.Context, h *session.SandboxHandle) error {
	return a.P.Teardown(ctx, fromSessionHandle(h))
}

func (a Adapter) Reattach(ctx context.Context, h *session.SandboxHandle, s session.SandboxSpec) error {
	return a.P.Reattach(ctx, fromSessionHandle(h), toSpec(s))
}

func (a Adapter) Launcher(h *session.SandboxHandle) agentproc.Launcher {
	return a.P.Launcher(fromSessionHandle(h))
}

func toSpec(s session.SandboxSpec) Spec {
	mounts := make([]Mount, len(s.CredFiles))
	for i, m := range s.CredFiles {
		mounts[i] = Mount{Host: m.Host, Container: m.Container}
	}
	return Spec{
		SessionName:        s.SessionName,
		RepoURL:            s.RepoURL,
		CloneRef:           s.CloneRef,
		RepoDir:            s.RepoDir,
		GitHubPAT:          s.GitHubPAT,
		GitUserName:        s.GitUserName,
		GitUserEmail:       s.GitUserEmail,
		CredFiles:          mounts,
		AgentEnv:           s.AgentEnv,
		EgressAllow:        s.EgressAllow,
		DiscordBotToken:    s.DiscordBotToken,
		DiscordReadGuildID: s.DiscordReadGuildID,
	}
}

func toSessionHandle(h *Handle) *session.SandboxHandle {
	return &session.SandboxHandle{
		Name:             h.Name,
		AgentContainer:   h.AgentContainer,
		DindContainer:    h.DindContainer,
		ProxyContainer:   h.ProxyContainer,
		DiscordContainer: h.DiscordContainer,
		IntNetwork:       h.IntNetwork,
		ExtNetwork:       h.ExtNetwork,
		CertVolume:       h.CertVolume,
		WorkVolume:       h.WorkVolume,
		Workdir:          h.Workdir,
	}
}

func fromSessionHandle(h *session.SandboxHandle) *Handle {
	return &Handle{
		Name:             h.Name,
		AgentContainer:   h.AgentContainer,
		DindContainer:    h.DindContainer,
		ProxyContainer:   h.ProxyContainer,
		DiscordContainer: h.DiscordContainer,
		IntNetwork:       h.IntNetwork,
		ExtNetwork:       h.ExtNetwork,
		CertVolume:       h.CertVolume,
		WorkVolume:       h.WorkVolume,
		Workdir:          h.Workdir,
	}
}
