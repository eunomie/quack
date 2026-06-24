package session

import (
	"context"
	"strings"
	"testing"

	"github.com/eunomie/quack/internal/askmcp"
)

// newAskSession registers a live session for ask tests and returns it plus the
// replier. The driver's scripted turns back any answer that gets fed as a turn
// (a number-reaction answer); pass an empty fakeDriver when no turn is expected.
func newAskSession(t *testing.T, d *fakeDriver) (*Service, *liveSession, *fakeReplier) {
	t.Helper()
	svc, r := newHeadlessService(d)
	ls := svc.newSession(context.Background(), sessionRecord{
		Name: "demo", AgentName: "claude", Workdir: "/wt", ThreadID: "thread-1",
		RootChannelID: "c", RootMessageID: "m1",
	})
	return svc, ls, r
}

func TestAsk_PostsAndReturnsImmediately(t *testing.T) {
	svc, ls, r := newAskSession(t, &fakeDriver{})

	a, err := svc.ResolveAsk(context.Background(), ls.askToken,
		askmcp.Question{Header: "REPL", Text: "Inline or expanded?", Options: []string{"inline", "expanded"}})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	// The call does not block: it returns at once with a stop-and-wait note and no
	// choice (the answer comes later as a new turn).
	if a.Choice != "" || !strings.Contains(a.Note, "End your turn") {
		t.Fatalf("answer = %+v, want an empty choice and a wait note", a)
	}
	// The question was posted with its options and number reactions.
	if !anyContains(r.posts, "Inline or expanded?") || !anyContains(r.posts, "1️⃣ inline") {
		t.Fatalf("question not posted with options: %v", r.posts)
	}
	if !hasStr(r.reacts, "thread-1|msg-1|1️⃣") || !hasStr(r.reacts, "thread-1|msg-1|2️⃣") {
		t.Fatalf("number reactions not added: %v", r.reacts)
	}
	if !svc.HasPendingAsk("thread-1") {
		t.Errorf("question should be pending after posting")
	}
}

func TestAsk_ReactionAnswerFeedsTurn(t *testing.T) {
	// A scripted turn backs the answer that gets fed when the owner taps an option.
	d := &fakeDriver{turns: []scripted{{texts: []string{"ok"}, ref: "s"}}}
	svc, ls, _ := newAskSession(t, d)

	if _, err := svc.ResolveAsk(context.Background(), ls.askToken,
		askmcp.Question{Text: "Inline or expanded?", Options: []string{"inline", "expanded"}}); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	// Owner taps option 2 on the question message: it's fed back as a new turn.
	if !svc.AnswerAskReaction("thread-1", "msg-1", "2️⃣") {
		t.Fatalf("reaction answer should be accepted")
	}
	svc.waitIdle("thread-1")
	if len(d.seen) != 1 || d.seen[0].Prompt != "expanded" {
		t.Fatalf("answer turn = %+v, want a turn with prompt %q", d.seen, "expanded")
	}
	if svc.HasPendingAsk("thread-1") {
		t.Errorf("ask should be cleared after answering")
	}
}

// A number reaction on a different message, or out of range, is not an answer and
// leaves the question pending (and feeds no turn).
func TestAsk_ReactionIgnoredWhenNotMatching(t *testing.T) {
	svc, ls, _ := newAskSession(t, &fakeDriver{})
	if _, err := svc.ResolveAsk(context.Background(), ls.askToken,
		askmcp.Question{Text: "a or b?", Options: []string{"a", "b"}}); err != nil {
		t.Fatalf("resolve: %v", err)
	}

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

// A text reply clears the pending marker (the reply itself flows on through
// FeedThread as the next turn, covered by the headless tests).
func TestAsk_TextReplyClearsPending(t *testing.T) {
	svc, ls, _ := newAskSession(t, &fakeDriver{})
	if _, err := svc.ResolveAsk(context.Background(), ls.askToken, askmcp.Question{Text: "what name?"}); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !svc.HasPendingAsk("thread-1") {
		t.Fatalf("question should be pending")
	}
	svc.ClearPendingAsk("thread-1")
	if svc.HasPendingAsk("thread-1") {
		t.Errorf("pending ask should be cleared after a reply")
	}
}

func TestAsk_UnknownToken(t *testing.T) {
	svc, _, _ := newAskSession(t, &fakeDriver{})
	if _, err := svc.ResolveAsk(context.Background(), "bogus", askmcp.Question{Text: "x?"}); err == nil {
		t.Errorf("unknown token should error")
	}
}
