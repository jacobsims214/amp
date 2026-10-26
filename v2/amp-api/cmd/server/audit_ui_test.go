package main

// Audit log v2, reading and streaming it out (#2431, KB
// decisions/audit-log-v2.md section 4): the filters, the streamed CSV and
// NDJSON export (which records audit.export), instance webhooks for the
// account-level rows no org owns, and the optional auth header on
// deliveries.

import (
	"bufio"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/simstech/amp-api/internal/api"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/service"
	"github.com/simstech/amp-api/internal/webhook"
)

// seedAudit writes an org's entries with the v2 fields set, straight
// through the repository.
func seedAudit(t *testing.T, w *rbacWorld, e domain.AuditEntry) *domain.AuditEntry {
	t.Helper()
	if e.OrgID == nil {
		e.OrgID = &w.org.ID
	}
	if e.Summary == "" {
		e.Summary = e.Action
	}
	got, err := w.s.repo.InsertAudit(context.Background(), e)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestAuditUI_Filters(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	tag := fmt.Sprintf("ui%d", time.Now().UnixNano())
	act := tag + ".thing"
	seedAudit(t, w, domain.AuditEntry{Action: act, ActorEmail: "Pat%_x@corp.test", ActorType: domain.AuditActorHuman,
		SourceIP: "203.0.113.7", Client: domain.AuditClientWeb, Outcome: domain.AuditOutcomeSuccess, TargetType: "project", TargetID: "7"})
	seedAudit(t, w, domain.AuditEntry{Action: act, ActorEmail: "bot@corp.test", ActorType: domain.AuditActorAgent,
		SourceIP: "198.51.100.9", Client: domain.AuditClientMCP, Outcome: domain.AuditOutcomeDenied, Reason: "nope", TargetType: "project", TargetID: "8"})
	seedAudit(t, w, domain.AuditEntry{Action: act, ActorEmail: "patx@corp.test", ActorType: domain.AuditActorHuman,
		SourceIP: "2001:db8::5", Client: domain.AuditClientAPI, Outcome: domain.AuditOutcomeFailed, TargetType: "user", TargetID: "7"})

	list := func(q string) []map[string]any {
		t.Helper()
		out := ra.want(t, 200, "orgadmin", "GET", fmt.Sprintf("/api/audit?org_id=%d&action=%s&%s", w.org.ID, act, q), nil)
		var rows []map[string]any
		for _, e := range out["entries"].([]any) {
			rows = append(rows, e.(map[string]any))
		}
		return rows
	}
	for _, c := range []struct {
		q    string
		want []string // actor emails, newest first
	}{
		{"", []string{"patx@corp.test", "bot@corp.test", "Pat%_x@corp.test"}},
		{"ip=203.0.113.0/24", []string{"Pat%_x@corp.test"}},
		{"ip=198.51.100.9", []string{"bot@corp.test"}},
		{"ip=2001:db8::/32", []string{"patx@corp.test"}},
		{"outcome=denied", []string{"bot@corp.test"}},
		{"client=api", []string{"patx@corp.test"}},
		{"actor_type=agent", []string{"bot@corp.test"}},
		{"actor=PAT", []string{"patx@corp.test", "Pat%_x@corp.test"}},
		{"actor=" + url.QueryEscape("%_"), []string{"Pat%_x@corp.test"}}, // LIKE wildcards are literal
		{"target_type=project", []string{"bot@corp.test", "Pat%_x@corp.test"}},
		{"target_type=project&target_id=7", []string{"Pat%_x@corp.test"}},
		{"to=2000-01-01", nil},
		{"from=" + time.Now().Add(-time.Hour).UTC().Format(time.RFC3339), []string{"patx@corp.test", "bot@corp.test", "Pat%_x@corp.test"}},
	} {
		got := list(c.q)
		var emails []string
		for _, e := range got {
			emails = append(emails, e["actor_email"].(string))
		}
		if strings.Join(emails, ",") != strings.Join(c.want, ",") {
			t.Errorf("filter %q = %v, want %v", c.q, emails, c.want)
		}
	}
	// The v2 fields come back.
	if e := list("outcome=denied")[0]; e["source_ip"] != "198.51.100.9" || e["reason"] != "nope" || e["client"] != "mcp" ||
		e["actor_type"] != "agent" || e["chain_key"] != fmt.Sprintf("org:%d", w.org.ID) || e["hash"] == nil {
		t.Errorf("entry = %v", e)
	}
	// A bad value is a 400, never a wider result.
	for _, q := range []string{"ip=not-an-ip", "outcome=maybe", "client=fax", "actor_type=robot", "from=yesterday",
		"from=2026-02-02&to=2026-01-01", "limit=x"} {
		ra.want(t, 400, "orgadmin", "GET", fmt.Sprintf("/api/audit?org_id=%d&%s", w.org.ID, q), nil)
	}
}

// exportAs downloads an export as who.
func exportAs(t *testing.T, ra *restAs, who, query string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", ra.server.URL+"/api/audit/export?"+query, nil)
	req.Header.Set("X-Test-As", who)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func TestAuditUI_Export(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	tag := fmt.Sprintf("ex%d", time.Now().UnixNano())
	act := tag + ".row"
	// More than a page, so the export pages by id past the old 500 cap.
	n := service.AuditExportPage + 37
	for i := 0; i < n; i++ {
		e := domain.AuditEntry{Action: act, ActorEmail: "x@corp.test", Summary: fmt.Sprintf("row %d", i)}
		if i == 0 {
			// Formula-looking cells are defused in CSV.
			e.UserAgent, e.Summary, e.SourceIP = "=HYPERLINK(\"http://evil\")", "@SUM(A1)", "192.0.2.1"
			e.Before, e.After = map[string]any{"role": "member"}, map[string]any{"role": "admin"}
		}
		seedAudit(t, w, e)
	}
	q := fmt.Sprintf("org_id=%d&action=%s", w.org.ID, act)

	resp, body := exportAs(t, ra, "orgadmin", q+"&format=csv")
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/csv") ||
		!strings.Contains(resp.Header.Get("Content-Disposition"), fmt.Sprintf(`attachment; filename="amp-audit-org-%d-`, w.org.ID)) ||
		resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("csv export: %d %v", resp.StatusCode, resp.Header)
	}
	records, err := csv.NewReader(strings.NewReader(body)).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != n+1 || strings.Join(records[0], ",") != strings.Join(api.AuditCSVColumns, ",") {
		t.Fatalf("csv rows = %d (want %d), header %v", len(records), n+1, records[0])
	}
	col := map[string]int{}
	for i, c := range records[0] {
		col[c] = i
	}
	oldest := records[len(records)-1] // newest first
	if oldest[col["user_agent"]] != `'=HYPERLINK("http://evil")` || oldest[col["summary"]] != "'@SUM(A1)" ||
		oldest[col["source_ip"]] != "192.0.2.1" || oldest[col["before"]] != `{"role":"member"}` || oldest[col["after"]] != `{"role":"admin"}` ||
		oldest[col["chain_key"]] != fmt.Sprintf("org:%d", w.org.ID) || len(oldest[col["hash"]]) != 64 {
		t.Errorf("oldest csv row = %v", oldest)
	}
	if !strings.HasSuffix(oldest[col["at"]], "Z") {
		t.Errorf("at is not UTC: %s", oldest[col["at"]])
	}

	resp, body = exportAs(t, ra, "orgadmin", q+"&format=ndjson")
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/x-ndjson") {
		t.Fatalf("ndjson export: %d %v", resp.StatusCode, resp.Header)
	}
	sc := bufio.NewScanner(strings.NewReader(body))
	lines := 0
	var last domain.AuditEntry
	for sc.Scan() {
		if err := json.Unmarshal(sc.Bytes(), &last); err != nil {
			t.Fatalf("line %d: %v", lines, err)
		}
		lines++
	}
	if lines != n || last.UserAgent != `=HYPERLINK("http://evil")` || last.After["role"] != "admin" {
		t.Fatalf("ndjson lines = %d (want %d), last = %+v", lines, n, last)
	}

	// Each export is itself audited, with its filter and row count.
	exports := auditRows(t, w.s.repo, domain.AuditFilter{OrgID: w.org.ID, Action: "audit.export", Limit: 2})
	if len(exports) != 2 || exports[0].Details["format"] != "ndjson" || exports[1].Details["format"] != "csv" ||
		exports[0].Details["rows"] != float64(n) || exports[0].ActorEmail != w.orgadmin.Email {
		t.Fatalf("audit.export rows = %+v", exports)
	}
	if f, _ := exports[0].Details["filter"].(map[string]any); f["action"] != act || f["org_id"] != float64(w.org.ID) {
		t.Errorf("recorded filter = %v", exports[0].Details["filter"])
	}

	// Same access as /api/audit: a member gets a JSON 403 and nothing is
	// exported or recorded as an export.
	resp, body = exportAs(t, ra, "dave", q)
	if resp.StatusCode != 403 || !strings.Contains(body, `"error"`) {
		t.Fatalf("member export: %d %s", resp.StatusCode, body)
	}
	resp, _ = exportAs(t, ra, "orgadmin", "format=csv") // the whole log: global admins only
	if resp.StatusCode != 403 {
		t.Fatalf("org admin whole-log export: %d", resp.StatusCode)
	}
	if resp, _ = exportAs(t, ra, "root", "format=csv&action="+act); resp.StatusCode != 200 {
		t.Fatalf("global admin export: %d", resp.StatusCode)
	}
	if resp, _ = exportAs(t, ra, "orgadmin", q+"&format=xml"); resp.StatusCode != 400 {
		t.Fatalf("bad format: %d", resp.StatusCode)
	}
	if got := auditRows(t, w.s.repo, domain.AuditFilter{OrgID: w.org.ID, Action: "audit.export"}); len(got) != 2 {
		t.Errorf("refused exports were recorded as exports: %d rows", len(got))
	}
	// An empty export is a header only.
	resp, body = exportAs(t, ra, "orgadmin", q+"&outcome=failed&format=csv")
	if resp.StatusCode != 200 || strings.Count(body, "\n") != 1 {
		t.Errorf("empty export: %d %q", resp.StatusCode, body)
	}
}

