# quack Headless Bidirectional Sessions — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add an opt-in `headless=true` mode that makes a quack session conversational from Discord — the agent's per-turn answers stream into the session thread, and messages posted in that thread are forwarded to the agent as its next turn.

**Architecture:** A unified **resume-per-turn** model. Each turn is one non-interactive agent invocation that emits a JSON event stream and resumes the prior turn by id (`claude -p … --resume <id>`; `codex exec resume <id> …`). Agent CLIs sit behind an `agentproc.Driver` interface (faked in tests); a per-thread, in-memory registry serializes turns and maps Discord threads to sessions. The default (non-headless) tmux path is untouched.

**Tech Stack:** Go 1.23; `bwmarrin/discordgo`; stdlib `os/exec` + `encoding/json`. Spec: `hack/designs/2026-06-01-quack-headless-bidirectional-design.md`.

---

## Commit convention (StGit — every task)

This repo uses StGit. **Never** `git commit`; **never** add `Co-Authored-By`. End each task:

```bash
stg new <patch-slug> -m "<subject>

Signed-off-by: Yves Brissaud <yves.brissaud@gmail.com>"
git add <files>
stg refresh
```

Each task names its `<patch-slug>`, `<subject>`, and `<files>`.

---

## File structure

```
internal/command/directive.go          # + Headless bool, parse `headless=`
internal/agent/agent.go                 # + Headless/PermissionMode/AllowedTools fields
internal/agentproc/                      # NEW — headless agent drivers
  driver.go                              # Driver interface, Turn, Event, TurnDone
  claude.go                              # claude resume-per-turn driver + JSON parse
  claude_test.go
  codex.go                               # codex resume-per-turn driver + JSON parse
  codex_test.go
  testdata/claude-turn.jsonl             # recorded fixture
  testdata/codex-turn.jsonl              # recorded fixture
internal/session/
  render.go                              # pure: answer assembly + Discord 2000-char split
  render_test.go
  headless.go                            # registry + turn loop + event→Discord
  headless_test.go                       # faked Driver + Replier
  service.go                             # branch headless vs tmux (modify)
internal/discord/bot.go                  # route thread msgs, /stop, archive (modify)
cmd/quack/main.go                        # build drivers + registry (modify)
config.example.toml                      # headless agent settings (modify)
README.md                                # headless usage (modify)
```

---

## Task 1: `headless` directive flag

**Files:**
- Modify: `internal/command/directive.go`
- Modify: `internal/command/directive_test.go`

- [ ] **Step 1: Add the failing test**

Append to `internal/command/directive_test.go`:
```go
func TestParse_Headless(t *testing.T) {
	d, err := Parse("repo/x headless=true\nGo.")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !d.Headless {
		t.Errorf("Headless = false, want true")
	}

	d2, _ := Parse("repo/x\nGo.")
	if d2.Headless {
		t.Errorf("Headless defaulted true")
	}

	if _, err := Parse("repo/x headless=maybe\nGo."); err == nil {
		t.Errorf("expected error for non-bool headless")
	}
}
```

- [ ] **Step 2: Run it, verify it fails**

Run: `go test ./internal/command/ -run TestParse_Headless -v`
Expected: FAIL — `d.Headless undefined`.

- [ ] **Step 3: Implement**

In `internal/command/directive.go`, add `"strconv"` to imports, add the field to `Directive`:
```go
	Base     string // optional base branch
	Headless bool   // optional: run conversational headless session
	Prompt   string // required, verbatim, may be multiline
```
Add a case to the flag switch (before `default:`):
```go
		case "headless":
			b, perr := strconv.ParseBool(val)
			if perr != nil {
				return nil, &UsageError{Msg: fmt.Sprintf("bad headless %q (want true/false). %s", val, usage)}
			}
			d.Headless = b
```

- [ ] **Step 4: Run, verify pass**

Run: `go test ./internal/command/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**
  - patch-slug: `command-headless-flag`
  - subject: `feat(command): parse headless directive flag`
  - files: `internal/command/directive.go internal/command/directive_test.go`

---

## Task 2: agent config fields for headless

**Files:**
- Modify: `internal/agent/agent.go`
- Modify: `internal/agent/agent_test.go`

- [ ] **Step 1: Add the failing test**

Append to `internal/agent/agent_test.go`:
```go
func TestHeadlessDefaults(t *testing.T) {
	a := Agent{Command: "claude", Headless: true}
	if got := a.Mode(); got != "acceptEdits" {
		t.Errorf("Mode default = %q, want acceptEdits", got)
	}
	b := Agent{Command: "claude", Headless: true, PermissionMode: "bypassPermissions"}
	if got := b.Mode(); got != "bypassPermissions" {
		t.Errorf("Mode = %q", got)
	}
}
```

- [ ] **Step 2: Run, verify fail**

Run: `go test ./internal/agent/ -run TestHeadlessDefaults -v`
Expected: FAIL — `Headless`/`Mode` undefined.

- [ ] **Step 3: Implement**

In `internal/agent/agent.go`, extend the struct and add `Mode`:
```go
type Agent struct {
	Command        string `toml:"command"`
	EffortTemplate string `toml:"effort_template"`
	Headless       bool   `toml:"headless"`        // has a headless driver
	PermissionMode string `toml:"permission_mode"` // claude: acceptEdits|bypassPermissions
	AllowedTools   string `toml:"allowed_tools"`   // claude: --allowedTools value
}

// Mode returns the headless permission mode, defaulting to acceptEdits.
func (a Agent) Mode() string {
	if a.PermissionMode == "" {
		return "acceptEdits"
	}
	return a.PermissionMode
}
```

- [ ] **Step 4: Run, verify pass**

Run: `go test ./internal/agent/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**
  - patch-slug: `agent-headless-config`
  - subject: `feat(agent): headless config fields and permission-mode default`
  - files: `internal/agent/agent.go internal/agent/agent_test.go`

