package worktree

import "testing"

func TestSlugify(t *testing.T) {
	cases := map[string]string{
		"Fix the cache pin bug":       "fix-the-cache-pin-bug",
		"  Investigate FLAKY test!!!": "investigate-flaky-test",
		"":                            "session",
		"---":                         "session",
	}
	for in, want := range cases {
		if got := Slugify(in); got != want {
			t.Errorf("Slugify(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSlugifyMaxLen(t *testing.T) {
	got := Slugify("aaaaaaaaaa bbbbbbbbbb cccccccccc dddddddddd eeeeeeeeee ffffffffff")
	if len(got) > maxSlugLen {
		t.Errorf("slug too long: %d > %d (%q)", len(got), maxSlugLen, got)
	}
	if got[len(got)-1] == '-' {
		t.Errorf("slug ends with hyphen: %q", got)
	}
}

func TestPath(t *testing.T) {
	got := Path("/home/user/dev/src/github.com/dagger/dagger", "fix-cache")
	want := "/home/user/dev/src/github.com/dagger/dagger-worktrees/fix-cache"
	if got != want {
		t.Errorf("Path = %q, want %q", got, want)
	}
}
