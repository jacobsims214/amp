package service

// The attachment download gate (epic 202, story 549, AMP #3963; build slice G3's
// serving half; approved contract artifact 36 v2 §1 and decision D3).
//
// The ticket's three criteria, and where each is answered:
//
//	1. Expired / revoked / foreign grants refused:
//	   TestServeRequestDownloadRefusesDeadGrants (expired, spent, revoked-by-delete,
//	   another person's token, an unknown token) and
//	   TestDownloadCapabilityRefusesForeignCallers (a stranger, a sibling team's
//	   member, another requester, a nil request — all the surface's one 404).
//	2. The scan/hold matrix:
//	   TestAttachmentDownloadRefusalRefused (pending/infected/purged/held refused, with
//	   the matrix source being domain.RequestAttachmentDownloadRefusal — this test pins
//	   that the *gate* honors every arm of it, not that the function exists) and
//	   TestHeldFileIsServedToALeadOnly (the team's plain member needs the lead check;
//	   a lead and an org admin pass).
//	3. Headers and private-byte access:
//	   TestCreateRequestDownloadGrantServesBytes (mint → serve returns exactly the
//	   bytes, once; the response record carries the sniffed type and name, and the
//	   audit rows carry ids but never the token) is here; the headers are
//	   cmd/server/request_download_rest_test.go's, because they are written by the
//	   handler and proving them anywhere else would prove a copy.
//
// Every test here needs Postgres (the grant ledger, the attachment rows, the request
// rows): it reuses request_uploads_test.go's fixture, which is request_submit_test.go's
// tenant plus a sibling-team member. The pure capability and state functions are
// table-tested without a database.
//
// Run against a disposable Postgres, never a live stack, per
// how-to/run-one-amp-api-test-with-disposable-postgres.md:
//
//	TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:PORT/amp_test?sslmode=disable \
//	  go test ./internal/service -run 'TestRequestDownload|TestServeRequestDownload|TestAttachmentDownload|TestHeldFile|TestDownloadCapability|TestCreateRequestDownloadGrant' -count=1 -race

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/repository"
)

// ---- pure decisions ----

// TestDownloadCapabilityRefusesForeignCallers is the visibility matrix, with no
// database: the requester, a team member and nobody. Every "nobody" is the surface's one
// not-found, so a foreign attachment and a missing one are the same answer.
func TestDownloadCapabilityRefusesForeignCallers(t *testing.T) {
	requester := 11
	other := 22
	req := &domain.TeamRequest{ID: 7, OrgID: 1, TeamID: 2, RequesterUserID: &requester}
	connectorRequest := &domain.TeamRequest{ID: 8, OrgID: 1, TeamID: 2}
	me := func(id int) authz.Principal { return authz.Principal{UserID: id, Email: "u@example.test"} }

	cases := []struct {
		name string
		r    *domain.TeamRequest
		p    authz.Principal
		role authz.InboxRole
		want RequestDownloadCapability
	}{
		{"the requester, no team role", req, me(requester), authz.InboxNone, RequestDownloadRequester},
		{"a team member", req, me(other), authz.InboxMember, RequestDownloadTeam},
		{"a team member who is also the requester", req, me(requester), authz.InboxMember, RequestDownloadTeam},
		{"another requester", req, me(other), authz.InboxNone, RequestDownloadNone},
		{"a stranger", req, me(999), authz.InboxNone, RequestDownloadNone},
		{"a connector request belongs to nobody's own view", connectorRequest, me(other), authz.InboxNone, RequestDownloadNone},
		{"local dev with no user is not a requester", req, authz.Principal{Super: true}, authz.InboxMember, RequestDownloadTeam},
		{"no user and no role", req, authz.Principal{}, authz.InboxNone, RequestDownloadNone},
		{"nil request", nil, me(requester), authz.InboxMember, RequestDownloadNone},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := RequestDownloadCapabilityFor(c.r, c.p, c.role); got != c.want {
				t.Fatalf("capability = %s, want %s", got, c.want)
			}
		})
	}
}

