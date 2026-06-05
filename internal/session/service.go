package session

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/eunomie/quack/internal/agent"
	"github.com/eunomie/quack/internal/agentproc"
	"github.com/eunomie/quack/internal/command"
	"github.com/eunomie/quack/internal/repo"
	"github.com/eunomie/quack/internal/worktree"
)

// Git is the subset of git operations quack needs.
type Git interface {
	IsRepo(path string) bool
	PrimaryClone(path string) (string, error)
	Exists(clonePath string) bool
	PathExists(path string) bool
	Clone(ctx context.Context, url, clonePath string) error
	Fetch(ctx context.Context, clonePath string) error
	DefaultBranch(ctx context.Context, clonePath string) (string, error)
	BranchExists(ctx context.Context, clonePath, branch string) bool
	RefExists(ctx context.Context, clonePath, ref string) bool
	Remotes(ctx context.Context, clonePath string) []string
	AddWorktree(ctx context.Context, clonePath, worktreePath, branch, baseRef string) error
}

// Tmux creates detached tmux sessions.
type Tmux interface {
	SessionExists(name string) bool
	NewSession(ctx context.Context, o NewSessionOpts) error
}

// NewSessionOpts describes a tmux session to create. Argv is exec'd directly
// (no shell), so the prompt needs no escaping.
type NewSessionOpts struct {
	Name string
	Dir  string
	Env  []string
	Argv []string
}

// Replier posts back to Discord (threads + messages + reactions).
type Replier interface {
	OpenThread(ctx context.Context, channelID, messageID, name string, autoArchiveMin int) (threadID string, err error)
	Post(ctx context.Context, channelID, content string) (messageID string, err error)
	// PostSilent posts a message with Discord's suppress-notifications flag, so
	// it appears in the thread but triggers no push/desktop notification. Used
	// for tool activity and progress chrome so only the agent's actual answers
	// notify.
	PostSilent(ctx context.Context, channelID, content string) (messageID string, err error)
	Edit(ctx context.Context, channelID, messageID, content string) error
	Delete(ctx context.Context, channelID, messageID string) error
	RenameThread(ctx context.Context, threadID, name string) error
	ArchiveThread(ctx context.Context, threadID string) error
	React(ctx context.Context, channelID, messageID, emoji string) error
	Unreact(ctx context.Context, channelID, messageID, emoji string) error
}

// Message is one recent Discord message, used as context for the fluent infer step.
type Message struct {
	Author  string
	Content string
}

// History reads recent Discord messages. It is a read-only sibling of Replier,
// supplied by the same discord adapter and wired in main.go.
type History interface {
	// RecentMessages returns up to limit messages in channelID posted before
	// beforeID, ordered oldest-first.
	RecentMessages(ctx context.Context, channelID, beforeID string, limit int) ([]Message, error)
}

// Status reactions placed on the user's own triggering message.
const (
	emojiWorking = "👀"
	emojiDone    = "✅"
	emojiError   = "❌"
	emojiStopped = "🛑"
)

// Scratch session policy: a no-target command (a quick question) runs in the
// configured scratch dir at a moderate effort. `temp-dir` is the reserved target
// that opts back into a throwaway temp dir with the agent's default effort.
const (
	targetTempDir = "temp-dir"
	scratchEffort = "medium"
)

// Config is the orchestrator's runtime configuration.
type Config struct {
	DevSrcRoot           string
	ScratchDir           string // workspace for no-target "quick question" sessions
	CloneProtocol        string
	DefaultAgent         string
	NameAgent            string // agent used to name sessions (default claude)
	InferAgent           string // agent for the fluent `! ` infer step
	InferEffort          string // effort for the infer one-shot
	InferGuidance        string // standing hints folded into the infer prompt
	InferHistoryLimit    int    // recent Discord messages fed to the infer agent
	StateDir             string
	ThreadAutoArchiveMin int
	AskTimeout           time.Duration // how long an ask_user waits for the owner (0 => default)
	Agents               map[string]agent.Agent
	// FastCommands are trigger→argv launchers run directly (bypassing the agent)
	// when their trigger is the first word of a tracked-thread message.
	FastCommands []FastCommand
}

// Request is one parsed-but-unprocessed Discord command.
type Request struct {
	Content     string       // mention-stripped
	Attachments []Attachment // files dropped on the command (e.g. screenshots)
	Origin      Origin       // guild/channel/message/author/createdAt set; thread/reply empty

	// InThread is set when the mention arrived inside an existing thread (commonly
	// a forum post). The session then runs in place in that thread — Origin.ChannelID
	// is the thread id — instead of opening a new one. ThreadName is the thread's
	// current title, used verbatim as the Discord-facing session title.
	InThread   bool
	ThreadName string
}

