package session

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/eunomie/quack/internal/agent"
	"github.com/eunomie/quack/internal/command"
	"github.com/eunomie/quack/internal/worktree"
)

// inferred is the JSON the infer one-shot returns. Worktree/Headless are
// pointers so an omitted field is distinguishable from an explicit false.
type inferred struct {
	Target   string `json:"target"`
	Base     string `json:"base"`
	Worktree *bool  `json:"worktree"`
	Agent    string `json:"agent"`
	Effort   string `json:"effort"`
	Name     string `json:"name"`
	Headless *bool  `json:"headless"`
	Context  string `json:"context"`
}

// inferEfforts is the accepted effort vocabulary; anything else is dropped so a
// hallucinated value can't reach the agent.
var inferEfforts = map[string]bool{"low": true, "medium": true, "high": true, "xhigh": true}

// extractJSON narrows raw model output to the outermost JSON object, stripping a
// leading ```json / ``` fence and any surrounding prose. Returns "" if none.
func extractJSON(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[i+1:]
		}
		// Strip from the closing fence onward, not just as a suffix, so any prose
		// the model adds after the fence can't pull the brace scan into garbage.
		if j := strings.Index(s, "\n```"); j >= 0 {
			s = s[:j]
		} else {
			s = strings.TrimSuffix(strings.TrimSpace(s), "```")
		}
		s = strings.TrimSpace(s)
	}
	start := strings.IndexByte(s, '{')
	end := strings.LastIndexByte(s, '}')
	if start < 0 || end < start {
		return ""
	}
	return s[start : end+1]
}

// parseInferred extracts and unmarshals the infer one-shot output.
func parseInferred(out string) (inferred, error) {
	js := extractJSON(out)
	if js == "" {
		return inferred{}, fmt.Errorf("no JSON object in infer output")
	}
	var inf inferred
	if err := json.Unmarshal([]byte(js), &inf); err != nil {
		return inferred{}, fmt.Errorf("parse infer JSON: %w", err)
	}
	return inf, nil
}

// guidanceBlock wraps standing infer hints in a fixed framing, or "" when s is
// empty or all-whitespace. The framing constrains the hints to target/name
// resolution so a prior can't make the agent invent a target for a request that
// names no repo. The trailing blank line separates it from the conversation
// section that follows in the prompt template.
func guidanceBlock(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	return "Environment hints (use ONLY to resolve the target repo/path and the name; never invent a target when the request names no repo/dir):\n" + s + "\n\n"
}

// contextBlock wraps the infer agent's resolved context, or "" when s is empty or all-whitespace.
func contextBlock(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	return "<quack-resolved-context>\n" + s + "\n</quack-resolved-context>"
}

const inferPromptTemplate = `You route a natural-language request into a single JSON object describing how to launch a coding-agent session. Reply with ONLY the JSON object — no prose, no markdown fences.

Schema (all fields required):
{"target":"","base":"","worktree":true,"agent":"","effort":"","name":"","headless":true,"context":""}

- target: a GitHub repo "owner/repo", an absolute or ~ path, the literal "temp-dir", or "" when the request names no repo/dir.
- base: base branch to start from, or "" for the repo default.
- worktree: true to work in an isolated worktree (default); false only if the user explicitly wants to work directly in the checkout.
- agent: "claude", "codex", or "" for the default.
- effort: one of "low", "medium", "high", "xhigh", or "" for the default; pick higher for harder tasks.
- name: a short lowercase kebab-case branch name (2-4 words) describing the task.
- headless: true (default) for a Discord conversation; false only if the user explicitly asks for an interactive or tmux session.
- context: if the request refers to something discussed earlier (e.g. "this feature", "that bug"), resolve it into one short paragraph using the conversation below; otherwise "".

%sRecent Discord conversation (oldest first), for resolving references and naming:
<conversation>
%s
</conversation>

Request:
%s`

// inferAgentName picks the infer one-shot agent (belt-and-suspenders: config.Load already pre-fills InferAgent from NameAgent).
func (s *Service) inferAgentName() string {
	return orDefault(s.cfg.InferAgent, orDefault(s.cfg.NameAgent, s.cfg.DefaultAgent))
}

func (s *Service) inferEffort() string { return orDefault(s.cfg.InferEffort, "medium") }

func (s *Service) inferHistoryLimit() int {
	if s.cfg.InferHistoryLimit > 0 {
		return s.cfg.InferHistoryLimit
	}
	return 20
}

// recentHistory fetches and renders recent channel messages as "author: text"
// lines (oldest-first), each capped, for the infer prompt. Returns "" on any
// failure or when no history reader is wired.
func (s *Service) recentHistory(ctx context.Context, o Origin) string {
	if s.history == nil {
		return ""
	}
	msgs, err := s.history.RecentMessages(ctx, o.ChannelID, o.MessageID, s.inferHistoryLimit())
	if err != nil || len(msgs) == 0 {
		return ""
	}
	var b strings.Builder
	for _, m := range msgs {
		text := strings.TrimSpace(m.Content)
		if text == "" {
			continue
		}
		if r := []rune(text); len(r) > 400 {
			text = string(r[:400]) + "…"
		}
		b.WriteString(m.Author)
		b.WriteString(": ")
		b.WriteString(text)
		b.WriteByte('\n')
	}
	return strings.TrimSpace(b.String())
}

