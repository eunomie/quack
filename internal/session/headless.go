package session

import (
	"context"
	"sync"
	"time"

	"github.com/eunomie/quack/internal/agentproc"
)

// toolEditInterval throttles in-place edits of the per-turn tool message so a
// fast tool stream stays well under Discord's edit rate limit (~5/5s per channel).
const toolEditInterval = time.Second

// turnReq is one queued turn plus the user message to react on for status.
type turnReq struct {
	channelID string
	messageID string
	text      string
}

// inPlaceOpts carries the extras for a session that runs inside a user-owned
// thread (a forum post): titleBase is the post's own name, used verbatim as the
// Discord title (no owner/repo label); inPlace leaves the thread open on stop
// instead of archiving it. The zero value is an ordinary auto-created thread.
type inPlaceOpts struct {
	inPlace   bool
	titleBase string
}

type liveSession struct {
	driver     agentproc.Driver
	agentName  string
	workdir    string
	effort     string
	name       string
	label      string // workspace label shown in the thread title (owner/repo or dir)
	titleBase  string // verbatim Discord title (the post name); empty => name+label
	inPlace    bool   // session runs in a user-owned thread; don't archive on stop
	threadID   string
	askToken   string // routes ask_user MCP calls back to this session
	sessionRef string // guarded by mu (read by PromoteThread from another goroutine)

	// pending is the in-flight owner question (ask_user), if any. It is set by the
	// MCP handler goroutine and resolved by an owner reaction/reply, so it is
	// guarded by askMu (separate from mu).
	askMu   sync.Mutex
	pending *pendingAsk

	// Root (triggering) message + the status emoji currently shown on it. The
	// global status tracks the latest turn so the channel view stays current.
	// Touched only by the single runLoop goroutine.
	rootChannelID   string
	rootMessageID   string
	lastGlobalEmoji string

	// sess is the live streaming process for a StreamDriver (claude). It is owned
	// by the stream loop goroutine — opened lazily on the first turn and reopened
	// (resuming by ref) if the process dies. nil for the per-turn (codex) path.
	sess agentproc.Session

	queue  chan turnReq
	done   chan struct{}
	stop   chan struct{}
	cancel context.CancelFunc
	title  *titleUpdater
	idle   sync.WaitGroup
	mu     sync.Mutex
	closed bool
}

// UseDrivers registers the headless agent drivers, keyed by agent name.
func (s *Service) UseDrivers(d map[string]agentproc.Driver) { s.drivers = d }

// UseHistory supplies the Discord history reader for the fluent infer step.
func (s *Service) UseHistory(h History) { s.history = h }

func (s *Service) startHeadless(ctx context.Context, agentName, threadID, workdir, effort, name, label string, first turnReq, opts ...inPlaceOpts) {
	var ip inPlaceOpts
	if len(opts) > 0 {
		ip = opts[0]
	}
	ls := s.newSession(ctx, sessionRecord{
		Name:          name,
		Label:         label,
		TitleBase:     ip.titleBase,
		InPlace:       ip.inPlace,
		AgentName:     agentName,
		Workdir:       workdir,
		Effort:        effort,
		ThreadID:      threadID,
		RootChannelID: first.channelID,
		RootMessageID: first.messageID,
	})
	// Persist immediately (with an empty ref) so a restart before the first turn
	// completes still keeps the thread tracked; the ref is filled in per turn.
	s.persistRecord(ls.record())
	ls.enqueue(first)
}

// FeedThread enqueues a thread message as the next turn (channelID/messageID are
// the user's message, for status reactions). Any attachments are mirrored to the
// session state dir and referenced in the turn text. Returns false if not a
// tracked thread.
func (s *Service) FeedThread(ctx context.Context, threadID, channelID, messageID, text string, atts []Attachment) bool {
	s.hmu.Lock()
	ls, ok := s.sessions[threadID]
	s.hmu.Unlock()
	if !ok {
		return false
	}
	if block := s.saveAttachments(ctx, ls.name, atts); block != "" {
		if text == "" {
			text = block
		} else {
			text += "\n\n" + block
		}
	}
	return ls.enqueue(turnReq{channelID: channelID, messageID: messageID, text: text})
}

