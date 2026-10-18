package service

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/simstech/amp-api/internal/actor"
	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
)

// ---- Artifacts: things agents make for people to review ----

// PublishArtifactRequest publishes a new artifact, or a new version of one
// (ArtifactID set).
type PublishArtifactRequest struct {
	ArtifactID int
	ProjectID  int
	TaskID     *int
	Title      string
	Kind       string
	Content    []byte
	Note       string
}

// ArtifactURL is where people view an artifact in AMP, or "" when AMP's
// address isn't known.
func (s *Service) ArtifactURL(id int) string {
	if s.AppURL == "" {
		return ""
	}
	return fmt.Sprintf("%s/artifacts/%d", strings.TrimSuffix(s.AppURL, "/"), id)
}

func (s *Service) withURL(a *domain.Artifact) *domain.Artifact {
	if a != nil {
		a.URL = s.ArtifactURL(a.ID)
	}
	return a
}

// artifactContentType checks content against its kind and returns the type
// AMP serves it as.
func artifactContentType(kind string, content []byte) (string, error) {
	sniffed := http.DetectContentType(content)
	switch kind {
	case domain.ArtifactHTML:
		return "text/html; charset=utf-8", nil
	case domain.ArtifactSVG:
		if !strings.Contains(string(content[:min(len(content), 4096)]), "<svg") {
			return "", authz.BadRequest("kind svg needs SVG markup (an <svg> element)")
		}
		return "image/svg+xml", nil
	case domain.ArtifactMarkdown:
		return "text/markdown; charset=utf-8", nil
	case domain.ArtifactText:
		return "text/plain; charset=utf-8", nil
	case domain.ArtifactPDF:
		if sniffed != "application/pdf" {
			return "", authz.BadRequest("kind pdf needs a PDF file")
		}
		return "application/pdf", nil
	case domain.ArtifactImage:
		if !strings.HasPrefix(sniffed, "image/") {
			return "", authz.BadRequest("kind image needs a PNG, JPEG, GIF or WebP file (got %s)", sniffed)
		}
		return sniffed, nil
	}
	return "", authz.BadRequest("kind must be one of html, svg, image, markdown, pdf, text")
}

// PublishArtifact stores an artifact, or a new version of one, and notes it
// on its ticket. Anyone who can work the ticket may publish to it; without a
// ticket, any project member. A new version goes back into review.
func (s *Service) PublishArtifact(ctx context.Context, p authz.Principal, req PublishArtifactRequest) (*domain.Artifact, error) {
	if len(req.Content) == 0 {
		return nil, authz.BadRequest("content is empty")
	}
	if len(req.Content) > domain.MaxArtifactBytes {
		return nil, authz.BadRequest("content is %d bytes; the limit is %d (10 MB)", len(req.Content), domain.MaxArtifactBytes)
	}
	req.Title = strings.TrimSpace(req.Title)

	var existing *domain.Artifact
	if req.ArtifactID != 0 {
		a, err := s.Repo.GetArtifact(ctx, req.ArtifactID)
		if err != nil {
			return nil, authz.NotFound("artifact %d not found", req.ArtifactID)
		}
		existing = a
		req.ProjectID, req.TaskID = a.ProjectID, a.TaskID
		if req.Kind == "" {
			req.Kind = a.Kind
		}
		if req.Kind != a.Kind {
			return nil, authz.BadRequest("artifact %d is %s; a new version must be too (publish a new artifact for a different kind)", a.ID, a.Kind)
		}
	} else if req.Title == "" {
		return nil, authz.BadRequest("title is required")
	}
	contentType, err := artifactContentType(req.Kind, req.Content)
	if err != nil {
		return nil, err
	}

	if req.TaskID != nil {
		t, _, err := s.LoadTask(ctx, p, *req.TaskID, authz.TaskWork)
		if err != nil {
			return nil, err
		}
		// A design system phase has one artifact; further rounds are versions
		// of it, so the approval the publish step checks is unambiguous.
		if phase, _ := t.Metadata["design_system_phase"].(string); phase != "" && existing == nil {
			if arts, err := s.Repo.ListArtifacts(ctx, t.ProjectID, req.TaskID); err == nil && len(arts) > 0 {
				return nil, authz.BadRequest("ticket %d (design system phase %q) already has artifact #%d: publish a new version of it with artifact_id=%d", t.ID, phase, arts[0].ID, arts[0].ID)
			}
		}
		if req.ProjectID != 0 && req.ProjectID != t.ProjectID {
			return nil, authz.BadRequest("ticket %d is in project %d, not %d", t.ID, t.ProjectID, req.ProjectID)
		}
		req.ProjectID = t.ProjectID
	} else if _, err := s.Authz.RequireProject(ctx, p, req.ProjectID, authz.Member); err != nil {
		return nil, err
	}

	if existing != nil {
		if n, err := s.Repo.CountArtifactVersions(ctx, existing.ID); err == nil && n >= domain.MaxArtifactVersions {
			return nil, authz.BadRequest("artifact %d has %d versions, the most one artifact keeps; publish a new artifact", existing.ID, n)
		}
	}
	var a *domain.Artifact
	if existing != nil {
		a, err = s.Repo.AddArtifactVersion(ctx, existing.ID, req.Title, req.Content, contentType, req.Note, p.UserRef(), s.artifactExpiry())
	} else {
		a, err = s.Repo.CreateArtifact(ctx, domain.Artifact{ProjectID: req.ProjectID, TaskID: req.TaskID, Title: req.Title,
			Kind: req.Kind, CreatedBy: p.UserRef()}, req.Content, contentType, req.Note, s.artifactExpiry())
	}
	if err != nil {
		return nil, err
	}
	s.withURL(a)
	s.audit(ctx, p, domain.AuditEntry{Action: "artifact.publish", ProjectID: intRef(a.ProjectID), TargetType: "artifact",
		TargetID: idStr(a.ID), Summary: fmt.Sprintf("published %q v%d", a.Title, a.CurrentVersion)})
	if a.TaskID != nil {
		body := fmt.Sprintf("Published artifact #%d %q v%d (%s) for review.", a.ID, a.Title, a.CurrentVersion, a.Kind)
		if req.Note != "" {
			body += " " + req.Note
		}
		if a.URL != "" {
			body += "\n" + a.URL
		}
		s.comment(a.ProjectID, *a.TaskID, p, body)
	}
	return a, nil
}

