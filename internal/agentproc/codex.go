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

// Codex drives the codex CLI in headless resume-per-turn mode.
type Codex struct {
	Command        string
	EffortTemplate string
}

func (d Codex) args(t Turn) []string {
	args := []string{"exec"}
	if t.SessionRef != "" {
		args = append(args, "resume", "--json", t.SessionRef, t.Prompt)
		return args
	} else if t.Effort != "" && d.EffortTemplate != "" {
		args = append(args, strings.Fields(strings.ReplaceAll(d.EffortTemplate, "{effort}", t.Effort))...)
	}
	return append(args, "--json", t.Prompt)
}

func (d Codex) RunTurn(ctx context.Context, t Turn, emit func(Event)) TurnDone {
	command := d.Command
	if command == "" {
		command = "codex"
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
	done := parseCodexStream(stdout, emit)
	werr := cmd.Wait()
	if done.Err == nil && werr != nil {
		done.Err = fmt.Errorf("codex exited: %w", werr)
	}
	return done
}

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
	werr := cmd.Wait()
	if b.Len() == 0 && werr != nil {
		return "", fmt.Errorf("codex oneshot: %w", werr)
	}
	return b.String(), nil
}

// SuggestName runs a quick one-shot to name the task.
func (d Codex) SuggestName(ctx context.Context, prompt string) (string, error) {
	return d.OneShot(ctx, nameGenPrompt+prompt, "low")
}

type codexLine struct {
	Type     string `json:"type"`
	ThreadID string `json:"thread_id"`
	Item     struct {
		Type    string `json:"type"`
		Text    string `json:"text"`
		Command string `json:"command"`
	} `json:"item"`
	Message string `json:"message"`
	Error   string `json:"error"`
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
				if strings.TrimSpace(m.Item.Command) != "" {
					emit(ToolActivity{Label: m.Item.Command})
				}
			case "file_change", "mcp_tool_call", "web_search":
				emit(ToolActivity{Label: m.Item.Type})
			}
		case "turn.failed", "error":
			msg := strings.TrimSpace(m.Error)
			if msg == "" {
				msg = strings.TrimSpace(m.Message)
			}
			if msg == "" {
				msg = "codex turn failed"
			}
			done.Err = fmt.Errorf("%s", msg)
		}
	}
	if err := sc.Err(); err != nil && done.Err == nil {
		done.Err = err
	}
	return done
}
