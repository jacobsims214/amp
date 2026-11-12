package service

// The requester projection's proving tests (AMP #3954, epic 202 story 548, build
// slice E3): a golden JSON leak test, hostile text through every free-text field,
// the fail-closed degradations, and the rule the whole slice exists for — that
// completed work never becomes a declared delivery.
//
// The golden test is the one to keep: it marshals a fully populated view built
// from a maximally internal record (a claim, an assignee, metadata, a duplicate
// pointer, open links to tickets and artifacts, private comment bodies) and
// asserts the bytes contain none of those values. It fails if a field is added to
// the view without a decision about it, and it fails if the projection starts
// reading one more column off the record.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/simstech/amp-api/internal/domain"
)

// internalRequest is a request row at its most internal: every field a requester
// must never see is populated, with values chosen so a leak is unmistakable in
// the serialized bytes (the sentinels below).
func internalRequest() *domain.TeamRequest {
	claim := time.Date(2026, 11, 8, 14, 31, 0, 0, time.UTC)
	needs := time.Date(2026, 11, 10, 9, 15, 0, 0, time.UTC)
	planned := time.Date(2026, 11, 9, 10, 15, 0, 0, time.UTC)
	delivered := time.Date(2026, 11, 11, 16, 12, 0, 0, time.UTC)
	closed := delivered.Add(24 * time.Hour)
	requester, claimer, owner, duplicate, acceptedBy := 31, 9, 12, 77, 31
	return &domain.TeamRequest{
		ID:               149, // sentinel: the serial must never reach the requester
		PublicRef:        "8f2q1x7k4m2p9t5w",
		OrgID:            3, // sentinel
		TeamID:           4, // sentinel
		RequesterUserID:  &requester,
		RequesterContact: "dana@example.com",
		SourceKind:       domain.RequestSourceWeb,
		Subject:          "October invoice export totals don't match the ledger",
		InputBrief:       "The export is short by about €4,300 across 11 rows.",
		OutcomeBrief:     "Included: the corrected CSV and a note. Not included: November's export.",
		State:            domain.RequestStatusDelivered,
		ClaimUserID:      &claimer, ClaimedAt: &claim,
		OwnerUserID:    &owner,
		DuplicateOf:    &duplicate,
		RejectReason:   "REJECT-REASON-SENTINEL",
		NeedsInfoSince: &needs,
		PlannedAt:      &planned,
		DeliveredAt:    &delivered,
		AcceptedBy:     &acceptedBy,
		ClosedAt:       &closed,
		ReopenedCount:  2,
		Rev:            7,
		Metadata: domain.Metadata{
			"jira_key":        "PLAT-88",
			"request_ref":     "8f2q1x7k4m2p9t5w",
			"internal_note":   "METADATA-SENTINEL",
			"github_pr_url":   "https://github.com/acme/amp/pull/9999",
			"merge_gate_hint": "BRANCH-SENTINEL",
		},
		CreatedAt: time.Date(2026, 11, 8, 9, 42, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 11, 11, 16, 12, 0, 0, time.UTC),
	}
}

// internalSentinels are the exact strings the golden test greps the serialized
// view for. Each is a value that lives on the record or on a row adjacent to it
// and that a requester must never be shown.
var internalSentinels = []string{
	"PLAT-88",                       // metadata value (a connector label)
	"METADATA-SENTINEL",             // metadata value (an internal note)
	"github.com/acme/amp/pull/9999", // a branch/PR URL
	"BRANCH-SENTINEL",               // a merge-gate hint
	"REJECT-REASON-SENTINEL",        // a reject reason on a non-rejected request
	"dana@example.com",              // the contact snapshot (the requester knows it; it is not the view's)
	"requester_cover_note",          // an internal comment body the team did not share
	"INTERNAL-COMMENT-SENTINEL",
	"AGENT-SENTINEL", // an agent/worker identity
	"WORKER-SENTINEL",
	"TICKET-TITLE-SENTINEL",
	"MERGE-STATE-SENTINEL",
}

