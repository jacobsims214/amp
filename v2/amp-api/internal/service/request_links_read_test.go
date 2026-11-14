package service

// Tests for the links READ: Service.ListRequestLinks (AMP #4272, epic 202 story 549).
//
// The repository's read (RequestLinks) is proven by
// internal/repository/request_links_test.go's TestRequestLinks*; the route is
// proven end to end in cmd/server/request_links_read_rest_test.go; the pure query
// decoder is internal/api/request_links_read_test.go's. What belongs HERE is what
// only the service adds, which is the ticket's authorization and its documented
// requester decision:
//
//  1. WHO MAY READ A REQUEST'S LINEAGE. Every principal who may read the request
//     reads its links — a team member, a lead, an org owner (and a global admin,
//     which InboxRole grants by design) — because this read authorizes through
//     the detail read's own load-and-decide rather than a second rule of its own.
//  2. A REQUESTER IS REFUSED, NOT FILTERED (the ticket's decision, in
//     ListRequestLinks' header). The projection carries the public summary of the
//     same facts (its milestones) and has no field for a link row, so a requester
//     who is not on the inbox gets the surface's one 404 — after, never before,
//     authorizeRequestRead, so a requester who IS also a member still gets the
//     team's rows.
//  3. EVERY OTHER REFUSAL IS THE DETAIL READ'S. A sibling team's request, another
//     org's request, another requester's request, an unissued team and an id that
//     never existed are one 404 whose message is byte-identical to
//     GetRequest's for the same caller — the check that produced it is the same
//     check, not a resemblance.
//  4. THE KIND FILTER IS THE CALLER'S, AND IT DOES NOT WIDEN ANYTHING. One kind,
//     several kinds and no kinds all answer exactly the request's own rows: a
//     filter can narrow the lineage and can never reach another request's.
//
// Run against a disposable Postgres with every migration applied:
//
//	TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:PORT/amp_test?sslmode=disable \
//	go test ./internal/service -run 'RequestLinks' -count=1 -race
//
// The fixture is request_links_test.go's linkFixture (newLinkFixture), reused
// rather than copied so this suite and the attach suite cannot drift apart.

import (
	"testing"

	"github.com/simstech/amp-api/internal/auth"
	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
)

// readLinks is the service call under test, with the fixture's own shapes filled
// in: the named request in the fixture's inbox, optionally filtered by kind.
func (f *linkFixture) readLinks(p authz.Principal, req *domain.TeamRequest, kinds ...domain.RequestLinkKind) (*RequestLinks, error) {
	f.t.Helper()
	return f.svc.ListRequestLinks(f.ctx, p, RequestLinksInput{
		RequestReadInput: RequestReadInput{RequestID: req.ID, TeamID: req.TeamID},
		Kinds:            kinds,
	})
}

// linkEverythingWith attaches one relation of each kind a request can carry through
// the general path, so a read has a lineage worth filtering. It returns the rows in
// attach order, which is the order the read must answer with.
func (f *linkFixture) linkEverythingWith(p authz.Principal, req *domain.TeamRequest) []*domain.RequestLink {
	f.t.Helper()
	var out []*domain.RequestLink
	for _, tc := range []struct {
		kind   domain.RequestLinkKind
		target domain.RequestTargetKind
		id     int
	}{
		{domain.RequestLinkRoutesTo, domain.RequestTargetProject, f.project},
		{domain.RequestLinkPlannedAs, domain.RequestTargetEpic, f.epic},
		{domain.RequestLinkPlannedAs, domain.RequestTargetStory, f.story},
		{domain.RequestLinkPlannedAs, domain.RequestTargetTask, f.task},
		{domain.RequestLinkDeliveredBy, domain.RequestTargetTask, f.task2},
		{domain.RequestLinkDeliveredBy, domain.RequestTargetArtifact, f.artifact},
	} {
		res, err := f.attach(p, req, tc.kind, tc.target, tc.id)
		if err != nil {
			f.t.Fatalf("attach %s→%s %d: %v", tc.kind, tc.target, tc.id, err)
		}
		out = append(out, res.Link)
	}
	return out
}

