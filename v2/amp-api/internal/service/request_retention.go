package service

// Team request attachment retention: the D3 clock, the authorized keep and
// delete, and the purge job (epic 202, story 549, AMP #3964; approved product
// contract artifact 36 v2 §1 and decision D3; build slice G4 of
// discoveries/team-inbox-approved-build-slices.md).
//
// The rule this file implements, in the contract's own words: attachments are
// "kept while the request is active, then 90 days after close, then purged, with
// a team 'keep' to pin them". Everything below is one of the four things that
// sentence needs and neither the domain nor the repository could own:
//
//  1. WHO. Keeping a file past its window is a team's decision about its record
//     (D3 says "a team keep"), and deleting one is either the team moderating its
//     inbox or the requester withdrawing their own content. Who may do which is
//     RequestRetentionCapabilityFor, a pure function, so the matrix is testable
//     without a database and a surface cannot invent a wider rule. It deliberately
//     mirrors the upload capability's shape (requester on their own request, a
//     team member on any request in the inbox) without being it: a requester may
//     DELETE their own file and may not keep it, because "never delete this" is a
//     retention promise only the team that owns the record can make.
//
//  2. WHEN. The clock is the request's, not the file's, and it is computed in
//     one place — the repository's requestRetentionExpiryExpr — so a close, a
//     reopen and the purge cannot disagree about when 90 days starts. The
//     lifecycle's write path calls syncAttachmentRetention after a move that
//     changes the request's posture (close, reopen, acceptance), so a reopen
//     clears the stored expiry in the same breath as the state change instead of
//     waiting for the next sweep.
//
//  3. WHAT HAPPENS. A keep pins (keep/kept_by/kept_at set, expires_at cleared);
//     an unkeep recomputes the window from the request's close moment, so a file
//     already past it becomes due immediately rather than starting a fresh 90
//     days — D3 is a window after close, not after somebody changed their mind.
//     A delete removes the row, and 056 puts the bytes IN the row, so the delete
//     IS the physical cleanup: there is no second store to leak behind.
//
//  4. HOW OFTEN. RunRequestAttachmentPurge is a bounded periodic job in the house
//     shape (the RunArtifactPurge/RunReceiptSweep pattern): one ticker, one
//     advisory lock so a single replica does the work, a bounded number of
//     batches per pass, and no new scheduler. It also reconciles the stored
//     expiry against the request's current posture first, which is what repairs a
//     stamp a failed transition write or an out-of-band edit left behind.
//
// Every purge and every keep/delete writes exactly one audit row, after the
// change committed (audit.go's rule: a failure is logged, never returned, because
// the change already happened). The purge audits as AMP itself rather than as any
// person, and records ids, the sniffed type and the size — never the file's name,
// its bytes or its digest, because those three are the requester's content and
// "who did what" is answered by the ids.
//
// What this file deliberately does NOT do:
//
//   - No request deletion, and no cascade beyond the file. A retention job that
//     deleted requests would destroy the record of what was asked; the purge
//     removes files only, and a request's own lifecycle (including the trash) is
//     053's.
//   - No download decision. Whether bytes may be SERVED is #3963's gate
//     (domain.RequestAttachmentDownloadRefusal); a purged file is simply gone, so
//     its gate answers not-found.
//   - No scanner, no verdict, no release. A held or unsupported file is purged on
//     the same clock as any other; the hold governs serving, not retention.
//   - No endpoint, no MCP tool, no UI. This is the service those adapters (F5,
//     I3, J2) call; #3964 ships no route.
//   - No live purge in a test. Every test drives a disposable Postgres with its
//     own orgs, teams and requests; nothing here ever touches a running AMP.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/repository"
)

// RequestAttachmentPurgeBatch is how many files one statement of the purge
// deletes, and how many rows the reconcile pass corrects at a time. Small enough
// that the statement never holds a long lock, large enough that a backlog is
// worked off in a handful of batches rather than one row per tick.
const RequestAttachmentPurgeBatch = 500

// RequestAttachmentPurgeBatches bounds one pass of the job: a backlog is worked
// off over a few ticks instead of one pass running for minutes. The next tick
// continues where this one stopped, which is why the pass is allowed to stop
// early at all.
const RequestAttachmentPurgeBatches = 10

