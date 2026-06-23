package session

import (
	"context"
	"strings"
	"testing"

	"github.com/eunomie/quack/internal/agent"
	"github.com/eunomie/quack/internal/agentproc"
)

func TestExtractJSON(t *testing.T) {
	cases := map[string]string{
		`{"a":1}`:                                      `{"a":1}`,
		"```json\n{\"a\":1}\n```":                      `{"a":1}`,
		"```\n{\"a\":1}\n```":                          `{"a":1}`,
		"sure, here:\n{\"a\":1}\nthanks":               `{"a":1}`,
		"```json\n{\"a\":1}\n```\nuse {bar} next time": `{"a":1}`,
		"no json here":                                 ``,
	}
	for in, want := range cases {
		if got := extractJSON(in); got != want {
			t.Errorf("extractJSON(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMapInferred(t *testing.T) {
	agents := map[string]agent.Agent{"claude": {Command: "claude"}}

	tru := true
	fls := false
	inf := inferred{
		Target:   " dagger/dagger ",
		Base:     "main",
		Worktree: &fls,
		Agent:    "bogus",     // unknown -> dropped
		Effort:   "ludicrous", // invalid -> dropped
		Name:     "Fix The Bug",
		Headless: &tru,
		Context:  "they meant the cache pin bug",
	}
	dir := mapInferred(inf, agents, "fix the cache pin bug")

	if dir.Target != "dagger/dagger" {
		t.Errorf("Target = %q", dir.Target)
	}
	if !dir.NoWorktree {
		t.Errorf("worktree:false should set NoWorktree")
	}
	if dir.Agent != "" {
		t.Errorf("unknown agent should drop to empty, got %q", dir.Agent)
	}
	if dir.Effort != "" {
		t.Errorf("invalid effort should drop to empty, got %q", dir.Effort)
	}
	if dir.Name != "fix-the-bug" {
		t.Errorf("Name = %q, want fix-the-bug", dir.Name)
	}
	if !dir.Headless {
		t.Errorf("headless should be true")
	}
	if !strings.Contains(dir.Prompt, "<quack-resolved-context>") || !strings.HasSuffix(dir.Prompt, "fix the cache pin bug") {
		t.Errorf("Prompt should prepend the resolved context block, got %q", dir.Prompt)
	}
}

func TestMapInferred_DefaultsWhenOmitted(t *testing.T) {
	agents := map[string]agent.Agent{"claude": {Command: "claude"}}
	dir := mapInferred(inferred{Agent: "claude", Effort: "high"}, agents, "do it")
	if dir.NoWorktree {
		t.Errorf("omitted worktree should default to worktree on (NoWorktree=false)")
	}
	if !dir.Headless {
		t.Errorf("omitted headless should default to true")
	}
	if dir.Agent != "claude" || dir.Effort != "high" {
		t.Errorf("known agent/effort should pass through, got %+v", dir)
	}
	if dir.Prompt != "do it" {
		t.Errorf("Prompt = %q, want raw passthrough", dir.Prompt)
	}
}

func TestMapInferred_MaxEffortPassesThrough(t *testing.T) {
	agents := map[string]agent.Agent{"claude": {Command: "claude"}}
	dir := mapInferred(inferred{Agent: "claude", Effort: "max"}, agents, "do it")
	if dir.Effort != "max" {
		t.Errorf("max effort should pass through, got %q", dir.Effort)
	}
}

func TestMapInferred_FableAgent(t *testing.T) {
	// When a fable agent is configured, the infer step may route to it.
	agents := map[string]agent.Agent{
		"claude": {Command: "claude"},
		"fable":  {Command: "claude", Model: "claude-fable-5"},
	}
	dir := mapInferred(inferred{Agent: "fable"}, agents, "use the most powerful model")
	if dir.Agent != "fable" {
		t.Errorf("configured fable agent should pass through, got %q", dir.Agent)
	}
}

func TestParseInferred(t *testing.T) {
	if _, err := parseInferred("not json"); err == nil {
		t.Errorf("expected error for non-JSON output")
	}
	inf, err := parseInferred("```json\n{\"target\":\"a/b\",\"name\":\"x\"}\n```")
	if err != nil {
		t.Fatal(err)
	}
	if inf.Target != "a/b" || inf.Name != "x" {
		t.Errorf("parsed = %+v", inf)
	}
}

func TestRecentHistory_Formats(t *testing.T) {
	svc, _, _, r, _ := newTestService()
	r.recent = []Message{
		{Author: "alice", Content: "we should add feature A"},
		{Author: "bob", Content: "  "}, // blank -> skipped
		{Author: "alice", Content: "yeah in dagger/dagger"},
	}
	got := svc.recentHistory(context.Background(), baseOrigin())
	want := "alice: we should add feature A\nalice: yeah in dagger/dagger"
	if got != want {
		t.Errorf("recentHistory = %q, want %q", got, want)
	}
}

func TestRecentHistory_NilHistory(t *testing.T) {
	svc, _, _, _, _ := newTestService()
	svc.history = nil
	if got := svc.recentHistory(context.Background(), baseOrigin()); got != "" {
		t.Errorf("nil history should yield empty string, got %q", got)
	}
}

func TestInferDirective_HappyPath(t *testing.T) {
	svc, _, _, _, _ := newTestService()
	d := &fakeDriver{oneShot: `{"target":"dagger/dagger","name":"feature-a","effort":"high","headless":false}`}
	svc.drivers = map[string]agentproc.Driver{"claude": d}

	dir, ok := svc.inferDirective(context.Background(), "in dagger/dagger build feature A", "alice: build feature A")
	if !ok {
		t.Fatal("expected ok")
	}
	if dir.Target != "dagger/dagger" || dir.Name != "feature-a" || dir.Effort != "high" || dir.Headless {
		t.Errorf("dir = %+v", dir)
	}
	if len(d.oneShotSeen) != 1 || !strings.Contains(d.oneShotSeen[0], "build feature A") {
		t.Errorf("infer prompt should carry the request, got %v", d.oneShotSeen)
	}
	if !strings.Contains(d.oneShotSeen[0], "alice: build feature A") {
		t.Errorf("infer prompt should carry the history, got %q", d.oneShotSeen[0])
	}
	if len(d.oneShotEffortSeen) != 1 || d.oneShotEffortSeen[0] != "medium" {
		t.Errorf("infer effort should default to medium, got %v", d.oneShotEffortSeen)
	}
}

func TestInferDirective_FailsGracefully(t *testing.T) {
	svc, _, _, _, _ := newTestService()

	// bad JSON
	d := &fakeDriver{oneShot: "I think you want dagger"}
	svc.drivers = map[string]agentproc.Driver{"claude": d}
	if _, ok := svc.inferDirective(context.Background(), "x", ""); ok {
		t.Errorf("unparseable output should report not-ok")
	}

	// OneShot itself errors
	derr := &fakeDriver{oneShotErr: context.DeadlineExceeded}
	svc.drivers = map[string]agentproc.Driver{"claude": derr}
	if _, ok := svc.inferDirective(context.Background(), "x", ""); ok {
		t.Errorf("OneShot error should report not-ok")
	}

	// no driver available
	svc.drivers = map[string]agentproc.Driver{}
	if _, ok := svc.inferDirective(context.Background(), "x", ""); ok {
		t.Errorf("missing driver should report not-ok")
	}
}

func TestRecentHistory_Truncates(t *testing.T) {
	svc, _, _, r, _ := newTestService()
	long := strings.Repeat("x", 450)
	r.recent = []Message{{Author: "alice", Content: long}}
	got := svc.recentHistory(context.Background(), baseOrigin())
	if !strings.HasPrefix(got, "alice: "+strings.Repeat("x", 400)+"…") {
		t.Errorf("long message should be truncated to 400 runes with an ellipsis, got len=%d", len([]rune(got)))
	}
	if strings.Contains(got, strings.Repeat("x", 401)) {
		t.Errorf("message should not exceed the 400-rune cap")
	}
}

func TestDirectivePrefix(t *testing.T) {
	cases := []struct {
		in   string
		spec string
		ok   bool
	}{
		{"! dagger/dagger\nGo.", "dagger/dagger\nGo.", true}, // one space after ! is dropped
		{"!dagger/dagger\nGo.", "dagger/dagger\nGo.", true},  // no space also opts in
		{"!\nGo.", "\nGo.", true},                            // empty directive line kept for Parse
		{"! ", "", true},
		{"dagger/dagger\nfoo", "", false}, // no marker -> fluent path
		{"plain question", "", false},
	}
	for _, c := range cases {
		spec, ok := directivePrefix(c.in)
		if ok != c.ok || spec != c.spec {
			t.Errorf("directivePrefix(%q) = (%q,%v), want (%q,%v)", c.in, spec, ok, c.spec, c.ok)
		}
	}
}

func TestHandle_Fluent_Tmux(t *testing.T) {
	svc, g, tx, r, _ := newTestService()
	g.existing["/src/github.com/dagger/dagger"] = true
	d := &fakeDriver{oneShot: `{"target":"dagger/dagger","name":"feature-a","effort":"high","headless":false}`}
	svc.drivers = map[string]agentproc.Driver{"claude": d}

	svc.Handle(context.Background(), Request{
		Content: "in dagger/dagger build feature A",
		Origin:  baseOrigin(),
	})

	if len(g.worktrees) != 1 || !strings.Contains(g.worktrees[0], "dagger-worktrees/feature-a|feature-a|origin/main") {
		t.Fatalf("worktrees = %v", g.worktrees)
	}
	if len(tx.created) != 1 || tx.created[0].Dir != "/src/github.com/dagger/dagger-worktrees/feature-a" {
		t.Fatalf("tmux = %v", tx.created)
	}
	if !hasStr(tx.created[0].Argv, "high") {
		t.Errorf("inferred effort high should reach argv, got %v", tx.created[0].Argv)
	}
	if !hasStr(r.reacts, "c|m|"+emojiWorking) {
		t.Errorf("expected early working reaction, got %v", r.reacts)
	}
	if !anyContains(r.posts, "interpreted as") {
		t.Errorf("expected the muted interpretation echo, got %v", r.posts)
	}
	var echo *postedMsg
	for i := range r.posts {
		if strings.Contains(r.posts[i].content, "interpreted as") {
			echo = &r.posts[i]
		}
	}
	if echo == nil || !echo.silent {
		t.Errorf("interpretation echo should be posted silently, got %+v", echo)
	}
	if len(d.oneShotSeen) != 1 || !strings.Contains(d.oneShotSeen[0], "build feature A") {
		t.Errorf("infer one-shot should see the raw request, got %v", d.oneShotSeen)
	}
	if !hasStr(r.reacts, "c|m|"+emojiDone) {
		t.Errorf("expected done reaction after launch, got %v", r.reacts)
	}
	if !hasStr(r.unreacts, "c|m|"+emojiWorking) {
		t.Errorf("expected the early working reaction to be cleared, got %v", r.unreacts)
	}
}

func TestHandle_Fluent_Fallback(t *testing.T) {
	svc, g, tx, r, _ := newTestService()
	// OneShot errors -> infer fails -> graceful headless fallback in the scratch
	// dir. The same driver still serves the task turn via its turns script.
	d := &fakeDriver{
		oneShotErr: context.DeadlineExceeded,
		turns:      []scripted{{texts: []string{"answer"}, ref: "s"}},
	}
	svc.drivers = map[string]agentproc.Driver{"claude": d}

	svc.Handle(context.Background(), Request{
		Content: "just answer this quick question",
		Origin:  baseOrigin(),
	})
	svc.waitIdle(r.threadID)

	ls := svc.sessions[r.threadID]
	svc.StopThread(context.Background(), r.threadID, Caller{Role: RoleOwner, UserID: "u"})
	<-ls.title.done

	if len(g.cloned) != 0 || len(g.worktrees) != 0 {
		t.Errorf("fallback should not clone/worktree: cloned=%v worktrees=%v", g.cloned, g.worktrees)
	}
	if len(tx.created) != 0 {
		t.Errorf("headless fallback should not open a tmux session, got %v", tx.created)
	}
	if len(d.seen) != 1 || d.seen[0].Workdir != "/scratch" {
		t.Fatalf("fallback should run headless in the scratch dir, got %+v", d.seen)
	}
	if !strings.Contains(d.seen[0].Prompt, "just answer this quick question") {
		t.Errorf("fallback should run the raw request, got %q", d.seen[0].Prompt)
	}
	if !anyContains(r.posts, "couldn't interpret") {
		t.Errorf("expected a muted fallback note, got %v", r.posts)
	}
}

func TestHandle_Fluent_EmptyRaw(t *testing.T) {
	svc, _, tx, r, _ := newTestService()
	svc.Handle(context.Background(), Request{
		Content: "   ",
		Origin:  baseOrigin(),
	})
	if len(r.threads) != 0 {
		t.Errorf("empty fluent request should not open a thread, got %v", r.threads)
	}
	if len(tx.created) != 0 {
		t.Errorf("empty fluent request should not launch, got %v", tx.created)
	}
	if !hasStr(r.reacts, "c|m|"+emojiError) {
		t.Errorf("expected error reaction, got %v", r.reacts)
	}
	if len(r.posts) == 0 || !strings.Contains(r.posts[len(r.posts)-1].content, "nothing to do") {
		t.Errorf("expected a 'nothing to do' reply, got %v", r.posts)
	}
}

func TestHandle_Fluent_EmptyRaw_Reply(t *testing.T) {
	svc, _, _, r, _ := newTestService()
	o := baseOrigin()
	o.RepliedToID = "rm"
	o.RepliedToAuthor = "bob"
	o.RepliedToContent = "please fix the flaky cache test"
	svc.Handle(context.Background(), Request{Content: "   ", Origin: o})
	if !hasStr(r.reacts, "c|m|"+emojiWorking) {
		t.Errorf("a bare ping in a reply should proceed (working reaction), got %v", r.reacts)
	}
	for _, p := range r.posts {
		if strings.Contains(p.content, "nothing to do") {
			t.Errorf("a bare ping in a reply should not say 'nothing to do', got %v", r.posts)
		}
	}
}

func TestGuidanceBlock(t *testing.T) {
	if got := guidanceBlock("  "); got != "" {
		t.Errorf("blank guidance should yield empty, got %q", got)
	}
	got := guidanceBlock("bare dagger means dagger/dagger")
	if !strings.Contains(got, "Environment hints") || !strings.Contains(got, "never invent a target") {
		t.Errorf("guidance block missing the fixed framing, got %q", got)
	}
	if !strings.Contains(got, "bare dagger means dagger/dagger") {
		t.Errorf("guidance block should carry the user text, got %q", got)
	}
	// The trailing blank line is load-bearing: it separates the hints from the
	// conversation section that follows the %s slot in the template.
	if !strings.HasSuffix(got, "\n\n") {
		t.Errorf("guidance block should end with a blank line for spacing, got %q", got)
	}
	// Surrounding whitespace on the input is trimmed before embedding.
	if trimmed := guidanceBlock("  hint  "); strings.Contains(trimmed, "  hint  ") {
		t.Errorf("guidance block should trim surrounding whitespace, got %q", trimmed)
	}
}

func TestInferDirective_Guidance(t *testing.T) {
	svc, _, _, _, _ := newTestService()
	svc.cfg.InferGuidance = "bare dagger means dagger/dagger"
	d := &fakeDriver{oneShot: `{"target":"dagger/dagger","name":"x"}`}
	svc.drivers = map[string]agentproc.Driver{"claude": d}

	if _, ok := svc.inferDirective(context.Background(), "build it", ""); !ok {
		t.Fatal("expected ok")
	}
	if len(d.oneShotSeen) != 1 || !strings.Contains(d.oneShotSeen[0], "Environment hints") {
		t.Errorf("guidance should be injected into the infer prompt, got %v", d.oneShotSeen)
	}
	if !strings.Contains(d.oneShotSeen[0], "bare dagger means dagger/dagger") {
		t.Errorf("guidance text missing from prompt, got %q", d.oneShotSeen[0])
	}
}

func TestInferDirective_NoGuidanceWhenUnset(t *testing.T) {
	svc, _, _, _, _ := newTestService()
	d := &fakeDriver{oneShot: `{"target":"a/b","name":"x"}`}
	svc.drivers = map[string]agentproc.Driver{"claude": d}

	if _, ok := svc.inferDirective(context.Background(), "build it", ""); !ok {
		t.Fatal("expected ok")
	}
	if strings.Contains(d.oneShotSeen[0], "Environment hints") {
		t.Errorf("no guidance configured should omit the hints section, got %q", d.oneShotSeen[0])
	}
}

func TestHandle_Fluent_Headless(t *testing.T) {
	svc, g, _, r, _ := newTestService()
	g.existing["/src/github.com/dagger/dagger"] = true
	d := &fakeDriver{
		oneShot: `{"target":"dagger/dagger","name":"fix-bug","headless":true,"context":"the cache pin bug"}`,
		turns:   []scripted{{texts: []string{"on it"}, ref: "s"}},
	}
	svc.drivers = map[string]agentproc.Driver{"claude": d}

	svc.Handle(context.Background(), Request{
		Content: "fix this in dagger/dagger",
		Origin:  baseOrigin(),
	})
	svc.waitIdle(r.threadID)

	ls := svc.sessions[r.threadID]
	svc.StopThread(context.Background(), r.threadID, Caller{Role: RoleOwner, UserID: "u"})
	<-ls.title.done

	if len(d.seen) != 1 {
		t.Fatalf("expected one task turn, got %d", len(d.seen))
	}
	if !strings.Contains(d.seen[0].Prompt, "<quack-context>") || !strings.Contains(d.seen[0].Prompt, "fix this in dagger/dagger") {
		t.Errorf("task prompt should carry quack-context + raw request, got %q", d.seen[0].Prompt)
	}
	if !strings.Contains(d.seen[0].Prompt, "<quack-resolved-context>") {
		t.Errorf("task prompt should carry the resolved context block, got %q", d.seen[0].Prompt)
	}
	if !anyContains(r.posts, "on it") {
		t.Errorf("agent answer not posted: %v", r.posts)
	}
}
