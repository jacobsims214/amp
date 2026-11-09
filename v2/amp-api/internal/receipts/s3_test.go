package receipts

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
)

func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestConfigFromEnv(t *testing.T) {
	// Unset: local, as before.
	cfg, err := ConfigFromEnv(envMap(nil))
	if err != nil || cfg.Backend != "" || cfg.S3 != (S3Config{}) {
		t.Fatalf("default: %+v %v", cfg, err)
	}
	cfg, err = ConfigFromEnv(envMap(map[string]string{"RECEIPT_STORE": "local", "RECEIPT_DIR": "/d", "RECEIPT_S3_USE_SSL": "nonsense"}))
	if err != nil || cfg.Backend != "local" || cfg.Dir != "/d" {
		t.Fatalf("local ignores the s3 settings: %+v %v", cfg, err)
	}
	// s3 defaults: region us-east-1, TLS on, path-style on.
	cfg, err = ConfigFromEnv(envMap(map[string]string{
		"RECEIPT_STORE": " S3 ", "RECEIPT_S3_ENDPOINT": "s3.example:443", "RECEIPT_S3_BUCKET": "ev",
		"RECEIPT_S3_ACCESS_KEY": "ak", "RECEIPT_S3_SECRET_KEY": "sk",
	}))
	want := S3Config{Endpoint: "s3.example:443", Bucket: "ev", AccessKey: "ak", SecretKey: "sk", Region: "us-east-1", UseSSL: true, PathStyle: true}
	if err != nil || cfg.Backend != "s3" || cfg.S3 != want {
		t.Fatalf("s3 defaults: %+v %v", cfg, err)
	}
	cfg, err = ConfigFromEnv(envMap(map[string]string{
		"RECEIPT_STORE": "s3", "RECEIPT_S3_REGION": "auto", "RECEIPT_S3_USE_SSL": "false", "RECEIPT_S3_PATH_STYLE": "0",
		"RECEIPT_S3_SPOOL_DIR": "/spool",
	}))
	if err != nil || cfg.S3.Region != "auto" || cfg.S3.UseSSL || cfg.S3.PathStyle || cfg.S3.SpoolDir != "/spool" {
		t.Fatalf("s3 overrides: %+v %v", cfg, err)
	}
	for _, name := range []string{"RECEIPT_S3_USE_SSL", "RECEIPT_S3_PATH_STYLE"} {
		if _, err := ConfigFromEnv(envMap(map[string]string{"RECEIPT_STORE": "s3", name: "yes please"})); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s malformed: %v", name, err)
		}
	}
}

func TestNewS3_Validation(t *testing.T) {
	good := S3Config{Endpoint: "seaweedfs:8333", Bucket: "ev", AccessKey: "ak", SecretKey: "secret-value", SpoolDir: t.TempDir()}
	for name, tc := range map[string]struct {
		mod  func(*S3Config)
		want string
	}{
		"no endpoint":   {func(c *S3Config) { c.Endpoint = " " }, "RECEIPT_S3_ENDPOINT"},
		"no bucket":     {func(c *S3Config) { c.Bucket = "" }, "RECEIPT_S3_BUCKET"},
		"no keys":       {func(c *S3Config) { c.AccessKey, c.SecretKey = "", "" }, "RECEIPT_S3_ACCESS_KEY, RECEIPT_S3_SECRET_KEY"},
		"path":          {func(c *S3Config) { c.Endpoint = "https://s3.example/bucket" }, "no path"},
		"host path":     {func(c *S3Config) { c.Endpoint = "s3.example/bucket" }, "host:port"},
		"scheme":        {func(c *S3Config) { c.Endpoint = "ftp://s3.example" }, "http or https"},
		"userinfo":      {func(c *S3Config) { c.Endpoint = "https://u:p@s3.example" }, "credentials"},
		"spool missing": {func(c *S3Config) { c.SpoolDir = "/dev/null/x" }, "spool directory"},
	} {
		cfg := good
		tc.mod(&cfg)
		_, err := New(Config{Backend: "s3", S3: cfg})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
		if err != nil && strings.Contains(err.Error(), "secret-value") {
			t.Errorf("%s: the secret key is in the error: %v", name, err)
		}
	}
	if _, err := New(Config{Backend: "gcs"}); err == nil || !strings.Contains(err.Error(), "local or s3") {
		t.Errorf("unknown backend: %v", err)
	}
	// A URL decides TLS; host:port uses UseSSL.
	for ep, secure := range map[string]bool{"http://seaweedfs:8333": false, "https://acct.r2.cloudflarestorage.com/": true, "seaweedfs:8333": false} {
		cfg := good
		cfg.Endpoint, cfg.UseSSL = ep, false
		st, err := New(Config{Backend: "s3", S3: cfg})
		if err != nil {
			t.Fatalf("%s: %v", ep, err)
		}
		s := st.(*S3)
		if got := strings.HasPrefix(s.Endpoint(), "https://"); got != secure {
			t.Errorf("%s: endpoint %s, want https %v", ep, s.Endpoint(), secure)
		}
		if strings.Contains(s.Endpoint(), "secret-value") || s.Bucket() != "ev" {
			t.Errorf("%s: endpoint %s bucket %s", ep, s.Endpoint(), s.Bucket())
		}
	}
}

