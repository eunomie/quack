package session

import (
	"testing"

	"github.com/eunomie/quack/internal/command"
)

func TestClampGuestDirective(t *testing.T) {
	d := &command.Directive{Headless: false, Prompt: "x", Target: "o/r"}
	note := clampGuestDirective(d)
	if !d.Headless {
		t.Fatal("guest must be forced headless")
	}
	if note == "" {
		t.Fatal("expected a note explaining the clamp")
	}
	d2 := &command.Directive{Headless: true, NoWorktree: true, Target: "o/r", Prompt: "x"}
	if note2 := clampGuestDirective(d2); note2 != "" {
		t.Fatalf("no clamp note expected when already headless, got %q", note2)
	}
	if d2.NoWorktree {
		t.Fatal("guest no-wt must be cleared")
	}
}

func TestGuestTargetRejectsHostPaths(t *testing.T) {
	for _, tgt := range []string{"/abs/path", "~/x", "./rel", "temp-dir"} {
		if err := guestTargetAllowed(tgt); err == nil {
			t.Fatalf("target %q should be rejected for guests", tgt)
		}
	}
	for _, tgt := range []string{"", "owner/repo"} {
		if err := guestTargetAllowed(tgt); err != nil {
			t.Fatalf("target %q should be allowed for guests: %v", tgt, err)
		}
	}
}
