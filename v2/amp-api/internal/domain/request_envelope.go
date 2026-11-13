package domain

// External request envelopes (epic 202, story 549, AMP #3970; approved product
// contract artifact 36 v2 §5 and its deferred list, which ends phase 0 at the
// envelope; build slices H1/H2 of
// discoveries/team-inbox-approved-build-slices.md, specified field by field in
// discoveries/team-request-external-envelope.md).
//
// A request can arrive from outside the plan: a GitHub issue, a Jira issue, a
// Wiz finding, a pricing alarm. Each vendor speaks its own shape, and letting
// each one reach intake would put vendor knowledge in the one place that must
// not have any — the code that writes a team's queue. So every external source
// is first normalized into ONE envelope, and intake sees only that. This file is
// the envelope: what it holds, what it refuses, and the two identities its
// replay rules are built on. Vendor translation (payload -> envelope) is
// deliberately NOT here.
//
// PHASE 0 IS A CONTRACT, NOT A CONNECTOR. What ships with this file is the type
// plus its contract tests: no HTTP server, no signed endpoint, no registration
// table, no sealed secret, no vendor package, no credential, no outbound call of
// any kind. The signed intake endpoint is phase 1 and vendor adapters are phase
// 2, each behind its own decision; a bounded type and fixtures that prove the
// four real vendor shapes fit it are what phase 1 needs to exist first.
//
// TRUSTED HALF, UNTRUSTED HALF — the split is the type system's, not a rule.
// An envelope is built from two inputs that never mix:
//
//	IntegrationBinding   which registered connector instance delivered this, and
//	                     the one org and team it is bound to. It comes from the
//	                     registration row whose own secret signed the delivery
//	                     (phase 1), so it is the tenant by construction.
//	RequestEnvelopeInput everything extracted from the vendor's payload: the
//	                     vendor object id, the delivery id, the event, the text,
//	                     the hints, the raw bytes. It has NO integration id and
//	                     NO org/team/project field at all.
//
// That is why "the payload cannot choose its tenant" needs no check to hold: a
// payload cannot express one. The rest of this file only makes sure the *other*
// route to a destination is closed too — a vendor label named `team_id` or
// `project` cannot ride the envelope's metadata into a later reader
// (RequestEnvelopeReservedKeys), and the payload's own copies of those claims are
// counted and dropped rather than stored.
//
// TWO IDENTITIES, AND THEY ANSWER TWO DIFFERENT QUESTIONS (D12, #3654 C3):
//
//	EventKey   (integration_id, event_id) — "have I handled this delivery?"
//	           Delivery idempotency: a replay of the same event is a no-op that
//	           returns the first result, never a second anything. This is
//	           ClaimGitHubDelivery's rule (039) applied to requests, and its
//	           table is phase 1's.
//	ObjectKey  (team_id, connector, external_id) — "is this the same vendor
//	           object?" Request identity: a second event about one issue (a
//	           comment, a label, a renamed key) appends to the request that
//	           already exists and never creates a second one, and never advances
//	           its state. It is exactly 053's partial unique index
//	           uq_team_requests_source_external, so the key this file computes and
//	           the key the database enforces are the same key.
//
// The external id is the vendor's IMMUTABLE object id, not its display name
// (D12: "Jira keys rename and GitHub issues move"; #3658 question 1). The
// human-facing key or URL is a label on the envelope, kept for reading and never
// used to dedup — the contract fixtures in cmd/server prove a renamed issue
// still lands on one request.
//
// BOUNDS. Two different answers, because the two things are different:
//
//	identities (integration id, external id, event id) are REFUSED over bound,
//	           never truncated. A truncated key identifies something else: two
//	           event ids differing past the cut become one delivery, and a cut
//	           external id dedups two vendor objects onto one request.
//	text (subject, body) is refused exactly as a web form's is, by the same
//	           domain functions (NormalizeRequestSubject, NormalizeRequestBrief),
//	           so an envelope can only produce a submission intake accepts and a
//	           connector sees the same 400 a requester does.
//	the raw excerpt is TRUNCATED, because it is a sample by definition: it exists
//	           so an operator can see which payload produced a request, never so
//	           anything can be replayed from it. The raw body itself is not kept.
//
// What this file deliberately does not do: verify signatures, decide whether an
// integration may deliver (registration and its allowlisted events), store
// anything, look anything up, fetch any URL (an external_url is inert text and
// nothing more, the SSRF rule of §5), or turn an envelope into a request. Each
// belongs to the phase that owns it, and the contract tests guard the one thing
// this file must never grow: a way out.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Envelope bounds. Where a bound belongs to something else it is REUSED rather
// than restated (the body is a request brief, the external id is the column's),
// so a change to the product's real bound moves here with it instead of leaving
// two numbers that disagree.
const (
	// MaxRequestEnvelopeIntegrationIDLen caps a registered integration id
	// ("github-issues/acme-ops"): the vendor and the instance, bounded so a
	// registration id stays a token. Phase 1's registration table is where it is
	// validated for real.
	MaxRequestEnvelopeIntegrationIDLen = 200
	// MaxRequestEnvelopeEventIDLen caps a delivery/event id. It matches the
	// GitHub hook's own delivery-id check (service/github_hook.go: 1-100), so one
	// bound applies to a delivery id wherever in AMP it appears.
	MaxRequestEnvelopeEventIDLen = 100
	// MaxRequestEnvelopeExternalIDLen is a connector's vendor object id: the
	// bound is team_requests.source_external_id's (053), reused so the envelope
	// can never produce a value the column would refuse.
	MaxRequestEnvelopeExternalIDLen = MaxRequestExternalIDLen
	// MaxRequestEnvelopeEventNameLen caps the vendor's own event name and its
	// action ("jira:issue_updated", "severity_changed"). They are labels the
	// envelope never branches on, so a longer one is a translator mistake.
	MaxRequestEnvelopeEventNameLen = 100
	// MaxRequestEnvelopeURLLen caps an external_url. It is inert display text:
	// stored, linked to, never fetched.
	MaxRequestEnvelopeURLLen = 2000
	// MaxRequestEnvelopeExcerptLen caps raw_excerpt in characters. A JSON
	// delivery's first kilobyte names the object, the event and enough of the
	// body for an operator to recognise it; the rest is not a diagnostic.
	MaxRequestEnvelopeExcerptLen = 1000
	// RequestEnvelopeClockSkew is how far ahead of our own clock a vendor
	// occurred_at may be. occurred_at is first-report-wins, so a timestamp in the
	// future could claim a moment that never happened; a day of slack absorbs
	// ordinary vendor clock drift without allowing that.
	RequestEnvelopeClockSkew = 24 * time.Hour
)