// TestAttachmentDownloadRefusalRefused is the scan/hold matrix as the *gate* applies it:
// each state the domain names maps to a refusal, and only Served is ready.
func TestAttachmentDownloadRefusalRefused(t *testing.T) {
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	past := now.Add(-time.Hour)
	future := now.Add(time.Hour)
	base := func() *domain.RequestAttachment {
		return &domain.RequestAttachment{
			ID: 1, RequestID: 2, FileName: "notes.txt", ContentType: "text/plain",
			SizeBytes: 5, SHA256: strings.Repeat("a", 64), ScanState: domain.RequestScanUnsupported,
		}
	}
	cases := []struct {
		name string
		edit func(*domain.RequestAttachment)
		want domain.RequestAttachmentRefusal
	}{
		{"unsupported is served but never clean", nil, domain.RequestAttachmentServed},
		{"clean is served", func(a *domain.RequestAttachment) { a.ScanState = domain.RequestScanClean }, domain.RequestAttachmentServed},
		{"pending is refused", func(a *domain.RequestAttachment) { a.ScanState = domain.RequestScanPending }, domain.RequestAttachmentRefusedPending},
		{"infected is refused", func(a *domain.RequestAttachment) { a.ScanState = domain.RequestScanInfected }, domain.RequestAttachmentRefusedInfected},
		{"an unknown state fails closed", func(a *domain.RequestAttachment) { a.ScanState = "scanned-ish" }, domain.RequestAttachmentRefusedPending},
		{"a held archive is refused", func(a *domain.RequestAttachment) {
			a.FileName, a.ContentType = "bundle.zip", "application/zip"
		}, domain.RequestAttachmentRefusedHeld},
		{"a held executable is refused", func(a *domain.RequestAttachment) {
			a.FileName, a.ContentType = "setup.exe", "application/octet-stream"
		}, domain.RequestAttachmentRefusedHeld},
		{"a script held by name is refused", func(a *domain.RequestAttachment) { a.FileName = "deploy.sh" }, domain.RequestAttachmentRefusedHeld},
		{"a released archive is served", func(a *domain.RequestAttachment) {
			a.FileName, a.ContentType, a.ReleasedAt = "bundle.zip", "application/zip", &past
		}, domain.RequestAttachmentServed},
		{"a purged file is refused", func(a *domain.RequestAttachment) { a.ExpiresAt = &past }, domain.RequestAttachmentRefusedPurged},
		{"a future expiry is served", func(a *domain.RequestAttachment) { a.ExpiresAt = &future }, domain.RequestAttachmentServed},
		{"a kept file past its window is served", func(a *domain.RequestAttachment) {
			a.ExpiresAt, a.Keep = &past, true
		}, domain.RequestAttachmentServed},
		{"infected outranks a hold", func(a *domain.RequestAttachment) {
			a.ScanState, a.FileName = domain.RequestScanInfected, "deploy.sh"
		}, domain.RequestAttachmentRefusedInfected},
		{"a hold outranks a purge", func(a *domain.RequestAttachment) {
			a.FileName, a.ExpiresAt = "deploy.sh", &past
		}, domain.RequestAttachmentRefusedHeld},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := base()
			if c.edit != nil {
				c.edit(a)
			}
			state := RequestAttachmentDownloadStateAt(a, now)
			if state.Refusal != c.want {
				t.Fatalf("refusal = %q, want %q", state.Refusal, c.want)
			}
			if wantReady := c.want == domain.RequestAttachmentServed; state.Ready != wantReady {
				t.Fatalf("ready = %v, want %v", state.Ready, wantReady)
			}
			// The surface's answer for a refused state is a coded 4xx that names the
			// reason and nothing else; a served state has no error at all.
			if c.want != domain.RequestAttachmentServed {
				err := requestDownloadRefusal(a, c.want)
				if got := authz.StatusOf(err, 0); got != 403 {
					t.Fatalf("status = %d, want 403 (%v)", got, err)
				}
				if got := authz.CodeOf(err); got != authz.CodeRequestInvalid {
					t.Fatalf("code = %q, want %q", got, authz.CodeRequestInvalid)
				}
				if strings.Contains(err.Error(), "clean") {
					t.Fatalf("refusal message claims a scan result: %q", err)
				}
			}
		})
	}
}