// Service orchestrates a session launch.
type Service struct {
	cfg     Config
	git     Git
	tmux    Tmux
	reply   Replier
	history History

	mkdirAll  func(path string, perm uint32) error
	writeFile func(path string, data []byte, perm uint32) error
	remove    func(path string) error
	readDir   func(path string) ([]string, error) // immediate subdir names under path
	readFile  func(path string) ([]byte, error)
	trustDir  func(dir string) error
	mkdirTemp func() (string, error)
	newToken  func() string
	fetchURL  func(ctx context.Context, url string) ([]byte, error)

	locks keyedMutex

	drivers  map[string]agentproc.Driver
	hmu      sync.Mutex
	sessions map[string]*liveSession
	// askByToken resolves an ask_user MCP call (which carries only a per-session
	// token) back to its session. Guarded by hmu alongside sessions.
	askByToken map[string]*liveSession

	runner Runner
}

// New builds a Service with real filesystem writers.
func New(cfg Config, g Git, tx Tmux, r Replier) *Service {
	return &Service{
		cfg: cfg, git: g, tmux: tx, reply: r,
		mkdirAll:  func(p string, m uint32) error { return os.MkdirAll(p, os.FileMode(m)) },
		writeFile: func(p string, d []byte, m uint32) error { return os.WriteFile(p, d, os.FileMode(m)) },
		remove:    os.Remove,
		readDir:   readSubdirs,
		readFile:  os.ReadFile,
		trustDir:  ensureClaudeTrust,
		mkdirTemp: func() (string, error) { return os.MkdirTemp("", "quack-*") },
		newToken:  defaultToken,
		fetchURL:  httpFetch,
	}
}

// Handle processes one command end-to-end, reporting progress/errors via Replier.
//
// The default is the fluent infer path: the whole message is a natural-language
// request handed to the infer one-shot. A leading `!` opts into the explicit
// directive grammar instead, where the first line carries the args (repo/path,
// agent, effort, base, name, …) and the prompt follows.
func (s *Service) Handle(ctx context.Context, req Request) {
	if spec, ok := directivePrefix(req.Content); ok {
		dir, err := command.Parse(spec)
		if err != nil {
			_ = s.reply.React(ctx, req.Origin.ChannelID, req.Origin.MessageID, emojiError)
			_, _ = s.reply.Post(ctx, req.Origin.ChannelID, "🦆 "+err.Error())
			return
		}
		explicit := dir.Name != ""
		s.run(ctx, req, dir, explicit, "", "")
		return
	}

	s.handleFluent(ctx, req, strings.TrimSpace(req.Content))
}

