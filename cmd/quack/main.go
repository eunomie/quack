// Command quack runs the Discord bot that starts agent sessions.
package main

import (
	"context"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/eunomie/quack/internal/agentproc"
	"github.com/eunomie/quack/internal/askmcp"
	"github.com/eunomie/quack/internal/cmdexec"
	"github.com/eunomie/quack/internal/config"
	"github.com/eunomie/quack/internal/discord"
	"github.com/eunomie/quack/internal/gitexec"
	"github.com/eunomie/quack/internal/sandbox"
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
	for _, fc := range cfg.FastCommands {
		scfg.FastCommands = append(scfg.FastCommands, session.FastCommand{
			Trigger: fc.Trigger,
			Argv:    fc.Argv,
		})
	}

	var svc *session.Service

	// Owner-answered questions: serve the ask_user MCP tool on a localhost port and
	// hand each headless claude its base URL. resolveAsk runs once svc exists (well
	// before any session can ask), so the closure captures it safely.
	askLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatalf("ask server listen: %v", err)
	}
	askURL := "http://" + askLn.Addr().String() + "/mcp"
	askSrv := askmcp.New(func(ctx context.Context, token string, q askmcp.Question) (askmcp.Answer, error) {
		return svc.ResolveAsk(ctx, token, q)
	})
	go func() {
		if err := http.Serve(askLn, askSrv); err != nil {
			log.Printf("ask server stopped: %v", err)
		}
	}()

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
				AskMCPURL:      askURL,
			}
		case "codex":
			drivers[name] = agentproc.Codex{Command: a.Command, EffortTemplate: a.EffortTemplate, SandboxMode: a.Sandbox()}
		default:
			log.Printf("agent %q has headless=true but command %q has no driver; ignoring", name, a.Command)
		}
	}

	bot, err := discord.New(cfg.Discord.Token, discord.Allow{
		UserIDs:      cfg.Discord.UserIDs(),
		GuildIDs:     cfg.Discord.GuildIDs(),
		ChannelIDs:   cfg.Discord.ChannelIDs(),
		OwnerUserIDs: cfg.Discord.OwnerIDs(),
		GuestRoleIDs: cfg.Discord.GuestRoles(),
	}, cfg.Discord.IgnorePrefixes, func(r session.Replier) *session.Service {
		svc = session.New(scfg, g, tx, r)
		svc.UseDrivers(drivers)
		svc.UseRunner(cmdexec.New())
		if h, ok := r.(session.History); ok {
			svc.UseHistory(h)
		}
		// Wire the sandbox when guests are configured, OR when a guest github_pat is
		// set even with no guest roles: the PAT is the owner's "I've configured the
		// sandbox" signal, so an owner can dogfood `! sandbox` without exposing it to
		// any guest first.
		if len(cfg.Discord.GuestRoles()) > 0 || cfg.Guest.GitHubPAT != "" {
			prov := &sandbox.DockerProvisioner{
				D:          sandbox.NewDocker(),
				AgentImage: cfg.Guest.Image,
				ProxyImage: cfg.Guest.ProxyImage,
				DindImage:  cfg.Guest.DindImage,
				ProxyPort:  cfg.Guest.ProxyPort,
			}
			svc.UseSandbox(sandbox.Adapter{P: prov}, session.GuestPolicy{
				GitHubPAT:        cfg.Guest.GitHubPAT,
				GitUserName:      cfg.Guest.GitUserName,
				GitUserEmail:     cfg.Guest.GitUserEmail,
				EgressAllow:      cfg.Guest.EgressAllow,
				CredFiles:        parseMounts(cfg.Guest.CredFiles),
				AllowedTools:     cfg.Guest.AllowedTools,
				DisallowedTools:  cfg.Guest.DisallowedTools,
				DisallowedSkills: cfg.Guest.DisallowedSkills,
				AllowedSkills:    cfg.Guest.AllowedSkills,
			})
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

// parseMounts turns "host:container" strings into SandboxMounts, skipping malformed entries.
func parseMounts(in []string) []session.SandboxMount {
	out := make([]session.SandboxMount, 0, len(in))
	for _, m := range in {
		if i := strings.IndexByte(m, ':'); i > 0 && i < len(m)-1 {
			out = append(out, session.SandboxMount{Host: m[:i], Container: m[i+1:]})
		}
	}
	return out
}
