package main

// Webhooks: who manages them, what they receive, signatures, retries,
// auto-disable, and the private-network guard.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/simstech/amp-api/internal/service"
	"github.com/simstech/amp-api/internal/webhook"
)

// receiver records what webhooks POST to it; respond decides the status.
type receiver struct {
	srv     *httptest.Server
	mu      sync.Mutex
	got     []received
	respond func(n int) int // n = requests so far, 1-based
}

type received struct {
	event, delivery, signature, body string
}

func newReceiver(t *testing.T) *receiver {
	t.Helper()
	rc := &receiver{respond: func(int) int { return 200 }}
	rc.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		rc.mu.Lock()
		rc.got = append(rc.got, received{r.Header.Get("X-AMP-Event"), r.Header.Get("X-AMP-Delivery"), r.Header.Get("X-AMP-Signature"), string(b)})
		n := len(rc.got)
		rc.mu.Unlock()
		w.WriteHeader(rc.respond(n))
	}))
	t.Cleanup(rc.srv.Close)
	return rc
}

// wait returns the requests received once cond holds, or fails.
func (rc *receiver) wait(t *testing.T, what string, cond func([]received) bool) []received {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		rc.mu.Lock()
		got := append([]received(nil), rc.got...)
		rc.mu.Unlock()
		if cond(got) {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("waiting for %s; got %d requests: %+v", what, len(got), got)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func events(got []received) []string {
	var out []string
	for _, r := range got {
		out = append(out, r.event)
	}
	return out
}

// startWebhooks runs a dispatcher for the world's hub and the audit log,
// with fast retries.
func startWebhooks(t *testing.T, w *rbacWorld, opts webhook.Options) {
	t.Helper()
	if opts.Poll == 0 {
		opts.Poll = 20 * time.Millisecond
	}
	if opts.Backoff == nil {
		opts.Backoff = []time.Duration{50 * time.Millisecond}
	}
	d := webhook.New(w.s.repo, opts)
	w.s.hub.AddSink(d.Event)
	t.Cleanup(service.OnAudit(d.Audit))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go d.Run(ctx)
}

func allowPrivateWebhooks(t *testing.T) {
	prev := service.AllowPrivateWebhooks
	service.AllowPrivateWebhooks = true
	t.Cleanup(func() { service.AllowPrivateWebhooks = prev })
}

func TestWebhooks_ProjectEventsAreSignedAndFiltered(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	allowPrivateWebhooks(t)
	startWebhooks(t, w, webhook.Options{AllowPrivateNetworks: true})
	rc := newReceiver(t)

	hooks := fmt.Sprintf("/api/projects/%d/webhooks", w.s.projectID)
	body := map[string]any{"name": "CI", "url": rc.srv.URL + "/hook", "events": []string{"task.created", "task.completed"}}
	ra.want(t, 403, "alice", "POST", hooks, body) // members don't manage webhooks
	ra.want(t, 400, "owner", "POST", hooks, map[string]any{"url": rc.srv.URL, "events": []string{"task.exploded"}})
	ra.want(t, 400, "owner", "POST", hooks, map[string]any{"url": "ftp://example.com"})
	hook := ra.want(t, 200, "owner", "POST", hooks, body)["webhook"].(map[string]any)
	secret, _ := hook["secret"].(string)
	if !strings.HasPrefix(secret, "whsec_") {
		t.Fatalf("created webhook = %v", hook)
	}
	// The secret isn't listed.
	for _, h := range ra.want(t, 200, "owner", "GET", hooks, nil)["webhooks"].([]any) {
		if _, ok := h.(map[string]any)["secret"]; ok {
			t.Error("listed webhook includes its secret")
		}
	}

	owner := w.as(w.owner)
	id := taskID(t, owner.call(t, "amp_create_task", map[string]any{
		"project_id": float64(w.s.projectID), "story_id": float64(w.s.storyID), "name": "Ship <it>", "description": "d", "acceptance_criteria": "a",
	}))
	owner.call(t, "amp_dispatch_task", map[string]any{"task_id": float64(id), "agent_id": "worker"})
	owner.finish(t, id)

	got := rc.wait(t, "created and completed", func(g []received) bool { return len(g) >= 2 })
	time.Sleep(100 * time.Millisecond) // anything else would have arrived by now
	got = rc.wait(t, "", func([]received) bool { return true })
	if ev := events(got); len(ev) != 2 || ev[0] != "task.created" || ev[1] != "task.completed" {
		t.Fatalf("events = %v (dispatch is filtered out)", ev)
	}
	for _, r := range got {
		// Checked the way a receiver would, without AMP's code.
		ts, sig, _ := strings.Cut(strings.TrimPrefix(r.signature, "t="), ",v1=")
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write([]byte(ts + "." + r.body))
		if sig != hex.EncodeToString(mac.Sum(nil)) {
			t.Errorf("bad signature %q on %s", r.signature, r.event)
		}
		if sent, err := strconv.ParseInt(ts, 10, 64); err != nil || time.Since(time.Unix(sent, 0)) > time.Minute {
			t.Errorf("signature timestamp %q", ts)
		}
		var env webhook.Envelope
		if err := json.Unmarshal([]byte(r.body), &env); err != nil {
			t.Fatal(err)
		}
		data := env.Data.(map[string]any)
		if env.Type != r.event || env.ProjectID != w.s.projectID || env.OrgID != w.org.ID || int(data["id"].(float64)) != id || !strings.HasPrefix(env.ID, "evt_") {
			t.Errorf("envelope = %+v", env)
		}
	}

	hookID := int(hook["id"].(float64))
	deliveries := ra.want(t, 200, "owner", "GET", fmt.Sprintf("/api/webhooks/%d/deliveries", hookID), nil)["deliveries"].([]any)
	if len(deliveries) != 2 || deliveries[0].(map[string]any)["status"] != "delivered" {
		t.Errorf("deliveries = %v", deliveries)
	}
	ra.want(t, 403, "alice", "GET", fmt.Sprintf("/api/webhooks/%d/deliveries", hookID), nil)

	// Rotating the secret changes the signature; turning it off stops it.
	newSecret := ra.want(t, 200, "owner", "POST", fmt.Sprintf("/api/webhooks/%d/rotate-secret", hookID), nil)["webhook"].(map[string]any)["secret"].(string)
	ra.want(t, 200, "owner", "POST", fmt.Sprintf("/api/webhooks/%d/test", hookID), nil)
	got = rc.wait(t, "ping", func(g []received) bool { return len(g) >= 3 })
	ping := got[2]
	ts, sig, _ := strings.Cut(strings.TrimPrefix(ping.signature, "t="), ",v1=")
	if ping.event != "ping" || sig != webhook.Sign(newSecret, ts, ping.body) {
		t.Errorf("ping = %+v", ping)
	}
	ra.want(t, 200, "owner", "PATCH", fmt.Sprintf("/api/webhooks/%d", hookID), map[string]any{"active": false})
	owner.call(t, "amp_create_task", map[string]any{
		"project_id": float64(w.s.projectID), "story_id": float64(w.s.storyID), "name": "quiet", "description": "d", "acceptance_criteria": "a",
	})
	// Deliveries already queued wait while it's off.
	if _, err := w.s.repo.EnqueueDelivery(context.Background(), hookID, "task.created", `{}`); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	if n := len(rc.wait(t, "", func([]received) bool { return true })); n != 3 {
		t.Errorf("a turned-off webhook received %d requests, want 3", n)
	}
	// Changes to webhooks are audited.
	if acts := auditActions(t, ra, "owner", fmt.Sprintf("project_id=%d&action=webhook.", w.s.projectID)); len(acts) != 3 {
		t.Errorf("webhook audit = %v", acts)
	}
}

// Org webhooks see every project in the org and, opted in, its audit
// entries; the Slack format sends {"text": ...}.
func TestWebhooks_OrgWebhookSlackAndAudit(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	allowPrivateWebhooks(t)
	startWebhooks(t, w, webhook.Options{AllowPrivateNetworks: true})
	rc := newReceiver(t)

	hooks := fmt.Sprintf("/api/orgs/%d/webhooks", w.org.ID)
	ra.want(t, 403, "lead", "POST", hooks, map[string]any{"url": rc.srv.URL})
	ra.want(t, 200, "orgadmin", "POST", hooks, map[string]any{"url": rc.srv.URL, "format": "slack", "events": []string{"audit.*", "task.completed"}})

	ra.want(t, 201, "orgadmin", "POST", fmt.Sprintf("/api/orgs/%d/teams", w.org.ID), map[string]any{"name": "Night shift"})
	owner := w.as(w.owner)
	id := taskID(t, owner.call(t, "amp_create_task", map[string]any{
		"project_id": float64(w.s.projectID), "story_id": float64(w.s.storyID), "name": "Ship <it>", "description": "d", "acceptance_criteria": "a",
	}))
	owner.finish(t, id)

	got := rc.wait(t, "audit entry and completion", func(g []received) bool {
		ev := strings.Join(events(g), " ")
		return strings.Contains(ev, "audit.team.create") && strings.Contains(ev, "task.completed")
	})
	texts := map[string]string{}
	for _, r := range got {
		var m map[string]string
		if err := json.Unmarshal([]byte(r.body), &m); err != nil {
			t.Fatalf("slack body %q: %v", r.body, err)
		}
		texts[r.event] = m["text"]
	}
	if txt := texts["audit.team.create"]; !strings.Contains(txt, w.orgadmin.Email) || !strings.Contains(txt, "Night shift") {
		t.Errorf("audit text = %q", txt)
	}
	if txt := texts["task.completed"]; !strings.Contains(txt, fmt.Sprintf("#%d Ship &lt;it&gt; completed", id)) {
		t.Errorf("completion text = %q (all: %v)", txt, texts)
	}
	if _, ok := texts["task.created"]; ok {
		t.Error("task.created was sent though not subscribed")
	}
}

// Failed deliveries are retried; deliveries that keep failing turn the
// webhook off.
func TestWebhooks_RetriesAndAutoDisable(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	allowPrivateWebhooks(t)
	startWebhooks(t, w, webhook.Options{AllowPrivateNetworks: true, MaxAttempts: 2, DisableAfter: 2})
	flaky := newReceiver(t)
	flaky.respond = func(n int) int {
		if n == 1 {
			return 503
		}
		return 200
	}
	hook := ra.want(t, 200, "owner", "POST", fmt.Sprintf("/api/projects/%d/webhooks", w.s.projectID), map[string]any{"url": flaky.srv.URL})["webhook"].(map[string]any)
	hookID := int(hook["id"].(float64))
	ra.want(t, 200, "owner", "POST", fmt.Sprintf("/api/webhooks/%d/test", hookID), nil)
	flaky.wait(t, "retry", func(g []received) bool { return len(g) >= 2 })
	// The dispatcher records the outcome after reading the response; wait
	// for it rather than guessing how long that takes under load.
	var d map[string]any
	eventually(t, "the retried delivery to be recorded", func() bool {
		d = ra.want(t, 200, "owner", "GET", fmt.Sprintf("/api/webhooks/%d/deliveries", hookID), nil)["deliveries"].([]any)[0].(map[string]any)
		return d["status"] == "delivered"
	})
	if d["attempts"].(float64) != 2 {
		t.Errorf("delivery after a retry = %v", d)
	}

	down := newReceiver(t)
	down.respond = func(int) int { return 500 }
	ra.want(t, 200, "owner", "PATCH", fmt.Sprintf("/api/webhooks/%d", hookID), map[string]any{"url": down.srv.URL})
	for i := 0; i < 2; i++ {
		ra.want(t, 200, "owner", "POST", fmt.Sprintf("/api/webhooks/%d/test", hookID), nil)
		down.wait(t, "attempts", func(g []received) bool { return len(g) >= 2*(i+1) })
		eventually(t, "the failed delivery to be recorded", func() bool {
			got, _ := w.s.repo.GetWebhook(context.Background(), hookID)
			return got != nil && got.ConsecutiveFailures >= i+1
		})
	}
	got, _ := w.s.repo.GetWebhook(context.Background(), hookID)
	if got.Active || got.ConsecutiveFailures != 2 || !strings.Contains(got.LastError, "HTTP 500") {
		t.Errorf("after two failed deliveries: active=%v failures=%d last_error=%q", got.Active, got.ConsecutiveFailures, got.LastError)
	}
	ra.want(t, 400, "owner", "POST", fmt.Sprintf("/api/webhooks/%d/test", hookID), nil)
	ra.want(t, 200, "owner", "PATCH", fmt.Sprintf("/api/webhooks/%d", hookID), map[string]any{"active": true})
	if got, _ := w.s.repo.GetWebhook(context.Background(), hookID); got.ConsecutiveFailures != 0 {
		t.Errorf("re-enabling kept %d failures", got.ConsecutiveFailures)
	}
}

// Without WEBHOOK_ALLOW_PRIVATE_NETWORKS, webhooks can't be pointed at or
// connect to private addresses.
func TestWebhooks_PrivateNetworksRefused(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	hooks := fmt.Sprintf("/api/projects/%d/webhooks", w.s.projectID)
	for _, u := range []string{"http://127.0.0.1:8080/x", "http://localhost/x", "http://10.1.2.3/x", "http://169.254.169.254/latest",
		"http://amp-postgres-rw.amp.svc.cluster.local:5432/"} {
		ra.want(t, 400, "owner", "POST", hooks, map[string]any{"url": u})
	}

	// And the dispatcher refuses to connect to one, whatever the URL says
	// (a DNS name can resolve to anything): here the stored URL is a
	// loopback address the API check would have refused.
	startWebhooks(t, w, webhook.Options{})
	rc := newReceiver(t)
	allowPrivateWebhooks(t) // only to store it
	hook := ra.want(t, 200, "owner", "POST", hooks, map[string]any{"url": rc.srv.URL})["webhook"].(map[string]any)
	hookID := int(hook["id"].(float64))
	ra.want(t, 200, "owner", "POST", fmt.Sprintf("/api/webhooks/%d/test", hookID), nil)
	deadline := time.Now().Add(5 * time.Second)
	for {
		ds, _ := w.s.repo.ListDeliveries(context.Background(), hookID, 1)
		if len(ds) == 1 && ds[0].Attempts > 0 {
			if !strings.Contains(ds[0].LastError, "private, loopback, and link-local addresses are not allowed") {
				t.Errorf("last error = %q", ds[0].LastError)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("delivery never attempted")
		}
		time.Sleep(20 * time.Millisecond)
	}
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if len(rc.got) != 0 {
		t.Errorf("the private receiver got %d requests", len(rc.got))
	}
}

// eventually polls cond until it holds or a generous deadline passes.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
