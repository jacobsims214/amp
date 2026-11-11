package repository

// Tests for request_links.go against a real Postgres (AMP #3875, epic 202 story
// 535; build slice C4). They run only when TEST_DATABASE_URL is set:
//
//	TEST_DATABASE_URL=postgres://postgres:postgres@localhost:5432/amp_test?sslmode=disable \
//	go test ./internal/repository/ -run 'RequestLink|LinkRequest|MarkDuplicate|DetachDuplicate|UnlinkRequest' \
//	    -count=1 -race
//
// The ticket's three criteria are what these assert, in order:
//
//  1. a replay yields one link and preserves the first attribution: the second
//     attach answers changed=false, returns the row the first attach wrote with
//     its created_by/created_at untouched, appends no second history row and does
//     not re-stamp planned_at — and it does so even after the target is gone,
//     because a relation already stored is stored.
//  2. concurrent A→B and B→A duplicate folds cannot cycle and cannot cross a
//     tenant: exactly one wins, the other is refused as a cycle, every duplicate
//     points straight at a non-duplicate canonical (one hop, never a chain), and a
//     canonical in another org — or in another team of the same org — is an
//     ordinary not-found that writes nothing.
//  3. a tombstone survives its target's deletion and stays readable history, and a
//     detach is atomic: duplicate_of and the state leave `duplicate` in one
//     statement along with rev + 1, the reopen counters, the removed link row and
//     exactly one transition event — while the fold itself stays readable in the
//     append-only history.
//
// Beyond the criteria the tests pin the properties the code's comments claim: the
// target's tenancy comes from the target's own table and never from metadata, a
// caller that supplies an org the target disagrees with is a caller bug rather
// than a 404, routing appends rather than replacing (D9's one active project is a
// different question from "where has this been routed"), an unlink is idempotent,
// a duplicate_of relation may not be written through the general link paths, and
// a project with no org has no tenant to verify.
//
// Each test builds its own orgs, teams, users, project hierarchy and requests with
// a random suffix (requestRowsFixture), so repeated runs and tests in the same
// database don't see each other's rows. Nothing here writes outside those rows.

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/simstech/amp-api/internal/domain"
)

// requestLinksFixture is requestRowsFixture plus the project hierarchy a link can
// point at (one project in the fixture's org, one more in the same org, one in the
// other org), and the second user the attribution assertions need.
type requestLinksFixture struct {
	*requestRowsFixture
	user2 int

	project      int // in org
	project2     int // in org, for re-routing (D9)
	projectOther int // in the other org, for cross-tenant checks
	projectNoOrg int // a pre-004 project with no org: no tenant to verify

	epic      int
	story     int
	task      int
	task2     int // a second ticket in the same project
	artifact  int
	receipt   int
	taskOther int // a ticket in the other org's project
	teamOther int // a team in the other org: the tenant a cross-org request lives in
}

