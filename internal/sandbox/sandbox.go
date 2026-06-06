package sandbox

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/eunomie/quack/internal/agentproc"
)

// Mount is a host:container credential file copied into the agent container
// (claude/codex/dagger auth), writable so OAuth tokens can refresh.
type Mount struct {
	Host      string
	Container string
}

// Spec describes one guest session's sandbox. Secrets (GitHubPAT) live here, not
// on the persisted Handle.
type Spec struct {
	SessionName  string
	RepoURL      string // "" => empty sandbox (no clone)
	CloneRef     string // base branch/ref; "" => remote default
	RepoDir      string // in-container dir name; default basename of RepoURL
	GitHubPAT    string
	GitUserName  string
	GitUserEmail string
	CredFiles    []Mount
	AgentEnv     []string
	EgressAllow  []string
}

// Handle identifies a provisioned sandbox. Persisted in the session record —
// so it holds only non-secret identifiers.
type Handle struct {
	Name           string `json:"name"`
	AgentContainer string `json:"agent_container"`
	DindContainer  string `json:"dind_container"`
	ProxyContainer string `json:"proxy_container"`
	IntNetwork     string `json:"int_network"`
	ExtNetwork     string `json:"ext_network"`
	CertVolume     string `json:"cert_volume"`
	WorkVolume     string `json:"work_volume"`
	Workdir        string `json:"workdir"`
}

// DockerProvisioner provisions guest sandboxes using the Docker CLI.
type DockerProvisioner struct {
	D          *Docker
	AgentImage string
	ProxyImage string
	DindImage  string
	ProxyPort  string // default "8888"
}

func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			return r
		case r >= 'A' && r <= 'Z':
			return r + 32
		default:
			return '-'
		}
	}, s)
}

func (p *DockerProvisioner) handleFor(sessionName string) *Handle {
	n := sanitize(sessionName)
	return &Handle{
		Name:           n,
		AgentContainer: "quack-" + n + "-agent",
		DindContainer:  "quack-" + n + "-dind",
		ProxyContainer: "quack-" + n + "-proxy",
		IntNetwork:     "quack-" + n + "-int",
		ExtNetwork:     "quack-" + n + "-ext",
		CertVolume:     "quack-" + n + "-certs",
		WorkVolume:     "quack-" + n + "-work",
		Workdir:        "/work",
	}
}

func (p *DockerProvisioner) port() string {
	if p.ProxyPort == "" {
		return "8888"
	}
	return p.ProxyPort
}

// bringUp creates the networks, volumes, proxy, dind sidecar, and agent
// container, and seeds git credentials — everything except cloning the repo.
// Shared by Provision (first time) and Reattach (rebuild against existing
// volumes without re-cloning).
func (p *DockerProvisioner) bringUp(ctx context.Context, h *Handle, spec Spec) error {
	port := p.port()

	if err := p.D.CreateNetwork(ctx, h.IntNetwork, true); err != nil {
		return err
	}
	if err := p.D.CreateNetwork(ctx, h.ExtNetwork, false); err != nil {
		return err
	}
	if err := p.D.CreateVolume(ctx, h.CertVolume); err != nil {
		return err
	}
	if err := p.D.CreateVolume(ctx, h.WorkVolume); err != nil {
		return err
	}

	// Egress proxy: internal net (agent reaches it) + external net (it reaches the allow-list).
	if err := p.D.Run(ctx, "--name", h.ProxyContainer, "--network", h.IntNetwork,
		"-e", "ALLOW="+strings.Join(spec.EgressAllow, ","),
		"-e", "ADDR=:"+port,
		p.ProxyImage); err != nil {
		return err
	}
	if err := p.D.ConnectNetwork(ctx, h.ExtNetwork, h.ProxyContainer); err != nil {
		return err
	}

	// dind sidecar: external net (registry pulls) + internal net (agent reaches its API).
	if err := p.D.Run(ctx, "--name", h.DindContainer, "--privileged", "--network", h.ExtNetwork,
		"-e", "DOCKER_TLS_CERTDIR=/certs",
		"-v", h.CertVolume+":/certs",
		p.DindImage); err != nil {
		return err
	}
	// Alias the dind sidecar as "docker" on the agent's network: dind's TLS server
	// cert is issued for the hostname "docker" (its canonical sibling name), not
	// the per-session container name, so the agent must reach it at tcp://docker.
	if err := p.D.ConnectNetwork(ctx, h.IntNetwork, h.DindContainer, "docker"); err != nil {
		return err
	}

	// Agent container: internal net only (no direct egress).
	runArgs := []string{
		"--name", h.AgentContainer,
		"--network", h.IntNetwork,
		"-v", h.WorkVolume + ":/work",
		"-v", h.CertVolume + ":/certs:ro",
		"-e", "HTTPS_PROXY=http://" + h.ProxyContainer + ":" + port,
		"-e", "HTTP_PROXY=http://" + h.ProxyContainer + ":" + port,
		"-e", "NO_PROXY=docker,localhost,127.0.0.1",
		"-e", "DOCKER_HOST=tcp://docker:2376",
		"-e", "DOCKER_TLS_VERIFY=1",
		"-e", "DOCKER_CERT_PATH=/certs/client",
		"-e", "GIT_AUTHOR_NAME=" + spec.GitUserName,
		"-e", "GIT_COMMITTER_NAME=" + spec.GitUserName,
		"-e", "GIT_AUTHOR_EMAIL=" + spec.GitUserEmail,
		"-e", "GIT_COMMITTER_EMAIL=" + spec.GitUserEmail,
		"-e", "GH_TOKEN=" + spec.GitHubPAT,
	}
	for _, e := range spec.AgentEnv {
		runArgs = append(runArgs, "-e", e)
	}
	runArgs = append(runArgs, "--entrypoint", "sleep", p.AgentImage, "infinity")
	if err := p.D.Run(ctx, runArgs...); err != nil {
		return err
	}

	// Copy shared credential files into the agent's writable home, rather than
	// bind-mounting them read-only: claude/codex/dagger refresh their OAuth tokens
	// on use and must be able to rewrite the file. Re-sourced from current config
	// on every (re)provision, so the host copies stay authoritative.
	for _, m := range spec.CredFiles {
		if dir := filepath.Dir(m.Container); dir != "." && dir != "/" {
			if _, err := p.D.Exec(ctx, h.AgentContainer, "mkdir", "-p", dir); err != nil {
				return err
			}
		}
		if err := p.D.Copy(ctx, m.Host, h.AgentContainer, m.Container); err != nil {
			return err
		}
	}

	// Seed git credentials (HTTPS store) so push/gh work without an SSH key.
	cred := fmt.Sprintf("https://x-access-token:%s@github.com", spec.GitHubPAT)
	seed := "git config --global credential.helper store && " +
		"git config --global user.name \"$GIT_AUTHOR_NAME\" && " +
		"git config --global user.email \"$GIT_AUTHOR_EMAIL\" && " +
		"printf '%s\\n' '" + cred + "' > ~/.git-credentials && chmod 600 ~/.git-credentials"
	if _, err := p.D.Exec(ctx, h.AgentContainer, "sh", "-lc", seed); err != nil {
		return err
	}

	return nil
}

