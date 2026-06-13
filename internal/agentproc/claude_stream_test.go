package agentproc

import (
	"bytes"
	"strings"
	"sync"
	"testing"
)

func TestClaudeStreamArgs(t *testing.T) {
	d := Claude{
		Command:        "claude",
		Model:          "claude-x",
		EffortTemplate: "--effort {effort}",
		NameTemplate:   "--name {name}",
		PermissionMode: "plan",
		AllowedTools:   "Read,Bash",
		Settings:       "/tmp/s.json",
	}

	fresh := strings.Join(d.streamArgs(OpenOpts{Effort: "high", Name: "triage"}), " ")
	for _, want := range []string{
		"--input-format stream-json", "--output-format stream-json",
		"--permission-mode plan", "--allowedTools Read,Bash",
		"--settings /tmp/s.json", "--model claude-x",
		"--effort high", "--name triage",
		"--append-system-prompt",
	} {
		if !strings.Contains(fresh, want) {
			t.Errorf("fresh args missing %q in: %s", want, fresh)
		}
	}
	if !strings.Contains(fresh, discordFormatNudge) {
		t.Errorf("fresh args missing the Discord format nudge: %s", fresh)
	}
	if strings.Contains(fresh, "--resume") {
		t.Errorf("fresh session must not resume: %s", fresh)
	}

	// A resumed session resumes by ref and drops the first-turn name/effort, but
	// still carries the standing Discord format nudge so it reaches sessions that
	// predate it.
	res := strings.Join(d.streamArgs(OpenOpts{SessionRef: "sess-42"}), " ")
	if !strings.Contains(res, "--resume sess-42") {
		t.Errorf("resume args missing --resume: %s", res)
	}
	if !strings.Contains(res, discordFormatNudge) {
		t.Errorf("resume args missing the Discord format nudge: %s", res)
	}
	if strings.Contains(res, "--name") || strings.Contains(res, "--effort") {
		t.Errorf("resumed session must not re-apply name/effort: %s", res)
	}
}

func TestClaudeStreamReadEvents(t *testing.T) {
	lines := strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"sess-1"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Working on it."}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{"command":"foo.go"}}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Done."}]}}`,
		`{"type":"result","subtype":"success","session_id":"sess-1"}`,
		// A second turn that is interrupted by the owner.
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Starting…"}]}}`,
		`{"type":"result","subtype":"error_during_execution","session_id":"sess-1"}`,
	}, "\n")

	s := &claudeSession{events: make(chan Event, 64)}
	s.read(strings.NewReader(lines))

	var got []Event
	for e := range s.events { // read() closed the channel at EOF
		got = append(got, e)
	}

	want := []Event{
		AssistantText{Text: "Working on it."},
		ToolActivity{Label: "Read foo.go"},
		AssistantText{Text: "Done."},
		TurnComplete{},
		AssistantText{Text: "Starting…"},
		TurnComplete{Interrupted: true},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d events, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("event %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if s.SessionRef() != "sess-1" {
		t.Errorf("session ref = %q, want sess-1", s.SessionRef())
	}
}

// writeCloser adapts a bytes.Buffer to io.WriteCloser for capturing stdin frames.
type writeCloser struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *writeCloser) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}
func (w *writeCloser) Close() error { return nil }
func (w *writeCloser) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

func TestClaudeStreamFrames(t *testing.T) {
	w := &writeCloser{}
	s := &claudeSession{stdin: w}

	if err := s.Send("hello there"); err != nil {
		t.Fatalf("send: %v", err)
	}
	if err := s.Interrupt(); err != nil {
		t.Fatalf("interrupt: %v", err)
	}

	out := w.String()
	frames := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(frames) != 2 {
		t.Fatalf("want 2 newline-delimited frames, got %d: %q", len(frames), out)
	}
	if !strings.Contains(frames[0], `"type":"user"`) ||
		!strings.Contains(frames[0], `"text":"hello there"`) ||
		!strings.Contains(frames[0], `"role":"user"`) {
		t.Errorf("user frame malformed: %s", frames[0])
	}
	if !strings.Contains(frames[1], `"type":"control_request"`) ||
		!strings.Contains(frames[1], `"subtype":"interrupt"`) {
		t.Errorf("interrupt frame malformed: %s", frames[1])
	}
}
