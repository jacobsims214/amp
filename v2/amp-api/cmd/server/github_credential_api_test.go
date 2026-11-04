package main

// Project GitHub credentials over REST (AMP #3155, epic 183 story 453).
//
// The three things this ticket asks for, proven against the real mux and a
// real database:
//
//   - only a project owner can attach, rotate, delete or read the credential
//     state — unauthenticated callers get 401, a member or team lead 403,
//     someone outside the project the same 404 a missing project gets;
//   - the token is write-only: it goes in a request body and comes back out
//     of nothing, in no response and in no log line;
//   - the status endpoint reports the configured state, the metadata, and
//     whether this server can hold a token at all — never the token.
//
// No test here talks to GitHub, and no real credential exists: every token
// is an isolated fixture string.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/simstech/amp-api/internal/api"
	"github.com/simstech/amp-api/internal/auth"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/secretbox"
)

// credPath is a project's credential route.
func credPath(projectID int) string {
	return fmt.Sprintf("/api/projects/%d/github-credential", projectID)
}

// credRaw sends a request and returns the status and the raw body, so a test
// can search the answer for the token (the decoded map would hide the bytes
// a handler wrote).
func (ra *restAs) credRaw(t *testing.T, who, method, path string, body any) (int, string) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, ra.server.URL+path, rd)
	req.Header.Set("Content-Type", "application/json")
	if who != "" {
		req.Header.Set("X-Test-As", who)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

// Only a project owner reads or changes a project's GitHub credential. The
// whole route group is checked, because "the write is protected" is not the
// property that matters: reading the state is a settings surface too.
func TestGitHubCredentialAPI_OwnerOnly(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	pid, token := w.s.projectID, ghToken("api1")
	path := credPath(pid)

	// No session at all: 401, on every method.
	for _, m := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		if code, _ := ra.credRaw(t, "", m, path, map[string]any{"token": token}); code != 401 {
			t.Fatalf("unauthenticated %s %s: %d, want 401", m, path, code)
		}
	}
	if code, _ := ra.credRaw(t, "", http.MethodPost, path+"/rotate", map[string]any{"token": token}); code != 401 {
		t.Fatalf("unauthenticated rotate: %d, want 401", code)
	}

	// A project member, a second member, and the lead of a linked team are
	// all refused — attaching a credential is handing AMP somebody else's
	// token, and deleting one takes away what the project can read.
	for _, who := range []string{"alice", "bob", "carol", "lead"} {
		for _, req := range []struct{ method, path string }{
			{http.MethodGet, path},
			{http.MethodPut, path},
			{http.MethodPost, path + "/rotate"},
			{http.MethodDelete, path},
		} {
			code, body := ra.credRaw(t, who, req.method, req.path, map[string]any{"token": token, "label": "ci"})
			if code != 403 {
				t.Fatalf("%s %s %s: %d %s, want 403", who, req.method, req.path, code, body)
			}
		}
	}

	// Someone who can't see the project gets 404, so its existence isn't
	// leaked: dave is in the org but on no team linked to the project,
	// outsider is in another org.
	for _, who := range []string{"dave", "outsider"} {
		if code, body := ra.credRaw(t, who, http.MethodGet, path, nil); code != 404 {
			t.Fatalf("%s reading: %d %s, want 404", who, code, body)
		}
		if code, _ := ra.credRaw(t, who, http.MethodPut, path, map[string]any{"token": token}); code != 404 {
			t.Fatalf("%s attaching: %d, want 404", who, code)
		}
	}

	// Nothing any of them sent was stored.
	if _, err := w.s.repo.GetGitHubCredential(context.Background(), pid, ""); err == nil {
		t.Fatal("a refused attach left a credential behind")
	}

	// The owner attaches, and an org admin (owner level or above) may read
	// and rotate. The status is 200 for both.
	out := ra.want(t, 200, "owner", http.MethodPut, path, map[string]any{"token": token, "label": "ci"})
	if out["configured"] != true || out["host"] != "github.com" || out["encrypted"] != true {
		t.Fatalf("attach status = %v", out)
	}
	if cred, _ := out["credential"].(map[string]any); cred == nil || cred["label"] != "ci" {
		t.Fatalf("attach credential = %v", out["credential"])
	}
	ra.want(t, 200, "orgadmin", http.MethodGet, path, nil)
	ra.want(t, 200, "orgadmin", http.MethodPost, path+"/rotate", map[string]any{"token": ghToken("api2"), "label": "ci v2"})
	ra.want(t, 200, "owner", http.MethodDelete, path, nil)
}