// RequestRetentionCapability is who may keep or delete one request's files.
//
// Two values, like the upload capability, and deliberately not a ladder: a
// requester may remove their own file, and a team member may do anything to any
// request in the inbox. Nothing widens one into the other.
type RequestRetentionCapability int

const (
	// RequestRetentionNone: no standing. Reported as the request surface's single
	// not-found, never as a 403 — a caller with no standing must not learn the
	// request or its files exist.
	RequestRetentionNone RequestRetentionCapability = iota
	// RequestRetentionRequester: the person the request was submitted by. They may
	// DELETE their own file (withdrawing their content) and may not keep it: D3's
	// keep is a team's retention promise, and a requester cannot extend the life of
	// their own upload.
	RequestRetentionRequester
	// RequestRetentionTeam: a member of the inbox's team (authz.InboxMember, which
	// includes its lead and an owner/admin of its org). They may keep, unkeep and
	// delete.
	RequestRetentionTeam
)

func (c RequestRetentionCapability) String() string {
	switch c {
	case RequestRetentionRequester:
		return "requester"
	case RequestRetentionTeam:
		return "team"
	}
	return "none"
}

// MayKeep reports whether the capability includes pinning a file past its window.
// Only the team's does (see the type's comment), and stating it as a method keeps
// every caller — and the refusal's message — on the same answer.
func (c RequestRetentionCapability) MayKeep() bool { return c >= RequestRetentionTeam }

// RequestRetentionCapabilityFor decides, purely, what p may do to r's files given
// p's standing role on r's team.
//
// The team wins when a caller is both (a triager filing a request to their own
// inbox is still triaging it), and "their own" is the durable requester column
// rather than an audit field: a request submitted by a connector or service
// client has no requester user, so nobody can claim it as their own.
func RequestRetentionCapabilityFor(r *domain.TeamRequest, p authz.Principal, role authz.InboxRole) RequestRetentionCapability {
	if r == nil {
		return RequestRetentionNone
	}
	if role >= authz.InboxMember {
		return RequestRetentionTeam
	}
	if p.HasUser() && r.RequesterUserID != nil && *r.RequesterUserID == p.UserID {
		return RequestRetentionRequester
	}
	return RequestRetentionNone
}

// RequestAttachmentRetentionInput names one file on one request in one tenant.
// The tenant is required and never inferred from the attachment id: an id-only
// lookup is the cross-tenant primitive the request surface exists without.
type RequestAttachmentRetentionInput struct {
	RequestID    int
	OrgID        int
	TeamID       int
	AttachmentID int64
}

func (in RequestAttachmentRetentionInput) ref() repository.RequestAttachmentRef {
	return repository.RequestAttachmentRef{
		AttachmentID: in.AttachmentID,
		RequestID:    in.RequestID,
		OrgID:        in.OrgID,
		TeamID:       in.TeamID,
	}
}

// loadRetentionTarget loads the request, its team and the caller's capability on
// it, and refuses a caller with no standing as the surface's single not-found.
//
// The team is loaded from the request rather than from the caller because the
// caller is not trusted to name the org: the request row carries the tenant, and
// asking more than once would be a second chance to disagree with the first
// answer.
func (s *Service) loadRetentionTarget(ctx context.Context, p authz.Principal, in RequestAttachmentRetentionInput) (*domain.TeamRequest, RequestRetentionCapability, error) {
	if in.RequestID <= 0 || in.AttachmentID <= 0 {
		return nil, RequestRetentionNone, authz.RequestNotFound()
	}
	if in.OrgID <= 0 || in.TeamID <= 0 {
		return nil, RequestRetentionNone, authz.RequestInvalid("a request attachment names the request's tenant (org and team), as resolved by an authorized read")
	}
	req, err := s.Repo.GetRequest(ctx, in.RequestID, in.OrgID, in.TeamID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, RequestRetentionNone, authz.RequestNotFound()
		}
		return nil, RequestRetentionNone, fmt.Errorf("load request %d for attachment retention: %w", in.RequestID, err)
	}
	role, err := s.Authz.InboxRole(ctx, p, req.TeamID)
	if err != nil {
		return nil, RequestRetentionNone, fmt.Errorf("resolve standing on team %d for attachment retention: %w", req.TeamID, err)
	}
	capability := RequestRetentionCapabilityFor(req, p, role)
	if capability == RequestRetentionNone {
		return nil, RequestRetentionNone, authz.RequestNotFound()
	}
	return req, capability, nil
}