// RequestEnvelopeEvent is what happened to the vendor object, in the one
// vocabulary every source shares. It is deliberately closed: a translator speaks
// these verbs and keeps the vendor's own event name in VendorEvent, so intake
// never branches on a vendor string and a new vendor cannot widen the vocabulary
// by arriving with an event name nobody expected (#3658 question 2's proposal,
// decided here for phase 0).
type RequestEnvelopeEvent string

const (
	// RequestEnvelopeCreated: the vendor object came into existence — an issue
	// opened, a finding raised, an alarm first crossed its threshold.
	RequestEnvelopeCreated RequestEnvelopeEvent = "created"
	// RequestEnvelopeUpdated: bounded fields of the object changed — a severity
	// bump, a re-keyed issue, a renamed title.
	RequestEnvelopeUpdated RequestEnvelopeEvent = "updated"
	// RequestEnvelopeCommented: someone spoke on the object.
	RequestEnvelopeCommented RequestEnvelopeEvent = "commented"
	// RequestEnvelopeClosed: the object was resolved or closed at the vendor.
	RequestEnvelopeClosed RequestEnvelopeEvent = "closed"
	// RequestEnvelopeAlert: the object is an alert rather than a tracked item
	// (a security finding, a pricing threshold). It is a verb of its own because
	// an alert's lifecycle never maps cleanly onto an issue's.
	RequestEnvelopeAlert RequestEnvelopeEvent = "alert"
)

// RequestEnvelopeEvents are the five verbs, in the order the discovery lists
// them. Nothing in phase 0 stores them, so there is no schema CHECK to mirror
// yet; NormalizeRequestEnvelopeEvent is the one place that decides membership.
var RequestEnvelopeEvents = []RequestEnvelopeEvent{
	RequestEnvelopeCreated, RequestEnvelopeUpdated, RequestEnvelopeCommented,
	RequestEnvelopeClosed, RequestEnvelopeAlert,
}