// TestRequestLinks_WhoMayRead is criteria 1 and 3: everyone with standing on the
// request reads its lineage, and everyone without it gets the detail read's own
// 404, byte for byte.
func TestRequestLinks_WhoMayRead(t *testing.T) {
	f := newLinkFixture(t)
	requester := f.customer
	req := f.newRequest("linksreadwho", domain.RequestStatusTriaging, &requester)
	wrote := f.linkEverythingWith(f.principal(f.member), req)

	// Everybody whose view of the request is the team's reads the whole lineage.
	// The four are the four ways InboxRole grants standing (a team member, a team
	// lead, an org owner, a global admin), so this is the contract's triage row
	// applied to a read.
	for name, p := range map[string]authz.Principal{
		"a team member": f.principal(f.member),
		"a team lead":   f.principal(f.lead),
		"an org owner":  f.principal(f.admin),
		"a global admin": {
			UserID: f.admin, Email: f.emailOf(f.admin), Channel: auth.ChannelUI,
			AuthMethod: auth.AuthDexSession, Super: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := f.readLinks(p, req)
			if err != nil {
				t.Fatalf("%s could not read the lineage: %v", name, err)
			}
			if !got.View.Full() {
				t.Fatalf("%s got view %q, want the team's", name, got.View.String())
			}
			if got.RequestID != req.ID {
				t.Fatalf("request_id = %d, want %d", got.RequestID, req.ID)
			}
			if got.Count != len(wrote) || len(got.Links) != len(wrote) {
				t.Fatalf("count %d, %d rows; want the %d attached", got.Count, len(got.Links), len(wrote))
			}
			for i, l := range got.Links {
				if l.ID != wrote[i].ID {
					t.Fatalf("row %d is link %d, want %d (attach order)", i, l.ID, wrote[i].ID)
				}
			}
		})
	}

	// 2. A requester who is not on the inbox is refused. Their own detail read
	// answers 200 (the projection), which is what makes this a decision rather than
	// an inability: they may see the request and not its internal lineage.
	if read, err := f.svc.GetRequest(f.ctx, f.principal(requester), RequestReadInput{RequestID: req.ID, TeamID: req.TeamID}); err != nil {
		t.Fatalf("the requester cannot read their own request at all: %v", err)
	} else if read.View.Full() {
		t.Fatal("the requester got the team's record: the fixture cannot test the requester arm")
	}
	denied, err := f.readLinks(f.principal(requester), req)
	if err == nil {
		t.Fatalf("the requester read the lineage: %+v", denied)
	}
	if code, status := authz.CodeOf(err), authz.StatusOf(err, 500); code != authz.CodeRequestNotFound || status != 404 {
		t.Fatalf("the requester's refusal = %d %q, want 404 %q (%v)", status, code, authz.CodeRequestNotFound, err)
	}

	// 3. Every refusal is the detail read's own, byte for byte: the same caller
	// naming the same request through GetRequest gets the same message. That is what
	// "the check that produced it is the same check" means, and it is why a route
	// added here cannot answer 403 or a differently-worded 404.
	//
	// Each case pairs a principal with the request it names and the team it names
	// it under, including two cases where the caller is fine and the REQUEST is
	// someone else's.
	otherCustomer := f.newCustomer("links-read-outside")
	unknown := 987654321
	foreignReq := f.newRequestIn(f.otherTeam, "linksreadfor", domain.RequestStatusTriaging, &requester)

	var messages []string
	for _, tc := range []struct {
		name string
		p    authz.Principal
		in   RequestLinksInput
	}{
		{"a customer who is not the requester", f.principal(otherCustomer),
			RequestLinksInput{RequestReadInput: RequestReadInput{RequestID: req.ID, TeamID: f.team}}},
		{"a member of a sibling team", f.principal(f.otherTeamMember()),
			RequestLinksInput{RequestReadInput: RequestReadInput{RequestID: req.ID, TeamID: f.team}}},
		{"a member of a team in another org", f.principal(f.stranger),
			RequestLinksInput{RequestReadInput: RequestReadInput{RequestID: req.ID, TeamID: f.team}}},
		{"an agent account with no role", f.agentPrincipal(),
			RequestLinksInput{RequestReadInput: RequestReadInput{RequestID: req.ID, TeamID: f.team}}},
		{"an id that never existed", f.principal(f.member),
			RequestLinksInput{RequestReadInput: RequestReadInput{RequestID: unknown, TeamID: f.team}}},
		{"an unissued team", f.principal(f.member),
			RequestLinksInput{RequestReadInput: RequestReadInput{RequestID: req.ID, TeamID: 999999}}},
		{"a sibling team's request under this team", f.principal(f.member),
			RequestLinksInput{RequestReadInput: RequestReadInput{RequestID: foreignReq.ID, TeamID: f.team}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := f.svc.ListRequestLinks(f.ctx, tc.p, tc.in)
			if err == nil {
				t.Fatalf("%s read a lineage: %+v", tc.name, got)
			}
			if got != nil {
				t.Fatalf("%s got a non-nil answer beside an error: %+v", tc.name, got)
			}
			if code, status := authz.CodeOf(err), authz.StatusOf(err, 500); code != authz.CodeRequestNotFound || status != 404 {
				t.Fatalf("%s refusal = %d %q, want 404 %q (%v)", tc.name, status, code, authz.CodeRequestNotFound, err)
			}
			// The detail read's own answer for the same caller and request: same
			// message, always.
			_, detailErr := f.svc.GetRequest(f.ctx, tc.p, tc.in.RequestReadInput)
			if detailErr == nil {
				t.Fatalf("%s: the detail read succeeded where the links read refused", tc.name)
			}
			if detailErr.Error() != err.Error() {
				t.Fatalf("%s: the links read refused with %q, the detail read with %q — they must be one refusal",
					tc.name, err, detailErr)
			}
			messages = append(messages, err.Error())
		})
	}
	for _, m := range messages[1:] {
		if m != messages[0] {
			t.Fatalf("refusals differ (%q vs %q): a 404 must not depend on who asked or which id", m, messages[0])
		}
	}

	// A submit-only credential reads nothing: the scope gate names the operation
	// and never the request.
	scoped := authz.Principal{
		UserID: f.member, Email: f.emailOf(f.member), Channel: auth.ChannelToken, ViaToken: true,
		AuthMethod: auth.AuthAPIToken, Scope: domain.TokenScopeSubmit, ScopeTeamID: f.team,
	}
	if _, err := f.readLinks(scoped, req); err == nil {
		t.Fatal("a submit-only token read a request's lineage")
	} else if authz.CodeOf(err) != authz.CodeTokenScopeDenied {
		t.Fatalf("submit-only refusal code = %q, want %q (%v)", authz.CodeOf(err), authz.CodeTokenScopeDenied, err)
	}
}

