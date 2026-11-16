package service

// Team requests: the outcome aggregate and the delivery it gates (epic 202, story
// 549, AMP #3966; build slice E2 of
// discoveries/team-inbox-approved-build-slices.md; approved product contract
// artifact 36 v2 §3, §4; the outcome contract in
// discoveries/team-request-planning-lineage.md).
//
// A request's status is not a ticket's status, and the whole point of this file
// is that the difference is structural rather than a convention someone
// remembers. Two of the contract's invariants meet here:
//
//   I8. Nothing about linked work may move a request. So this aggregate never
//       writes: no state, no rev, no event. It reports, and its report is what
//       every view reads — the team's rail, the requester's stepper, a queue
//       cell — so those views cannot disagree (the WaitingOn rule).
//
//   I2. Delivery is a statement the team makes with evidence, not a conclusion
//       the server draws from "all the tickets are done". So DeliveredEligible
//       requires the request's OWN state to be delivered (or accepted) AND its
//       brief AND its evidence. A request whose five linked tickets are all
//       completed is Reported() as "all linked work is done, not delivered":
//       true, useful, and explicitly not delivery.
//
// # Two halves, one definition
//
// BuildRequestOutcome is pure: it takes the typed facts (a checkpoint on each
// linked target, its tombstone, a receipt's checks, a ticket's runs) and returns
// the aggregate. Every case the ticket asks to be "honest" — held, gated,
// deleted, partial — is therefore a table test with no database in the way.
//
// Service.RequestOutcome is the thin read that RESOLVES those facts
// (repository.RequestOutcomeFactsFor, tenant-scoped) and hands them to the pure
// function. It is the one place a view should call; the pure function is exported
// so a test, and the requester projection's caller, can build the same words from
// facts they already have.
//
// # What it reads, and what it refuses to read
//
// The aggregate reads only "is this done, gated, held, verified, gone". It never
// reads a name, a description, an assignee, an owner, a branch, a comment, a
// token count or a metadata key. That is not tidiness: the requester projection is
// built from this aggregate (RequesterMilestonesFor), so a field the aggregate
// carries is a field that can leak. The notes it emits are made of counts and the
// contract's own words — "2 of 3 items are still in progress" — never of anything
// internal.
//
// # Delivery
//
// DeliverRequest is the team stating the outcome exists. It is the lifecycle's
// write (#3960's TransitionRequest, one atomic state + rev + event), and this file
// adds the only thing that write cannot know: whether the evidence the aggregate
// can SEE agrees with the evidence the caller is asserting. A delivery is accepted
// only when the aggregate's own DeliveredEligible is true for the same facts —
// one definition, checked twice, and a disagreement fails rather than being
// silently reported as delivered.
//
// There is deliberately no endpoint, no MCP tool and no UI here (F3/F4/J2 are
// their own slices). And there is deliberately no path by which this file accepts
// an outcome on a requester's behalf: explicit acceptance stays the lifecycle's
// requester-only, web-UI-only act (I1, D5), and this file cannot reach it.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/repository"
)

// ---- The vocabulary ----
//
// The phases are the aggregate's own words, and the set is closed for the same
// reason domain.RequestStatuses is: a view renders what it is given, and a phase
// this vocabulary does not have is a bug it should be able to see rather than a
// word it invents.
type RequestPhase string

const (
	// RequestPhaseUnlinked: the request has no linked work at all. It is a
	// request, not an empty project — nothing has been planned for it yet.
	RequestPhaseUnlinked RequestPhase = "unlinked"
	// RequestPhaseNotStarted: linked work exists and none of it has begun.
	RequestPhaseNotStarted RequestPhase = "not_started"
	// RequestPhaseInProgress: at least one piece is underway, or some are done
	// and others are not.
	RequestPhaseInProgress RequestPhase = "in_progress"
	// RequestPhaseBlocked: something is waiting on a person (a question on a
	// ticket, a held receipt, a merge a person must make). The word for the team
	// rail: "there is a person in the way".
	RequestPhaseBlocked RequestPhase = "blocked"
	// RequestPhaseDelivered: the request itself is delivered (or accepted) with
	// its evidence. The only phase that says the outcome exists.
	RequestPhaseDelivered RequestPhase = "delivered"
	// RequestPhaseClosed: the request is rejected or folded into a duplicate.
	// Its linked work does not make it delivered.
	RequestPhaseClosed RequestPhase = "closed"
)

