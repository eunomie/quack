package session

import (
	"context"
	"strings"
	"sync"
)

// threadTitle builds a thread title from an optional status emoji, an optional
// workspace label (a repo "owner/repo" or a directory name), and the session
// name, joining the non-empty parts with a single space. Thread names are plain
// text, so no markdown is used.
func threadTitle(emoji, label, name string) string {
	parts := make([]string, 0, 3)
	for _, p := range []string{emoji, label, name} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, " ")
}

// titleUpdater applies thread-title status changes on a background goroutine.
// Discord rate-limits thread renames heavily (~2 per 10 minutes), so updates are
// best-effort and coalesced latest-wins: a newer status replaces any still-pending
// one, and a rename that blocks on the rate limiter blocks only this goroutine,
// never a turn. The title is always reconstructed as "<emoji> <label> <name>" so
// status icons never stack and the workspace label stays anchored to the name.
type titleUpdater struct {
	reply    Replier
	threadID string
	name     string
	label    string

	ch       chan string
	stopCh   chan struct{}
	stopOnce sync.Once
	done     chan struct{}
}

func newTitleUpdater(reply Replier, threadID, name, label string) *titleUpdater {
	tu := &titleUpdater{
		reply:    reply,
		threadID: threadID,
		name:     name,
		label:    label,
		ch:       make(chan string, 1),
		stopCh:   make(chan struct{}),
		done:     make(chan struct{}),
	}
	go tu.run()
	return tu
}

// set requests that the title show emoji as its status prefix. Non-blocking; if a
// prior request is still pending it is replaced (latest wins).
func (tu *titleUpdater) set(emoji string) {
	title := threadTitle(emoji, tu.label, tu.name)
	for {
		select {
		case tu.ch <- title:
			return
		default:
			select {
			case <-tu.ch: // drop the stale pending value, then retry the send
			default:
			}
		}
	}
}

// stop signals the updater to apply any pending title and exit. Idempotent and
// non-blocking; the goroutine closes done when it has exited.
func (tu *titleUpdater) stop() {
	tu.stopOnce.Do(func() { close(tu.stopCh) })
}

func (tu *titleUpdater) run() {
	defer close(tu.done)
	for {
		select {
		case title := <-tu.ch:
			_ = tu.reply.RenameThread(context.Background(), tu.threadID, title)
		case <-tu.stopCh:
			select {
			case title := <-tu.ch: // final drain: apply the latest pending title
				_ = tu.reply.RenameThread(context.Background(), tu.threadID, title)
			default:
			}
			return
		}
	}
}
