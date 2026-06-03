package session

import "testing"

func TestThreadTitle_JoinsNonEmptyParts(t *testing.T) {
	cases := []struct {
		emoji, label, name, want string
	}{
		{emojiDone, "eunomie/quack", "thread-title", emojiDone + " eunomie/quack thread-title"},
		{emojiWorking, "", "demo", emojiWorking + " demo"},
		{"", "eunomie/quack", "thread-title", "eunomie/quack thread-title"},
		{"", "", "demo", "demo"},
	}
	for _, c := range cases {
		if got := threadTitle(c.emoji, c.label, c.name); got != c.want {
			t.Errorf("threadTitle(%q,%q,%q) = %q, want %q", c.emoji, c.label, c.name, got, c.want)
		}
	}
}

func TestTitleUpdater_AppliesLatestStatus(t *testing.T) {
	r := newFakeReplier()
	tu := newTitleUpdater(r, "thread-1", "demo", "acme/widget")

	tu.set(emojiWorking)
	tu.set(emojiDone)
	tu.stop()
	<-tu.done

	if len(r.renames) == 0 {
		t.Fatalf("expected at least one thread rename, got none")
	}
	last := r.renames[len(r.renames)-1]
	if want := "thread-1|" + emojiDone + " acme/widget demo"; last != want {
		t.Fatalf("final title = %q, want %q (renames=%v)", last, want, r.renames)
	}
}

func TestTitleUpdater_StopIsIdempotent(t *testing.T) {
	r := newFakeReplier()
	tu := newTitleUpdater(r, "thread-1", "demo", "")
	tu.set(emojiWorking)
	tu.stop()
	tu.stop() // must not panic (double close)
	<-tu.done
}
