package repository

// The download-grant ledger (AMP #3963, migration 063): the properties that are about the
// *store* rather than the gate. The gate's behaviour is service/request_downloads_test.go's;
// this file asks what a service test cannot see:
//
//   - single use is atomic (two concurrent consumes of one hash yield exactly one row), not
//     read-then-write luck;
//   - a grant without a person, an attachment or a request is refused — no wildcard row and
//     no public row;
//   - deleting an attachment takes its grants with it (the FK cascade), so a URL cannot
//     outlive the bytes it names;
//   - revocation is a delete scoped to a tenant, and a reference with no selector is
//     refused rather than emptying the table;
//   - the guard test: this layer imports no net/http, names no disposition, no URL and no
//     scan verdict, so no header can be written three layers below the handler (the
//     request_attachments.go guard's twin).
//
// It reuses request_attachments_test.go's fixture (a request, a user, an org and a team),
// so the tenant setup is the one the other attachment suites already prove.
//
// Run against a disposable Postgres, per
// how-to/run-one-amp-api-test-with-disposable-postgres.md:
//
//	TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:PORT/amp_test?sslmode=disable \
//	  go test ./internal/repository -run 'TestRequestDownloadGrant' -count=1 -race -v

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/simstech/amp-api/internal/domain"
)

// grantFixture is the attachments fixture plus one uploaded file: everything a grant row
// needs to be a *valid* grant, so each test can break exactly one thing.
type grantFixture struct {
	*requestAttachmentsFixture
	req  *domain.TeamRequest
	file *domain.RequestAttachment
}

func newGrantFixture(t *testing.T) *grantFixture {
	t.Helper()
	f := &grantFixture{requestAttachmentsFixture: newRequestAttachmentsFixture(t)}
	f.req = f.newRequest("grant")
	f.file = f.mustInsert(f.req, "notes.txt", "text/plain", []byte("private\n"))
	return f
}

// token is a fresh, unique token string per call, so tests never share a ledger row.
func grantToken() string {
	b := sha256.Sum256([]byte(fmt.Sprintf("grant-%d", time.Now().UnixNano())))
	return base64.RawURLEncoding.EncodeToString(b[:])
}

func grantHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// grant is a valid row for this fixture's file, minted to its user and valid for an hour.
func (f *grantFixture) grant(token string) domain.RequestDownloadGrant {
	now := requestTime(0)
	return domain.RequestDownloadGrant{
		GrantHash:    grantHash(token),
		AttachmentID: f.file.ID,
		RequestID:    f.req.ID,
		OrgID:        f.req.OrgID,
		TeamID:       f.req.TeamID,
		UserID:       f.user,
		IssuedAt:     now,
		ExpiresAt:    now.Add(time.Hour),
	}
}

