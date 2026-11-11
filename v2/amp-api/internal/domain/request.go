package domain

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Team inbox requests (epic 202; product contract artifact 36 v2, build slice
// A1). A request is not a ticket: it arrives from outside the plan, the team
// triages it, plans work for it and delivers an outcome, and only the
// requester accepts it. Nothing inbound approves a plan, dispatches work or
// changes a ticket's state.
//
// This file holds the record's types, the fixed vocabularies the schema also
// checks (nine lifecycle states, comment kinds, event actions, link kinds and
// targets, sources) and the bounded shapes intake normalizes before anything
// is stored. Which move is legal and who may make it belongs to the service's
// one transition function; tables and constraints live in migration 053+.
// Metadata is the extension space for connector labels and routing hints, and
// never stands in for a typed relation or an authorization decision.

// RequestStatus is a request's lifecycle state. Reopen is a transition, not a
// state (D6): a reopened request goes back to triaging or needs_information
// with reopened_count raised, so the rules never have to double.
type RequestStatus string

const (
	RequestStatusNew               RequestStatus = "new"
	RequestStatusTriaging          RequestStatus = "triaging"
	RequestStatusNeedsInformation  RequestStatus = "needs_information"
	RequestStatusAccepted          RequestStatus = "accepted"
	RequestStatusInProgress        RequestStatus = "in_progress"
	RequestStatusDelivered         RequestStatus = "delivered"
	RequestStatusRequesterAccepted RequestStatus = "requester_accepted"
	RequestStatusRejected          RequestStatus = "rejected"
	RequestStatusDuplicate         RequestStatus = "duplicate"
)

// RequestStatuses is the fixed lifecycle, in lifecycle order. `state` in the
// schema is a CHECK over exactly these nine values.
var RequestStatuses = []RequestStatus{
	RequestStatusNew, RequestStatusTriaging, RequestStatusNeedsInformation,
	RequestStatusAccepted, RequestStatusInProgress, RequestStatusDelivered,
	RequestStatusRequesterAccepted, RequestStatusRejected, RequestStatusDuplicate,
}

// RequestSourceKind is how a request came in. Only a connector carries the
// vendor's immutable object id; a web form's retries are covered by the
// submit idempotency key instead (D12).
type RequestSourceKind string

const (
	RequestSourceWeb       RequestSourceKind = "web"
	RequestSourceMCP       RequestSourceKind = "mcp"
	RequestSourceAPI       RequestSourceKind = "api"
	RequestSourceConnector RequestSourceKind = "connector"
)

// RequestSourceKinds are the four intake sources.
var RequestSourceKinds = []RequestSourceKind{
	RequestSourceWeb, RequestSourceMCP, RequestSourceAPI, RequestSourceConnector,
}

// RequestCommentKind is what a request comment is. A question is the team
// asking the requester something (needs_information obliges a preceding one),
// and the answer is what returns the request to where it was.
type RequestCommentKind string

const (
	RequestCommentDiscussion RequestCommentKind = "comment"
	RequestCommentQuestion   RequestCommentKind = "question"
	RequestCommentAnswer     RequestCommentKind = "answer"
	RequestCommentSystem     RequestCommentKind = "system"
)

// RequestCommentKinds are the four discussion kinds (append-only rows).
var RequestCommentKinds = []RequestCommentKind{
	RequestCommentDiscussion, RequestCommentQuestion, RequestCommentAnswer, RequestCommentSystem,
}

// RequestEventAction is what one append-only history row records: the submit,
// one state transition (from/to), a claim or its release, an assignee
// transfer, a link or its detach, and a versioned content edit. Every such act
// writes exactly one row.
type RequestEventAction string

const (
	RequestActionSubmit     RequestEventAction = "submit"
	RequestActionTransition RequestEventAction = "transition"
	RequestActionClaim      RequestEventAction = "claim"
	RequestActionRelease    RequestEventAction = "release"
	RequestActionAssign     RequestEventAction = "assign"
	RequestActionLink       RequestEventAction = "link"
	RequestActionUnlink     RequestEventAction = "unlink"
	// RequestActionEdit is a versioned content edit (AMP #3873's UpdateRequest,
	// widened by migration 062). Like every non-transition verb it names no
	// states; the revision it moved between is in the row's detail.
	RequestActionEdit RequestEventAction = "edit"
)

// RequestEventActions are the history actions. The set mirrors migration 054's
// request_events_action CHECK as widened by 062: the database and the domain
// may not disagree about the vocabulary (#3754's rule, restated in 054).
var RequestEventActions = []RequestEventAction{
	RequestActionSubmit, RequestActionTransition, RequestActionClaim,
	RequestActionRelease, RequestActionAssign, RequestActionLink, RequestActionUnlink,
	RequestActionEdit,
}

// RequestLinkKind is the durable relation a link row carries: routing a
// request to one project, the plan items serving it, the tasks, artifacts and
// receipts that delivered it, and links between requests.
type RequestLinkKind string

const (
	RequestLinkRoutesTo    RequestLinkKind = "routes_to"
	RequestLinkPlannedAs   RequestLinkKind = "planned_as"
	RequestLinkDeliveredBy RequestLinkKind = "delivered_by"
	RequestLinkDuplicateOf RequestLinkKind = "duplicate_of"
	RequestLinkRelatesTo   RequestLinkKind = "relates_to"
)

// RequestLinkKinds are the five link kinds (schema CHECK).
var RequestLinkKinds = []RequestLinkKind{
	RequestLinkRoutesTo, RequestLinkPlannedAs, RequestLinkDeliveredBy,
	RequestLinkDuplicateOf, RequestLinkRelatesTo,
}

// RequestTargetKind is what a link may point at. Deliberately no database
// foreign key on target_id: a link outlives the ticket or artifact it names,
// so deleting the target tombstones the link instead of erasing lineage.
type RequestTargetKind string

const (
	RequestTargetProject  RequestTargetKind = "project"
	RequestTargetEpic     RequestTargetKind = "epic"
	RequestTargetStory    RequestTargetKind = "story"
	RequestTargetTask     RequestTargetKind = "task"
	RequestTargetArtifact RequestTargetKind = "artifact"
	RequestTargetReceipt  RequestTargetKind = "receipt"
	RequestTargetRequest  RequestTargetKind = "request"
)

// RequestTargetKinds are the seven link targets (schema CHECK).
var RequestTargetKinds = []RequestTargetKind{
	RequestTargetProject, RequestTargetEpic, RequestTargetStory, RequestTargetTask,
	RequestTargetArtifact, RequestTargetReceipt, RequestTargetRequest,
}

// requestLinkTargets is the target kind each link kind may point at (contract
// §3 with D9 and D10): routes_to is the one project a request is routed to,
// planned_as the epics, stories and tasks its plan is made of, delivered_by
// the tasks, artifacts and receipts that prove delivery, and duplicate_of and
// relates_to only ever point at another request.
var requestLinkTargets = map[RequestLinkKind][]RequestTargetKind{
	RequestLinkRoutesTo:    {RequestTargetProject},
	RequestLinkPlannedAs:   {RequestTargetEpic, RequestTargetStory, RequestTargetTask},
	RequestLinkDeliveredBy: {RequestTargetTask, RequestTargetArtifact, RequestTargetReceipt},
	RequestLinkDuplicateOf: {RequestTargetRequest},
	RequestLinkRelatesTo:   {RequestTargetRequest},
}

// Bounds on what intake stores. Where migration 053 (team_requests) also
// constrains a column it is a generous backstop (its comment says so): these
// are the product's real bounds, deliberately tighter. public_ref and
// requester_contact are the exceptions: the schema pins their shape exactly,
// so the domain must accept no more than it stores.
const (
	// MaxRequestSubjectLen caps the one-line subject.
	MaxRequestSubjectLen = 200
	// MaxRequestBriefLen caps input_brief and outcome_brief, the markdown the
	// requester wrote and the team's delivery note.
	MaxRequestBriefLen = 20000
	// MaxRequestRejectReasonLen caps the reason a rejected request carries.
	MaxRequestRejectReasonLen = 2000
	// MaxRequestContactLen caps the contact snapshot. It is the schema's bound
	// (053), because a contact may be a handle from another tool rather than an
	// address, and this column is the durable record of who asked.
	MaxRequestContactLen = 500
	// MaxRequestExternalIDLen caps a connector's vendor object id.
	MaxRequestExternalIDLen = 255

	// MinRequestPublicRefLen and MaxRequestPublicRefLen match the schema's
	// public_ref shape CHECK exactly (053): 16 to 64 characters of letters,
	// digits, "-" or "_". A shorter token is guessable, and storing the
	// mockups' REQ-8F2Q1 display form is refused, so 16 is the floor. The
	// display prefix is stripped on the way in.
	MinRequestPublicRefLen = 16
	MaxRequestPublicRefLen = 64
	// RequestRefPrefix is the display prefix of a public reference.
	RequestRefPrefix = "REQ-"
)

// TeamRequest is one request in a team's inbox: what the requester asked for,
// who asked, and everything triage added around it. The requested content is
// immutable; edits are versioned with Rev.
//
// Two kinds of ownership are kept apart on purpose (#3654 C5, D4):
// ClaimUserID is the triage claim — "I am looking at it right now", one at a
// time, released by hand and never expired by a clock — while OwnerUserID is
// the assignee who owns the outcome: durable, transferred explicitly, and a
// transfer clears the claim. They are never one field, or the shared queue and
// a person's own work would disagree about who is working.
//
// Rev is the optimistic-concurrency token every write carries as If-Match /
// expected_rev: a stale rev is a conflict, never a lost update. The entry
// timestamps are first-write-wins, so a repeated report never moves one.
type TeamRequest struct {
	ID        int    `json:"id"`
	PublicRef string `json:"public_ref"`
	OrgID     int    `json:"org_id"`
	TeamID    int    `json:"team_id"`

	// RequesterUserID is the signed-in requester, when there is one, and nil
	// for a service or connector submission. RequesterContact is the snapshot
	// taken at submit and kept even if the user is later deleted, so a
	// request always says who asked.
	RequesterUserID  *int   `json:"requester_user_id"`
	RequesterContact string `json:"requester_contact"`

	SourceKind RequestSourceKind `json:"source_kind"`
	// SourceExternalID is the vendor's immutable id: it, not a key or URL,
	// dedups a connector's replay.
	SourceExternalID string `json:"source_external_id"`

	Subject      string `json:"subject"`
	InputBrief   string `json:"input_brief"`
	OutcomeBrief string `json:"outcome_brief"`

	// State is the lifecycle state; the column is `state`, and the schema
	// CHECK uses the same nine values (RequestStatuses).
	State RequestStatus `json:"state"`

	// The triage claim (see the type comment).
	ClaimUserID *int       `json:"claim_user_id"`
	ClaimedAt   *time.Time `json:"claimed_at"`
	// OwnerUserID is the assignee who owns the outcome.
	OwnerUserID *int `json:"owner_user_id"`

	// DuplicateOf points at the canonical request once this one is flattened;
	// a canonical is never a duplicate.
	DuplicateOf  *int   `json:"duplicate_of"`
	RejectReason string `json:"reject_reason"`

	// NeedsInfoSince is when the team last asked a question, so a later clock
	// can exclude the wait (D8); no SLA ships with it.
	NeedsInfoSince *time.Time `json:"needs_info_since"`
	PlannedAt      *time.Time `json:"planned_at"`
	DeliveredAt    *time.Time `json:"delivered_at"`
	// AcceptedAt and AcceptedBy record the requester's acceptance. Acceptance
	// is the requester's alone (I1, D5): no team member, agent, token or
	// connector may set either.
	AcceptedAt *time.Time `json:"accepted_at"`
	AcceptedBy *int       `json:"accepted_by"`
	ClosedAt   *time.Time `json:"closed_at"`
	ReopenedAt *time.Time `json:"reopened_at"`

	ReopenedCount int `json:"reopened_count"`
	Rev           int `json:"rev"`

	// Metadata is connector labels and routing hints only (domain.Metadata
	// limits apply); durable relations live in link rows.
	Metadata  Metadata  `json:"metadata"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// RequestComment is one row of a request's discussion: append-only, never
// updated, so the thread is also the record. Author is the display name at
// write time and AuthorUserID the AMP user behind it when there is one (a
// connector's service client posts as its own identity, not as the requester).
// A question is what needs_information obliges an answer to.
type RequestComment struct {
	ID           int64              `json:"id"`
	RequestID    int                `json:"request_id"`
	Kind         RequestCommentKind `json:"kind"`
	Body         string             `json:"body"`
	Author       string             `json:"author"`
	AuthorUserID *int               `json:"author_user_id"`
	CreatedAt    time.Time          `json:"created_at"`
}

// RequestEvent is one row of a request's history: the submit, a transition
// with its from/to states, a claim or release, an assignee transfer, a link or
// its detach. Append-only, one row per act. Detail holds the JSON that explains
// it (a transfer's previous assignee, a link's target); FromStatus and
// ToStatus are set for transitions only.
type RequestEvent struct {
	ID          int64              `json:"id"`
	RequestID   int                `json:"request_id"`
	OrgID       int                `json:"org_id"`
	TeamID      int                `json:"team_id"`
	Action      RequestEventAction `json:"action"`
	FromStatus  RequestStatus      `json:"from_status,omitempty"`
	ToStatus    RequestStatus      `json:"to_status,omitempty"`
	Actor       string             `json:"actor"`
	ActorUserID *int               `json:"actor_user_id"`
	Detail      string             `json:"detail,omitempty"`
	CreatedAt   time.Time          `json:"created_at"`
}

// RequestLink is the durable relation from a request to planned or delivered
// work, to the project it is routed to, or to another request. It is a typed
// row, never metadata. There is no foreign key on TargetID by design: a link
// outlives the ticket or artifact it names, so the target's deletion
// tombstones the link instead of erasing the lineage.
type RequestLink struct {
	ID         int64             `json:"id"`
	RequestID  int               `json:"request_id"`
	LinkKind   RequestLinkKind   `json:"link_kind"`
	TargetKind RequestTargetKind `json:"target_kind"`
	TargetID   int               `json:"target_id"`
	CreatedBy  *int              `json:"created_by"`
	CreatedAt  time.Time         `json:"created_at"`
}

// Validate normalizes a comment's kind and body: a known kind, a non-empty
// body bounded like a brief.
func (c *RequestComment) Validate() error {
	kind, err := NormalizeRequestCommentKind(string(c.Kind))
	if err != nil {
		return err
	}
	body, err := NormalizeRequestBrief("body", c.Body)
	if err != nil {
		return err
	}
	if body == "" {
		return errors.New("body is required: a comment says something")
	}
	c.Kind, c.Body = kind, body
	return nil
}

// Validate normalizes an event's action and statuses: a known action, and any
// from/to state a known one. A transition names both ends; the other actions
// name neither.
func (e *RequestEvent) Validate() error {
	action, err := NormalizeRequestEventAction(string(e.Action))
	if err != nil {
		return err
	}
	if e.FromStatus != "" {
		if e.FromStatus, err = NormalizeRequestStatus(string(e.FromStatus)); err != nil {
			return err
		}
	}
	if e.ToStatus != "" {
		if e.ToStatus, err = NormalizeRequestStatus(string(e.ToStatus)); err != nil {
			return err
		}
	}
	if action == RequestActionTransition && (e.FromStatus == "" || e.ToStatus == "") {
		return errors.New("a transition event names the state it left and the state it entered")
	}
	if action != RequestActionTransition && (e.FromStatus != "" || e.ToStatus != "") {
		return fmt.Errorf("only a transition event names states; a %s event explains itself in detail", action)
	}
	e.Action = action
	return nil
}

// Validate normalizes a link kind and target: the allowed pair, and a positive
// target id. Whether the caller may link at all, and whether the target is in
// the same org, is the service's check.
func (l *RequestLink) Validate() error {
	kind, target, err := NormalizeRequestLink(string(l.LinkKind), string(l.TargetKind))
	if err != nil {
		return err
	}
	if l.TargetID <= 0 {
		return errors.New("target_id must name the linked record")
	}
	if target == RequestTargetRequest && l.TargetID == l.RequestID {
		return errors.New("a request cannot link to itself")
	}
	l.LinkKind, l.TargetKind = kind, target
	return nil
}

// Claimed reports whether a triager holds the request's triage claim. A claim
// never expires by time (D4): only a release or an assignee transfer ends it.
func (r *TeamRequest) Claimed() bool { return r.ClaimUserID != nil }

// Validate normalizes the record's text fields in place and checks its shape:
// known vocabularies, bounded lengths, the org/team scope, the two ownership
// fields staying consistent, and the terminal states carrying what they must
// (a duplicate its canonical, a rejection its reason). Where a request may go
// next, and who may move it, is the service transition function's job.
func (r *TeamRequest) Validate() error {
	if r.OrgID <= 0 || r.TeamID <= 0 {
		return errors.New("a request belongs to exactly one org and one team")
	}
	if r.Rev < 1 {
		return errors.New("rev is the concurrency token and starts at 1")
	}
	if r.ReopenedCount < 0 {
		return errors.New("reopened_count is never negative")
	}
	ref, err := NormalizeRequestPublicRef(r.PublicRef)
	if err != nil {
		return err
	}
	subject, err := NormalizeRequestSubject(r.Subject)
	if err != nil {
		return err
	}
	written, err := NormalizeRequestBrief("input_brief", r.InputBrief)
	if err != nil {
		return err
	}
	outcome, err := NormalizeRequestBrief("outcome_brief", r.OutcomeBrief)
	if err != nil {
		return err
	}
	reject, err := boundedRequestText("reject_reason", r.RejectReason, MaxRequestRejectReasonLen)
	if err != nil {
		return err
	}
	contact, err := NormalizeRequestContact(r.RequesterContact)
	if err != nil {
		return err
	}
	kind, externalID, err := NormalizeRequestSource(string(r.SourceKind), r.SourceExternalID)
	if err != nil {
		return err
	}
	state, err := NormalizeRequestStatus(string(r.State))
	if err != nil {
		return err
	}
	if r.RequesterUserID != nil && *r.RequesterUserID <= 0 {
		return errors.New("requester_user_id must be a user id")
	}
	if contact == "" {
		return errors.New("requester_contact is required: it is the contact snapshot taken at submit, so deleting the user never erases who asked")
	}
	// The claim and the acceptance record who and when as one-way implications,
	// never a biconditional: every user foreign key is ON DELETE SET NULL, so a
	// stored row may keep the moment after the person's row is gone.
	if r.ClaimUserID != nil && r.ClaimedAt == nil {
		return errors.New("a triage claim records when it was taken (claimed_at)")
	}
	if r.ClaimUserID != nil && *r.ClaimUserID <= 0 {
		return errors.New("claim_user_id must be a user id")
	}
	if r.OwnerUserID != nil && *r.OwnerUserID <= 0 {
		return errors.New("owner_user_id must be a user id")
	}
	if r.DuplicateOf != nil {
		if *r.DuplicateOf <= 0 {
			return errors.New("duplicate_of must be a request id")
		}
		if *r.DuplicateOf == r.ID {
			return errors.New("a request cannot be a duplicate of itself")
		}
	}
	// Terminal states carry what makes them honest (I2, I3), and an entry into a
	// state records its first-write-wins timestamp. The service stamps these in
	// the same write that moves the state; 053's CHECKs are the backstop, so a
	// record this accepts can always be stored.
	if reject == "" && state == RequestStatusRejected {
		return errors.New("a rejected request carries reject_reason: the requester sees why it was declined")
	}
	if state == RequestStatusDuplicate && r.DuplicateOf == nil {
		return errors.New("a duplicate must name the canonical request in duplicate_of")
	}
	if state == RequestStatusNeedsInformation && r.NeedsInfoSince == nil {
		return errors.New("needs_information records since when it has been waiting (needs_info_since)")
	}
	if state == RequestStatusDelivered || state == RequestStatusRequesterAccepted {
		if outcome == "" {
			return errors.New("delivered is evidenced: outcome_brief says what is and is not included")
		}
		if r.DeliveredAt == nil {
			return errors.New("delivered records delivered_at")
		}
	}
	if r.AcceptedBy != nil {
		if *r.AcceptedBy <= 0 {
			return errors.New("accepted_by must be a user id")
		}
		if r.AcceptedAt == nil {
			return errors.New("accepted_by records when the acceptance happened (accepted_at)")
		}
		if r.RequesterUserID != nil && *r.AcceptedBy != *r.RequesterUserID {
			return errors.New("accepted_by is the requester: acceptance is the requester's alone, and no on-behalf acceptance ships in v1")
		}
	}
	if state == RequestStatusRequesterAccepted && r.AcceptedAt == nil {
		return errors.New("requester_accepted records accepted_at")
	}

	r.PublicRef, r.Subject = ref, subject
	r.InputBrief, r.OutcomeBrief, r.RejectReason = written, outcome, reject
	r.RequesterContact = contact
	r.SourceKind, r.SourceExternalID = kind, externalID
	r.State = state
	return nil
}

// NormalizeRequestStatus validates a lifecycle state (" Triaging " is
// triaging). An empty state means new: intake lands there.
func NormalizeRequestStatus(s string) (RequestStatus, error) {
	st := RequestStatus(strings.ToLower(strings.TrimSpace(s)))
	if st == "" {
		return RequestStatusNew, nil
	}
	if !slices.Contains(RequestStatuses, st) {
		return "", fmt.Errorf("status must be one of %s", requestNames(RequestStatuses))
	}
	return st, nil
}

// NormalizeRequestCommentKind validates a discussion kind; empty means a
// plain comment.
func NormalizeRequestCommentKind(s string) (RequestCommentKind, error) {
	k := RequestCommentKind(strings.ToLower(strings.TrimSpace(s)))
	if k == "" {
		return RequestCommentDiscussion, nil
	}
	if !slices.Contains(RequestCommentKinds, k) {
		return "", fmt.Errorf("comment kind must be one of %s", requestNames(RequestCommentKinds))
	}
	return k, nil
}

// NormalizeRequestEventAction validates a history action.
func NormalizeRequestEventAction(s string) (RequestEventAction, error) {
	a := RequestEventAction(strings.ToLower(strings.TrimSpace(s)))
	if !slices.Contains(RequestEventActions, a) {
		return "", fmt.Errorf("event action must be one of %s", requestNames(RequestEventActions))
	}
	return a, nil
}

// NormalizeRequestLink validates a link kind and the kind of target it points
// at, refusing a pair the contract never allows (a planned_as link to an
// artifact, a routes_to link to a ticket).
func NormalizeRequestLink(kind, targetKind string) (RequestLinkKind, RequestTargetKind, error) {
	k := RequestLinkKind(strings.ToLower(strings.TrimSpace(kind)))
	targets, ok := requestLinkTargets[k]
	if !ok {
		return "", "", fmt.Errorf("link kind must be one of %s", requestNames(RequestLinkKinds))
	}
	t := RequestTargetKind(strings.ToLower(strings.TrimSpace(targetKind)))
	if !slices.Contains(targets, t) {
		return "", "", fmt.Errorf("a %s link points at %s, not %q", k, requestNames(targets), targetKind)
	}
	return k, t, nil
}

// NormalizeRequestSource validates where a request came from and its external
// id. An empty kind is a web submission. A connector must carry the vendor's
// immutable object id (that is what dedups a replay), and a web form must not:
// its retries are the submit idempotency key, a different mechanism.
func NormalizeRequestSource(kind, externalID string) (RequestSourceKind, string, error) {
	k := RequestSourceKind(strings.ToLower(strings.TrimSpace(kind)))
	if k == "" {
		k = RequestSourceWeb
	}
	if !slices.Contains(RequestSourceKinds, k) {
		return "", "", fmt.Errorf("source_kind must be one of %s", requestNames(RequestSourceKinds))
	}
	id, err := boundedRequestText("source_external_id", externalID, MaxRequestExternalIDLen)
	if err != nil {
		return "", "", err
	}
	switch k {
	case RequestSourceConnector:
		if id == "" {
			return "", "", errors.New("source_external_id is required for a connector source: the vendor's immutable object id is what dedups its replay")
		}
	case RequestSourceWeb:
		if id != "" {
			return "", "", errors.New("source_external_id belongs to connector sources only; a web submission's retries use the idempotency key")
		}
	}
	return k, id, nil
}

// NormalizeRequestPublicRef validates the opaque reference the requester sees:
// 16 to 64 characters of letters, digits, "-" or "_" — the schema's shape
// exactly (053) — so a bare serial (149), a padded number (00000149) or the
// mockups' REQ-8F2Q1 display rendering can never be stored. Generation
// is the service's: it mints a random token (never a per-team sequence) and
// the database's unique constraint on public_ref keeps it unique. The display
// form's REQ- prefix is accepted and stripped, so a pasted reference
// normalizes.
func NormalizeRequestPublicRef(ref string) (string, error) {
	r := strings.TrimSpace(ref)
	if len(r) > len(RequestRefPrefix) && strings.EqualFold(r[:len(RequestRefPrefix)], RequestRefPrefix) {
		r = strings.TrimSpace(r[len(RequestRefPrefix):])
	}
	if r == "" {
		return "", errors.New("public_ref is required: it is the reference the requester sees")
	}
	// Characters and letters before length, so a bare serial is told why it
	// was refused rather than how long it is.
	letter := false
	for _, c := range r {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
			letter = true
		case c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return "", fmt.Errorf("public_ref may hold letters, digits, \"-\" and \"_\", not %q: it is an opaque token", c)
		}
	}
	if !letter {
		return "", errors.New("public_ref must not be a plain number: it is an opaque token, never a guessable sequence")
	}
	if n := utf8.RuneCountInString(r); n < MinRequestPublicRefLen || n > MaxRequestPublicRefLen {
		return "", fmt.Errorf("public_ref must be %d to %d characters, got %d", MinRequestPublicRefLen, MaxRequestPublicRefLen, n)
	}
	return r, nil
}

// NormalizeRequestSubject validates the one-line subject.
func NormalizeRequestSubject(s string) (string, error) {
	subject, err := boundedRequestText("subject", s, MaxRequestSubjectLen)
	if err != nil {
		return "", err
	}
	if subject == "" {
		return "", errors.New("subject is required: one line saying what the request is about")
	}
	return subject, nil
}

// NormalizeRequestBrief validates input_brief or outcome_brief (field names
// the error): markdown, trimmed, bounded, and allowed to be empty until it is
// needed.
func NormalizeRequestBrief(field, s string) (string, error) {
	return boundedRequestText(field, s, MaxRequestBriefLen)
}

// NormalizeRequestContact validates the requester contact snapshot: trimmed,
// bounded, one line. It is deliberately looser than an email address, because
// the contract allows an email or a handle from another tool.
func NormalizeRequestContact(s string) (string, error) {
	contact, err := boundedRequestText("requester_contact", s, MaxRequestContactLen)
	if err != nil {
		return "", err
	}
	for _, c := range contact {
		if unicode.IsSpace(c) || unicode.IsControl(c) {
			return "", errors.New("requester_contact must be a single email address or handle, with no spaces")
		}
	}
	return contact, nil
}

// boundedRequestText trims s and refuses it over limit characters, naming the
// field so the caller's error reads like the other validation messages.
func boundedRequestText(field, s string, limit int) (string, error) {
	s = strings.TrimSpace(s)
	if n := utf8.RuneCountInString(s); n > limit {
		return "", fmt.Errorf("%s is over %d characters (got %d)", field, limit, n)
	}
	return s, nil
}

// requestNames lists a typed vocabulary's values for an error message.
func requestNames[T ~string](names []T) string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = string(n)
	}
	return strings.Join(out, ", ")
}
