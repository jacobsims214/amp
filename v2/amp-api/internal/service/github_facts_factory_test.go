package service

// AMP #3217: the production reader factory under concurrency.
//
// GitHubClientReaderFactory returns a closure over one githubfacts.Options. The
// closure's whole job is to bind ONE refresh's token source to a client, and the
// coordinator calls it from every background refresh — up to
// DefaultGitHubFactsMaxRefresh (8) at once. So a factory that wrote the token
// into the struct it captured would be an unsynchronized write on state shared
// by every call, and worse, could hand one project's credential to another
// project's read.
//
// This file pins both halves of that: no data race (the -race detector sees the
// concurrent factory calls below, which is what CI's `go test -race` runs) and no
// cross-talk (each request must carry the token ITS factory call was given — a
// race can leak a token even when a single run happens not to trip the detector).
//
// The transport is the assertion surface: it answers every read and records the
// Authorization header against the slug in the URL, so "which credential reached
// this read" is observed on the wire, exactly where it matters.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/simstech/amp-api/internal/githubfacts"
)

// authRecordingTransport answers every repository-facts read and records, per
// request path, the Authorization header it carried.
type authRecordingTransport struct {
	mu       sync.Mutex
	bySlug   map[string][]string
	requests int
}

func (t *authRecordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	auth := req.Header.Get("Authorization")

	// The repository slug is the two path segments after /repos/.
	rest := strings.TrimPrefix(req.URL.Path, "/repos/")
	owner, tail, _ := strings.Cut(rest, "/")
	repo, _, _ := strings.Cut(tail, "/")
	slug := owner + "/" + repo

	t.mu.Lock()
	if t.bySlug == nil {
		t.bySlug = make(map[string][]string)
	}
	t.bySlug[slug] = append(t.bySlug[slug], auth)
	t.requests++
	t.mu.Unlock()

	// A minimal well-formed answer per read, so every call completes three
	// requests (repo, rules, protection) and no retry path is entered.
	var body string
	switch {
	case strings.Contains(req.URL.Path, "/rules/branches/"):
		body = `[]`
	case strings.Contains(req.URL.Path, "/protection"):
		body = `{}`
	default:
		body = `{"default_branch":"trunk"}`
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}, nil
}

// TestGitHubClientReaderFactoryIsConcurrencySafe is the probe: 8 goroutines (the
// coordinator's concurrency bound), each with its own token source and its own
// repository, all building readers through ONE factory and reading through them
// concurrently. Every request must carry its own call's token, and -race must see
// no unsynchronized access.
func TestGitHubClientReaderFactoryIsConcurrencySafe(t *testing.T) {
	const (
		workers = 8
		rounds  = 25
	)

	rt := &authRecordingTransport{}
	factory := GitHubClientReaderFactory(githubfacts.Options{Transport: rt})

	// A barrier, so all workers hit the factory together rather than one after
	// another: the write-then-read this test is about only overlaps when the
	// calls actually overlap.
	var start sync.WaitGroup
	start.Add(1)
	var done sync.WaitGroup

	for w := 0; w < workers; w++ {
		done.Add(1)
		go func(w int) {
			defer done.Done()
			slug := fmt.Sprintf("acme/repo%d", w)
			token := fmt.Sprintf("token-%d", w)
			start.Wait()
			for i := 0; i < rounds; i++ {
				// One factory call per refresh, exactly as the coordinator
				// does it: the token source belongs to this project's read.
				tok := func(context.Context) (string, error) { return token, nil }
				reader := factory(tok)
				facts, err := reader.Facts(context.Background(), githubfacts.Request{Slug: slug})
				if err != nil {
					t.Errorf("worker %d round %d: Facts: %v", w, i, err)
					return
				}
				if facts == nil {
					t.Errorf("worker %d round %d: nil facts", w, i)
					return
				}
			}
		}(w)
	}

	start.Done()
	done.Wait()

	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.requests == 0 {
		t.Fatal("no request reached the transport: the probe proved nothing")
	}
	for w := 0; w < workers; w++ {
		slug := fmt.Sprintf("acme/repo%d", w)
		want := "Bearer token-" + fmt.Sprint(w)
		got := rt.bySlug[slug]
		if len(got) == 0 {
			t.Errorf("%s: no request was made", slug)
			continue
		}
		for _, auth := range got {
			if auth != want {
				t.Fatalf("%s: a read carried %q, want its own call's credential %q — the factory shared state across calls",
					slug, auth, want)
			}
		}
	}
}
