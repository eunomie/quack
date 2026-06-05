package session

import (
	"context"
	"testing"

	"github.com/eunomie/quack/internal/agentproc"
)

// A guest's headless turn must run through the container launcher (not on the
// host), and stopping the session must tear the sandbox down.
func TestHeadless_GuestTurnUsesContainerLauncherAndTearsDownOnStop(t *testing.T) {
	d := &fakeDriver{turns: []scripted{{texts: []string{"ok"}, ref: "g-ref"}}}
	svc, _, _, _ := newHeadlessServiceFakes(d)
	fs := &fakeSandboxer{}
	svc.UseSandbox(fs, GuestPolicy{GitHubPAT: "PAT", GitUserName: "O", GitUserEmail: "o@e", EgressAllow: []string{"github.com"}})

	handle := &SandboxHandle{AgentContainer: "q-agent", Workdir: "/work/r", Name: "guest-sess"}
	svc.startHeadless(context.Background(), "claude", "thread-g", "/work/r", "high", "guest-sess", "owner/repo",
		RoleGuest, handle,
		turnReq{channelID: "c", messageID: "m1", text: "do the thing"})
	svc.waitIdle("thread-g")

	if len(d.seen) != 1 {
		t.Fatalf("driver saw %d turns, want 1", len(d.seen))
	}
	got := d.seen[0].Launcher
	if got == nil {
		t.Fatalf("guest turn must carry a launcher; got nil")
	}
	cl, ok := got.(agentproc.ContainerLauncher)
	if !ok {
		t.Fatalf("guest turn launcher = %T, want agentproc.ContainerLauncher", got)
	}
	if cl.Container != "q-agent" || cl.Workdir != "/work/r" {
		t.Errorf("container launcher = %+v, want container q-agent / workdir /work/r", cl)
	}

	if !svc.StopThread(context.Background(), "thread-g") {
		t.Fatalf("stop should report it ended a tracked session")
	}
	if fs.teardowns != 1 {
		t.Errorf("Teardown called %d times on stop, want exactly 1", fs.teardowns)
	}
}

// End-to-end: a RoleGuest command takes the guest path — it provisions a
// sandbox (rather than the owner clone/worktree switch) and launches a tracked
// headless session.
func TestHandle_GuestPathProvisionsSandbox(t *testing.T) {
	svc, g, tx, r, _ := newTestService()
	d := &fakeDriver{turns: []scripted{{texts: []string{"on it"}, ref: "g-sess"}}}
	svc.drivers = map[string]agentproc.Driver{"claude": d}
	fs := &fakeSandboxer{}
	svc.UseSandbox(fs, GuestPolicy{GitHubPAT: "PAT", GitUserName: "O", GitUserEmail: "o@e", EgressAllow: []string{"github.com"}})

	svc.Handle(context.Background(), Request{
		Content: "! owner/repo\nDo it.",
		Origin:  baseOrigin(),
		Role:    RoleGuest,
	})
	svc.waitIdle(r.threadID)

	// The guest path provisions a sandbox instead of cloning/worktreeing on the host.
	if fs.gotSpec.SessionName == "" {
		t.Fatalf("expected Provision to be called on the guest path; sandboxer spec = %+v", fs.gotSpec)
	}
	if len(g.cloned) != 0 || len(g.worktrees) != 0 {
		t.Errorf("guest path must not clone/worktree on the host: cloned=%v worktrees=%v", g.cloned, g.worktrees)
	}
	if len(tx.created) != 0 {
		t.Errorf("guest is forced headless: must not launch tmux, got %v", tx.created)
	}
	if !svc.Tracked(r.threadID) {
		t.Fatalf("guest session should be a tracked headless session")
	}
	// The turn ran through the container launcher reconstructed from the handle.
	if len(d.seen) != 1 || d.seen[0].Launcher == nil {
		t.Fatalf("guest turn should carry a container launcher: %+v", d.seen)
	}
}

// PromoteThread refuses a sandboxed guest session: handing it a host tmux
// session would break out of the jail, so it stays headless (tracked), the
// sandbox is not torn down, and an owner-only refusal is posted.
func TestPromoteThread_RefusesGuestSandbox(t *testing.T) {
	d := &fakeDriver{turns: []scripted{{texts: []string{"ok"}, ref: "g-ref"}}}
	svc, _, r, _ := newHeadlessServiceFakes(d)
	fs := &fakeSandboxer{}
	svc.UseSandbox(fs, GuestPolicy{GitHubPAT: "PAT", GitUserName: "O", GitUserEmail: "o@e", EgressAllow: []string{"github.com"}})

	handle := &SandboxHandle{AgentContainer: "q-agent", Workdir: "/work/r", Name: "guest-sess"}
	svc.startHeadless(context.Background(), "claude", "thread-g", "/work/r", "high", "guest-sess", "owner/repo",
		RoleGuest, handle,
		turnReq{channelID: "c", messageID: "m1", text: "do the thing"})
	svc.waitIdle("thread-g")

	if !svc.PromoteThread(context.Background(), "thread-g") {
		t.Fatalf("promote should report handled")
	}
	if !svc.Tracked("thread-g") {
		t.Fatalf("refused promotion must leave the guest session tracked")
	}
	if fs.teardowns != 0 {
		t.Errorf("refusing promotion must not tear down the sandbox, got %d teardowns", fs.teardowns)
	}
	if !anyContains(r.posts, "owner-only") {
		t.Errorf("expected an owner-only refusal message, got %v", r.posts)
	}
}

