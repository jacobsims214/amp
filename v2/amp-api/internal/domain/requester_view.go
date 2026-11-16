package domain

// Requester-safe request projection: the requester-facing object and the
// structured inputs it is built from (epic 202, story 548, AMP #3954; approved
// product contract artifact 36 v2 §4; approved requester mockup artifact 38 v1;
// build slice E3 of discoveries/team-inbox-approved-build-slices.md).
//
// A requester is an invited customer with no org role and no project
// membership, so they see one request — their own — through a server-computed
// projection, never a record and never the tools that read the inbox. Contract
// §4 says what that object is: "state word, milestone counts, shared
// deliverables — never tickets, branches or agent identities".
//
// This file is the whitelist, and it is a *type*, not a filter. There is no
// field on RequesterRequestView for a project, an epic, a story, a ticket, its
// title or id, a branch, a merge state, a metadata key, a user id, an agent or
// worker identity, a claim, an assignee, a revision, a comment id, an audit or
// history row, or an unshared comment. A projection built from this type cannot
// leak them by forgetting a `json:"-"`: the only way a value reaches the
// requester is by a field that exists here, and each field is justified against
// the approved mockup in its own comment.
//
// Three boundaries are deliberate.
//
//  1. NO WORK ITEMS REACH THIS FILE. The inputs carry milestones, shared
//     comments and shared files — never a task, its state, a merge gate or a
//     "closed count". Delivery therefore cannot be inferred from finished
//     tickets even by accident: there is nothing here to count. A delivered
//     milestone that the request's own lifecycle state has not reached is
//     refused below (RequesterMilestoneDoneAllowed), so no upstream aggregate
//     can declare delivery through this projection either. The request is
//     delivered when the team's `delivered` transition says so (contract §4);
//     that is a lifecycle act, never a derivation.
//
//  2. INPUTS ARE ALREADY-AUTHORIZED FACTS. Who may see the request is decided
//     before the projection runs (authz.RequestViewFor / InboxRole, slice B4);
//     which comments the team chose to share and which files it shared are the
//     read path's decisions. This file assumes none of that: it consumes the
//     facts it is handed and whitelists, sanitizes and fails closed. Nothing
//     here reads a URL, a principal or a database, so a visibility decision
//     cannot be smuggled in through the projection.
//
//  3. MISSING IS SAFE, NEVER EMPTY-SUCCESS. A file whose download reference is
//     absent is not "ready"; an output the team did not share is not in the
//     list at all; an unknown vocabulary value is dropped rather than passed
//     through. Every degradation in this file moves toward showing the
//     requester *less*, which is the only direction an uncertain projection may
//     move.

import (
	"strings"
	"time"
	"unicode/utf8"
)

