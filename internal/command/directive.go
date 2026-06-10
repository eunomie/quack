// Package command parses quack's freeform mention grammar into a Directive.
package command

import (
	"fmt"
	"strconv"
	"strings"
)

// Directive is a parsed quack command.
type Directive struct {
	Target     string // repo ref or path; "" => run in a fresh temp dir
	Agent      string // optional ("" => config default)
	Effort     string // optional
	Name       string // optional session name
	Base       string // optional base branch
	Headless   bool   // defaults true; bare `no-headless` (or headless=false) turns it off
	NoWorktree bool   // bare `no-wt`: run directly in the repo, no worktree (dangerous)
	Sandbox    bool   // bare `sandbox`: force this session into the guest Docker sandbox, even for an owner (the `sandbox` keyword)
	Prompt     string // required, verbatim, may be multiline
}

// UsageError is returned for malformed input; its message is safe to show the user.
type UsageError struct{ Msg string }

func (e *UsageError) Error() string { return e.Msg }

const usage = "usage: one line => the whole message is the prompt (runs in the scratch dir).\nmulti-line => @quack [repo-or-path|temp-dir] [claude|codex|fable] [no-headless] [no-wt] [effort=] [name=] [base=]\n<prompt on the following line(s)>"

// Parse parses content that has already had the bot mention stripped.
//
// A single-line mention (no newline) is taken verbatim as the prompt with no
// directive parsing, so a bare question runs in the scratch workspace.
//
// Otherwise line 1 is the directive line (it may be empty) and everything after
// the first newline is the prompt. The directive line holds, in any order: an
// optional target (repo ref, path, or the literal `temp-dir`) as the first
// non-flag/non-keyword token; bare keywords (`codex`/`claude`/`fable`,
// `no-headless`/`headless`); and key=value flags (agent=, effort=, name=, base=,
// headless=). Headless defaults to true.
func Parse(content string) (*Directive, error) {
	d := &Directive{Headless: true}

	first, rest, multiline := strings.Cut(content, "\n")
	if !multiline {
		// Single line: the whole message is the prompt, no directive.
		d.Prompt = strings.TrimSpace(content)
		if d.Prompt == "" {
			return nil, &UsageError{Msg: "missing prompt (mention me with a question). " + usage}
		}
		return d, nil
	}

	targetSet := false
	for _, tok := range strings.Fields(first) {
		if key, val, ok := strings.Cut(tok, "="); ok {
			switch key {
			case "agent":
				d.Agent = val
			case "effort":
				d.Effort = val
			case "name":
				d.Name = val
			case "base":
				d.Base = val
			case "headless":
				b, perr := strconv.ParseBool(val)
				if perr != nil {
					return nil, &UsageError{Msg: fmt.Sprintf("bad headless %q (want true/false). %s", val, usage)}
				}
				d.Headless = b
			default:
				return nil, &UsageError{Msg: fmt.Sprintf("unknown flag %q. %s", key, usage)}
			}
			continue
		}

		switch tok {
		case "codex", "claude", "fable":
			d.Agent = tok
		case "no-headless":
			d.Headless = false
		case "headless":
			d.Headless = true
		case "no-wt":
			d.NoWorktree = true
		case "sandbox":
			d.Sandbox = true
		default:
			if targetSet {
				return nil, &UsageError{Msg: fmt.Sprintf("unexpected token %q. %s", tok, usage)}
			}
			d.Target = tok
			targetSet = true
		}
	}

	d.Prompt = strings.TrimSpace(rest)
	if d.Prompt == "" {
		return nil, &UsageError{Msg: "missing prompt (put it on the line after the mention). " + usage}
	}
	return d, nil
}
