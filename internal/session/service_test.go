package session

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/eunomie/quack/internal/agent"
	"github.com/eunomie/quack/internal/agentproc"
)

func newTestService() (*Service, *fakeGit, *fakeTmux, *fakeReplier, *memFS) {
	g, tx, r, fs := newFakeGit(), newFakeTmux(), newFakeReplier(), newMemFS()
	svc := &Service{
		cfg: Config{
			DevSrcRoot:           "/src",
			ScratchDir:           "/scratch",
			CloneProtocol:        "ssh",
			DefaultAgent:         "claude",
			NameAgent:            "claude",
			StateDir:             "/state",
			ThreadAutoArchiveMin: 10080,
			Agents: map[string]agent.Agent{
				"claude": {Command: "claude", EffortTemplate: "--effort {effort}", NameTemplate: "-n {name}", ResumeTemplate: "--resume {session}", DefaultEffort: "xhigh"},
			},
		},
		git: g, tmux: tx, reply: r,
		mkdirAll:  fs.mkdirAll,
		writeFile: fs.writeFile,
		remove:    fs.remove,
		readDir:   fs.readDir,
		readFile:  fs.readFile,
		newToken:  func() string { return "tok" },
		mkdirTemp: func() (string, error) { return "/tmp/quack-test", nil },
	}
	svc.history = r
	return svc, g, tx, r, fs
}

func baseOrigin() Origin {
	return Origin{GuildID: "g", ChannelID: "c", MessageID: "m", AuthorID: "u", Author: "alice", CreatedAt: "2026-05-31T17:00:00Z"}
}

func TestHandle_HappyPath_ExistingRepo(t *testing.T) {
	svc, g, tx, r, fs := newTestService()
	g.existing["/src/github.com/dagger/dagger"] = true

	svc.Handle(context.Background(), Request{
		Content: "! dagger/dagger no-headless effort=high name=fix-cache\nFix the pin bug.",
		Origin:  baseOrigin(),
	})

	if len(r.threads) != 1 || !strings.HasSuffix(r.threads[0], "|fix-cache") {
		t.Fatalf("threads = %v", r.threads)
	}
	if len(g.cloned) != 0 || len(g.fetched) != 1 {
		t.Fatalf("cloned=%v fetched=%v", g.cloned, g.fetched)
	}
	if len(g.worktrees) != 1 || !strings.Contains(g.worktrees[0], "/src/github.com/dagger/dagger-worktrees/fix-cache|fix-cache|origin/main") {
		t.Fatalf("worktrees = %v", g.worktrees)
	}
	if len(tx.created) != 1 {
		t.Fatalf("tmux sessions = %d", len(tx.created))
	}
	got := tx.created[0]
	if got.Name != "quack/fix-cache" || got.Dir != "/src/github.com/dagger/dagger-worktrees/fix-cache" {
		t.Errorf("session name/dir = %q %q", got.Name, got.Dir)
	}
	argv := got.Argv
	if argv[0] != "claude" || argv[1] != "-n" || argv[2] != "fix-cache" || argv[3] != "--effort" || argv[4] != "high" {
		t.Errorf("argv prefix = %v", argv)
	}
	final := argv[len(argv)-1]
	if !strings.Contains(final, "<quack-context>") || !strings.HasSuffix(final, "Fix the pin bug.") {
		t.Errorf("final argv (prompt) = %q", final)
	}
	if _, ok := fs.files["/state/sessions/fix-cache/context.json"]; !ok {
		t.Errorf("context.json not written; files=%v", fs.files)
	}
	if len(r.edits) == 0 || !strings.Contains(r.edits[len(r.edits)-1].content, "fix-cache") {
		t.Errorf("edits = %v", r.edits)
	}
	if !hasStr(r.reacts, "c|m|"+emojiDone) {
		t.Errorf("expected done reaction on the command message, got %v", r.reacts)
	}
}

