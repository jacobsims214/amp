package domain

// The external-request envelope's own contract (AMP #3970, epic 202, story 549;
// build slices H1/H2 of discoveries/team-inbox-approved-build-slices.md, spec in
// discoveries/team-request-external-envelope.md).
//
// The four vendor fixtures themselves live in cmd/server/request_envelope_test.go,
// where the same shapes are driven through the real submit + comment paths; this
// file is the envelope's unit contract and runs with no database at all:
//
//  1. Bounds. Identities are refused over their limit and never truncated (a
//     shortened key identifies something else); text is refused exactly as a web
//     form's is; the raw excerpt is the one thing truncated, because it is a
//     sample.
//  2. Tenant. The payload cannot choose where its request lands: the tenant is the
//     trusted binding's, RequestEnvelopeInput has no such field, and a hint named
//     after a tenant/routing/identity key is dropped and counted.
//  3. Identity. The two keys are the two the replay rules use — (integration,
//     event) for a delivery and (team, connector, external id) for the object —
//     and payload_hash is stable for identical bytes and different for different
//     ones.
//
// Plus the guard the phase-0 boundary needs to stay true: the file imports no
// network, transport or storage package, so no outbound call can be written here
// without this test failing.

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode"
)

// envelopeBinding is the trusted half used throughout: one registered integration
// bound to one org and team.
func envelopeBinding() IntegrationBinding {
	return IntegrationBinding{IntegrationID: "github-issues/acme-ops", OrgID: 7, TeamID: 12}
}

// envelopeReceivedAt is AMP's clock for the fixtures: a fixed instant, so a hash
// and a bound assertion are reproducible.
func envelopeReceivedAt() time.Time {
	return time.Date(2026, 11, 13, 9, 30, 0, 0, time.UTC)
}

// envelopeInput is one well-formed payload: a GitHub issue opened.
func envelopeInput() RequestEnvelopeInput {
	return RequestEnvelopeInput{
		ExternalID:  "gh-issue-1841209",
		EventID:     "72d3162e-cc78-11e3-81ab-4c9367dc0958",
		Event:       RequestEnvelopeCreated,
		VendorEvent: "issues",
		Action:      "opened",
		OccurredAt:  envelopeReceivedAt().Add(-3 * time.Minute),
		Subject:     "CSV export of the team queue",
		Body:        "We need the queue as CSV weekly, for the Monday review.",
		ExternalURL: "https://github.com/acme/ops/issues/42",
		Labels:      map[string]string{"issue_key": "acme/ops#42", "repository": "acme/ops"},
		Fields:      map[string]string{"severity": "minor"},
		Raw:         []byte(`{"action":"opened","issue":{"number":42,"title":"CSV export of the team queue"}}`),
	}
}

// ---- 1. bounds ----

