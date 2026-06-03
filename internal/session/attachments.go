package session

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"time"
)

// Attachment is a Discord message attachment (e.g. a screenshot the user
// dropped on the command). quack mirrors it to local disk so the agent can open
// it — agents read images from files, not from ephemeral Discord CDN URLs.
type Attachment struct {
	Filename    string
	URL         string
	ContentType string
}

// maxAttachmentBytes caps a single download so a hostile/huge URL can't blow up
// memory. Discord's own attachment limit is well below this.
const maxAttachmentBytes = 50 << 20 // 50 MiB

// saveAttachments downloads each attachment into the session's state dir and
// returns a <quack-attachments> prompt block pointing the agent at the local
// copies by absolute path. It returns "" when there are no attachments. A
// single download/save failure is noted in the block rather than failing the
// whole turn — the rest of the prompt still goes through.
func (s *Service) saveAttachments(ctx context.Context, sessName string, atts []Attachment) string {
	if len(atts) == 0 {
		return ""
	}
	dir := filepath.Join(s.cfg.StateDir, "sessions", sessName, "attachments")
	_ = s.mkdirAll(dir, 0o755)

	used := map[string]bool{}
	var lines []string
	for _, a := range atts {
		path := filepath.Join(dir, uniqueName(safeFilename(a.Filename), used))
		data, err := s.fetchURL(ctx, a.URL)
		if err != nil {
			lines = append(lines, fmt.Sprintf("- %s (download failed: %v)", a.Filename, err))
			continue
		}
		if err := s.writeFile(path, data, 0o644); err != nil {
			lines = append(lines, fmt.Sprintf("- %s (save failed: %v)", a.Filename, err))
			continue
		}
		ct := a.ContentType
		if ct == "" {
			ct = "unknown type"
		}
		lines = append(lines, fmt.Sprintf("- %s (%s)", path, ct))
	}

	return "<quack-attachments>\n" +
		"The user attached these files; quack saved local copies you can open (e.g. with the Read tool):\n" +
		strings.Join(lines, "\n") + "\n</quack-attachments>"
}

// safeFilename reduces a Discord-supplied filename to a single safe path element.
func safeFilename(name string) string {
	name = filepath.Base(strings.TrimSpace(name))
	name = strings.ReplaceAll(name, "\\", "_")
	if name == "" || name == "." || name == ".." {
		return "attachment"
	}
	return name
}

// uniqueName returns name, or name with a "-2"/"-3"/… suffix (before the
// extension) if it has already been used, recording the result in used.
func uniqueName(name string, used map[string]bool) string {
	if !used[name] {
		used[name] = true
		return name
	}
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 2; ; i++ {
		cand := fmt.Sprintf("%s-%d%s", stem, i, ext)
		if !used[cand] {
			used[cand] = true
			return cand
		}
	}
}

// httpFetch is the default attachment downloader: a plain GET with a timeout and
// a size ceiling. Discord attachment URLs are pre-signed, so no auth is needed.
func httpFetch(ctx context.Context, url string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxAttachmentBytes))
}