// The token is write-only: no response carries it, on any route or in any
// error, and no log line does either.
func TestGitHubCredentialAPI_TokenNeverComesBack(t *testing.T) {
	var logs syncBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	pid := w.s.projectID
	path := credPath(pid)
	first, second := ghToken("w1"), ghToken("w2")

	// Every answer to every route is searched for both tokens and for the
	// token prefix a real one carries.
	check := func(what, body string, tokens ...string) {
		t.Helper()
		for _, tok := range tokens {
			if strings.Contains(body, tok) {
				t.Fatalf("%s returned the token: %s", what, body)
			}
		}
		if strings.Contains(body, "ghp_") {
			t.Fatalf("%s returned token-shaped material: %s", what, body)
		}
	}

	_, body := ra.credRaw(t, "owner", http.MethodPut, path, map[string]any{"token": first, "label": "ci"})
	check("attach", body, first)

	_, body = ra.credRaw(t, "owner", http.MethodGet, path, nil)
	check("status", body, first)
	if !strings.Contains(body, `"configured":true`) {
		t.Fatalf("status after attach = %s", body)
	}

	_, body = ra.credRaw(t, "owner", http.MethodPost, path+"/rotate", map[string]any{"token": second, "label": "ci v2"})
	check("rotate", body, first, second)
	if !strings.Contains(body, `"rotated_at"`) {
		t.Fatalf("a rotation should date itself: %s", body)
	}

	// A refused write answers without echoing what was sent: an unusable
	// token shape (a pasted header line) and a token in the query string.
	_, body = ra.credRaw(t, "owner", http.MethodPut, path, map[string]any{"token": "Authorization: Bearer " + first})
	check("a refused token shape", body, first)
	code, body := ra.credRaw(t, "owner", http.MethodPut, path+"?token="+first, map[string]any{"label": "ci"})
	if code != 400 {
		t.Fatalf("a token in the query string: %d %s, want 400", code, body)
	}
	check("the query-string refusal", body, first)

	// A body that doesn't decode is answered with the shape to send, not a
	// rendering of what was sent (the body holds the token).
	req, _ := http.NewRequest(http.MethodPut, ra.server.URL+path, strings.NewReader(`{"token": "`+first+`", oops}`))
	req.Header.Set("X-Test-As", "owner")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("a malformed body: %d %s", resp.StatusCode, raw)
	}
	check("a malformed body", string(raw), first)

	// The stored value is sealed under the configured key and holds no
	// token text — the API's write went through the sealing storage layer.
	stored := storedCredentialSecret(t, testDSN(t), pid)
	if !secretbox.IsSealed(stored) || strings.Contains(stored, second) {
		t.Fatalf("stored value = %q", stored)
	}

	// Delete answers with what it removed, not what it removed it from.
	_, body = ra.credRaw(t, "owner", http.MethodDelete, path, nil)
	check("delete", body, first, second)

	// The audit log recorded the three actions the API performed, by host and
	// label, and holds no token either.
	rows := auditRows(t, w.s.repo, domain.AuditFilter{ActorUserID: w.owner.User.ID, Action: "project.github_credential."})
	seen := map[string]bool{}
	for _, e := range rows {
		seen[e.Action] = true
		if b := mustJSON(e); strings.Contains(b, first) || strings.Contains(b, second) || strings.Contains(b, "ghp_") {
			t.Fatalf("%s audit row holds the token: %s", e.Action, b)
		}
	}
	for _, action := range []string{
		"project.github_credential.attach", "project.github_credential.rotate", "project.github_credential.delete",
	} {
		if !seen[action] {
			t.Fatalf("no %s audit row from the API: %v", action, rows)
		}
	}

	// Nothing along the way logged a token either.
	if got := logs.String(); strings.Contains(got, first) || strings.Contains(got, second) || strings.Contains(got, "ghp_") {
		t.Fatalf("a log line holds the token:\n%s", got)
	}
}

