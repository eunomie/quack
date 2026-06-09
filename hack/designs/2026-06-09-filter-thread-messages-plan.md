# Filter Thread Messages Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let users hold side-conversations inside a tracked quack thread by silently dropping messages that reply to a non-bot user or start with a configured ignore-prefix.

**Architecture:** A gateway-layer filter in `internal/discord`. `Bot.onMessage`'s tracked-thread block gains a `ignoredInThread` guard that drops side-chat before any handling, leaving no reaction. A new `[discord] ignore_prefixes` config list (default `["_ "]`) flows through `discord.New` to the `Bot`. The `session` orchestrator is untouched.

**Tech Stack:** Go, `bwmarrin/discordgo`, BurntSushi `toml`. Tests are stdlib `testing`, table-driven.

---

### Task 1: Config field + default

**Files:**
- Modify: `internal/config/config.go` (Discord struct ~line 47; defaults in `Load` ~line 124)
- Test: `internal/config/config_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/config/config_test.go`. The existing `sample` config (no `ignore_prefixes`) must default to `["_ "]`; a second config with an explicit empty list must stay empty.

```go
func TestLoad_IgnorePrefixesDefault(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(sample), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DISCORD_BOT_TOKEN", "tok")
	t.Setenv("HOME", "/home/tester")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Discord.IgnorePrefixes; len(got) != 1 || got[0] != "_ " {
		t.Errorf("IgnorePrefixes default = %q, want [\"_ \"]", got)
	}
}

func TestLoad_IgnorePrefixesExplicitEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	// Insert `ignore_prefixes = []` into sample's existing [discord] block,
	// just before the thread_auto_archive_minutes key.
	cfgText := sample[:strings.Index(sample, "thread_auto_archive_minutes")] +
		"ignore_prefixes = []\n" +
		sample[strings.Index(sample, "thread_auto_archive_minutes"):]
	if err := os.WriteFile(path, []byte(cfgText), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DISCORD_BOT_TOKEN", "tok")
	t.Setenv("HOME", "/home/tester")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Discord.IgnorePrefixes == nil || len(cfg.Discord.IgnorePrefixes) != 0 {
		t.Errorf("IgnorePrefixes = %q, want explicit empty []", cfg.Discord.IgnorePrefixes)
	}
}
```

Add `"strings"` to the test file's imports.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/config/ -run TestLoad_IgnorePrefixes -v`
Expected: FAIL — `cfg.Discord.IgnorePrefixes` undefined (compile error).

- [ ] **Step 3: Add the struct field**

In `internal/config/config.go`, add to the `Discord` struct (after `ThreadAutoArchiveMinutes`):

```go
	ThreadAutoArchiveMinutes int      `toml:"thread_auto_archive_minutes"`
	IgnorePrefixes           []string `toml:"ignore_prefixes"` // tracked-thread messages starting with one of these are kept out of the agent (nil => default ["_ "])
```

- [ ] **Step 4: Add the default in `Load`**

In `internal/config/config.go`, after the `ThreadAutoArchiveMinutes` default block (~line 126), add:

```go
	if cfg.Discord.IgnorePrefixes == nil {
		cfg.Discord.IgnorePrefixes = []string{"_ "}
	}
```

This distinguishes "unset" (nil → default) from "explicitly empty" (`[]` → preserved as a non-nil empty slice), because TOML decodes `ignore_prefixes = []` into a non-nil zero-length slice.

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/config/ -v`
Expected: PASS (all config tests, including the two new ones).

- [ ] **Step 6: Commit**

```bash
stg new filter-cfg -m "config: add discord.ignore_prefixes (default \"_ \")

Signed-off-by: Yves Brissaud <yves.brissaud@gmail.com>"
stg add internal/config/config.go internal/config/config_test.go
stg refresh
```

---

### Task 2: The `ignoredInThread` filter

**Files:**
- Modify: `internal/discord/bot.go` (`Bot` struct ~line 17; `New` ~line 36; `onMessage` tracked block ~line 66; new helper)
- Test: `internal/discord/bot_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/discord/bot_test.go`. A reply is modeled by setting `ReferencedMessage` inline, so `referencedMessage` never touches the (nil) session.

```go
// threadMsg builds a tracked-thread message: optional reply target and content.
func threadMsg(content string, repliedToBot *bool) *discordgo.MessageCreate {
	m := &discordgo.MessageCreate{Message: &discordgo.Message{Content: content}}
	if repliedToBot != nil {
		m.ReferencedMessage = &discordgo.Message{
			Author: &discordgo.User{ID: "X", Bot: *repliedToBot},
		}
	}
	return m
}

