package discord

import (
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
)

func TestFlattenMessagePlainContent(t *testing.T) {
	if got := flattenMessage(&discordgo.Message{Content: "hello"}); got != "hello" {
		t.Errorf("flattenMessage = %q, want %q", got, "hello")
	}
}

// A docs-feedback webhook message: empty Content, payload entirely in an embed.
func TestFlattenMessageEmbed(t *testing.T) {
	m := &discordgo.Message{
		Embeds: []*discordgo.MessageEmbed{{
			Title:       "New docs feedback",
			Description: "the page is confusing",
			Fields: []*discordgo.MessageEmbedField{
				{Name: "page", Value: "/language/objects"},
				{Name: "excerpt", Value: "objects are prototypes"},
			},
		}},
	}
	got := flattenMessage(m)
	for _, want := range []string{"New docs feedback", "the page is confusing", "page: /language/objects", "excerpt: objects are prototypes"} {
		if !strings.Contains(got, want) {
			t.Errorf("flattened text missing %q\n%s", want, got)
		}
	}
}

func TestFlattenMessageEmpty(t *testing.T) {
	if got := flattenMessage(&discordgo.Message{}); got != "" {
		t.Errorf("flattenMessage = %q, want empty", got)
	}
}
