package session

import (
	"encoding/json"
	"strings"
	"testing"
)

func sampleOrigin() Origin {
	return Origin{
		GuildID: "g1", ChannelID: "c1", ThreadID: "t1",
		MessageID: "m1", ReplyID: "r1",
		AuthorID: "u1", Author: "alice", CreatedAt: "2026-05-31T17:00:00Z",
	}
}

func TestPermalink(t *testing.T) {
	if got := sampleOrigin().Permalink(); got != "https://discord.com/channels/g1/c1/m1" {
		t.Errorf("Permalink = %q", got)
	}
}

func TestPromptHeader(t *testing.T) {
	h := sampleOrigin().PromptHeader()
	for _, want := range []string{"<quack-context>", "channel_id: c1", "thread_id: t1", "reply_message_id: r1", "permalink: https://discord.com/channels/g1/c1/m1", "</quack-context>"} {
		if !strings.Contains(h, want) {
			t.Errorf("header missing %q\n%s", want, h)
		}
	}
}

func TestPromptHeaderRepliedTo(t *testing.T) {
	o := sampleOrigin()
	o.RepliedToID = "m0"
	o.RepliedToAuthor = "bob"
	o.RepliedToContent = "the original feedback text"
	h := o.PromptHeader()
	for _, want := range []string{"<quack-replied-to>", "message_id: m0", "from bob", "the original feedback text", "</quack-replied-to>"} {
		if !strings.Contains(h, want) {
			t.Errorf("header missing %q\n%s", want, h)
		}
	}
}

func TestPromptHeaderNoReply(t *testing.T) {
	if strings.Contains(sampleOrigin().PromptHeader(), "quack-replied-to") {
		t.Error("replied-to block should be absent when no referenced message")
	}
}

func TestEnvVars(t *testing.T) {
	env := sampleOrigin().EnvVars("fix-cache", "/state/sessions/fix-cache/context.json")
	want := map[string]string{
		"QUACK_CHANNEL_ID":       "c1",
		"QUACK_THREAD_ID":        "t1",
		"QUACK_REPLY_MESSAGE_ID": "r1",
		"QUACK_SESSION_NAME":     "fix-cache",
		"QUACK_CONTEXT_FILE":     "/state/sessions/fix-cache/context.json",
	}
	got := map[string]string{}
	for _, e := range env {
		k, v, _ := strings.Cut(e, "=")
		got[k] = v
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("env %s = %q, want %q", k, got[k], v)
		}
	}
}

func TestContextJSON(t *testing.T) {
	b, err := sampleOrigin().ContextJSON("fix-cache")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]string
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m["session_name"] != "fix-cache" || m["permalink"] != "https://discord.com/channels/g1/c1/m1" || m["channel_id"] != "c1" {
		t.Errorf("json = %v", m)
	}
}