// comment posts on a ticket as p; a failure is not the caller's problem.
func (s *Service) comment(projectID, taskID int, p authz.Principal, body string) {
	reply := make(chan actor.ReplyAddComment, 1)
	if err := s.Registry.Send(projectID, &actor.MsgAddComment{Req: domain.AddCommentRequest{
		TaskID: taskID, Author: p.Email, Body: body, AuthorUserID: p.UserRef(),
	}, ReplyCh: reply}); err == nil {
		<-reply
	}
}

// GetArtifact loads an artifact and its versions for a project member.
func (s *Service) GetArtifact(ctx context.Context, p authz.Principal, id int) (*domain.Artifact, []domain.ArtifactVersion, error) {
	a, err := s.Repo.GetArtifact(ctx, id)
	if err != nil {
		return nil, nil, authz.NotFound("artifact %d not found", id)
	}
	if _, err := s.Authz.RequireProject(ctx, p, a.ProjectID, authz.Member); err != nil {
		if authz.StatusOf(err, 0) == 404 {
			return nil, nil, authz.NotFound("artifact %d not found", id)
		}
		return nil, nil, err
	}
	versions, err := s.Repo.ListArtifactVersions(ctx, id)
	return s.withURL(a), versions, err
}

// ArtifactContent loads one version's content (0: the current one) for a project member.
func (s *Service) ArtifactContent(ctx context.Context, p authz.Principal, id, version int) (*domain.Artifact, *domain.ArtifactVersion, error) {
	a, _, err := s.GetArtifact(ctx, p, id)
	if err != nil {
		return nil, nil, err
	}
	v, err := s.Repo.GetArtifactVersion(ctx, id, version)
	if err != nil {
		return nil, nil, authz.NotFound("%s", err.Error())
	}
	return a, v, nil
}

// ListArtifacts lists a project's artifacts (or one ticket's) for its members.
func (s *Service) ListArtifacts(ctx context.Context, p authz.Principal, projectID int, taskID *int) ([]domain.Artifact, error) {
	if _, err := s.Authz.RequireProject(ctx, p, projectID, authz.Member); err != nil {
		return nil, err
	}
	out, err := s.Repo.ListArtifacts(ctx, projectID, taskID)
	for i := range out {
		s.withURL(&out[i])
	}
	return out, err
}