// TestRequestLinks_RequesterWhoIsAlsoAMemberGetsTheTeamView is the second half of
// the documented requester decision: the refusal is AFTER the view decision, so a
// requester who is also an inbox member is an inbox member here exactly as they
// are on the detail read. A read that refused on "is this person the requester?"
// would be a second, wrong rule.
func TestRequestLinks_RequesterWhoIsAlsoAMemberGetsTheTeamView(t *testing.T) {
	f := newLinkFixture(t)
	// The member submits their own request to their own inbox, which the product
	// allows (the fixture's own fourth request does it) — so the caller is both.
	own := f.newRequest("linksreadself", domain.RequestStatusTriaging, &f.member)
	wrote := f.linkEverythingWith(f.principal(f.member), own)

	got, err := f.readLinks(f.principal(f.member), own)
	if err != nil {
		t.Fatalf("a member reading their own request's lineage: %v", err)
	}
	if !got.View.Full() {
		t.Fatalf("view = %q, want the team's: a member is a member, whatever else they are", got.View.String())
	}
	if len(got.Links) != len(wrote) {
		t.Fatalf("%d rows, want the %d attached", len(got.Links), len(wrote))
	}
}

// TestRequestLinks_FilterAndShape is criterion 1's filter half and criterion 2's
// shape half, at the service seam: the filter narrows to the kinds named (and to
// nothing else), an empty filter means every kind, and the rows are the stored
// domain rows the UI's `RequestLink` already is — no field added, none dropped.
func TestRequestLinks_FilterAndShape(t *testing.T) {
	f := newLinkFixture(t)
	requester := f.customer
	req := f.newRequest("linksreadflt", domain.RequestStatusInProgress, &requester)
	wrote := f.linkEverythingWith(f.principal(f.member), req)
	member := f.principal(f.member)

	// No filter: every row, in attach order.
	all, err := f.readLinks(member, req)
	if err != nil {
		t.Fatalf("unfiltered read: %v", err)
	}
	if len(all.Links) != len(wrote) || all.Count != len(wrote) {
		t.Fatalf("unfiltered = %d rows (count %d), want %d", len(all.Links), all.Count, len(wrote))
	}

	// One kind: only that kind, still in attach order.
	planned, err := f.readLinks(member, req, domain.RequestLinkPlannedAs)
	if err != nil {
		t.Fatalf("planned_as read: %v", err)
	}
	if planned.Count != 3 {
		t.Fatalf("planned_as = %d rows, want the epic, story and ticket", planned.Count)
	}
	for _, l := range planned.Links {
		if l.LinkKind != domain.RequestLinkPlannedAs {
			t.Fatalf("the filter returned a %s row: %+v", l.LinkKind, l)
		}
	}
	for i, want := range []int{f.epic, f.story, f.task} {
		if planned.Links[i].TargetID != want {
			t.Fatalf("planned row %d targets %d, want %d (attach order)", i, planned.Links[i].TargetID, want)
		}
	}

	// Several kinds: their union, and nothing else.
	twoKinds, err := f.readLinks(member, req, domain.RequestLinkRoutesTo, domain.RequestLinkDeliveredBy)
	if err != nil {
		t.Fatalf("two-kind read: %v", err)
	}
	if twoKinds.Count != 3 {
		t.Fatalf("routes_to+delivered_by = %d rows, want the route and the two pieces of evidence", twoKinds.Count)
	}

	// A kind with no rows is an empty list, not nil and not an error: "nothing
	// relates these requests" is a fact the rail renders.
	none, err := f.readLinks(member, req, domain.RequestLinkRelatesTo)
	if err != nil {
		t.Fatalf("relates_to read: %v", err)
	}
	if none.Links == nil {
		t.Fatal("a kind with no rows answered nil: it encodes as null, which a client reads as \"not served\"")
	}
	if none.Count != 0 || len(none.Links) != 0 {
		t.Fatalf("relates_to = %+v, want an empty non-nil slice", none)
	}

	// The shape (criterion 2): every field the UI's `RequestLink` has is present
	// and populated on a stored row, and the read adds no member of its own. The
	// assertion is on the domain type's fields, which ARE that interface's — a
	// field renamed on the domain type would break the client, and this is where
	// that is caught without a browser.
	stored := all.Links[0]
	if stored.ID == 0 || stored.RequestID != req.ID || stored.LinkKind == "" || stored.TargetKind == "" ||
		stored.TargetID == 0 || stored.CreatedBy == nil || stored.CreatedAt.IsZero() {
		t.Fatalf("the read dropped a field the client's RequestLink carries: %+v", stored)
	}

	// The answer is the request's own lineage and no one else's: a sibling team's
	// request, read through the same filter by the same caller, is still that
	// request's rows (here: none), not this one's.
	foreignReq := f.newRequestIn(f.otherTeam, "linksreadflt2", domain.RequestStatusNew, &requester)
	foreign, err := f.readLinks(f.principal(f.member), foreignReq, domain.RequestLinkPlannedAs)
	if err == nil {
		t.Fatalf("a sibling team's request was readable: %+v", foreign)
	}
	if authz.CodeOf(err) != authz.CodeRequestNotFound {
		t.Fatalf("a sibling team's request = %v, want request.not_found", err)
	}
}
