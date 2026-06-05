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

// TurnComplete marks the end of one response in a streaming session: the agent
// finished answering the most recent input. Unlike a one-shot RunTurn (whose
// terminal state is TurnDone), a streaming process stays alive across turns, so
// each answer's boundary is signalled in-band. Interrupted is set when the turn
// ended because the owner interjected (the agent was cut off to read a new
// message), as opposed to finishing on its own; Err carries a genuine turn error.
type TurnComplete struct {
	Interrupted bool
	Err         error
}

func (AssistantText) isEvent() {}
func (ToolActivity) isEvent()  {}
func (TurnComplete) isEvent()  {}

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

// OpenOpts configures a streaming session at process start. SessionRef resumes a
// prior conversation ("" starts fresh); Effort and Name are applied only on a
// fresh start (a resumed session keeps both), mirroring Turn.
type OpenOpts struct {
	SessionRef string
	Workdir    string
	Effort     string
	Name       string
}

// Session is a live, resumable agent process that accepts interleaved input. One
// process serves a whole conversation: each Send is a turn, and the owner can
// Interrupt the in-flight turn to steer it. Events streams the same in-flight
// Events as a Driver, with a TurnComplete after each answer. A StreamDriver
// enables mid-turn interjection that one-process-per-turn cannot.
type Session interface {
	// Send delivers a user message as the next turn. It does not block on the
	// answer; the answer streams out of Events and ends with a TurnComplete.
	Send(text string) error
	// Interrupt cuts off the in-flight turn so the agent reads the next Send
	// instead of finishing its current work. The interrupted turn ends with a
	// TurnComplete{Interrupted: true}.
	Interrupt() error
	// Events is the in-flight event stream for the whole session. It is closed
	// when the process exits (cleanly or not), signalling the session is dead.
	Events() <-chan Event
	// SessionRef returns the latest resume token seen on the stream, for
	// persistence; "" until the process reports one.
	SessionRef() string
	// Close ends the process: closes stdin and kills the child if needed.
	Close() error
}

// StreamDriver opens a persistent streaming Session. A Driver may also implement
// StreamDriver (claude does); one that doesn't (codex) keeps the per-turn model.
type StreamDriver interface {
	OpenSession(ctx context.Context, o OpenOpts) (Session, error)
}

const nameGenPrompt = "Reply with ONLY a short lowercase kebab-case git branch name (2-4 words; no spaces, slashes, or quotes) describing this task — nothing else.\n\nTask: "