// TestProjectRequesterRequestGoldenLeak is the acceptance criterion's golden
// test: a fully populated projection of a maximally internal record serializes
// to bytes holding none of the internal values, and exactly the whitelisted keys.
func TestProjectRequesterRequestGoldenLeak(t *testing.T) {
	at := time.Date(2026, 11, 10, 9, 15, 0, 0, time.UTC)
	view := ProjectRequesterRequest(RequesterProjectionInput{
		Request:  internalRequest(),
		TeamName: "Northwind support desk",
		Milestones: []domain.RequesterMilestoneInput{
			{Key: domain.RequesterMilestoneReceived, Done: true, At: ptrTime(at)},
			{Key: domain.RequesterMilestoneReviewing, Done: true, At: ptrTime(at)},
			{Key: domain.RequesterMilestoneInProgress, Done: true, At: ptrTime(at)},
			{Key: domain.RequesterMilestoneDelivered, Done: true, At: ptrTime(at)},
		},
		Comments: []domain.RequesterCommentInput{
			// A shared question and answer: these are the only comment bodies a
			// requester sees, and the read path decided to share them.
			{Kind: domain.RequestCommentQuestion, Body: "Which ledger are you comparing against?", At: at},
			{Kind: domain.RequestCommentAnswer, Body: "The October close.", FromRequester: true, At: at},
		},
		Outputs: []domain.RequesterOutputInput{
			{Source: domain.RequesterOutputFromRequester, Name: "october-export-totals.csv", SizeBytes: 186368, Status: domain.RequesterOutputReady, DownloadRef: "grant_opaque_1", At: ptrTime(at)},
			{Source: domain.RequesterOutputFromTeam, Name: "october-invoice-export-corrected.csv", SizeBytes: 188416, Status: domain.RequesterOutputReady, DownloadRef: "grant_opaque_2", At: ptrTime(at)},
		},
	})
	if view == nil {
		t.Fatal("ProjectRequesterRequest returned nil for a real request")
	}
	raw, err := json.Marshal(view)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	body := string(raw)
	for _, bad := range internalSentinels {
		if strings.Contains(body, bad) {
			t.Errorf("the requester view leaks %q:\n%s", bad, body)
		}
	}
	// The serial, the ids and the internal counts never appear as bare numbers
	// either: this catches a struct tag that names one of them (the field-list
	// test in the domain package catches a renamed field, this catches a leak in
	// a value).
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	allowed := map[string]bool{
		"public_ref": true, "state": true, "state_word": true, "subject": true,
		"input_brief": true, "outcome_brief": true, "reject_reason": true,
		"team_name": true, "created_at": true, "needs_reply_since": true,
		"delivered_at": true, "accepted_at": true, "closed_at": true,
		"milestones": true, "comments": true, "outputs": true, "question": true,
	}
	for key := range got {
		if !allowed[key] {
			t.Errorf("view serialized an unexpected key %q", key)
		}
	}
	// The whitelisted values are there and correct: the opaque ref, the state
	// word, the requester's own words, the shared output names and the two-word
	// attributions.
	if got["public_ref"] != "8f2q1x7k4m2p9t5w" {
		t.Errorf("public_ref = %v, want the opaque token", got["public_ref"])
	}
	if got["state_word"] != "Delivered — your call" {
		t.Errorf("state_word = %v", got["state_word"])
	}
	for _, want := range []string{
		"october-invoice-export-corrected.csv", "grant_opaque_1", "grant_opaque_2",
		"Which ledger are you comparing against?", "The October close.",
		"Northwind support desk", "Included: the corrected CSV",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the requester view is missing %q:\n%s", want, body)
		}
	}
	// The reject reason is not shown on a delivered request, even though the
	// record carries one.
	if strings.Contains(body, "reject_reason") {
		t.Errorf("a delivered request serialized reject_reason:\n%s", body)
	}
}

