package session

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fastService builds a minimal Service with one configured fast command and one
// tracked live session ("thread-1", workdir "/work"), plus the given runner
// behaviour. Constructed as a struct literal (in-package) so no real
// git/tmux/driver is needed.
func fastService(out []byte, runErr error) (*Service, *fakeReplier, *fakeRunner) {
	r := newFakeReplier()
	run := &fakeRunner{out: out, err: runErr}
	svc := &Service{
		cfg: Config{
			FastCommands: []FastCommand{
				{Trigger: "/revue", Argv: []string{"/bin/revue.sh"}},
			},
		},
		reply:  r,
		runner: run,
		sessions: map[string]*liveSession{
			"thread-1": {threadID: "thread-1", workdir: "/work", name: "sess"},
		},
	}
	return svc, r, run
}

func has(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func TestMatchFastCommand(t *testing.T) {
	svc, _, _ := fastService(nil, nil)
	cases := []struct {
		text     string
		want     bool
		wantArgs string // comma-joined
	}{
		{"/revue", true, ""},
		{"/revue main", true, "main"},
		{"/revue from workspace branch", true, "from,workspace,branch"},
		{"please /revue", false, ""},
		{"/open-zed", false, ""},
		{"", false, ""},
	}
	for _, c := range cases {
		fc, args, ok := svc.matchFastCommand(c.text)
		if ok != c.want {
			t.Fatalf("%q: ok=%v want %v", c.text, ok, c.want)
		}
		if !ok {
			continue
		}
		if fc.Trigger != "/revue" {
			t.Fatalf("%q: trigger=%q", c.text, fc.Trigger)
		}
		if got := strings.Join(args, ","); got != c.wantArgs {
			t.Fatalf("%q: args=%q want %q", c.text, got, c.wantArgs)
		}
	}
}

func TestRunFastCommand_NoMatch(t *testing.T) {
	svc, _, run := fastService(nil, nil)
	if svc.RunFastCommand(context.Background(), "thread-1", "msg-1", "hello there") {
		t.Fatal("non-trigger text should return false (fall through to the agent)")
	}
	if run.runs != 0 {
		t.Fatalf("runner should not run on a non-match: runs=%d", run.runs)
	}
}

func TestRunFastCommand_NoSession(t *testing.T) {
	svc, _, _ := fastService(nil, nil)
	if svc.RunFastCommand(context.Background(), "thread-unknown", "msg-1", "/revue") {
		t.Fatal("untracked thread should return false")
	}
}

func TestExecFastCommand_Success(t *testing.T) {
	svc, r, run := fastService([]byte("http://host:8080\n"), nil)
	ls := svc.sessions["thread-1"]

	svc.execFastCommand(context.Background(), ls, "msg-1",
		FastCommand{Argv: []string{"/bin/revue.sh"}}, []string{"main"})

	if run.dir != "/work" {
		t.Fatalf("cwd = %q, want /work", run.dir)
	}
	if got := strings.Join(run.argv, " "); got != "/bin/revue.sh main" {
		t.Fatalf("argv = %q", got)
	}
	if len(r.posts) != 1 || r.posts[0].content != "http://host:8080" {
		t.Fatalf("posts = %v", r.posts)
	}
	if !has(r.reacts, "thread-1|msg-1|👀") || !has(r.reacts, "thread-1|msg-1|✅") {
		t.Fatalf("reacts = %v", r.reacts)
	}
	if !has(r.unreacts, "thread-1|msg-1|👀") {
		t.Fatalf("unreacts = %v", r.unreacts)
	}
}

func TestExecFastCommand_Failure_PostsError(t *testing.T) {
	svc, r, _ := fastService(nil, errors.New("boom"))
	ls := svc.sessions["thread-1"]

	svc.execFastCommand(context.Background(), ls, "msg-1",
		FastCommand{Argv: []string{"/bin/revue.sh"}}, nil)

	if len(r.posts) != 1 || !strings.Contains(r.posts[0].content, "boom") {
		t.Fatalf("posts = %v", r.posts)
	}
	if !has(r.reacts, "thread-1|msg-1|❌") {
		t.Fatalf("reacts = %v", r.reacts)
	}
}

func TestExecFastCommand_Failure_PostsOutputNotError(t *testing.T) {
	svc, r, _ := fastService([]byte("port in use\n"), errors.New("exit 1"))
	ls := svc.sessions["thread-1"]

	svc.execFastCommand(context.Background(), ls, "msg-1",
		FastCommand{Argv: []string{"/bin/revue.sh"}}, nil)

	// When the command produced output, that output is shown (the error string is
	// redundant), still flagged with ❌.
	if len(r.posts) != 1 || r.posts[0].content != "port in use" {
		t.Fatalf("posts = %v", r.posts)
	}
	if !has(r.reacts, "thread-1|msg-1|❌") {
		t.Fatalf("reacts = %v", r.reacts)
	}
}