---

## Task 3: agentproc contract

**Files:**
- Create: `internal/agentproc/driver.go`

- [ ] **Step 1: Write the contract** (no test — pure type definitions exercised by Tasks 4–5)

`internal/agentproc/driver.go`:
```go
// Package agentproc runs coding agents headless, one process per turn,
// resuming the prior turn's session by id. It normalizes each agent's JSON
// event stream into a small Event set the rest of quack consumes.
package agentproc

import "context"

// Turn is one request to an agent.
type Turn struct {
	SessionRef string // agent-opaque resume token; "" = first turn
	Prompt     string // user text (first turn includes the quack-context header)
	Workdir    string // child process working directory (the worktree)
	Effort     string // pass-through; applied on the first turn only
}

// Event is emitted while a turn runs.
type Event interface{ isEvent() }

// AssistantText is a chunk of the agent's answer.
type AssistantText struct{ Text string }

// ToolActivity is a compact one-line note about tool/command use.
type ToolActivity struct{ Label string }

func (AssistantText) isEvent() {}
func (ToolActivity) isEvent()  {}

// TurnDone is the terminal result of a turn (returned, not emitted).
type TurnDone struct {
	SessionRef string  // ref to resume the next turn
	CostUSD    float64 // best-effort, 0 if unknown
	Err        error   // non-nil if the turn failed
}

// Driver runs one turn as a headless child, emitting in-flight Events via emit
// and returning the terminal TurnDone.
type Driver interface {
	RunTurn(ctx context.Context, t Turn, emit func(Event)) TurnDone
}
```

- [ ] **Step 2: Verify it builds**

Run: `go build ./internal/agentproc/`
Expected: exits 0.

- [ ] **Step 3: Commit**
  - patch-slug: `agentproc-contract`
  - subject: `feat(agentproc): driver/event contract for headless turns`
  - files: `internal/agentproc/driver.go`

---

## Task 4: claude driver

**Files:**
- Create: `internal/agentproc/claude.go`
- Create: `internal/agentproc/claude_test.go`
- Create: `internal/agentproc/testdata/claude-turn.jsonl`

- [ ] **Step 1: Record a real fixture** *(verify-at-impl gate)*

Run a real headless turn in a throwaway dir to capture the actual event shapes, then save a trimmed copy:
```bash
cd "$(mktemp -d)" && git init -q -b main . && git commit -q --allow-empty -m init
claude -p "Say hello in one short sentence, then stop." \
  --output-format stream-json --verbose --permission-mode acceptEdits \
  > /tmp/claude-sample.jsonl 2>&1 || true
cat /tmp/claude-sample.jsonl
```
Copy 3–5 representative lines (one `assistant`/text, one `tool_use` if present, the final `result`) into `internal/agentproc/testdata/claude-turn.jsonl`. If field names differ from those used below (`type`, `session_id`, `message.content[].{type,text,name}`, `result`, `is_error`, `total_cost_usd`), update both the fixture and the structs in Step 3 to match what the CLI actually emits. Also confirm auth works without `ANTHROPIC_API_KEY` (reuses `~/.claude`); if not, document the required env in Task 11.

Minimal fixture to start (`internal/agentproc/testdata/claude-turn.jsonl`):
```json
{"type":"system","subtype":"init","session_id":"sess-abc"}
{"type":"assistant","session_id":"sess-abc","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"git status"}}]}}
{"type":"assistant","session_id":"sess-abc","message":{"content":[{"type":"text","text":"Hello! Working tree is clean."}]}}
{"type":"result","subtype":"success","session_id":"sess-abc","result":"Hello! Working tree is clean.","is_error":false,"total_cost_usd":0.012}
```

- [ ] **Step 2: Write the failing parse test**

`internal/agentproc/claude_test.go`:
```go
package agentproc

import (
	"os"
	"strings"
	"testing"
)

func TestParseClaudeStream(t *testing.T) {
	f, err := os.Open("testdata/claude-turn.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	var texts, tools []string
	done := parseClaudeStream(f, func(e Event) {
		switch ev := e.(type) {
		case AssistantText:
			texts = append(texts, ev.Text)
		case ToolActivity:
			tools = append(tools, ev.Label)
		}
	})

	if done.SessionRef != "sess-abc" {
		t.Errorf("SessionRef = %q", done.SessionRef)
	}
	if done.Err != nil {
		t.Errorf("Err = %v", done.Err)
	}
	if len(texts) != 1 || !strings.Contains(texts[0], "Hello") {
		t.Errorf("texts = %v", texts)
	}
	if len(tools) != 1 || !strings.Contains(tools[0], "Bash") {
		t.Errorf("tools = %v", tools)
	}
}

func TestClaudeArgs(t *testing.T) {
	d := Claude{Command: "claude", EffortTemplate: "--effort {effort}", PermissionMode: "acceptEdits", AllowedTools: "Read,Edit"}
	first := d.args(Turn{Prompt: "hi", Effort: "high"})
	if !contains(first, "-p") || !contains(first, "hi") || !contains(first, "--effort") || !contains(first, "high") {
		t.Errorf("first args = %v", first)
	}
	if contains(first, "--resume") {
		t.Errorf("first turn must not resume: %v", first)
	}
	next := d.args(Turn{Prompt: "again", SessionRef: "sess-abc", Effort: "high"})
	if !contains(next, "--resume") || !contains(next, "sess-abc") {
		t.Errorf("resume args = %v", next)
	}
	if contains(next, "--effort") {
		t.Errorf("effort must apply only on first turn: %v", next)
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
```

- [ ] **Step 3: Run, verify fail**

Run: `go test ./internal/agentproc/ -run Claude -v`
Expected: FAIL — `parseClaudeStream`/`Claude` undefined.

- [ ] **Step 4: Implement**