// RequestOutcomeItemKind is what one delivered_by target is. It is the same
// closed set the contract allows as delivery evidence (D10: a task, an artifact
// or a receipt), and nothing else can appear here.
type RequestOutcomeItemKind string

const (
	RequestOutcomeTask     RequestOutcomeItemKind = "task"
	RequestOutcomeArtifact RequestOutcomeItemKind = "artifact"
	RequestOutcomeReceipt  RequestOutcomeItemKind = "receipt"
)

// RequestOutcomeItemState is one piece of evidence's honest state. Exactly one
// value per item, in the order of "how far from delivered" — the aggregate's
// whole job is to pick the right one rather than the flattering one.
type RequestOutcomeItemState string

const (
	// RequestOutcomeOpen: not started, or underway, nothing in its way.
	RequestOutcomeOpen RequestOutcomeItemState = "open"
	// RequestOutcomeBlocked: waiting on a person's answer (a ticket asked a
	// question).
	RequestOutcomeBlocked RequestOutcomeItemState = "blocked"
	// RequestOutcomeDone: the work is finished and owes nothing more.
	RequestOutcomeDone RequestOutcomeItemState = "done"
	// RequestOutcomeMergeGated: finished, but its work still waits on a merge
	// (needed or abandoned: MergeGates is true for both, and an abandoned gate
	// survives until a person says it is not needed).
	RequestOutcomeMergeGated RequestOutcomeItemState = "merge_gated"
	// RequestOutcomeHeld: finished, but a verification receipt is held for a
	// person ("pending verify").
	RequestOutcomeHeld RequestOutcomeItemState = "held"
	// RequestOutcomeDeleted: the target no longer exists. A re-planned slice,
	// never silence and never done.
	RequestOutcomeDeleted RequestOutcomeItemState = "deleted"
	// RequestOutcomeUnverified: an artifact whose review has not approved the
	// version that is current. It exists; it is not agreed to yet.
	RequestOutcomeUnverified RequestOutcomeItemState = "unverified"
)

// RequestOutcomeItem is one piece of linked evidence, as the aggregate reports
// it. Kind and ID are ids: internal to the team rail, and never emitted on the
// requester side (the projection counts them and drops them).
type RequestOutcomeItem struct {
	Kind  RequestOutcomeItemKind  `json:"kind"`
	ID    int                     `json:"id"`
	State RequestOutcomeItemState `json:"state"`
	// MergeState is set only for a merge-gated task, so a rail can say "PR
	// closed without merging" instead of "waiting on a merge" for both.
	MergeState string `json:"merge_state,omitempty"`
	// DeletedAt is the tombstone date when the target is gone and the trash
	// still remembers it (deleted_tasks keeps 30 days); empty otherwise.
	DeletedAt string `json:"deleted_at,omitempty"`
	// VerificationStatus is a receipt's derived status (domain.StatusRank's
	// ladder), set only for a receipt.
	VerificationStatus string `json:"verification_status,omitempty"`
	// Rework is how many of the piece's runs died or were stopped. Reported on
	// the team rail, counted in the projection's note, never named.
	Rework int `json:"rework,omitempty"`
}

