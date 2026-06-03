// Package agent builds the argv used to launch a coding agent.
package agent

import "strings"

// Agent describes how to launch one coding agent.
type Agent struct {
	Command        string `toml:"command"`         // executable, e.g. "claude"
	EffortTemplate string `toml:"effort_template"` // contains "{effort}", e.g. "--effort {effort}"
	NameTemplate   string `toml:"name_template"`   // contains "{name}", e.g. "-n {name}"
	ResumeTemplate string `toml:"resume_template"` // contains "{session}", e.g. "--resume {session}"
	DefaultEffort  string `toml:"default_effort"`  // used when the command gives no effort
	Headless       bool   `toml:"headless"`        // has a headless driver
	PermissionMode string `toml:"permission_mode"` // claude: acceptEdits|bypassPermissions|auto
	AllowedTools   string `toml:"allowed_tools"`   // claude: --allowedTools value
	Settings       string `toml:"settings"`        // claude: --settings JSON or file (e.g. sandbox)
	// InteractiveArgs are extra flags appended to interactive (no-headless) and
	// /attach launches only — not the headless driver, which governs permissions
	// via PermissionMode. When unset, claude defaults to
	// "--dangerously-skip-permissions" (its usual interactive workflow). Set it
	// to override (e.g. "" to disable, or other flags); a pointer so an explicit
	// empty value is distinguishable from "not configured".
	InteractiveArgs *string `toml:"interactive_args"`
}

// Argv builds the launch argv: command, optional session-name flags, optional
// effort flags, any InteractiveArgs, then the prompt as the final argument.
// name/effort are passed through verbatim; an empty value or empty template
// adds no flags.
func (a Agent) Argv(effort, name, prompt string) []string {
	argv := []string{a.Command}
	if name != "" && a.NameTemplate != "" {
		argv = append(argv, strings.Fields(strings.ReplaceAll(a.NameTemplate, "{name}", name))...)
	}
	if effort != "" && a.EffortTemplate != "" {
		argv = append(argv, strings.Fields(strings.ReplaceAll(a.EffortTemplate, "{effort}", effort))...)
	}
	argv = append(argv, a.interactiveArgs()...)
	return append(argv, prompt)
}

// interactiveArgs returns the extra flags for interactive and /attach launches.
// claude defaults to bypassing the permission prompt when InteractiveArgs is
// unset, matching the usual `claude --dangerously-skip-permissions` workflow; an
// explicit value (including "") overrides that default.
func (a Agent) interactiveArgs() []string {
	s := ""
	switch {
	case a.InteractiveArgs != nil:
		s = *a.InteractiveArgs
	case a.Command == "claude":
		s = "--dangerously-skip-permissions"
	}
	return strings.Fields(s)
}

// EffortOr returns effort, or the agent's DefaultEffort when effort is empty.
func (a Agent) EffortOr(effort string) string {
	if effort != "" {
		return effort
	}
	return a.DefaultEffort
}

// ResumeArgv builds the argv to resume an existing session interactively (used
// when promoting a headless session to a tmux one). Returns nil if the agent
// has no resume template or no session id.
func (a Agent) ResumeArgv(session string) []string {
	if a.ResumeTemplate == "" || session == "" {
		return nil
	}
	argv := []string{a.Command}
	argv = append(argv, strings.Fields(strings.ReplaceAll(a.ResumeTemplate, "{session}", session))...)
	return append(argv, a.interactiveArgs()...)
}

// Mode returns the headless permission mode, defaulting to acceptEdits.
func (a Agent) Mode() string {
	if a.PermissionMode == "" {
		return "acceptEdits"
	}
	return a.PermissionMode
}
