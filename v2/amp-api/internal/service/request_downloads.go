package service

// Team request attachment downloads: the one gate that decides whether a
// requester's private file may be handed to a caller, and the short-lived grant
// that carries that decision to the byte-serving handler (epic 202, story 549,
// AMP #3963; approved product contract artifact 36 v2 §1 and decision D3; build
// slice G3 of discoveries/team-inbox-approved-build-slices.md, the serving half).
//
// Three layers already exist and this file is the seam above them:
//
//   - domain/request_attachment.go owns the numbers, the scan vocabulary and the
//     declared hold, and — importantly — the one download decision:
//     RequestAttachmentDownloadRefusal, which is infected → pending → held →
//     purged. This file CALLS it and does not re-implement it, because a second
//     copy of "may these bytes be served" is how a mint and a serve end up
//     disagreeing.
//   - repository/request_attachments.go owns the rows, and LoadRequestAttachment
//     returns the whole record with bytes so the gate decides on the row it is
//     about to serve.
//   - repository/request_download_grants.go (migration 063) owns the grant ledger:
//     one row per minted URL, single-use by an atomic UPDATE, revocable.
//
// The four questions this file answers, and the rule for each:
//
//  1. **Who may download an attachment?** The person the request was submitted by
//     (their own request's files, always — being the requester, not having uploaded,
//     is the privilege) and a member of the inbox's team (which includes its lead and
//     an owner/admin of its org). Everyone else is authz.RequestNotFound: a foreign
//     attachment id and an unknown one must be the same bytes of answer, or ids become
//     an existence oracle. That is the same two-value RequestDownloadCapability shape
//     the upload path uses (RequestUploadCapabilityFor), for the same reason.
//
//     The restricted-file hold is a STATE (question 2), not a per-caller permission.
//     A held archive or executable is refused to *everyone* until a team lead has
//     checked and released it — the release act is #3964's, and it is the lead-only,
//     audited authority the contract names ("a team lead can release after checking").
//     Once `released_at` is set the hold is gone and every reader the request's view
//     admits may have the file, which is what a release means. Deciding it here per
//     caller instead would create a second, weaker gate that could disagree with the
//     release.
//
//  2. **Which files may be served at all?** Only the ones
//     domain.RequestAttachmentDownloadRefusal serves: not infected, not pending,
//     not held-for-release, not purged. "Unsupported" (no scanner exists in v1) is
//     served, honestly labelled — it is not clean and nothing here says it is. The
//     scan state is read from the row on every mint and every serve, never cached.
//
//  3. **What does a URL grant?** One file, for five minutes, to one person, once.
//     The token is 32 random bytes; only its sha256 is stored, so the ledger holds
//     no usable credential. There is no request-wide, team-wide or wildcard grant:
//     the row names an attachment and nothing else.
//
//  4. **What keeps it honest after the mint?** Every serve re-checks the person
//     against the rows: the grant's own recipient must be the authenticated caller
//     (possession is not enough), the request's current view must still admit them
//     (a requester whose access was removed, a team member who left the team), the
//     attachment must still exist, and the state gate must still say served. A
//     revoked grant is a deleted row and fails the first lookup; an expired one
//     fails the consume's WHERE clause. Nothing is cached, so all of that takes
//     effect on the next click.
//
// What this file deliberately does NOT do:
//
//   - No public URLs and no bypass of the auth proxy. A mint needs an
//     authenticated principal with a row in this service's own records, and a
//     serve needs an authenticated principal too — the URL is an additional
//     capability, never a substitute for being signed in. There is no "anonymous
//     download" path anywhere in this file, and no AWS-style presigned primitive
//     to reach for.
//   - No retention, no keep, no release. A release (writing released_at) is
//     #3964's audited lead-only act; this gate reads the hold it produces. A purge
//     deletes the row and the grant's FK cascade takes its grants with it.
//   - No bytes in the response object. The mint returns a URL fragment and the
//     metadata a panel shows; the serve returns the record and the bytes for the
//     handler to write, and nothing here writes a header.
//   - No token in the audit log or in any log line. The audit rows name the
//     request, the attachment and ids — never the token, never the hash, never the
//     URL.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/repository"
)

