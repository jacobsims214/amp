package service

// Tests for request_outcome.go, the outcome aggregate and the delivery gate
// (AMP #3966, epic 202 story 549; build slice E2). They own three things:
//
//  1. Honesty. A held receipt, a merge gate, a deleted target and a partly
//     delivered request each read as what they are — never as "delivered" and
//     never as silence. The cases are a table over the pure BuildRequestOutcome,
//     because that is where the meaning lives: no database, no clock, no
//     principal, so a case is one literal and one expectation.
//  2. The delivery gate. The evidence and brief a delivery must carry are
//     validated for real (DB-backed, through Service.DeliverRequest), and a
//     delivery that the aggregate cannot see as eligible is refused with the
//     contract's words — including a link to a target that has since been
//     deleted, and a brief with no evidence at all.
//  3. The requester's whitelist. The milestones the aggregate produces carry the
//     closed five-step vocabulary, only counts-and-words notes, and never a
//     ticket, project, epic, name or id. A delivery the request never made stays
//     undelivered however much linked work is done (I8), and the projection's own
//     gate (domain.RequesterMilestoneDoneAllowed) is asked, not re-implemented.
//
// The DB-backed cases run against a real Postgres with every migration applied,
// when TEST_DATABASE_URL is set (the same disposable fixture the lifecycle and
// link suites use, and the same tenant-isolation rules):
//
//	TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:PORT/amp_test?sslmode=disable \
//	go test ./internal/service -run 'RequestOutcome|DeliverRequest|RequesterMilestones' -count=1 -race

import (
	"strings"
	"testing"
	"time"

	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/repository"
)

// ---- 1. The pure aggregate: honest cases ----

// outcomeRequest is the stored row an aggregate case builds on: a delivered
// request with its brief set, so each case can take away exactly the one thing it
// is about.
func outcomeRequest() *domain.TeamRequest {
	created := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	delivered := created.Add(4 * time.Hour)
	return &domain.TeamRequest{
		ID: 7, PublicRef: "REQ-0123456789abcdef", OrgID: 1, TeamID: 2,
		Subject: "Please add a CSV export", InputBrief: "Weekly review.",
		OutcomeBrief: "The export is live; the scheduled variant is not included.",
		State:        domain.RequestStatusDelivered,
		DeliveredAt:  &delivered,
		CreatedAt:    created,
	}
}

func deliveredLink(kind domain.RequestTargetKind, id int) domain.RequestLink {
	return domain.RequestLink{LinkKind: domain.RequestLinkDeliveredBy, TargetKind: kind, TargetID: id}
}

func doneTask(id int) repository.RequestOutcomeLinkTarget {
	return repository.RequestOutcomeLinkTarget{Kind: domain.RequestTargetTask, ID: id, TaskState: domain.TaskStateCompleted}
}

func openTask(id int) repository.RequestOutcomeLinkTarget {
	return repository.RequestOutcomeLinkTarget{Kind: domain.RequestTargetTask, ID: id, TaskState: domain.TaskStateInProgress, TaskStarted: true}
}

func questionTask(id int) repository.RequestOutcomeLinkTarget {
	return repository.RequestOutcomeLinkTarget{
		Kind: domain.RequestTargetTask, ID: id, TaskState: domain.TaskStateBlocked,
		TaskBlocked: "Which timezone should the report use?",
	}
}

func gatedTask(id int) repository.RequestOutcomeLinkTarget {
	return repository.RequestOutcomeLinkTarget{
		Kind: domain.RequestTargetTask, ID: id, TaskState: domain.TaskStateCompleted,
		MergeState: domain.MergeNeeded, MergeGated: true,
	}
}

func heldReceipt(id int) repository.RequestOutcomeLinkTarget {
	return repository.RequestOutcomeLinkTarget{
		Kind: domain.RequestTargetReceipt, ID: id, ReceiptState: domain.ReceiptFinal, ReceiptHeld: true,
	}
}

func approvedArtifact(id int) repository.RequestOutcomeLinkTarget {
	return repository.RequestOutcomeLinkTarget{
		Kind: domain.RequestTargetArtifact, ID: id, ArtifactStatus: domain.ArtifactApproved, ArtifactApproved: true,
	}
}

func inReviewArtifact(id int) repository.RequestOutcomeLinkTarget {
	return repository.RequestOutcomeLinkTarget{
		Kind: domain.RequestTargetArtifact, ID: id, ArtifactStatus: domain.ArtifactInReview,
	}
}

func deletedTask(id int) repository.RequestOutcomeLinkTarget {
	at := "2026-11-11 10:00:00+00"
	return repository.RequestOutcomeLinkTarget{Kind: domain.RequestTargetTask, ID: id, Deleted: true, DeletedAt: &at}
}

