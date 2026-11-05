package receipts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// S3Config configures the S3 backend: any S3-compatible store (AWS S3,
// Cloudflare R2, SeaweedFS, Garage, MinIO).
type S3Config struct {
	// Endpoint is host:port, or a URL (http:// or https://, which then
	// decides UseSSL). No path.
	Endpoint string
	Bucket   string
	// AccessKey and SecretKey sign every request. Never logged.
	AccessKey string
	SecretKey string
	// Region (DefaultS3Region when empty).
	Region string
	// UseSSL: https to the endpoint. False for an in-cluster SeaweedFS.
	UseSSL bool
	// PathStyle: bucket in the path (endpoint/bucket/key) rather than the
	// host name. SeaweedFS, Garage and MinIO need it; S3 and R2 accept it.
	PathStyle bool
	// SpoolDir holds an upload while it is hashed and measured, before it
	// is sent (os.TempDir() when empty).
	SpoolDir string
}

// DefaultS3Region is the region when none is set (SeaweedFS and Garage
// accept any; R2 uses "auto", which also accepts this).
const DefaultS3Region = "us-east-1"

// spoolPattern names S3's temporary files, so CleanTemp only touches its own.
const spoolPattern = "amp-receipt-spool-*"

// S3 keeps evidence as objects in one bucket, named by key. Serving reads
// through amp-api (Serve), never by a presigned URL: the members-only check
// and the safety headers stay in one place, and an in-cluster store needs
// no public host name.
type S3 struct {
	c      *minio.Client
	bucket string
	spool  string

	mu    sync.Mutex
	ready bool // the bucket is known to exist
}

// endpointHost turns Endpoint into minio's host[:port] and whether to use
// TLS (a URL's scheme overrides useSSL).
func endpointHost(endpoint string, useSSL bool) (string, bool, error) {
	ep := strings.TrimSpace(endpoint)
	if ep == "" {
		return "", false, errors.New("receipt store s3: RECEIPT_S3_ENDPOINT is empty")
	}
	if !strings.Contains(ep, "://") {
		if strings.ContainsAny(ep, "/?#@") {
			return "", false, fmt.Errorf("receipt store s3: endpoint %q must be host:port or a URL with no path", ep)
		}
		return ep, useSSL, nil
	}
	u, err := url.Parse(ep)
	if err != nil {
		return "", false, fmt.Errorf("receipt store s3: endpoint %q: %w", ep, err)
	}
	switch u.Scheme {
	case "https":
		useSSL = true
	case "http":
		useSSL = false
	default:
		return "", false, fmt.Errorf("receipt store s3: endpoint %q: scheme must be http or https", ep)
	}
	if u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return "", false, fmt.Errorf("receipt store s3: endpoint %q must be a URL with no path, query or credentials", ep)
	}
	return u.Host, useSSL, nil
}

// NewS3 checks cfg and makes the client. It doesn't reach the store; Ready
// does.
func NewS3(cfg S3Config) (*S3, error) {
	host, secure, err := endpointHost(cfg.Endpoint, cfg.UseSSL)
	if err != nil {
		return nil, err
	}
	var missing []string
	if strings.TrimSpace(cfg.Bucket) == "" {
		missing = append(missing, "RECEIPT_S3_BUCKET")
	}
	if cfg.AccessKey == "" {
		missing = append(missing, "RECEIPT_S3_ACCESS_KEY")
	}
	if cfg.SecretKey == "" {
		missing = append(missing, "RECEIPT_S3_SECRET_KEY")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("receipt store s3: %s not set", strings.Join(missing, ", "))
	}
	region := strings.TrimSpace(cfg.Region)
	if region == "" {
		region = DefaultS3Region
	}
	lookup := minio.BucketLookupDNS
	if cfg.PathStyle {
		lookup = minio.BucketLookupPath
	}
	c, err := minio.New(host, &minio.Options{
		Creds:        credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure:       secure,
		Region:       region,
		BucketLookup: lookup,
	})
	if err != nil {
		return nil, fmt.Errorf("receipt store s3: %w", err)
	}
	spool := cfg.SpoolDir
	if strings.TrimSpace(spool) == "" {
		spool = os.TempDir()
	}
	if err := os.MkdirAll(spool, 0o750); err != nil {
		return nil, fmt.Errorf("receipt store s3: spool directory %q: %w", spool, err)
	}
	probe, err := os.CreateTemp(spool, spoolPattern)
	if err != nil {
		return nil, fmt.Errorf("receipt store s3: spool directory %q isn't writable: %w", spool, err)
	}
	probe.Close()
	_ = os.Remove(probe.Name())
	return &S3{c: c, bucket: strings.TrimSpace(cfg.Bucket), spool: spool}, nil
}

// Bucket is the store's bucket.
func (s *S3) Bucket() string { return s.bucket }

// Endpoint is the store's endpoint URL (no credentials in it).
func (s *S3) Endpoint() string { return s.c.EndpointURL().String() }

// Ready makes sure the bucket exists, creating it if it doesn't. Put calls
// it until it has succeeded once, so a store that was down when amp-api
// started is picked up when it comes back.
func (s *S3) Ready(ctx context.Context) error {
	// The lock covers the flag only: while the store is down, concurrent
	// uploads each fail on their own deadline instead of queueing here.
	s.mu.Lock()
	ready := s.ready
	s.mu.Unlock()
	if ready {
		return nil
	}
	ok, err := s.c.BucketExists(ctx, s.bucket)
	if err != nil {
		if s3Error(err).Code == "AccessDenied" {
			return fmt.Errorf("receipt store s3: bucket %q: access denied to HeadBucket: the key needs s3:ListBucket on the bucket (and create, if it doesn't exist yet), or the credentials are wrong: %w", s.bucket, err)
		}
		return fmt.Errorf("receipt store s3: bucket %q: %w", s.bucket, err)
	}
	if !ok {
		if err := s.c.MakeBucket(ctx, s.bucket, minio.MakeBucketOptions{}); err != nil {
			code := s3Error(err).Code
			if code != "BucketAlreadyOwnedByYou" && code != "BucketAlreadyExists" {
				return fmt.Errorf("receipt store s3: create bucket %q: %w", s.bucket, err)
			}
		}
	}
	s.mu.Lock()
	s.ready = true
	s.mu.Unlock()
	return nil
}

