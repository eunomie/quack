# quack Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build `quack`, a Go daemon that starts Claude/Codex agent sessions from Discord — resolving/cloning a repo, creating a git worktree, and launching the agent in a tmux session with the prompt as its initial argument, reporting back in a Discord thread.

**Architecture:** A single binary connects to the Discord Gateway (outbound only). A pure-logic core (`command`, `repo`, `worktree`, `agent`, `session`) is fully unit-tested; all side effects (git, tmux, Discord, filesystem) sit behind interfaces in `internal/session` and are implemented by thin adapter packages. An orchestrator (`session.Service.Handle`) ties parse → thread → resolve/clone → worktree → launch → reply together.

**Tech Stack:** Go 1.23; `github.com/bwmarrin/discordgo` (Gateway); `github.com/BurntSushi/toml` (config); stdlib `os/exec` for git/tmux. Design spec: `hack/designs/2026-05-31-quack-design.md`.

---

## Commit convention (StGit — used by every task)

This repo uses StGit. **Never** use `git commit`; **never** add `Co-Authored-By`. End each task with a new patch:

```bash
stg new <patch-slug> -m "<subject>

Signed-off-by: Yves Brissaud <yves.brissaud@gmail.com>"
git add <files>
stg refresh
```

Each task below names its `<patch-slug>`, `<subject>`, and `<files>`.

---

## File structure

```
quack/
  go.mod                       # module github.com/eunomie/quack
  .gitignore
  cmd/quack/main.go            # wiring: config → adapters → bot → run
  internal/command/            # directive grammar (pure)
    directive.go
    directive_test.go
  internal/repo/               # repo-ref resolution + path mapping (pure)
    repo.go
    repo_test.go
  internal/worktree/           # slug + worktree path (pure)
    worktree.go
    worktree_test.go
  internal/agent/              # agent argv + effort template (pure)
    agent.go
    agent_test.go
  internal/session/            # orchestrator, Origin/context, interfaces (pure + fakes)
    origin.go
    origin_test.go
    service.go
    service_test.go
    fakes_test.go
  internal/config/             # TOML config load
    config.go
    config_test.go
  internal/gitexec/            # Git interface impl via `git`
    gitexec.go
    gitexec_integration_test.go
  internal/tmuxexec/           # Tmux interface impl via `tmux`
    tmuxexec.go
    tmuxexec_integration_test.go
  internal/discord/            # discordgo gateway + Replier impl + handler
    bot.go
    replier.go
  config.example.toml
  quack.service
  README.md
```

Each package has one responsibility. Side effects are isolated to `gitexec`, `tmuxexec`, `discord`, and the injected filesystem funcs in `session`, so the core is testable without git/tmux/Discord.

---

## Task 1: Project bootstrap

**Files:**
- Create: `go.mod`
- Create: `.gitignore`

- [ ] **Step 1: Initialize the module**

Run:
```bash
cd /home/user/dev/src/github.com/eunomie/quack
go mod init github.com/eunomie/quack
go mod edit -go=1.23
```

- [ ] **Step 2: Create `.gitignore`**

```gitignore
# build output
/quack
# local secrets / overrides
*.local.toml
config.toml
# editor
.DS_Store
```

- [ ] **Step 3: Verify the module builds (no packages yet)**

Run: `go build ./...`
Expected: exits 0 with no output (no packages to build yet).

- [ ] **Step 4: Commit** (StGit convention)
  - patch-slug: `bootstrap-module`
  - subject: `chore: bootstrap go module and gitignore`
  - files: `go.mod .gitignore`

---

## Task 2: Command directive parsing (`internal/command`)

Parses the mention-stripped message: line 1 = directive line (first token = target, rest = `key=value` flags); the remainder = prompt.

**Files:**
- Create: `internal/command/directive.go`
- Create: `internal/command/directive_test.go`

- [ ] **Step 1: Write the failing test**

`internal/command/directive_test.go`:
```go
package command

import "testing"

func TestParse_Full(t *testing.T) {
	in := "dagger/dagger agent=claude effort=high name=fix-cache base=main\nLine one of prompt.\nLine two."
	d, err := Parse(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Target != "dagger/dagger" {
		t.Errorf("Target = %q", d.Target)
	}
	if d.Agent != "claude" || d.Effort != "high" || d.Name != "fix-cache" || d.Base != "main" {
		t.Errorf("flags = %+v", d)
	}
	if d.Prompt != "Line one of prompt.\nLine two." {
		t.Errorf("Prompt = %q", d.Prompt)
	}
}

func TestParse_TargetAndPromptOnly(t *testing.T) {
	d, err := Parse("./some/dir\nDo the thing.")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Target != "./some/dir" || d.Agent != "" || d.Prompt != "Do the thing." {
		t.Errorf("got %+v", d)
	}
}

func TestParse_BlankLineSeparator(t *testing.T) {
	d, err := Parse("repo/x\n\nprompt body")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Prompt != "prompt body" {
		t.Errorf("Prompt = %q", d.Prompt)
	}
}

func TestParse_Errors(t *testing.T) {
	cases := map[string]string{
		"missing target": "",
		"no prompt":      "repo/x agent=claude",
		"unknown flag":   "repo/x bogus=1\nprompt",
		"flag no equals": "repo/x claude\nprompt",
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse(in); err == nil {
				t.Fatalf("expected error for %q", in)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/command/ -v`
Expected: FAIL — `undefined: Parse`.

- [ ] **Step 3: Write minimal implementation**

`internal/command/directive.go`:
```go
// Package command parses quack's freeform mention grammar into a Directive.
package command

import (
	"fmt"
	"strings"
)

// Directive is a parsed quack command.
type Directive struct {
	Target string // repo ref or path (required)
	Agent  string // optional
	Effort string // optional
	Name   string // optional session name
	Base   string // optional base branch
	Prompt string // required, verbatim, may be multiline
}

// UsageError is returned for malformed input; its message is safe to show the user.
type UsageError struct{ Msg string }

func (e *UsageError) Error() string { return e.Msg }

const usage = "usage: @quack <repo-or-path> [agent=] [effort=] [name=] [base=]\\n<prompt>"

// Parse parses content that has already had the bot mention stripped.
func Parse(content string) (*Directive, error) {
	first, rest, _ := strings.Cut(content, "\n")
	tokens := strings.Fields(first)
	if len(tokens) == 0 {
		return nil, &UsageError{Msg: "missing repo/path. " + usage}
	}

	d := &Directive{Target: tokens[0]}
	for _, tok := range tokens[1:] {
		key, val, ok := strings.Cut(tok, "=")
		if !ok {
			return nil, &UsageError{Msg: fmt.Sprintf("bad flag %q (want key=value). %s", tok, usage)}
		}
		switch key {
		case "agent":
			d.Agent = val
		case "effort":
			d.Effort = val
		case "name":
			d.Name = val
		case "base":
			d.Base = val
		default:
			return nil, &UsageError{Msg: fmt.Sprintf("unknown flag %q. %s", key, usage)}
		}
	}

	d.Prompt = strings.TrimSpace(rest)
	if d.Prompt == "" {
		return nil, &UsageError{Msg: "missing prompt (put it after the first line). " + usage}
	}
	return d, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/command/ -v`
Expected: PASS (all subtests).

- [ ] **Step 5: Commit** (StGit convention)
  - patch-slug: `command-parse`
  - subject: `feat(command): parse mention directive grammar`
  - files: `internal/command/directive.go internal/command/directive_test.go`

---

## Task 3: Repo reference resolution (`internal/repo`)

Classifies a target token as path vs repo-ref, parses all ref forms, and maps to a clone path + clone URL.

**Files:**
- Create: `internal/repo/repo.go`
- Create: `internal/repo/repo_test.go`

- [ ] **Step 1: Write the failing test**

`internal/repo/repo_test.go`:
```go
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/repo/ -v`
Expected: FAIL — `undefined: IsPath` (and others).

- [ ] **Step 3: Write minimal implementation**

