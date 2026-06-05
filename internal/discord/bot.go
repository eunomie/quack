// Package discord wires the Discord Gateway to the session orchestrator.
package discord

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"

	"github.com/bwmarrin/discordgo"

	"github.com/eunomie/quack/internal/session"
)

// Bot owns the discordgo session and dispatches commands to the orchestrator.
type Bot struct {
	s       *discordgo.Session
	svc     *session.Service
	allowed Allow

	mu        sync.Mutex
	roleCache map[string]map[string]bool // guildID -> role IDs that address the bot
}

// Allow is the authorization allowlist. Each dimension is a set of permitted
// ids; an empty list means "any" for that dimension (except OwnerUserIDs, where
// empty means no-one is an owner, not everyone — the "any" loophole is
// intentionally absent for the trust-level decision).
type Allow struct {
	UserIDs      []string
	GuildIDs     []string
	ChannelIDs   []string // empty = any channel in the guild
	OwnerUserIDs []string // explicit owners; empty = no owners (NOT "any")
	GuestRoleIDs []string // Discord role ids whose members are guests
}

// New builds a Bot. svcFor returns the orchestrator for a given Replier so the
// Replier can be bound to this discordgo session.
func New(token string, allowed Allow, svcFor func(session.Replier) *session.Service) (*Bot, error) {
	s, err := discordgo.New("Bot " + token)
	if err != nil {
		return nil, err
	}
	s.Identify.Intents = discordgo.IntentsGuilds | discordgo.IntentsGuildMessages | discordgo.IntentMessageContent | discordgo.IntentsGuildMessageReactions

	b := &Bot{s: s, allowed: allowed}
	b.svc = svcFor(&replier{s: s})
	s.AddHandler(b.onMessage)
	s.AddHandler(b.onThreadUpdate)
	s.AddHandler(b.onReaction)
	return b, nil
}

// Run opens the gateway connection and blocks until ctx is cancelled.
func (b *Bot) Run(ctx context.Context) error {
	if err := b.s.Open(); err != nil {
		return err
	}
	defer b.s.Close()
	log.Printf("quack connected")
	<-ctx.Done()
	return nil
}