// TestProjectRequesterRequestNoWorkItemsCounted is the acceptance criterion's
// third clause: the projection cannot auto-declare delivery. A request whose
// linked work is all finished — which the input cannot even express, and which
// the aggregate reports as delivered milestones — still projects as undelivered
// while its own state says in_progress. The delivered step stays pending, the
// outcome brief stays hidden and no delivered timestamp appears.
func TestProjectRequesterRequestNoWorkItemsCounted(t *testing.T) {
	r := internalRequest()
	r.State = domain.RequestStatusInProgress
	r.DeliveredAt = nil
	r.ClosedAt = nil
	at := time.Date(2026, 11, 10, 9, 15, 0, 0, time.UTC)
	view := ProjectRequesterRequest(RequesterProjectionInput{
		Request:  r,
		TeamName: "Desk",
		Milestones: []domain.RequesterMilestoneInput{
			{Key: domain.RequesterMilestoneReceived, Done: true, At: ptrTime(at)},
			{Key: domain.RequesterMilestoneReviewing, Done: true, At: ptrTime(at)},
			{Key: domain.RequesterMilestoneInProgress, Done: true, At: ptrTime(at)},
			// The aggregate says the work is done. The request has not been
			// delivered, so the step must not be.
			{Key: domain.RequesterMilestoneDelivered, Done: true, At: ptrTime(at)},
			{Key: domain.RequesterMilestoneAccepted, Done: true, At: ptrTime(at)},
		},
	})
	steps := map[domain.RequesterMilestoneKey]domain.RequesterMilestone{}
	for _, m := range view.Milestones {
		steps[m.Key] = m
	}
	if steps[domain.RequesterMilestoneDelivered].Done {
		t.Error("an in_progress request shows the delivered step done: completed linked work declared a delivery")
	}
	if steps[domain.RequesterMilestoneDelivered].At != nil {
		t.Error("a pending delivered step carries a timestamp")
	}
	if steps[domain.RequesterMilestoneAccepted].Done {
		t.Error("an in_progress request shows the accepted step done")
	}
	if view.OutcomeBrief != "" {
		t.Errorf("an in_progress request exposes an outcome brief: %q", view.OutcomeBrief)
	}
	if view.DeliveredAt != nil {
		t.Error("an in_progress request carries a delivered timestamp")
	}
	if view.StateWord != "In progress" {
		t.Errorf("state word = %q", view.StateWord)
	}

	// Delivered is the only thing that flips it: same input, the lifecycle
	// state delivered.
	r.State = domain.RequestStatusDelivered
	r.DeliveredAt = ptrTime(at)
	view = ProjectRequesterRequest(RequesterProjectionInput{
		Request: r,
		Milestones: []domain.RequesterMilestoneInput{
			{Key: domain.RequesterMilestoneDelivered, Done: true, At: ptrTime(at)},
		},
	})
	for _, m := range view.Milestones {
		if m.Key == domain.RequesterMilestoneDelivered && !m.Done {
			t.Error("a delivered request does not show the delivered step done")
		}
	}
	if view.OutcomeBrief == "" {
		t.Error("a delivered request hides its outcome brief")
	}
	// And requester_accepted is the only thing that marks acceptance.
	if view.AcceptedAt != nil && r.State != domain.RequestStatusRequesterAccepted {
		t.Error("a merely delivered request carries an accepted timestamp")
	}
}

