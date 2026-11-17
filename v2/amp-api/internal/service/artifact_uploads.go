package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/simstech/amp-api/internal/auth"
	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/receipts"
	"github.com/simstech/amp-api/internal/repository"
)

// ArtifactUploadPath is the artifact upload route's prefix; the token follows it.
const ArtifactUploadPath = "/api/artifact-uploads/"

// artifactUploadStaleClaim is how long a claimed upload that never finished
// (its server died mid-request) blocks a retry of the same URL.
const artifactUploadStaleClaim = 2 * time.Minute

// ArtifactUpload is a minted one-time URL that publishes the file PUT to it.
type ArtifactUpload struct {
	URL       string    `json:"url"`
	Method    string    `json:"method"`
	Bytes     int64     `json:"bytes"`
	SHA256    string    `json:"sha256"`
	ExpiresAt time.Time `json:"expires_at"`
}

// uploadCaller is the minting principal as stored with the upload. Request
// metadata (Req) is left out: the upload's own request has its own.
type uploadCaller struct {
	UserID      int    `json:"user_id"`
	Email       string `json:"email"`
	Super       bool   `json:"super,omitempty"`
	Agent       bool   `json:"agent,omitempty"`
	ViaToken    bool   `json:"via_token,omitempty"`
	Scope       string `json:"scope,omitempty"`
	ScopeTeamID int    `json:"scope_team_id,omitempty"`
	Channel     string `json:"channel,omitempty"`
	AuthMethod  string `json:"auth_method,omitempty"`
	TokenID     int64  `json:"token_id,omitempty"`
	SessionID   string `json:"session_id,omitempty"`
	ManagedBy   *int   `json:"managed_by,omitempty"`
}

func callerOf(p authz.Principal) uploadCaller {
	return uploadCaller{UserID: p.UserID, Email: p.Email, Super: p.Super, Agent: p.Agent, ViaToken: p.ViaToken, Scope: p.Scope,
		ScopeTeamID: p.ScopeTeamID, Channel: p.Channel, AuthMethod: p.AuthMethod, TokenID: p.TokenID, SessionID: p.SessionID,
		ManagedBy: p.ManagedBy}
}

func (c uploadCaller) principal() authz.Principal {
	return authz.Principal{UserID: c.UserID, Email: c.Email, Super: c.Super, Agent: c.Agent, ViaToken: c.ViaToken, Scope: c.Scope,
		ScopeTeamID: c.ScopeTeamID, Channel: c.Channel, AuthMethod: c.AuthMethod, TokenID: c.TokenID, SessionID: c.SessionID,
		ManagedBy: c.ManagedBy}
}

type uploadRequest struct {
	ArtifactID int    `json:"artifact_id,omitempty"`
	ProjectID  int    `json:"project_id,omitempty"`
	TaskID     *int   `json:"task_id,omitempty"`
	Title      string `json:"title,omitempty"`
	Kind       string `json:"kind"`
	Note       string `json:"note,omitempty"`
}

var artifactKinds = map[string]bool{domain.ArtifactHTML: true, domain.ArtifactSVG: true, domain.ArtifactImage: true,
	domain.ArtifactMarkdown: true, domain.ArtifactPDF: true, domain.ArtifactText: true}

