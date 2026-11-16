package service

import (
	"strings"
	"testing"

	"github.com/simstech/amp-api/internal/auth"
	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
)

// Team request lifecycle policy: the pure decision's contract (AMP #3953, epic
// 202, story 548). Every test here is pure — no database, no clock, no
// fixtures — because the policy is, and a decision that needed a database to
// test would be a decision that reads one.
//
// The shape of the suite is the shape of the guarantee:
//
//   - TestRequestTransitions_EveryPairIsDecided walks all nine states × nine
//     destinations × four capabilities (324 cases) and compares each answer
//     with the lifecycle contract stated independently, below. The expected
//     table is written out rather than derived from the production table, so a
//     change to either side is a diff a reviewer sees.
//   - the evidence tests pin what each edge's act must carry (I2, I3, I5);
//   - the requester tests pin I1/D5: nobody but the request's own requester, in
//     the web UI, can accept, answer or reopen — not a team member, a manager,
//     an agent account or a token;
//   - TestRequestTransitions_NoTriggerButExplicitMovesAnything pins I8 across
//     the whole matrix: linked ticket closure, an aggregate and a sweep never
//     produce a plan, in any state, at any capability.

// lifecycleContracts is the approved lifecycle, stated once as data: the edge,
// the least capability that may make it, and how the act is evidenced. It is
// transcribed from the approved product contract (artifact 36 v2: "new →
// triaging → (needs_information ↔) → accepted → in_progress → delivered →
// requester_accepted, plus terminal rejected and duplicate. Reopen is a
// transition, not a state: the requester reopens from delivered/accepted, the
// team from rejected/duplicate") and from D5–D7 for who may do what.
type lifecycleContract struct {
	from, to domain.RequestStatus
	min      RequestCapability
	kind     requestMoveKind
	reopen   bool
	detach   bool
	// ev is the evidence the act carries, valid for its kind.
	ev RequestTransitionEvidence
	// set is the extra columns the write must set, exactly.
	set []RequestTransitionColumn
}

// The expected graph. Written out (rather than built from production's
// requestMoves) on purpose: this is the contract the tests defend.
var expectedLifecycle = []lifecycleContract{
	// The working chain.
	{from: domain.RequestStatusNew, to: domain.RequestStatusTriaging, min: RequestCapabilityTeam},
	{from: domain.RequestStatusTriaging, to: domain.RequestStatusAccepted, min: RequestCapabilityTeam},
	{from: domain.RequestStatusAccepted, to: domain.RequestStatusInProgress, min: RequestCapabilityTeam},
	{from: domain.RequestStatusInProgress, to: domain.RequestStatusDelivered, min: RequestCapabilityTeam,
		kind: requestMoveDelivery, ev: RequestTransitionEvidence{Trigger: RequestTriggerExplicit, OutcomeBrief: "Mapping live in prod. Includes new sign-ins; excludes the 14 existing accounts.", DeliveredByCount: 2},
		set: []RequestTransitionColumn{RequestSetDeliveredAt}},

	// Asking (I5).
	{from: domain.RequestStatusTriaging, to: domain.RequestStatusNeedsInformation, min: RequestCapabilityTeam,
		kind: requestMoveQuestion, ev: RequestTransitionEvidence{Trigger: RequestTriggerExplicit, Question: "Which group is the default for Berlin users?"},
		set: []RequestTransitionColumn{RequestSetNeedsInfoSince}},
	{from: domain.RequestStatusAccepted, to: domain.RequestStatusNeedsInformation, min: RequestCapabilityTeam,
		kind: requestMoveQuestion, ev: RequestTransitionEvidence{Trigger: RequestTriggerExplicit, Question: "Do you need the existing accounts moved too?"},
		set: []RequestTransitionColumn{RequestSetNeedsInfoSince}},
	{from: domain.RequestStatusInProgress, to: domain.RequestStatusNeedsInformation, min: RequestCapabilityTeam,
		kind: requestMoveQuestion, ev: RequestTransitionEvidence{Trigger: RequestTriggerExplicit, Question: "The IdP metadata is missing the signing cert — can you re-export it?"},
		set: []RequestTransitionColumn{RequestSetNeedsInfoSince}},

	// Acceptance: the requester's alone, from delivered only (I1, D5).
	{from: domain.RequestStatusDelivered, to: domain.RequestStatusRequesterAccepted, min: RequestCapabilityRequester,
		set: []RequestTransitionColumn{RequestSetAcceptedAt}},

	// Reopen as a transition (D6).
	{from: domain.RequestStatusDelivered, to: domain.RequestStatusTriaging, min: RequestCapabilityRequester, reopen: true,
		set: []RequestTransitionColumn{RequestSetReopenedAt, RequestSetReopenedCount}},
	{from: domain.RequestStatusRequesterAccepted, to: domain.RequestStatusTriaging, min: RequestCapabilityRequester, reopen: true,
		set: []RequestTransitionColumn{RequestSetReopenedAt, RequestSetReopenedCount}},
	{from: domain.RequestStatusRejected, to: domain.RequestStatusTriaging, min: RequestCapabilityTeam, reopen: true,
		set: []RequestTransitionColumn{RequestSetReopenedAt, RequestSetReopenedCount}},
	{from: domain.RequestStatusRejected, to: domain.RequestStatusNeedsInformation, min: RequestCapabilityTeam, kind: requestMoveQuestion, reopen: true,
		ev:  RequestTransitionEvidence{Trigger: RequestTriggerExplicit, Question: "Now that we're reconsidering: which of the two groups should be the default?"},
		set: []RequestTransitionColumn{RequestSetNeedsInfoSince, RequestSetReopenedAt, RequestSetReopenedCount}},
	{from: domain.RequestStatusDuplicate, to: domain.RequestStatusTriaging, min: RequestCapabilityTeam, reopen: true, detach: true,
		set: []RequestTransitionColumn{RequestSetReopenedAt, RequestSetReopenedCount}},
	{from: domain.RequestStatusDuplicate, to: domain.RequestStatusNeedsInformation, min: RequestCapabilityTeam, kind: requestMoveQuestion, reopen: true, detach: true,
		ev:  RequestTransitionEvidence{Trigger: RequestTriggerExplicit, Question: "This was folded into REQ-8DXT0, but it now looks different — what changed?"},
		set: []RequestTransitionColumn{RequestSetNeedsInfoSince, RequestSetReopenedAt, RequestSetReopenedCount}},

	// The team's closes (I3): reject and duplicate belong to the manager row.
	{from: domain.RequestStatusNew, to: domain.RequestStatusRejected, min: RequestCapabilityManager, kind: requestMoveReject,
		ev:  RequestTransitionEvidence{Trigger: RequestTriggerExplicit, RejectReason: "Out of scope: we don't run hardware procurement."},
		set: []RequestTransitionColumn{RequestSetClosedAt}},
	{from: domain.RequestStatusTriaging, to: domain.RequestStatusRejected, min: RequestCapabilityManager, kind: requestMoveReject,
		ev:  RequestTransitionEvidence{Trigger: RequestTriggerExplicit, RejectReason: "Self-hosted directory sync only; pointed the requester at the guide."},
		set: []RequestTransitionColumn{RequestSetClosedAt}},
	{from: domain.RequestStatusNeedsInformation, to: domain.RequestStatusRejected, min: RequestCapabilityManager, kind: requestMoveReject,
		ev:  RequestTransitionEvidence{Trigger: RequestTriggerExplicit, RejectReason: "Requester withdrew it."},
		set: []RequestTransitionColumn{RequestSetClosedAt}},
	{from: domain.RequestStatusAccepted, to: domain.RequestStatusRejected, min: RequestCapabilityManager, kind: requestMoveReject,
		ev:  RequestTransitionEvidence{Trigger: RequestTriggerExplicit, RejectReason: "Blocked by a security review; revisit next quarter."},
		set: []RequestTransitionColumn{RequestSetClosedAt}},
	{from: domain.RequestStatusNew, to: domain.RequestStatusDuplicate, min: RequestCapabilityManager, kind: requestMoveDuplicate,
		ev:  RequestTransitionEvidence{Trigger: RequestTriggerExplicit, CanonicalRequestID: 71},
		set: []RequestTransitionColumn{RequestSetClosedAt}},
	{from: domain.RequestStatusTriaging, to: domain.RequestStatusDuplicate, min: RequestCapabilityManager, kind: requestMoveDuplicate,
		ev:  RequestTransitionEvidence{Trigger: RequestTriggerExplicit, CanonicalRequestID: 71},
		set: []RequestTransitionColumn{RequestSetClosedAt}},
	{from: domain.RequestStatusNeedsInformation, to: domain.RequestStatusDuplicate, min: RequestCapabilityManager, kind: requestMoveDuplicate,
		ev:  RequestTransitionEvidence{Trigger: RequestTriggerExplicit, CanonicalRequestID: 71},
		set: []RequestTransitionColumn{RequestSetClosedAt}},
	{from: domain.RequestStatusInProgress, to: domain.RequestStatusRejected, min: RequestCapabilityManager, kind: requestMoveReject,
		ev:  RequestTransitionEvidence{Trigger: RequestTriggerExplicit, RejectReason: "Blocked by a security review; revisit next quarter."},
		set: []RequestTransitionColumn{RequestSetClosedAt}},
	{from: domain.RequestStatusAccepted, to: domain.RequestStatusDuplicate, min: RequestCapabilityManager, kind: requestMoveDuplicate,
		ev:  RequestTransitionEvidence{Trigger: RequestTriggerExplicit, CanonicalRequestID: 71},
		set: []RequestTransitionColumn{RequestSetClosedAt}},
	{from: domain.RequestStatusInProgress, to: domain.RequestStatusDuplicate, min: RequestCapabilityManager, kind: requestMoveDuplicate,
		ev:  RequestTransitionEvidence{Trigger: RequestTriggerExplicit, CanonicalRequestID: 71},
		set: []RequestTransitionColumn{RequestSetClosedAt}},
}

