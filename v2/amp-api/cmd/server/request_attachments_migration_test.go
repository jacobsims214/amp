package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/repository"
)

// Migration 056 adds request_attachments, the private files a requester sends
// with a request (AMP #3759; approved contract artifact 36 v2 §1 and decision
// D3, build slice G1).
//
// These tests prove what the schema is responsible for, on a real database, so
// none of them depends on the upload service, the download path or the purge job
// that will later use the table:
//
//   - it applies both ways (fresh, and on top of 055 with existing work), and
//     re-running its SQL by hand adds nothing;
//   - the schema accepts exactly the rows domain.RequestAttachment.Validate
//     accepts — every rule in one and not the other fails here;
//   - scan_state is the contract's closed set and defaults to "no verdict",
//     never to "clean";
//   - size_bytes is positive, inside the per-file bound, and equal to the bytes'
//     own length;
//   - content_type is a type the sniffer can produce, and the name is a display
//     name rather than a path;
//   - a release is only storable on a file that is actually held, and the schema
//     and domain.RequestAttachment.Held agree about which files those are;
//   - deleting the request takes its attachments and their bytes with it, while
//     deleting a user never deletes a file;
//   - there is no second byte store and no URL to fetch: an attachment is not an
//     artifact and never reuses one.

// attachmentsDB is the 053 fixture (two orgs, two teams, a user) plus helpers for
// request_attachments.
type attachmentsDB struct{ *teamRequestsDB }

func newAttachmentsDB(t *testing.T) *attachmentsDB {
	t.Helper()
	return &attachmentsDB{newTeamRequestsDB(t)}
}

// attachment is one request_attachments row under test. Zero values are what the
// schema's defaults supply, so a test states only the rule it is proving.
type attachment struct {
	request int
	name    string // as given when nameSet, otherwise "notes.txt"
	nameSet bool
	typ     string // as given when typSet, otherwise text/plain
	typSet  bool
	content []byte // nil = the standard sample
	size    int    // as given when sizeSet, otherwise len(content)
	sizeSet bool
	sha     string // as given when shaSet, otherwise sha256(content)
	shaSet  bool
	scan    string // "" = the column's default ('unsupported')
	user    int    // as given when userSet, otherwise NULL
	userSet bool

	keep       bool
	keptAt     *time.Time
	keptBy     int
	expiresAt  *time.Time
	releasedAt *time.Time
	releasedBy int
}

func shaOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (d *attachmentsDB) insertAttachment(a attachment) (int64, error) {
	d.t.Helper()
	name, typ := "notes.txt", "text/plain"
	if a.nameSet {
		name = a.name
	}
	if a.typSet {
		typ = a.typ
	}
	content := a.content
	if content == nil {
		content = []byte("just some notes\n")
	}
	size := len(content)
	if a.sizeSet {
		size = a.size
	}
	sum := shaOf(content)
	if a.shaSet {
		sum = a.sha
	}
	var user, keptBy, releasedBy *int
	if a.userSet {
		user = &a.user
	}
	if a.keptBy > 0 {
		keptBy = &a.keptBy
	}
	if a.releasedBy > 0 {
		releasedBy = &a.releasedBy
	}
	var scan *string
	if a.scan != "" {
		scan = &a.scan
	}
	var id int64
	err := d.db.QueryRow(d.ctx, `INSERT INTO request_attachments
		(request_id, file_name, content_type, size_bytes, sha256, scan_state, uploaded_by,
		 keep, kept_by, kept_at, expires_at, released_at, released_by, content)
		VALUES ($1, $2, $3, $4, $5, COALESCE($6, 'unsupported'), $7,
		        $8, $9, $10, $11, $12, $13, $14) RETURNING id`,
		a.request, name, typ, size, sum, scan, user,
		a.keep, keptBy, a.keptAt, a.expiresAt, a.releasedAt, releasedBy, content).Scan(&id)
	return id, err
}

func (d *attachmentsDB) mustAttachment(a attachment) int64 {
	d.t.Helper()
	id, err := d.insertAttachment(a)
	if err != nil {
		d.t.Fatalf("insert attachment %+v: %v", a, err)
	}
	return id
}

func (d *attachmentsDB) tryInsertAttachmentErr(t *testing.T, a attachment) error {
	t.Helper()
	if _, err := d.insertAttachment(a); err != nil {
		return err
	}
	t.Fatalf("%+v was accepted", a)
	return nil
}

func (d *attachmentsDB) attachmentCount(requestID int) int {
	d.t.Helper()
	var n int
	if err := d.db.QueryRow(d.ctx, `SELECT count(*) FROM request_attachments WHERE request_id = $1`, requestID).Scan(&n); err != nil {
		d.t.Fatalf("count attachments of request %d: %v", requestID, err)
	}
	return n
}

func (d *attachmentsDB) hasIndex(name string) bool {
	d.t.Helper()
	var ok bool
	if err := d.db.QueryRow(d.ctx, `SELECT EXISTS (SELECT 1 FROM pg_indexes
		WHERE tablename = 'request_attachments' AND indexname = $1)`, name).Scan(&ok); err != nil {
		d.t.Fatalf("look up index %s: %v", name, err)
	}
	return ok
}

