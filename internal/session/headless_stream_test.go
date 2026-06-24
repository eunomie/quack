package session

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/eunomie/quack/internal/agentproc"
)

// fakeStreamSession is a scriptable agentproc.Session: the test pushes events via
// emit and inspects what the loop sent/interrupted.
type fakeStreamSession struct {
	events chan agentproc.Event

	mu         sync.Mutex
	sent       []string
	interrupts int
	ref        string
	closed     bool
}

func newFakeStreamSession(ref string) *fakeStreamSession {
	return &fakeStreamSession{events: make(chan agentproc.Event, 64), ref: ref}
}

func (s *fakeStreamSession) Send(text string) error {
	s.mu.Lock()
	s.sent = append(s.sent, text)
	s.mu.Unlock()
	return nil
}
func (s *fakeStreamSession) Interrupt() error {
	s.mu.Lock()
	s.interrupts++
	s.mu.Unlock()
	return nil
}
func (s *fakeStreamSession) Events() <-chan agentproc.Event { return s.events }
func (s *fakeStreamSession) SessionRef() string             { return s.ref }
func (s *fakeStreamSession) Close() error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	return nil
}

func (s *fakeStreamSession) emit(ev agentproc.Event) { s.events <- ev }
func (s *fakeStreamSession) sentCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sent)
}
func (s *fakeStreamSession) interruptCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.interrupts
}

// fakeStreamDriver implements both Driver and StreamDriver; OpenSession hands back
// a pre-built session the test controls and signals on opened.
type fakeStreamDriver struct {
	sess     *fakeStreamSession
	opened   chan struct{}
	openErr  error
	openSeen []agentproc.OpenOpts
}

func newFakeStreamDriver(sess *fakeStreamSession) *fakeStreamDriver {
	return &fakeStreamDriver{sess: sess, opened: make(chan struct{}, 4)}
}

func (d *fakeStreamDriver) OpenSession(ctx context.Context, o agentproc.OpenOpts) (agentproc.Session, error) {
	d.openSeen = append(d.openSeen, o)
	if d.openErr != nil {
		return nil, d.openErr
	}
	d.opened <- struct{}{}
	return d.sess, nil
}

// Driver methods — unused on the stream path but required to satisfy the interface.
func (d *fakeStreamDriver) RunTurn(ctx context.Context, t agentproc.Turn, emit func(agentproc.Event)) agentproc.TurnDone {
	return agentproc.TurnDone{}
}
func (d *fakeStreamDriver) OneShot(ctx context.Context, prompt, effort string) (string, error) {
	return "", nil
}
func (d *fakeStreamDriver) SuggestName(ctx context.Context, prompt string) (string, error) {
	return "", nil
}

func newStreamService(d agentproc.Driver) (*Service, *fakeReplier) {
	g, r, fs := newFakeGit(), newFakeReplier(), newMemFS()
	svc := New(Config{StateDir: "/state"}, g, newFakeTmux(), r)
	svc.drivers = map[string]agentproc.Driver{"claude": d}
	svc.mkdirAll, svc.writeFile, svc.remove = fs.mkdirAll, fs.writeFile, fs.remove
	svc.readDir, svc.readFile = fs.readDir, fs.readFile
	return svc, r
}

// waitFor polls cond until true or the deadline, failing the test on timeout.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestStream_FirstTurnRendersAndPersists(t *testing.T) {
	sess := newFakeStreamSession("sess-1")
	d := newFakeStreamDriver(sess)
	svc, r := newStreamService(d)

	svc.startHeadless(context.Background(), "claude", "thread-1", "/wt", "high", "demo", "",
		RoleOwner, nil, "owner", turnReq{channelID: "c", messageID: "m1", text: "do the thing"})

	<-d.opened
	waitFor(t, "first send", func() bool { return sess.sentCount() == 1 })
	if sess.sent[0] != "do the thing" {
		t.Fatalf("sent = %q", sess.sent[0])
	}
	// A fresh session carries effort/name through OpenOpts (resume token empty).
	if o := d.openSeen[0]; o.SessionRef != "" || o.Effort != "high" || o.Name != "demo" {
		t.Fatalf("open opts = %+v", o)
	}

	sess.emit(agentproc.AssistantText{Text: "All done."})
	sess.emit(agentproc.TurnComplete{})
	svc.waitIdle("thread-1")

	if !anyContains(r.posts, "All done.") {
		t.Fatalf("answer not posted: %v", r.posts)
	}
	// Root message shows working then done.
	if last := lastReactOn(r.reacts, "c|m1"); last != emojiDone {
		t.Fatalf("root status = %q, want done", last)
	}
	// The resume token was persisted.
	if svc.sessions["thread-1"].ref() != "sess-1" {
		t.Fatalf("ref not captured: %q", svc.sessions["thread-1"].ref())
	}
}