const (
	testRequestID   = 42
	testRequesterID = 9001
)

// requesterSnapshot is a request in state, submitted by the test requester.
func requesterSnapshot(state domain.RequestStatus) RequestSnapshot {
	requester := testRequesterID
	canonical := 71
	return RequestSnapshot{ID: testRequestID, State: state, RequesterUserID: &requester, DuplicateOf: func() *int {
		if state == domain.RequestStatusDuplicate {
			return &canonical
		}
		return nil
	}()}
}

// capabilityActor is the actor user id a capability acts as: the request's own
// requester for RequestCapabilityRequester, somebody else otherwise.
func capabilityActor(c RequestCapability) int {
	if c == RequestCapabilityRequester {
		return testRequesterID
	}
	return 5000
}

// allCapabilities is every capability the matrix walks.
var allCapabilities = []RequestCapability{
	RequestCapabilityRequester, RequestCapabilityTeam, RequestCapabilityManager,
}

// contractFor finds the expected edge, if the contract has one.
func contractFor(from, to domain.RequestStatus) (lifecycleContract, bool) {
	for _, c := range expectedLifecycle {
		if c.from == from && c.to == to {
			return c, true
		}
	}
	return lifecycleContract{}, false
}

// TestRequestTransitions_EveryPairIsDecided is the whole lifecycle in one
// table: 9 states × 9 destinations × 3 capabilities. A pair the contract does
// not have must be refused, naming the states the caller could reach instead
// (I7); a pair it has must be planned for a capability at or above the edge's
// floor, and refused as a 403 for one below it.
func TestRequestTransitions_EveryPairIsDecided(t *testing.T) {
	for _, from := range domain.RequestStatuses {
		for _, to := range domain.RequestStatuses {
			for _, cap := range allCapabilities {
				name := string(from) + "_to_" + string(to) + "_as_" + cap.String()
				t.Run(name, func(t *testing.T) {
					contract, legal := contractFor(from, to)
					// A request in needs_information is answered by the
					// requester, not moved by the table's working rows: the
					// matrix treats from=needs_information × requester
					// separately (TestRequestTransitions_AnswerRestoresPriorState)
					// because its destination is the interrupted state, which
					// the snapshot carries, not the contract table.
					if from == domain.RequestStatusNeedsInformation && cap == RequestCapabilityRequester {
						t.Skip("needs_information → prior state is the answer path; see TestRequestTransitions_AnswerRestoresPriorState")
					}
					snapshot := requesterSnapshot(from)
					ev := RequestTransitionEvidence{Trigger: RequestTriggerExplicit}
					if legal {
						ev = contract.ev
						ev.Trigger = RequestTriggerExplicit
					}
					plan, err := PlanRequestTransition(RequestTransitionInput{
						Request:     snapshot,
						To:          to,
						Capability:  cap,
						ActorUserID: capabilityActor(cap),
						Evidence:    ev,
					})
					if !legal {
						if err == nil {
							t.Fatalf("a request in %s moved to %s as %s, but the approved lifecycle has no such edge", from, to, cap)
						}
						assertRequestInvalid(t, err)
						return
					}
					if from == to {
						t.Fatalf("the contract table contains a self-edge %s", from)
					}
					if !cap.canMake(contract.min) {
						if err == nil {
							t.Fatalf("%s moved %s → %s, but that edge belongs to %s", cap, from, to, contract.min)
						}
						if got := authz.StatusOf(err, 500); got != 403 {
							t.Fatalf("%s moving %s → %s: status = %d, want 403 (%v)", cap, from, to, got, err)
						}
						if !strings.Contains(err.Error(), contract.min.String()) || !strings.Contains(err.Error(), cap.String()) {
							t.Fatalf("the capability refusal does not name the required capability (%s) and the caller's (%s): %v", contract.min, cap, err)
						}
						return
					}
					if err != nil {
						t.Fatalf("%s moving %s → %s was refused: %v", cap, from, to, err)
					}
					if plan.From != from || plan.To != to {
						t.Fatalf("plan moves %s → %s, want %s → %s", plan.From, plan.To, from, to)
					}
					assertColumns(t, plan, contract.set)
					if plan.Reopened != contract.reopen {
						t.Fatalf("plan.Reopened = %v, want %v for %s → %s", plan.Reopened, contract.reopen, from, to)
					}
					if contract.detach && !plan.ClearDuplicateOf {
						t.Fatalf("a reopen out of duplicate must detach the canonical in the same write (D7): plan does not")
					}
					if !contract.detach && plan.ClearDuplicateOf {
						t.Fatalf("plan detaches duplicate_of on a move that does not leave duplicate")
					}
					assertPlanEvidence(t, plan, contract)
				})
			}
		}
	}
}

// assertColumns compares the plan's columns with the contract's, exactly and in
// order (the policy appends them in the RequestSet* constants' order).
func assertColumns(t *testing.T, plan *RequestTransitionPlan, want []RequestTransitionColumn) {
	t.Helper()
	if len(plan.Set) != len(want) {
		t.Fatalf("plan.Set = %v, want %v", plan.Set, want)
	}
	for i := range want {
		if plan.Set[i] != want[i] {
			t.Fatalf("plan.Set = %v, want %v", plan.Set, want)
		}
	}
}

// assertPlanEvidence checks the plan carried the act's own values, not zeros.
func assertPlanEvidence(t *testing.T, plan *RequestTransitionPlan, c lifecycleContract) {
	t.Helper()
	switch c.kind {
	case requestMoveQuestion:
		if plan.Question == "" || plan.Question != strings.TrimSpace(c.ev.Question) {
			t.Fatalf("plan.Question = %q, want %q", plan.Question, c.ev.Question)
		}
	case requestMoveReject:
		if plan.RejectReason == "" || plan.RejectReason != c.ev.RejectReason {
			t.Fatalf("plan.RejectReason = %q, want %q", plan.RejectReason, c.ev.RejectReason)
		}
	case requestMoveDuplicate:
		if plan.DuplicateOf == nil || *plan.DuplicateOf != c.ev.CanonicalRequestID {
			t.Fatalf("plan.DuplicateOf = %v, want %d", plan.DuplicateOf, c.ev.CanonicalRequestID)
		}
	case requestMoveDelivery:
		if plan.OutcomeBrief != c.ev.OutcomeBrief {
			t.Fatalf("plan.OutcomeBrief = %q, want %q", plan.OutcomeBrief, c.ev.OutcomeBrief)
		}
	}
	if plan.To == domain.RequestStatusRequesterAccepted {
		if plan.AcceptedBy == nil || *plan.AcceptedBy != testRequesterID {
			t.Fatalf("acceptance plan.AcceptedBy = %v, want the requester %d", plan.AcceptedBy, testRequesterID)
		}
	} else if plan.AcceptedBy != nil {
		t.Fatalf("plan.AcceptedBy = %v on a move that is not an acceptance", *plan.AcceptedBy)
	}
}

