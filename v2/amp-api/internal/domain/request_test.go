package domain

import (
	"go/parser"
	"go/token"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A request's typed shape (contract artifact 36 v2, build slice A1): the nine
// approved states, the fixed vocabularies, the bounded intake values, and the
// opacity of the public reference. Legal transitions are the service's tests.

func validRequest() *TeamRequest {
	claimed := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	assignee := 12
	return &TeamRequest{
		ID: 7, PublicRef: "8f2q1x7k4m2p9t5w", OrgID: 1, TeamID: 4,
		RequesterUserID:  ptrInt(31),
		RequesterContact: "dana@example.com",
		SourceKind:       RequestSourceWeb,
		Subject:          "Team inbox: show quiet tickets first",
		InputBrief:       "Every morning I scroll 40 tickets to find the three nobody touched.",
		OutcomeBrief:     "",
		State:            RequestStatusTriaging,
		ClaimUserID:      ptrInt(9),
		ClaimedAt:        &claimed,
		OwnerUserID:      &assignee,
		Rev:              3,
		Metadata:         Metadata{"jira_key": "PLAT-88"},
	}
}

// The nine states are the contract's, in lifecycle order, and nothing else is
// a state: a tenth value is refused by NormalizeRequestStatus.
func TestRequestStatuses(t *testing.T) {
	want := []string{
		"new", "triaging", "needs_information", "accepted", "in_progress",
		"delivered", "requester_accepted", "rejected", "duplicate",
	}
	if got := len(RequestStatuses); got != len(want) {
		t.Fatalf("RequestStatuses has %d values, want the approved nine: %v", got, RequestStatuses)
	}
	for i, w := range want {
		if string(RequestStatuses[i]) != w {
			t.Errorf("RequestStatuses[%d] = %q, want %q", i, RequestStatuses[i], w)
		}
		got, err := NormalizeRequestStatus(w)
		if err != nil || string(got) != w {
			t.Errorf("NormalizeRequestStatus(%q) = %q, %v", w, got, err)
		}
	}
	for _, bad := range []string{"archived", "closed", "in-progress", "in progress", "done"} {
		if got, err := NormalizeRequestStatus(bad); err == nil {
			t.Errorf("NormalizeRequestStatus(%q) = %q, want an error naming the nine states", bad, got)
		} else if !strings.Contains(err.Error(), "requester_accepted") {
			t.Errorf("NormalizeRequestStatus(%q) error %q should name the allowed states", bad, err)
		}
	}
	if got, err := NormalizeRequestStatus(""); err != nil || got != RequestStatusNew {
		t.Errorf("empty status = %q, %v; want new", got, err)
	}
	if got, err := NormalizeRequestStatus(" Triaging "); err != nil || got != RequestStatusTriaging {
		t.Errorf("padded status = %q, %v; want triaging", got, err)
	}
}

// Every vocabulary is closed: an unknown comment kind, event action, source,
// link kind or target is refused, with the allowed values in the message.
func TestRequestVocabulariesRejectUnknown(t *testing.T) {
	if _, err := NormalizeRequestCommentKind("note"); err == nil {
		t.Error("comment kind \"note\" accepted; want refused")
	}
	for _, k := range RequestCommentKinds {
		if got, err := NormalizeRequestCommentKind(string(k)); err != nil || got != k {
			t.Errorf("NormalizeRequestCommentKind(%q) = %q, %v", k, got, err)
		}
	}
	if got, err := NormalizeRequestCommentKind(""); err != nil || got != RequestCommentDiscussion {
		t.Errorf("empty comment kind = %q, %v; want comment", got, err)
	}

	if _, err := NormalizeRequestEventAction("merge"); err == nil {
		t.Error("event action \"merge\" accepted; want refused")
	}
	for _, a := range RequestEventActions {
		if got, err := NormalizeRequestEventAction(string(a)); err != nil || got != a {
			t.Errorf("NormalizeRequestEventAction(%q) = %q, %v", a, got, err)
		}
	}

	if _, _, err := NormalizeRequestLink("owns", "task"); err == nil {
		t.Error("link kind \"owns\" accepted; want refused")
	}
	for _, k := range RequestLinkKinds {
		if got, _, err := NormalizeRequestLink(string(k), ""); err == nil {
			t.Errorf("NormalizeRequestLink(%q, \"\") = %q, want the target refused too", k, got)
		}
	}
}

// A link kind only points at the targets the contract allows: routes_to at one
// project, planned_as at epics/stories/tasks, delivered_by at
// tasks/artifacts/receipts, duplicate_of and relates_to at requests.
func TestRequestLinkTargets(t *testing.T) {
	allowed := map[RequestLinkKind][]RequestTargetKind{
		RequestLinkRoutesTo:    {RequestTargetProject},
		RequestLinkPlannedAs:   {RequestTargetEpic, RequestTargetStory, RequestTargetTask},
		RequestLinkDeliveredBy: {RequestTargetTask, RequestTargetArtifact, RequestTargetReceipt},
		RequestLinkDuplicateOf: {RequestTargetRequest},
		RequestLinkRelatesTo:   {RequestTargetRequest},
	}
	for kind, targets := range allowed {
		for _, target := range RequestTargetKinds {
			_, gotTarget, err := NormalizeRequestLink(string(kind), string(target))
			ok := false
			for _, a := range targets {
				if target == a {
					ok = true
				}
			}
			if ok && err != nil {
				t.Errorf("%s -> %s refused: %v", kind, target, err)
			}
			if !ok && err == nil {
				t.Errorf("%s -> %s accepted, want refused", kind, target)
			}
			if ok && gotTarget != target {
				t.Errorf("%s -> %s normalized to %q", kind, target, gotTarget)
			}
		}
	}
	// Exactly the contract's five kinds and seven targets: no extras.
	if len(requestLinkTargets) != len(RequestLinkKinds) {
		t.Errorf("requestLinkTargets covers %d kinds, want %d", len(requestLinkTargets), len(RequestLinkKinds))
	}
	if len(RequestTargetKinds) != 7 {
		t.Errorf("RequestTargetKinds = %v, want project|epic|story|task|artifact|receipt|request", RequestTargetKinds)
	}
}

// The public reference is opaque: a serial or a zero-padded number is refused,
// the display prefix normalizes away, and the length is bounded.
func TestNormalizeRequestPublicRef(t *testing.T) {
	for _, ok := range []string{"8f2q1x7k4m2p9t5w", "REQ-8f2q1x7k4m2p9t5w", "req-8F2Q1X7K4M2P9T5W", "Ab-12_cd9efgh76_"} {
		if got, err := NormalizeRequestPublicRef(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		} else if strings.HasPrefix(strings.ToLower(got), "req-") {
			t.Errorf("%q normalized to %q, want the display prefix stripped", ok, got)
		}
	}
	if got, err := NormalizeRequestPublicRef("  REQ-8F2q1X7k4M2p9T5w  "); err != nil || got != "8F2q1X7k4M2p9T5w" {
		t.Errorf("padded display ref = %q, %v; want 8F2q1X7k4M2p9T5w", got, err)
	}
	for name, want := range map[string]string{
		"":                      "required",
		"149":                   "opaque token",
		"00000149":              "opaque token",
		"8f2q1x7k":              "16 to 64",
		strings.Repeat("a", 65): "16 to 64",
		"8f2q1x7k4m2p9:t5w":     "letters, digits",
		"8f2q1x7k4m2p9t5w!":     "letters, digits",
		// The mockups' display rendering is never the stored token.
		"REQ-8F2Q1": "16 to 64",
	} {
		if got, err := NormalizeRequestPublicRef(name); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("NormalizeRequestPublicRef(%q) = %q, %v; want an error containing %q", name, got, err, want)
		}
	}
}