// KeepRequestAttachment pins one file past its retention window (keep true) or
// lets the request's own clock apply again (keep false), and is the only path in
// the product that writes keep/kept_by/kept_at/expires_at.
//
// Order: the caller's standing first (so a stranger's request is refused before
// anything is read about the file), then the capability's own rule (a requester
// may not pin), then the write. A file in another team, another org, on another
// request or nowhere is the surface's single not-found and nothing is written.
//
// An unkeep recomputes the expiry from the request's close moment, so a file
// whose 90 days have already passed is due on the very next purge rather than
// given a fresh window. That is D3 read literally, and it is deliberately not
// artifacts' unkeep rule (KeepArtifact moves expires_at out to a new window):
// an attachment's window is a fact about the request, not about when somebody
// changed their mind about keeping it.
func (s *Service) KeepRequestAttachment(ctx context.Context, p authz.Principal, in RequestAttachmentRetentionInput, keep bool) (*domain.RequestAttachment, error) {
	// A submit-scoped credential reaches exactly one operation — the submission
	// itself (authz/token_scope.go) — and changing a stored file's retention is
	// not it. Fail closed, and with the same answer the surface gate gives.
	if err := p.RequireUnscoped("change an attachment's retention"); err != nil {
		return nil, err
	}
	req, capability, err := s.loadRetentionTarget(ctx, p, in)
	if err != nil {
		return nil, err
	}
	if !capability.MayKeep() {
		// A requester sees their own request, so this is the honest "you can see
		// it, not do it" refusal rather than a 404: keeping a file past the window
		// is the team's retention decision (D3), not the uploader's.
		return nil, authz.Forbidden("keeping a request attachment past its %d-day window is the team's decision (D3): the requester who sent it can delete it, and cannot pin it", domain.RequestAttachmentRetentionDays)
	}
	a, err := s.Repo.SetRequestAttachmentKeep(ctx, repository.RequestAttachmentKeepWrite{
		RequestAttachmentRef: in.ref(),
		Keep:                 keep,
		KeptBy:               p.UserRef(),
	})
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, authz.RequestNotFound()
		}
		return nil, fmt.Errorf("keep attachment %d on request %d: %w", in.AttachmentID, req.ID, err)
	}
	s.recordAttachmentRetention(ctx, p, req, a, map[bool]string{true: "request.attachment.keep", false: "request.attachment.unkeep"}[keep])
	return a, nil
}

// DeleteRequestAttachment removes one file and its bytes, authorized, audited and
// idempotent.
//
// It answers changed=false when the row was already gone, which is what makes a
// retry after a lost response safe (the DeleteRequestAttachment repository rule).
// A no-op writes no audit row: an entry saying somebody deleted a file that was
// already gone would be a record of an act that did not happen.
func (s *Service) DeleteRequestAttachment(ctx context.Context, p authz.Principal, in RequestAttachmentRetentionInput) (bool, error) {
	if err := p.RequireUnscoped("delete a request attachment"); err != nil {
		return false, err
	}
	req, capability, err := s.loadRetentionTarget(ctx, p, in)
	if err != nil {
		return false, err
	}
	// Read before the delete, for the audit row: the stored facts (type, size) are
	// gone once the row is. A missing file is the surface's not-found, which is
	// also what a foreign tenant's file answers — the two are indistinguishable on
	// purpose.
	att, err := s.Repo.GetRequestAttachment(ctx, in.ref())
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return false, authz.RequestNotFound()
		}
		return false, fmt.Errorf("load attachment %d on request %d: %w", in.AttachmentID, req.ID, err)
	}
	// The capability is checked before the write, and only ever widens to
	// "requester deletes their own file": there is no caller a delete is allowed
	// for whom this refuses, because RequestRetentionCapabilityFor's two values
	// are exactly the two callers the contract gives an attachment to.
	if capability == RequestRetentionNone {
		return false, authz.RequestNotFound()
	}
	changed, err := s.Repo.DeleteRequestAttachment(ctx, in.ref())
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return false, authz.RequestNotFound()
		}
		return false, fmt.Errorf("delete attachment %d on request %d: %w", in.AttachmentID, req.ID, err)
	}
	if changed {
		s.recordAttachmentRetention(ctx, p, req, att, "request.attachment.delete")
	}
	return changed, nil
}