// unready makes the next Put check (and recreate) the bucket: it went away
// while amp-api was running.
func (s *S3) unready() {
	s.mu.Lock()
	s.ready = false
	s.mu.Unlock()
}

// s3Error is the S3 error response inside err, if there is one.
func s3Error(err error) minio.ErrorResponse {
	var r minio.ErrorResponse
	if errors.As(err, &r) {
		return r
	}
	return minio.ToErrorResponse(err)
}

// AuthError reports whether err is the store refusing our credentials:
// that won't fix itself, unlike a store that is still starting.
func AuthError(err error) bool {
	switch s3Error(err).Code {
	case "AccessDenied", "InvalidAccessKeyId", "SignatureDoesNotMatch", "InvalidToken":
		return true
	}
	return false
}

// notFound reports a missing object. A missing bucket is not that: it is an
// error, so the sweep and purge don't take evidence for deleted. (A HEAD
// carries no error body, so minio reports a missing bucket on StatObject as
// NoSuchKey; PutObject and RemoveObject do say NoSuchBucket.)
func notFound(err error) bool {
	r := s3Error(err)
	if r.Code == "NoSuchBucket" {
		return false
	}
	return r.Code == "NoSuchKey" || r.Code == "NotFound" || (r.Code == "" && r.StatusCode == http.StatusNotFound)
}

func (s *S3) Put(ctx context.Context, key, contentType string, r io.Reader, max int64) (string, int64, error) {
	if err := ValidateKey(key); err != nil {
		return "", 0, err
	}
	if err := s.Ready(ctx); err != nil {
		return "", 0, err
	}
	if _, err := s.c.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{}); err == nil {
		return "", 0, ErrExists
	} else if !notFound(err) {
		return "", 0, fmt.Errorf("receipt store s3: %w", err)
	}
	// Spool, hashing as it is written and reading at most max+1 bytes: a
	// body over max stores nothing, and the upload's size is known.
	tmp, err := os.CreateTemp(s.spool, spoolPattern)
	if err != nil {
		return "", 0, err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), &ctxReader{ctx: ctx, r: io.LimitReader(r, max+1)})
	if err != nil {
		return "", n, err
	}
	if n > max {
		return "", n, ErrTooLarge
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return "", n, err
	}
	// If-None-Match: * closes the gap between the check above and this
	// write on stores that honour it (S3, R2, SeaweedFS). A store that
	// refuses the header (501 NotImplemented) gets the plain PUT: the check
	// above covers it.
	conditional := true
	for {
		opts := minio.PutObjectOptions{ContentType: contentType, DisableMultipart: true}
		if conditional {
			opts.SetMatchETagExcept("*")
		}
		_, err := s.c.PutObject(ctx, s.bucket, key, tmp, n, opts)
		if err == nil {
			break
		}
		r := s3Error(err)
		switch {
		case r.Code == "PreconditionFailed" || r.StatusCode == http.StatusPreconditionFailed:
			return "", n, ErrExists
		case conditional && (r.Code == "NotImplemented" || r.StatusCode == http.StatusNotImplemented):
			conditional = false
			if _, err := tmp.Seek(0, io.SeekStart); err != nil {
				return "", n, err
			}
			continue
		case r.Code == "NoSuchBucket":
			s.unready() // the next Put recreates it
		}
		return "", n, fmt.Errorf("receipt store s3: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func (s *S3) Serve(w http.ResponseWriter, r *http.Request, key string) error {
	if err := ValidateKey(key); err != nil {
		return err
	}
	obj, err := s.c.GetObject(r.Context(), s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return fmt.Errorf("receipt store s3: %w", err)
	}
	defer obj.Close()
	info, err := obj.Stat()
	if err != nil {
		if notFound(err) {
			return ErrNotFound
		}
		return fmt.Errorf("receipt store s3: %w", err)
	}
	// *minio.Object is an io.ReadSeeker: ServeContent answers Range with
	// ranged GETs. The caller set Content-Type, so nothing is sniffed.
	http.ServeContent(w, r, "", info.LastModified, obj)
	return nil
}

func (s *S3) Delete(ctx context.Context, key string) error {
	if err := ValidateKey(key); err != nil {
		return err
	}
	if err := s.c.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{}); err != nil && !notFound(err) {
		return fmt.Errorf("receipt store s3: %w", err)
	}
	return nil
}

// CleanTemp removes spool files older than olderThan that a crash left
// behind (a finished Put removes its own). The receipt sweep calls it.
func (s *S3) CleanTemp(olderThan time.Duration) (int, error) {
	matches, err := filepath.Glob(filepath.Join(s.spool, spoolPattern))
	if err != nil {
		return 0, err
	}
	cutoff := time.Now().Add(-olderThan)
	n := 0
	for _, p := range matches {
		info, err := os.Lstat(p)
		if err != nil || !info.Mode().IsRegular() || info.ModTime().After(cutoff) {
			continue
		}
		if err := os.Remove(p); err == nil {
			n++
		}
	}
	return n, nil
}
