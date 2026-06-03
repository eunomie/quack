package session

import "math/rand"

// backMessages are the cheerful "I'm back" lines a rehydrated session posts to
// its thread after the quack service restarts, so you can tell at a glance that
// it survived and is live again. One is picked at random per restored session.
//
// A few carry a Tenor gif link on its own line, which Discord auto-embeds. The
// slugs are plain (no raw apostrophes) so the embed resolves cleanly.
var backMessages = []string{
	"🦆 *shakes off the dust* — back online and ready to quack!",
	"⚡ Rebooted and reconnected. Right where we left off — fire away! 🦆",
	"🔌 Plugged back in. Drop your next message whenever you're ready.",
	"🪄 *poof* — I'm back! Nothing lost, ready for your next turn.",
	"☕ Grabbed a coffee, now I'm back. What's next?",
	"🛟 Survived the restart! Resuming this session — go ahead.",
	"🎉 And we're back! This session lives on. 🦆",
	"🦆💨 Back from the void.\nhttps://tenor.com/view/duck-quack-gif-26493149",
	"😴 *wakes up* Did someone say my name?\nhttps://tenor.com/view/quack-duck-gif-10176815426187201747",
	"🏃 Sprinting back into this thread!\nhttps://tenor.com/view/duck-quack-running-run-duckling-gif-17120760958434934628",
	"🧙 Well, I'm back.\nhttps://tenor.com/view/samwise-gamgee-well-i%E2%80%99m-back-gif-1603406057439684719",
}

// randomBackMessage returns a random "I'm back" greeting. Go's global rand is
// auto-seeded, so successive restarts vary.
func randomBackMessage() string {
	return backMessages[rand.Intn(len(backMessages))]
}