// RequestEnvelopeReservedKeys are metadata names an envelope never carries, in
// any casing. Each one is a name that something downstream could mistake for a
// decision:
//
//   - the tenant (org, team, organization, tenant) and the routing target
//     (project): where a request lands is the registration's, read from the
//     trusted binding. The contract's rule is "metadata is never an
//     authorization decision"; this list makes the tenancy case structural
//     rather than a convention a translator has to remember.
//   - who asked and who is acting (requester, actor, owner, assignee): those come
//     from the principal at submit, never from content.
//   - the identity intake dedups on (integration_id, source_kind,
//     source_external_id, public_ref): a label must never be able to look like
//     the key.
//
// A payload's claim under one of these names is DROPPED, not refused (#3658:
// "the payload's claimed project/tenant is ignored"). Refusing would turn an
// ordinary vendor payload — many of them name an org, a project or a status —
// into a delivery that can never be accepted, and the honest behaviour for a
// claim AMP is never going to read is to not store it. The envelope counts what
// it dropped (ReservedKeysDropped) so phase 1 can record that a payload tried.
var RequestEnvelopeReservedKeys = []string{
	"org", "org_id", "organization", "organization_id", "tenant",
	"team", "team_id", "project", "project_id",
	"integration", "integration_id", "source_kind", "source_external_id", "public_ref",
	"requester", "requester_user_id", "actor", "owner", "owner_user_id", "assignee",
}

// IsReservedRequestEnvelopeKey reports whether a hint key is one an envelope
// never carries; case is ignored, because a payload's casing is not a promise.
func IsReservedRequestEnvelopeKey(key string) bool {
	k := strings.ToLower(strings.TrimSpace(key))
	return slices.Contains(RequestEnvelopeReservedKeys, k)
}

// IntegrationBinding is the trusted half of an envelope: the registered
// integration instance that delivered a request, and the ONE org and team it is
// bound to.
//
// In phase 1 this value is built by the intake endpoint from the registration row
// it looked up by the integration id in the route, after verifying the delivery's
// signature with that registration's own sealed secret. The payload is never a
// source for any of the three fields — that is why it does not need a "tenant"
// field of its own to be safe: there is nothing to compare, because the
// untrusted half cannot name one.
//
// Both ids are required and positive: a binding of org 0 or team 0 would be a
// registration that lost its tenant, and an envelope carrying it could only fail
// later, somewhere less obvious.
type IntegrationBinding struct {
	// IntegrationID is the registration's id: the vendor and the instance, e.g.
	// "github-issues/acme-ops". It gates authorization and delivery dedup, so it
	// is a typed identity and never free-form metadata.
	IntegrationID string
	// OrgID and TeamID are the tenant the requests this integration delivers
	// belong to. They come from the registration row, never from a payload.
	OrgID  int
	TeamID int
}

// Validate checks the binding is one an envelope can be built on.
func (b IntegrationBinding) Validate() error {
	if _, err := validateEnvelopeToken("integration_id", b.IntegrationID, MaxRequestEnvelopeIntegrationIDLen); err != nil {
		return err
	}
	if b.OrgID <= 0 || b.TeamID <= 0 {
		return errors.New("an integration is bound to exactly one org and one team, read from its registration: an envelope without a tenant could only fail later, somewhere less obvious")
	}
	return nil
}