// run launches a session from a fully-formed directive — shared by the plain
// grammar (Handle) and the fluent infer path (handleFluent). explicit reports
// whether the name is user-pinned (a collision is an error, not a bump);
// suggested is a pre-computed name that skips the naming agent when non-empty;
// preface, when non-empty, is posted muted right after the ack (the fluent path
// uses it to echo how the request was interpreted, or to note a fallback).
// Callers that add the 👀 working reaction before calling run (the fluent path)
// have it cleared on the terminal outcome; on the plain path nothing added it, so
// the Unreact is a harmless no-op.
func (s *Service) run(ctx context.Context, req Request, dir *command.Directive, explicit bool, suggested, preface string) {
	agentName := orDefault(dir.Agent, s.cfg.DefaultAgent)
	ag, ok := s.cfg.Agents[agentName]
	if !ok {
		_ = s.reply.Unreact(ctx, req.Origin.ChannelID, req.Origin.MessageID, emojiWorking)
		_ = s.reply.React(ctx, req.Origin.ChannelID, req.Origin.MessageID, emojiError)
		_, _ = s.reply.Post(ctx, req.Origin.ChannelID, "🦆 unknown agent: "+agentName)
		return
	}

	token := s.newToken()
	provisional := s.provisionalName(dir, token)
	// A no-target "quick question" defaults to a moderate effort, still
	// overridable with an explicit effort.
	if dir.Target == "" && dir.Effort == "" {
		dir.Effort = scratchEffort
	}
	effort := ag.EffortOr(dir.Effort)

	// A mention already inside a thread (a forum post) runs in place: drive that
	// thread directly. Discord can't nest threads anyway, so OpenThread would only
	// fail here. Otherwise open a fresh thread off the triggering message.
	threadID := req.Origin.ChannelID
	if !req.InThread {
		if id, err := s.reply.OpenThread(ctx, req.Origin.ChannelID, req.Origin.MessageID, provisional, s.cfg.ThreadAutoArchiveMin); err == nil {
			threadID = id
		}
	}
	req.Origin.ThreadID = threadID

	target := dir.Target
	if target == "" {
		target = s.cfg.ScratchDir
	}
	ackID, _ := s.reply.PostSilent(ctx, threadID, mutedText("🦆 on it — preparing `"+target+"`…"))
	req.Origin.ReplyID = ackID

	if preface != "" {
		_, _ = s.reply.PostSilent(ctx, threadID, preface)
	}

	// report edits the ack in place, or posts fresh if the ack never landed.
	report := func(content string) {
		if ackID != "" {
			_ = s.reply.Edit(ctx, threadID, ackID, content)
		} else {
			_, _ = s.reply.Post(ctx, threadID, content)
		}
	}
	fail := func(msg string) {
		_ = s.reply.Unreact(ctx, req.Origin.ChannelID, req.Origin.MessageID, emojiWorking)
		_ = s.reply.React(ctx, req.Origin.ChannelID, req.Origin.MessageID, emojiError)
		report("❌ " + msg)
	}

	// With no explicit name and none pre-supplied, ask the agent to name the task
	// (falls back to the repo-base-random scheme if it errors or no driver exists).
	if !explicit && suggested == "" {
		suggested = s.suggestName(ctx, agentName, dir.Prompt)
	}

	prep, err := s.prepare(ctx, dir, provisional, explicit, token, suggested)
	if err != nil {
		fail(err.Error())
		return
	}

	// Title the thread "<owner/repo|dir> <name>" so multiple threads are easy to
	// place at a glance. The provisional thread name has no label, so a labelled
	// workspace always renames; an unlabelled one only renames if the name changed.
	//
	// Headless sessions skip this rename: their titleUpdater renames to
	// "👀 <label> <name>" the moment the first turn starts working, so renaming
	// here too would edit the thread name twice in quick succession and hit
	// Discord's heavy thread-rename rate limit (~2 per 10 minutes). Letting the
	// titleUpdater own the title collapses startup to a single rename.
	if desired := threadTitle("", prep.label, prep.name); !dir.Headless && desired != provisional && threadID != req.Origin.ChannelID {
		_ = s.reply.RenameThread(ctx, threadID, desired)
	}

	sessDir := filepath.Join(s.cfg.StateDir, "sessions", prep.name)
	contextFile := filepath.Join(sessDir, "context.json")
	if data, jerr := req.Origin.ContextJSON(prep.name); jerr == nil {
		_ = s.mkdirAll(sessDir, 0o755)
		_ = s.writeFile(contextFile, data, 0o644)
	}

	s.maybeTrust(ag, prep.workdir)

	if dir.NoWorktree {
		_, _ = s.reply.PostSilent(ctx, threadID, mutedText("⚠️ no worktree: running directly in the repo checkout. Parallel sessions on the same repo can conflict — use this sparingly."))
	}

	fullPrompt := req.Origin.PromptHeader() + "\n\n" + dir.Prompt
	if block := s.saveAttachments(ctx, prep.name, req.Attachments); block != "" {
		fullPrompt += "\n\n" + block
	}
	if dir.Headless {
		if _, ok := s.drivers[agentName]; !ok {
			fail("headless not supported for agent: " + agentName)
			return
		}
		report(mutedText(successMessage(prep, effort, ag) + "\n_(headless: reply in this thread to talk to it; `/stop` to end)_"))
		s.startHeadless(ctx, agentName, threadID, prep.workdir, effort, prep.name, prep.label,
			turnReq{channelID: req.Origin.ChannelID, messageID: req.Origin.MessageID, text: fullPrompt},
			inPlaceOpts{inPlace: req.InThread, titleBase: req.ThreadName})
		return
	}

	opts := NewSessionOpts{
		Name: "quack/" + prep.name,
		Dir:  prep.workdir,
		Env:  req.Origin.EnvVars(prep.name, contextFile),
		Argv: ag.Argv(effort, prep.name, fullPrompt),
	}
	if err := s.tmux.NewSession(ctx, opts); err != nil {
		fail("launch failed: " + err.Error())
		return
	}

	_ = s.reply.Unreact(ctx, req.Origin.ChannelID, req.Origin.MessageID, emojiWorking)
	_ = s.reply.React(ctx, req.Origin.ChannelID, req.Origin.MessageID, emojiDone)
	report(mutedText(successMessage(prep, effort, ag)))
}

type prepResult struct {
	workdir  string
	name     string
	branch   string
	isolated bool
	// label is the short workspace identifier shown in the Discord thread title:
	// "owner/repo" for a repo ref, the directory basename for a path, or "" for a
	// fresh temp workspace (no repo/dir).
	label string
}

