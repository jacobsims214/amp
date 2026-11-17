package service

// Publishing an artifact through a one-time upload URL (AMP #4937). Runs
// against Postgres when TEST_DATABASE_URL is set:
//
//	TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:PORT/amp_test?sslmode=disable \
//	go test ./internal/service -run ArtifactUpload -count=1

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/simstech/amp-api/internal/auth"
	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
)

type artUploadFixture struct {
	t        *testing.T
	ctx      context.Context
	svc      *Service
	db       *pgx.Conn
	task     int
	worker   authz.Principal
	stranger authz.Principal
}

func newArtUploadFixture(t *testing.T) *artUploadFixture {
	t.Helper()
	repo := submitTestRepo(t)
	ctx := context.Background()
	db, err := pgx.Connect(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { db.Close(context.Background()) })
	suffix := rand.IntN(1_000_000_000)
	q := func(sql string, args ...any) int {
		t.Helper()
		var id int
		if err := db.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return id
	}
	user := func(label string) authz.Principal {
		email := fmt.Sprintf("upload-%s-%d@upload.test", label, suffix)
		id := q(`INSERT INTO users (subject, email, kind) VALUES ($1, $2, $3) RETURNING id`,
			fmt.Sprintf("upload-%s-%d", label, suffix), email, domain.UserKindHuman)
		return authz.Principal{UserID: id, Email: email, Channel: auth.ChannelMCP, AuthMethod: auth.AuthDexSession}
	}
	org := q(`INSERT INTO orgs (name, slug) VALUES ('Upload org', $1) RETURNING id`, fmt.Sprintf("upload-%d", suffix))
	project := q(`INSERT INTO projects (name, code, org_id) VALUES ('Uploads', $1, $2) RETURNING id`, fmt.Sprintf("UPL%d", suffix), org)
	epic := q(`INSERT INTO epics (project_id, name) VALUES ($1, 'E') RETURNING id`, project)
	story := q(`INSERT INTO stories (project_id, epic_id, name) VALUES ($1, $2, 'S') RETURNING id`, project, epic)
	f := &artUploadFixture{t: t, ctx: ctx, db: db,
		task:     q(`INSERT INTO tasks (project_id, epic_id, story_id, name) VALUES ($1, $2, $3, 'Mockup') RETURNING id`, project, epic, story),
		worker:   user("worker"),
		stranger: user("stranger"),
	}
	if _, err := db.Exec(ctx, `INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'owner')`, org, f.worker.UserID); err != nil {
		t.Fatal(err)
	}
	f.svc = New(repo, nil, nil)
	f.svc.AppURL = "https://amp.example.test"
	return f
}

// screenshotHTML is a ~2 MB self-contained page, the size a mockup with
// embedded screenshots reaches.
func screenshotHTML(seed string) []byte {
	var b bytes.Buffer
	b.WriteString("<!doctype html><html><body><h1>" + seed + "</h1><img src=\"data:image/png;base64,")
	for b.Len() < 2<<20 {
		b.WriteString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk")
	}
	b.WriteString("\"></body></html>")
	return b.Bytes()
}