// headerReceiver records deliveries with the auth header they carried.
type headerReceiver struct {
	srv *httptest.Server
	mu  sync.Mutex
	got []headerDelivery
}

type headerDelivery struct {
	event, auth, signature, body string
}

func newHeaderReceiver(t *testing.T, header string) *headerReceiver {
	t.Helper()
	rc := &headerReceiver{}
	rc.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		rc.mu.Lock()
		rc.got = append(rc.got, headerDelivery{r.Header.Get("X-AMP-Event"), r.Header.Get(header), r.Header.Get("X-AMP-Signature"), string(b)})
		rc.mu.Unlock()
	}))
	t.Cleanup(rc.srv.Close)
	return rc
}

func (rc *headerReceiver) find(t *testing.T, what string, match func(headerDelivery) bool) headerDelivery {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		rc.mu.Lock()
		for _, d := range rc.got {
			if match(d) {
				rc.mu.Unlock()
				return d
			}
		}
		rc.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatalf("no delivery of %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (rc *headerReceiver) has(match func(headerDelivery) bool) bool {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	for _, d := range rc.got {
		if match(d) {
			return true
		}
	}
	return false
}

func TestAuditUI_InstanceWebhookAndAuthHeader(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	allowPrivateWebhooks(t)
	siem := newHeaderReceiver(t, "Authorization")
	orgRC := newHeaderReceiver(t, "Authorization")
	const token = "Splunk 0f8b-test-hec-token"

	// Only global admins manage instance webhooks.
	body := map[string]any{"name": "SIEM", "url": siem.srv.URL + "/services/collector", "auth_header": "authorization: " + token}
	ra.want(t, 403, "orgadmin", "POST", "/api/webhooks", body)
	ra.want(t, 403, "orgadmin", "GET", "/api/webhooks", nil)
	// They carry audit entries only.
	ra.want(t, 400, "root", "POST", "/api/webhooks", map[string]any{"url": siem.srv.URL, "events": []string{"task.created"}})
	// Header rules: not one AMP or HTTP sets, no line breaks, "Name: value".
	for _, bad := range []string{"X-AMP-Signature: x", "Content-Type: text/plain", "Host: evil", "Authorization", "Bad Name: x",
		"Authorization: a\r\nX-Evil: 1"} {
		ra.want(t, 400, "root", "POST", "/api/webhooks", map[string]any{"url": siem.srv.URL, "auth_header": bad})
	}
	hook := ra.want(t, 200, "root", "POST", "/api/webhooks", body)["webhook"].(map[string]any)
	id := int(hook["id"].(float64))
	t.Cleanup(func() { _ = w.s.repo.DeleteWebhook(context.Background(), id) })
	// Instance webhooks get the request context by default (#2439 M2).
	if hook["instance"] != true || hook["include_context"] != true || hook["auth_header_name"] != "Authorization" || hook["auth_header_set"] != true ||
		fmt.Sprint(hook["events"]) != "[audit.*]" || strings.Contains(mustJSON(hook), token) {
		t.Fatalf("created = %v", hook)
	}
	// Reads never show the value.
	listed := ra.want(t, 200, "root", "GET", "/api/webhooks", nil)
	if strings.Contains(mustJSON(listed), token) || !strings.Contains(mustJSON(listed), `"auth_header_set":true`) {
		t.Fatalf("listed = %v", listed)
	}
	// The audit row of the change records the header's name only.
	created := auditRows(t, w.s.repo, domain.AuditFilter{Action: "webhook.create", TargetType: "webhook", TargetID: fmt.Sprint(id)})
	if len(created) != 1 || strings.Contains(mustJSON(created[0]), token) || created[0].After["delivery_header"] != "Authorization" {
		t.Fatalf("webhook.create row = %+v", created)
	}
	// Moving the webhook needs the header again, so an edit can't carry a
	// credential someone else entered to a new address.
	ra.want(t, 400, "root", "PATCH", fmt.Sprintf("/api/webhooks/%d", id), map[string]any{"url": siem.srv.URL + "/elsewhere"})

	// An org webhook on audit.* too, to show it doesn't get account rows.
	ra.want(t, 200, "orgadmin", "POST", fmt.Sprintf("/api/orgs/%d/webhooks", w.org.ID),
		map[string]any{"url": orgRC.srv.URL, "events": []string{"audit.*"}})

	startWebhooks(t, w, webhook.Options{AllowPrivateNetworks: true})
	// An account-level entry (no org): only the instance webhook gets it,
	// with the auth header and a valid signature.
	tokenName := fmt.Sprintf("siem-%d", time.Now().UnixNano())
	ra.want(t, 200, "alice", "POST", "/api/users/me/tokens", map[string]any{"name": tokenName})
	d := siem.find(t, "token.create", func(d headerDelivery) bool {
		return d.event == "audit.token.create" && strings.Contains(d.body, tokenName)
	})
	if d.auth != token {
		t.Errorf("auth header on delivery = %q", d.auth)
	}
	sig := strings.SplitN(d.signature, ",", 2)
	if len(sig) != 2 || sig[1] != "v1="+webhook.Sign(hook["secret"].(string), strings.TrimPrefix(sig[0], "t="), d.body) {
		t.Errorf("signature %q does not verify", d.signature)
	}
	var env struct {
		ID   string            `json:"id"`
		Data domain.AuditEntry `json:"data"`
	}
	if err := json.Unmarshal([]byte(d.body), &env); err != nil || env.Data.ChainKey != "global" || env.Data.SchemaVersion != 2 ||
		!strings.HasPrefix(env.ID, "evt_audit_") {
		t.Errorf("envelope = %s (%v)", d.body, err)
	}
	// An org entry reaches both.
	team := fmt.Sprintf("SIEM team %d", time.Now().UnixNano())
	ra.want(t, 201, "orgadmin", "POST", fmt.Sprintf("/api/orgs/%d/teams", w.org.ID), map[string]any{"name": team})
	isTeam := func(d headerDelivery) bool { return d.event == "audit.team.create" && strings.Contains(d.body, team) }
	siem.find(t, "team.create (instance)", isTeam)
	if o := orgRC.find(t, "team.create (org)", isTeam); o.auth != "" {
		t.Errorf("org webhook got an auth header it doesn't have: %q", o.auth)
	}
	if orgRC.has(func(d headerDelivery) bool { return strings.Contains(d.body, tokenName) }) {
		t.Error("an org webhook received an account-level entry")
	}

	// Removing the header stops sending it.
	upd := ra.want(t, 200, "root", "PATCH", fmt.Sprintf("/api/webhooks/%d", id), map[string]any{"auth_header": ""})["webhook"].(map[string]any)
	if upd["auth_header_set"] != false {
		t.Fatalf("after removal = %v", upd)
	}
}

