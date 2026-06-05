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

func TestLoad_AgentModel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	body := "[agents.infer]\ncommand = \"claude\"\nmodel = \"claude-haiku-4-5-20251001\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	a, ok := cfg.Agents["infer"]
	if !ok {
		t.Fatalf("agents[\"infer\"] not found")
	}
	if a.Model != "claude-haiku-4-5-20251001" {
		t.Errorf("infer agent model = %q, want %q", a.Model, "claude-haiku-4-5-20251001")
	}
}

func TestLoad_InferGuidance(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	body := "infer_guidance = \"bare dagger means dagger/dagger\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.InferGuidance != "bare dagger means dagger/dagger" {
		t.Errorf("InferGuidance = %q", cfg.InferGuidance)
	}
}

func TestDiscordAllowlistMerge(t *testing.T) {
	d := Discord{
		AllowedUserID:    "u1",
		AllowedUserIDs:   []string{"u2", "u3"},
		AllowedGuildIDs:  []string{"g1", "g2"},
		AllowedChannelID: "c1",
	}
	if got, want := d.UserIDs(), []string{"u1", "u2", "u3"}; !equalIDs(got, want) {
		t.Errorf("UserIDs() = %v, want %v (singular merged with list)", got, want)
	}
	if got, want := d.GuildIDs(), []string{"g1", "g2"}; !equalIDs(got, want) {
		t.Errorf("GuildIDs() = %v, want %v (list only)", got, want)
	}
	if got, want := d.ChannelIDs(), []string{"c1"}; !equalIDs(got, want) {
		t.Errorf("ChannelIDs() = %v, want %v (singular only)", got, want)
	}
	// An unset dimension yields an empty list ("any").
	if got := (Discord{}).GuildIDs(); len(got) != 0 {
		t.Errorf("empty GuildIDs() = %v, want empty (any)", got)
	}
	// Empty strings inside a list are dropped.
	if got := (Discord{AllowedGuildIDs: []string{"", "g1"}}).GuildIDs(); !equalIDs(got, []string{"g1"}) {
		t.Errorf("GuildIDs() with blank entry = %v, want [g1]", got)
	}
}

func equalIDs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestLoad_AllowedGuildIDs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	body := "[discord]\nallowed_guild_ids = [\"g1\", \"g2\"]\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Discord.GuildIDs(); !equalIDs(got, []string{"g1", "g2"}) {
		t.Errorf("GuildIDs() = %v, want [g1 g2]", got)
	}
}

func TestOwnerAndGuestAccessors(t *testing.T) {
	d := Discord{
		AllowedUserID: "legacy",
		OwnerUserIDs:  []string{"owner1"},
		GuestRoleID:   "grole",
		GuestRoleIDs:  []string{"grole2"},
	}
	owners := d.OwnerIDs()
	if len(owners) != 2 || owners[0] != "owner1" || owners[1] != "legacy" {
		t.Fatalf("owners = %v", owners)
	}
	roles := d.GuestRoles()
	if len(roles) != 2 || roles[0] != "grole" || roles[1] != "grole2" {
		t.Fatalf("guest roles = %v", roles)
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
