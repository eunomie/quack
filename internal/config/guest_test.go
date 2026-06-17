package config

import "testing"

func TestGuestConfigDefaults(t *testing.T) {
	g := Guest{}.WithDefaults()
	if g.ProxyPort != "8888" || g.DindImage != "docker:dind" {
		t.Fatalf("defaults: %+v", g)
	}
	if g.Image != "quack-sandbox:latest" || g.ProxyImage != "quack-egress:latest" {
		t.Fatalf("image defaults: %+v", g)
	}
	if len(g.EgressAllow) == 0 {
		t.Fatal("expected default egress allow-list")
	}
	if g.ForkOwner != "eunomie-quack" {
		t.Fatalf("fork_owner default = %q, want eunomie-quack", g.ForkOwner)
	}
	if g.DefaultRepo != "dagger/dagger" {
		t.Fatalf("default_repo default = %q, want dagger/dagger", g.DefaultRepo)
	}
}

func TestGuestConfigKeepsExplicitForkAndRepo(t *testing.T) {
	g := Guest{ForkOwner: "acme", DefaultRepo: "acme/widgets"}.WithDefaults()
	if g.ForkOwner != "acme" || g.DefaultRepo != "acme/widgets" {
		t.Fatalf("explicit values overwritten: %+v", g)
	}
}
