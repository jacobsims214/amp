package main

// Attachment downloads over REST (epic 202, story 549, AMP #3963; build slice G3's
// serving half; approved contract artifact 36 v2 §1 and decision D3).
//
// internal/service/request_downloads_test.go proves the gate: who may have a file, which
// states refuse it, expiry, revocation, single use and the auth recheck. This file asks
// only the questions that are about *the route and the headers*, because those live in
// internal/api/request_download.go and proving them anywhere else would prove a copy:
//
//	Criterion 3 — headers/private-byte access:
//	  TestRequestDownloadREST_ServesPrivateBytesOnce (the mint answers a URL on AMP's own
//	  origin; following it returns exactly the uploaded bytes, under
//	  Content-Disposition: attachment and X-Content-Type-Options: nosniff with the row's
//	  sniffed type, no-store, and no Content-Disposition that could ever be inline; the
//	  same URL again is 404, so the bytes are reachable once)
//	  TestRequestDownloadREST_FileNameIsHeaderSafe (a name with a quote, a semicolon and a
//	  non-ASCII letter cannot break or extend the header: the fallback is quoted ASCII and
//	  the real name travels in filename*)
//	Criterion 1 — expired/revoked/foreign grants refused, on the route:
//	  TestRequestDownloadREST_RefusalsAreIndistinguishable (another person's token, an
//	  unknown token, a foreign attachment's mint and a nonexistent request all answer the
//	  same 404 body; a revoked token answers it too)
//
// Everything goes through the real mux (api.RestHandler.Register, so deniedMux, the
// submit-scope gate and the denied() audit are in front of the handler) and the real
// auth.NewAPITokenVerifier, and the database is a scratch database per test — so the
// counts and the refusals are the product's, not a stub's.
//
// Run against a disposable Postgres (never a live stack), per
// how-to/run-one-amp-api-test-with-disposable-postgres.md:
//
//	TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:PORT/amp_test?sslmode=disable \
//	  go test ./cmd/server -run 'TestRequestDownloadREST' -count=1 -race -v

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/simstech/amp-api/internal/api"
	"github.com/simstech/amp-api/internal/auth"
	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/repository"
	"github.com/simstech/amp-api/internal/service"
)

// downloadWorld is one tenant with an inbox team and the people a download's caller can
// be. It is deliberately smaller than the submission world: downloads need a request
// with a file, not a queue.
type downloadWorld struct {
	t   *testing.T
	srv *httptest.Server
	ctx context.Context
	db  *pgx.Conn
	svc *service.Service

	org, team, otherOrg, foreign int

	requester *auth.Identity // the customer who submitted
	member    *auth.Identity // a member of the inbox team
	admin     *auth.Identity // an owner of the inbox's org (the release/revoke authority class)
	other     *auth.Identity // another customer, no relationship to the request
	stranger  *auth.Identity // an owner of another org

	requesterTok, memberTok, otherTok, strangerTok string

	request *domain.TeamRequest
	fileID  int64
	content []byte
}