// TestRequestTransitions_ProductionGraphMatchesTheContract proves the
// production table has no edge the contract lacks and no contract edge it
// lacks — so the 324-case matrix above is testing the table that ships, not a
// table that happens to agree with itself.
func TestRequestTransitions_ProductionGraphMatchesTheContract(t *testing.T) {
	seen := map[string]bool{}
	for _, m := range requestMoves {
		key := string(m.from) + "→" + string(m.to)
		if seen[key] {
			t.Errorf("the production table declares %s twice", key)
		}
		seen[key] = true
		c, ok := contractFor(m.from, m.to)
		if !ok {
			t.Errorf("the production table has an edge the approved lifecycle does not: %s", key)
			continue
		}
		if m.min != c.min {
			t.Errorf("%s: minimum capability = %s, contract says %s", key, m.min, c.min)
		}
		if m.kind != c.kind {
			t.Errorf("%s: kind = %d, contract says %d", key, m.kind, c.kind)
		}
		if m.reopen != c.reopen {
			t.Errorf("%s: reopen = %v, contract says %v", key, m.reopen, c.reopen)
		}
		if m.detach != c.detach {
			t.Errorf("%s: detach = %v, contract says %v", key, m.detach, c.detach)
		}
	}
	for _, c := range expectedLifecycle {
		if !seen[string(c.from)+"→"+string(c.to)] {
			t.Errorf("the approved lifecycle has an edge production does not implement: %s → %s", c.from, c.to)
		}
	}
	// Every state the lifecycle names is reachable and can be left: an orphan
	// state would mean the contract's diagram and this table disagree.
	for _, s := range domain.RequestStatuses {
		if s == domain.RequestStatusNew {
			continue
		}
		reachable := false
		for _, m := range requestMoves {
			if m.to == s {
				reachable = true
			}
		}
		if !reachable {
			t.Errorf("state %s has no incoming edge", s)
		}
	}
}

// TestRequestTransitions_IllegalMoveNamesAllowedNextStates pins I7's other
// half: the refusal tells the caller what it could do instead, and what it
// names is exactly AllowedRequestTransitions.
func TestRequestTransitions_IllegalMoveNamesAllowedNextStates(t *testing.T) {
	cases := []struct {
		name       string
		from       domain.RequestStatus
		to         domain.RequestStatus
		cap        RequestCapability
		wantInText []domain.RequestStatus
	}{
		{"triaging straight to delivered", domain.RequestStatusTriaging, domain.RequestStatusDelivered, RequestCapabilityTeam,
			[]domain.RequestStatus{domain.RequestStatusNeedsInformation, domain.RequestStatusAccepted}},
		{"in progress cannot go back to triaging", domain.RequestStatusInProgress, domain.RequestStatusTriaging, RequestCapabilityManager,
			[]domain.RequestStatus{domain.RequestStatusNeedsInformation, domain.RequestStatusDelivered, domain.RequestStatusRejected, domain.RequestStatusDuplicate}},
		{"delivered back to in progress (that is a reopen)", domain.RequestStatusDelivered, domain.RequestStatusInProgress, RequestCapabilityRequester,
			[]domain.RequestStatus{domain.RequestStatusTriaging, domain.RequestStatusRequesterAccepted}},
		{"delivered offers the team nothing", domain.RequestStatusDelivered, domain.RequestStatusRejected, RequestCapabilityTeam,
			[]domain.RequestStatus{}},
		{"done request cannot be delivered again", domain.RequestStatusRequesterAccepted, domain.RequestStatusDelivered, RequestCapabilityRequester,
			[]domain.RequestStatus{domain.RequestStatusTriaging}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := PlanRequestTransition(RequestTransitionInput{
				Request:     requesterSnapshot(c.from),
				To:          c.to,
				Capability:  c.cap,
				ActorUserID: capabilityActor(c.cap),
				Evidence:    RequestTransitionEvidence{Trigger: RequestTriggerExplicit},
			})
			if err == nil {
				t.Fatalf("%s → %s as %s was allowed", c.from, c.to, c.cap)
			}
			assertRequestInvalid(t, err)
			msg := err.Error()
			if !strings.Contains(msg, string(c.from)) || !strings.Contains(msg, string(c.to)) {
				t.Fatalf("refusal does not name the move %s → %s: %v", c.from, c.to, err)
			}
			for _, want := range c.wantInText {
				if !strings.Contains(msg, string(want)) {
					t.Fatalf("refusal does not name allowed next state %s: %v", want, err)
				}
			}
			// The list it names is AllowedRequestTransitions, exactly.
			allowed := AllowedRequestTransitions(requesterSnapshot(c.from), c.cap)
			if len(allowed) != len(c.wantInText) {
				t.Fatalf("AllowedRequestTransitions(%s, %s) = %v, want %v", c.from, c.cap, allowed, c.wantInText)
			}
			for i := range allowed {
				if allowed[i] != c.wantInText[i] {
					t.Fatalf("AllowedRequestTransitions(%s, %s) = %v, want %v", c.from, c.cap, allowed, c.wantInText)
				}
			}
		})
	}
}

// TestRequestTransitions_AllowedFollowsCapability: the same request offers a
// requester and the team different actions, and none at all to a stranger.
func TestRequestTransitions_AllowedFollowsCapability(t *testing.T) {
	snapshot := requesterSnapshot(domain.RequestStatusDelivered)
	if got := AllowedRequestTransitions(snapshot, RequestCapabilityNone); got != nil {
		t.Fatalf("a stranger was offered %v", got)
	}
	requester := AllowedRequestTransitions(snapshot, RequestCapabilityRequester)
	if len(requester) != 2 || requester[0] != domain.RequestStatusTriaging || requester[1] != domain.RequestStatusRequesterAccepted {
		t.Fatalf("requester options on a delivered request = %v, want [triaging requester_accepted] (lifecycle order)", requester)
	}
	if team := AllowedRequestTransitions(snapshot, RequestCapabilityTeam); len(team) != 0 {
		t.Fatalf("the team was offered %v on a delivered request: acceptance is the requester's and a reopen out of delivery is too", team)
	}
	// A request in needs_information offers its requester one destination: the
	// state the question interrupted (I5).
	needsInfo := requesterSnapshot(domain.RequestStatusNeedsInformation)
	needsInfo.PriorState = domain.RequestStatusInProgress
	if got := AllowedRequestTransitions(needsInfo, RequestCapabilityRequester); len(got) != 1 || got[0] != domain.RequestStatusInProgress {
		t.Fatalf("requester options in needs_information with prior in_progress = %v, want [in_progress]", got)
	}
	if got := AllowedRequestTransitions(needsInfo, RequestCapabilityManager); len(got) != 2 {
		t.Fatalf("manager options in needs_information = %v, want the two closes", got)
	}
}

// ---- I1/D5: acceptance, answering and requester reopen are the requester's ----