// Migration 056 applies on a fresh database and on top of 055 with existing work
// in it: the table starts empty, nothing existing changes, and re-running the SQL
// by hand adds nothing.
func TestMigration_056RequestAttachments(t *testing.T) {
	ctx := context.Background()

	t.Run("fresh", func(t *testing.T) {
		dsn := freshDatabase(t)
		repo, err := repository.New(ctx, dsn)
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		defer repo.Close()
		if err := repo.RunMigrations(ctx, migrations); err != nil {
			t.Fatalf("fresh migrate: %v", err)
		}
		db, err := pgx.Connect(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close(ctx)
		var n int
		if err := db.QueryRow(ctx, `SELECT COUNT(*) FROM request_attachments`).Scan(&n); err != nil {
			t.Fatalf("request_attachments: %v", err)
		}
		if n != 0 {
			t.Fatalf("request_attachments starts with %d rows, want 0", n)
		}
		// The table must be usable the moment it exists: a binary file attached to
		// a request the app just created, with its bytes intact.
		d := newAttachmentsDB(t)
		reqID := d.insertReq(req{ref: "ref_FRESH056_0000000001"})
		payload := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\xff\xfe")
		id := d.mustAttachment(attachment{
			request: reqID, name: "screenshot.png", nameSet: true, typ: "image/png", typSet: true,
			content: payload, scan: "clean", user: d.user, userSet: true,
		})
		var got []byte
		var storedType, storedScan string
		if err := d.db.QueryRow(d.ctx, `SELECT content, content_type, scan_state
			FROM request_attachments WHERE id = $1`, id).Scan(&got, &storedType, &storedScan); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("stored bytes %q, want %q (a binary round-trip through BYTEA)", got, payload)
		}
		if storedType != "image/png" || storedScan != "clean" {
			t.Fatalf("stored type %q scan %q", storedType, storedScan)
		}
	})

	t.Run("upgrade from 055", func(t *testing.T) {
		dsn := freshDatabase(t)
		repo, err := repository.New(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer repo.Close()
		var before []repository.Migration
		for _, m := range migrations {
			if m.Version < "056" {
				before = append(before, m)
			}
		}
		if len(before) == len(migrations) {
			t.Fatal("no migration 056")
		}
		if err := repo.RunMigrations(ctx, before); err != nil {
			t.Fatalf("migrate to 055: %v", err)
		}
		db, err := pgx.Connect(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close(ctx)
		// Work that already exists at 055: a project with a ticket, and a request
		// in triage with a discussion row and a link to that ticket.
		var projectID, epic, story, taskID, orgID, teamID, userID, requestID int
		q := func(sql string, dst *int, args ...any) {
			t.Helper()
			if err := db.QueryRow(ctx, sql, args...).Scan(dst); err != nil {
				t.Fatalf("%s: %v", sql, err)
			}
		}
		q(`INSERT INTO projects (name, code) VALUES ('up056', 'up056') RETURNING id`, &projectID)
		q(`INSERT INTO epics (project_id, name) VALUES ($1, 'e') RETURNING id`, &epic, projectID)
		q(`INSERT INTO stories (project_id, epic_id, name) VALUES ($1, $2, 's') RETURNING id`, &story, projectID, epic)
		q(`INSERT INTO tasks (project_id, epic_id, story_id, name) VALUES ($1, $2, $3, 't') RETURNING id`, &taskID, projectID, epic, story)
		q(`INSERT INTO orgs (name, slug) VALUES ('pre-056', 'pre056') RETURNING id`, &orgID)
		q(`INSERT INTO teams (org_id, name) VALUES ($1, 'pre-056 desk') RETURNING id`, &teamID, orgID)
		q(`INSERT INTO users (subject, email) VALUES ('pre056-sub', 'pre056@x.test') RETURNING id`, &userID)
		q(`INSERT INTO team_requests (public_ref, org_id, team_id, requester_user_id, requester_contact, subject, state)
			VALUES ('ref_PRE056UPGRADE0001', $1, $2, $3, 'pre056@x.test', 'a request before 056', 'triaging') RETURNING id`,
			&requestID, orgID, teamID, userID)
		q(`INSERT INTO request_comments (request_id, kind, body, author)
			VALUES ($1, 'comment', 'here are the logs', 'Desk lead') RETURNING id`, new(int), requestID)
		q(`INSERT INTO request_links (request_id, org_id, link_kind, target_kind, target_id, created_by)
			VALUES ($1, $2, 'planned_as', 'task', $3, $4) RETURNING id`, new(int), requestID, orgID, taskID, userID)

		if err := repo.RunMigrations(ctx, migrations); err != nil {
			t.Fatalf("upgrade to 056: %v", err)
		}
		// Existing work is untouched and the new table is empty: an upgrade adds a
		// place to keep files, it never invents one.
		got, err := repo.GetTask(ctx, taskID)
		if err != nil || got.ProjectID != projectID {
			t.Fatalf("existing ticket after upgrade: %+v %v", got, err)
		}
		var attachments, links int
		if err := db.QueryRow(ctx, `SELECT count(*) FROM request_attachments`).Scan(&attachments); err != nil || attachments != 0 {
			t.Fatalf("request_attachments after upgrade: %d %v", attachments, err)
		}
		if err := db.QueryRow(ctx, `SELECT count(*) FROM request_links WHERE request_id = $1`, requestID).Scan(&links); err != nil || links != 1 {
			t.Fatalf("the pre-056 lineage row: %d %v", links, err)
		}
		var state string
		if err := db.QueryRow(ctx, `SELECT state FROM team_requests WHERE id = $1`, requestID).Scan(&state); err != nil || state != "triaging" {
			t.Fatalf("pre-existing request state = %q (%v), want triaging", state, err)
		}
		// The pre-existing request can carry a file now: an upgrade opens the
		// attachment store for work that was already there.
		if _, err := db.Exec(ctx, `INSERT INTO request_attachments
			(request_id, file_name, content_type, size_bytes, sha256, uploaded_by, content)
			VALUES ($1, 'rollout.log', 'text/plain', 5, $2, $3, 'hello')`,
			requestID, shaOf([]byte("hello")), userID); err != nil {
			t.Fatalf("attachment on a pre-056 request: %v", err)
		}

		// Re-running 056's SQL by hand is harmless, twice over: nothing is added a
		// second time (a duplicated constraint or index would be a schema that
		// drifts every time someone fixes it by hand).
		counts := func() (constraints, indexes int) {
			t.Helper()
			if err := db.QueryRow(ctx, `SELECT count(*) FROM pg_constraint WHERE conrelid = 'request_attachments'::regclass`).Scan(&constraints); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRow(ctx, `SELECT count(*) FROM pg_indexes WHERE tablename = 'request_attachments'`).Scan(&indexes); err != nil {
				t.Fatal(err)
			}
			return constraints, indexes
		}
		cBefore, iBefore := counts()
		reapplied := false
		for _, m := range migrations {
			if m.Version == "056_request_attachments" {
				reapplied = true
				for i := 0; i < 2; i++ {
					if _, err := db.Exec(ctx, m.SQL); err != nil {
						t.Fatalf("re-running 056 (pass %d): %v", i+1, err)
					}
				}
			}
		}
		if !reapplied {
			t.Fatal("migration 056_request_attachments not found in the embedded set")
		}
		cAfter, iAfter := counts()
		if cAfter != cBefore {
			t.Fatalf("re-running 056 changed the constraint count %d -> %d", cBefore, cAfter)
		}
		if iAfter != iBefore {
			t.Fatalf("re-running 056 changed the index count %d -> %d", iBefore, iAfter)
		}
		var n int
		if err := db.QueryRow(ctx, `SELECT count(*) FROM request_attachments WHERE request_id = $1`, requestID).Scan(&n); err != nil || n != 1 {
			t.Fatalf("request_attachments after re-applying 056 = %d (%v), want 1", n, err)
		}
	})
}

// The schema accepts exactly the rows domain.RequestAttachment.Validate accepts.
// For every case the domain refuses, the database refuses it too, and for every
// case the domain allows, the database stores it. This is the drift guard between
// request_attachment.go and the table: a rule added to one and not the other
// fails here.
func TestMigration_056MatchesDomainValidate(t *testing.T) {
	d := newAttachmentsDB(t)
	reqID := d.insertReq(req{ref: "ref_DOMAIN_056_00000001"})
	user := d.user
	now := time.Now()
	payload := []byte("hello, world\n")
	// 251 + len(".txt") = 255, the domain's limit, in characters and bytes.
	long255 := strings.Repeat("a", 251) + ".txt"
	long256 := strings.Repeat("a", 252) + ".txt"

	type c struct {
		name   string
		mutate func(a *domain.RequestAttachment, content *[]byte)
	}
	cases := []c{
		{"a plain text file", func(a *domain.RequestAttachment, _ *[]byte) {}},
		{"an image", func(a *domain.RequestAttachment, _ *[]byte) { a.FileName, a.ContentType = "shot.png", "image/png" }},
		{"a held archive", func(a *domain.RequestAttachment, _ *[]byte) {
			a.FileName, a.ContentType = "bundle.zip", "application/zip"
		}},
		{"a held binary", func(a *domain.RequestAttachment, _ *[]byte) {
			a.FileName, a.ContentType = "tool.exe", "application/octet-stream"
		}},
		{"a held script name over text bytes", func(a *domain.RequestAttachment, _ *[]byte) { a.FileName = "deploy.sh" }},
		{"a kept file", func(a *domain.RequestAttachment, _ *[]byte) { a.Keep, a.KeptAt, a.KeptBy = true, &now, &user }},
		{"a kept file with no kept_by", func(a *domain.RequestAttachment, _ *[]byte) { a.Keep, a.KeptAt = true, &now }},
		{"a released held file", func(a *domain.RequestAttachment, _ *[]byte) {
			a.FileName, a.ContentType = "bundle.zip", "application/zip"
			a.ReleasedAt, a.ReleasedBy = &now, &user
		}},
		{"a released file with no releaser", func(a *domain.RequestAttachment, _ *[]byte) {
			a.FileName, a.ContentType = "bundle.zip", "application/zip"
			a.ReleasedAt = &now
		}},
		{"a file on an active request (no expiry)", func(a *domain.RequestAttachment, _ *[]byte) { a.ExpiresAt = nil }},
		{"a file past its retention window", func(a *domain.RequestAttachment, _ *[]byte) { a.ExpiresAt = &now }},
		{"the 255-character name limit", func(a *domain.RequestAttachment, _ *[]byte) { a.FileName = long255 }},
		{"no uploader", func(a *domain.RequestAttachment, _ *[]byte) { a.UploadedBy = nil }},

		// Refused by the domain and by the schema.
		{"an empty name", func(a *domain.RequestAttachment, _ *[]byte) { a.FileName = "  " }},
		{"a path in the name", func(a *domain.RequestAttachment, _ *[]byte) { a.FileName = "logs/app.log" }},
		{"a backslash in the name", func(a *domain.RequestAttachment, _ *[]byte) { a.FileName = `logs\app.log` }},
		{"a dot-dot name", func(a *domain.RequestAttachment, _ *[]byte) { a.FileName = ".." }},
		{"a control character in the name", func(a *domain.RequestAttachment, _ *[]byte) { a.FileName = "ap\tp.log" }},
		{"a name over 255 characters", func(a *domain.RequestAttachment, _ *[]byte) { a.FileName = long256 }},
		{"a client type the sniffer never returns", func(a *domain.RequestAttachment, _ *[]byte) { a.ContentType = "application/json" }},
		{"an SVG claimed as an image", func(a *domain.RequestAttachment, _ *[]byte) { a.ContentType = "image/svg+xml" }},
		{"a parameterized content type", func(a *domain.RequestAttachment, _ *[]byte) { a.ContentType = "text/plain; charset=utf-8" }},
		{"a zero size", func(a *domain.RequestAttachment, _ *[]byte) { a.SizeBytes = 0 }},
		{"a negative size", func(a *domain.RequestAttachment, _ *[]byte) { a.SizeBytes = -1 }},
		{"a short digest", func(a *domain.RequestAttachment, _ *[]byte) { a.SHA256 = "abc123" }},
		{"an upper-case digest", func(a *domain.RequestAttachment, _ *[]byte) { a.SHA256 = strings.ToUpper(a.SHA256) }},
		{"an invented scan state", func(a *domain.RequestAttachment, _ *[]byte) { a.ScanState = "scanned" }},
		{"keep with no kept_at", func(a *domain.RequestAttachment, _ *[]byte) { a.Keep = true }},
		{"kept_by with no kept_at", func(a *domain.RequestAttachment, _ *[]byte) { a.KeptBy = &user }},
		{"released_by with no released_at", func(a *domain.RequestAttachment, _ *[]byte) { a.ReleasedBy = &user }},
		{"a release on a file that was never held", func(a *domain.RequestAttachment, _ *[]byte) { a.ReleasedAt = &now }},
		{"a zero user id", func(a *domain.RequestAttachment, _ *[]byte) { zero := 0; a.UploadedBy = &zero }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			content := append([]byte(nil), payload...)
			a := domain.RequestAttachment{
				RequestID: reqID, FileName: "notes.txt", ContentType: "text/plain",
				SizeBytes: int64(len(payload)), SHA256: shaOf(payload),
				ScanState: domain.RequestScanUnsupported, UploadedBy: &user, Content: payload,
			}
			tc.mutate(&a, &content)
			// Char length vs rune count are the same for these ASCII names.
			wantOK := a.Validate() == nil

			id, err := d.insertAttachment(attachment{
				request: a.RequestID, typ: a.ContentType, typSet: true,
				content: content, name: a.FileName, nameSet: true, size: int(a.SizeBytes), sizeSet: true,
				sha: a.SHA256, shaSet: true,
				scan: string(a.ScanState), user: derefInt(a.UploadedBy), userSet: a.UploadedBy != nil,
				keep: a.Keep, keptAt: a.KeptAt, keptBy: derefInt(a.KeptBy),
				expiresAt: a.ExpiresAt, releasedAt: a.ReleasedAt, releasedBy: derefInt(a.ReleasedBy),
			})
			switch {
			case wantOK && err != nil:
				t.Fatalf("the domain accepts this row but the schema refused it: %v", err)
			case !wantOK && err == nil:
				t.Fatalf("the domain refuses this row but the schema stored it as %d", id)
			}
			if err == nil {
				if _, delErr := d.db.Exec(d.ctx, `DELETE FROM request_attachments WHERE id = $1`, id); delErr != nil {
					t.Fatal(delErr)
				}
			}
		})
	}

	// Two rules the equivalence above cannot cover, because they are not shape
	// rules: the row must belong to a request that exists (the FK, not a CHECK),
	// and size_bytes must be the bytes' OWN length (which domain.Validate, given
	// metadata only, has no way to see). Both are asserted where they belong.
	wantSQLState(t, "an attachment on a request that does not exist",
		d.tryInsertAttachmentErr(t, attachment{request: 999999}), "23503")
	wantSQLState(t, "a size that disagrees with the stored bytes",
		d.tryInsertAttachmentErr(t, attachment{request: reqID, size: len(payload) + 1, sizeSet: true}), "23514")
	// ... while the true size is of course fine.
	d.mustAttachment(attachment{request: reqID, content: payload})
}

func derefInt(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

// scan_state is the contract's closed set, and its default is "no verdict" — not
// "clean" (a security claim AMP cannot back) and not "pending" (a download held
// behind a scanner that never runs).
func TestMigration_056ScanStates(t *testing.T) {
	d := newAttachmentsDB(t)
	reqID := d.insertReq(req{ref: "ref_SCAN_056_000000001"})

	// Every state the domain knows is storable, and there are exactly four: a
	// fifth would mean the schema and the contract disagree.
	if len(domain.RequestScanStates) != 4 {
		t.Fatalf("domain.RequestScanStates has %d states, want the contract's 4", len(domain.RequestScanStates))
	}
	for _, state := range domain.RequestScanStates {
		id := d.mustAttachment(attachment{request: reqID, scan: string(state)})
		var stored string
		if err := d.db.QueryRow(d.ctx, `SELECT scan_state FROM request_attachments WHERE id = $1`, id).Scan(&stored); err != nil {
			t.Fatal(err)
		}
		if stored != string(state) {
			t.Fatalf("scan state %q stored as %q", state, stored)
		}
	}

	// An invented state is refused in every spelling a careless caller might use:
	// there is no case folding and no other acceptance path.
	for _, bad := range []string{"scanned", "unknown", "CLEAN", "Clean", " ", "clean ", "clean;drop", "infected2"} {
		wantSQLState(t, "scan state "+bad, d.tryInsertAttachmentErr(t, attachment{request: reqID, scan: bad}), "23514")
	}

	// The default is "unsupported": forgetting to set a state can never claim the
	// file was scanned, and can never wedge it in pending either.
	var id int64
	if err := d.db.QueryRow(d.ctx, `INSERT INTO request_attachments
		(request_id, file_name, content_type, size_bytes, sha256, content)
		VALUES ($1, 'a.txt', 'text/plain', 2, $2, 'hi') RETURNING id`,
		reqID, shaOf([]byte("hi"))).Scan(&id); err != nil {
		t.Fatalf("insert without a scan state: %v", err)
	}
	var stored string
	if err := d.db.QueryRow(d.ctx, `SELECT scan_state FROM request_attachments WHERE id = $1`, id).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != string(domain.RequestScanUnsupported) {
		t.Fatalf("default scan state = %q, want %q", stored, domain.RequestScanUnsupported)
	}
}

// The per-file bound is the domain's, in both directions, and size_bytes must be
// the bytes' own length. 25 MiB is the contract's "25 MB each", so the boundary is
// pinned exactly: a constant that drifts in the domain and not in the schema is a
// limit that looks enforced and is not.
func TestMigration_056SizeBound(t *testing.T) {
	d := newAttachmentsDB(t)
	reqID := d.insertReq(req{ref: "ref_SIZE_056_000000001"})

	// Exactly the cap is storable: the bound is inclusive.
	atCap := bytes.Repeat([]byte{'a'}, domain.MaxRequestAttachmentBytes)
	id := d.mustAttachment(attachment{request: reqID, content: atCap})
	var stored int
	if err := d.db.QueryRow(d.ctx, `SELECT size_bytes FROM request_attachments WHERE id = $1`, id).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if int64(stored) != int64(domain.MaxRequestAttachmentBytes) {
		t.Fatalf("stored size %d, want the domain's cap %d", stored, domain.MaxRequestAttachmentBytes)
	}
	// The schema's literal has to be the domain's constant: if someone edits one,
	// this fails before a 26 MiB file is stored.
	var capLiteral int
	if err := d.db.QueryRow(d.ctx, `SELECT COALESCE((
		SELECT (regexp_match(pg_get_constraintdef(oid), 'size_bytes <= (\d+)'))[1]::int
		FROM pg_constraint WHERE conname = 'request_attachments_size_within_limit'), 0)`).Scan(&capLiteral); err != nil {
		t.Fatal(err)
	}
	if capLiteral != domain.MaxRequestAttachmentBytes {
		t.Fatalf("the schema's per-file cap is %d, the domain's is %d", capLiteral, domain.MaxRequestAttachmentBytes)
	}

	d.mustExec(`DELETE FROM request_attachments WHERE id = $1`, id)

	// Over the cap is refused, whether the bytes or only the claim are over: the
	// size is checked against the bound and against the bytes, and neither check
	// stands in for the other.
	over := bytes.Repeat([]byte{'a'}, domain.MaxRequestAttachmentBytes+1)
	wantSQLState(t, "one byte over the per-file limit",
		d.tryInsertAttachmentErr(t, attachment{request: reqID, content: over}), "23514")
	wantSQLState(t, "a declared size over the limit",
		d.tryInsertAttachmentErr(t, attachment{request: reqID, size: domain.MaxRequestAttachmentBytes + 1, sizeSet: true}), "23514")
	wantSQLState(t, "a negative size",
		d.tryInsertAttachmentErr(t, attachment{request: reqID, size: -1, sizeSet: true}), "23514")
	wantSQLState(t, "a zero size",
		d.tryInsertAttachmentErr(t, attachment{request: reqID, size: 0, sizeSet: true, content: []byte{}}), "23514")

	// A digest is 64 lower-case hex characters — the form the domain normalizes
	// to, so the same bytes always produce the same ETag string.
	good := shaOf([]byte("x"))
	for _, bad := range []string{"", good[:63], good + "a", strings.Repeat("g", 64), strings.ToUpper(good), " " + good} {
		wantSQLState(t, "sha256 "+bad, d.tryInsertAttachmentErr(t, attachment{request: reqID, sha: bad, shaSet: true}), "23514")
	}
}

// content_type is the sniffed type and the file_name is a display name. The
// comparison runs against the domain's own table, so a type added to
// request_attachment.go without the schema following (or the reverse) fails here.
func TestMigration_056ContentTypeAndName(t *testing.T) {
	d := newAttachmentsDB(t)
	reqID := d.insertReq(req{ref: "ref_TYPE_056_000000001"})

	// Every type the domain stores is storable here, in the form the column
	// holds; every type it refuses is refused here. The candidates include the
	// types a client would like to declare and the sniffer never returns.
	allowed := map[string]bool{}
	for _, candidate := range attachmentContentTypeCandidates(t) {
		normalized, _, domainErr := domain.NormalizeRequestAttachmentContentType(candidate)
		switch {
		case domainErr == nil && normalized == candidate:
			allowed[candidate] = true
			d.mustAttachment(attachment{request: reqID, typ: candidate, typSet: true})
		case domainErr == nil:
			// A normalizing input: the column only ever holds the normalized form.
			wantSQLState(t, "the un-normalized content type "+candidate,
				d.tryInsertAttachmentErr(t, attachment{request: reqID, typ: candidate, typSet: true}), "23514")
		default:
			wantSQLState(t, "refused content type "+candidate,
				d.tryInsertAttachmentErr(t, attachment{request: reqID, typ: candidate, typSet: true}), "23514")
		}
	}
	// Guard the guard: the accepted set must really cover the domain's table, or
	// this test would pass by checking almost nothing.
	if len(allowed) < 13 {
		t.Fatalf("only %d content types were exercised as storable: %v", len(allowed), allowed)
	}

	// The name is a display name: no separators, no dot segments, no control
	// characters (CR/LF would split the Content-Disposition header), bounded at
	// the domain's limit.
	for _, bad := range []string{"", "  ", "a/b.txt", `a\b.txt`, "..", ".", "a\tb.txt",
		"a\nb.txt", "a\r\nX: y.txt", strings.Repeat("a", 256)} {
		wantSQLState(t, "file name "+bad, d.tryInsertAttachmentErr(t, attachment{request: reqID, name: bad, nameSet: true}), "23514")
	}
	// Exactly 255 characters is fine, and it is the domain's own limit.
	if got, err := domain.NormalizeRequestAttachmentFileName(strings.Repeat("a", 251) + ".txt"); err != nil || len(got) != domain.MaxRequestAttachmentNameLen {
		t.Fatalf("expected a %d-character name to be valid: %q %v", domain.MaxRequestAttachmentNameLen, got, err)
	}
	d.mustAttachment(attachment{request: reqID, name: strings.Repeat("a", 251) + ".txt", nameSet: true})
	// Odd-but-legal names are names: an emoji, a space, a colon (a display name
	// may hold one; only the download path has to be careful with it).
	for _, ok := range []string{"Quarterly report (final).pdf", "café.txt", "a:b.txt", ".bashrc"} {
		d.mustAttachment(attachment{request: reqID, name: ok, nameSet: true})
	}
}

var (
	contentTypeRe = regexp.MustCompile(`^[a-z]+\.[a-z0-9.+-]*/[a-z0-9.+-]+$`)
	quotedLitRe   = regexp.MustCompile(`"([^"\n]*)"`)
)

// attachmentContentTypeCandidates is the matrix of types the schema-versus-domain
// comparison covers: a fixed list plus everything quoted in the domain's source
// and in this migration, so neither side can grow a value the matrix silently
// skips.
func attachmentContentTypeCandidates(t *testing.T) []string {
	t.Helper()
	seen := map[string]bool{}
	var out []string
	add := func(v string) {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	for _, v := range []string{
		// Accepted: every entry in domain.requestAttachmentTypeKinds.
		"application/pdf", "text/html", "text/xml", "text/plain",
		"image/png", "image/jpeg", "image/gif", "image/webp", "image/bmp",
		"application/zip", "application/x-gzip", "application/octet-stream", "application/wasm",
		// Refused: what a client declares, what a sniffer never returns, junk.
		"application/json", "text/csv", "text/markdown", "image/svg+xml", "application/x-sh",
		"application/x-msdownload", "video/mp4", "application/msword",
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		"application/x-rar-compressed", "application/x-7z-compressed",
		"text/plain; charset=utf-8", "TEXT/PLAIN", " application/pdf ", "", " ", "text", "/plain",
		"application/octet-stream; charset=binary",
	} {
		add(v)
	}
	// Everything the two files name, so a type added to one is always exercised.
	for _, path := range []string{
		filepath.Join("..", "..", "internal", "domain", "request_attachment.go"),
		filepath.Join("migrations", "056_request_attachments.sql"),
	} {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, lit := range quotedLitRe.FindAllStringSubmatch(string(src), -1) {
			if contentTypeRe.MatchString(lit[1]) {
				add(lit[1])
			}
		}
	}
	return out
}

// The hold: a release is evidence that a hold existed, so released_at is only
// storable on a file that is actually held — a restricted sniffed type, or a name
// that says code (which can escalate a hold and never lift one). The schema's
// escape hatches and domain.RequestAttachment.Held are compared over the same
// matrix, in both directions.
func TestMigration_056ReleaseOnlyWhenHeld(t *testing.T) {
	d := newAttachmentsDB(t)
	reqID := d.insertReq(req{ref: "ref_HOLD_056_000000001"})
	now := time.Now()

	// The type decides the hold when the name does not. "file.bin.doc" holds no
	// script extension, so only a held type can make these releasable.
	for _, typ := range attachmentContentTypeCandidates(t) {
		normalized, _, err := domain.NormalizeRequestAttachmentContentType(typ)
		if err != nil {
			continue
		}
		probe := domain.RequestAttachment{FileName: "file.bin.doc", ContentType: normalized}
		held := probe.Held()
		_, insertErr := d.insertAttachment(attachment{
			request: reqID, name: "file.bin.doc", nameSet: true, typ: normalized, typSet: true,
			releasedAt: &now, releasedBy: d.user,
		})
		switch {
		case held && insertErr != nil:
			t.Fatalf("a held type %q cannot be released: %v", normalized, insertErr)
		case !held && insertErr == nil:
			t.Fatalf("a %q file, which is never held, accepted a release", normalized)
		}
	}

	// The name decides the hold when the bytes sniff as plain text: sniffing
	// cannot tell a shell script from a shopping list.
	for _, ext := range attachmentNameExtensionCandidates(t) {
		name := "payload" + ext
		probe := domain.RequestAttachment{FileName: name, ContentType: "text/plain"}
		held := probe.Held()
		_, insertErr := d.insertAttachment(attachment{
			request: reqID, name: name, nameSet: true, typ: "text/plain", typSet: true,
			content:    []byte("not really a script\n"),
			releasedAt: &now, releasedBy: d.user,
		})
		switch {
		case held && insertErr != nil:
			t.Fatalf("the code-like name %q cannot be released: %v", name, insertErr)
		case !held && insertErr == nil:
			t.Fatalf("the name %q accepted a release but the domain says it is not held", name)
		}
	}

	// The timestamp may outlive the actor (released_by is ON DELETE SET NULL), so
	// a release with no named releaser is a legitimate row; a releaser with no
	// timestamp is not.
	d.mustAttachment(attachment{request: reqID, name: "bundle.zip", nameSet: true, typ: "application/zip", typSet: true, releasedAt: &now})
	wantSQLState(t, "a releaser with no release timestamp",
		d.tryInsertAttachmentErr(t, attachment{request: reqID, name: "bundle.zip", nameSet: true, typ: "application/zip", typSet: true, releasedBy: d.user}), "23514")

	// Deleting the releaser nulls the link and leaves the release intact: the hold
	// was released, whoever the person was.
	var released int64
	if err := d.db.QueryRow(d.ctx, `INSERT INTO request_attachments
		(request_id, file_name, content_type, size_bytes, sha256, released_at, released_by, content)
		VALUES ($1, 'x.zip', 'application/zip', 2, $2, $3, $4, 'hi') RETURNING id`,
		reqID, shaOf([]byte("hi")), now, d.user).Scan(&released); err != nil {
		t.Fatal(err)
	}
	d.mustExec(`DELETE FROM users WHERE id = $1`, d.user)
	var releasedAt *time.Time
	var releasedBy *int
	if err := d.db.QueryRow(d.ctx, `SELECT released_at, released_by FROM request_attachments WHERE id = $1`, released).Scan(&releasedAt, &releasedBy); err != nil {
		t.Fatal(err)
	}
	if releasedAt == nil || releasedBy != nil {
		t.Fatalf("after the releaser was deleted: released_at %v released_by %v, want the timestamp kept and the actor nulled", releasedAt, releasedBy)
	}
}

// attachmentNameExtensionCandidates is the extension matrix: every suffix in
// domain.requestAttachmentScriptExtensions (held) plus ordinary ones that must not
// be, plus anything either file quotes.
func attachmentNameExtensionCandidates(t *testing.T) []string {
	t.Helper()
	seen := map[string]bool{}
	var out []string
	add := func(v string) {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	for _, v := range []string{
		".sh", ".bash", ".zsh", ".fish", ".ksh",
		".ps1", ".psm1", ".bat", ".cmd", ".com",
		".exe", ".dll", ".scr", ".msi",
		".jar", ".vbs", ".js", ".mjs", ".py", ".rb", ".pl", ".php",
		// Ordinary suffixes: text bytes with one of these were never held.
		".txt", ".pdf", ".png", ".zip", ".log", ".csv", ".json", ".html", ".md", ".tar.gz", ".gz",
	} {
		add(v)
	}
	for _, path := range []string{
		filepath.Join("..", "..", "internal", "domain", "request_attachment.go"),
		filepath.Join("migrations", "056_request_attachments.sql"),
	} {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, lit := range quotedLitRe.FindAllStringSubmatch(string(src), -1) {
			if v := lit[1]; len(v) > 1 && v[0] == '.' && !strings.ContainsAny(v, " \t*+[]()\\") {
				add(v)
			}
		}
	}
	return out
}

// The approved deletion semantics, and no orphan bytes: an attachment is part of
// the request's record, so deleting the request (or its team, or its org) takes
// its files and their bytes with it. Deleting a USER never deletes a file.
func TestMigration_056RequestDeletionTakesAttachments(t *testing.T) {
	t.Run("the request", func(t *testing.T) {
		d := newAttachmentsDB(t)
		reqID := d.insertReq(req{ref: "ref_DEL_056_request_001"})
		other := d.insertReq(req{ref: "ref_DEL_056_other_0001"})
		d.mustAttachment(attachment{request: reqID, content: bytes.Repeat([]byte{0xAB}, 4096), user: d.user, userSet: true})
		d.mustAttachment(attachment{request: reqID, name: "b.zip", nameSet: true, typ: "application/zip", typSet: true, user: d.user, userSet: true})
		d.mustAttachment(attachment{request: other})

		// The request deletes cleanly: nothing blocks it (a NO ACTION foreign key
		// would, and an orphaned attachment would be worse).
		d.mustExec(`DELETE FROM team_requests WHERE id = $1`, reqID)
		var left int64
		var bytesLeft int64
		if err := d.db.QueryRow(d.ctx, `SELECT count(*), COALESCE(SUM(octet_length(content)), 0)
			FROM request_attachments WHERE request_id = $1`, reqID).Scan(&left, &bytesLeft); err != nil {
			t.Fatal(err)
		}
		if left != 0 || bytesLeft != 0 {
			t.Fatalf("after deleting the request: %d attachments and %d bytes of its content remain", left, bytesLeft)
		}
		// The other request's file is untouched: retention follows a request, not
		// the table.
		if n := d.attachmentCount(other); n != 1 {
			t.Fatalf("another request's attachments after the delete: %d, want 1", n)
		}
	})

	t.Run("the team and the org", func(t *testing.T) {
		d := newAttachmentsDB(t)
		teamReq := d.insertReq(req{ref: "ref_DEL_056_team_00001", team: d.team2})
		d.mustAttachment(attachment{request: teamReq, content: bytes.Repeat([]byte{'x'}, 1024)})
		// A whole team going takes its requests' files (the 053 cascade).
		d.mustExec(`DELETE FROM teams WHERE id = $1`, d.team2)
		if n := d.attachmentCount(teamReq); n != 0 {
			t.Fatalf("attachments after their team was deleted: %d, want 0", n)
		}
		// ... and so does an org going.
		foreignTeam := d.insert(`INSERT INTO teams (org_id, name) VALUES ($1, 'other desk') RETURNING id`, d.other)
		orgReq := d.insertReq(req{ref: "ref_DEL_056_org_000001", org: d.other, team: foreignTeam})
		d.mustAttachment(attachment{request: orgReq})
		d.mustExec(`DELETE FROM orgs WHERE id = $1`, d.other)
		if n := d.attachmentCount(orgReq); n != 0 {
			t.Fatalf("attachments after their org was deleted: %d, want 0", n)
		}
	})

	t.Run("a user is not a lifecycle", func(t *testing.T) {
		d := newAttachmentsDB(t)
		reqID := d.insertReq(req{ref: "ref_DEL_056_user_00001"})
		now := time.Now()
		id := d.mustAttachment(attachment{
			request: reqID, name: "b.zip", nameSet: true, typ: "application/zip", typSet: true,
			user: d.user, userSet: true, keep: true, keptAt: &now, keptBy: d.user,
		})
		// Deleting the uploader (and keeper) must not delete the file: the request
		// is still there and the file is part of what it was sent with.
		d.mustExec(`DELETE FROM users WHERE id = $1`, d.user)
		var uploadedBy, keptBy *int
		var keptAt *time.Time
		var kept bool
		if err := d.db.QueryRow(d.ctx, `SELECT uploaded_by, kept_by, kept_at, keep
			FROM request_attachments WHERE id = $1`, id).Scan(&uploadedBy, &keptBy, &keptAt, &kept); err != nil {
			t.Fatalf("the attachment did not survive its uploader's deletion: %v", err)
		}
		if uploadedBy != nil || keptBy != nil || keptAt == nil || !kept {
			t.Fatalf("after the uploader was deleted: uploaded_by %v kept_by %v kept_at %v keep %v", uploadedBy, keptBy, keptAt, kept)
		}
		if n := d.attachmentCount(reqID); n != 1 {
			t.Fatalf("attachments after the uploader was deleted: %d, want 1", n)
		}
	})

	t.Run("no bytes survive anywhere", func(t *testing.T) {
		// The bytes live in the row, so deleting the row has to give the space back.
		// Measure it rather than trusting it: a 25 MiB file, then the request
		// deleted and the table reclaimed, and the relation is back to the size it
		// was before the file arrived.
		d := newAttachmentsDB(t)
		size := func() int64 {
			t.Helper()
			var n int64
			if err := d.db.QueryRow(d.ctx, `SELECT pg_total_relation_size('request_attachments')`).Scan(&n); err != nil {
				t.Fatal(err)
			}
			return n
		}
		d.mustExec(`VACUUM request_attachments`)
		empty := size()

		reqID := d.insertReq(req{ref: "ref_DEL_056_bytes_0001"})
		// Incompressible bytes: a periodic or zero-filled payload compresses inside
		// its TOAST value and would prove nothing about where the bytes went.
		big := make([]byte, domain.MaxRequestAttachmentBytes)
		if _, err := rand.New(rand.NewSource(56)).Read(big); err != nil {
			t.Fatal(err)
		}
		id := d.mustAttachment(attachment{request: reqID, content: big})
		d.mustExec(`VACUUM request_attachments`)
		grown := size()
		if grown-empty < int64(domain.MaxRequestAttachmentBytes/2) {
			t.Fatalf("a 25 MiB attachment grew the relation by only %d bytes: the bytes are not stored in this table", grown-empty)
		}
		var stored int64
		if err := d.db.QueryRow(d.ctx, `SELECT octet_length(content) FROM request_attachments WHERE id = $1`, id).Scan(&stored); err != nil {
			t.Fatal(err)
		}
		if stored != int64(len(big)) {
			t.Fatalf("stored %d bytes, want %d", stored, len(big))
		}

		// Deleting the request removes the row; the space it held is reusable and
		// nothing of it is readable anywhere in the table.
		d.mustExec(`DELETE FROM team_requests WHERE id = $1`, reqID)
		var holders int
		if err := d.db.QueryRow(d.ctx, `SELECT count(*) FROM request_attachments WHERE content = $1`, big).Scan(&holders); err != nil {
			t.Fatal(err)
		}
		if holders != 0 {
			t.Fatalf("%d rows still hold the deleted request's bytes", holders)
		}
		// VACUUM FULL rewrites the relation, so this is the bytes actually gone
		// rather than merely dead to the next insert.
		d.mustExec(`VACUUM FULL request_attachments`)
		after := size()
		if after > empty+int64(domain.MaxRequestAttachmentBytes/4) {
			t.Fatalf("after the request was deleted the relation is %d bytes, %d above its empty size (%d): the bytes were not reclaimed", after, after-empty, empty)
		}
	})
}

// The retention columns are the purge job's whole query, and the partial index
// serves it. D3: a file is kept while its request is active (no expires_at), then
// 90 days after close, then purged — unless a team member pins it.
func TestMigration_056RetentionColumns(t *testing.T) {
	d := newAttachmentsDB(t)
	reqID := d.insertReq(req{ref: "ref_RETAIN_056_0000001"})
	now := time.Now()
	past, future := now.Add(-time.Hour), now.Add(48*time.Hour)

	active := d.mustAttachment(attachment{request: reqID, name: "active.txt", nameSet: true})                                         // no expiry
	expired := d.mustAttachment(attachment{request: reqID, name: "expired.txt", nameSet: true, expiresAt: &past})                     // due
	kept := d.mustAttachment(attachment{request: reqID, name: "kept.txt", nameSet: true, expiresAt: &past, keep: true, keptAt: &now}) // pinned
	notDue := d.mustAttachment(attachment{request: reqID, name: "not-due.txt", nameSet: true, expiresAt: &future})                    // later

	// The purge job's statement (G4), exactly as written here: expired and unkept.
	d.mustExec(`DELETE FROM request_attachments WHERE NOT keep AND expires_at IS NOT NULL AND expires_at <= NOW()`)

	remaining := map[int64]bool{}
	rows, err := d.db.Query(d.ctx, `SELECT id FROM request_attachments WHERE request_id = $1`, reqID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		remaining[id] = true
	}
	for name, id := range map[string]int64{"active": active, "kept": kept, "not-yet-due": notDue} {
		if !remaining[id] {
			t.Fatalf("the purge deleted the %s file (%d), which D3 keeps", name, id)
		}
	}
	if remaining[expired] {
		t.Fatal("the purge left the expired, unkept file behind")
	}

	// The partial index the job rides on exists, so the scan never walks the files
	// D3 keeps.
	if !d.hasIndex("idx_request_attachments_expiry") {
		t.Fatal("idx_request_attachments_expiry is missing: the purge would scan every kept and active file")
	}
	// A pinned file records when it was pinned; the timestamp may outlive the
	// actor (kept_by is ON DELETE SET NULL).
	wantSQLState(t, "a pinned file with no kept_at",
		d.tryInsertAttachmentErr(t, attachment{request: reqID, keep: true}), "23514")
	wantSQLState(t, "a keeper with no kept_at",
		d.tryInsertAttachmentErr(t, attachment{request: reqID, keptBy: d.user}), "23514")
	// A kept file may carry no expiry at all (pinned forever), which is the shape
	// RequestAttachmentExpiresAt produces for a keep.
	d.mustAttachment(attachment{request: reqID, keep: true, keptAt: &now})
}

// The table is additive and deliberately has no second byte store, no URL and no
// artifact reuse. This is asserted against the catalogue, because an absent column
// is the thing hardest to notice when it appears.
func TestMigration_056LeavesExistingSchemaAlone(t *testing.T) {
	ctx := context.Background()
	dsn := freshDatabase(t)
	repo, err := repository.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.RunMigrations(ctx, migrations); err != nil {
		t.Fatalf("fresh migrate: %v", err)
	}
	db, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(ctx)

	// The request tables keep the index sets their own tests pin (053: 9,
	// 054: 3 and 5, 055: 5), and the artifact tables keep theirs (018/019: 4).
	// 056 adds no index to any of them.
	for table, want := range map[string]int{
		"team_requests":    9,
		"request_comments": 3,
		"request_events":   5,
		"request_links":    5,
		"artifacts":        4,
	} {
		var got int
		if err := db.QueryRow(ctx, `SELECT count(*) FROM pg_indexes WHERE tablename = $1`, table).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("%s has %d indexes, want the %d it had before 056", table, got, want)
		}
	}
	var backRefs int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM pg_constraint
		WHERE conrelid IN ('team_requests'::regclass, 'request_comments'::regclass,
		                   'request_events'::regclass, 'request_links'::regclass)
		  AND confrelid = 'request_attachments'::regclass`).Scan(&backRefs); err != nil {
		t.Fatal(err)
	}
	if backRefs != 0 {
		t.Fatalf("an existing request table references request_attachments (%d FKs), want the attachment to hang off the request", backRefs)
	}

	// The columns are the domain record and nothing else: no url, uri, path or
	// storage key, because a column like that is how an uploaded file starts being
	// fetched from somewhere AMP does not control.
	rows, err := db.Query(ctx, `SELECT column_name FROM information_schema.columns
		WHERE table_name = 'request_attachments' ORDER BY ordinal_position`)
	if err != nil {
		t.Fatal(err)
	}
	var cols []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		cols = append(cols, c)
	}
	rows.Close()
	want := []string{"id", "request_id", "file_name", "content_type", "size_bytes", "sha256",
		"scan_state", "uploaded_by", "created_at", "keep", "kept_by", "kept_at", "expires_at",
		"released_at", "released_by", "content"}
	if !reflect.DeepEqual(cols, want) {
		t.Fatalf("request_attachments columns = %v, want %v", cols, want)
	}

	// Exactly one bytea column: the bytes are the row. A second one, or a text
	// column holding a locator, would be a store to leak.
	var byteaCols int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
		WHERE table_name = 'request_attachments' AND data_type = 'bytea'`).Scan(&byteaCols); err != nil {
		t.Fatal(err)
	}
	if byteaCols != 1 {
		t.Fatalf("request_attachments has %d bytea columns, want exactly the content column", byteaCols)
	}

	// The only foreign keys are the request (the lifecycle) and the three user
	// references: nothing to fetch from, nothing else that can hold a file, and no
	// reuse of the artifact tables.
	var fks []string
	rows, err = db.Query(ctx, `SELECT conname FROM pg_constraint
		WHERE conrelid = 'request_attachments'::regclass AND contype = 'f' ORDER BY conname`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		fks = append(fks, name)
	}
	rows.Close()
	wantFKs := []string{"request_attachments_kept_by_fkey", "request_attachments_released_by_fkey",
		"request_attachments_request_id_fkey", "request_attachments_uploaded_by_fkey"}
	if !reflect.DeepEqual(fks, wantFKs) {
		t.Fatalf("request_attachments foreign keys = %v, want %v", fks, wantFKs)
	}
	var artifactFKs int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM pg_constraint
		WHERE conrelid = 'request_attachments'::regclass AND contype = 'f'
		  AND confrelid IN ('artifacts'::regclass, 'artifact_versions'::regclass)`).Scan(&artifactFKs); err != nil {
		t.Fatal(err)
	}
	if artifactFKs != 0 {
		t.Fatalf("request_attachments has %d foreign keys onto the artifact tables, want 0 (an attachment is not an artifact)", artifactFKs)
	}

	// Its own index set: primary key, the panel read, the purge scan, the held
	// badge — and nothing more.
	var indexes int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM pg_indexes WHERE tablename = 'request_attachments'`).Scan(&indexes); err != nil {
		t.Fatal(err)
	}
	if indexes != 4 {
		t.Fatalf("request_attachments has %d indexes, want 4", indexes)
	}

	// The request FK is ON DELETE CASCADE, not NO ACTION or RESTRICT: a request
	// with attachments deletes cleanly, leaving nothing behind.
	var deleteAction string
	if err := db.QueryRow(ctx, `SELECT confdeltype::text FROM pg_constraint
		WHERE conname = 'request_attachments_request_id_fkey'`).Scan(&deleteAction); err != nil {
		t.Fatal(err)
	}
	if deleteAction != "c" {
		t.Fatalf("the request foreign key's ON DELETE action is %q, want 'c' (CASCADE)", deleteAction)
	}
	d := newAttachmentsDB(t)
	reqID := d.insertReq(req{ref: "ref_SCHEMA_056_0000001"})
	d.mustAttachment(attachment{request: reqID})
	d.mustExec(`DELETE FROM team_requests WHERE id = $1`, reqID)
	if n := d.attachmentCount(reqID); n != 0 {
		t.Fatalf("%d attachments survived their request", n)
	}

	// The migration is embedded, and its prefix is unique (TestMigrationPrefixes
	// covers the set; this states the version the file must carry).
	found := false
	for _, m := range migrations {
		if m.Version == "056_request_attachments" {
			found = true
		}
	}
	if !found {
		t.Fatal("migration 056_request_attachments is not in the embedded set")
	}
}