// BeginArtifactUpload checks a publish as p without its content and mints a
// one-time URL for it: the file PUT there (exactly bytes long, with this
// sha256) is published as p, so an agent publishes a large local file
// without re-typing it into a tool call. Nothing is published until then.
func (s *Service) BeginArtifactUpload(ctx context.Context, p authz.Principal, req PublishArtifactRequest, bytes int64, sum string) (*ArtifactUpload, error) {
	sum = strings.ToLower(strings.TrimSpace(sum))
	if bytes <= 0 || bytes > domain.MaxArtifactBytes {
		return nil, authz.BadRequest("bytes must be 1 to %d (10 MB): the file's exact size (wc -c < file)", domain.MaxArtifactBytes)
	}
	if !sha256Re.MatchString(sum) {
		return nil, authz.BadRequest("sha256 must be 64 hex digits: the file's (shasum -a 256 file)")
	}
	if _, err := s.prepareArtifactPublish(ctx, p, &req); err != nil {
		return nil, err
	}
	if !artifactKinds[req.Kind] {
		return nil, authz.BadRequest("kind must be one of html, svg, image, markdown, pdf, text")
	}
	caller, err := json.Marshal(callerOf(p))
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(uploadRequest{ArtifactID: req.ArtifactID, ProjectID: req.ProjectID, TaskID: req.TaskID,
		Title: req.Title, Kind: req.Kind, Note: req.Note})
	if err != nil {
		return nil, err
	}
	token, hash, err := receipts.NewUploadToken()
	if err != nil {
		return nil, err
	}
	expires := time.Now().Add(receipts.UploadTokenTTL).UTC().Truncate(time.Second)
	if err := s.Repo.PutArtifactUpload(ctx, hash, p.UserRef(), caller, body, bytes, sum, expires); err != nil {
		return nil, err
	}
	return &ArtifactUpload{URL: strings.TrimRight(s.AppURL, "/") + ArtifactUploadPath + token, Method: "PUT",
		Bytes: bytes, SHA256: sum, ExpiresAt: expires}, nil
}

