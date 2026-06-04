package discord

import (
	"testing"

	"github.com/bwmarrin/discordgo"

	"github.com/eunomie/quack/internal/command"
)

func msg(mentions []string, roles []string) *discordgo.MessageCreate {
	var users []*discordgo.User
	for _, id := range mentions {
		users = append(users, &discordgo.User{ID: id})
	}
	return &discordgo.MessageCreate{Message: &discordgo.Message{
		Mentions:     users,
		MentionRoles: roles,
	}}
}

// A direct user mention (<@botID>) addresses the bot.
func TestMentionsBot_UserMention(t *testing.T) {
	if !mentionsBot(msg([]string{"BOT"}, nil), "BOT", nil) {
		t.Error("user mention of the bot should count")
	}
	if mentionsBot(msg([]string{"SOMEONE"}, nil), "BOT", nil) {
		t.Error("user mention of someone else should not count")
	}
}

// A role mention (<@&roleID>) addresses the bot only when the role is one the
// bot answers to. Regression: the 08:28 drop was a role mention that landed in
// MentionRoles, which the old user-only check ignored.
func TestMentionsBot_RoleMention(t *testing.T) {
	m := msg(nil, []string{"ROLE"})
	if mentionsBot(m, "BOT", nil) {
		t.Error("role mention with no known bot roles should not count")
	}
	if mentionsBot(m, "BOT", map[string]bool{"OTHER": true}) {
		t.Error("mention of an unrelated role should not count")
	}
	if !mentionsBot(m, "BOT", map[string]bool{"ROLE": true}) {
		t.Error("mention of the bot's role should count")
	}
}

func authMsg(userID, guildID, channelID string) *discordgo.MessageCreate {
	return &discordgo.MessageCreate{Message: &discordgo.Message{
		Author:    &discordgo.User{ID: userID},
		GuildID:   guildID,
		ChannelID: channelID,
	}}
}

// An empty allowlist permits any message; a populated dimension restricts to
// its members and combines with the others via AND.
func TestAuthorized_MultiGuild(t *testing.T) {
	open := &Bot{} // no allowlist => any
	if !open.authorized(authMsg("u9", "g9", "c9")) {
		t.Error("empty allowlist should authorize any message")
	}

	b := &Bot{allowed: Allow{
		UserIDs:  []string{"u1", "u2"},
		GuildIDs: []string{"g1", "g2"},
	}}
	if !b.authorized(authMsg("u2", "g2", "anychan")) {
		t.Error("user and guild both in the allowlist should be authorized")
	}
	if b.authorized(authMsg("u2", "g3", "anychan")) {
		t.Error("guild g3 not in allowlist should be rejected")
	}
	if b.authorized(authMsg("u3", "g1", "anychan")) {
		t.Error("user u3 not in allowlist should be rejected")
	}

	// Channel restriction applies to top-level mentions but not thread feeds.
	chanBound := &Bot{allowed: Allow{ChannelIDs: []string{"c1"}}}
	if chanBound.authorized(authMsg("u", "g", "c2")) {
		t.Error("channel c2 not in allowlist should be rejected")
	}
	if !chanBound.authorizedThread(authMsg("u", "g", "c2")) {
		t.Error("thread auth ignores channel restriction")
	}
}

// Mention alone on line 1, prompt on line 2 — the directive line is empty.
// Regression: stripMention must not collapse the separating newline.
func TestStripMention_EmptyDirectiveLine(t *testing.T) {
	stripped := stripMention("<@B> \nRead the gist and summarize it", "B", nil)
	d, err := command.Parse(stripped)
	if err != nil {
		t.Fatalf("Parse(%q): %v", stripped, err)
	}
	if d.Target != "" {
		t.Errorf("Target = %q, want empty (no repo)", d.Target)
	}
	if d.Prompt != "Read the gist and summarize it" {
		t.Errorf("Prompt = %q", d.Prompt)
	}
}

// A directive on the same line as the mention still parses.
func TestStripMention_SameLineDirective(t *testing.T) {
	stripped := stripMention("<@B> dagger/dagger codex effort=high\nfix it", "B", nil)
	d, err := command.Parse(stripped)
	if err != nil {
		t.Fatalf("Parse(%q): %v", stripped, err)
	}
	if d.Target != "dagger/dagger" || d.Agent != "codex" || d.Effort != "high" || d.Prompt != "fix it" {
		t.Errorf("got %+v", d)
	}
}

// A role mention of the bot (<@&roleID>) must also be stripped, or the leftover
// token becomes the first directive word and Parse errors. Regression for the
// 08:28 drop: the bot is addressed via its managed role, not its user id.
func TestStripMention_BotRoleMention(t *testing.T) {
	stripped := stripMention("<@&ROLE> dagger/dagger\nfix it", "B", map[string]bool{"ROLE": true})
	d, err := command.Parse(stripped)
	if err != nil {
		t.Fatalf("Parse(%q): %v", stripped, err)
	}
	if d.Target != "dagger/dagger" || d.Prompt != "fix it" {
		t.Errorf("got %+v", d)
	}
}