// TestBuildRequestOutcomeCases is the honest-case table: the five situations the
// ticket names (all done, one gated, one held, deleted, closeout-and-abandoned)
// plus the ones that are easy to get wrong (a question, an artifact in review, an
// unlinked request, a request closed while its work is done).
func TestBuildRequestOutcomeCases(t *testing.T) {
	for _, c := range []struct {
		name    string
		request *domain.TeamRequest
		links   []domain.RequestLink
		targets []repository.RequestOutcomeLinkTarget
		runs    []repository.RequestOutcomeRunFacts
		noArt   bool

		wantPhase    RequestPhase
		wantEligible bool
		wantCounts   map[RequestOutcomeItemState]int
		wantRework   int
		wantItems    int
	}{
		{
			name:      "all done and not gated: the work is finished, the delivery is the request's to make",
			request:   outcomeRequest(),
			links:     []domain.RequestLink{deliveredLink(domain.RequestTargetTask, 11), deliveredLink(domain.RequestTargetTask, 12)},
			targets:   []repository.RequestOutcomeLinkTarget{doneTask(11), doneTask(12)},
			wantPhase: RequestPhaseDelivered, wantEligible: true,
			wantCounts: map[RequestOutcomeItemState]int{RequestOutcomeDone: 2}, wantItems: 2,
		},
		{
			name:       "one gated: a merge that has not happened is not delivery",
			request:    outcomeRequest(),
			links:      []domain.RequestLink{deliveredLink(domain.RequestTargetTask, 11), deliveredLink(domain.RequestTargetTask, 12)},
			targets:    []repository.RequestOutcomeLinkTarget{doneTask(11), gatedTask(12)},
			wantPhase:  RequestPhaseDelivered, // the REQUEST is delivered; the item is not
			wantCounts: map[RequestOutcomeItemState]int{RequestOutcomeDone: 1, RequestOutcomeMergeGated: 1}, wantItems: 2,
			wantEligible: true,
		},
		{
			name:       "one gated and the request still in progress: blocked, and not deliverable",
			request:    inProgress(outcomeRequest()),
			links:      []domain.RequestLink{deliveredLink(domain.RequestTargetTask, 11), deliveredLink(domain.RequestTargetTask, 12)},
			targets:    []repository.RequestOutcomeLinkTarget{doneTask(11), gatedTask(12)},
			wantPhase:  RequestPhaseBlocked,
			wantCounts: map[RequestOutcomeItemState]int{RequestOutcomeDone: 1, RequestOutcomeMergeGated: 1}, wantItems: 2,
		},
		{
			name:       "abandoned merge gate is still a gate (a person must say it is not needed)",
			request:    inProgress(outcomeRequest()),
			links:      []domain.RequestLink{deliveredLink(domain.RequestTargetTask, 11)},
			targets:    []repository.RequestOutcomeLinkTarget{{Kind: domain.RequestTargetTask, ID: 11, TaskState: domain.TaskStateCompleted, MergeState: domain.MergeAbandoned, MergeGated: true}},
			wantPhase:  RequestPhaseBlocked,
			wantCounts: map[RequestOutcomeItemState]int{RequestOutcomeMergeGated: 1}, wantItems: 1,
		},
		{
			name:       "one held receipt: pending verify, never delivered",
			request:    inProgress(outcomeRequest()),
			links:      []domain.RequestLink{deliveredLink(domain.RequestTargetReceipt, 21)},
			targets:    []repository.RequestOutcomeLinkTarget{heldReceipt(21)},
			wantPhase:  RequestPhaseBlocked,
			wantCounts: map[RequestOutcomeItemState]int{RequestOutcomeHeld: 1}, wantItems: 1,
		},
		{
			name:       "a deleted linked task is a tombstone, not silence and not done",
			request:    inProgress(outcomeRequest()),
			links:      []domain.RequestLink{deliveredLink(domain.RequestTargetTask, 31), deliveredLink(domain.RequestTargetTask, 32)},
			targets:    []repository.RequestOutcomeLinkTarget{deletedTask(31), doneTask(32)},
			wantPhase:  RequestPhaseInProgress,
			wantCounts: map[RequestOutcomeItemState]int{RequestOutcomeDeleted: 1, RequestOutcomeDone: 1}, wantItems: 2,
		},
		{
			name:      "a link whose target vanished entirely is still a tombstone",
			request:   inProgress(outcomeRequest()),
			links:     []domain.RequestLink{deliveredLink(domain.RequestTargetTask, 33)},
			wantPhase: RequestPhaseInProgress,
			// The service's read would mark it Deleted; with no target given at
			// all the aggregate must not count it as done either.
			wantCounts: map[RequestOutcomeItemState]int{RequestOutcomeDeleted: 1}, wantItems: 1,
		},
		{
			name:       "closeouts and abandoned runs are counted as rework",
			request:    inProgress(outcomeRequest()),
			links:      []domain.RequestLink{deliveredLink(domain.RequestTargetTask, 41)},
			targets:    []repository.RequestOutcomeLinkTarget{doneTask(41)},
			runs:       []repository.RequestOutcomeRunFacts{{TaskID: 41, Runs: 3, Abandoned: 2}},
			wantPhase:  RequestPhaseInProgress,
			wantCounts: map[RequestOutcomeItemState]int{RequestOutcomeDone: 1}, wantItems: 1, wantRework: 2,
		},
		{
			name:       "a question on a ticket puts a person in the way",
			request:    inProgress(outcomeRequest()),
			links:      []domain.RequestLink{deliveredLink(domain.RequestTargetTask, 51), deliveredLink(domain.RequestTargetTask, 52)},
			targets:    []repository.RequestOutcomeLinkTarget{questionTask(51), openTask(52)},
			wantPhase:  RequestPhaseBlocked,
			wantCounts: map[RequestOutcomeItemState]int{RequestOutcomeBlocked: 1, RequestOutcomeOpen: 1}, wantItems: 2,
		},
		{
			name:       "a scheduled block is a schedule, not a question",
			request:    inProgress(outcomeRequest()),
			links:      []domain.RequestLink{deliveredLink(domain.RequestTargetTask, 53)},
			targets:    []repository.RequestOutcomeLinkTarget{{Kind: domain.RequestTargetTask, ID: 53, TaskState: domain.TaskStateBlocked, TaskBlocked: "scheduled:2026-11-13T09:00:00Z"}},
			wantPhase:  RequestPhaseNotStarted,
			wantCounts: map[RequestOutcomeItemState]int{RequestOutcomeOpen: 1}, wantItems: 1,
		},
		{
			name:       "an artifact in review exists but is not agreed to",
			request:    inProgress(outcomeRequest()),
			links:      []domain.RequestLink{deliveredLink(domain.RequestTargetArtifact, 61)},
			targets:    []repository.RequestOutcomeLinkTarget{inReviewArtifact(61)},
			wantPhase:  RequestPhaseInProgress,
			wantCounts: map[RequestOutcomeItemState]int{RequestOutcomeUnverified: 1}, wantItems: 1,
		},
		{
			name:       "an approved artifact is done",
			request:    inProgress(outcomeRequest()),
			links:      []domain.RequestLink{deliveredLink(domain.RequestTargetArtifact, 62)},
			targets:    []repository.RequestOutcomeLinkTarget{approvedArtifact(62)},
			wantPhase:  RequestPhaseInProgress,
			wantCounts: map[RequestOutcomeItemState]int{RequestOutcomeDone: 1}, wantItems: 1,
		},
		{
			name:      "an unlinked request is unlinked, not empty and not done",
			request:   inProgress(outcomeRequest()),
			wantPhase: RequestPhaseUnlinked, wantItems: 0,
		},
		{
			name:      "planned but not yet delivered-by: not started",
			request:   inProgress(outcomeRequest()),
			links:     []domain.RequestLink{{LinkKind: domain.RequestLinkPlannedAs, TargetKind: domain.RequestTargetTask, TargetID: 71}},
			wantPhase: RequestPhaseNotStarted, wantItems: 0,
		},
		{
			name:      "a rejected request with its work done is closed, not delivered",
			request:   rejected(outcomeRequest()),
			links:     []domain.RequestLink{deliveredLink(domain.RequestTargetTask, 81)},
			targets:   []repository.RequestOutcomeLinkTarget{doneTask(81)},
			wantPhase: RequestPhaseClosed,
			wantCounts: map[RequestOutcomeItemState]int{
				RequestOutcomeDone: 1,
			}, wantItems: 1,
		},
		{
			name:    "an explicit no-artifact delivery is eligible with no links",
			request: outcomeRequest(), noArt: true,
			wantPhase: RequestPhaseDelivered, wantEligible: true, wantItems: 0,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			out := BuildRequestOutcome(RequestOutcomeInput{
				Request: c.request, Links: c.links, Targets: c.targets, Runs: c.runs, NoArtifact: c.noArt,
			})
			if out == nil {
				t.Fatal("BuildRequestOutcome returned nil for a request")
			}
			if out.Phase != c.wantPhase {
				t.Errorf("phase = %q, want %q", out.Phase, c.wantPhase)
			}
			if out.DeliveredEligible != c.wantEligible {
				t.Errorf("delivered_eligible = %v, want %v (blocked reason %q)", out.DeliveredEligible, c.wantEligible, out.BlockedReason)
			}
			if out.Total != c.wantItems {
				t.Errorf("total = %d, want %d", out.Total, c.wantItems)
			}
			got := map[RequestOutcomeItemState]int{}
			for _, it := range out.Items {
				got[it.State]++
			}
			for state, want := range c.wantCounts {
				if got[state] != want {
					t.Errorf("%s count = %d, want %d (all: %v)", state, got[state], want, got)
				}
			}
			for state, n := range got {
				if _, ok := c.wantCounts[state]; !ok && n > 0 {
					t.Errorf("unexpected %s count = %d (all: %v)", state, n, got)
				}
			}
			if out.Rework != c.wantRework {
				t.Errorf("rework = %d, want %d", out.Rework, c.wantRework)
			}
			// The counts are derived from the items, so they cannot disagree:
			// assert the invariant rather than just the number.
			if sum := out.Done + out.Open + out.Blocked + out.Gated + out.Held + out.Deleted + out.Unverified; sum != out.Total {
				t.Errorf("item states sum to %d, total is %d: a state is uncounted", sum, out.Total)
			}
			// A blocked reason is offered only while a delivery is still owed.
			if out.DeliveredEligible && out.BlockedReason != "" {
				t.Errorf("an eligible delivery carries a blocked reason %q", out.BlockedReason)
			}
			if !out.DeliveredEligible && out.Phase != RequestPhaseDelivered && out.Phase != RequestPhaseClosed && out.BlockedReason == "" {
				t.Errorf("a delivery that is not yet possible says nothing about why")
			}
		})
	}
}

