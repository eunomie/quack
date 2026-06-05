package session

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/eunomie/quack/internal/agentproc"
)

// sessionRecord is the durable state of a headless session, written under
// state_dir/sessions/<name>/session.json so the session survives a restart of
// the quack service. The agent's own conversation history (claude's session
// jsonl, codex's thread) persists independently on disk; SessionRef is the
// opaque token that resumes it. Everything else here is what's needed to
// rebuild the in-memory liveSession and keep posting to the right Discord
// thread.
//
// Interactive (tmux) sessions need no record: they run detached, independent of
// this process, and already outlive a restart.
type sessionRecord struct {
	Name          string `json:"name"`
	Label         string `json:"label"`                // workspace label for the thread title (owner/repo or dir)
	TitleBase     string `json:"title_base,omitempty"` // verbatim Discord title (post name); empty => name+label
	InPlace       bool   `json:"in_place,omitempty"`   // user-owned thread; don't archive on stop
	AgentName     string `json:"agent_name"`
	Workdir       string `json:"workdir"`
	Effort        string `json:"effort"`
	ThreadID      string `json:"thread_id"`
	RootChannelID string `json:"root_channel_id"`
	RootMessageID string `json:"root_message_id"`
	SessionRef    string `json:"session_ref"`
	AskToken      string `json:"ask_token,omitempty"` // routes ask_user MCP calls back to this session
	AuthorID      string `json:"author_id"`           // Discord id of the user who started the session (own-session-only gate)

	// Guest-session sandbox. Role distinguishes guest from owner across a restart;
	// Sandbox holds only non-secret container/volume identifiers (the PAT and
	// other secrets are re-sourced from current GuestPolicy at rehydrate, never
	// persisted here). Both empty/nil for owner sessions.
	Role    Role           `json:"role"`
	Sandbox *SandboxHandle `json:"sandbox,omitempty"`
}

// record snapshots the session's durable state. The non-ref fields are set once
// at construction; SessionRef is guarded by mu (updated after each turn).
func (ls *liveSession) record() sessionRecord {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	return sessionRecord{
		Name:          ls.name,
		Label:         ls.label,
		TitleBase:     ls.titleBase,
		InPlace:       ls.inPlace,
		AgentName:     ls.agentName,
		Workdir:       ls.workdir,
		Effort:        ls.effort,
		ThreadID:      ls.threadID,
		RootChannelID: ls.rootChannelID,
		RootMessageID: ls.rootMessageID,
		SessionRef:    ls.sessionRef,
		AskToken:      ls.askToken,
		AuthorID:      ls.authorID,
		Role:          ls.role,
		Sandbox:       ls.sandbox,
	}
}

func (s *Service) recordPath(name string) string {
	return filepath.Join(s.cfg.StateDir, "sessions", name, "session.json")
}

// persistRecord writes the session's durable state. Best-effort: failing to
// persist costs only resilience across a restart, never the turn itself.
func (s *Service) persistRecord(rec sessionRecord) {
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return
	}
	_ = s.mkdirAll(filepath.Join(s.cfg.StateDir, "sessions", rec.Name), 0o755)
	_ = s.writeFile(s.recordPath(rec.Name), data, 0o644)
}

// removeRecord drops a session's durable state once it ends or is promoted to
// tmux, so a later restart doesn't resurrect it.
func (s *Service) removeRecord(name string) { _ = s.remove(s.recordPath(name)) }

// titleParts chooses what the thread title is built from. An in-place session
// (a forum post) uses the post's own name verbatim with no workspace label, so
// threadTitle renders "<emoji> <post name>". An ordinary session uses the
// workspace label + session name, as before.
func titleParts(rec sessionRecord) (name, label string) {
	if rec.TitleBase != "" {
		return rec.TitleBase, ""
	}
	return rec.Name, rec.Label
}

