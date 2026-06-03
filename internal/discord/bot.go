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

// Allow is the authorization allowlist.
type Allow struct {
	UserID    string
	GuildID   string
	ChannelID string // optional ("" = any channel in the guild)
}

// New builds a Bot. svcFor returns the orchestrator for a given Replier so the
// Replier can be bound to this discordgo session.
func New(token string, allowed Allow, svcFor func(session.Replier) *session.Service) (*Bot, error) {
	s, err := discordgo.New("Bot " + token)
	if err != nil {
		return nil, err
	}
	s.Identify.Intents = discordgo.IntentsGuilds | discordgo.IntentsGuildMessages | discordgo.IntentMessageContent

	b := &Bot{s: s, allowed: allowed}
	b.svc = svcFor(&replier{s: s})
	s.AddHandler(b.onMessage)
	s.AddHandler(b.onThreadUpdate)
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
		if !b.authorizedThread(m) {
			return
		}
		content := strings.TrimSpace(m.Content)
		atts := toAttachments(m.Attachments)
		if content == "" && len(atts) == 0 {
			return
		}
		if content == "/stop" || strings.HasPrefix(content, "/stop ") {
			b.svc.StopThread(context.Background(), m.ChannelID)
			return
		}
		if content == "/attach" || strings.HasPrefix(content, "/attach ") {
			b.svc.PromoteThread(context.Background(), m.ChannelID)
			return
		}
		b.svc.FeedThread(context.Background(), m.ChannelID, m.ChannelID, m.ID, content, atts)
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
	if !b.authorized(m) {
		_, _ = s.ChannelMessageSend(m.ChannelID, "🦆 not authorized")
		return
	}

	content := stripMention(m.Content, botID, botRoles)
	created := m.Timestamp.Format("2006-01-02T15:04:05Z07:00")

	req := session.Request{
		Content:     content,
		Attachments: toAttachments(m.Attachments),
		Origin: session.Origin{
			GuildID:   m.GuildID,
			ChannelID: m.ChannelID,
			MessageID: m.ID,
			AuthorID:  m.Author.ID,
			Author:    m.Author.Username,
			CreatedAt: created,
		},
	}
	go b.svc.Handle(context.Background(), req)
}

func (b *Bot) onThreadUpdate(s *discordgo.Session, t *discordgo.ThreadUpdate) {
	if t.Channel == nil || t.ThreadMetadata == nil || !t.ThreadMetadata.Archived {
		return
	}
	if b.svc.Tracked(t.ID) {
		b.svc.StopThread(context.Background(), t.ID)
	}
}

func (b *Bot) authorized(m *discordgo.MessageCreate) bool {
	if b.allowed.UserID != "" && m.Author.ID != b.allowed.UserID {
		return false
	}
	if b.allowed.GuildID != "" && m.GuildID != b.allowed.GuildID {
		return false
	}
	if b.allowed.ChannelID != "" && m.ChannelID != b.allowed.ChannelID {
		return false
	}
	return true
}

func (b *Bot) authorizedThread(m *discordgo.MessageCreate) bool {
	if b.allowed.UserID != "" && m.Author.ID != b.allowed.UserID {
		return false
	}
	if b.allowed.GuildID != "" && m.GuildID != b.allowed.GuildID {
		return false
	}
	return true
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
