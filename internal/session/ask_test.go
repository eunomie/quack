package session

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/eunomie/quack/internal/askmcp"
)

// newAskSession registers a live session (no turns enqueued) for ask tests and
// returns it plus the replier.
func newAskSession(t *testing.T) (*Service, *liveSession, *fakeReplier) {
	t.Helper()
	d := &fakeDriver{}
	svc, r := newHeadlessService(d)
	ls := svc.newSession(context.Background(), sessionRecord{
		Name: "demo", AgentName: "claude", Workdir: "/wt", ThreadID: "thread-1",
		RootChannelID: "c", RootMessageID: "m1",
	})
	return svc, ls, r
}

func TestAsk_ReactionAnswer(t *testing.T) {
	svc, ls, r := newAskSession(t)

	type res struct {
		a   askmcp.Answer
		err error
	}
	done := make(chan res, 1)
	go func() {
		a, err := svc.resolveAsk(context.Background(), ls.askToken,
			askmcp.Question{Header: "REPL", Text: "Inline or expanded?", Options: []string{"inline", "expanded"}})
		done <- res{a, err}
	}()

	// Wait for the question to be posted and pending.
	waitFor(t, "pending ask", func() bool { return svc.HasPendingAsk("thread-1") })

	// The question message carries the options and number reactions were added.
	if !anyContains(r.posts, "Inline or expanded?") || !anyContains(r.posts, "1️⃣ inline") {
		t.Fatalf("question not posted with options: %v", r.posts)
	}
	if !hasStr(r.reacts, "thread-1|msg-1|1️⃣") || !hasStr(r.reacts, "thread-1|msg-1|2️⃣") {
		t.Fatalf("number reactions not added: %v", r.reacts)
	}

	// Owner taps option 2 on the question message.
	if !svc.AnswerAskReaction("thread-1", "msg-1", "2️⃣") {
		t.Fatalf("reaction answer should be accepted")
	}
	got := <-done
	if got.err != nil || got.a.Choice != "expanded" {
		t.Fatalf("answer = %+v err=%v, want choice=expanded", got.a, got.err)
	}
	if svc.HasPendingAsk("thread-1") {
		t.Errorf("ask should be cleared after answering")
	}
}

func TestAsk_TextAnswer(t *testing.T) {
	svc, ls, _ := newAskSession(t)
	done := make(chan askmcp.Answer, 1)
	go func() {
		a, _ := svc.resolveAsk(context.Background(), ls.askToken, askmcp.Question{Text: "what name?"})
		done <- a
	}()
	waitFor(t, "pending ask", func() bool { return svc.HasPendingAsk("thread-1") })

	if !svc.AnswerAskText("thread-1", "call it widget") {
		t.Fatalf("text answer should be accepted")
	}
	if a := <-done; a.Choice != "call it widget" {
		t.Fatalf("answer = %+v, want free-form text", a)
	}
}

// A number reaction on a different message, or out of range, is not an answer.
func TestAsk_ReactionIgnoredWhenNotMatching(t *testing.T) {
	svc, ls, _ := newAskSession(t)
	go svc.resolveAsk(context.Background(), ls.askToken,
		askmcp.Question{Text: "a or b?", Options: []string{"a", "b"}})
	waitFor(t, "pending ask", func() bool { return svc.HasPendingAsk("thread-1") })

	if svc.AnswerAskReaction("thread-1", "other-msg", "1️⃣") {
		t.Errorf("reaction on a different message must not answer")
	}
	if svc.AnswerAskReaction("thread-1", "msg-1", "3️⃣") {
		t.Errorf("out-of-range option must not answer")
	}
	if !svc.HasPendingAsk("thread-1") {
		t.Errorf("ask should still be pending")
	}
}

func TestAsk_TimeoutFallback(t *testing.T) {
	svc, ls, r := newAskSession(t)
	svc.cfg.AskTimeout = 20 * time.Millisecond

	a, err := svc.resolveAsk(context.Background(), ls.askToken, askmcp.Question{Text: "slow?"})
	if err != nil {
		t.Fatalf("timeout should not be an error: %v", err)
	}
	if a.Choice != "" || !strings.Contains(a.Note, "proceed") {
		t.Fatalf("timeout answer = %+v, want a proceed-on-your-own note", a)
	}
	if !anyContains(r.posts, "proceeding on my own") {
		t.Errorf("timeout should post a notice: %v", r.posts)
	}
}

func TestAsk_UnknownToken(t *testing.T) {
	svc, _, _ := newAskSession(t)
	if _, err := svc.resolveAsk(context.Background(), "bogus", askmcp.Question{Text: "x?"}); err == nil {
		t.Errorf("unknown token should error")
	}
}

func TestAsk_StopAbandonsPending(t *testing.T) {
	svc, ls, _ := newAskSession(t)
	done := make(chan error, 1)
	go func() {
		_, err := svc.resolveAsk(context.Background(), ls.askToken, askmcp.Question{Text: "wait?"})
		done <- err
	}()
	waitFor(t, "pending ask", func() bool { return svc.HasPendingAsk("thread-1") })

	svc.StopThread(context.Background(), "thread-1")
	select {
	case err := <-done:
		if err == nil {
			t.Errorf("a stopped session should abandon the question with an error")
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("ask did not return after stop")
	}
}