// Error classification: a missing bucket is never a missing object (the
// sweep would take evidence for deleted), and wrapped errors are seen.
func TestS3_ErrorKinds(t *testing.T) {
	wrap := func(r minio.ErrorResponse) error { return fmt.Errorf("receipt store s3: %w", r) }
	for name, tc := range map[string]struct {
		err            error
		notFound, auth bool
	}{
		"no such key":     {wrap(minio.ErrorResponse{Code: "NoSuchKey", StatusCode: 404}), true, false},
		"bare 404":        {wrap(minio.ErrorResponse{StatusCode: 404}), true, false},
		"no such bucket":  {wrap(minio.ErrorResponse{Code: "NoSuchBucket", StatusCode: 404}), false, false},
		"access denied":   {wrap(minio.ErrorResponse{Code: "AccessDenied", StatusCode: 403}), false, true},
		"bad key id":      {minio.ErrorResponse{Code: "InvalidAccessKeyId", StatusCode: 403}, false, true},
		"internal":        {wrap(minio.ErrorResponse{Code: "InternalError", StatusCode: 500}), false, false},
		"not an s3 error": {errors.New("dial tcp: connection refused"), false, false},
	} {
		if got := notFound(tc.err); got != tc.notFound {
			t.Errorf("%s: notFound %v, want %v", name, got, tc.notFound)
		}
		if got := AuthError(tc.err); got != tc.auth {
			t.Errorf("%s: AuthError %v, want %v", name, got, tc.auth)
		}
	}
}