`internal/repo/repo.go`:
```go
// Package repo classifies and resolves quack target tokens (repo refs / paths).
package repo

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Ref is a parsed git repository reference.
type Ref struct {
	Host  string
	Owner string
	Repo  string
}

// IsPath reports whether token should be treated as a filesystem path
// rather than a repo reference.
func IsPath(token string) bool {
	return strings.HasPrefix(token, "/") ||
		strings.HasPrefix(token, "~") ||
		strings.HasPrefix(token, ".")
}

// ParseRef parses owner/repo, host/owner/repo, https URLs, and scp-style ssh URLs.
func ParseRef(token string) (Ref, error) {
	s := strings.TrimSuffix(token, ".git")

	if rest, ok := strings.CutPrefix(s, "git@"); ok { // git@host:owner/repo
		host, path, ok := strings.Cut(rest, ":")
		if !ok {
			return Ref{}, fmt.Errorf("invalid ssh ref %q", token)
		}
		owner, repo, ok := strings.Cut(path, "/")
		if !ok {
			return Ref{}, fmt.Errorf("invalid ssh ref %q", token)
		}
		return Ref{Host: host, Owner: owner, Repo: repo}, nil
	}

	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "http://")

	parts := strings.Split(s, "/")
	switch len(parts) {
	case 2:
		return Ref{Host: "github.com", Owner: parts[0], Repo: parts[1]}, nil
	case 3:
		return Ref{Host: parts[0], Owner: parts[1], Repo: parts[2]}, nil
	default:
		return Ref{}, fmt.Errorf("cannot parse repo ref %q", token)
	}
}

// ClonePath returns the on-disk clone location: root/host/owner/repo.
func (r Ref) ClonePath(devSrcRoot string) string {
	return filepath.Join(devSrcRoot, r.Host, r.Owner, r.Repo)
}

// CloneURL builds the clone URL for the given protocol ("ssh" or "https").
func (r Ref) CloneURL(protocol string) string {
	if protocol == "https" {
		return fmt.Sprintf("https://%s/%s/%s.git", r.Host, r.Owner, r.Repo)
	}
	return fmt.Sprintf("git@%s:%s/%s.git", r.Host, r.Owner, r.Repo)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/repo/ -v`
Expected: PASS.

- [ ] **Step 5: Commit** (StGit convention)
  - patch-slug: `repo-resolve`
  - subject: `feat(repo): resolve repo refs to clone path and URL`
  - files: `internal/repo/repo.go internal/repo/repo_test.go`

---

## Task 4: Worktree naming + path (`internal/worktree`)

**Files:**
- Create: `internal/worktree/worktree.go`
- Create: `internal/worktree/worktree_test.go`

- [ ] **Step 1: Write the failing test**

`internal/worktree/worktree_test.go`:
```go
package worktree

import "testing"

func TestSlugify(t *testing.T) {
	cases := map[string]string{
		"Fix the cache pin bug":        "fix-the-cache-pin",
		"  Investigate FLAKY test!!!":  "investigate-flaky-test",
		"":                             "session",
		"---":                          "session",
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/worktree/ -v`
Expected: FAIL — `undefined: Slugify`.

- [ ] **Step 3: Write minimal implementation**

`internal/worktree/worktree.go`:
```go
// Package worktree derives session slugs and worktree paths.
package worktree

import (
	"regexp"
	"strings"
)

const maxSlugLen = 40

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// Slugify turns a prompt into a short, filesystem/branch-safe slug.
// Returns "session" when the prompt has no usable characters.
func Slugify(prompt string) string {
	s := nonSlug.ReplaceAllString(strings.ToLower(prompt), "-")
	s = strings.Trim(s, "-")
	if s == "" {
		return "session"
	}
	if len(s) > maxSlugLen {
		s = s[:maxSlugLen]
		if i := strings.LastIndex(s, "-"); i > 0 {
			s = s[:i]
		}
		s = strings.Trim(s, "-")
	}
	return s
}

// Path returns the worktree directory for a clone, following the
// "<clone>-worktrees/<name>" convention observed in dagger/dagger.
func Path(clonePath, name string) string {
	return clonePath + "-worktrees/" + name
}
```

Note: `Slugify("Fix the cache pin bug")` yields `fix-the-cache-pin` because the 18-char result is under `maxSlugLen`; the max-len test uses a longer input.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/worktree/ -v`
Expected: PASS.

- [ ] **Step 5: Commit** (StGit convention)
  - patch-slug: `worktree-naming`
  - subject: `feat(worktree): slugify prompts and build worktree paths`
  - files: `internal/worktree/worktree.go internal/worktree/worktree_test.go`

---

## Task 5: Agent argv + effort template (`internal/agent`)

**Files:**
- Create: `internal/agent/agent.go`
- Create: `internal/agent/agent_test.go`

- [ ] **Step 1: Write the failing test**

`internal/agent/agent_test.go`:
```go
package agent

import (
	"reflect"
	"testing"
)