func (s *Service) prepare(ctx context.Context, dir *command.Directive, provisional string, explicit bool, token, suggested string) (prepResult, error) {
	switch dir.Target {
	case "":
		return s.prepareScratch(provisional, suggested)
	case targetTempDir:
		return s.prepareTemp(provisional, suggested)
	}
	if repo.IsPath(dir.Target) {
		return s.prepareFromPath(ctx, dir, provisional, explicit, token, suggested)
	}
	return s.prepareFromRef(ctx, dir, provisional, explicit, token, suggested)
}

// prepareScratch runs in the shared scratch directory (no repo/dir given). It's
// a stable, non-isolated workspace for quick questions — created if missing.
// Concurrent scratch sessions share it, which is fine for read-only Q&A.
func (s *Service) prepareScratch(provisional, suggested string) (prepResult, error) {
	if err := s.mkdirAll(s.cfg.ScratchDir, 0o755); err != nil {
		return prepResult{}, fmt.Errorf("create scratch dir: %w", err)
	}
	return prepResult{workdir: s.cfg.ScratchDir, name: orDefault(suggested, provisional), isolated: false}, nil
}

// prepareTemp runs in a fresh, non-isolated temporary directory — the explicit
// `temp-dir` escape hatch for a throwaway workspace.
func (s *Service) prepareTemp(provisional, suggested string) (prepResult, error) {
	dir, err := s.mkdirTemp()
	if err != nil {
		return prepResult{}, fmt.Errorf("create temp dir: %w", err)
	}
	return prepResult{workdir: dir, name: orDefault(suggested, provisional), isolated: false}, nil
}

func (s *Service) prepareFromRef(ctx context.Context, dir *command.Directive, provisional string, explicit bool, token, suggested string) (prepResult, error) {
	ref, err := repo.ParseRef(dir.Target)
	if err != nil {
		return prepResult{}, err
	}
	clonePath := ref.ClonePath(s.cfg.DevSrcRoot)

	unlock := s.locks.lock(clonePath)
	defer unlock()

	if !s.git.Exists(clonePath) {
		if err := s.git.Clone(ctx, ref.CloneURL(s.cfg.CloneProtocol), clonePath); err != nil {
			return prepResult{}, fmt.Errorf("clone %s: %w", dir.Target, err)
		}
	} else if err := s.git.Fetch(ctx, clonePath); err != nil {
		return prepResult{}, fmt.Errorf("fetch %s: %w", dir.Target, err)
	}
	label := ref.Owner + "/" + ref.Repo
	if dir.NoWorktree {
		return prepResult{workdir: clonePath, name: orDefault(suggested, provisional), isolated: false, label: label}, nil
	}
	res, err := s.makeWorktree(ctx, dir, clonePath, explicit, token, suggested)
	if err != nil {
		return prepResult{}, err
	}
	res.label = label
	return res, nil
}

func (s *Service) prepareFromPath(ctx context.Context, dir *command.Directive, provisional string, explicit bool, token, suggested string) (prepResult, error) {
	p := expandHome(dir.Target)
	if !s.git.PathExists(p) {
		return prepResult{}, fmt.Errorf("path does not exist: %s", dir.Target)
	}
	label := filepath.Base(p)
	if !s.git.IsRepo(p) {
		return prepResult{workdir: p, name: orDefault(suggested, provisional), isolated: false, label: label}, nil
	}
	if dir.NoWorktree {
		return prepResult{workdir: p, name: orDefault(suggested, provisional), isolated: false, label: label}, nil
	}
	clonePath, err := s.git.PrimaryClone(p)
	if err != nil {
		return prepResult{}, err
	}
	unlock := s.locks.lock(clonePath)
	defer unlock()
	if err := s.git.Fetch(ctx, clonePath); err != nil {
		return prepResult{}, fmt.Errorf("fetch %s: %w", dir.Target, err)
	}
	res, err := s.makeWorktree(ctx, dir, clonePath, explicit, token, suggested)
	if err != nil {
		return prepResult{}, err
	}
	res.label = filepath.Base(clonePath)
	return res, nil
}