// recordAttachmentRetention writes the one audit row a keep, unkeep or delete
// gets: ids, the sniffed type and the size, on the request's org, targeting the
// request. Never the file's name, its bytes or its digest — those are the
// requester's content, and none of them answers "who did what".
func (s *Service) recordAttachmentRetention(ctx context.Context, p authz.Principal, req *domain.TeamRequest, att *domain.RequestAttachment, action string) {
	s.audit(ctx, p, domain.AuditEntry{
		Action:     action,
		OrgID:      intRef(req.OrgID),
		TargetType: "request",
		TargetID:   idStr(req.ID),
		TargetName: req.PublicRef,
		Summary:    retentionSummary(action, req, att),
		Details: map[string]any{
			"request_id":    req.ID,
			"attachment_id": att.ID,
			"content_type":  att.ContentType,
			"size_bytes":    att.SizeBytes,
			"keep":          att.Keep,
			"request_state": string(req.State),
		},
	})
}

// retentionSummary is the sentence a person reads in the log. It names the request
// and the size and never the file, for the reason above.
func retentionSummary(action string, req *domain.TeamRequest, att *domain.RequestAttachment) string {
	what := fmt.Sprintf("the %d-byte %s attachment #%d", att.SizeBytes, att.ContentType, att.ID)
	switch action {
	case "request.attachment.keep":
		return fmt.Sprintf("pinned %s on request %s: it is no longer purged %d days after the request closes", what, req.PublicRef, domain.RequestAttachmentRetentionDays)
	case "request.attachment.unkeep":
		return fmt.Sprintf("stopped keeping %s on request %s: the request's own %d-day window applies again", what, req.PublicRef, domain.RequestAttachmentRetentionDays)
	default:
		return fmt.Sprintf("deleted %s from request %s, and its bytes with it", what, req.PublicRef)
	}
}

// ---- The purge ----

// PurgeExpiredRequestAttachments deletes every file whose request closed more
// than domain.RequestAttachmentRetentionDays ago and that nobody kept, and audits
// each one. It answers how many were purged.
//
// It reconciles the stored expiry against the requests' current posture first.
// That is not a precaution: a close that committed but whose expiry write failed,
// a reopen whose clear was lost, and any row written before this slice existed
// all leave expires_at disagreeing with the request, and the purge's predicate
// reads expires_at. The reconcile is the step that makes the job self-healing
// instead of dependent on every writer having succeeded.
//
// The pass is bounded (RequestAttachmentPurgeBatches × RequestAttachmentPurgeBatch)
// and safe from several replicas: the repository's DELETE takes FOR UPDATE SKIP
// LOCKED, and RunRequestAttachmentPurge adds one advisory lock on top so only one
// replica runs a pass at all.
func (s *Service) PurgeExpiredRequestAttachments(ctx context.Context) (int, error) {
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	if _, err := s.Repo.SyncAllRequestAttachmentExpiry(ctx, RequestAttachmentPurgeBatch*RequestAttachmentPurgeBatches); err != nil {
		// A reconcile failure must not stop the purge: the purge's own predicate
		// re-checks that the request is still closed, so an unreconciled row is
		// either correctly skippable (a reopen left a stale stamp) or correctly
		// due (a close never stamped). Report it and carry on.
		slog.Warn("request attachment expiry reconcile failed", "err", err)
	}
	// time.Now(), deliberately not s.Now: that clock exists so a test can drive the
	// submission limiter's fixed window (see Service.Now), and a retention window
	// measured on a clock someone can move is a window that can be made to purge
	// early. The tests here reach "90 days later" by ageing the request's close
	// moment instead, which is the fact the rule is about.
	now := time.Now()
	total := 0
	amp := authz.Principal{Email: "AMP"}
	for batch := 0; batch < RequestAttachmentPurgeBatches; batch++ {
		gone, err := s.Repo.PurgeExpiredRequestAttachments(ctx, now, RequestAttachmentPurgeBatch)
		if err != nil {
			// A pass that stopped mid-way still deleted (and audited) what it got
			// through, so the count travels with the error instead of being lost: the
			// caller logs both, and the next tick continues from where this one
			// stopped because the deleted rows are gone.
			return total, fmt.Errorf("purge expired request attachments (after %d files): %w", total, err)
		}
		for _, p := range gone {
			slog.Info("request attachment purged", "attachment_id", p.AttachmentID,
				"request_id", p.RequestID, "team_id", p.TeamID, "size_bytes", p.SizeBytes, "content_type", p.ContentType)
			s.audit(ctx, amp, domain.AuditEntry{
				Action:     "request.attachment.purge",
				OrgID:      intRef(p.OrgID),
				TargetType: "request",
				TargetID:   idStr(p.RequestID),
				TargetName: p.PublicRef,
				Summary:    fmt.Sprintf("purged attachment #%d (%d bytes, %s) from request %s: %d days passed after the request closed and nobody kept it", p.AttachmentID, p.SizeBytes, p.ContentType, p.PublicRef, domain.RequestAttachmentRetentionDays),
				Details: map[string]any{
					"request_id":    p.RequestID,
					"attachment_id": p.AttachmentID,
					"content_type":  p.ContentType,
					"size_bytes":    p.SizeBytes,
					"team_id":       p.TeamID,
				},
			})
		}
		total += len(gone)
		if len(gone) < RequestAttachmentPurgeBatch {
			break
		}
	}
	return total, nil
}

