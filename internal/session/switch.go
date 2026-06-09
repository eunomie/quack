package session

import (
	"context"
	"strings"
	"time"

	"github.com/eunomie/quack/internal/agentproc"
)

// summaryTimeout bounds the outgoing agent's handoff-summary turn so a wedged
// agent can't hang a switch; on timeout the switch proceeds with whatever
// streamed (possibly empty).
const summaryTimeout = 3 * time.Minute

// handoffPrompt asks the outgoing agent to summarize the session for a different
// agent that has no access to its history.
const handoffPrompt = "You are about to hand this session off to a DIFFERENT coding agent that has no access to your conversation history or memory. Write a thorough handoff so it can continue seamlessly. Cover: the goal of this session, what you've done so far, key decisions and why, approaches you tried and rejected, the current state of the working tree (which files changed and what is committed), and the concrete next steps. Be specific with file paths and identifiers. Output ONLY the handoff text."

// runSummaryTurn runs one resume-by-ref turn on the outgoing driver asking for a
// handoff summary, streams it to the thread (a fresh renderer, so it reads like a
// normal answer), and returns the assistant text. The session's run loop must
// already be stopped: this is a standalone process (claude --resume / a codex
// resume turn), independent of any live ls.sess.
func (s *Service) runSummaryTurn(ctx context.Context, ls *liveSession, ref string) string {
	rend := newTurnRender(s, ls)
	var sb strings.Builder
	done := ls.driver.RunTurn(ctx, agentproc.Turn{
		SessionRef: ref,
		Prompt:     handoffPrompt,
		Workdir:    ls.workdir,
		Effort:     ls.effort,
		Name:       ls.name,
		Launcher:   ls.launcher, // guest sessions: summarize inside the container
	}, func(e agentproc.Event) {
		switch ev := e.(type) {
		case agentproc.AssistantText:
			sb.WriteString(ev.Text)
			rend.handle(ctx, ev.Text, false)
		case agentproc.ToolActivity:
			rend.handle(ctx, ev.Label, true)
		}
	})
	rend.finalizeTools(ctx)
	rend.flushPending(ctx, true)
	if done.Err != nil {
		_, _ = s.reply.Post(ctx, ls.threadID, "⚠️ handoff summary failed: "+done.Err.Error())
	}
	return strings.TrimSpace(sb.String())
}

// wrapHandoff frames a captured summary as the <quack-handoff> block seeded onto
// the new agent's next turn. Empty summary ⇒ empty block (nothing to seed).
func wrapHandoff(fromAgent, summary string) string {
	if strings.TrimSpace(summary) == "" {
		return ""
	}
	return "<quack-handoff>\nA previous agent (" + fromAgent + ") worked on this session and left this handoff for you. You do not share its memory or conversation history; treat the following as your context for continuing the work.\n\n" + summary + "\n</quack-handoff>"
}

// SwitchAgent handles a /<name> switch command in a tracked thread. It returns
// true when the first token is a switch trigger (so the bot does not fall through
// to feed the text to the agent), false when it isn't (fall through). The slow
// work — summarize, tear down, rebuild — runs in a goroutine, like a fast command.
func (s *Service) SwitchAgent(ctx context.Context, threadID, channelID, messageID, text string, atts []Attachment, caller Caller) bool {
	target, prompt, ok := s.matchSwitch(text)
	if !ok {
		return false
	}
	s.hmu.Lock()
	ls, tracked := s.sessions[threadID]
	if !tracked {
		s.hmu.Unlock()
		return false
	}
	// A guest may only switch a session it started; drop (but report handled, so
	// "/codex …" is never fed as a prompt to a session the guest can't touch).
	if !ls.canModify(caller) {
		s.hmu.Unlock()
		return true
	}
	// Claim the switch under hmu so two switch messages racing on the same thread
	// can't both tear down and rebuild it — the loser's rebuilt session would be
	// orphaned (a leaked loop goroutine + agent child). A second switch while one
	// is already in flight is dropped (reported handled, not fed as text).
	if ls.switching {
		s.hmu.Unlock()
		return true
	}
	ls.switching = true
	s.hmu.Unlock()
	// context.Background(): the switch must outlive the caller's request context
	// (it tears down and rebuilds a session that keeps running afterward).
	go s.doSwitch(context.Background(), ls, target, prompt, channelID, messageID, atts, caller)
	return true
}

