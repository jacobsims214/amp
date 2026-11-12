package service

// The requester-safe request projection (epic 202, story 548, AMP #3954;
// approved product contract artifact 36 v2 §4; approved requester mockup
// artifact 38 v1; build slice E3 of
// discoveries/team-inbox-approved-build-slices.md).
//
// One pure function turns already-authorized facts into the one object a
// requester is allowed to see. It is pure on purpose:
//
//   - No database, no HTTP, no principal, no clock and no authorization
//     decision. Who may see this request was decided before it runs
//     (authz.RequestViewFor with InboxRole, slice B4); which comments the team
//     chose to share and which files it shared are the read path's decisions.
//     The projection's only job is the whitelist, the sanitizing and the
//     fail-closed degradations below.
//   - Nothing here counts tickets. The input carries no work item, no merge
//     gate, no receipt and no "closed" tally, so delivery cannot be inferred
//     from finished work even by mistake. A milestone the aggregate reports as
//     done is re-gated against the request's own lifecycle state
//     (domain.RequesterMilestoneDoneAllowed), so the projection cannot announce
//     a delivery the team never stated. Contract §4: "A closing ticket never
//     flips a request".
//
// The shape it returns (domain.RequesterRequestView) is the whitelist; this file
// is the only place that fills it, and it fills it from named structs rather
// than from the record, so a field added to TeamRequest tomorrow reaches nobody
// here.

import (
	"strings"

	"github.com/simstech/amp-api/internal/domain"
)

// RequesterProjectionInput is everything the projection may look at. Each part
// is already authorized and already filtered by the read path; nil and empty are
// both "nothing the team chose to share", never "show everything".
type RequesterProjectionInput struct {
	// Request is the stored record. The projection reads only the requester-safe
	// subset of it: the opaque ref, the requester's own subject and brief, the
	// lifecycle state and the lifecycle timestamps. Every other column on the
	// record (serial id, org, team, claim, assignee, duplicate pointer, revision,
	// metadata, internal owners, update time) is deliberately not read here and
	// has no field to reach in the view.
	Request *domain.TeamRequest

	// TeamName is the desk's display name, resolved by the read path. Empty
	// renders as empty rather than as an id.
	TeamName string

	// Milestones are the public steps the outcome aggregate established
	// (slice E2). The projection orders them by the closed vocabulary, gates
	// each "done" against the lifecycle state and drops any step it does not
	// know.
	Milestones []domain.RequesterMilestoneInput

	// Comments are the comments the team chose to share with this requester, in
	// the order they should read. Nothing here searches for a private comment:
	// a comment the read path did not hand over does not exist for this
	// projection.
	Comments []domain.RequesterCommentInput

	// Outputs are the files the read path decided this requester may see: their
	// own uploads and the outputs the team explicitly shared. A ready file must
	// carry a DownloadRef to be advertised as ready.
	Outputs []domain.RequesterOutputInput
}

// ProjectRequesterRequest builds the requester's view of one request, or nil
// when there is no request to project. Everything it emits is either on the
// whitelist, degraded toward "less", or both.
func ProjectRequesterRequest(in RequesterProjectionInput) *domain.RequesterRequestView {
	r := in.Request
	if r == nil {
		return nil
	}
	view := &domain.RequesterRequestView{
		PublicRef:    domain.SanitizeRequesterText(strings.TrimSpace(r.PublicRef)),
		State:        r.State,
		StateWord:    domain.RequesterStateWord(r.State),
		Subject:      domain.RequesterNote(r.Subject),
		InputBrief:   domain.RequesterNote(r.InputBrief),
		TeamName:     domain.RequesterNote(in.TeamName),
		CreatedAt:    r.CreatedAt,
		OutcomeBrief: domain.RequesterNote(r.OutcomeBrief),
		RejectReason: domain.RequesterNote(r.RejectReason),
		DeliveredAt:  r.DeliveredAt,
		AcceptedAt:   r.AcceptedAt,
		ClosedAt:     r.ClosedAt,
		Milestones:   requesterMilestones(in.Milestones, r.State),
		Comments:     requesterComments(in.Comments),
		Outputs:      requesterOutputs(in.Outputs),
		Question:     requesterQuestion(in.Comments, r.State),
	}
	if r.State == domain.RequestStatusNeedsInformation {
		view.NeedsReplySince = r.NeedsInfoSince
	}

	// One function decides which of those claims the lifecycle state supports,
	// and it is the only place that decides. Everything above copies; this
	// clears. That is why the call is load-bearing rather than decorative: a
	// state that is not delivered loses the outcome brief, the delivery
	// timestamp and the delivered milestone here, and a state that is not
	// requester_accepted loses the acceptance too, whatever the record carried.
	enforceDeliveryClaim(view)
	return view
}