// StoreArtifactUpload publishes the file PUT to a one-time artifact upload
// URL (no session: the token is the credential, and the publish is
// re-authorized as the caller who minted it). Unknown token: 404. Expired:
// 410. Another upload publishing with it: 409. Length or sha256 not as
// declared: 422 and the URL stays usable. Already used: the artifact it
// published, so a retry after a lost response doesn't add a version.
func (s *Service) StoreArtifactUpload(ctx context.Context, token string, body io.Reader) (*domain.Artifact, error) {
	if !receipts.ValidTokenShape(token) {
		return nil, authz.NotFound("upload URL not found")
	}
	hash := receipts.HashToken(token)
	cur, err := s.Repo.ArtifactUploadByToken(ctx, hash)
	if err != nil {
		return nil, err
	}
	if cur == nil || cur.DoneAt != nil || time.Now().After(cur.ExpiresAt) ||
		(cur.ClaimedAt != nil && time.Since(*cur.ClaimedAt) < artifactUploadStaleClaim) {
		return s.unclaimableUpload(ctx, cur)
	}

	// The body is read before claiming: a slow upload could otherwise
	// outlive its claim, letting a second PUT take it over and publish too.
	content, err := io.ReadAll(io.LimitReader(body, cur.Bytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(content)) != cur.Bytes {
		return nil, &authz.Error{Status: 422, Msg: fmt.Sprintf("the upload is %s bytes, but %d were declared; nothing was published",
			sizeOf(len(content), cur.Bytes), cur.Bytes)}
	}
	got := sha256.Sum256(content)
	if hex.EncodeToString(got[:]) != cur.SHA256 {
		return nil, &authz.Error{Status: 422, Msg: "the upload's sha256 doesn't match the one declared; nothing was published"}
	}

	row, err := s.Repo.ClaimArtifactUpload(ctx, hash, artifactUploadStaleClaim)
	if err != nil {
		return nil, err
	}
	if row == nil {
		cur, err := s.Repo.ArtifactUploadByToken(ctx, hash)
		if err != nil {
			return nil, err
		}
		return s.unclaimableUpload(ctx, cur)
	}
	done := false
	defer func() {
		if done {
			return
		}
		if err := s.Repo.ReleaseArtifactUpload(context.WithoutCancel(ctx), row.ID, *row.ClaimedAt); err != nil {
			slog.Warn("artifact upload: release the claim", "id", row.ID, "err", err)
		}
	}()

	var c uploadCaller
	var r uploadRequest
	if err := json.Unmarshal(row.Caller, &c); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(row.Request, &r); err != nil {
		return nil, err
	}
	p, err := s.uploadPrincipal(ctx, c)
	if err != nil {
		return nil, err
	}
	a, err := s.PublishArtifact(ctx, p, PublishArtifactRequest{ArtifactID: r.ArtifactID, ProjectID: r.ProjectID,
		TaskID: r.TaskID, Title: r.Title, Kind: r.Kind, Note: r.Note, Content: content})
	if err != nil {
		return nil, err
	}
	// Published already; a retry would add a second version, so the claim is
	// kept rather than released even when marking it used fails.
	done = true
	if ok, err := s.Repo.FinishArtifactUpload(ctx, row.ID, *row.ClaimedAt, a.ID, a.CurrentVersion); err != nil || !ok {
		slog.Warn("artifact upload: mark it used", "id", row.ID, "artifact", a.ID, "claim_held", ok, "err", err)
	}
	return a, nil
}

// uploadPrincipal is who the upload publishes as now. The route skips the
// auth middleware, so its checks run here: the account must still exist and
// be active, and the API token the URL was minted with must still be live.
// Super and the rest of the identity come from the current user row; only
// the credential's scope and channel are kept from the mint.
func (s *Service) uploadPrincipal(ctx context.Context, c uploadCaller) (authz.Principal, error) {
	p := c.principal()
	if c.UserID == 0 {
		// Only auth-off local dev mints without a user.
		if c.AuthMethod == auth.AuthLocalDev {
			return p, nil
		}
		return authz.Principal{}, authz.Forbidden("this upload URL has no AMP user; nothing was published")
	}
	u, err := s.Repo.GetUserByID(ctx, c.UserID)
	if errors.Is(err, repository.ErrNotFound) {
		return authz.Principal{}, authz.Forbidden("the account that made this upload URL no longer exists; nothing was published")
	}
	if err != nil {
		return authz.Principal{}, err
	}
	if u.DisabledAt != nil {
		return authz.Principal{}, authz.Forbidden("the account that made this upload URL is deactivated; nothing was published")
	}
	if c.TokenID != 0 {
		tok, err := s.Repo.GetAPIToken(ctx, int(c.TokenID))
		if err != nil && !errors.Is(err, repository.ErrNotFound) {
			return authz.Principal{}, err
		}
		if err != nil || tok.UserID != u.ID || tok.RevokedAt != nil || (tok.ExpiresAt != nil && !time.Now().Before(*tok.ExpiresAt)) {
			return authz.Principal{}, authz.Forbidden("the API token this upload URL was made with is revoked or expired; nothing was published")
		}
	}
	roles, err := s.Repo.UserRoles(ctx, u.ID)
	if err != nil {
		return authz.Principal{}, err
	}
	u.Roles = roles
	p.Email, p.Agent, p.ManagedBy = u.Email, u.IsAgent(), u.ManagedBy
	// Local dev's Super is the auth-off mode's (authz.FromContext), not a role.
	p.Super = c.AuthMethod == auth.AuthLocalDev || (u.HasRole(domain.RoleAdmin) && !u.IsAgent())
	return p, nil
}

// unclaimableUpload answers a PUT to an upload URL that can't be claimed now,
// from its row (nil when the token is unknown).
func (s *Service) unclaimableUpload(ctx context.Context, cur *repository.ArtifactUploadRow) (*domain.Artifact, error) {
	switch {
	case cur == nil:
		return nil, authz.NotFound("upload URL not found: call amp_publish_artifact again for a new one")
	case cur.DoneAt != nil && cur.ArtifactID != nil:
		a, err := s.Repo.GetArtifact(ctx, *cur.ArtifactID)
		if err != nil {
			return nil, authz.NotFound("upload URL already used, and its artifact %d is gone", *cur.ArtifactID)
		}
		return s.withURL(a), nil
	case time.Now().After(cur.ExpiresAt):
		return nil, &authz.Error{Status: 410, Msg: "upload URL expired: call amp_publish_artifact again for a new one"}
	default:
		return nil, &authz.Error{Status: 409, Msg: "another request is uploading to this URL"}
	}
}

func sizeOf(n int, declared int64) string {
	if int64(n) > declared {
		return "more than " + fmt.Sprint(declared)
	}
	return fmt.Sprint(n)
}
