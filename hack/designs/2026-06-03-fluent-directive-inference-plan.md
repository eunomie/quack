# Fluent Directive Inference Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add an opt-in fluent path — a Discord mention starting with `! ` is handed to a low-effort one-shot agent that reads the request plus recent channel context and emits the launch directive, then quack launches exactly as it does today.

**Architecture:** The infer step produces the *same* `command.Directive` struct the plain grammar produces, so the entire downstream launch flow is shared. `Service.Handle` gains one early branch; the post-directive tail is extracted into a shared `run` method. A new read-only one-shot (`Driver.OneShot`) drives the inference; a new `History` port fetches recent Discord messages to inline as context.

**Tech Stack:** Go, discordgo, BurntSushi/toml. Unit-tested with the existing in-package fakes (`internal/session/fakes_test.go`) — no real agent or network.

**Design doc:** `hack/designs/2026-06-03-fluent-directive-inference.md`

---

## File Structure

| File | Change | Responsibility |
|------|--------|----------------|
| `internal/config/config.go` | modify | new `infer_agent` / `infer_effort` / `infer_history_limit` fields + defaults |
| `internal/config/config_test.go` | modify | test the new defaults |
| `cmd/quack/main.go` | modify | plumb the new config fields; wire the `History` port |
| `internal/agentproc/driver.go` | modify | add `OneShot` to the `Driver` interface |
| `internal/agentproc/claude.go` | modify | implement `OneShot`; re-express `SuggestName` on it |
| `internal/agentproc/codex.go` | modify | implement `OneShot`; re-express `SuggestName` on it |
| `internal/session/service.go` | modify | `Message` + `History` types, `history` field + `UseHistory`; `Config` fields; extract `run`; route `! ` |
| `internal/session/infer.go` | create | fluent path: prefix detection, history fetch, infer one-shot, JSON parse/map, `handleFluent` |
| `internal/session/infer_test.go` | create | unit tests for the pure helpers + `inferDirective` + fluent routing |
| `internal/session/headless_test.go` | modify | `fakeDriver.OneShot` |
| `internal/session/fakes_test.go` | modify | `fakeReplier.RecentMessages` |
| `internal/session/service_test.go` | modify | set `svc.history` in `newTestService` |
| `internal/discord/replier.go` | modify | implement `RecentMessages` |
| `config.example.toml` | modify | document the new knobs |
| `AGENTS.md` | modify | document the fluent path |

---

## Task 1: Config knobs

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`
- Modify: `internal/session/service.go` (add `Config` fields)
- Modify: `cmd/quack/main.go` (plumb fields)

- [ ] **Step 1: Write the failing test**

Add to `internal/config/config_test.go`:

```go
func TestLoad_InferDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("name_agent = \"codex\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.InferEffort != "medium" {
		t.Errorf("InferEffort = %q, want medium", cfg.InferEffort)
	}
	if cfg.InferHistoryLimit != 20 {
		t.Errorf("InferHistoryLimit = %d, want 20", cfg.InferHistoryLimit)
	}
	if cfg.InferAgent != "codex" {
		t.Errorf("InferAgent = %q, want codex (defaults to name_agent)", cfg.InferAgent)
	}
}
```

Ensure `internal/config/config_test.go` imports `os`, `path/filepath`, `testing` (add any that are missing).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/config -run TestLoad_InferDefaults`
Expected: FAIL — `cfg.InferEffort` undefined (compile error).

- [ ] **Step 3: Add the fields and defaults**

In `internal/config/config.go`, add to the `Config` struct (after `NameAgent`):

```go
	NameAgent         string                 `toml:"name_agent"`
	InferAgent        string                 `toml:"infer_agent"`         // agent for the fluent `! ` infer step (default: name_agent)
	InferEffort       string                 `toml:"infer_effort"`        // effort for the infer one-shot (default: medium)
	InferHistoryLimit int                    `toml:"infer_history_limit"` // recent messages fed to the infer agent (default: 20)
```

In `Load`, after the `NameAgent` default block (`if cfg.NameAgent == "" { cfg.NameAgent = "claude" }`), add:

```go
	if cfg.InferAgent == "" {
		cfg.InferAgent = cfg.NameAgent
	}
	if cfg.InferEffort == "" {
		cfg.InferEffort = "medium"
	}
	if cfg.InferHistoryLimit == 0 {
		cfg.InferHistoryLimit = 20
	}
```

- [ ] **Step 4: Add the session.Config fields**

In `internal/session/service.go`, add to the `Config` struct (after `NameAgent`):

```go
	NameAgent            string // agent used to name sessions (default claude)
	InferAgent           string // agent for the fluent `! ` infer step
	InferEffort          string // effort for the infer one-shot
	InferHistoryLimit    int    // recent Discord messages fed to the infer agent
```

- [ ] **Step 5: Plumb through main.go**

In `cmd/quack/main.go`, add to the `session.Config{...}` literal (after `NameAgent: cfg.NameAgent,`):

```go
		NameAgent:            cfg.NameAgent,
		InferAgent:           cfg.InferAgent,
		InferEffort:          cfg.InferEffort,
		InferHistoryLimit:    cfg.InferHistoryLimit,
```

- [ ] **Step 6: Run test to verify it passes**

Run: `go test ./internal/config -run TestLoad_InferDefaults && go build ./...`
Expected: PASS and a clean build.

- [ ] **Step 7: Commit**

```bash
stg new fluent-config-knobs -m "feat(config): add infer_agent/infer_effort/infer_history_limit

Config knobs for the upcoming fluent (! prefix) infer step: which agent
runs the one-shot, at what effort, and how many recent Discord messages to
feed it. Defaults: name_agent, medium, 20.

Signed-off-by: Yves Brissaud <yves.brissaud@gmail.com>"
git add internal/config/config.go internal/config/config_test.go internal/session/service.go cmd/quack/main.go
stg refresh
```

---

## Task 2: `Driver.OneShot`

**Files:**
- Modify: `internal/agentproc/driver.go`
- Modify: `internal/agentproc/claude.go`
- Modify: `internal/agentproc/codex.go`
- Modify: `internal/session/headless_test.go` (`fakeDriver`)

- [ ] **Step 1: Add `OneShot` to the interface**

In `internal/agentproc/driver.go`, add to the `Driver` interface (after `RunTurn`):

```go
	// OneShot runs a single read-only turn (no edits) and returns the agent's
	// final text. Used for quick structured queries like naming and the fluent
	// directive inference. effort is applied if the agent supports it.
	OneShot(ctx context.Context, prompt, effort string) (string, error)
```

