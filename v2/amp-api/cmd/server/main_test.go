package main

import (
	"strings"
	"testing"
)

// The startup log names the database without its password.
func TestRedactDSN(t *testing.T) {
	got := redactDSN("postgres://amp:s3cret-pw@db.internal:5432/amp?sslmode=require")
	if strings.Contains(got, "s3cret-pw") || !strings.Contains(got, "db.internal:5432/amp") {
		t.Fatalf("redacted DSN: %s", got)
	}
}