// inferDirective runs the read-only infer one-shot and maps its JSON into a
// Directive. Returns (nil, false) on any failure so the caller can fall back.
func (s *Service) inferDirective(ctx context.Context, raw, history string) (*command.Directive, bool) {
	name := s.inferAgentName()
	d, ok := s.drivers[name]
	if !ok {
		if d, ok = s.drivers[s.cfg.DefaultAgent]; !ok {
			return nil, false
		}
	}
	// 60s ceiling — the infer prompt carries the full request plus recent history, so allow more headroom than the quick naming one-shot.
	ictx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	convo := history
	if convo == "" {
		convo = "(no recent messages)"
	}
	out, err := d.OneShot(ictx, fmt.Sprintf(inferPromptTemplate, guidanceBlock(s.cfg.InferGuidance), convo, raw), s.inferEffort())
	if err != nil {
		return nil, false
	}
	inf, err := parseInferred(out)
	if err != nil {
		return nil, false
	}
	return mapInferred(inf, s.cfg.Agents, raw), true
}

// directivePrefix reports whether content opts out of the default fluent path
// into the explicit directive grammar (a leading "!") and returns the spec that
// follows, with the marker and one optional space removed. The remaining first
// line is the directive line for command.Parse; any leading newline is kept so an
// empty directive line still separates from the prompt. Isolated here so the
// trigger is cheap to change.
func directivePrefix(content string) (string, bool) {
	rest, ok := strings.CutPrefix(content, "!")
	if !ok {
		return "", false
	}
	return strings.TrimPrefix(rest, " "), true
}

// interpretationNote renders the muted echo of how a fluent request was
// interpreted, posted into the thread so the user can see/correct it.
func interpretationNote(dir *command.Directive) string {
	mode := "headless"
	if !dir.Headless {
		mode = "interactive"
	}
	workspace := "worktree `" + orDefault(dir.Name, "(auto)") + "`"
	if dir.NoWorktree {
		workspace = "no-wt"
	}
	return fmt.Sprintf("-# 🦆 interpreted as: `%s` · agent `%s` · effort `%s` · %s · %s",
		orDefault(dir.Target, "scratch"),
		orDefault(dir.Agent, "default"),
		orDefault(dir.Effort, "default"),
		workspace,
		mode)
}

// handleFluent runs the fluent path: react immediately, fetch recent Discord
// context, infer the directive, echo the interpretation, then launch via run.
// On any infer failure it falls back to a scratch-dir run of the raw request.
func (s *Service) handleFluent(ctx context.Context, req Request, raw string) {
	if raw == "" {
		_ = s.reply.React(ctx, req.Origin.ChannelID, req.Origin.MessageID, emojiError)
		_, _ = s.reply.Post(ctx, req.Origin.ChannelID, "🦆 nothing to do — mention me with a request")
		return
	}
	// Instant feedback while the infer one-shot runs (it's on the critical path
	// before the thread opens).
	_ = s.reply.React(ctx, req.Origin.ChannelID, req.Origin.MessageID, emojiWorking)

	history := s.recentHistory(ctx, req.Origin)
	dir, ok := s.inferDirective(ctx, raw, history)
	if !ok {
		fallback := &command.Directive{Headless: true, Prompt: raw}
		s.run(ctx, req, fallback, false, "", "-# 🦆 couldn't interpret that — running it in the scratch dir")
		return
	}
	// The inferred name is a suggestion (auto-suffix on collision), so pass it as
	// `suggested` with explicit=false; setting it on the directive too gives a nice
	// provisional thread name before the labelled rename.
	s.run(ctx, req, dir, false, dir.Name, interpretationNote(dir))
}

// mapInferred validates the inferred fields and builds the same Directive the
// plain grammar produces. Unknown agent/effort are dropped (config defaults
// apply); the name is slugified; worktree/headless default on; any resolved
// context is prepended to the raw prompt.
func mapInferred(inf inferred, agents map[string]agent.Agent, raw string) *command.Directive {
	agentName := strings.TrimSpace(inf.Agent)
	if _, ok := agents[agentName]; !ok {
		agentName = ""
	}
	effort := strings.TrimSpace(inf.Effort)
	if !inferEfforts[effort] {
		effort = ""
	}
	name := worktree.Slugify(inf.Name)
	if name == "session" { // Slugify's empty sentinel
		name = ""
	}
	headless := true
	if inf.Headless != nil {
		headless = *inf.Headless
	}
	noWorktree := false
	if inf.Worktree != nil {
		noWorktree = !*inf.Worktree
	}
	prompt := raw
	if block := contextBlock(inf.Context); block != "" {
		prompt = block + "\n\n" + raw
	}
	return &command.Directive{
		Target:     strings.TrimSpace(inf.Target),
		Base:       strings.TrimSpace(inf.Base),
		Agent:      agentName,
		Effort:     effort,
		Name:       name,
		Headless:   headless,
		NoWorktree: noWorktree,
		Prompt:     prompt,
	}
}