// An auth header is a credential: it is only sent over https (unless
// private networks are allowed, as in local development), and webhook
// addresses keep refusing internal ranges.
func TestAuditUI_AuthHeaderNeedsHTTPS(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	ra.want(t, 400, "root", "POST", "/api/webhooks", map[string]any{"url": "http://siem.example.com/in", "auth_header": "DD-API-KEY: k"})
	hook := ra.want(t, 200, "root", "POST", "/api/webhooks", map[string]any{"url": "https://siem.example.com/in", "auth_header": "dd-api-key: k"})["webhook"].(map[string]any)
	id := int(hook["id"].(float64))
	t.Cleanup(func() { _ = w.s.repo.DeleteWebhook(context.Background(), id) })
	if hook["auth_header_name"] != "Dd-Api-Key" {
		t.Errorf("header name = %v", hook["auth_header_name"])
	}
	for _, u := range []string{"https://10.0.0.5/x", "https://[64:ff9b::a00:5]/x", "https://100.64.1.1/", "https://localhost/x"} {
		ra.want(t, 400, "root", "POST", "/api/webhooks", map[string]any{"url": u})
	}
}

// amp_audit_log returns the v2 fields and takes the same filters as the
// REST list; the original arguments and the {entries, count} shape keep
// working.
func TestAuditUI_MCPAuditLog(t *testing.T) {
	w := newRBACWorld(t)
	act := fmt.Sprintf("mcp%d.thing", time.Now().UnixNano())
	seedAudit(t, w, domain.AuditEntry{Action: act, ActorEmail: "a@corp.test", SourceIP: "203.0.113.50", Client: domain.AuditClientWeb,
		Outcome: domain.AuditOutcomeDenied, Reason: "not yours", UserAgent: "Mozilla/5.0", Before: map[string]any{"x": 1.0}})
	seedAudit(t, w, domain.AuditEntry{Action: act, ActorEmail: "b@corp.test", SourceIP: "198.51.100.50", Client: domain.AuditClientMCP})

	as := w.as(w.orgadmin)
	got := as.call(t, "amp_audit_log", map[string]any{"org_id": float64(w.org.ID), "action": act})
	if num(t, got, "count") != 2 {
		t.Fatalf("unfiltered = %v", got)
	}
	got = as.call(t, "amp_audit_log", map[string]any{"org_id": float64(w.org.ID), "action": act, "ip": "203.0.113.0/24", "outcome": "denied"})
	entries, _ := got["entries"].([]any)
	if len(entries) != 1 {
		t.Fatalf("filtered = %v", got)
	}
	e := entries[0].(map[string]any)
	for k, want := range map[string]any{"source_ip": "203.0.113.50", "client": "web", "outcome": "denied", "reason": "not yours",
		"user_agent": "Mozilla/5.0", "schema_version": float64(2), "chain_key": fmt.Sprintf("org:%d", w.org.ID)} {
		if e[k] != want {
			t.Errorf("%s = %v, want %v", k, e[k], want)
		}
	}
	if b, _ := e["before"].(map[string]any); b["x"] != 1.0 {
		t.Errorf("before = %v", e["before"])
	}
	if msg := mcpCall(t, as, as.callCtx(), "amp_audit_log", map[string]any{"org_id": float64(w.org.ID), "ip": "nope"}); !strings.Contains(msg, "ip must be") {
		t.Errorf("bad ip: %q", msg)
	}
}

