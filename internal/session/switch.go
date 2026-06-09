package session

import "strings"

// matchSwitch reports whether text's first whitespace-delimited token is a
// switch trigger — "/<name>" for a configured agent that is headless and opted
// in with switchable=true. On a match it returns the agent name and the rest of
// the line (the inline prompt, internal spacing preserved, empty for a bare
// switch). A trigger anywhere but first does not match. /stop, /attach and fast
// commands are matched earlier in the bot, so they take precedence over a
// same-named agent.
func (s *Service) matchSwitch(text string) (agentName, prompt string, ok bool) {
	trimmed := strings.TrimSpace(text)
	fields := strings.Fields(trimmed)
	if len(fields) == 0 {
		return "", "", false
	}
	tok := fields[0]
	if !strings.HasPrefix(tok, "/") {
		return "", "", false
	}
	name := strings.TrimPrefix(tok, "/")
	ag, exists := s.cfg.Agents[name]
	if !exists || !ag.Headless || !ag.Switchable {
		return "", "", false
	}
	return name, strings.TrimSpace(trimmed[len(tok):]), true
}