func newRequestLinksFixture(t *testing.T) *requestLinksFixture {
	t.Helper()
	base := newRequestRowsFixture(t)
	f := &requestLinksFixture{requestRowsFixture: base}
	insert := func(sql string, dst *int, args ...any) {
		t.Helper()
		if err := f.r.db.QueryRow(f.ctx, sql, args...).Scan(dst); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	insert(`INSERT INTO users (subject, email) VALUES ($1, $2) RETURNING id`, &f.user2,
		fmt.Sprintf("links-user2-%d", f.suffix), fmt.Sprintf("links-user2-%d@links.test", f.suffix))

	code := func(label string) string { return fmt.Sprintf("%s%d", label, f.suffix) }
	insert(`INSERT INTO projects (name, code, org_id) VALUES ('links project', $1, $2) RETURNING id`,
		&f.project, code("lnk"), f.org)
	insert(`INSERT INTO projects (name, code, org_id) VALUES ('links project 2', $1, $2) RETURNING id`,
		&f.project2, code("lnk2"), f.org)
	insert(`INSERT INTO projects (name, code, org_id) VALUES ('other project', $1, $2) RETURNING id`,
		&f.projectOther, code("lnko"), f.other)
	insert(`INSERT INTO projects (name, code) VALUES ('no org project', $1) RETURNING id`,
		&f.projectNoOrg, code("lnkn"))

	insert(`INSERT INTO epics (project_id, name) VALUES ($1, 'epic') RETURNING id`, &f.epic, f.project)
	insert(`INSERT INTO stories (project_id, epic_id, name) VALUES ($1, $2, 'story') RETURNING id`,
		&f.story, f.project, f.epic)
	insert(`INSERT INTO tasks (project_id, epic_id, story_id, name) VALUES ($1, $2, $3, 'task') RETURNING id`,
		&f.task, f.project, f.epic, f.story)
	insert(`INSERT INTO tasks (project_id, epic_id, story_id, name) VALUES ($1, $2, $3, 'task 2') RETURNING id`,
		&f.task2, f.project, f.epic, f.story)
	insert(`INSERT INTO artifacts (project_id, title, kind) VALUES ($1, 'the mockup', 'html') RETURNING id`,
		&f.artifact, f.project)
	insert(`INSERT INTO task_receipts (task_id, project_id, summary, state) VALUES ($1, $2, 'done', 'final') RETURNING id`,
		&f.receipt, f.task, f.project)
	insert(`INSERT INTO teams (org_id, name) VALUES ($1, 'Other desk') RETURNING id`, &f.teamOther, f.other)
	otherEpic := 0
	insert(`INSERT INTO epics (project_id, name) VALUES ($1, 'other epic') RETURNING id`, &otherEpic, f.projectOther)
	otherStory := 0
	insert(`INSERT INTO stories (project_id, epic_id, name) VALUES ($1, $2, 'other story') RETURNING id`,
		&otherStory, f.projectOther, otherEpic)
	insert(`INSERT INTO tasks (project_id, epic_id, story_id, name) VALUES ($1, $2, $3, 'other org task') RETURNING id`,
		&f.taskOther, f.projectOther, otherEpic, otherStory)
	return f
}

// newRequest writes one request in the fixture's first team and returns it as
// stored (id, rev=1, no claim, no owner).
func (f *requestLinksFixture) newRequest(label string, state domain.RequestStatus) *domain.TeamRequest {
	f.t.Helper()
	base := requestTime(-time.Hour)
	req := &domain.TeamRequest{
		PublicRef:        f.ref(label),
		OrgID:            f.org,
		TeamID:           f.team,
		RequesterContact: "requester@links.test",
		SourceKind:       domain.RequestSourceWeb,
		Subject:          "Link me " + label,
		State:            state,
		Rev:              1,
		Metadata:         domain.Metadata{},
		CreatedAt:        base,
		UpdatedAt:        base,
	}
	req.ID = f.insertRequest(req)
	return req
}

// otherTeamRequest writes one request in the fixture's second team — the same org,
// a different tenant — which every write in this file must treat as not-found.
func (f *requestLinksFixture) otherTeamRequest(label string) *domain.TeamRequest {
	f.t.Helper()
	req := f.newRequest(label, domain.RequestStatusNew)
	if _, err := f.r.db.Exec(f.ctx, `UPDATE team_requests SET team_id = $2 WHERE id = $1`, req.ID, f.team2); err != nil {
		f.t.Fatalf("move request %d to the second team: %v", req.ID, err)
	}
	req.TeamID = f.team2
	return req
}

// otherOrgRequest writes one request in the other org's own team, which is the
// tenant a cross-org fold or link must never reach.
func (f *requestLinksFixture) otherOrgRequest(label string) *domain.TeamRequest {
	f.t.Helper()
	base := requestTime(-time.Hour)
	req := &domain.TeamRequest{
		PublicRef:        f.ref(label),
		OrgID:            f.other,
		TeamID:           f.teamOther,
		RequesterContact: "requester@links.test",
		SourceKind:       domain.RequestSourceWeb,
		Subject:          "Elsewhere " + label,
		State:            domain.RequestStatusNew,
		Rev:              1,
		Metadata:         domain.Metadata{},
		CreatedAt:        base,
		UpdatedAt:        base,
	}
	req.ID = f.insertRequest(req)
	return req
}

// events returns the request's history in append order, read straight from the
// table so the assertions see what was actually stored.
func (f *requestLinksFixture) events(requestID int) []domain.RequestEvent {
	f.t.Helper()
	rows, err := f.r.db.Query(f.ctx, `SELECT id, request_id, org_id, team_id, action, from_state, to_state,
		actor, actor_user_id, detail, created_at FROM request_events WHERE request_id = $1 ORDER BY id`, requestID)
	if err != nil {
		f.t.Fatalf("history of request %d: %v", requestID, err)
	}
	defer rows.Close()
	var out []domain.RequestEvent
	for rows.Next() {
		var ev domain.RequestEvent
		if err := rows.Scan(&ev.ID, &ev.RequestID, &ev.OrgID, &ev.TeamID, &ev.Action,
			&ev.FromStatus, &ev.ToStatus, &ev.Actor, &ev.ActorUserID, &ev.Detail, &ev.CreatedAt); err != nil {
			f.t.Fatalf("scan history of request %d: %v", requestID, err)
		}
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		f.t.Fatalf("history of request %d: %v", requestID, err)
	}
	return out
}

// actions summarises the request's history as action counts, which is how these
// tests state "exactly one link event and no others".
func (f *requestLinksFixture) actions(requestID int) map[domain.RequestEventAction]int {
	f.t.Helper()
	counts := map[domain.RequestEventAction]int{}
	for _, ev := range f.events(requestID) {
		counts[ev.Action]++
	}
	return counts
}

// linkRows reads the request's stored links directly, so an assertion can see a
// row the public read might filter.
func (f *requestLinksFixture) linkRows(requestID int) []domain.RequestLink {
	f.t.Helper()
	rows, err := f.r.db.Query(f.ctx, `SELECT `+requestLinkCols+` FROM request_links
		WHERE request_id = $1 ORDER BY id`, requestID)
	if err != nil {
		f.t.Fatalf("links of request %d: %v", requestID, err)
	}
	defer rows.Close()
	out := []domain.RequestLink{}
	for rows.Next() {
		var l domain.RequestLink
		if err := scanRequestLink(rows, &l); err != nil {
			f.t.Fatalf("scan link of request %d: %v", requestID, err)
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		f.t.Fatalf("links of request %d: %v", requestID, err)
	}
	return out
}

// mustGet loads the request through the tenant-scoped read, so assertions see the
// stored row rather than the value a write returned.
func (f *requestLinksFixture) mustGet(req *domain.TeamRequest) *domain.TeamRequest {
	f.t.Helper()
	got, err := f.r.GetRequest(f.ctx, req.ID, f.org, req.TeamID)
	if err != nil {
		f.t.Fatalf("GetRequest(%d): %v", req.ID, err)
	}
	return got
}

// linkWrite is the caller's part of an attach for this fixture's tenant, with the
// target's verified org spelled out (the one thing the caller must supply).
func (f *requestLinksFixture) linkWrite(req *domain.TeamRequest, kind domain.RequestLinkKind,
	target domain.RequestTargetKind, targetID, targetOrg int) RequestLinkWrite {
	return RequestLinkWrite{
		RequestID:   req.ID,
		OrgID:       f.org,
		TeamID:      req.TeamID,
		LinkKind:    kind,
		TargetKind:  target,
		TargetID:    targetID,
		TargetOrgID: targetOrg,
		CreatedBy:   &f.user,
		Actor:       "triager",
		ActorUserID: &f.user,
	}
}

// dupWrite is the caller's part of a fold or a detach for this fixture's tenant.
func (f *requestLinksFixture) dupWrite(req *domain.TeamRequest, canonical int) RequestDuplicateWrite {
	return RequestDuplicateWrite{
		RequestID:   req.ID,
		OrgID:       f.org,
		TeamID:      req.TeamID,
		CanonicalID: canonical,
	}
}

// TestLinkRequestReplayKeepsFirstAttribution is criterion 1: replaying an attach
// yields one link, keeps the first attribution, and writes nothing at all the
// second time.
func TestLinkRequestReplayKeepsFirstAttribution(t *testing.T) {
	f := newRequestLinksFixture(t)
	req := f.newRequest("linkreplay000", domain.RequestStatusTriaging)

	first, changed, err := f.r.LinkRequest(f.ctx, f.linkWrite(req, domain.RequestLinkPlannedAs,
		domain.RequestTargetEpic, f.epic, f.org))
	if err != nil {
		t.Fatalf("first attach: %v", err)
	}
	if !changed {
		t.Fatal("first attach answered changed=false: nothing was written")
	}
	if first.ID == 0 || first.CreatedBy == nil || *first.CreatedBy != f.user {
		t.Fatalf("first attach stored %+v, want a row created by user %d", first, f.user)
	}
	if rows := f.linkRows(req.ID); len(rows) != 1 {
		t.Fatalf("after the first attach there are %d link rows, want 1", len(rows))
	}
	stamped := f.mustGet(req)
	if stamped.PlannedAt == nil {
		t.Fatal("a planned_as attach did not stamp planned_at")
	}
	// A second relation of the same kind: the stamp is first-write-wins across
	// planned_as links, not per target.
	if _, changed, err := f.r.LinkRequest(f.ctx, f.linkWrite(req, domain.RequestLinkPlannedAs,
		domain.RequestTargetStory, f.story, f.org)); err != nil || !changed {
		t.Fatalf("second planned_as attach: changed=%v err=%v, want a new row", changed, err)
	}
	afterSecond := f.mustGet(req)
	if afterSecond.PlannedAt == nil || !afterSecond.PlannedAt.Equal(*stamped.PlannedAt) {
		t.Fatalf("planned_at moved on a second planned_as attach: %v -> %v", stamped.PlannedAt, afterSecond.PlannedAt)
	}

	// The replay: a different creator, the same relation.
	replay := f.linkWrite(req, domain.RequestLinkPlannedAs, domain.RequestTargetEpic, f.epic, f.org)
	replay.CreatedBy = &f.user2
	replay.ActorUserID = &f.user2
	got, changed, err := f.r.LinkRequest(f.ctx, replay)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if changed {
		t.Fatal("replay answered changed=true: it wrote a second link")
	}
	if got.ID != first.ID {
		t.Fatalf("replay returned link %d, want the first attach's row %d", got.ID, first.ID)
	}
	if got.CreatedBy == nil || *got.CreatedBy != f.user {
		t.Fatalf("replay rewrote created_by to %v: first attribution must win", got.CreatedBy)
	}
	if !got.CreatedAt.Equal(first.CreatedAt) {
		t.Fatalf("replay moved created_at %v -> %v: first attribution must win", first.CreatedAt, got.CreatedAt)
	}
	if rows := f.linkRows(req.ID); len(rows) != 2 { // epic + story, not three
		t.Fatalf("after the replay there are %d link rows, want 2", len(rows))
	}

	// One history row per real attach, none for the replay: two attaches, two events.
	acts := f.actions(req.ID)
	if acts[domain.RequestActionLink] != 2 {
		t.Fatalf("link events = %d, want 2 (one per real attach, none for the replay)", acts[domain.RequestActionLink])
	}
	if total := len(f.events(req.ID)); total != 2 {
		t.Fatalf("history rows = %d, want exactly the 2 link events", total)
	}
	// rev is the token for versioned content: a lineage stamp must not invalidate a
	// client's copy of the brief.
	afterReplay := f.mustGet(req)
	if afterReplay.Rev != 1 {
		t.Fatalf("rev = %d after link writes, want 1 (links are lineage, not content edits)", afterReplay.Rev)
	}
}

// TestLinkRequestTargetTenancyIsVerified is the tenant half of criterion 2 for the
// attach path: the org comes from the target's own table, a caller that supplies a
// different one is a bug rather than a 404, and a target in another org is the
// surface's one not-found.
func TestLinkRequestTargetTenancyIsVerified(t *testing.T) {
	f := newRequestLinksFixture(t)
	req := f.newRequest("linktenant000", domain.RequestStatusTriaging)
	// Metadata that names the other org, to prove nothing in the write path reads it
	// (an inherited or patched JSONB key is not a tenant check).
	if _, err := f.r.db.Exec(f.ctx, `UPDATE team_requests SET metadata = $2 WHERE id = $1`,
		req.ID, fmt.Sprintf(`{"org_id": %d, "target_org_id": %d}`, f.other, f.other)); err != nil {
		t.Fatalf("set metadata: %v", err)
	}

	cases := []struct {
		name       string
		kind       domain.RequestLinkKind
		target     domain.RequestTargetKind
		targetID   int
		targetOrg  int
		wantErr    error
		wantDetail string
	}{
		{
			name: "a verified org that matches the target and the request", kind: domain.RequestLinkDeliveredBy,
			target: domain.RequestTargetArtifact, targetID: f.artifact, targetOrg: f.org,
		},
		{
			name: "no verified org at all: the caller did not resolve the target", kind: domain.RequestLinkDeliveredBy,
			target: domain.RequestTargetTask, targetID: f.task, targetOrg: 0, wantErr: ErrRequestLinkTargetIdentity,
		},
		{
			name: "an org the target's own table disagrees with", kind: domain.RequestLinkDeliveredBy,
			target: domain.RequestTargetTask, targetID: f.task, targetOrg: f.other, wantErr: ErrRequestLinkTargetIdentity,
		},
		{
			name: "a target in another org is answered as missing, not as wrong-tenant", kind: domain.RequestLinkDeliveredBy,
			target: domain.RequestTargetTask, targetID: f.taskOther, targetOrg: f.other, wantErr: ErrNotFound,
		},
		{
			name: "a project with no org has no tenant to verify", kind: domain.RequestLinkRoutesTo,
			target: domain.RequestTargetProject, targetID: f.projectNoOrg, targetOrg: f.org, wantErr: ErrNotFound,
		},
		{
			name: "an id that names nothing", kind: domain.RequestLinkDeliveredBy,
			target: domain.RequestTargetTask, targetID: 1 << 30, targetOrg: f.org, wantErr: ErrNotFound,
		},
		{
			name: "a request in another org as the target", kind: domain.RequestLinkRelatesTo,
			target: domain.RequestTargetRequest, targetID: 1 << 30, targetOrg: f.other, wantErr: ErrNotFound,
		},
		{
			name: "the request itself: a self-link names nothing", kind: domain.RequestLinkRelatesTo,
			target: domain.RequestTargetRequest, targetID: req.ID, targetOrg: f.org, wantDetail: "itself",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, changed, err := f.r.LinkRequest(f.ctx, f.linkWrite(req, tc.kind, tc.target, tc.targetID, tc.targetOrg))
			if tc.wantErr == nil && tc.wantDetail == "" {
				if err != nil {
					t.Fatalf("attach: %v", err)
				}
				if !changed {
					t.Fatal("attach answered changed=false, want a new row")
				}
				return
			}
			if err == nil {
				t.Fatalf("attach was accepted; want an error")
			}
			if changed {
				t.Fatal("a refused attach reported changed=true")
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("error = %v, want errors.Is(%v)", err, tc.wantErr)
			}
			if tc.wantDetail != "" && !strings.Contains(err.Error(), tc.wantDetail) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.wantDetail)
			}
		})
	}

	// No refusal wrote anything: the one successful case above is the only row, and
	// no history was appended for the refusals.
	if rows := f.linkRows(req.ID); len(rows) != 1 {
		t.Fatalf("link rows = %d after the refusals, want 1", len(rows))
	}
	if acts := f.actions(req.ID); acts[domain.RequestActionLink] != 1 {
		t.Fatalf("link events = %d, want 1", acts[domain.RequestActionLink])
	}
}