// RequestDownloadCapability is who may read one request's attachment. Two values,
// not a ladder, exactly like RequestUploadCapability: the requester reads their
// own request's files, a team member reads the inbox's files, and neither is a
// weaker form of the other. The held-file rule below is a further condition on the
// team arm, not a third capability.
type RequestDownloadCapability int

const (
	// RequestDownloadNone: no standing. Reported as the request surface's single
	// not-found, never a 403 — a caller who may not read a file must not learn
	// that it exists.
	RequestDownloadNone RequestDownloadCapability = iota
	// RequestDownloadRequester: the person the request was submitted by.
	RequestDownloadRequester
	// RequestDownloadTeam: a member of the inbox's team (authz.InboxMember, which
	// includes its lead and an owner/admin of its org).
	RequestDownloadTeam
)

func (c RequestDownloadCapability) String() string {
	switch c {
	case RequestDownloadRequester:
		return "requester"
	case RequestDownloadTeam:
		return "team"
	}
	return "none"
}

// RequestDownloadCapabilityFor decides, purely, whether p may download a file on r
// given p's standing role on r's team. It is the read counterpart of
// RequestUploadCapabilityFor and deliberately the same shape: the team wins when a
// caller is both (a triager's own submission is still the inbox's file), and "their
// own" is the durable requester column rather than an audit field, so a
// connector-submitted request belongs to nobody's personal view.
func RequestDownloadCapabilityFor(r *domain.TeamRequest, p authz.Principal, role authz.InboxRole) RequestDownloadCapability {
	if r == nil {
		return RequestDownloadNone
	}
	if role >= authz.InboxMember {
		return RequestDownloadTeam
	}
	if p.HasUser() && r.RequesterUserID != nil && *r.RequesterUserID == p.UserID {
		return RequestDownloadRequester
	}
	return RequestDownloadNone
}

