# Infer guidance & per-agent model — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add two opt-in config levers to quack's fluent infer step — a free-text `infer_guidance` string folded into the infer prompt, and a per-agent `model` field (claude driver) so the infer one-shot can run on a faster model — without touching the working coding agent.

**Architecture:** `infer_guidance` is a new `config.Config` field, plumbed to `session.Config` and rendered into `inferPromptTemplate` via a `guidanceBlock` helper that wraps the user's text in fixed "resolution-hints-only" framing (omitted entirely when empty). `model` is a new `agent.Agent` field consumed by `agentproc.Claude` (emitted as `--model` on every turn and in the one-shot path); it is isolated to infer by giving infer its own `[agents.infer]` entry. A required `main.go` change switches driver dispatch from the map key to `a.Command` so a custom-named agent entry registers a driver.

**Tech Stack:** Go, BurntSushi/toml, the existing fake-based `internal/session` tests, Stacked Git (stg) for commits.

**Spec:** `hack/designs/2026-06-04-infer-guidance-and-model.md`

**Conventions for this plan:**
- Go toolchain: `go` should be on `PATH`; if not, it is at `/usr/local/go/bin/go`.
- Commits use Stacked Git, **not** `git commit`. Each task ends by creating one stg patch with a `Signed-off-by: Yves Brissaud <yves.brissaud@gmail.com>` trailer and **no** AI attribution. The recipe (verified working on an empty/non-empty stack):
  ```bash
  stg new <patch-name> -m "<subject>

  <body>

  Signed-off-by: Yves Brissaud <yves.brissaud@gmail.com>"
  git add <files>
  stg refresh
  ```
- The design doc is already patch `design-infer-guidance-model`; these tasks stack on top.

---

## File Structure

| File | Change | Responsibility |
|------|--------|----------------|
| `internal/agentproc/claude.go` | modify | add `Model` field; emit `--model`; extract `oneShotArgs` |
| `internal/agentproc/claude_test.go` | modify | assert `--model` on first/resume/one-shot args |
| `internal/agent/agent.go` | modify | add `Model` config field on `Agent` |
| `cmd/quack/main.go` | modify | dispatch on `a.Command`; wire `Model`; map `InferGuidance` |
| `internal/config/config.go` | modify | add `InferGuidance` field |
| `internal/config/config_test.go` | modify | assert `model` and `infer_guidance` load |
| `internal/session/service.go` | modify | add `InferGuidance` to `session.Config` |
| `internal/session/infer.go` | modify | add `guidanceBlock`; inject into the prompt template |
| `internal/session/infer_test.go` | modify | assert guidance present/absent in the infer prompt |
| `config.example.toml` | modify | document `infer_guidance` + `[agents.infer]` model example |
| `AGENTS.md` | modify | note the two new levers |

---

## Task 1: `--model` flag on the claude driver

**Files:**
- Modify: `internal/agentproc/claude.go`
- Test: `internal/agentproc/claude_test.go`

- [ ] **Step 1: Write the failing test**

Add this function to `internal/agentproc/claude_test.go` (after `TestClaudeArgs_Settings`, before the `contains` helper):

