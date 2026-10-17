package kb

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
)

// The AMP guide (the global KB) is the markdown under v2/kb in the AMP
// repo. The image copies it in, and every replica syncs it into the
// "kb_global_0" collection on startup. Writes are deterministic upserts
// keyed on path, so replicas syncing at once write the same thing, and a
// replica still running an older image never re-syncs.

// GlobalAuthor is recorded as the author of every guide doc.
const GlobalAuthor = "amp"

// GuideDoc is one markdown file of the guide as read from disk.
type GuideDoc struct {
	Path    string
	Title   string
	Tags    []string
	Content string
}

// SyncResult says what a guide sync changed.
type SyncResult struct {
	Written   []string `json:"written"`
	Unchanged int      `json:"unchanged"`
	Deleted   []string `json:"deleted"`
}

// ReadGuide loads every .md file under dir except README.md (which is for
// people editing the guide), in path order.
func ReadGuide(dir string) ([]GuideDoc, error) {
	var docs []GuideDoc
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(p, ".md") {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "README.md" {
			return nil
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		docs = append(docs, parseGuideDoc(rel, string(raw)))
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(docs, func(i, j int) bool { return docs[i].Path < docs[j].Path })
	return docs, nil
}

// parseGuideDoc reads optional front matter ("title:" and "tags: [a, b]")
// and returns the body without it. With no title it uses the first
// heading, then the file name.
func parseGuideDoc(path, raw string) GuideDoc {
	d := GuideDoc{Path: path, Tags: []string{}}
	body := strings.ReplaceAll(raw, "\r\n", "\n")
	if rest, ok := strings.CutPrefix(body, "---\n"); ok {
		if fm, after, found := strings.Cut(rest, "\n---\n"); found {
			body = after
			for _, line := range strings.Split(fm, "\n") {
				key, val, ok := strings.Cut(line, ":")
				if !ok {
					continue
				}
				val = strings.TrimSpace(val)
				switch strings.TrimSpace(key) {
				case "title":
					d.Title = strings.Trim(val, `"'`)
				case "tags":
					for _, t := range strings.Split(strings.Trim(val, "[]"), ",") {
						if t = strings.Trim(strings.TrimSpace(t), `"'`); t != "" {
							d.Tags = append(d.Tags, t)
						}
					}
				}
			}
		}
	}
	d.Content = strings.TrimLeft(body, "\n")
	if d.Title == "" {
		for _, line := range strings.Split(d.Content, "\n") {
			if h, ok := strings.CutPrefix(line, "# "); ok {
				d.Title = strings.TrimSpace(h)
				break
			}
		}
	}
	if d.Title == "" {
		d.Title = strings.TrimSuffix(filepath.Base(path), ".md")
	}
	return d
}

// SyncGlobal makes the global KB hold exactly the guide under dir: docs
// whose title, tags or content changed are rewritten, and docs no longer
// on disk are removed. Unchanged docs aren't touched, so a restart with
// the same image costs one read per doc.
func (s *Service) SyncGlobal(ctx context.Context, dir string) (*SyncResult, error) {
	docs, err := ReadGuide(dir)
	if err != nil {
		return nil, fmt.Errorf("read guide %s: %w", dir, err)
	}
	if len(docs) == 0 {
		// An empty or missing folder is a packaging mistake, not a request
		// to wipe the guide.
		return nil, fmt.Errorf("no guide docs in %s", dir)
	}
	sc := Global()
	if err := s.ensureCollection(ctx, sc); err != nil {
		return nil, err
	}
	res := &SyncResult{Written: []string{}, Deleted: []string{}}
	keep := map[string]bool{}
	for _, d := range docs {
		keep[d.Path] = true
		if cur, err := s.GetDoc(ctx, sc, d.Path); err == nil &&
			cur.Title == d.Title && cur.Content == d.Content && slices.Equal(cur.Tags, d.Tags) {
			res.Unchanged++
			continue
		}
		if _, err := s.writeDocNow(ctx, sc, d.Path, d.Title, d.Content, GlobalAuthor, d.Tags, "", nil); err != nil {
			return res, fmt.Errorf("write %s: %w", d.Path, err)
		}
		res.Written = append(res.Written, d.Path)
	}
	existing, err := s.ListDocs(ctx, sc, "")
	if err != nil {
		return res, err
	}
	for _, e := range existing {
		if keep[e.Path] {
			continue
		}
		if err := s.deleteDocNow(ctx, sc, e.Path); err != nil {
			return res, fmt.Errorf("delete %s: %w", e.Path, err)
		}
		res.Deleted = append(res.Deleted, e.Path)
	}
	return res, nil
}

// SyncGlobalWithRetry runs SyncGlobal until it succeeds, backing off while
// Typesense comes up, and gives up after about ten minutes or when ctx
// ends. Meant to run in its own goroutine at startup.
func (s *Service) SyncGlobalWithRetry(ctx context.Context, dir string) {
	wait := 2 * time.Second
	deadline := time.Now().Add(10 * time.Minute)
	for {
		res, err := s.SyncGlobal(ctx, dir)
		if err == nil {
			s.log.Info("AMP guide synced", "dir", dir, "written", len(res.Written), "unchanged", res.Unchanged, "deleted", len(res.Deleted))
			return
		}
		if strings.HasPrefix(err.Error(), "no guide docs") || strings.HasPrefix(err.Error(), "read guide") {
			s.log.Warn("AMP guide not synced", "err", err)
			return
		}
		if time.Now().After(deadline) {
			s.log.Error("AMP guide sync gave up", "err", err)
			return
		}
		s.log.Warn("AMP guide sync failed; retrying", "err", err, "in", wait)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if wait < time.Minute {
			wait *= 2
		}
	}
}