func TestHandle_Attachments(t *testing.T) {
	svc, g, tx, _, fs := newTestService()
	g.existing["/src/github.com/dagger/dagger"] = true
	svc.fetchURL = func(context.Context, string) ([]byte, error) { return []byte("PNG"), nil }

	svc.Handle(context.Background(), Request{
		Content:     "! dagger/dagger no-headless name=shot\nWhat's in this screenshot?",
		Attachments: []Attachment{{Filename: "shot.png", URL: "http://x/1", ContentType: "image/png"}},
		Origin:      baseOrigin(),
	})

	if _, ok := fs.files["/state/sessions/shot/attachments/shot.png"]; !ok {
		t.Fatalf("attachment not saved under the session state dir; files=%v", fs.files)
	}
	if len(tx.created) != 1 {
		t.Fatalf("tmux sessions = %d", len(tx.created))
	}
	prompt := tx.created[0].Argv[len(tx.created[0].Argv)-1]
	if !strings.Contains(prompt, "<quack-attachments>") ||
		!strings.Contains(prompt, "/state/sessions/shot/attachments/shot.png") {
		t.Fatalf("prompt should point the agent at the saved attachment: %q", prompt)
	}
}

func TestHandle_ClonesMissingRepo(t *testing.T) {
	svc, g, _, _, _ := newTestService()
	svc.Handle(context.Background(), Request{
		Content: "! dagger/dagger no-headless\nDo it.",
		Origin:  baseOrigin(),
	})
	if len(g.cloned) != 1 || !strings.Contains(g.cloned[0], "git@github.com:dagger/dagger.git -> /src/github.com/dagger/dagger") {
		t.Fatalf("cloned = %v", g.cloned)
	}
}

func TestHandle_PlainDirectory(t *testing.T) {
	svc, g, tx, _, _ := newTestService()
	g.pathExists["/home/tester/scratch"] = true
	t.Setenv("HOME", "/home/tester")
	svc.Handle(context.Background(), Request{
		Content: "! /home/tester/scratch no-headless\nPoke around.",
		Origin:  baseOrigin(),
	})
	if len(g.worktrees) != 0 {
		t.Errorf("expected no worktree for plain dir, got %v", g.worktrees)
	}
	if len(tx.created) != 1 || tx.created[0].Dir != "/home/tester/scratch" {
		t.Errorf("tmux dir = %v", tx.created)
	}
}

func TestHandle_NoTarget_ScratchDir(t *testing.T) {
	svc, g, tx, _, _ := newTestService()
	svc.Handle(context.Background(), Request{
		Content: "! no-headless\nQuick question.",
		Origin:  baseOrigin(),
	})
	if len(g.worktrees) != 0 || len(g.cloned) != 0 {
		t.Errorf("scratch session should not clone/worktree: cloned=%v worktrees=%v", g.cloned, g.worktrees)
	}
	if len(tx.created) != 1 || tx.created[0].Dir != "/scratch" {
		t.Fatalf("tmux should run in the scratch dir, got %v", tx.created)
	}
	if tx.created[0].Name != "quack/tok" {
		t.Errorf("session name = %q, want quack/tok (pure random)", tx.created[0].Name)
	}
	// No effort given + scratch => default to medium (not the agent's xhigh).
	if !hasStr(tx.created[0].Argv, "medium") {
		t.Errorf("scratch effort should default to medium, argv = %v", tx.created[0].Argv)
	}
}

func TestHandle_ScratchEffortOverridable(t *testing.T) {
	svc, _, tx, _, _ := newTestService()
	svc.Handle(context.Background(), Request{
		Content: "! effort=high no-headless\nQuick question.",
		Origin:  baseOrigin(),
	})
	if len(tx.created) != 1 || tx.created[0].Dir != "/scratch" {
		t.Fatalf("expected scratch dir, got %v", tx.created)
	}
	if !hasStr(tx.created[0].Argv, "high") || hasStr(tx.created[0].Argv, "medium") {
		t.Errorf("explicit effort=high should win over the scratch default, argv = %v", tx.created[0].Argv)
	}
}

func TestHandle_TempDirKeyword(t *testing.T) {
	svc, g, tx, _, _ := newTestService()
	svc.Handle(context.Background(), Request{
		Content: "! temp-dir no-headless\nDo it in a throwaway dir.",
		Origin:  baseOrigin(),
	})
	if len(g.worktrees) != 0 || len(g.cloned) != 0 {
		t.Errorf("temp-dir session should not clone/worktree: cloned=%v worktrees=%v", g.cloned, g.worktrees)
	}
	if len(tx.created) != 1 || tx.created[0].Dir != "/tmp/quack-test" {
		t.Fatalf("temp-dir keyword should run in a fresh temp dir, got %v", tx.created)
	}
	if tx.created[0].Name != "quack/tok" {
		t.Errorf("session name = %q, want quack/tok (pure random)", tx.created[0].Name)
	}
	// temp-dir is the faithful old-behavior escape hatch: agent default effort, not medium.
	if !hasStr(tx.created[0].Argv, "xhigh") {
		t.Errorf("temp-dir should keep the agent default effort, argv = %v", tx.created[0].Argv)
	}
}

