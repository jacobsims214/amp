package api

// The discussion read's query decoder (AMP #4270).
//
// The route itself is exercised end to end in cmd/server/request_comments_rest_test.go
// (through the real mux, the real verifier and a real Postgres), which is where
// every other request route's tests live. What belongs here is the piece that is
// pure: parseRequestThreadQuery owns the two parameters' rules, and a decoder
// that quietly read -5 as "unset" — or that accepted a cursor it could not page
// with — is exactly the bug a client would see as "paging stopped working",
// never as an error. So the bounds and the refusals are asserted here, without a
// database, in denied_test.go's style.

import (
	"net/url"
	"testing"

	"github.com/simstech/amp-api/internal/authz"
)

func TestRequestCommentThreadQueryAcceptsTheCursorAndThePageSize(t *testing.T) {
	for _, tc := range []struct {
		name      string
		query     string
		wantAfter int64
		wantLimit int
	}{
		{"no parameters", "", 0, 0},
		{"after_id only", "after_id=41", 41, 0},
		{"limit only", "limit=25", 0, 25},
		{"both", "after_id=41&limit=25", 41, 25},
		{"zero is the first page", "after_id=0", 0, 0},
		{"whitespace around a value", "after_id=%2041%20&limit=%2025%20", 41, 25},
		{"a large cursor", "after_id=9223372036854775807", 9223372036854775807, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q, err := url.ParseQuery(tc.query)
			if err != nil {
				t.Fatal(err)
			}
			in, err := parseRequestThreadQuery(q)
			if err != nil {
				t.Fatalf("parseRequestThreadQuery(%q) = %v", tc.query, err)
			}
			if in.AfterID != tc.wantAfter {
				t.Fatalf("AfterID = %d, want %d", in.AfterID, tc.wantAfter)
			}
			if in.Limit != tc.wantLimit {
				t.Fatalf("Limit = %d, want %d", in.Limit, tc.wantLimit)
			}
			// The name is the read's, not the decoder's: it must never be filled
			// from a query parameter (a thread is one request's, named by the
			// path).
			if in.RequestID != 0 || in.PublicRef != "" || in.TeamID != 0 {
				t.Fatalf("the decoder invented a request name: %+v", in.RequestReadInput)
			}
		})
	}
}

func TestRequestCommentThreadQueryRefusesWhatItCannotPageWith(t *testing.T) {
	for _, tc := range []struct{ name, query string }{
		{"non-numeric after_id", "after_id=next"},
		{"negative after_id", "after_id=-1"},
		{"fractional after_id", "after_id=1.5"},
		{"non-numeric limit", "limit=lots"},
		{"negative limit", "limit=-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q, err := url.ParseQuery(tc.query)
			if err != nil {
				t.Fatal(err)
			}
			_, err = parseRequestThreadQuery(q)
			if err == nil {
				t.Fatalf("%q was accepted: a pager cannot act on a reinterpretation of it", tc.query)
			}
			if code := authz.CodeOf(err); code != authz.CodeRequestInvalid {
				t.Fatalf("%q refused with code %q, want %q", tc.query, code, authz.CodeRequestInvalid)
			}
		})
	}
}
