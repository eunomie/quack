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
	Name       string // session display name; applied on the first turn only
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
	SessionRef string
	CostUSD    float64
	Err        error
}

// Driver runs one turn as a headless child, emitting in-flight Events via emit
// and returning the terminal TurnDone.
type Driver interface {
	RunTurn(ctx context.Context, t Turn, emit func(Event)) TurnDone
	// OneShot runs a single read-only turn (no edits) and returns the agent's
	// final text. Used for quick structured queries like naming and the fluent
	// directive inference. effort is applied if the agent supports it.
	OneShot(ctx context.Context, prompt, effort string) (string, error)
	// SuggestName asks the agent (one-shot, no tools) for a short branch name
	// describing the task. The caller slugifies and falls back on error.
	SuggestName(ctx context.Context, prompt string) (string, error)
}

const nameGenPrompt = "Reply with ONLY a short lowercase kebab-case git branch name (2-4 words; no spaces, slashes, or quotes) describing this task — nothing else.\n\nTask: "