// The status endpoint is a settings surface: it says whether a credential is
// attached, for which host, and whether this server can hold one at all —
// and a server with no encryption key says so instead of failing silently
// when someone tries to attach.
func TestGitHubCredentialAPI_StatusAndNoKey(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	pid := w.s.projectID
	path := credPath(pid)

	// Nothing attached: configured false, the default host, encrypted true
	// (the test world has a key), and nothing wrong.
	out := ra.want(t, 200, "owner", http.MethodGet, path, nil)
	if out["configured"] != false || out["host"] != "github.com" || out["encrypted"] != true {
		t.Fatalf("empty status = %v", out)
	}
	if _, has := out["credential"]; has {
		t.Fatalf("empty status carries a credential: %v", out)
	}
	if _, has := out["last_error"]; has {
		t.Fatalf("empty status reports a problem: %v", out)
	}

	// A rotate of nothing is reported as such (404 from the service), not
	// quietly turned into an attach.
	if code, body := ra.credRaw(t, "owner", http.MethodPost, path+"/rotate", map[string]any{"token": ghToken("nk1")}); code != 404 {
		t.Fatalf("rotate with nothing attached: %d %s, want 404", code, body)
	}
	// A missing token is a 400 with the service's own message.
	if code, body := ra.credRaw(t, "owner", http.MethodPut, path, map[string]any{"label": "ci"}); code != 400 || !strings.Contains(body, "token") {
		t.Fatalf("attach without a token: %d %s, want 400", code, body)
	}

	// With no encryption key the server can't hold a token: the status says
	// so, and an attach is refused naming the setting rather than storing a
	// token in the clear.
	w.s.repo.SetSecretsKeys(nil)
	t.Cleanup(func() { w.s.repo.SetSecretsKeys(defaultTestSecretsKeys(t)) })

	out = ra.want(t, 200, "owner", http.MethodGet, path, nil)
	if out["configured"] != false || out["encrypted"] != false || out["last_error"] != "no_encryption_key" {
		t.Fatalf("status with no key = %v", out)
	}
	code, body := ra.credRaw(t, "owner", http.MethodPut, path, map[string]any{"token": ghToken("nk2"), "label": "ci"})
	if code != 400 || !strings.Contains(body, secretbox.EnvKey) {
		t.Fatalf("attach with no key: %d %s, want a 400 naming %s", code, body, secretbox.EnvKey)
	}
	if _, err := w.s.repo.GetGitHubCredential(context.Background(), pid, ""); err == nil {
		t.Fatal("a refused attach with no key left a row behind")
	}

	// With the key back, the same request works, and the status clears the
	// problem.
	w.s.repo.SetSecretsKeys(defaultTestSecretsKeys(t))
	out = ra.want(t, 200, "owner", http.MethodPut, path, map[string]any{"token": ghToken("nk3"), "label": "ci"})
	if out["configured"] != true || out["encrypted"] != true || out["last_error"] != nil {
		t.Fatalf("attach after the key came back = %v", out)
	}
}

// Hosts and labels are metadata the API checks, so a pasted URL is a clear
// 400 rather than a database constraint reported as a storage failure. One
// host's credential is not another's.
func TestGitHubCredentialAPI_HostAndLabel(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	pid := w.s.projectID
	path := credPath(pid)

	ra.want(t, 200, "owner", http.MethodPut, path, map[string]any{"token": ghToken("h1"), "label": "dotcom"})

	// The default host is configured; an enterprise host is not, and
	// deleting one leaves the other alone.
	if out := ra.want(t, 200, "owner", http.MethodGet, path+"?host=ghe.example.com", nil); out["configured"] != false {
		t.Fatalf("enterprise host = %v, want not configured", out)
	}
	out := ra.want(t, 200, "owner", http.MethodPut, path+"?host=ghe.example.com", map[string]any{"token": ghToken("h2"), "label": "ghe"})
	if out["configured"] != true || out["host"] != "ghe.example.com" {
		t.Fatalf("attach for the enterprise host = %v", out)
	}
	if out := ra.want(t, 200, "owner", http.MethodDelete, path+"?host=ghe.example.com", nil); out["deleted"] != true || out["host"] != "ghe.example.com" {
		t.Fatalf("delete for the enterprise host = %v", out)
	}
	if out := ra.want(t, 200, "owner", http.MethodGet, path, nil); out["configured"] != true {
		t.Fatalf("deleting one host's credential removed another's: %v", out)
	}

	// A URL or a path is not a host.
	for _, host := range []string{"https://github.com", "github.com/", "github .com", strings.Repeat("h", 300)} {
		code, body := ra.credRaw(t, "owner", http.MethodPut, path, map[string]any{"token": ghToken("h3"), "host": host})
		if code != 400 {
			t.Fatalf("host %q: %d %s, want 400", host, code, body)
		}
	}
	// A label is a name, not a document.
	code, body := ra.credRaw(t, "owner", http.MethodPut, path, map[string]any{"token": ghToken("h4"), "label": strings.Repeat("l", 200)})
	if code != 400 {
		t.Fatalf("an over-long label: %d %s, want 400", code, body)
	}
	// The token itself is never a host-shaped value to be stored somewhere
	// else: a body with no token is a 400, not an empty credential.
	if code, _ := ra.credRaw(t, "owner", http.MethodPut, path, map[string]any{"host": "github.com"}); code != 400 {
		t.Fatalf("a body with no token: %d, want 400", code)
	}

	// The route exists, so a method it doesn't serve is 405 with Allow (not
	// a 404 that would read as "no such route"), and an unknown sub-path is
	// 404.
	if code, _ := ra.credRaw(t, "owner", http.MethodPost, path, map[string]any{"token": ghToken("h5")}); code != 405 {
		t.Fatalf("POST on the collection: %d, want 405", code)
	}
	if code, _ := ra.credRaw(t, "owner", http.MethodGet, path+"/rotate", nil); code != 405 {
		t.Fatalf("GET on rotate: %d, want 405", code)
	}
	if code, _ := ra.credRaw(t, "owner", http.MethodGet, path+"/nope", nil); code != 404 {
		t.Fatalf("unknown sub-path: %d, want 404", code)
	}
}