// ReviewArtifact is a person's verdict on an artifact's current version:
// approve it, or ask for changes (with what to change). Agent accounts can't
// review: like plan approval, it's a person's call. On a ticket, the reviewer
// is someone who can plan it (its owner or creator, or a lead or project
// owner); without one, any member.
// If the ticket is blocked on a question (the agent asking for review), the
// verdict answers it, so the ticket goes back to ready for the next dispatch.
func (s *Service) ReviewArtifact(ctx context.Context, p authz.Principal, id, version int, approve bool, note string) (*domain.Artifact, error) {
	if p.Agent {
		return nil, authz.Forbidden("agent accounts can't review artifacts; a person approves or asks for changes")
	}
	note = strings.TrimSpace(note)
	if !approve && note == "" {
		return nil, authz.BadRequest("say what to change")
	}
	a, _, err := s.GetArtifact(ctx, p, id)
	if err != nil {
		return nil, err
	}
	if version == 0 {
		version = a.CurrentVersion
	}
	if version != a.CurrentVersion {
		return nil, authz.BadRequest("version %d isn't the latest (v%d is); review the latest version", version, a.CurrentVersion)
	}
	var task *domain.Task
	if a.TaskID != nil {
		t, _, err := s.LoadTask(ctx, p, *a.TaskID, authz.TaskPlan)
		if err != nil {
			return nil, err
		}
		task = t
	}
	status, verb := domain.ArtifactApproved, "Approved"
	if !approve {
		status, verb = domain.ArtifactChangesRequested, "Changes requested on"
	}
	a, err = s.Repo.ReviewArtifact(ctx, id, version, status, p.UserRef())
	if err != nil {
		return nil, err
	}
	s.withURL(a)
	s.audit(ctx, p, domain.AuditEntry{Action: "artifact." + map[bool]string{true: "approve", false: "request_changes"}[approve],
		ProjectID: intRef(a.ProjectID), TargetType: "artifact", TargetID: idStr(a.ID), Summary: fmt.Sprintf("%s %q v%d", strings.ToLower(verb), a.Title, version)})
	if task != nil {
		body := fmt.Sprintf("%s artifact #%d %q v%d.", verb, a.ID, a.Title, version)
		if note != "" {
			body += "\n\n" + note
		}
		if task.State == domain.TaskStateBlocked && task.BlockReason != "" && !strings.HasPrefix(task.BlockReason, "scheduled:") {
			if _, err := s.answer(ctx, p, task, body); err != nil {
				return a, err
			}
		} else {
			s.comment(a.ProjectID, task.ID, p, body)
		}
	}
	return a, nil
}

// ---- Retention ----

func (s *Service) artifactRetention() time.Duration {
	if s.ArtifactRetention > 0 {
		return s.ArtifactRetention
	}
	return domain.DefaultArtifactRetentionDays * 24 * time.Hour
}

func (s *Service) artifactExpiry() time.Time { return time.Now().Add(s.artifactRetention()) }

// KeepArtifact keeps an artifact so it never expires, or (keep false) lets it
// expire again, no sooner than a full retention period from now. A person on
// the project decides; agent accounts can't, and agents should only do it when
// their person asks.
func (s *Service) KeepArtifact(ctx context.Context, p authz.Principal, id int, keep bool) (*domain.Artifact, error) {
	if p.Agent {
		return nil, authz.Forbidden("agent accounts can't keep artifacts; a person decides what to keep")
	}
	a, _, err := s.GetArtifact(ctx, p, id)
	if err != nil {
		return nil, err
	}
	a, err = s.Repo.KeepArtifact(ctx, id, keep, p.UserRef(), s.artifactExpiry())
	if err != nil {
		return nil, err
	}
	verb := map[bool]string{true: "keep", false: "unkeep"}[keep]
	s.audit(ctx, p, domain.AuditEntry{Action: "artifact." + verb, ProjectID: intRef(a.ProjectID), TargetType: "artifact",
		TargetID: idStr(a.ID), Summary: fmt.Sprintf("%s %q", map[bool]string{true: "kept", false: "stopped keeping"}[keep], a.Title)})
	return s.withURL(a), nil
}

// LetArtifactExpire dismisses the expiry warning: the person saw it and is
// fine with the artifact going.
func (s *Service) LetArtifactExpire(ctx context.Context, p authz.Principal, id int) (*domain.Artifact, error) {
	a, _, err := s.GetArtifact(ctx, p, id)
	if err != nil {
		return nil, err
	}
	if err := s.Repo.DismissArtifactExpiry(ctx, id); err != nil {
		return nil, err
	}
	a, err = s.Repo.GetArtifact(ctx, id)
	return s.withURL(a), err
}

