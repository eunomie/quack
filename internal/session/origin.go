package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Origin is the captured Discord context for a session.
type Origin struct {
	GuildID   string
	ChannelID string
	ThreadID  string // set after the thread is opened
	MessageID string // the triggering message
	ReplyID   string // quack's ack message (set after posting)
	AuthorID  string
	Author    string
	CreatedAt string // RFC3339
}

// Permalink returns a Discord deep-link to the triggering message.
func (o Origin) Permalink() string {
	return fmt.Sprintf("https://discord.com/channels/%s/%s/%s", o.GuildID, o.ChannelID, o.MessageID)
}

// PromptHeader renders the <quack-context> block prepended to the agent prompt.
func (o Origin) PromptHeader() string {
	return fmt.Sprintf(`<quack-context>
guild_id: %s   channel_id: %s   thread_id: %s
message_id: %s   reply_message_id: %s
author: %s (id %s)
permalink: %s
</quack-context>`,
		o.GuildID, o.ChannelID, o.ThreadID,
		o.MessageID, o.ReplyID,
		o.Author, o.AuthorID,
		o.Permalink())
}

// EnvVars returns the QUACK_* environment for the tmux session.
func (o Origin) EnvVars(sessionName, contextFile string) []string {
	return []string{
		"QUACK_GUILD_ID=" + o.GuildID,
		"QUACK_CHANNEL_ID=" + o.ChannelID,
		"QUACK_THREAD_ID=" + o.ThreadID,
		"QUACK_MESSAGE_ID=" + o.MessageID,
		"QUACK_REPLY_MESSAGE_ID=" + o.ReplyID,
		"QUACK_PERMALINK=" + o.Permalink(),
		"QUACK_CONTEXT_FILE=" + contextFile,
		"QUACK_SESSION_NAME=" + sessionName,
	}
}

// ContextJSON marshals the structured context written to the state dir.
func (o Origin) ContextJSON(sessionName string) ([]byte, error) {
	doc := map[string]string{
		"session_name":     sessionName,
		"guild_id":         o.GuildID,
		"channel_id":       o.ChannelID,
		"thread_id":        o.ThreadID,
		"message_id":       o.MessageID,
		"reply_message_id": o.ReplyID,
		"author_id":        o.AuthorID,
		"author":           o.Author,
		"created_at":       o.CreatedAt,
		"permalink":        o.Permalink(),
	}
	return json.MarshalIndent(doc, "", "  ")
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		return filepath.Join(os.Getenv("HOME"), strings.TrimPrefix(p, "~"))
	}
	return p
}