// Identities are refused over their bound, never truncated: two event ids that
// differ past a cut would become one delivery, and a cut external id would dedup
// two vendor objects onto one request.
func TestRequestEnvelopeIdentitiesAreRefusedNotTruncated(t *testing.T) {
	at := envelopeReceivedAt()
	cases := []struct {
		name  string
		mut   func(*RequestEnvelopeInput)
		field string
	}{
		{"external id", func(in *RequestEnvelopeInput) { in.ExternalID = strings.Repeat("g", MaxRequestEnvelopeExternalIDLen+1) }, "external_id"},
		{"event id", func(in *RequestEnvelopeInput) { in.EventID = strings.Repeat("e", MaxRequestEnvelopeEventIDLen+1) }, "event_id"},
		{"missing external id", func(in *RequestEnvelopeInput) { in.ExternalID = "  " }, "external_id"},
		{"missing event id", func(in *RequestEnvelopeInput) { in.EventID = "" }, "event_id"},
		{"external id with a space", func(in *RequestEnvelopeInput) { in.ExternalID = "gh issue 42" }, "external_id"},
		{"event id with a newline", func(in *RequestEnvelopeInput) { in.EventID = "evt\n1" }, "event_id"},
		{"unknown event", func(in *RequestEnvelopeInput) { in.Event = "deleted" }, "event"},
		{"missing event", func(in *RequestEnvelopeInput) { in.Event = "" }, "event"},
		{"vendor event over bound", func(in *RequestEnvelopeInput) { in.VendorEvent = strings.Repeat("v", MaxRequestEnvelopeEventNameLen+1) }, "vendor_event"},
		{"action with a control character", func(in *RequestEnvelopeInput) { in.Action = "opened\x00" }, "action"},
		{"missing occurred_at", func(in *RequestEnvelopeInput) { in.OccurredAt = time.Time{} }, "occurred_at"},
		{"occurred_at in the future", func(in *RequestEnvelopeInput) { in.OccurredAt = at.Add(RequestEnvelopeClockSkew + time.Hour) }, "occurred_at"},
		{"missing subject", func(in *RequestEnvelopeInput) { in.Subject = "   " }, "subject"},
		{"subject over bound", func(in *RequestEnvelopeInput) { in.Subject = strings.Repeat("s", MaxRequestSubjectLen+1) }, "subject"},
		{"body over bound", func(in *RequestEnvelopeInput) { in.Body = strings.Repeat("b", MaxRequestBriefLen+1) }, "body"},
		{"url over bound", func(in *RequestEnvelopeInput) {
			in.ExternalURL = "https://example.test/" + strings.Repeat("u", MaxRequestEnvelopeURLLen)
		}, "external_url"},
		{"url with a space", func(in *RequestEnvelopeInput) { in.ExternalURL = "https://example.test/a b" }, "external_url"},
		{"no raw body", func(in *RequestEnvelopeInput) { in.Raw = nil }, "raw"},
		{"too many labels", func(in *RequestEnvelopeInput) {
			in.Labels = map[string]string{}
			for i := range MaxMetadataKeys + 1 {
				in.Labels["label_"+strconv.Itoa(i)] = "x"
			}
		}, "labels"},
		{"label value over bound", func(in *RequestEnvelopeInput) {
			in.Labels["summary"] = strings.Repeat("l", MaxMetadataValueLen+1)
		}, "labels"},
		{"label key over bound", func(in *RequestEnvelopeInput) {
			in.Labels[strings.Repeat("k", MaxMetadataKeyLen+1)] = "x"
		}, "labels"},
		{"labels and fields together over bound", func(in *RequestEnvelopeInput) {
			in.Labels = map[string]string{}
			in.Fields = map[string]string{}
			for i := range MaxMetadataKeys {
				in.Labels["l_"+strconv.Itoa(i)] = "x"
			}
			in.Fields["one_more"] = "x"
		}, "hints together"},
		{"a key in both labels and fields", func(in *RequestEnvelopeInput) { in.Fields["severity"] = "minor"; in.Labels["severity"] = "minor" }, "both carry"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := envelopeInput()
			c.mut(&in)
			env, err := NewRequestEnvelope(envelopeBinding(), in, at)
			if err == nil {
				t.Fatalf("the envelope was accepted (%q); want a refusal naming %s", env.ExternalID, c.field)
			}
			if !strings.Contains(err.Error(), c.field) {
				t.Errorf("refusal %q does not name %q", err, c.field)
			}
		})
	}
}

