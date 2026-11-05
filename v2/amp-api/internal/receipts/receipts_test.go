package receipts

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func newLocal(t *testing.T) *Local {
	t.Helper()
	l, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// storeContract is the Store contract every backend keeps (store.go): Put
// hashes and writes once, refuses past max storing nothing, Serve answers
// Range with 206 and leaves Content-Type alone, a missing key is
// ErrNotFound, Delete is idempotent. prefix keeps runs against a shared
// bucket apart.
func storeContract(t *testing.T, s Store, prefix string) {
	t.Helper()
	ctx := context.Background()
	k := func(rest string) string { return prefix + "/" + rest }

	t.Run("put once", func(t *testing.T) {
		body := []byte("frames of a video")
		key := k("1/2/attempts/3/abcdef012345/video.mp4")
		got, n, err := s.Put(ctx, key, "video/mp4", bytes.NewReader(body), 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		if got != sum(body) || n != int64(len(body)) {
			t.Fatalf("put: %s %d, want %s %d", got, n, sum(body), len(body))
		}
		if _, _, err := s.Put(ctx, key, "video/mp4", strings.NewReader("other"), 1<<20); !errors.Is(err, ErrExists) {
			t.Fatalf("second put: %v, want ErrExists", err)
		}
		if b := served(t, s, key, ""); !bytes.Equal(b, body) {
			t.Fatalf("stored bytes: %q", b)
		}
		if err := s.Delete(ctx, key); err != nil {
			t.Fatal(err)
		}
		if err := s.Delete(ctx, key); err != nil {
			t.Fatalf("deleting a missing key: %v", err)
		}
		if err := serveErr(s, key); !errors.Is(err, ErrNotFound) {
			t.Fatalf("serve after delete: %v, want ErrNotFound", err)
		}
		if _, _, err := s.Put(ctx, key, "video/mp4", bytes.NewReader(body), 1<<20); err != nil {
			t.Fatalf("put after delete: %v", err)
		}
		_ = s.Delete(ctx, key)
	})

	t.Run("too large stores nothing", func(t *testing.T) {
		key := k("1/2/attempts/3/abc123/console.txt")
		if _, _, err := s.Put(ctx, key, "text/plain", strings.NewReader("12345678901"), 10); !errors.Is(err, ErrTooLarge) {
			t.Fatalf("put: %v, want ErrTooLarge", err)
		}
		if err := serveErr(s, key); !errors.Is(err, ErrNotFound) {
			t.Fatalf("a refused put stored something: %v", err)
		}
		// Exactly max is fine.
		if _, n, err := s.Put(ctx, key, "text/plain", strings.NewReader("1234567890"), 10); err != nil || n != 10 {
			t.Fatalf("put at max: %d %v", n, err)
		}
		_ = s.Delete(ctx, key)
	})

	t.Run("serve range", func(t *testing.T) {
		body := []byte("0123456789abcdefghij")
		key := k("1/2/attempts/3/abc123/video.mp4")
		if _, _, err := s.Put(ctx, key, "video/mp4", bytes.NewReader(body), 1<<20); err != nil {
			t.Fatal(err)
		}
		defer s.Delete(ctx, key)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req.Header.Set("Range", "bytes=5-9")
		rec.Header().Set("Content-Type", "video/mp4")
		if err := s.Serve(rec, req, key); err != nil {
			t.Fatal(err)
		}
		res := rec.Result()
		got, _ := io.ReadAll(res.Body)
		if res.StatusCode != http.StatusPartialContent || string(got) != "56789" || res.Header.Get("Content-Range") != "bytes 5-9/20" {
			t.Fatalf("range: %d %q %q", res.StatusCode, got, res.Header.Get("Content-Range"))
		}
		if res.Header.Get("Content-Type") != "video/mp4" {
			t.Fatalf("content type: %q", res.Header.Get("Content-Type"))
		}
		// An open-ended range (what a video element asks for) and the tail.
		if b := served(t, s, key, "bytes=15-"); string(b) != "fghij" {
			t.Fatalf("open range: %q", b)
		}
		if b := served(t, s, key, "bytes=-3"); string(b) != "hij" {
			t.Fatalf("suffix range: %q", b)
		}
	})

	t.Run("not found", func(t *testing.T) {
		if err := serveErr(s, k("1/2/attempts/3/abc123/poster.gif")); !errors.Is(err, ErrNotFound) {
			t.Fatalf("missing: %v", err)
		}
		if err := s.Delete(ctx, k("1/2/attempts/3/abc123/never.txt")); err != nil {
			t.Fatalf("deleting a key never stored: %v", err)
		}
	})

	t.Run("bad keys", func(t *testing.T) {
		for _, key := range []string{"", "/1/2", "1/../2", `1\2`} {
			if _, _, err := s.Put(ctx, key, "text/plain", strings.NewReader("x"), 10); err == nil {
				t.Errorf("put %q: accepted", key)
			}
		}
	})
}

// served is the body Serve writes for key (with a Range header when rng
// isn't empty); it fails the test on an error.
func served(t *testing.T, s Store, key, rng string) []byte {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	if rng != "" {
		req.Header.Set("Range", rng)
	}
	if err := s.Serve(rec, req, key); err != nil {
		t.Fatalf("serve %s: %v", key, err)
	}
	b, _ := io.ReadAll(rec.Result().Body)
	return b
}

func serveErr(s Store, key string) error {
	return s.Serve(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil), key)
}

func TestLocal_Contract(t *testing.T) {
	l := newLocal(t)
	storeContract(t, l, "p")
	// The temporary files are gone.
	if left, _ := os.ReadDir(filepath.Join(l.Root(), ".tmp")); len(left) != 0 {
		t.Fatalf("temporary files left: %v", left)
	}
}

func TestLocal_StoresOnDisk(t *testing.T) {
	l := newLocal(t)
	body := []byte("frames of a video")
	key := "1/2/attempts/3/abcdef012345/video.mp4"
	if _, _, err := l.Put(context.Background(), key, "video/mp4", bytes.NewReader(body), 1<<20); err != nil {
		t.Fatal(err)
	}
	on, err := os.ReadFile(filepath.Join(l.Root(), filepath.FromSlash(key)))
	if err != nil || !bytes.Equal(on, body) {
		t.Fatalf("stored bytes: %q %v", on, err)
	}
	tooBig := "1/2/attempts/3/abc123/console.txt"
	if _, _, err := l.Put(context.Background(), tooBig, "text/plain", strings.NewReader("12345678901"), 10); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("put: %v, want ErrTooLarge", err)
	}
	if _, err := os.Stat(filepath.Join(l.Root(), filepath.FromSlash(tooBig))); !os.IsNotExist(err) {
		t.Fatalf("a refused put stored something: %v", err)
	}
}

