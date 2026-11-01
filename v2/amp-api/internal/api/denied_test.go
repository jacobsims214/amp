package api

import "testing"

func TestDeniedReason(t *testing.T) {
	for body, want := range map[string]string{
		`{"error":"only org admins can do that"}` + "\n": "only org admins can do that",
		`{"error":"a \"quoted\" word"}`:                  `a "quoted" word`,
		`{"error":"cut sh`:                               `{"error":"cut sh`,
		"forbidden\n":                                    "forbidden",
		"":                                               "forbidden",
	} {
		if got := deniedReason([]byte(body)); got != want {
			t.Errorf("deniedReason(%q) = %q, want %q", body, got, want)
		}
	}
}