// RequestEnvelopeInput is the untrusted half: everything a vendor translator
// extracted from one delivery. It has no integration id and no org, team or
// project field, so a payload cannot name its destination and a translator cannot
// be tricked into carrying one.
//
// Every field is bounded by NewRequestEnvelope; see the bounds above for which
// ones are refused and which are truncated.
type RequestEnvelopeInput struct {
	// ExternalID is the vendor's IMMUTABLE object id (a Jira issue id, a GitHub
	// issue id, a Wiz finding id, an alarm id) — the object-level identity D12
	// dedups on. The display key ("OPS-412", "owner/repo#42") belongs in Labels.
	ExternalID string
	// EventID is this delivery's own id (a webhook GUID, a notification id). It
	// is the event-level identity: a replay of the same event changes nothing.
	EventID string

	// Event is what happened, in the envelope's closed vocabulary. The vendor's
	// own names go in VendorEvent and Action, where they are labels.
	Event       RequestEnvelopeEvent
	VendorEvent string
	Action      string

	// OccurredAt is when the vendor says it happened. It is first-report-wins
	// downstream, so it may not be zero and may not be far in the future.
	OccurredAt time.Time

	// Subject is the one line a team sees in its queue, and Body the request
	// itself, as markdown. Both are untrusted text: they are bounded exactly as a
	// web form's are, and they reach an agent only through QuoteUntrusted.
	Subject string
	Body    string

	// ExternalURL is the vendor's canonical URL for the object. It is stored as
	// inert text and never fetched server-side (§5's SSRF rule); only http and
	// https are accepted so no surface can be handed something actionable.
	ExternalURL string

	// Labels are the vendor's own classification of the object (severity, area,
	// jira_key, repository) and Fields are the translator's normalized hints.
	// Both are bounded like metadata, neither gates anything, and a key reserved
	// by RequestEnvelopeReservedKeys is dropped from either.
	Labels map[string]string
	Fields map[string]string

	// Raw is the delivery body as received. The envelope keeps only its sha256
	// and a short excerpt; the raw bytes are never retained, so this is the one
	// field that must never be logged.
	Raw []byte
}

// RequestEnvelope is one normalized external delivery: the trusted tenant it is
// bound to, the two identities its replay rules use, the bounded text a team will
// see, and a way to prove which payload produced it.
//
// It is immutable once built. The tenant and integration id are unexported and
// reachable only through accessors, and the hint maps are copied on the way in
// and out, so no caller can move an envelope to another tenant or widen what it
// carries after NewRequestEnvelope has bounded it.
type RequestEnvelope struct {
	binding IntegrationBinding

	ExternalID  string
	EventID     string
	Event       RequestEnvelopeEvent
	VendorEvent string
	Action      string

	OccurredAt time.Time
	// ReceivedAt is when AMP received the delivery, from AMP's clock. It is a
	// constructor argument rather than an input field because Arrival is not the
	// payload's to claim: first-write-wins timestamps and every age a team sees
	// are measured from it.
	ReceivedAt time.Time

	Subject     string
	Body        string
	ExternalURL string

	labels map[string]string
	fields map[string]string
	// dropped is how many hint keys named something only the trusted half may
	// decide (see RequestEnvelopeReservedKeys). Phase 1 records it; nothing reads
	// it to decide anything.
	dropped int

	// PayloadHash is the lowercase hex sha256 of the raw body: stable for
	// identical bytes, so an operator can tie a request back to the delivery that
	// produced it without AMP keeping the body.
	PayloadHash string
	// RawExcerpt is a bounded, single-line sample of the raw body — enough to
	// recognise it, never enough to replay it.
	RawExcerpt string
}

// RequestEnvelopeEventKey is the delivery identity: one registered integration
// plus one vendor event id. Phase 1 claims it per delivery (the
// ClaimGitHubDelivery shape: unique per integration, one handler wins, a replay
// is a no-op returning the first result, a 5xx-ending attempt may be redelivered).
type RequestEnvelopeEventKey struct {
	IntegrationID string
	EventID       string
}

// RequestEnvelopeObjectKey is the request identity: one team, the connector
// source, and the vendor's immutable object id. It is exactly migration 053's
// uq_team_requests_source_external index, so "the same object" means the same
// thing to this file and to the database.
type RequestEnvelopeObjectKey struct {
	TeamID     int
	SourceKind RequestSourceKind
	ExternalID string
}