// An owner's headless turn carries no launcher (nil => DirectLauncher in the
// driver) and never touches the sandbox.
func TestHeadless_OwnerTurnHasNoLauncherOrSandbox(t *testing.T) {
	d := &fakeDriver{turns: []scripted{{texts: []string{"ok"}, ref: "o-ref"}}}
	svc, _, _, _ := newHeadlessServiceFakes(d)
	fs := &fakeSandboxer{}
	svc.UseSandbox(fs, GuestPolicy{})

	svc.startHeadless(context.Background(), "claude", "thread-o", "/wt", "high", "owner-sess", "owner/repo",
		RoleOwner, nil,
		turnReq{channelID: "c", messageID: "m1", text: "go"})
	svc.waitIdle("thread-o")

	if d.seen[0].Launcher != nil {
		t.Errorf("owner turn launcher = %v, want nil (DirectLauncher)", d.seen[0].Launcher)
	}
	svc.StopThread(context.Background(), "thread-o")
	if fs.teardowns != 0 {
		t.Errorf("owner stop must not tear down a sandbox, got %d teardowns", fs.teardowns)
	}
}

// Rehydrate restores a guest session by reattaching its sandbox (not by the
// owner worktree check, since a guest workdir is an in-container path), and the
// rebuilt session gets a container launcher.
func TestHeadless_RehydrateReattachesGuestSandbox(t *testing.T) {
	d := &fakeDriver{}
	svc, _, _, fs := newHeadlessServiceFakes(d)
	sb := &fakeSandboxer{}
	svc.UseSandbox(sb, GuestPolicy{GitHubPAT: "PAT", GitUserName: "O", GitUserEmail: "o@e", EgressAllow: []string{"github.com"}})

	seedRecord(fs, sessionRecord{
		Name: "guest-demo", Label: "owner/repo", AgentName: "claude",
		Workdir: "/work/r", // in-container path; git.PathExists would be false
		ThreadID: "thread-g", RootChannelID: "c", RootMessageID: "m1", SessionRef: "sess-1",
		Role:    RoleGuest,
		Sandbox: &SandboxHandle{AgentContainer: "q-agent", Workdir: "/work/r", Name: "guest-demo"},
	})

	if n := svc.Rehydrate(context.Background()); n != 1 {
		t.Fatalf("Rehydrate restored %d guest sessions, want 1", n)
	}
	if sb.reattaches != 1 {
		t.Errorf("Reattach called %d times, want exactly 1", sb.reattaches)
	}
	// Secrets come from current policy, never the persisted record.
	if sb.reattachSpec.GitHubPAT != "PAT" {
		t.Errorf("reattach spec PAT = %q, want it re-sourced from policy", sb.reattachSpec.GitHubPAT)
	}
	if sb.reattachSpec.SessionName != "guest-demo" {
		t.Errorf("reattach spec SessionName = %q, want guest-demo", sb.reattachSpec.SessionName)
	}
	if !svc.Tracked("thread-g") {
		t.Fatalf("guest thread should be tracked after rehydration")
	}
	ls := svc.sessions["thread-g"]
	if ls.launcher == nil {
		t.Fatalf("rehydrated guest session must have a container launcher")
	}
	if _, ok := ls.launcher.(agentproc.ContainerLauncher); !ok {
		t.Errorf("rehydrated guest launcher = %T, want agentproc.ContainerLauncher", ls.launcher)
	}
	if ls.sandbox == nil {
		t.Errorf("rehydrated guest session must keep its sandbox handle")
	}
}

// A guest record can't be restored when sandbox support isn't configured
// (UseSandbox was never called) — it is skipped, not resurrected without a box.
func TestHeadless_RehydrateSkipsGuestWhenNoSandbox(t *testing.T) {
	d := &fakeDriver{}
	svc, _, _, fs := newHeadlessServiceFakes(d) // no UseSandbox

	seedRecord(fs, sessionRecord{
		Name: "guest-orphan", AgentName: "claude", Workdir: "/work/r",
		ThreadID: "thread-go", SessionRef: "sess-1",
		Role:    RoleGuest,
		Sandbox: &SandboxHandle{AgentContainer: "q-agent", Workdir: "/work/r"},
	})

	if n := svc.Rehydrate(context.Background()); n != 0 {
		t.Fatalf("Rehydrate restored %d, want 0 when guest sandbox support is unconfigured", n)
	}
	if svc.Tracked("thread-go") {
		t.Errorf("guest session must not be restored without a configured sandboxer")
	}
}