func TestIgnoredInThread(t *testing.T) {
	yes, no := true, false
	cases := []struct {
		name     string
		prefixes []string
		content  string
		reply    *bool // nil = not a reply, &true = reply to bot, &false = reply to human
		want     bool
	}{
		{"plain text feeds", []string{"_ "}, "do the thing", nil, false},
		{"reply to human dropped", []string{"_ "}, "side note", &no, true},
		{"reply to bot feeds", []string{"_ "}, "and now this", &yes, false},
		{"underscore-space prefix dropped", []string{"_ "}, "_ note to self", nil, true},
		{"markdown italic feeds", []string{"_ "}, "_italic_ word", nil, false},
		{"custom prefix respected", []string{"//"}, "// aside", nil, true},
		{"empty prefixes disables prefix match", nil, "_ note", nil, false},
		{"empty prefixes still drops reply to human", nil, "anything", &no, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := &Bot{ignorePrefixes: tc.prefixes}
			if got := b.ignoredInThread(nil, threadMsg(tc.content, tc.reply)); got != tc.want {
				t.Errorf("ignoredInThread = %v, want %v", got, tc.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/discord/ -run TestIgnoredInThread -v`
Expected: FAIL — `Bot` has no field `ignorePrefixes` and no method `ignoredInThread` (compile error).

- [ ] **Step 3: Add the `ignorePrefixes` field to `Bot`**

In `internal/discord/bot.go`, add to the `Bot` struct:

```go
type Bot struct {
	s       *discordgo.Session
	svc     *session.Service
	allowed Allow

	ignorePrefixes []string // tracked-thread messages starting with one of these are dropped (side-chat)

	mu        sync.Mutex
	roleCache map[string]map[string]bool // guildID -> role IDs that address the bot
}
```

- [ ] **Step 4: Write the helper**

In `internal/discord/bot.go`, add near `referencedMessage` (after it, ~line 174):

```go
// ignoredInThread reports whether a tracked-thread message is side-chat that
// must not reach the agent: a reply to a non-bot user, or content starting with
// a configured ignore prefix. Dropped messages get no reaction — the absence of
// quack's 👀 marker is itself the signal that it wasn't forwarded.
func (b *Bot) ignoredInThread(s *discordgo.Session, m *discordgo.MessageCreate) bool {
	if ref := referencedMessage(s, m); ref != nil && ref.Author != nil && !ref.Author.Bot {
		return true
	}
	content := strings.TrimSpace(m.Content)
	for _, p := range b.ignorePrefixes {
		if p != "" && strings.HasPrefix(content, p) {
			return true
		}
	}
	return false
}
```

(`strings` is already imported in `bot.go`.)

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./internal/discord/ -run TestIgnoredInThread -v`
Expected: PASS (all 8 subtests).

- [ ] **Step 6: Commit**

```bash
stg new filter-helper -m "discord: ignoredInThread drops side-chat in tracked threads

A reply to a non-bot user, or content starting with a configured ignore
prefix, is side-chat and must not reach the agent.

Signed-off-by: Yves Brissaud <yves.brissaud@gmail.com>"
stg add internal/discord/bot.go internal/discord/bot_test.go
stg refresh
```

---

### Task 3: Wire the filter into `onMessage` and `New`

**Files:**
- Modify: `internal/discord/bot.go` (`New` ~line 36; `onMessage` tracked block ~line 66)
- Modify: `cmd/quack/main.go` (`discord.New` call ~line 106)

- [ ] **Step 1: Add the guard in `onMessage`**

In `internal/discord/bot.go`, inside the `if b.svc.Tracked(m.ChannelID)` block, immediately after the `authorizedThread` check and before the empty-content check:

```go
	if b.svc.Tracked(m.ChannelID) {
		if !b.authorizedThread(m) {
			return
		}
		if b.ignoredInThread(s, m) {
			return
		}
		content := strings.TrimSpace(m.Content)
		atts := toAttachments(m.Attachments)
		if content == "" && len(atts) == 0 {
			return
		}
		// ... unchanged from here
```

- [ ] **Step 2: Add the `New` parameter**

In `internal/discord/bot.go`, change `New` to accept the prefixes and store them:

```go
// New builds a Bot. svcFor returns the orchestrator for a given Replier so the
// Replier can be bound to this discordgo session. ignorePrefixes are content
// prefixes that mark a tracked-thread message as side-chat (see ignoredInThread).
func New(token string, allowed Allow, ignorePrefixes []string, svcFor func(session.Replier) *session.Service) (*Bot, error) {
	s, err := discordgo.New("Bot " + token)
	if err != nil {
		return nil, err
	}
	s.Identify.Intents = discordgo.IntentsGuilds | discordgo.IntentsGuildMessages | discordgo.IntentMessageContent | discordgo.IntentsGuildMessageReactions

	b := &Bot{s: s, allowed: allowed, ignorePrefixes: ignorePrefixes}
	b.svc = svcFor(&replier{s: s})
	s.AddHandler(b.onMessage)
	s.AddHandler(b.onThreadUpdate)
	s.AddHandler(b.onReaction)
	return b, nil
}
```

- [ ] **Step 3: Update the call site in main.go**

In `cmd/quack/main.go`, change the `discord.New` call (~line 106) to pass the config list as the new third argument:

```go
	bot, err := discord.New(cfg.Discord.Token, discord.Allow{
		UserIDs:    cfg.Discord.UserIDs(),
		GuildIDs:   cfg.Discord.GuildIDs(),
		ChannelIDs: cfg.Discord.ChannelIDs(),
	}, cfg.Discord.IgnorePrefixes, func(r session.Replier) *session.Service {
```

(The closure argument and the rest of the call are unchanged.)

- [ ] **Step 4: Build and run the full test suite**

Run: `go build ./... && go test ./... && go vet ./...`
Expected: build succeeds, all tests PASS, vet clean. (A compile failure here means a `discord.New` caller was missed — search with `grep -rn "discord.New(" --include=*.go`.)

- [ ] **Step 5: Commit**

```bash
stg new filter-wire -m "discord: drop side-chat in tracked threads; wire ignore_prefixes

onMessage now skips a tracked-thread message that ignoredInThread flags,
before any handling. New plumbs config.Discord.IgnorePrefixes through.

Signed-off-by: Yves Brissaud <yves.brissaud@gmail.com>"
stg add internal/discord/bot.go cmd/quack/main.go
stg refresh
```

---

### Task 4: Docs

**Files:**
- Modify: `config.example.toml`
- Modify: `AGENTS.md`

- [ ] **Step 1: Document the config key**

In `config.example.toml`, under the `[discord]` section, add (match the file's existing comment style):

```toml
# Messages in a tracked thread that start with one of these prefixes are treated
# as side-chat and never forwarded to the agent (no reaction). Replies to a
# non-bot user are likewise ignored. Default when unset: ["_ "]. Set to [] to
# disable prefix matching (the reply rule still applies).
ignore_prefixes = ["_ "]
```

Find the `[discord]` block first: `grep -n "\[discord\]" config.example.toml`.

- [ ] **Step 2: Document the behavior in AGENTS.md**

In `AGENTS.md`, in the request-flow section, after the fast-command paragraph (the one ending "See `hack/designs/2026-06-05-fast-slash-commands.md`."), add a new paragraph:

```markdown
   A message in a tracked thread that is **side-chat** is dropped before
   `FeedThread` and never starts a turn: a Discord **reply to a non-bot user**
   (replies to quack's own messages still feed — that's the conversation), or
   content starting with one of `[discord].ignore_prefixes` (default `"_ "`,
   chosen to avoid Markdown italics; `[]` disables it). Dropped messages get no
   reaction, so the absence of quack's 👀 is the signal it wasn't forwarded
   (`ignoredInThread` in `internal/discord/bot.go`). See
   `hack/designs/2026-06-09-filter-thread-messages.md`.
```

- [ ] **Step 3: Sanity check the build is still green**

Run: `go build ./...`
Expected: success (docs-only change, but confirm nothing was edited by mistake).

- [ ] **Step 4: Commit**

```bash
stg new filter-docs -m "docs: document discord.ignore_prefixes and the thread side-chat filter

Signed-off-by: Yves Brissaud <yves.brissaud@gmail.com>"
stg add config.example.toml AGENTS.md
stg refresh
```

---

## Verification

After all tasks:

```sh
go build ./... && go test ./... && go vet ./...
```

Manual check (optional, requires a running quack): in a tracked thread, send `_ hello` → no reaction, no turn; reply to one of your own messages → no reaction; reply to a quack message → 👀 and a turn; plain message → 👀 and a turn.