func (b *Bot) onMessage(s *discordgo.Session, m *discordgo.MessageCreate) {
	if m.Author == nil || m.Author.Bot {
		return
	}
	if b.svc.Tracked(m.ChannelID) {
		role, ok := b.resolveRole(m.Author.ID, m.GuildID, m.ChannelID, memberRoleIDs(m.Member))
		if !ok {
			return
		}
		caller := session.Caller{Role: role, UserID: m.Author.ID}
		content := strings.TrimSpace(m.Content)
		atts := toAttachments(m.Attachments)
		if content == "" && len(atts) == 0 {
			return
		}
		if content == "/stop" || strings.HasPrefix(content, "/stop ") {
			b.svc.StopThread(context.Background(), m.ChannelID, caller)
			return
		}
		if content == "/attach" || strings.HasPrefix(content, "/attach ") {
			// Promotion hands a session a host tmux outside any sandbox jail, so it
			// is owner-only — a guest must never trigger it, even on an owner's session.
			if role == session.RoleGuest {
				return
			}
			b.svc.PromoteThread(context.Background(), m.ChannelID)
			return
		}
		// Fast path: a configured trigger (e.g. /revue) runs its binary directly in
		// the session's workdir, skipping the agent. Like /stop and /attach it's an
		// explicit command, so it takes precedence over treating the text as an
		// ask_user answer. Falls through when the message isn't a fast command.
		if b.svc.RunFastCommand(context.Background(), m.ChannelID, m.ID, content) {
			return
		}
		// While the agent is blocked on an ask_user question, a text reply is the
		// answer, not a new turn. Empty content (an attachment-only message) falls
		// through to FeedThread so it can still interject.
		if content != "" && b.svc.AnswerAskText(m.ChannelID, content) {
			return
		}
		b.svc.FeedThread(context.Background(), m.ChannelID, m.ChannelID, m.ID, content, atts, caller)
		return
	}

	if s.State == nil || s.State.User == nil {
		return
	}
	botID := s.State.User.ID
	var botRoles map[string]bool
	if len(m.MentionRoles) > 0 { // only pay for role resolution when it can matter
		botRoles = b.botRoleIDs(s, m.GuildID)
	}
	if !mentionsBot(m, botID, botRoles) {
		return
	}
	// A mention inside an existing thread (commonly a forum post) runs in place:
	// quack drives that thread rather than opening a new one. Resolve the channel
	// to detect it and to authorize against the thread's parent — a thread/post id
	// is never itself in a channel allowlist.
	ch := resolveChannel(s, m.ChannelID)
	inThread, threadName, parentID := threadContext(ch)
	// Authorize + resolve the user's role (owner vs sandboxed guest). When the
	// mention is in a thread, the channel allowlist is gated on the parent.
	authChannel := m.ChannelID
	if inThread {
		authChannel = parentID
	}
	role, ok := b.resolveRole(m.Author.ID, m.GuildID, authChannel, memberRoleIDs(m.Member))
	if !ok {
		_, _ = s.ChannelMessageSend(m.ChannelID, "🦆 not authorized")
		return
	}

	content := stripMention(m.Content, botID, botRoles)
	created := m.Timestamp.Format("2006-01-02T15:04:05Z07:00")

	origin := session.Origin{
		GuildID:   m.GuildID,
		ChannelID: m.ChannelID,
		MessageID: m.ID,
		AuthorID:  m.Author.ID,
		Author:    m.Author.Username,
		CreatedAt: created,
	}
	if ref := referencedMessage(s, m); ref != nil && ref.Author != nil {
		origin.RepliedToID = ref.ID
		origin.RepliedToAuthor = ref.Author.Username
		origin.RepliedToContent = flattenMessage(ref)
	}

	req := session.Request{
		Content:     content,
		Attachments: toAttachments(m.Attachments),
		Origin:      origin,
		InThread:    inThread,
		ThreadName:  threadName,
		Role:        role,
	}
	go b.svc.Handle(context.Background(), req)
}

// referencedMessage resolves the message a triggering message replies to. The
// gateway usually inlines it as ReferencedMessage; when it only carries the
// MessageReference (e.g. the referenced message wasn't cached), fall back to a
// REST fetch. Returns nil when the message isn't a reply or can't be resolved.
func referencedMessage(s *discordgo.Session, m *discordgo.MessageCreate) *discordgo.Message {
	if m.ReferencedMessage != nil {
		return m.ReferencedMessage
	}
	ref := m.MessageReference
	if ref == nil || ref.MessageID == "" {
		return nil
	}
	chID := ref.ChannelID
	if chID == "" {
		chID = m.ChannelID
	}
	msg, err := s.ChannelMessage(chID, ref.MessageID)
	if err != nil {
		return nil
	}
	return msg
}

// stopEmoji is the unicode reaction that halts a running session. It is also
// quack's own "stopped" status marker (emojiStopped), so the glyph the bot
// stamps when a session ends is the same one a user stamps to end it.
const stopEmoji = "🛑"

// onReaction halts a running session when an authorized user stamps a stop
// reaction on one of its messages — a faster, more visceral interrupt than
// typing /stop, for when the agent starts doing something it shouldn't. The
// bot's own status reactions are ignored so its 🛑 stopped-marker can't loop.
func (b *Bot) onReaction(s *discordgo.Session, r *discordgo.MessageReactionAdd) {
	if s.State == nil || s.State.User == nil || r.UserID == s.State.User.ID {
		return
	}
	// Resolve the reactor's trust level so the own-session guard applies: a guest
	// may stop only its own session, an owner any. r.Member carries the reactor's
	// roles on guild reactions; if absent, fall back to a member lookup.
	role, ok := b.resolveRole(r.UserID, r.GuildID, r.ChannelID, reactorRoleIDs(s, r))
	if !ok {
		return
	}
	caller := session.Caller{Role: role, UserID: r.UserID}
	if isStopReaction(r.Emoji) {
		b.svc.StopByMessage(context.Background(), r.ChannelID, r.MessageID, caller)
		return
	}
	// A number reaction on a pending ask_user question is the owner's answer. A
	// reaction always lands inside the thread, whose id is the session key.
	b.svc.AnswerAskReaction(r.ChannelID, r.MessageID, r.Emoji.Name)
}