Leave `SuggestName` in the interface; it is now a thin wrapper over `OneShot`.

- [ ] **Step 2: Implement `OneShot` for claude and re-express `SuggestName`**

In `internal/agentproc/claude.go`, replace the whole `SuggestName` method with:

```go
// OneShot runs a single read-only turn (plan mode, no edits) and returns the
// agent's final text.
func (d Claude) OneShot(ctx context.Context, prompt, effort string) (string, error) {
	command := d.Command
	if command == "" {
		command = "claude"
	}
	args := []string{"-p", prompt, "--output-format", "json", "--permission-mode", "plan"}
	if effort != "" && d.EffortTemplate != "" {
		args = append(args, strings.Fields(strings.ReplaceAll(d.EffortTemplate, "{effort}", effort))...)
	}
	out, err := exec.CommandContext(ctx, command, args...).Output()
	if err != nil {
		return "", err
	}
	var r struct {
		Result string `json:"result"`
	}
	if err := json.Unmarshal(out, &r); err != nil {
		return "", fmt.Errorf("parse oneshot: %w", err)
	}
	return r.Result, nil
}

// SuggestName runs a quick read-only one-shot to name the task.
func (d Claude) SuggestName(ctx context.Context, prompt string) (string, error) {
	return d.OneShot(ctx, nameGenPrompt+prompt, "low")
}
```

- [ ] **Step 3: Implement `OneShot` for codex and re-express `SuggestName`**

In `internal/agentproc/codex.go`, replace the whole `SuggestName` method with:

```go
// OneShot runs a single turn and returns the agent's accumulated final text.
func (d Codex) OneShot(ctx context.Context, prompt, effort string) (string, error) {
	command := d.Command
	if command == "" {
		command = "codex"
	}
	args := []string{"exec"}
	if effort != "" && d.EffortTemplate != "" {
		args = append(args, strings.Fields(strings.ReplaceAll(d.EffortTemplate, "{effort}", effort))...)
	}
	args = append(args, "--json", prompt)
	cmd := exec.CommandContext(ctx, command, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return "", err
	}
	var b strings.Builder
	parseCodexStream(stdout, func(e Event) {
		if a, ok := e.(AssistantText); ok {
			b.WriteString(a.Text)
		}
	})
	_ = cmd.Wait()
	return b.String(), nil
}

// SuggestName runs a quick one-shot to name the task.
func (d Codex) SuggestName(ctx context.Context, prompt string) (string, error) {
	return d.OneShot(ctx, nameGenPrompt+prompt, "low")
}
```

- [ ] **Step 4: Add `OneShot` to the test fake**

In `internal/session/headless_test.go`, add fields to `fakeDriver`:

```go
type fakeDriver struct {
	turns   []scripted
	seen    []agentproc.Turn
	suggest string // returned by SuggestName ("" => caller falls back)

	oneShot     string   // returned by OneShot
	oneShotErr  error    // returned by OneShot
	oneShotSeen []string // prompts passed to OneShot
}
```

And add the method (next to `SuggestName`):

```go
func (f *fakeDriver) OneShot(ctx context.Context, prompt, effort string) (string, error) {
	f.oneShotSeen = append(f.oneShotSeen, prompt)
	return f.oneShot, f.oneShotErr
}
```

- [ ] **Step 5: Verify build, vet, and existing tests pass**

Run: `go build ./... && go vet ./... && go test ./internal/agentproc ./internal/session`
Expected: PASS (the exec wrapper itself is covered by the `QUACK_INTEGRATION=1` tests; behavior of `SuggestName` is unchanged since it now calls `OneShot` with identical args).

- [ ] **Step 6: Commit**

```bash
stg new fluent-oneshot-driver -m "feat(agentproc): add Driver.OneShot read-only one-shot

Generalize the naming one-shot into OneShot(prompt, effort): a single
read-only turn returning the agent's final text, for both naming and the
upcoming fluent directive inference. SuggestName is now a thin wrapper
(nameGenPrompt + low effort), so its behavior is unchanged.

Signed-off-by: Yves Brissaud <yves.brissaud@gmail.com>"
git add internal/agentproc/driver.go internal/agentproc/claude.go internal/agentproc/codex.go internal/session/headless_test.go
stg refresh
```

---

## Task 3: Discord history port

**Files:**
- Modify: `internal/session/service.go` (add `Message`, `History`, `history` field, `UseHistory`)
- Modify: `internal/discord/replier.go` (implement `RecentMessages`)
- Modify: `cmd/quack/main.go` (wire `History`)
- Modify: `internal/session/fakes_test.go` (`fakeReplier.RecentMessages`)

- [ ] **Step 1: Add the `Message` type, `History` interface, field, and setter**

In `internal/session/service.go`, after the `Replier` interface block, add:

```go
// Message is one recent Discord message, used as context for the fluent infer step.
type Message struct {
	Author  string
	Content string
}

// History reads recent Discord messages. It is a read-only sibling of Replier,
// supplied by the same discord adapter and wired in main.go.
type History interface {
	// RecentMessages returns up to limit messages in channelID posted before
	// beforeID, ordered oldest-first.
	RecentMessages(ctx context.Context, channelID, beforeID string, limit int) ([]Message, error)
}
```

Add a field to the `Service` struct (after `reply Replier`):

```go
	reply   Replier
	history History
```

Add a setter (next to `UseDrivers` in `internal/session/headless.go`, or after `New` in `service.go`):

```go
// UseHistory supplies the Discord history reader for the fluent infer step.
func (s *Service) UseHistory(h History) { s.history = h }
```

- [ ] **Step 2: Implement `RecentMessages` in the discord adapter**

In `internal/discord/replier.go`, add (the file already imports `context` and `discordgo`; add `"github.com/eunomie/quack/internal/session"` to the imports):

```go
// RecentMessages returns up to limit messages before beforeID, oldest-first.
func (r *replier) RecentMessages(ctx context.Context, channelID, beforeID string, limit int) ([]session.Message, error) {
	if limit <= 0 {
		limit = 20
	}
	msgs, err := r.s.ChannelMessages(channelID, limit, beforeID, "", "")
	if err != nil {
		return nil, err
	}
	out := make([]session.Message, 0, len(msgs))
	// discordgo returns newest-first; reverse to chronological order.
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if m.Author == nil {
			continue
		}
		out = append(out, session.Message{Author: m.Author.Username, Content: m.Content})
	}
	return out, nil
}
```

