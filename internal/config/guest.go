package config

// Guest configures the sandbox applied to non-owner (guest) sessions. The whole
// feature is inert unless [discord].guest_role_id(s) is also set.
type Guest struct {
	Image            string   `toml:"image"`             // agent container image
	ProxyImage       string   `toml:"proxy_image"`       // egress proxy image
	DindImage        string   `toml:"dind_image"`        // docker:dind
	ProxyPort        string   `toml:"proxy_port"`        // default 8888
	GitHubPAT        string   `toml:"github_pat"`        // fine-grained PAT (or via QUACK_GUEST_GITHUB_PAT env)
	GitUserName      string   `toml:"git_user_name"`     // commit identity for guests
	GitUserEmail     string   `toml:"git_user_email"`
	EgressAllow      []string `toml:"egress_allow"`      // proxy allow-list hosts
	ModelCredMounts  []string `toml:"model_cred_mounts"` // "host:container" ro binds
	AllowedTools     string   `toml:"allowed_tools"`     // claude --allowedTools for guests
	DisallowedTools  string   `toml:"disallowed_tools"`  // claude --disallowedTools for guests
	DisallowedSkills []string `toml:"disallowed_skills"`
	AllowedSkills    []string `toml:"allowed_skills"`
}

// WithDefaults fills unset fields with sensible defaults.
func (g Guest) WithDefaults() Guest {
	if g.Image == "" {
		g.Image = "quack-sandbox:latest"
	}
	if g.ProxyImage == "" {
		g.ProxyImage = "quack-egress:latest"
	}
	if g.DindImage == "" {
		g.DindImage = "docker:dind"
	}
	if g.ProxyPort == "" {
		g.ProxyPort = "8888"
	}
	if len(g.EgressAllow) == 0 {
		g.EgressAllow = []string{"api.anthropic.com", "api.openai.com", "github.com", "api.github.com", "codeload.github.com"}
	}
	return g
}
