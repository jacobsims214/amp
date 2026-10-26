package service

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strings"

	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/webhook"
)

// Webhooks: project owners manage their project's; org owners and admins
// manage org-wide ones, which fire for every project in the org. Either
// can opt into audit entries for its scope ("audit.*"): the same entries
// its managers can read in the audit log.

// AllowPrivateWebhooks lets webhook URLs point at private, loopback, and
// link-local addresses (set from WEBHOOK_ALLOW_PRIVATE_NETWORKS; the
// dispatcher enforces the same rule when it connects).
var AllowPrivateWebhooks bool

// WebhookChange is a webhook as created or edited; nil fields are kept.
type WebhookChange struct {
	Name   *string   `json:"name"`
	URL    *string   `json:"url"`
	Format *string   `json:"format"`
	Events *[]string `json:"events"`
	Active *bool     `json:"active"`
}

// requireWebhookScope checks p may manage webhooks for an org or a project
// (exactly one of them).
func (s *Service) requireWebhookScope(ctx context.Context, p authz.Principal, orgID, projectID *int) error {
	switch {
	case (orgID == nil) == (projectID == nil):
		return authz.BadRequest("a webhook belongs to exactly one org or one project")
	case orgID != nil:
		_, err := s.Authz.RequireOrgAdmin(ctx, p, *orgID)
		return err
	default:
		_, err := s.Authz.RequireProject(ctx, p, *projectID, authz.Owner)
		return err
	}
}

// loadWebhook fetches a webhook p may manage.
func (s *Service) loadWebhook(ctx context.Context, p authz.Principal, id int) (*domain.Webhook, error) {
	w, err := s.Repo.GetWebhook(ctx, id)
	if err != nil {
		return nil, authz.NotFound("webhook %d not found", id)
	}
	if err := s.requireWebhookScope(ctx, p, w.OrgID, w.ProjectID); err != nil {
		var ae *authz.Error
		if ok := asAuthz(err, &ae); ok && ae.Status == 404 {
			return nil, authz.NotFound("webhook %d not found", id)
		}
		return nil, err
	}
	return w, nil
}

func (s *Service) ListWebhooks(ctx context.Context, p authz.Principal, orgID, projectID *int) ([]domain.Webhook, error) {
	if err := s.requireWebhookScope(ctx, p, orgID, projectID); err != nil {
		return nil, err
	}
	return s.Repo.ListWebhooks(ctx, orgID, projectID)
}

// CreateWebhook adds a webhook. The result carries its signing secret,
// which isn't shown again (rotate it to get a new one).
func (s *Service) CreateWebhook(ctx context.Context, p authz.Principal, orgID, projectID *int, c WebhookChange) (*domain.Webhook, error) {
	if err := s.requireWebhookScope(ctx, p, orgID, projectID); err != nil {
		return nil, err
	}
	w := domain.Webhook{OrgID: orgID, ProjectID: projectID, Format: domain.WebhookFormatJSON, Active: true, CreatedBy: p.UserRef(), Events: []string{}}
	if c.URL == nil {
		return nil, authz.BadRequest("url is required")
	}
	if err := applyWebhookChange(&w, c); err != nil {
		return nil, err
	}
	secret, err := webhook.NewSecret()
	if err != nil {
		return nil, err
	}
	w.Secret = secret
	created, err := s.Repo.CreateWebhook(ctx, w)
	if err != nil {
		return nil, err
	}
	e := webhookAudit("webhook.create", created, "added webhook "+describeWebhook(created))
	e.After = webhookValue(created)
	s.audit(ctx, p, e)
	return created, nil
}

func (s *Service) UpdateWebhook(ctx context.Context, p authz.Principal, id int, c WebhookChange) (*domain.Webhook, error) {
	w, err := s.loadWebhook(ctx, p, id)
	if err != nil {
		return nil, err
	}
	before := webhookValue(w)
	if err := applyWebhookChange(w, c); err != nil {
		return nil, err
	}
	updated, err := s.Repo.UpdateWebhook(ctx, *w)
	if err != nil {
		return nil, err
	}
	e := webhookAudit("webhook.update", updated, "changed webhook "+describeWebhook(updated))
	e.Before, e.After = before, webhookValue(updated)
	s.audit(ctx, p, e)
	return updated, nil
}

func (s *Service) DeleteWebhook(ctx context.Context, p authz.Principal, id int) error {
	w, err := s.loadWebhook(ctx, p, id)
	if err != nil {
		return err
	}
	if err := s.Repo.DeleteWebhook(ctx, id); err != nil {
		return err
	}
	e := webhookAudit("webhook.delete", w, "deleted webhook "+describeWebhook(w))
	e.Before = webhookValue(w)
	s.audit(ctx, p, e)
	return nil
}