// TestDownloadGrantPathIsNotAGuessableOrAbsoluteURL pins the two properties of a minted
// URL that are visible without a database: it is a path on AMP's own origin, and the token
// in it is not derived from the attachment or request id.
func TestDownloadGrantPathIsNotAGuessableOrAbsoluteURL(t *testing.T) {
	tok, hash, err := newDownloadGrantToken()
	if err != nil {
		t.Fatalf("mint a token: %v", err)
	}
	if len(tok) != 43 {
		t.Fatalf("token length = %d, want 43 (32 bytes base64url)", len(tok))
	}
	if strings.ContainsAny(tok, "+/=") {
		t.Fatalf("token %q is not base64url without padding", tok)
	}
	if hashDownloadGrantToken(tok) != hash {
		t.Fatal("the stored hash is not the token's sha256")
	}
	path := RequestDownloadGrantPath(tok)
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		t.Fatalf("download path %q is absolute; it must go through AMP's own surface", path)
	}
	if !strings.HasPrefix(path, "/api/request-downloads/") {
		t.Fatalf("download path %q is not under the serve route", path)
	}
	if got := RequestDownloadGrantToken(path); got != tok {
		t.Fatalf("token round-trip = %q, want %q", got, tok)
	}
	if got := RequestDownloadGrantToken("/api/tasks/7"); got != "" {
		t.Fatalf("a foreign path answered a token %q", got)
	}
	// Two mints of the same attachment differ: nothing about the token is derivable
	// from what a caller already knows.
	tok2, _, err := newDownloadGrantToken()
	if err != nil || tok2 == tok {
		t.Fatalf("two mints produced the same token (%v)", err)
	}
}

// ---- database-backed: mint, serve, refuse ----

// downloadFixture is the upload fixture plus the pieces the download gate's tests need:
// a request with a file on it, and the people who may and may not have it.
type downloadFixture struct {
	*uploadFixture
	// lead is an org member with the lead role on the inbox team.
	lead int
}

func newDownloadFixture(t *testing.T) *downloadFixture {
	t.Helper()
	f := &downloadFixture{uploadFixture: newUploadFixture(t)}
	f.lead = f.newUser("lead", domain.UserKindHuman)
	f.exec(`INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'member')`, f.org, f.lead)
	f.exec(`INSERT INTO team_members (team_id, user_id, role) VALUES ($1, $2, 'lead')`, f.team, f.lead)
	return f
}

// requestWithFile submits a request as userID and attaches content named name.
func (f *downloadFixture) requestWithFile(userID int, name string, content []byte) (*domain.TeamRequest, *domain.RequestAttachment) {
	f.t.Helper()
	req := f.newRequest(userID, "A request with a file "+f.suffix())
	att, err := f.upload(userID, req, name, content)
	if err != nil {
		f.t.Fatalf("upload %q: %v", name, err)
	}
	return req, att
}

// mint is one mint through the service.
func (f *downloadFixture) mint(userID int, req *domain.TeamRequest, attachmentID int64) (*RequestDownloadURL, error) {
	f.t.Helper()
	return f.svc.CreateRequestDownloadGrant(f.ctx, f.principal(userID), RequestDownloadGrantInput{
		RequestID: req.ID, AttachmentID: attachmentID, OrgID: req.OrgID, TeamID: req.TeamID,
	})
}

// serve is one serve through the service.
func (f *downloadFixture) serve(userID int, token string) (*domain.RequestAttachment, error) {
	f.t.Helper()
	_, att, err := f.svc.ServeRequestDownload(f.ctx, f.principal(userID), token)
	if att != nil {
		return att, err
	}
	return nil, err
}

// wantNotFound asserts the surface's single 404 (no code, no detail) — the answer for a
// foreign attachment exactly as for an unknown one.
func wantDownloadNotFound(t *testing.T, what string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: expected a refusal, got none", what)
	}
	if got := authz.StatusOf(err, 0); got != 404 {
		t.Fatalf("%s: status = %d (%v), want 404", what, got, err)
	}
	if got, want := err.Error(), authz.RequestNotFound().Error(); got != want {
		t.Fatalf("%s: message = %q, want %q (a different message is an oracle)", what, got, want)
	}
}