func TestArgv(t *testing.T) {
	claude := Agent{Command: "claude", EffortTemplate: "--effort {effort}"}
	codex := Agent{Command: "codex", EffortTemplate: "--config model_reasoning_effort={effort}"}

	cases := []struct {
		name   string
		a      Agent
		effort string
		prompt string
		want   []string
	}{
		{"claude with effort", claude, "high", "PROMPT", []string{"claude", "--effort", "high", "PROMPT"}},
		{"codex with effort", codex, "xhigh", "PROMPT", []string{"codex", "--config", "model_reasoning_effort=xhigh", "PROMPT"}},
		{"no effort", claude, "", "PROMPT", []string{"claude", "PROMPT"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.a.Argv(tc.effort, tc.prompt)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Argv = %v, want %v", got, tc.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/agent/ -v`
Expected: FAIL — `undefined: Agent`.

- [ ] **Step 3: Write minimal implementation**

`internal/agent/agent.go`:
```go
// Package agent builds the argv used to launch a coding agent.
package agent

import "strings"

// Agent describes how to launch one coding agent.
type Agent struct {
	Command        string // executable, e.g. "claude"
	EffortTemplate string // contains "{effort}", e.g. "--effort {effort}"
}

// Argv builds the launch argv: command, optional effort flags, then the prompt
// as the final argument. effort is passed through verbatim; an empty effort or
// empty template adds no flags.
func (a Agent) Argv(effort, prompt string) []string {
	argv := []string{a.Command}
	if effort != "" && a.EffortTemplate != "" {
		rendered := strings.ReplaceAll(a.EffortTemplate, "{effort}", effort)
		argv = append(argv, strings.Fields(rendered)...)
	}
	return append(argv, prompt)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/agent/ -v`
Expected: PASS.

- [ ] **Step 5: Commit** (StGit convention)
  - patch-slug: `agent-argv`
  - subject: `feat(agent): build launch argv with pass-through effort`
  - files: `internal/agent/agent.go internal/agent/agent_test.go`

---

## Task 6: Discord origin + context rendering (`internal/session/origin.go`)

`Origin` captures the Discord context and renders it three ways (prompt header, env vars, JSON).

**Files:**
- Create: `internal/session/origin.go`
- Create: `internal/session/origin_test.go`

- [ ] **Step 1: Write the failing test**

`internal/session/origin_test.go`:
```go
package session

import (
	"encoding/json"
	"strings"
	"testing"
)

func sampleOrigin() Origin {
	return Origin{
		GuildID: "g1", ChannelID: "c1", ThreadID: "t1",
		MessageID: "m1", ReplyID: "r1",
		AuthorID: "u1", Author: "yves", CreatedAt: "2026-05-31T17:00:00Z",
	}
}

func TestPermalink(t *testing.T) {
	if got := sampleOrigin().Permalink(); got != "https://discord.com/channels/g1/c1/m1" {
		t.Errorf("Permalink = %q", got)
	}
}

func TestPromptHeader(t *testing.T) {
	h := sampleOrigin().PromptHeader()
	for _, want := range []string{"<quack-context>", "channel_id: c1", "thread_id: t1", "reply_message_id: r1", "permalink: https://discord.com/channels/g1/c1/m1", "</quack-context>"} {
		if !strings.Contains(h, want) {
			t.Errorf("header missing %q\n%s", want, h)
		}
	}
}

func TestEnvVars(t *testing.T) {
	env := sampleOrigin().EnvVars("fix-cache", "/state/sessions/fix-cache/context.json")
	want := map[string]string{
		"QUACK_CHANNEL_ID":        "c1",
		"QUACK_THREAD_ID":         "t1",
		"QUACK_REPLY_MESSAGE_ID":  "r1",
		"QUACK_SESSION_NAME":      "fix-cache",
		"QUACK_CONTEXT_FILE":      "/state/sessions/fix-cache/context.json",
	}
	got := map[string]string{}
	for _, e := range env {
		k, v, _ := strings.Cut(e, "=")
		got[k] = v
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("env %s = %q, want %q", k, got[k], v)
		}
	}
}

func TestContextJSON(t *testing.T) {
	b, err := sampleOrigin().ContextJSON("fix-cache")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]string
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m["session_name"] != "fix-cache" || m["permalink"] != "https://discord.com/channels/g1/c1/m1" || m["channel_id"] != "c1" {
		t.Errorf("json = %v", m)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/session/ -run 'Permalink|PromptHeader|EnvVars|ContextJSON' -v`
Expected: FAIL — `undefined: Origin`.

- [ ] **Step 3: Write minimal implementation**

`internal/session/origin.go`:
```go
package session

import (
	"encoding/json"
	"fmt"
)

// Origin is the captured Discord context for a session.
type Origin struct {
	GuildID   string
	ChannelID string
	ThreadID  string // set after the thread is opened
	MessageID string // the triggering message
	ReplyID   string // quack's ack message (set after posting)
	AuthorID  string
	Author    string
	CreatedAt string // RFC3339
}

// Permalink returns a Discord deep-link to the triggering message.
func (o Origin) Permalink() string {
	return fmt.Sprintf("https://discord.com/channels/%s/%s/%s", o.GuildID, o.ChannelID, o.MessageID)
}

// PromptHeader renders the <quack-context> block prepended to the agent prompt.
func (o Origin) PromptHeader() string {
	return fmt.Sprintf(`<quack-context>
guild_id: %s   channel_id: %s   thread_id: %s
message_id: %s   reply_message_id: %s
author: %s (id %s)
permalink: %s
</quack-context>`,
		o.GuildID, o.ChannelID, o.ThreadID,
		o.MessageID, o.ReplyID,
		o.Author, o.AuthorID,
		o.Permalink())
}

// EnvVars returns the QUACK_* environment for the tmux session.
func (o Origin) EnvVars(sessionName, contextFile string) []string {
	return []string{
		"QUACK_GUILD_ID=" + o.GuildID,
		"QUACK_CHANNEL_ID=" + o.ChannelID,
		"QUACK_THREAD_ID=" + o.ThreadID,
		"QUACK_MESSAGE_ID=" + o.MessageID,
		"QUACK_REPLY_MESSAGE_ID=" + o.ReplyID,
		"QUACK_PERMALINK=" + o.Permalink(),
		"QUACK_CONTEXT_FILE=" + contextFile,
		"QUACK_SESSION_NAME=" + sessionName,
	}
}

// ContextJSON marshals the structured context written to the state dir.
func (o Origin) ContextJSON(sessionName string) ([]byte, error) {
	doc := map[string]string{
		"session_name":     sessionName,
		"guild_id":         o.GuildID,
		"channel_id":       o.ChannelID,
		"thread_id":        o.ThreadID,
		"message_id":       o.MessageID,
		"reply_message_id": o.ReplyID,
		"author_id":        o.AuthorID,
		"author":           o.Author,
		"created_at":       o.CreatedAt,
		"permalink":        o.Permalink(),
	}
	return json.MarshalIndent(doc, "", "  ")
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/session/ -run 'Permalink|PromptHeader|EnvVars|ContextJSON' -v`
Expected: PASS.

- [ ] **Step 5: Commit** (StGit convention)
  - patch-slug: `session-origin`
  - subject: `feat(session): capture and render Discord origin context`
  - files: `internal/session/origin.go internal/session/origin_test.go`

---

## Task 7: Config loading (`internal/config`)

Loads TOML config, applies the `DISCORD_BOT_TOKEN` env override, and expands `~`.

**Files:**
- Create: `internal/config/config.go`
- Create: `internal/config/config_test.go`

- [ ] **Step 1: Add the TOML dependency**

Run: `go get github.com/BurntSushi/toml@v1.4.0`

- [ ] **Step 2: Write the failing test**

`internal/config/config_test.go`:
```go
package config

import (
	"os"
	"path/filepath"
	"testing"
)

const sample = `
dev_src_root = "~/dev/src"
clone_protocol = "ssh"
default_agent = "claude"

[discord]
allowed_user_id = "111"
allowed_guild_id = "222"
thread_auto_archive_minutes = 10080

[agents.claude]
command = "claude"
effort_template = "--effort {effort}"

[agents.codex]
command = "codex"
effort_template = "--config model_reasoning_effort={effort}"
`

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(sample), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DISCORD_BOT_TOKEN", "tok-from-env")
	t.Setenv("HOME", "/home/tester")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Discord.Token != "tok-from-env" {
		t.Errorf("token = %q, want env override", cfg.Discord.Token)
	}
	if cfg.DevSrcRoot != "/home/tester/dev/src" {
		t.Errorf("DevSrcRoot = %q (tilde not expanded)", cfg.DevSrcRoot)
	}
	if cfg.DefaultAgent != "claude" {
		t.Errorf("DefaultAgent = %q", cfg.DefaultAgent)
	}
	if cfg.Discord.ThreadAutoArchiveMinutes != 10080 {
		t.Errorf("archive minutes = %d", cfg.Discord.ThreadAutoArchiveMinutes)
	}
	a, ok := cfg.Agents["codex"]
	if !ok || a.Command != "codex" || a.EffortTemplate != "--config model_reasoning_effort={effort}" {
		t.Errorf("codex agent = %+v ok=%v", a, ok)
	}
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/config/ -v`
Expected: FAIL — `undefined: Load`.

- [ ] **Step 4: Write minimal implementation**

`internal/config/config.go`:
```go
// Package config loads quack's TOML configuration.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/eunomie/quack/internal/agent"
)

// Config is the full quack configuration.
type Config struct {
	DevSrcRoot    string                 `toml:"dev_src_root"`
	CloneProtocol string                 `toml:"clone_protocol"`
	DefaultAgent  string                 `toml:"default_agent"`
	StateDir      string                 `toml:"state_dir"`
	Discord       Discord                `toml:"discord"`
	Agents        map[string]agent.Agent `toml:"agents"`
}

// Discord holds Discord-specific settings.
type Discord struct {
	Token                    string `toml:"token"`
	AllowedUserID            string `toml:"allowed_user_id"`
	AllowedGuildID           string `toml:"allowed_guild_id"`
	AllowedChannelID         string `toml:"allowed_channel_id"`
	ThreadAutoArchiveMinutes int    `toml:"thread_auto_archive_minutes"`
}

// Load reads config from path, applies the DISCORD_BOT_TOKEN env override,
// expands ~ in paths, and fills defaults.
func Load(path string) (*Config, error) {
	var cfg Config
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}

	if tok := os.Getenv("DISCORD_BOT_TOKEN"); tok != "" {
		cfg.Discord.Token = tok
	}

	cfg.DevSrcRoot = expandHome(cfg.DevSrcRoot)
	cfg.StateDir = expandHome(cfg.StateDir)

	if cfg.DevSrcRoot == "" {
		cfg.DevSrcRoot = expandHome("~/dev/src")
	}
	if cfg.StateDir == "" {
		cfg.StateDir = expandHome("~/.local/state/quack")
	}
	if cfg.CloneProtocol == "" {
		cfg.CloneProtocol = "ssh"
	}
	if cfg.DefaultAgent == "" {
		cfg.DefaultAgent = "claude"
	}
	if cfg.Discord.ThreadAutoArchiveMinutes == 0 {
		cfg.Discord.ThreadAutoArchiveMinutes = 10080
	}
	return &cfg, nil
}

func expandHome(p string) string {
	if p == "" {
		return ""
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		return filepath.Join(os.Getenv("HOME"), strings.TrimPrefix(p, "~"))
	}
	return p
}
```

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./internal/config/ -v`
Expected: PASS.

- [ ] **Step 6: Commit** (StGit convention)
  - patch-slug: `config-load`
  - subject: `feat(config): load TOML config with env override`
  - files: `internal/config/config.go internal/config/config_test.go go.mod go.sum`

---

## Task 8: Orchestrator interfaces + happy path (`internal/session/service.go`)

Defines the `Git`/`Tmux`/`Replier` interfaces and `Service.Handle` for the happy path (repo ref that already exists → worktree → tmux → reply). Tested with fakes.

**Files:**
- Create: `internal/session/service.go`
- Create: `internal/session/fakes_test.go`
- Create: `internal/session/service_test.go`

- [ ] **Step 1: Write the fakes**

`internal/session/fakes_test.go`:
```go
package session

import (
	"context"
	"fmt"
)

type fakeGit struct {
	existing   map[string]bool // clonePaths that exist
	repos      map[string]bool // paths that are git repos
	primary    map[string]string
	branches   map[string]bool // "clone\x00branch" -> exists
	pathExists map[string]bool

	cloned   []string
	fetched  []string
	worktrees []string // "clone|wtPath|branch|base"
}

func newFakeGit() *fakeGit {
	return &fakeGit{
		existing: map[string]bool{}, repos: map[string]bool{}, primary: map[string]string{},
		branches: map[string]bool{}, pathExists: map[string]bool{},
	}
}

func (f *fakeGit) IsRepo(p string) bool                 { return f.repos[p] }
func (f *fakeGit) PrimaryClone(p string) (string, error) {
	if c, ok := f.primary[p]; ok {
		return c, nil
	}
	return p, nil
}
func (f *fakeGit) Exists(clonePath string) bool { return f.existing[clonePath] }
func (f *fakeGit) PathExists(p string) bool     { return f.pathExists[p] }
func (f *fakeGit) Clone(ctx context.Context, url, clonePath string) error {
	f.cloned = append(f.cloned, url+" -> "+clonePath)
	f.existing[clonePath] = true
	return nil
}
func (f *fakeGit) Fetch(ctx context.Context, clonePath string) error {
	f.fetched = append(f.fetched, clonePath)
	return nil
}
func (f *fakeGit) DefaultBranch(ctx context.Context, clonePath string) (string, error) {
	return "main", nil
}
func (f *fakeGit) BranchExists(ctx context.Context, clonePath, branch string) bool {
	return f.branches[clonePath+"\x00"+branch]
}
func (f *fakeGit) AddWorktree(ctx context.Context, clonePath, wtPath, branch, baseRef string) error {
	f.worktrees = append(f.worktrees, fmt.Sprintf("%s|%s|%s|%s", clonePath, wtPath, branch, baseRef))
	f.pathExists[wtPath] = true
	return nil
}

type fakeTmux struct {
	sessions map[string]bool
	created  []NewSessionOpts
}

func newFakeTmux() *fakeTmux { return &fakeTmux{sessions: map[string]bool{}} }

func (f *fakeTmux) SessionExists(name string) bool { return f.sessions[name] }
func (f *fakeTmux) NewSession(ctx context.Context, o NewSessionOpts) error {
	f.created = append(f.created, o)
	f.sessions[o.Name] = true
	return nil
}

type postedMsg struct{ channel, content string }

type fakeReplier struct {
	threadID    string
	openErr     error
	threads     []string // "channel|message|name"
	posts       []postedMsg
	edits       []postedMsg // channel == messageID here
	renames     []string
	nextID      int
}

func newFakeReplier() *fakeReplier { return &fakeReplier{threadID: "thread-1"} }

func (f *fakeReplier) OpenThread(ctx context.Context, channelID, messageID, name string, autoArchiveMin int) (string, error) {
	if f.openErr != nil {
		return "", f.openErr
	}
	f.threads = append(f.threads, channelID+"|"+messageID+"|"+name)
	return f.threadID, nil
}
func (f *fakeReplier) Post(ctx context.Context, channelID, content string) (string, error) {
	f.nextID++
	f.posts = append(f.posts, postedMsg{channelID, content})
	return fmt.Sprintf("msg-%d", f.nextID), nil
}
func (f *fakeReplier) Edit(ctx context.Context, channelID, messageID, content string) error {
	f.edits = append(f.edits, postedMsg{messageID, content})
	return nil
}
func (f *fakeReplier) RenameThread(ctx context.Context, threadID, name string) error {
	f.renames = append(f.renames, threadID+"|"+name)
	return nil
}

// memFS captures filesystem writes.
type memFS struct{ files map[string][]byte }

func newMemFS() *memFS { return &memFS{files: map[string][]byte{}} }
func (m *memFS) mkdirAll(string, uint32) error { return nil }
func (m *memFS) writeFile(path string, data []byte, _ uint32) error {
	m.files[path] = data
	return nil
}
```

- [ ] **Step 2: Write the failing happy-path test**

`internal/session/service_test.go`:
```go
package session

import (
	"context"
	"strings"
	"testing"

	"github.com/eunomie/quack/internal/agent"
)

func newTestService() (*Service, *fakeGit, *fakeTmux, *fakeReplier, *memFS) {
	g, tx, r, fs := newFakeGit(), newFakeTmux(), newFakeReplier(), newMemFS()
	svc := &Service{
		cfg: Config{
			DevSrcRoot:           "/src",
			CloneProtocol:        "ssh",
			DefaultAgent:         "claude",
			StateDir:             "/state",
			ThreadAutoArchiveMin: 10080,
			Agents: map[string]agent.Agent{
				"claude": {Command: "claude", EffortTemplate: "--effort {effort}"},
			},
		},
		git: g, tmux: tx, reply: r,
		mkdirAll: fs.mkdirAll, writeFile: fs.writeFile,
	}
	return svc, g, tx, r, fs
}

func baseOrigin() Origin {
	return Origin{GuildID: "g", ChannelID: "c", MessageID: "m", AuthorID: "u", Author: "yves", CreatedAt: "2026-05-31T17:00:00Z"}
}

func TestHandle_HappyPath_ExistingRepo(t *testing.T) {
	svc, g, tx, r, fs := newTestService()
	g.existing["/src/github.com/dagger/dagger"] = true

	svc.Handle(context.Background(), Request{
		Content: "dagger/dagger effort=high name=fix-cache\nFix the pin bug.",
		Origin:  baseOrigin(),
	})

	// thread opened off the triggering message, named after the session
	if len(r.threads) != 1 || !strings.HasSuffix(r.threads[0], "|fix-cache") {
		t.Fatalf("threads = %v", r.threads)
	}
	// existing repo is fetched, not cloned
	if len(g.cloned) != 0 || len(g.fetched) != 1 {
		t.Fatalf("cloned=%v fetched=%v", g.cloned, g.fetched)
	}
	// worktree created at the dagger-worktrees path on origin/main
	if len(g.worktrees) != 1 || !strings.Contains(g.worktrees[0], "/src/github.com/dagger/dagger-worktrees/fix-cache|fix-cache|origin/main") {
		t.Fatalf("worktrees = %v", g.worktrees)
	}
	// tmux session launched with prompt (incl. context header) as final argv
	if len(tx.created) != 1 {
		t.Fatalf("tmux sessions = %d", len(tx.created))
	}
	got := tx.created[0]
	if got.Name != "quack/fix-cache" || got.Dir != "/src/github.com/dagger/dagger-worktrees/fix-cache" {
		t.Errorf("session name/dir = %q %q", got.Name, got.Dir)
	}
	argv := got.Argv
	if argv[0] != "claude" || argv[1] != "--effort" || argv[2] != "high" {
		t.Errorf("argv prefix = %v", argv[:3])
	}
	final := argv[len(argv)-1]
	if !strings.Contains(final, "<quack-context>") || !strings.HasSuffix(final, "Fix the pin bug.") {
		t.Errorf("final argv (prompt) = %q", final)
	}
	// context.json written under the state dir (not the worktree)
	if _, ok := fs.files["/state/sessions/fix-cache/context.json"]; !ok {
		t.Errorf("context.json not written; files=%v", fs.files)
	}
	// ack edited with success
	if len(r.edits) == 0 || !strings.Contains(r.edits[len(r.edits)-1].content, "fix-cache") {
		t.Errorf("edits = %v", r.edits)
	}
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/session/ -run TestHandle_HappyPath -v`
Expected: FAIL — `undefined: Service` / `Config` / `Request`.

- [ ] **Step 4: Write the implementation**

`internal/session/service.go`:
```go
package session

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/eunomie/quack/internal/agent"
	"github.com/eunomie/quack/internal/command"
	"github.com/eunomie/quack/internal/repo"
	"github.com/eunomie/quack/internal/worktree"
)

// Git is the subset of git operations quack needs.
type Git interface {
	IsRepo(path string) bool
	PrimaryClone(path string) (string, error)
	Exists(clonePath string) bool
	PathExists(path string) bool
	Clone(ctx context.Context, url, clonePath string) error
	Fetch(ctx context.Context, clonePath string) error
	DefaultBranch(ctx context.Context, clonePath string) (string, error)
	BranchExists(ctx context.Context, clonePath, branch string) bool
	AddWorktree(ctx context.Context, clonePath, worktreePath, branch, baseRef string) error
}

// Tmux creates detached tmux sessions.
type Tmux interface {
	SessionExists(name string) bool
	NewSession(ctx context.Context, o NewSessionOpts) error
}

// NewSessionOpts describes a tmux session to create. Argv is exec'd directly
// (no shell), so the prompt needs no escaping.
type NewSessionOpts struct {
	Name string
	Dir  string
	Env  []string
	Argv []string
}

// Replier posts back to Discord (threads + messages).
type Replier interface {
	OpenThread(ctx context.Context, channelID, messageID, name string, autoArchiveMin int) (threadID string, err error)
	Post(ctx context.Context, channelID, content string) (messageID string, err error)
	Edit(ctx context.Context, channelID, messageID, content string) error
	RenameThread(ctx context.Context, threadID, name string) error
}

// Config is the orchestrator's runtime configuration.
type Config struct {
	DevSrcRoot           string
	CloneProtocol        string
	DefaultAgent         string
	StateDir             string
	ThreadAutoArchiveMin int
	Agents               map[string]agent.Agent
}

// Request is one parsed-but-unprocessed Discord command.
type Request struct {
	Content string // mention-stripped
	Origin  Origin // guild/channel/message/author/createdAt set; thread/reply empty
}

// Service orchestrates a session launch.
type Service struct {
	cfg   Config
	git   Git
	tmux  Tmux
	reply Replier

	mkdirAll  func(path string, perm uint32) error
	writeFile func(path string, data []byte, perm uint32) error

	locks keyedMutex
}

// New builds a Service with real filesystem writers.
func New(cfg Config, g Git, tx Tmux, r Replier) *Service {
	return &Service{
		cfg: cfg, git: g, tmux: tx, reply: r,
		mkdirAll:  func(p string, m uint32) error { return os.MkdirAll(p, os.FileMode(m)) },
		writeFile: func(p string, d []byte, m uint32) error { return os.WriteFile(p, d, os.FileMode(m)) },
	}
}

// Handle processes one command end-to-end, reporting progress/errors via Replier.
func (s *Service) Handle(ctx context.Context, req Request) {
	dir, err := command.Parse(req.Content)
	if err != nil {
		_, _ = s.reply.Post(ctx, req.Origin.ChannelID, "🦆 "+err.Error())
		return
	}

	ag, ok := s.cfg.Agents[orDefault(dir.Agent, s.cfg.DefaultAgent)]
	if !ok {
		_, _ = s.reply.Post(ctx, req.Origin.ChannelID, "🦆 unknown agent: "+orDefault(dir.Agent, s.cfg.DefaultAgent))
		return
	}

	provisional := dir.Name
	explicit := provisional != ""
	if !explicit {
		provisional = worktree.Slugify(dir.Prompt)
	}

	// Open a thread off the triggering message; fall back to the channel.
	threadID, err := s.reply.OpenThread(ctx, req.Origin.ChannelID, req.Origin.MessageID, provisional, s.cfg.ThreadAutoArchiveMin)
	if err != nil {
		threadID = req.Origin.ChannelID
	}
	req.Origin.ThreadID = threadID

	ackID, _ := s.reply.Post(ctx, threadID, "🦆 on it — preparing `"+dir.Target+"`…")
	req.Origin.ReplyID = ackID

	fail := func(msg string) {
		if ackID != "" {
			_ = s.reply.Edit(ctx, threadID, ackID, "❌ "+msg)
		} else {
			_, _ = s.reply.Post(ctx, threadID, "❌ "+msg)
		}
	}

	prep, err := s.prepare(ctx, dir, provisional, explicit)
	if err != nil {
		fail(err.Error())
		return
	}

	// Rename the thread if the session name changed (collision bump).
	if prep.name != provisional && threadID != req.Origin.ChannelID {
		_ = s.reply.RenameThread(ctx, threadID, prep.name)
	}

	// Write the structured context file under the state dir.
	sessDir := filepath.Join(s.cfg.StateDir, "sessions", prep.name)
	contextFile := filepath.Join(sessDir, "context.json")
	if data, jerr := req.Origin.ContextJSON(prep.name); jerr == nil {
		_ = s.mkdirAll(sessDir, 0o755)
		_ = s.writeFile(contextFile, data, 0o644)
	}

	// Launch the agent in tmux.
	fullPrompt := req.Origin.PromptHeader() + "\n\n" + dir.Prompt
	opts := NewSessionOpts{
		Name: "quack/" + prep.name,
		Dir:  prep.workdir,
		Env:  req.Origin.EnvVars(prep.name, contextFile),
		Argv: ag.Argv(dir.Effort, fullPrompt),
	}
	if err := s.tmux.NewSession(ctx, opts); err != nil {
		fail("launch failed: " + err.Error())
		return
	}

	fail = nil // no longer used; success below
	_ = s.reply.Edit(ctx, threadID, ackID, successMessage(prep, dir, ag))
}

type prepResult struct {
	workdir  string
	name     string
	branch   string
	isolated bool
}

// prepare resolves the target, clones if needed, and creates the worktree.
func (s *Service) prepare(ctx context.Context, dir *command.Directive, provisional string, explicit bool) (prepResult, error) {
	if repo.IsPath(dir.Target) {
		return s.prepareFromPath(ctx, dir, provisional, explicit)
	}
	return s.prepareFromRef(ctx, dir, provisional, explicit)
}

func (s *Service) prepareFromRef(ctx context.Context, dir *command.Directive, provisional string, explicit bool) (prepResult, error) {
	ref, err := repo.ParseRef(dir.Target)
	if err != nil {
		return prepResult{}, err
	}
	clonePath := ref.ClonePath(s.cfg.DevSrcRoot)

	unlock := s.locks.lock(clonePath)
	defer unlock()

	if !s.git.Exists(clonePath) {
		if err := s.git.Clone(ctx, ref.CloneURL(s.cfg.CloneProtocol), clonePath); err != nil {
			return prepResult{}, fmt.Errorf("clone %s: %w", dir.Target, err)
		}
	} else if err := s.git.Fetch(ctx, clonePath); err != nil {
		return prepResult{}, fmt.Errorf("fetch %s: %w", dir.Target, err)
	}
	return s.makeWorktree(ctx, dir, clonePath, provisional, explicit)
}

func (s *Service) prepareFromPath(ctx context.Context, dir *command.Directive, provisional string, explicit bool) (prepResult, error) {
	p := expandHome(dir.Target)
	if !s.git.PathExists(p) {
		return prepResult{}, fmt.Errorf("path does not exist: %s", dir.Target)
	}
	if !s.git.IsRepo(p) {
		// Plain directory: run in place, no worktree.
		return prepResult{workdir: p, name: provisional, isolated: false}, nil
	}
	clonePath, err := s.git.PrimaryClone(p)
	if err != nil {
		return prepResult{}, err
	}
	unlock := s.locks.lock(clonePath)
	defer unlock()
	return s.makeWorktree(ctx, dir, clonePath, provisional, explicit)
}

func (s *Service) makeWorktree(ctx context.Context, dir *command.Directive, clonePath, provisional string, explicit bool) (prepResult, error) {
	name, err := s.resolveName(ctx, clonePath, provisional, explicit)
	if err != nil {
		return prepResult{}, err
	}
	base := dir.Base
	if base == "" {
		b, err := s.git.DefaultBranch(ctx, clonePath)
		if err != nil {
			return prepResult{}, fmt.Errorf("detect default branch: %w", err)
		}
		base = b
	}
	wtPath := worktree.Path(clonePath, name)
	if err := s.git.AddWorktree(ctx, clonePath, wtPath, name, "origin/"+base); err != nil {
		return prepResult{}, fmt.Errorf("create worktree: %w", err)
	}
	return prepResult{workdir: wtPath, name: name, branch: name, isolated: true}, nil
}

// resolveName finds a free name. An explicit name that collides is an error;
// a generated name is bumped (-2, -3, ...).
func (s *Service) resolveName(ctx context.Context, clonePath, base string, explicit bool) (string, error) {
	candidate := base
	for i := 2; ; i++ {
		if s.free(ctx, clonePath, candidate) {
			return candidate, nil
		}
		if explicit {
			return "", fmt.Errorf("name %q already exists (worktree/branch/tmux) — pick another", base)
		}
		candidate = fmt.Sprintf("%s-%d", base, i)
	}
}

func (s *Service) free(ctx context.Context, clonePath, name string) bool {
	return !s.git.PathExists(worktree.Path(clonePath, name)) &&
		!s.git.BranchExists(ctx, clonePath, name) &&
		!s.tmux.SessionExists("quack/"+name)
}

func successMessage(p prepResult, dir *command.Directive, ag agent.Agent) string {
	effort := dir.Effort
	if effort == "" {
		effort = "(default)"
	}
	iso := "worktree branch `" + p.branch + "`"
	if !p.isolated {
		iso = "_(plain directory — no worktree/isolation)_"
	}
	return fmt.Sprintf("🦆 session **%s** is up\n• dir: `%s`\n• %s\n• agent: `%s` · effort: `%s`\n• attach: `tmux attach -t quack/%s`",
		p.name, p.workdir, iso, ag.Command, effort, p.name)
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// keyedMutex serializes work per clone path.
type keyedMutex struct {
	mu sync.Mutex
	m  map[string]*sync.Mutex
}

func (k *keyedMutex) lock(key string) func() {
	k.mu.Lock()
	if k.m == nil {
		k.m = map[string]*sync.Mutex{}
	}
	mu, ok := k.m[key]
	if !ok {
		mu = &sync.Mutex{}
		k.m[key] = mu
	}
	k.mu.Unlock()
	mu.Lock()
	return mu.Unlock
}
```

Add `expandHome` to the `session` package (used by `prepareFromPath`). Append to `internal/session/origin.go`:
```go
import-note: add "os", "path/filepath", "strings" to origin.go imports.

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		return filepath.Join(os.Getenv("HOME"), strings.TrimPrefix(p, "~"))
	}
	return p
}
```

> Implementation note: `origin.go` currently imports only `encoding/json` and `fmt`. When adding `expandHome`, update its import block to `("encoding/json"; "fmt"; "os"; "path/filepath"; "strings")`.

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./internal/session/ -run TestHandle_HappyPath -v`
Expected: PASS.

- [ ] **Step 6: Run the whole package**

Run: `go test ./internal/session/ -v`
Expected: PASS (origin tests + happy path).

- [ ] **Step 7: Commit** (StGit convention)
  - patch-slug: `session-orchestrator`
  - subject: `feat(session): orchestrate launch happy path with fakes`
  - files: `internal/session/service.go internal/session/fakes_test.go internal/session/service_test.go`

---

## Task 9: Orchestrator edge cases (`internal/session`)

Adds tests (and verifies the Task 8 implementation handles them) for: clone-needed, plain directory, explicit-name collision, generated-name bump, and unknown agent.

**Files:**
- Modify: `internal/session/service_test.go` (append tests)

- [ ] **Step 1: Write the failing/edge tests**

Append to `internal/session/service_test.go`:
```go
func TestHandle_ClonesMissingRepo(t *testing.T) {
	svc, g, _, _, _ := newTestService()
	// repo not in g.existing → must clone
	svc.Handle(context.Background(), Request{
		Content: "dagger/dagger\nDo it.",
		Origin:  baseOrigin(),
	})
	if len(g.cloned) != 1 || !strings.Contains(g.cloned[0], "git@github.com:dagger/dagger.git -> /src/github.com/dagger/dagger") {
		t.Fatalf("cloned = %v", g.cloned)
	}
}

func TestHandle_PlainDirectory(t *testing.T) {
	svc, g, tx, _, _ := newTestService()
	g.pathExists["/home/tester/scratch"] = true
	// not a repo
	t.Setenv("HOME", "/home/tester")
	svc.Handle(context.Background(), Request{
		Content: "/home/tester/scratch\nPoke around.",
		Origin:  baseOrigin(),
	})
	if len(g.worktrees) != 0 {
		t.Errorf("expected no worktree for plain dir, got %v", g.worktrees)
	}
	if len(tx.created) != 1 || tx.created[0].Dir != "/home/tester/scratch" {
		t.Errorf("tmux dir = %v", tx.created)
	}
}

func TestHandle_ExplicitNameCollision(t *testing.T) {
	svc, g, _, r, _ := newTestService()
	g.existing["/src/github.com/dagger/dagger"] = true
	g.pathExists["/src/github.com/dagger/dagger-worktrees/taken"] = true
	svc.Handle(context.Background(), Request{
		Content: "dagger/dagger name=taken\nGo.",
		Origin:  baseOrigin(),
	})
	if len(g.worktrees) != 0 {
		t.Errorf("should not create worktree on explicit collision: %v", g.worktrees)
	}
	last := r.edits[len(r.edits)-1].content
	if !strings.Contains(last, "already exists") {
		t.Errorf("expected collision error, got %q", last)
	}
}

func TestHandle_GeneratedNameBumps(t *testing.T) {
	svc, g, _, _, _ := newTestService()
	g.existing["/src/github.com/dagger/dagger"] = true
	g.pathExists["/src/github.com/dagger/dagger-worktrees/fix-the-bug"] = true // first slug taken
	svc.Handle(context.Background(), Request{
		Content: "dagger/dagger\nFix the bug.",
		Origin:  baseOrigin(),
	})
	if len(g.worktrees) != 1 || !strings.Contains(g.worktrees[0], "dagger-worktrees/fix-the-bug-2|fix-the-bug-2|") {
		t.Fatalf("expected bumped name, got %v", g.worktrees)
	}
}

func TestHandle_UnknownAgent(t *testing.T) {
	svc, _, tx, r, _ := newTestService()
	svc.Handle(context.Background(), Request{
		Content: "dagger/dagger agent=bogus\nGo.",
		Origin:  baseOrigin(),
	})
	if len(tx.created) != 0 {
		t.Errorf("should not launch for unknown agent")
	}
	if len(r.posts) == 0 || !strings.Contains(r.posts[len(r.posts)-1].content, "unknown agent") {
		t.Errorf("posts = %v", r.posts)
	}
}

func TestHandle_UsageErrorRepliesInChannel(t *testing.T) {
	svc, _, _, r, _ := newTestService()
	svc.Handle(context.Background(), Request{Content: "", Origin: baseOrigin()})
	if len(r.threads) != 0 {
		t.Errorf("should not open a thread on usage error")
	}
	if len(r.posts) != 1 || !strings.Contains(r.posts[0].content, "missing repo") {
		t.Errorf("posts = %v", r.posts)
	}
}
```

- [ ] **Step 2: Run the tests**

Run: `go test ./internal/session/ -v`
Expected: PASS for all. If `TestHandle_GeneratedNameBumps` reveals the first slug differs from `fix-the-bug`, adjust the pre-seeded `pathExists` key to match `worktree.Slugify("Fix the bug.")` — verify with a quick `go test -run TestSlugify` mental check (`"Fix the bug."` → `fix-the-bug`).

- [ ] **Step 3: Commit** (StGit convention)
  - patch-slug: `session-edge-cases`
  - subject: `test(session): cover clone, plain dir, collisions, errors`
  - files: `internal/session/service_test.go`

---

## Task 10: Git adapter (`internal/gitexec`)

Implements `session.Git` by shelling out to `git`. Includes an opt-in integration test.

**Files:**
- Create: `internal/gitexec/gitexec.go`
- Create: `internal/gitexec/gitexec_integration_test.go`

- [ ] **Step 1: Write the implementation**

`internal/gitexec/gitexec.go`:
```go
// Package gitexec implements session.Git using the git CLI.
package gitexec

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Git runs git commands on the host.
type Git struct{}

// New returns a Git adapter.
func New() *Git { return &Git{} }

func (Git) run(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// PathExists reports whether path exists on disk.
func (Git) PathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Exists reports whether clonePath is a git working tree.
func (g Git) Exists(clonePath string) bool {
	return g.PathExists(filepath.Join(clonePath, ".git")) || g.IsRepo(clonePath)
}

// IsRepo reports whether path is inside a git repository.
func (g Git) IsRepo(path string) bool {
	if !g.PathExists(path) {
		return false
	}
	out, err := g.run(context.Background(), path, "rev-parse", "--is-inside-work-tree")
	return err == nil && out == "true"
}

// PrimaryClone resolves path to the main worktree (parent of the common git dir).
func (g Git) PrimaryClone(path string) (string, error) {
	common, err := g.run(context.Background(), path, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", err
	}
	return filepath.Dir(common), nil // strip trailing ".git"
}

// Clone clones url into clonePath, creating parent directories.
func (g Git) Clone(ctx context.Context, url, clonePath string) error {
	if err := os.MkdirAll(filepath.Dir(clonePath), 0o755); err != nil {
		return err
	}
	_, err := g.run(ctx, "", "clone", url, clonePath)
	return err
}

// Fetch fetches origin in clonePath.
func (g Git) Fetch(ctx context.Context, clonePath string) error {
	_, err := g.run(ctx, clonePath, "fetch", "origin")
	return err
}

// DefaultBranch returns origin's default branch (e.g. "main").
func (g Git) DefaultBranch(ctx context.Context, clonePath string) (string, error) {
	out, err := g.run(ctx, clonePath, "rev-parse", "--abbrev-ref", "origin/HEAD")
	if err != nil {
		// Fallback: ask the remote.
		out, err = g.run(ctx, clonePath, "symbolic-ref", "refs/remotes/origin/HEAD")
		if err != nil {
			return "", err
		}
	}
	return strings.TrimPrefix(strings.TrimPrefix(out, "origin/"), "refs/remotes/origin/"), nil
}

// BranchExists reports whether a local branch exists.
func (g Git) BranchExists(ctx context.Context, clonePath, branch string) bool {
	_, err := g.run(ctx, clonePath, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	return err == nil
}

// AddWorktree creates a new worktree on a new branch from baseRef.
func (g Git) AddWorktree(ctx context.Context, clonePath, worktreePath, branch, baseRef string) error {
	_, err := g.run(ctx, clonePath, "worktree", "add", "-b", branch, worktreePath, baseRef)
	return err
}
```

- [ ] **Step 2: Write the opt-in integration test**

`internal/gitexec/gitexec_integration_test.go`:
```go
package gitexec

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestWorktreeAdd_Integration(t *testing.T) {
	if os.Getenv("QUACK_INTEGRATION") == "" {
		t.Skip("set QUACK_INTEGRATION=1 to run (needs git)")
	}
	dir := t.TempDir()
	clone := filepath.Join(dir, "repo")

	// Create a tiny repo with a default branch and a commit.
	mustRun(t, "", "git", "init", "-b", "main", clone)
	mustRun(t, clone, "git", "config", "user.email", "t@example.com")
	mustRun(t, clone, "git", "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(clone, "f.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, clone, "git", "add", "-A")
	mustRun(t, clone, "git", "commit", "-m", "init")

	g := New()
	if !g.IsRepo(clone) {
		t.Fatal("IsRepo = false")
	}
	wt := filepath.Join(dir, "repo-worktrees", "feature")
	if err := g.AddWorktree(context.Background(), clone, wt, "feature", "main"); err != nil {
		t.Fatalf("AddWorktree: %v", err)
	}
	if !g.PathExists(filepath.Join(wt, "f.txt")) {
		t.Fatal("worktree file missing")
	}
	if !g.BranchExists(context.Background(), clone, "feature") {
		t.Fatal("branch not created")
	}
}

func mustRun(t *testing.T, dir, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
}
```

- [ ] **Step 3: Run unit build + opt-in integration**

Run: `go build ./internal/gitexec/`
Expected: exits 0.
Run: `QUACK_INTEGRATION=1 go test ./internal/gitexec/ -run Integration -v`
Expected: PASS (requires `git`).

- [ ] **Step 4: Commit** (StGit convention)
  - patch-slug: `gitexec-adapter`
  - subject: `feat(gitexec): implement Git via git CLI`
  - files: `internal/gitexec/gitexec.go internal/gitexec/gitexec_integration_test.go`

---

## Task 11: Tmux adapter (`internal/tmuxexec`)

Implements `session.Tmux` via the `tmux` CLI, passing argv directly (no shell) and setting per-session env + `remain-on-exit`.

**Files:**
- Create: `internal/tmuxexec/tmuxexec.go`
- Create: `internal/tmuxexec/tmuxexec_integration_test.go`

- [ ] **Step 1: Write the implementation**

`internal/tmuxexec/tmuxexec.go`:
```go
// Package tmuxexec implements session.Tmux using the tmux CLI.
package tmuxexec

import (
	"context"
	"os/exec"

	"github.com/eunomie/quack/internal/session"
)

// Tmux runs tmux commands on the host.
type Tmux struct {
	// Socket, if set, passes `-L <socket>` (used by tests).
	Socket string
}

// New returns a Tmux adapter on the default server.
func New() *Tmux { return &Tmux{} }

func (t Tmux) base() []string {
	if t.Socket != "" {
		return []string{"-L", t.Socket}
	}
	return nil
}

// SessionExists reports whether a tmux session with the given name exists.
func (t Tmux) SessionExists(name string) bool {
	args := append(t.base(), "has-session", "-t", "="+name)
	return exec.Command("tmux", args...).Run() == nil
}

// NewSession creates a detached session running o.Argv in o.Dir with o.Env,
// then enables remain-on-exit so the pane is inspectable after the agent exits.
func (t Tmux) NewSession(ctx context.Context, o session.NewSessionOpts) error {
	args := append(t.base(), "new-session", "-d", "-s", o.Name, "-c", o.Dir)
	for _, e := range o.Env {
		args = append(args, "-e", e)
	}
	args = append(args, "--")
	args = append(args, o.Argv...)

	if out, err := exec.CommandContext(ctx, "tmux", args...).CombinedOutput(); err != nil {
		return wrap(err, out)
	}

	remain := append(t.base(), "set-option", "-t", o.Name, "remain-on-exit", "on")
	_ = exec.CommandContext(ctx, "tmux", remain...).Run()
	return nil
}

func wrap(err error, out []byte) error {
	if len(out) == 0 {
		return err
	}
	return &cmdError{err: err, out: string(out)}
}

type cmdError struct {
	err error
	out string
}

func (e *cmdError) Error() string { return e.err.Error() + ": " + e.out }
```

> Note: `Tmux` references `session.NewSessionOpts`, so this package imports `internal/session`. `session` must **not** import `tmuxexec` (it only defines the interface) — keep the dependency one-way.

- [ ] **Step 2: Write the opt-in integration test**

`internal/tmuxexec/tmuxexec_integration_test.go`:
```go
package tmuxexec

import (
	"context"
	"os"
	"os/exec"
	"testing"

	"github.com/eunomie/quack/internal/session"
)

func TestNewSession_Integration(t *testing.T) {
	if os.Getenv("QUACK_INTEGRATION") == "" {
		t.Skip("set QUACK_INTEGRATION=1 to run (needs tmux)")
	}
	sock := "quacktest"
	tx := &Tmux{Socket: sock}
	defer exec.Command("tmux", "-L", sock, "kill-server").Run()

	name := "quack/itest"
	if tx.SessionExists(name) {
		t.Fatal("session should not exist yet")
	}
	err := tx.NewSession(context.Background(), session.NewSessionOpts{
		Name: name,
		Dir:  t.TempDir(),
		Env:  []string{"QUACK_SESSION_NAME=itest"},
		Argv: []string{"sleep", "30"},
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if !tx.SessionExists(name) {
		t.Fatal("session should exist after creation")
	}
}
```

- [ ] **Step 3: Build + opt-in integration**

Run: `go build ./internal/tmuxexec/`
Expected: exits 0.
Run: `QUACK_INTEGRATION=1 go test ./internal/tmuxexec/ -run Integration -v`
Expected: PASS (requires `tmux`).

- [ ] **Step 4: Commit** (StGit convention)
  - patch-slug: `tmuxexec-adapter`
  - subject: `feat(tmuxexec): implement Tmux via tmux CLI`
  - files: `internal/tmuxexec/tmuxexec.go internal/tmuxexec/tmuxexec_integration_test.go`

---

## Task 12: Discord adapter + handler (`internal/discord`)

Connects to the Gateway, enforces the allowlist, builds a `session.Request`, and implements `session.Replier` via discordgo (threads + messages). This layer is thin and exercised manually (no live-Discord unit tests).

**Files:**
- Create: `internal/discord/replier.go`
- Create: `internal/discord/bot.go`

- [ ] **Step 1: Add discordgo**

Run: `go get github.com/bwmarrin/discordgo@v0.28.1`

- [ ] **Step 2: Write the Replier**

`internal/discord/replier.go`:
```go
package discord

import (
	"context"

	"github.com/bwmarrin/discordgo"
)

// replier implements session.Replier using discordgo.
type replier struct {
	s *discordgo.Session
}

func (r *replier) OpenThread(ctx context.Context, channelID, messageID, name string, autoArchiveMin int) (string, error) {
	th, err := r.s.MessageThreadStartComplex(channelID, messageID, &discordgo.ThreadStart{
		Name:                name,
		AutoArchiveDuration: autoArchiveMin,
	})
	if err != nil {
		return "", err
	}
	return th.ID, nil
}

func (r *replier) Post(ctx context.Context, channelID, content string) (string, error) {
	m, err := r.s.ChannelMessageSend(channelID, content)
	if err != nil {
		return "", err
	}
	return m.ID, nil
}

func (r *replier) Edit(ctx context.Context, channelID, messageID, content string) error {
	_, err := r.s.ChannelMessageEdit(channelID, messageID, content)
	return err
}

func (r *replier) RenameThread(ctx context.Context, threadID, name string) error {
	_, err := r.s.ChannelEdit(threadID, &discordgo.ChannelEdit{Name: name})
	return err
}
```

- [ ] **Step 3: Write the bot/handler**

`internal/discord/bot.go`:
```go
// Package discord wires the Discord Gateway to the session orchestrator.
package discord

import (
	"context"
	"log"
	"strings"

	"github.com/bwmarrin/discordgo"

	"github.com/eunomie/quack/internal/session"
)

// Bot owns the discordgo session and dispatches commands to the orchestrator.
type Bot struct {
	s       *discordgo.Session
	svc     *session.Service
	allowed Allow
}

// Allow is the authorization allowlist.
type Allow struct {
	UserID    string
	GuildID   string
	ChannelID string // optional ("" = any channel in the guild)
}

// New builds a Bot. svcFor returns the orchestrator for a given Replier so the
// Replier can be bound to this discordgo session.
func New(token string, allowed Allow, svcFor func(session.Replier) *session.Service) (*Bot, error) {
	s, err := discordgo.New("Bot " + token)
	if err != nil {
		return nil, err
	}
	s.Identify.Intents = discordgo.IntentsGuilds | discordgo.IntentsGuildMessages | discordgo.IntentMessageContent

	b := &Bot{s: s, allowed: allowed}
	b.svc = svcFor(&replier{s: s})
	s.AddHandler(b.onMessage)
	return b, nil
}

// Run opens the gateway connection and blocks until ctx is cancelled.
func (b *Bot) Run(ctx context.Context) error {
	if err := b.s.Open(); err != nil {
		return err
	}
	defer b.s.Close()
	log.Printf("quack connected")
	<-ctx.Done()
	return nil
}

func (b *Bot) onMessage(s *discordgo.Session, m *discordgo.MessageCreate) {
	if m.Author == nil || m.Author.Bot {
		return
	}
	if !mentions(m, s.State.User.ID) {
		return
	}
	if !b.authorized(m) {
		_, _ = s.ChannelMessageSend(m.ChannelID, "🦆 not authorized")
		return
	}

	content := stripMention(m.Content, s.State.User.ID)
	created := m.Timestamp.Format("2006-01-02T15:04:05Z07:00")

	req := session.Request{
		Content: content,
		Origin: session.Origin{
			GuildID:   m.GuildID,
			ChannelID: m.ChannelID,
			MessageID: m.ID,
			AuthorID:  m.Author.ID,
			Author:    m.Author.Username,
			CreatedAt: created,
		},
	}
	go b.svc.Handle(context.Background(), req)
}

func (b *Bot) authorized(m *discordgo.MessageCreate) bool {
	if b.allowed.UserID != "" && m.Author.ID != b.allowed.UserID {
		return false
	}
	if b.allowed.GuildID != "" && m.GuildID != b.allowed.GuildID {
		return false
	}
	if b.allowed.ChannelID != "" && m.ChannelID != b.allowed.ChannelID {
		return false
	}
	return true
}

func mentions(m *discordgo.MessageCreate, botID string) bool {
	for _, u := range m.Mentions {
		if u.ID == botID {
			return true
		}
	}
	return false
}

func stripMention(content, botID string) string {
	content = strings.ReplaceAll(content, "<@"+botID+">", "")
	content = strings.ReplaceAll(content, "<@!"+botID+">", "")
	return strings.TrimSpace(content)
}
```

- [ ] **Step 4: Verify it builds**

Run: `go build ./internal/discord/`
Expected: exits 0.

- [ ] **Step 5: Commit** (StGit convention)
  - patch-slug: `discord-gateway`
  - subject: `feat(discord): gateway handler, allowlist, replier`
  - files: `internal/discord/bot.go internal/discord/replier.go go.mod go.sum`

---

## Task 13: Main wiring (`cmd/quack/main.go`)

**Files:**
- Create: `cmd/quack/main.go`

- [ ] **Step 1: Write main**

`cmd/quack/main.go`:
```go
// Command quack runs the Discord bot that starts agent sessions.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/eunomie/quack/internal/config"
	"github.com/eunomie/quack/internal/discord"
	"github.com/eunomie/quack/internal/gitexec"
	"github.com/eunomie/quack/internal/session"
	"github.com/eunomie/quack/internal/tmuxexec"
)

func main() {
	defaultCfg := filepath.Join(os.Getenv("HOME"), ".config", "quack", "config.toml")
	cfgPath := flag.String("config", defaultCfg, "path to config.toml")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if cfg.Discord.Token == "" {
		log.Fatal("no Discord token (set [discord].token or DISCORD_BOT_TOKEN)")
	}

	scfg := session.Config{
		DevSrcRoot:           cfg.DevSrcRoot,
		CloneProtocol:        cfg.CloneProtocol,
		DefaultAgent:         cfg.DefaultAgent,
		StateDir:             cfg.StateDir,
		ThreadAutoArchiveMin: cfg.Discord.ThreadAutoArchiveMinutes,
		Agents:               cfg.Agents,
	}

	g := gitexec.New()
	tx := tmuxexec.New()

	bot, err := discord.New(cfg.Discord.Token, discord.Allow{
		UserID:    cfg.Discord.AllowedUserID,
		GuildID:   cfg.Discord.AllowedGuildID,
		ChannelID: cfg.Discord.AllowedChannelID,
	}, func(r session.Replier) *session.Service {
		return session.New(scfg, g, tx, r)
	})
	if err != nil {
		log.Fatalf("discord: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := bot.Run(ctx); err != nil {
		log.Fatalf("run: %v", err)
	}
}
```

- [ ] **Step 2: Build the whole project**

Run: `go build ./...`
Expected: exits 0, produces no stray binary in packages.
Run: `go build -o quack ./cmd/quack`
Expected: produces `./quack`.

- [ ] **Step 3: Commit** (StGit convention)
  - patch-slug: `cmd-main`
  - subject: `feat(cmd): wire config, adapters, and gateway in main`
  - files: `cmd/quack/main.go`

---

## Task 14: Ops files (config example, systemd unit, README)

**Files:**
- Create: `config.example.toml`
- Create: `quack.service`
- Create: `README.md`

- [ ] **Step 1: Write `config.example.toml`**

```toml
# Copy to ~/.config/quack/config.toml and fill in.
# The token may instead come from the DISCORD_BOT_TOKEN env var (preferred).
dev_src_root   = "~/dev/src"
state_dir      = "~/.local/state/quack"
clone_protocol = "ssh"        # or "https"
default_agent  = "claude"

[discord]
# token = "..."               # prefer DISCORD_BOT_TOKEN env instead
allowed_user_id  = "YOUR_DISCORD_USER_ID"
allowed_guild_id = "YOUR_GUILD_ID"
# allowed_channel_id = "OPTIONAL_CHANNEL_ID"
thread_auto_archive_minutes = 10080   # 7 days

[agents.claude]
command         = "claude"
effort_template = "--effort {effort}"   # confirm the real claude flag

[agents.codex]
command         = "codex"
effort_template = "--config model_reasoning_effort={effort}"
```

- [ ] **Step 2: Write `quack.service`**

```ini
[Unit]
Description=quack Discord agent-launcher bot
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
# Token kept out of the unit; use an EnvironmentFile with chmod 600.
EnvironmentFile=%h/.config/quack/env
# Ensure agent CLIs + git + tmux are on PATH for the service.
Environment=PATH=%h/.local/bin:/usr/local/bin:/usr/bin:/bin
ExecStart=%h/.local/bin/quack --config %h/.config/quack/config.toml
Restart=always
RestartSec=5

[Install]
WantedBy=default.target
```

> `%h/.config/quack/env` contains `DISCORD_BOT_TOKEN=...` and is `chmod 600`. Install as a **user** service: `systemctl --user enable --now quack`.

- [ ] **Step 3: Write `README.md`**

```markdown
# quack 🦆

Start Claude/Codex agent sessions from Discord. quack runs on your machine,
connects out to the Discord Gateway (no inbound/port-forwarding/tailscale),
and on a mention resolves a repo, creates a git worktree, and launches the
agent in a tmux session with your prompt.

## Usage

    @quack <repo-or-path> [agent=claude] [effort=high] [name=fix-x] [base=main]
    <your multiline prompt>

Example:

    @quack dagger/dagger effort=high name=fix-cache
    Investigate the directory cache pin bug; reproduce with a failing test.

quack replies in a per-session thread with the worktree path and
`tmux attach -t quack/<name>`.

## Setup

1. Create a Discord application + bot; enable the **Message Content** intent.
2. Invite the bot to your private server.
3. `cp config.example.toml ~/.config/quack/config.toml` and fill in IDs.
4. Put `DISCORD_BOT_TOKEN=...` in `~/.config/quack/env` (chmod 600).
5. `go build -o ~/.local/bin/quack ./cmd/quack`
6. Install `quack.service` under `~/.config/systemd/user/` and
   `systemctl --user enable --now quack`.

## Design

See `hack/designs/2026-05-31-quack-design.md`.

## Tests

    go test ./...                              # unit tests
    QUACK_INTEGRATION=1 go test ./...          # + git/tmux integration (needs git, tmux)
```

- [ ] **Step 4: Commit** (StGit convention)
  - patch-slug: `ops-files`
  - subject: `docs: add config example, systemd unit, and README`
  - files: `config.example.toml quack.service README.md`

---

## Task 15: Final verification

- [ ] **Step 1: Vet + full unit test run**

Run:
```bash
go vet ./...
go test ./...
```
Expected: vet clean; all unit tests PASS (integration tests skip without `QUACK_INTEGRATION`).

- [ ] **Step 2: Integration run (if git + tmux present)**

Run: `QUACK_INTEGRATION=1 go test ./...`
Expected: PASS including gitexec/tmuxexec integration.

- [ ] **Step 3: Build the binary**

Run: `go build -o quack ./cmd/quack && ./quack --help`
Expected: prints flag usage (`-config`), exits.

- [ ] **Step 4: Review the patch series**

Run: `stg series`
Expected: a clean ordered series of all task patches, each signed off.

---

## Self-review notes (author)

**Spec coverage** — every spec section maps to a task:
- Outbound Gateway + intents → Task 12; systemd → Task 14.
- Security allowlist (before side effects) → Task 12 (`authorized` checked before `Handle`); usage handled in Task 8/9.
- Command grammar + defaults → Task 2 (parse) + Task 8 (defaults: agent, name slug, base).
- Repo resolution (refs, paths, primary clone, clone-if-missing) → Tasks 3, 8, 9, 10.
- Worktree convention + collision rules → Tasks 4, 8, 9.
- Agent launch via argv + remain-on-exit + env → Tasks 5, 11; ops/PATH/creds → Task 14.
- Effort pass-through template → Tasks 5, 7.
- Discord context (header + context.json + env + thread + rename + fallback) → Tasks 6, 8, 12.
- Reply UX (ack → edit, no silent failures) → Task 8 (`fail` edits ack; every stage reports).
- Testing strategy (pure unit + faked orchestration + opt-in integration) → Tasks 2–11, 15.

**Type consistency** — interface method sets in `session` (Task 8) match the fakes (Task 8) and the adapters (`gitexec` Task 10, `tmuxexec` Task 11, `discord` Task 12). `NewSessionOpts`, `Origin`, `Config`, `Request`, `Service.New`, and `agent.Agent.Argv` signatures are used identically across tasks.

**Known confirm-at-impl item:** the real `claude` effort flag string in `config.example.toml`/agent config is a value, not a structural unknown; adjust once confirmed on the host. The Claude remote-web surface needs no special flag (host is configured for remote control everywhere).