func sumOf(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func (f *artUploadFixture) begin(p authz.Principal, req PublishArtifactRequest, content []byte) (*ArtifactUpload, string) {
	f.t.Helper()
	u, err := f.svc.BeginArtifactUpload(f.ctx, p, req, int64(len(content)), sumOf(content))
	if err != nil {
		f.t.Fatalf("BeginArtifactUpload: %v", err)
	}
	if !strings.HasPrefix(u.URL, "https://amp.example.test"+ArtifactUploadPath) {
		f.t.Fatalf("upload URL %q isn't under %s", u.URL, ArtifactUploadPath)
	}
	return u, strings.TrimPrefix(u.URL, "https://amp.example.test"+ArtifactUploadPath)
}

func (f *artUploadFixture) content(id, version int) []byte {
	f.t.Helper()
	v, err := f.svc.Repo.GetArtifactVersion(f.ctx, id, version)
	if err != nil {
		f.t.Fatalf("version %d of %d: %v", version, id, err)
	}
	return v.Content
}

func TestArtifactUpload_PublishesToTicketThenAsNewVersion(t *testing.T) {
	f := newArtUploadFixture(t)
	v1 := screenshotHTML("v1")
	_, token := f.begin(f.worker, PublishArtifactRequest{TaskID: &f.task, Title: "Settings mockup", Kind: "html"}, v1)

	a, err := f.svc.StoreArtifactUpload(f.ctx, token, bytes.NewReader(v1))
	if err != nil {
		t.Fatalf("StoreArtifactUpload: %v", err)
	}
	if a.TaskID == nil || *a.TaskID != f.task || a.CurrentVersion != 1 || a.URL == "" {
		t.Fatalf("published %+v, want v1 on ticket %d with a url", a, f.task)
	}
	if got := f.content(a.ID, 1); !bytes.Equal(got, v1) {
		t.Fatalf("stored %d bytes, want the %d uploaded", len(got), len(v1))
	}

	// A retry of the same URL (its response lost) answers with the same
	// artifact and adds nothing.
	again, err := f.svc.StoreArtifactUpload(f.ctx, token, bytes.NewReader(v1))
	if err != nil || again.ID != a.ID || again.CurrentVersion != 1 {
		t.Fatalf("retry = %+v, %v; want artifact %d still at v1", again, err, a.ID)
	}

	v2 := screenshotHTML("v2")
	_, token2 := f.begin(f.worker, PublishArtifactRequest{ArtifactID: a.ID, Note: "tighter spacing"}, v2)
	b, err := f.svc.StoreArtifactUpload(f.ctx, token2, bytes.NewReader(v2))
	if err != nil {
		t.Fatalf("StoreArtifactUpload v2: %v", err)
	}
	if b.ID != a.ID || b.CurrentVersion != 2 {
		t.Fatalf("v2 publish = artifact %d v%d, want %d v2", b.ID, b.CurrentVersion, a.ID)
	}
	if got := f.content(a.ID, 2); !bytes.Equal(got, v2) {
		t.Fatal("v2 content differs from the upload")
	}
}

func TestArtifactUpload_WrongBytesAreRefusedAndURLStaysUsable(t *testing.T) {
	f := newArtUploadFixture(t)
	html := screenshotHTML("x")
	_, token := f.begin(f.worker, PublishArtifactRequest{TaskID: &f.task, Title: "Mockup", Kind: "html"}, html)

	tampered := append([]byte{}, html...)
	tampered[100] ^= 1
	for name, body := range map[string][]byte{"different sha256": tampered, "short": html[:len(html)-1], "long": append(append([]byte{}, html...), 'x')} {
		if _, err := f.svc.StoreArtifactUpload(f.ctx, token, bytes.NewReader(body)); authz.StatusOf(err, 0) != 422 {
			t.Fatalf("%s: err = %v, want 422", name, err)
		}
	}
	if a, err := f.svc.StoreArtifactUpload(f.ctx, token, bytes.NewReader(html)); err != nil || a.CurrentVersion != 1 {
		t.Fatalf("the right file after refusals = %+v, %v; want v1", a, err)
	}
}

func TestArtifactUpload_Refusals(t *testing.T) {
	f := newArtUploadFixture(t)
	html := screenshotHTML("r")
	req := PublishArtifactRequest{TaskID: &f.task, Title: "Mockup", Kind: "html"}

	if _, err := f.svc.BeginArtifactUpload(f.ctx, f.stranger, req, int64(len(html)), sumOf(html)); err == nil {
		t.Fatal("a stranger to the project minted an upload URL")
	}
	for name, c := range map[string]struct {
		size int64
		sum  string
		kind string
	}{
		"too big":  {domain.MaxArtifactBytes + 1, sumOf(html), "html"},
		"no size":  {0, sumOf(html), "html"},
		"bad sum":  {10, "abc", "html"},
		"bad kind": {10, sumOf(html), "exe"},
	} {
		r := req
		r.Kind = c.kind
		if _, err := f.svc.BeginArtifactUpload(f.ctx, f.worker, r, c.size, c.sum); authz.StatusOf(err, 0) != 400 {
			t.Errorf("%s: err = %v, want 400", name, err)
		}
	}

	if _, err := f.svc.StoreArtifactUpload(f.ctx, "not-a-token", bytes.NewReader(html)); authz.StatusOf(err, 0) != 404 {
		t.Errorf("junk token: %v, want 404", err)
	}
	_, token := f.begin(f.worker, req, html)
	if _, err := f.db.Exec(f.ctx, `UPDATE artifact_uploads SET expires_at = NOW() - INTERVAL '1 second' WHERE token_hash = $1`,
		sumOf([]byte(token))); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.StoreArtifactUpload(f.ctx, token, bytes.NewReader(html)); authz.StatusOf(err, 0) != 410 {
		t.Errorf("expired: %v, want 410", err)
	}
}

// stallingReader hands out its first chunk, signals started, then holds the
// rest until release is closed: an upload on a slow link.
type stallingReader struct {
	data    []byte
	off     int
	started chan struct{}
	release chan struct{}
}

func (r *stallingReader) Read(p []byte) (int, error) {
	if r.off >= len(r.data) {
		return 0, io.EOF
	}
	if r.off > 0 {
		select {
		case <-r.release:
		case <-time.After(30 * time.Second):
			return 0, errors.New("stallingReader: never released")
		}
	} else {
		defer close(r.started)
	}
	n := copy(p[:min(len(p), 4096)], r.data[r.off:])
	r.off += n
	return n, nil
}

func TestArtifactUpload_SlowUploadRacingAStaleTakeoverPublishesOnce(t *testing.T) {
	f := newArtUploadFixture(t)
	html := screenshotHTML("race")
	_, token := f.begin(f.worker, PublishArtifactRequest{TaskID: &f.task, Title: "Mockup", Kind: "html"}, html)

	slow := &stallingReader{data: html, started: make(chan struct{}), release: make(chan struct{})}
	type result struct {
		a   *domain.Artifact
		err error
	}
	slowDone := make(chan result, 1)
	go func() {
		a, err := f.svc.StoreArtifactUpload(f.ctx, token, slow)
		slowDone <- result{a, err}
	}()
	select {
	case <-slow.started:
	case <-time.After(10 * time.Second):
		t.Fatal("the slow upload never started reading")
	}

	// Whatever claim the row holds now has gone stale, as it would after two
	// minutes of streaming, so a second PUT may take it over.
	if _, err := f.db.Exec(f.ctx, `UPDATE artifact_uploads SET claimed_at = NOW() - INTERVAL '3 minutes' WHERE token_hash = $1`,
		sumOf([]byte(token))); err != nil {
		t.Fatal(err)
	}
	fast, err := f.svc.StoreArtifactUpload(f.ctx, token, bytes.NewReader(html))
	if err != nil {
		t.Fatalf("the second PUT: %v", err)
	}
	close(slow.release)
	var got result
	select {
	case got = <-slowDone:
	case <-time.After(30 * time.Second):
		t.Fatal("the slow upload never returned")
	}
	if got.err != nil || got.a.ID != fast.ID || got.a.CurrentVersion != 1 {
		t.Fatalf("the slow upload = %+v, %v; want artifact %d at v1", got.a, got.err, fast.ID)
	}
	var versions int
	if err := f.db.QueryRow(f.ctx, `SELECT COUNT(*) FROM artifact_versions WHERE artifact_id = $1`, fast.ID).Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if versions != 1 {
		t.Fatalf("artifact %d has %d versions, want 1", fast.ID, versions)
	}
}

func TestArtifactUpload_ReleaseAndFinishActOnlyOnTheirOwnClaim(t *testing.T) {
	f := newArtUploadFixture(t)
	html := screenshotHTML("claim")
	_, token := f.begin(f.worker, PublishArtifactRequest{TaskID: &f.task, Title: "Mockup", Kind: "html"}, html)
	hash := sumOf([]byte(token))
	repo := f.svc.Repo

	first, err := repo.ClaimArtifactUpload(f.ctx, hash, artifactUploadStaleClaim)
	if err != nil || first == nil || first.ClaimedAt == nil {
		t.Fatalf("first claim = %+v, %v", first, err)
	}
	if _, err := f.db.Exec(f.ctx, `UPDATE artifact_uploads SET claimed_at = NOW() - INTERVAL '3 minutes' WHERE id = $1`, first.ID); err != nil {
		t.Fatal(err)
	}
	stale, err := repo.ArtifactUploadByToken(f.ctx, hash)
	if err != nil {
		t.Fatal(err)
	}
	second, err := repo.ClaimArtifactUpload(f.ctx, hash, artifactUploadStaleClaim)
	if err != nil || second == nil || second.ClaimedAt == nil {
		t.Fatalf("takeover = %+v, %v", second, err)
	}

	if err := repo.ReleaseArtifactUpload(f.ctx, stale.ID, *stale.ClaimedAt); err != nil {
		t.Fatal(err)
	}
	if ok, err := repo.FinishArtifactUpload(f.ctx, stale.ID, *stale.ClaimedAt, 1, 1); err != nil || ok {
		t.Fatalf("finish under a taken-over claim = %v, %v; want false", ok, err)
	}
	now, err := repo.ArtifactUploadByToken(f.ctx, hash)
	if err != nil {
		t.Fatal(err)
	}
	if now.ClaimedAt == nil || !now.ClaimedAt.Equal(*second.ClaimedAt) || now.DoneAt != nil {
		t.Fatalf("after the stale request's release and finish, row = claimed %v done %v; want the takeover's claim %v, not done",
			now.ClaimedAt, now.DoneAt, *second.ClaimedAt)
	}

	if ok, err := repo.FinishArtifactUpload(f.ctx, second.ID, *second.ClaimedAt, 1, 1); err != nil || !ok {
		t.Fatalf("finish under its own claim = %v, %v; want true", ok, err)
	}
	if ok, err := repo.FinishArtifactUpload(f.ctx, second.ID, *second.ClaimedAt, 1, 2); err != nil || ok {
		t.Fatalf("a second finish = %v, %v; want false", ok, err)
	}
}