- [ ] **Step 3: Wire the history port in main.go**

In `cmd/quack/main.go`, inside the `svcFor` closure, after `svc.UseDrivers(drivers)`:

```go
			svc = session.New(scfg, g, tx, r)
			svc.UseDrivers(drivers)
			if h, ok := r.(session.History); ok {
				svc.UseHistory(h)
			}
			return svc
```

- [ ] **Step 4: Add `RecentMessages` to the test fake**

In `internal/session/fakes_test.go`, add fields to `fakeReplier` (after `nextID int`):

```go
	recent    []Message // returned by RecentMessages
	recentErr error
```

And add the method (after `Unreact`):

```go
func (f *fakeReplier) RecentMessages(ctx context.Context, channelID, beforeID string, limit int) ([]Message, error) {
	return f.recent, f.recentErr
}
```

- [ ] **Step 5: Verify build, vet, tests**

Run: `go build ./... && go vet ./... && go test ./internal/session ./internal/discord`
Expected: PASS (`history` is unused for now — Go permits unused struct fields).

- [ ] **Step 6: Commit**

```bash
stg new fluent-history-port -m "feat(session): add Discord History port for infer context

Read-only sibling of Replier: RecentMessages fetches recent channel
messages (oldest-first) so the fluent infer step can resolve references
like \"this feature\" and pick a good name. Implemented by the discord
replier and wired in main.go via a type assertion.

Signed-off-by: Yves Brissaud <yves.brissaud@gmail.com>"
git add internal/session/service.go internal/session/headless.go internal/discord/replier.go cmd/quack/main.go internal/session/fakes_test.go
stg refresh
```

(If you placed `UseHistory` in `service.go` instead of `headless.go`, adjust the `git add` list accordingly.)

---

## Task 4: Pure infer helpers (JSON parse + map)

**Files:**
- Create: `internal/session/infer.go`
- Create: `internal/session/infer_test.go`

- [ ] **Step 1: Write the failing tests**

Create `internal/session/infer_test.go`:

