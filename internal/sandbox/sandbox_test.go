package sandbox

import (
	"context"
	"strings"
	"testing"
)

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

func TestHandleHoldsNoSecrets(t *testing.T) {
	d, _ := recordingDocker()
	p := &DockerProvisioner{D: d, AgentImage: "i", ProxyImage: "px", DindImage: "docker:dind"}
	sb, _ := p.Provision(context.Background(), Spec{SessionName: "q", GitHubPAT: "SECRET", EgressAllow: []string{"x"}})
	// The PAT must not be serialized onto the handle.
	if strings.Contains(strings.ToLower(sb.AgentContainer+sb.Workdir+sb.Name), "secret") {
		t.Fatal("handle should not embed secrets")
	}
}
