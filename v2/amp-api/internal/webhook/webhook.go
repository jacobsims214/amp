// Package webhook delivers AMP events to webhooks.
//
// Events reach the Dispatcher from the SSE hub (ticket, story, epic, and
// project changes, comments) and from the audit log. For each one it finds
// the matching webhooks and writes a delivery per webhook to the
// webhook_deliveries outbox; a delivery loop then POSTs them, retrying with
// backoff. The outbox lives in Postgres and deliveries are leased, so
// several replicas can run dispatchers and a restart loses nothing already
// written. (A hub event still queued in memory when the process dies is
// lost; the queue is drained in milliseconds.)
//
// Audit entries never pass through memory: a trigger queues every audit_log
// row in audit_outbox in the statement that writes it (migration 033), and
// the audit loop leases them from there, matches them, and removes them, so
// each is matched at least once even across restarts and a full queue (the
// SIEM path; #2429). Audit only wakes that loop.
//
// Every POST carries:
//
//	X-AMP-Event: task.completed
//	X-AMP-Delivery: 1234
//	X-AMP-Signature: t=1760000000,v1=<hex HMAC-SHA256(secret, "<t>.<body>")>
//
// A receiver checks the signature with the webhook's secret and rejects
// old timestamps to stop replays. A webhook may also carry one static
// header of its own (AuthHeaderName, e.g. "Authorization: Splunk <token>")
// for SIEM collectors that authenticate that way; its value is a secret
// and is never logged.
//
// Instance webhooks (no org, no project; global admins) receive every
// audit entry, including the account-level ones no org owns: the SIEM
// stream of the whole AMP.
package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/repository"
)

const (
	// MaxAttempts is how many times a delivery is tried before it fails.
	MaxAttempts = 8
	// DisableAfter turns a webhook off after this many deliveries in a row
	// have failed for good; editing or re-enabling it resets the count.
	DisableAfter = 20
	// Keep is how long finished deliveries are kept for inspection.
	Keep = 14 * 24 * time.Hour
)

// backoff is the wait before attempt n+1 (n = attempts made so far).
var backoff = []time.Duration{10 * time.Second, 30 * time.Second, 2 * time.Minute, 10 * time.Minute,
	30 * time.Minute, time.Hour, 3 * time.Hour}

// Options configure a Dispatcher.
type Options struct {
	// AllowPrivateNetworks lets webhooks reach loopback, private, and
	// link-local addresses. Off by default: a webhook URL is chosen by a
	// project owner, and must not become a way to reach services inside
	// the cluster.
	AllowPrivateNetworks bool
	// Poll is how often the delivery loop looks for due deliveries.
	Poll time.Duration
	// Timeout bounds one POST.
	Timeout time.Duration
	// Backoff, MaxAttempts, and DisableAfter override the package defaults
	// (for tests).
	Backoff      []time.Duration
	MaxAttempts  int
	DisableAfter int
}

// Dispatcher matches events to webhooks and delivers them.
type Dispatcher struct {
	repo   *repository.Repo
	opts   Options
	client *http.Client
	in     chan item
	wake   chan struct{}
	// auditWake nudges the audit loop (Audit).
	auditWake chan struct{}

	mu       sync.Mutex
	projects map[int]projectInfo // names and orgs for rendering, cached briefly
	// systemAudit records the dispatcher's own audit entries (OnSystemAudit).
	systemAudit func(context.Context, domain.AuditEntry)
	// dropped counts each webhook's dropped deliveries since its last
	// webhook.delivery.dropped entry.
	dropped map[int]*dropCount
}

// dropCount is a webhook's dropped deliveries not yet in an audit entry.
type dropCount struct {
	n    int
	last time.Time // when the last entry was written
}

// DroppedAuditEvery is how often, at most, a webhook's dropped deliveries
// are recorded (webhook.delivery.dropped): a dead receiver drops every
// delivery, and each entry is itself delivered to it.
const DroppedAuditEvery = time.Hour

type projectInfo struct {
	name  string
	orgID int
	at    time.Time
}

// item is an event or audit entry waiting to be matched.
type item struct {
	event *domain.Event
	audit *domain.AuditEntry
}