// TestLinkRequestTombstoneSurvivesTargetDeletion is part of criterion 3: the link
// outlives the ticket it names, stays readable, and a replay of the same relation
// is still a replay rather than a 404.
func TestLinkRequestTombstoneSurvivesTargetDeletion(t *testing.T) {
	f := newRequestLinksFixture(t)
	req := f.newRequest("linktombstone0", domain.RequestStatusInProgress)

	link, changed, err := f.r.LinkRequest(f.ctx, f.linkWrite(req, domain.RequestLinkDeliveredBy,
		domain.RequestTargetTask, f.task, f.org))
	if err != nil || !changed {
		t.Fatalf("attach: changed=%v err=%v", changed, err)
	}

	// The plan changes and the ticket is deleted. The delete must not be blocked by
	// the link (no FK on target_id), and must not take the link with it.
	if _, err := f.r.db.Exec(f.ctx, `DELETE FROM tasks WHERE id = $1`, f.task); err != nil {
		t.Fatalf("delete the linked task: %v (a link must not block its target's deletion)", err)
	}

	stored, err := f.r.RequestLinks(f.ctx, req.ID, f.org, f.team)
	if err != nil {
		t.Fatalf("RequestLinks: %v", err)
	}
	found := false
	for _, l := range stored {
		if l.ID == link.ID {
			found = true
			if l.TargetID != f.task {
				t.Fatalf("tombstone names target %d, want %d", l.TargetID, f.task)
			}
		}
	}
	if !found {
		t.Fatalf("the link %d vanished with its target: a link is lineage, not a join", link.ID)
	}

	// A replay after the deletion is still a replay: the tombstone answers, so a
	// harmless retry does not turn into a 404 and does not manufacture a second row.
	got, changed, err := f.r.LinkRequest(f.ctx, f.linkWrite(req, domain.RequestLinkDeliveredBy,
		domain.RequestTargetTask, f.task, f.org))
	if err != nil {
		t.Fatalf("replay against a tombstone: %v", err)
	}
	if changed {
		t.Fatal("a replay against a tombstone wrote a row")
	}
	if got.ID != link.ID {
		t.Fatalf("replay returned link %d, want the tombstone %d", got.ID, link.ID)
	}
	if rows := f.linkRows(req.ID); len(rows) != 1 {
		t.Fatalf("link rows = %d, want the one tombstone", len(rows))
	}

	// A NEW relation to the deleted target is not a replay: the target has to exist
	// and be in this tenant before a row may claim it.
	if _, _, err := f.r.LinkRequest(f.ctx, f.linkWrite(req, domain.RequestLinkDeliveredBy,
		domain.RequestTargetTask, f.task2, f.other)); !errors.Is(err, ErrRequestLinkTargetIdentity) {
		t.Fatalf("a new relation with a wrong verified org = %v, want ErrRequestLinkTargetIdentity", err)
	}
	if _, err := f.r.db.Exec(f.ctx, `DELETE FROM tasks WHERE id = $1`, f.task2); err != nil {
		t.Fatalf("delete the second task: %v", err)
	}
	if _, _, err := f.r.LinkRequest(f.ctx, f.linkWrite(req, domain.RequestLinkDeliveredBy,
		domain.RequestTargetTask, f.task2, f.org)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a new relation to a deleted target = %v, want ErrNotFound", err)
	}
}

