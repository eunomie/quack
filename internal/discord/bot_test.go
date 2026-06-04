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

// The stop reaction is matched by the unicode 🛑 or any custom emoji named
// "stop"; everything else is ignored so unrelated reactions don't kill a run.
func TestIsStopReaction(t *testing.T) {
	cases := []struct {
		name string
		e    discordgo.Emoji
		want bool
	}{
		{"unicode stop", discordgo.Emoji{Name: "🛑"}, true},
		{"custom :stop:", discordgo.Emoji{ID: "640663708065988619", Name: "stop"}, true},
		{"custom :STOP: case-insensitive", discordgo.Emoji{ID: "1", Name: "STOP"}, true},
		{"unrelated unicode", discordgo.Emoji{Name: "👍"}, false},
		{"custom named otherwise", discordgo.Emoji{ID: "2", Name: "thumbsup"}, false},
		{"custom literally named 🛑 is not a unicode match", discordgo.Emoji{ID: "3", Name: "🛑"}, false},
	}
	for _, c := range cases {
		if got := isStopReaction(c.e); got != c.want {
			t.Errorf("%s: isStopReaction(%+v) = %v, want %v", c.name, c.e, got, c.want)
		}
	}
}

// A stop reaction obeys the same user+guild allowlist as a thread feed and
// ignores the channel restriction.
func TestAuthorizedReaction(t *testing.T) {
	open := &Bot{}
	if !open.authorizedReaction("g9", "u9") {
		t.Error("empty allowlist should authorize any reaction")
	}
	b := &Bot{allowed: Allow{UserIDs: []string{"u1"}, GuildIDs: []string{"g1"}, ChannelIDs: []string{"c1"}}}
	if !b.authorizedReaction("g1", "u1") {
		t.Error("user+guild in allowlist should be authorized regardless of channel")
	}
	if b.authorizedReaction("g2", "u1") {
		t.Error("guild not in allowlist should be rejected")
	}
	if b.authorizedReaction("g1", "u2") {
		t.Error("user not in allowlist should be rejected")
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

// threadContext recognizes a thread (forum post or regular thread) by its channel
// type and surfaces the post name + parent id; a normal text channel is not in place.
func TestThreadContext(t *testing.T) {
	forumPost := &discordgo.Channel{
		Type:     discordgo.ChannelTypeGuildPublicThread,
		Name:     "Help with login",
		ParentID: "forum1",
	}
	in, name, parent := threadContext(forumPost)
	if !in || name != "Help with login" || parent != "forum1" {
		t.Errorf("threadContext(forum post) = %v,%q,%q; want true,\"Help with login\",\"forum1\"", in, name, parent)
	}

	textChan := &discordgo.Channel{Type: discordgo.ChannelTypeGuildText, Name: "general"}
	if in, _, _ := threadContext(textChan); in {
		t.Error("a normal text channel is not an in-place thread")
	}

	if in, _, _ := threadContext(nil); in {
		t.Error("an unresolved channel (nil) is not an in-place thread")
	}
}

// An in-thread mention is authorized against the thread's PARENT channel, since a
// thread/post id is never itself in a channel allowlist.
func TestAuthorizedParent(t *testing.T) {
	open := &Bot{}
	if !open.authorizedParent(authMsg("u9", "g9", "post1"), "forum1") {
		t.Error("empty allowlist should authorize any in-thread mention")
	}

	b := &Bot{allowed: Allow{ChannelIDs: []string{"forum1"}}}
	if !b.authorizedParent(authMsg("u", "g", "post1"), "forum1") {
		t.Error("parent channel forum1 is allowlisted, should authorize")
	}
	if b.authorizedParent(authMsg("u", "g", "post1"), "forum2") {
		t.Error("parent channel forum2 not in allowlist should be rejected")
	}
	if b.authorized(authMsg("u", "g", "post1")) {
		t.Error("guard: the post id itself is not in the channel allowlist (proves why parent-based auth is needed)")
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