// reactorRoleIDs returns the reacting user's guild role ids. The reaction event
// usually inlines them on r.Member; when it doesn't (e.g. an uncached member),
// fall back to a state/REST member lookup — mirroring resolveBotRoles. Returns
// nil on any failure, leaving role resolution to reject if a guest role was
// required.
func reactorRoleIDs(s *discordgo.Session, r *discordgo.MessageReactionAdd) []string {
	if r.Member != nil {
		return r.Member.Roles
	}
	if r.GuildID == "" {
		return nil
	}
	member, err := s.State.Member(r.GuildID, r.UserID)
	if err != nil {
		if member, err = s.GuildMember(r.GuildID, r.UserID); err != nil {
			return nil
		}
	}
	return member.Roles
}

// isStopReaction reports whether a reaction should halt a session: the unicode
// 🛑, or any custom guild emoji named "stop" (e.g. a server's own <:stop:>).
// Discord delivers a unicode emoji with an empty ID and the glyph as its name,
// and a custom emoji with a snowflake ID and its short name — so matching the
// name covers a custom :stop: in any guild without per-guild configuration.
func isStopReaction(e discordgo.Emoji) bool {
	if e.ID != "" {
		return strings.EqualFold(e.Name, "stop")
	}
	return e.Name == stopEmoji
}

func (b *Bot) onThreadUpdate(s *discordgo.Session, t *discordgo.ThreadUpdate) {
	if t.Channel == nil || t.ThreadMetadata == nil || !t.ThreadMetadata.Archived {
		return
	}
	if b.svc.Tracked(t.ID) {
		// Archiving is an unconditional stop, not a user action against a specific
		// session, so it runs as an owner caller and bypasses the own-session guard.
		b.svc.StopThread(context.Background(), t.ID, session.Caller{Role: session.RoleOwner})
	}
}

// resolveRole decides a user's trust level. Owners (explicit id match) get full
// access. Otherwise a member holding a configured guest role, inside an allowed
// guild+channel, is a guest. ok=false => the request is rejected.
func (b *Bot) resolveRole(userID, guildID, channelID string, memberRoles []string) (session.Role, bool) {
	for _, id := range b.allowed.OwnerUserIDs {
		if id == userID {
			return session.RoleOwner, true
		}
	}
	if !allows(b.allowed.GuildIDs, guildID) || !allows(b.allowed.ChannelIDs, channelID) {
		return 0, false
	}
	for _, want := range b.allowed.GuestRoleIDs {
		for _, have := range memberRoles {
			if want == have {
				return session.RoleGuest, true
			}
		}
	}
	return 0, false
}

// memberRoleIDs returns the author's guild role ids (nil-safe).
func memberRoleIDs(m *discordgo.Member) []string {
	if m == nil {
		return nil
	}
	return m.Roles
}

// resolveChannel returns the channel for channelID, preferring the gateway state
// cache and falling back to a REST fetch. Returns nil when it can't be resolved —
// the caller treats that as "not a thread", so the normal open-a-thread path runs.
func resolveChannel(s *discordgo.Session, channelID string) *discordgo.Channel {
	if s.State != nil {
		if ch, err := s.State.Channel(channelID); err == nil && ch != nil {
			return ch
		}
	}
	ch, err := s.Channel(channelID)
	if err != nil {
		return nil
	}
	return ch
}

// threadContext reports whether a mention's channel is an existing thread — a
// forum post or a regular thread — that quack should run in place rather than
// opening a sub-thread (Discord can't nest threads). When it is, it returns the
// thread's display name and its parent channel id; the parent is what a channel
// allowlist is checked against, since the thread id itself is never listed.
func threadContext(ch *discordgo.Channel) (inThread bool, name, parentID string) {
	if ch == nil || !ch.IsThread() {
		return false, "", ""
	}
	return true, ch.Name, ch.ParentID
}