func TestHandle_ThreadTitle_DirLabel(t *testing.T) {
	svc, g, _, r, _ := newTestService()
	g.pathExists["/home/tester/scratch"] = true
	t.Setenv("HOME", "/home/tester")
	svc.Handle(context.Background(), Request{
		Content: "! /home/tester/scratch no-headless\nPoke around.",
		Origin:  baseOrigin(),
	})
	// A plain directory labels the title with its basename: "<dir> <name>".
	if len(r.renames) != 1 || r.renames[0] != "thread-1|scratch scratch-tok" {
		t.Fatalf("expected dir label in title, got %v", r.renames)
	}
}

func TestHandle_ThreadTitle_ScratchHasNoLabel(t *testing.T) {
	svc, _, _, r, _ := newTestService()
	svc.Handle(context.Background(), Request{
		Content: "! no-headless\nQuick question.",
		Origin:  baseOrigin(),
	})
	// A scratch workspace has no repo/dir, so the title stays the bare name —
	// the provisional thread name already matches, so no rename is issued.
	if len(r.renames) != 0 {
		t.Fatalf("scratch workspace should carry no label and need no rename, got %v", r.renames)
	}
}

func TestHandle_GitRepoPath(t *testing.T) {
	svc, g, tx, r, fs := newTestService()
	t.Setenv("HOME", "/home/tester")
	p := "/home/tester/code/widget"
	g.pathExists[p] = true
	g.repos[p] = true
	g.primary[p] = p // PrimaryClone resolves to the main clone

	svc.Handle(context.Background(), Request{
		Content: "! ~/code/widget no-headless name=tidy\nClean it up.",
		Origin:  baseOrigin(),
	})

	if len(g.cloned) != 0 || len(g.fetched) != 1 || g.fetched[0] != p {
		t.Fatalf("cloned=%v fetched=%v", g.cloned, g.fetched)
	}
	if len(g.worktrees) != 1 || !strings.Contains(g.worktrees[0], "/home/tester/code/widget-worktrees/tidy|tidy|origin/main") {
		t.Fatalf("worktrees = %v", g.worktrees)
	}
	if len(tx.created) != 1 || tx.created[0].Dir != "/home/tester/code/widget-worktrees/tidy" {
		t.Fatalf("tmux dir = %v", tx.created)
	}
	if _, ok := fs.files["/state/sessions/tidy/context.json"]; !ok {
		t.Errorf("context.json not written; files=%v", fs.files)
	}
	// A path target labels the title with the repo directory's short name.
	if len(r.renames) != 1 || r.renames[0] != "thread-1|widget tidy" {
		t.Fatalf("expected repo dir label in title, got %v", r.renames)
	}
}

func TestHandle_AutoName_RepoBaseRandom(t *testing.T) {
	svc, g, _, r, _ := newTestService()
	g.existing["/src/github.com/dagger/dagger"] = true
	svc.Handle(context.Background(), Request{
		Content: "! dagger/dagger no-headless\nFix it.",
		Origin:  baseOrigin(),
	})
	// auto name = <repo>-<base>-<token>
	if len(g.worktrees) != 1 || !strings.Contains(g.worktrees[0], "dagger-worktrees/dagger-main-tok|dagger-main-tok|origin/main") {
		t.Fatalf("worktrees = %v", g.worktrees)
	}
	// thread opened with provisional <repo>-<token>, renamed to "<owner/repo> <name>"
	if len(r.renames) != 1 || r.renames[0] != "thread-1|dagger/dagger dagger-main-tok" {
		t.Fatalf("expected rename to labelled final name, got %v", r.renames)
	}
}

