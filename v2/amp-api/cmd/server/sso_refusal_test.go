package main

// #69: a sign-in that JIT provisioning refuses gets a sentence saying why and
// who can fix it, end to end over REST; the audit event is unchanged, and a
// sign-in that provisions works as before.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/simstech/amp-api/internal/api"
	"github.com/simstech/amp-api/internal/auth"
	"github.com/simstech/amp-api/internal/repository"
)

func TestSSO_ProvisionRefusalSaysWhy(t *testing.T) {
	s := setupMCP(t)
	ctx := context.Background()
	// The verifier's own repo, so the sign-up domains don't leak into other tests.
	repo, err := repository.New(ctx, testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(repo.Close)
	repo.SetSignupDomains([]string{"@corp.test"})

	idp := newTestIdP(t)
	v, err := auth.NewVerifier(ctx, auth.Config{IssuerURL: testIssuer, DiscoveryURL: idp.server.URL, Repo: repo})
	if err != nil {
		t.Fatal(err)
	}
	var events []auth.Event
	v.OnEvent = func(_ context.Context, ev auth.Event) {
		if ev.Kind == auth.EventSignupRefused {
			events = append(events, ev)
		}
	}
	mux := http.NewServeMux()
	api.NewRestHandler(s.reg, s.repo, s.hub, s.kb).Register(mux)
	srv := v.WithAudiences("rest").Middleware(mux)
	get := func(jwt, path string) (int, string) {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("Authorization", "Bearer "+jwt)
		srv.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}

	sfx := fmt.Sprint(time.Now().UnixNano())
	for _, tc := range []struct {
		name   string
		claims map[string]any
		want   auth.ProvisionRefusal
		reason string // the audit event's Reason, as before #69 ("" for no event)
	}{
		{"unverified email", map[string]any{"sub": "unv-" + sfx, "email": "unv-" + sfx + "@corp.test", "email_verified": false},
			auth.RefusalUnverifiedEmail, "email not verified: an unverified email is never provisioned or linked to an AMP user"},
		{"domain not allowed", map[string]any{"sub": "dom-" + sfx, "email": "dom-" + sfx + "@gmail.test"},
			auth.RefusalDomainNotAllowed, "sign-ups are limited to allowed email domains, and this email has no invite"},
		{"no email", map[string]any{"sub": "nomail-" + sfx}, auth.RefusalNoEmail, ""},
		{"local dev subject", map[string]any{"sub": auth.LocalDevSubject, "email": "ld-" + sfx + "@corp.test"},
			auth.RefusalReservedIdentity, "the Local Dev identity (local-dev@amp.local) is reserved for AMP with sign-in off; sign in with another account"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events = nil
			jwt := idp.token(t, tc.claims)
			code, body := get(jwt, "/api/my-work")
			if code != 403 || !strings.Contains(body, jsonText(tc.want.Message())) {
				t.Fatalf("/api/my-work: %d %s, want 403 with %q", code, body, tc.want.Message())
			}
			// The admin-only route says the role and the same reason.
			code, body = get(jwt, "/api/users")
			if code != 403 || !strings.Contains(body, "this needs the AMP admin role") || !strings.Contains(body, jsonText(tc.want.Message())) {
				t.Fatalf("/api/users: %d %s", code, body)
			}
			if strings.Contains(body, jwt) {
				t.Fatal("refusal echoes the token")
			}
			if tc.reason == "" {
				if len(events) != 0 {
					t.Fatalf("signup-refused events %+v, want none", events)
				}
			} else if len(events) != 2 || events[0].Reason != tc.reason {
				t.Fatalf("signup-refused events %+v, want two with reason %q", events, tc.reason)
			}
		})
	}

	// A sign-in that provisions is an AMP user as before, and a member is
	// told which role it lacks and who it is.
	email := "ok-" + sfx + "@corp.test"
	// Another real user first, so this sign-in is never the bootstrap admin,
	// whatever order the suite runs in or however empty the database is.
	if _, _, err := repo.ProvisionUserFromClaims(ctx, "seed-"+sfx, "seed-"+sfx+"@corp.test", "Seed", nil); err != nil {
		t.Fatal(err)
	}
	jwt := idp.token(t, map[string]any{"sub": "ok-" + sfx, "email": email, "email_verified": true})
	if code, body := get(jwt, "/api/me"); code != 200 || !strings.Contains(body, `"user_id"`) {
		t.Fatalf("/api/me for a provisioned sign-in: %d %s", code, body)
	}
	code, body := get(jwt, "/api/users")
	if code != 403 || !strings.Contains(body, "this needs the AMP admin role, which your account ("+email+") doesn't have") {
		t.Fatalf("/api/users as a member: %d %s", code, body)
	}
}

// The relay's lookup-only verifier refuses the reserved Local Dev identity
// (#165) and now records why, like the provisioning verifier does.
func TestSSO_LookupOnlyReservedIdentitySaysWhy(t *testing.T) {
	s := setupMCP(t)
	idp := newTestIdP(t)
	v, err := auth.NewVerifier(context.Background(), auth.Config{IssuerURL: testIssuer, DiscoveryURL: idp.server.URL, Repo: s.repo, LookupOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	var seen *auth.Identity
	h := v.WithAudiences("mcp").Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen, _ = auth.FromContext(r.Context())
	}))
	req := httptest.NewRequest("POST", "/relay/v1/sessions", nil)
	req.Header.Set("Authorization", "Bearer "+idp.token(t, map[string]any{"sub": auth.LocalDevSubject, "email": auth.LocalDevEmail, "aud": "mcp"}))
	h.ServeHTTP(httptest.NewRecorder(), req)
	if seen == nil || seen.User != nil || seen.ProvisionRefusal != auth.RefusalReservedIdentity {
		t.Fatalf("lookup-only Local Dev sign-in: %+v", seen)
	}
}

// jsonText is s as encoding/json writes it inside a string (the messages
// have no characters it escapes other than quotes).
func jsonText(s string) string { return strings.ReplaceAll(s, `"`, `\"`) }
