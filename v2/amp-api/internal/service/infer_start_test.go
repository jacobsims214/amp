package service

import "testing"

// A comment signed as the dispatched worker is the worker's (AMP #2949):
// the old worker skills sign "amp-worker" whatever their specialist, a
// manager signs as itself or not at all.
func TestSignedAsWorker(t *testing.T) {
	for _, c := range []struct {
		author, agent string
		want          bool
	}{
		{"amp-worker", "amp:amp-worker-backend", true},
		{"amp-worker-backend", "amp:amp-worker-backend", true},
		{"amp:amp-worker-backend", "amp:amp-worker-backend", true},
		{"AMP-Worker", "amp-worker", true},
		{"amp-worker", "amp-worker", true},
		{"amp-manager", "amp:amp-worker-backend", false},
		{"agent", "amp:amp-worker-backend", false},
		{"", "amp:amp-worker-backend", false},
		{"amp", "amp:amp-worker-backend", false},
		{"amp-worker", "", false},
		{"jane@example.com (relayed by agent)", "amp-worker", false},
	} {
		if got := SignedAsWorker(c.author, c.agent); got != c.want {
			t.Errorf("SignedAsWorker(%q, %q) = %v, want %v", c.author, c.agent, got, c.want)
		}
	}
}