`internal/agentproc/claude.go`:
```go
package agentproc

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// Claude drives the `claude` CLI in headless resume-per-turn mode.
type Claude struct {
	Command        string
	EffortTemplate string
	PermissionMode string
	AllowedTools   string
}

func (d Claude) args(t Turn) []string {
	a := []string{"-p", t.Prompt, "--output-format", "stream-json", "--verbose",
		"--permission-mode", d.PermissionMode}
	if d.AllowedTools != "" {
		a = append(a, "--allowedTools", d.AllowedTools)
	}
	if t.SessionRef != "" {
		a = append(a, "--resume", t.SessionRef)
	} else if t.Effort != "" && d.EffortTemplate != "" {
		a = append(a, strings.Fields(strings.ReplaceAll(d.EffortTemplate, "{effort}", t.Effort))...)
	}
	return a
}

func (d Claude) RunTurn(ctx context.Context, t Turn, emit func(Event)) TurnDone {
	cmd := exec.CommandContext(ctx, d.Command, d.args(t)...)
	cmd.Dir = t.Workdir
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return TurnDone{Err: err}
	}
	cmd.Stderr = cmd.Stdout // fold stderr into the same stream for diagnostics
	if err := cmd.Start(); err != nil {
		return TurnDone{Err: err}
	}
	done := parseClaudeStream(stdout, emit)
	werr := cmd.Wait()
	if done.SessionRef == "" && done.Err == nil && werr != nil {
		done.Err = fmt.Errorf("claude exited: %w", werr)
	}
	return done
}

type claudeLine struct {
	Type      string `json:"type"`
	SessionID string `json:"session_id"`
	Message   struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
			Name string `json:"name"`
		} `json:"content"`
	} `json:"message"`
	Result       string  `json:"result"`
	IsError      bool    `json:"is_error"`
	TotalCostUSD float64 `json:"total_cost_usd"`
}

func parseClaudeStream(r io.Reader, emit func(Event)) TurnDone {
	var done TurnDone
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1024*1024), 8*1024*1024) // large lines
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var m claudeLine
		if json.Unmarshal([]byte(line), &m) != nil {
			continue // tolerate non-JSON noise
		}
		if m.SessionID != "" {
			done.SessionRef = m.SessionID
		}
		switch m.Type {
		case "assistant":
			for _, c := range m.Message.Content {
				switch c.Type {
				case "text":
					if strings.TrimSpace(c.Text) != "" {
						emit(AssistantText{Text: c.Text})
					}
				case "tool_use":
					emit(ToolActivity{Label: "🔧 " + c.Name})
				}
			}
		case "result":
			done.CostUSD = m.TotalCostUSD
			if m.IsError {
				done.Err = fmt.Errorf("%s", strings.TrimSpace(m.Result))
			}
		}
	}
	return done
}
```

- [ ] **Step 5: Run, verify pass**

Run: `go test ./internal/agentproc/ -run Claude -v`
Expected: PASS.

- [ ] **Step 6: Commit**
  - patch-slug: `agentproc-claude`
  - subject: `feat(agentproc): claude headless resume-per-turn driver`
  - files: `internal/agentproc/claude.go internal/agentproc/claude_test.go internal/agentproc/testdata/claude-turn.jsonl`

---

## Task 5: codex driver

**Files:**
- Create: `internal/agentproc/codex.go`
- Create: `internal/agentproc/codex_test.go`
- Create: `internal/agentproc/testdata/codex-turn.jsonl`

- [ ] **Step 1: Record a real fixture** *(verify-at-impl gate)*

```bash
cd "$(mktemp -d)" && git init -q -b main . && git commit -q --allow-empty -m init
codex exec "Say hello in one sentence, then stop." --json > /tmp/codex-sample.jsonl 2>&1 || true
cat /tmp/codex-sample.jsonl
# confirm the resume invocation for follow-ups:
codex exec --help 2>&1 | grep -iA2 resume || codex exec resume --help 2>&1 | head
```
Save representative lines to `internal/agentproc/testdata/codex-turn.jsonl`. Reconcile the struct in Step 3 and the resume arg in `args()` with what the installed `codex` emits/accepts. Starter fixture (adjust to reality):
```json
{"type":"thread.started","thread_id":"th-xyz"}
{"type":"item.completed","item":{"type":"command_execution","command":"git status"}}
{"type":"item.completed","item":{"type":"agent_message","text":"Hello! All good."}}
{"type":"turn.completed","thread_id":"th-xyz"}
```

- [ ] **Step 2: Write the failing test**

`internal/agentproc/codex_test.go`:
```go
package agentproc

import (
	"os"
	"strings"
	"testing"
)

func TestParseCodexStream(t *testing.T) {
	f, err := os.Open("testdata/codex-turn.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	var texts, tools []string
	done := parseCodexStream(f, func(e Event) {
		switch ev := e.(type) {
		case AssistantText:
			texts = append(texts, ev.Text)
		case ToolActivity:
			tools = append(tools, ev.Label)
		}
	})
	if done.SessionRef != "th-xyz" {
		t.Errorf("SessionRef = %q", done.SessionRef)
	}
	if len(texts) != 1 || !strings.Contains(texts[0], "Hello") {
		t.Errorf("texts = %v", texts)
	}
	if len(tools) != 1 {
		t.Errorf("tools = %v", tools)
	}
}

func TestCodexArgs(t *testing.T) {
	d := Codex{Command: "codex"}
	first := d.args(Turn{Prompt: "hi"})
	if first[0] != "exec" || !contains(first, "hi") || !contains(first, "--json") {
		t.Errorf("first = %v", first)
	}
	next := d.args(Turn{Prompt: "again", SessionRef: "th-xyz"})
	if next[0] != "exec" || next[1] != "resume" || !contains(next, "th-xyz") {
		t.Errorf("resume = %v", next)
	}
}
```