// Provision stands up a fresh sandbox and clones the repo (empty sandbox if no
// RepoURL). The clone uses the remote default branch when CloneRef is empty.
func (p *DockerProvisioner) Provision(ctx context.Context, spec Spec) (*Handle, error) {
	h := p.handleFor(spec.SessionName)
	if err := p.bringUp(ctx, h, spec); err != nil {
		return nil, err
	}
	if spec.RepoURL != "" {
		dir := spec.RepoDir
		if dir == "" {
			dir = repoBase(spec.RepoURL)
		}
		clone := []string{"git", "clone"}
		if spec.CloneRef != "" {
			clone = append(clone, "--branch", spec.CloneRef)
		}
		clone = append(clone, spec.RepoURL, "/work/"+dir)
		if _, err := p.D.Exec(ctx, h.AgentContainer, clone...); err != nil {
			return nil, err
		}
		h.Workdir = "/work/" + dir
	}
	return h, nil
}

func repoBase(url string) string {
	u := strings.TrimSuffix(url, ".git")
	if i := strings.LastIndex(u, "/"); i >= 0 {
		return u[i+1:]
	}
	return u
}

// Teardown removes the whole container set. Best-effort: continues past
// individual failures so a partial provision can still be cleaned up.
func (p *DockerProvisioner) Teardown(ctx context.Context, h *Handle) error {
	for _, c := range []string{h.AgentContainer, h.DindContainer, h.ProxyContainer} {
		if c != "" {
			_ = p.D.Remove(ctx, c)
		}
	}
	for _, n := range []string{h.IntNetwork, h.ExtNetwork} {
		if n != "" {
			_ = p.D.RemoveNetwork(ctx, n)
		}
	}
	for _, v := range []string{h.WorkVolume, h.CertVolume} {
		if v != "" {
			_ = p.D.RemoveVolume(ctx, v)
		}
	}
	return nil
}

// Launcher returns the per-session launcher that runs turns inside the agent
// container.
func (p *DockerProvisioner) Launcher(h *Handle) agentproc.Launcher {
	return agentproc.ContainerLauncher{Container: h.AgentContainer, Workdir: h.Workdir}
}

// Reattach restores a sandbox after a quack restart. If the agent container
// still exists (running or stopped), it just (re)starts the set. If the
// containers are gone but the work volume persists (e.g. host reboot with manual
// container pruning), it rebuilds them via bringUp WITHOUT re-cloning — the work
// volume already holds the clone. Secrets come from spec (current config), never
// from the persisted handle.
//
// NOTE (host-verification follow-up): on the rebuild path the networks/volumes
// from the original provision may still exist; making the create calls tolerant
// of "already exists" is validated against real Docker in the integration step.
func (p *DockerProvisioner) Reattach(ctx context.Context, h *Handle, spec Spec) error {
	if p.D.Exists(ctx, h.AgentContainer) {
		_ = p.D.Start(ctx, h.ProxyContainer)
		_ = p.D.Start(ctx, h.DindContainer)
		_ = p.D.Start(ctx, h.AgentContainer)
		return nil
	}
	return p.bringUp(ctx, h, spec)
}