// The SIEM stream never turns itself off (#2439 M4): an instance webhook
// that keeps failing stays on and backs off, and the deliveries it drops
// are recorded as webhook.delivery.dropped.
func TestAuditUI_InstanceWebhookIsNeverAutoDisabled(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	allowPrivateWebhooks(t)
	down := newReceiver(t)
	down.respond = func(int) int { return 503 }
	hook := ra.want(t, 200, "root", "POST", "/api/webhooks", map[string]any{"name": "dead SIEM", "url": down.srv.URL})["webhook"].(map[string]any)
	id := int(hook["id"].(float64))
	t.Cleanup(func() { _ = w.s.repo.DeleteWebhook(context.Background(), id) })
	startWebhooks(t, w, webhook.Options{AllowPrivateNetworks: true, MaxAttempts: 1, DisableAfter: 1})
	for i := 0; i < 2; i++ {
		ra.want(t, 200, "alice", "POST", "/api/users/me/tokens", map[string]any{"name": fmt.Sprintf("dead-siem-%d-%d", i, time.Now().UnixNano())})
	}
	eventually(t, "two deliveries failed for good", func() bool {
		got, _ := w.s.repo.GetWebhook(context.Background(), id)
		return got != nil && got.ConsecutiveFailures >= 2
	})
	got, _ := w.s.repo.GetWebhook(context.Background(), id)
	if !got.Active {
		t.Fatalf("instance webhook turned off after %d failures", got.ConsecutiveFailures)
	}
	if off := auditRows(t, w.s.repo, domain.AuditFilter{Action: "webhook.disable", TargetType: "webhook", TargetID: fmt.Sprint(id)}); len(off) != 0 {
		t.Fatalf("webhook.disable for an instance webhook: %+v", off)
	}
	var dropped []domain.AuditEntry
	eventually(t, "a webhook.delivery.dropped entry", func() bool {
		dropped, _ = w.s.repo.ListAudit(context.Background(), domain.AuditFilter{Action: "webhook.delivery.dropped", TargetType: "webhook", TargetID: fmt.Sprint(id)})
		return len(dropped) > 0
	})
	if dropped[0].ActorType != domain.AuditActorSystem || dropped[0].OrgID != nil {
		t.Fatalf("dropped entry = %+v", dropped[0])
	}
}