// TestRequestDownloadGrantSingleUseIsAtomic: two goroutines consume the same token at the
// same instant; exactly one wins and the row records one use. A read-then-write would let
// both through whenever the scheduler interleaved them, so this is the test that pins the
// UPDATE ... WHERE used_at IS NULL shape.
func TestRequestDownloadGrantSingleUseIsAtomic(t *testing.T) {
	f := newGrantFixture(t)
	token := grantToken()
	if _, err := f.r.InsertRequestDownloadGrant(f.ctx, f.grant(token)); err != nil {
		t.Fatalf("insert: %v", err)
	}
	hash := grantHash(token)
	now := requestTime(0)

	var wg sync.WaitGroup
	results := make([]error, 2)
	start := make(chan struct{})
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, results[i] = f.r.ConsumeRequestDownloadGrant(context.Background(), hash, now)
		}(i)
	}
	close(start)
	wg.Wait()

	won, refused := 0, 0
	for _, err := range results {
		switch {
		case err == nil:
			won++
		case errors.Is(err, ErrNotFound):
			refused++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if won != 1 || refused != 1 {
		t.Fatalf("consumes: %d won, %d refused; want exactly one of each", won, refused)
	}
	// Spent stays spent while still inside its window.
	if _, err := f.r.ConsumeRequestDownloadGrant(f.ctx, hash, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a spent grant was consumed again: %v", err)
	}
}

// TestRequestDownloadGrantRefusesWildcardRows: every shape that would make a grant broader
// than one file for one person is refused before the insert. The table's NOT NULLs are a
// backstop; this is the first line.
func TestRequestDownloadGrantRefusesWildcardRows(t *testing.T) {
	f := newGrantFixture(t)
	base := f.grant(grantToken())
	cases := map[string]func(g *domain.RequestDownloadGrant){
		"no person":              func(g *domain.RequestDownloadGrant) { g.UserID = 0 },
		"no attachment":          func(g *domain.RequestDownloadGrant) { g.AttachmentID = 0 },
		"no request":             func(g *domain.RequestDownloadGrant) { g.RequestID = 0 },
		"no org":                 func(g *domain.RequestDownloadGrant) { g.OrgID = 0 },
		"no team":                func(g *domain.RequestDownloadGrant) { g.TeamID = 0 },
		"expiry not after issue": func(g *domain.RequestDownloadGrant) { g.ExpiresAt = g.IssuedAt },
		"malformed hash":         func(g *domain.RequestDownloadGrant) { g.GrantHash = "not-a-hash" },
	}
	for name, edit := range cases {
		g := base
		edit(&g)
		if _, err := f.r.InsertRequestDownloadGrant(f.ctx, g); err == nil {
			t.Fatalf("%s: the insert accepted it", name)
		}
	}
	// And a duplicate hash is not storable: one token, one row.
	g := f.grant(grantToken())
	if _, err := f.r.InsertRequestDownloadGrant(f.ctx, g); err != nil {
		t.Fatalf("insert a fresh grant: %v", err)
	}
	if _, err := f.r.InsertRequestDownloadGrant(f.ctx, g); err == nil {
		t.Fatal("the same hash was stored twice")
	}
}

// TestRequestDownloadGrantLookupIsByHashOnly pins that the id is not an access path: a
// grant found by its hash round-trips, and a hash that cannot name a row is the ordinary
// not-found rather than a query.
func TestRequestDownloadGrantLookupIsByHashOnly(t *testing.T) {
	f := newGrantFixture(t)
	token := grantToken()
	written, err := f.r.InsertRequestDownloadGrant(f.ctx, f.grant(token))
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if written.UserID != f.user || written.AttachmentID != f.file.ID || written.UsedAt != nil {
		t.Fatalf("written grant = %+v", written)
	}
	got, err := f.r.GetRequestDownloadGrant(f.ctx, grantHash(token))
	if err != nil {
		t.Fatalf("get by hash: %v", err)
	}
	if got.ID != written.ID {
		t.Fatalf("got id %d, want %d", got.ID, written.ID)
	}
	h := grantHash(token)
	for _, junk := range []string{"", "abc", h[:63], h + "0", strings.ToUpper(h), h[:32] + "Z" + h[33:]} {
		if _, err := f.r.GetRequestDownloadGrant(f.ctx, junk); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Get %q = %v, want ErrNotFound", junk, err)
		}
	}
}

// TestRequestDownloadGrantsDieWithTheirAttachment: the FK cascade means a purge (or a
// deleted request) takes the live URLs with it, so a token cannot outlive the bytes.
func TestRequestDownloadGrantsDieWithTheirAttachment(t *testing.T) {
	f := newGrantFixture(t)
	token := grantToken()
	if _, err := f.r.InsertRequestDownloadGrant(f.ctx, f.grant(token)); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := f.r.DeleteRequestAttachment(f.ctx, f.attRef(f.req, f.file)); err != nil {
		t.Fatalf("delete the attachment: %v", err)
	}
	if _, err := f.r.GetRequestDownloadGrant(f.ctx, grantHash(token)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the grant outlived its attachment: %v", err)
	}
}