func TestHandle_DefaultBranchFallback(t *testing.T) {
	svc, g, _, _, _ := newTestService()
	g.existing["/src/github.com/dagger/dagger"] = true
	g.defaultBranchErr = errors.New("origin/HEAD not set") // detection fails

	svc.Handle(context.Background(), Request{
		Content: "! dagger/dagger no-headless\nGo.",
		Origin:  baseOrigin(),
	})

	if len(g.worktrees) != 1 || !strings.Contains(g.worktrees[0], "|origin/main") {
		t.Fatalf("expected fallback to origin/main, got %v", g.worktrees)
	}
}

func TestHandle_BaseResolvesFromUpstream(t *testing.T) {
	svc, g, _, _, _ := newTestService()
	g.existing["/src/github.com/dagger/dagger"] = true
	// Fork layout: the requested branch lives on upstream, not on the fork (origin).
	g.remotes = []string{"origin", "upstream"}
	g.refs = map[string]bool{"upstream/1.0-beta": true}

	svc.Handle(context.Background(), Request{
		Content: "! dagger/dagger no-headless base=1.0-beta\nGo.",
		Origin:  baseOrigin(),
	})

	if len(g.worktrees) != 1 || !strings.HasSuffix(g.worktrees[0], "|upstream/1.0-beta") {
		t.Fatalf("expected worktree based on upstream/1.0-beta, got %v", g.worktrees)
	}
}

func TestHandle_BaseResolvesFromOtherRemote(t *testing.T) {
	svc, g, _, _, _ := newTestService()
	g.existing["/src/github.com/dagger/dagger"] = true
	// Branch only on a non-standard remote name (neither origin nor upstream).
	g.remotes = []string{"origin", "fork"}
	g.refs = map[string]bool{"fork/feature": true}

	svc.Handle(context.Background(), Request{
		Content: "! dagger/dagger no-headless base=feature\nGo.",
		Origin:  baseOrigin(),
	})

	if len(g.worktrees) != 1 || !strings.HasSuffix(g.worktrees[0], "|fork/feature") {
		t.Fatalf("expected worktree based on fork/feature, got %v", g.worktrees)
	}
}

func TestHandle_BaseResolvesFromLocalBranch(t *testing.T) {
	svc, g, _, _, _ := newTestService()
	g.existing["/src/github.com/dagger/dagger"] = true
	// A purely local branch with no remote-tracking copy.
	g.remotes = []string{"origin"}
	g.refs = map[string]bool{"experiment": true}

	svc.Handle(context.Background(), Request{
		Content: "! dagger/dagger no-headless base=experiment\nGo.",
		Origin:  baseOrigin(),
	})

	if len(g.worktrees) != 1 || !strings.HasSuffix(g.worktrees[0], "|experiment") {
		t.Fatalf("expected worktree based on local branch experiment, got %v", g.worktrees)
	}
}

func TestHandle_BaseNotFound(t *testing.T) {
	svc, g, _, r, _ := newTestService()
	g.existing["/src/github.com/dagger/dagger"] = true
	g.remotes = []string{"origin", "upstream"}
	g.refs = map[string]bool{} // nothing resolves

	svc.Handle(context.Background(), Request{
		Content: "! dagger/dagger no-headless base=ghost\nGo.",
		Origin:  baseOrigin(),
	})

	if len(g.worktrees) != 0 {
		t.Fatalf("should not create a worktree when base is unknown: %v", g.worktrees)
	}
	last := r.edits[len(r.edits)-1].content
	if !strings.Contains(last, "ghost") {
		t.Errorf("expected error mentioning the missing base, got %q", last)
	}
}

func TestHandle_NoWorktree(t *testing.T) {
	svc, g, tx, r, _ := newTestService()
	g.existing["/src/github.com/dagger/dagger"] = true
	svc.Handle(context.Background(), Request{
		Content: "! dagger/dagger no-wt no-headless\nGo.",
		Origin:  baseOrigin(),
	})
	if len(g.worktrees) != 0 {
		t.Errorf("no-wt must not create a worktree: %v", g.worktrees)
	}
	if len(tx.created) != 1 || tx.created[0].Dir != "/src/github.com/dagger/dagger" {
		t.Fatalf("should run directly in the clone, got %v", tx.created)
	}
	if !anyContains(r.posts, "no worktree") {
		t.Errorf("expected the danger warning, got %v", r.posts)
	}
}