// The bound is inclusive, exactly: a value AT the limit is accepted (nothing
// legitimate is refused), and the accepted value is stored unchanged (nothing is
// silently rewritten).
func TestRequestEnvelopeBoundsAreInclusiveAndLossless(t *testing.T) {
	at := envelopeReceivedAt()
	in := envelopeInput()
	in.ExternalID = strings.Repeat("g", MaxRequestEnvelopeExternalIDLen)
	in.EventID = strings.Repeat("e", MaxRequestEnvelopeEventIDLen)
	in.Subject = strings.Repeat("s", MaxRequestSubjectLen)
	in.Body = strings.Repeat("b", MaxRequestBriefLen)
	in.ExternalURL = "https://example.test/" + strings.Repeat("u", MaxRequestEnvelopeURLLen-len("https://example.test/"))
	// One hint short of the ceiling on each map, because labels and fields share the
	// metadata budget: one more label here and the pair would be over it.
	in.Labels = map[string]string{}
	for i := range MaxMetadataKeys - 1 {
		in.Labels["label_"+strconv.Itoa(i)] = strings.Repeat("v", MaxMetadataValueLen)
	}
	env, err := NewRequestEnvelope(envelopeBinding(), in, at)
	if err != nil {
		t.Fatalf("an envelope at every bound was refused: %v", err)
	}
	if env.ExternalID != in.ExternalID || env.EventID != in.EventID {
		t.Error("an identity at its bound was rewritten on the way in")
	}
	if env.Subject != in.Subject || env.Body != in.Body {
		t.Error("text at its bound was rewritten on the way in")
	}
	if got := len(env.Labels()); got != MaxMetadataKeys-1 {
		t.Errorf("labels = %d, want all %d kept", got, MaxMetadataKeys-1)
	}
	if got := len(env.Metadata()); got != MaxMetadataKeys {
		t.Errorf("metadata = %d, want labels + fields (%d)", got, MaxMetadataKeys)
	}
}

// The raw excerpt is the one field that is truncated: it is a sample, so an
// oversized body yields a bounded, single-line excerpt with a visible cut, and no
// control character reaches a log line.
func TestRequestEnvelopeExcerptIsTruncatedNotRefused(t *testing.T) {
	in := envelopeInput()
	in.Raw = []byte("{\"body\":\"" + strings.Repeat("x", MaxRequestEnvelopeExcerptLen*3) + "\"}\n\t\"second\":1")
	env, err := NewRequestEnvelope(envelopeBinding(), in, envelopeReceivedAt())
	if err != nil {
		t.Fatalf("a large raw body was refused; the excerpt is a sample and only it is bounded: %v", err)
	}
	if n := len([]rune(env.RawExcerpt)); n > MaxRequestEnvelopeExcerptLen+1 {
		t.Errorf("raw_excerpt is %d characters, over %d + the cut marker", n, MaxRequestEnvelopeExcerptLen)
	}
	if !strings.HasSuffix(env.RawExcerpt, "…") {
		t.Errorf("a truncated excerpt does not say it was cut: %q", env.RawExcerpt[max(0, len(env.RawExcerpt)-20):])
	}
	if strings.ContainsAny(env.RawExcerpt, "\n\t\r") {
		t.Errorf("raw_excerpt carries a control character: %q", env.RawExcerpt)
	}
	// Invalid UTF-8 is replaced rather than refused: the excerpt is display text,
	// and a vendor body's encoding is not AMP's to police.
	in.Raw = []byte("ok \xff\xfe body")
	env, err = NewRequestEnvelope(envelopeBinding(), in, envelopeReceivedAt())
	if err != nil {
		t.Fatalf("a body with invalid UTF-8 was refused: %v", err)
	}
	if !strings.HasPrefix(env.RawExcerpt, "ok ") || !strings.HasSuffix(env.RawExcerpt, " body") {
		t.Errorf("raw_excerpt = %q, want the readable part of the body kept around the replaced bytes", env.RawExcerpt)
	}
	for _, r := range env.RawExcerpt {
		if unicode.IsControl(r) {
			t.Errorf("raw_excerpt carries a control character %q", r)
		}
	}
}