- [ ] **Step 3: Run, verify fail**

Run: `go test ./internal/agentproc/ -run Codex -v`
Expected: FAIL — undefined.

- [ ] **Step 4: Implement**

`internal/agentproc/codex.go`:
```go
package agentproc

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// Codex drives the `codex` CLI in headless resume-per-turn mode
// (`codex exec … --json`, follow-ups via `codex exec resume <id> …`).
type Codex struct {
	Command string
}

func (d Codex) args(t Turn) []string {
	if t.SessionRef != "" {
		return []string{"exec", "resume", t.SessionRef, t.Prompt, "--json"}
	}
	return []string{"exec", t.Prompt, "--json"}
}

func (d Codex) RunTurn(ctx context.Context, t Turn, emit func(Event)) TurnDone {
	cmd := exec.CommandContext(ctx, d.Command, d.args(t)...)
	cmd.Dir = t.Workdir
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return TurnDone{Err: err}
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return TurnDone{Err: err}
	}
	done := parseCodexStream(stdout, emit)
	werr := cmd.Wait()
	if done.Err == nil && werr != nil && done.SessionRef == "" {
		done.Err = fmt.Errorf("codex exited: %w", werr)
	}
	return done
}

type codexLine struct {
	Type     string `json:"type"`
	ThreadID string `json:"thread_id"`
	Item     struct {
		Type    string `json:"type"`
		Text    string `json:"text"`
		Command string `json:"command"`
	} `json:"item"`
}

func parseCodexStream(r io.Reader, emit func(Event)) TurnDone {
	var done TurnDone
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1024*1024), 8*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var m codexLine
		if json.Unmarshal([]byte(line), &m) != nil {
			continue
		}
		if m.ThreadID != "" {
			done.SessionRef = m.ThreadID
		}
		switch m.Type {
		case "item.completed", "item.updated":
			switch m.Item.Type {
			case "agent_message":
				if strings.TrimSpace(m.Item.Text) != "" {
					emit(AssistantText{Text: m.Item.Text})
				}
			case "command_execution":
				emit(ToolActivity{Label: "▶ " + m.Item.Command})
			case "file_change", "mcp_tool_call", "web_search":
				emit(ToolActivity{Label: "🔧 " + m.Item.Type})
			}
		case "turn.failed", "error":
			done.Err = fmt.Errorf("codex turn failed")
		}
	}
	return done
}
```

- [ ] **Step 5: Run, verify pass**

Run: `go test ./internal/agentproc/ -v`
Expected: PASS.

- [ ] **Step 6: Commit**
  - patch-slug: `agentproc-codex`
  - subject: `feat(agentproc): codex headless resume-per-turn driver`
  - files: `internal/agentproc/codex.go internal/agentproc/codex_test.go internal/agentproc/testdata/codex-turn.jsonl`

---

## Task 6: Discord answer rendering (pure)

**Files:**
- Create: `internal/session/render.go`
- Create: `internal/session/render_test.go`

- [ ] **Step 1: Write the failing test**

`internal/session/render_test.go`:
```go
package session

import (
	"strings"
	"testing"
)

func TestSplitMessage(t *testing.T) {
	if got := splitMessage("short", 2000); len(got) != 1 || got[0] != "short" {
		t.Fatalf("got %v", got)
	}
	big := strings.Repeat("a", 4500)
	parts := splitMessage(big, 2000)
	if len(parts) != 3 {
		t.Fatalf("want 3 parts, got %d", len(parts))
	}
	for _, p := range parts {
		if len(p) > 2000 {
			t.Errorf("part too long: %d", len(p))
		}
	}
	// prefers newline boundaries
	nl := strings.Repeat("x", 1990) + "\n" + strings.Repeat("y", 100)
	if p := splitMessage(nl, 2000); !strings.HasSuffix(p[0], "x") {
		t.Errorf("did not break on newline: %q", p[0][len(p[0])-5:])
	}
}

func TestEmptyAnswerPlaceholder(t *testing.T) {
	if got := answerOrPlaceholder(""); got == "" {
		t.Errorf("empty answer should produce a placeholder")
	}
}
```

- [ ] **Step 2: Run, verify fail**

Run: `go test ./internal/session/ -run 'SplitMessage|EmptyAnswer' -v`
Expected: FAIL — undefined.

- [ ] **Step 3: Implement**

`internal/session/render.go`:
```go
package session

import "strings"

const discordMax = 2000

// splitMessage breaks s into chunks <= max chars, preferring a newline boundary
// near the end of each chunk.
func splitMessage(s string, max int) []string {
	if max <= 0 {
		max = discordMax
	}
	var out []string
	for len(s) > max {
		cut := max
		if nl := strings.LastIndexByte(s[:max], '\n'); nl > max/2 {
			cut = nl
		}
		out = append(out, strings.TrimRight(s[:cut], "\n"))
		s = strings.TrimLeft(s[cut:], "\n")
	}
	if s != "" {
		out = append(out, s)
	}
	if len(out) == 0 {
		out = []string{""}
	}
	return out
}

// answerOrPlaceholder substitutes a note when a turn produced no visible text.
func answerOrPlaceholder(s string) string {
	if strings.TrimSpace(s) == "" {
		return "_(no text response this turn)_"
	}
	return s
}
```

- [ ] **Step 4: Run, verify pass**

Run: `go test ./internal/session/ -run 'SplitMessage|EmptyAnswer' -v`
Expected: PASS.

- [ ] **Step 5: Commit**
  - patch-slug: `session-render`
  - subject: `feat(session): answer assembly and Discord message split`
  - files: `internal/session/render.go internal/session/render_test.go`

---

## Task 7: headless registry + turn loop

**Files:**
- Create: `internal/session/headless.go`
- Create: `internal/session/headless_test.go`

