package main

// Contract fixtures for the external-request envelope (AMP #3970, epic 202,
// story 549; approved product contract artifact 36 v2 §5, build slice H2 of
// discoveries/team-inbox-approved-build-slices.md, spec in
// discoveries/team-request-external-envelope.md).
//
// These are FIXTURES, NOT CONNECTORS. Each row below is one real vendor shape — a
// GitHub issue event, a Jira issue event, a Wiz finding notification, a polled
// pricing alarm — reduced to a small payload, a translator that turns it into
// domain.RequestEnvelopeInput, and the assertions the envelope contract makes about
// what happens next:
//
//  1. Mapping. The payload's vendor object id becomes the envelope's external_id,
//     its delivery id becomes event_id, and its event is one of the envelope's five
//     verbs. Nothing about the vendor's own layout survives into intake.
//  2. Event-level replay. The same delivery twice is ONE request: a redelivery is
//     answered with the request the first delivery produced, and writes nothing.
//  3. Object-level identity. A second event about the same vendor object — a
//     comment, a severity bump, a renamed Jira key — appends to the request that
//     already exists (through the real comment path), never creates a second
//     request, never advances its state, never touches rev.
//  4. Tenant. Where a request lands is the registration's, never the payload's.
//     Each payload carries its own tenancy claim (a Jira project, a Wiz project, a
//     repository owner, a pricing alarm's "team") and the request still lands in
//     the fixture's team with none of those claims in its metadata.
//  5. No outbound request. Every fixture's external_url points at a canary server
//     that counts what reaches it. Intake is receive-only: the canary must see
//     nothing, ever (the SSRF posture — an external_url is inert text).
//
// The vendor knowledge lives HERE, in the fixtures, on purpose: phase 0 ships the
// envelope and its contract, and phase 2's adapters grow from these rows. There is
// no signed endpoint, no registration table, no sealed secret and no credential in
// any of it — the create and comment paths used below are the surfaces S1/S3
// already shipped, and this file changes no production code.
//
// Run against a disposable Postgres (never a live stack):
//
//	TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:PORT/amp_test?sslmode=disable \
//	  go test ./cmd/server -run 'TestRequestEnvelope' -count=1 -race -v

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/service"
)

// envelopeOccurredAt is when the fixtures' events happened at the vendor, derived
// from the fixture clock (reqSubmitDB.now) so first-report-wins and every age a
// team would see are stable rather than dependent on when the test ran.
var envelopeOccurredAt = envelopeNow().Add(-90 * time.Second)

// envelopeNow matches reqSubmitDB's injected clock, so a fixture can derive a
// vendor timestamp without reading a real clock.
func envelopeNow() time.Time { return time.Date(2026, 11, 11, 20, 0, 0, 0, time.UTC) }

// envelopeCanary is the server every fixture's external_url points at. Intake must
// never reach it: an external_url is stored text, never a fetch target (contract
// §5, and discovery #3658's "never fetched server-side"). It counts what arrives,
// so "no outbound request" is proven behaviourally rather than by reading code.
type envelopeCanary struct {
	hits    atomic.Int64
	baseURL string
}

// start binds a real listener and counts every request that reaches it. It answers
// 418 so a fetch that did happen would also be visibly wrong to the fetcher.
func (c *envelopeCanary) start(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.hits.Add(1)
		w.WriteHeader(http.StatusTeapot)
	}))
	t.Cleanup(srv.Close)
	c.baseURL = srv.URL
}

// url is an inert vendor link on the canary: the path names the vendor object, and
// the host is the listener that must never be contacted.
func (c *envelopeCanary) url(path string) string { return c.baseURL + path }

// ---- the four vendor shapes ----

// envelopeFixture is one vendor's delivery, from the vendor's raw bytes to the
// envelope and the intake calls a phase-1 endpoint will make.
type envelopeFixture struct {
	// name is the vendor and the object, for the subtest.
	name string
	// integration is the registered integration instance that would deliver this
	// vendor's events (phase 1's registration row) — the trusted half's identity.
	integration string

	// payload is the vendor's raw body for the first event; %s is its own object
	// URL, which the test fills with a canary link. deliveryID is the vendor's
	// delivery/event id (a header value or a body field).
	payload    string
	deliveryID string
	// translate is what phase 2's adapter will be: raw vendor bytes plus the
	// vendor's delivery id in, one bounded envelope input out.
	translate func(t *testing.T, raw []byte, deliveryID string) domain.RequestEnvelopeInput

	// second is a later event about the SAME vendor object (a comment, a severity
	// change, a re-key) with its own delivery id: object identity must hold while
	// event identity differs.
	second string
	// secondID is that later delivery's own id.
	secondID string
	// secondComment is the discussion kind the appended text lands as.
	secondComment domain.RequestCommentKind

	// wantExternalID is the vendor's immutable object id the envelope must carry
	// (D12: never the display key), and wantSubject a phrase the one-line subject
	// must contain.
	wantExternalID string
	// wantEvent is the envelope verb, and wantVendorEvent the vendor's own name
	// kept as a label.
	wantEvent       domain.RequestEnvelopeEvent
	wantVendorEvent string
	wantSubject     string

	// claims are the payload's own tenancy/routing claims. A reserved one must be
	// absent from metadata; any other is stored as inert text.
	claims map[string]string
}