// An external_url is inert text and only http/https is accepted: a payload must
// not be able to hand a surface a javascript: or data: link.
func TestRequestEnvelopeExternalURLIsInertAndHTTPOnly(t *testing.T) {
	at := envelopeReceivedAt()
	for _, bad := range []string{
		"javascript:alert(1)", "data:text/html,<script>x</script>", "file:///etc/passwd",
		"ftp://example.test/x", "//example.test/x", "example.test/x",
	} {
		in := envelopeInput()
		in.ExternalURL = bad
		if _, err := NewRequestEnvelope(envelopeBinding(), in, at); err == nil {
			t.Errorf("external_url %q accepted; only http(s) is inert enough to store", bad)
		}
	}
	in := envelopeInput()
	in.ExternalURL = "http://jira.internal.test/browse/OPS-412"
	if _, err := NewRequestEnvelope(envelopeBinding(), in, at); err != nil {
		t.Errorf("an http URL was refused: %v", err)
	}
	in.ExternalURL = ""
	if _, err := NewRequestEnvelope(envelopeBinding(), in, at); err != nil {
		t.Errorf("an envelope without a URL was refused: %v", err)
	}
}

// ---- 2. tenant ----

// The tenant comes from the binding and nowhere else. The strongest form of this
// test is structural: the untrusted half has no field that could carry one, so
// there is nothing to check at runtime. The behavioural half follows — the same
// payload bound to two registrations produces two envelopes in two tenants.
func TestRequestEnvelopeTenantIsTheBindingAndOnlyTheBinding(t *testing.T) {
	typ := reflect.TypeOf(RequestEnvelopeInput{})
	for i := range typ.NumField() {
		name := strings.ToLower(typ.Field(i).Name)
		for _, forbidden := range []string{"org", "team", "tenant", "project", "integration", "requester", "owner", "assignee", "state"} {
			if strings.Contains(name, forbidden) {
				t.Errorf("RequestEnvelopeInput.%s could carry a %q decision: the untrusted half must not be able to name a tenant, a destination or who asked", typ.Field(i).Name, forbidden)
			}
		}
	}
	in := envelopeInput()
	mine, err := NewRequestEnvelope(IntegrationBinding{IntegrationID: "github-issues/acme-ops", OrgID: 7, TeamID: 12}, in, envelopeReceivedAt())
	if err != nil {
		t.Fatalf("the envelope was refused: %v", err)
	}
	theirs, err := NewRequestEnvelope(IntegrationBinding{IntegrationID: "github-issues/other-ops", OrgID: 9, TeamID: 30}, in, envelopeReceivedAt())
	if err != nil {
		t.Fatalf("the second envelope was refused: %v", err)
	}
	if mine.OrgID() != 7 || mine.TeamID() != 12 || mine.IntegrationID() != "github-issues/acme-ops" {
		t.Errorf("envelope bound to (7,12) reports org=%d team=%d integration=%q", mine.OrgID(), mine.TeamID(), mine.IntegrationID())
	}
	if theirs.OrgID() != 9 || theirs.TeamID() != 30 {
		t.Errorf("the same payload in another registration reports org=%d team=%d, want 9/30", theirs.OrgID(), theirs.TeamID())
	}
	if mine.Metadata()["team_id"] != nil || mine.Metadata()["org_id"] != nil {
		t.Error("the envelope's metadata names a tenant; only the binding may")
	}
	// A binding without a tenant cannot produce an envelope at all.
	if _, err := NewRequestEnvelope(IntegrationBinding{IntegrationID: "github-issues/acme-ops"}, in, envelopeReceivedAt()); err == nil {
		t.Error("an integration with no org/team produced an envelope")
	}
	// The integration id is an identity: it is required and bounded too.
	if _, err := NewRequestEnvelope(IntegrationBinding{OrgID: 7, TeamID: 12}, in, envelopeReceivedAt()); err == nil {
		t.Error("an envelope with no integration id was accepted")
	}
	if _, err := NewRequestEnvelope(IntegrationBinding{IntegrationID: strings.Repeat("i", MaxRequestEnvelopeIntegrationIDLen+1), OrgID: 7, TeamID: 12}, in, envelopeReceivedAt()); err == nil {
		t.Error("an over-long integration id was accepted")
	}
}