// clearSwitching releases a switch claim (see SwitchAgent) so a switch that was
// rejected before any teardown leaves the session switchable again.
func (s *Service) clearSwitching(ls *liveSession) {
	s.hmu.Lock()
	ls.switching = false
	s.hmu.Unlock()
}

// doSwitch performs the switch: guard, tear down the old loop, summarize the
// outgoing agent, rebuild the session with the new driver, then seed lazily. All
// guards that can reject the switch run before any teardown, so a rejected switch
// never disturbs the live session.
func (s *Service) doSwitch(ctx context.Context, ls *liveSession, target, prompt, channelID, messageID string, atts []Attachment, caller Caller) {
	if s.drivers[target] == nil {
		_, _ = s.reply.Post(ctx, ls.threadID, "❌ unknown agent: "+target)
		s.clearSwitching(ls) // rejected before teardown: stay switchable
		return
	}
	if target == ls.agentName {
		_, _ = s.reply.Post(ctx, ls.threadID, "already on "+target)
		if strings.TrimSpace(prompt) != "" || len(atts) > 0 {
			s.FeedThread(ctx, ls.threadID, channelID, messageID, prompt, atts, caller)
		}
		s.clearSwitching(ls) // rejected before teardown: stay switchable
		return
	}

	oldRef := ls.ref()
	oldAgent := ls.agentName
	threadID := ls.threadID

	// Tear down the old loop. cancelPending/close cut off any in-flight turn — the
	// user asked to switch now.
	s.hmu.Lock()
	delete(s.sessions, threadID)
	delete(s.askByToken, ls.askToken)
	s.hmu.Unlock()
	ls.cancelPending()
	ls.close()
	<-ls.done

	// Summarize, if the outgoing agent ever produced a resumable ref.
	summary := ""
	if oldRef != "" {
		_, _ = s.reply.Post(ctx, threadID, "🔄 switching to "+target+" — summarizing the handoff…")
		sctx, cancel := context.WithTimeout(ctx, summaryTimeout)
		summary = s.runSummaryTurn(sctx, ls, oldRef)
		cancel()
	}

	// Rebuild in place with the new driver. record() carries label/role/sandbox/
	// askToken forward; newSession picks the right loop type and (for a sandbox)
	// rebuilds the guest launcher + driver.
	rec := ls.record()
	rec.AgentName = target
	rec.SessionRef = ""
	rec.PendingHandoff = wrapHandoff(oldAgent, summary)
	// context.Background(): the rebuilt session must outlive this call (same reason
	// startHeadless/Rehydrate detach from the request context).
	newls := s.newSession(context.Background(), rec)
	s.persistRecord(newls.record())

	// The old ls (with switching=true) is now unreferenced; the rebuilt newls
	// starts switchable, so no claim needs releasing here.
	_, _ = s.reply.Post(ctx, threadID, "🔄 switched to "+target)
	if strings.TrimSpace(prompt) != "" || len(atts) > 0 {
		s.FeedThread(ctx, threadID, channelID, messageID, prompt, atts, caller)
	}
}

// matchSwitch reports whether text's first whitespace-delimited token is a
// switch trigger — "/<name>" for a configured agent that is headless and opted
// in with switchable=true. On a match it returns the agent name and the rest of
// the line (the inline prompt, internal spacing preserved, empty for a bare
// switch). A trigger anywhere but first does not match. /stop, /attach and fast
// commands are matched earlier in the bot, so they take precedence over a
// same-named agent.
func (s *Service) matchSwitch(text string) (agentName, prompt string, ok bool) {
	trimmed := strings.TrimSpace(text)
	fields := strings.Fields(trimmed)
	if len(fields) == 0 {
		return "", "", false
	}
	tok := fields[0]
	if !strings.HasPrefix(tok, "/") {
		return "", "", false
	}
	name := strings.TrimPrefix(tok, "/")
	ag, exists := s.cfg.Agents[name]
	if !exists || !ag.Headless || !ag.Switchable {
		return "", "", false
	}
	return name, strings.TrimSpace(trimmed[len(tok):]), true
}