// TestCreateRequestDownloadGrantServesBytes is criterion 3's service half: a mint
// produces a URL, the URL serves exactly the bytes that were uploaded, and it does so
// once — the second attempt is refused even though the grant has not expired.
func TestCreateRequestDownloadGrantServesBytes(t *testing.T) {
	f := newDownloadFixture(t)
	content := []byte("the requester's private notes\n")
	req, att := f.requestWithFile(f.customer, "notes.txt", content)

	grant, err := f.mint(f.customer, req, att.ID)
	if err != nil {
		t.Fatalf("mint as the requester: %v", err)
	}
	if grant.Token == "" || !strings.HasPrefix(grant.URL, "/api/request-downloads/") {
		t.Fatalf("mint answered token %q url %q", grant.Token, grant.URL)
	}
	if want := time.Duration(domain.RequestDownloadGrantTTL); grant.ExpiresAt.Sub(f.now) > want+time.Second || grant.ExpiresAt.Sub(f.now) < want-time.Second {
		t.Fatalf("expires_at = %s, want about %s from now", grant.ExpiresAt, want)
	}
	if grant.ContentType != "text/plain" || grant.FileName != "notes.txt" {
		t.Fatalf("mint metadata = %q %q", grant.FileName, grant.ContentType)
	}

	served, err := f.serve(f.customer, grant.Token)
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	if string(served.Content) != string(content) {
		t.Fatalf("served bytes = %q, want %q (private bytes must be the uploaded ones)", served.Content, content)
	}
	if served.ContentType != "text/plain" || served.FileName != "notes.txt" {
		t.Fatalf("served record = %q %q", served.FileName, served.ContentType)
	}

	// Single use: the same URL again is the surface's not-found.
	_, err = f.serve(f.customer, grant.Token)
	wantDownloadNotFound(t, "a spent grant", err)
}

// TestServeRequestDownloadRefusesDeadGrants is criterion 1, part one: expired, spent,
// revoked (deleted) and unknown grants are all refused, and refused as the one 404.
func TestServeRequestDownloadRefusesDeadGrants(t *testing.T) {
	f := newDownloadFixture(t)
	req, att := f.requestWithFile(f.customer, "notes.txt", []byte("private\n"))

	t.Run("expired", func(t *testing.T) {
		grant, err := f.mint(f.customer, req, att.ID)
		if err != nil {
			t.Fatalf("mint: %v", err)
		}
		f.withClock(f.now.Add(domain.RequestDownloadGrantTTL + time.Second))
		_, err = f.serve(f.customer, grant.Token)
		wantDownloadNotFound(t, "an expired grant", err)
	})

	t.Run("revoked by delete", func(t *testing.T) {
		grant, err := f.mint(f.customer, req, att.ID)
		if err != nil {
			t.Fatalf("mint: %v", err)
		}
		// Revoking another person's live link is an access act: a plain member and the
		// requester herself are refused; the team's lead may.
		ref := domain.RequestDownloadGrantRef{RequestID: req.ID, OrgID: req.OrgID, TeamID: req.TeamID}
		for who, userID := range map[string]int{"a plain team member": f.member, "the requester": f.customer} {
			if _, err := f.svc.RevokeRequestDownloadGrants(f.ctx, f.principal(userID), ref); err == nil {
				t.Fatalf("%s revoked another person's download link", who)
			}
		}
		n, err := f.svc.RevokeRequestDownloadGrants(f.ctx, f.principal(f.lead), ref)
		if err != nil || n < 1 {
			t.Fatalf("revoke by the lead = %d, %v (want at least this grant)", n, err)
		}
		_, err = f.serve(f.customer, grant.Token)
		wantDownloadNotFound(t, "a revoked grant", err)
	})

	t.Run("another person's token", func(t *testing.T) {
		grant, err := f.mint(f.customer, req, att.ID)
		if err != nil {
			t.Fatalf("mint: %v", err)
		}
		// The lead is allowed this request, so the refusal is the grant's person — not
		// a capability failure. That is the property: possession is not authorization.
		_, err = f.serve(f.lead, grant.Token)
		wantDownloadNotFound(t, "another person's token", err)
	})

	t.Run("unknown and malformed tokens", func(t *testing.T) {
		for _, token := range []string{"", "not-a-token", strings.Repeat("a", 43)} {
			_, err := f.serve(f.customer, token)
			wantDownloadNotFound(t, fmt.Sprintf("token %q", token), err)
		}
	})

	t.Run("a token serves only the file it names", func(t *testing.T) {
		// Two requests, one file each; each token serves its own bytes and never the
		// other's — the grant is one attachment, not a capability over the request.
		otherReq, otherAtt := f.requestWithFile(f.customer, "other.txt", []byte("other\n"))
		grant, err := f.mint(f.customer, otherReq, otherAtt.ID)
		if err != nil {
			t.Fatalf("mint the other: %v", err)
		}
		served, err := f.serve(f.customer, grant.Token)
		if err != nil {
			t.Fatalf("serve the other: %v", err)
		}
		if served.ID == att.ID || served.RequestID != otherReq.ID || string(served.Content) != "other\n" {
			t.Fatalf("a token for request %d served attachment %d of request %d", otherReq.ID, served.ID, served.RequestID)
		}
	})
}

