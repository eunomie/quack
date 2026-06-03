# Fluent directive inference

Date: 2026-06-03

> **Update (2026-06-03):** the trigger was reversed shortly after this design
> landed. Fluent inference is now the **default** path (any mention with no
> prefix), and a leading **`!`** opts into the explicit `internal/command`
> grammar instead (first line = directive args, rest = prompt). The prefix helper
> is `directivePrefix`. The mechanics below are unchanged — only which path is the
> default. Read "`! ` prefix → fluent" as "no prefix → fluent; `!` prefix →
> explicit grammar" throughout.

## Problem

quack's command grammar is precise but front-loaded: to steer a session you put
the constraints on the *first line* of the mention — target repo/path, base
branch, agent, effort, name, headless, worktree — and the prompt on the lines
after. That's teachable but not fluent. A user thinking in prose ("in
dagger/dagger create feature A with claude at high effort") has to mentally
recompile their intent into the directive format.

We want a path where the user just *says what they want* in one natural sentence
and quack figures out the fields itself — without losing any of the existing
control.

## Goal

A new, opt-in **fluent** path: a mention whose text starts with `! ` is handed to
an agent that reads the request (plus recent Discord context) and emits the
directive fields. quack then launches exactly as it does today. The plain
(non-`!`) grammar is untouched.

This is a first version with a deliberately simple trigger (`! `) that may be
revised once it has been used.

### Examples

- `! in dagger/dagger create the feature A` → target `dagger/dagger`, worktree,
  inferred name (e.g. `feature-a`), default agent, headless.
- `! create an alias 'gs' that prints git status` → no target → scratch dir,
  prompt run as-is.
- `! create this feature in eunomie/java-sdk` → target `eunomie/java-sdk`; "this
  feature" is resolved from the recent Discord conversation by the infer agent,
  which also yields a sensible name and a short context blurb passed to the
  working agent.

## Design

### Trigger & routing (`internal/session/service.go`)

`Handle` gains a single branch at the top, before `command.Parse`: if the
mention-stripped content starts with `! `, the remainder is the raw request and
control goes to the new infer path; otherwise the existing grammar runs
unchanged. The prefix test is a one-line helper (`fluentPrefix`) so the trigger
is cheap to change later.

The key simplifying property: **both paths produce a `command.Directive`**. The
infer path fills the same struct the parser fills, so everything downstream
(`prepare` → launch, headless or tmux) is shared and unchanged.

### The infer path (new `internal/session/infer.go`)

1. **React 👀** on the user's triggering message immediately — instant feedback
   while the infer agent runs (it is now on the critical path before the thread
   opens).
2. **Fetch recent Discord history** — one REST call for ~`infer_history_limit`
   messages before the trigger, ordered oldest→newest, rendered as `author:
   text` lines, length-capped. This is what lets the infer agent resolve
   references like "this feature" and pick a good name — instead of doing an
   agentic Discord-reading loop, quack inlines the context so the call stays a
   single fast one-shot.
3. **Run one read-only one-shot** on the configured infer agent (defaults to
   `name_agent`) at `infer_effort` (default `medium`). The prompt contains the
   raw request and the channel history and asks for **only** a JSON object.
4. **Parse + validate** the JSON into a `command.Directive` (below).
5. **Echo the interpretation** back to the thread, **muted** (`PostSilent`, no
   notification), e.g. `interpreted as: repo dagger/dagger · agent claude ·
   effort xhigh · worktree feature-a · headless`.
6. **Launch** via the shared flow.

### JSON contract

The infer agent must reply with exactly one JSON object:

```json
{
  "target":   "",          // "" | "owner/repo" | "/abs/path" | "~/path" | "temp-dir"
  "base":     "",          // "" => repo default branch
  "worktree": true,        // false => run directly in the checkout (no-wt)
  "agent":    "",          // "" | "claude" | "codex"
  "effort":   "",          // "" | low | medium | high | xhigh
  "name":     "kebab-name",// short task name
  "headless": true,
  "context":  ""           // optional: 1-paragraph resolution of references like "this feature"
}
```

Mapping and validation → `Directive`:

| JSON | Directive | Validation / fallback |
|------|-----------|-----------------------|
| `target` | `Target` | passed through; downstream `prepare` already errors on a bad repo/path |
| `base` | `Base` | passed through |
| `worktree` | `NoWorktree = !worktree` | — |
| `agent` | `Agent` | must be a configured agent; unknown → dropped (config default applies) |
| `effort` | `Effort` | must be a known effort token; unknown → dropped |
| `name` | `Name` | slugified (`worktree.Slugify`); empty/`session` sentinel → dropped |
| `headless` | `Headless` | — |
| `context` | (prompt) | non-empty → prepended to the working agent's prompt |

