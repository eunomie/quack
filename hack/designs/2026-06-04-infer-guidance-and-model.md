# Infer guidance & per-agent model

Date: 2026-06-04

## Problem

The fluent path (default mention) hands a natural-language request plus recent
Discord history to the **infer one-shot**, which emits a `command.Directive` as
JSON (see `2026-06-03-fluent-directive-inference.md`). Two gaps:

1. **No standing environment priors.** The infer agent only knows the request and
   the recent conversation. Stable facts that would sharpen resolution — where
   repos live locally, that a bare "dagger" means `dagger/dagger`, which owners
   the user works under — have to be re-derived from history every time, or are
   simply unavailable. This mostly costs *target* and *name* quality.
2. **Infer is pinned to the working agent's model.** The infer step runs on the
   configured `infer_agent` (defaults to `name_agent`, i.e. claude) at the CLI's
   default model. It sits on the **critical path before the thread opens**
   (the fluent-inference doc flags latency as the main trade-off), yet it is a
   cheap, bounded task — read a request, emit validated JSON — that a faster model
   would serve well. There is no way to select a model per agent today.

We want to (a) feed the infer agent optional standing guidance and (b) let it run
on a faster model — **without** touching the working coding agent, which already
receives `<quack-context>` plus the resolved-context block and must not be biased
by environment priors.

## Goal

Two small, independent, opt-in config levers:

- `infer_guidance` — a free-text string folded into the infer prompt as labeled
  environment hints. Empty (the default) ⇒ no change to today's behavior.
- a per-agent `model` field (claude driver) so a dedicated `[agents.infer]` entry
  can run the infer one-shot on a faster model, leaving the working agent on the
  CLI default.

Non-goals: structured guidance (repo-alias maps), a codex `model` field, and any
system prompt on the working agent. All explicitly deferred (YAGNI).

## Design

### Feature 1 — `infer_guidance` (free-text resolution hints)

**Config.** New optional top-level field on `config.Config`:

```go
InferGuidance string `toml:"infer_guidance"` // standing hints for the infer one-shot
```

No default — empty means the feature is off. Plumbed through to
`session.Config.InferGuidance` and mapped in `cmd/quack/main.go`, exactly like the
existing `Infer*` fields.

**Prompt injection (`internal/session/infer.go`).** `inferPromptTemplate` gains a
third `%s` for an optional section placed **between the field rules and the
conversation**. quack supplies a fixed wrapper; the user's text goes inside it:

```
Environment hints (use ONLY to resolve the target repo/path and the name;
never invent a target when the request names no repo/dir):
<infer_guidance text>
```

A helper renders this:

```go
// guidanceBlock wraps standing infer hints, or "" when s is empty/all-whitespace.
func guidanceBlock(s string) string
```

Returning `""` when guidance is empty makes the entire section (header + framing)
vanish, so users who don't set it see byte-identical prompts to today. The fixed
"never invent a target" framing is the mitigation for the over-attach risk: a
strong prior must not turn a scratch question ("how does git rebase work?") into a
repo session. The user's text stays pure hints, e.g.:

```toml
infer_guidance = """
Repos are cloned under ~/dev/src. A bare "dagger" means dagger/dagger.
I mostly work on the eunomie/* and dagger/* owners.
"""
```

The call site becomes
`fmt.Sprintf(inferPromptTemplate, guidanceBlock(s.cfg.InferGuidance), convo, raw)`.
Output is still the same validated JSON, so a bad hint stays bounded by
`mapInferred` (unknown agent/effort dropped, bad target errors in `prepare`) and
the existing scratch-dir fallback.

### Feature 2 — per-agent `model` (claude-only), isolated to infer

**Config.** `agent.Agent` gains:

```go
Model string `toml:"model"` // claude: --model value; unset => CLI default
```

**Driver.** `agentproc.Claude` gains a `Model string` field. `--model <model>` is
appended when set, in **both** code paths:

- `args(Turn)` (RunTurn) — so a deliberately model-pinned working agent also works.
- the one-shot path (`OneShot`) — used by infer and `SuggestName`.