func New(repo *repository.Repo, opts Options) *Dispatcher {
	if opts.Poll <= 0 {
		opts.Poll = time.Second
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 10 * time.Second
	}
	if len(opts.Backoff) == 0 {
		opts.Backoff = backoff
	}
	if opts.MaxAttempts <= 0 {
		opts.MaxAttempts = MaxAttempts
	}
	if opts.DisableAfter <= 0 {
		opts.DisableAfter = DisableAfter
	}
	d := &Dispatcher{repo: repo, opts: opts, in: make(chan item, 4096), wake: make(chan struct{}, 1), auditWake: make(chan struct{}, 1),
		projects: map[int]projectInfo{}, dropped: map[int]*dropCount{}}
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	if !opts.AllowPrivateNetworks {
		// Checked on the resolved address of every connection, so DNS
		// tricks and redirects can't get around it.
		dialer.Control = func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			if ip := net.ParseIP(host); ip != nil && Blocked(ip) {
				return fmt.Errorf("webhook to %s refused: private, loopback, and link-local addresses are not allowed", ip)
			}
			return nil
		}
	}
	d.client = &http.Client{
		Timeout:   opts.Timeout,
		Transport: &http.Transport{DialContext: dialer.DialContext, Proxy: nil, MaxIdleConnsPerHost: 4},
		// A webhook answers where it was told to; a redirect is a failure.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return d
}

// Blocked reports whether ip is off limits without AllowPrivateNetworks.
func Blocked(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsMulticast() {
		return true
	}
	for _, n := range blockedNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// blockedNets are the other ranges a webhook must not reach: carrier-grade
// NAT, "this network", benchmarking, reserved, and NAT64 (which maps any
// IPv4 address, private ones included, into IPv6).
var blockedNets = func() []*net.IPNet {
	var out []*net.IPNet
	for _, c := range []string{"100.64.0.0/10", "0.0.0.0/8", "198.18.0.0/15", "240.0.0.0/4", "64:ff9b::/96", "64:ff9b:1::/48"} {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			panic(err)
		}
		out = append(out, n)
	}
	return out
}()

// ValidAuthHeader checks a webhook's extra header: a plain header name
// that doesn't replace one AMP sets or one that shapes the request, and a
// value with no line breaks.
func ValidAuthHeader(name, value string) error {
	if name == "" || len(name) > 64 {
		return errors.New("the header name must be 1 to 64 characters")
	}
	for _, c := range name {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return fmt.Errorf("the header name %q may only use letters, digits, '-' and '_'", name)
		}
	}
	canon := http.CanonicalHeaderKey(name)
	if reservedHeaders[canon] || strings.HasPrefix(canon, "X-Amp-") || strings.HasPrefix(canon, "Proxy-") || strings.HasPrefix(canon, "Sec-") {
		return fmt.Errorf("the header %s is set by AMP or by HTTP itself; pick another", canon)
	}
	if value == "" || len(value) > 4096 {
		return errors.New("the header value must be 1 to 4096 characters")
	}
	for _, c := range value {
		if c == '\r' || c == '\n' || c == 0 {
			return errors.New("the header value can't contain line breaks")
		}
	}
	return nil
}

var reservedHeaders = map[string]bool{
	"Host": true, "Content-Type": true, "Content-Length": true, "Content-Encoding": true, "Transfer-Encoding": true,
	"Connection": true, "Keep-Alive": true, "Upgrade": true, "Te": true, "Trailer": true, "Expect": true,
	"User-Agent": true, "Cookie": true, "Forwarded": true, "X-Forwarded-For": true, "X-Forwarded-Host": true,
	"X-Forwarded-Proto": true, "X-Real-Ip": true,
}

// Event queues a hub event. It never blocks (the hub calls it inline).
func (d *Dispatcher) Event(e domain.Event) {
	select {
	case d.in <- item{event: &e}:
	default:
		slog.Warn("webhook queue full: event dropped", "type", e.Type, "project_id", e.ProjectID)
	}
}

// Audit is told about each audit entry this process writes, and wakes the
// audit loop; the entry itself is read from audit_outbox, where a trigger
// queued it. It never blocks.
func (d *Dispatcher) Audit(domain.AuditEntry) {
	select {
	case d.auditWake <- struct{}{}:
	default:
	}
}

// OnSystemAudit sets how the dispatcher records its own audit entries
// (#2439 M4): webhook.disable when it turns a failing webhook off, and
// webhook.delivery.dropped when deliveries fail for good, so the SIEM
// stream can't stop unnoticed. main passes the service's system writer.
func (d *Dispatcher) OnSystemAudit(fn func(context.Context, domain.AuditEntry)) {
	d.mu.Lock()
	d.systemAudit = fn
	d.mu.Unlock()
}