// RequestOutcome is the aggregate: what the linked work actually amounts to,
// honestly, plus the delivery gate.
//
// Every count is derived from Items, so a caller can render the counts and the
// list and they cannot disagree. DeliveredEligible is the one decision that is
// not a count, and it is deliberately NOT "Done == Total" (see BuildRequestOutcome).
type RequestOutcome struct {
	// Phase is the one word for the team rail.
	Phase RequestPhase `json:"phase"`
	// Items is every delivered_by target, in the order the links were written.
	Items []RequestOutcomeItem `json:"items"`

	// Total / Done / Open / Blocked / Gated / Held / Deleted / Unverified count
	// the items by state. Total is len(Items).
	Total      int `json:"total"`
	Done       int `json:"done"`
	Open       int `json:"open"`
	Blocked    int `json:"blocked"`
	Gated      int `json:"gated"`
	Held       int `json:"held"`
	Deleted    int `json:"deleted"`
	Unverified int `json:"unverified"`
	// Rework is the sum of the items' abandoned runs: runs that died or were
	// stopped. "5 of 5 done" with 3 abandoned runs is not the same delivery as
	// one without, and the count is how the difference is stated.
	Rework int `json:"rework"`

	// Linked counts the request's other lineage: planned_as epics/stories/
	// tickets and the one routes_to project. It is "is this request planned at
	// all", separate from "is its work evidence of delivery".
	Linked int `json:"linked"`
	// RoutedProjectID is the request's active routes_to target, 0 for none. It
	// is an id the team rail shows and the projection never does.
	RoutedProjectID int `json:"routed_project_id,omitempty"`

	// OutcomeBriefSet says the request carries the delivery note (I2's first
	// half). Kept apart from the brief's text: this type is safe to log.
	OutcomeBriefSet bool `json:"outcome_brief_set"`
	// NoArtifact says the delivery was the explicit "there is nothing to link"
	// record (I2's second half), rather than links.
	NoArtifact bool `json:"no_artifact"`

	// DeliveredEligible is the delivery gate for THESE facts: the request's own
	// state reached delivered/requester_accepted, its brief is set, and it has
	// either verified delivered_by links or the explicit no-artifact record.
	// Nothing about the tickets' states can make it true.
	DeliveredEligible bool `json:"delivered_eligible"`
	// BlockedReason says, in the contract's words, what is missing before a
	// delivery could be made — nil when DeliveredEligible is true or the request
	// is already delivered. It is what a Deliver panel disables its button on
	// (E2's "do not look to be stuck" rule), never a refusal of its own.
	BlockedReason string `json:"blocked_reason,omitempty"`
}

// ---- The pure aggregate ----

// RequestOutcomeInput is the aggregate's whole world: the request as stored, its
// links, and the resolved checkpoint of each link target. A caller builds it from
// repository.RequestOutcomeFacts (Service.RequestOutcome does) or from literals
// (a test, or a view that already has the facts).
type RequestOutcomeInput struct {
	Request *domain.TeamRequest
	Links   []domain.RequestLink
	Targets []repository.RequestOutcomeLinkTarget
	// Runs is the per-ticket run facts (abandoned runs are rework), one entry per
	// ticket the aggregate reports on.
	Runs []repository.RequestOutcomeRunFacts
	// NoArtifact is the explicit "the outcome exists and there is nothing to
	// link" record. It is passed in rather than derived, because it is a
	// statement about an act (a delivery), not about the targets.
	NoArtifact bool
}