// envelopeFixtures builds the four rows. The canary is captured so a translator can
// build the vendor's object URL without a package-level variable.
func envelopeFixtures(canary *envelopeCanary) []envelopeFixture {
	return []envelopeFixture{
		{
			name:        "GitHub issue",
			integration: "github-issues/acme-ops",
			deliveryID:  "72d3162e-cc78-11e3-81ab-4c9367dc0958",
			payload: `{
			  "action": "opened",
			  "repository": {"id": 771, "full_name": "acme/ops", "owner": {"login": "acme"}},
			  "issue": {
			    "id": 1841209, "number": 42,
			    "title": "CSV export of the team queue",
			    "body": "We need the triage queue as CSV weekly, for the Monday review.",
			    "html_url": %q,
			    "labels": [{"name": "area:export"}]
			  }
			}`,
			translate: func(t *testing.T, raw []byte, deliveryID string) domain.RequestEnvelopeInput {
				t.Helper()
				// The vendor's own shape, and only the vendor's own shape: this is
				// what phase 2's adapter will hold.
				var e struct {
					Action     string `json:"action"`
					Repository struct {
						ID       int64  `json:"id"`
						FullName string `json:"full_name"`
						Owner    struct {
							Login string `json:"login"`
						} `json:"owner"`
					} `json:"repository"`
					Issue struct {
						ID      int64  `json:"id"`
						Number  int    `json:"number"`
						Title   string `json:"title"`
						Body    string `json:"body"`
						HTMLURL string `json:"html_url"`
						Labels  []struct {
							Name string `json:"name"`
						} `json:"labels"`
					} `json:"issue"`
					Comment *struct {
						ID   int64  `json:"id"`
						Body string `json:"body"`
					} `json:"comment"`
				}
				if err := json.Unmarshal(raw, &e); err != nil {
					t.Fatalf("parse github payload: %v", err)
				}
				labels := map[string]string{
					"repository": e.Repository.FullName,
					// A reserved name, deliberately: the payload's repository owner
					// looks like tenancy and is dropped rather than stored.
					"owner": e.Repository.Owner.Login,
					// The human-facing key is a LABEL: an issue that moves repos
					// keeps its immutable id (D12).
					"issue_key": fmt.Sprintf("%s#%d", e.Repository.FullName, e.Issue.Number),
				}
				for _, l := range e.Issue.Labels {
					labels["label:"+l.Name] = "true"
				}
				event, subject, body := domain.RequestEnvelopeCreated, e.Issue.Title, e.Issue.Body
				if e.Comment != nil {
					// A comment delivery is the same issue seen again: the object
					// id does not move and the comment is the new text.
					event, subject, body = domain.RequestEnvelopeCommented, "Comment on "+e.Issue.Title, e.Comment.Body
				}
				return domain.RequestEnvelopeInput{
					ExternalID:  fmt.Sprintf("%d", e.Issue.ID),
					EventID:     deliveryID,
					Event:       event,
					VendorEvent: "issues",
					Action:      e.Action,
					OccurredAt:  envelopeOccurredAt,
					Subject:     subject,
					Body:        body,
					ExternalURL: e.Issue.HTMLURL,
					Labels:      labels,
					Raw:         raw,
				}
			},
			second: `{
			  "action": "created",
			  "repository": {"id": 771, "full_name": "acme/ops", "owner": {"login": "acme"}},
			  "issue": {"id": 1841209, "number": 42, "title": "CSV export of the team queue"},
			  "comment": {"id": 991, "body": "Adding our finance team, they run the review."}
			}`,
			secondID:        "b0f9b1c8-1f5a-11ee-be56-0242ac120002",
			secondComment:   domain.RequestCommentDiscussion,
			wantExternalID:  "1841209",
			wantEvent:       domain.RequestEnvelopeCreated,
			wantVendorEvent: "issues",
			wantSubject:     "CSV export",
			claims: map[string]string{
				"repository": "acme/ops",
				"owner":      "acme",
			},
		},
		{
			name:        "Jira issue",
			integration: "jira/ops-desk",
			deliveryID:  "evt-77c9a1e0-4c2e-4f0e-9e7a-1d1a0f6b2e11",
			payload: `{
			  "webhookEvent": "jira:issue_created",
			  "issue": {
			    "id": "10230", "key": "OPS-412", "self": %q,
			    "fields": {
			      "summary": "Monthly pricing export for finance",
			      "description": "Finance closes the month with this; today they copy it by hand.",
			      "project": {"id": "10001", "key": "OPS", "name": "External ops"}
			    }
			  }
			}`,
			translate: func(t *testing.T, raw []byte, deliveryID string) domain.RequestEnvelopeInput {
				t.Helper()
				var e struct {
					WebhookEvent string `json:"webhookEvent"`
					Issue        struct {
						ID     string `json:"id"`
						Key    string `json:"key"`
						Self   string `json:"self"`
						Fields struct {
							Summary     string `json:"summary"`
							Description string `json:"description"`
							Project     struct {
								ID   string `json:"id"`
								Key  string `json:"key"`
								Name string `json:"name"`
							} `json:"project"`
						} `json:"fields"`
					} `json:"issue"`
					Comment *struct {
						Body string `json:"body"`
					} `json:"comment"`
				}
				if err := json.Unmarshal(raw, &e); err != nil {
					t.Fatalf("parse jira payload: %v", err)
				}
				// A created issue and an updated one are the same object: only the
				// text and the verb move, and an update's body is the comment that
				// caused it when the description is not resent.
				event, body := domain.RequestEnvelopeCreated, e.Issue.Fields.Description
				action := "issue_created"
				if e.WebhookEvent == "jira:issue_updated" {
					event, action = domain.RequestEnvelopeUpdated, "issue_updated"
					if e.Comment != nil && e.Comment.Body != "" {
						body = e.Comment.Body
					}
				}
				return domain.RequestEnvelopeInput{
					// The immutable internal id, never the key: Jira renames keys
					// and a dedup key that changes creates duplicate requests (D12).
					ExternalID:  e.Issue.ID,
					EventID:     deliveryID,
					Event:       event,
					VendorEvent: e.WebhookEvent,
					Action:      action,
					OccurredAt:  envelopeOccurredAt,
					Subject:     e.Issue.Fields.Summary,
					Body:        body,
					ExternalURL: e.Issue.Self,
					Labels: map[string]string{
						"jira_key": e.Issue.Key,
					},
					Fields: map[string]string{
						"jira_project_name": e.Issue.Fields.Project.Name,
						// Reserved: the payload's Jira project is not an AMP project,
						// and a payload may never name a destination.
						"project": e.Issue.Fields.Project.Key,
					},
					Raw: raw,
				}
			},
			second: `{
			  "webhookEvent": "jira:issue_updated",
			  "issue": {
			    "id": "10230", "key": "OPS-999",
			    "fields": {"summary": "Monthly pricing export (renamed)", "project": {"id": "10001", "key": "OPS"}}
			  },
			  "comment": {"body": "Finance asked to be copied on the next run."}
			}`,
			secondID:        "evt-2a0e5c11-77b3-4a1c-8a55-9c0b2f7d3c44",
			secondComment:   domain.RequestCommentDiscussion,
			wantExternalID:  "10230",
			wantEvent:       domain.RequestEnvelopeCreated,
			wantVendorEvent: "jira:issue_created",
			wantSubject:     "Monthly pricing export",
			claims: map[string]string{
				"jira_project_name": "External ops",
				"project":           "OPS",
			},
		},
		{
			name:        "Wiz finding",
			integration: "wiz/security-alerts",
			deliveryID:  "notif-5d0c8f3a-9b21-4f7e-b0a1-7c5e2d9a4b13",
			payload: `{
			  "type": "finding",
			  "id": "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
			  "name": "Public S3 bucket holds customer exports",
			  "severity": "HIGH",
			  "status": "open",
			  "createdAt": "2026-11-11T19:40:00Z",
			  "resource": {"id": "s3-prod-exports", "name": "prod-customer-exports", "type": "BUCKET"},
			  "portalUrl": %q,
			  "project": {"id": "wiz-proj-9", "name": "production"}
			}`,
			translate: func(t *testing.T, raw []byte, deliveryID string) domain.RequestEnvelopeInput {
				t.Helper()
				var e struct {
					Type      string `json:"type"`
					ID        string `json:"id"`
					Name      string `json:"name"`
					Severity  string `json:"severity"`
					CreatedAt string `json:"createdAt"`
					PortalURL string `json:"portalUrl"`
					Resource  struct {
						Name string `json:"name"`
						Type string `json:"type"`
					} `json:"resource"`
					Project struct {
						ID   string `json:"id"`
						Name string `json:"name"`
					} `json:"project"`
				}
				if err := json.Unmarshal(raw, &e); err != nil {
					t.Fatalf("parse wiz payload: %v", err)
				}
				occurred, err := time.Parse(time.RFC3339, e.CreatedAt)
				if err != nil {
					t.Fatalf("parse wiz createdAt: %v", err)
				}
				return domain.RequestEnvelopeInput{
					ExternalID:  e.ID,
					EventID:     deliveryID,
					Event:       domain.RequestEnvelopeAlert,
					VendorEvent: "finding",
					Action:      "created",
					OccurredAt:  occurred,
					Subject:     e.Name,
					Body:        "A " + e.Severity + " severity finding on " + e.Resource.Name + ".",
					ExternalURL: e.PortalURL,
					Fields: map[string]string{
						"severity":      e.Severity,
						"resource_type": e.Resource.Type,
						// A Wiz project is a label, not an AMP team: tenancy is the
						// registration's.
						"wiz_project_name": e.Project.Name,
						// Reserved: a payload may not name an AMP project either.
						"project": e.Project.ID,
					},
					Raw: raw,
				}
			},
			second: `{
			  "type": "finding",
			  "id": "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
			  "name": "Public S3 bucket holds customer exports",
			  "severity": "CRITICAL",
			  "createdAt": "2026-11-11T19:40:00Z",
			  "resource": {"id": "s3-prod-exports", "name": "prod-customer-exports", "type": "BUCKET"},
			  "project": {"id": "wiz-proj-9", "name": "production"}
			}`,
			secondID:        "notif-8f1d2c47-0a63-4e19-9b7d-3d8e6a1c5f02",
			secondComment:   domain.RequestCommentDiscussion,
			wantExternalID:  "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
			wantEvent:       domain.RequestEnvelopeAlert,
			wantVendorEvent: "finding",
			wantSubject:     "Public S3 bucket",
			claims: map[string]string{
				"wiz_project_name": "production",
				"project":          "wiz-proj-9",
			},
		},
		{
			name:        "pricing alarm",
			integration: "pricing/alarms",
			deliveryID:  "alert-2049",
			payload: `{
			  "alarm_id": "price-7781",
			  "alert_id": "alert-2049",
			  "metric": "aws.ec2.on_demand.us-east-1",
			  "threshold_usd": 0.42,
			  "observed_usd": 0.61,
			  "observed_at": "2026-11-11T19:58:00Z",
			  "alarm_url": %q,
			  "team": "untouchables"
			}`,
			translate: func(t *testing.T, raw []byte, deliveryID string) domain.RequestEnvelopeInput {
				t.Helper()
				var e struct {
					AlarmID    string  `json:"alarm_id"`
					AlertID    string  `json:"alert_id"`
					Metric     string  `json:"metric"`
					Threshold  float64 `json:"threshold_usd"`
					Observed   float64 `json:"observed_usd"`
					ObservedAt string  `json:"observed_at"`
					AlarmURL   string  `json:"alarm_url"`
					Team       string  `json:"team"`
				}
				if err := json.Unmarshal(raw, &e); err != nil {
					t.Fatalf("parse pricing payload: %v", err)
				}
				occurred, err := time.Parse(time.RFC3339, e.ObservedAt)
				if err != nil {
					t.Fatalf("parse pricing observed_at: %v", err)
				}
				return domain.RequestEnvelopeInput{
					// The alarm is the object; the alert is one firing of it, so the
					// next poll that raises the same alarm is a second event on one
					// request, never a second request.
					ExternalID:  e.AlarmID,
					EventID:     deliveryID,
					Event:       domain.RequestEnvelopeAlert,
					VendorEvent: "pricing.poll",
					Action:      "threshold_breached",
					// A POLLED source: the event happened when it happened and we
					// learned of it on the next poll, which is why the two clocks are
					// separate fields.
					OccurredAt:  occurred,
					Subject:     "Pricing alarm: " + e.Metric,
					Body:        fmt.Sprintf("Observed $%.2f, over the $%.2f threshold.", e.Observed, e.Threshold),
					ExternalURL: e.AlarmURL,
					Fields: map[string]string{
						"metric":        e.Metric,
						"threshold_usd": fmt.Sprintf("%.2f", e.Threshold),
						"observed_usd":  fmt.Sprintf("%.2f", e.Observed),
						"alert_id":      e.AlertID,
						// Reserved: the payload's notion of a team is not an AMP
						// team, and metadata may never decide tenancy.
						"team": e.Team,
					},
					Raw: raw,
				}
			},
			second: `{
			  "alarm_id": "price-7781",
			  "alert_id": "alert-2110",
			  "metric": "aws.ec2.on_demand.us-east-1",
			  "threshold_usd": 0.42,
			  "observed_usd": 0.58,
			  "observed_at": "2026-11-11T19:59:00Z",
			  "team": "untouchables"
			}`,
			secondID:        "alert-2110",
			secondComment:   domain.RequestCommentDiscussion,
			wantExternalID:  "price-7781",
			wantEvent:       domain.RequestEnvelopeAlert,
			wantVendorEvent: "pricing.poll",
			wantSubject:     "Pricing alarm",
			claims: map[string]string{
				"team": "untouchables",
			},
		},
	}
}