// RequesterRequestView is one request as its own requester sees it. Every field
// maps to something the approved requester mockup (artifact 38 v1) renders or
// the contract requires the requester to be told; nothing else is here.
type RequesterRequestView struct {
	// PublicRef is the opaque reference the requester quotes (D1) — the same
	// token the submission answered with, never the serial.
	PublicRef string `json:"public_ref"`

	// State is the lifecycle state in the contract's own nine-value vocabulary
	// and StateWord is the word the approved mockup shows for it (the tag in
	// the header and the list row). A UI branches on State and displays
	// StateWord; neither is an internal identity.
	State     RequestStatus `json:"state"`
	StateWord string        `json:"state_word"`

	// Subject and InputBrief are the requester's own words. The record is
	// immutable (contract §1), so this is what they asked for, never a team
	// rewrite of it.
	Subject    string `json:"subject"`
	InputBrief string `json:"input_brief"`

	// OutcomeBrief is what the team said it delivered — contract §4's "what is
	// and is not included" (the mockup's "What the desk delivered"). It is
	// present only once the request is delivered: an outcome brief written
	// mid-flight stays hidden, because delivery is a lifecycle act, not a text
	// field.
	OutcomeBrief string `json:"outcome_brief,omitempty"`

	// RejectReason is why a declined request was declined. The domain's own
	// invariant is that a rejected request carries one, and the requester is
	// the party who needs it.
	RejectReason string `json:"reject_reason,omitempty"`

	// TeamName is the desk's display name — the one team fact the mockup shows
	// ("Northwind support desk"). The read path supplies it; nothing here looks
	// a team up.
	TeamName string `json:"team_name,omitempty"`

	// The timestamps a requester is told: when they submitted it (the mockup's
	// "opened Nov 8"), when the desk last asked them something, and when it was
	// delivered, accepted and closed. Entry timestamps are first-write-wins, so
	// a repeated report never moves one.
	CreatedAt       time.Time  `json:"created_at"`
	NeedsReplySince *time.Time `json:"needs_reply_since,omitempty"`
	DeliveredAt     *time.Time `json:"delivered_at,omitempty"`
	AcceptedAt      *time.Time `json:"accepted_at,omitempty"`
	ClosedAt        *time.Time `json:"closed_at,omitempty"`

	// Milestones is the public stepper ("Received · Being reviewed · In
	// progress · Delivered · You confirm"), built from the aggregate's explicit
	// input — this projection invents no step and counts nothing.
	Milestones []RequesterMilestone `json:"milestones"`

	// Comments is the discussion the team chose to share with the requester,
	// attributed only as "you" or "the desk".
	Comments []RequesterComment `json:"comments"`

	// Outputs is the files on this request: the requester's own uploads and the
	// outputs the team explicitly shared, each with a download reference only
	// when it may actually be downloaded.
	Outputs []RequesterOutput `json:"outputs"`

	// Question is the open question the desk asked when it needs more from the
	// requester (state needs_information). It is absent when the team shared no
	// question, which is the honest answer: this projection never manufactures
	// one.
	Question *RequesterQuestion `json:"question,omitempty"`
}

// RequesterMilestoneKey is one step of a request's public progress. The set is
// closed and derived from the approved mockup's stepper — five steps, no
// ticket, project or phase name among them.
type RequesterMilestoneKey string

const (
	// RequesterMilestoneReceived: the desk has the request (the mockup's
	// "Received", the submit's own moment).
	RequesterMilestoneReceived RequesterMilestoneKey = "received"
	// RequesterMilestoneReviewing: triage is looking at it ("Being reviewed").
	RequesterMilestoneReviewing RequesterMilestoneKey = "reviewing"
	// RequesterMilestoneInProgress: the desk accepted it and work is underway
	// ("In progress", "Work started").
	RequesterMilestoneInProgress RequesterMilestoneKey = "in_progress"
	// RequesterMilestoneDelivered: the team delivered an outcome ("Delivered —
	// your call").
	RequesterMilestoneDelivered RequesterMilestoneKey = "delivered"
	// RequesterMilestoneAccepted: the requester confirmed it ("You confirm",
	// "Accepted").
	RequesterMilestoneAccepted RequesterMilestoneKey = "accepted"
)

// RequesterMilestoneKeys is the closed step vocabulary, in stepper order.
var RequesterMilestoneKeys = []RequesterMilestoneKey{
	RequesterMilestoneReceived, RequesterMilestoneReviewing,
	RequesterMilestoneInProgress, RequesterMilestoneDelivered, RequesterMilestoneAccepted,
}

// RequesterMilestone is one public step: the server says whether it happened
// and when, and may attach one sanitized sentence the requester can read. The
// step's label is the UI's copy, not ours — the word a person sees is product
// text, and shipping it from the API would pin a translation to this type.
type RequesterMilestone struct {
	Key  RequesterMilestoneKey `json:"key"`
	Done bool                  `json:"done"`
	// At is when the step happened, present only for a done step.
	At *time.Time `json:"at,omitempty"`
	// Note is a public sentence about the step ("We reproduced the mismatch and
	// are re-running the export"). It is a claim from the aggregate, bounded and
	// sanitized like every other text this file emits.
	Note string `json:"note,omitempty"`
}