// BuildRequestOutcome is the one definition of what a request's linked work
// amounts to. It is pure: no clock, no database, no principal, and no
// authorization decision — who may see this is decided before it runs.
//
// The rules, in the order they matter:
//
//  1. Delivery is the request's own act. DeliveredEligible is false unless the
//     state itself is delivered/requester_accepted. A request whose linked
//     tickets are all Done is reported as such (Phase in_progress, all items
//     done) and is explicitly NOT delivered: that is I8, and it is why this
//     function is not `Done == Total`.
//  2. A deleted target is never done. It is a re-planned slice: the plan moved
//     and the lineage stays readable (D10's tombstone).
//  3. Merge gates and held receipts are "done but not delivered": they count as
//     neither open work nor available evidence for the gate, and they are what
//     turns the phase to blocked (a person must act).
//  4. A question on a ticket is a person in the way (I5's shape), so it is
//     blocked too, and the request's phase says a person is in the way rather
//     than showing a cheerful progress count.
//  5. Evidence for the gate is evidence that exists and is not deleted. An
//     artifact in review is evidence (the outcome exists) but not a done piece;
//     that distinction is the artifact's state, not the gate's.
func BuildRequestOutcome(in RequestOutcomeInput) *RequestOutcome {
	r := in.Request
	if r == nil {
		return nil
	}
	out := &RequestOutcome{OutcomeBriefSet: strings.TrimSpace(r.OutcomeBrief) != "", NoArtifact: in.NoArtifact}
	targets := make(map[string]repository.RequestOutcomeLinkTarget, len(in.Targets))
	for _, t := range in.Targets {
		targets[requestOutcomeTargetKey(t.Kind, t.ID)] = t
	}
	runs := make(map[int]repository.RequestOutcomeRunFacts, len(in.Runs))
	for _, f := range in.Runs {
		runs[f.TaskID] = f
	}

	// Lineage first: routed / planned / duplicate, which is "is this request
	// being worked at all" and is not evidence of delivery.
	for _, l := range in.Links {
		switch l.LinkKind {
		case domain.RequestLinkRoutesTo:
			if out.RoutedProjectID == 0 {
				out.RoutedProjectID = l.TargetID
			}
		case domain.RequestLinkPlannedAs:
			out.Linked++
		}
	}

	seen := map[string]bool{}
	for _, l := range in.Links {
		if l.LinkKind != domain.RequestLinkDeliveredBy {
			continue
		}
		key := requestOutcomeTargetKey(l.TargetKind, l.TargetID)
		if seen[key] {
			continue
		}
		seen[key] = true
		t, ok := targets[key]
		if !ok {
			// A delivered_by link to a kind that cannot be delivery evidence
			// (the schema's CHECK forbids it, D10) is reported as deleted rather
			// than counted: the aggregate must never turn an unrecognised row
			// into "something was delivered".
			t = repository.RequestOutcomeLinkTarget{Kind: l.TargetKind, ID: l.TargetID, Deleted: true}
		}
		item := requestOutcomeItem(t, runs)
		out.Items = append(out.Items, item)
		out.Rework += item.Rework
		switch item.State {
		case RequestOutcomeDone:
			out.Done++
		case RequestOutcomeOpen:
			out.Open++
		case RequestOutcomeBlocked:
			out.Blocked++
		case RequestOutcomeMergeGated:
			out.Gated++
		case RequestOutcomeHeld:
			out.Held++
		case RequestOutcomeDeleted:
			out.Deleted++
		case RequestOutcomeUnverified:
			out.Unverified++
		}
	}
	out.Total = len(out.Items)

	// The gate (I2). The request's own state is the first term and nothing else
	// can substitute for it.
	deliveredState := r.State == domain.RequestStatusDelivered || r.State == domain.RequestStatusRequesterAccepted
	hasEvidence := out.Done+out.Unverified+out.Gated+out.Held > 0 || in.NoArtifact
	switch {
	case !deliveredState && (r.State == domain.RequestStatusRejected || r.State == domain.RequestStatusDuplicate):
		out.Phase = RequestPhaseClosed
	case deliveredState:
		out.Phase = RequestPhaseDelivered
		out.DeliveredEligible = out.OutcomeBriefSet && hasEvidence
	case out.Total == 0 && out.Linked == 0:
		out.Phase = RequestPhaseUnlinked
	case out.Blocked > 0 || out.Held > 0 || out.Gated > 0:
		out.Phase = RequestPhaseBlocked
	case out.Total == 0 || (out.Open == out.Total && out.Deleted == 0):
		// Work has been planned (or not) but nothing has produced an outcome yet:
		// every piece is open, or there is no evidence piece at all. ("Only
		// tombstones" is deliberately not here: something was done and re-planned,
		// which is in progress, not untouched.)
		out.Phase = RequestPhaseNotStarted
	default:
		// Some work is done or reviewable and some is not; or all of it is done
		// and the team has not delivered. Both are "in progress" to the team,
		// and the second one is the case I8 exists for.
		out.Phase = RequestPhaseInProgress
	}
	if !out.DeliveredEligible && out.Phase != RequestPhaseDelivered && out.Phase != RequestPhaseClosed {
		out.BlockedReason = requestOutcomeBlockedReason(r, out)
	}
	return out
}