// TestProjectRequesterRequestHostileText runs every free-text field through the
// text path: control sequences, a bidi override on a file name (the
// "invoice\u202Etxt.exe" trick), an HTML-looking subject and a 40 000-character
// brief. Nothing invisible survives, everything readable does, and the result is
// valid JSON.
func TestProjectRequesterRequestHostileText(t *testing.T) {
	at := time.Date(2026, 11, 10, 9, 15, 0, 0, time.UTC)
	r := internalRequest()
	r.Subject = "Invoice\x00 export\x1b[31m totals \u202Egnp.exe"
	r.InputBrief = "<script>alert('x')</script> \u202E reversed \u200Bhidden " + strings.Repeat("é", domain.MaxRequestBriefLen+100)
	r.OutcomeBrief = "Included:\x00 the CSV\nNot included: \u2066November\u2069"
	r.RejectReason = "n/a"
	r.State = domain.RequestStatusDelivered
	view := ProjectRequesterRequest(RequesterProjectionInput{
		Request:  r,
		TeamName: "Desk\u202Eevil",
		Comments: []domain.RequesterCommentInput{
			{Kind: domain.RequestCommentDiscussion, Body: "**bold** <img src=x onerror=alert(1)>\x07", FromRequester: false, At: at},
		},
		Outputs: []domain.RequesterOutputInput{
			{Source: domain.RequesterOutputFromTeam, Name: "invoice\u202Etxt.exe", SizeBytes: 10, Status: domain.RequesterOutputReady, DownloadRef: "g1", At: ptrTime(at)},
		},
		Milestones: []domain.RequesterMilestoneInput{
			{Key: domain.RequesterMilestoneDelivered, Done: true, At: ptrTime(at), Note: "We \u202Eshipped\u202C it\x00!"},
		},
	})
	for name, got := range map[string]string{
		"subject":       view.Subject,
		"input_brief":   view.InputBrief,
		"outcome_brief": view.OutcomeBrief,
		"team_name":     view.TeamName,
		"comment":       view.Comments[0].Body,
		"output_name":   view.Outputs[0].Name,
	} {
		if strings.ContainsAny(got, "\x00\x07\x1b") {
			t.Errorf("%s kept a control character: %q", name, got)
		}
		for _, invisible := range []string{"\u202E", "\u202C", "\u2066", "\u2069", "\u200B"} {
			if strings.Contains(got, invisible) {
				t.Errorf("%s kept the invisible character %U: %q", name, []rune(invisible)[0], got)
			}
		}
	}
	for _, m := range view.Milestones {
		if strings.Contains(m.Note, "\u202E") || strings.Contains(m.Note, "\x00") {
			t.Errorf("milestone %q kept an invisible character: %q", m.Key, m.Note)
		}
	}
	// Readable content survives, including the HTML-looking text (it is rendered
	// by the shared Markdown component, which keeps remote images inert — D13)
	// and the markdown.
	if !strings.Contains(view.InputBrief, "<script>alert('x')</script>") {
		t.Errorf("input_brief lost its literal text: %q", view.InputBrief[:60])
	}
	if !strings.Contains(view.Comments[0].Body, "**bold** <img src=x onerror=alert(1)>") {
		t.Errorf("shared comment lost its markdown/HTML: %q", view.Comments[0].Body)
	}
	if view.Outputs[0].Name != "invoicetxt.exe" {
		t.Errorf("output name = %q, want the bidi override removed", view.Outputs[0].Name)
	}
	if n := len([]rune(view.InputBrief)); n != domain.MaxRequestBriefLen {
		t.Errorf("input_brief is %d runes, want it bounded to %d", n, domain.MaxRequestBriefLen)
	}
	raw, err := json.Marshal(view)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !json.Valid(raw) {
		t.Error("the hostile-text view is not valid JSON")
	}
}