// A payload's own tenancy claim is dropped and counted, never stored and never
// refused: refusing would make an ordinary vendor payload (many name an org, a
// project or a status) undeliverable, and the claim is one AMP will never read.
func TestRequestEnvelopeDropsReservedHints(t *testing.T) {
	in := envelopeInput()
	in.Labels["org_id"] = "42"
	in.Labels["Team"] = "someone-else"
	in.Labels["PROJECT"] = "99"
	in.Labels["assignee"] = "dana@example.com"
	in.Labels["jira_key"] = "OPS-412" // a real label, kept
	in.Fields["source_external_id"] = "spoofed"
	in.Fields["severity"] = "critical"
	env, err := NewRequestEnvelope(envelopeBinding(), in, envelopeReceivedAt())
	if err != nil {
		t.Fatalf("a payload claiming a tenant was refused instead of having the claim dropped: %v", err)
	}
	md := env.Metadata()
	for _, k := range []string{"org_id", "Team", "PROJECT", "assignee", "source_external_id"} {
		if _, ok := md[k]; ok {
			t.Errorf("metadata carries %q: a reserved key must never reach a request's metadata", k)
		}
	}
	if md["jira_key"] != "OPS-412" || md["severity"] != "critical" {
		t.Errorf("ordinary hints were dropped too: %v", md)
	}
	if got := env.ReservedKeysDropped(); got != 5 {
		t.Errorf("ReservedKeysDropped() = %d, want 5 (the five claims above)", got)
	}
	if env.OrgID() != 7 || env.TeamID() != 12 {
		t.Error("a payload's tenant claim moved the envelope's tenant")
	}
	// Every reserved key is refused case-insensitively by the predicate itself.
	for _, k := range RequestEnvelopeReservedKeys {
		for _, variant := range []string{k, strings.ToUpper(k), " " + k + " "} {
			if !IsReservedRequestEnvelopeKey(variant) {
				t.Errorf("IsReservedRequestEnvelopeKey(%q) = false for reserve list entry %q", variant, k)
			}
		}
	}
	if IsReservedRequestEnvelopeKey("severity") {
		t.Error("an ordinary hint is reserved")
	}
}

// The hint maps are copies on the way in and on the way out: neither the caller's
// map nor a returned one can change what an envelope carries.
func TestRequestEnvelopeHintsAreCopied(t *testing.T) {
	in := envelopeInput()
	env, err := NewRequestEnvelope(envelopeBinding(), in, envelopeReceivedAt())
	if err != nil {
		t.Fatalf("the envelope was refused: %v", err)
	}
	in.Labels["late"] = "added after the envelope was built"
	if _, ok := env.Labels()["late"]; ok {
		t.Error("a map mutated after NewRequestEnvelope changed the envelope")
	}
	env.Labels()["mutated"] = "x"
	if _, ok := env.Labels()["mutated"]; ok {
		t.Error("a returned label map writes through to the envelope")
	}
	if _, ok := env.Metadata()["mutated"]; ok {
		t.Error("Metadata() shares state with Labels()")
	}
}

// ---- 3. identity and replay ----