func (d *Dispatcher) recordSystem(ctx context.Context, e domain.AuditEntry) {
	d.mu.Lock()
	fn := d.systemAudit
	d.mu.Unlock()
	if fn != nil {
		fn(context.WithoutCancel(ctx), e)
	}
}

// Wake makes the delivery loop look for due deliveries now.
func (d *Dispatcher) Wake() {
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// Run matches and delivers until ctx ends.
func (d *Dispatcher) Run(ctx context.Context) {
	go d.matchLoop(ctx)
	go d.auditLoop(ctx)
	d.deliverLoop(ctx)
}

// auditOutboxBatch is how many audit entries the audit loop leases at once,
// and auditOutboxLease how long it has to match them before another
// dispatcher may.
const (
	auditOutboxBatch = 100
	auditOutboxLease = time.Minute
)

// auditLoop matches the audit entries queued in audit_outbox, whoever wrote
// them (any replica, authadmin, or m1's relay), until ctx ends.
func (d *Dispatcher) auditLoop(ctx context.Context) {
	tick := time.NewTicker(d.opts.Poll)
	defer tick.Stop()
	for {
		d.drainAudit(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-d.auditWake:
		}
	}
}

// drainAudit matches queued audit entries until none are due. An entry
// whose matching fails stays queued and is retried when its lease ends.
func (d *Dispatcher) drainAudit(ctx context.Context) {
	for ctx.Err() == nil {
		entries, err := d.repo.ClaimAuditOutbox(ctx, auditOutboxBatch, auditOutboxLease)
		if err != nil {
			if ctx.Err() == nil {
				slog.Error("reading the audit outbox failed", "err", err)
			}
			return
		}
		done := make([]int64, 0, len(entries))
		for i := range entries {
			if err := d.match(ctx, item{audit: &entries[i]}); err != nil {
				if ctx.Err() == nil {
					slog.Error("webhook matching failed; the audit entry stays queued", "audit_id", entries[i].ID, "action", entries[i].Action, "err", err)
				}
				continue
			}
			done = append(done, entries[i].ID)
		}
		if err := d.repo.AckAuditOutbox(context.WithoutCancel(ctx), done); err != nil {
			slog.Error("removing matched audit entries from the outbox failed; they will be matched again", "err", err)
			return
		}
		if len(entries) < auditOutboxBatch {
			return
		}
	}
}

func (d *Dispatcher) matchLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case it := <-d.in:
			if err := d.match(ctx, it); err != nil && ctx.Err() == nil {
				slog.Error("webhook matching failed", "err", err)
			}
		}
	}
}

// match writes a delivery for every webhook that wants it.
func (d *Dispatcher) match(ctx context.Context, it item) error {
	var typ string
	var projectID, orgID int
	switch {
	case it.event != nil:
		typ, projectID = string(it.event.Type), it.event.ProjectID
		if projectID == 0 {
			return nil
		}
	case it.audit != nil:
		typ = "audit." + it.audit.Action
		if it.audit.ProjectID != nil {
			projectID = *it.audit.ProjectID
		}
		if it.audit.OrgID != nil {
			orgID = *it.audit.OrgID
		}
		// Account-level entries (no org, no project) reach only the
		// instance webhooks, which WebhooksForEvent adds for every audit
		// entry.
	}
	hooks, err := d.repo.WebhooksForEvent(ctx, projectID, orgID, it.audit != nil)
	if err != nil || len(hooks) == 0 {
		return err
	}
	if orgID == 0 && projectID != 0 {
		orgID = d.project(ctx, projectID).orgID
	}
	env := envelope(it, typ, projectID, orgID)
	queued := false
	for _, h := range hooks {
		if !domain.WebhookMatches(h.Events, typ) {
			continue
		}
		body, err := d.render(ctx, h.Format, forHook(env, it, h), it)
		if err != nil {
			return err
		}
		if _, err := d.repo.EnqueueDelivery(ctx, h.ID, typ, body); err != nil {
			return err
		}
		queued = true
	}
	if queued {
		d.Wake()
	}
	return nil
}

// Envelope is the JSON body of a json-format delivery.
type Envelope struct {
	ID        string    `json:"id"` // unique per event, the same across webhooks
	Type      string    `json:"type"`
	At        time.Time `json:"at"`
	ProjectID int       `json:"project_id,omitempty"`
	OrgID     int       `json:"org_id,omitempty"`
	Data      any       `json:"data"`
}

