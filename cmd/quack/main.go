// Command quack runs the Discord bot that starts agent sessions.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/eunomie/quack/internal/agentproc"
	"github.com/eunomie/quack/internal/config"
	"github.com/eunomie/quack/internal/discord"
	"github.com/eunomie/quack/internal/gitexec"
	"github.com/eunomie/quack/internal/session"
	"github.com/eunomie/quack/internal/tmuxexec"
)

func main() {
	defaultCfg := filepath.Join(os.Getenv("HOME"), ".config", "quack", "config.toml")
	cfgPath := flag.String("config", defaultCfg, "path to config.toml")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if cfg.Discord.Token == "" {
		log.Fatal("no Discord token (set [discord].token or DISCORD_BOT_TOKEN)")
	}

	scfg := session.Config{
		DevSrcRoot:           cfg.DevSrcRoot,
		ScratchDir:           cfg.ScratchDir,
		CloneProtocol:        cfg.CloneProtocol,
		DefaultAgent:         cfg.DefaultAgent,
		NameAgent:            cfg.NameAgent,
		InferAgent:           cfg.InferAgent,
		InferEffort:          cfg.InferEffort,
		InferGuidance:        cfg.InferGuidance,
		InferHistoryLimit:    cfg.InferHistoryLimit,
		StateDir:             cfg.StateDir,
		ThreadAutoArchiveMin: cfg.Discord.ThreadAutoArchiveMinutes,
		Agents:               cfg.Agents,
	}

	g := gitexec.New()
	tx := tmuxexec.New()
	drivers := map[string]agentproc.Driver{}
	for name, a := range cfg.Agents {
		if !a.Headless {
			continue
		}
		switch a.Command {
		case "claude":
			drivers[name] = agentproc.Claude{
				Command:        a.Command,
				Model:          a.Model,
				EffortTemplate: a.EffortTemplate,
				NameTemplate:   a.NameTemplate,
				PermissionMode: a.Mode(),
				AllowedTools:   a.AllowedTools,
				Settings:       a.Settings,
			}
		case "codex":
			drivers[name] = agentproc.Codex{Command: a.Command, EffortTemplate: a.EffortTemplate}
		default:
			log.Printf("agent %q has headless=true but command %q has no driver; ignoring", name, a.Command)
		}
	}

	var svc *session.Service
	bot, err := discord.New(cfg.Discord.Token, discord.Allow{
		UserIDs:    cfg.Discord.UserIDs(),
		GuildIDs:   cfg.Discord.GuildIDs(),
		ChannelIDs: cfg.Discord.ChannelIDs(),
	}, func(r session.Replier) *session.Service {
		svc = session.New(scfg, g, tx, r)
		svc.UseDrivers(drivers)
		if h, ok := r.(session.History); ok {
			svc.UseHistory(h)
		}
		return svc
	})
	if err != nil {
		log.Fatalf("discord: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Restore headless sessions persisted by a previous run before the gateway
	// opens, so a thread message can't race the rebuild. Interactive (tmux)
	// sessions outlive a restart on their own and need nothing here.
	if n := svc.Rehydrate(ctx); n > 0 {
		log.Printf("quack: restored %d headless session(s)", n)
	}

	if err := bot.Run(ctx); err != nil {
		log.Fatalf("run: %v", err)
	}
}