// requestOutcomeItem decides one target's state from its own row, never from the
// link. A receipt's state folds its ticket's state in, because a receipt on a
// ticket whose work is merge-gated is finished but not delivered either.
func requestOutcomeItem(t repository.RequestOutcomeLinkTarget, runs map[int]repository.RequestOutcomeRunFacts) RequestOutcomeItem {
	item := RequestOutcomeItem{ID: t.ID, DeletedAt: valueOrEmpty(t.DeletedAt)}
	switch t.Kind {
	case domain.RequestTargetTask:
		item.Kind = RequestOutcomeTask
		item.Rework = runs[t.ID].Abandoned
		item.MergeState = t.MergeState
		switch {
		case t.Deleted:
			item.State = RequestOutcomeDeleted
		case t.TaskState == domain.TaskStateBlocked && t.TaskBlocked != "" && !strings.HasPrefix(t.TaskBlocked, "scheduled:"):
			// Blocked on a question: someone must answer before it moves (I5).
			// A "scheduled:" reason is the scheduler holding it, not a person.
			item.State = RequestOutcomeBlocked
		case t.TaskState != domain.TaskStateCompleted:
			// Backlog, in progress (started or not), or blocked on a dependency
			// it will be released from: open work, and the started/not-started
			// distinction is the task's own Phase, not a second state here.
			item.State = RequestOutcomeOpen
		case t.MergeGated:
			item.State = RequestOutcomeMergeGated
		default:
			item.State = RequestOutcomeDone
		}
	case domain.RequestTargetArtifact:
		item.Kind = RequestOutcomeArtifact
		switch {
		case t.Deleted:
			item.State = RequestOutcomeDeleted
		case t.ArtifactApproved:
			item.State = RequestOutcomeDone
		default:
			// It exists — so it is evidence the outcome has something to point
			// at — but the version that is current has not been approved.
			item.State = RequestOutcomeUnverified
		}
	case domain.RequestTargetReceipt:
		item.Kind = RequestOutcomeReceipt
		item.VerificationStatus = domain.DeriveStatus(domain.Receipt{AgentID: t.ReceiptAgentID}, t.ReceiptChecks)
		switch {
		case t.Deleted:
			item.State = RequestOutcomeDeleted
		case t.ReceiptHeld:
			item.State = RequestOutcomeHeld
		default:
			item.State = RequestOutcomeDone
		}
	default:
		// Not a delivery kind at all (a project, an epic, a request): the count
		// above already treats it as no evidence, and here it is reported as a
		// deleted-shaped item rather than as done.
		item.Kind = RequestOutcomeItemKind(t.Kind)
		item.State = RequestOutcomeDeleted
	}
	return item
}

// requestOutcomeBlockedReason says what a delivery would still be missing, in the
// contract's words, or "" when it would not be missing anything. It is a
// sentence for a Deliver panel's disabled state, not a refusal: the refusal, when
// a caller tries anyway, is the policy's.
func requestOutcomeBlockedReason(req *domain.TeamRequest, out *RequestOutcome) string {
	var missing []string
	// The request's own state is the first thing a delivery needs, and the only
	// state delivery leaves from is in_progress (the lifecycle's one delivery
	// edge). Saying so is what lets a Deliver panel explain itself on a request
	// that is still being triaged rather than looking mysteriously disabled.
	if req.State != domain.RequestStatusInProgress {
		missing = append(missing, fmt.Sprintf("the request to be in progress before it can be delivered (it is %s)", req.State))
	}
	if !out.OutcomeBriefSet {
		missing = append(missing, "the outcome brief (what is and is not included)")
	}
	if out.Total == 0 && !out.NoArtifact {
		missing = append(missing, "a delivered link to the work that delivered it (a task, an artifact or a receipt), or an explicit no-artifact record")
	}
	switch {
	case len(missing) == 0:
		// Everything a delivery needs is present, and the request still is not
		// delivered: the only thing missing is the act itself, and saying so is
		// what keeps a ready Deliver panel from looking broken.
		return "the team has not delivered it yet: everything a delivery needs is in place"
	case len(missing) == 1:
		return "delivery needs " + missing[0]
	default:
		return "delivery needs " + missing[0] + " and " + missing[1]
	}
}

// requestOutcomeTargetKey is a target's identity for the aggregate's maps.
func requestOutcomeTargetKey(kind domain.RequestTargetKind, id int) string {
	return string(kind) + ":" + fmt.Sprint(id)
}

// valueOrEmpty reads an optional string for a JSON-safe report.
func valueOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// ---- The requester's milestones ----