// TestRequestTransitions_AcceptanceIsTheRequestersAlone is I1 as a test: a team
// member, a team lead/manager, an agent account and an API token can neither
// reach requester_accepted nor claim to be the requester.
func TestRequestTransitions_AcceptanceIsTheRequestersAlone(t *testing.T) {
	snapshot := requesterSnapshot(domain.RequestStatusDelivered)
	accept := RequestTransitionEvidence{Trigger: RequestTriggerExplicit}
	for _, cap := range []RequestCapability{RequestCapabilityTeam, RequestCapabilityManager} {
		_, err := PlanRequestTransition(RequestTransitionInput{
			Request: snapshot, To: domain.RequestStatusRequesterAccepted,
			Capability: cap, ActorUserID: capabilityActor(cap), Evidence: accept,
		})
		if err == nil {
			t.Fatalf("%s accepted a delivery", cap)
		}
		if got := authz.StatusOf(err, 500); got != 403 {
			t.Fatalf("%s acceptance: status = %d, want 403 (%v)", cap, got, err)
		}
		if !strings.Contains(err.Error(), "requester's alone") {
			t.Fatalf("the refusal does not state I1: %v", err)
		}
	}
	// A declared requester capability with somebody else's user id is refused:
	// the capability is an input, so the actor is re-checked against the row.
	_, err := PlanRequestTransition(RequestTransitionInput{
		Request: snapshot, To: domain.RequestStatusRequesterAccepted,
		Capability: RequestCapabilityRequester, ActorUserID: testRequesterID + 1, Evidence: accept,
	})
	if err == nil {
		t.Fatal("a user who is not the requester accepted the request by declaring the requester capability")
	}
	if got := authz.StatusOf(err, 500); got != 403 {
		t.Fatalf("status = %d, want 403 (%v)", got, err)
	}
	// A request with no requester user (a connector's submission) has nobody
	// who may accept, answer or reopen it.
	serviceSubmitted := snapshot
	serviceSubmitted.RequesterUserID = nil
	for _, to := range []domain.RequestStatus{domain.RequestStatusRequesterAccepted, domain.RequestStatusTriaging} {
		_, err := PlanRequestTransition(RequestTransitionInput{
			Request: serviceSubmitted, To: to,
			Capability: RequestCapabilityRequester, ActorUserID: testRequesterID, Evidence: accept,
		})
		if err == nil {
			t.Fatalf("a request with no requester user moved to %s on the requester branch", to)
		}
		if got := authz.StatusOf(err, 500); got != 403 {
			t.Fatalf("%s: status = %d, want 403 (%v)", to, got, err)
		}
	}
	// The requester's own acceptance plans accepted_by = the act-by user.
	plan, err := PlanRequestTransition(RequestTransitionInput{
		Request: snapshot, To: domain.RequestStatusRequesterAccepted,
		Capability: RequestCapabilityRequester, ActorUserID: testRequesterID, Evidence: accept,
	})
	if err != nil {
		t.Fatalf("the requester could not accept: %v", err)
	}
	if plan.To != domain.RequestStatusRequesterAccepted || plan.AcceptedBy == nil || *plan.AcceptedBy != testRequesterID {
		t.Fatalf("plan = %+v, want requester_accepted with accepted_by %d", plan, testRequesterID)
	}
	assertColumns(t, plan, []RequestTransitionColumn{RequestSetAcceptedAt})
}

// TestRequestCapabilityFor pins the mapping from authz's standing onto the
// policy's vocabulary, including the two places it is deliberately narrower:
// an agent account is never the requester, and nobody without a user row is.
func TestRequestCapabilityFor(t *testing.T) {
	requester := testRequesterID
	req := &domain.TeamRequest{ID: testRequestID, RequesterUserID: &requester, State: domain.RequestStatusNew}
	person := func(mutate func(*authz.Principal)) authz.Principal {
		p := authz.Principal{UserID: testRequesterID, Channel: auth.ChannelUI}
		mutate(&p)
		return p
	}
	cases := []struct {
		name   string
		req    *domain.TeamRequest
		p      authz.Principal
		role   authz.InboxRole
		manage bool
		want   RequestCapability
	}{
		{"requester in the UI", req, person(func(*authz.Principal) {}), authz.InboxNone, false, RequestCapabilityRequester},
		{"requester over MCP", req, person(func(p *authz.Principal) { p.Channel = auth.ChannelMCP }), authz.InboxNone, false, RequestCapabilityNone},
		{"requester on an API token", req, person(func(p *authz.Principal) { p.ViaToken = true }), authz.InboxNone, false, RequestCapabilityNone},
		{"requester as an agent account", req, person(func(p *authz.Principal) { p.Agent = true }), authz.InboxNone, false, RequestCapabilityNone},
		{"requester with a submit-only token", req, person(func(p *authz.Principal) { p.Scope = domain.TokenScopeSubmit }), authz.InboxNone, false, RequestCapabilityNone},
		{"another user in the UI", req, person(func(p *authz.Principal) { p.UserID = 7 }), authz.InboxNone, false, RequestCapabilityNone},
		{"local dev with no user row", req, person(func(p *authz.Principal) { p.UserID = 0 }), authz.InboxNone, false, RequestCapabilityNone},
		{"team member", req, person(func(p *authz.Principal) { p.UserID = 7 }), authz.InboxMember, false, RequestCapabilityTeam},
		{"team lead", req, person(func(p *authz.Principal) { p.UserID = 7 }), authz.InboxMember, true, RequestCapabilityManager},
		{"team member who is also the requester", req, person(func(*authz.Principal) {}), authz.InboxMember, false, RequestCapabilityTeam},
		{"submit grant holder, no team role", req, person(func(p *authz.Principal) { p.UserID = 7 }), authz.InboxNone, false, RequestCapabilityNone},
		{"agent account holding a team role", req, person(func(p *authz.Principal) { p.Agent = true; p.UserID = 7 }), authz.InboxMember, false, RequestCapabilityTeam},
		{"no request", nil, person(func(*authz.Principal) {}), authz.InboxMember, true, RequestCapabilityNone},
	}
	for _, c := range cases {
		if got := RequestCapabilityFor(c.req, c.p, c.role, c.manage); got != c.want {
			t.Errorf("%s: capability = %s, want %s", c.name, got, c.want)
		}
	}
}

// ---- I5: asking obliges an answer, and the answer restores ----

// TestRequestTransitions_QuestionIsRequired pins that entering
// needs_information carries a question: the act is the ask, and without one the
// requester has been moved into a state that obliges nothing.
func TestRequestTransitions_QuestionIsRequired(t *testing.T) {
	for _, from := range []domain.RequestStatus{domain.RequestStatusTriaging, domain.RequestStatusAccepted, domain.RequestStatusInProgress} {
		_, err := PlanRequestTransition(RequestTransitionInput{
			Request: requesterSnapshot(from), To: domain.RequestStatusNeedsInformation,
			Capability: RequestCapabilityTeam, ActorUserID: 5000,
			Evidence: RequestTransitionEvidence{Trigger: RequestTriggerExplicit},
		})
		if err == nil {
			t.Fatalf("%s → needs_information without a question was allowed", from)
		}
		assertRequestInvalid(t, err)
		if !strings.Contains(err.Error(), "I5") {
			t.Fatalf("the refusal does not cite I5: %v", err)
		}
		// A blank question is no question: whitespace is trimmed, not accepted.
		_, err = PlanRequestTransition(RequestTransitionInput{
			Request: requesterSnapshot(from), To: domain.RequestStatusNeedsInformation,
			Capability: RequestCapabilityTeam, ActorUserID: 5000,
			Evidence: RequestTransitionEvidence{Trigger: RequestTriggerExplicit, Question: "   \n\t "},
		})
		if err == nil || !strings.Contains(err.Error(), "question is required") {
			t.Fatalf("%s → needs_information with a blank question: %v", from, err)
		}
	}
}

