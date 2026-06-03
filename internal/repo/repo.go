// Package repo classifies and resolves quack target tokens (repo refs / paths).
package repo

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Ref is a parsed git repository reference.
type Ref struct {
	Host  string
	Owner string
	Repo  string
}

// IsPath reports whether token should be treated as a filesystem path rather
// than a repo reference.
func IsPath(token string) bool {
	return strings.HasPrefix(token, "/") ||
		strings.HasPrefix(token, "~") ||
		strings.HasPrefix(token, ".")
}

// ParseRef parses owner/repo, host/owner/repo, https URLs, and scp-style ssh URLs.
func ParseRef(token string) (Ref, error) {
	s := strings.TrimSuffix(token, ".git")

	if rest, ok := strings.CutPrefix(s, "git@"); ok {
		host, path, ok := strings.Cut(rest, ":")
		if !ok {
			return Ref{}, fmt.Errorf("invalid ssh ref %q", token)
		}
		owner, repoName, ok := strings.Cut(path, "/")
		if !ok || owner == "" || repoName == "" || strings.Contains(repoName, "/") {
			return Ref{}, fmt.Errorf("invalid ssh ref %q", token)
		}
		return Ref{Host: host, Owner: owner, Repo: repoName}, nil
	}

	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "http://")

	parts := strings.Split(s, "/")
	switch len(parts) {
	case 2:
		if parts[0] == "" || parts[1] == "" {
			return Ref{}, fmt.Errorf("cannot parse repo ref %q", token)
		}
		return Ref{Host: "github.com", Owner: parts[0], Repo: parts[1]}, nil
	case 3:
		if parts[0] == "" || parts[1] == "" || parts[2] == "" {
			return Ref{}, fmt.Errorf("cannot parse repo ref %q", token)
		}
		return Ref{Host: parts[0], Owner: parts[1], Repo: parts[2]}, nil
	default:
		return Ref{}, fmt.Errorf("cannot parse repo ref %q", token)
	}
}

// ClonePath returns the on-disk clone location: root/host/owner/repo.
func (r Ref) ClonePath(devSrcRoot string) string {
	return filepath.Join(devSrcRoot, r.Host, r.Owner, r.Repo)
}

// CloneURL builds the clone URL for the given protocol ("ssh" or "https").
func (r Ref) CloneURL(protocol string) string {
	if protocol == "https" {
		return fmt.Sprintf("https://%s/%s/%s.git", r.Host, r.Owner, r.Repo)
	}
	return fmt.Sprintf("git@%s:%s/%s.git", r.Host, r.Owner, r.Repo)
}