func envelope(it item, typ string, projectID, orgID int) Envelope {
	id := make([]byte, 8)
	_, _ = rand.Read(id)
	env := Envelope{ID: "evt_" + hex.EncodeToString(id), Type: typ, ProjectID: projectID, OrgID: orgID}
	if it.event != nil {
		env.At, env.Data = it.event.At, it.event.Payload
	} else {
		env.At, env.Data = it.audit.At, it.audit
		// Delivery is at least once: a stable id lets receivers drop a
		// repeat.
		env.ID = fmt.Sprintf("evt_audit_%d", it.audit.ID)
	}
	if env.At.IsZero() {
		env.At = time.Now()
	}
	return env
}

// forHook is env as webhook h may see it: an audit entry's request context
// (source_ip, user_agent, session_id, token_id, before, after, and the raw
// X-Forwarded-For in details.xff: personal data added by audit log v2) only
// goes to webhooks that opted in with include_context (#2439 M2). Hooks
// made before v2 keep the v1 shape.
func forHook(env Envelope, it item, h domain.Webhook) Envelope {
	if it.audit == nil || h.IncludeContext {
		return env
	}
	e := *it.audit
	e.SourceIP, e.UserAgent, e.SessionID, e.TokenID, e.Before, e.After = "", "", "", nil, nil, nil
	if _, ok := e.Details["xff"]; ok {
		details := make(map[string]any, len(e.Details))
		for k, v := range e.Details {
			if k != "xff" {
				details[k] = v
			}
		}
		e.Details = details
	}
	env.Data = &e
	return env
}

func (d *Dispatcher) render(ctx context.Context, format string, env Envelope, it item) (string, error) {
	if format == domain.WebhookFormatSlack {
		b, err := json.Marshal(map[string]string{"text": d.slackText(ctx, env, it)})
		return string(b), err
	}
	b, err := json.Marshal(env)
	return string(b), err
}

// project returns a project's name and org, cached for a minute.
func (d *Dispatcher) project(ctx context.Context, id int) projectInfo {
	d.mu.Lock()
	info, ok := d.projects[id]
	d.mu.Unlock()
	if ok && time.Since(info.at) < time.Minute {
		return info
	}
	info = projectInfo{name: fmt.Sprintf("project %d", id), at: time.Now()}
	if p, err := d.repo.GetProject(ctx, id); err == nil {
		info.name = p.Name
		if p.OrgID != nil {
			info.orgID = *p.OrgID
		}
	}
	d.mu.Lock()
	d.projects[id] = info
	d.mu.Unlock()
	return info
}

// ---- Delivery ----

func (d *Dispatcher) deliverLoop(ctx context.Context) {
	tick := time.NewTicker(d.opts.Poll)
	defer tick.Stop()
	prune := time.NewTicker(time.Hour)
	defer prune.Stop()
	for {
		d.deliverDue(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-d.wake:
		case <-prune.C:
			if n, err := d.repo.PruneDeliveries(ctx, Keep); err != nil {
				slog.Warn("pruning webhook deliveries failed", "err", err)
			} else if n > 0 {
				slog.Info("pruned old webhook deliveries", "count", n)
			}
		}
	}
}

// deliverDue sends what's due, a batch at a time, a few at once.
func (d *Dispatcher) deliverDue(ctx context.Context) {
	for ctx.Err() == nil {
		due, err := d.repo.ClaimDueDeliveries(ctx, 16, 2*d.opts.Timeout+30*time.Second)
		if err != nil {
			if ctx.Err() == nil {
				slog.Error("claiming webhook deliveries failed", "err", err)
			}
			return
		}
		if len(due) == 0 {
			return
		}
		var wg sync.WaitGroup
		sem := make(chan struct{}, 4)
		for _, dd := range due {
			wg.Add(1)
			sem <- struct{}{}
			go func(dd repository.DueDelivery) {
				defer wg.Done()
				defer func() { <-sem }()
				d.deliver(ctx, dd)
			}(dd)
		}
		wg.Wait()
	}
}