// Intake values are bounded: the subject, the briefs, the contact snapshot and
// a connector's external id each refuse an over-long or malformed value.
func TestNormalizeRequestBounds(t *testing.T) {
	long := strings.Repeat("x", MaxRequestSubjectLen+1)
	if _, err := NormalizeRequestSubject(long); err == nil || !strings.Contains(err.Error(), "subject is over") {
		t.Errorf("over-long subject: %v, want a subject error", err)
	}
	if _, err := NormalizeRequestSubject("   "); err == nil {
		t.Error("blank subject accepted; want required")
	}
	if got, err := NormalizeRequestSubject("  Fix the queue  "); err != nil || got != "Fix the queue" {
		t.Errorf("subject = %q, %v; want trimmed", got, err)
	}
	// Multi-byte text counts characters, not bytes.
	if _, err := NormalizeRequestSubject(strings.Repeat("é", MaxRequestSubjectLen)); err != nil {
		t.Errorf("%d accented characters refused: %v", MaxRequestSubjectLen, err)
	}

	if _, err := NormalizeRequestBrief("input_brief", strings.Repeat("x", MaxRequestBriefLen+1)); err == nil ||
		!strings.Contains(err.Error(), "input_brief is over") {
		t.Errorf("over-long input_brief: %v, want a field-naming error", err)
	}
	if got, err := NormalizeRequestBrief("outcome_brief", ""); err != nil || got != "" {
		t.Errorf("empty outcome_brief = %q, %v; want allowed", got, err)
	}

	if MaxRequestContactLen != 500 {
		t.Errorf("MaxRequestContactLen = %d; migration 053 pins requester_contact at 500", MaxRequestContactLen)
	}
	if _, err := NormalizeRequestContact("dana@example.com "); err != nil {
		t.Errorf("trailing space in contact refused: %v", err)
	}
	for _, bad := range []string{"dana @example.com", "dana\tx", strings.Repeat("a", MaxRequestContactLen+1)} {
		if got, err := NormalizeRequestContact(bad); err == nil {
			t.Errorf("contact %q accepted as %q; want refused", bad, got)
		}
	}
}