// RequesterAuthor is who wrote a shared comment, reduced to the only two
// attributions a requester may see. Internal owners, triagers, agents, workers
// and service clients are all "the desk": which person on the team wrote it is
// the team's business, and an audit identity is never the requester's.
type RequesterAuthor string

const (
	// RequesterAuthorYou: the requester wrote it.
	RequesterAuthorYou RequesterAuthor = "you"
	// RequesterAuthorDesk: the team side wrote it, whoever that was.
	RequesterAuthorDesk RequesterAuthor = "desk"
)

// RequesterAuthors is the closed attribution vocabulary.
var RequesterAuthors = []RequesterAuthor{RequesterAuthorYou, RequesterAuthorDesk}

// RequesterComment is one comment the team chose to share, as the requester
// sees it: the kind (a question is what the answer box answers), the body
// sanitized as text, who wrote it in the two-word attribution above, and when.
// No comment id, request id, author user id, author display name or edit
// history is here.
type RequesterComment struct {
	Kind RequestCommentKind `json:"kind"`
	From RequesterAuthor    `json:"from"`
	Body string             `json:"body"`
	At   time.Time          `json:"at"`
}

// RequesterQuestion is the desk's open question — the mockup's "The desk needs
// a bit more from you" card: the question itself and when it was asked.
type RequesterQuestion struct {
	Body string    `json:"body"`
	At   time.Time `json:"at"`
}

// RequesterOutputSource is whose file an output is: the requester's own upload,
// or something the team explicitly shared as a delivered output. The mockup
// separates the two panels ("What you asked for" / "What the desk delivered"),
// so one list with a source is enough — and it keeps every output on the same
// download rules.
type RequesterOutputSource string

const (
	// RequesterOutputFromRequester: the requester uploaded it to this request.
	RequesterOutputFromRequester RequesterOutputSource = "you"
	// RequesterOutputFromTeam: the team explicitly shared it with the
	// requester. An output the team did not share is not in the list at all.
	RequesterOutputFromTeam RequesterOutputSource = "desk"
)

// RequesterOutputSources is the closed source vocabulary.
var RequesterOutputSources = []RequesterOutputSource{
	RequesterOutputFromRequester, RequesterOutputFromTeam,
}

// RequesterOutputStatus is the only status word a requester sees about a file.
// It is deliberately three words and no domain state: an artifact's
// in_review/approved/changes_requested, an attachment's scan_state and its
// restricted-file hold are internal facts, and the mockup shows a single
// "Ready" tag (or "Scanning") rather than any of them.
type RequesterOutputStatus string

const (
	// RequesterOutputReady: the file may be downloaded right now. It is the
	// only status that carries a download reference — see the invariant in
	// requesterOutputs.
	RequesterOutputReady RequesterOutputStatus = "ready"
	// RequesterOutputPreparing: the file exists but is not downloadable yet
	// (being scanned, held for a lead's release, still being produced).
	RequesterOutputPreparing RequesterOutputStatus = "preparing"
	// RequesterOutputUnavailable: the file cannot be served (refused, purged,
	// or a state nothing can vouch for). It is the fail-closed default.
	RequesterOutputUnavailable RequesterOutputStatus = "unavailable"
)

// RequesterOutputStatuses is the closed status vocabulary.
var RequesterOutputStatuses = []RequesterOutputStatus{
	RequesterOutputReady, RequesterOutputPreparing, RequesterOutputUnavailable,
}

