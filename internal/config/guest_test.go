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
	if g.DefaultRepo != "dagger/dagger" {
		t.Fatalf("default_repo default = %q, want dagger/dagger", g.DefaultRepo)
	}
}

func TestGuestConfigKeepsExplicitRepo(t *testing.T) {
	g := Guest{DefaultRepo: "acme/widgets"}.WithDefaults()
	if g.DefaultRepo != "acme/widgets" {
		t.Fatalf("explicit value overwritten: %+v", g)
	}
}