- [ ] **Step 1: Write the fakes + failing test**

`internal/session/headless_test.go`:
```go
package session

import (
	"context"
	"strings"
	"testing"

	"github.com/eunomie/quack/internal/agentproc"
)

// fakeDriver scripts events per turn and a session ref to return.
type fakeDriver struct {
	turns []scripted
	seen  []agentproc.Turn
}
type scripted struct {
	texts []string
	tools []string
	ref   string
	err   error
}

func (f *fakeDriver) RunTurn(ctx context.Context, t agentproc.Turn, emit func(agentproc.Event)) agentproc.TurnDone {
	i := len(f.seen)
	f.seen = append(f.seen, t)
	s := f.turns[i]
	for _, x := range s.tools {
		emit(agentproc.ToolActivity{Label: x})
	}
	for _, x := range s.texts {
		emit(agentproc.AssistantText{Text: x})
	}
	return agentproc.TurnDone{SessionRef: s.ref, Err: s.err}
}

func newHeadlessService(d agentproc.Driver) (*Service, *fakeReplier) {
	r := newFakeReplier()
	svc := New(Config{StateDir: "/state"}, newFakeGit(), newFakeTmux(), r)
	svc.drivers = map[string]agentproc.Driver{"claude": d}
	return svc, r
}

func TestHeadless_FirstTurnAndResume(t *testing.T) {
	d := &fakeDriver{turns: []scripted{
		{texts: []string{"Hi there."}, tools: []string{"🔧 Bash"}, ref: "sess-1"},
		{texts: []string{"Did it."}, ref: "sess-1"},
	}}
	svc, r := newHeadlessService(d)

	// first turn
	svc.startHeadless(context.Background(), "claude", "thread-1", "/wt", "high",
		"<quack-context>…</quack-context>\n\nDo the thing.")
	svc.waitIdle("thread-1")

	if len(d.seen) != 1 || d.seen[0].SessionRef != "" || d.seen[0].Workdir != "/wt" {
		t.Fatalf("first turn = %+v", d.seen)
	}
	if !anyContains(r.posts, "Hi there.") {
		t.Fatalf("answer not posted: %v", r.posts)
	}

	// second turn from a thread message → must resume sess-1
	svc.feedThread(context.Background(), "thread-1", "now commit")
	svc.waitIdle("thread-1")
	if len(d.seen) != 2 || d.seen[1].SessionRef != "sess-1" || d.seen[1].Prompt != "now commit" {
		t.Fatalf("resume turn = %+v", d.seen)
	}
}

func TestHeadless_StopEndsSession(t *testing.T) {
	d := &fakeDriver{turns: []scripted{{texts: []string{"ok"}, ref: "s"}}}
	svc, _ := newHeadlessService(d)
	svc.startHeadless(context.Background(), "claude", "thread-2", "/wt", "", "go")
	svc.waitIdle("thread-2")
	if !svc.stopThread(context.Background(), "thread-2") {
		t.Fatalf("stop should report it ended a tracked session")
	}
	if svc.tracked("thread-2") {
		t.Fatalf("session still tracked after stop")
	}
	// a message in a now-untracked thread is ignored by feedThread
	if svc.feedThread(context.Background(), "thread-2", "hello") {
		t.Fatalf("feed should report false for untracked thread")
	}
}

func anyContains(ms []postedMsg, sub string) bool {
	for _, m := range ms {
		if strings.Contains(m.content, sub) {
			return true
		}
	}
	return false
}
```

Note: `waitIdle` is a test-only helper added in Step 3 to make the async turn loop deterministic.

- [ ] **Step 2: Run, verify fail**

Run: `go test ./internal/session/ -run TestHeadless -v`
Expected: FAIL — `drivers`/`startHeadless`/etc. undefined.

- [ ] **Step 3: Implement**

`internal/session/headless.go`:
```go
package session

import (
	"context"
	"strings"
	"sync"

	"github.com/eunomie/quack/internal/agentproc"
)

// liveSession is one tracked headless conversation. Between turns no process
// runs; sessionRef carries the agent's resume token forward.
type liveSession struct {
	driver     agentproc.Driver
	workdir    string
	effort     string
	threadID   string
	sessionRef string

	queue chan string
	done  chan struct{} // closed when the loop drains and exits
	mu    sync.Mutex
	idle  *sync.WaitGroup // test sync: counts in-flight + queued turns
}

// startHeadless registers a session and runs its first turn asynchronously.
func (s *Service) startHeadless(ctx context.Context, agentName, threadID, workdir, effort, firstPrompt string) {
	d := s.drivers[agentName]
	ls := &liveSession{
		driver: d, workdir: workdir, effort: effort, threadID: threadID,
		queue: make(chan string, 32), idle: &sync.WaitGroup{},
	}
	s.hmu.Lock()
	if s.sessions == nil {
		s.sessions = map[string]*liveSession{}
	}
	s.sessions[threadID] = ls
	s.hmu.Unlock()

	ls.idle.Add(1)
	ls.queue <- firstPrompt
	go s.runLoop(ctx, ls)
}

// feedThread enqueues a message as the next turn. Returns false if the thread
// is not a tracked headless session.
func (s *Service) feedThread(ctx context.Context, threadID, text string) bool {
	s.hmu.Lock()
	ls, ok := s.sessions[threadID]
	s.hmu.Unlock()
	if !ok {
		return false
	}
	ls.idle.Add(1)
	ls.queue <- text
	return true
}

// stopThread ends a tracked session. Returns false if not tracked.
func (s *Service) stopThread(ctx context.Context, threadID string) bool {
	s.hmu.Lock()
	ls, ok := s.sessions[threadID]
	if ok {
		delete(s.sessions, threadID)
	}
	s.hmu.Unlock()
	if !ok {
		return false
	}
	close(ls.queue)
	_, _ = s.reply.Post(ctx, threadID, "🛑 session stopped")
	return true
}

func (s *Service) tracked(threadID string) bool {
	s.hmu.Lock()
	defer s.hmu.Unlock()
	_, ok := s.sessions[threadID]
	return ok
}

// waitIdle blocks until all queued/in-flight turns for a thread finish (tests).
func (s *Service) waitIdle(threadID string) {
	s.hmu.Lock()
	ls, ok := s.sessions[threadID]
	s.hmu.Unlock()
	if ok {
		ls.idle.Wait()
	}
}

func (s *Service) runLoop(ctx context.Context, ls *liveSession) {
	for prompt := range ls.queue {
		s.runTurn(ctx, ls, prompt)
		ls.idle.Done()
	}
}

func (s *Service) runTurn(ctx context.Context, ls *liveSession, prompt string) {
	var answer string
	var tools []string
	done := ls.driver.RunTurn(ctx, agentproc.Turn{
		SessionRef: ls.sessionRef, Prompt: prompt, Workdir: ls.workdir, Effort: ls.effort,
	}, func(e agentproc.Event) {
		switch ev := e.(type) {
		case agentproc.AssistantText:
			answer += ev.Text
		case agentproc.ToolActivity:
			if len(tools) < 12 {
				tools = append(tools, ev.Label)
			}
		}
	})

	if done.SessionRef != "" {
		ls.sessionRef = done.SessionRef
	}
	if len(tools) > 0 {
		_, _ = s.reply.Post(ctx, ls.threadID, strings.Join(tools, "\n"))
	}
	if done.Err != nil {
		_, _ = s.reply.Post(ctx, ls.threadID, "❌ "+done.Err.Error())
		return
	}
	for _, chunk := range splitMessage(answerOrPlaceholder(answer), discordMax) {
		_, _ = s.reply.Post(ctx, ls.threadID, chunk)
	}
}
```

