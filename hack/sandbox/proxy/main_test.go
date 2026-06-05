package main

import "testing"

func TestAllowedHost(t *testing.T) {
	allow := parseAllow("api.anthropic.com,github.com,api.github.com")
	cases := map[string]bool{
		"api.anthropic.com:443":   true,
		"github.com:443":          true,
		"codeload.github.com:443": false, // not listed
		"evil.com:443":            false,
		"api.github.com:443":      true,
	}
	for hostport, want := range cases {
		if got := allowed(allow, hostport); got != want {
			t.Fatalf("allowed(%q) = %v, want %v", hostport, got, want)
		}
	}
}