// inProgress and rejected are the two state moves the table needs without
// copying the whole record.
func inProgress(r *domain.TeamRequest) *domain.TeamRequest {
	c := *r
	c.State, c.DeliveredAt = domain.RequestStatusInProgress, nil
	return &c
}

func rejected(r *domain.TeamRequest) *domain.TeamRequest {
	c := *r
	c.State, c.DeliveredAt, c.OutcomeBrief = domain.RequestStatusRejected, nil, ""
	c.RejectReason = "Not something this desk does."
	return &c
}

// TestBuildRequestOutcomeNeverInfersDeliveryFromDoneWork is I8 as a property:
// however many linked pieces are done, a request that never delivered is not
// delivered. It is the case the whole slice exists for, so it gets its own test
// rather than one row in the table above.
func TestBuildRequestOutcomeNeverInfersDeliveryFromDoneWork(t *testing.T) {
	req := inProgress(outcomeRequest())
	links := []domain.RequestLink{
		deliveredLink(domain.RequestTargetTask, 1), deliveredLink(domain.RequestTargetTask, 2),
		deliveredLink(domain.RequestTargetArtifact, 3), deliveredLink(domain.RequestTargetReceipt, 4),
	}
	targets := []repository.RequestOutcomeLinkTarget{doneTask(1), doneTask(2), approvedArtifact(3),
		{Kind: domain.RequestTargetReceipt, ID: 4, ReceiptState: domain.ReceiptFinal}}

	out := BuildRequestOutcome(RequestOutcomeInput{Request: req, Links: links, Targets: targets})
	if out.Phase == RequestPhaseDelivered {
		t.Fatal("an in-progress request with all linked work done reported itself delivered (I8)")
	}
	if out.DeliveredEligible {
		t.Fatal("an in-progress request was delivery-eligible: completion was inferred from closure (I8)")
	}
	if out.Done != 4 || out.Total != 4 {
		t.Errorf("done/total = %d/%d, want 4/4: the work IS done, and that is what it should report", out.Done, out.Total)
	}
	// And the blocked reason must be about the request's own act, not the work's:
	// everything a delivery needs is present, and the only thing missing is that
	// the team has not stated it.
	if !strings.Contains(out.BlockedReason, "not delivered") {
		t.Errorf("blocked reason %q does not say that the delivery itself is what is missing", out.BlockedReason)
	}
}