In `internal/session/service.go`, add the new fields to the `Service` struct and `New`:
```go
type Service struct {
	cfg   Config
	git   Git
	tmux  Tmux
	reply Replier

	mkdirAll  func(path string, perm uint32) error
	writeFile func(path string, data []byte, perm uint32) error

	locks keyedMutex

	drivers  map[string]agentproc.Driver // headless drivers by agent name
	hmu      sync.Mutex
	sessions map[string]*liveSession
}
```
Add `"github.com/eunomie/quack/internal/agentproc"` to `service.go` imports. **Leave `New`'s signature unchanged** (so existing callers keep compiling through the plan) and add a setter:
```go
// UseDrivers registers the headless agent drivers, keyed by agent name.
func (s *Service) UseDrivers(d map[string]agentproc.Driver) { s.drivers = d }
```
The headless test sets `svc.drivers` directly (same package); `cmd/quack/main.go` calls `UseDrivers` in Task 10. Because `New` is untouched, the module continues to build after this task.

- [ ] **Step 4: Confirm existing constructors still compile**

`New`'s signature is unchanged, so `cmd/quack/main.go` and the existing `newTestService` (which builds `&Service{...}` directly) keep compiling. `newHeadlessService` uses the 4-arg `New` then sets the unexported `drivers` field directly (same package). No caller changes needed.

- [ ] **Step 5: Run, verify pass**

Run: `go test ./internal/session/ -v`
Expected: PASS (render + headless + existing tmux tests).

- [ ] **Step 6: Commit**
  - patch-slug: `session-headless`
  - subject: `feat(session): headless registry and resume-per-turn loop`
  - files: `internal/session/headless.go internal/session/headless_test.go internal/session/service.go internal/session/service_test.go`

---

## Task 8: route headless in Service.Handle

**Files:**
- Modify: `internal/session/service.go`
- Modify: `internal/session/service_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/session/service_test.go`:
```go
func TestHandle_HeadlessStartsSession(t *testing.T) {
	svc, g, tx, r, _ := newTestService()
	d := &fakeDriver{turns: []scripted{{texts: []string{"on it"}, ref: "sess-9"}}}
	svc.drivers = map[string]agentproc.Driver{"claude": d}
	g.existing["/src/github.com/dagger/dagger"] = true

	svc.Handle(context.Background(), Request{
		Content: "dagger/dagger headless=true name=triage\nFix the bug.",
		Origin:  baseOrigin(),
	})
	svc.waitIdle(r.threadID)

	if len(tx.created) != 0 {
		t.Errorf("headless must not launch tmux: %v", tx.created)
	}
	if len(d.seen) != 1 || !strings.Contains(d.seen[0].Prompt, "Fix the bug.") {
		t.Fatalf("driver turn = %+v", d.seen)
	}
	if !anyContains(r.posts, "on it") {
		t.Errorf("agent answer not posted: %v", r.posts)
	}
}
```
(Add `"github.com/eunomie/quack/internal/agentproc"` to the test imports.)

- [ ] **Step 2: Run, verify fail**

Run: `go test ./internal/session/ -run TestHandle_Headless -v`
Expected: FAIL — headless branch not wired.

- [ ] **Step 3: Implement the branch**

In `internal/session/service.go` `Handle`, after the worktree/context.json is prepared and **before** the tmux launch, insert the headless branch. Locate:
```go
	fullPrompt := req.Origin.PromptHeader() + "\n\n" + dir.Prompt
	opts := NewSessionOpts{
```
Insert immediately above it:
```go
	fullPrompt := req.Origin.PromptHeader() + "\n\n" + dir.Prompt

	if dir.Headless {
		if _, ok := s.drivers[agentName]; !ok {
			fail("headless not supported for agent: " + agentName)
			return
		}
		_ = s.reply.Edit(ctx, threadID, ackID, successMessage(prep, dir, ag)+"\n_(headless — reply in this thread to talk to it; `/stop` to end)_")
		s.startHeadless(ctx, agentName, threadID, prep.workdir, dir.Effort, fullPrompt)
		return
	}

	opts := NewSessionOpts{
```
Remove the now-duplicated `fullPrompt :=` line that previously preceded `opts` (the insert keeps a single declaration). Confirm `agentName` is in scope (declared earlier in `Handle`).

