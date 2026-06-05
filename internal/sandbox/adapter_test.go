package sandbox

import (
	"reflect"
	"testing"

	"github.com/eunomie/quack/internal/session"
)

func TestSpecRoundTripsThroughConverters(t *testing.T) {
	in := session.SandboxSpec{
		SessionName: "s", RepoURL: "https://x/y", CloneRef: "main", RepoDir: "y",
		GitHubPAT: "PAT", GitUserName: "O", GitUserEmail: "o@e",
		ModelMounts: []session.SandboxMount{{Host: "/h", Container: "/c"}},
		AgentEnv:    []string{"A=B"}, EgressAllow: []string{"github.com"},
	}
	got := toSpec(in)
	if got.SessionName != "s" || got.RepoURL != "https://x/y" || got.GitHubPAT != "PAT" {
		t.Fatalf("toSpec lost fields: %+v", got)
	}
	if len(got.ModelMounts) != 1 || got.ModelMounts[0].Host != "/h" || got.ModelMounts[0].Container != "/c" {
		t.Fatalf("toSpec mounts: %+v", got.ModelMounts)
	}
	if !reflect.DeepEqual(got.EgressAllow, []string{"github.com"}) {
		t.Fatalf("toSpec egress: %+v", got.EgressAllow)
	}
}

func TestHandleRoundTripsThroughConverters(t *testing.T) {
	h := &Handle{Name: "n", AgentContainer: "a", DindContainer: "d", ProxyContainer: "p",
		IntNetwork: "i", ExtNetwork: "e", CertVolume: "cv", WorkVolume: "wv", Workdir: "/work/y"}
	sh := toSessionHandle(h)
	back := fromSessionHandle(sh)
	if !reflect.DeepEqual(h, back) {
		t.Fatalf("handle round-trip mismatch: %+v vs %+v", h, back)
	}
}
