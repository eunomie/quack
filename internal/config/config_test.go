package config

import (
	"os"
	"path/filepath"
	"testing"
)

const sample = `
dev_src_root = "~/dev/src"
clone_protocol = "ssh"
default_agent = "claude"

[discord]
allowed_user_id = "111"
allowed_guild_id = "222"
thread_auto_archive_minutes = 10080

[agents.claude]
command = "claude"
effort_template = "--effort {effort}"

[agents.codex]
command = "codex"
effort_template = "--config model_reasoning_effort={effort}"
`

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(sample), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DISCORD_BOT_TOKEN", "tok-from-env")
	t.Setenv("HOME", "/home/tester")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Discord.Token != "tok-from-env" {
		t.Errorf("token = %q, want env override", cfg.Discord.Token)
	}
	if cfg.DevSrcRoot != "/home/tester/dev/src" {
		t.Errorf("DevSrcRoot = %q (tilde not expanded)", cfg.DevSrcRoot)
	}
	if cfg.ScratchDir != "/home/tester/dev/work" {
		t.Errorf("ScratchDir default = %q, want ~/dev/work expanded", cfg.ScratchDir)
	}
	if cfg.DefaultAgent != "claude" {
		t.Errorf("DefaultAgent = %q", cfg.DefaultAgent)
	}
	if cfg.NameAgent != "claude" {
		t.Errorf("NameAgent default = %q, want claude", cfg.NameAgent)
	}
	if cfg.Discord.ThreadAutoArchiveMinutes != 10080 {
		t.Errorf("archive minutes = %d", cfg.Discord.ThreadAutoArchiveMinutes)
	}
	a, ok := cfg.Agents["codex"]
	if !ok || a.Command != "codex" || a.EffortTemplate != "--config model_reasoning_effort={effort}" {
		t.Errorf("codex agent = %+v ok=%v", a, ok)
	}
}

func TestLoad_InferDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("name_agent = \"codex\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.InferEffort != "medium" {
		t.Errorf("InferEffort = %q, want medium", cfg.InferEffort)
	}
	if cfg.InferHistoryLimit != 20 {
		t.Errorf("InferHistoryLimit = %d, want 20", cfg.InferHistoryLimit)
	}
	if cfg.InferAgent != "codex" {
		t.Errorf("InferAgent = %q, want codex (defaults to name_agent)", cfg.InferAgent)
	}
}

func TestLoad_InferAgentExplicitOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("name_agent = \"claude\"\ninfer_agent = \"codex\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.InferAgent != "codex" {
		t.Errorf("InferAgent = %q, want codex (explicit value must win over name_agent fallback)", cfg.InferAgent)
	}
}

func TestLoad_ScratchDirExplicit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("scratch_dir = \"~/scratch\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", "/home/tester")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ScratchDir != "/home/tester/scratch" {
		t.Errorf("ScratchDir = %q, want explicit value expanded", cfg.ScratchDir)
	}
}
