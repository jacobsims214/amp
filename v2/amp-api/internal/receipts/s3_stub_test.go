package receipts

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// stubS3 is just enough S3 for Put: the bucket exists, objects don't, and
// PUT answers as put says. It records each PUT's If-None-Match and body.
type stubS3 struct {
	put  func(ifNoneMatch string) int
	mu   sync.Mutex
	puts []string // If-None-Match of each PUT, "" when absent
	body []string
}

func (s *stubS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 2)
	switch {
	case r.Method == http.MethodHead && (len(parts) == 1 || parts[1] == ""):
		w.WriteHeader(http.StatusOK) // BucketExists
	case r.Method == http.MethodHead:
		w.WriteHeader(http.StatusNotFound) // StatObject: no such key
	case r.Method == http.MethodPut:
		b, _ := io.ReadAll(r.Body)
		inm := r.Header.Get("If-None-Match")
		s.mu.Lock()
		s.puts = append(s.puts, inm)
		s.body = append(s.body, string(b))
		s.mu.Unlock()
		code := s.put(inm)
		if code == http.StatusOK {
			w.Header().Set("ETag", `"d41d8cd98f00b204e9800998ecf8427e"`)
			w.WriteHeader(http.StatusOK)
			return
		}
		errCode := map[int]string{412: "PreconditionFailed", 501: "NotImplemented"}[code]
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(code)
		io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>`+errCode+`</Code><Message>stub</Message></Error>`)
	default:
		http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusTeapot)
	}
}

func newStubStore(t *testing.T, put func(string) int) (*S3, *stubS3) {
	t.Helper()
	stub := &stubS3{put: put}
	srv := httptest.NewServer(stub)
	t.Cleanup(srv.Close)
	st, err := NewS3(S3Config{Endpoint: srv.URL, Bucket: "evidence", AccessKey: "a", SecretKey: "s", PathStyle: true, SpoolDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	return st, stub
}

// Put's own handling of the conditional PUT, without a real store: a 412
// (the object appeared after the stat) is ErrExists; a 501 (a store that
// refuses If-None-Match) gets one plain PUT of the same bytes.
func TestS3_PutConditional(t *testing.T) {
	ctx := context.Background()
	key := "1/2/attempts/3/abc123/video.mp4"

	st, stub := newStubStore(t, func(string) int { return http.StatusPreconditionFailed })
	if _, _, err := st.Put(ctx, key, "video/mp4", strings.NewReader("frames"), 1<<20); !errors.Is(err, ErrExists) {
		t.Fatalf("412: %v, want ErrExists", err)
	}
	if len(stub.puts) != 1 || stub.puts[0] != "*" {
		t.Fatalf("412: PUTs %q, want one with If-None-Match: *", stub.puts)
	}

	st, stub = newStubStore(t, func(inm string) int {
		if inm != "" {
			return http.StatusNotImplemented
		}
		return http.StatusOK
	})
	sum, n, err := st.Put(ctx, key, "video/mp4", strings.NewReader("frames"), 1<<20)
	if err != nil || n != 6 || sum != sum256("frames") {
		t.Fatalf("501 then plain PUT: %s %d %v", sum, n, err)
	}
	if len(stub.puts) != 2 || stub.puts[0] != "*" || stub.puts[1] != "" {
		t.Fatalf("501: PUTs %q, want If-None-Match: * then none", stub.puts)
	}
	// The retry re-read the spool from the start.
	for i, b := range stub.body {
		if !strings.Contains(b, "frames") {
			t.Fatalf("PUT %d body %q: the retry didn't resend the bytes", i, b)
		}
	}

	// A store that answers 501 to the plain PUT too: an error, not a loop.
	st, stub = newStubStore(t, func(string) int { return http.StatusNotImplemented })
	if _, _, err := st.Put(ctx, key, "video/mp4", strings.NewReader("frames"), 1<<20); err == nil || errors.Is(err, ErrExists) {
		t.Fatalf("501 twice: %v", err)
	}
	if len(stub.puts) != 2 {
		t.Fatalf("501 twice: %d PUTs, want 2", len(stub.puts))
	}
}

func sum256(s string) string { return sum([]byte(s)) }
