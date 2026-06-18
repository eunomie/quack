package sandbox

import (
	"context"
	"errors"
	"strings"
	"testing"
)

var errInspect = errors.New("no such container")

func recordingDocker() (*Docker, *[][]string) {
	var calls [][]string
	d := &Docker{run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string{name}, args...))
		return []byte("ok"), nil
	}}
	return d, &calls
}

func hasCall(calls [][]string, substr string) bool {
	for _, c := range calls {
		if strings.Contains(strings.Join(c, " "), substr) {
			return true
		}
	}
	return false
}

func TestProvisionCreatesContainerSetAndClones(t *testing.T) {
	d, calls := recordingDocker()
	p := &DockerProvisioner{D: d, AgentImage: "quack-sandbox:latest", ProxyImage: "quack-egress:latest", DindImage: "docker:dind"}
	sb, err := p.Provision(context.Background(), Spec{
		SessionName: "feat-x",
		RepoURL:     "https://github.com/o/r",
		CloneRef:    "main",
		GitHubPAT:   "PAT",
		GitUserName: "Owner", GitUserEmail: "o@e",
		EgressAllow: []string{"github.com"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if sb.AgentContainer == "" || sb.Workdir == "" {
		t.Fatalf("handle incomplete: %+v", sb)
	}
	for _, want := range []string{
		"network create --internal", "volume create",
		"quack-egress:latest", "docker:dind", "quack-sandbox:latest",
		"git clone",
		// dind must be aliased "docker" on the agent net (its TLS cert's SAN) and
		// the agent must reach it there — verified live on the host (spike P4).
		"network connect --alias docker",
		"DOCKER_HOST=tcp://docker:2376",
	} {
		if !hasCall(*calls, want) {
			t.Fatalf("missing call containing %q in %v", want, *calls)
		}
	}
}

func TestProvisionEmptySandboxSkipsClone(t *testing.T) {
	d, calls := recordingDocker()
	p := &DockerProvisioner{D: d, AgentImage: "i", ProxyImage: "px", DindImage: "docker:dind"}
	sb, err := p.Provision(context.Background(), Spec{SessionName: "q", EgressAllow: []string{"x"}})
	if err != nil {
		t.Fatal(err)
	}
	if hasCall(*calls, "git clone") {
		t.Fatal("empty sandbox should not clone")
	}
	if sb.Workdir != "/work" {
		t.Fatalf("workdir = %q, want /work", sb.Workdir)
	}
}

func TestProvisionCopiesCredFilesWritable(t *testing.T) {
	d, calls := recordingDocker()
	p := &DockerProvisioner{D: d, AgentImage: "i", ProxyImage: "px", DindImage: "docker:dind"}
	_, err := p.Provision(context.Background(), Spec{
		SessionName: "q", EgressAllow: []string{"x"},
		CredFiles: []Mount{{Host: "/h/.config/dagger/credentials.json", Container: "/root/.config/dagger/credentials.json"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Creds are COPIED into the writable home (so OAuth can refresh), after the
	// parent dir is created — not bind-mounted read-only.
	if !hasCall(*calls, "mkdir -p /root/.config/dagger") {
		t.Fatalf("parent dir must be created before cp: %v", *calls)
	}
	if !hasCall(*calls, "cp /h/.config/dagger/credentials.json quack-q-agent:/root/.config/dagger/credentials.json") {
		t.Fatalf("cred files must be copied in (docker cp): %v", *calls)
	}
	if hasCall(*calls, "credentials.json:ro") {
		t.Fatal("creds must be copied, not read-only mounted")
	}
}

func TestProvisionKeepsOriginAtSource(t *testing.T) {
	d, calls := recordingDocker()
	p := &DockerProvisioner{D: d, AgentImage: "i", ProxyImage: "px", DindImage: "docker:dind"}
	_, err := p.Provision(context.Background(), Spec{
		SessionName: "feat-x", EgressAllow: []string{"x"},
		RepoURL: "https://github.com/dagger/dagger", CloneRef: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	// No fork: origin stays pointed at the clone source so guests branch and open
	// PRs directly on it. The provisioner must not rewrite the remotes.
	for _, unwanted := range []string{"remote rename", "remote add"} {
		if hasCall(*calls, unwanted) {
			t.Fatalf("origin must be left at the source, found %q in %v", unwanted, *calls)
		}
	}
}

func TestProvisionWritesAgentGuidance(t *testing.T) {
	d, calls := recordingDocker()
	p := &DockerProvisioner{D: d, AgentImage: "i", ProxyImage: "px", DindImage: "docker:dind"}
	_, err := p.Provision(context.Background(), Spec{
		SessionName: "q", EgressAllow: []string{"x"},
		GitUserName: "Tester", GitUserEmail: "dev@example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	var guidance string
	for _, c := range *calls {
		joined := strings.Join(c, " ")
		if strings.Contains(joined, "/root/.claude/CLAUDE.md") && strings.Contains(joined, "/root/.codex/AGENTS.md") {
			guidance = joined
		}
	}
	if guidance == "" {
		t.Fatalf("guidance not written to both claude+codex homes: %v", *calls)
	}
	// Sign-off identity is templated from the configured guest identity.
	if !strings.Contains(guidance, "Signed-off-by: Tester <dev@example.com>") {
		t.Fatalf("guidance missing templated sign-off: %s", guidance)
	}
	for _, want := range []string{"stg", "Co-Authored-By", "dagger/dagger"} {
		if !strings.Contains(guidance, want) {
			t.Fatalf("guidance missing %q: %s", want, guidance)
		}
	}
}

func TestHandleHoldsNoSecrets(t *testing.T) {
	d, _ := recordingDocker()
	p := &DockerProvisioner{D: d, AgentImage: "i", ProxyImage: "px", DindImage: "docker:dind"}
	sb, _ := p.Provision(context.Background(), Spec{SessionName: "q", GitHubPAT: "SECRET", EgressAllow: []string{"x"}})
	// The PAT must not be serialized onto the handle.
	if strings.Contains(strings.ToLower(sb.AgentContainer+sb.Workdir+sb.Name), "secret") {
		t.Fatal("handle should not embed secrets")
	}
}

func TestTeardownRemovesEverything(t *testing.T) {
	d, calls := recordingDocker()
	p := &DockerProvisioner{D: d}
	h := &Handle{AgentContainer: "a", DindContainer: "dd", ProxyContainer: "px",
		IntNetwork: "int", ExtNetwork: "ext", CertVolume: "cv", WorkVolume: "wv"}
	if err := p.Teardown(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"rm -f a", "rm -f dd", "rm -f px", "network rm int", "network rm ext", "volume rm -f wv", "volume rm -f cv"} {
		if !hasCall(*calls, want) {
			t.Fatalf("teardown missing %q in %v", want, *calls)
		}
	}
}

func TestProvisionRunsDiscordBrokerWhenConfigured(t *testing.T) {
	d, calls := recordingDocker()
	p := &DockerProvisioner{D: d, AgentImage: "i", ProxyImage: "px", DindImage: "docker:dind",
		DiscordImage: "quack-discord-broker:latest"}
	_, err := p.Provision(context.Background(), Spec{
		SessionName: "q", EgressAllow: []string{"x"},
		DiscordBotToken: "BOTTOKEN", DiscordReadGuildID: "G",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"--name quack-q-discord",
		"quack-discord-broker:latest",
		"GUILD_ID=G",
		"--network-alias quack-discord",               // stable in-sandbox hostname
		"network connect quack-q-ext quack-q-discord", // egress to discord.com
	} {
		if !hasCall(*calls, want) {
			t.Fatalf("missing broker call %q in %v", want, *calls)
		}
	}
	// The agent must reach the broker directly, not through the CONNECT-only proxy.
	if !hasCall(*calls, "NO_PROXY=docker,quack-discord") {
		t.Fatalf("agent NO_PROXY must include the broker alias: %v", *calls)
	}
}

func TestProvisionSkipsBrokerWhenImageUnset(t *testing.T) {
	d, calls := recordingDocker()
	p := &DockerProvisioner{D: d, AgentImage: "i", ProxyImage: "px", DindImage: "docker:dind"} // no DiscordImage
	_, err := p.Provision(context.Background(), Spec{
		SessionName: "q", EgressAllow: []string{"x"},
		DiscordBotToken: "BOTTOKEN", DiscordReadGuildID: "G",
	})
	if err != nil {
		t.Fatal(err)
	}
	if hasCall(*calls, "quack-q-discord") {
		t.Fatalf("broker must not run when DiscordImage is unset: %v", *calls)
	}
	if hasCall(*calls, "quack-discord") {
		t.Fatalf("NO_PROXY must not name the broker when it isn't run: %v", *calls)
	}
}

func TestProvisionSkipsBrokerWhenGuildUnset(t *testing.T) {
	d, calls := recordingDocker()
	p := &DockerProvisioner{D: d, AgentImage: "i", ProxyImage: "px", DindImage: "docker:dind",
		DiscordImage: "quack-discord-broker:latest"}
	_, err := p.Provision(context.Background(), Spec{
		SessionName: "q", EgressAllow: []string{"x"}, DiscordBotToken: "BOTTOKEN", // no guild id
	})
	if err != nil {
		t.Fatal(err)
	}
	if hasCall(*calls, "quack-q-discord") {
		t.Fatalf("broker must not run without a read guild id: %v", *calls)
	}
}

func TestTeardownRemovesBroker(t *testing.T) {
	d, calls := recordingDocker()
	p := &DockerProvisioner{D: d}
	h := &Handle{AgentContainer: "a", DindContainer: "dd", ProxyContainer: "px", DiscordContainer: "dc",
		IntNetwork: "int", ExtNetwork: "ext", CertVolume: "cv", WorkVolume: "wv"}
	if err := p.Teardown(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	if !hasCall(*calls, "rm -f dc") {
		t.Fatalf("teardown must remove the broker container: %v", *calls)
	}
}

func TestReattachStartsBroker(t *testing.T) {
	var calls [][]string
	d := &Docker{run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string{name}, args...))
		return []byte("ok"), nil // inspect succeeds -> agent exists
	}}
	p := &DockerProvisioner{D: d}
	h := &Handle{AgentContainer: "quack-q-agent", DindContainer: "quack-q-dind",
		ProxyContainer: "quack-q-proxy", DiscordContainer: "quack-q-discord"}
	if err := p.Reattach(context.Background(), h, Spec{}); err != nil {
		t.Fatal(err)
	}
	if !hasCall(calls, "start quack-q-discord") {
		t.Fatalf("reattach should start the broker container; calls=%v", calls)
	}
}

func TestLauncherBindsAgentContainer(t *testing.T) {
	p := &DockerProvisioner{}
	l := p.Launcher(&Handle{AgentContainer: "q-agent", Workdir: "/work/r"})
	cmd := l.Command(context.Background(), "claude", []string{"-p", "x"}, "/host", nil)
	got := strings.Join(cmd.Args, " ")
	if !strings.Contains(got, "exec -i -w /work/r q-agent claude -p x") {
		t.Fatalf("launcher argv = %q", got)
	}
}

func TestReattachRecreatesWhenAgentMissing(t *testing.T) {
	var calls [][]string
	// Fake runner: `inspect` (used by Exists) fails -> agent considered missing;
	// everything else succeeds.
	d := &Docker{run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string{name}, args...))
		for _, a := range args {
			if a == "inspect" {
				return nil, errInspect
			}
		}
		return []byte("ok"), nil
	}}
	p := &DockerProvisioner{D: d, AgentImage: "i", ProxyImage: "px", DindImage: "docker:dind"}
	h := &Handle{Name: "q", AgentContainer: "quack-q-agent", DindContainer: "quack-q-dind",
		ProxyContainer: "quack-q-proxy", IntNetwork: "quack-q-int", ExtNetwork: "quack-q-ext",
		CertVolume: "quack-q-certs", WorkVolume: "quack-q-work", Workdir: "/work/r"}
	if err := p.Reattach(context.Background(), h, Spec{SessionName: "q", EgressAllow: []string{"x"}}); err != nil {
		t.Fatal(err)
	}
	// Missing agent -> rebuild via bringUp (agent container run), but NOT a re-clone.
	if !hasCall(calls, "quack-sandbox") && !hasCall(calls, "--name quack-q-agent") {
		t.Fatalf("reattach should rebuild the agent container; calls=%v", calls)
	}
	if hasCall(calls, "git clone") {
		t.Fatal("reattach must not re-clone (work volume persists)")
	}
}

func TestReattachStartsWhenAgentExists(t *testing.T) {
	var calls [][]string
	d := &Docker{run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string{name}, args...))
		return []byte("ok"), nil // inspect succeeds -> agent exists
	}}
	p := &DockerProvisioner{D: d}
	h := &Handle{AgentContainer: "quack-q-agent", DindContainer: "quack-q-dind", ProxyContainer: "quack-q-proxy"}
	if err := p.Reattach(context.Background(), h, Spec{}); err != nil {
		t.Fatal(err)
	}
	if !hasCall(calls, "start quack-q-agent") {
		t.Fatalf("reattach of an existing sandbox should start containers; calls=%v", calls)
	}
	if hasCall(calls, "run -d") {
		t.Fatal("reattach of an existing sandbox should not re-run containers")
	}
}
