package session

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/eunomie/quack/internal/askmcp"
)

// askWaitNote is the tool result returned the instant ask_user posts its
// question. The call deliberately does NOT block on the answer: the owner may
// take minutes or hours to reply, and parking the streaming process mid-turn for
// that long would hold the MCP connection open, leave the turn un-interruptible
// (a pending tool call), and lose the question on any daemon restart. Instead the
// agent ends its turn, and the owner's reply arrives later as an ordinary new
// turn it continues from. The note leans hard on "stop and wait" because nothing
// mechanically prevents the agent from proceeding once the call returns.
const askWaitNote = "Your question has been posted to the Discord thread. End your turn now — " +
	"do not take any further action, and do not guess at the answer. The owner may take a while " +
	"to reply (possibly hours). When they do, their answer arrives as a new message in this " +
	"thread and you continue from there. Stop here and wait."

// numberEmojis are the one-tap option reactions, in order. The owner can react
// with one to pick an option, or reply with text to answer free-form.
var numberEmojis = []string{"1️⃣", "2️⃣", "3️⃣", "4️⃣", "5️⃣", "6️⃣", "7️⃣", "8️⃣", "9️⃣", "🔟"}

// pendingAsk marks an in-flight owner question (ask_user). It exists only so a
// number reaction on the question message can be mapped back to its option text;
// the answer itself enters as an ordinary turn, so there is no goroutine to wake.
type pendingAsk struct {
	options []string
	msgID   string // the question message, matched by a number reaction
}

// ResolveAsk is the askmcp.AskFunc: it maps a tool call's session token back to a
// live session, posts the question to that session's thread, and returns
// immediately telling the agent to end its turn. The owner's answer is delivered
// later as a new turn (AnswerAskReaction for a number tap, or a plain reply that
// flows through FeedThread). Unknown token → error (reported as a tool error).
func (s *Service) ResolveAsk(ctx context.Context, token string, q askmcp.Question) (askmcp.Answer, error) {
	s.hmu.Lock()
	ls, ok := s.askByToken[token]
	s.hmu.Unlock()
	if !ok {
		return askmcp.Answer{}, errors.New("no live session for this token")
	}
	s.postOwnerQuestion(ctx, ls, q)
	return askmcp.Answer{Note: askWaitNote}, nil
}

// postOwnerQuestion posts the question to the session's thread (with one-tap
// number reactions) and records it as pending so a number reaction resolves to
// its option text.
func (s *Service) postOwnerQuestion(ctx context.Context, ls *liveSession, q askmcp.Question) {
	options := q.Options
	if len(options) > len(numberEmojis) {
		options = options[:len(numberEmojis)]
	}

	msgID, _ := s.reply.Post(ctx, ls.threadID, formatQuestion(q.Header, q.Text, options))
	for i := range options {
		_ = s.reply.React(ctx, ls.threadID, msgID, numberEmojis[i])
	}

	ls.askMu.Lock()
	ls.pending = &pendingAsk{options: options, msgID: msgID}
	ls.askMu.Unlock()
}

// formatQuestion renders the owner-facing question: optional bold header, the
// question, the numbered options, and a hint on how to answer.
func formatQuestion(header, text string, options []string) string {
	var b strings.Builder
	if header != "" {
		fmt.Fprintf(&b, "**%s**\n", header)
	}
	b.WriteString("❓ " + text)
	for i, opt := range options {
		fmt.Fprintf(&b, "\n%s %s", numberEmojis[i], opt)
	}
	if len(options) > 0 {
		b.WriteString("\n-# react with a number or reply to answer")
	} else {
		b.WriteString("\n-# reply to answer")
	}
	return b.String()
}

// HasPendingAsk reports whether the session is currently waiting on an owner
// answer. The streaming run loop treats a reply as the next turn regardless, so
// this is informational (and used in tests); the pending marker's real job is to
// route a number reaction to its option text.
func (s *Service) HasPendingAsk(threadID string) bool {
	s.hmu.Lock()
	ls, ok := s.sessions[threadID]
	s.hmu.Unlock()
	if !ok {
		return false
	}
	ls.askMu.Lock()
	defer ls.askMu.Unlock()
	return ls.pending != nil
}

// ClearPendingAsk drops any pending question for the thread. Called when the
// owner replies in text: the reply already flows on as the next turn, and
// clearing the marker stops a late number-reaction on the now-answered question
// from re-injecting a duplicate answer. No-op when nothing is pending.
func (s *Service) ClearPendingAsk(threadID string) {
	s.hmu.Lock()
	ls, ok := s.sessions[threadID]
	s.hmu.Unlock()
	if ok {
		ls.cancelPending()
	}
}

// AnswerAskReaction treats a number reaction on the pending question's message as
// the owner's answer: it maps the emoji to an offered option, clears the pending
// marker, and feeds that choice back as a new turn the agent continues from.
// Returns false when the reaction isn't on the pending question or isn't an
// offered option (the bot then ignores it as an ordinary reaction).
func (s *Service) AnswerAskReaction(threadID, messageID, emoji string) bool {
	s.hmu.Lock()
	ls, ok := s.sessions[threadID]
	s.hmu.Unlock()
	if !ok {
		return false
	}
	ls.askMu.Lock()
	p := ls.pending
	ls.askMu.Unlock()
	if p == nil || p.msgID != messageID {
		return false
	}
	idx := emojiIndex(emoji)
	if idx < 0 || idx >= len(p.options) {
		return false
	}
	choice := p.options[idx]
	ls.cancelPending()
	// Feed the choice as a new turn, attributed to the question message (a reaction
	// has no message of its own). The agent's preceding ask_user is the immediately
	// prior context, so the bare option text reads as the owner's answer.
	ls.enqueue(turnReq{channelID: threadID, messageID: messageID, text: choice})
	return true
}

// cancelPending drops any in-flight question marker. Called when the session ends
// or switches agents, and when an answer is delivered.
func (ls *liveSession) cancelPending() {
	ls.askMu.Lock()
	ls.pending = nil
	ls.askMu.Unlock()
}

// emojiIndex maps a number reaction to a zero-based option index, or -1 if it
// isn't one of the option emojis.
func emojiIndex(emoji string) int {
	for i, e := range numberEmojis {
		if e == emoji {
			return i
		}
	}
	return -1
}
