// Package config loads quack's TOML configuration.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/eunomie/quack/internal/agent"
)

// Config is the full quack configuration.
type Config struct {
	DevSrcRoot        string                 `toml:"dev_src_root"`
	ScratchDir        string                 `toml:"scratch_dir"`
	CloneProtocol     string                 `toml:"clone_protocol"`
	DefaultAgent      string                 `toml:"default_agent"`
	NameAgent         string                 `toml:"name_agent"`
	InferAgent        string                 `toml:"infer_agent"`         // agent for the fluent `! ` infer step (default: name_agent)
	InferEffort       string                 `toml:"infer_effort"`        // effort for the infer one-shot (default: medium)
	InferHistoryLimit int                    `toml:"infer_history_limit"` // recent messages fed to the infer agent (default: 20)
	StateDir          string                 `toml:"state_dir"`
	Discord           Discord                `toml:"discord"`
	Agents            map[string]agent.Agent `toml:"agents"`
}

// Discord holds Discord-specific settings.
type Discord struct {
	Token                    string `toml:"token"`
	AllowedUserID            string `toml:"allowed_user_id"`
	AllowedGuildID           string `toml:"allowed_guild_id"`
	AllowedChannelID         string `toml:"allowed_channel_id"`
	ThreadAutoArchiveMinutes int    `toml:"thread_auto_archive_minutes"`
}

// Load reads config from path, applies the DISCORD_BOT_TOKEN env override,
// expands ~ in paths, and fills defaults.
func Load(path string) (*Config, error) {
	var cfg Config
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}

	if tok := os.Getenv("DISCORD_BOT_TOKEN"); tok != "" {
		cfg.Discord.Token = tok
	}

	cfg.DevSrcRoot = expandHome(cfg.DevSrcRoot)
	cfg.ScratchDir = expandHome(cfg.ScratchDir)
	cfg.StateDir = expandHome(cfg.StateDir)

	if cfg.DevSrcRoot == "" {
		cfg.DevSrcRoot = expandHome("~/dev/src")
	}
	if cfg.ScratchDir == "" {
		cfg.ScratchDir = expandHome("~/dev/work")
	}
	if cfg.StateDir == "" {
		cfg.StateDir = expandHome("~/.local/state/quack")
	}
	if cfg.CloneProtocol == "" {
		cfg.CloneProtocol = "ssh"
	}
	if cfg.DefaultAgent == "" {
		cfg.DefaultAgent = "claude"
	}
	if cfg.NameAgent == "" {
		cfg.NameAgent = "claude"
	}
	if cfg.InferAgent == "" {
		cfg.InferAgent = cfg.NameAgent
	}
	if cfg.InferEffort == "" {
		cfg.InferEffort = "medium"
	}
	if cfg.InferHistoryLimit == 0 {
		cfg.InferHistoryLimit = 20
	}
	if cfg.Discord.ThreadAutoArchiveMinutes == 0 {
		cfg.Discord.ThreadAutoArchiveMinutes = 10080
	}
	if cfg.Agents == nil {
		cfg.Agents = map[string]agent.Agent{}
	}
	return &cfg, nil
}

func expandHome(p string) string {
	if p == "" {
		return ""
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		return filepath.Join(os.Getenv("HOME"), strings.TrimPrefix(p, "~"))
	}
	return p
}