// The headline feature: a message sent while a turn is in flight interrupts it and
// is processed as the next turn.
func TestStream_MidTurnInterjection(t *testing.T) {
	sess := newFakeStreamSession("sess-1")
	d := newFakeStreamDriver(sess)
	svc, r := newStreamService(d)

	svc.startHeadless(context.Background(), "claude", "thread-1", "/wt", "", "demo", "",
		RoleOwner, nil, "owner", turnReq{channelID: "c", messageID: "m1", text: "long task"})
	<-d.opened
	waitFor(t, "first send", func() bool { return sess.sentCount() == 1 })

	// Owner interjects mid-turn. The loop must Interrupt the in-flight turn and Send
	// the new message as the next turn.
	if !svc.FeedThread(context.Background(), "thread-1", "thread-1", "m2", "actually, stop and do X", nil, Caller{Role: RoleOwner, UserID: "owner"}) {
		t.Fatalf("feed should report tracked thread")
	}
	waitFor(t, "interrupt + second send", func() bool {
		return sess.interruptCount() == 1 && sess.sentCount() == 2
	})
	if sess.sent[1] != "actually, stop and do X" {
		t.Fatalf("second send = %q", sess.sent[1])
	}

	// Turn 1 ends interrupted; turn 2 then answers.
	sess.emit(agentproc.TurnComplete{Interrupted: true})
	sess.emit(agentproc.AssistantText{Text: "Doing X instead."})
	sess.emit(agentproc.TurnComplete{})
	svc.waitIdle("thread-1")

	if !anyContains(r.posts, "Doing X instead.") {
		t.Fatalf("interjected turn's answer not posted: %v", r.posts)
	}
	// The interrupted turn 1 must NOT be marked done or error — it was superseded.
	if hasStr(r.reacts, "thread-1|m2|"+emojiDone) == false {
		t.Errorf("turn 2 (m2) should be marked done, got %v", r.reacts)
	}
	// Global ends on done (turn 2), not stuck on working.
	if last := lastReactOn(r.reacts, "c|m1"); last != emojiDone {
		t.Fatalf("root status = %q, want done after interjected turn", last)
	}
}

// A background task the agent kicked off completes after its turn ended, so the
// harness re-invokes the agent with no user message in between. The follow-up it
// produces arrives while nothing is in flight; it must still reach the thread
// instead of being dropped, and must drive the root status back through done.
func TestStream_BackgroundContinuationRenders(t *testing.T) {
	sess := newFakeStreamSession("sess-1")
	d := newFakeStreamDriver(sess)
	svc, r := newStreamService(d)

	svc.startHeadless(context.Background(), "claude", "thread-1", "/wt", "", "demo", "",
		RoleOwner, nil, "owner", turnReq{channelID: "c", messageID: "m1", text: "kick off a background build"})
	<-d.opened
	waitFor(t, "first send", func() bool { return sess.sentCount() == 1 })

	// First turn ends: the agent backgrounded the work and stopped talking.
	sess.emit(agentproc.AssistantText{Text: "Build started in the background."})
	sess.emit(agentproc.TurnComplete{})
	svc.waitIdle("thread-1")

	// The build finishes; the harness re-invokes the agent with no user turn. Its
	// unprompted follow-up arrives while the inflight FIFO is empty.
	sess.emit(agentproc.AssistantText{Text: "Build finished: all green."})
	sess.emit(agentproc.TurnComplete{})

	waitFor(t, "background follow-up posted", func() bool {
		return anyContains(r.posts, "Build finished: all green.")
	})
	// The continuation drove the global status through working back to done — it
	// neither stays stuck on working nor goes silent.
	if last := lastReactOn(r.reacts, "c|m1"); last != emojiDone {
		t.Fatalf("root status = %q, want done after continuation", last)
	}
}

func TestStream_StopClosesSession(t *testing.T) {
	sess := newFakeStreamSession("sess-1")
	d := newFakeStreamDriver(sess)
	svc, _ := newStreamService(d)

	svc.startHeadless(context.Background(), "claude", "thread-1", "/wt", "", "demo", "",
		RoleOwner, nil, "owner", turnReq{channelID: "c", messageID: "m1", text: "go"})
	<-d.opened
	waitFor(t, "first send", func() bool { return sess.sentCount() == 1 })
	sess.emit(agentproc.AssistantText{Text: "hi"})
	sess.emit(agentproc.TurnComplete{})
	svc.waitIdle("thread-1")

	if !svc.StopThread(context.Background(), "thread-1", Caller{Role: RoleOwner, UserID: "owner"}) {
		t.Fatalf("stop should end the session")
	}
	waitFor(t, "session closed", func() bool {
		sess.mu.Lock()
		defer sess.mu.Unlock()
		return sess.closed
	})
	if svc.Tracked("thread-1") {
		t.Fatalf("session still tracked after stop")
	}
}
