package discord

import (
	"strings"

	"github.com/bwmarrin/discordgo"
)

// flattenMessage renders a message to plain text. Discord webhook messages
// (e.g. the docs-feedback widget) carry an empty Content and put their real
// payload in embeds, so a bare m.Content loses everything that matters. This
// appends each embed's author/title/description/fields/footer so the agent and
// the infer one-shot see the actual content, not a blank string.
func flattenMessage(m *discordgo.Message) string {
	var b strings.Builder
	write := func(s string) {
		if s = strings.TrimSpace(s); s == "" {
			return
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(s)
	}

	write(m.Content)
	for _, e := range m.Embeds {
		if e == nil {
			continue
		}
		if e.Author != nil {
			write(e.Author.Name)
		}
		write(e.Title)
		write(e.Description)
		for _, f := range e.Fields {
			if f == nil {
				continue
			}
			write(f.Name + ": " + f.Value)
		}
		if e.Footer != nil {
			write(e.Footer.Text)
		}
	}
	return b.String()
}