// TestHeldFileIsRefusedUntilReleased is criterion 2's hold half: an archive is stored
// (the upload path accepts it) but refused to EVERY reader — the requester included —
// until a lead has released it; after the release the request's ordinary view admits it
// and the requester gets their own file back. The refusal names the hold rather than
// pretending the file is missing, because the caller can already see it in the panel.
func TestHeldFileIsRefusedUntilReleased(t *testing.T) {
	f := newDownloadFixture(t)
	req, att := f.requestWithFile(f.customer, "bundle.zip", zipBytes())

	for who, userID := range map[string]int{
		"the requester": f.customer,
		"a team member": f.member,
		"the team lead": f.lead,
		"an org admin":  f.admin,
	} {
		grant, err := f.mint(userID, req, att.ID)
		if err == nil {
			t.Fatalf("%s minted a link for a held file: %+v", who, grant)
		}
		if authz.StatusOf(err, 0) != 403 || !strings.Contains(err.Error(), "held") {
			t.Fatalf("%s: held mint = %v, want a 403 naming the hold", who, err)
		}
	}

	// The hold outlives a minted link: a URL issued before the file became held (here:
	// released and then not) is re-checked at serve time, never trusted.
	f.exec(`UPDATE request_attachments SET released_at = NOW(), released_by = $2 WHERE id = $1`, att.ID, f.admin)
	grant, err := f.mint(f.customer, req, att.ID)
	if err != nil {
		t.Fatalf("the requester's released file was refused: %v", err)
	}
	// Un-release it (the row's state changes under a live URL, as an altered verdict or
	// a retention stamp would) and the URL stops working.
	f.exec(`UPDATE request_attachments SET released_at = NULL, released_by = NULL WHERE id = $1`, att.ID)
	_, err = f.serve(f.customer, grant.Token)
	if err == nil {
		t.Fatal("a URL minted for a released file served it after the release was removed")
	}

	// Released, the file is served to the requester and readable as bytes.
	f.exec(`UPDATE request_attachments SET released_at = NOW(), released_by = $2 WHERE id = $1`, att.ID, f.admin)
	grant, err = f.mint(f.customer, req, att.ID)
	if err != nil {
		t.Fatalf("a released file was refused to the requester: %v", err)
	}
	served, err := f.serve(f.customer, grant.Token)
	if err != nil {
		t.Fatalf("serve a released file: %v", err)
	}
	if string(served.Content) != string(zipBytes()) {
		t.Fatal("served bytes are not the released file's")
	}
}

// TestDownloadRefusalByScanState is criterion 2's scan half against real rows: a pending
// or infected file is refused with a 403 naming the reason, an unsupported one is served,
// and a purged one is refused — the state read from the database each time, never cached.
func TestDownloadRefusalByScanState(t *testing.T) {
	for _, c := range []struct {
		state domain.RequestScanState
		want  string // the word the refusal must name; "" means served
	}{
		{domain.RequestScanPending, "scanned"},
		{domain.RequestScanInfected, "flagged"},
		{domain.RequestScanUnsupported, ""},
		{domain.RequestScanClean, ""},
	} {
		t.Run(string(c.state), func(t *testing.T) {
			f := newDownloadFixture(t)
			req, att := f.requestWithFile(f.customer, "notes.txt", htmlBytes())
			f.exec(`UPDATE request_attachments SET scan_state = $2 WHERE id = $1`, att.ID, string(c.state))
			grant, err := f.mint(f.customer, req, att.ID)
			if c.want == "" {
				if err != nil {
					t.Fatalf("mint a %s file: %v", c.state, err)
				}
				if _, err := f.serve(f.customer, grant.Token); err != nil {
					t.Fatalf("serve a %s file: %v", c.state, err)
				}
				return
			}
			if err == nil {
				t.Fatal("minted a link for a refused state")
			}
			if authz.StatusOf(err, 0) != 403 || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("refusal = %v, want a 403 naming %q", err, c.want)
			}
		})
	}
}

