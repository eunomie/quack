package agentproc

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
)

// streamArgs builds the argv for a persistent streaming session: one claude
// process that reads user messages from stdin as JSON and streams JSON events
// back, staying alive across turns so the owner can interject mid-turn.
func (d Claude) streamArgs(o OpenOpts) []string {
	mode := d.PermissionMode
	if mode == "" {
		mode = "acceptEdits"
	}
	a := []string{
		"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--permission-mode", mode,
	}
	if d.AllowedTools != "" {
		a = append(a, "--allowedTools", d.AllowedTools)
	}
	if d.Settings != "" {
		a = append(a, "--settings", d.Settings)
	}
	if d.Model != "" {
		a = append(a, "--model", d.Model)
	}
	// Owner-answered questions: expose quack's ask_user MCP tool, disallow the
	// UI-less native AskUserQuestion, and nudge the model toward the MCP tool.
	if d.AskMCPURL != "" && o.AskToken != "" {
		a = append(a,
			"--mcp-config", askMCPConfig(d.AskMCPURL, o.AskToken),
			"--disallowedTools", "AskUserQuestion",
			"--append-system-prompt", askNudge,
		)
	}
	if o.SessionRef != "" {
		return append(a, "--resume", o.SessionRef)
	}
	// Fresh session: set the display name and effort (a resumed session keeps both).
	if o.Name != "" && d.NameTemplate != "" {
		a = append(a, strings.Fields(strings.ReplaceAll(d.NameTemplate, "{name}", o.Name))...)
	}
	if o.Effort != "" && d.EffortTemplate != "" {
		a = append(a, strings.Fields(strings.ReplaceAll(d.EffortTemplate, "{effort}", o.Effort))...)
	}
	return a
}

// askMCPConfig builds the --mcp-config JSON registering quack's ask_user server
// for this session (the token routes a tool call back to its thread). The call
// returns immediately (the answer comes later as a new turn), so no per-server
// tool-call timeout is needed.
func askMCPConfig(baseURL, token string) string {
	cfg := map[string]any{
		"mcpServers": map[string]any{
			"quack": map[string]any{
				"type": "http",
				"url":  baseURL + "?s=" + token,
			},
		},
	}
	data, _ := json.Marshal(cfg)
	return string(data)
}

// OpenSession starts a persistent streaming claude process. It implements
// StreamDriver, so the orchestrator drives claude as one long-lived session
// (Send/Interrupt) instead of one process per turn.
func (d Claude) OpenSession(ctx context.Context, o OpenOpts) (Session, error) {
	command := d.Command
	if command == "" {
		command = "claude"
	}
	ctx, cancel := context.WithCancel(ctx)
	l := o.Launcher
	if l == nil {
		l = DirectLauncher{}
	}
	cmd := l.Command(ctx, command, d.streamArgs(o), o.Workdir, nil)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, err
	}
	s := &claudeSession{
		stdin:  stdin,
		cancel: cancel,
		events: make(chan Event, 32),
		wait:   cmd.Wait,
	}
	go s.read(stdout)
	return s, nil
}

// claudeSession is a live streaming claude process. The read goroutine owns the
// event stream and the latest session ref; Send/Interrupt write framed JSON to
// stdin (guarded so concurrent writes can't interleave).
type claudeSession struct {
	stdin  io.WriteCloser
	cancel context.CancelFunc
	events chan Event
	wait   func() error

	wmu    sync.Mutex // serializes stdin writes
	reqSeq int

	mu  sync.Mutex
	ref string
}

func (s *claudeSession) Events() <-chan Event { return s.events }

func (s *claudeSession) SessionRef() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ref
}

// userFrame / controlFrame are the stream-json input frames claude accepts on
// stdin: a user message (the next turn) and a control request (interrupt).
type userFrame struct {
	Type    string      `json:"type"`
	Message userMessage `json:"message"`
}

type userMessage struct {
	Role    string         `json:"role"`
	Content []userTextPart `json:"content"`
}

type userTextPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type controlFrame struct {
	Type      string         `json:"type"`
	RequestID string         `json:"request_id"`
	Request   controlRequest `json:"request"`
}

type controlRequest struct {
	Subtype string `json:"subtype"`
}

func (s *claudeSession) Send(text string) error {
	return s.write(userFrame{
		Type:    "user",
		Message: userMessage{Role: "user", Content: []userTextPart{{Type: "text", Text: text}}},
	})
}

func (s *claudeSession) Interrupt() error {
	s.wmu.Lock()
	s.reqSeq++
	id := fmt.Sprintf("int-%d", s.reqSeq)
	s.wmu.Unlock()
	return s.write(controlFrame{
		Type:      "control_request",
		RequestID: id,
		Request:   controlRequest{Subtype: "interrupt"},
	})
}

func (s *claudeSession) write(frame any) error {
	data, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if _, err := s.stdin.Write(append(data, '\n')); err != nil {
		return err
	}
	return nil
}

func (s *claudeSession) Close() error {
	_ = s.stdin.Close()
	s.cancel()
	if s.wait != nil {
		_ = s.wait()
	}
	return nil
}

// read parses the stream-json output, emitting the same Events as the per-turn
// parser plus a TurnComplete at each result boundary. It closes the events
// channel when the process's stdout ends, signalling the session is dead.
func (s *claudeSession) read(r io.Reader) {
	defer close(s.events)
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
			s.mu.Lock()
			s.ref = m.SessionID
			s.mu.Unlock()
		}
		switch m.Type {
		case "assistant":
			for _, c := range m.Message.Content {
				switch c.Type {
				case "text":
					if strings.TrimSpace(c.Text) != "" {
						s.emit(AssistantText{Text: c.Text})
					}
				case "tool_use":
					label := c.Name
					if c.Input.Command != "" {
						label += " " + c.Input.Command
					}
					s.emit(ToolActivity{Label: label})
				}
			}
		case "result":
			// error_during_execution is how an interrupt surfaces: the turn was
			// cut off so the agent can read the next message, not a real failure.
			if m.Subtype == "error_during_execution" {
				s.emit(TurnComplete{Interrupted: true})
				continue
			}
			var err error
			if m.IsError {
				err = fmt.Errorf("%s", strings.TrimSpace(m.Result))
			}
			s.emit(TurnComplete{Err: err})
		}
	}
}

func (s *claudeSession) emit(e Event) { s.events <- e }