// ---- the contract test ----

// TestRequestEnvelopeContract runs the four vendor fixtures through the real
// submission and discussion paths: one request per vendor OBJECT, however many
// deliveries arrive about it, and nothing outbound.
func TestRequestEnvelopeContract(t *testing.T) {
	w := newReqSubmitDB(t)
	canary := &envelopeCanary{}
	canary.start(t)
	fixtures := envelopeFixtures(canary)
	integration := w.integrationAccount("inbox-integration")

	for _, fx := range fixtures {
		t.Run(fx.name, func(t *testing.T) {
			// 1. Mapping: the vendor payload becomes one bounded envelope, and the
			// trusted half (the registration) supplies the tenant.
			// Each fixture's vendor link is its own canary path, so a stray fetch
			// would be attributable to the fixture that carried it.
			vendorURL := canary.url("/vendor/" + fx.integration)
			first := w.envelope(t, fx, fmt.Sprintf(fx.payload, vendorURL), fx.deliveryID, fx.translate)
			w.wantEnvelopeMapping(t, first, fx)
			w.wantNoTenantClaims(t, first, fx)

			// The first delivery creates exactly one request, in the registration's
			// team and org.
			created := w.submitEnvelope(t, integration, first)
			if !created.Created {
				t.Fatalf("the first delivery did not create a request (answered with request %d)", created.Request.ID)
			}
			if created.Request.TeamID != w.team || created.Request.OrgID != w.org {
				t.Errorf("the request landed in org %d team %d, want the registration's %d/%d: a payload cannot choose its tenant",
					created.Request.OrgID, created.Request.TeamID, w.org, w.team)
			}
			if created.Request.SourceExternalID != first.ExternalID {
				t.Errorf("source_external_id = %q, want the vendor's immutable object id %q", created.Request.SourceExternalID, first.ExternalID)
			}
			// And the assertion on the RECORD, not just the envelope: a claim the
			// payload made is absent from what intake stored.
			w.wantStoredWithoutClaims(t, created.Request.ID, fx)

			// 2. Event-level replay: the same delivery again. One request, nothing
			// written, the same answer.
			replay := w.envelope(t, fx, fmt.Sprintf(fx.payload, vendorURL), fx.deliveryID, fx.translate)
			again := w.submitEnvelope(t, integration, replay)
			if again.Request.ID != created.Request.ID {
				t.Fatalf("a redelivery answered with request %d, want the original %d", again.Request.ID, created.Request.ID)
			}
			if again.Created {
				t.Error("a redelivery reported that it created a request")
			}
			if got := w.requestsFor(first.ExternalID); got != 1 {
				t.Fatalf("%d requests for one vendor object after a redelivery, want 1", got)
			}

			// 3. Object-level identity: a later event about the same object appends
			// to the existing request and never creates a second one.
			second := w.envelope(t, fx, fx.second, fx.secondID, fx.translate)
			if second.EventKey() == first.EventKey() {
				t.Fatal("the second event carries the first delivery's event id: the fixture does not test a distinct event")
			}
			if second.ObjectKey() != first.ObjectKey() {
				t.Fatalf("the second event has object key %+v, want the first event's %+v: a renamed key or a comment is still one vendor object",
					second.ObjectKey(), first.ObjectKey())
			}
			appended := w.submitEnvelope(t, integration, second)
			if appended.Created {
				t.Fatalf("a second event about vendor object %q created a second request %d", first.ExternalID, appended.Request.ID)
			}
			if appended.Request.ID != created.Request.ID {
				t.Fatalf("a second event answered with request %d, want %d", appended.Request.ID, created.Request.ID)
			}
			// The append itself: the event's text becomes a row on the existing
			// request's discussion, which is where inbound vendor commentary belongs
			// until a triager decides what to do with it.
			before := w.requestComments(created.Request.ID)
			if _, _, err := w.svc.CommentOnRequest(w.ctx, integration, w.team, created.Request.ID,
				service.RequestDiscussionInput{Kind: fx.secondComment, Body: second.Body}); err != nil {
				t.Fatalf("append the second event's text: %v", err)
			}
			if got := w.requestComments(created.Request.ID); got != before+1 {
				t.Errorf("comments = %d after the append, want %d", got, before+1)
			}
			if got := w.requestsFor(first.ExternalID); got != 1 {
				t.Errorf("%d requests for one vendor object after two events, want 1", got)
			}
			w.wantUnadvanced(t, created.Request.ID, fixtureState{state: domain.RequestStatusNew, rev: 1})
		})
	}

	// The whole table wrote one request per vendor object and nothing anywhere else:
	// no fixture crossed into the other team or the other org.
	if got := w.count(`SELECT count(*) FROM team_requests WHERE org_id = $1`, w.org); got != len(fixtures) {
		t.Errorf("%d requests in the inbox org, want one per fixture (%d)", got, len(fixtures))
	}
	if got := w.count(`SELECT count(*) FROM team_requests WHERE team_id = $1`, w.otherTeam); got != 0 {
		t.Errorf("%d requests landed in another team", got)
	}
	if got := w.count(`SELECT count(*) FROM team_requests WHERE org_id = $1`, w.otherOrg); got != 0 {
		t.Errorf("%d requests landed in another org", got)
	}

	// 5. No outbound request: nothing intake did above reached the canary, so the
	// external_url each payload carried was stored text and never a fetch target.
	if hits := canary.hits.Load(); hits != 0 {
		t.Errorf("request intake made %d outbound request(s) to the vendor URL a payload named: an external_url is inert text (§5's SSRF rule)", hits)
	}
}

