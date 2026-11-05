// Package receipts is the verification receipts evidence store (#126,
// v2/docs/specs/verification-receipts.md, "Evidence store"): the bytes of a
// receipt's screenshots, videos and logs. Postgres holds only their metadata
// (receipt_evidence); evidence is never an artifact (D3).
//
// Two backends, chosen by RECEIPT_STORE: local (the default; a directory: a
// docker volume, or a ReadWriteMany PVC in Helm) and s3 (any S3-compatible
// store: the bundled, opt-in SeaweedFS, or AWS S3, R2, Garage). Either way
// evidence is served through amp-api (Serve), never by a presigned URL, so
// the members-only check and the safety headers stay in one place.
// Retention is driven by Postgres (service.RunReceiptSweep calls Delete), so
// a backend needs no lifecycle rules of its own, and must not have any: a
// bucket rule would also delete kept receipts.
package receipts

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

var (
	// ErrExists: Put found the key already stored.
	ErrExists = errors.New("receipt evidence: key already stored")
	// ErrTooLarge: Put read more than its max.
	ErrTooLarge = errors.New("receipt evidence: larger than allowed")
	// ErrNotFound: Serve or Delete found no object at the key.
	ErrNotFound = errors.New("receipt evidence: not found")
)

// Store keeps evidence bytes by key (ValidateKey's form).
type Store interface {
	// Put writes key once and returns the sha256 (lowercase hex) and length
	// of the stored bytes. ErrExists if the key is stored; ErrTooLarge past
	// max (nothing is stored then).
	Put(ctx context.Context, key, contentType string, r io.Reader, max int64) (sha256 string, n int64, err error)
	// Serve writes the object to w; local uses http.ServeContent, so Range
	// works. The caller sets the safety headers (Content-Type included)
	// first. ErrNotFound when there is no object.
	Serve(w http.ResponseWriter, r *http.Request, key string) error
	// Delete removes the object; a missing one is not an error.
	Delete(ctx context.Context, key string) error
}

// Defaults (spec, "Config").
const (
	DefaultDir           = "data/receipts"
	DefaultMaxBytes      = 209715200 // 200 MiB
	DefaultRetentionDays = 90
)

// Config selects and configures the store.
type Config struct {
	// Backend is "local" (the default) or "s3".
	Backend string
	// Dir is local's root (DefaultDir when empty).
	Dir string
	// S3 configures the s3 backend.
	S3 S3Config
}

// New opens the configured store. An error means evidence storage isn't
// available; amp-api keeps running without it.
func New(cfg Config) (Store, error) {
	switch strings.ToLower(strings.TrimSpace(cfg.Backend)) {
	case "", "local":
		dir := cfg.Dir
		if strings.TrimSpace(dir) == "" {
			dir = DefaultDir
		}
		return NewLocal(dir)
	case "s3":
		return NewS3(cfg.S3)
	}
	return nil, fmt.Errorf("unknown receipt store %q (local or s3)", cfg.Backend)
}
