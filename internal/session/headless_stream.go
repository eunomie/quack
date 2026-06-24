package session

import (
	"context"
	"strings"

	"github.com/eunomie/quack/internal/agentproc"
)

// runStreamLoop drives a StreamDriver (claude) session: one persistent process
// serves the whole conversation, so a message posted while the agent is working
// interrupts the in-flight turn and is read next — the owner can steer mid-turn
// instead of waiting for the turn to finish.
//
// inflight is the FIFO of turns whose result hasn't arrived yet: the head is the
// turn currently streaming, and an interjection appends behind it. claude emits
// one result per message in send order, so results pop the head in order. rend
// renders the head turn; it is renewed when the head completes and another turn
// is waiting.
func (s *Service) runStreamLoop(ctx context.Context, ls *liveSession) {
	defer func() {
		if ls.sess != nil {
			_ = ls.sess.Close()
		}
		close(ls.done)
	}()

	sd := ls.driver.(agentproc.StreamDriver)
	var inflight []turnReq
	var rend *turnRender

	for {
		// No process yet and nothing queued: wait for a message before paying to
		// spawn the agent (a rehydrated session sits here until the next message).
		if ls.sess == nil && len(inflight) == 0 {
			select {
			case tr := <-ls.queue:
				if !s.ensureSession(ctx, ls, sd) {
					s.failTurn(ctx, ls, tr, "could not start the agent process")
					ls.idle.Done()
					continue
				}
				s.streamBegin(ctx, ls, &inflight, &rend, tr)
			case <-ls.stop:
				return
			}
			continue
		}

		// A turn is streaming (or the process is alive and idle): take events,
		// accept an interjection, or stop.
		select {
		case tr := <-ls.queue:
			// Cut off whatever is streaming — an in-flight turn or an unprompted
			// background continuation — so the agent reads this message next.
			if rend != nil {
				_ = ls.sess.Interrupt()
			}
			s.streamBegin(ctx, ls, &inflight, &rend, tr)
		case ev, ok := <-ls.sess.Events():
			if !ok {
				s.streamSessionDied(ctx, ls, &inflight, &rend)
				continue
			}
			s.streamEvent(ctx, ls, &inflight, &rend, ev)
		case <-ls.stop:
			return
		}
	}
}

// ensureSession opens the streaming process if it isn't running, resuming the
// prior conversation by ref. Returns false if the process couldn't start.
func (s *Service) ensureSession(ctx context.Context, ls *liveSession, sd agentproc.StreamDriver) bool {
	if ls.sess != nil {
		return true
	}
	sess, err := sd.OpenSession(ctx, agentproc.OpenOpts{
		SessionRef: ls.ref(),
		Workdir:    ls.workdir,
		Effort:     ls.effort,
		Name:       ls.name,
		AskToken:   ls.askToken,
		Launcher:   ls.launcher, // guest sessions: run the streaming claude inside the container
	})
	if err != nil {
		return false
	}
	ls.sess = sess
	return true
}

// streamBegin sends a turn to the live process, marks it working, and appends it
// to the in-flight FIFO. A fresh renderer is created when this is the head turn.
func (s *Service) streamBegin(ctx context.Context, ls *liveSession, inflight *[]turnReq, rend **turnRender, tr turnReq) {
	isRoot := ls.isRootTurn(tr)
	s.beginTurnStatus(ctx, ls, tr, isRoot)
	_ = ls.sess.Send(ls.consumeHandoff(tr.text))
	*inflight = append(*inflight, tr)
	ls.lastTr = tr
	// Open a renderer only when nothing is streaming. If a turn (or a background
	// continuation) is already open, it keeps its renderer until its own (possibly
	// interrupted) TurnComplete; advanceRender then hands this turn a fresh one.
	if *rend == nil {
		*rend = newTurnRender(s, ls)
	}
}

// streamReopen opens a continuation render for output the agent produced with no
// turn in flight: a background task it kicked off completed and the harness
// re-invoked it after its turn had ended. Render the follow-up like any other
// turn instead of dropping it. A continuation holds no idle slot (nothing was
// enqueued for it) and hangs its status off the most recent turn's trigger (the
// root message until a turn has run), captured now so a later interjection that
// updates lastTr can't steal it.
func (s *Service) streamReopen(ctx context.Context, ls *liveSession, rend **turnRender) {
	tr := ls.lastTr
	s.beginTurnStatus(ctx, ls, tr, ls.isRootTurn(tr))
	r := newTurnRender(s, ls)
	r.continuation = true
	r.contTr = tr
	*rend = r
}