// RequesterMilestonesFor maps the aggregate onto the requester projection's
// closed vocabulary: five steps, each "done" only when the request's own
// lifecycle says so. It is the one bridge between the aggregate and the
// requester's stepper, so the team rail and the requester view read the same
// facts (E3's "counts match the member view" rule) while the requester only ever
// sees words.
//
// The gate is not this function's invention: the projection re-gates every step
// against the lifecycle state itself (domain.RequesterMilestoneDoneAllowed), so a
// bug here cannot announce a delivery the request never made. This function's job
// is the NOTE — the one sentence a requester gets beyond "in progress" — and the
// note is built from counts and the contract's words only. It never names a
// ticket, a project, an epic, a branch, a person or an agent.
func RequesterMilestonesFor(req *domain.TeamRequest, out *RequestOutcome) []domain.RequesterMilestoneInput {
	if req == nil {
		return nil
	}
	created := req.CreatedAt
	steps := []domain.RequesterMilestoneInput{
		{Key: domain.RequesterMilestoneReceived, Done: true, At: &created},
		{Key: domain.RequesterMilestoneReviewing},
		{Key: domain.RequesterMilestoneInProgress},
		{Key: domain.RequesterMilestoneDelivered, At: req.DeliveredAt},
		{Key: domain.RequesterMilestoneAccepted, At: req.AcceptedAt},
	}
	// Done comes from the ONE rule the projection enforces, so the list cannot
	// say "done" for a state the projection would then undo.
	for i := range steps {
		steps[i].Done = domain.RequesterMilestoneDoneAllowed(steps[i].Key, req.State)
	}
	if out != nil {
		for i := range steps {
			switch steps[i].Key {
			case domain.RequesterMilestoneInProgress:
				// The note describes the work the desk is doing, in counts. It is
				// shown on a pending step (the projection keeps notes on pending
				// steps too), which is what makes "3 of 4 items are finished" a
				// useful thing to read while the request is still in progress.
				steps[i].Note = requesterProgressNote(out)
			case domain.RequesterMilestoneDelivered:
				if steps[i].Done && out.NoArtifact {
					// The delivery was the explicit "nothing to link" record: say
					// so, because a requester looking for a file should not think
					// one went missing.
					steps[i].Note = "Delivered: there is no file attached to this outcome"
				}
			}
		}
	}
	return steps
}

// requesterProgressNote is the requester's one progress sentence: how much of the
// desk's work is finished, and — when it is not all of it — that the rest is
// still being worked on. Counts only, and only from the delivered_by evidence,
// because that is the work the requester is being told about.
//
// It says nothing when there is nothing to say: an unlinked request's stepper
// already reads "being reviewed", and inventing "0 of 0 done" would be noise.
func requesterProgressNote(out *RequestOutcome) string {
	work := out.Total - out.Deleted
	if work <= 0 {
		return ""
	}
	// Held and gated pieces are finished as work and not finished as outcomes;
	// a requester is told what is done, and told the desk is still on it, without
	// the internal words for why.
	finished := out.Done + out.Gated + out.Held + out.Unverified
	switch {
	case finished == 0:
		return fmt.Sprintf("%d of %d items are in progress", work-out.Done, work)
	case finished == work && out.Done == work:
		// Every piece is done. If the request is not delivered yet, saying "all
		// finished, the desk is wrapping up" is the honest sentence; it is
		// deliberately NOT "delivered" (I8), and the projection's gate would
		// withhold that word anyway.
		if out.Phase == RequestPhaseInProgress || out.Phase == RequestPhaseBlocked {
			return fmt.Sprintf("All %d items are finished; the desk is still completing the delivery", work)
		}
		return fmt.Sprintf("All %d items are finished", work)
	default:
		return fmt.Sprintf("%d of %d items are finished", finished, work)
	}
}

// ---- The service read ----

