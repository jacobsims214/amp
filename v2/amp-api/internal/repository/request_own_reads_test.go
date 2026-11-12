package repository

// Tests for the own-request reads (AMP #3958, epic 202 story 549). The
// database-backed behaviour — a requester sees only their own rows, across
// desks, with cursor-free totals — is pinned end-to-end by the service suite
// (internal/service/request_views_test.go), which drives these functions against
// a real Postgres. What is left to pin here is the part no database can answer:
// the two caller-bug refusals, which are deliberately not ErrNotFound.
//
// They matter because ErrNotFound is the surface's 404: a lookup made without
// the requester it is scoped by must not read as "that request is not yours",
// or a missing argument hides behind a legitimate answer forever. These run
// without a database because both refusals are decided before a query.

import (
	"errors"
	"testing"
)

func TestOwnRequestReads_RefuseACallerBugNotNotFound(t *testing.T) {
	// A Repo with no pool: reaching the database would panic, so a test that
	// passes proves the refusal happened before the query.
	r := &Repo{}

	t.Run("a lookup with no requester", func(t *testing.T) {
		_, err := r.GetRequestByRequester(t.Context(), 1, 0)
		if !errors.Is(err, ErrRequestRequester) {
			t.Fatalf("err = %v, want ErrRequestRequester", err)
		}
		if errors.Is(err, ErrNotFound) {
			t.Fatalf("a missing requester answered ErrNotFound, which the surface turns into a 404")
		}
		if errors.Is(err, ErrRequestTenantScope) {
			t.Fatalf("a missing requester answered ErrRequestTenantScope; these are two different caller bugs")
		}
	})

	t.Run("a reference lookup with no requester", func(t *testing.T) {
		_, err := r.GetRequestByRequesterRef(t.Context(), "someopaqueToken0123", 0)
		if !errors.Is(err, ErrRequestRequester) {
			t.Fatalf("err = %v, want ErrRequestRequester", err)
		}
	})

	t.Run("a list with no requester", func(t *testing.T) {
		_, err := r.ListOwnRequests(t.Context(), OwnRequestFilter{})
		if !errors.Is(err, ErrRequestRequester) {
			t.Fatalf("err = %v, want ErrRequestRequester", err)
		}
		_, err = r.CountOwnRequests(t.Context(), OwnRequestFilter{})
		if !errors.Is(err, ErrRequestRequester) {
			t.Fatalf("count err = %v, want ErrRequestRequester", err)
		}
	})

	t.Run("a non-positive id is an ordinary missing request", func(t *testing.T) {
		// The serial starts at 1, so id 0 (or a negative route parameter) cannot
		// name a row: it belongs on the same 404 as an id that does not exist,
		// not in the caller-bug class.
		_, err := r.GetRequestByRequester(t.Context(), 0, 42)
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	})
}