Guard `headless=true` for an unsupported agent at parse-acceptance time is not needed — the runtime check above is sufficient and keeps `command` decoupled from driver availability.

- [ ] **Step 4: Run, verify pass**

Run: `go test ./internal/session/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**
  - patch-slug: `session-handle-headless`
  - subject: `feat(session): route headless directive to a resume-per-turn session`
  - files: `internal/session/service.go internal/session/service_test.go`

---

## Task 9: Discord thread routing + /stop + archive

**Files:**
- Modify: `internal/discord/bot.go`

This layer is thin and verified manually (no live-Discord unit tests, per the base design). Add methods to `Service` it depends on (already present: `feedThread`, `stopThread`, `tracked`).

- [ ] **Step 1: Route thread messages and `/stop` in `onMessage`**

In `internal/discord/bot.go`, replace the body of `onMessage` after the bot/self guard with thread-aware routing:
```go
func (b *Bot) onMessage(s *discordgo.Session, m *discordgo.MessageCreate) {
	if m.Author == nil || m.Author.Bot {
		return
	}

	// Headless back-channel: messages inside a tracked session thread.
	if b.svc.Tracked(m.ChannelID) {
		if !b.authorized(m) {
			return
		}
		content := strings.TrimSpace(m.Content)
		if content == "/stop" || content == "stop" {
			b.svc.StopThread(context.Background(), m.ChannelID)
			return
		}
		b.svc.FeedThread(context.Background(), m.ChannelID, content)
		return
	}

	if s.State == nil || s.State.User == nil || !mentions(m, s.State.User.ID) {
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
```
Add `"context"` to imports if not present (it is).

- [ ] **Step 2: Export the Service methods used by the bot**

The bot calls `Tracked`, `StopThread`, `FeedThread` (exported). In `internal/session/headless.go`, rename the lowercase helpers to exported wrappers, or export directly. Simplest: rename `tracked`→`Tracked`, `feedThread`→`FeedThread`, `stopThread`→`StopThread` everywhere (headless.go + headless_test.go). Keep `startHeadless`/`runLoop`/`runTurn`/`waitIdle` unexported.

- [ ] **Step 3: Handle thread archive → stop**

Add an archive handler. In `New` (bot.go) after `s.AddHandler(b.onMessage)`:
```go
	s.AddHandler(b.onThreadUpdate)
```
Add the method:
```go
func (b *Bot) onThreadUpdate(s *discordgo.Session, t *discordgo.ThreadUpdate) {
	if t.ThreadMetadata != nil && t.ThreadMetadata.Archived && b.svc.Tracked(t.ID) {
		b.svc.StopThread(context.Background(), t.ID)
	}
}
```
(If `discordgo.ThreadUpdate`/`ThreadMetadata.Archived` field names differ in v0.28.1, adjust to the SDK — verify with `go doc github.com/bwmarrin/discordgo.ThreadUpdate`.)

- [ ] **Step 4: Build**

Run: `go build ./internal/discord/ ./internal/session/`
Expected: exits 0. Then `go test ./internal/session/ -v` (rename propagation) → PASS.

- [ ] **Step 5: Commit**
  - patch-slug: `discord-headless-routing`
  - subject: `feat(discord): route thread messages, /stop, and archive to headless sessions`
  - files: `internal/discord/bot.go internal/session/headless.go internal/session/headless_test.go`

---

## Task 10: main wiring (build drivers)

**Files:**
- Modify: `cmd/quack/main.go`

- [ ] **Step 1: Build the driver map and pass it to session.New**

In `cmd/quack/main.go`, add imports `"github.com/eunomie/quack/internal/agentproc"`. After `scfg := session.Config{…}` and before `discord.New`, build drivers from configured agents:
```go
	drivers := map[string]agentproc.Driver{}
	for name, a := range cfg.Agents {
		if !a.Headless {
			continue
		}
		switch name {
		case "claude":
			drivers[name] = agentproc.Claude{
				Command: a.Command, EffortTemplate: a.EffortTemplate,
				PermissionMode: a.Mode(), AllowedTools: a.AllowedTools,
			}
		case "codex":
			drivers[name] = agentproc.Codex{Command: a.Command}
		default:
			log.Printf("agent %q has headless=true but no driver; ignoring", name)
		}
	}
```
Update the `svcFor` closure to register drivers (note `New` stays 4-arg):
```go
	}, func(r session.Replier) *session.Service {
		svc := session.New(scfg, g, tx, r)
		svc.UseDrivers(drivers)
		return svc
	})