// RotateWebhookSecret replaces a webhook's signing secret and returns it.
func (s *Service) RotateWebhookSecret(ctx context.Context, p authz.Principal, id int) (*domain.Webhook, error) {
	w, err := s.loadWebhook(ctx, p, id)
	if err != nil {
		return nil, err
	}
	secret, err := webhook.NewSecret()
	if err != nil {
		return nil, err
	}
	if err := s.Repo.SetWebhookSecret(ctx, id, secret); err != nil {
		return nil, err
	}
	s.audit(ctx, p, webhookAudit("webhook.rotate_secret", w, "rotated the signing secret of webhook "+describeWebhook(w)))
	w.Secret = secret
	return w, nil
}

// TestWebhook queues a "ping" delivery.
func (s *Service) TestWebhook(ctx context.Context, p authz.Principal, id int) (int64, error) {
	w, err := s.loadWebhook(ctx, p, id)
	if err != nil {
		return 0, err
	}
	if !w.Active {
		return 0, authz.BadRequest("webhook %d is turned off; turn it on to test it", id)
	}
	body := `{"type":"ping","data":{"message":"Test message from AMP: this webhook works."}}`
	if w.Format == domain.WebhookFormatSlack {
		body = `{"text":"👋 Test message from AMP: this webhook works."}`
	}
	return s.Repo.EnqueueDelivery(ctx, id, string(domain.EventPing), body)
}

func (s *Service) ListDeliveries(ctx context.Context, p authz.Principal, id, limit int) ([]domain.WebhookDelivery, error) {
	if _, err := s.loadWebhook(ctx, p, id); err != nil {
		return nil, err
	}
	return s.Repo.ListDeliveries(ctx, id, limit)
}

func applyWebhookChange(w *domain.Webhook, c WebhookChange) error {
	if c.Name != nil {
		w.Name = strings.TrimSpace(*c.Name)
	}
	if c.URL != nil {
		u, err := validateWebhookURL(*c.URL)
		if err != nil {
			return err
		}
		w.URL = u
	}
	if c.Format != nil {
		switch *c.Format {
		case domain.WebhookFormatJSON, domain.WebhookFormatSlack:
			w.Format = *c.Format
		default:
			return authz.BadRequest("format must be json or slack")
		}
	}
	if c.Events != nil {
		events, err := domain.ValidateWebhookEvents(*c.Events)
		if err != nil {
			return authz.BadRequest("%s", err.Error())
		}
		w.Events = events
	}
	if c.Active != nil {
		w.Active = *c.Active
	}
	return nil
}

func validateWebhookURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" {
		return "", authz.BadRequest("url must be an http(s) URL")
	}
	if !AllowPrivateWebhooks {
		host := u.Hostname()
		ip := net.ParseIP(host)
		if strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".localhost") ||
			strings.HasSuffix(strings.ToLower(host), ".svc.cluster.local") || (ip != nil && webhook.Blocked(ip)) {
			return "", authz.BadRequest("webhooks can't reach private, loopback, or in-cluster addresses on this server")
		}
	}
	return raw, nil
}

func webhookAudit(action string, w *domain.Webhook, summary string) domain.AuditEntry {
	return domain.AuditEntry{Action: action, OrgID: w.OrgID, ProjectID: w.ProjectID, TargetType: "webhook", TargetID: idStr(w.ID),
		Summary: summary, Details: map[string]any{"url": redactURL(w.URL), "format": w.Format, "events": w.Events, "active": w.Active}}
}

func describeWebhook(w *domain.Webhook) string {
	if w.Name != "" {
		return w.Name + " (" + redactURL(w.URL) + ")"
	}
	return redactURL(w.URL)
}

// redactURL keeps a URL's scheme and host: the path of a Slack (and many
// other) webhook URLs is itself a credential.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "(invalid url)"
	}
	if u.Path == "" || u.Path == "/" {
		return u.Scheme + "://" + u.Host
	}
	return u.Scheme + "://" + u.Host + "/…"
}

func asAuthz(err error, target **authz.Error) bool { return errors.As(err, target) }

// webhookValue is a webhook's configuration as an audit before/after
// value. The signing secret never appears, and redact() cuts the URL to
// scheme and host (its path is often itself a credential).
func webhookValue(w *domain.Webhook) map[string]any {
	events := make([]any, 0, len(w.Events))
	for _, e := range w.Events {
		events = append(events, e)
	}
	return map[string]any{"name": w.Name, "url": w.URL, "format": w.Format, "events": events, "active": w.Active}
}
