package discord

import (
	"testing"

	"github.com/bwmarrin/discordgo"

	"github.com/eunomie/quack/internal/command"
	"github.com/eunomie/quack/internal/session"
)

func TestResolveRole(t *testing.T) {
	b := &Bot{allowed: Allow{
		OwnerUserIDs: []string{"owner"},
		GuestRoleIDs: []string{"guildrole"},
		GuildIDs:     []string{"g"},
	}}
	// owner id -> owner
	if role, ok := b.resolveRole("owner", "g", "chan", nil); !ok || role != session.RoleOwner {
		t.Fatalf("owner: role=%v ok=%v", role, ok)
	}
	// member holding the guest role, in an allowed guild -> guest
	if role, ok := b.resolveRole("someone", "g", "chan", []string{"guildrole"}); !ok || role != session.RoleGuest {
		t.Fatalf("guest: role=%v ok=%v", role, ok)
	}
	// neither owner nor guest-role -> rejected
	if _, ok := b.resolveRole("nobody", "g", "chan", []string{"other"}); ok {
		t.Fatal("should be rejected")
	}
	// guest role but wrong guild -> rejected
	if _, ok := b.resolveRole("someone", "other", "chan", []string{"guildrole"}); ok {
		t.Fatal("guest outside allowed guild should be rejected")
	}
	// SECURITY: empty owner list must NOT make everyone an owner
	b2 := &Bot{allowed: Allow{GuildIDs: []string{"g"}}}
	if _, ok := b2.resolveRole("anyone", "g", "chan", nil); ok {
		t.Fatal("empty owner list + no guest role must reject")
	}
}

// The channel allowlist now gates the owner too: when allowed_channel_ids is
// set, the bot ignores the owner outside those channels (an empty list still
// means "any channel").
func TestResolveRole_OwnerGatedByChannel(t *testing.T) {
	b := &Bot{allowed: Allow{OwnerUserIDs: []string{"owner"}, ChannelIDs: []string{"c1"}}}
	if role, ok := b.resolveRole("owner", "g", "c1", nil); !ok || role != session.RoleOwner {
		t.Errorf("owner in an allowed channel should authorize, got role=%v ok=%v", role, ok)
	}
	if _, ok := b.resolveRole("owner", "g", "c2", nil); ok {
		t.Error("owner outside the channel allowlist must be rejected")
	}
	// With no channel allowlist, the owner authorizes anywhere.
	any := &Bot{allowed: Allow{OwnerUserIDs: []string{"owner"}}}
	if _, ok := any.resolveRole("owner", "g", "anywhere", nil); !ok {
		t.Error("owner should authorize in any channel when no channel allowlist is set")
	}
}

// ownerSandboxDefault is inert until trusted channels are configured; once set,
// only the trusted channels give the owner an unsandboxed default.
func TestOwnerSandboxDefault(t *testing.T) {
	off := &Bot{allowed: Allow{}}
	if off.ownerSandboxDefault("any") {
		t.Error("no trusted channels => owner default must be unsandboxed (feature inert)")
	}
	b := &Bot{allowed: Allow{TrustedChannelIDs: []string{"trusted"}}}
	if b.ownerSandboxDefault("trusted") {
		t.Error("owner in a trusted channel must default to unsandboxed")
	}
	if !b.ownerSandboxDefault("other") {
		t.Error("owner outside the trusted channels must default to sandboxed")
	}
}

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
// thread/post id is never itself in a channel allowlist. onMessage passes the
// parent as resolveRole's channel for in-thread mentions.
func TestResolveRole_InThreadParentChannel(t *testing.T) {
	// An owner is matched by id regardless of channel.
	owner := &Bot{allowed: Allow{OwnerUserIDs: []string{"u9"}}}
	if role, ok := owner.resolveRole("u9", "g9", "forum1", nil); !ok || role != session.RoleOwner {
		t.Error("owner should authorize regardless of channel")
	}

	// A guest is gated on the parent channel being allowlisted.
	b := &Bot{allowed: Allow{GuestRoleIDs: []string{"grole"}, GuildIDs: []string{"g"}, ChannelIDs: []string{"forum1"}}}
	if role, ok := b.resolveRole("someone", "g", "forum1", []string{"grole"}); !ok || role != session.RoleGuest {
		t.Error("guest in the allowlisted parent channel forum1 should authorize")
	}
	if _, ok := b.resolveRole("someone", "g", "forum2", []string{"grole"}); ok {
		t.Error("guest whose parent channel forum2 isn't allowlisted should be rejected")
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

// threadMsg builds a tracked-thread message: optional reply target and content.
func threadMsg(content string, repliedToBot *bool) *discordgo.MessageCreate {
	m := &discordgo.MessageCreate{Message: &discordgo.Message{Content: content}}
	if repliedToBot != nil {
		m.ReferencedMessage = &discordgo.Message{
			Author: &discordgo.User{ID: "X", Bot: *repliedToBot},
		}
	}
	return m
}

func TestIgnoredInThread(t *testing.T) {
	yes, no := true, false
	cases := []struct {
		name     string
		prefixes []string
		content  string
		reply    *bool // nil = not a reply, &true = reply to bot, &false = reply to human
		want     bool
	}{
		{"plain text feeds", []string{"_ "}, "do the thing", nil, false},
		{"reply to human dropped", []string{"_ "}, "side note", &no, true},
		{"reply to bot feeds", []string{"_ "}, "and now this", &yes, false},
		{"underscore-space prefix dropped", []string{"_ "}, "_ note to self", nil, true},
		{"markdown italic feeds", []string{"_ "}, "_italic_ word", nil, false},
		{"custom prefix respected", []string{"//"}, "// aside", nil, true},
		{"empty prefixes disables prefix match", nil, "_ note", nil, false},
		{"empty prefixes still drops reply to human", nil, "anything", &no, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := &Bot{ignorePrefixes: tc.prefixes}
			if got := b.ignoredInThread(nil, threadMsg(tc.content, tc.reply)); got != tc.want {
				t.Errorf("ignoredInThread = %v, want %v", got, tc.want)
			}
		})
	}
}