// RequestDownloadGrant is one issued download URL's server-side half: the opaque
// token a panel puts in a link, and when it dies. The token is the only thing a
// caller may hand out; the attachment and request ids are included for the panel's
// own bookkeeping but are never needed to use the URL.
type RequestDownloadURL struct {
	// Token goes in the URL. It is returned once, to the caller that minted it,
	// and then only its hash exists anywhere (including in this process).
	Token string `json:"token"`
	// URL is the path a client follows, relative to AMP's own origin (/api/...):
	// a download goes through AMP's authenticated surface, never a storage
	// endpoint and never a public bucket address.
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`

	AttachmentID int64  `json:"attachment_id"`
	RequestID    int    `json:"request_id"`
	FileName     string `json:"file_name"`
	ContentType  string `json:"content_type"`
	SizeBytes    int64  `json:"size_bytes"`
}

// RequestTeamOrg loads a team and returns it, for a caller that has only a team id from
// a route and needs its org to name the tenant. It is a plain read with no authority of
// its own: the tenant-scoped request read that follows is what refuses a pair that does
// not exist, so this cannot be used to probe another org's teams — a team that is not
// there is the request surface's not-found, exactly like a request that is not.
func (s *Service) RequestTeamOrg(ctx context.Context, teamID int) (*domain.Team, error) {
	if teamID <= 0 {
		return nil, authz.RequestNotFound()
	}
	team, err := s.Repo.GetTeam(ctx, teamID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, authz.RequestNotFound()
		}
		return nil, fmt.Errorf("load team %d: %w", teamID, err)
	}
	return team, nil
}

// RequireRequestAttachmentForDownload loads one attachment and decides whether p may
// download it, returning the metadata (no bytes).
//
// The order is deliberate: the request is loaded inside its tenant first, so an
// unknown request and a foreign one answer identically; then the caller's standing
// on the team is resolved from the rows on this call; then the capability decides.
// Only after all three does the attachment get loaded — so an unauthorized caller
// never causes a read of another tenant's file row, and the refusal cannot leak
// whether the attachment id existed.
func (s *Service) RequireRequestAttachmentForDownload(ctx context.Context, p authz.Principal, ref repository.RequestAttachmentRef) (*domain.TeamRequest, *domain.RequestAttachment, error) {
	return s.requireRequestAttachment(ctx, p, ref, false)
}

// requireRequestAttachment is the shared body of the mint, the serve and the
// requester-safe panel read. withBytes selects LoadRequestAttachment (the only path
// to content) over GetRequestAttachment (metadata only), so a panel never loads
// 25 MiB to render a name.
func (s *Service) requireRequestAttachment(ctx context.Context, p authz.Principal, ref repository.RequestAttachmentRef, withBytes bool) (*domain.TeamRequest, *domain.RequestAttachment, error) {
	if err := s.requireNotSubmitScoped(p, "download request attachments"); err != nil {
		return nil, nil, err
	}
	req, err := s.Repo.GetRequest(ctx, ref.RequestID, ref.OrgID, ref.TeamID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, nil, authz.RequestNotFound()
		}
		return nil, nil, fmt.Errorf("load request %d for a download: %w", ref.RequestID, err)
	}
	role, err := s.Authz.InboxRole(ctx, p, req.TeamID)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve standing on team %d for a download: %w", req.TeamID, err)
	}
	view, err := s.Authz.RequestAccess(ctx, p, req)
	if err != nil {
		return nil, nil, err
	}
	capability := RequestDownloadCapabilityFor(req, p, role)
	if capability == RequestDownloadNone && !view.Own() {
		// The one not-found, for a foreign attachment exactly as for an unknown
		// request: no id, no team, no state.
		return nil, nil, authz.RequestNotFound()
	}

	var att *domain.RequestAttachment
	if withBytes {
		att, err = s.Repo.LoadRequestAttachment(ctx, ref)
	} else {
		att, err = s.Repo.GetRequestAttachment(ctx, ref)
	}
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, nil, authz.RequestNotFound()
		}
		if errors.Is(err, repository.ErrRequestTenantScope) {
			return nil, nil, err
		}
		return nil, nil, fmt.Errorf("load attachment %d of request %d: %w", ref.AttachmentID, ref.RequestID, err)
	}

	return req, att, nil
}

// RequestAttachmentDownloadState is the *claim* a gate makes about a file for a
// surface that is listing it rather than serving it: ready, or the reason it is
// not. It exists so a panel's "Scanning"/"Waiting for a lead" wording comes from the
// same function that would refuse the download, instead of a second opinion.
type RequestAttachmentDownloadState struct {
	// Ready is true exactly when domain.RequestAttachmentDownloadRefusal serves the
	// file. It says nothing about the caller — that is the capability's answer, and a
	// surface combines the two.
	Ready bool `json:"ready"`
	// Refusal is the domain's word for why not ("scan_pending", "held_for_release",
	// "scan_infected", "purged"), empty when Ready.
	Refusal domain.RequestAttachmentRefusal `json:"refusal,omitempty"`
	// Held reports the derived restricted-file hold, whether or not it refused:
	// a file waiting for a lead is shown as such even while "held" is its reason.
	Held bool `json:"held"`
}

// RequestAttachmentDownloadStateAt is the state a listing shows for one file, and the
// same function the mint and the serve gate on. It is pure over (attachment, now) and
// does not consult authorization: it answers "may these bytes ever be served", while
// the capability answers "may this caller have them". The two are combined at the
// surface, where both are known.
func RequestAttachmentDownloadStateAt(att *domain.RequestAttachment, now time.Time) RequestAttachmentDownloadState {
	refusal := domain.RequestAttachmentDownloadRefusal(att, now)
	return RequestAttachmentDownloadState{
		Ready:   refusal == domain.RequestAttachmentServed,
		Refusal: refusal,
		Held:    att.Held(),
	}
}

// RequestDownloadGrantInput is one mint: which attachment, in which tenant.
type RequestDownloadGrantInput struct {
	RequestID    int
	AttachmentID int64
	OrgID        int
	TeamID       int
}

// CreateRequestDownloadGrant authorizes a download and mints a short-lived,
// single-use URL for it. It is the only mint in the product.
//
// It authorizes by calling the same requireRequestAttachment the serve path calls,
// so a URL can only be minted for a caller who could have the bytes anyway, and it
// mints for one attachment only — there is no input field for a request, a team or a
// set, and no code path that loops. The scan/hold gate is checked here so a URL is
// never minted for a file that may not be served (a pending, infected or unreleased
// file); it is checked again at the serve, because the row can change in between
// (a scan verdict arriving, a purge), and the serve is the moment that matters.
//
// The token is 32 random bytes from crypto/rand, base64url without padding (the
// receipt-upload token shape), and only its sha256 is stored.
func (s *Service) CreateRequestDownloadGrant(ctx context.Context, p authz.Principal, in RequestDownloadGrantInput) (*RequestDownloadURL, error) {
	ref := repository.RequestAttachmentRef{AttachmentID: in.AttachmentID, RequestID: in.RequestID, OrgID: in.OrgID, TeamID: in.TeamID}
	req, att, err := s.requireRequestAttachment(ctx, p, ref, false)
	if err != nil {
		return nil, err
	}
	now := s.now()
	if state := RequestAttachmentDownloadStateAt(att, now); !state.Ready {
		return nil, requestDownloadRefusal(att, state.Refusal)
	}

	token, grantHash, err := newDownloadGrantToken()
	if err != nil {
		return nil, fmt.Errorf("mint a download grant: %w", err)
	}
	row, err := s.Repo.InsertRequestDownloadGrant(ctx, domain.RequestDownloadGrant{
		GrantHash:    grantHash,
		AttachmentID: att.ID,
		RequestID:    req.ID,
		OrgID:        req.OrgID,
		TeamID:       req.TeamID,
		UserID:       p.UserID,
		IssuedBy:     p.UserRef(),
		IssuedAt:     now.UTC().Truncate(time.Second),
		ExpiresAt:    now.UTC().Add(domain.RequestDownloadGrantTTL).Truncate(time.Second),
	})
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			// The attachment was deleted between the decision and the write.
			return nil, authz.RequestNotFound()
		}
		return nil, fmt.Errorf("mint a download grant for attachment %d: %w", att.ID, err)
	}

	// The audit row records that a URL was issued and for which file — never the
	// token, never its hash, never the URL. A log that held a live credential would
	// be a download path of its own.
	s.audit(ctx, p, domain.AuditEntry{
		Action:     "request.attachment.download_grant",
		OrgID:      intRef(req.OrgID),
		TargetType: "request",
		TargetID:   idStr(req.ID),
		TargetName: req.PublicRef,
		Summary:    fmt.Sprintf("issued a download link for %s on request %s", att.FileName, req.PublicRef),
		Details: map[string]any{
			"request_id":    req.ID,
			"attachment_id": att.ID,
			"content_type":  att.ContentType,
			"scan_state":    string(att.ScanState),
			"held":          att.Held(),
			"expires_at":    row.ExpiresAt.UTC().Format(time.RFC3339),
		},
	})

	return &RequestDownloadURL{
		Token:        token,
		URL:          RequestDownloadGrantPath(token),
		ExpiresAt:    row.ExpiresAt,
		AttachmentID: att.ID,
		RequestID:    req.ID,
		FileName:     att.FileName,
		ContentType:  att.ContentType,
		SizeBytes:    att.SizeBytes,
	}, nil
}

// RequestDownloadGrantPath is the path a minted token is used at. It lives here so
// the mint and the handler cannot disagree about the route, and so the token's only
// mention in a URL is written once. It is a path, not an absolute URL: a download
// goes to AMP's own origin (APP_PUBLIC_URL + this), through the same auth proxy as
// every other route, never to a storage endpoint.
func RequestDownloadGrantPath(token string) string { return RequestDownloadGrantPrefix + token }

// RequestDownloadGrantPrefix is the route prefix of the serve handler.
const RequestDownloadGrantPrefix = "/api/request-downloads/"

// RequestDownloadGrantToken is the token part of a request-download path, or "" when
// the path is not one. Exported so the handler and its tests parse a path exactly one
// way — the prefix is written once, here, beside the mint that produces it.
func RequestDownloadGrantToken(path string) string {
	if !strings.HasPrefix(path, RequestDownloadGrantPrefix) {
		return ""
	}
	return strings.TrimPrefix(path, RequestDownloadGrantPrefix)
}

// ServeRequestDownload is the serve half: it takes a grant token and the caller's
// principal and returns the bytes to write, or the refusal to report.
//
// The checks, in order, and why each is here rather than at the mint:
//
//  1. The grant must exist. A revoked grant is deleted and an unknown token was
//     never issued; both are the same 404.
//  2. It must belong to this caller. Possession is not authorization: the URL is a
//     bearer credential, so a leaked link is only useful to the person it was minted
//     for. This is the check that makes "no token in logs" a mitigation rather than
//     the only defence.
//  3. It must not have expired — the atomic consume enforces this, and the check
//     here gives the honest answer before anything is loaded.
//  4. The attachment must still exist, and the request's current view must still
//     admit the caller. This is the auth recheck: the *same* authorization the mint
//     used, re-run against today's rows, so a requester whose request was reassigned,
//     a team member whose membership ended, or a file that became held all refuse a
//     URL that was minted while they were allowed.
//  5. The state gate must still say served. A verdict that arrived, a hold that was
//     set, or a purge that stamped an expiry refuses the download even though the URL
//     is otherwise valid.
//  6. Only then is the grant spent (atomically) and the bytes returned.
//
// Ordering 4 and 5 before 6 matters: a failed recheck must not burn the grant, so a
// caller who is refused for a transient reason can retry. It also means a revoked or
// expired grant is refused *before* any row of bytes is read.
func (s *Service) ServeRequestDownload(ctx context.Context, p authz.Principal, token string) (*domain.TeamRequest, *domain.RequestAttachment, error) {
	if err := s.requireNotSubmitScoped(p, "download request attachments"); err != nil {
		return nil, nil, err
	}
	grant, err := s.Repo.GetRequestDownloadGrant(ctx, hashDownloadGrantToken(token))
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, nil, authz.RequestNotFound()
		}
		return nil, nil, fmt.Errorf("load download grant: %w", err)
	}
	now := s.now()
	if grant.UserID != p.UserID || p.UserID == 0 {
		// Wrong person (or a grant whose person is gone): the same not-found, so a
		// leaked URL does not tell a stranger it was valid.
		return nil, nil, authz.RequestNotFound()
	}
	if !grant.Active(now) {
		return nil, nil, requestDownloadGrantGone()
	}

	ref := repository.RequestAttachmentRef{
		AttachmentID: grant.AttachmentID, RequestID: grant.RequestID,
		OrgID: grant.OrgID, TeamID: grant.TeamID,
	}
	req, att, err := s.requireRequestAttachment(ctx, p, ref, true)
	if err != nil {
		return nil, nil, err
	}
	if state := RequestAttachmentDownloadStateAt(att, now); !state.Ready {
		return nil, nil, requestDownloadRefusal(att, state.Refusal)
	}

	// The token is spent only now, after every check passed: single-use is the
	// atomic UPDATE's job (two concurrent serves of one URL yield one body), and
	// re-reading the row it returns is what proves it was still unspent.
	spent, err := s.Repo.ConsumeRequestDownloadGrant(ctx, grant.GrantHash, now)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, nil, requestDownloadGrantGone()
		}
		return nil, nil, fmt.Errorf("spend download grant: %w", err)
	}
	s.audit(ctx, p, domain.AuditEntry{
		Action:     "request.attachment.download",
		OrgID:      intRef(req.OrgID),
		TargetType: "request",
		TargetID:   idStr(req.ID),
		TargetName: req.PublicRef,
		Summary:    fmt.Sprintf("downloaded %s on request %s", att.FileName, req.PublicRef),
		Details: map[string]any{
			"request_id":    req.ID,
			"attachment_id": att.ID,
			"content_type":  att.ContentType,
			"size_bytes":    att.SizeBytes,
			"scan_state":    string(att.ScanState),
			"held":          att.Held(),
			"grant_id":      spent.ID,
		},
	})
	return req, att, nil
}

// RevokeRequestDownloadGrants deletes the unspent grants a reference names, which is how a
// surface says "these links no longer work": the next click finds no row, on every replica,
// with no cache to invalidate. It is the ticket's "revocation" verb.
//
// It is deliberately NOT open to every reader of a request. Revoking *another person's*
// live link is an access act — a team member taking back a requester's URL — so it needs
// authz.RequireTeamManager on the request's team (a lead, or an org owner/admin), the same
// gate the submit-grant administration uses. A tenant-wide revoke with no request named
// (an org's links) is a global-admin act. An attachment-scoped or person-scoped revoke
// without a request is not offered here: it would have nowhere to check authority from.
func (s *Service) RevokeRequestDownloadGrants(ctx context.Context, p authz.Principal, ref domain.RequestDownloadGrantRef) (int, error) {
	if err := s.requireNotSubmitScoped(p, "revoke download links"); err != nil {
		return 0, err
	}
	switch {
	case ref.RequestID > 0:
		// Authority comes from the request's own team, which is also what makes the
		// tenant in the reference checked rather than trusted: a request in another
		// tenant than the one supplied is the surface's not-found.
		req, err := s.Repo.GetRequest(ctx, ref.RequestID, ref.OrgID, ref.TeamID)
		if err != nil {
			if errors.Is(err, repository.ErrNotFound) {
				return 0, authz.RequestNotFound()
			}
			return 0, fmt.Errorf("load request %d to revoke its download links: %w", ref.RequestID, err)
		}
		team, err := s.Repo.GetTeam(ctx, req.TeamID)
		if err != nil {
			if errors.Is(err, repository.ErrNotFound) {
				return 0, authz.RequestNotFound()
			}
			return 0, fmt.Errorf("load team %d to revoke its download links: %w", req.TeamID, err)
		}
		if err := s.Authz.RequireTeamManager(ctx, p, team); err != nil {
			// A plain member asking to revoke someone else's link learns nothing about
			// whether that request has links: the same not-found the download gate uses.
			return 0, authz.RequestNotFound()
		}
	case ref.OrgID > 0 && ref.TeamID == 0 && ref.AttachmentID == 0 && ref.UserID == 0:
		if !p.Super {
			return 0, authz.Forbidden("revoking every download link in an org is a global-admin action")
		}
	default:
		// An attachment- or person-scoped revoke with no request has nowhere to check
		// authority from, so it is not a shape this service offers.
		return 0, authz.BadRequest("revoke download links by request (request_id with its org and team), or an org's links as a global admin")
	}
	return s.Repo.RevokeRequestDownloadGrants(ctx, ref)
}

// ListRequestAttachmentsForDownload is the panel read: every file on one request with
// the state a listing shows for it (ready, or why not). It goes through the same
// authorization as a download, so a request's files are never listable by someone who
// could not read one, and the bytes are not loaded.
func (s *Service) ListRequestAttachmentsForDownload(ctx context.Context, p authz.Principal, requestID, orgID, teamID int) (*domain.TeamRequest, []RequestAttachmentPanelEntry, error) {
	if err := s.requireNotSubmitScoped(p, "read request attachments"); err != nil {
		return nil, nil, err
	}
	req, err := s.Repo.GetRequest(ctx, requestID, orgID, teamID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, nil, authz.RequestNotFound()
		}
		return nil, nil, fmt.Errorf("load request %d for its attachments: %w", requestID, err)
	}
	role, err := s.Authz.InboxRole(ctx, p, req.TeamID)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve standing on team %d for its attachments: %w", req.TeamID, err)
	}
	view, err := s.Authz.RequestAccess(ctx, p, req)
	if err != nil {
		return nil, nil, err
	}
	if RequestDownloadCapabilityFor(req, p, role) == RequestDownloadNone && !view.Own() {
		return nil, nil, authz.RequestNotFound()
	}
	atts, err := s.Repo.ListRequestAttachments(ctx, requestID, orgID, teamID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, nil, authz.RequestNotFound()
		}
		return nil, nil, fmt.Errorf("list attachments of request %d: %w", requestID, err)
	}
	now := s.now()
	out := make([]RequestAttachmentPanelEntry, 0, len(atts))
	for i := range atts {
		// One state function for the panel and the gate, so a listing's "waiting for a
		// lead" and the URL's refusal cannot disagree.
		out = append(out, RequestAttachmentPanelEntry{
			Attachment: &atts[i],
			State:      RequestAttachmentDownloadStateAt(&atts[i], now),
		})
	}
	return req, out, nil
}

// RequestAttachmentPanelEntry is one row of a request's attachment panel: the
// metadata (never the bytes) and what a listing may claim about it.
type RequestAttachmentPanelEntry struct {
	Attachment *domain.RequestAttachment      `json:"attachment"`
	State      RequestAttachmentDownloadState `json:"state"`
}

// ---- Helpers ----

// newDownloadGrantToken mints a grant token and its stored hash: 32 random bytes,
// base64url without padding (the receipt-upload token shape, and long enough that a
// guess is not a threat model). The token exists in memory between the mint's return
// and the caller's URL; nothing here keeps it.
func newDownloadGrantToken() (token, hash string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	token = base64.RawURLEncoding.EncodeToString(b)
	return token, hashDownloadGrantToken(token), nil
}

// hashDownloadGrantToken is a grant token's stored form: the lower-case hex sha256 of
// it (the api_tokens and receipt-upload rule). A malformed token hashes to something
// that cannot match a row, which is the answer we want anyway.
func hashDownloadGrantToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// requireNotSubmitScoped refuses a submit-only credential: its scope is exactly one
// operation (its own bound team's submission), and reading a request's private files
// is not it. It is the same fail-closed direction the upload path takes.
func (s *Service) requireNotSubmitScoped(p authz.Principal, what string) error {
	return p.RequireUnscoped(what)
}

// requestDownloadGrantGone is the answer for a grant that is unknown, revoked, spent
// or expired: one 404, never content and never a hint about which of the four it was
// — the same reasoning as RequestNotFound, applied to a URL instead of an id.
func requestDownloadGrantGone() error {
	return authz.RequestNotFound()
}

// requestDownloadRefusal maps the domain's refusal word to the surface's answer. All
// of them are the request surface's not-found except the ones a *legitimate* caller
// needs to understand:
//
//   - scan_pending / scan_infected / held_for_release / purged are answered 403 with
//     the request.invalid code and a message that says which, because the caller is
//     already authorized (they can see the request in the UI) and "try again later"
//     versus "a lead must release it" is the actionable difference. The message names
//     nothing about another party.
//   - Anything else (an unrecognised value) fails closed as the same 403.
//
// Note what is NOT said: never "clean", never a size, never a name. And the 403 is
// only ever reached by a caller who already passed authorization, so it is not an
// existence oracle.
func requestDownloadRefusal(att *domain.RequestAttachment, refusal domain.RequestAttachmentRefusal) error {
	switch refusal {
	case domain.RequestAttachmentRefusedPending:
		return authz.Coded(403, authz.CodeRequestInvalid,
			"%s is still being scanned, so it cannot be downloaded yet; try again once the scan finishes", att.FileName)
	case domain.RequestAttachmentRefusedInfected:
		return authz.Coded(403, authz.CodeRequestInvalid,
			"%s was flagged by a scan and cannot be downloaded", att.FileName)
	case domain.RequestAttachmentRefusedHeld:
		return authz.Coded(403, authz.CodeRequestInvalid,
			"%s is held for a team lead to release, or ask the team for a different format", att.FileName)
	case domain.RequestAttachmentRefusedPurged:
		return authz.Coded(403, authz.CodeRequestInvalid,
			"%s is past its retention window and has been purged", att.FileName)
	}
	return authz.Coded(403, authz.CodeRequestInvalid,
		"%s cannot be downloaded", att.FileName)
}
