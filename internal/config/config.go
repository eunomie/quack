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
	InferGuidance     string                 `toml:"infer_guidance"`      // standing hints folded into the infer prompt (empty => off)
	InferHistoryLimit int                    `toml:"infer_history_limit"` // recent messages fed to the infer agent (default: 20)
	StateDir          string                 `toml:"state_dir"`
	Discord           Discord                `toml:"discord"`
	Agents            map[string]agent.Agent `toml:"agents"`
}

// Discord holds Discord-specific settings.
//
// The allowlist accepts either a single id (allowed_*_id) or a list
// (allowed_*_ids); both are merged. An empty merged list means "any". Keeping
// the singular fields working means an existing config can never silently widen
// to allow everyone after an upgrade.
type Discord struct {
	Token                    string   `toml:"token"`
	AllowedUserID            string   `toml:"allowed_user_id"`
	AllowedUserIDs           []string `toml:"allowed_user_ids"`
	AllowedGuildID           string   `toml:"allowed_guild_id"`
	AllowedGuildIDs          []string `toml:"allowed_guild_ids"`
	AllowedChannelID         string   `toml:"allowed_channel_id"`
	AllowedChannelIDs        []string `toml:"allowed_channel_ids"`
	ThreadAutoArchiveMinutes int      `toml:"thread_auto_archive_minutes"`
}

// UserIDs, GuildIDs, and ChannelIDs return the merged allowlist for each
// dimension (singular field + list). An empty result means "any".
func (d Discord) UserIDs() []string    { return mergeIDs(d.AllowedUserID, d.AllowedUserIDs) }
func (d Discord) GuildIDs() []string   { return mergeIDs(d.AllowedGuildID, d.AllowedGuildIDs) }
func (d Discord) ChannelIDs() []string { return mergeIDs(d.AllowedChannelID, d.AllowedChannelIDs) }

// mergeIDs combines a singular id with a list, dropping empties.
func mergeIDs(single string, many []string) []string {
	out := make([]string, 0, len(many)+1)
	if single != "" {
		out = append(out, single)
	}
	for _, id := range many {
		if id != "" {
			out = append(out, id)
		}
	}
	return out
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