To keep the one-shot flag unit-testable without exec, extract the argv build into a
helper:

```go
func (d Claude) oneShotArgs(prompt, effort string) []string
```

`OneShot` calls it, then execs. The codex driver is untouched; a `model` set on a
codex agent entry is silently ignored (documented in `config.example.toml`).

**Isolation via a dedicated agent entry.** The user points `infer_agent` at a new
entry that sets `model`; the working `[agents.claude]` sets none and keeps the CLI
default:

```toml
[agents.infer]
command = "claude"
model   = "claude-haiku-4-5-20251001"
infer_agent = "infer"
```

`SuggestName` on that agent rides along for free.

### Required change: driver dispatch keys on command, not map name

`cmd/quack/main.go` currently builds drivers with `switch name`, so it only
creates a Claude driver for an agent literally named `"claude"` (and Codex for
`"codex"`); any other name hits the `default` branch and is silently skipped. A
`[agents.infer]` entry would therefore never register a driver, and
`inferDirective` would fall back to the default agent — the model override would
silently do nothing.

The fix: dispatch on the executable instead —

```go
switch a.Command {
case "claude": // build agentproc.Claude{..., Model: a.Model}
case "codex":  // build agentproc.Codex{...}
default:       // log "no driver; ignoring"
}
```

This makes the map key a free-form label, which the `infer_agent` / `name_agent` /
`default_agent` indirection already assumed, and is a latent correctness fix
(custom-named agents never worked before). Exact-match on `"claude"` / `"codex"`
is preserved, consistent with the existing `a.Command == "claude"` checks in
`internal/agent`; a full-path command falls to `default` as it does today.

## Data flow (unchanged shape)

```
mention (no !) → handleFluent
  → recentHistory (Discord)
  → inferDirective:
      prompt = inferPromptTemplate(guidanceBlock(cfg.InferGuidance), history, raw)
      OneShot on drivers[infer_agent]  ← may carry --model via the agent's Model
      → parseInferred → mapInferred → Directive
  → run (shared launch path, unchanged)
  (on any infer failure → scratch-dir fallback, unchanged)
```

## Error handling

- Empty `infer_guidance` → section omitted; identical to today.
- Empty `model` → no `--model` flag; CLI default (today's behavior).
- Bad `model` string → claude CLI errors → infer `OneShot` errors → existing
  graceful scratch-dir fallback already covers it (with the muted note).

## Testing (all with existing fakes, no network)

- `internal/config/config_test.go`: `infer_guidance` and an agent `model` load
  from TOML.
- `internal/agentproc/claude_test.go`: `--model` present when `Model` set / absent
  when empty, in first-turn args, resume args, and `oneShotArgs`.
- `internal/session/infer_test.go`: the infer prompt (captured via the fake
  driver's `oneShotSeen`) **contains** the guidance text + framing when set, and
  **omits** the section when empty.
- Dispatch: an `[agents.infer]` with `command = "claude"` registers a Claude
  driver (covered by the `switch a.Command` change; verified manually on run).

## Files touched

`internal/config/config.go`, `internal/agent/agent.go`,
`internal/agentproc/claude.go`, `internal/session/service.go` (Config struct),
`internal/session/infer.go`, `cmd/quack/main.go`, `config.example.toml`,
`AGENTS.md`, plus the tests above.

## Trade-offs

- **Free-text over structured guidance.** A single string is the natural fit for
  an LLM prompt and keeps the open-sourced repo free of the maintainer's personal
  values (they live in the local config). A repo-alias map would add guard-rails
  but a rigid vocabulary; deferred until a string proves insufficient.
- **Smaller infer model trades a little quality for latency.** A faster model is
  slightly weaker at resolving references ("this feature") and naming. The JSON
  validation and scratch fallback absorb the downside; Sonnet is the safe middle,
  Haiku worth measuring. Lowering `infer_effort` to `low` remains the zero-code
  speed lever.
- **Dispatch change is a (small) behavior change.** Switching `switch name` →
  `switch a.Command` is necessary for the dedicated-infer-agent approach and fixes
  latent dead code, but it does alter how drivers are selected; covered by the
  manual-run check.