The JSON is extracted defensively (trimming any ```` ```json ```` fence) before
`json.Unmarshal`.

### Prompts

- The **working agent** receives the user's raw text **verbatim** (minus `! `),
  unchanged — the same text the infer agent saw. On top of that it still gets the
  existing `<quack-context>` header, and, when the infer agent produced one, the
  resolved `context` block prepended. The infer step only *enriches* the prompt;
  it never rewrites it.
- The **name** comes from infer, which **replaces** the separate `suggestName`
  LLM call on this path (one fewer round-trip). It is treated as a *suggestion*,
  not an explicit name: collisions auto-suffix (`-2`, `-3`, …) via the existing
  `resolveName(explicit=false)`, never a hard error.

### Fallback (graceful)

The fluent path never hard-fails on infer trouble. If the one-shot errors, times
out (~60s ceiling), or returns unparseable JSON, quack falls back to today's
no-target behavior — run the raw text as a **scratch-dir** prompt at the scratch
effort — and notes the fallback **muted** in the thread. A malformed individual
field is dropped (per the table) rather than failing the whole directive.

### Discord history (`internal/discord`)

The Discord port gains a read method:

```go
RecentMessages(ctx, channelID, beforeID string, limit int) ([]Message, error)
```

implemented via discordgo's `ChannelMessages` (newest-first; reversed to
chronological). `Message` is a small `{Author, Content}` value owned by the
`session` package's interface. The unit-test fake returns canned messages, so no
network is needed in tests. It is exposed through the existing consumer-side
interface seam (either extending `Replier` or a sibling reader interface — an
implementation detail kept on the Discord side).

### Agent one-shot (`internal/agentproc`)

Add a generic read-only one-shot to `Driver`:

```go
OneShot(ctx context.Context, prompt, effort string) (string, error)
```

returning the agent's raw final text. claude uses `--output-format json` +
`--permission-mode plan` + the effort template; codex uses `exec --json` with the
effort template, accumulating `AssistantText`. The existing `SuggestName` is a
fixed-prompt, fixed-effort special case of this and may later be re-expressed on
top of it; for this change it is left as-is to limit blast radius. The infer
prompt construction and JSON parsing live in `session`, not in the drivers, so
the contract has a single home.

### Small refactor

Extract `Handle`'s post-directive tail (from `prepare` through the
headless/tmux launch) into a shared `launch(...)` helper. Both the plain and
fluent paths converge there, keeping `Handle` readable and the new path thin.

### Config (`internal/config`)

Three new optional fields, plumbed through `session.Config` and `main.go`:

| Field | Default | Purpose |
|-------|---------|---------|
| `infer_agent` | `name_agent` | which agent runs the infer one-shot |
| `infer_effort` | `medium` | speed/quality lever, tunable without a rebuild |
| `infer_history_limit` | `20` | how many recent messages to feed the infer agent |

Documented in `config.example.toml` and `AGENTS.md`.

## Testing

All with fakes (a fake infer driver returning canned JSON, a fake Replier
returning canned history) — consistent with the existing fake-based `session`
tests, no real agent or network:

- clean request → correct `Directive` and launch
- no target inferred → scratch dir, raw prompt
- unknown `agent` / `effort` → dropped, defaults apply
- unparseable JSON / one-shot error → graceful scratch fallback + muted note
- name collision → auto-suffix
- non-empty `context` → prepended to the working prompt
- `! ` routing vs plain grammar (plain path unaffected)

## Trade-offs

- **Latency on the critical path.** The infer one-shot runs before the thread
  opens; the 👀 reaction covers the gap. At `medium` one-shot this is a few
  seconds. If it drags, dropping `infer_effort` to `low` is the first lever — we
  measure with real use (per the agreed plan to defer Discord-reading work to
  the infer agent and revisit only if it proves too slow).
- **Trigger is a magic prefix.** `! ` is a deliberately minimal v1 trigger and
  may be reversed/changed; it is isolated in one helper to make that easy.
- **Inlined history, not full reading.** The infer agent sees a bounded window
  of recent messages, not the whole channel. That keeps the call fast and is
  enough for "what were we just talking about"; deeper history would need the
  working agent (which has the permalink and can read Discord itself).
