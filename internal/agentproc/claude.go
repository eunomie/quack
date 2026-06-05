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

// Claude drives the claude CLI in headless resume-per-turn mode.
type Claude struct {
	Command        string
	Model          string // --model flag value; empty means use the CLI default. Codex has no equivalent field.
	EffortTemplate string
	NameTemplate   string
	PermissionMode string
	AllowedTools   string
	// DisallowedTools is passed verbatim to `claude --disallowedTools`
	// (e.g. "Skill(open-zed),Skill(other)"). Empty means no restriction.
	// Exact Skill(<name>) matcher form confirmed in host-verification spike P3.
	DisallowedTools string
	Settings        string // passed verbatim to `claude --settings` (JSON or file path)

	// AskMCPURL is the base URL of quack's ask_user MCP server (empty disables the
	// feature). When set on a streaming session, the per-session OpenOpts.AskToken
	// is appended as ?s=<token> so the server can tell which session is asking; the
	// native AskUserQuestion is disallowed and a system-prompt nudge steers the
	// model to the MCP tool, so a headless question blocks on the owner's real answer.
	AskMCPURL string
}

// askNudge tells the model to use the owner-answered MCP tool instead of guessing
// when it needs a decision only the owner can make (the native AskUserQuestion has
// no UI in headless mode).
const askNudge = "You are running headless in a Discord thread. When you need a decision only the " +
	"owner can make — a design choice, a yes/no, picking between options — call the " +
	"mcp__quack__ask_user tool and wait for their answer instead of guessing or proceeding " +
	"on your own. It posts the question to the thread and blocks until they reply."

func (d Claude) args(t Turn) []string {
	mode := d.PermissionMode
	if mode == "" {
		mode = "acceptEdits"
	}
	a := []string{"-p", t.Prompt, "--output-format", "stream-json", "--verbose", "--permission-mode", mode}
	if d.AllowedTools != "" {
		a = append(a, "--allowedTools", d.AllowedTools)
	}
	if d.DisallowedTools != "" {
		a = append(a, "--disallowedTools", d.DisallowedTools)
	}
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
	// First turn: set the display name and effort (a resumed session keeps both).
	if t.Name != "" && d.NameTemplate != "" {
		a = append(a, strings.Fields(strings.ReplaceAll(d.NameTemplate, "{name}", t.Name))...)
	}
	if t.Effort != "" && d.EffortTemplate != "" {
		a = append(a, strings.Fields(strings.ReplaceAll(d.EffortTemplate, "{effort}", t.Effort))...)
	}
	return a
}

func (d Claude) RunTurn(ctx context.Context, t Turn, emit func(Event)) TurnDone {
	command := d.Command
	if command == "" {
		command = "claude"
	}
	l := t.Launcher
	if l == nil {
		l = DirectLauncher{}
	}
	cmd := l.Command(ctx, command, d.args(t), t.Workdir, nil)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return TurnDone{Err: err}
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return TurnDone{Err: err}
	}
	done := parseClaudeStream(stdout, emit)
	werr := cmd.Wait()
	if done.Err == nil && werr != nil {
		done.Err = fmt.Errorf("claude exited: %w", werr)
	}
	return done
}

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

// SuggestName runs a quick read-only one-shot to name the task.
func (d Claude) SuggestName(ctx context.Context, prompt string) (string, error) {
	return d.OneShot(ctx, nameGenPrompt+prompt, "low")
}

type claudeLine struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	SessionID string `json:"session_id"`
	Message   struct {
		Content []struct {
			Type  string `json:"type"`
			Text  string `json:"text"`
			Name  string `json:"name"`
			Input struct {
				Command string `json:"command"`
			} `json:"input"`
		} `json:"content"`
	} `json:"message"`
	Result       string  `json:"result"`
	IsError      bool    `json:"is_error"`
	TotalCostUSD float64 `json:"total_cost_usd"`
}

func parseClaudeStream(r io.Reader, emit func(Event)) TurnDone {
	var done TurnDone
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1024*1024), 8*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var m claudeLine
		if json.Unmarshal([]byte(line), &m) != nil {
			continue
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
					label := c.Name
					if c.Input.Command != "" {
						label += " " + c.Input.Command
					}
					emit(ToolActivity{Label: label})
				}
			}
		case "result":
			done.CostUSD = m.TotalCostUSD
			if m.IsError {
				done.Err = fmt.Errorf("%s", strings.TrimSpace(m.Result))
			}
		}
	}
	if err := sc.Err(); err != nil && done.Err == nil {
		done.Err = err
	}
	return done
}