// StopThread ends a tracked session. Returns false if not tracked.
func (s *Service) StopThread(ctx context.Context, threadID string) bool {
	s.hmu.Lock()
	ls, ok := s.sessions[threadID]
	if ok {
		delete(s.sessions, threadID)
		delete(s.askByToken, ls.askToken)
	}
	s.hmu.Unlock()
	if !ok {
		return false
	}
	// Abandon any in-flight question so its MCP call returns instead of hanging.
	ls.cancelPending()
	ls.close()
	// Wait for the run loop to fully exit before touching the root-message status:
	// lastGlobalEmoji is otherwise owned by the runLoop goroutine, and the wait is
	// bounded (close() cancels any in-flight turn, killing the agent child).
	<-ls.done
	s.removeRecord(ls.name)
	// Mark the session stopped on the root (triggering) message so the channel view
	// makes clear it's no longer running, replacing whatever status it last carried.
	s.markGlobalStopped(ctx, ls)
	_, _ = s.reply.Post(ctx, threadID, "session stopped")
	// Close an auto-created thread now the session is gone (it's already removed
	// from the tracking map, so the archive event no-ops in onThreadUpdate). An
	// in-place thread is the user's own (a forum post) — leave it open.
	if !ls.inPlace {
		_ = s.reply.ArchiveThread(ctx, threadID)
	}
	return true
}

// StopByMessage stops the session a reaction landed on, identified by the
// reacted message. A reaction inside the thread matches by channel (the thread
// id is the session key); a reaction on the original triggering message in the
// parent channel matches by its recorded root channel+message. Returns false if
// no session matches. Lets a stop reaction halt a run from either surface — the
// thread where the agent streams, or the channel view where the root message
// carries the status emoji.
func (s *Service) StopByMessage(ctx context.Context, channelID, messageID string) bool {
	s.hmu.Lock()
	threadID := ""
	if _, ok := s.sessions[channelID]; ok {
		threadID = channelID
	} else {
		for id, ls := range s.sessions {
			// rootChannelID/rootMessageID are set once at session creation and
			// never mutated, so reading them under hmu is safe.
			if ls.rootChannelID == channelID && ls.rootMessageID == messageID {
				threadID = id
				break
			}
		}
	}
	s.hmu.Unlock()
	if threadID == "" {
		return false
	}
	return s.StopThread(ctx, threadID)
}

// PromoteThread converts a headless session into an attachable tmux session,
// resuming the same agent session so the conversation continues. Returns false
// if the thread isn't a tracked headless session.
func (s *Service) PromoteThread(ctx context.Context, threadID string) bool {
	s.hmu.Lock()
	ls, ok := s.sessions[threadID]
	s.hmu.Unlock()
	if !ok {
		return false
	}

	ref := ls.ref()
	if ref == "" {
		_, _ = s.reply.Post(ctx, threadID, "⏳ not ready to attach yet — wait for the first response, then try again")
		return true
	}
	argv := s.cfg.Agents[ls.agentName].ResumeArgv(ref)
	if argv == nil {
		_, _ = s.reply.Post(ctx, threadID, "❌ promotion not supported for agent: "+ls.agentName)
		return true
	}

	// Hand off: stop the headless loop, then resume the session interactively in tmux.
	s.hmu.Lock()
	delete(s.sessions, threadID)
	delete(s.askByToken, ls.askToken)
	s.hmu.Unlock()
	ls.cancelPending()
	ls.close()
	// The agent session now lives in tmux; drop the headless record so a restart
	// doesn't try to resume it back into a headless thread.
	s.removeRecord(ls.name)

	name := "quack/" + ls.name
	if err := s.tmux.NewSession(ctx, NewSessionOpts{Name: name, Dir: ls.workdir, Argv: argv}); err != nil {
		_, _ = s.reply.Post(ctx, threadID, "❌ promote failed: "+err.Error())
		return true
	}
	_, _ = s.reply.Post(ctx, threadID, "🖥️ promoted to a local session — attach with:\n`tmux attach -t "+name+"`")
	return true
}

func (s *Service) Tracked(threadID string) bool {
	s.hmu.Lock()
	defer s.hmu.Unlock()
	_, ok := s.sessions[threadID]
	return ok
}

func (s *Service) waitIdle(threadID string) {
	s.hmu.Lock()
	ls, ok := s.sessions[threadID]
	s.hmu.Unlock()
	if ok {
		ls.idle.Wait()
	}
}

func (ls *liveSession) enqueue(tr turnReq) bool {
	ls.mu.Lock()
	if ls.closed {
		ls.mu.Unlock()
		return false
	}
	ls.idle.Add(1)
	ls.mu.Unlock()
	select {
	case ls.queue <- tr:
		return true
	case <-ls.done:
		ls.idle.Done()
		return false
	}
}

func (ls *liveSession) ref() string {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	return ls.sessionRef
}