func newDownloadWorld(t *testing.T) *downloadWorld {
	t.Helper()
	ctx := context.Background()
	dsn := freshDatabase(t)
	repo, err := repository.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(repo.Close)
	if err := repo.RunMigrations(ctx, migrations); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	db, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close(context.Background()) })

	w := &downloadWorld{t: t, ctx: ctx, db: db}
	insert := func(sql string, args ...any) int {
		t.Helper()
		var id int
		if err := db.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return id
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := db.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	sfx := uniq("download")
	// The very first sign-in in a scratch database becomes a global admin, and a global
	// admin reaches every inbox by design — so the bootstrap account is a throwaway no
	// test uses, and every person this world tests with is pinned as a plain member
	// (z.RequireTeamManager, authz.InboxRole).
	if _, err := repo.UpsertUserFromClaims(ctx, sfx+"-bootstrap", "bootstrap-"+sfx+"@download.test", "bootstrap", nil); err != nil {
		t.Fatalf("bootstrap user: %v", err)
	}
	sfxUser := func(label, kind string) *auth.Identity {
		u, err := repo.UpsertUserFromClaims(ctx, sfx+"-"+label,
			fmt.Sprintf("%s-%s@download.test", label, sfx), label, nil)
		if err != nil {
			t.Fatalf("user %s: %v", label, err)
		}
		if err := repo.SetUserRole(ctx, u.ID, domain.RoleMember, true); err != nil {
			t.Fatal(err)
		}
		if err := repo.SetUserRole(ctx, u.ID, domain.RoleAdmin, false); err != nil {
			t.Fatalf("pin %s as a non-admin (a scratch database's first sign-in is a global admin): %v", label, err)
		}
		if kind == domain.UserKindCustomer {
			exec(`UPDATE users SET kind = $2 WHERE id = $1`, u.ID, kind)
			u.Kind = kind
		}
		return &auth.Identity{Subject: u.Subject, Email: u.Email, Name: label, User: u}
	}
	w.org = insert(`INSERT INTO orgs (name, slug) VALUES ('Download org', $1) RETURNING id`, sfx)
	w.team = insert(`INSERT INTO teams (org_id, name) VALUES ($1, 'Inbox') RETURNING id`, w.org)
	w.otherOrg = insert(`INSERT INTO orgs (name, slug) VALUES ('Download other', $1) RETURNING id`, sfx+"-o")
	w.foreign = insert(`INSERT INTO teams (org_id, name) VALUES ($1, 'Foreign inbox') RETURNING id`, w.otherOrg)

	// Not a global admin: a global admin reaches every inbox by design (authz.InboxRole),
	// so a "stranger" must be an ordinary owner of another org to test the refusal at all.
	w.stranger = sfxUser("stranger", domain.UserKindHuman)
	w.member = sfxUser("member", domain.UserKindHuman)
	w.requester = sfxUser("requester", domain.UserKindCustomer)
	w.other = sfxUser("othercustomer", domain.UserKindCustomer)
	w.admin = sfxUser("admin", domain.UserKindHuman)

	exec(`INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'member')`, w.org, w.member.User.ID)
	exec(`INSERT INTO team_members (team_id, user_id, role) VALUES ($1, $2, 'member')`, w.team, w.member.User.ID)
	exec(`INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'owner')`, w.org, w.admin.User.ID)
	exec(`INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'owner')`, w.otherOrg, w.stranger.User.ID)
	exec(`INSERT INTO team_submit_grants (team_id, org_id, user_id) VALUES ($1, $2, $3)`, w.team, w.org, w.requester.User.ID)

	// The handler's own service, on the real mux, behind the real token verifier.
	mux := http.NewServeMux()
	h := api.NewRestHandler(nil, repo, nil, nil)
	w.svc = h.Service()
	h.Register(mux)
	w.srv = httptest.NewServer(auth.NewAPITokenVerifier(repo).Middleware(mux))
	t.Cleanup(w.srv.Close)

	w.requesterTok = w.mintToken(w.requester)
	w.memberTok = w.mintToken(w.member)
	w.otherTok = w.mintToken(w.other)
	w.strangerTok = w.mintToken(w.stranger)

	// The request, submitted through the real intake path, and its file, uploaded through
	// the real upload path — so every row is the product's.
	res, err := w.svc.SubmitRequest(ctx, w.principal(w.requester.User.ID), service.SubmitRequestInput{
		TeamID: w.team, Source: domain.RequestSourceWeb,
		Subject: "Please look at the attached file", InputBrief: "The file has the details.",
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	w.request = res.Request
	w.content = []byte("the requester's private notes: do not leak these\n")
	att, err := w.svc.AddRequestAttachment(ctx, w.principal(w.requester.User.ID), service.AddRequestAttachmentInput{
		RequestID: w.request.ID, OrgID: w.request.OrgID, TeamID: w.request.TeamID,
		FileName: "notes.txt", Content: w.content,
	})
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	w.fileID = att.ID
	return w
}

func (w *downloadWorld) principal(userID int) authz.Principal {
	w.t.Helper()
	var email string
	if err := w.db.QueryRow(w.ctx, `SELECT email FROM users WHERE id = $1`, userID).Scan(&email); err != nil {
		w.t.Fatalf("email of %d: %v", userID, err)
	}
	p := authz.Principal{UserID: userID, Email: email, Channel: auth.ChannelUI, AuthMethod: auth.AuthDexSession}
	return p
}

func (w *downloadWorld) mintToken(ident *auth.Identity) string {
	w.t.Helper()
	ui := authz.Principal{UserID: ident.User.ID, Email: ident.Email, Channel: auth.ChannelUI, AuthMethod: auth.AuthDexSession}
	tok, err := w.svc.CreateToken(w.ctx, ui, ident.User.ID, "download-"+uniq("t"), 0)
	if err != nil {
		w.t.Fatalf("mint a token: %v", err)
	}
	return tok.Token
}

// httpResult is one response: status, headers and raw body.
type httpResult struct {
	status int
	header http.Header
	body   []byte
}

func (w *downloadWorld) do(token, method, path string) httpResult {
	w.t.Helper()
	req, err := http.NewRequest(method, w.srv.URL+path, nil)
	if err != nil {
		w.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		w.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return httpResult{status: resp.StatusCode, header: resp.Header, body: body}
}

// mintPath is the route that mints a link for this world's file.
func (w *downloadWorld) mintPath() string {
	return fmt.Sprintf("/api/teams/%d/requests/%d/attachments/%d/download", w.team, w.request.ID, w.fileID)
}

// mint asks for a link and returns the URL's path and the answer.
func (w *downloadWorld) mint(token string) (string, httpResult) {
	w.t.Helper()
	r := w.do(token, http.MethodPost, w.mintPath())
	if r.status != 200 {
		return "", r
	}
	var body struct {
		Download struct {
			URL string `json:"url"`
		} `json:"download"`
	}
	if err := json.Unmarshal(r.body, &body); err != nil {
		w.t.Fatalf("decode mint: %v (%s)", err, r.body)
	}
	if body.Download.URL == "" {
		w.t.Fatalf("mint answered no url: %s", r.body)
	}
	return body.Download.URL, r
}

// TestRequestDownloadREST_ServesPrivateBytesOnce is criterion 3: the mint names a URL on
// AMP's own origin, following it returns exactly the uploaded bytes under the download
// posture, and the same URL a second time returns nothing.
func TestRequestDownloadREST_ServesPrivateBytesOnce(t *testing.T) {
	w := newDownloadWorld(t)
	url, minted := w.mint(w.requesterTok)
	if !strings.HasPrefix(url, "/api/request-downloads/") {
		t.Fatalf("minted url %q is not a path on AMP's own surface", url)
	}
	if strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://") {
		t.Fatalf("minted url %q is absolute (a storage address)", url)
	}
	if got := minted.header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("mint Cache-Control = %q, want no-store", got)
	}
	if strings.Contains(string(minted.body), w.requesterTok) {
		t.Fatal("the mint echoed the caller's credential")
	}

	served := w.do(w.requesterTok, http.MethodGet, url)
	if served.status != 200 {
		t.Fatalf("serve = %d %s", served.status, served.body)
	}
	if string(served.body) != string(w.content) {
		t.Fatalf("served %q, want the uploaded bytes", served.body)
	}
	if got := served.header.Get("Content-Disposition"); got != `attachment; filename="notes.txt"` {
		t.Fatalf("Content-Disposition = %q, want an attachment with the row's name", got)
	}
	if strings.Contains(strings.ToLower(served.header.Get("Content-Disposition")), "inline") {
		t.Fatal("a download was served inline")
	}
	if got := served.header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := served.header.Get("Content-Type"); got != "text/plain" {
		t.Fatalf("Content-Type = %q, want the sniffed type", got)
	}
	if got := served.header.Get("Cache-Control"); !strings.Contains(got, "no-store") || !strings.Contains(got, "private") {
		t.Fatalf("Cache-Control = %q, want private, no-store", got)
	}
	if got := served.header.Get("Content-Security-Policy"); got != "sandbox" {
		t.Fatalf("Content-Security-Policy = %q, want sandbox", got)
	}
	if got := served.header.Get("Referrer-Policy"); got != "no-referrer" {
		t.Fatalf("Referrer-Policy = %q", got)
	}

	// Single use: the same URL again is the surface's 404, and it carries no bytes.
	again := w.do(w.requesterTok, http.MethodGet, url)
	if again.status != 404 || len(again.body) == 0 || strings.Contains(string(again.body), "private notes") {
		t.Fatalf("a spent URL answered %d %q", again.status, again.body)
	}
}

// TestRequestDownloadREST_FileNameIsHeaderSafe proves the header work on a name that
// would otherwise break it: a quote, a semicolon and a non-ASCII letter. The fallback is
// quoted printable ASCII with the breakers neutralised, and the real name travels in
// filename* — never as a second parameter the fallback could have added.
func TestRequestDownloadREST_FileNameIsHeaderSafe(t *testing.T) {
	w := newDownloadWorld(t)
	name := "Rechnung März; \"x\".txt"
	att, err := w.svc.AddRequestAttachment(w.ctx, w.principal(w.requester.User.ID), service.AddRequestAttachmentInput{
		RequestID: w.request.ID, OrgID: w.request.OrgID, TeamID: w.request.TeamID,
		FileName: name, Content: []byte("a second private file\n"),
	})
	if err != nil {
		t.Fatalf("upload the awkward name: %v", err)
	}
	r := w.do(w.requesterTok, http.MethodPost,
		fmt.Sprintf("/api/teams/%d/requests/%d/attachments/%d/download", w.team, w.request.ID, att.ID))
	if r.status != 200 {
		t.Fatalf("mint = %d %s", r.status, r.body)
	}
	var body struct {
		Download struct {
			URL string `json:"url"`
		} `json:"download"`
	}
	if err := json.Unmarshal(r.body, &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	served := w.do(w.requesterTok, http.MethodGet, body.Download.URL)
	if served.status != 200 {
		t.Fatalf("serve = %d %s", served.status, served.body)
	}
	disp := served.header.Get("Content-Disposition")
	// The header must parse as exactly one attachment disposition with at most the two
	// name parameters: a semicolon inside the fallback would have added one.
	if strings.Count(disp, ";") > 2 {
		t.Fatalf("Content-Disposition grew parameters: %q", disp)
	}
	if !strings.HasPrefix(disp, "attachment; filename=\"") {
		t.Fatalf("Content-Disposition = %q", disp)
	}
	if !strings.Contains(disp, "filename*=UTF-8''") {
		t.Fatalf("the real name is not in filename*: %q", disp)
	}
	if strings.Contains(disp, "März") {
		t.Fatalf("non-ASCII reached the header raw: %q", disp)
	}
	if strings.Contains(disp, `\"`) {
		t.Fatalf("a quote reached the fallback: %q", disp)
	}
}

// TestRequestDownloadREST_RefusalsAreIndistinguishable is criterion 1 on the route: a
// token belonging to someone else, an unknown token, a foreign attachment's mint and a
// nonexistent request answer the same 404 body, so an id or a URL is never an oracle.
func TestRequestDownloadREST_RefusalsAreIndistinguishable(t *testing.T) {
	w := newDownloadWorld(t)

	// A live URL minted for the requester, presented by another customer: the grant's
	// person is part of the check, so this is a 404, not bytes.
	url, _ := w.mint(w.requesterTok)
	other := w.do(w.otherTok, http.MethodGet, url)
	if other.status != 404 {
		t.Fatalf("another customer's fetch of a requester's URL = %d %s", other.status, other.body)
	}
	// The same URL with no credential at all never reaches the handler: the verifier
	// answers, which is the "no bypass of the auth proxy" property.
	anon := w.do("", http.MethodGet, url)
	if anon.status != 401 && anon.status != 403 {
		t.Fatalf("an anonymous fetch of a download URL = %d %s", anon.status, anon.body)
	}

	// The unknown-token answer, which every refusal must match.
	unknown := w.do(w.requesterTok, http.MethodGet, "/api/request-downloads/"+strings.Repeat("A", 43))

	for what, got := range map[string]httpResult{"another person's token": other, "an unknown token": unknown} {
		if got.status != 404 {
			t.Fatalf("%s = %d %s", what, got.status, got.body)
		}
		if strings.TrimSpace(string(got.body)) != strings.TrimSpace(string(unknown.body)) {
			t.Fatalf("%s body %q differs from an unknown token's %q (an oracle)", what, got.body, unknown.body)
		}
	}

	// A stranger's mint for a file in another org, and a request id that does not exist:
	// the same 404 body the unknown token got.
	strangerMint := w.do(w.strangerTok, http.MethodPost, w.mintPath())
	if strangerMint.status != 404 {
		t.Fatalf("a stranger's mint = %d %s", strangerMint.status, strangerMint.body)
	}
	missing := w.do(w.requesterTok, http.MethodPost,
		fmt.Sprintf("/api/teams/%d/requests/%d/attachments/%d/download", w.team, w.request.ID+1_000_000, w.fileID))
	if missing.status != 404 {
		t.Fatalf("a nonexistent request's mint = %d %s", missing.status, missing.body)
	}
	if strings.TrimSpace(string(strangerMint.body)) != strings.TrimSpace(string(missing.body)) {
		t.Fatalf("a stranger's mint body %q differs from a missing request's %q", strangerMint.body, missing.body)
	}

	// A revoked URL is the same 404 as an unknown one, and the requester's own token is
	// unaffected by a revoke that named a different request.
	revokedURL, _ := w.mint(w.requesterTok)
	// An ordinary team member cannot revoke a requester's live link (an access act), so
	// the revoke here comes from the same route's authority class: an org admin. The
	// member's attempt is refused and leaves the URL working.
	if _, err := w.svc.RevokeRequestDownloadGrants(w.ctx, w.principal(w.member.User.ID),
		domain.RequestDownloadGrantRef{RequestID: w.request.ID, OrgID: w.request.OrgID, TeamID: w.request.TeamID}); err == nil {
		t.Fatal("a plain team member revoked another person's download link")
	}
	if served := w.do(w.requesterTok, http.MethodGet, revokedURL); served.status != 200 {
		t.Fatalf("a refused revoke killed the URL: %d %s", served.status, served.body)
	}
	revokedURL, _ = w.mint(w.requesterTok)
	if n, err := w.svc.RevokeRequestDownloadGrants(w.ctx, w.principal(w.admin.User.ID),
		domain.RequestDownloadGrantRef{RequestID: w.request.ID, OrgID: w.request.OrgID, TeamID: w.request.TeamID}); err != nil || n < 1 {
		t.Fatalf("revoke by an org admin = %d, %v", n, err)
	}
	revoked := w.do(w.requesterTok, http.MethodGet, revokedURL)
	if revoked.status != 404 || strings.TrimSpace(string(revoked.body)) != strings.TrimSpace(string(unknown.body)) {
		t.Fatalf("a revoked URL = %d %q, want the unknown-token 404", revoked.status, revoked.body)
	}
}

// TestRequestDownloadREST_TeamMemberAndCatalog exercises the other arm of the capability:
// a team member may mint and serve a plain document (no hold), and a team member is
// refused on a file they have no standing on — the second org's — with the same 404.
func TestRequestDownloadREST_TeamMemberAndCatalog(t *testing.T) {
	w := newDownloadWorld(t)
	url, minted := w.mint(w.memberTok)
	if minted.status != 200 {
		t.Fatalf("a team member's mint = %d %s", minted.status, minted.body)
	}
	if got := w.do(w.memberTok, http.MethodGet, url); got.status != 200 || string(got.body) != string(w.content) {
		t.Fatalf("a team member's serve = %d %q", got.status, got.body)
	}
	foreignMint := w.do(w.memberTok, http.MethodPost,
		fmt.Sprintf("/api/teams/%d/requests/%d/attachments/%d/download", w.foreign, w.request.ID, w.fileID))
	if foreignMint.status != 404 {
		t.Fatalf("a member's mint against another org's team = %d %s", foreignMint.status, foreignMint.body)
	}
}