func TestValidateKey(t *testing.T) {
	good := []string{"1/2/attempts/3/abcdef012345/video.mp4", "a", "a.b/c_d-e"}
	for _, k := range good {
		if err := ValidateKey(k); err != nil {
			t.Errorf("%q: %v", k, err)
		}
	}
	bad := []string{"", "/1/2", "1//2", "1/2/", "1/../2", "..", "1/./2", ".", `1\2`, "1/2/a b", "1/2/%2e%2e", "1/2/é", strings.Repeat("a", MaxKeyBytes+1)}
	for _, k := range bad {
		if err := ValidateKey(k); err == nil {
			t.Errorf("%q: accepted", k)
		}
	}
	l := newLocal(t)
	for _, k := range append(bad, ".tmp/x") {
		if _, _, err := l.Put(context.Background(), k, "text/plain", strings.NewReader("x"), 10); err == nil {
			t.Errorf("put %q: accepted", k)
		}
	}
}

func TestKeyAndNames(t *testing.T) {
	if k := Key(7, 9, 11, "ABCDEF0123456789", "video.mp4"); k != "7/9/attempts/11/abcdef012345/video.mp4" || ValidateKey(k) != nil {
		t.Fatalf("key: %q", k)
	}
	if KeyProject("7/9/x") != 7 || KeyProject("x/9") != 0 {
		t.Fatal("KeyProject")
	}
	for name, want := range map[string]string{"video.mp4": "video/mp4", "screenshot-3.png": "image/png", "console.txt": "text/plain; charset=utf-8", "trace.zip": "application/zip"} {
		if got, ok := ContentType(name); !ok || got != want {
			t.Errorf("%s: %q %v", name, got, ok)
		}
	}
	for _, name := range []string{"index.html", "x.svg", "screenshot-.png", "screenshot-1.svg", "../video.mp4", "Video.mp4"} {
		if _, ok := ContentType(name); ok {
			t.Errorf("%s: allowed", name)
		}
	}
	tok, hash, err := NewUploadToken()
	if err != nil || !ValidTokenShape(tok) || HashToken(tok) != hash || len(hash) != 64 {
		t.Fatalf("token: %q %q %v", tok, hash, err)
	}
	if ValidTokenShape("short") || ValidTokenShape(strings.Repeat("!", 43)) {
		t.Fatal("token shape")
	}
}

func TestLocal_CleanTemp(t *testing.T) {
	l := newLocal(t)
	old := filepath.Join(l.Root(), ".tmp", "put-old")
	fresh := filepath.Join(l.Root(), ".tmp", "put-fresh")
	for _, p := range []string{old, fresh} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	if n, err := l.CleanTemp(time.Hour); err != nil || n != 1 {
		t.Fatalf("clean: %d %v", n, err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("old temp file left")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatal("fresh temp file removed")
	}
}
