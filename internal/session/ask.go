package session

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/eunomie/quack/internal/askmcp"
)

// defaultAskTimeout bounds how long an ask_user waits for the owner before the
// agent is told to proceed on its own — preserving the "never hang forever"
// property while making the autonomous fallback explicit, not silent.
const defaultAskTimeout = 10 * time.Minute

// askFallbackNote is returned to the agent when the owner doesn't answer in time.
const askFallbackNote = "The owner didn't answer in time — proceed using your best judgement, " +
	"and say which choice you made so they can correct you."

// numberEmojis are the one-tap option reactions, in order. The owner can react
// with one to pick an option, or reply with text to answer free-form.
var numberEmojis = []string{"1️⃣", "2️⃣", "3️⃣", "4️⃣", "5️⃣", "6️⃣", "7️⃣", "8️⃣", "9️⃣", "🔟"}

// pendingAsk is an in-flight owner question. The MCP handler goroutine blocks on
// reply/done while an owner reaction or reply (routed by the bot) resolves it.
type pendingAsk struct {
	options []string
	msgID   string // the question message, matched by a number reaction
	reply   chan string
	done    chan struct{}
	once    sync.Once
}

func (p *pendingAsk) cancel() { p.once.Do(func() { close(p.done) }) }

var errAskAbandoned = errors.New("question abandoned (session ended)")

// ResolveAsk is the askmcp.AskFunc: it maps a tool call's session token back to a
// live session and blocks on the owner's answer. Unknown token → error (reported
// to the agent as a tool error).
func (s *Service) ResolveAsk(ctx context.Context, token string, q askmcp.Question) (askmcp.Answer, error) {
	s.hmu.Lock()
	ls, ok := s.askByToken[token]
	s.hmu.Unlock()
	if !ok {
		return askmcp.Answer{}, errors.New("no live session for this token")
	}
	return s.askOwner(ctx, ls, q)
}

// askOwner posts the question to the session's thread (with one-tap number
// reactions) and blocks until the owner answers, the session ends, the caller's
// context is cancelled, or the timeout elapses.
func (s *Service) askOwner(ctx context.Context, ls *liveSession, q askmcp.Question) (askmcp.Answer, error) {
	options := q.Options
	if len(options) > len(numberEmojis) {
		options = options[:len(numberEmojis)]
	}

	msgID, _ := s.reply.Post(ctx, ls.threadID, formatQuestion(q.Header, q.Text, options))
	for i := range options {
		_ = s.reply.React(ctx, ls.threadID, msgID, numberEmojis[i])
	}

	p := &pendingAsk{
		options: options,
		msgID:   msgID,
		reply:   make(chan string, 1),
		done:    make(chan struct{}),
	}
	ls.askMu.Lock()
	ls.pending = p
	ls.askMu.Unlock()
	defer func() {
		ls.askMu.Lock()
		if ls.pending == p {
			ls.pending = nil
		}
		ls.askMu.Unlock()
	}()

	timeout := s.cfg.AskTimeout
	if timeout <= 0 {
		timeout = defaultAskTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case choice := <-p.reply:
		_, _ = s.reply.PostSilent(ctx, ls.threadID, mutedText("✅ got it: "+choice))
		return askmcp.Answer{Choice: choice}, nil
	case <-p.done:
		return askmcp.Answer{}, errAskAbandoned
	case <-ctx.Done():
		return askmcp.Answer{}, ctx.Err()
	case <-timer.C:
		_, _ = s.reply.PostSilent(ctx, ls.threadID, mutedText("⏳ no answer — proceeding on my own"))
		return askmcp.Answer{Note: askFallbackNote}, nil
	}
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
// answer, so the bot routes the next message/reaction to the answer rather than
// starting a new turn.
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

// AnswerAskText delivers a free-form reply as the answer to a pending question.
// Returns false if there is no pending question (the caller then treats the
// message as a normal turn).
func (s *Service) AnswerAskText(threadID, text string) bool {
	s.hmu.Lock()
	ls, ok := s.sessions[threadID]
	s.hmu.Unlock()
	if !ok {
		return false
	}
	return ls.deliverAsk(text)
}

// AnswerAskReaction delivers a number-reaction choice as the answer, but only
// when it lands on the pending question's message and maps to an offered option.
// Returns false otherwise (the reaction is then handled normally).
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
	return ls.deliverAsk(p.options[idx])
}

// deliverAsk hands a choice to the in-flight question (non-blocking; a second
// answer is dropped). Returns false if no question is pending.
func (ls *liveSession) deliverAsk(choice string) bool {
	ls.askMu.Lock()
	p := ls.pending
	ls.askMu.Unlock()
	if p == nil {
		return false
	}
	select {
	case p.reply <- choice:
		return true
	default:
		return false
	}
}

// cancelPending abandons any in-flight question so its blocked MCP call returns
// instead of hanging when the session ends.
func (ls *liveSession) cancelPending() {
	ls.askMu.Lock()
	p := ls.pending
	ls.askMu.Unlock()
	if p != nil {
		p.cancel()
	}
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