// A connector names the vendor's immutable id (it is what dedups a replay) and
// a web submission does not; an unknown source is refused.
func TestNormalizeRequestSource(t *testing.T) {
	if got, id, err := NormalizeRequestSource("", ""); err != nil || got != RequestSourceWeb || id != "" {
		t.Errorf("empty source = %q, %q, %v; want web", got, id, err)
	}
	for _, k := range RequestSourceKinds {
		if _, _, err := NormalizeRequestSource(string(k), "vendor-1"); k == RequestSourceWeb && err == nil {
			t.Error("web submission with an external id accepted; want refused")
		} else if k != RequestSourceWeb && err != nil {
			t.Errorf("%s with an external id refused: %v", k, err)
		}
	}
	if _, _, err := NormalizeRequestSource("connector", "  "); err == nil {
		t.Error("connector without an external id accepted; want required")
	}
	if _, _, err := NormalizeRequestSource("email", ""); err == nil {
		t.Error("source \"email\" accepted; just the four approved kinds")
	}
	if _, _, err := NormalizeRequestSource("connector", strings.Repeat("x", MaxRequestExternalIDLen+1)); err == nil {
		t.Error("over-long external id accepted; want refused")
	}
}

// The whole record validates, and each broken part is refused by name.
func TestTeamRequestValidate(t *testing.T) {
	r := validRequest()
	if err := r.Validate(); err != nil {
		t.Fatalf("valid request refused: %v", err)
	}
	cases := map[string]struct {
		edit func(r *TeamRequest)
		want string
	}{
		"no org":                   {func(r *TeamRequest) { r.OrgID = 0 }, "one org and one team"},
		"no team":                  {func(r *TeamRequest) { r.TeamID = 0 }, "one org and one team"},
		"rev zero":                 {func(r *TeamRequest) { r.Rev = 0 }, "starts at 1"},
		"negative reopen count":    {func(r *TeamRequest) { r.ReopenedCount = -1 }, "never negative"},
		"bad status":               {func(r *TeamRequest) { r.State = "archived" }, "status must be one of"},
		"bad source":               {func(r *TeamRequest) { r.SourceKind = "email" }, "source_kind must be one of"},
		"connector, no id":         {func(r *TeamRequest) { r.SourceKind = RequestSourceConnector }, "source_external_id is required"},
		"serial ref":               {func(r *TeamRequest) { r.PublicRef = "149" }, "opaque token"},
		"no subject":               {func(r *TeamRequest) { r.Subject = "" }, "subject is required"},
		"subject too long":         {func(r *TeamRequest) { r.Subject = strings.Repeat("x", MaxRequestSubjectLen+1) }, "subject is over"},
		"brief too long":           {func(r *TeamRequest) { r.InputBrief = strings.Repeat("x", MaxRequestBriefLen+1) }, "input_brief is over"},
		"reject too long":          {func(r *TeamRequest) { r.RejectReason = strings.Repeat("x", MaxRequestRejectReasonLen+1) }, "reject_reason is over"},
		"contact with a space":     {func(r *TeamRequest) { r.RequesterContact = "dana x" }, "single email address or handle"},
		"claim without the moment": {func(r *TeamRequest) { r.ClaimedAt = nil }, "records when it was taken"},
		"needs_information without a moment": {func(r *TeamRequest) {
			r.State = RequestStatusNeedsInformation
		}, "needs_info_since"},
		"delivered without a brief": {func(r *TeamRequest) {
			now := time.Now()
			r.State, r.DeliveredAt = RequestStatusDelivered, &now
		}, "delivered is evidenced"},
		"delivered without a moment": {func(r *TeamRequest) {
			r.State, r.OutcomeBrief = RequestStatusDelivered, "shipped the queue"
		}, "records delivered_at"},
		"accepted at without a moment": {func(r *TeamRequest) { r.AcceptedAt = nil; r.AcceptedBy = ptrInt(31) }, "records when the acceptance"},
		"no contact snapshot":          {func(r *TeamRequest) { r.RequesterContact = "  " }, "contact snapshot taken at submit"},
		"Bogus user id":                {func(r *TeamRequest) { r.RequesterUserID = ptrInt(0) }, "requester_user_id"},
		"duplicate of self": {func(r *TeamRequest) {
			r.State, r.DuplicateOf = RequestStatusDuplicate, ptrInt(r.ID)
		}, "duplicate of itself"},
		"duplicate without a canonical": {func(r *TeamRequest) { r.State = RequestStatusDuplicate }, "must name the canonical"},
		"rejected without a reason":     {func(r *TeamRequest) { r.State = RequestStatusRejected }, "reject_reason"},
		"accepted by a non-requester": {func(r *TeamRequest) {
			now := time.Now()
			r.AcceptedAt, r.AcceptedBy = &now, ptrInt(9)
		}, "the requester's alone"},
		"accepted without a moment": {func(r *TeamRequest) {
			now := time.Now()
			r.State, r.OutcomeBrief, r.DeliveredAt = RequestStatusRequesterAccepted, "shipped the queue", &now
		}, "records accepted_at"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			r := validRequest()
			c.edit(r)
			err := r.Validate()
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want %q", err, c.want)
			}
		})
	}
}