// allows reports whether id passes an allowlist dimension: an empty list
// permits any id, otherwise id must be a member.
func allows(list []string, id string) bool {
	if len(list) == 0 {
		return true
	}
	for _, x := range list {
		if x == id {
			return true
		}
	}
	return false
}

// mentionsBot reports whether m addresses the bot: a direct user mention of
// botID, or a role mention of any role in botRoles. Discord routes a user
// mention (<@id>) into m.Mentions and a role mention (<@&id>) into
// m.MentionRoles, and its autocomplete offers the bot's managed role under the
// same "@quack" label as the bot user — so both must be honoured.
func mentionsBot(m *discordgo.MessageCreate, botID string, botRoles map[string]bool) bool {
	for _, u := range m.Mentions {
		if u.ID == botID {
			return true
		}
	}
	for _, roleID := range m.MentionRoles {
		if botRoles[roleID] {
			return true
		}
	}
	return false
}

// botRoleIDs returns the set of role IDs in guildID that address the bot — its
// managed integration role(s). It is resolved once per guild and cached;
// resolution failures are logged and not cached, so a later message retries.
func (b *Bot) botRoleIDs(s *discordgo.Session, guildID string) map[string]bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.roleCache == nil {
		b.roleCache = map[string]map[string]bool{}
	}
	if set, ok := b.roleCache[guildID]; ok {
		return set
	}
	set, err := resolveBotRoles(s, guildID, s.State.User.ID)
	if err != nil {
		log.Printf("quack: resolve bot roles for guild %s: %v", guildID, err)
		return map[string]bool{}
	}
	b.roleCache[guildID] = set
	return set
}

// resolveBotRoles intersects the bot member's assigned roles with the guild's
// managed roles, yielding exactly the integration role Discord created for the
// bot (the one "@bot" autocompletes to).
func resolveBotRoles(s *discordgo.Session, guildID, botID string) (map[string]bool, error) {
	member, err := s.State.Member(guildID, botID)
	if err != nil {
		if member, err = s.GuildMember(guildID, botID); err != nil {
			return nil, fmt.Errorf("bot member: %w", err)
		}
	}
	roles, err := s.GuildRoles(guildID)
	if err != nil {
		return nil, fmt.Errorf("guild roles: %w", err)
	}
	managed := map[string]bool{}
	for _, r := range roles {
		if r.Managed {
			managed[r.ID] = true
		}
	}
	set := map[string]bool{}
	for _, id := range member.Roles {
		if managed[id] {
			set[id] = true
		}
	}
	return set, nil
}

// toAttachments maps Discord's attachment list to the session's representation,
// skipping any with no URL to download.
func toAttachments(in []*discordgo.MessageAttachment) []session.Attachment {
	if len(in) == 0 {
		return nil
	}
	out := make([]session.Attachment, 0, len(in))
	for _, a := range in {
		if a == nil || a.URL == "" {
			continue
		}
		out = append(out, session.Attachment{
			Filename:    a.Filename,
			URL:         a.URL,
			ContentType: a.ContentType,
		})
	}
	return out
}

func stripMention(content, botID string, botRoles map[string]bool) string {
	content = strings.ReplaceAll(content, "<@"+botID+">", "")
	content = strings.ReplaceAll(content, "<@!"+botID+">", "")
	// A role mention (<@&roleID>) addresses the bot too; strip the bot's own
	// role(s) so the leftover token doesn't become the first directive word.
	for roleID := range botRoles {
		content = strings.ReplaceAll(content, "<@&"+roleID+">", "")
	}
	// Trim only spaces/tabs, never newlines: the first newline separates the
	// (possibly empty) directive line from the prompt, and command.Parse relies
	// on it. TrimSpace here would merge an empty directive line into the prompt.
	return strings.Trim(content, " \t")
}