// ExpiringArtifacts lists the caller's artifacts (created by them, or on
// their tickets) that expire within `within` and that nobody kept: what the
// home page warns about.
func (s *Service) ExpiringArtifacts(ctx context.Context, p authz.Principal, within time.Duration) ([]domain.Artifact, error) {
	if !p.HasUser() {
		return []domain.Artifact{}, nil
	}
	list, err := s.Repo.ExpiringArtifactsFor(ctx, p.UserID, time.Now().Add(within))
	if err != nil {
		return nil, err
	}
	out := make([]domain.Artifact, 0, len(list))
	for i := range list {
		// Only projects they can still see.
		if lvl, err := s.Authz.ProjectLevel(ctx, p, list[i].ProjectID); err == nil && lvl > authz.None {
			out = append(out, *s.withURL(&list[i]))
		}
	}
	return out, nil
}

// PurgeExpiredArtifacts deletes expired artifacts nobody kept, and notes each
// on its ticket. Run it periodically; it's safe from several replicas.
func (s *Service) PurgeExpiredArtifacts(ctx context.Context) (int, error) {
	gone, err := s.Repo.DeleteExpiredArtifacts(ctx, time.Now())
	if err != nil {
		return 0, err
	}
	amp := authz.Principal{Email: "AMP"}
	for _, g := range gone {
		slog.Info("artifact expired", "artifact_id", g.ID, "project_id", g.ProjectID, "title", g.Title)
		s.audit(ctx, amp, domain.AuditEntry{Action: "artifact.expire", ProjectID: intRef(g.ProjectID), TargetType: "artifact",
			TargetID: idStr(g.ID), Summary: fmt.Sprintf("deleted %q: it expired and nobody kept it", g.Title)})
		if g.TaskID != nil {
			s.comment(g.ProjectID, *g.TaskID, amp, fmt.Sprintf("Artifact #%d %q expired and was deleted: nobody chose to keep it.", g.ID, g.Title))
		}
	}
	return len(gone), nil
}

// RunArtifactPurge purges expired artifacts every interval until ctx ends.
func (s *Service) RunArtifactPurge(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if n, err := s.PurgeExpiredArtifacts(ctx); err != nil {
			slog.Warn("artifact purge failed", "err", err)
		} else if n > 0 {
			slog.Info("artifact purge", "deleted", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// ---- Views and activity ----

// artifactViewWindow de-duplicates views: one entry per person, artifact and
// version in this window, however often they reload.
const artifactViewWindow = 30 * time.Minute

// RecordArtifactView audits that p looked at an artifact version (via is
// "viewer", "open" or "mcp"). Failures are logged, never the caller's problem.
func (s *Service) RecordArtifactView(ctx context.Context, p authz.Principal, a *domain.Artifact, version int, via string) {
	if version == 0 {
		version = a.CurrentVersion
	}
	target := fmt.Sprintf("%d", a.ID)
	seen, err := s.Repo.RecentAudit(ctx, "artifact.view", "artifact", target, p.UserRef(), p.Email, time.Now().Add(-artifactViewWindow))
	if err != nil {
		slog.Warn("artifact view audit", "err", err)
		return
	}
	if seen {
		return
	}
	s.audit(ctx, p, domain.AuditEntry{Action: "artifact.view", ProjectID: intRef(a.ProjectID), TargetType: "artifact", TargetID: target,
		Summary: fmt.Sprintf("viewed %q v%d", a.Title, version), Details: map[string]any{"version": version, "via": via}})
}

// ArtifactActivity is the audit trail of one artifact (published, reviewed,
// kept, viewed, expired). Its creator, its ticket's owner, and project leads
// and above may read it; other members can see the artifact, not who looked.
func (s *Service) ArtifactActivity(ctx context.Context, p authz.Principal, id int) ([]domain.AuditEntry, error) {
	a, _, err := s.GetArtifact(ctx, p, id)
	if err != nil {
		return nil, err
	}
	lvl, err := s.Authz.ProjectLevel(ctx, p, a.ProjectID)
	if err != nil {
		return nil, err
	}
	allowed := lvl >= authz.Lead || (a.CreatedBy != nil && p.UserID != 0 && *a.CreatedBy == p.UserID)
	if !allowed && a.TaskID != nil {
		if t, err := s.Repo.GetTask(ctx, *a.TaskID); err == nil && t.OwnerUserID != nil && *t.OwnerUserID == p.UserID && p.UserID != 0 {
			allowed = true
		}
	}
	if !allowed {
		return nil, authz.Forbidden("only the artifact's creator, its ticket's owner, and project leads and owners can see its activity")
	}
	return s.Repo.TargetAudit(ctx, "artifact", fmt.Sprintf("%d", id), 200)
}