// Validate normalizes in place: a record pasted back with padding, mixed case
// and the display ref stores the canonical form.
func TestTeamRequestValidateNormalizes(t *testing.T) {
	r := validRequest()
	r.PublicRef, r.Subject, r.InputBrief = " REQ-8F2q1X7k4M2p9T5w ", "  Fix the queue ", " see above "
	r.SourceKind, r.State = " WEB ", " TRIAGING"
	if err := r.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if r.PublicRef != "8F2q1X7k4M2p9T5w" || r.Subject != "Fix the queue" || r.InputBrief != "see above" {
		t.Errorf("normalized to %q / %q / %q", r.PublicRef, r.Subject, r.InputBrief)
	}
	if r.SourceKind != RequestSourceWeb || r.State != RequestStatusTriaging {
		t.Errorf("source/state = %q / %q", r.SourceKind, r.State)
	}
}

// The contact snapshot is the durable record of who asked: it validates with
// no user behind it, which is what lets a request outlive the requester's
// deletion (requester_user_id is cleared, requester_contact is kept).
func TestRequesterContactSurvivesUserDeletion(t *testing.T) {
	r := validRequest()
	r.RequesterUserID = nil
	r.RequesterContact = "dana@example.com"
	if err := r.Validate(); err != nil {
		t.Fatalf("request without a user row refused: %v", err)
	}
	if r.RequesterContact != "dana@example.com" {
		t.Errorf("contact snapshot = %q, want it kept", r.RequesterContact)
	}
	// The snapshot is required whatever the user column says: the user id alone
	// would vanish with the row, so it is never the only record of who asked.
	r = validRequest()
	r.RequesterContact = ""
	if err := r.Validate(); err == nil || !strings.Contains(err.Error(), "snapshot taken at submit") {
		t.Errorf("request without a contact snapshot: %v, want it refused", err)
	}
}

// The triage claim and the assignee are separate: the claim is one holder and
// its moment, never a clock-bound thing, and holding both is allowed
// (someone claimed it and also owns the outcome).
func TestTriageClaimIsNotTheAssignee(t *testing.T) {
	r := validRequest()
	if !r.Claimed() {
		t.Error("Claimed() = false with a claim set")
	}
	// Assignee set, claim released: the request is owned but unclaimed.
	r.ClaimUserID, r.ClaimedAt = nil, nil
	if err := r.Validate(); err != nil {
		t.Fatalf("assigned but unclaimed refused: %v", err)
	}
	if r.Claimed() {
		t.Error("Claimed() = true with no claim")
	}
	// Claim set, no assignee: claimed but nobody owns the outcome yet.
	r = validRequest()
	r.OwnerUserID = nil
	if err := r.Validate(); err != nil {
		t.Fatalf("claimed but unassigned refused: %v", err)
	}
	// A claim has no expiry field at all: D4 keeps claims manual.
	if hasField(r, "claim_expires_at") {
		t.Error("TeamRequest grew a claim expiry: D4 keeps claims manual, never expiring")
	}
}