// The two keys are the two replay rules: (integration, event) for a delivery,
// (team, connector, external id) for the object. The object key is 053's unique
// index, so a later caller and the database agree by construction.
func TestRequestEnvelopeKeysAreTheTwoReplayIdentities(t *testing.T) {
	in := envelopeInput()
	env, err := NewRequestEnvelope(envelopeBinding(), in, envelopeReceivedAt())
	if err != nil {
		t.Fatalf("the envelope was refused: %v", err)
	}
	if got, want := env.EventKey(), (RequestEnvelopeEventKey{IntegrationID: "github-issues/acme-ops", EventID: in.EventID}); got != want {
		t.Errorf("EventKey() = %+v, want %+v", got, want)
	}
	if got, want := env.ObjectKey(), (RequestEnvelopeObjectKey{TeamID: 12, SourceKind: RequestSourceConnector, ExternalID: in.ExternalID}); got != want {
		t.Errorf("ObjectKey() = %+v, want %+v", got, want)
	}
	// A second event about the same object shares the object key and differs at
	// the event key: that is exactly how "append, never re-create" is decided.
	second := in
	second.EventID = "another-delivery"
	second.Event = RequestEnvelopeUpdated
	second.Action = "edited"
	other, err := NewRequestEnvelope(envelopeBinding(), second, envelopeReceivedAt())
	if err != nil {
		t.Fatalf("the second event was refused: %v", err)
	}
	if other.ObjectKey() != env.ObjectKey() {
		t.Error("two events about one vendor object produced different object keys")
	}
	if other.EventKey() == env.EventKey() {
		t.Error("two deliveries produced the same event key")
	}
	// The same event delivered to another integration is another delivery.
	foreign, err := NewRequestEnvelope(IntegrationBinding{IntegrationID: "jira/other", OrgID: 7, TeamID: 12}, in, envelopeReceivedAt())
	if err != nil {
		t.Fatalf("the foreign integration's envelope was refused: %v", err)
	}
	if foreign.EventKey() == env.EventKey() {
		t.Error("one event id in two integrations produced one delivery key")
	}
	// A rename is a label, never the identity: the vendor's immutable id is what
	// dedups (D12).
	renamed := in
	renamed.EventID = "third-delivery"
	renamed.Labels = map[string]string{"issue_key": "OPS-999-renamed"}
	renamed.ExternalURL = "https://example.test/browse/OPS-999-renamed"
	renamedEnv, err := NewRequestEnvelope(envelopeBinding(), renamed, envelopeReceivedAt())
	if err != nil {
		t.Fatalf("the renamed event was refused: %v", err)
	}
	if renamedEnv.ObjectKey() != env.ObjectKey() {
		t.Error("a renamed vendor key changed the object identity: the immutable id dedups, the key is a label")
	}
}

// payload_hash is stable for identical bytes and different for different ones, so
// an operator can prove which delivery produced a request without AMP keeping the
// body.
func TestRequestEnvelopePayloadHashIsStableAndRawIsNotKept(t *testing.T) {
	at := envelopeReceivedAt()
	mk := func(raw []byte) *RequestEnvelope {
		t.Helper()
		in := envelopeInput()
		in.Raw = raw
		env, err := NewRequestEnvelope(envelopeBinding(), in, at)
		if err != nil {
			t.Fatalf("the envelope was refused: %v", err)
		}
		return env
	}
	a, b := mk([]byte(`{"n":1}`)), mk([]byte(`{"n":1}`))
	if a.PayloadHash != b.PayloadHash {
		t.Errorf("identical bytes produced different hashes: %q vs %q", a.PayloadHash, b.PayloadHash)
	}
	if c := mk([]byte(`{"n":2}`)); c.PayloadHash == a.PayloadHash {
		t.Error("different bytes produced the same hash")
	}
	// A known sha256, so the field is the documented digest and not something
	// that merely looks like one.
	const want = "5feceb66ffc86f38d952786c6d696c79c2dbc239dd4e91b46729d73a27fb57e9" // sha256("0")
	if env := mk([]byte("0")); env.PayloadHash != want {
		t.Errorf("payload_hash = %q, want sha256 of the raw bytes %q", env.PayloadHash, want)
	}
	if len(a.PayloadHash) != 64 {
		t.Errorf("payload_hash is %d characters, want lowercase hex sha256 (64)", len(a.PayloadHash))
	}
}

// The envelope is immutable in the way that matters: the tenant and the
// integration id are reachable only through accessors, so no caller can move a
// built envelope into another tenant by assigning a field.
func TestRequestEnvelopeTenantIsNotAssignable(t *testing.T) {
	typ := reflect.TypeOf(RequestEnvelope{})
	for i := range typ.NumField() {
		f := typ.Field(i)
		if !f.IsExported() {
			continue
		}
		lower := strings.ToLower(f.Name)
		if strings.Contains(lower, "org") || strings.Contains(lower, "team") || strings.Contains(lower, "tenant") || strings.Contains(lower, "integration") {
			t.Errorf("RequestEnvelope.%s is exported: the trusted half must be reachable only through its accessors", f.Name)
		}
	}
}

