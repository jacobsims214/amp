package actor

import "testing"

// The history says "by its owner" only when its owner acted (#129): a ticket
// started with no owner doesn't claim one.
func TestHumanWorkDetail(t *testing.T) {
	one, two := 1, 2
	cases := []struct {
		by, owner *int
		want      string
	}{
		{&one, &one, "Started by its owner (human work)"},
		{&one, &two, "Started (human work)"},
		{&one, nil, "Started (human work)"},
		{nil, nil, "Started (human work)"},
	}
	for _, c := range cases {
		if got := humanWorkDetail("Started", c.by, c.owner); got != c.want {
			t.Errorf("humanWorkDetail(%v, %v) = %q, want %q", c.by, c.owner, got, c.want)
		}
	}
}
