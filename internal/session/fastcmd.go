package session

import (
	"context"
	"strings"
	"time"
)

// FastCommand maps a trigger token (e.g. "/revue") to an argv that quack execs
// directly — bypassing the agent — when the trigger is the first word of a
// message in a tracked session thread. The user's trailing tokens are appended
// to Argv as arguments; the command runs with cwd = the session's workdir.
type FastCommand struct {
	Trigger string
	Argv    []string
}

// Runner execs a command in a directory and returns its combined output. The
// os/exec adapter lives in internal/cmdexec; session depends only on this
// interface so it stays unit-testable with a fake.
type Runner interface {
	Run(ctx context.Context, dir string, argv []string) ([]byte, error)
}

// UseRunner injects the command runner used for fast commands.
func (s *Service) UseRunner(r Runner) { s.runner = r }

// fastCommandTimeout bounds a fast command. revue/open-zed daemonize and return
// within a few seconds; the ceiling just stops a wedged binary from hanging.
const fastCommandTimeout = 30 * time.Second

// matchFastCommand reports whether text's first whitespace-delimited token is a
// configured trigger. On a match it returns the command and the remaining tokens
// as its arguments. A trigger appearing anywhere but first does not match.
func (s *Service) matchFastCommand(text string) (FastCommand, []string, bool) {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return FastCommand{}, nil, false
	}
	for _, fc := range s.cfg.FastCommands {
		if fields[0] == fc.Trigger {
			return fc, fields[1:], true
		}
	}
	return FastCommand{}, nil, false
}

// RunFastCommand intercepts a tracked-thread message that names a fast command,
// running it directly instead of feeding it to the agent. It returns false when
// the message isn't a fast command (caller falls through to FeedThread) or the
// thread isn't a tracked session. The command runs in its own goroutine so the
// Discord gateway handler isn't blocked by a slow binary; the incoming ctx is
// deliberately not propagated to it (it's the handler's context, cancelled the
// moment onMessage returns), so the detached run gets a fresh context bounded by
// its own timeout.
func (s *Service) RunFastCommand(ctx context.Context, threadID, messageID, text string) bool {
	fc, args, ok := s.matchFastCommand(text)
	if !ok {
		return false
	}
	s.hmu.Lock()
	ls, ok := s.sessions[threadID]
	s.hmu.Unlock()
	if !ok {
		return false
	}
	go s.execFastCommand(context.Background(), ls, messageID, fc, args)
	return true
}

// execFastCommand runs one fast command in the session's workdir and reports the
// result to the thread: 👀 while it runs, the command's combined output posted,
// then ✅ on success or ❌ on a non-zero exit / timeout. It touches neither the
// turn queue nor the agent's resume token. On a non-zero exit it shows the
// command's output when there is any, otherwise the error string.
func (s *Service) execFastCommand(ctx context.Context, ls *liveSession, messageID string, fc FastCommand, args []string) {
	_ = s.reply.React(ctx, ls.threadID, messageID, emojiWorking)

	argv := append(append([]string{}, fc.Argv...), args...)
	runCtx, cancel := context.WithTimeout(ctx, fastCommandTimeout)
	defer cancel()
	out, err := s.runner.Run(runCtx, ls.workdir, argv)

	_ = s.reply.Unreact(ctx, ls.threadID, messageID, emojiWorking)

	body := strings.TrimSpace(string(out))
	if body != "" {
		for _, chunk := range splitMessage(body, discordMax) {
			_, _ = s.reply.Post(ctx, ls.threadID, chunk)
		}
	}
	if err != nil {
		if body == "" {
			_, _ = s.reply.Post(ctx, ls.threadID, "❌ "+err.Error())
		}
		_ = s.reply.React(ctx, ls.threadID, messageID, emojiError)
		return
	}
	_ = s.reply.React(ctx, ls.threadID, messageID, emojiDone)
}