// TestServeRequestDownloadRechecksTheCaller is the "auth recheck/revocation" half of the
// ticket: a URL minted while the caller was allowed stops working when the reason they
// were allowed goes away. Losing the team membership is the case that matters; it is
// checked from the rows on every serve, so it takes effect on the very next click.
func TestServeRequestDownloadRechecksTheCaller(t *testing.T) {
	f := newDownloadFixture(t)
	req, att := f.requestWithFile(f.customer, "notes.txt", []byte("private\n"))

	grant, err := f.mint(f.member, req, att.ID)
	if err != nil {
		t.Fatalf("mint as a team member: %v", err)
	}
	// The same member's URL, served by a team member, works...
	if _, err := f.serve(f.member, grant.Token); err != nil {
		t.Fatalf("serve as a team member: %v", err)
	}

	// ...and a *second* grant stops working once the member leaves the team.
	grant2, err := f.mint(f.member, req, att.ID)
	if err != nil {
		t.Fatalf("mint a second link: %v", err)
	}
	f.exec(`DELETE FROM team_members WHERE team_id = $1 AND user_id = $2`, f.team, f.member)
	_, err = f.serve(f.member, grant2.Token)
	wantDownloadNotFound(t, "a URL minted before the membership ended", err)
}

// TestDownloadGrantMintRefusesForeignCallers is criterion 1's foreign half through the
// mint: a stranger, a sibling team's member and another requester cannot mint a URL for a
// file that is not theirs — and each gets the same not-found an unknown request gets.
func TestDownloadGrantMintRefusesForeignCallers(t *testing.T) {
	f := newDownloadFixture(t)
	req, att := f.requestWithFile(f.customer, "notes.txt", []byte("private\n"))

	for who, userID := range map[string]int{
		"a sibling team's member":        f.sibling,
		"a stranger from another org":    f.stranger,
		"another customer with no grant": f.otherCust,
	} {
		_, err := f.mint(userID, req, att.ID)
		wantDownloadNotFound(t, who, err)
	}

	// A foreign request (another org's) and the same attachment id answered for the
	// wrong tenant are also the one 404, not a hint that the pair exists elsewhere.
	foreign := f.newForeignRequest()
	_, err := f.svc.CreateRequestDownloadGrant(f.ctx, f.principal(f.stranger), RequestDownloadGrantInput{
		RequestID: foreign.ID, AttachmentID: att.ID, OrgID: foreign.OrgID, TeamID: foreign.TeamID,
	})
	wantDownloadNotFound(t, "an attachment id from another tenant", err)
}

// TestServeRequestDownloadRefusesAnAttachmentThatWentAway proves the grant is re-checked
// against the rows rather than trusted to name something: the file is deleted between the
// mint and the serve (a purge does exactly this), and the URL refuses.
func TestServeRequestDownloadRefusesAnAttachmentThatWentAway(t *testing.T) {
	f := newDownloadFixture(t)
	req, att := f.requestWithFile(f.customer, "notes.txt", []byte("private\n"))
	grant, err := f.mint(f.customer, req, att.ID)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	f.exec(`DELETE FROM request_attachments WHERE id = $1`, att.ID)
	_, err = f.serve(f.customer, grant.Token)
	wantDownloadNotFound(t, "an attachment deleted after the mint", err)

	// The grant's own row went with it (the FK cascade), so the token is simply gone.
	if _, err := f.svc.Repo.GetRequestDownloadGrant(f.ctx, hashDownloadGrantToken(grant.Token)); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("the grant row outlived its attachment: %v", err)
	}
}