// RequesterOutput is one file the requester may see on their request: a display
// name, a size, one of the three status words, when it arrived, and — only when
// it is ready — an opaque download reference.
//
// The reference is opaque on purpose. It is whatever the read path minted (a
// short-lived signed grant, per contract §1 and slice G3), never an attachment
// or artifact row id: a serial is guessable, and the requester surface must
// never teach anyone to enumerate one. The projection passes the reference
// through untouched and refuses to pair it with a status that is not ready.
type RequesterOutput struct {
	Source    RequesterOutputSource `json:"source"`
	Name      string                `json:"name"`
	SizeBytes int64                 `json:"size_bytes"`
	Status    RequesterOutputStatus `json:"status"`
	// At is when the file was uploaded or delivered.
	At *time.Time `json:"at,omitempty"`
	// DownloadRef is the opaque handle the download route accepts, present only
	// for a ready file.
	DownloadRef string `json:"download_ref,omitempty"`
}

// ---- The projection's inputs ----
//
// These are the facts the read path hands over: already authorized, already
// filtered to what the team chose to share, already computed by the outcome
// aggregate (slice E2). Naming them here is the contract between that aggregate
// and this projection; the projection itself computes no outcome.

// RequesterMilestoneInput is one milestone the aggregate established: its step,
// whether it happened, when, and one optional public sentence.
type RequesterMilestoneInput struct {
	Key  RequesterMilestoneKey
	Done bool
	At   *time.Time
	Note string
}

// RequesterCommentInput is one comment the team chose to share, with the one
// attribution the projection needs. FromRequester is set from the stored
// author's identity by the read path — an internal owner, an agent, a worker or
// a connector's service client all answer false, and all become "the desk".
type RequesterCommentInput struct {
	Kind          RequestCommentKind
	Body          string
	FromRequester bool
	At            time.Time
}

// RequesterOutputInput is one file the read path decided the requester may see.
// Status is the read path's mapping of the file's internal state onto the three
// public words; the projection validates it, and a ready file without a
// DownloadRef degrades to unavailable rather than advertising a download that
// cannot happen.
type RequesterOutputInput struct {
	Source      RequesterOutputSource
	Name        string
	SizeBytes   int64
	Status      RequesterOutputStatus
	At          *time.Time
	DownloadRef string
}

// ---- Decisions ----

// RequesterStateWord is the word the approved requester mockup shows for a
// lifecycle state. The six content words come straight from artifact 38 v1;
// the two terminal states have no mockup word of their own and share the
// mockup's list filter "Closed", which is also what a requester sees on a
// request the desk will not proceed with. An unrecognised state answers ""
// rather than guessing a word — the vocabulary is closed by the schema, so an
// empty word is a bug made visible instead of a state invented.
func RequesterStateWord(status RequestStatus) string {
	switch status {
	case RequestStatusNew:
		return "Received"
	case RequestStatusTriaging:
		return "Being reviewed"
	case RequestStatusNeedsInformation:
		return "Needs your reply"
	case RequestStatusAccepted, RequestStatusInProgress:
		return "In progress"
	case RequestStatusDelivered:
		return "Delivered — your call"
	case RequestStatusRequesterAccepted:
		return "Accepted"
	case RequestStatusRejected, RequestStatusDuplicate:
		return "Closed"
	}
	return ""
}

// RequesterMilestoneDoneAllowed reports whether a milestone may be shown as
// done at a lifecycle state — the gate that keeps this projection from
// declaring delivery nobody stated.
//
// It is the earliest state at which the step can honestly be true: the request
// has been received from `new` onward, is being reviewed once triage began, is
// in progress once the desk accepted it, is delivered only in the two delivered
// states, and is accepted only in requester_accepted. A `delivered` milestone
// from any aggregate — however many of the request's tickets are complete, and
// whatever the merge gates and receipts say — therefore renders as a *pending*
// step until the request's own lifecycle says delivered. That is contract §4's
// "a closing ticket never flips a request" enforced at the last layer that could
// break it.
//
// "Received" is the one step allowed at every state, including one this file
// does not recognise: a record that exists was received, so that claim is
// certain whatever the state column says. Every other step is a closed list, and
// a state outside the vocabulary reaches none of them.
func RequesterMilestoneDoneAllowed(key RequesterMilestoneKey, state RequestStatus) bool {
	switch key {
	case RequesterMilestoneReceived:
		return true
	case RequesterMilestoneReviewing:
		// Triage began: every state but a brand-new request.
		switch state {
		case RequestStatusTriaging, RequestStatusNeedsInformation, RequestStatusAccepted,
			RequestStatusInProgress, RequestStatusDelivered, RequestStatusRequesterAccepted,
			RequestStatusRejected, RequestStatusDuplicate:
			return true
		}
		return false
	case RequesterMilestoneInProgress:
		switch state {
		case RequestStatusAccepted, RequestStatusInProgress,
			RequestStatusDelivered, RequestStatusRequesterAccepted:
			return true
		}
		return false
	case RequesterMilestoneDelivered:
		return state == RequestStatusDelivered || state == RequestStatusRequesterAccepted
	case RequesterMilestoneAccepted:
		return state == RequestStatusRequesterAccepted
	}
	return false
}