// ---- 2. The requester's whitelist ----

// TestRequesterMilestonesAreTheClosedVocabulary is the projection half: the
// aggregate's data reaches the requester only as the five known steps, with counts
// and words, and never as a ticket, project, epic, name or id.
func TestRequesterMilestonesAreTheClosedVocabulary(t *testing.T) {
	req := inProgress(outcomeRequest())
	links := []domain.RequestLink{
		deliveredLink(domain.RequestTargetTask, 4111), deliveredLink(domain.RequestTargetTask, 4112),
		deliveredLink(domain.RequestTargetArtifact, 4113),
	}
	targets := []repository.RequestOutcomeLinkTarget{doneTask(4111), gatedTask(4112), inReviewArtifact(4113)}
	out := BuildRequestOutcome(RequestOutcomeInput{Request: req, Links: links, Targets: targets})

	steps := RequesterMilestonesFor(req, out)
	if len(steps) != len(domain.RequesterMilestoneKeys) {
		t.Fatalf("milestones = %d, want the %d closed steps", len(steps), len(domain.RequesterMilestoneKeys))
	}
	known := map[domain.RequesterMilestoneKey]bool{}
	for _, k := range domain.RequesterMilestoneKeys {
		known[k] = true
	}
	for i, s := range steps {
		if s.Key != domain.RequesterMilestoneKeys[i] {
			t.Errorf("step %d is %q, want %q (the closed vocabulary, in order)", i, s.Key, domain.RequesterMilestoneKeys[i])
		}
		if !known[s.Key] {
			t.Errorf("step %q is not in the closed vocabulary", s.Key)
		}
		// Every note is requester-safe by the projection's own sanitizer, which
		// is the LAST gate; this test checks the aggregate does not hand it
		// something that needed sanitizing.
		if s.Note != "" && s.Note != domain.RequesterNote(s.Note) {
			t.Errorf("step %q note %q is not already requester-safe", s.Key, s.Note)
		}
		for _, leak := range []string{"#", "#4111", "#4112", "#4113", "task", "ticket", "project", "epic", "artifact", "receipt", "branch"} {
			if strings.Contains(strings.ToLower(s.Note), leak) {
				t.Errorf("step %q note %q names something internal (%q)", s.Key, s.Note, leak)
			}
		}
	}
	// Delivered is not claimed: the request is in progress.
	for _, s := range steps {
		if s.Key == domain.RequesterMilestoneDelivered && s.Done {
			t.Fatal("the delivered step is done while the request is in progress: the aggregate announced a delivery nobody made (I8)")
		}
	}
	// The in-progress note is the counts sentence.
	var progress string
	for _, s := range steps {
		if s.Key == domain.RequesterMilestoneInProgress {
			progress = s.Note
		}
	}
	if progress == "" {
		t.Error("no progress note for a request with linked work")
	}
}

// TestRequesterMilestonesGateIsTheProjectionsOwn checks the one decision this
// file must not restate: whether a step may be shown done is
// domain.RequesterMilestoneDoneAllowed's answer, for exactly the same state. A
// step asserted done here and refused there would be a projection the aggregate
// contradicts, and the projection's gate is the one that wins — this pins that
// they cannot disagree.
func TestRequesterMilestonesGateIsTheProjectionsOwn(t *testing.T) {
	for _, st := range domain.RequestStatuses {
		req := *outcomeRequest()
		req.State = st
		if st == domain.RequestStatusDelivered || st == domain.RequestStatusRequesterAccepted {
			at := time.Date(2026, 11, 12, 13, 0, 0, 0, time.UTC)
			req.DeliveredAt = &at
		}
		out := BuildRequestOutcome(RequestOutcomeInput{Request: &req})
		for _, s := range RequesterMilestonesFor(&req, out) {
			if want := domain.RequesterMilestoneDoneAllowed(s.Key, st); s.Done != want {
				t.Errorf("state %s step %q done = %v, want %v (the projection's gate)", st, s.Key, s.Done, want)
			}
		}
	}
}

