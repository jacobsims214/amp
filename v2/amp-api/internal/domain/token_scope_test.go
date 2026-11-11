package domain

// The API token scope vocabulary (AMP #3772, build slice B5). The pure half of
// the submit-only credential: the two values, the one function that validates a
// requested scope, and the predicate the capability layer branches on.
//
// The closed vocabulary matters more than it looks. A token's scope is an
// attenuation of the account behind it, so a value nobody recognises must be an
// error rather than a silent "full": the safe default for a credential is the
// narrower scope, and only an explicit, valid request may widen it.

import "testing"

func TestNormalizeTokenScope(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		// Absent scope: every caller (and every token row) that predates
		// scopes is full, unchanged.
		{"empty is full", "", TokenScopeFull, false},
		{"full", "full", TokenScopeFull, false},
		// Forgiving about case and whitespace, like NormalizeUserKind.
		{"full with spaces and case", "  FULL ", TokenScopeFull, false},
		{"submit", "submit", TokenScopeSubmit, false},
		{"submit with spaces and case", " Submit\t", TokenScopeSubmit, false},
		// Unknown values fail closed. These are the interesting ones: a
		// typo, a value from a newer deployment, and the strings that look
		// like they might mean "everything".
		{"unknown word", "admin", "", true},
		{"plural", "submits", "", true},
		{"prefix", "sub", "", true},
		{"wildcard", "*", "", true},
		{"old spelling", "read", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := NormalizeTokenScope(c.in)
			if (err != nil) != c.wantErr {
				t.Fatalf("NormalizeTokenScope(%q) error = %v, wantErr %v", c.in, err, c.wantErr)
			}
			if got != c.want {
				t.Fatalf("NormalizeTokenScope(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestTokenScopesAreClosed pins the vocabulary itself. The migration's CHECK
// names exactly these two values (migration 060), so adding a scope means
// widening both — and this test is where that decision becomes visible instead
// of arriving as a constraint violation in production.
func TestTokenScopesAreClosed(t *testing.T) {
	want := []string{"full", "submit"}
	if len(TokenScopes) != len(want) {
		t.Fatalf("TokenScopes = %v, want %v", TokenScopes, want)
	}
	for i := range want {
		if TokenScopes[i] != want[i] {
			t.Fatalf("TokenScopes = %v, want %v (order matters: it is the schema CHECK's order)", TokenScopes, want)
		}
	}
	// Every listed value must normalize to itself: a value the schema accepts
	// but this function rejects would be a token nobody could mint.
	for _, s := range TokenScopes {
		if got, err := NormalizeTokenScope(s); err != nil || got != s {
			t.Fatalf("NormalizeTokenScope(%q) = %q, %v; a listed scope must round-trip", s, got, err)
		}
	}
}

func TestAPITokenSubmitOnly(t *testing.T) {
	cases := []struct {
		scope string
		want  bool
	}{
		// The zero value is what every pre-scope row and every Dex session
		// has; it must never be read as submit-only.
		{"", false},
		{TokenScopeFull, false},
		{TokenScopeSubmit, true},
	}
	for _, c := range cases {
		tok := APIToken{Scope: c.scope}
		if got := tok.SubmitOnly(); got != c.want {
			t.Errorf("APIToken{Scope: %q}.SubmitOnly() = %v, want %v", c.scope, got, c.want)
		}
	}
}