func TestHandle_ExplicitNameCollision(t *testing.T) {
	svc, g, _, r, _ := newTestService()
	g.existing["/src/github.com/dagger/dagger"] = true
	g.pathExists["/src/github.com/dagger/dagger-worktrees/taken"] = true
	svc.Handle(context.Background(), Request{
		Content: "! dagger/dagger no-headless name=taken\nGo.",
		Origin:  baseOrigin(),
	})
	if len(g.worktrees) != 0 {
		t.Errorf("should not create worktree on explicit collision: %v", g.worktrees)
	}
	last := r.edits[len(r.edits)-1].content
	if !strings.Contains(last, "already exists") {
		t.Errorf("expected collision error, got %q", last)
	}
}

func TestHandle_GeneratedNameBumps(t *testing.T) {
	svc, g, _, r, _ := newTestService()
	g.existing["/src/github.com/dagger/dagger"] = true
	g.pathExists["/src/github.com/dagger/dagger-worktrees/dagger-main-tok"] = true // first auto name taken
	svc.Handle(context.Background(), Request{
		Content: "! dagger/dagger no-headless\nFix the bug.",
		Origin:  baseOrigin(),
	})
	if len(g.worktrees) != 1 || !strings.Contains(g.worktrees[0], "dagger-worktrees/dagger-main-tok-2|dagger-main-tok-2|") {
		t.Fatalf("expected bumped name, got %v", g.worktrees)
	}
	if len(r.renames) != 1 || r.renames[0] != "thread-1|dagger/dagger dagger-main-tok-2" {
		t.Fatalf("expected labelled thread rename, got %v", r.renames)
	}
}

func TestHandle_UnknownAgent(t *testing.T) {
	svc, _, tx, r, _ := newTestService()
	svc.Handle(context.Background(), Request{
		Content: "! dagger/dagger agent=bogus\nGo.",
		Origin:  baseOrigin(),
	})
	if len(tx.created) != 0 {
		t.Errorf("should not launch for unknown agent")
	}
	if len(r.posts) == 0 || !strings.Contains(r.posts[len(r.posts)-1].content, "unknown agent") {
		t.Errorf("posts = %v", r.posts)
	}
}

func TestHandle_UsageErrorRepliesInChannel(t *testing.T) {
	svc, _, _, r, _ := newTestService()
	// Multi-line with a directive but an empty prompt — a single line would be
	// taken as the prompt, so the empty-prompt error needs the directive form.
	svc.Handle(context.Background(), Request{Content: "! dagger/dagger\n", Origin: baseOrigin()})
	if len(r.threads) != 0 {
		t.Errorf("should not open a thread on usage error")
	}
	if len(r.posts) != 1 || !strings.Contains(r.posts[0].content, "missing prompt") {
		t.Errorf("posts = %v", r.posts)
	}
	if !hasStr(r.reacts, "c|m|"+emojiError) {
		t.Errorf("expected error reaction on usage error, got %v", r.reacts)
	}
}

func TestHandle_PreseedsClaudeTrust(t *testing.T) {
	svc, g, _, _, _ := newTestService()
	g.existing["/src/github.com/dagger/dagger"] = true
	var trusted []string
	svc.trustDir = func(dir string) error { trusted = append(trusted, dir); return nil }

	svc.Handle(context.Background(), Request{
		Content: "! dagger/dagger no-headless name=fix\nGo.",
		Origin:  baseOrigin(),
	})

	if len(trusted) != 1 || trusted[0] != "/src/github.com/dagger/dagger-worktrees/fix" {
		t.Fatalf("expected worktree pre-trusted, got %v", trusted)
	}
}

func TestHandle_SkipsTrustForNonClaude(t *testing.T) {
	svc, g, _, _, _ := newTestService()
	svc.cfg.Agents["codex"] = agent.Agent{Command: "codex"}
	g.existing["/src/github.com/dagger/dagger"] = true
	called := false
	svc.trustDir = func(string) error { called = true; return nil }

	svc.Handle(context.Background(), Request{
		Content: "! dagger/dagger codex no-headless name=fix\nGo.",
		Origin:  baseOrigin(),
	})
	if called {
		t.Fatalf("trustDir should not be called for codex")
	}
}