// TestLinkRequestRouteAppendsAndKeepsOneActive is D9 read against the lineage: one
// active project at a time, re-routing appends instead of replacing, and nothing in
// the history is deleted by a re-route.
func TestLinkRequestRouteAppendsAndKeepsOneActive(t *testing.T) {
	f := newRequestLinksFixture(t)
	req := f.newRequest("linkroutes0000", domain.RequestStatusTriaging)

	if _, changed, err := f.r.LinkRequest(f.ctx, f.linkWrite(req, domain.RequestLinkRoutesTo,
		domain.RequestTargetProject, f.project, f.org)); err != nil || !changed {
		t.Fatalf("first route: changed=%v err=%v", changed, err)
	}
	active, err := f.r.ActiveRequestRoute(f.ctx, req.ID, f.org, f.team)
	if err != nil || active == nil || active.TargetID != f.project {
		t.Fatalf("active route = %+v err=%v, want project %d", active, err, f.project)
	}

	// A second project without superseding it is D9's refusal, and it tells the
	// caller which project the request is on so the 409 can name it.
	_, _, err = f.r.LinkRequest(f.ctx, f.linkWrite(req, domain.RequestLinkRoutesTo,
		domain.RequestTargetProject, f.project2, f.org))
	var conflict *RequestRouteConflict
	if !errors.As(err, &conflict) {
		t.Fatalf("second route = %v, want *RequestRouteConflict", err)
	}
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("RequestRouteConflict does not match ErrConflict: %v", err)
	}
	if conflict.CurrentID != f.project || conflict.WantedID != f.project2 {
		t.Fatalf("conflict names current=%d wanted=%d, want %d/%d", conflict.CurrentID, conflict.WantedID, f.project, f.project2)
	}
	if rows := f.linkRows(req.ID); len(rows) != 1 {
		t.Fatalf("a refused re-route wrote a row: %d links", len(rows))
	}

	// A deliberate re-route supersedes: the new row is appended and the previous
	// routing stays readable.
	w := f.linkWrite(req, domain.RequestLinkRoutesTo, domain.RequestTargetProject, f.project2, f.org)
	w.SupersedeRoute = true
	if _, changed, err := f.r.LinkRequest(f.ctx, w); err != nil || !changed {
		t.Fatalf("superseding route: changed=%v err=%v", changed, err)
	}
	active, err = f.r.ActiveRequestRoute(f.ctx, req.ID, f.org, f.team)
	if err != nil || active == nil || active.TargetID != f.project2 {
		t.Fatalf("active route after the re-route = %+v err=%v, want project %d", active, err, f.project2)
	}
	all, err := f.r.RequestLinks(f.ctx, req.ID, f.org, f.team, domain.RequestLinkRoutesTo)
	if err != nil {
		t.Fatalf("RequestLinks(routes_to): %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("routing history has %d rows, want 2 (the superseded route is lineage, not deleted)", len(all))
	}
	if all[0].TargetID != f.project || all[1].TargetID != f.project2 {
		t.Fatalf("routing history order = %d,%d, want %d,%d (attach order)",
			all[0].TargetID, all[1].TargetID, f.project, f.project2)
	}
	// The superseded row's event says which project it replaced, so a reader can
	// tell a re-route from two unrelated routes.
	var sawSupersede bool
	for _, ev := range f.events(req.ID) {
		if ev.Action == domain.RequestActionLink && strings.Contains(ev.Detail, fmt.Sprintf(`"superseded_project_id":%d`, f.project)) {
			sawSupersede = true
		}
	}
	if !sawSupersede {
		t.Fatalf("no link event names the superseded project; history: %v", f.events(req.ID))
	}

	// Unlinking the active route leaves the older one as the answer, and a replay of
	// the unlink writes nothing.
	if changed, err := f.r.UnlinkRequest(f.ctx, f.linkWrite(req, domain.RequestLinkRoutesTo,
		domain.RequestTargetProject, f.project2, f.org)); err != nil || !changed {
		t.Fatalf("unlink active route: changed=%v err=%v", changed, err)
	}
	active, err = f.r.ActiveRequestRoute(f.ctx, req.ID, f.org, f.team)
	if err != nil || active == nil || active.TargetID != f.project {
		t.Fatalf("active route after the unlink = %+v err=%v, want the older project %d", active, err, f.project)
	}
	eventsBefore := len(f.events(req.ID))
	if changed, err := f.r.UnlinkRequest(f.ctx, f.linkWrite(req, domain.RequestLinkRoutesTo,
		domain.RequestTargetProject, f.project2, f.org)); err != nil || changed {
		t.Fatalf("replayed unlink: changed=%v err=%v, want a no-op", changed, err)
	}
	if after := len(f.events(req.ID)); after != eventsBefore {
		t.Fatalf("a replayed unlink appended %d history rows", after-eventsBefore)
	}
}

// TestLinkRequestRefusesDuplicateKindAndBadCreator pins the two write-path guards
// that are not about concurrency: a duplicate_of relation belongs to MarkDuplicate,
// and a created_by that names no user is an ordinary not-found.
func TestLinkRequestRefusesDuplicateKindAndBadCreator(t *testing.T) {
	f := newRequestLinksFixture(t)
	req := f.newRequest("linkguards0000", domain.RequestStatusTriaging)
	canonical := f.newRequest("linkguardscan00", domain.RequestStatusNew)

	if _, _, err := f.r.LinkRequest(f.ctx, f.linkWrite(req, domain.RequestLinkDuplicateOf,
		domain.RequestTargetRequest, canonical.ID, f.org)); !errors.Is(err, ErrRequestLinkDuplicate) {
		t.Fatalf("duplicate_of through LinkRequest = %v, want ErrRequestLinkDuplicate", err)
	}
	if rows := f.linkRows(req.ID); len(rows) != 0 {
		t.Fatalf("the refused duplicate_of wrote %d rows", len(rows))
	}

	bad := f.linkWrite(req, domain.RequestLinkRelatesTo, domain.RequestTargetRequest, canonical.ID, f.org)
	missing := 1 << 30
	bad.CreatedBy = &missing
	if _, _, err := f.r.LinkRequest(f.ctx, bad); !errors.Is(err, ErrNotFound) {
		t.Fatalf("attach with created_by naming no user = %v, want ErrNotFound", err)
	}
	if rows := f.linkRows(req.ID); len(rows) != 0 {
		t.Fatalf("the refused attach wrote %d rows", len(rows))
	}
}