// TestRequesterMilestonesNoArtifactDeliverySaysSo covers the one note that is
// about the delivery itself: a requester told "delivered" with no file should be
// told there is no file, rather than left to guess one went missing.
func TestRequesterMilestonesNoArtifactDeliverySaysSo(t *testing.T) {
	req := outcomeRequest()
	req.OutcomeBrief = "The recommendation is in the reply; there is no file."
	out := BuildRequestOutcome(RequestOutcomeInput{Request: req, NoArtifact: true})
	steps := RequesterMilestonesFor(req, out)
	var note string
	for _, s := range steps {
		if s.Key == domain.RequesterMilestoneDelivered {
			if !s.Done {
				t.Fatal("a delivered request's delivered step is not done")
			}
			note = s.Note
		}
	}
	if !strings.Contains(note, "no file") {
		t.Errorf("delivered step note = %q, want it to say there is no file", note)
	}
}

// TestBuildRequestOutcomeNilRequest is the shape guard: a nil request has no
// aggregate, rather than an empty one that reads like "nothing is wrong".
func TestBuildRequestOutcomeNilRequest(t *testing.T) {
	if out := BuildRequestOutcome(RequestOutcomeInput{}); out != nil {
		t.Fatalf("BuildRequestOutcome(nil request) = %+v, want nil", out)
	}
	if got := RequesterMilestonesFor(nil, &RequestOutcome{}); got != nil {
		t.Fatalf("RequesterMilestonesFor(nil) = %v, want nil", got)
	}
}

// TestRequesterDetailCarriesTheAggregateNote is the wiring: the requester's detail
// view is built from the aggregate (so its notes and the team rail come from one
// definition), the note is counts-and-words only, and the whitelist is unchanged —
// no field of the linked work reaches the view.
func TestRequesterDetailCarriesTheAggregateNote(t *testing.T) {
	f := newLifecycleFixture(t) // the DB fixture; this case needs the real read
	requester := f.customer
	req := f.newRequestIn(f.team, "detailNote", domain.RequestStatusInProgress, &requester)
	f.attachDelivered(req, f.task)

	p := f.principal(requester)
	row, out, err := f.svc.RequestOutcome(f.ctx, req.ID, f.org, f.team)
	if err != nil {
		t.Fatalf("outcome: %v", err)
	}
	steps := RequesterMilestonesFor(row, out)
	var note string
	for _, s := range steps {
		if s.Key == domain.RequesterMilestoneInProgress {
			note = s.Note
		}
	}
	if note == "" {
		t.Fatal("no progress note although the request has one linked item")
	}
	// And through the real detail read: the note lands, the state word is the
	// projection's, and nothing internal appears in the serialized view.
	view, err := f.svc.projectRequesterRequest(f.ctx, p, req, false)
	if err != nil {
		t.Fatalf("projectRequesterRequest: %v", err)
	}
	found := false
	for _, m := range view.Milestones {
		if m.Key == domain.RequesterMilestoneInProgress && m.Note != "" {
			found = true
			if strings.Contains(m.Note, "#") || strings.Contains(m.Note, "task") {
				t.Errorf("the view's note %q names internal work", m.Note)
			}
		}
	}
	if !found {
		t.Errorf("the detail view's in-progress step carries no aggregate note: %+v", view.Milestones)
	}
}

// ---- 3. Delivery, against a real database ----

// newTask writes one more ticket in the fixture's project, for a case that has to
// delete a linked target without disturbing the shared ones.
func (f *lifecycleFixture) newTask(name string) int {
	f.t.Helper()
	var id int
	if err := f.db.QueryRow(f.ctx, `INSERT INTO tasks (project_id, epic_id, story_id, name)
		VALUES ($1, $2, $3, $4) RETURNING id`, f.project, f.epic, f.story, name).Scan(&id); err != nil {
		f.t.Fatalf("insert task %q: %v", name, err)
	}
	return id
}

// newOutcomeFixture reuses the suite's lifecycle tenant: an inbox team in an org
// with a project whose tickets a delivery can link, the people around it, and a
// requester to deliver to. It is the same fixture the lifecycle and link tests
// own, so the three request suites cannot drift about what a tenant looks like.
func newOutcomeFixture(t *testing.T) *lifecycleFixture {
	t.Helper()
	return newLifecycleFixture(t)
}

