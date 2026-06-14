package session

import (
	"context"
	"sync"
	"time"
)

// typingInterval re-triggers Discord's typing indicator before its ~10s
// server-side expiry, so the bot shows as "typing…" continuously while a turn is
// in flight rather than flickering off every few seconds.
const typingInterval = 8 * time.Second

// typingPump keeps the typing indicator alive on a thread while the agent works a
// turn: it triggers once immediately, then every typingInterval, until stopped.
// It is owned by the stream loop goroutine — start it when a burst begins, stop
// it when the inflight queue drains.
type typingPump struct {
	stopCh chan struct{}
	once   sync.Once
}

func startTypingPump(reply Replier, threadID string) *typingPump {
	p := &typingPump{stopCh: make(chan struct{})}
	go func() {
		t := time.NewTicker(typingInterval)
		defer t.Stop()
		_ = reply.Typing(context.Background(), threadID) // show it right away
		for {
			select {
			case <-t.C:
				_ = reply.Typing(context.Background(), threadID)
			case <-p.stopCh:
				return
			}
		}
	}()
	return p
}

// stop halts the pump. Nil-safe and idempotent; a final in-flight trigger is
// harmless (the indicator just expires on its own).
func (p *typingPump) stop() {
	if p == nil {
		return
	}
	p.once.Do(func() { close(p.stopCh) })
}
