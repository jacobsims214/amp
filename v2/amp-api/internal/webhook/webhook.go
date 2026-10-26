// Package webhook delivers AMP events to webhooks.
//
// Events reach the Dispatcher from the SSE hub (ticket, story, epic, and
// project changes, comments) and from the audit log. For each one it finds
// the matching webhooks and writes a delivery per webhook to the
// webhook_deliveries outbox; a delivery loop then POSTs them, retrying with
// backoff. The outbox lives in Postgres and deliveries are leased, so
// several replicas can run dispatchers and a restart loses nothing already
// written. (An event still queued in memory when the process dies is lost;
// the queue is drained in milliseconds.)
//
// Every POST carries:
//
//	X-AMP-Event: task.completed
//	X-AMP-Delivery: 1234
//	X-AMP-Signature: t=1760000000,v1=<hex HMAC-SHA256(secret, "<t>.<body>")>
//
// A receiver checks the signature with the webhook's secret and rejects
// old timestamps to stop replays.
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

	mu       sync.Mutex
	projects map[int]projectInfo // names and orgs for rendering, cached briefly
}

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
	d := &Dispatcher{repo: repo, opts: opts, in: make(chan item, 4096), wake: make(chan struct{}, 1), projects: map[int]projectInfo{}}
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
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsMulticast() || cgnat.Contains(ip)
}

var _, cgnat, _ = net.ParseCIDR("100.64.0.0/10")

// Event queues a hub event. It never blocks (the hub calls it inline).
func (d *Dispatcher) Event(e domain.Event) {
	select {
	case d.in <- item{event: &e}:
	default:
		slog.Warn("webhook queue full: event dropped", "type", e.Type, "project_id", e.ProjectID)
	}
}

// Audit queues an audit entry. It never blocks.
func (d *Dispatcher) Audit(e domain.AuditEntry) {
	select {
	case d.in <- item{audit: &e}:
	default:
		slog.Warn("webhook queue full: audit entry dropped", "action", e.Action)
	}
}

// Wake makes the delivery loop look for due deliveries now.
func (d *Dispatcher) Wake() {
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// Run matches and delivers until ctx ends. It also matches the remote.*
// audit entries amp-relay recorded (relayAuditLoop).
func (d *Dispatcher) Run(ctx context.Context) {
	go d.matchLoop(ctx)
	go d.relayAuditLoop(ctx)
	d.deliverLoop(ctx)
}

// relayAuditLoop takes the audit entries amp-relay recorded (through
// relay_record_audit, which queues them in relay_audit_outbox) and matches
// them like amp-api's own. amp-relay writes no webhook deliveries: amp-api
// renders and signs every body from the stored entry, so a compromised relay
// can't make AMP sign content of its choosing (#2399, re-review #2394 N3).
func (d *Dispatcher) relayAuditLoop(ctx context.Context) {
	tick := time.NewTicker(d.opts.Poll)
	defer tick.Stop()
	for {
		for {
			entries, err := d.repo.ClaimRelayAudits(ctx, 100)
			if err != nil {
				if ctx.Err() == nil {
					slog.Warn("reading amp-relay's audit entries failed", "err", err)
				}
				break
			}
			for i := range entries {
				if err := d.match(ctx, item{audit: &entries[i]}); err != nil && ctx.Err() == nil {
					slog.Error("webhook matching failed", "action", entries[i].Action, "err", err)
				}
			}
			if len(entries) < 100 {
				break
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
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
		if projectID == 0 && orgID == 0 {
			return nil // account-level entries belong to no org
		}
	}
	hooks, err := d.repo.WebhooksForEvent(ctx, projectID, orgID)
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
		body, err := d.render(ctx, h.Format, env, it)
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
	}
	if env.At.IsZero() {
		env.At = time.Now()
	}
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
	if err := d.repo.FinishDelivery(context.WithoutCancel(ctx), dd, ok, status, msg, retryAt, d.opts.DisableAfter); err != nil {
		slog.Error("recording webhook delivery failed", "delivery_id", dd.ID, "err", err)
	}
}

// post sends one delivery; any non-2xx answer is an error.
func (d *Dispatcher) post(ctx context.Context, dd repository.DueDelivery) (int, error) {
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