func TestS3_CleanTemp(t *testing.T) {
	dir := t.TempDir()
	st, err := NewS3(S3Config{Endpoint: "127.0.0.1:1", Bucket: "ev", AccessKey: "a", SecretKey: "s", SpoolDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(dir, "amp-receipt-spool-old")
	fresh := filepath.Join(dir, "amp-receipt-spool-fresh")
	other := filepath.Join(dir, "someone-elses-file")
	for _, p := range []string{old, fresh, other} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-2 * time.Hour)
	for _, p := range []string{old, other} {
		if err := os.Chtimes(p, past, past); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := st.CleanTemp(time.Hour); err != nil || n != 1 {
		t.Fatalf("clean: %d %v", n, err)
	}
	for p, keep := range map[string]bool{old: false, fresh: true, other: true} {
		if _, err := os.Stat(p); (err == nil) != keep {
			t.Errorf("%s: kept %v, want %v", filepath.Base(p), err == nil, keep)
		}
	}
}

// TestS3_Contract runs the store contract against a real S3-compatible
// store, when RECEIPT_S3_TEST_ENDPOINT is set (with _BUCKET, _ACCESS_KEY,
// _SECRET_KEY; _USE_SSL defaults to false). The bucket is created if
// missing. For example, with SeaweedFS (`brew install seaweedfs`):
//
//	weed server -dir=/tmp/sw -s3 -s3.port=8333 -s3.config=s3.json
//	RECEIPT_S3_TEST_ENDPOINT=127.0.0.1:8333 RECEIPT_S3_TEST_BUCKET=amp-test \
//	  RECEIPT_S3_TEST_ACCESS_KEY=... RECEIPT_S3_TEST_SECRET_KEY=... go test ./internal/receipts/ -run S3
func TestS3_Contract(t *testing.T) {
	ep := os.Getenv("RECEIPT_S3_TEST_ENDPOINT")
	if ep == "" {
		t.Skip("RECEIPT_S3_TEST_ENDPOINT not set")
	}
	st, err := NewS3(S3Config{
		Endpoint: ep, Bucket: os.Getenv("RECEIPT_S3_TEST_BUCKET"),
		AccessKey: os.Getenv("RECEIPT_S3_TEST_ACCESS_KEY"), SecretKey: os.Getenv("RECEIPT_S3_TEST_SECRET_KEY"),
		UseSSL: os.Getenv("RECEIPT_S3_TEST_USE_SSL") == "true", PathStyle: true, SpoolDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := st.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	storeContract(t, st, fmt.Sprintf("test-%d", time.Now().UnixNano()))
	// A Put removes its spool file whatever happened.
	if left, _ := filepath.Glob(filepath.Join(st.spool, spoolPattern)); len(left) != 0 {
		t.Fatalf("spool files left: %v", left)
	}

	// The If-None-Match: * path: an object that appears between the check
	// and the write (placed here with the raw client, behind Put's back)
	// still ends in ErrExists, and isn't overwritten.
	raceKey := fmt.Sprintf("test-race-%d/1/2/attempts/3/abc123/video.mp4", time.Now().UnixNano())
	if _, err := st.c.PutObject(ctx, st.bucket, raceKey, bytes.NewReader([]byte("first")), 5, minio.PutObjectOptions{}); err != nil {
		t.Fatal(err)
	}
	opts := minio.PutObjectOptions{DisableMultipart: true}
	opts.SetMatchETagExcept("*")
	_, err = st.c.PutObject(ctx, st.bucket, raceKey, bytes.NewReader([]byte("second")), 6, opts)
	if r := s3Error(err); r.Code != "PreconditionFailed" && r.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("conditional overwrite: %v (the store ignores If-None-Match)", err)
	}
	if b := served(t, st, raceKey, ""); string(b) != "first" {
		t.Fatalf("overwritten: %q", b)
	}
	_ = st.Delete(ctx, raceKey)

	t.Run("bucket gone", func(t *testing.T) {
		// A bucket that goes away while amp-api runs: Delete says so (not "already
		// gone", or the sweep would take the evidence for deleted).
		gone, err := NewS3(S3Config{Endpoint: ep, Bucket: os.Getenv("RECEIPT_S3_TEST_BUCKET") + "-gone",
			AccessKey: os.Getenv("RECEIPT_S3_TEST_ACCESS_KEY"), SecretKey: os.Getenv("RECEIPT_S3_TEST_SECRET_KEY"),
			UseSSL: os.Getenv("RECEIPT_S3_TEST_USE_SSL") == "true", PathStyle: true, SpoolDir: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		if err := gone.Ready(ctx); err != nil {
			if AuthError(err) {
				t.Logf("bucket-gone check skipped: the key can't create another bucket (%v)", err)
				return
			}
			t.Fatal(err)
		}
		if err := gone.c.RemoveBucket(ctx, gone.bucket); err != nil {
			t.Fatal(err)
		}
		if err := gone.Delete(ctx, "1/2/attempts/3/abc123/video.mp4"); err == nil {
			t.Fatal("delete in a missing bucket: no error")
		}
		// S3 and R2 refuse the first Put (NoSuchBucket, which makes the next
		// Put recreate the bucket); SeaweedFS recreates it on the PUT itself.
		// Either way the store works again by the second Put.
		gk := "1/2/attempts/3/abc123/video.mp4"
		if _, _, err := gone.Put(ctx, gk, "video/mp4", strings.NewReader("x"), 10); err != nil {
			if _, _, err := gone.Put(ctx, gk, "video/mp4", strings.NewReader("x"), 10); err != nil {
				t.Fatalf("put after the bucket went away: %v, want it recreated", err)
			}
		}
		if b := served(t, gone, gk, ""); string(b) != "x" {
			t.Fatalf("after recreating: %q", b)
		}
		_ = gone.Delete(ctx, gk)
		_ = gone.c.RemoveBucket(ctx, gone.bucket)
	})

	// Wrong credentials are an AuthError (amp-api then leaves the store out).
	bad, err := NewS3(S3Config{Endpoint: ep, Bucket: os.Getenv("RECEIPT_S3_TEST_BUCKET"), AccessKey: "nobody", SecretKey: "wrong",
		UseSSL: os.Getenv("RECEIPT_S3_TEST_USE_SSL") == "true", PathStyle: true, SpoolDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := bad.Ready(ctx); err == nil || !AuthError(err) {
		t.Fatalf("bad credentials: %v, want an AuthError", err)
	}
	// Nothing listening: an error, but not an AuthError (retried later).
	down, _ := NewS3(S3Config{Endpoint: "127.0.0.1:1", Bucket: "ev", AccessKey: "a", SecretKey: "s", PathStyle: true, SpoolDir: t.TempDir()})
	if err := down.Ready(ctx); err == nil || AuthError(err) {
		t.Fatalf("store down: %v", err)
	}
}