// TestMarkDuplicateConcurrentOppositeFoldsCannotCycle is criterion 2's headline:
// two triagers marking A a duplicate of B and B a duplicate of A at the same time.
// Whatever the scheduler does, the outcome is one canonical, one duplicate, one
// hop — never a cycle and never a two-hop chain.
func TestMarkDuplicateConcurrentOppositeFoldsCannotCycle(t *testing.T) {
	f := newRequestLinksFixture(t)

	// Repeated because the interesting interleavings are decided by the lock order,
	// and one pass proves only that that pass was safe.
	for round := 0; round < 6; round++ {
		a := f.newRequest(fmt.Sprintf("linkcyclea%02d", round), domain.RequestStatusTriaging)
		b := f.newRequest(fmt.Sprintf("linkcycleb%02d", round), domain.RequestStatusTriaging)

		start := make(chan struct{})
		type result struct {
			name    string
			changed bool
			err     error
		}
		results := make(chan result, 2)
		var wg sync.WaitGroup
		fold := func(name string, req *domain.TeamRequest, canonical int) {
			defer wg.Done()
			w := f.dupWrite(req, canonical)
			w.CreatedBy = &f.user
			w.Actor = name
			w.ActorUserID = &f.user
			<-start
			_, changed, err := f.r.MarkDuplicate(f.ctx, w)
			results <- result{name: name, changed: changed, err: err}
		}
		wg.Add(2)
		go fold("A-into-B", a, b.ID)
		go fold("B-into-A", b, a.ID)
		close(start)
		wg.Wait()
		close(results)

		var won, refused int
		for res := range results {
			switch {
			case res.err == nil && res.changed:
				won++
			case res.err != nil:
				refused++
				if !errors.Is(res.err, ErrConflict) {
					t.Fatalf("round %d: %s was refused with %v, want ErrConflict", round, res.name, res.err)
				}
				var dup *RequestDuplicateRefused
				if !errors.As(res.err, &dup) {
					t.Fatalf("round %d: %s refused with %v, want *RequestDuplicateRefused", round, res.name, res.err)
				}
				if dup.Reason != duplicateRefusedCycle && dup.Reason != duplicateRefusedFolded {
					t.Fatalf("round %d: %s refused as %q, want a cycle or an existing fold", round, res.name, dup.Reason)
				}
			default:
				t.Fatalf("round %d: %s answered changed=false with no error", round, res.name)
			}
		}
		if won != 1 || refused != 1 {
			t.Fatalf("round %d: %d folds won and %d were refused, want exactly one of each", round, won, refused)
		}

		// Exactly one canonical, one duplicate, and the duplicate points one hop at a
		// request that is not itself a duplicate.
		var duplicates, canonicals int
		for _, id := range []int{a.ID, b.ID} {
			var dupOf *int
			var state string
			if err := f.r.db.QueryRow(f.ctx, `SELECT duplicate_of, state FROM team_requests WHERE id = $1`, id).
				Scan(&dupOf, &state); err != nil {
				t.Fatalf("read request %d: %v", id, err)
			}
			if dupOf != nil {
				duplicates++
				if state != string(domain.RequestStatusDuplicate) {
					t.Fatalf("round %d: request %d has duplicate_of=%d but state %q", round, id, *dupOf, state)
				}
				var canonState string
				var canonDup *int
				if err := f.r.db.QueryRow(f.ctx, `SELECT state, duplicate_of FROM team_requests WHERE id = $1`, *dupOf).
					Scan(&canonState, &canonDup); err != nil {
					t.Fatalf("read canonical %d: %v", *dupOf, err)
				}
				if canonDup != nil {
					t.Fatalf("round %d: request %d points at %d, which is itself a duplicate: the chain is %d hops",
						round, id, *dupOf, 2)
				}
				if canonState == string(domain.RequestStatusDuplicate) {
					t.Fatalf("round %d: canonical %d is in the duplicate state", round, *dupOf)
				}
			} else {
				canonicals++
			}
		}
		if duplicates != 1 || canonicals != 1 {
			t.Fatalf("round %d: %d duplicates and %d canonicals, want one of each", round, duplicates, canonicals)
		}

		// The link rows agree with the typed column: one duplicate_of row, pointing at
		// the canonical.
		var linkCount int
		if err := f.r.db.QueryRow(f.ctx, `SELECT count(*) FROM request_links
			WHERE (request_id = $1 OR request_id = $2) AND link_kind = $3`,
			a.ID, b.ID, string(domain.RequestLinkDuplicateOf)).Scan(&linkCount); err != nil {
			t.Fatalf("count duplicate links: %v", err)
		}
		if linkCount != 1 {
			t.Fatalf("round %d: %d duplicate_of link rows, want 1", round, linkCount)
		}
	}
}

