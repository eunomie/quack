package session

import (
	"strings"
	"testing"
)

func TestDiscordPromptBlockEnabled(t *testing.T) {
	s := &Service{guest: GuestPolicy{
		DiscordBrokerURL:   "http://quack-discord:8080",
		DiscordReadGuildID: "G123",
	}}
	b := s.discordPromptBlock("THREAD42")
	for _, want := range []string{
		"<quack-discord>",
		"http://quack-discord:8080",
		"THREAD42",
		"/channels/THREAD42/messages",
		"G123",
	} {
		if !strings.Contains(b, want) {
			t.Errorf("block missing %q:\n%s", want, b)
		}
	}
}

func TestDiscordPromptBlockDisabled(t *testing.T) {
	s := &Service{guest: GuestPolicy{}} // no broker URL -> broker off
	if got := s.discordPromptBlock("T"); got != "" {
		t.Fatalf("disabled broker should yield empty block, got %q", got)
	}
}