func (s *Service) makeWorktree(ctx context.Context, dir *command.Directive, clonePath string, explicit bool, token, suggested string) (prepResult, error) {
	base := dir.Base
	if base == "" {
		// Detect the repo's default branch; fall back to main when it can't be
		// determined (e.g. origin/HEAD isn't set) instead of failing the session.
		if b, derr := s.git.DefaultBranch(ctx, clonePath); derr == nil && b != "" {
			base = b
		} else {
			base = "main"
		}
	}
	baseRef, err := s.resolveBaseRef(ctx, clonePath, base)
	if err != nil {
		return prepResult{}, err
	}
	candidate := dir.Name
	if !explicit {
		candidate = orDefault(suggested, worktreeName(clonePath, base, token))
	}
	name, err := s.resolveName(ctx, clonePath, candidate, explicit)
	if err != nil {
		return prepResult{}, err
	}
	wtPath := worktree.Path(clonePath, name)
	if err := s.git.AddWorktree(ctx, clonePath, wtPath, name, baseRef); err != nil {
		return prepResult{}, fmt.Errorf("create worktree: %w", err)
	}
	return prepResult{workdir: wtPath, name: name, branch: name, isolated: true}, nil
}

// resolveBaseRef finds the git ref the new worktree should branch from. It
// prefers remote-tracking refs so the worktree starts from fresh remote state,
// trying origin then upstream then any other remote, and finally a local
// branch of the same name. base may already be remote-qualified (e.g.
// "upstream/1.0-beta"), in which case it is used verbatim when it resolves.
//
// This matters for fork checkouts (origin = your fork, upstream = canonical):
// a branch like "1.0-beta" often exists only on upstream, so "origin/1.0-beta"
// would fail with "invalid reference".
func (s *Service) resolveBaseRef(ctx context.Context, clonePath, base string) (string, error) {
	var candidates []string
	seen := map[string]bool{}
	add := func(ref string) {
		if ref == "" || seen[ref] {
			return
		}
		seen[ref] = true
		candidates = append(candidates, ref)
	}
	if strings.Contains(base, "/") {
		add(base)
	}
	add("origin/" + base)
	add("upstream/" + base)
	for _, r := range s.git.Remotes(ctx, clonePath) {
		add(r + "/" + base)
	}
	add(base) // local branch, tag, or any other ref git can resolve

	for _, c := range candidates {
		if s.git.RefExists(ctx, clonePath, c) {
			return c, nil
		}
	}
	return "", fmt.Errorf("base %q not found on any remote or locally (tried: %s)", base, strings.Join(candidates, ", "))
}

func (s *Service) resolveName(ctx context.Context, clonePath, base string, explicit bool) (string, error) {
	candidate := base
	for i := 2; ; i++ {
		if s.free(ctx, clonePath, candidate) {
			return candidate, nil
		}
		if explicit {
			return "", fmt.Errorf("name %q already exists (worktree/branch/tmux); pick another", base)
		}
		candidate = fmt.Sprintf("%s-%d", base, i)
	}
}

func (s *Service) free(ctx context.Context, clonePath, name string) bool {
	return !s.git.PathExists(worktree.Path(clonePath, name)) &&
		!s.git.BranchExists(ctx, clonePath, name) &&
		!s.tmux.SessionExists("quack/"+name)
}

func successMessage(p prepResult, effort string, ag agent.Agent) string {
	if effort == "" {
		effort = "(default)"
	}
	iso := "worktree branch `" + p.branch + "`"
	if !p.isolated {
		iso = "_(no worktree/isolation)_"
	}
	return fmt.Sprintf("🦆 session **%s** is up\n• dir: `%s`\n• %s\n• agent: `%s` · effort: `%s`\n• attach: `tmux attach -t quack/%s`",
		p.name, p.workdir, iso, ag.Command, effort, p.name)
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// suggestName asks the agent for a short branch name for the prompt, slugified.
// Returns "" on any failure (caller falls back to the default auto-name).
func (s *Service) suggestName(ctx context.Context, agentName, prompt string) string {
	// Prefer the configured naming agent (best summarizer); fall back to the
	// task agent's driver if the namer isn't available.
	namer := orDefault(s.cfg.NameAgent, agentName)
	d, ok := s.drivers[namer]
	if !ok {
		d, ok = s.drivers[agentName]
	}
	if !ok {
		return ""
	}
	nctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	raw, err := d.SuggestName(nctx, prompt)
	if err != nil {
		return ""
	}
	if slug := worktree.Slugify(raw); slug != "session" { // "session" = Slugify's empty sentinel
		return slug
	}
	return ""
}

type keyedMutex struct {
	mu sync.Mutex
	m  map[string]*sync.Mutex
}

func (k *keyedMutex) lock(key string) func() {
	k.mu.Lock()
	if k.m == nil {
		k.m = map[string]*sync.Mutex{}
	}
	mu, ok := k.m[key]
	if !ok {
		mu = &sync.Mutex{}
		k.m[key] = mu
	}
	k.mu.Unlock()
	mu.Lock()
	return mu.Unlock
}
