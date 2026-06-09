package agent

import (
	"reflect"
	"testing"
)

func strptr(s string) *string { return &s }

func TestArgv(t *testing.T) {
	claude := Agent{Command: "claude", EffortTemplate: "--effort {effort}", NameTemplate: "-n {name}"}
	codex := Agent{Command: "codex", EffortTemplate: "--config model_reasoning_effort={effort}"}

	cases := []struct {
		name         string
		a            Agent
		effort, sess string
		prompt       string
		want         []string
	}{
		// claude defaults to --dangerously-skip-permissions on interactive launches.
		{"claude name+effort", claude, "xhigh", "dagger-main-7k2p", "P", []string{"claude", "-n", "dagger-main-7k2p", "--effort", "xhigh", "--dangerously-skip-permissions", "P"}},
		// codex defaults to --dangerously-bypass-approvals-and-sandbox on interactive launches.
		{"codex effort (no name template)", codex, "xhigh", "x", "P", []string{"codex", "--config", "model_reasoning_effort=xhigh", "--dangerously-bypass-approvals-and-sandbox", "P"}},
		{"claude bare", claude, "", "", "P", []string{"claude", "--dangerously-skip-permissions", "P"}},
		{"claude interactive_args override", Agent{Command: "claude", InteractiveArgs: strptr("--foo --bar")}, "", "", "P", []string{"claude", "--foo", "--bar", "P"}},
		{"claude interactive_args disabled", Agent{Command: "claude", InteractiveArgs: strptr("")}, "", "", "P", []string{"claude", "P"}},
		{"codex interactive_args set", Agent{Command: "codex", InteractiveArgs: strptr("--yolo")}, "", "", "P", []string{"codex", "--yolo", "P"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.a.Argv(tc.effort, tc.sess, tc.prompt)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Argv = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestEffortOr(t *testing.T) {
	a := Agent{DefaultEffort: "xhigh"}
	if got := a.EffortOr(""); got != "xhigh" {
		t.Errorf("EffortOr(\"\") = %q, want xhigh", got)
	}
	if got := a.EffortOr("high"); got != "high" {
		t.Errorf("EffortOr(high) = %q, want high", got)
	}
}

func TestResumeArgv(t *testing.T) {
	claude := Agent{Command: "claude", ResumeTemplate: "--resume {session}"}
	// /attach also gets claude's --dangerously-skip-permissions default.
	if got := claude.ResumeArgv("abc"); !reflect.DeepEqual(got, []string{"claude", "--resume", "abc", "--dangerously-skip-permissions"}) {
		t.Errorf("ResumeArgv = %v", got)
	}
	disabled := Agent{Command: "claude", ResumeTemplate: "--resume {session}", InteractiveArgs: strptr("")}
	if got := disabled.ResumeArgv("abc"); !reflect.DeepEqual(got, []string{"claude", "--resume", "abc"}) {
		t.Errorf("ResumeArgv (disabled) = %v", got)
	}
	if claude.ResumeArgv("") != nil {
		t.Errorf("empty session should yield nil")
	}
	if (Agent{Command: "x"}).ResumeArgv("abc") != nil {
		t.Errorf("no resume template should yield nil")
	}
}

func TestHeadlessDefaults(t *testing.T) {
	a := Agent{Command: "claude", Headless: true}
	if got := a.Mode(); got != "acceptEdits" {
		t.Errorf("Mode default = %q, want acceptEdits", got)
	}
	b := Agent{Command: "claude", Headless: true, PermissionMode: "bypassPermissions"}
	if got := b.Mode(); got != "bypassPermissions" {
		t.Errorf("Mode = %q", got)
	}
}

func TestSandboxDefaults(t *testing.T) {
	a := Agent{Command: "codex", Headless: true}
	if got := a.Sandbox(); got != "danger-full-access" {
		t.Errorf("Sandbox default = %q, want danger-full-access", got)
	}
	b := Agent{Command: "codex", Headless: true, SandboxMode: "workspace-write"}
	if got := b.Sandbox(); got != "workspace-write" {
		t.Errorf("Sandbox = %q, want workspace-write", got)
	}
}