// RequesterNote is one string as the requester projection emits it: trimmed,
// sanitized (SanitizeRequesterText) and bounded to MaxRequestBriefLen. Every
// free-text field of the view goes through it, so no note can carry a control
// sequence, an invisible bidi override, or an unbounded body into the requester
// surface. It is the projection's only text path.
func RequesterNote(s string) string {
	s = strings.TrimSpace(SanitizeRequesterText(s))
	if utf8.RuneCountInString(s) <= MaxRequestBriefLen {
		return s
	}
	// Cut on a rune boundary, never mid-sequence, so the result stays valid
	// UTF-8 and valid JSON.
	runes := []rune(s)
	return string(runes[:MaxRequestBriefLen])
}

// SanitizeRequesterText makes one user-authored or team-authored string safe to
// hand to a requester surface as *text*, and changes nothing it does not have to.
//
// What it removes is only ever invisible: control characters that could drive a
// terminal or a log, and the Unicode formatting characters that let one string
// impersonate another — bidi overrides and isolates (the "exactly what is
// written is not what is shown" attack), left/right marks, zero-width spaces
// and word joiners. What it keeps is everything readable, including markdown,
// HTML-looking text (a request that contains `<script>` is a request that
// contains those characters, and it is rendered by the shared Markdown
// component, which renders remote images inert — D13), the requester's own
// emoji (ZWJ sequences survive: only the direction and zero-width controls are
// dropped), and every line break a brief uses.
//
// Invalid UTF-8 is replaced rather than carried, so the value is always valid
// JSON. This is not an HTML escaper and must not become one: escaping here
// would double-escape markdown at render time, and the serializer already
// escapes `<`, `>` and `&` for transport.
func SanitizeRequesterText(s string) string {
	if s == "" {
		return ""
	}
	valid := strings.ToValidUTF8(s, "")
	var b strings.Builder
	b.Grow(len(valid))
	for _, r := range valid {
		if requesterTextRune(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// requesterTextRune reports whether one rune survives SanitizeRequesterText.
// The ranges are the invisible ones; everything else is text the requester may
// write or be shown.
func requesterTextRune(r rune) bool {
	switch {
	case r == '\n' || r == '\t':
		return true
	case r < 0x20, r == 0x7F:
		// C0 controls (ESC, BEL, NUL, CR…) and DEL.
		return false
	case r >= 0x80 && r <= 0x9F:
		// C1 controls.
		return false
	case r == 0x2028 || r == 0x2029:
		// Line/paragraph separator: a line break no one typed.
		return false
	case r == 0x200B:
		// Zero-width space: hides characters from a reader.
		return false
	case r == 0x200E || r == 0x200F:
		// Left-to-right / right-to-left mark.
		return false
	case r >= 0x202A && r <= 0x202E:
		// Bidi embedding and override.
		return false
	case r >= 0x2066 && r <= 0x2069:
		// Bidi isolates.
		return false
	case r >= 0x2060 && r <= 0x2064:
		// Word joiner and invisible operators.
		return false
	case r == 0xFEFF:
		// Byte-order mark used as a zero-width no-break space.
		return false
	}
	return true
}