// TestDeliverRequestEvidenceAndBriefValidation is criterion 2: a delivery is
// refused without its brief or without evidence, and accepted with either
// evidence or an explicit no-artifact record — through the real write, so the
// gate and the transaction are tested together.
func TestDeliverRequestEvidenceAndBriefValidation(t *testing.T) {
	f := newOutcomeFixture(t)
	requester := f.customer
	lead := f.principal(f.lead)

	// Every case gets its own request, because a delivery changes state and a
	// second attempt on the same row would be a different test.
	deliver := func(t *testing.T, req *domain.TeamRequest, brief string, noArtifact bool) error {
		t.Helper()
		_, err := f.svc.DeliverRequest(f.ctx, lead, DeliverRequestInput{
			RequestID: req.ID, TeamID: req.TeamID, ExpectedRev: req.Rev,
			OutcomeBrief: brief, NoArtifact: noArtifact,
		})
		return err
	}

	t.Run("no brief is refused", func(t *testing.T) {
		req := f.newRequest("noBrief0", domain.RequestStatusInProgress, &requester)
		f.attachDelivered(req, f.task)
		err := deliver(t, req, "   ", false)
		if err == nil || authz.StatusOf(err, 500) != 400 {
			t.Fatalf("delivery without a brief: err = %v, want 400 request.invalid", err)
		}
		if got := f.get(req.ID, req.TeamID); got.State != domain.RequestStatusInProgress {
			t.Errorf("a refused delivery moved the request to %s", got.State)
		}
	})

	t.Run("no evidence and no no-artifact record is refused", func(t *testing.T) {
		req := f.newRequest("noEvid00", domain.RequestStatusInProgress, &requester)
		err := deliver(t, req, "Delivered, written down.", false)
		if err == nil || authz.StatusOf(err, 500) != 400 {
			t.Fatalf("brief-only delivery: err = %v, want 400 request.invalid", err)
		}
		if !strings.Contains(err.Error(), "evidence") {
			t.Errorf("refusal %q does not name the missing evidence", err)
		}
		if got := f.get(req.ID, req.TeamID); got.State != domain.RequestStatusInProgress {
			t.Errorf("a refused delivery moved the request to %s", got.State)
		}
	})

	t.Run("evidence and a brief are accepted, atomically", func(t *testing.T) {
		req := f.newRequest("deliver0", domain.RequestStatusInProgress, &requester)
		f.attachDelivered(req, f.task)
		req = f.get(req.ID, req.TeamID)
		eventsBefore := f.events(req.ID)
		res, err := f.svc.DeliverRequest(f.ctx, lead, DeliverRequestInput{
			RequestID: req.ID, TeamID: req.TeamID, ExpectedRev: req.Rev,
			OutcomeBrief: "The CSV export is live; the scheduled variant is not included.",
		})
		if err != nil {
			t.Fatalf("deliver with evidence: %v", err)
		}
		if res.Request.State != domain.RequestStatusDelivered {
			t.Errorf("state = %s, want delivered", res.Request.State)
		}
		if res.Request.Rev != req.Rev+1 {
			t.Errorf("rev = %d, want %d (one atomic bump)", res.Request.Rev, req.Rev+1)
		}
		if res.Request.DeliveredAt == nil || res.Request.OutcomeBrief == "" {
			t.Error("the delivery did not record its brief and delivered_at")
		}
		if got := f.events(req.ID); got != eventsBefore+1 {
			t.Errorf("history rows = %d, want exactly one more than %d", got, eventsBefore)
		}
	})

	t.Run("an explicit no-artifact delivery is accepted with no links", func(t *testing.T) {
		req := f.newRequest("noArtif0", domain.RequestStatusInProgress, &requester)
		res, err := f.svc.DeliverRequest(f.ctx, lead, DeliverRequestInput{
			RequestID: req.ID, TeamID: req.TeamID, ExpectedRev: req.Rev,
			OutcomeBrief: "The recommendation is in the reply; there is no artifact.",
			NoArtifact:   true,
		})
		if err != nil {
			t.Fatalf("no-artifact delivery: %v", err)
		}
		if res.Request.State != domain.RequestStatusDelivered {
			t.Errorf("state = %s, want delivered", res.Request.State)
		}
	})

	t.Run("both links and a no-artifact record is refused as ambiguous", func(t *testing.T) {
		req := f.newRequest("both0000", domain.RequestStatusInProgress, &requester)
		f.attachDelivered(req, f.task)
		req = f.get(req.ID, req.TeamID)
		err := deliver(t, req, "Delivered.", true)
		if err == nil || authz.StatusOf(err, 500) != 400 {
			t.Fatalf("links plus no-artifact: err = %v, want 400 request.invalid", err)
		}
		if !strings.Contains(err.Error(), "not both") {
			t.Errorf("refusal %q does not say the two are exclusive", err)
		}
	})

	t.Run("a tombstoned link is not delivery evidence", func(t *testing.T) {
		// D10's whole point: the link survives its target, so a delivery naming a
		// ticket that has since been deleted must not read as evidence. The
		// aggregate reports the item as deleted, so the gate's own count of
		// evidence for the *transaction* still sees the row — which is exactly why
		// this service check exists in front of it. It gets its own ticket: the
		// shared fixture's are reused by later subtests.
		gone := f.newTask("Gone work")
		req := f.newRequest("tombst00", domain.RequestStatusInProgress, &requester)
		f.attachDelivered(req, gone)
		if err := f.svc.Repo.TrashTasks(f.ctx, []int{gone}, nil); err != nil {
			t.Fatalf("trash linked task: %v", err)
		}
		if _, err := f.db.Exec(f.ctx, `DELETE FROM tasks WHERE id = $1`, gone); err != nil {
			t.Fatalf("delete linked task: %v", err)
		}
		req = f.get(req.ID, req.TeamID)
		_, out, err := f.svc.RequestOutcome(f.ctx, req.ID, f.org, req.TeamID)
		if err != nil {
			t.Fatalf("outcome: %v", err)
		}
		if out.Deleted != 1 {
			t.Fatalf("the deleted target is not reported as a tombstone: %+v", out)
		}
		if out.Items[0].State != RequestOutcomeDeleted {
			t.Errorf("item state = %s, want deleted", out.Items[0].State)
		}
		if out.Phase == RequestPhaseDelivered {
			t.Error("a request whose only evidence is a tombstone reported itself delivered")
		}
		if out.DeliveredEligible {
			t.Error("a request whose only evidence is a tombstone was delivery-eligible")
		}
	})

	t.Run("a requester cannot deliver their own request", func(t *testing.T) {
		req := f.newRequest("reqDeliv0", domain.RequestStatusInProgress, &requester)
		f.attachDelivered(req, f.task)
		req = f.get(req.ID, req.TeamID)
		_, err := f.svc.DeliverRequest(f.ctx, f.principal(requester), DeliverRequestInput{
			RequestID: req.ID, TeamID: req.TeamID, ExpectedRev: req.Rev, OutcomeBrief: "Done.",
		})
		if err == nil || authz.StatusOf(err, 500) != 403 {
			t.Fatalf("requester delivery: err = %v, want 403", err)
		}
		if got := f.get(req.ID, req.TeamID); got.State != domain.RequestStatusInProgress {
			t.Errorf("a refused delivery moved the request to %s", got.State)
		}
	})

	t.Run("a stranger cannot deliver and learns nothing", func(t *testing.T) {
		req := f.newRequest("strDeliv0", domain.RequestStatusInProgress, &requester)
		_, err := f.svc.DeliverRequest(f.ctx, f.principal(f.stranger), DeliverRequestInput{
			RequestID: req.ID, TeamID: req.TeamID, ExpectedRev: req.Rev, OutcomeBrief: "Done.", NoArtifact: true,
		})
		if err == nil || authz.StatusOf(err, 500) != 404 {
			t.Fatalf("stranger delivery: err = %v, want 404", err)
		}
	})

	t.Run("a submit-scoped token cannot deliver", func(t *testing.T) {
		req := f.newRequest("tokDeliv0", domain.RequestStatusInProgress, &requester)
		tok := f.tokenPrincipal(f.member)
		tok.Scope = domain.TokenScopeSubmit
		tok.ScopeTeamID = f.team
		_, err := f.svc.DeliverRequest(f.ctx, tok, DeliverRequestInput{
			RequestID: req.ID, TeamID: req.TeamID, ExpectedRev: req.Rev, OutcomeBrief: "Done.", NoArtifact: true,
		})
		if err == nil || authz.StatusOf(err, 500) != 403 {
			t.Fatalf("submit-only delivery: err = %v, want 403", err)
		}
	})
}