// RequestOutcome resolves and aggregates. It is the definition every view should
// read: the team rail, the queue cell, and the requester projection's milestone
// input.
//
// Authorization is the read path's, not this method's second copy of it: the
// caller is expected to have loaded the request through the authorized read
// (GetRequest / ListRequests, authz.RequestViewFor). This method takes the org and
// team it was loaded with and refuses to read outside them, so a caller cannot
// aim it at another tenant even by mistake — the repository's tenant where-clause
// is the enforcement, and a request in another team comes back ErrNotFound.
func (s *Service) RequestOutcome(ctx context.Context, requestID, orgID, teamID int) (*domain.TeamRequest, *RequestOutcome, error) {
	facts, err := s.Repo.RequestOutcomeFactsFor(ctx, requestID, orgID, teamID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, nil, authz.RequestNotFound()
		}
		return nil, nil, fmt.Errorf("read outcome facts of request %d: %w", requestID, err)
	}
	return facts.Request, BuildRequestOutcome(requestOutcomeInput(facts)), nil
}

// RequestOutcomesFor resolves a page of requests in one pass. The returned map is
// keyed by request id and holds only ids in this tenant; a request that is not
// found is absent rather than an error, because a list is filtered before it gets
// here and one stale id must not fail a whole queue.
func (s *Service) RequestOutcomesFor(ctx context.Context, orgID, teamID int, requestIDs []int) (map[int]*RequestOutcome, error) {
	facts, err := s.Repo.RequestOutcomeFactsForRequests(ctx, orgID, teamID, requestIDs)
	if err != nil {
		return nil, fmt.Errorf("read outcome facts of %d requests: %w", len(requestIDs), err)
	}
	out := make(map[int]*RequestOutcome, len(facts))
	for id, f := range facts {
		out[id] = BuildRequestOutcome(requestOutcomeInput(f))
	}
	return out, nil
}

// requestOutcomeInput flattens the repository's fact bundle into the pure
// function's input, including the runs (which the input type carries separately
// so a test can state one ticket's runs without a target list).
func requestOutcomeInput(f *repository.RequestOutcomeFacts) RequestOutcomeInput {
	return RequestOutcomeInput{
		Request:    f.Request,
		Links:      f.Links,
		Targets:    f.Targets,
		Runs:       f.Runs,
		NoArtifact: f.NoArtifact,
	}
}

// ---- Delivery ----

// DeliverRequestInput is one delivery, as a surface receives it: the request, the
// inbox it is in, and — for the deliver act only — the brief and the explicit
// no-artifact statement.
//
// There is no field for a link: attaching a delivered_by target is the attach
// path's act (#3962's AttachRequest), and a delivery that took links from its
// caller would be a delivery the server could not verify. The evidence is
// whatever the request has when this runs.
type DeliverRequestInput struct {
	RequestID   int
	TeamID      int
	ExpectedRev int
	// OutcomeBrief is what is and is not included (I2). Required.
	OutcomeBrief string
	// NoArtifact is the explicit "the outcome exists and there is nothing to
	// link" record. It is a statement, not an absence: without it a delivery
	// needs verified delivered_by links, and with links already present setting
	// it is refused (the policy's "not both").
	NoArtifact bool
}