```go
package session

import (
	"strings"
	"testing"

	"github.com/eunomie/quack/internal/agent"
)

func TestExtractJSON(t *testing.T) {
	cases := map[string]string{
		`{"a":1}`:                         `{"a":1}`,
		"```json\n{\"a\":1}\n```":         `{"a":1}`,
		"```\n{\"a\":1}\n```":             `{"a":1}`,
		"sure, here:\n{\"a\":1}\nthanks":  `{"a":1}`,
		"no json here":                    ``,
	}
	for in, want := range cases {
		if got := extractJSON(in); got != want {
			t.Errorf("extractJSON(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMapInferred(t *testing.T) {
	agents := map[string]agent.Agent{"claude": {Command: "claude"}}

	tru := true
	fls := false
	inf := inferred{
		Target:   " dagger/dagger ",
		Base:     "main",
		Worktree: &fls,
		Agent:    "bogus", // unknown -> dropped
		Effort:   "ludicrous", // invalid -> dropped
		Name:     "Fix The Bug",
		Headless: &tru,
		Context:  "they meant the cache pin bug",
	}
	dir := mapInferred(inf, agents, "fix the cache pin bug")

	if dir.Target != "dagger/dagger" {
		t.Errorf("Target = %q", dir.Target)
	}
	if !dir.NoWorktree {
		t.Errorf("worktree:false should set NoWorktree")
	}
	if dir.Agent != "" {
		t.Errorf("unknown agent should drop to empty, got %q", dir.Agent)
	}
	if dir.Effort != "" {
		t.Errorf("invalid effort should drop to empty, got %q", dir.Effort)
	}
	if dir.Name != "fix-the-bug" {
		t.Errorf("Name = %q, want fix-the-bug", dir.Name)
	}
	if !dir.Headless {
		t.Errorf("headless should be true")
	}
	if !strings.Contains(dir.Prompt, "<quack-resolved-context>") || !strings.HasSuffix(dir.Prompt, "fix the cache pin bug") {
		t.Errorf("Prompt should prepend the resolved context block, got %q", dir.Prompt)
	}
}

func TestMapInferred_DefaultsWhenOmitted(t *testing.T) {
	agents := map[string]agent.Agent{"claude": {Command: "claude"}}
	dir := mapInferred(inferred{Agent: "claude", Effort: "high"}, agents, "do it")
	if dir.NoWorktree {
		t.Errorf("omitted worktree should default to worktree on (NoWorktree=false)")
	}
	if !dir.Headless {
		t.Errorf("omitted headless should default to true")
	}
	if dir.Agent != "claude" || dir.Effort != "high" {
		t.Errorf("known agent/effort should pass through, got %+v", dir)
	}
	if dir.Prompt != "do it" {
		t.Errorf("Prompt = %q, want raw passthrough", dir.Prompt)
	}
}

func TestParseInferred(t *testing.T) {
	if _, err := parseInferred("not json"); err == nil {
		t.Errorf("expected error for non-JSON output")
	}
	inf, err := parseInferred("```json\n{\"target\":\"a/b\",\"name\":\"x\"}\n```")
	if err != nil {
		t.Fatal(err)
	}
	if inf.Target != "a/b" || inf.Name != "x" {
		t.Errorf("parsed = %+v", inf)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/session -run 'TestExtractJSON|TestMapInferred|TestParseInferred'`
Expected: FAIL — `extractJSON`, `mapInferred`, `inferred`, `parseInferred` undefined.

- [ ] **Step 3: Create `internal/session/infer.go` with the pure helpers**

```go
package session

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/eunomie/quack/internal/agent"
	"github.com/eunomie/quack/internal/command"
	"github.com/eunomie/quack/internal/worktree"
)

// inferred is the JSON the infer one-shot returns. Worktree/Headless are
// pointers so an omitted field is distinguishable from an explicit false.
type inferred struct {
	Target   string `json:"target"`
	Base     string `json:"base"`
	Worktree *bool  `json:"worktree"`
	Agent    string `json:"agent"`
	Effort   string `json:"effort"`
	Name     string `json:"name"`
	Headless *bool  `json:"headless"`
	Context  string `json:"context"`
}

// inferEfforts is the accepted effort vocabulary; anything else is dropped so a
// hallucinated value can't reach the agent.
var inferEfforts = map[string]bool{"low": true, "medium": true, "high": true, "xhigh": true}

// extractJSON narrows raw model output to the outermost JSON object, stripping a
// leading ```json / ``` fence and any surrounding prose. Returns "" if none.
func extractJSON(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[i+1:]
		}
		s = strings.TrimSuffix(strings.TrimSpace(s), "```")
		s = strings.TrimSpace(s)
	}
	start := strings.IndexByte(s, '{')
	end := strings.LastIndexByte(s, '}')
	if start < 0 || end < start {
		return ""
	}
	return s[start : end+1]
}

// parseInferred extracts and unmarshals the infer one-shot output.
func parseInferred(out string) (inferred, error) {
	js := extractJSON(out)
	if js == "" {
		return inferred{}, fmt.Errorf("no JSON object in infer output")
	}
	var inf inferred
	if err := json.Unmarshal([]byte(js), &inf); err != nil {
		return inferred{}, fmt.Errorf("parse infer JSON: %w", err)
	}
	return inf, nil
}

// contextBlock wraps the infer agent's resolved context, or "" when empty.
func contextBlock(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	return "<quack-resolved-context>\n" + s + "\n</quack-resolved-context>"
}

// mapInferred validates the inferred fields and builds the same Directive the
// plain grammar produces. Unknown agent/effort are dropped (config defaults
// apply); the name is slugified; worktree/headless default on; any resolved
// context is prepended to the raw prompt.
func mapInferred(inf inferred, agents map[string]agent.Agent, raw string) *command.Directive {
	agentName := strings.TrimSpace(inf.Agent)
	if _, ok := agents[agentName]; !ok {
		agentName = ""
	}
	effort := strings.TrimSpace(inf.Effort)
	if !inferEfforts[effort] {
		effort = ""
	}
	name := worktree.Slugify(inf.Name)
	if name == "session" { // Slugify's empty sentinel
		name = ""
	}
	headless := true
	if inf.Headless != nil {
		headless = *inf.Headless
	}
	noWorktree := false
	if inf.Worktree != nil {
		noWorktree = !*inf.Worktree
	}
	prompt := raw
	if block := contextBlock(inf.Context); block != "" {
		prompt = block + "\n\n" + raw
	}
	return &command.Directive{
		Target:     strings.TrimSpace(inf.Target),
		Base:       strings.TrimSpace(inf.Base),
		Agent:      agentName,
		Effort:     effort,
		Name:       name,
		Headless:   headless,
		NoWorktree: noWorktree,
		Prompt:     prompt,
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/session -run 'TestExtractJSON|TestMapInferred|TestParseInferred'`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
stg new fluent-infer-pure -m "feat(session): JSON parse + directive mapping for the infer step

Pure helpers for the fluent path: extractJSON narrows model output to the
outermost object (stripping fences/prose), parseInferred unmarshals it, and
mapInferred validates the fields into the same command.Directive the plain
grammar produces — unknown agent/effort dropped, name slugified,
worktree/headless defaulting on, resolved context prepended.

Signed-off-by: Yves Brissaud <yves.brissaud@gmail.com>"
git add internal/session/infer.go internal/session/infer_test.go
stg refresh
```

---

## Task 5: Service infer methods (history fetch + one-shot)

**Files:**
- Modify: `internal/session/infer.go` (add config helpers, `recentHistory`, `inferDirective`, prompt)
- Modify: `internal/session/infer_test.go` (add tests)
- Modify: `internal/session/service_test.go` (`newTestService` wires `history`)

- [ ] **Step 1: Wire history into the standard test service**

In `internal/session/service_test.go`, in `newTestService`, after the `svc := &Service{...}` literal is assigned, set the history fake (the struct literal does not list `history`, so set it after):

```go
	svc.history = r
	return svc, g, tx, r, fs
```

(Replace the existing trailing `return svc, g, tx, r, fs` with the two lines above.)

- [ ] **Step 2: Write the failing tests**

Add to `internal/session/infer_test.go`:

```go
import (
	"context"
	"strings"
	"testing"

	"github.com/eunomie/quack/internal/agent"
	"github.com/eunomie/quack/internal/agentproc"
)
```

(Merge these imports with the file's existing import block — `context`, `agentproc` are new.)

```go
func TestRecentHistory_Formats(t *testing.T) {
	svc, _, _, r, _ := newTestService()
	r.recent = []Message{
		{Author: "alice", Content: "we should add feature A"},
		{Author: "bob", Content: "  "}, // blank -> skipped
		{Author: "alice", Content: "yeah in dagger/dagger"},
	}
	got := svc.recentHistory(context.Background(), baseOrigin())
	want := "alice: we should add feature A\nalice: yeah in dagger/dagger"
	if got != want {
		t.Errorf("recentHistory = %q, want %q", got, want)
	}
}

func TestRecentHistory_NilHistory(t *testing.T) {
	svc, _, _, _, _ := newTestService()
	svc.history = nil
	if got := svc.recentHistory(context.Background(), baseOrigin()); got != "" {
		t.Errorf("nil history should yield empty string, got %q", got)
	}
}

func TestInferDirective_HappyPath(t *testing.T) {
	svc, _, _, _, _ := newTestService()
	d := &fakeDriver{oneShot: `{"target":"dagger/dagger","name":"feature-a","effort":"high","headless":false}`}
	svc.drivers = map[string]agentproc.Driver{"claude": d}

	dir, ok := svc.inferDirective(context.Background(), "in dagger/dagger build feature A", "alice: build feature A")
	if !ok {
		t.Fatal("expected ok")
	}
	if dir.Target != "dagger/dagger" || dir.Name != "feature-a" || dir.Effort != "high" || dir.Headless {
		t.Errorf("dir = %+v", dir)
	}
	if len(d.oneShotSeen) != 1 || !strings.Contains(d.oneShotSeen[0], "build feature A") {
		t.Errorf("infer prompt should carry the request, got %v", d.oneShotSeen)
	}
	if !strings.Contains(d.oneShotSeen[0], "alice: build feature A") {
		t.Errorf("infer prompt should carry the history, got %q", d.oneShotSeen[0])
	}
}

func TestInferDirective_FailsGracefully(t *testing.T) {
	svc, _, _, _, _ := newTestService()

	// bad JSON
	d := &fakeDriver{oneShot: "I think you want dagger"}
	svc.drivers = map[string]agentproc.Driver{"claude": d}
	if _, ok := svc.inferDirective(context.Background(), "x", ""); ok {
		t.Errorf("unparseable output should report not-ok")
	}

	// no driver available
	svc.drivers = map[string]agentproc.Driver{}
	if _, ok := svc.inferDirective(context.Background(), "x", ""); ok {
		t.Errorf("missing driver should report not-ok")
	}
}
```

- [ ] **Step 3: Run tests to verify they fail**

Run: `go test ./internal/session -run 'TestRecentHistory|TestInferDirective'`
Expected: FAIL — `recentHistory`, `inferDirective` undefined.

- [ ] **Step 4: Add the methods to `internal/session/infer.go`**

Add these imports to `infer.go`'s import block: `"context"` and `"time"`.

Append to `internal/session/infer.go`:

```go
const inferPromptTemplate = `You route a natural-language request into a single JSON object describing how to launch a coding-agent session. Reply with ONLY the JSON object — no prose, no markdown fences.

Schema (all fields required):
{"target":"","base":"","worktree":true,"agent":"","effort":"","name":"","headless":true,"context":""}

- target: a GitHub repo "owner/repo", an absolute or ~ path, the literal "temp-dir", or "" when the request names no repo/dir.
- base: base branch to start from, or "" for the repo default.
- worktree: true to work in an isolated worktree (default); false only if the user explicitly wants to work directly in the checkout.
- agent: "claude", "codex", or "" for the default.
- effort: one of "low", "medium", "high", "xhigh", or "" for the default; pick higher for harder tasks.
- name: a short lowercase kebab-case branch name (2-4 words) describing the task.
- headless: true (default) for a Discord conversation; false only if the user explicitly asks for an interactive or tmux session.
- context: if the request refers to something discussed earlier (e.g. "this feature", "that bug"), resolve it into one short paragraph using the conversation below; otherwise "".

Recent Discord conversation (oldest first), for resolving references and naming:
<conversation>
%s
</conversation>

Request:
%s`

func (s *Service) inferAgentName() string {
	return orDefault(s.cfg.InferAgent, orDefault(s.cfg.NameAgent, s.cfg.DefaultAgent))
}

func (s *Service) inferEffort() string { return orDefault(s.cfg.InferEffort, "medium") }

func (s *Service) inferHistoryLimit() int {
	if s.cfg.InferHistoryLimit > 0 {
		return s.cfg.InferHistoryLimit
	}
	return 20
}

// recentHistory fetches and renders recent channel messages as "author: text"
// lines (oldest-first), each capped, for the infer prompt. Returns "" on any
// failure or when no history reader is wired.
func (s *Service) recentHistory(ctx context.Context, o Origin) string {
	if s.history == nil {
		return ""
	}
	msgs, err := s.history.RecentMessages(ctx, o.ChannelID, o.MessageID, s.inferHistoryLimit())
	if err != nil || len(msgs) == 0 {
		return ""
	}
	var b strings.Builder
	for _, m := range msgs {
		text := strings.TrimSpace(m.Content)
		if text == "" {
			continue
		}
		if r := []rune(text); len(r) > 400 {
			text = string(r[:400]) + "…"
		}
		b.WriteString(m.Author)
		b.WriteString(": ")
		b.WriteString(text)
		b.WriteByte('\n')
	}
	return strings.TrimSpace(b.String())
}

// inferDirective runs the read-only infer one-shot and maps its JSON into a
// Directive. Returns (nil, false) on any failure so the caller can fall back.
func (s *Service) inferDirective(ctx context.Context, raw, history string) (*command.Directive, bool) {
	name := s.inferAgentName()
	d, ok := s.drivers[name]
	if !ok {
		if d, ok = s.drivers[s.cfg.DefaultAgent]; !ok {
			return nil, false
		}
	}
	ictx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	convo := history
	if convo == "" {
		convo = "(no recent messages)"
	}
	out, err := d.OneShot(ictx, fmt.Sprintf(inferPromptTemplate, convo, raw), s.inferEffort())
	if err != nil {
		return nil, false
	}
	inf, err := parseInferred(out)
	if err != nil {
		return nil, false
	}
	return mapInferred(inf, s.cfg.Agents, raw), true
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/session -run 'TestRecentHistory|TestInferDirective'`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
stg new fluent-infer-service -m "feat(session): infer one-shot with inlined Discord history

recentHistory renders the recent channel messages as author: text lines;
inferDirective runs the read-only one-shot (60s ceiling, configured
agent/effort) and maps the JSON to a Directive, returning not-ok on any
failure so callers fall back gracefully.

Signed-off-by: Yves Brissaud <yves.brissaud@gmail.com>"
git add internal/session/infer.go internal/session/infer_test.go internal/session/service_test.go
stg refresh
```

---

## Task 6: Extract `run` from `Handle`

**Files:**
- Modify: `internal/session/service.go`

This is a behavior-preserving refactor: the existing `Handle` body (everything after parsing) moves into a new `run` method. No test changes — the existing `service_test.go` suite is the regression guard.

- [ ] **Step 1: Replace `Handle` and add `run`**

In `internal/session/service.go`, replace the entire `Handle` function (currently lines ~142–255, from `func (s *Service) Handle` through its closing brace) with:

```go
// Handle processes one command end-to-end, reporting progress/errors via Replier.
func (s *Service) Handle(ctx context.Context, req Request) {
	dir, err := command.Parse(req.Content)
	if err != nil {
		_ = s.reply.React(ctx, req.Origin.ChannelID, req.Origin.MessageID, emojiError)
		_, _ = s.reply.Post(ctx, req.Origin.ChannelID, "🦆 "+err.Error())
		return
	}
	s.run(ctx, req, dir, dir.Name != "", "", "")
}

// run launches a session from a fully-formed directive — shared by the plain
// grammar (Handle) and the fluent infer path (handleFluent). explicit reports
// whether the name is user-pinned (a collision is an error, not a bump);
// suggested is a pre-computed name that skips the naming agent when non-empty;
// preface, when non-empty, is posted muted right after the ack (the fluent path
// uses it to echo how the request was interpreted, or to note a fallback).
func (s *Service) run(ctx context.Context, req Request, dir *command.Directive, explicit bool, suggested, preface string) {
	agentName := orDefault(dir.Agent, s.cfg.DefaultAgent)
	ag, ok := s.cfg.Agents[agentName]
	if !ok {
		_ = s.reply.React(ctx, req.Origin.ChannelID, req.Origin.MessageID, emojiError)
		_, _ = s.reply.Post(ctx, req.Origin.ChannelID, "🦆 unknown agent: "+agentName)
		return
	}

	token := s.newToken()
	provisional := s.provisionalName(dir, token)
	// A no-target "quick question" defaults to a moderate effort, still
	// overridable with an explicit effort.
	if dir.Target == "" && dir.Effort == "" {
		dir.Effort = scratchEffort
	}
	effort := ag.EffortOr(dir.Effort)

	threadID, err := s.reply.OpenThread(ctx, req.Origin.ChannelID, req.Origin.MessageID, provisional, s.cfg.ThreadAutoArchiveMin)
	if err != nil {
		threadID = req.Origin.ChannelID
	}
	req.Origin.ThreadID = threadID

	target := dir.Target
	if target == "" {
		target = s.cfg.ScratchDir
	}
	ackID, _ := s.reply.PostSilent(ctx, threadID, "🦆 on it — preparing `"+target+"`…")
	req.Origin.ReplyID = ackID

	if preface != "" {
		_, _ = s.reply.PostSilent(ctx, threadID, preface)
	}

	// report edits the ack in place, or posts fresh if the ack never landed.
	report := func(content string) {
		if ackID != "" {
			_ = s.reply.Edit(ctx, threadID, ackID, content)
		} else {
			_, _ = s.reply.Post(ctx, threadID, content)
		}
	}
	fail := func(msg string) {
		_ = s.reply.Unreact(ctx, req.Origin.ChannelID, req.Origin.MessageID, emojiWorking)
		_ = s.reply.React(ctx, req.Origin.ChannelID, req.Origin.MessageID, emojiError)
		report("❌ " + msg)
	}

	// With no explicit name and none pre-supplied, ask the agent to name the task
	// (falls back to the repo-base-random scheme if it errors or no driver exists).
	if !explicit && suggested == "" {
		suggested = s.suggestName(ctx, agentName, dir.Prompt)
	}

	prep, err := s.prepare(ctx, dir, provisional, explicit, token, suggested)
	if err != nil {
		fail(err.Error())
		return
	}

	// Title the thread "<owner/repo|dir> <name>" so multiple threads are easy to
	// place at a glance.
	if desired := threadTitle("", prep.label, prep.name); desired != provisional && threadID != req.Origin.ChannelID {
		_ = s.reply.RenameThread(ctx, threadID, desired)
	}

	sessDir := filepath.Join(s.cfg.StateDir, "sessions", prep.name)
	contextFile := filepath.Join(sessDir, "context.json")
	if data, jerr := req.Origin.ContextJSON(prep.name); jerr == nil {
		_ = s.mkdirAll(sessDir, 0o755)
		_ = s.writeFile(contextFile, data, 0o644)
	}

	s.maybeTrust(ag, prep.workdir)

	if dir.NoWorktree {
		_, _ = s.reply.PostSilent(ctx, threadID, "⚠️ no worktree: running directly in the repo checkout. Parallel sessions on the same repo can conflict — use this sparingly.")
	}

	fullPrompt := req.Origin.PromptHeader() + "\n\n" + dir.Prompt
	if block := s.saveAttachments(ctx, prep.name, req.Attachments); block != "" {
		fullPrompt += "\n\n" + block
	}
	if dir.Headless {
		if _, ok := s.drivers[agentName]; !ok {
			fail("headless not supported for agent: " + agentName)
			return
		}
		report(successMessage(prep, effort, ag) + "\n_(headless: reply in this thread to talk to it; `/stop` to end)_")
		s.startHeadless(ctx, agentName, threadID, prep.workdir, effort, prep.name, prep.label,
			turnReq{channelID: req.Origin.ChannelID, messageID: req.Origin.MessageID, text: fullPrompt})
		return
	}

	opts := NewSessionOpts{
		Name: "quack/" + prep.name,
		Dir:  prep.workdir,
		Env:  req.Origin.EnvVars(prep.name, contextFile),
		Argv: ag.Argv(effort, prep.name, fullPrompt),
	}
	if err := s.tmux.NewSession(ctx, opts); err != nil {
		fail("launch failed: " + err.Error())
		return
	}

	_ = s.reply.Unreact(ctx, req.Origin.ChannelID, req.Origin.MessageID, emojiWorking)
	_ = s.reply.React(ctx, req.Origin.ChannelID, req.Origin.MessageID, emojiDone)
	report(successMessage(prep, effort, ag))
}
```

- [ ] **Step 2: Run the full session suite to verify behavior is preserved**

Run: `go test ./internal/session`
Expected: PASS (all existing tests). The only behavioral additions are two `Unreact(emojiWorking)` calls on the tmux success/fail paths — no-ops here since `Handle` never adds that reaction, and not asserted by any test.

- [ ] **Step 3: Commit**

```bash
stg new fluent-extract-run -m "refactor(session): extract run() from Handle

Move the post-directive launch tail of Handle into a shared run(dir,
explicit, suggested, preface) so the upcoming fluent path can reuse the
exact same launch flow. Behavior-preserving; Handle now just parses and
delegates.

Signed-off-by: Yves Brissaud <yves.brissaud@gmail.com>"
git add internal/session/service.go
stg refresh
```

---

## Task 7: Fluent routing (`! ` prefix → infer → run)

**Files:**
- Modify: `internal/session/infer.go` (add `fluentPrefix`, `interpretationNote`, `handleFluent`)
- Modify: `internal/session/service.go` (route `! ` in `Handle`)
- Modify: `internal/session/infer_test.go` (routing tests)

- [ ] **Step 1: Write the failing tests**

Add to `internal/session/infer_test.go`:

```go
func TestFluentPrefix(t *testing.T) {
	cases := []struct {
		in   string
		rest string
		ok   bool
	}{
		{"! do the thing", "do the thing", true},
		{"!  spaced  ", "spaced", true},
		{"! ", "", true},
		{"!nope", "", false},
		{"dagger/dagger\nfoo", "", false},
		{"plain question", "", false},
	}
	for _, c := range cases {
		rest, ok := fluentPrefix(c.in)
		if ok != c.ok || rest != c.rest {
			t.Errorf("fluentPrefix(%q) = (%q,%v), want (%q,%v)", c.in, rest, ok, c.rest, c.ok)
		}
	}
}

func TestHandle_Fluent_Tmux(t *testing.T) {
	svc, g, tx, r, _ := newTestService()
	g.existing["/src/github.com/dagger/dagger"] = true
	d := &fakeDriver{oneShot: `{"target":"dagger/dagger","name":"feature-a","effort":"high","headless":false}`}
	svc.drivers = map[string]agentproc.Driver{"claude": d}

	svc.Handle(context.Background(), Request{
		Content: "! in dagger/dagger build feature A",
		Origin:  baseOrigin(),
	})

	if len(g.worktrees) != 1 || !strings.Contains(g.worktrees[0], "dagger-worktrees/feature-a|feature-a|origin/main") {
		t.Fatalf("worktrees = %v", g.worktrees)
	}
	if len(tx.created) != 1 || tx.created[0].Dir != "/src/github.com/dagger/dagger-worktrees/feature-a" {
		t.Fatalf("tmux = %v", tx.created)
	}
	if !hasStr(tx.created[0].Argv, "high") {
		t.Errorf("inferred effort high should reach argv, got %v", tx.created[0].Argv)
	}
	if !hasStr(r.reacts, "c|m|"+emojiWorking) {
		t.Errorf("expected early working reaction, got %v", r.reacts)
	}
	if !anyContains(r.posts, "interpreted as") {
		t.Errorf("expected the muted interpretation echo, got %v", r.posts)
	}
	// The interpretation echo is muted (no notification).
	var echo *postedMsg
	for i := range r.posts {
		if strings.Contains(r.posts[i].content, "interpreted as") {
			echo = &r.posts[i]
		}
	}
	if echo == nil || !echo.silent {
		t.Errorf("interpretation echo should be posted silently, got %+v", echo)
	}
	if len(d.oneShotSeen) != 1 || !strings.Contains(d.oneShotSeen[0], "build feature A") {
		t.Errorf("infer one-shot should see the raw request, got %v", d.oneShotSeen)
	}
}

func TestHandle_Fluent_Fallback(t *testing.T) {
	svc, g, tx, r, _ := newTestService()
	d := &fakeDriver{oneShotErr: context.DeadlineExceeded}
	svc.drivers = map[string]agentproc.Driver{"claude": d}

	svc.Handle(context.Background(), Request{
		Content: "! just answer this quick question",
		Origin:  baseOrigin(),
	})

	if len(g.cloned) != 0 || len(g.worktrees) != 0 {
		t.Errorf("fallback should not clone/worktree: cloned=%v worktrees=%v", g.cloned, g.worktrees)
	}
	if len(tx.created) != 1 || tx.created[0].Dir != "/scratch" {
		t.Fatalf("fallback should run in the scratch dir, got %v", tx.created)
	}
	prompt := tx.created[0].Argv[len(tx.created[0].Argv)-1]
	if !strings.Contains(prompt, "just answer this quick question") {
		t.Errorf("fallback should run the raw request, got %q", prompt)
	}
	if !anyContains(r.posts, "couldn't interpret") {
		t.Errorf("expected a muted fallback note, got %v", r.posts)
	}
}

func TestHandle_Fluent_Headless(t *testing.T) {
	svc, g, _, r, _ := newTestService()
	g.existing["/src/github.com/dagger/dagger"] = true
	d := &fakeDriver{
		oneShot: `{"target":"dagger/dagger","name":"fix-bug","headless":true,"context":"the cache pin bug"}`,
		turns:   []scripted{{texts: []string{"on it"}, ref: "s"}},
	}
	svc.drivers = map[string]agentproc.Driver{"claude": d}

	svc.Handle(context.Background(), Request{
		Content: "! fix this in dagger/dagger",
		Origin:  baseOrigin(),
	})
	svc.waitIdle(r.threadID)

	ls := svc.sessions[r.threadID]
	svc.StopThread(context.Background(), r.threadID)
	<-ls.title.done

	if len(d.seen) != 1 {
		t.Fatalf("expected one task turn, got %d", len(d.seen))
	}
	if !strings.Contains(d.seen[0].Prompt, "<quack-context>") || !strings.Contains(d.seen[0].Prompt, "fix this in dagger/dagger") {
		t.Errorf("task prompt should carry quack-context + raw request, got %q", d.seen[0].Prompt)
	}
	if !strings.Contains(d.seen[0].Prompt, "<quack-resolved-context>") {
		t.Errorf("task prompt should carry the resolved context block, got %q", d.seen[0].Prompt)
	}
	if !anyContains(r.posts, "on it") {
		t.Errorf("agent answer not posted: %v", r.posts)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/session -run 'TestFluentPrefix|TestHandle_Fluent'`
Expected: FAIL — `fluentPrefix`, `handleFluent`, routing not present.

- [ ] **Step 3: Add the fluent helpers to `internal/session/infer.go`**

Append to `internal/session/infer.go`:

```go
// fluentPrefix reports whether content opts into the fluent path ("! " prefix)
// and returns the trimmed request that follows. Isolated here so the trigger is
// cheap to change.
func fluentPrefix(content string) (string, bool) {
	if rest, ok := strings.CutPrefix(content, "! "); ok {
		return strings.TrimSpace(rest), true
	}
	return "", false
}

// interpretationNote renders the muted echo of how a fluent request was
// interpreted, posted into the thread so the user can see/correct it.
func interpretationNote(dir *command.Directive) string {
	wt := "worktree"
	if dir.NoWorktree {
		wt = "no-wt"
	}
	mode := "headless"
	if !dir.Headless {
		mode = "interactive"
	}
	return fmt.Sprintf("-# 🦆 interpreted as: `%s` · agent `%s` · effort `%s` · %s `%s` · %s",
		orDefault(dir.Target, "scratch"),
		orDefault(dir.Agent, "default"),
		orDefault(dir.Effort, "default"),
		wt,
		orDefault(dir.Name, "(auto)"),
		mode)
}

// handleFluent runs the fluent path: react immediately, fetch recent Discord
// context, infer the directive, echo the interpretation, then launch via run.
// On any infer failure it falls back to a scratch-dir run of the raw request.
func (s *Service) handleFluent(ctx context.Context, req Request, raw string) {
	if raw == "" {
		_ = s.reply.React(ctx, req.Origin.ChannelID, req.Origin.MessageID, emojiError)
		_, _ = s.reply.Post(ctx, req.Origin.ChannelID, "🦆 nothing after `! ` — tell me what to do")
		return
	}
	// Instant feedback while the infer one-shot runs (it's on the critical path
	// before the thread opens).
	_ = s.reply.React(ctx, req.Origin.ChannelID, req.Origin.MessageID, emojiWorking)

	history := s.recentHistory(ctx, req.Origin)
	dir, ok := s.inferDirective(ctx, raw, history)
	if !ok {
		fallback := &command.Directive{Headless: true, Prompt: raw}
		s.run(ctx, req, fallback, false, "", "-# 🦆 couldn't interpret that — running it in the scratch dir")
		return
	}
	// The inferred name is a suggestion (auto-suffix on collision), so pass it as
	// `suggested` with explicit=false; setting it on the directive too gives a nice
	// provisional thread name before the labelled rename.
	s.run(ctx, req, dir, false, dir.Name, interpretationNote(dir))
}
```

- [ ] **Step 4: Route `! ` in `Handle`**

In `internal/session/service.go`, change `Handle` to branch first:

```go
// Handle processes one command end-to-end, reporting progress/errors via Replier.
func (s *Service) Handle(ctx context.Context, req Request) {
	if raw, ok := fluentPrefix(req.Content); ok {
		s.handleFluent(ctx, req, raw)
		return
	}

	dir, err := command.Parse(req.Content)
	if err != nil {
		_ = s.reply.React(ctx, req.Origin.ChannelID, req.Origin.MessageID, emojiError)
		_, _ = s.reply.Post(ctx, req.Origin.ChannelID, "🦆 "+err.Error())
		return
	}
	s.run(ctx, req, dir, dir.Name != "", "", "")
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/session -run 'TestFluentPrefix|TestHandle_Fluent'`
Expected: PASS.

- [ ] **Step 6: Run the full session suite**

Run: `go test ./internal/session`
Expected: PASS (existing tests unaffected — they use no `! ` prefix).

- [ ] **Step 7: Commit**

```bash
stg new fluent-routing -m "feat(session): route '! ' mentions through the infer path

A mention starting with '! ' is handed to handleFluent: react immediately,
inline recent Discord context, run the infer one-shot, echo the muted
interpretation, then launch via the shared run(). Infer failures fall back
to a scratch-dir run of the raw request. The plain grammar is untouched.

Signed-off-by: Yves Brissaud <yves.brissaud@gmail.com>"
git add internal/session/infer.go internal/session/service.go internal/session/infer_test.go
stg refresh
```

---

## Task 8: Documentation

**Files:**
- Modify: `config.example.toml`
- Modify: `AGENTS.md`

- [ ] **Step 1: Document the config knobs**

In `config.example.toml`, after the `name_agent = ...` line, add:

```toml
name_agent     = "claude"     # agent used to auto-name sessions (best summarizer)
# infer_agent = "claude"        # agent for the fluent "! " path (default: name_agent)
# infer_effort = "medium"       # effort for the infer one-shot; lower = faster (low|medium|high|xhigh)
# infer_history_limit = 20      # recent channel messages fed to the infer agent for context
```

- [ ] **Step 2: Document the fluent path in AGENTS.md**

In `AGENTS.md`, under "### Command grammar (`internal/command`)", append a paragraph:

```markdown
A **fluent mention** opts out of the grammar entirely: if the mention starts
with `! `, the rest is a natural-language request handed to a low-effort
read-only one-shot (the configured `infer_agent`, default `name_agent`) that —
given the request plus recent channel messages — emits the directive fields
(target, base, worktree, agent, effort, name, headless) as JSON. quack maps that
to the same `command.Directive` and launches via the shared `run`, so everything
downstream is identical. The infer step also yields a resolved-context blurb
(for references like "this feature") prepended to the working agent's prompt, and
replaces the separate naming call. On any infer failure it falls back to a
scratch-dir run of the raw request. See `internal/session/infer.go` and
`hack/designs/2026-06-03-fluent-directive-inference.md`.
```

- [ ] **Step 3: Verify build**

Run: `go build ./...`
Expected: clean (docs only; nothing to compile, but confirm nothing else broke).

- [ ] **Step 4: Commit**

```bash
stg new fluent-docs -m "docs(quack): document the fluent (! prefix) path

Add the infer_agent/infer_effort/infer_history_limit knobs to the example
config and describe the fluent path in AGENTS.md.

Signed-off-by: Yves Brissaud <yves.brissaud@gmail.com>"
git add config.example.toml AGENTS.md
stg refresh
```

---

## Task 9: Final verification

- [ ] **Step 1: Build, vet, full unit tests**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: all PASS.

- [ ] **Step 2: Confirm the patch stack**

Run: `stg series`
Expected: the eight patches in order (`fluent-config-knobs` … `fluent-docs`).

- [ ] **Step 3 (optional): Integration smoke**

If an authenticated `claude` CLI is on PATH, sanity-check the one-shot end-to-end with `QUACK_INTEGRATION=1 go test ./internal/agentproc`. This is optional and not required to consider the feature complete.

- [ ] **Step 4: Do NOT restart the service**

Per the original request, do not run `systemctl --user restart quack.service`. The work stays built and tested but undeployed until the user decides.

---

## Self-Review

**1. Spec coverage** (against `2026-06-03-fluent-directive-inference.md`):
- Trigger & routing → Task 7 (`fluentPrefix` + `Handle` branch). ✓
- Infer path (react, fetch history, one-shot, parse, echo, launch) → Tasks 5 + 7. ✓
- JSON contract + validation table → Task 4 (`mapInferred`). ✓
- Prompts (raw verbatim + quack-context + resolved context; name replaces suggestName) → Tasks 4 (context prepend) + 6/7 (run reuse, suggested name). ✓
- Fallback → Task 7 (`handleFluent` fallback branch). ✓
- Discord history port → Task 3. ✓
- `OneShot` → Task 2. ✓
- Small refactor (`run`) → Task 6. ✓
- Config knobs → Task 1. ✓
- Testing matrix (clean, no target→scratch, unknown agent/effort dropped, bad JSON→fallback, collision→suffix, context prepended, routing) → covered across Tasks 4/5/7 (collision→suffix is the existing `resolveName(explicit=false)` path exercised by `TestHandle_GeneratedNameBumps`; the inferred name flows through it unchanged). ✓

**2. Placeholder scan:** No TBD/TODO; every code step shows complete code; every test step shows the assertions and the exact run command.

**3. Type consistency:** `OneShot(ctx, prompt, effort)` is identical across the interface, both drivers, and the fake. `RecentMessages(ctx, channelID, beforeID, limit)` is identical across `History`, the replier, and the fake. `mapInferred(inf, agents, raw)`, `inferDirective(ctx, raw, history)`, `recentHistory(ctx, o)`, `run(ctx, req, dir, explicit, suggested, preface)`, `fluentPrefix(content)`, `handleFluent(ctx, req, raw)`, and `interpretationNote(dir)` are used consistently between their definitions and call sites. The `inferred` struct field names match the JSON tags used in tests.