// TestRequestOutcomeReadsTheLiveRow is the service read half: the aggregate it
// returns is the same one the pure function builds from the stored facts, and a
// request in another tenant is not found rather than aggregated.
func TestRequestOutcomeReadsTheLiveRow(t *testing.T) {
	f := newOutcomeFixture(t)
	requester := f.customer
	req := f.newRequest("outcome0", domain.RequestStatusInProgress, &requester)
	f.attachDelivered(req, f.task)
	// A routed project is lineage, not evidence.
	if _, _, err := f.svc.Repo.LinkRequest(f.ctx, repository.RequestLinkWrite{
		RequestID: req.ID, OrgID: f.org, TeamID: req.TeamID,
		LinkKind: domain.RequestLinkRoutesTo, TargetKind: domain.RequestTargetProject, TargetID: f.project,
		TargetOrgID: f.org, Actor: "triager",
	}); err != nil {
		t.Fatalf("route request: %v", err)
	}

	row, out, err := f.svc.RequestOutcome(f.ctx, req.ID, f.org, req.TeamID)
	if err != nil {
		t.Fatalf("RequestOutcome: %v", err)
	}
	if row.ID != req.ID {
		t.Errorf("row id = %d, want %d", row.ID, req.ID)
	}
	if out.RoutedProjectID != f.project {
		t.Errorf("routed project = %d, want %d", out.RoutedProjectID, f.project)
	}
	if out.Phase != RequestPhaseNotStarted {
		t.Errorf("phase = %q, want not_started (the linked ticket has not run)", out.Phase)
	}
	if out.Total != 1 || out.Open != 1 {
		t.Errorf("items/total = %d/%d, want one open item: %+v", out.Open, out.Total, out.Items)
	}

	// Another org, and another team in the same org, are the same not-found.
	otherOrg := f.foreign
	if _, _, err := f.svc.RequestOutcome(f.ctx, req.ID, otherOrg, req.TeamID); err == nil || authz.StatusOf(err, 500) != 404 {
		t.Errorf("a request read through another tenant: err = %v, want 404", err)
	}
	if _, _, err := f.svc.RequestOutcome(f.ctx, req.ID, f.org, f.otherTeam); err == nil || authz.StatusOf(err, 500) != 404 {
		t.Errorf("a request read through another team: err = %v, want 404", err)
	}
}

// TestRequestOutcomesForBatchMatchesTheSingleRead proves the batch form a queue
// read uses agrees with the single read for every row, including a request that
// is not in the tenant (absent, never an error).
func TestRequestOutcomesForBatchMatchesTheSingleRead(t *testing.T) {
	f := newOutcomeFixture(t)
	requester := f.customer
	a := f.newRequest("batchA000", domain.RequestStatusInProgress, &requester)
	b := f.newRequest("batchB000", domain.RequestStatusInProgress, &requester)
	f.attachDelivered(a, f.task)
	f.attachDelivered(b, f.task2)
	foreign := f.newRequestIn(f.otherTeam, "batchF000", domain.RequestStatusInProgress, &requester)

	page, err := f.svc.RequestOutcomesFor(f.ctx, f.org, f.team, []int{a.ID, b.ID, foreign.ID, 987654321})
	if err != nil {
		t.Fatalf("RequestOutcomesFor: %v", err)
	}
	if len(page) != 2 {
		t.Fatalf("batch returned %d aggregates, want 2 (the foreign request and the unknown id are absent)", len(page))
	}
	for _, req := range []*domain.TeamRequest{a, b} {
		_, single, err := f.svc.RequestOutcome(f.ctx, req.ID, f.org, req.TeamID)
		if err != nil {
			t.Fatalf("single read of %d: %v", req.ID, err)
		}
		batch, ok := page[req.ID]
		if !ok {
			t.Fatalf("request %d missing from the batch", req.ID)
		}
		if batch.Phase != single.Phase || batch.Total != single.Total || batch.Done != single.Done {
			t.Errorf("request %d: batch %+v disagrees with single %+v", req.ID, batch, single)
		}
	}
}