// newSession builds a liveSession from a record, registers it under its thread,
// and starts its runLoop. It does NOT enqueue a turn: startHeadless adds the
// first turn; a rehydrated session waits for the next Discord message.
func (s *Service) newSession(ctx context.Context, rec sessionRecord) *liveSession {
	titleName, titleLabel := titleParts(rec)
	turnCtx, cancel := context.WithCancel(ctx)
	// The ask token routes ask_user MCP calls back to this session; mint one for a
	// new session, keep the persisted one across a restart.
	askToken := rec.AskToken
	if askToken == "" {
		askToken = s.newToken()
	}
	ls := &liveSession{
		driver:    s.drivers[rec.AgentName],
		agentName: rec.AgentName,
		workdir:   rec.Workdir,
		effort:    rec.Effort,
		name:      rec.Name,
		label:     rec.Label,
		titleBase: rec.TitleBase,
		inPlace:   rec.InPlace,
		threadID:  rec.ThreadID,
		askToken:  askToken,

		sessionRef:    rec.SessionRef,
		rootChannelID: rec.RootChannelID,
		rootMessageID: rec.RootMessageID,
		authorID:      rec.AuthorID,
		role:          rec.Role,

		queue:  make(chan turnReq, 32),
		done:   make(chan struct{}),
		stop:   make(chan struct{}),
		cancel: cancel,
		title:  newTitleUpdater(s.reply, rec.ThreadID, titleName, titleLabel),
	}
	// A guest record carries a sandbox handle: reconstruct the container launcher
	// so the session's turns run inside the box. This is the single seam shared by
	// the fresh-start (startHeadless) and rehydrate paths.
	if rec.Sandbox != nil {
		ls.sandbox = rec.Sandbox
		ls.launcher = s.sandbox.Launcher(rec.Sandbox)
	}
	// Guest sessions run with a tool-restricted driver so host-escaping skills
	// (e.g. open-zed) are blocked. Owner sessions keep the base driver unchanged.
	if rec.Role.IsGuest() {
		ls.driver = s.guestDriver(rec.AgentName)
	}

	s.hmu.Lock()
	if s.sessions == nil {
		s.sessions = map[string]*liveSession{}
	}
	s.sessions[rec.ThreadID] = ls
	if s.askByToken == nil {
		s.askByToken = map[string]*liveSession{}
	}
	s.askByToken[askToken] = ls
	s.hmu.Unlock()

	// A StreamDriver (claude) runs one persistent process the owner can interject
	// into; a plain Driver (codex) keeps the per-turn loop.
	if _, ok := ls.driver.(agentproc.StreamDriver); ok {
		go s.runStreamLoop(turnCtx, ls)
	} else {
		go s.runLoop(turnCtx, ls)
	}
	return ls
}

// Rehydrate restores headless sessions persisted by a previous run so they keep
// working across a restart of the quack service. For each record it rebuilds the
// in-memory session and resumes the agent on the next Discord message in the
// thread — no past turn is replayed, but it posts a random "I'm back" greeting
// so you can see the session survived the restart. Records whose worktree is
// gone or whose agent driver is no longer configured are skipped. Returns the
// number restored.
//
// Call it once at startup, before the Discord gateway opens, so no incoming
// command or thread message races the rebuild.
func (s *Service) Rehydrate(ctx context.Context) int {
	root := filepath.Join(s.cfg.StateDir, "sessions")
	names, err := s.readDir(root)
	if err != nil {
		return 0 // no state dir yet (fresh install) — nothing to restore
	}
	restored := 0
	for _, name := range names {
		data, err := s.readFile(filepath.Join(root, name, "session.json"))
		if err != nil {
			continue // dir without a record (e.g. attachments-only) — skip
		}
		var rec sessionRecord
		if json.Unmarshal(data, &rec) != nil {
			continue
		}
		if rec.ThreadID == "" || rec.Name == "" {
			continue
		}
		if _, ok := s.drivers[rec.AgentName]; !ok {
			continue // agent no longer configured
		}
		if rec.Role.IsGuest() {
			// A guest workdir is an in-container path, so the owner worktree check
			// doesn't apply. Bring the sandbox back instead, re-sourcing secrets from
			// current policy (never from the persisted handle).
			if s.sandbox == nil {
				continue // guest sandbox support not configured; can't restore
			}
			if rec.Sandbox == nil {
				continue // guest record without a handle is unrestorable
			}
			if err := s.sandbox.Reattach(ctx, rec.Sandbox, s.guestReattachSpec(rec)); err != nil {
				continue // sandbox gone (e.g. volume removed) — skip
			}
		} else if !s.git.PathExists(rec.Workdir) {
			continue // owner worktree removed: nothing to resume into
		}
		s.newSession(ctx, rec)
		// Greet the thread so you can tell the session came back alive after the
		// restart. Best-effort: a failed post never blocks the restore.
		_, _ = s.reply.Post(ctx, rec.ThreadID, randomBackMessage())
		restored++
	}
	return restored
}

// readSubdirs lists the immediate subdirectory names under path.
func readSubdirs(path string) ([]string, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	return names, nil
}