```go
func TestClaudeArgs_Model(t *testing.T) {
	d := Claude{Command: "claude", Model: "claude-haiku-4-5-20251001"}

	// --model applies on the first turn...
	first := d.args(Turn{Prompt: "hi"})
	if !contains(first, "--model") || !contains(first, "claude-haiku-4-5-20251001") {
		t.Errorf("first turn missing --model: %v", first)
	}
	// ...and on resume turns too (each turn is a fresh claude process).
	next := d.args(Turn{Prompt: "again", SessionRef: "sess-1"})
	if !contains(next, "--model") {
		t.Errorf("resume turn missing --model: %v", next)
	}
	// ...and in the one-shot path.
	os := d.oneShotArgs("p", "low")
	if !contains(os, "--model") || !contains(os, "claude-haiku-4-5-20251001") {
		t.Errorf("oneShotArgs missing --model: %v", os)
	}

	// Unset Model => no --model flag.
	none := Claude{Command: "claude"}.args(Turn{Prompt: "hi"})
	if contains(none, "--model") {
		t.Errorf("no Model should add no --model flag: %v", none)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails (does not compile)**

Run: `go test ./internal/agentproc/ -run TestClaudeArgs_Model -v`
Expected: FAIL — build error, `d.Model` and `d.oneShotArgs` undefined.

- [ ] **Step 3: Add the `Model` field to the `Claude` struct**

In `internal/agentproc/claude.go`, change the struct (insert `Model` after `Command`):

```go
// Claude drives the claude CLI in headless resume-per-turn mode.
type Claude struct {
	Command        string
	Model          string // --model value; empty => CLI default (codex ignores its own)
	EffortTemplate string
	NameTemplate   string
	PermissionMode string
	AllowedTools   string
	Settings       string // passed verbatim to `claude --settings` (JSON or file path)
}
```

- [ ] **Step 4: Emit `--model` in `args` (both first and resume turns)**

In `internal/agentproc/claude.go`, in the `args` method, add the `--model` append immediately after the `Settings` block and before the `if t.SessionRef != ""` check, so it applies to every turn:

```go
	if d.Settings != "" {
		a = append(a, "--settings", d.Settings)
	}
	if d.Model != "" {
		a = append(a, "--model", d.Model)
	}
	if t.SessionRef != "" {
		a = append(a, "--resume", t.SessionRef)
		return a
	}
```

- [ ] **Step 5: Extract `oneShotArgs` and emit `--model` there too**

In `internal/agentproc/claude.go`, replace the body of `OneShot` so the argv build is a separate, testable helper:

```go
// oneShotArgs builds the argv for a single read-only (plan-mode) turn.
func (d Claude) oneShotArgs(prompt, effort string) []string {
	args := []string{"-p", prompt, "--output-format", "json", "--permission-mode", "plan"}
	if d.Model != "" {
		args = append(args, "--model", d.Model)
	}
	if effort != "" && d.EffortTemplate != "" {
		args = append(args, strings.Fields(strings.ReplaceAll(d.EffortTemplate, "{effort}", effort))...)
	}
	return args
}