// requesterMilestones builds the stepper: one entry per closed-vocabulary key,
// in stepper order, each "done" re-gated against the lifecycle state. A key the
// view does not know is not a step, and a duplicate is the first one only.
func requesterMilestones(inputs []domain.RequesterMilestoneInput, state domain.RequestStatus) []domain.RequesterMilestone {
	byKey := make(map[domain.RequesterMilestoneKey]domain.RequesterMilestoneInput, len(inputs))
	for _, in := range inputs {
		if _, ok := byKey[in.Key]; !ok {
			byKey[in.Key] = in
		}
	}
	out := make([]domain.RequesterMilestone, 0, len(domain.RequesterMilestoneKeys))
	for _, key := range domain.RequesterMilestoneKeys {
		step := domain.RequesterMilestone{Key: key}
		if in, ok := byKey[key]; ok {
			// The gate is the point: a step the aggregate called done stays
			// pending while the request's own state has not reached it. So a
			// request whose linked tickets are all complete but which the team
			// has not delivered shows "In progress", never "Delivered".
			if in.Done && domain.RequesterMilestoneDoneAllowed(key, state) {
				step.Done = true
				step.At = in.At
			}
			// A note may still describe the step while it is pending ("we are
			// re-running the export"); it is sanitized and bounded like all text.
			step.Note = domain.RequesterNote(in.Note)
		}
		out = append(out, step)
	}
	return out
}

// requesterComments reduces each shared comment to the whitelist. An unknown
// kind is dropped rather than passed through (the schema closes the vocabulary,
// so an unknown value is a bug, not a new kind), and attribution collapses to
// "you" or "the desk".
func requesterComments(inputs []domain.RequesterCommentInput) []domain.RequesterComment {
	out := make([]domain.RequesterComment, 0, len(inputs))
	for _, in := range inputs {
		if !isRequestCommentKind(in.Kind) {
			continue
		}
		from := domain.RequesterAuthorDesk
		if in.FromRequester {
			from = domain.RequesterAuthorYou
		}
		out = append(out, domain.RequesterComment{
			Kind: in.Kind,
			From: from,
			Body: domain.RequesterNote(in.Body),
			At:   in.At,
		})
	}
	return out
}

// requesterQuestion is the desk's open question, when there is one: only in
// needs_information (the one state that asks the requester something), and only
// from a question comment the team actually shared. No question shared means no
// card — the projection does not invent a prompt, and it never reaches for a
// private comment to fill the slot.
func requesterQuestion(inputs []domain.RequesterCommentInput, state domain.RequestStatus) *domain.RequesterQuestion {
	if state != domain.RequestStatusNeedsInformation {
		return nil
	}
	var latest *domain.RequesterCommentInput
	for i := range inputs {
		in := inputs[i]
		if in.Kind != domain.RequestCommentQuestion {
			continue
		}
		if latest == nil || !in.At.Before(latest.At) {
			latest = &inputs[i]
		}
	}
	if latest == nil {
		return nil
	}
	body := domain.RequesterNote(latest.Body)
	if body == "" {
		return nil
	}
	return &domain.RequesterQuestion{Body: body, At: latest.At}
}