```

- [ ] **Step 2: Build the binary**

Run: `go build ./... && go build -o quack ./cmd/quack`
Expected: exits 0.

- [ ] **Step 3: Commit**
  - patch-slug: `cmd-headless-wiring`
  - subject: `feat(cmd): construct headless drivers and inject into the service`
  - files: `cmd/quack/main.go`

---

## Task 11: opt-in integration test (real claude turn + resume)

**Files:**
- Create: `internal/agentproc/claude_integration_test.go`

- [ ] **Step 1: Write the gated integration test**

`internal/agentproc/claude_integration_test.go`:
```go
package agentproc

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestClaudeRunTurn_Integration(t *testing.T) {
	if os.Getenv("QUACK_INTEGRATION") == "" {
		t.Skip("set QUACK_INTEGRATION=1 to run (needs an authenticated claude CLI)")
	}
	dir := t.TempDir()
	run(t, dir, "git", "init", "-q", "-b", "main", ".")
	run(t, dir, "git", "commit", "-q", "--allow-empty", "-m", "init")

	d := Claude{Command: "claude", PermissionMode: "acceptEdits", AllowedTools: "Read"}
	var ans1 strings.Builder
	done1 := d.RunTurn(context.Background(), Turn{
		Prompt:  "Reply with exactly the word PONG and nothing else.",
		Workdir: dir,
	}, func(e Event) {
		if a, ok := e.(AssistantText); ok {
			ans1.WriteString(a.Text)
		}
	})
	if done1.Err != nil || done1.SessionRef == "" {
		t.Fatalf("turn1: err=%v ref=%q", done1.Err, done1.SessionRef)
	}
	if !strings.Contains(strings.ToUpper(ans1.String()), "PONG") {
		t.Fatalf("turn1 answer = %q", ans1.String())
	}

	// resume must continue the SAME session
	done2 := d.RunTurn(context.Background(), Turn{
		SessionRef: done1.SessionRef,
		Prompt:     "What single word did you just say? Reply with only that word.",
		Workdir:    dir,
	}, func(e Event) {})
	if done2.Err != nil {
		t.Fatalf("turn2 err: %v", done2.Err)
	}
	if done2.SessionRef != done1.SessionRef {
		t.Fatalf("session ref changed on resume: %q -> %q", done1.SessionRef, done2.SessionRef)
	}
	_ = filepath.Separator
}

func run(t *testing.T, dir string, name string, args ...string) {
	t.Helper()
	c := exec.Command(name, args...)
	c.Dir = dir
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
}
```

- [ ] **Step 2: Build + run (if claude available)**

Run: `go build ./internal/agentproc/`
Run: `QUACK_INTEGRATION=1 go test ./internal/agentproc/ -run Integration -v`
Expected: PASS (or document the auth env needed if it fails on credentials — feed back into Task 4 Step 1 findings).

- [ ] **Step 3: Commit**
  - patch-slug: `agentproc-claude-integration`
  - subject: `test(agentproc): opt-in real claude turn + resume`
  - files: `internal/agentproc/claude_integration_test.go`

---

## Task 12: ops docs (config + README)

**Files:**
- Modify: `config.example.toml`
- Modify: `README.md`

- [ ] **Step 1: Document headless agent settings in `config.example.toml`**

Update the agent sections:
```toml
[agents.claude]
command         = "claude"
effort_template = "--effort {effort}"
headless        = true              # enable `headless=true` Discord conversations
permission_mode = "acceptEdits"     # or "bypassPermissions" (fully sandboxed hosts)
allowed_tools   = "Bash(git *),Read,Edit,Write"

[agents.codex]
command         = "codex"
effort_template = "--config model_reasoning_effort={effort}"
headless        = true
```

- [ ] **Step 2: Document usage in `README.md`**

Add after the Usage example block:
```markdown
### Headless (conversational) sessions

Add `headless=true` to talk to the agent from the thread:

    @quack dagger/dagger headless=true name=triage
    Investigate the flaky cache test; reproduce it first.

quack runs the agent non-interactively and posts each answer back in the
thread. Reply in the thread to send another turn; post `/stop` (or archive the
thread) to end it. Without the flag you get the default interactive tmux session
(`tmux attach -t quack/<name>`), with no Discord back-channel.
```

- [ ] **Step 3: Build/verify docs don't break anything**

Run: `go build ./...`
Expected: exits 0.

- [ ] **Step 4: Commit**
  - patch-slug: `headless-ops-docs`
  - subject: `docs: document headless mode (config + README)`
  - files: `config.example.toml README.md`

---

## Task 13: Final verification

- [ ] **Step 1: Vet + full unit run**

Run:
```bash
go vet ./...
go test ./...
```
Expected: vet clean; all unit tests PASS (integration skipped without `QUACK_INTEGRATION`).

- [ ] **Step 2: Integration (if claude/codex present + authenticated)**

Run: `QUACK_INTEGRATION=1 go test ./...`
Expected: PASS, including the claude resume test.

- [ ] **Step 3: Build the binary + smoke the flag**

Run: `go build -o quack ./cmd/quack && ./quack --help`
Expected: prints `-config` usage. (Live behavior is exercised manually from Discord with `headless=true`.)

- [ ] **Step 4: Review the series**

Run: `stg series`
Expected: a clean ordered series of all task patches, each signed off, no `Co-Authored-By`.

---

## Self-review notes (author)

**Spec coverage** — each spec section maps to a task:
- `headless=true` flag → Task 1; headless config fields → Task 2.
- Unified resume-per-turn driver contract → Task 3; claude driver → Task 4; codex driver → Task 5.
- Output (per-turn answer + progress, 2000-char split) → Tasks 6, 7.
- In-memory registry + turn loop + serialized turns → Task 7.
- Route headless in Handle (default tmux untouched) → Task 8.
- Input routing + `/stop` + archive → Task 9.
- Driver construction/wiring → Task 10.
- Security (token never to agent) → inherent: quack spawns and posts; no token in driver env.
- Testing (pure parse fixtures, faked orchestration, opt-in integration) → Tasks 4–7, 11.
- Verify-at-impl items (claude auth, tool JSON, codex resume) → Task 4 Step 1, Task 5 Step 1, Task 9 Step 3.

**Type consistency** — `agentproc.{Turn,Event,AssistantText,ToolActivity,TurnDone,Driver}` defined in Task 3 are used identically in Tasks 4–7, 10, 11. `New` keeps its 4-arg signature; `Service.UseDrivers` (Task 7) registers the drivers built in Task 10. `Service.{Tracked,FeedThread,StopThread}` (exported via the Task 9 rename) are the exact names the bot calls.

**Known confirm-at-impl** — exact claude headless auth/env, the precise tool-activity JSON fields, and codex's `resume` invocation are pinned by recorded fixtures (Tasks 4–5) and the integration test (Task 11), not assumed.
