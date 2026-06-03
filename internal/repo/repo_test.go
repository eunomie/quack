package repo

import "testing"

func TestIsPath(t *testing.T) {
	paths := []string{"/abs", "~/home", "./rel", "../up"}
	for _, p := range paths {
		if !IsPath(p) {
			t.Errorf("IsPath(%q) = false, want true", p)
		}
	}
	for _, r := range []string{"dagger/dagger", "github.com/a/b", "https://x/y/z"} {
		if IsPath(r) {
			t.Errorf("IsPath(%q) = true, want false", r)
		}
	}
}

func TestParseRef(t *testing.T) {
	want := Ref{Host: "github.com", Owner: "dagger", Repo: "dagger"}
	cases := []string{
		"dagger/dagger",
		"github.com/dagger/dagger",
		"https://github.com/dagger/dagger",
		"https://github.com/dagger/dagger.git",
		"git@github.com:dagger/dagger.git",
	}
	for _, in := range cases {
		got, err := ParseRef(in)
		if err != nil {
			t.Fatalf("ParseRef(%q): %v", in, err)
		}
		if got != want {
			t.Errorf("ParseRef(%q) = %+v, want %+v", in, got, want)
		}
	}

	other, err := ParseRef("gitlab.com/foo/bar")
	if err != nil {
		t.Fatal(err)
	}
	if other != (Ref{Host: "gitlab.com", Owner: "foo", Repo: "bar"}) {
		t.Errorf("got %+v", other)
	}

	if _, err := ParseRef("bogus"); err == nil {
		t.Error("expected error for single-segment ref")
	}
}

func TestClonePathAndURL(t *testing.T) {
	r := Ref{Host: "github.com", Owner: "dagger", Repo: "dagger"}
	if got := r.ClonePath("/home/user/dev/src"); got != "/home/user/dev/src/github.com/dagger/dagger" {
		t.Errorf("ClonePath = %q", got)
	}
	if got := r.CloneURL("ssh"); got != "git@github.com:dagger/dagger.git" {
		t.Errorf("ssh URL = %q", got)
	}
	if got := r.CloneURL("https"); got != "https://github.com/dagger/dagger.git" {
		t.Errorf("https URL = %q", got)
	}
}