// RunRequestAttachmentPurge purges expired request attachments every interval
// until ctx ends, on one replica at a time.
//
// It is the house's periodic-job shape (RunArtifactPurge, RunReceiptSweep) rather
// than a scheduler: one ticker, one advisory lock held for the pass, the first
// pass immediately so a restart does not skip a window, and no registration in
// any framework. lock may be nil, which means "no coordination" — the shape a
// test uses, and the property that still holds without it is that every DELETE is
// idempotent and safe to race.
func (s *Service) RunRequestAttachmentPurge(ctx context.Context, interval time.Duration, lock TryLock) {
	if interval <= 0 {
		interval = time.Hour
	}
	run := func() {
		n, err := s.PurgeExpiredRequestAttachments(ctx)
		if err != nil {
			if ctx.Err() == nil {
				slog.Warn("request attachment purge failed", "err", err)
			}
			return
		}
		if n > 0 {
			slog.Info("request attachment purge", "purged", n)
		}
	}
	if lock == nil {
		run()
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				run()
			}
		}
	}
	runLocked(ctx, interval, lock, "request-attachment-purge", run)
}

// ---- The lifecycle's side of the clock ----

// syncAttachmentRetention brings one request's attachments' expiry in step with
// the move that just committed, and is the lifecycle write path's only contact
// with retention.
//
// It is called after the transition committed, for the moves that change the
// request's retention posture (a close, a reopen, an acceptance), and its failure
// is logged rather than returned: the move already happened, and the next purge
// pass reconciles what this call could not (the purge's predicate re-checks the
// request's state, so an unsynced row is never wrongly deleted). The idempotent
// UPDATE means a no-op — a close of a request with no files, a move that does not
// change the posture — writes nothing.
//
// It lives here rather than in request_lifecycle.go so the retention rule keeps
// one home: the lifecycle file calls this and knows nothing about 90 days.
func (s *Service) syncAttachmentRetention(ctx context.Context, req *domain.TeamRequest, to domain.RequestStatus, reopened bool) {
	if req == nil {
		return
	}
	if !reopened && !requestStatusStartsRetentionClock(to) {
		// A working-state move cannot change a file's expiry: the clock is the
		// request's close, and this move is not one.
		return
	}
	if _, err := s.Repo.SyncRequestAttachmentExpiry(ctx, req.ID, req.OrgID, req.TeamID); err != nil {
		slog.Warn("request attachment expiry sync failed", "request_id", req.ID, "to_state", string(to), "err", err)
	}
}

// requestStatusStartsRetentionClock reports whether entering this state can start
// (or, on a reopen, stop) the D3 clock. It is deliberately the same set of states
// requestRetentionCloseExpr reads in SQL, and the retention table test drives
// every one of the nine states through the purge, so a state added to one side
// and not the other fails that table rather than silently becoming a close whose
// files never expire (or an expiry for a request that is still active).
func requestStatusStartsRetentionClock(to domain.RequestStatus) bool {
	switch to {
	case domain.RequestStatusRejected, domain.RequestStatusDuplicate, domain.RequestStatusRequesterAccepted:
		return true
	default:
		return false
	}
}