func TestHandle_AgentNamesSession(t *testing.T) {
	svc, g, _, r, _ := newTestService()
	d := &fakeDriver{turns: []scripted{{texts: []string{"done"}, ref: "s"}}, suggest: "readme-suggestions"}
	svc.drivers = map[string]agentproc.Driver{"claude": d}
	g.existing["/src/github.com/eunomie/revue"] = true

	// only a repo, no name= -> the agent names it
	svc.Handle(context.Background(), Request{
		Content: "! eunomie/revue\nPropose suggestions to improve the readme.",
		Origin:  baseOrigin(),
	})
	svc.waitIdle(r.threadID)

	// Stop the session so the async title updater drains and exits before we read
	// renames (otherwise the title goroutine races the assertions below).
	ls := svc.sessions[r.threadID]
	svc.StopThread(context.Background(), r.threadID)
	<-ls.title.done

	if len(g.worktrees) != 1 || !strings.Contains(g.worktrees[0], "revue-worktrees/readme-suggestions|readme-suggestions|origin/main") {
		t.Fatalf("worktrees = %v", g.worktrees)
	}
	// Headless sessions don't rename to the bare labelled name first; the status
	// updater owns the title, so the very first rename already carries the working
	// icon ("👀 <owner/repo> <agent-name>") — one rename at startup, not two, to
	// stay under Discord's thread-rename rate limit.
	if len(r.renames) == 0 || !strings.HasSuffix(r.renames[0], "|"+emojiWorking+" eunomie/revue readme-suggestions") {
		t.Fatalf("expected first rename to carry the working icon and labelled name, got %v", r.renames)
	}
	if last := r.renames[len(r.renames)-1]; !strings.HasSuffix(last, "|"+emojiDone+" eunomie/revue readme-suggestions") {
		t.Fatalf("expected final title to carry the done icon and label, got %v", r.renames)
	}
	if len(d.seen) != 1 || d.seen[0].Name != "readme-suggestions" {
		t.Fatalf("session name passed to driver = %+v", d.seen)
	}
}

func TestHandle_NamerOverridesTaskAgent(t *testing.T) {
	svc, g, _, r, _ := newTestService()                                                                // cfg.NameAgent = "claude"
	dc := &fakeDriver{suggest: "named-by-claude"}                                                      // namer
	dx := &fakeDriver{turns: []scripted{{texts: []string{"ok"}, ref: "s"}}, suggest: "named-by-codex"} // task agent
	svc.drivers = map[string]agentproc.Driver{"claude": dc, "codex": dx}
	svc.cfg.Agents["codex"] = agent.Agent{Command: "codex"}
	g.existing["/src/github.com/eunomie/revue"] = true

	// task runs on codex, but naming uses the configured namer (claude)
	svc.Handle(context.Background(), Request{
		Content: "! eunomie/revue codex\nImprove the readme.",
		Origin:  baseOrigin(),
	})
	svc.waitIdle(r.threadID)

	if len(g.worktrees) != 1 || !strings.Contains(g.worktrees[0], "revue-worktrees/named-by-claude|named-by-claude|") {
		t.Fatalf("expected claude-named worktree, got %v", g.worktrees)
	}
	if len(dx.seen) != 1 {
		t.Fatalf("task should run on codex (1 turn), got %d", len(dx.seen))
	}
	if len(dc.seen) != 0 {
		t.Fatalf("claude should only name, not run the task; got %d turns", len(dc.seen))
	}
}

func TestPromoteThread(t *testing.T) {
	svc, _, tx, r, _ := newTestService()
	d := &fakeDriver{turns: []scripted{{texts: []string{"hi"}, ref: "sess-1"}}}
	svc.drivers = map[string]agentproc.Driver{"claude": d}

	svc.startHeadless(context.Background(), "claude", "thread-9", "/wt", "xhigh", "dagger-main-tok", "",
		RoleOwner, nil, turnReq{channelID: "c", messageID: "m", text: "go"})
	svc.waitIdle("thread-9") // first turn done -> sessionRef captured

	if !svc.PromoteThread(context.Background(), "thread-9") {
		t.Fatalf("promote should report handled")
	}
	if svc.Tracked("thread-9") {
		t.Fatalf("session should be untracked after promotion")
	}
	if len(tx.created) != 1 {
		t.Fatalf("expected a tmux session, got %v", tx.created)
	}
	got := tx.created[0]
	if got.Name != "quack/dagger-main-tok" || got.Dir != "/wt" {
		t.Errorf("tmux name/dir = %q %q", got.Name, got.Dir)
	}
	// /attach resumes claude with its interactive --dangerously-skip-permissions default.
	if len(got.Argv) != 4 || got.Argv[0] != "claude" || got.Argv[1] != "--resume" || got.Argv[2] != "sess-1" || got.Argv[3] != "--dangerously-skip-permissions" {
		t.Errorf("resume argv = %v", got.Argv)
	}
	if !anyContains(r.posts, "tmux attach -t quack/dagger-main-tok") {
		t.Errorf("attach instructions not posted: %v", r.posts)
	}
}