// DeliverRequest is the team stating the outcome exists: one atomic lifecycle
// write (state, rev, history) behind the aggregate's gate.
//
// The two checks it adds to the lifecycle's are the ones that make the delivery
// truthful rather than merely well-formed:
//
//   - the aggregate is rebuilt from the facts as they are NOW, and a delivery
//     whose brief or evidence does not make DeliveredEligible true is refused
//     with what is missing. This is what stops a delivery naming a project or a
//     task the request never linked: the gate only looks at the request's own
//     links, which the attach path verified into this tenant.
//   - a link that is deleted does not count as evidence (the item is Deleted, so
//     it is neither Done nor Unverified), which is the tombstone rule applied to
//     the gate itself: "we delivered this with a ticket that has since been
//     deleted" needs a live target or the no-artifact record, not a memory.
//
// Not this file's job, deliberately: the requester's acceptance (I1, D5: the
// requester's alone, in the web UI, via AcceptRequest — unreachable from here),
// any link writing, any planning, and any audit beyond the one row the lifecycle's
// transition already records.
func (s *Service) DeliverRequest(ctx context.Context, p authz.Principal, in DeliverRequestInput) (*TransitionRequestResult, error) {
	// A submit-scoped credential submits and does nothing else, deliveries
	// included (authz token_scope). Refused by name before the request is loaded,
	// so a narrowed token cannot probe request ids through this path.
	if err := p.RequireUnscoped("deliver a request"); err != nil {
		return nil, err
	}
	if in.RequestID <= 0 || in.TeamID <= 0 {
		// The same shapes TransitionRequest refuses, answered the same way so a
		// surface has one error for "not a request you named".
		return nil, authz.RequestNotFound()
	}
	// Delivery is the inbox team's act (triage, member or manager), resolved from
	// the rows on this call — the same loader, and the same per-call re-check,
	// every other request write starts from.
	_, req, capability, err := s.loadInboxRequest(ctx, p, in.RequestID, in.TeamID)
	if err != nil {
		return nil, err
	}
	if !capability.canMake(RequestCapabilityTeam) {
		if capability == RequestCapabilityNone {
			return nil, authz.RequestNotFound()
		}
		return nil, authz.Forbidden("delivering request %s is the inbox team's act: being its requester means following it and accepting it, not stating what the desk delivered (I2). Nothing was changed.", req.PublicRef)
	}

	// The gate, on the same facts the rail shows. One definition: the aggregate
	// is asked, and its answer is the answer.
	facts, err := s.Repo.RequestOutcomeFactsFor(ctx, req.ID, req.OrgID, req.TeamID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, authz.RequestNotFound()
		}
		return nil, fmt.Errorf("read outcome facts of request %d: %w", req.ID, err)
	}
	// The brief and the no-artifact record being delivered are the caller's, so
	// the aggregate is asked about them, not about what the row carried a moment
	// ago: a delivery note written now is what makes the brief set.
	probe := requestOutcomeInput(facts)
	// A copy, so the probed brief is the caller's and the stored row is untouched.
	probeRow := *facts.Request
	probeRow.OutcomeBrief = in.OutcomeBrief
	probe.Request = &probeRow
	probe.NoArtifact = in.NoArtifact
	probeLinks := len(requestOutcomeEvidenceLinks(facts.Links))
	if probeLinks == 0 && !in.NoArtifact {
		return nil, authz.RequestInvalid(
			"delivered needs evidence: link what delivered request %s (a task, an artifact or a receipt), or state explicitly that there is nothing to link (no_artifact). A brief alone does not say the outcome exists (I2, D10).", req.PublicRef)
	}
	if probeLinks > 0 && in.NoArtifact {
		return nil, authz.RequestInvalid(
			"request %s already has %d delivered link(s): a delivery is either linked to its evidence or explicitly no-artifact, not both. Say which it is, so the requester's view can state it honestly (I2).", req.PublicRef, probeLinks)
	}
	if strings.TrimSpace(in.OutcomeBrief) == "" {
		return nil, authz.RequestInvalid("delivered is evidenced: outcome_brief says what is and is not included (I2)")
	}

	// The one state machine makes the write. Its own evidence check counts the
	// same links in the same transaction, so a link that vanished between the
	// aggregate and the write is refused there rather than delivered.
	return s.TransitionRequest(ctx, p, TransitionRequestInput{
		RequestID:    req.ID,
		TeamID:       req.TeamID,
		To:           domain.RequestStatusDelivered,
		ExpectedRev:  in.ExpectedRev,
		OutcomeBrief: in.OutcomeBrief,
		NoArtifact:   in.NoArtifact,
	})
}

// requestOutcomeEvidenceLinks is the subset of a request's links that can evidence
// a delivery at all: kind delivered_by AND one of D10's three target kinds. A row
// that fails either half is not evidence, whatever produced it — the same
// validation the lifecycle's countDeliveredBy makes, stated once here so the
// delivery gate and the lifecycle's transaction cannot disagree about what counts.
func requestOutcomeEvidenceLinks(links []domain.RequestLink) []domain.RequestLink {
	out := make([]domain.RequestLink, 0, len(links))
	for _, l := range links {
		if l.LinkKind != domain.RequestLinkDeliveredBy {
			continue
		}
		switch l.TargetKind {
		case domain.RequestTargetTask, domain.RequestTargetArtifact, domain.RequestTargetReceipt:
			out = append(out, l)
		}
	}
	return out
}
