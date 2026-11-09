package receipts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// Local is the directory store: one file per key under its root. Writes go
// to a temporary file first, hashed as they are written, and are linked into
// place with os.Link, which is atomic and fails if the key exists: a key is
// written once, on NFS too.
type Local struct {
	root string
}

// NewLocal opens (creating it if needed) a local store rooted at dir.
func NewLocal(dir string) (*Local, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("receipt store %q: %w", dir, err)
	}
	if err := os.MkdirAll(filepath.Join(abs, ".tmp"), 0o750); err != nil {
		return nil, fmt.Errorf("receipt store %q: %w", abs, err)
	}
	// Writable? (A read-only volume would only fail at the first upload.)
	probe, err := os.CreateTemp(filepath.Join(abs, ".tmp"), "probe-*")
	if err != nil {
		return nil, fmt.Errorf("receipt store %q isn't writable: %w", abs, err)
	}
	probe.Close()
	_ = os.Remove(probe.Name())
	return &Local{root: abs}, nil
}

// Root is the store's directory.
func (l *Local) Root() string { return l.root }

// path is key's file, after ValidateKey.
func (l *Local) path(key string) (string, error) {
	if err := ValidateKey(key); err != nil {
		return "", err
	}
	if strings.HasPrefix(key, ".tmp/") || key == ".tmp" {
		return "", fmt.Errorf("evidence key %q is reserved", key)
	}
	p := filepath.Join(l.root, filepath.FromSlash(path.Clean(key)))
	if !strings.HasPrefix(p, l.root+string(filepath.Separator)) {
		return "", fmt.Errorf("evidence key %q leaves the store", key)
	}
	return p, nil
}

func (l *Local) Put(ctx context.Context, key, contentType string, r io.Reader, max int64) (string, int64, error) {
	dst, err := l.path(key)
	if err != nil {
		return "", 0, err
	}
	if _, err := os.Lstat(dst); err == nil {
		return "", 0, ErrExists
	}
	tmp, err := os.CreateTemp(filepath.Join(l.root, ".tmp"), "put-*")
	if err != nil {
		return "", 0, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // after the link, only the temporary name goes
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), &ctxReader{ctx: ctx, r: io.LimitReader(r, max+1)})
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", n, err
	}
	if n > max {
		return "", n, ErrTooLarge
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return "", n, err
	}
	if err := os.Chmod(tmpName, 0o640); err != nil {
		return "", n, err
	}
	if err := os.Link(tmpName, dst); err != nil {
		if errors.Is(err, os.ErrExist) {
			return "", n, ErrExists
		}
		return "", n, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func (l *Local) Serve(w http.ResponseWriter, r *http.Request, key string) error {
	p, err := l.path(key)
	if err != nil {
		return err
	}
	f, err := os.Open(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ErrNotFound
		}
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return ErrNotFound
	}
	// The caller set Content-Type, so ServeContent never sniffs; name is
	// only used for that sniffing, so it is left empty.
	http.ServeContent(w, r, "", st.ModTime(), f)
	return nil
}

func (l *Local) Delete(ctx context.Context, key string) error {
	p, err := l.path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// CleanTemp removes temporary files older than olderThan: uploads a crash
// or a cut connection left behind (a finished Put removes its own). It
// returns how many it removed.
func (l *Local) CleanTemp(olderThan time.Duration) (int, error) {
	dir := filepath.Join(l.root, ".tmp")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	cutoff := time.Now().Add(-olderThan)
	n := 0
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() || info.ModTime().After(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err == nil {
			n++
		}
	}
	return n, nil
}

// ctxReader stops a copy when ctx ends.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