// NewRequestEnvelope bounds and normalizes one delivery into an envelope.
//
// The three arguments are the three things an envelope is made of, kept apart on
// purpose: binding is the trusted registration (and is the only source of the
// tenant), in is what the payload said, and receivedAt is AMP's clock.
//
// It is pure: no clock read, no I/O, no network, no storage. A caller that gives
// it the same arguments gets the same envelope, which is what lets a fixture
// assert an exact hash.
func NewRequestEnvelope(binding IntegrationBinding, in RequestEnvelopeInput, receivedAt time.Time) (*RequestEnvelope, error) {
	if err := binding.Validate(); err != nil {
		return nil, err
	}
	externalID, err := validateEnvelopeToken("external_id", in.ExternalID, MaxRequestEnvelopeExternalIDLen)
	if err != nil {
		return nil, err
	}
	eventID, err := validateEnvelopeToken("event_id", in.EventID, MaxRequestEnvelopeEventIDLen)
	if err != nil {
		return nil, err
	}
	event, err := NormalizeRequestEnvelopeEvent(string(in.Event))
	if err != nil {
		return nil, err
	}
	vendorEvent, err := boundedEnvelopeLabel("vendor_event", in.VendorEvent, MaxRequestEnvelopeEventNameLen)
	if err != nil {
		return nil, err
	}
	action, err := boundedEnvelopeLabel("action", in.Action, MaxRequestEnvelopeEventNameLen)
	if err != nil {
		return nil, err
	}
	if in.OccurredAt.IsZero() {
		return nil, errors.New("occurred_at is required: the vendor's own event time is the first report, and it is kept first-write-wins")
	}
	if in.OccurredAt.After(receivedAt.Add(RequestEnvelopeClockSkew)) {
		return nil, fmt.Errorf("occurred_at is more than %s ahead of the clock AMP received the delivery on: a vendor time in the future could claim a moment that never happened", RequestEnvelopeClockSkew)
	}
	if receivedAt.IsZero() {
		return nil, errors.New("received_at is required: when AMP received a delivery is AMP's to stamp, never the payload's")
	}
	subject, err := NormalizeRequestSubject(in.Subject)
	if err != nil {
		return nil, err
	}
	body, err := NormalizeRequestBrief("body", in.Body)
	if err != nil {
		return nil, err
	}
	externalURL, err := normalizeEnvelopeURL(in.ExternalURL)
	if err != nil {
		return nil, err
	}
	labels, droppedLabels, err := normalizeEnvelopeHints("labels", in.Labels)
	if err != nil {
		return nil, err
	}
	fields, droppedFields, err := normalizeEnvelopeHints("fields", in.Fields)
	if err != nil {
		return nil, err
	}
	if both := sharedKeys(labels, fields); len(both) > 0 {
		// Not merged with one winning: a translator that classifies one hint as
		// both a vendor label and a normalized field has not decided what it is,
		// and guessing would hide the mistake behind a value.
		return nil, fmt.Errorf("labels and fields both carry %s: a hint is one or the other", strings.Join(both, ", "))
	}
	if total := len(labels) + len(fields); total > MaxMetadataKeys {
		return nil, fmt.Errorf("labels and fields carry %d hints together, over the %d a request's metadata holds: an envelope's hints all become metadata", total, MaxMetadataKeys)
	}
	if len(in.Raw) == 0 {
		return nil, errors.New("raw is required: payload_hash is what lets an operator prove which delivery produced a request")
	}

	sum := sha256.Sum256(in.Raw)
	return &RequestEnvelope{
		binding:     binding,
		ExternalID:  externalID,
		EventID:     eventID,
		Event:       event,
		VendorEvent: vendorEvent,
		Action:      action,
		OccurredAt:  in.OccurredAt.UTC(),
		ReceivedAt:  receivedAt.UTC(),
		Subject:     subject,
		Body:        body,
		ExternalURL: externalURL,
		labels:      labels,
		fields:      fields,
		dropped:     droppedLabels + droppedFields,
		PayloadHash: hex.EncodeToString(sum[:]),
		RawExcerpt:  envelopeExcerpt(in.Raw),
	}, nil
}

// IntegrationID is the registration that delivered this envelope. It gates
// authorization and delivery dedup, so it is never a payload value.
func (e *RequestEnvelope) IntegrationID() string { return e.binding.IntegrationID }

// OrgID is the org the envelope's request belongs to, from the registration.
func (e *RequestEnvelope) OrgID() int { return e.binding.OrgID }

// TeamID is the inbox the envelope's request belongs to, from the registration.
func (e *RequestEnvelope) TeamID() int { return e.binding.TeamID }

// Binding is the trusted half the envelope was built on, for a caller that hands
// the whole thing on rather than three fields.
func (e *RequestEnvelope) Binding() IntegrationBinding { return e.binding }

