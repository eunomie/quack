package session

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

type fakeGit struct {
	existing   map[string]bool
	repos      map[string]bool
	primary    map[string]string
	branches   map[string]bool
	pathExists map[string]bool
	refs       map[string]bool // ref string -> resolvable (nil = single-remote default)
	remotes    []string        // nil = default ["origin"]

	cloned    []string
	fetched   []string
	worktrees []string

	defaultBranchErr error
}

func newFakeGit() *fakeGit {
	return &fakeGit{
		existing:   map[string]bool{},
		repos:      map[string]bool{},
		primary:    map[string]string{},
		branches:   map[string]bool{},
		pathExists: map[string]bool{},
	}
}

func (f *fakeGit) IsRepo(p string) bool { return f.repos[p] }
func (f *fakeGit) PrimaryClone(p string) (string, error) {
	if c, ok := f.primary[p]; ok {
		return c, nil
	}
	return p, nil
}
func (f *fakeGit) Exists(clonePath string) bool { return f.existing[clonePath] }
func (f *fakeGit) PathExists(p string) bool     { return f.pathExists[p] }
func (f *fakeGit) Clone(ctx context.Context, url, clonePath string) error {
	f.cloned = append(f.cloned, url+" -> "+clonePath)
	f.existing[clonePath] = true
	return nil
}
func (f *fakeGit) Fetch(ctx context.Context, clonePath string) error {
	f.fetched = append(f.fetched, clonePath)
	return nil
}
func (f *fakeGit) DefaultBranch(ctx context.Context, clonePath string) (string, error) {
	if f.defaultBranchErr != nil {
		return "", f.defaultBranchErr
	}
	return "main", nil
}
func (f *fakeGit) BranchExists(ctx context.Context, clonePath, branch string) bool {
	return f.branches[clonePath+"\x00"+branch]
}
func (f *fakeGit) RefExists(ctx context.Context, clonePath, ref string) bool {
	if f.refs == nil {
		// Default single-remote repo: origin carries every ref.
		return strings.HasPrefix(ref, "origin/")
	}
	return f.refs[ref]
}
func (f *fakeGit) Remotes(ctx context.Context, clonePath string) []string {
	if f.remotes == nil {
		return []string{"origin"}
	}
	return f.remotes
}
func (f *fakeGit) AddWorktree(ctx context.Context, clonePath, wtPath, branch, baseRef string) error {
	f.worktrees = append(f.worktrees, fmt.Sprintf("%s|%s|%s|%s", clonePath, wtPath, branch, baseRef))
	f.pathExists[wtPath] = true
	f.branches[clonePath+"\x00"+branch] = true
	return nil
}

type fakeTmux struct {
	sessions map[string]bool
	created  []NewSessionOpts
}

func newFakeTmux() *fakeTmux { return &fakeTmux{sessions: map[string]bool{}} }

func (f *fakeTmux) SessionExists(name string) bool { return f.sessions[name] }
func (f *fakeTmux) NewSession(ctx context.Context, o NewSessionOpts) error {
	f.created = append(f.created, o)
	f.sessions[o.Name] = true
	return nil
}

type postedMsg struct {
	channel, content string
	silent           bool
}

type fakeReplier struct {
	threadID  string
	openErr   error
	threads   []string
	posts     []postedMsg
	edits     []postedMsg
	deletes   []string // "channel|message"
	renames   []string
	archived  []string // threadIDs closed via ArchiveThread
	reacts    []string // "channel|message|emoji"
	unreacts  []string // "channel|message|emoji"
	nextID    int
	recent    []Message // returned by RecentMessages
	recentErr error
}

func newFakeReplier() *fakeReplier { return &fakeReplier{threadID: "thread-1"} }

func (f *fakeReplier) OpenThread(ctx context.Context, channelID, messageID, name string, autoArchiveMin int) (string, error) {
	if f.openErr != nil {
		return "", f.openErr
	}
	f.threads = append(f.threads, channelID+"|"+messageID+"|"+name)
	return f.threadID, nil
}
func (f *fakeReplier) Post(ctx context.Context, channelID, content string) (string, error) {
	f.nextID++
	f.posts = append(f.posts, postedMsg{channel: channelID, content: content})
	return fmt.Sprintf("msg-%d", f.nextID), nil
}
func (f *fakeReplier) PostSilent(ctx context.Context, channelID, content string) (string, error) {
	f.nextID++
	f.posts = append(f.posts, postedMsg{channel: channelID, content: content, silent: true})
	return fmt.Sprintf("msg-%d", f.nextID), nil
}
func (f *fakeReplier) Edit(ctx context.Context, channelID, messageID, content string) error {
	f.edits = append(f.edits, postedMsg{channel: messageID, content: content})
	return nil
}
func (f *fakeReplier) Delete(ctx context.Context, channelID, messageID string) error {
	f.deletes = append(f.deletes, channelID+"|"+messageID)
	return nil
}
func (f *fakeReplier) RenameThread(ctx context.Context, threadID, name string) error {
	f.renames = append(f.renames, threadID+"|"+name)
	return nil
}
func (f *fakeReplier) ArchiveThread(ctx context.Context, threadID string) error {
	f.archived = append(f.archived, threadID)
	return nil
}
func (f *fakeReplier) React(ctx context.Context, channelID, messageID, emoji string) error {
	f.reacts = append(f.reacts, channelID+"|"+messageID+"|"+emoji)
	return nil
}
func (f *fakeReplier) Unreact(ctx context.Context, channelID, messageID, emoji string) error {
	f.unreacts = append(f.unreacts, channelID+"|"+messageID+"|"+emoji)
	return nil
}

func (f *fakeReplier) RecentMessages(ctx context.Context, channelID, beforeID string, limit int) ([]Message, error) {
	return f.recent, f.recentErr
}

type memFS struct{ files map[string][]byte }

func newMemFS() *memFS                         { return &memFS{files: map[string][]byte{}} }
func (m *memFS) mkdirAll(string, uint32) error { return nil }
func (m *memFS) writeFile(path string, data []byte, _ uint32) error {
	m.files[path] = data
	return nil
}
func (m *memFS) remove(path string) error {
	delete(m.files, path)
	return nil
}
func (m *memFS) readFile(path string) ([]byte, error) {
	if d, ok := m.files[path]; ok {
		return d, nil
	}
	return nil, fmt.Errorf("not found: %s", path)
}

type fakeRunner struct {
	dir  string
	argv []string
	out  []byte
	err  error
	runs int
}

func (f *fakeRunner) Run(ctx context.Context, dir string, argv []string) ([]byte, error) {
	f.runs++
	f.dir = dir
	f.argv = argv
	return f.out, f.err
}

// readDir returns the immediate subdirectory names under path (those that hold
// at least one file), sorted for deterministic test ordering.
func (m *memFS) readDir(path string) ([]string, error) {
	prefix := strings.TrimSuffix(path, "/") + "/"
	seen := map[string]bool{}
	var names []string
	for f := range m.files {
		rest, ok := strings.CutPrefix(f, prefix)
		if !ok {
			continue
		}
		i := strings.Index(rest, "/")
		if i < 0 {
			continue
		}
		if name := rest[:i]; !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names, nil
}