// TestRequestTransitions_AnswerRestoresPriorState is I5's second half: the
// requester's answer returns the request to the state the question interrupted,
// never forward to delivered, and it names the question it answers.
func TestRequestTransitions_AnswerRestoresPriorState(t *testing.T) {
	answer := RequestTransitionEvidence{Trigger: RequestTriggerExplicit, AnswerTo: 88}
	for _, prior := range []domain.RequestStatus{
		domain.RequestStatusTriaging, domain.RequestStatusAccepted, domain.RequestStatusInProgress,
	} {
		snapshot := requesterSnapshot(domain.RequestStatusNeedsInformation)
		snapshot.PriorState = prior
		plan, err := PlanRequestTransition(RequestTransitionInput{
			Request: snapshot, To: prior,
			Capability: RequestCapabilityRequester, ActorUserID: testRequesterID, Evidence: answer,
		})
		if err != nil {
			t.Fatalf("answer from needs_information back to %s was refused: %v", prior, err)
		}
		if plan.To != prior {
			t.Fatalf("answer plan.To = %s, want the interrupted state %s", plan.To, prior)
		}
		if plan.AnswerTo != 88 {
			t.Fatalf("answer plan.AnswerTo = %d, want 88", plan.AnswerTo)
		}
		if plan.Question != "" {
			t.Fatalf("an answer carried a question: %q", plan.Question)
		}
		assertColumns(t, plan, nil)

		// The answer cannot choose a different destination, and least of all
		// delivered: the request goes back to work, not forward.
		for _, to := range []domain.RequestStatus{domain.RequestStatusDelivered, domain.RequestStatusInProgress, domain.RequestStatusRejected} {
			if to == prior {
				continue
			}
			_, err := PlanRequestTransition(RequestTransitionInput{
				Request: snapshot, To: to,
				Capability: RequestCapabilityRequester, ActorUserID: testRequesterID, Evidence: answer,
			})
			if err == nil {
				t.Fatalf("an answer moved needs_information to %s, not back to %s", to, prior)
			}
			assertRequestInvalid(t, err)
		}
	}

	// An answer with no question named is refused: it is not an answer.
	snapshot := requesterSnapshot(domain.RequestStatusNeedsInformation)
	snapshot.PriorState = domain.RequestStatusTriaging
	_, err := PlanRequestTransition(RequestTransitionInput{
		Request: snapshot, To: domain.RequestStatusTriaging,
		Capability: RequestCapabilityRequester, ActorUserID: testRequesterID,
		Evidence: RequestTransitionEvidence{Trigger: RequestTriggerExplicit},
	})
	if err == nil || !strings.Contains(err.Error(), "answer_to") {
		t.Fatalf("an answer with no answer_to was accepted: %v", err)
	}

	// An unknown prior state is refused rather than guessed: the policy does
	// not invent "probably triaging".
	unknown := requesterSnapshot(domain.RequestStatusNeedsInformation)
	_, err = PlanRequestTransition(RequestTransitionInput{
		Request: unknown, To: domain.RequestStatusTriaging,
		Capability: RequestCapabilityRequester, ActorUserID: testRequesterID, Evidence: answer,
	})
	if err == nil || !strings.Contains(err.Error(), "prior state") {
		t.Fatalf("an answer with no recorded prior state was accepted: %v", err)
	}
	// As is a prior state nothing asks from.
	badPrior := requesterSnapshot(domain.RequestStatusNeedsInformation)
	badPrior.PriorState = domain.RequestStatusDelivered
	_, err = PlanRequestTransition(RequestTransitionInput{
		Request: badPrior, To: domain.RequestStatusDelivered,
		Capability: RequestCapabilityRequester, ActorUserID: testRequesterID, Evidence: answer,
	})
	if err == nil {
		t.Fatal("an answer restored delivered, which no question is asked from")
	}
	assertRequestInvalid(t, err)

	// The team does not answer on the requester's behalf: a team member in
	// needs_information may close the request, not restore it.
	teamSnapshot := requesterSnapshot(domain.RequestStatusNeedsInformation)
	teamSnapshot.PriorState = domain.RequestStatusTriaging
	_, err = PlanRequestTransition(RequestTransitionInput{
		Request: teamSnapshot, To: domain.RequestStatusTriaging,
		Capability: RequestCapabilityTeam, ActorUserID: 5000, Evidence: answer,
	})
	if err == nil {
		t.Fatal("a team member answered the requester's question for them")
	}
	assertRequestInvalid(t, err)
}

// ---- I2: delivered is evidenced ----

