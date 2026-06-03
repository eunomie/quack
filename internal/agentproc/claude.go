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
	EffortTemplate string
	NameTemplate   string
	PermissionMode string
	AllowedTools   string
	Settings       string // passed verbatim to `claude --settings` (JSON or file path)
}

func (d Claude) args(t Turn) []string {
	mode := d.PermissionMode
	if mode == "" {
		mode = "acceptEdits"
	}
	a := []string{"-p", t.Prompt, "--output-format", "stream-json", "--verbose", "--permission-mode", mode}
	if d.AllowedTools != "" {
		a = append(a, "--allowedTools", d.AllowedTools)
	}
	if d.Settings != "" {
		a = append(a, "--settings", d.Settings)
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
	cmd := exec.CommandContext(ctx, command, d.args(t)...)
	cmd.Dir = t.Workdir
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

type claudeLine struct {
	Type      string `json:"type"`
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
