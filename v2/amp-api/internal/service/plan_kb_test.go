package service

import (
	"context"
	"strings"
	"testing"

	"github.com/simstech/amp-api/internal/kb"
)

// A KB AMP can't read is a warning whose message says so in plain words: the
// cause (a host, a path) goes to the server log, never into the plan check
// people and agents read (#129).
func TestKBContextIssue_UnreadableKBDoesNotLeakTheError(t *testing.T) {
	s := &Service{KB: kb.New("http://127.0.0.1:1", "secret-key", "", "")}
	iss := s.kbContextIssue(context.Background(), 1)
	if iss == nil || iss.Kind != "no_context" || iss.Severity != "warn" {
		t.Fatalf("unreadable KB: want a no_context warning, got %+v", iss)
	}
	for _, leak := range []string{"127.0.0.1", "http", "dial", "connect", "secret-key"} {
		if strings.Contains(iss.Message, leak) || strings.Contains(iss.Fix, leak) {
			t.Fatalf("plan check leaks %q: %q / %q", leak, iss.Message, iss.Fix)
		}
	}
	if (&Service{}).kbContextIssue(context.Background(), 1).Severity != "warn" {
		t.Fatal("no KB configured: want a warning")
	}
}