// TestMarkDuplicateFlattensAndRefusesBadPairs covers the fold rules that do not
// need a race: canonical flattening, self, an already-folded request, a request
// that has duplicates of its own, and the cross-tenant refusals (another org, and
// another team in the same org — the package answers both as not-found).
func TestMarkDuplicateFlattensAndRefusesBadPairs(t *testing.T) {
	f := newRequestLinksFixture(t)
	fold := func(req *domain.TeamRequest, canonical int) (*domain.TeamRequest, bool, error) {
		t.Helper()
		w := f.dupWrite(req, canonical)
		w.CreatedBy = &f.user
		w.Actor = "triager"
		w.ActorUserID = &f.user
		return f.r.MarkDuplicate(f.ctx, w)
	}

	canonical := f.newRequest("linkflatcanon00", domain.RequestStatusTriaging)
	first := f.newRequest("linkflatfirst00", domain.RequestStatusTriaging)
	second := f.newRequest("linkflatsecond0", domain.RequestStatusTriaging)
	nested := f.newRequest("linkflatnested0", domain.RequestStatusTriaging)

	// First fold: first -> canonical.
	if got, changed, err := fold(first, canonical.ID); err != nil || !changed {
		t.Fatalf("fold first into canonical: changed=%v err=%v", changed, err)
	} else if got.State != domain.RequestStatusDuplicate || got.DuplicateOf == nil || *got.DuplicateOf != canonical.ID {
		t.Fatalf("folded request = state %q duplicate_of %v, want duplicate/%d", got.State, got.DuplicateOf, canonical.ID)
	}

	// Replay: the same fold answers changed=false and moves nothing.
	before := f.mustGet(first)
	if _, changed, err := fold(first, canonical.ID); err != nil || changed {
		t.Fatalf("replayed fold: changed=%v err=%v, want a no-op", changed, err)
	}
	after := f.mustGet(first)
	if after.Rev != before.Rev {
		t.Fatalf("a replayed fold moved rev %d -> %d", before.Rev, after.Rev)
	}

	// Flattening: naming the duplicate as the canonical lands on its root, so no
	// chain of two hops can be created by a fold.
	if _, changed, err := fold(nested, first.ID); err != nil || !changed {
		t.Fatalf("fold nested into the duplicate: changed=%v err=%v", changed, err)
	}
	if got := f.mustGet(nested); got.DuplicateOf == nil || *got.DuplicateOf != canonical.ID {
		t.Fatalf("nested was folded into %v, want the canonical %d (flattening)", got.DuplicateOf, canonical.ID)
	}
	if got := f.mustGet(first); got.DuplicateOf == nil || *got.DuplicateOf != canonical.ID {
		t.Fatalf("first now points at %v, want the canonical %d: a second hop is a chain", got.DuplicateOf, canonical.ID)
	}

	// A request that has duplicates of its own may not become a duplicate.
	if _, _, err := fold(canonical, second.ID); !isDuplicateRefusal(err, duplicateRefusedHasDuplicates) {
		t.Fatalf("folding a canonical = %v, want a has_duplicates refusal", err)
	}
	// Self.
	if _, _, err := fold(second, second.ID); !isDuplicateRefusal(err, duplicateRefusedSelf) {
		t.Fatalf("self fold = %v, want a self refusal", err)
	}
	// Already folded into a different canonical.
	if _, _, err := fold(first, second.ID); !isDuplicateRefusal(err, duplicateRefusedFolded) {
		t.Fatalf("folding an already-folded request = %v, want an already_folded refusal", err)
	}
	// Cross-tenant: a canonical in another org, and one in another team of the same
	// org, are both the one not-found — a wrong tenant is never distinguishable from
	// a missing id, and neither is a fold.
	otherOrgCanonical := f.otherOrgRequest("linkflatother00")
	if _, _, err := fold(second, otherOrgCanonical.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("fold into another org's request = %v, want ErrNotFound", err)
	}
	otherTeamCanonical := f.otherTeamRequest("linkflatothertm0")
	if _, _, err := fold(second, otherTeamCanonical.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("fold into another team's request = %v, want ErrNotFound", err)
	}
	// The same refusal from the other side: a caller scoped to this team naming a
	// request in the other team has named nothing. (The write is addressed by request
	// id plus the caller's org AND team, so the sibling-team row is simply not there.)
	if _, _, err := fold(otherTeamCanonical, canonical.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("folding a request from another team = %v, want ErrNotFound", err)
	}
	// The forged-scope case the fold's lock set has to refuse on its own: the caller
	// claims this team and names a request that lives in the sibling team. Every read
	// the fold makes is scoped by the caller's tenant, so the row cannot be locked,
	// written or returned — a fold scoped by org alone would write another team's
	// request with this team's authorization.
	forged := f.dupWrite(second, canonical.ID)
	forged.RequestID = otherTeamCanonical.ID // a request in team2...
	forged.TeamID = f.team                   // ...addressed with team1's tenant
	forged.Actor = "triager"
	if _, _, err := f.r.MarkDuplicate(f.ctx, forged); !errors.Is(err, ErrNotFound) {
		t.Fatalf("fold of another team's request under this team's scope = %v, want ErrNotFound", err)
	}
	if _, err := f.r.GetRequest(f.ctx, otherTeamCanonical.ID, f.org, f.team2); err != nil {
		t.Fatalf("GetRequest(sibling-team request): %v", err)
	} else if otherTeamCanonical.DuplicateOf != nil {
		t.Fatal("the sibling team's request was folded by a write scoped to another team")
	}
	// And the same forged scope cannot be used to detach a sibling team's duplicate.
	if _, _, err := f.r.DetachDuplicate(f.ctx, RequestDuplicateWrite{
		RequestID: otherTeamCanonical.ID, OrgID: f.org, TeamID: f.team, Actor: "triager",
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("detach of another team's request under this team's scope = %v, want ErrNotFound", err)
	}

	// None of the refusals wrote anything: `second` is still a plain canonical, and
	// the canonical still has exactly the two duplicates it started with.
	if got := f.mustGet(second); got.DuplicateOf != nil || got.State == domain.RequestStatusDuplicate {
		t.Fatalf("a refused fold moved request %d: %+v", second.ID, got)
	}
	if got, err := f.r.GetRequest(f.ctx, otherOrgCanonical.ID, f.other, f.teamOther); err != nil {
		t.Fatalf("GetRequest(cross-org canonical): %v", err)
	} else if got.DuplicateOf != nil || got.State == domain.RequestStatusDuplicate {
		t.Fatalf("the other org's request was moved by a refused fold: %+v", got)
	}
	var children int
	if err := f.r.db.QueryRow(f.ctx, `SELECT count(*) FROM team_requests WHERE duplicate_of = $1`, canonical.ID).
		Scan(&children); err != nil {
		t.Fatalf("count duplicates: %v", err)
	}
	if children != 2 {
		t.Fatalf("canonical has %d duplicates, want 2", children)
	}
}

// TestDetachDuplicateIsAtomicAndIdempotent is criterion 3's second half: the fold
// is undone in one statement — pointer, state, rev, reopen counters, the link row
// and one transition event — the fold itself stays in the append-only history, and
// a second detach is a no-op.
func TestDetachDuplicateIsAtomicAndIdempotent(t *testing.T) {
	f := newRequestLinksFixture(t)
	canonical := f.newRequest("linkdetachcan00", domain.RequestStatusTriaging)
	folded := f.newRequest("linkdetachreq00", domain.RequestStatusTriaging)

	w := f.dupWrite(folded, canonical.ID)
	w.CreatedBy = &f.user
	w.Actor = "triager"
	w.ActorUserID = &f.user
	if _, changed, err := f.r.MarkDuplicate(f.ctx, w); err != nil || !changed {
		t.Fatalf("fold: changed=%v err=%v", changed, err)
	}
	atFold := f.mustGet(folded)
	if atFold.ClosedAt == nil {
		t.Fatal("the fold did not stamp closed_at")
	}
	eventsAtFold := len(f.events(folded.ID))

	// A stale rev is refused before anything moves.
	stale := f.dupWrite(folded, canonical.ID)
	stale.ReopenTo = domain.RequestStatusTriaging
	stale.ExpectRev = atFold.Rev + 10
	stale.Actor = "triager"
	if _, _, err := f.r.DetachDuplicate(f.ctx, stale); !isRevConflict(err, atFold.Rev) {
		t.Fatalf("detach with a stale rev = %v, want a conflict carrying rev %d", err, atFold.Rev)
	}
	if got := f.mustGet(folded); got.DuplicateOf == nil || got.Rev != atFold.Rev {
		t.Fatalf("a refused detach moved the request: duplicate_of=%v rev=%d", got.DuplicateOf, got.Rev)
	}

	detach := f.dupWrite(folded, canonical.ID)
	detach.ReopenTo = domain.RequestStatusNeedsInformation
	detach.ExpectRev = atFold.Rev
	detach.Actor = "triager"
	detach.ActorUserID = &f.user
	got, changed, err := f.r.DetachDuplicate(f.ctx, detach)
	if err != nil || !changed {
		t.Fatalf("detach: changed=%v err=%v", changed, err)
	}
	if got.DuplicateOf != nil {
		t.Fatalf("detach left duplicate_of = %v", got.DuplicateOf)
	}
	if got.State != domain.RequestStatusNeedsInformation {
		t.Fatalf("detach reopened to %q, want needs_information", got.State)
	}
	if got.Rev != atFold.Rev+1 {
		t.Fatalf("detach moved rev to %d, want exactly %d", got.Rev, atFold.Rev+1)
	}
	if got.ReopenedCount != 1 || got.ReopenedAt == nil {
		t.Fatalf("detach recorded reopened_count=%d reopened_at=%v, want 1 and a timestamp", got.ReopenedCount, got.ReopenedAt)
	}
	// 053's CHECK requires needs_info_since while in needs_information.
	if got.NeedsInfoSince == nil {
		t.Fatal("detach left the request in needs_information with no needs_info_since")
	}

	// The one link row that claimed the fold is gone, and the fold is still readable
	// in history: the detach is a transition from duplicate to needs_information.
	if rows := f.linkRows(folded.ID); len(rows) != 0 {
		t.Fatalf("detach left %d duplicate_of link rows, want 0", len(rows))
	}
	events := f.events(folded.ID)
	if len(events) != eventsAtFold+1 {
		t.Fatalf("detach appended %d history rows, want exactly 1", len(events)-eventsAtFold)
	}
	last := events[len(events)-1]
	if last.Action != domain.RequestActionTransition ||
		last.FromStatus != domain.RequestStatusDuplicate || last.ToStatus != domain.RequestStatusNeedsInformation {
		t.Fatalf("detach's history row = %s %q->%q, want transition duplicate->needs_information",
			last.Action, last.FromStatus, last.ToStatus)
	}
	if !strings.Contains(last.Detail, fmt.Sprintf(`"canonical_request_id":%d`, canonical.ID)) {
		t.Fatalf("detach detail = %q, want it to name the canonical %d", last.Detail, canonical.ID)
	}
	// The canonical never moved, and it no longer has a duplicate.
	if c := f.mustGet(canonical); c.DuplicateOf != nil || c.State == domain.RequestStatusDuplicate {
		t.Fatalf("the canonical moved: %+v", c)
	}
	var stillFolded int
	if err := f.r.db.QueryRow(f.ctx, `SELECT count(*) FROM team_requests WHERE duplicate_of = $1`, canonical.ID).
		Scan(&stillFolded); err != nil {
		t.Fatalf("count duplicates: %v", err)
	}
	if stillFolded != 0 {
		t.Fatalf("the canonical still has %d duplicates", stillFolded)
	}

	// Idempotent in the way a release is: a second detach answers changed=false,
	// leaves rev alone and appends no second event.
	before := f.mustGet(folded)
	detachAgain := detach
	detachAgain.ExpectRev = before.Rev
	if _, changed, err := f.r.DetachDuplicate(f.ctx, detachAgain); err != nil || changed {
		t.Fatalf("replayed detach: changed=%v err=%v, want a no-op", changed, err)
	}
	after := f.mustGet(folded)
	if after.Rev != before.Rev || after.ReopenedCount != 1 {
		t.Fatalf("a replayed detach moved rev %d->%d or reopened_count to %d", before.Rev, after.Rev, after.ReopenedCount)
	}
	if n := len(f.events(folded.ID)); n != len(events) {
		t.Fatalf("a replayed detach appended %d history rows", n-len(events))
	}

	// Detaching something that was never a duplicate is already detached.
	plain := f.newRequest("linkdetachplain0", domain.RequestStatusTriaging)
	if _, changed, err := f.r.DetachDuplicate(f.ctx, f.dupWrite(plain, 0)); err != nil || changed {
		t.Fatalf("detach of a non-duplicate: changed=%v err=%v, want a no-op", changed, err)
	}

	// A reopen destination that is a lifecycle move rather than a reopen is refused,
	// so this helper cannot quietly become a second state machine.
	again := f.newRequest("linkdetachdest00", domain.RequestStatusTriaging)
	w2 := f.dupWrite(again, canonical.ID)
	w2.Actor = "triager"
	if _, changed, err := f.r.MarkDuplicate(f.ctx, w2); err != nil || !changed {
		t.Fatalf("second fold: changed=%v err=%v", changed, err)
	}
	bad := f.dupWrite(again, canonical.ID)
	bad.ReopenTo = domain.RequestStatusDelivered
	bad.Actor = "triager"
	if _, _, err := f.r.DetachDuplicate(f.ctx, bad); err == nil {
		t.Fatal("detaching to delivered was accepted; a reopen is triaging or needs_information (D6)")
	}
	if got := f.mustGet(again); got.DuplicateOf == nil {
		t.Fatal("the refused detach cleared duplicate_of anyway")
	}
}

// TestMarkDuplicateDoesNotStrandAChildFoldedInConcurrently forces the one
// interleaving the advisory read cannot see: a child is folded into a request by a
// transaction that has not committed yet, and that same request is folded into a
// canonical at the same time.
//
// The write must not land while the child still points at it, or the child sits two
// hops from the canonical — the chain the flattening rule exists to prevent, and one
// a later detach of the middle request would orphan. The fold therefore has to
// notice the child under the lock (re-reading is what does) and refuse, rather than
// trust the read it made before the lock.
func TestMarkDuplicateDoesNotStrandAChildFoldedInConcurrently(t *testing.T) {
	f := newRequestLinksFixture(t)
	canonical := f.newRequest("linkstrandcanon0", domain.RequestStatusTriaging)
	middle := f.newRequest("linkstrandmiddle", domain.RequestStatusTriaging)
	child := f.newRequest("linkstrandchild0", domain.RequestStatusTriaging)

	// A triager's fold of child -> middle, still open: it has written the child and
	// taken middle's row lock, and nobody else can see either yet.
	tx, err := f.r.db.Begin(f.ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(f.ctx, `UPDATE team_requests
		SET state = $2, duplicate_of = $3, closed_at = NOW(), rev = rev + 1
		WHERE id = $1`, child.ID, string(domain.RequestStatusDuplicate), middle.ID); err != nil {
		tx.Rollback(f.ctx)
		t.Fatalf("fold the child in an open transaction: %v", err)
	}
	var held int
	if err := tx.QueryRow(f.ctx, `SELECT id FROM team_requests WHERE id = $1 FOR UPDATE`, middle.ID).Scan(&held); err != nil {
		tx.Rollback(f.ctx)
		t.Fatalf("hold the middle request's row: %v", err)
	}

	// The competing fold of middle -> canonical, which must block on that row.
	type foldResult struct {
		changed bool
		err     error
	}
	done := make(chan foldResult, 1)
	go func() {
		w := f.dupWrite(middle, canonical.ID)
		w.CreatedBy = &f.user
		w.Actor = "other triager"
		w.ActorUserID = &f.user
		_, changed, err := f.r.MarkDuplicate(f.ctx, w)
		done <- foldResult{changed: changed, err: err}
	}()

	// If it does not block, the row lock is not doing its job and the rest of this
	// test would prove nothing; fail rather than race on.
	select {
	case res := <-done:
		tx.Rollback(f.ctx)
		t.Fatalf("the fold of the middle request did not block on the row the other triager holds: %+v", res)
	case <-time.After(300 * time.Millisecond):
	}

	// Commit the child fold. The blocked fold now re-reads the rows it locked.
	if err := tx.Commit(f.ctx); err != nil {
		t.Fatalf("commit the child fold: %v", err)
	}
	res := <-done

	// The outcome, from the stored rows: middle is either still a canonical with a
	// child (the fold was refused) or a duplicate whose child moved with it. What it
	// must never be is a duplicate with a child still pointing at it.
	var middleDup *int
	var middleState string
	if err := f.r.db.QueryRow(f.ctx, `SELECT duplicate_of, state FROM team_requests WHERE id = $1`, middle.ID).
		Scan(&middleDup, &middleState); err != nil {
		t.Fatalf("read the middle request: %v", err)
	}
	var childDup *int
	if err := f.r.db.QueryRow(f.ctx, `SELECT duplicate_of FROM team_requests WHERE id = $1`, child.ID).
		Scan(&childDup); err != nil {
		t.Fatalf("read the child request: %v", err)
	}
	if middleDup != nil && childDup != nil && *childDup == middle.ID {
		t.Fatalf("the child points at request %d, which is itself a duplicate of %d: a two-hop chain",
			middle.ID, *middleDup)
	}
	// With the child committed first, the fold has to be refused: middle has a
	// duplicate of its own and cannot become one.
	if !isDuplicateRefusal(res.err, duplicateRefusedHasDuplicates) {
		t.Fatalf("fold of a request that gained a duplicate = %v (changed=%v, middle now %q), "+
			"want a has_duplicates refusal: the read made before the lock cannot see a fold that "+
			"commits while the lock is being taken", res.err, res.changed, middleState)
	}
	if res.changed {
		t.Fatal("a refused fold reported changed=true")
	}
	if rows := f.linkRows(middle.ID); len(rows) != 0 {
		t.Fatalf("the refused fold wrote %d duplicate_of link rows", len(rows))
	}
}

// TestMarkDuplicateRefusesAChildInAnotherTeam pins the one shape 053 permits that a
// team-scoped write cannot complete: the composite key that keeps a duplicate in its
// canonical's ORG is (duplicate_of, org_id), so a child in a sibling team of the same
// org is a legal relation. Folding the middle request would strand that child two hops
// from the canonical, and this tenant cannot lock or move the child to flatten it, so
// the fold is refused and nothing moves.
func TestMarkDuplicateRefusesAChildInAnotherTeam(t *testing.T) {
	f := newRequestLinksFixture(t)
	canonical := f.newRequest("linkcrosschildc0", domain.RequestStatusTriaging)
	middle := f.newRequest("linkcrosschildm0", domain.RequestStatusTriaging)
	// The child lives in the sibling team (same org, different tenant).
	child := f.otherTeamRequest("linkcrosschildx0")
	if _, err := f.r.db.Exec(f.ctx, `UPDATE team_requests
		SET state = $2, duplicate_of = $3, closed_at = NOW(), rev = rev + 1
		WHERE id = $1`, child.ID, string(domain.RequestStatusDuplicate), middle.ID); err != nil {
		t.Fatalf("fold the sibling team's request into the middle request: %v", err)
	}
	if _, err := f.r.db.Exec(f.ctx, `INSERT INTO request_links
		(request_id, org_id, link_kind, target_kind, target_id) VALUES ($1, $2, $3, $4, $5)`,
		child.ID, f.org, string(domain.RequestLinkDuplicateOf), string(domain.RequestTargetRequest), middle.ID); err != nil {
		t.Fatalf("store the sibling team's duplicate link: %v", err)
	}

	w := f.dupWrite(middle, canonical.ID)
	w.CreatedBy = &f.user
	w.Actor = "triager"
	w.ActorUserID = &f.user
	if _, changed, err := f.r.MarkDuplicate(f.ctx, w); !isDuplicateRefusal(err, duplicateRefusedChildElsewhere) {
		t.Fatalf("fold with a child in a sibling team = %v (changed=%v), want a %q refusal",
			err, changed, duplicateRefusedChildElsewhere)
	}
	// Nothing moved: the middle request is still a canonical, and the child still
	// points at it (a one-hop chain, which is what it was).
	if got := f.mustGet(middle); got.DuplicateOf != nil || got.State == domain.RequestStatusDuplicate {
		t.Fatalf("the refused fold moved the middle request: %+v", got)
	}
	var childDup *int
	if err := f.r.db.QueryRow(f.ctx, `SELECT duplicate_of FROM team_requests WHERE id = $1`, child.ID).Scan(&childDup); err != nil {
		t.Fatalf("read the child: %v", err)
	}
	if childDup == nil || *childDup != middle.ID {
		t.Fatalf("the child now points at %v, want %d: a refused fold must move nothing", childDup, middle.ID)
	}
	if rows := f.linkRows(middle.ID); len(rows) != 0 {
		t.Fatalf("the refused fold wrote %d link rows", len(rows))
	}
}

// TestMarkDuplicateConcurrentManyIntoOneCanonical is the other direction of the
// race: several requests folding into one canonical at once all land, each one hop
// from it, and the canonical gains duplicates but never a second identity.
func TestMarkDuplicateConcurrentManyIntoOneCanonical(t *testing.T) {
	f := newRequestLinksFixture(t)
	canonical := f.newRequest("linkmanycanon000", domain.RequestStatusTriaging)

	const n = 6
	requests := make([]*domain.TeamRequest, n)
	for i := range requests {
		requests[i] = f.newRequest(fmt.Sprintf("linkmany%06d", i), domain.RequestStatusTriaging)
	}

	start := make(chan struct{})
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i, req := range requests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := f.dupWrite(req, canonical.ID)
			w.CreatedBy = &f.user
			w.Actor = fmt.Sprintf("triager-%d", i)
			w.ActorUserID = &f.user
			<-start
			_, changed, err := f.r.MarkDuplicate(f.ctx, w)
			if err == nil && !changed {
				err = fmt.Errorf("fold of request %d answered changed=false", req.ID)
			}
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent fold into one canonical: %v", err)
		}
	}

	var duplicates int
	if err := f.r.db.QueryRow(f.ctx, `SELECT count(*) FROM team_requests
		WHERE duplicate_of = $1 AND state = $2`, canonical.ID, string(domain.RequestStatusDuplicate)).
		Scan(&duplicates); err != nil {
		t.Fatalf("count duplicates: %v", err)
	}
	if duplicates != n {
		t.Fatalf("%d requests are duplicates of the canonical, want %d", duplicates, n)
	}
	var links int
	if err := f.r.db.QueryRow(f.ctx, `SELECT count(*) FROM request_links
		WHERE link_kind = $1 AND target_id = $2`, string(domain.RequestLinkDuplicateOf), canonical.ID).
		Scan(&links); err != nil {
		t.Fatalf("count duplicate links: %v", err)
	}
	if links != n {
		t.Fatalf("%d duplicate_of link rows, want %d", links, n)
	}
	// Every duplicate sits one hop from a canonical that is not itself a duplicate.
	var maxHops int
	if err := f.r.db.QueryRow(f.ctx, `SELECT count(*) FROM team_requests d
		JOIN team_requests c ON c.id = d.duplicate_of
		WHERE d.duplicate_of = $1 AND c.duplicate_of IS NOT NULL`, canonical.ID).Scan(&maxHops); err != nil {
		t.Fatalf("count two-hop chains: %v", err)
	}
	if maxHops != 0 {
		t.Fatalf("%d duplicates point at a canonical that is itself a duplicate", maxHops)
	}
}

// isDuplicateRefusal reports whether err is a fold refusal for reason.
func isDuplicateRefusal(err error, reason string) bool {
	var refused *RequestDuplicateRefused
	if !errors.As(err, &refused) {
		return false
	}
	return refused.Reason == reason
}

// isRevConflict reports whether err is a stale-rev conflict carrying current.
func isRevConflict(err error, current int) bool {
	var conflict *RequestRevConflict
	if !errors.As(err, &conflict) {
		return false
	}
	return conflict.Current == current
}