// SourceKind is how a request from an envelope arrived: always a connector. It is
// a method rather than a field because an envelope from anything else would be a
// different type.
func (e *RequestEnvelope) SourceKind() RequestSourceKind { return RequestSourceConnector }

// Labels is a copy of the vendor's own classification hints.
func (e *RequestEnvelope) Labels() map[string]string { return copyHints(e.labels) }

// Fields is a copy of the translator's normalized hints.
func (e *RequestEnvelope) Fields() map[string]string { return copyHints(e.fields) }

// ReservedKeysDropped is how many hint keys the payload named that only the
// trusted half may decide. It is evidence, not input: an envelope's behaviour is
// the same whether this is 0 or 12.
func (e *RequestEnvelope) ReservedKeysDropped() int { return e.dropped }

// Metadata is the hints as the request's own metadata, which is what the create
// path takes. Labels and fields merge (they cannot collide — NewRequestEnvelope
// refuses a shared key), and Metadata.Clean is applied so the envelope can only
// produce metadata a create accepts.
func (e *RequestEnvelope) Metadata() Metadata {
	m := make(Metadata, len(e.labels)+len(e.fields))
	for k, v := range e.labels {
		m[k] = v
	}
	for k, v := range e.fields {
		m[k] = v
	}
	clean, err := m.Clean()
	if err != nil {
		// Unreachable by construction: every key and value went through the same
		// bounds Metadata.Split enforces, and NewRequestEnvelope refused the rest.
		// Returning the bounded set rather than panicking is the honest fallback
		// for a function whose contract cannot fail.
		return m
	}
	return clean
}

// EventKey is the delivery identity: (integration, event id).
func (e *RequestEnvelope) EventKey() RequestEnvelopeEventKey {
	return RequestEnvelopeEventKey{IntegrationID: e.binding.IntegrationID, EventID: e.EventID}
}

// ObjectKey is the request identity: (team, connector, vendor object id) — 053's
// partial unique index, computed here so both sides of a replay agree.
func (e *RequestEnvelope) ObjectKey() RequestEnvelopeObjectKey {
	return RequestEnvelopeObjectKey{
		TeamID:     e.binding.TeamID,
		SourceKind: RequestSourceConnector,
		ExternalID: e.ExternalID,
	}
}

// NormalizeRequestEnvelopeEvent validates an envelope verb (" Updated " is
// updated). It is required: an envelope that does not say what happened cannot be
// normalized into a request or an append.
func NormalizeRequestEnvelopeEvent(s string) (RequestEnvelopeEvent, error) {
	e := RequestEnvelopeEvent(strings.ToLower(strings.TrimSpace(s)))
	if e == "" {
		return "", errors.New("event is required: the envelope's verb says what happened to the vendor object")
	}
	if !slices.Contains(RequestEnvelopeEvents, e) {
		return "", fmt.Errorf("event must be one of %s (the vendor's own name belongs in vendor_event)", requestNames(RequestEnvelopeEvents))
	}
	return e, nil
}

// validateEnvelopeToken checks one identity: required, printable ASCII with no
// spaces (so it is safe in a log line, a header and a unique index), and within
// its bound.
//
// Over the bound it REFUSES and never truncates — see the file comment: a shorter
// key identifies something else.
func validateEnvelopeToken(field, s string, limit int) (string, error) {
	v := strings.TrimSpace(s)
	if v == "" {
		return "", fmt.Errorf("%s is required: it is an identity the envelope dedups on", field)
	}
	for _, r := range v {
		if r <= ' ' || r == 0x7f || r > '~' {
			return "", fmt.Errorf("%s must be printable ASCII with no spaces: an identity is a token, and it must be safe to log, to put in a header and to store in a unique index", field)
		}
	}
	if n := utf8.RuneCountInString(v); n > limit {
		return "", fmt.Errorf("%s is over %d characters (got %d): an identity is refused rather than truncated, because a shorter key identifies something else", field, limit, n)
	}
	return v, nil
}