// TestRequestDownloadGrantRevoke: a revoke needs a selector, is scoped to its tenant, and
// deletes exactly the unspent rows it names.
func TestRequestDownloadGrantRevoke(t *testing.T) {
	f := newGrantFixture(t)
	fresh, spent := grantToken(), grantToken()
	for _, token := range []string{fresh, spent} {
		if _, err := f.r.InsertRequestDownloadGrant(f.ctx, f.grant(token)); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	if _, err := f.r.ConsumeRequestDownloadGrant(f.ctx, grantHash(spent), requestTime(0)); err != nil {
		t.Fatalf("consume: %v", err)
	}

	// No selector: refused, and nothing deleted.
	if _, err := f.r.RevokeRequestDownloadGrants(f.ctx, domain.RequestDownloadGrantRef{}); err == nil {
		t.Fatal("a revoke with no selector was accepted")
	}
	// A narrow selector without its tenant: refused, so a guessed id cannot reach far.
	if _, err := f.r.RevokeRequestDownloadGrants(f.ctx, domain.RequestDownloadGrantRef{RequestID: f.req.ID}); !errors.Is(err, ErrRequestTenantScope) {
		t.Fatalf("a tenant-less revoke = %v, want ErrRequestTenantScope", err)
	}
	n, err := f.r.RevokeRequestDownloadGrants(f.ctx, domain.RequestDownloadGrantRef{
		RequestID: f.req.ID, OrgID: f.req.OrgID, TeamID: f.req.TeamID,
	})
	if err != nil || n != 1 {
		t.Fatalf("revoke = %d, %v; want exactly the one unspent grant", n, err)
	}
	if _, err := f.r.GetRequestDownloadGrant(f.ctx, grantHash(fresh)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a revoked grant is still there: %v", err)
	}
	if _, err := f.r.GetRequestDownloadGrant(f.ctx, grantHash(spent)); err != nil {
		t.Fatalf("the revoke deleted a spent grant: %v", err)
	}
}

// TestRequestDownloadGrantExpiryHousekeeping: the sweep deletes only grants past their
// window — a live one survives, because the cleanup is tidiness and never a security step.
func TestRequestDownloadGrantExpiryHousekeeping(t *testing.T) {
	f := newGrantFixture(t)
	live, old := grantToken(), grantToken()
	if _, err := f.r.InsertRequestDownloadGrant(f.ctx, f.grant(live)); err != nil {
		t.Fatalf("insert live: %v", err)
	}
	expired := f.grant(old)
	// A row minted an hour ago and already expired: the table allows it (the service never
	// mints one), which is exactly the state the sweep exists to clean up.
	expired.IssuedAt, expired.ExpiresAt = requestTime(-2*time.Hour), requestTime(-time.Hour)
	if _, err := f.r.InsertRequestDownloadGrant(f.ctx, expired); err != nil {
		t.Fatalf("insert expired: %v", err)
	}
	if _, err := f.r.DeleteExpiredRequestDownloadGrants(f.ctx, requestTime(0)); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if _, err := f.r.GetRequestDownloadGrant(f.ctx, grantHash(live)); err != nil {
		t.Fatalf("the sweep deleted a live grant: %v", err)
	}
	if _, err := f.r.GetRequestDownloadGrant(f.ctx, grantHash(old)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the sweep left an expired grant: %v", err)
	}
}

// TestRequestDownloadGrantStoreStaysALedger parses this file's AST (not its text, which is
// allowed to explain these rules) and fails if the store grows a way to write a header, log
// a token or decide that bytes may be served.
func TestRequestDownloadGrantStoreStaysALedger(t *testing.T) {
	const file = "request_download_grants.go"
	fs := token.NewFileSet()
	f, err := parser.ParseFile(fs, file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	allowed := map[string]bool{"context": true, "errors": true, "fmt": true, "time": true,
		"github.com/jackc/pgx/v5": true, "github.com/simstech/amp-api/internal/domain": true}
	for _, imp := range f.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		if !allowed[path] {
			t.Fatalf("%s imports %q: a grant store is a ledger, not a serving layer", file, path)
		}
	}
	banned := map[string]bool{
		"DetectContentType": true, "SetHeader": true, "WriteHeader": true, "slog": true, "log": true,
		"RequestScanClean": true, "ContentDisposition": true, "RequestAttachmentDisposition": true,
		"RequestAttachmentDownloadRefusal": true, "ContentType": true,
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.Ident:
			if banned[x.Name] {
				t.Fatalf("%s names %q: the store must not decide serving, logging or a verdict", file, x.Name)
			}
		case *ast.BasicLit:
			if x.Kind == token.STRING {
				for _, s := range []string{"http://", "https://", "Content-Disposition", "/api/"} {
					if strings.Contains(x.Value, s) {
						t.Fatalf("%s has the string literal %q: no URL and no header three layers below the handler", file, s)
					}
				}
			}
		}
		return true
	})
}