// TestProjectRequesterRequestFailClosed covers the degradations, each of which
// must show the requester less rather than more: a ready file with no download
// reference, an unknown output status, an unknown comment kind, an unknown
// milestone key, and a duplicate milestone key.
func TestProjectRequesterRequestFailClosed(t *testing.T) {
	at := time.Date(2026, 11, 10, 9, 15, 0, 0, time.UTC)
	r := internalRequest()
	r.State = domain.RequestStatusDelivered
	view := ProjectRequesterRequest(RequesterProjectionInput{
		Request: r,
		Milestones: []domain.RequesterMilestoneInput{
			{Key: domain.RequesterMilestoneReceived, Done: true, At: ptrTime(at)},
			// A later duplicate must not override the first.
			{Key: domain.RequesterMilestoneReceived, Done: false},
			{Key: "shipped_to_prod", Done: true, Note: "LEAK"},
		},
		Comments: []domain.RequesterCommentInput{
			{Kind: "internal_note", Body: "LEAK", At: at},
			{Kind: domain.RequestCommentDiscussion, Body: "shared", At: at},
		},
		Outputs: []domain.RequesterOutputInput{
			// Ready but nothing can serve it: must not advertise a download.
			{Source: domain.RequesterOutputFromTeam, Name: "a.csv", SizeBytes: 1, Status: domain.RequesterOutputReady},
			// An unknown status is dropped, not guessed.
			{Source: domain.RequesterOutputFromTeam, Name: "b.csv", SizeBytes: 1, Status: "approved"},
			// A reference on a non-ready file is stripped.
			{Source: domain.RequesterOutputFromTeam, Name: "c.csv", SizeBytes: 1, Status: domain.RequesterOutputPreparing, DownloadRef: "must-not-appear"},
			// An unknown source is dropped.
			{Source: "worker", Name: "d.csv", SizeBytes: 1, Status: domain.RequesterOutputReady, DownloadRef: "g"},
			// An empty name is dropped.
			{Source: domain.RequesterOutputFromTeam, Name: "   ", SizeBytes: 1, Status: domain.RequesterOutputReady, DownloadRef: "g"},
		},
	})
	if len(view.Milestones) != len(domain.RequesterMilestoneKeys) {
		t.Errorf("milestones = %d, want the closed vocabulary's %d", len(view.Milestones), len(domain.RequesterMilestoneKeys))
	}
	if view.Milestones[0].Key != domain.RequesterMilestoneReceived || !view.Milestones[0].Done {
		t.Errorf("received = %+v, want done (the first input wins)", view.Milestones[0])
	}
	raw, _ := json.Marshal(view)
	if strings.Contains(string(raw), "LEAK") {
		t.Errorf("fail-closed filtering let an unknown value through: %s", raw)
	}
	if len(view.Comments) != 1 || view.Comments[0].Body != "shared" {
		t.Errorf("comments = %+v, want only the known-kind shared one", view.Comments)
	}
	if len(view.Outputs) != 2 {
		t.Fatalf("outputs = %+v, want 2 (a kept, b dropped, c kept preparing, d dropped, empty dropped)", view.Outputs)
	}
	if view.Outputs[0].Status != domain.RequesterOutputUnavailable || view.Outputs[0].DownloadRef != "" {
		t.Errorf("a ready file with no reference = %+v, want unavailable and no ref", view.Outputs[0])
	}
	if view.Outputs[1].Status != domain.RequesterOutputPreparing || view.Outputs[1].DownloadRef != "" {
		t.Errorf("a preparing file = %+v, want its ref stripped", view.Outputs[1])
	}
	if strings.Contains(string(raw), "must-not-appear") {
		t.Error("a reference on a non-ready file reached the requester")
	}
	// A nil request is no view, not an empty one.
	if got := ProjectRequesterRequest(RequesterProjectionInput{}); got != nil {
		t.Errorf("a nil request projected to %+v, want nil", got)
	}
}

// TestProjectRequesterRequestQuestion covers the needs-more-information card: it
// appears only in needs_information, only from a shared question, and never from
// a shared comment of another kind.
func TestProjectRequesterRequestQuestion(t *testing.T) {
	at := time.Date(2026, 11, 10, 9, 15, 0, 0, time.UTC)
	later := at.Add(time.Hour)
	r := internalRequest()

	// A question while the request is in progress: no card (the desk is not
	// waiting on the requester).
	r.State = domain.RequestStatusInProgress
	view := ProjectRequesterRequest(RequesterProjectionInput{
		Request:  r,
		Comments: []domain.RequesterCommentInput{{Kind: domain.RequestCommentQuestion, Body: "which ledger?", At: at}},
	})
	if view.Question != nil {
		t.Error("an in_progress request shows a needs-reply card")
	}

	// needs_information with two shared questions: the latest is the open one.
	r.State = domain.RequestStatusNeedsInformation
	r.NeedsInfoSince = &at
	view = ProjectRequesterRequest(RequesterProjectionInput{
		Request: r,
		Comments: []domain.RequesterCommentInput{
			{Kind: domain.RequestCommentQuestion, Body: "first?", At: at},
			{Kind: domain.RequestCommentQuestion, Body: "which ledger?", At: later},
		},
	})
	if view.Question == nil || view.Question.Body != "which ledger?" {
		t.Errorf("question = %+v, want the latest shared question", view.Question)
	}
	if view.NeedsReplySince == nil || !view.NeedsReplySince.Equal(at) {
		t.Errorf("needs_reply_since = %v, want %v", view.NeedsReplySince, at)
	}

	// needs_information with no shared question: no card. The projection must not
	// invent a prompt, and must not reach for a private one.
	view = ProjectRequesterRequest(RequesterProjectionInput{
		Request:  r,
		Comments: []domain.RequesterCommentInput{{Kind: domain.RequestCommentDiscussion, Body: "internal", At: at}},
	})
	if view.Question != nil {
		t.Errorf("a needs_information request without a shared question invented one: %+v", view.Question)
	}

	// A question body that sanitizes to nothing is not a card either.
	view = ProjectRequesterRequest(RequesterProjectionInput{
		Request:  r,
		Comments: []domain.RequesterCommentInput{{Kind: domain.RequestCommentQuestion, Body: " \x00\u202E ", At: at}},
	})
	if view.Question != nil {
		t.Errorf("an empty question produced a card: %+v", view.Question)
	}
}

