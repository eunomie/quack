package session

import (
	"context"
	"strings"
	"time"
)

// turnRender renders one turn's event stream into Discord messages. It is shared
// by both the per-turn loop (codex) and the streaming loop (claude) so the two
// paths post identically. All methods run on a single loop goroutine, so the
// state needs no locking.
//
// Within a turn: tool activity collapses into ONE muted subtext line, refreshed
// to the latest tool by delete+repost (never edit — an edited "-#" line wraps its
// "(edited)" marker onto a second line) and throttled under Discord's edit rate
// limit. When the burst ends it is finalized to a one-line summary ("read 2 files
// · ran 3 commands"). Assistant text is held back until the next event reveals
// whether it was narration (a run immediately followed by tool use → muted) or the
// answer (a run with nothing after it → posted normally so it notifies).
type turnRender struct {
	s  *Service
	ls *liveSession

	toolMsgID string
	toolText  string
	lastEdit  time.Time
	tally     toolTally

	pending []string
	posted  bool

	// continuation marks a render the agent drove on its own — a background task
	// it started completed and re-invoked it after its turn ended — rather than
	// one a user message triggered. It holds no idle slot, warrants no "(no text
	// response)" placeholder if it stays silent, and hangs its status off contTr
	// (the most recent turn's trigger, captured when the continuation opened).
	continuation bool
	contTr       turnReq
}

func newTurnRender(s *Service, ls *liveSession) *turnRender {
	return &turnRender{s: s, ls: ls}
}

// finalizeTools replaces the live tool message with the muted burst summary by
// DELETING it and POSTING the summary fresh (not editing — see the type doc).
func (r *turnRender) finalizeTools(ctx context.Context) {
	if r.toolMsgID == "" {
		return
	}
	final := r.toolText
	if sum := r.tally.summary(); sum != "" {
		final = sum
	}
	_ = r.s.reply.Delete(ctx, r.ls.threadID, r.toolMsgID)
	_, _ = r.s.reply.PostSilent(ctx, r.ls.threadID, final)
	r.toolMsgID = ""
	r.toolText = ""
	r.tally = toolTally{}
}

// flushPending posts the buffered text run: as the answer (normal, notifies) when
// asAnswer, otherwise as muted narration (silent subtext).
func (r *turnRender) flushPending(ctx context.Context, asAnswer bool) {
	for _, text := range r.pending {
		if asAnswer {
			for _, chunk := range splitMessage(text, discordMax) {
				_, _ = r.s.reply.Post(ctx, r.ls.threadID, chunk)
			}
			r.posted = true
		} else {
			for _, chunk := range splitMessage(mutedText(text), discordMax) {
				_, _ = r.s.reply.PostSilent(ctx, r.ls.threadID, chunk)
			}
		}
	}
	r.pending = r.pending[:0]
}

// handle applies one in-flight event (text or tool activity). TurnComplete is a
// streaming-only terminal event handled by the loop, not here.
func (r *turnRender) handle(ctx context.Context, label string, isTool bool) {
	if isTool {
		if strings.TrimSpace(label) == "" {
			return
		}
		// The text run that preceded this tool was narration — mute it.
		r.flushPending(ctx, false)
		r.tally.add(label)
		r.toolText = mutedText(toolLine(label))
		if r.toolMsgID != "" && time.Since(r.lastEdit) < toolEditInterval {
			return
		}
		if r.toolMsgID != "" {
			_ = r.s.reply.Delete(ctx, r.ls.threadID, r.toolMsgID)
		}
		r.toolMsgID, _ = r.s.reply.PostSilent(ctx, r.ls.threadID, r.toolText)
		r.lastEdit = time.Now()
		return
	}
	if strings.TrimSpace(label) == "" {
		return
	}
	// Text means the agent stopped using tools and is talking: finalize any open
	// burst, then buffer this block with earlier ones in the same run.
	r.finalizeTools(ctx)
	r.pending = append(r.pending, label)
}

// beginTurnStatus marks a turn as working: the global status on the root message
// (channel view) always, plus a per-turn working reaction on an in-thread
// follow-up (turn 1's trigger IS the root, so it needs no separate marker).
func (s *Service) beginTurnStatus(ctx context.Context, ls *liveSession, tr turnReq, isRoot bool) {
	s.setGlobalStatus(ctx, ls, emojiWorking)
	if !isRoot {
		_ = s.reply.React(ctx, tr.channelID, tr.messageID, emojiWorking)
	}
}

// endTurnDone applies a turn's terminal status: clears the per-turn working
// marker, then posts the answer / error and sets the done/error status. Shared by
// both loops.
func (s *Service) endTurnDone(ctx context.Context, ls *liveSession, tr turnReq, isRoot bool, err error, posted, continuation bool) {
	if !isRoot {
		_ = s.reply.Unreact(ctx, tr.channelID, tr.messageID, emojiWorking)
	}
	if err != nil {
		if !isRoot {
			_ = s.reply.React(ctx, tr.channelID, tr.messageID, emojiError)
		}
		s.setGlobalStatus(ctx, ls, emojiError)
		_, _ = s.reply.Post(ctx, ls.threadID, "error: "+err.Error())
		return
	}
	// A continuation that stayed silent posts nothing: it's a background follow-up
	// the user never prompted, so a "(no text response)" placeholder would be noise.
	if !posted && !continuation {
		_, _ = s.reply.Post(ctx, ls.threadID, answerOrPlaceholder(""))
	}
	if !isRoot {
		_ = s.reply.React(ctx, tr.channelID, tr.messageID, emojiDone)
	}
	s.setGlobalStatus(ctx, ls, emojiDone)
}

// isRootTurn reports whether a turn's trigger message is the session's root
// (triggering) message, which already carries the global status.
func (ls *liveSession) isRootTurn(tr turnReq) bool {
	return tr.channelID == ls.rootChannelID && tr.messageID == ls.rootMessageID
}