func TestPromoteThread_NotReady(t *testing.T) {
	svc, _, tx, r, _ := newTestService()
	d := &fakeDriver{turns: []scripted{{texts: []string{"hi"}, ref: ""}}} // no session ref captured
	svc.drivers = map[string]agentproc.Driver{"claude": d}

	svc.startHeadless(context.Background(), "claude", "thread-10", "/wt", "", "n", "",
		RoleOwner, nil, turnReq{channelID: "c", messageID: "m", text: "go"})
	svc.waitIdle("thread-10")

	if !svc.PromoteThread(context.Background(), "thread-10") {
		t.Fatalf("should report handled")
	}
	if !svc.Tracked("thread-10") {
		t.Fatalf("not-ready promotion should leave the session tracked")
	}
	if len(tx.created) != 0 {
		t.Errorf("should not launch tmux when not ready: %v", tx.created)
	}
	if !anyContains(r.posts, "not ready") {
		t.Errorf("expected not-ready message, got %v", r.posts)
	}
}

func TestHandle_HeadlessIsDefault(t *testing.T) {
	svc, g, tx, r, _ := newTestService()
	d := &fakeDriver{turns: []scripted{{texts: []string{"on it"}, ref: "sess-9"}}}
	svc.drivers = map[string]agentproc.Driver{"claude": d}
	g.existing["/src/github.com/dagger/dagger"] = true

	// no `no-headless` keyword -> headless by default
	svc.Handle(context.Background(), Request{
		Content: "! dagger/dagger\nFix the bug.",
		Origin:  baseOrigin(),
	})
	svc.waitIdle(r.threadID)

	if len(tx.created) != 0 {
		t.Errorf("headless must not launch tmux: %v", tx.created)
	}
	if len(d.seen) != 1 || !strings.Contains(d.seen[0].Prompt, "Fix the bug.") {
		t.Fatalf("driver turn = %+v", d.seen)
	}
	if d.seen[0].Effort != "xhigh" {
		t.Errorf("claude effort should default to xhigh, got %q", d.seen[0].Effort)
	}
	if d.seen[0].Name != "dagger-main-tok" {
		t.Errorf("session name passed to driver = %q", d.seen[0].Name)
	}
	if !anyContains(r.posts, "on it") {
		t.Errorf("agent answer not posted: %v", r.posts)
	}
	if !hasStr(r.reacts, "c|m|"+emojiWorking) || !hasStr(r.reacts, "c|m|"+emojiDone) {
		t.Errorf("expected working+done reactions on the command message, got %v", r.reacts)
	}
}

// A mention already inside a thread (a forum post) runs in place: quack drives
// that thread (Origin.ChannelID) and never opens a new one.
func TestHandle_InThread_RunsInPlace(t *testing.T) {
	svc, _, tx, r, _ := newTestService()

	svc.Handle(context.Background(), Request{
		Content:    "! no-headless\nhi",
		InThread:   true,
		ThreadName: "Help with login",
		Origin:     Origin{GuildID: "g", ChannelID: "post1", MessageID: "m", AuthorID: "u", Author: "alice", CreatedAt: "2026-06-04T17:00:00Z"},
	})

	if len(r.threads) != 0 {
		t.Fatalf("OpenThread must be skipped in place; threads = %v", r.threads)
	}
	if len(tx.created) != 1 {
		t.Fatalf("expected one tmux session, got %d", len(tx.created))
	}
	// Ack/progress posts go into the post itself, not a new thread.
	for _, p := range r.posts {
		if p.channel != "post1" {
			t.Errorf("post to %q, want the in-place thread post1: %+v", p.channel, p)
		}
	}
	// The start-rename is skipped in place (threadID == channelID), so the user's
	// post title is left untouched on the interactive path.
	if len(r.renames) != 0 {
		t.Errorf("no rename expected in place on the no-headless path; renames = %v", r.renames)
	}
}