// TestProjectRequesterRequestAttribution pins the two-word attribution: the
// requester's own comment is "you", everything else — an owner, a triager, an
// agent, a worker, a connector's service client — is "the desk", and no display
// name or user id reaches the view.
func TestProjectRequesterRequestAttribution(t *testing.T) {
	at := time.Date(2026, 11, 10, 9, 15, 0, 0, time.UTC)
	r := internalRequest()
	r.State = domain.RequestStatusInProgress
	view := ProjectRequesterRequest(RequesterProjectionInput{
		Request: r,
		Comments: []domain.RequesterCommentInput{
			{Kind: domain.RequestCommentAnswer, Body: "mine", FromRequester: true, At: at},
			{Kind: domain.RequestCommentDiscussion, Body: "theirs", FromRequester: false, At: at},
		},
	})
	if len(view.Comments) != 2 {
		t.Fatalf("comments = %d, want 2", len(view.Comments))
	}
	if view.Comments[0].From != domain.RequesterAuthorYou {
		t.Errorf("the requester's own comment attributes %q, want %q", view.Comments[0].From, domain.RequesterAuthorYou)
	}
	if view.Comments[1].From != domain.RequesterAuthorDesk {
		t.Errorf("a team comment attributes %q, want %q", view.Comments[1].From, domain.RequesterAuthorDesk)
	}
	raw, _ := json.Marshal(view)
	for _, bad := range []string{"amp-worker", "agent@", "worker_id", "author_user_id", "author\":"} {
		if strings.Contains(string(raw), bad) {
			t.Errorf("attribution leaked %q: %s", bad, raw)
		}
	}
}

// TestEnforceDeliveryClaimIsTheLastDefence pins the second half of "a closed
// ticket never delivers a request": enforceDeliveryClaim clears a delivery claim
// the lifecycle state does not support, and can only ever clear. It runs last in
// ProjectRequesterRequest, so it is tested directly too — otherwise the milestone
// gate would mask it (and a later reassembly of the view would lose the rule
// silently, which is exactly the shape this defence exists for).
func TestEnforceDeliveryClaimIsTheLastDefence(t *testing.T) {
	at := time.Date(2026, 11, 10, 9, 15, 0, 0, time.UTC)
	build := func(state domain.RequestStatus) *domain.RequesterRequestView {
		return &domain.RequesterRequestView{
			State:        state,
			OutcomeBrief: "we shipped it",
			DeliveredAt:  ptrTime(at),
			AcceptedAt:   ptrTime(at),
			Milestones: []domain.RequesterMilestone{
				{Key: domain.RequesterMilestoneInProgress, Done: true, At: ptrTime(at)},
				{Key: domain.RequesterMilestoneDelivered, Done: true, At: ptrTime(at)},
				{Key: domain.RequesterMilestoneAccepted, Done: true, At: ptrTime(at)},
			},
		}
	}
	for _, state := range domain.RequestStatuses {
		v := build(state)
		enforceDeliveryClaim(v)
		delivered := state == domain.RequestStatusDelivered || state == domain.RequestStatusRequesterAccepted
		accepted := state == domain.RequestStatusRequesterAccepted
		if !delivered {
			if v.OutcomeBrief != "" {
				t.Errorf("%s: outcome brief survived a non-delivered state", state)
			}
			if v.DeliveredAt != nil {
				t.Errorf("%s: delivered timestamp survived a non-delivered state", state)
			}
		}
		if !accepted && v.AcceptedAt != nil {
			t.Errorf("%s: accepted timestamp survived a non-accepted state", state)
		}
		for _, m := range v.Milestones {
			switch m.Key {
			case domain.RequesterMilestoneDelivered:
				if m.Done != delivered {
					t.Errorf("%s: delivered milestone done = %v, want %v", state, m.Done, delivered)
				}
			case domain.RequesterMilestoneAccepted:
				if m.Done != accepted {
					t.Errorf("%s: accepted milestone done = %v, want %v", state, m.Done, accepted)
				}
			default:
				// A step this function knows nothing about is never touched: it can
				// only clear a delivery claim, never any other fact.
				if !m.Done {
					t.Errorf("%s: enforceDeliveryClaim cleared the %q milestone", state, m.Key)
				}
			}
			if !delivered && (m.Key == domain.RequesterMilestoneDelivered || m.Key == domain.RequesterMilestoneAccepted) && m.At != nil {
				t.Errorf("%s: a cleared %q milestone kept its timestamp", state, m.Key)
			}
		}
	}
}