// TestDownloadAuditNamesIDsNeverTheToken pins the ticket's "no token in logs/audits": the
// mint and the serve each write one audit row naming ids and facts, and neither row's
// details contains the token or a URL.
func TestDownloadAuditNamesIDsNeverTheToken(t *testing.T) {
	f := newDownloadFixture(t)
	req, att := f.requestWithFile(f.customer, "notes.txt", []byte("private\n"))
	grant, err := f.mint(f.customer, req, att.ID)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if _, err := f.serve(f.customer, grant.Token); err != nil {
		t.Fatalf("serve: %v", err)
	}
	for _, action := range []string{"request.attachment.download_grant", "request.attachment.download"} {
		rows, err := f.svc.Repo.ListAudit(f.ctx, domain.AuditFilter{Action: action, Limit: 50})
		if err != nil {
			t.Fatalf("read audit %s: %v", action, err)
		}
		var found bool
		for _, row := range rows {
			if row.OrgID == nil || *row.OrgID != req.OrgID {
				continue
			}
			found = true
			var dump string
			if err := f.db.QueryRow(f.ctx, `SELECT details::text FROM audit_log WHERE id = $1`, row.ID).Scan(&dump); err != nil {
				t.Fatalf("read details: %v", err)
			}
			if strings.Contains(dump, grant.Token) {
				t.Fatalf("%s audit row holds the download token", action)
			}
			if strings.Contains(dump, "filename=") || strings.Contains(dump, "/api/request-downloads/") {
				t.Fatalf("%s audit row holds a download URL", action)
			}
		}
		if !found {
			t.Fatalf("no %s audit row for the org", action)
		}
	}
}

// TestListRequestAttachmentsForDownloadMarksHeldFiles proves the panel read (no bytes) uses
// the same gate: a held file is marked held and not ready for a plain member, while the
// requester's own file is ready — and a stranger cannot list them at all.
func TestListRequestAttachmentsForDownloadMarksHeldFiles(t *testing.T) {
	f := newDownloadFixture(t)
	req, _ := f.requestWithFile(f.customer, "notes.txt", []byte("private\n"))
	if _, err := f.upload(f.customer, req, "bundle.zip", zipBytes()); err != nil {
		t.Fatalf("upload an archive: %v", err)
	}
	_, entries, err := f.svc.ListRequestAttachmentsForDownload(f.ctx, f.principal(f.customer), req.ID, req.OrgID, req.TeamID)
	if err != nil {
		t.Fatalf("list as the requester: %v", err)
	}
	byName := map[string]RequestAttachmentPanelEntry{}
	for _, e := range entries {
		byName[e.Attachment.FileName] = e
	}
	if !byName["notes.txt"].State.Ready || byName["notes.txt"].State.Held {
		t.Fatalf("notes.txt state = %+v, want ready and not held", byName["notes.txt"].State)
	}
	if byName["bundle.zip"].State.Ready || !byName["bundle.zip"].State.Held {
		t.Fatalf("bundle.zip state = %+v, want held and not ready", byName["bundle.zip"].State)
	}
	if byName["bundle.zip"].State.Refusal != domain.RequestAttachmentRefusedHeld {
		t.Fatalf("bundle.zip refusal = %q", byName["bundle.zip"].State.Refusal)
	}
	// Every entry's bytes are absent: a panel never loads content.
	for _, e := range entries {
		if e.Attachment.Content != nil {
			t.Fatalf("the panel loaded bytes for %s", e.Attachment.FileName)
		}
	}
	if _, _, err := f.svc.ListRequestAttachmentsForDownload(f.ctx, f.principal(f.sibling), req.ID, req.OrgID, req.TeamID); err == nil {
		t.Fatal("a sibling team's member listed another team's attachments")
	}

	// Every reader sees the same state for a held file — the hold is a state, not a
	// per-caller permission — so the lead's panel says held too until a release.
	for who, userID := range map[string]int{"a team member": f.member, "the team lead": f.lead} {
		_, entries, err = f.svc.ListRequestAttachmentsForDownload(f.ctx, f.principal(userID), req.ID, req.OrgID, req.TeamID)
		if err != nil {
			t.Fatalf("list as %s: %v", who, err)
		}
		for _, e := range entries {
			if e.Attachment.FileName == "bundle.zip" && e.State.Ready {
				t.Fatalf("%s's panel said a held file was ready", who)
			}
		}
	}
}