// Comments, events and links each validate their own shape.
func TestRequestCommentEventLinkValidate(t *testing.T) {
	c := &RequestComment{Kind: " QUESTION ", Body: " Which boards matter? "}
	if err := c.Validate(); err != nil {
		t.Fatalf("valid question refused: %v", err)
	}
	if c.Kind != RequestCommentQuestion || c.Body != "Which boards matter?" {
		t.Errorf("comment = %q / %q", c.Kind, c.Body)
	}
	if err := (&RequestComment{Kind: "note", Body: "x"}).Validate(); err == nil {
		t.Error("unknown comment kind accepted")
	}
	if err := (&RequestComment{Body: "  "}).Validate(); err == nil {
		t.Error("empty comment body accepted")
	}
	if err := (&RequestComment{Body: strings.Repeat("x", MaxRequestBriefLen+1)}).Validate(); err == nil {
		t.Error("over-long comment body accepted")
	}

	e := &RequestEvent{Action: "transition", FromStatus: "triaging", ToStatus: "accepted"}
	if err := e.Validate(); err != nil {
		t.Fatalf("valid transition event refused: %v", err)
	}
	if err := (&RequestEvent{Action: "transition", ToStatus: "accepted"}).Validate(); err == nil {
		t.Error("transition without its from state accepted")
	}
	if err := (&RequestEvent{Action: "claim", ToStatus: "accepted"}).Validate(); err == nil {
		t.Error("claim event with a to state accepted; only a transition names states")
	}
	if err := (&RequestEvent{Action: "archive"}).Validate(); err == nil {
		t.Error("unknown event action accepted")
	}

	l := &RequestLink{RequestID: 7, LinkKind: "planned_as", TargetKind: "task", TargetID: 42}
	if err := l.Validate(); err != nil {
		t.Fatalf("valid link refused: %v", err)
	}
	if err := (&RequestLink{RequestID: 7, LinkKind: "routes_to", TargetKind: "task", TargetID: 42}).Validate(); err == nil {
		t.Error("routes_to -> task accepted; routing is to a project")
	}
	if err := (&RequestLink{RequestID: 7, LinkKind: "planned_as", TargetKind: "task"}).Validate(); err == nil {
		t.Error("link without a target id accepted")
	}
	if err := (&RequestLink{RequestID: 7, LinkKind: "relates_to", TargetKind: "request", TargetID: 7}).Validate(); err == nil {
		t.Error("link of a request to itself accepted")
	}
}

// The domain file adds no storage or network dependency: it parses to a set of
// stdlib imports with nothing that can reach Postgres or the network (the
// ticket's "no DB/network in this concern" boundary, kept as a guard).
func TestRequestDomainHasNoDBOrNetworkImports(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "request.go", nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse request.go: %v", err)
	}
	for _, spec := range f.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			t.Fatalf("import path %s: %v", spec.Path.Value, err)
		}
		switch path {
		case "errors", "fmt", "slices", "strings", "time", "unicode", "unicode/utf8":
		default:
			t.Errorf("request.go imports %q: the domain file must stay free of storage, network and generator code", path)
		}
	}
}

// hasField reports whether v's struct type has a JSON field with this name.
func hasField(v any, jsonName string) bool {
	t := reflect.TypeOf(v)
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	for i := 0; i < t.NumField(); i++ {
		name := strings.Split(t.Field(i).Tag.Get("json"), ",")[0]
		if name == jsonName {
			return true
		}
	}
	return false
}

// The schema (migration 053, team_requests) pins public_ref's shape with a
// CHECK. The domain must accept nothing that CHECK would refuse, or a
// validated record could fail to store; every reference this test finds valid
// is therefore also checked against the schema's own pattern.
func TestPublicRefAgreesWithTheSchemaShape(t *testing.T) {
	// The migration's CHECK, spelled out here so a change to either side shows
	// up as a failing test rather than a rejected insert.
	schemaRefShape := regexp.MustCompile(`^[A-Za-z0-9_-]{16,64}$`)
	refs := []string{
		"8f2q1x7k4m2p9t5w",
		"REQ-8f2q1x7k4m2p9t5w",
		"Ab-12_cd9efgh76_",
		strings.Repeat("a", MaxRequestPublicRefLen),
	}
	for _, ref := range refs {
		got, err := NormalizeRequestPublicRef(ref)
		if err != nil {
			t.Fatalf("%q refused by the domain: %v", ref, err)
		}
		if !schemaRefShape.MatchString(got) {
			t.Errorf("%q normalizes to %q, which migration 053's CHECK (%s) would refuse", ref, got, schemaRefShape)
		}
	}
}