// TestRequesterMilestonesGateIsNotMaskedByTheLastDefence pins the milestone gate
// on its own. Without this, the two layers hide each other: the gate is what
// keeps a *pending-at-this-state* step from being built at all, and
// enforceDeliveryClaim is what catches a view assembled some other way. Testing
// only the whole projection would pass with either one removed.
func TestRequesterMilestonesGateIsNotMaskedByTheLastDefence(t *testing.T) {
	at := time.Date(2026, 11, 10, 9, 15, 0, 0, time.UTC)
	// in_progress with the aggregate reporting every step done: the delivered and
	// accepted steps must come back pending from the builder itself.
	got := requesterMilestones([]domain.RequesterMilestoneInput{
		{Key: domain.RequesterMilestoneReceived, Done: true, At: ptrTime(at)},
		{Key: domain.RequesterMilestoneInProgress, Done: true, At: ptrTime(at)},
		{Key: domain.RequesterMilestoneDelivered, Done: true, At: ptrTime(at)},
		{Key: domain.RequesterMilestoneAccepted, Done: true, At: ptrTime(at)},
	}, domain.RequestStatusInProgress)
	supplied := map[domain.RequesterMilestoneKey]bool{
		domain.RequesterMilestoneReceived: true, domain.RequesterMilestoneInProgress: true,
		domain.RequesterMilestoneDelivered: true, domain.RequesterMilestoneAccepted: true,
	}
	for _, m := range got {
		switch m.Key {
		case domain.RequesterMilestoneDelivered, domain.RequesterMilestoneAccepted:
			if m.Done {
				t.Errorf("requesterMilestones marked %q done for an in_progress request", m.Key)
			}
			if m.At != nil {
				t.Errorf("requesterMilestones kept the timestamp on a pending %q step", m.Key)
			}
		default:
			if supplied[m.Key] != m.Done {
				t.Errorf("requesterMilestones %q done = %v, want %v (supplied done)", m.Key, m.Done, supplied[m.Key])
			}
		}
	}
	// The stepper is always the whole closed vocabulary, in order, whatever the
	// aggregate sent — a requester sees where the request is, not only what
	// happened.
	if len(got) != len(domain.RequesterMilestoneKeys) {
		t.Fatalf("stepper has %d steps, want %d", len(got), len(domain.RequesterMilestoneKeys))
	}
	for i, key := range domain.RequesterMilestoneKeys {
		if got[i].Key != key {
			t.Errorf("stepper[%d] = %q, want %q", i, got[i].Key, key)
		}
	}
	// A note survives on a pending step (it describes what is happening); the
	// state gate governs the claim, not the sentence.
	got = requesterMilestones([]domain.RequesterMilestoneInput{
		{Key: domain.RequesterMilestoneInProgress, Done: true, Note: "we are re-running the export"},
	}, domain.RequestStatusTriaging)
	for _, m := range got {
		if m.Key == domain.RequesterMilestoneInProgress {
			if m.Done {
				t.Error("a triaging request shows work in progress as done")
			}
			if m.Note != "we are re-running the export" {
				t.Errorf("the pending step lost its note: %q", m.Note)
			}
		}
	}
}

// ptrTime is the test's helper for an optional timestamp.
func ptrTime(t time.Time) *time.Time { return &t }

// compile-time guard: the input types are the contract between the aggregate and
// this projection, so a change to either shows up here.
var _ = func(in RequesterProjectionInput) *domain.RequesterRequestView {
	return ProjectRequesterRequest(in)
}
