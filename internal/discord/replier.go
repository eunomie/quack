package discord

import (
	"context"

	"github.com/bwmarrin/discordgo"
	"github.com/eunomie/quack/internal/session"
)

type replier struct {
	s *discordgo.Session
}

func (r *replier) OpenThread(ctx context.Context, channelID, messageID, name string, autoArchiveMin int) (string, error) {
	th, err := r.s.MessageThreadStartComplex(channelID, messageID, &discordgo.ThreadStart{
		Name:                name,
		AutoArchiveDuration: autoArchiveMin,
	})
	if err != nil {
		return "", err
	}
	return th.ID, nil
}

func (r *replier) Post(ctx context.Context, channelID, content string) (string, error) {
	return r.post(channelID, content, 0)
}

// PostSilent posts with Discord's suppress-notifications flag set: the message
// still lands in the thread but triggers no push/desktop notification. Used for
// tool activity and progress chrome so only the agent's answers notify.
func (r *replier) PostSilent(ctx context.Context, channelID, content string) (string, error) {
	return r.post(channelID, content, discordgo.MessageFlagsSuppressNotifications)
}

func (r *replier) post(channelID, content string, flags discordgo.MessageFlags) (string, error) {
	m, err := r.s.ChannelMessageSendComplex(channelID, &discordgo.MessageSend{
		Content: content,
		Flags:   flags,
	})
	if err != nil {
		return "", err
	}
	return m.ID, nil
}

func (r *replier) Edit(ctx context.Context, channelID, messageID, content string) error {
	_, err := r.s.ChannelMessageEdit(channelID, messageID, content)
	return err
}

func (r *replier) Delete(ctx context.Context, channelID, messageID string) error {
	return r.s.ChannelMessageDelete(channelID, messageID)
}

func (r *replier) RenameThread(ctx context.Context, threadID, name string) error {
	_, err := r.s.ChannelEdit(threadID, &discordgo.ChannelEdit{Name: name})
	return err
}

func (r *replier) ArchiveThread(ctx context.Context, threadID string) error {
	archived := true
	_, err := r.s.ChannelEdit(threadID, &discordgo.ChannelEdit{Archived: &archived})
	return err
}

func (r *replier) React(ctx context.Context, channelID, messageID, emoji string) error {
	return r.s.MessageReactionAdd(channelID, messageID, emoji)
}

func (r *replier) Unreact(ctx context.Context, channelID, messageID, emoji string) error {
	return r.s.MessageReactionRemove(channelID, messageID, emoji, "@me")
}

// RecentMessages returns up to limit messages before beforeID, oldest-first.
func (r *replier) RecentMessages(ctx context.Context, channelID, beforeID string, limit int) ([]session.Message, error) {
	if limit <= 0 {
		limit = 20
	}
	msgs, err := r.s.ChannelMessages(channelID, limit, beforeID, "", "")
	if err != nil {
		return nil, err
	}
	out := make([]session.Message, 0, len(msgs))
	// discordgo returns newest-first; reverse to chronological order.
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if m.Author == nil {
			continue
		}
		out = append(out, session.Message{Author: m.Author.Username, Content: flattenMessage(m)})
	}
	return out, nil
}
