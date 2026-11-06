package main

import (
	"regexp"
	"strings"
	"testing"
)

var migrationNameRe = regexp.MustCompile(`^(\d{3})_[^/]+$`)

// allowedDuplicatePrefixes holds the historical pairs that already shipped
// with a shared number. Never add to this; take the next free number.
var allowedDuplicatePrefixes = map[string]bool{"031": true}

// migrationPrefixProblems reports versions whose prefix is not three digits
// plus an underscore, and prefixes shared by more than one version (other
// than the allowlisted historical ones).
func migrationPrefixProblems(versions []string, allow map[string]bool) []string {
	var problems []string
	byPrefix := map[string][]string{}
	for _, v := range versions {
		m := migrationNameRe.FindStringSubmatch(v)
		if m == nil {
			problems = append(problems, v+": prefix must be 3 digits then an underscore")
			continue
		}
		byPrefix[m[1]] = append(byPrefix[m[1]], v)
	}
	for p, vs := range byPrefix {
		if len(vs) > 1 && !allow[p] {
			problems = append(problems, "duplicate prefix "+p+": "+strings.Join(vs, ", "))
		}
	}
	return problems
}

func TestMigrationPrefixes(t *testing.T) {
	t.Run("embedded migrations are well numbered", func(t *testing.T) {
		var versions []string
		for _, m := range mustLoadMigrations() {
			versions = append(versions, m.Version)
		}
		if len(versions) == 0 {
			t.Fatal("no embedded migrations found")
		}
		for _, p := range migrationPrefixProblems(versions, allowedDuplicatePrefixes) {
			t.Error(p)
		}
	})

	cases := []struct {
		name     string
		versions []string
		allow    map[string]bool
		wantBad  int
	}{
		{"unique ok", []string{"001_a", "002_b", "043_c"}, nil, 0},
		{"duplicate 043 fails", []string{"042_a", "043_b", "043_c"}, nil, 1},
		{"allowlisted 031 ok", []string{"031_audit_context", "031_remote_no_default_limits"}, map[string]bool{"031": true}, 0},
		{"allowlist does not cover other prefixes", []string{"031_a", "031_b", "043_c", "043_d"}, map[string]bool{"031": true}, 1},
		{"two digits fails", []string{"44_x"}, nil, 1},
		{"four digits fails", []string{"0044_x"}, nil, 1},
		{"no underscore fails", []string{"044x"}, nil, 1},
		{"README fails", []string{"README.md"}, nil, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := migrationPrefixProblems(c.versions, c.allow); len(got) != c.wantBad {
				t.Errorf("got %d problems %v, want %d", len(got), got, c.wantBad)
			}
		})
	}
}
