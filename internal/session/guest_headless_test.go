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