// OneShot runs a single read-only turn (plan mode, no edits) and returns the
// agent's final text.
func (d Claude) OneShot(ctx context.Context, prompt, effort string) (string, error) {
	command := d.Command
	if command == "" {
		command = "claude"
	}
	out, err := exec.CommandContext(ctx, command, d.oneShotArgs(prompt, effort)...).Output()
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
```

- [ ] **Step 6: Run the test to verify it passes**

Run: `go test ./internal/agentproc/ -run TestClaudeArgs_Model -v`
Expected: PASS

- [ ] **Step 7: Run the full suite + vet**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: all PASS (no behavior change for agents without `Model`).

- [ ] **Step 8: Commit (stg patch)**

```bash
stg new claude-driver-model -m "agentproc: --model flag for the claude driver

Add a Model field to the claude driver and pass --model on every turn
(first + resume) and in the one-shot path, so an agent can be pinned to a
specific model. Extract oneShotArgs so the one-shot flag is unit-testable.

Signed-off-by: Yves Brissaud <yves.brissaud@gmail.com>"
git add internal/agentproc/claude.go internal/agentproc/claude_test.go
stg refresh
```

---

## Task 2: `model` config field + driver dispatch on command

**Files:**
- Modify: `internal/agent/agent.go`
- Modify: `cmd/quack/main.go`
- Test: `internal/config/config_test.go`

- [ ] **Step 1: Write the failing test**

Add this function to `internal/config/config_test.go` (after `TestLoad_ScratchDirExplicit`):

```go
func TestLoad_AgentModel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	body := "[agents.infer]\ncommand = \"claude\"\nmodel = \"claude-haiku-4-5-20251001\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	a, ok := cfg.Agents["infer"]
	if !ok || a.Model != "claude-haiku-4-5-20251001" {
		t.Errorf("infer agent model = %+v ok=%v", a, ok)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails (does not compile)**

Run: `go test ./internal/config/ -run TestLoad_AgentModel -v`
Expected: FAIL — `a.Model` undefined.

- [ ] **Step 3: Add the `Model` field to `agent.Agent`**

In `internal/agent/agent.go`, insert `Model` after `Command` in the struct:

```go
// Agent describes how to launch one coding agent.
type Agent struct {
	Command        string `toml:"command"`         // executable, e.g. "claude"
	Model          string `toml:"model"`           // claude: --model value; unset => CLI default (codex ignores it)
	EffortTemplate string `toml:"effort_template"` // contains "{effort}", e.g. "--effort {effort}"
	NameTemplate   string `toml:"name_template"`   // contains "{name}", e.g. "-n {name}"
	ResumeTemplate string `toml:"resume_template"` // contains "{session}", e.g. "--resume {session}"
	DefaultEffort  string `toml:"default_effort"`  // used when the command gives no effort
	Headless       bool   `toml:"headless"`        // has a headless driver
	PermissionMode string `toml:"permission_mode"` // claude: acceptEdits|bypassPermissions|auto
	AllowedTools   string `toml:"allowed_tools"`   // claude: --allowedTools value
	Settings       string `toml:"settings"`        // claude: --settings JSON or file (e.g. sandbox)
	// InteractiveArgs are extra flags appended to interactive (no-headless) and
	// /attach launches only — not the headless driver, which governs permissions
	// via PermissionMode. When unset, claude defaults to
	// "--dangerously-skip-permissions" (its usual interactive workflow). Set it
	// to override (e.g. "" to disable, or other flags); a pointer so an explicit
	// empty value is distinguishable from "not configured".
	InteractiveArgs *string `toml:"interactive_args"`
}
```

- [ ] **Step 4: Run the config test to verify it passes**

Run: `go test ./internal/config/ -run TestLoad_AgentModel -v`
Expected: PASS

- [ ] **Step 5: Dispatch drivers on `a.Command` and wire `Model`**

In `cmd/quack/main.go`, replace the driver-building loop (the `for name, a := range cfg.Agents` block) so dispatch keys on the executable, not the map name, and the claude driver receives `Model`:

```go
	for name, a := range cfg.Agents {
		if !a.Headless {
			continue
		}
		switch a.Command {
		case "claude":
			drivers[name] = agentproc.Claude{
				Command:        a.Command,
				Model:          a.Model,
				EffortTemplate: a.EffortTemplate,
				NameTemplate:   a.NameTemplate,
				PermissionMode: a.Mode(),
				AllowedTools:   a.AllowedTools,
				Settings:       a.Settings,
			}
		case "codex":
			drivers[name] = agentproc.Codex{Command: a.Command, EffortTemplate: a.EffortTemplate}
		default:
			log.Printf("agent %q has headless=true but command %q has no driver; ignoring", name, a.Command)
		}
	}
```

- [ ] **Step 6: Build + vet + full suite**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: all PASS. (Existing config names agents `claude`/`codex` with matching commands, so dispatch is unchanged for them.)

- [ ] **Step 7: Commit (stg patch)**

```bash
stg new agent-model-dispatch -m "config,main: per-agent model field + dispatch on command

Add a model field to agent.Agent and pass it into the claude driver.
Switch main.go driver dispatch from the map key to a.Command so a
custom-named agent entry (e.g. [agents.infer]) registers a driver; the
key becomes a free-form label, which the infer_agent/name_agent
indirection already assumed.

Signed-off-by: Yves Brissaud <yves.brissaud@gmail.com>"
git add internal/agent/agent.go cmd/quack/main.go internal/config/config_test.go
stg refresh
```

---

## Task 3: `infer_guidance` config plumbing

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/session/service.go`
- Modify: `cmd/quack/main.go`
- Test: `internal/config/config_test.go`

- [ ] **Step 1: Write the failing test**

Add this function to `internal/config/config_test.go` (after `TestLoad_AgentModel`):

```go
func TestLoad_InferGuidance(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	body := "infer_guidance = \"bare dagger means dagger/dagger\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.InferGuidance != "bare dagger means dagger/dagger" {
		t.Errorf("InferGuidance = %q", cfg.InferGuidance)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails (does not compile)**

Run: `go test ./internal/config/ -run TestLoad_InferGuidance -v`
Expected: FAIL — `cfg.InferGuidance` undefined.

- [ ] **Step 3: Add `InferGuidance` to `config.Config`**

In `internal/config/config.go`, insert the field after `InferEffort`:

```go
	InferAgent        string                 `toml:"infer_agent"`         // agent for the fluent `! ` infer step (default: name_agent)
	InferEffort       string                 `toml:"infer_effort"`        // effort for the infer one-shot (default: medium)
	InferGuidance     string                 `toml:"infer_guidance"`      // standing hints folded into the infer prompt (empty => off)
	InferHistoryLimit int                    `toml:"infer_history_limit"` // recent messages fed to the infer agent (default: 20)
```

(No default is applied in `Load` — empty means the feature is off.)

- [ ] **Step 4: Run the config test to verify it passes**

Run: `go test ./internal/config/ -run TestLoad_InferGuidance -v`
Expected: PASS

- [ ] **Step 5: Add `InferGuidance` to `session.Config`**

In `internal/session/service.go`, insert the field after `InferEffort` in the `Config` struct:

```go
	InferAgent           string // agent for the fluent `! ` infer step
	InferEffort          string // effort for the infer one-shot
	InferGuidance        string // standing hints folded into the infer prompt
	InferHistoryLimit    int    // recent Discord messages fed to the infer agent
```

- [ ] **Step 6: Map it through in `main.go`**

In `cmd/quack/main.go`, add the mapping in the `scfg := session.Config{...}` literal, after `InferEffort`:

```go
		InferAgent:           cfg.InferAgent,
		InferEffort:          cfg.InferEffort,
		InferGuidance:        cfg.InferGuidance,
		InferHistoryLimit:    cfg.InferHistoryLimit,
```

- [ ] **Step 7: Build + vet + full suite**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: all PASS (field is wired but not yet consumed — no behavior change).

- [ ] **Step 8: Commit (stg patch)**

```bash
stg new infer-guidance-config -m "config,session: infer_guidance field

Plumb a new optional infer_guidance string from config.Config through
session.Config and main.go. Empty by default; consumed by the infer
prompt in the next patch.

Signed-off-by: Yves Brissaud <yves.brissaud@gmail.com>"
git add internal/config/config.go internal/session/service.go cmd/quack/main.go internal/config/config_test.go
stg refresh
```

---

## Task 4: inject guidance into the infer prompt

**Files:**
- Modify: `internal/session/infer.go`
- Test: `internal/session/infer_test.go`

- [ ] **Step 1: Write the failing tests**

Add these three functions to `internal/session/infer_test.go` (after `TestParseInferred`):

```go
func TestGuidanceBlock(t *testing.T) {
	if got := guidanceBlock("  "); got != "" {
		t.Errorf("blank guidance should yield empty, got %q", got)
	}
	got := guidanceBlock("bare dagger means dagger/dagger")
	if !strings.Contains(got, "Environment hints") || !strings.Contains(got, "never invent a target") {
		t.Errorf("guidance block missing the fixed framing, got %q", got)
	}
	if !strings.Contains(got, "bare dagger means dagger/dagger") {
		t.Errorf("guidance block should carry the user text, got %q", got)
	}
}

func TestInferDirective_Guidance(t *testing.T) {
	svc, _, _, _, _ := newTestService()
	svc.cfg.InferGuidance = "bare dagger means dagger/dagger"
	d := &fakeDriver{oneShot: `{"target":"dagger/dagger","name":"x"}`}
	svc.drivers = map[string]agentproc.Driver{"claude": d}

	if _, ok := svc.inferDirective(context.Background(), "build it", ""); !ok {
		t.Fatal("expected ok")
	}
	if len(d.oneShotSeen) != 1 || !strings.Contains(d.oneShotSeen[0], "Environment hints") {
		t.Errorf("guidance should be injected into the infer prompt, got %v", d.oneShotSeen)
	}
	if !strings.Contains(d.oneShotSeen[0], "bare dagger means dagger/dagger") {
		t.Errorf("guidance text missing from prompt, got %q", d.oneShotSeen[0])
	}
}

func TestInferDirective_NoGuidanceWhenUnset(t *testing.T) {
	svc, _, _, _, _ := newTestService()
	d := &fakeDriver{oneShot: `{"target":"a/b","name":"x"}`}
	svc.drivers = map[string]agentproc.Driver{"claude": d}

	if _, ok := svc.inferDirective(context.Background(), "build it", ""); !ok {
		t.Fatal("expected ok")
	}
	if strings.Contains(d.oneShotSeen[0], "Environment hints") {
		t.Errorf("no guidance configured should omit the hints section, got %q", d.oneShotSeen[0])
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail (does not compile)**

Run: `go test ./internal/session/ -run 'TestGuidanceBlock|TestInferDirective_Guidance|TestInferDirective_NoGuidanceWhenUnset' -v`
Expected: FAIL — `guidanceBlock` undefined.

- [ ] **Step 3: Add the `guidanceBlock` helper**

In `internal/session/infer.go`, add this function next to `contextBlock`:

```go
// guidanceBlock wraps standing infer hints in a fixed framing, or "" when s is
// empty or all-whitespace. The framing constrains the hints to target/name
// resolution so a prior can't make the agent invent a target for a request that
// names no repo. The trailing blank line separates it from the conversation
// section that follows in the prompt template.
func guidanceBlock(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	return "Environment hints (use ONLY to resolve the target repo/path and the name; never invent a target when the request names no repo/dir):\n" + s + "\n\n"
}
```

- [ ] **Step 4: Add the guidance slot to the prompt template**

In `internal/session/infer.go`, in `inferPromptTemplate`, insert a `%s` immediately before the `Recent Discord conversation` line. The relevant region becomes:

```go
- context: if the request refers to something discussed earlier (e.g. "this feature", "that bug"), resolve it into one short paragraph using the conversation below; otherwise "".

%sRecent Discord conversation (oldest first), for resolving references and naming:
<conversation>
%s
</conversation>

Request:
%s`
```

(The template now has three `%s`, in order: guidance, conversation, request.)

- [ ] **Step 5: Pass the guidance into the `Sprintf` call**

In `internal/session/infer.go`, in `inferDirective`, update the `OneShot` call to supply the guidance as the first format argument:

```go
	out, err := d.OneShot(ictx, fmt.Sprintf(inferPromptTemplate, guidanceBlock(s.cfg.InferGuidance), convo, raw), s.inferEffort())
```

- [ ] **Step 6: Run the new tests to verify they pass**

Run: `go test ./internal/session/ -run 'TestGuidanceBlock|TestInferDirective_Guidance|TestInferDirective_NoGuidanceWhenUnset' -v`
Expected: PASS

- [ ] **Step 7: Run the full session suite (guard the existing infer tests)**

Run: `go test ./internal/session/...`
Expected: PASS — including `TestInferDirective_HappyPath` (still finds the request + history in the prompt; the empty-guidance slot leaves the existing layout intact).

- [ ] **Step 8: Build + vet + full suite**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: all PASS.

- [ ] **Step 9: Commit (stg patch)**

```bash
stg new infer-guidance-prompt -m "session: inject infer_guidance into the infer prompt

Add guidanceBlock, which wraps standing hints in fixed
resolution-hints-only framing (omitted when empty), and splice it into
inferPromptTemplate between the field rules and the conversation. Output
is still validated JSON, so a bad hint stays bounded by mapInferred and
the scratch fallback.

Signed-off-by: Yves Brissaud <yves.brissaud@gmail.com>"
git add internal/session/infer.go internal/session/infer_test.go
stg refresh
```

---

## Task 5: document both levers

**Files:**
- Modify: `config.example.toml`
- Modify: `AGENTS.md`

- [ ] **Step 1: Document `infer_guidance` and the infer-agent example in `config.example.toml`**

In `config.example.toml`, add the `infer_guidance` line in the top-level infer block (after `infer_history_limit`):

```toml
# infer_agent = "claude"        # agent for the default fluent (natural-language) path (default: name_agent)
# infer_effort = "medium"       # effort for the infer one-shot; lower = faster (low|medium|high|xhigh)
# infer_history_limit = 20      # recent channel messages fed to the infer agent for context
# infer_guidance = """          # standing hints for the INFER agent only (not the working agent);
#   Repos are cloned under ~/dev/src. A bare "dagger" means dagger/dagger.
#   I mostly work on the eunomie/* and dagger/* owners.
#   """                         # use only to resolve targets/names; never invent a target when none is named
```

Then add a dedicated infer-agent example after the `[agents.codex]` block:

```toml
# A dedicated agent for the fluent infer one-shot, on a faster model — point
# infer_agent at it (infer_agent = "infer") to speed up routing while the
# working agent keeps its own model. Needs headless = true so a driver is built;
# model is claude-only (codex ignores it). Keep effort_template so infer_effort applies.
# [agents.infer]
# command         = "claude"
# model           = "claude-haiku-4-5-20251001"
# effort_template = "--effort {effort}"
# headless        = true
```

- [ ] **Step 2: Note the levers in `AGENTS.md`**

In `AGENTS.md`, in the `## Config` section, append this paragraph after the existing `[agents.<name>]` description:

```markdown
Two optional levers tune the fluent infer step without touching the working
agent: `infer_guidance` (a free-text string folded into the infer prompt as
resolution hints — repo shorthands, where clones live; it never rewrites the
request and is bounded by the same JSON validation) and a per-agent `model`
field (claude-only; codex ignores it). Point `infer_agent` at a dedicated
`[agents.infer]` entry with `model = …` and `headless = true` to run the infer
one-shot (and naming) on a faster model — drivers are now selected by the
agent's `command`, so the entry's name is a free-form label.
```

- [ ] **Step 3: Sanity-check the docs build nothing but read cleanly**

Run: `go build ./... && go test ./...`
Expected: all PASS (docs-only change; confirms nothing else drifted).

- [ ] **Step 4: Commit (stg patch)**

```bash
stg new docs-infer-guidance-model -m "docs: document infer_guidance and per-agent model

Document the two new infer levers in config.example.toml (with a
dedicated [agents.infer] model example) and AGENTS.md.

Signed-off-by: Yves Brissaud <yves.brissaud@gmail.com>"
git add config.example.toml AGENTS.md
stg refresh
```

---

## Final verification

- [ ] **Run the whole suite once more**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: all PASS.

- [ ] **Confirm the stack reads cleanly**

Run: `stg series`
Expected (top to bottom):
```
design-infer-guidance-model
claude-driver-model
agent-model-dispatch
infer-guidance-config
infer-guidance-prompt
docs-infer-guidance-model
```

- [ ] **(Optional) Build + restart to smoke-test against a real config**

Per AGENTS.md, to deploy: `go build -o ~/.local/bin/quack ./cmd/quack` then restart the unit. If validating live, add an `[agents.infer]` block (with `model` + `headless = true`) and an `infer_guidance` string to `~/.config/quack/config.toml`, set `infer_agent = "infer"`, and confirm a fluent mention still routes correctly. **Note (from AGENTS.md):** if this is run from inside a quack headless session, use the detached `systemd-run --user --on-active=10 systemctl --user restart quack.service` form, not a direct restart.

---

## Self-Review

**Spec coverage:**
- `infer_guidance` config field → Task 3. ✓
- Guidance framing + prompt injection + omitted-when-empty → Task 4 (`guidanceBlock`, template `%s`, tests for both present/absent). ✓
- Per-agent `model` on `agent.Agent` → Task 2. ✓
- `--model` in `args()` and `OneShot` (via `oneShotArgs`) → Task 1. ✓
- Dispatch `switch name` → `switch a.Command` → Task 2. ✓
- Codex untouched / ignores `model` → no codex change in any task; documented in Task 5. ✓
- Error handling (empty guidance → no-op; empty model → no flag; bad model → existing scratch fallback) → covered by the unset-path assertions in Tasks 1 & 4; the fallback path is pre-existing and unchanged. ✓
- Tests with existing fakes → Tasks 1, 2, 3, 4 all use `claude_test`/`config_test`/`infer_test` patterns; no network. ✓
- Docs (`config.example.toml`, `AGENTS.md`) → Task 5. ✓

**Placeholder scan:** No TBD/TODO/"handle edge cases"; every code step shows complete code and every run step shows the command + expected result.

**Type consistency:** `Claude.Model` (Task 1) ↔ `agent.Agent.Model` (Task 2) ↔ `main.go` `Model: a.Model` (Task 2). `guidanceBlock` (Task 4) ↔ its call in `inferDirective` (Task 4). `config.Config.InferGuidance` (Task 3) ↔ `session.Config.InferGuidance` (Task 3) ↔ `s.cfg.InferGuidance` use (Task 4). `oneShotArgs` defined and used in Task 1, asserted in Task 1's test. All consistent.