// received_at is AMP's clock and occurred_at is the vendor's: they are kept
// apart, and both are normalized to UTC so a fixture compares exactly.
func TestRequestEnvelopeKeepsTheTwoClocksApart(t *testing.T) {
	received := time.Date(2026, 11, 13, 9, 30, 0, 0, time.UTC)
	occurred := received.Add(-17 * time.Hour) // a polled source: the alarm fired long before we asked
	loc := time.FixedZone("vendor", -5*60*60)
	in := envelopeInput()
	in.OccurredAt = occurred.In(loc)
	env, err := NewRequestEnvelope(envelopeBinding(), in, received.In(loc))
	if err != nil {
		t.Fatalf("the envelope was refused: %v", err)
	}
	if !env.OccurredAt.Equal(occurred) {
		t.Errorf("occurred_at = %v, want %v", env.OccurredAt, occurred)
	}
	if !env.ReceivedAt.Equal(received) {
		t.Errorf("received_at = %v, want %v", env.ReceivedAt, received)
	}
	if env.ReceivedAt.Location() != time.UTC || env.OccurredAt.Location() != time.UTC {
		t.Error("the clocks are not normalized to UTC")
	}
	if !env.ReceivedAt.After(env.OccurredAt) {
		t.Error("a polled delivery must be able to arrive well after the event happened")
	}
}

// The five verbs are the closed vocabulary, and nothing else is one — a vendor's
// own string belongs in vendor_event, or intake would have to branch on a value
// no test covers.
func TestRequestEnvelopeEventVocabularyIsClosed(t *testing.T) {
	want := []string{"created", "updated", "commented", "closed", "alert"}
	got := make([]string, len(RequestEnvelopeEvents))
	for i, e := range RequestEnvelopeEvents {
		got[i] = string(e)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("RequestEnvelopeEvents = %v, want %v", got, want)
	}
	for _, w := range want {
		if e, err := NormalizeRequestEnvelopeEvent(" " + strings.ToUpper(w) + " "); err != nil || string(e) != w {
			t.Errorf("NormalizeRequestEnvelopeEvent(%q) = %q, %v", w, e, err)
		}
	}
	for _, bad := range []string{"", "deleted", "opened", "issue.opened", "severity_changed"} {
		if e, err := NormalizeRequestEnvelopeEvent(bad); err == nil {
			t.Errorf("NormalizeRequestEnvelopeEvent(%q) = %q, want a refusal naming the five verbs", bad, e)
		}
	}
}

// ---- the phase-0 boundary, as a guard ----

// The envelope file adds no network, transport or storage dependency. This is the
// phase-0 boundary kept structural: no signed endpoint, no vendor adapter, no
// credential and no outbound request can appear in this file without the import
// list changing, which this test refuses.
func TestRequestEnvelopeHasNoNetworkOrStorageImports(t *testing.T) {
	allowed := map[string]bool{
		"crypto/sha256": true, "encoding/hex": true, "errors": true, "fmt": true,
		"slices": true, "strings": true, "time": true, "unicode": true, "unicode/utf8": true,
	}
	files, err := filepath.Glob("request_envelope*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no request_envelope*.go file found")
	}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		f, parseErr := parser.ParseFile(token.NewFileSet(), file, nil, parser.ImportsOnly)
		if parseErr != nil {
			t.Fatalf("parse %s: %v", file, parseErr)
		}
		for _, spec := range f.Imports {
			path, unquoteErr := strconv.Unquote(spec.Path.Value)
			if unquoteErr != nil {
				t.Fatalf("%s: import path %s: %v", file, spec.Path.Value, unquoteErr)
			}
			if !allowed[path] {
				t.Errorf("%s imports %q: phase 0 is a contract with no endpoint, no vendor adapter, no credential and no outbound call, and the import list is where that boundary is enforced", file, path)
			}
		}
	}
}
