// The public MCP listener's body bound — the transport-level guard that keeps
// any authenticated credential from forcing an unbounded io.ReadAll.
//
// The end-to-end proof of what a caller SEES when the bound trips lives in
// request_submit_surface_test.go ("an over-bound body over MCP"): a real
// submission, padded with JSON whitespace, refused with HTTP 400 and a JSON-RPC
// PARSE_ERROR, writing nothing. That test needs a database and both listeners.
// This file pins the guard itself, so a future change to the middleware fails
// here — fast and without Postgres — instead of only in the parity suite.
package main

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/simstech/amp-api/internal/domain"
)

// TestLimitMCPBody covers the three things the middleware must get right:
// POST bodies are bounded, the bytes within the bound are unchanged (mcp-go
// still reads the exact JSON it would have read), and non-POST methods are
// left alone (GET is the SSE stream, DELETE ends a session).
func TestLimitMCPBody(t *testing.T) {
	// readAll is the handler under test: what mcp-go's ServeHTTP does with r.Body.
	var got []byte
	var gotErr error
	var gotMethod string
	h := limitMCPBody(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		got, gotErr = io.ReadAll(r.Body)
	}))

	// bodyAt builds a body of exactly n bytes, so the boundary is exact.
	bodyAt := func(n int64) string { return strings.Repeat("a", int(n)) }
	do := func(method string, body string) *httptest.ResponseRecorder {
		t.Helper()
		got, gotErr, gotMethod = nil, nil, ""
		var r *http.Request
		if method == http.MethodGet {
			r = httptest.NewRequest(method, "/", nil)
		} else {
			r = httptest.NewRequest(method, "/", strings.NewReader(body))
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}

	t.Run("a body at the bound is read whole", func(t *testing.T) {
		do(http.MethodPost, bodyAt(maxMCPRequestBody))
		if gotErr != nil {
			t.Fatalf("a body exactly at the bound failed: %v", gotErr)
		}
		if int64(len(got)) != maxMCPRequestBody {
			t.Fatalf("read %d bytes, want %d", len(got), maxMCPRequestBody)
		}
	})

	t.Run("a body over the bound stops at it", func(t *testing.T) {
		do(http.MethodPost, bodyAt(maxMCPRequestBody+1))
		var maxErr *http.MaxBytesError
		if !errors.As(gotErr, &maxErr) {
			t.Fatalf("an over-bound body read %d bytes with err %v, want *http.MaxBytesError", len(got), gotErr)
		}
		if maxErr.Limit != maxMCPRequestBody {
			t.Fatalf("MaxBytesError.Limit = %d, want %d", maxErr.Limit, maxMCPRequestBody)
		}
		if int64(len(got)) > maxMCPRequestBody {
			t.Fatalf("buffered %d bytes, over the %d bound", len(got), maxMCPRequestBody)
		}
	})

	// GET carries no body (it opens the SSE stream) and DELETE ends a session;
	// neither may be touched. Asserted by method, since the guard keys on POST.
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		t.Run(method+" is passed through", func(t *testing.T) {
			do(method, "")
			if gotErr != nil {
				t.Fatalf("%s: %v", method, gotErr)
			}
			if gotMethod != method {
				t.Fatalf("the handler saw %q, want %q", gotMethod, method)
			}
		})
	}
}

// TestMCPRequestBodyFitsAMaximumArtifact pins the number's REASON, so shrinking
// the bound back to REST's 256 KiB fails here with the explanation attached.
// The largest valid JSON-RPC on this listener is amp_publish_artifact carrying a
// domain.MaxArtifactBytes content; as base64 that is 4/3 of the content, and a
// bound at that content size or below would refuse a valid artifact publish.
func TestMCPRequestBodyFitsAMaximumArtifact(t *testing.T) {
	// Base64 inflates by 4/3 (plus padding), and the JSON-RPC envelope adds a
	// few hundred bytes of tool name, title, note and ids.
	const base64WorstCase = (domain.MaxArtifactBytes + 2) / 3 * 4
	const envelopeAllowance = 64 << 10
	if maxMCPRequestBody < base64WorstCase+envelopeAllowance {
		t.Fatalf("maxMCPRequestBody = %d does not fit a maximum artifact: %d bytes base64-encoded + %d of envelope = %d",
			maxMCPRequestBody, base64WorstCase, envelopeAllowance, base64WorstCase+envelopeAllowance)
	}
	if maxMCPRequestBody > 64<<20 {
		t.Fatalf("maxMCPRequestBody = %d is a 64 MiB+ memory guard; a request should never cost that", maxMCPRequestBody)
	}
}