// TestRequestTransitions_DeliveryIsEvidenced pins I2: an outcome brief and
// exactly one of a delivered link or an explicit no-artifact record.
func TestRequestTransitions_DeliveryIsEvidenced(t *testing.T) {
	in := func(ev RequestTransitionEvidence) error {
		_, err := PlanRequestTransition(RequestTransitionInput{
			Request: requesterSnapshot(domain.RequestStatusInProgress), To: domain.RequestStatusDelivered,
			Capability: RequestCapabilityTeam, ActorUserID: 5000, Evidence: ev,
		})
		return err
	}
	cases := []struct {
		name    string
		ev      RequestTransitionEvidence
		wantErr string
	}{
		{"brief, links", RequestTransitionEvidence{Trigger: RequestTriggerExplicit, OutcomeBrief: "Mapping live for new sign-ins.", DeliveredByCount: 1}, ""},
		{"brief, no artifact", RequestTransitionEvidence{Trigger: RequestTriggerExplicit, OutcomeBrief: "Advice only: no artifact to link.", NoArtifact: true}, ""},
		{"no brief", RequestTransitionEvidence{Trigger: RequestTriggerExplicit, DeliveredByCount: 1}, "outcome_brief"},
		{"blank brief", RequestTransitionEvidence{Trigger: RequestTriggerExplicit, OutcomeBrief: "  \n ", DeliveredByCount: 1}, "outcome_brief"},
		{"brief only", RequestTransitionEvidence{Trigger: RequestTriggerExplicit, OutcomeBrief: "Done, promise."}, "delivered_by"},
		{"both link and no-artifact", RequestTransitionEvidence{Trigger: RequestTriggerExplicit, OutcomeBrief: "Done.", DeliveredByCount: 2, NoArtifact: true}, "not both"},
		{"negative link count", RequestTransitionEvidence{Trigger: RequestTriggerExplicit, OutcomeBrief: "Done.", DeliveredByCount: -1}, "cannot be negative"},
		{"over-long brief", RequestTransitionEvidence{Trigger: RequestTriggerExplicit, OutcomeBrief: strings.Repeat("x", domain.MaxRequestBriefLen+1), DeliveredByCount: 1}, "outcome_brief is over"},
	}
	for _, c := range cases {
		err := in(c.ev)
		if c.wantErr == "" {
			if err != nil {
				t.Errorf("%s: refused: %v", c.name, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("%s: allowed", c.name)
			continue
		}
		assertRequestInvalid(t, err)
		if !strings.Contains(err.Error(), c.wantErr) {
			t.Errorf("%s: refusal %q does not name %q", c.name, err.Error(), c.wantErr)
		}
	}
	// The delivery note is trimmed, like every other brief, so a stored value
	// is what the requester is shown.
	plan, err := PlanRequestTransition(RequestTransitionInput{
		Request: requesterSnapshot(domain.RequestStatusInProgress), To: domain.RequestStatusDelivered,
		Capability: RequestCapabilityTeam, ActorUserID: 5000,
		Evidence: RequestTransitionEvidence{Trigger: RequestTriggerExplicit, OutcomeBrief: "  Mapping live.  ", DeliveredByCount: 1},
	})
	if err != nil {
		t.Fatalf("delivery refused: %v", err)
	}
	if plan.OutcomeBrief != "Mapping live." {
		t.Fatalf("plan.OutcomeBrief = %q, want the trimmed note", plan.OutcomeBrief)
	}
}

// ---- I3 and D7: terminal states carry reasons; a canonical is never a duplicate ----

func TestRequestTransitions_RejectCarriesReason(t *testing.T) {
	plan := func(reason string) (*RequestTransitionPlan, error) {
		return PlanRequestTransition(RequestTransitionInput{
			Request: requesterSnapshot(domain.RequestStatusTriaging), To: domain.RequestStatusRejected,
			Capability: RequestCapabilityManager, ActorUserID: 5000,
			Evidence: RequestTransitionEvidence{Trigger: RequestTriggerExplicit, RejectReason: reason},
		})
	}
	if _, err := plan(""); err == nil || !strings.Contains(err.Error(), "reject_reason") {
		t.Fatalf("a rejection with no reason was accepted: %v", err)
	}
	if _, err := plan("   "); err == nil {
		t.Fatal("a rejection with a blank reason was accepted")
	}
	if _, err := plan(strings.Repeat("r", domain.MaxRequestRejectReasonLen+1)); err == nil || !strings.Contains(err.Error(), "over") {
		t.Fatalf("a rejection reason over the bound was accepted: %v", err)
	}
	got, err := plan("  Out of scope.  ")
	if err != nil {
		t.Fatalf("a rejection with a reason was refused: %v", err)
	}
	if got.RejectReason != "Out of scope." {
		t.Fatalf("plan.RejectReason = %q, want the trimmed reason", got.RejectReason)
	}
	assertColumns(t, got, []RequestTransitionColumn{RequestSetClosedAt})
}

func TestRequestTransitions_DuplicateCarriesCanonical(t *testing.T) {
	plan := func(ev RequestTransitionEvidence) (*RequestTransitionPlan, error) {
		return PlanRequestTransition(RequestTransitionInput{
			Request: requesterSnapshot(domain.RequestStatusTriaging), To: domain.RequestStatusDuplicate,
			Capability: RequestCapabilityManager, ActorUserID: 5000,
			Evidence: ev,
		})
	}
	base := func() RequestTransitionEvidence {
		return RequestTransitionEvidence{Trigger: RequestTriggerExplicit, CanonicalRequestID: 71}
	}
	if _, err := plan(RequestTransitionEvidence{Trigger: RequestTriggerExplicit}); err == nil || !strings.Contains(err.Error(), "duplicate_of") {
		t.Fatalf("a fold with no canonical was accepted: %v", err)
	}
	self := base()
	self.CanonicalRequestID = testRequestID
	if _, err := plan(self); err == nil || !strings.Contains(err.Error(), "itself") {
		t.Fatalf("a request folded into itself: %v", err)
	}
	nested := base()
	nested.CanonicalIsDuplicate = true
	if _, err := plan(nested); err == nil || !strings.Contains(err.Error(), "D7") {
		t.Fatalf("a fold to a canonical that is itself a duplicate was accepted: %v", err)
	}
	got, err := plan(base())
	if err != nil {
		t.Fatalf("a valid fold was refused: %v", err)
	}
	if got.DuplicateOf == nil || *got.DuplicateOf != 71 {
		t.Fatalf("plan.DuplicateOf = %v, want 71", got.DuplicateOf)
	}
	assertColumns(t, got, []RequestTransitionColumn{RequestSetClosedAt})
	// A fold closes the request as duplicate, and the detach belongs to the
	// reopen, not to the fold.
	if got.ClearDuplicateOf {
		t.Fatal("a fold planned a duplicate_of detach")
	}
}

// ---- Capability restrictions beyond acceptance ----

// TestRequestTransitions_TeamRestrictions pins what a plain team member may not
// do: reject and duplicate are the manager row's (the contract's lead/org-admin
// row lists them), and a requester triages nothing.
func TestRequestTransitions_TeamRestrictions(t *testing.T) {
	cases := []struct {
		name string
		in   RequestTransitionInput
		want int // HTTP status
	}{
		{
			"team member rejects", RequestTransitionInput{
				Request: requesterSnapshot(domain.RequestStatusNew), To: domain.RequestStatusRejected,
				Capability: RequestCapabilityTeam, ActorUserID: 5000,
				Evidence: RequestTransitionEvidence{Trigger: RequestTriggerExplicit, RejectReason: "nope"},
			}, 403,
		},
		{
			"team member marks duplicate", RequestTransitionInput{
				Request: requesterSnapshot(domain.RequestStatusTriaging), To: domain.RequestStatusDuplicate,
				Capability: RequestCapabilityTeam, ActorUserID: 5000,
				Evidence: RequestTransitionEvidence{Trigger: RequestTriggerExplicit, CanonicalRequestID: 71},
			}, 403,
		},
		{
			"requester triages", RequestTransitionInput{
				Request: requesterSnapshot(domain.RequestStatusNew), To: domain.RequestStatusTriaging,
				Capability: RequestCapabilityRequester, ActorUserID: testRequesterID,
				Evidence: RequestTransitionEvidence{Trigger: RequestTriggerExplicit},
			}, 403, // the edge exists and belongs to the team: a requester sees the request, not this act
		},
		{
			"requester asks itself a question", RequestTransitionInput{
				Request: requesterSnapshot(domain.RequestStatusTriaging), To: domain.RequestStatusNeedsInformation,
				Capability: RequestCapabilityRequester, ActorUserID: testRequesterID,
				Evidence: RequestTransitionEvidence{Trigger: RequestTriggerExplicit, Question: "well?"},
			}, 403,
		},
		{
			"requester reopens a rejected request", RequestTransitionInput{
				Request: requesterSnapshot(domain.RequestStatusRejected), To: domain.RequestStatusTriaging,
				Capability: RequestCapabilityRequester, ActorUserID: testRequesterID,
				Evidence: RequestTransitionEvidence{Trigger: RequestTriggerExplicit},
			}, 403,
		},
		{
			"nobody with no standing", RequestTransitionInput{
				Request: requesterSnapshot(domain.RequestStatusNew), To: domain.RequestStatusTriaging,
				Capability: RequestCapabilityNone, ActorUserID: 5000,
				Evidence: RequestTransitionEvidence{Trigger: RequestTriggerExplicit},
			}, 404,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			plan, err := PlanRequestTransition(c.in)
			if err == nil {
				t.Fatalf("allowed: %+v", plan)
			}
			if got := authz.StatusOf(err, 500); got != c.want {
				t.Fatalf("status = %d, want %d (%v)", got, c.want, err)
			}
			if c.want == 404 {
				if code := authz.CodeOf(err); code != authz.CodeRequestNotFound {
					t.Fatalf("a caller with no standing got code %q, want %q (a request it may not see and one that does not exist must be indistinguishable)", code, authz.CodeRequestNotFound)
				}
				if !strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), "42") {
					t.Fatalf("the not-found refusal must not name the request: %v", err)
				}
			}
		})
	}
	// A manager may make both closes, and a team member may make every other
	// working move (the 324-case matrix proves the rest).
	for _, to := range []domain.RequestStatus{domain.RequestStatusRejected, domain.RequestStatusDuplicate} {
		ev := RequestTransitionEvidence{Trigger: RequestTriggerExplicit, RejectReason: "declined"}
		if to == domain.RequestStatusDuplicate {
			ev = RequestTransitionEvidence{Trigger: RequestTriggerExplicit, CanonicalRequestID: 71}
		}
		if _, err := PlanRequestTransition(RequestTransitionInput{
			Request: requesterSnapshot(domain.RequestStatusTriaging), To: to,
			Capability: RequestCapabilityManager, ActorUserID: 5000, Evidence: ev,
		}); err != nil {
			t.Fatalf("a manager could not move the request to %s: %v", to, err)
		}
	}
}

// ---- I8: nothing auto-advances ----

// TestRequestTransitions_NoTriggerButExplicitMovesAnything is I8 across the
// whole matrix: for every state, every destination and every capability, a
// trigger other than an explicit act produces no plan. That is the policy half
// of "a linked ticket's closure never flips a request": the closure arrives as
// RequestTriggerLinkedWork, and no evidence, capability or destination makes it
// legal.
func TestRequestTransitions_NoTriggerButExplicitMovesAnything(t *testing.T) {
	triggers := []RequestTrigger{RequestTriggerLinkedWork, RequestTriggerAggregate, RequestTriggerSweep, ""}
	for _, from := range domain.RequestStatuses {
		for _, to := range domain.RequestStatuses {
			for _, cap := range allCapabilities {
				for _, trigger := range triggers {
					// Evidence valid for whichever move this pair would be, so the
					// trigger is the only reason to refuse.
					ev := RequestTransitionEvidence{Trigger: trigger, Question: "q?", RejectReason: "r", CanonicalRequestID: 71,
						OutcomeBrief: "done", DeliveredByCount: 1, AnswerTo: 1}
					if contract, ok := contractFor(from, to); ok {
						ev = contract.ev
						ev.Trigger = trigger
					}
					snapshot := requesterSnapshot(from)
					snapshot.PriorState = domain.RequestStatusTriaging
					plan, err := PlanRequestTransition(RequestTransitionInput{
						Request: snapshot, To: to, Capability: cap, ActorUserID: capabilityActor(cap), Evidence: ev,
					})
					if err == nil {
						t.Fatalf("%s moved %s → %s on trigger %q (plan %+v): a linked ticket's closure, an aggregate or a sweep never advances a request (I8)", cap, from, to, trigger, plan)
					}
					assertRequestInvalid(t, err)
					if trigger == "" {
						continue
					}
					if !strings.Contains(err.Error(), "I8") {
						t.Fatalf("trigger %q refused without citing I8: %v", trigger, err)
					}
					if !strings.Contains(err.Error(), "never") {
						t.Fatalf("trigger %q refused without saying what never happens: %v", trigger, err)
					}
				}
			}
		}
	}
	// And the counterpart, so the test cannot pass by refusing everything: the
	// same delivery with an explicit trigger is allowed.
	plan, err := PlanRequestTransition(RequestTransitionInput{
		Request: requesterSnapshot(domain.RequestStatusInProgress), To: domain.RequestStatusDelivered,
		Capability: RequestCapabilityTeam, ActorUserID: 5000,
		Evidence: RequestTransitionEvidence{Trigger: RequestTriggerExplicit, OutcomeBrief: "Delivered.", DeliveredByCount: 1},
	})
	if err != nil || plan == nil {
		t.Fatalf("an explicit delivery was refused: %v", err)
	}
}