// TestRequestOutcomeHeldAndGatedFromTheDatabase is criteria 1's database half:
// a held receipt and a merge gate read as what they are through the real reads
// (held receipts are held for a person; a merge that has not been recorded keeps
// its ticket gated), not just through the pure function's literals.
func TestRequestOutcomeHeldAndGatedFromTheDatabase(t *testing.T) {
	f := newOutcomeFixture(t)
	requester := f.customer

	t.Run("a held receipt is pending verify", func(t *testing.T) {
		req := f.newRequest("heldRcpt0", domain.RequestStatusInProgress, &requester)
		var receipt int
		if err := f.db.QueryRow(f.ctx, `INSERT INTO task_receipts (task_id, project_id, summary, state, held)
			VALUES ($1, $2, 'done, check it', 'final', TRUE) RETURNING id`, f.task, f.project).Scan(&receipt); err != nil {
			t.Fatalf("insert held receipt: %v", err)
		}
		if _, _, err := f.svc.Repo.LinkRequest(f.ctx, repository.RequestLinkWrite{
			RequestID: req.ID, OrgID: f.org, TeamID: req.TeamID,
			LinkKind: domain.RequestLinkDeliveredBy, TargetKind: domain.RequestTargetReceipt, TargetID: receipt,
			TargetOrgID: f.org, Actor: "triager",
		}); err != nil {
			t.Fatalf("link receipt: %v", err)
		}
		_, out, err := f.svc.RequestOutcome(f.ctx, req.ID, f.org, req.TeamID)
		if err != nil {
			t.Fatalf("outcome: %v", err)
		}
		if out.Held != 1 {
			t.Errorf("held = %d, want 1: %+v", out.Held, out.Items)
		}
		if out.Phase != RequestPhaseBlocked {
			t.Errorf("phase = %q, want blocked (pending verify is a person's queue)", out.Phase)
		}
	})

	t.Run("a merge gate is not done", func(t *testing.T) {
		req := f.newRequest("gatedTsk0", domain.RequestStatusInProgress, &requester)
		// Complete the ticket with a merge needed, as a completion with a PR does.
		yes := true
		if err := f.svc.Repo.SetTaskMerge(f.ctx, f.task2, &yes, domain.MergeNeeded, nil, ""); err != nil {
			t.Fatalf("set merge needed: %v", err)
		}
		if _, err := f.db.Exec(f.ctx, `UPDATE tasks SET state = 'completed', completed_at = NOW() WHERE id = $1`, f.task2); err != nil {
			t.Fatalf("complete task: %v", err)
		}
		f.attachDelivered(req, f.task2)
		_, out, err := f.svc.RequestOutcome(f.ctx, req.ID, f.org, req.TeamID)
		if err != nil {
			t.Fatalf("outcome: %v", err)
		}
		if out.Gated != 1 || out.Done != 0 {
			t.Errorf("gated/done = %d/%d, want 1/0: a gated ticket is not delivery: %+v", out.Gated, out.Done, out.Items)
		}
		if out.Items[0].MergeState != domain.MergeNeeded {
			t.Errorf("item merge state = %q, want needed", out.Items[0].MergeState)
		}
		// Clean up the gate on this shared ticket so later subtests in this
		// fixture see an ungated ticket.
		if err := f.svc.Repo.SetTaskMerge(f.ctx, f.task2, nil, domain.MergeNone, nil, ""); err != nil {
			t.Fatalf("clear merge: %v", err)
		}
	})

	t.Run("a deleted target is a tombstone with its date", func(t *testing.T) {
		gone := f.newTask("Delivered then re-planned")
		req := f.newRequest("delTomb00", domain.RequestStatusInProgress, &requester)
		f.attachDelivered(req, gone)
		// The trash keeps the snapshot; the delete is what removes the row, and
		// the aggregate must read the pair as a tombstone.
		if err := f.svc.Repo.TrashTasks(f.ctx, []int{gone}, nil); err != nil {
			t.Fatalf("trash task: %v", err)
		}
		if _, err := f.db.Exec(f.ctx, `DELETE FROM tasks WHERE id = $1`, gone); err != nil {
			t.Fatalf("delete task: %v", err)
		}
		_, out, err := f.svc.RequestOutcome(f.ctx, req.ID, f.org, req.TeamID)
		if err != nil {
			t.Fatalf("outcome: %v", err)
		}
		if out.Deleted != 1 {
			t.Fatalf("deleted = %d, want 1: %+v", out.Deleted, out.Items)
		}
		if out.Items[0].DeletedAt == "" {
			t.Error("the tombstone carries no date although the trash still remembers it")
		}
		if out.Phase != RequestPhaseInProgress && out.Phase != RequestPhaseNotStarted {
			t.Errorf("phase = %q: a re-planned request should not read as blocked or delivered", out.Phase)
		}
	})
}