func (d *Dispatcher) deliver(ctx context.Context, dd repository.DueDelivery) {
	status, err := d.post(ctx, dd)
	ok := err == nil
	msg := ""
	var retryAt *time.Time
	if !ok {
		msg = err.Error()
		if n := dd.Attempts; n+1 < d.opts.MaxAttempts {
			t := time.Now().Add(d.opts.Backoff[min(n, len(d.opts.Backoff)-1)])
			retryAt = &t
		}
		slog.Warn("webhook delivery failed", "webhook_id", dd.WebhookID, "delivery_id", dd.ID, "attempt", dd.Attempts+1, "status", status, "err", msg, "will_retry", retryAt != nil)
	}
	out, err := d.repo.FinishDelivery(context.WithoutCancel(ctx), dd, ok, status, msg, retryAt, d.opts.DisableAfter)
	if err != nil {
		slog.Error("recording webhook delivery failed", "delivery_id", dd.ID, "err", err)
		return
	}
	if out.Failed {
		d.auditDropped(ctx, dd, out, status, msg)
	}
	if out.Disabled {
		slog.Warn("webhook turned off after repeated failures", "webhook_id", dd.WebhookID, "consecutive_failures", out.ConsecutiveFailures)
		d.recordSystem(ctx, domain.AuditEntry{Action: "webhook.disable", TargetType: "webhook", TargetID: strconv.Itoa(dd.WebhookID),
			OrgID: out.OrgID, ProjectID: out.ProjectID,
			Summary: fmt.Sprintf("turned webhook %d off after %d deliveries in a row failed", dd.WebhookID, out.ConsecutiveFailures),
			Details: map[string]any{"reason": "consecutive_failures", "consecutive_failures": out.ConsecutiveFailures,
				"last_status": status, "last_error": truncate(msg, 300)}})
	}
}

// auditDropped records a delivery that failed for good, at most once per
// webhook per DroppedAuditEvery (the entry counts the drops since the last
// one).
func (d *Dispatcher) auditDropped(ctx context.Context, dd repository.DueDelivery, out repository.DeliveryOutcome, status int, msg string) {
	d.mu.Lock()
	c := d.dropped[dd.WebhookID]
	if c == nil {
		c = &dropCount{}
		d.dropped[dd.WebhookID] = c
	}
	c.n++
	if !c.last.IsZero() && time.Since(c.last) < DroppedAuditEvery {
		d.mu.Unlock()
		return
	}
	n := c.n
	c.n, c.last = 0, time.Now()
	d.mu.Unlock()
	d.recordSystem(ctx, domain.AuditEntry{Action: "webhook.delivery.dropped", TargetType: "webhook", TargetID: strconv.Itoa(dd.WebhookID),
		OrgID: out.OrgID, ProjectID: out.ProjectID,
		Summary: fmt.Sprintf("dropped %d delivery(ies) to webhook %d after %d attempts each", n, dd.WebhookID, d.opts.MaxAttempts),
		Details: map[string]any{"dropped": n, "event_type": dd.EventType, "delivery_id": dd.ID, "attempts": d.opts.MaxAttempts,
			"last_status": status, "last_error": truncate(msg, 300), "consecutive_failures": out.ConsecutiveFailures}})
}

// post sends one delivery; any non-2xx answer is an error.
func (d *Dispatcher) post(ctx context.Context, dd repository.DueDelivery) (int, error) {
	if dd.SecretsErr != nil {
		// Never send unsigned, or without the header the receiver needs:
		// fail the attempt (it retries) until the key is back (#2441).
		return 0, fmt.Errorf("not sent: can't decrypt the webhook's stored secrets: %w", dd.SecretsErr)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, dd.URL, bytes.NewReader([]byte(dd.Body)))
	if err != nil {
		return 0, err
	}
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "AMP-Webhooks/1")
	req.Header.Set("X-AMP-Event", dd.EventType)
	req.Header.Set("X-AMP-Delivery", strconv.FormatInt(dd.ID, 10))
	req.Header.Set("X-AMP-Signature", "t="+ts+",v1="+Sign(dd.Secret, ts, dd.Body))
	if dd.AuthHeaderName != "" {
		// Validated when saved (ValidAuthHeader); it can't replace the
		// headers above.
		req.Header.Set(dd.AuthHeaderName, dd.AuthHeaderValue)
	}
	resp, err := d.client.Do(req)
	if err != nil {
		var opErr *net.OpError
		if errors.As(err, &opErr) && opErr.Op == "dial" {
			return 0, fmt.Errorf("connect: %v", opErr.Err)
		}
		return 0, err
	}
	defer resp.Body.Close()
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp.StatusCode, fmt.Errorf("HTTP %d: %s", resp.StatusCode, bytes.TrimSpace(snippet))
	}
	return resp.StatusCode, nil
}

// Sign is the v1 signature of body sent at timestamp ts.
func Sign(secret, ts, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "." + body))
	return hex.EncodeToString(mac.Sum(nil))
}

// NewSecret generates a webhook signing secret.
func NewSecret() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "whsec_" + hex.EncodeToString(b), nil
}