func (ls *liveSession) setRef(r string) {
	ls.mu.Lock()
	ls.sessionRef = r
	ls.mu.Unlock()
}

func (ls *liveSession) close() {
	ls.mu.Lock()
	first := !ls.closed
	if first {
		ls.closed = true
		ls.cancel()
		close(ls.stop)
	}
	ls.mu.Unlock()
	if first {
		ls.title.stop()
	}
}

func (s *Service) runLoop(ctx context.Context, ls *liveSession) {
	defer close(ls.done)
	for {
		select {
		case tr := <-ls.queue:
			s.runTurn(ctx, ls, tr)
			ls.idle.Done()
		case <-ls.stop:
			return
		}
	}
}

// setGlobalStatus reflects the session's current status on the thread's root
// (triggering) message so the channel view tracks the latest turn, replacing the
// previously-shown status emoji. Called only from the single runLoop goroutine.
func (s *Service) setGlobalStatus(ctx context.Context, ls *liveSession, emoji string) {
	prev := ls.lastGlobalEmoji
	if prev == emoji {
		return
	}
	if prev != "" {
		_ = s.reply.Unreact(ctx, ls.rootChannelID, ls.rootMessageID, prev)
	}
	_ = s.reply.React(ctx, ls.rootChannelID, ls.rootMessageID, emoji)
	ls.lastGlobalEmoji = emoji
	ls.title.set(emoji)
}

// markGlobalStopped replaces the root message's current status reaction with the
// stopped marker, so a stopped session reads as stopped (not done/working) in the
// channel view. Safe only after the run loop has exited (see StopThread), since
// lastGlobalEmoji is otherwise owned by the runLoop goroutine.
func (s *Service) markGlobalStopped(ctx context.Context, ls *liveSession) {
	if ls.lastGlobalEmoji == emojiStopped {
		return
	}
	if ls.lastGlobalEmoji != "" {
		_ = s.reply.Unreact(ctx, ls.rootChannelID, ls.rootMessageID, ls.lastGlobalEmoji)
	}
	_ = s.reply.React(ctx, ls.rootChannelID, ls.rootMessageID, emojiStopped)
	ls.lastGlobalEmoji = emojiStopped
}

// clearGlobalWorking removes the working marker from the root message when a turn
// is cancelled mid-flight, without claiming success or failure.
func (s *Service) clearGlobalWorking(ctx context.Context, ls *liveSession) {
	if ls.lastGlobalEmoji == emojiWorking {
		_ = s.reply.Unreact(ctx, ls.rootChannelID, ls.rootMessageID, emojiWorking)
		ls.lastGlobalEmoji = ""
	}
}

// runTurn runs one per-turn (codex) turn: it tracks the latest status globally on
// the root message (visible in the channel — turn 1's trigger IS the root, so it
// needs no separate per-turn marker; in-thread follow-ups get their own), renders
// the event stream, and applies the terminal status.
func (s *Service) runTurn(ctx context.Context, ls *liveSession, tr turnReq) {
	isRoot := ls.isRootTurn(tr)
	s.beginTurnStatus(ctx, ls, tr, isRoot)

	rend := newTurnRender(s, ls)
	done := ls.driver.RunTurn(ctx, agentproc.Turn{
		SessionRef: ls.ref(),
		Prompt:     tr.text,
		Workdir:    ls.workdir,
		Effort:     ls.effort,
		Name:       ls.name,
	}, func(e agentproc.Event) {
		switch ev := e.(type) {
		case agentproc.AssistantText:
			rend.handle(ctx, ev.Text, false)
		case agentproc.ToolActivity:
			rend.handle(ctx, ev.Label, true)
		}
	})

	// Session was stopped/archived mid-turn: cancelled on purpose, so clear the
	// working marker and stay quiet — StopThread posts the closing note.
	if ctx.Err() != nil {
		if !isRoot {
			_ = s.reply.Unreact(ctx, tr.channelID, tr.messageID, emojiWorking)
		}
		s.clearGlobalWorking(ctx, ls)
		return
	}

	if done.SessionRef != "" {
		ls.setRef(done.SessionRef)
		// Persist the (possibly rotated) resume token so a restart resumes the
		// latest turn, not an earlier one.
		s.persistRecord(ls.record())
	}
	rend.finalizeTools(ctx)      // any trailing tool steps
	rend.flushPending(ctx, true) // the trailing text run, with no tool after it, is the answer
	s.endTurnDone(ctx, ls, tr, isRoot, done.Err, rend.posted)
}
