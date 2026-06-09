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