// boundedEnvelopeLabel validates a vendor event name or action: optional, and
// bounded because it becomes a hint rather than a decision.
func boundedEnvelopeLabel(field, s string, limit int) (string, error) {
	v := strings.TrimSpace(s)
	if n := utf8.RuneCountInString(v); n > limit {
		return "", fmt.Errorf("%s is over %d characters (got %d)", field, limit, n)
	}
	for _, r := range v {
		if unicode.IsControl(r) {
			return "", fmt.Errorf("%s must be one line: a vendor event name is a label, not text", field)
		}
	}
	return v, nil
}

// normalizeEnvelopeURL validates an external_url. Only http and https are
// accepted, so a payload cannot hand a surface a javascript:, data: or file: URL;
// the value is otherwise opaque and inert, and nothing in AMP fetches it.
func normalizeEnvelopeURL(raw string) (string, error) {
	u := strings.TrimSpace(raw)
	if u == "" {
		return "", nil
	}
	if n := utf8.RuneCountInString(u); n > MaxRequestEnvelopeURLLen {
		return "", fmt.Errorf("external_url is over %d characters (got %d)", MaxRequestEnvelopeURLLen, n)
	}
	for _, r := range u {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return "", errors.New("external_url must be one URL with no spaces or control characters: it is inert text AMP stores and never fetches")
		}
	}
	lower := strings.ToLower(u)
	if !strings.HasPrefix(lower, "https://") && !strings.HasPrefix(lower, "http://") {
		return "", errors.New("external_url must be an http or https URL: it is stored as inert text and never fetched, and any other scheme is refused so no surface can be handed something actionable")
	}
	return u, nil
}

// normalizeEnvelopeHints bounds one hint map and returns it fresh, with any
// reserved key dropped and counted. See RequestEnvelopeReservedKeys for why a
// reserved key is dropped rather than refused.
func normalizeEnvelopeHints(field string, in map[string]string) (map[string]string, int, error) {
	if len(in) > MaxMetadataKeys {
		return nil, 0, fmt.Errorf("%s carries at most %d keys (got %d)", field, MaxMetadataKeys, len(in))
	}
	out := make(map[string]string, len(in))
	dropped := 0
	for k, v := range in {
		key := strings.TrimSpace(k)
		if key == "" {
			return nil, 0, fmt.Errorf("%s keys are 1-%d characters", field, MaxMetadataKeyLen)
		}
		if key != k {
			return nil, 0, fmt.Errorf("%s key %q has leading or trailing spaces", field, k)
		}
		if n := utf8.RuneCountInString(key); n > MaxMetadataKeyLen {
			return nil, 0, fmt.Errorf("%s key %q is over %d characters (got %d)", field, key, MaxMetadataKeyLen, n)
		}
		value := strings.TrimSpace(v)
		if n := utf8.RuneCountInString(value); n > MaxMetadataValueLen {
			return nil, 0, fmt.Errorf("%s[%q] is over %d characters (got %d)", field, key, MaxMetadataValueLen, n)
		}
		if IsReservedRequestEnvelopeKey(key) {
			dropped++
			continue
		}
		out[key] = value
	}
	return out, dropped, nil
}

// sharedKeys lists the keys two hint maps both carry, sorted for a stable error
// message.
func sharedKeys(a, b map[string]string) []string {
	var both []string
	for k := range a {
		if _, ok := b[k]; ok {
			both = append(both, k)
		}
	}
	slices.Sort(both)
	return both
}

// copyHints returns an independent copy, so a caller holding a hint map cannot
// change what an envelope carries.
func copyHints(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// envelopeExcerpt is the bounded, single-line sample of a raw body: invalid
// UTF-8 is replaced rather than stored, control characters become spaces, runs of
// whitespace collapse, and the result is cut at a character boundary with an
// ellipsis so a reader can see it was cut.
func envelopeExcerpt(raw []byte) string {
	line := strings.ToValidUTF8(string(raw), "\uFFFD")
	var b strings.Builder
	b.Grow(len(line))
	for _, r := range line {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			b.WriteByte(' ')
			continue
		}
		b.WriteRune(r)
	}
	flat := strings.Join(strings.Fields(b.String()), " ")
	runes := []rune(flat)
	if len(runes) <= MaxRequestEnvelopeExcerptLen {
		return flat
	}
	return string(runes[:MaxRequestEnvelopeExcerptLen]) + "…"
}