// TestRequestEnvelopeNoOutboundRequest is the guard as its own test, so the
// receive-only posture fails visibly even if the contract table changes shape. It
// drives a whole intake sequence — create, redelivery, append — with an
// external_url pointing at a live canary, and the canary must stay silent.
func TestRequestEnvelopeNoOutboundRequest(t *testing.T) {
	w := newReqSubmitDB(t)
	canary := &envelopeCanary{}
	canary.start(t)
	integration := w.integrationAccount("no-outbound")

	payload := fmt.Sprintf(`{"id":"outbound-check-1","name":"Does intake fetch this?","createdAt":"2026-11-11T19:55:00Z","html_url":%q}`, canary.url("/vendor-object"))
	translate := func(t *testing.T, raw []byte, deliveryID string) domain.RequestEnvelopeInput {
		t.Helper()
		var e struct {
			ID        string `json:"id"`
			Name      string `json:"name"`
			CreatedAt string `json:"createdAt"`
			HTMLURL   string `json:"html_url"`
		}
		if err := json.Unmarshal(raw, &e); err != nil {
			t.Fatalf("parse payload: %v", err)
		}
		occurred, err := time.Parse(time.RFC3339, e.CreatedAt)
		if err != nil {
			t.Fatalf("parse createdAt: %v", err)
		}
		return domain.RequestEnvelopeInput{
			ExternalID: e.ID, EventID: deliveryID, Event: domain.RequestEnvelopeCreated,
			OccurredAt: occurred, Subject: e.Name, Body: "Body.", ExternalURL: e.HTMLURL, Raw: raw,
		}
	}

	env := w.rawEnvelope(t, "guard/integration", payload, "delivery-1", translate)
	if env.ExternalURL != canary.url("/vendor-object") {
		t.Errorf("the envelope rewrote external_url = %q: it is stored text, verbatim", env.ExternalURL)
	}
	created := w.submitEnvelope(t, integration, env)
	if !created.Created {
		t.Fatal("the first delivery did not create a request")
	}
	w.submitEnvelope(t, integration, w.rawEnvelope(t, "guard/integration", payload, "delivery-1", translate))
	if _, _, err := w.svc.CommentOnRequest(w.ctx, integration, w.team, created.Request.ID,
		service.RequestDiscussionInput{Body: "still no fetch"}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if hits := canary.hits.Load(); hits != 0 {
		t.Fatalf("intake fetched the payload's URL %d time(s): intake is receive-only and an external_url is never fetched", hits)
	}
}

// ---- fixture plumbing ----

// integrationAccount provisions the account a registered integration submits as
// (phase 1 authenticates the integration; here it is the team member standing in for
// it) and returns its principal.
func (d *reqSubmitDB) integrationAccount(label string) authz.Principal {
	d.t.Helper()
	var id int
	if err := d.db.QueryRow(d.ctx,
		`INSERT INTO users (subject, email, kind) VALUES ($1, $2, $3) RETURNING id`,
		fmt.Sprintf("%s-%s", label, uniq("integration")), label+"@integration.test", domain.UserKindAgent).Scan(&id); err != nil {
		d.t.Fatalf("create the integration account: %v", err)
	}
	if _, err := d.db.Exec(d.ctx, `INSERT INTO team_members (team_id, user_id, role) VALUES ($1, $2, 'member')`, d.team, id); err != nil {
		d.t.Fatalf("add the integration account to the inbox team: %v", err)
	}
	return d.principal(id)
}

// envelope builds one envelope exactly as a phase-1 endpoint will: the binding from
// the registration (trusted), the fields from the vendor payload (untrusted), the
// clock from AMP.
func (d *reqSubmitDB) envelope(t *testing.T, fx envelopeFixture, payload, deliveryID string,
	translate func(*testing.T, []byte, string) domain.RequestEnvelopeInput) *domain.RequestEnvelope {
	t.Helper()
	return d.rawEnvelope(t, fx.integration, payload, deliveryID, translate)
}

// rawEnvelope is envelope with an explicit integration id, for the guard test.
func (d *reqSubmitDB) rawEnvelope(t *testing.T, integrationID, payload, deliveryID string,
	translate func(*testing.T, []byte, string) domain.RequestEnvelopeInput) *domain.RequestEnvelope {
	t.Helper()
	// The binding is the registration's: an integration id plus the one org and team
	// it is bound to. No part of it comes from the payload.
	binding := domain.IntegrationBinding{IntegrationID: integrationID, OrgID: d.org, TeamID: d.team}
	in := translate(t, []byte(payload), deliveryID)
	env, err := domain.NewRequestEnvelope(binding, in, d.now)
	if err != nil {
		t.Fatalf("build the envelope for %s: %v", integrationID, err)
	}
	return env
}

// submitEnvelope is what phase 1 will do with a validated envelope: hand its trusted
// tenant and its bounded fields to the one authorized intake path. It is
// deliberately thin — it invents no field and knows no vendor.
func (d *reqSubmitDB) submitEnvelope(t *testing.T, p authz.Principal, env *domain.RequestEnvelope) *service.SubmitRequestResult {
	t.Helper()
	res, err := d.svc.SubmitRequest(d.ctx, p, service.SubmitRequestInput{
		TeamID:           env.TeamID(),
		Source:           env.SourceKind(),
		SourceExternalID: env.ExternalID,
		Subject:          env.Subject,
		InputBrief:       env.Body,
		Metadata:         env.Metadata(),
	})
	if err != nil {
		t.Fatalf("submit the envelope: %v", err)
	}
	return res
}

// requestsFor is how many request rows carry one source external id in the fixture's
// inbox team.
func (d *reqSubmitDB) requestsFor(externalID string) int {
	d.t.Helper()
	return d.count(`SELECT count(*) FROM team_requests WHERE team_id = $1 AND source_kind = 'connector' AND source_external_id = $2`,
		d.team, externalID)
}

// requestComments is how many discussion rows a request has.
func (d *reqSubmitDB) requestComments(requestID int) int {
	d.t.Helper()
	return d.count(`SELECT count(*) FROM request_comments WHERE request_id = $1`, requestID)
}

// fixtureState is the part of a request's lifecycle an inbound event must never
// move.
type fixtureState struct {
	state domain.RequestStatus
	rev   int
}

// wantUnadvanced proves an inbound vendor event changed none of the record: state,
// revision, claim and assignee are exactly what intake wrote, the only history row
// is the submission's own, and nothing was linked or planned.
func (d *reqSubmitDB) wantUnadvanced(t *testing.T, requestID int, want fixtureState) {
	t.Helper()
	stored, err := d.svc.Repo.GetRequest(d.ctx, requestID, d.org, d.team)
	if err != nil {
		t.Fatalf("read request %d: %v", requestID, err)
	}
	if stored.State != want.state || stored.Rev != want.rev {
		t.Errorf("request %d is %s rev %d after inbound events; want %s rev %d (an event never advances a request)",
			requestID, stored.State, stored.Rev, want.state, want.rev)
	}
	if stored.ClaimUserID != nil || stored.OwnerUserID != nil {
		t.Errorf("request %d has claim %v / assignee %v: inbound events claim and assign nothing", requestID, stored.ClaimUserID, stored.OwnerUserID)
	}
	if got := d.count(`SELECT count(*) FROM request_events WHERE request_id = $1 AND action <> 'submit'`, requestID); got != 0 {
		t.Errorf("%d non-submit history rows: an inbound event appends text, never a transition", got)
	}
	if got := d.count(`SELECT count(*) FROM request_links WHERE request_id = $1`, requestID); got != 0 {
		t.Errorf("%d links written by inbound events: intake routes and plans nothing", got)
	}
}

// wantEnvelopeMapping pins what the payload -> envelope translation must produce: the
// vendor's immutable object id, its delivery id as the event id, one of the five
// verbs, the vendor's own names kept as labels, and the subject a team reads.
func (d *reqSubmitDB) wantEnvelopeMapping(t *testing.T, env *domain.RequestEnvelope, fx envelopeFixture) {
	t.Helper()
	if env.ExternalID != fx.wantExternalID {
		t.Errorf("external_id = %q, want the vendor's immutable object id %q", env.ExternalID, fx.wantExternalID)
	}
	if env.EventID != fx.deliveryID {
		t.Errorf("event_id = %q, want the delivery id %q", env.EventID, fx.deliveryID)
	}
	if env.Event != fx.wantEvent {
		t.Errorf("event = %q, want %q", env.Event, fx.wantEvent)
	}
	if env.VendorEvent != fx.wantVendorEvent {
		t.Errorf("vendor_event = %q, want the vendor's own name %q kept as a label", env.VendorEvent, fx.wantVendorEvent)
	}
	if !strings.Contains(env.Subject, fx.wantSubject) {
		t.Errorf("subject = %q, want it to contain %q", env.Subject, fx.wantSubject)
	}
	if env.Subject == "" || env.Body == "" {
		t.Error("the envelope carries no subject or body: the team would see a request with nothing in it")
	}
	if len(env.PayloadHash) != 64 {
		t.Errorf("payload_hash = %q, want a lowercase hex sha256", env.PayloadHash)
	}
	if env.ExternalURL == "" {
		t.Error("external_url is empty: the vendor link is kept as inert text so a triager can open it")
	}
	if env.OrgID() != d.org || env.TeamID() != d.team {
		t.Errorf("envelope tenant = %d/%d, want the registration's %d/%d", env.OrgID(), env.TeamID(), d.org, d.team)
	}
	if env.Binding().IntegrationID != fx.integration {
		t.Errorf("integration = %q, want the registration %q", env.Binding().IntegrationID, fx.integration)
	}
	// The object key is 053's unique index, so intake and the database agree on what
	// "the same vendor object" means.
	if key := env.ObjectKey(); key.TeamID != d.team || key.SourceKind != domain.RequestSourceConnector || key.ExternalID != env.ExternalID {
		t.Errorf("ObjectKey() = %+v, want (team %d, connector, %q)", key, d.team, env.ExternalID)
	}
}

// wantStoredWithoutClaims reads the row intake wrote and proves none of the
// payload's reserved claims reached the stored metadata. It is the half
// wantNoTenantClaims cannot see: the envelope is built by the trusted binding, but
// only the row says what a team's queue actually holds.
func (d *reqSubmitDB) wantStoredWithoutClaims(t *testing.T, requestID int, fx envelopeFixture) {
	t.Helper()
	stored, err := d.svc.Repo.GetRequest(d.ctx, requestID, d.org, d.team)
	if err != nil {
		t.Fatalf("read request %d: %v", requestID, err)
	}
	reservedSeen := 0
	for key := range fx.claims {
		if !domain.IsReservedRequestEnvelopeKey(key) {
			continue
		}
		reservedSeen++
		if _, ok := stored.Metadata[key]; ok {
			t.Errorf("the stored request's metadata carries %q, a claim the payload made: a payload can never name a tenant, a destination or an identity", key)
		}
	}
	if reservedSeen == 0 {
		t.Fatalf("%s claims no reserved key, so the stored-record assertion proves nothing", fx.name)
	}
	// The tenant the row carries is the registration's, in both halves.
	if stored.OrgID != d.org || stored.TeamID != d.team {
		t.Errorf("stored request is org %d team %d, want the registration's %d/%d", stored.OrgID, stored.TeamID, d.org, d.team)
	}
	if stored.SourceKind != domain.RequestSourceConnector {
		t.Errorf("source_kind = %q, want connector", stored.SourceKind)
	}
}

// wantNoTenantClaims proves the payload's own tenancy and routing claims did not
// reach the request's metadata, and that the envelope counted dropping them.
func (d *reqSubmitDB) wantNoTenantClaims(t *testing.T, env *domain.RequestEnvelope, fx envelopeFixture) {
	t.Helper()
	md := env.Metadata()
	reservedSeen := 0
	for key := range fx.claims {
		if !domain.IsReservedRequestEnvelopeKey(key) {
			continue
		}
		reservedSeen++
		if _, ok := md[key]; ok {
			t.Errorf("metadata carries the reserved key %q: a payload may not name a tenant or a destination", key)
		}
	}
	if reservedSeen == 0 {
		t.Fatalf("%s claims no reserved key, so the tenant assertion proves nothing", fx.name)
	}
	if env.ReservedKeysDropped() < reservedSeen {
		t.Errorf("%s claims %d reserved keys but ReservedKeysDropped() = %d", fx.name, reservedSeen, env.ReservedKeysDropped())
	}
	// What a payload may legitimately label survives: the envelope drops claims, not
	// vendor classification.
	for key, want := range fx.claims {
		if domain.IsReservedRequestEnvelopeKey(key) || strings.Contains(key, "project") {
			continue
		}
		if got, ok := md[key]; !ok || got != want {
			t.Errorf("metadata[%q] = %q (present %v), want the payload's label %q kept", key, got, ok, want)
		}
	}
	if _, ok := md["public_ref"]; ok {
		t.Error("the envelope's metadata carries public_ref: the reference is minted at submit, never sent in")
	}
}