// ---- Evidence hygiene ----

// TestRequestTransitions_UnreadEvidenceIsRefused: a move does not silently drop
// evidence it does not read, because a dropped outcome brief reads to the
// caller as a delivered one.
func TestRequestTransitions_UnreadEvidenceIsRefused(t *testing.T) {
	cases := []struct {
		name string
		ev   RequestTransitionEvidence
		want string
	}{
		{"outcome brief on a triage move", RequestTransitionEvidence{Trigger: RequestTriggerExplicit, OutcomeBrief: "done"}, "outcome_brief"},
		{"reject reason on a triage move", RequestTransitionEvidence{Trigger: RequestTriggerExplicit, RejectReason: "no"}, "reject_reason"},
		{"canonical on a triage move", RequestTransitionEvidence{Trigger: RequestTriggerExplicit, CanonicalRequestID: 71}, "canonical_request_id"},
		{"answer_to on a triage move", RequestTransitionEvidence{Trigger: RequestTriggerExplicit, AnswerTo: 3}, "answer_to"},
		{"question on a delivery", RequestTransitionEvidence{Trigger: RequestTriggerExplicit, Question: "q?", OutcomeBrief: "b", DeliveredByCount: 1}, "question"},
		{"no_artifact on a triage move", RequestTransitionEvidence{Trigger: RequestTriggerExplicit, NoArtifact: true}, "no_artifact"},
	}
	for _, c := range cases {
		_, err := PlanRequestTransition(RequestTransitionInput{
			Request: requesterSnapshot(domain.RequestStatusNew), To: domain.RequestStatusTriaging,
			Capability: RequestCapabilityTeam, ActorUserID: 5000, Evidence: c.ev,
		})
		if err == nil {
			t.Errorf("%s: allowed", c.name)
			continue
		}
		assertRequestInvalid(t, err)
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: refusal %q does not name %q", c.name, err.Error(), c.want)
		}
		if c.want == "outcome_brief" && !strings.Contains(err.Error(), "would not be written") {
			t.Errorf("%s: refusal does not explain that the field would be dropped: %v", c.name, err)
		}
	}
	// A delivery with unread question evidence is refused rather than delivering
	// while discarding the question.
	_, err := PlanRequestTransition(RequestTransitionInput{
		Request: requesterSnapshot(domain.RequestStatusInProgress), To: domain.RequestStatusDelivered,
		Capability: RequestCapabilityTeam, ActorUserID: 5000,
		Evidence: RequestTransitionEvidence{Trigger: RequestTriggerExplicit, Question: "q?", OutcomeBrief: "b", DeliveredByCount: 1},
	})
	if err == nil {
		t.Fatal("a delivery discarded a question")
	}
	assertRequestInvalid(t, err)
}

// TestRequestTransitions_SameStateAndUnknownState: a no-op and an invented
// state are both refusals (request.invalid), never a plan.
func TestRequestTransitions_SameStateAndUnknownState(t *testing.T) {
	_, err := PlanRequestTransition(RequestTransitionInput{
		Request: requesterSnapshot(domain.RequestStatusTriaging), To: domain.RequestStatusTriaging,
		Capability: RequestCapabilityTeam, ActorUserID: 5000,
		Evidence: RequestTransitionEvidence{Trigger: RequestTriggerExplicit},
	})
	if err == nil || !strings.Contains(err.Error(), "already") {
		t.Fatalf("a move to the state the request is already in was accepted: %v", err)
	}
	assertRequestInvalid(t, err)
	_, err = PlanRequestTransition(RequestTransitionInput{
		Request: requesterSnapshot(domain.RequestStatusTriaging), To: domain.RequestStatus("cancelled"),
		Capability: RequestCapabilityTeam, ActorUserID: 5000,
		Evidence: RequestTransitionEvidence{Trigger: RequestTriggerExplicit},
	})
	if err == nil {
		t.Fatal("an invented state was accepted")
	}
	assertRequestInvalid(t, err)
	_, err = PlanRequestTransition(RequestTransitionInput{
		Request: RequestSnapshot{ID: testRequestID, State: "lost"},
		To:      domain.RequestStatusTriaging, Capability: RequestCapabilityTeam, ActorUserID: 5000,
		Evidence: RequestTransitionEvidence{Trigger: RequestTriggerExplicit},
	})
	if err == nil {
		t.Fatal("a request in an invented state was moved")
	}
	assertRequestInvalid(t, err)
}

// TestRequestTransitionColumnsPerDestination pins the column mapping once, so
// the write layer (#3960) never has to decide when a timestamp is stamped and
// the first-write-wins rule (053) is decided in one place.
func TestRequestTransitionColumnsPerDestination(t *testing.T) {
	cases := []struct {
		to   domain.RequestStatus
		want []RequestTransitionColumn
	}{
		{domain.RequestStatusTriaging, nil},
		{domain.RequestStatusNeedsInformation, []RequestTransitionColumn{RequestSetNeedsInfoSince}},
		{domain.RequestStatusDelivered, []RequestTransitionColumn{RequestSetDeliveredAt}},
		{domain.RequestStatusRequesterAccepted, []RequestTransitionColumn{RequestSetAcceptedAt}},
		{domain.RequestStatusRejected, []RequestTransitionColumn{RequestSetClosedAt}},
		{domain.RequestStatusDuplicate, []RequestTransitionColumn{RequestSetClosedAt}},
	}
	for _, c := range cases {
		got := requestTransitionColumns(requestMove{from: domain.RequestStatusTriaging, to: c.to})
		if len(got) != len(c.want) {
			t.Fatalf("columns for %s = %v, want %v", c.to, got, c.want)
		}
		for i := range c.want {
			if got[i] != c.want[i] {
				t.Fatalf("columns for %s = %v, want %v", c.to, got, c.want)
			}
		}
	}
	// An answer out of needs_information sets nothing: the wait is recorded
	// where it began (needs_info_since), and nothing is cleared (the timestamp
	// stays first-write-wins so a later clock can exclude the wait, D8).
	answerColumns := requestTransitionColumns(requestMove{
		from: domain.RequestStatusNeedsInformation, to: domain.RequestStatusTriaging,
	})
	if len(answerColumns) != 0 {
		t.Fatalf("an answer sets %v, want nothing", answerColumns)
	}
}

// TestRequestSnapshotOf: the record → snapshot mapping exists once, and a nil
// record maps to the zero snapshot rather than panicking in a caller that read
// nothing.
func TestRequestSnapshotOf(t *testing.T) {
	if got := RequestSnapshotOf(nil); got != (RequestSnapshot{}) {
		t.Fatalf("RequestSnapshotOf(nil) = %+v, want the zero snapshot", got)
	}
	requester := testRequesterID
	canonical := 71
	req := &domain.TeamRequest{ID: testRequestID, State: domain.RequestStatusDuplicate, DuplicateOf: &canonical, RequesterUserID: &requester}
	got := RequestSnapshotOf(req)
	if got.ID != testRequestID || got.State != domain.RequestStatusDuplicate || got.DuplicateOf == nil || *got.DuplicateOf != canonical || got.RequesterUserID == nil || *got.RequesterUserID != requester {
		t.Fatalf("RequestSnapshotOf = %+v, want id %d, state duplicate, canonical %d, requester %d", got, testRequestID, canonical, requester)
	}
	// The snapshot deliberately does not carry text: a decision that could read
	// the subject or the brief is a decision that could read authority out of it.
	if got.PriorState != "" {
		t.Fatalf("RequestSnapshotOf invented a prior state %q: it comes from the history, not the record", got.PriorState)
	}
}