// streamEvent renders one event from the live process. AssistantText/ToolActivity
// feed the open renderer — reopening one as a background continuation if the
// agent spoke with no turn in flight; a TurnComplete finalizes the open turn and
// advances to the next in-flight turn.
func (s *Service) streamEvent(ctx context.Context, ls *liveSession, inflight *[]turnReq, rend **turnRender, ev agentproc.Event) {
	switch e := ev.(type) {
	case agentproc.AssistantText:
		if *rend == nil && strings.TrimSpace(e.Text) != "" {
			s.streamReopen(ctx, ls, rend)
		}
		if *rend != nil {
			(*rend).handle(ctx, e.Text, false)
		}
	case agentproc.ToolActivity:
		if *rend == nil && strings.TrimSpace(e.Label) != "" {
			s.streamReopen(ctx, ls, rend)
		}
		if *rend != nil {
			(*rend).handle(ctx, e.Label, true)
		}
	case agentproc.TurnComplete:
		if *rend == nil {
			return // defensive: a result with no open turn to attribute it to
		}
		// A continuation has no inflight entry: attribute it to the trigger it
		// captured when it opened, and don't pop the FIFO or release an idle slot.
		cont := (*rend).continuation
		head := (*rend).contTr
		if !cont {
			head = (*inflight)[0]
		}
		isRoot := ls.isRootTurn(head)

		// Persist the latest resume token so a restart resumes the newest turn.
		if ref := ls.sess.SessionRef(); ref != "" {
			ls.setRef(ref)
			s.persistRecord(ls.record())
		}

		// Stopped mid-turn: cancelled on purpose, stay quiet (StopThread closes up).
		if ctx.Err() != nil {
			if !cont {
				*inflight = (*inflight)[1:]
				ls.idle.Done()
			}
			s.advanceRender(ls, rend, *inflight)
			return
		}

		(*rend).finalizeTools(ctx)
		(*rend).flushPending(ctx, true) // show whatever the turn produced, even if cut off

		if e.Interrupted {
			// Superseded by an interjection: clear this turn's working marker but
			// don't claim done/error — the turn that replaced it owns the status.
			if !isRoot {
				_ = s.reply.Unreact(ctx, head.channelID, head.messageID, emojiWorking)
			}
			if !cont {
				*inflight = (*inflight)[1:]
			}
			// If nothing follows (an interrupt with no successor, e.g. a stray
			// error_during_execution), don't leave the global stuck on working.
			if len(*inflight) == 0 {
				s.clearGlobalWorking(ctx, ls)
			}
		} else {
			s.endTurnDone(ctx, ls, head, isRoot, e.Err, (*rend).posted, cont)
			if !cont {
				*inflight = (*inflight)[1:]
			}
		}
		if !cont {
			ls.idle.Done()
		}
		s.advanceRender(ls, rend, *inflight)
	}
}

// advanceRender renews the renderer for the new head turn, or clears it when no
// turn is in flight.
func (s *Service) advanceRender(ls *liveSession, rend **turnRender, inflight []turnReq) {
	if len(inflight) > 0 {
		*rend = newTurnRender(s, ls)
	} else {
		*rend = nil
	}
}

// streamSessionDied handles the process exiting unexpectedly: finalize and fail
// every in-flight turn, then drop the session so the next message reopens it
// (resuming by ref). On a deliberate stop (ctx cancelled) it stays quiet.
func (s *Service) streamSessionDied(ctx context.Context, ls *liveSession, inflight *[]turnReq, rend **turnRender) {
	// A continuation's background work died with the process: flush whatever it
	// produced, but it owns no inflight entry, so the per-turn loop below won't
	// touch its status — clear it here unless a queued turn will report the error.
	cont := *rend != nil && (*rend).continuation
	if *rend != nil {
		(*rend).finalizeTools(ctx)
		(*rend).flushPending(ctx, true)
	}
	quiet := ctx.Err() != nil
	for i, tr := range *inflight {
		isRoot := ls.isRootTurn(tr)
		if !isRoot {
			_ = s.reply.Unreact(ctx, tr.channelID, tr.messageID, emojiWorking)
		}
		if !quiet && i == 0 {
			if !isRoot {
				_ = s.reply.React(ctx, tr.channelID, tr.messageID, emojiError)
			}
			s.setGlobalStatus(ctx, ls, emojiError)
			_, _ = s.reply.Post(ctx, ls.threadID, "error: the agent process ended unexpectedly")
		}
		ls.idle.Done()
	}
	if quiet || (cont && len(*inflight) == 0) {
		s.clearGlobalWorking(ctx, ls)
	}
	*inflight = (*inflight)[:0]
	*rend = nil
	_ = ls.sess.Close()
	ls.sess = nil
}

// failTurn reports a turn that couldn't run at all (e.g. the process failed to
// start), without ending the session.
func (s *Service) failTurn(ctx context.Context, ls *liveSession, tr turnReq, msg string) {
	isRoot := ls.isRootTurn(tr)
	if !isRoot {
		_ = s.reply.React(ctx, tr.channelID, tr.messageID, emojiError)
	}
	s.setGlobalStatus(ctx, ls, emojiError)
	_, _ = s.reply.Post(ctx, ls.threadID, "error: "+msg)
}