// The route group sits behind the same cross-site write guard as every other
// REST route (main.go's RejectCrossSiteWrites): a browser-marked cross-site
// or sandboxed-origin write never reaches the handler, so a page can't attach
// a token as the viewer. Reads are unaffected (auth and CORS cover those).
func TestGitHubCredentialAPI_CrossSiteWritesRefused(t *testing.T) {
	w := newRBACWorld(t)
	pid, token := w.s.projectID, ghToken("cs1")
	path := credPath(pid)
	mux := apiMux(api.NewRestHandler(w.s.reg, w.s.repo, w.s.hub, w.s.kb))
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		c := *w.owner
		c.Listener, c.Channel = "rest", auth.ChannelUI
		api.RejectCrossSiteWrites(mux).ServeHTTP(rw, r.WithContext(auth.WithIdentity(r.Context(), &c)))
	}))
	defer srv.Close()

	do := func(method, origin, site string) int {
		body, _ := json.Marshal(map[string]any{"token": token, "label": "ci"})
		req, _ := http.NewRequest(method, srv.URL+path, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		if site != "" {
			req.Header.Set("Sec-Fetch-Site", site)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}

	for _, c := range []struct{ method, origin, site string }{
		{http.MethodPut, "null", ""},        // a sandboxed artifact page
		{http.MethodPut, "", "cross-site"},  // another site
		{http.MethodDelete, "null", ""},     // removing a credential is a write too
		{http.MethodPost, "", "cross-site"}, // rotate
	} {
		if code := do(c.method, c.origin, c.site); code != 403 {
			t.Fatalf("%s with Origin=%q Sec-Fetch-Site=%q: %d, want 403", c.method, c.origin, c.site, code)
		}
	}
	// Nothing was written by any of them.
	if _, err := w.s.repo.GetGitHubCredential(context.Background(), pid, ""); err == nil {
		t.Fatal("a cross-site write attached a credential")
	}
	// The same-origin write goes through, so the guard is what refused the
	// others and not the route.
	if code := do(http.MethodPut, "https://amp.example.com", "same-origin"); code != 200 {
		t.Fatalf("same-origin PUT: %d, want 200", code)
	}
}

// A read of the state never opens the sealed value: the credential the API
// returns is the metadata type, which has no token field at all.
func TestGitHubCredentialAPI_ReadUsesMetadataType(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	pid, token := w.s.projectID, ghToken("md1")
	path := credPath(pid)
	ra.want(t, 200, "owner", http.MethodPut, path, map[string]any{"token": token, "label": "ci"})

	_, body := ra.credRaw(t, "owner", http.MethodGet, path, nil)
	var out map[string]any
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("status is not JSON: %v (%s)", err, body)
	}
	cred, ok := out["credential"].(map[string]any)
	if !ok {
		t.Fatalf("status has no credential: %s", body)
	}
	for _, field := range []string{"sealed_secret", "secret", "token", "value", "encrypted_value"} {
		if _, has := cred[field]; has {
			t.Fatalf("the credential carries a %q field: %s", field, body)
		}
	}
	// The stored token is still readable only through the storage layer's
	// one accessor, which the API never calls.
	if got, err := w.s.repo.GitHubCredentialSecret(context.Background(), pid, ""); err != nil || got != token {
		t.Fatalf("the stored token = %q %v", got, err)
	}
}