// requesterOutputs reduces the shared files to the whitelist and enforces the
// one rule that matters about a download: a reference is advertised only for a
// file that is ready, and a ready file with no reference degrades to
// unavailable rather than showing a download that cannot work. The reference
// itself is opaque and passed through untouched (the read path minted it — a
// short-lived signed grant, slice G3 — and it is never an attachment or artifact
// row id).
//
// The same three-word status covers both sources on purpose: an artifact's
// in_review/approved/changes_requested, an attachment's scan_state and its
// restricted-file hold are internal facts, and the mockup shows "Ready" or
// "Scanning", not any of them.
func requesterOutputs(inputs []domain.RequesterOutputInput) []domain.RequesterOutput {
	out := make([]domain.RequesterOutput, 0, len(inputs))
	for _, in := range inputs {
		if !isRequesterOutputSource(in.Source) || !isRequesterOutputStatus(in.Status) {
			continue
		}
		name := domain.RequesterNote(in.Name)
		if name == "" {
			continue
		}
		status, ref := in.Status, strings.TrimSpace(in.DownloadRef)
		if status == domain.RequesterOutputReady && ref == "" {
			// The file is said to be ready but nothing can serve it. Fail
			// closed: no download, and a word that promises nothing.
			status = domain.RequesterOutputUnavailable
		}
		if status != domain.RequesterOutputReady {
			ref = ""
		}
		size := in.SizeBytes
		if size < 0 {
			size = 0
		}
		out = append(out, domain.RequesterOutput{
			Source:      in.Source,
			Name:        name,
			SizeBytes:   size,
			Status:      status,
			At:          in.At,
			DownloadRef: ref,
		})
	}
	return out
}

// enforceDeliveryClaim decides which delivery claims the lifecycle state
// supports, and it is the only decision of its kind in this file. It clears each
// claim the state does not support — the outcome brief and delivered timestamp
// unless the request is delivered, the reject reason unless it is rejected, the
// acceptance timestamp unless it is requester_accepted, and a done
// delivered/accepted milestone under the same two gates — and it never sets
// anything, so a state the vocabulary does not recognise clears every claim it
// covers.
//
// The gates are separate on purpose. `delivered` is the team's act and
// `requester_accepted` is the requester's alone (contract §4, D5), so a merely
// delivered request carrying an acceptance would put words in the requester's
// mouth. And because this is a single decision over a copied view, the rule
// holds for any way the view is assembled: a closing ticket, a merge gate or a
// receipt cannot make it here, and if some future path copied one in, the state
// check would still clear it.
func enforceDeliveryClaim(v *domain.RequesterRequestView) {
	delivered := v.State == domain.RequestStatusDelivered || v.State == domain.RequestStatusRequesterAccepted
	accepted := v.State == domain.RequestStatusRequesterAccepted
	if !delivered {
		v.OutcomeBrief = ""
		v.DeliveredAt = nil
	}
	if v.State != domain.RequestStatusRejected {
		v.RejectReason = ""
	}
	if !accepted {
		v.AcceptedAt = nil
	}
	for i := range v.Milestones {
		switch v.Milestones[i].Key {
		case domain.RequesterMilestoneDelivered:
			if !delivered {
				v.Milestones[i].Done = false
				v.Milestones[i].At = nil
			}
		case domain.RequesterMilestoneAccepted:
			if !accepted {
				v.Milestones[i].Done = false
				v.Milestones[i].At = nil
			}
		}
	}
}

// isRequestCommentKind reports whether kind is in the closed comment
// vocabulary (the same four values the schema's CHECK accepts).
func isRequestCommentKind(kind domain.RequestCommentKind) bool {
	for _, k := range domain.RequestCommentKinds {
		if k == kind {
			return true
		}
	}
	return false
}

// isRequesterOutputSource reports whether source is one of the two closed
// output sources.
func isRequesterOutputSource(source domain.RequesterOutputSource) bool {
	for _, s := range domain.RequesterOutputSources {
		if s == source {
			return true
		}
	}
	return false
}

// isRequesterOutputStatus reports whether status is one of the three public
// words. An unknown status is dropped, not guessed at.
func isRequesterOutputStatus(status domain.RequesterOutputStatus) bool {
	for _, s := range domain.RequesterOutputStatuses {
		if s == status {
			return true
		}
	}
	return false
}