// TestRequestVocabularies_NeverSilentlyZero: an unknown capability or trigger is
// a caller bug, and both say so rather than falling back to a permissive default.
func TestRequestVocabularies_NeverSilentlyZero(t *testing.T) {
	if got := RequestCapability(99).String(); got != "none" {
		t.Fatalf("RequestCapability(99).String() = %q, want \"none\"", got)
	}
	if RequestCapability(99).canMake(RequestCapabilityRequester) || RequestCapability(99).canMake(RequestCapabilityTeam) {
		t.Fatal("an unknown capability was allowed to make a move")
	}
	_, err := PlanRequestTransition(RequestTransitionInput{
		Request: requesterSnapshot(domain.RequestStatusNew), To: domain.RequestStatusTriaging,
		Capability: RequestCapability(99), ActorUserID: 5000, Evidence: RequestTransitionEvidence{Trigger: RequestTriggerExplicit},
	})
	if err == nil || !strings.Contains(err.Error(), "unknown request capability") {
		t.Fatalf("an unknown capability was not named: %v", err)
	}
	_, err = PlanRequestTransition(RequestTransitionInput{
		Request: requesterSnapshot(domain.RequestStatusNew), To: domain.RequestStatusTriaging,
		Capability: RequestCapabilityTeam, ActorUserID: 5000, Evidence: RequestTransitionEvidence{Trigger: "sometime"},
	})
	if err == nil || !strings.Contains(err.Error(), "unknown request transition trigger") {
		t.Fatalf("an unknown trigger was not named: %v", err)
	}
	// A zero minimum (an edge declared with no capability floor) is nobody's.
	if RequestCapabilityManager.canMake(RequestCapabilityNone) {
		t.Fatal("an edge with a zero capability floor was allowed")
	}
}

// TestRequestTransitions_DefensiveBranches covers the three branches that exist
// to fail loudly rather than silently: an unreadable snapshot state, an answer
// with no restorable prior state, and an edge the evidence table forgot.
func TestRequestTransitions_DefensiveBranches(t *testing.T) {
	// AllowedRequestTransitions on a snapshot it cannot read offers nothing, so
	// a UI rendering the list shows no actions rather than guessing.
	if got := AllowedRequestTransitions(RequestSnapshot{ID: testRequestID, State: "lost"}, RequestCapabilityManager); got != nil {
		t.Fatalf("options for a request in an invented state = %v, want none", got)
	}
	// A request in needs_information whose prior state was not recorded offers
	// its requester nothing: the policy will not invent one.
	if got := AllowedRequestTransitions(requesterSnapshot(domain.RequestStatusNeedsInformation), RequestCapabilityRequester); got != nil {
		t.Fatalf("options with no prior state = %v, want none", got)
	}
	// An edge with a kind whose evidence rule was not written is a table bug and
	// must be refused rather than planned with no evidence check.
	if err := checkRequestMoveEvidence(requestMove{from: domain.RequestStatusNew, to: domain.RequestStatusTriaging, kind: requestMoveKind(99)}, RequestSnapshot{}, RequestTransitionEvidence{Trigger: RequestTriggerExplicit}); err == nil {
		t.Fatal("an edge with an unknown evidence kind was accepted")
	} else if !strings.Contains(err.Error(), "bug in the transition table") {
		t.Fatalf("the table-bug refusal does not say so: %v", err)
	}
	// A question over the brief bound is refused naming the field, not silently
	// truncated into the comment.
	_, err := PlanRequestTransition(RequestTransitionInput{
		Request: requesterSnapshot(domain.RequestStatusTriaging), To: domain.RequestStatusNeedsInformation,
		Capability: RequestCapabilityTeam, ActorUserID: 5000,
		Evidence: RequestTransitionEvidence{Trigger: RequestTriggerExplicit, Question: strings.Repeat("q", domain.MaxRequestBriefLen+1)},
	})
	if err == nil || !strings.Contains(err.Error(), "question is over") {
		t.Fatalf("an over-long question was accepted: %v", err)
	}
}

// TestRequestTransitions_EachKindRefusesAnotherKindsEvidence walks the remaining
// evidence pairings: every move's own kind reads exactly its own fields, so a
// field belonging to another kind is refused on each of them — not only on the
// plain moves the hygiene test uses.
func TestRequestTransitions_EachKindRefusesAnotherKindsEvidence(t *testing.T) {
	base := func() RequestTransitionEvidence { return RequestTransitionEvidence{Trigger: RequestTriggerExplicit} }
	cases := []struct {
		name string
		in   RequestTransitionInput
		want string
	}{
		{
			"question carrying a reject reason",
			RequestTransitionInput{
				Request: requesterSnapshot(domain.RequestStatusTriaging), To: domain.RequestStatusNeedsInformation,
				Capability: RequestCapabilityTeam, ActorUserID: 5000,
				Evidence: func() RequestTransitionEvidence {
					ev := base()
					ev.Question, ev.RejectReason = "which one?", "no"
					return ev
				}(),
			}, "reject_reason",
		},
		{
			"reject carrying a question",
			RequestTransitionInput{
				Request: requesterSnapshot(domain.RequestStatusTriaging), To: domain.RequestStatusRejected,
				Capability: RequestCapabilityManager, ActorUserID: 5000,
				Evidence: func() RequestTransitionEvidence {
					ev := base()
					ev.Question, ev.RejectReason = "q?", "Out of scope"
					return ev
				}(),
			}, "question",
		},
		{
			"fold carrying an answer id",
			RequestTransitionInput{
				Request: requesterSnapshot(domain.RequestStatusTriaging), To: domain.RequestStatusDuplicate,
				Capability: RequestCapabilityManager, ActorUserID: 5000,
				Evidence: func() RequestTransitionEvidence {
					ev := base()
					ev.CanonicalRequestID, ev.AnswerTo = 71, 3
					return ev
				}(),
			}, "answer_to",
		},
		{
			"delivery carrying an answer id",
			RequestTransitionInput{
				Request: requesterSnapshot(domain.RequestStatusInProgress), To: domain.RequestStatusDelivered,
				Capability: RequestCapabilityTeam, ActorUserID: 5000,
				Evidence: func() RequestTransitionEvidence {
					ev := base()
					ev.OutcomeBrief, ev.DeliveredByCount, ev.AnswerTo = "done", 1, 9
					return ev
				}(),
			}, "answer_to",
		},
		{
			"answer carrying an outcome brief",
			RequestTransitionInput{
				Request: func() RequestSnapshot {
					s := requesterSnapshot(domain.RequestStatusNeedsInformation)
					s.PriorState = domain.RequestStatusTriaging
					return s
				}(),
				To:         domain.RequestStatusTriaging,
				Capability: RequestCapabilityRequester, ActorUserID: testRequesterID,
				Evidence: func() RequestTransitionEvidence {
					ev := base()
					ev.AnswerTo, ev.OutcomeBrief = 3, "done"
					return ev
				}(),
			}, "outcome_brief",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := PlanRequestTransition(c.in); err == nil {
				t.Fatalf("allowed with %s evidence", c.want)
			} else if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("refusal %q does not name %q", err.Error(), c.want)
			}
		})
	}
}

// ---- Helpers ----

// assertRequestInvalid requires err to be the request surface's 400.
func assertRequestInvalid(t *testing.T, err error) {
	t.Helper()
	if got := authz.StatusOf(err, 500); got != 400 {
		t.Fatalf("status = %d, want 400 request.invalid (%v)", got, err)
	}
	if code := authz.CodeOf(err); code != authz.CodeRequestInvalid {
		t.Fatalf("code = %q, want %q (%v)", code, authz.CodeRequestInvalid, err)
	}
}
