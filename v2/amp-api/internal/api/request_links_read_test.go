package api

// The links read's query decoder (AMP #4272).
//
// The route itself is exercised end to end in cmd/server/request_links_read_rest_test.go
// (through the real mux, the real verifier and a real Postgres), which is where
// every other request route's tests live. What belongs here is the piece that is
// pure: parseRequestLinksQuery owns the `kind` filter's rules, and a filter that
// quietly dropped an unknown word — or answered an empty list for it — is exactly
// the bug a client would read as "this request has nothing attached". So the
// accepted spellings, the dedup and every refusal are asserted here, without a
// database, in the same style as parseRequestThreadQuery's and
// parseRequestHistoryQuery's cases.

import (
	"net/url"
	"slices"
	"testing"

	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
)

func TestRequestLinksQueryAcceptsEverySpellingOfAKindFilter(t *testing.T) {
	for _, tc := range []struct {
		name  string
		query string
		want  []domain.RequestLinkKind
	}{
		// No filter is the rail's question: every relation, in the repository's
		// own order. An empty []RequestLinkKind must reach the repository as "no
		// kinds", which is its documented "all of them".
		{"no parameter", "", nil},
		{"one kind", "kind=planned_as", []domain.RequestLinkKind{domain.RequestLinkPlannedAs}},
		{"a repeated parameter", "kind=planned_as&kind=delivered_by",
			[]domain.RequestLinkKind{domain.RequestLinkPlannedAs, domain.RequestLinkDeliveredBy}},
		{"a comma-joined list", "kind=planned_as,delivered_by",
			[]domain.RequestLinkKind{domain.RequestLinkPlannedAs, domain.RequestLinkDeliveredBy}},
		// The queue's `state` filter accepts both spellings mixed and normalizes
		// case and padding, so a client does not learn a second convention here.
		{"mixed spellings, case and padding", "kind=%20ROUTES_TO%20&kind=Planned_As,+delivered_by",
			[]domain.RequestLinkKind{domain.RequestLinkRoutesTo, domain.RequestLinkPlannedAs, domain.RequestLinkDeliveredBy}},
		// A duplicate is a caller repeating itself, not an error: the
		// repository's `link_kind = ANY(...)` answers the same rows, and refusing
		// would punish a client that merged two filters.
		{"the same kind twice", "kind=planned_as&kind=planned_as",
			[]domain.RequestLinkKind{domain.RequestLinkPlannedAs}},
		{"the same kind in both spellings", "kind=planned_as,planned_as&kind=PLANNED_AS",
			[]domain.RequestLinkKind{domain.RequestLinkPlannedAs}},
		// Every value in the closed set is accepted, so a kind the matrix gains
		// is honored without this decoder changing.
		{"every kind", "kind=routes_to,planned_as,delivered_by,duplicate_of,relates_to",
			slices.Clone(domain.RequestLinkKinds)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q, err := url.ParseQuery(tc.query)
			if err != nil {
				t.Fatal(err)
			}
			in, err := parseRequestLinksQuery(q)
			if err != nil {
				t.Fatalf("parseRequestLinksQuery(%q) = %v", tc.query, err)
			}
			if !slices.Equal(in.Kinds, tc.want) {
				t.Fatalf("Kinds = %v, want %v", in.Kinds, tc.want)
			}
			// The name is the read's, not the decoder's: it must never be filled
			// from a query parameter (a lineage is one request's, named by the
			// path), and there is no cursor to invent.
			if in.RequestID != 0 || in.PublicRef != "" || in.TeamID != 0 {
				t.Fatalf("the decoder invented a request name: %+v", in.RequestReadInput)
			}
		})
	}
}

func TestRequestLinksQueryRefusesWhatItCannotFilterWith(t *testing.T) {
	for _, tc := range []struct{ name, query string }{
		{"an unknown kind", "kind=planned"},
		{"a target kind", "kind=task"},
		{"an empty value", "kind="},
		{"an empty element in a list", "kind=planned_as,,delivered_by"},
		{"a trailing comma", "kind=planned_as,"},
		{"one bad value among good ones", "kind=planned_as&kind=shipped"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q, err := url.ParseQuery(tc.query)
			if err != nil {
				t.Fatal(err)
			}
			_, err = parseRequestLinksQuery(q)
			if err == nil {
				t.Fatalf("%q was accepted: a filter nobody can act on must not become an empty answer", tc.query)
			}
			if code := authz.CodeOf(err); code != authz.CodeRequestInvalid {
				t.Fatalf("%q refused with code %q, want %q", tc.query, code, authz.CodeRequestInvalid)
			}
		})
	}
}
