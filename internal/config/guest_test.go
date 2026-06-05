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
}
