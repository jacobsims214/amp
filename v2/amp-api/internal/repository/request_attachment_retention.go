package repository

// Team requests: attachment retention — the D3 clock and the purge (epic 202,
// story 549, AMP #3964; approved product contract artifact 36 v2 §1 and decision
// D3; build slice G4 of discoveries/team-inbox-approved-build-slices.md).
//
// The table is 056's, the record is domain/request_attachment.go's and the store
// is request_attachments.go's. This file is the third thing D3 needs and neither
// of the other two could own:
//
//	an attachment is kept while its request is ACTIVE, then 90 days after the
//	request CLOSES, then purged — unless a team member pins it with keep.
//
// Three properties, and where each one lives:
//
//  1. The clock is derived from the request, never from a caller. One SQL
//     expression (requestRetentionExpiryExpr) reads the request's own state and
//     timestamp to answer "when does this file become purgeable?", and every
//     write here splices that same expression in: the stamp when a request
//     closes, the clear when it reopens, the keep's NULL, and the purge's
//     "is it due?". There is no second definition in Go, so the job and a
//     transition cannot disagree about when 90 days starts. The number of days
//     is a parameter, always domain.RequestAttachmentRetentionDays, so the
//     contract's window exists once in the codebase.
//
//  2. A REOPENED request cannot be purged early, structurally, and a RE-CLOSED
//     one restarts its window. `closed` is a fact about the request's CURRENT
//     state (rejected/duplicate/requester_accepted), so the moment a reopen
//     moves the request back into the working chain the purge predicate stops
//     matching — even if nobody has run the sweep that clears the stored
//     expires_at yet. The stale stamp is then corrected by
//     SyncRequestAttachmentExpiry, which is what makes a reopened file
//     downloadable again rather than reported "purged".
//
//     The close MOMENT is the latest close, not the first. 053's closed_at /
//     accepted_at are first-write-wins on purpose (they record "this request
//     was once closed, and when", which survives a reopen), so reading either
//     as D3's clock makes a re-closed request's files due the instant it is
//     closed again — the first close is already outside the window. The
//     expression therefore takes the newest transition into a closing state
//     from the append-only history and only falls back to the column when there
//     is no such row. See requestRetentionCloseExpr.
//
//  3. Deleting the row deletes the bytes. 056 stores content BYTEA in the row,
//     so the purge is one DELETE and there is no second store to leak behind and
//     no orphan to reap — the same property DeleteRequestAttachment relies on.
//     Nothing here writes a tombstone row: a purged file is gone, and the audit
//     log is where "it existed and was purged" survives (service/request_retention.go).
//
// What this file deliberately does NOT do:
//
//   - No authorization. Who may keep, unkeep or delete a file is the service's
//     (service/request_retention.go), from authz.InboxRole and the request's
//     requester column. This layer writes what it is told, inside the tenant it
//     is given.
//   - No audit row and no request_events row. A keep and a purge are not
//     lifecycle transitions (the claim file's rule), and the service owns their
//     history and audit context.
//   - No scan verdict and no hold decision. A purge deletes bytes whatever the
//     scan state says; the download gate (#3963) is the only place that decides
//     whether bytes may be SERVED, and this file never names a scan state.
//   - No request deletion. The purge removes attachments, never a request: a
//     request's own lifecycle (including the trash) is 053's, and a retention job
//     that deleted requests would destroy the record of what was asked.
//
// No schema change, no migration: 056's partial expiry index
// (idx_request_attachments_expiry (expires_at) WHERE NOT keep) is exactly the
// purge's scan, and the reconcile pass below walks the same index.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/simstech/amp-api/internal/domain"
)

// requestRetentionCloseExpr is when the request closed, or NULL while it is
// still active. It is the one definition of "closed" for D3, and it is a
// function of the request alias because every statement here joins team_requests
// and a bare column name would be ambiguous in some of them.
//
// Which states count, and why:
//
//   - rejected and duplicate are the contract's terminal states.
//   - requester_accepted is the successful end of the lifecycle: the requester
//     has accepted the delivery, so the request is done and its files start the
//     post-close clock.
//   - delivered is NOT closed: the team has stated the outcome exists and is
//     waiting for the requester, so the request is still active and its files
//     must not start a purge clock that could fire while a person still needs
//     them. That also means a request left in delivered forever retains its
//     files forever, which is the honest reading of "kept while active".
//
// WHEN it closed is the LATEST close, not the first one, and that is the whole
// reason this is not just `r.closed_at`. 053's closed_at and accepted_at are
// FIRST-WRITE-WINS (requestTransitionColumns uses COALESCE) because they record
// "this request was once closed, and when" — a fact that survives a reopen. But
// D3's window is not that fact: "90 days after close" means the close the
// request is under NOW, so a request that closed, reopened and closed again must
// restart the window at the SECOND close. Reading the latched column instead
// makes the file due the instant the team re-closes (first-close+90d is already
// in the past), which purges customer content that is inside the current window
// — the premature purge this slice exists to avoid.
//
// The latest close is the newest transition into a closing state, read from the
// append-only history (054) — which is what "append-only" is for: the row that
// says the request entered `rejected` for the second time. 054's
// idx_request_events_action (request_id, action, id DESC) serves exactly this
// ORDER BY id DESC LIMIT 1 lookup. The columns remain the fallback for a row
// with no such event (a fixture, or data written before this slice), so the
// expression is a superset of the old one rather than a second opinion: where
// the event exists it wins, where it does not the column answers as before.
func requestRetentionCloseExpr(alias string) string {
	return fmt.Sprintf(`CASE WHEN %[1]s.state IN ('rejected', 'duplicate', 'requester_accepted')
		THEN COALESCE(
			(SELECT e.created_at FROM request_events e
			  WHERE e.request_id = %[1]s.id AND e.action = 'transition'
			    AND e.to_state IN ('rejected', 'duplicate', 'requester_accepted')
			  ORDER BY e.id DESC LIMIT 1),
			CASE WHEN %[1]s.state = 'requester_accepted' THEN %[1]s.accepted_at
			     ELSE %[1]s.closed_at END)
		END`, alias)
}

// requestRetentionExpiryExpr is the close moment plus the contract's window, or
// NULL while the request is active. daysPlaceholder is the statement's parameter
// holding domain.RequestAttachmentRetentionDays; it is cast because Postgres
// cannot infer an untyped parameter inside make_interval.
//
// A NULL expiry is the schema's "may not be purged" (056): an active request's
// file and a kept file both have no expiry, and neither is ever deleted by the
// job. There is deliberately no "expire immediately" value.
func requestRetentionExpiryExpr(alias string, daysPlaceholder int) string {
	return fmt.Sprintf(`(%s) + make_interval(days => $%d::int)`, requestRetentionCloseExpr(alias), daysPlaceholder)
}

// RequestAttachmentKeepWrite pins or releases one file's retention, inside the
// request and tenant it belongs to.
//
// Keep is the whole decision: true pins the file (keep, kept_by, kept_at set and
// expires_at cleared, so nothing may purge it), false lets the request's own
// clock apply again. There is no field for the expiry itself — a caller that
// could name it could keep a file alive past the contract's window or purge one
// before it, and neither is a decision this layer may accept.
type RequestAttachmentKeepWrite struct {
	RequestAttachmentRef
	Keep bool
	// KeptBy is the person pinning the file, when the credential has an AMP
	// user. 056 makes kept_by nullable for exactly that reason.
	KeptBy *int
}

// SetRequestAttachmentKeep pins or unpins one attachment and returns the stored
// row (metadata only — the bytes are never read by a retention write).
//
// Unpinning does NOT restart the clock: the file's expiry is recomputed from the
// request's close moment, so a file whose 90 days have already passed becomes
// due immediately. That is D3 read literally ("90 days after close"), and it is
// deliberately not artifacts' rule (KeepArtifact's unkeep moves expires_at out to
// a fresh window), because an attachment's window is a fact about the request,
// not about when somebody changed their mind about keeping it.
//
// A file in another team of the same org, in another org, on another request, or
// with an id that does not exist is ErrNotFound and nothing is written — the
// request surface's one 404, so a keep cannot be used to probe for a file.
func (r *Repo) SetRequestAttachmentKeep(ctx context.Context, w RequestAttachmentKeepWrite) (*domain.RequestAttachment, error) {
	if err := checkRequestWriteScope(w.RequestID, w.OrgID, w.TeamID); err != nil {
		return nil, err
	}
	if w.AttachmentID <= 0 {
		return nil, fmt.Errorf("attachment %d: %w", w.AttachmentID, ErrNotFound)
	}
	if w.KeptBy != nil && *w.KeptBy <= 0 {
		return nil, fmt.Errorf("kept_by must be a user id (got %d)", *w.KeptBy)
	}
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// The request's row lock is the one every attachment write takes, so a keep
	// cannot land between an upload's count and its insert, and it also answers
	// "is this request in this tenant?" before anything is written.
	if _, err := lockRequest(ctx, tx, RequestClaimWrite{RequestID: w.RequestID, OrgID: w.OrgID, TeamID: w.TeamID}); err != nil {
		return nil, err
	}

	expiry := requestRetentionExpiryExpr("r", 7)
	a := &domain.RequestAttachment{}
	err = scanRequestAttachmentMeta(tx.QueryRow(ctx, `UPDATE request_attachments a
		   SET keep = $3,
		       kept_by = CASE WHEN $3 THEN $4::int ELSE NULL END,
		       kept_at = CASE WHEN $3 THEN NOW() ELSE NULL END,
		       expires_at = CASE WHEN $3 THEN NULL ELSE `+expiry+` END
		  FROM team_requests r
		 WHERE a.id = $1 AND a.request_id = $2 AND r.id = a.request_id
		   AND r.org_id = $5 AND r.team_id = $6
		RETURNING `+requestAttachmentMetaCols("a"),
		w.AttachmentID, w.RequestID, w.Keep, w.KeptBy, w.OrgID, w.TeamID,
		domain.RequestAttachmentRetentionDays), a)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("attachment %d on request %d: %w", w.AttachmentID, w.RequestID, ErrNotFound)
	}
	if err != nil {
		if isForeignKeyViolation(err) {
			// kept_by names a users row that is gone: the same not-found for that
			// person as an upload's departed uploader, never a 23503 as a 500.
			return nil, fmt.Errorf("attachment %d: kept_by names user %v, who does not exist: %w", w.AttachmentID, w.KeptBy, ErrNotFound)
		}
		return nil, fmt.Errorf("keep attachment %d on request %d: %w", w.AttachmentID, w.RequestID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return a, nil
}

// SyncRequestAttachmentExpiry brings one request's attachments' expires_at in
// step with the request's own retention posture: a file on a closed request gets
// close+90d, a file on an active (or reopened) request gets NULL, and a kept file
// always gets NULL.
//
// It is the write a transition makes after it commits, and the reconcile pass
// (SyncAllRequestAttachmentExpiry) makes it for everything the transition could
// not reach — a close that failed to stamp, a reopen whose clear was lost, a row
// written before this slice existed. It answers how many rows actually changed,
// so a no-op close/reopen pair writes nothing at all: an UPDATE whose new value
// equals the old one would still rewrite the row and re-evaluate 056's size
// CHECK, and "nothing to do" should cost nothing.
//
// The purge's own predicate does not depend on this having run (see the header's
// property 2), so a failure here is a stale timestamp, never a wrong deletion.
func (r *Repo) SyncRequestAttachmentExpiry(ctx context.Context, requestID, orgID, teamID int) (int, error) {
	if err := checkRequestWriteScope(requestID, orgID, teamID); err != nil {
		return 0, err
	}
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := lockRequest(ctx, tx, RequestClaimWrite{RequestID: requestID, OrgID: orgID, TeamID: teamID}); err != nil {
		return 0, err
	}
	want := `CASE WHEN NOT a.keep THEN ` + requestRetentionExpiryExpr("r", 4) + ` ELSE NULL END`
	tag, err := tx.Exec(ctx, `UPDATE request_attachments a
		   SET expires_at = `+want+`
		  FROM team_requests r
		 WHERE a.request_id = $1 AND r.id = a.request_id
		   AND r.org_id = $2 AND r.team_id = $3
		   AND a.expires_at IS DISTINCT FROM (`+want+`)`,
		requestID, orgID, teamID, domain.RequestAttachmentRetentionDays)
	if err != nil {
		return 0, fmt.Errorf("sync attachment expiry for request %d: %w", requestID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// SyncAllRequestAttachmentExpiry is the reconcile pass: it fixes up to limit rows
// whose stored expiry disagrees with their request, and answers how many it
// changed. The caller runs it periodically (service.RunRequestAttachmentPurge),
// so it is bounded by construction and safe from several replicas: the predicate
// shrinks as rows are corrected, so two replicas converge instead of fighting.
//
// Ordering by id (not by the request's timestamp) is what makes the pass
// progress: a row it corrects leaves the set, so the next pass starts at the
// next row rather than at the same first page forever.
func (r *Repo) SyncAllRequestAttachmentExpiry(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		limit = 500
	}
	want := `CASE WHEN NOT a.keep THEN ` + requestRetentionExpiryExpr("r", 1) + ` ELSE NULL END`
	tag, err := r.db.Exec(ctx, `WITH due AS (
		    SELECT a.id AS id, `+want+` AS want
		      FROM request_attachments a
		      JOIN team_requests r ON r.id = a.request_id
		     WHERE a.expires_at IS DISTINCT FROM (`+want+`)
		     ORDER BY a.id
		     LIMIT $2
		)
		UPDATE request_attachments a
		   SET expires_at = due.want
		  FROM due
		 WHERE a.id = due.id`,
		domain.RequestAttachmentRetentionDays, limit)
	if err != nil {
		return 0, fmt.Errorf("reconcile attachment expiry: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// RequestAttachmentPurgeRow is one file the purge deleted: enough for the audit
// row (which request in which tenant, how many bytes of what type) and nothing
// more. Deliberately no file name and no digest — the audit log is read by people
// asking who did what, and a requester's file name is their content (the
// upload path's rule).
type RequestAttachmentPurgeRow struct {
	AttachmentID int64
	RequestID    int
	OrgID        int
	TeamID       int
	SizeBytes    int64
	ContentType  string
	PublicRef    string
}

// PurgeExpiredRequestAttachments deletes up to limit attachments whose 90 days
// after their request closed have passed and that nobody kept, and returns what
// it deleted so the caller can audit each one.
//
// Three clauses carry the contract, and each is worth reading before changing it:
//
//   - NOT keep and expires_at IS NOT NULL AND expires_at <= now is D3 in one
//     line, exactly as 056 documents it, and 056's partial expiry index serves
//     it.
//   - the request must still be CLOSED, evaluated now, from the request's own
//     state — that is what stops a reopen from losing its files to a stamp
//     written before the reopen. It is a re-check, not a latch.
//   - FOR UPDATE SKIP LOCKED with a limit makes the pass bounded and safe on
//     several replicas at once: two jobs never hand each other the same row and
//     neither waits on the other. The lock is taken on the attachment rows only,
//     never on the request, so a purge cannot block a triager's transition.
//
// The DELETE re-states the three conditions against the row it is about to
// remove rather than trusting the CTE's snapshot. That matters for exactly one
// race: a person pinning a file with keep in the millisecond between this
// statement's snapshot and its DELETE. Postgres re-evaluates the target's own
// WHERE against the row version it is deleting, so the re-statement is what makes
// "a kept file is never purged" true even against a concurrent keep, instead of
// relying on the job never losing that race.
//
// The bytes live in the row (056), so the DELETE is the physical cleanup: no
// second store, no orphan, nothing left to reap.
func (r *Repo) PurgeExpiredRequestAttachments(ctx context.Context, now time.Time, limit int) ([]RequestAttachmentPurgeRow, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := r.db.Query(ctx, `WITH due AS (
		    SELECT a.id
		      FROM request_attachments a
		      JOIN team_requests r ON r.id = a.request_id
		     WHERE NOT a.keep
		       AND a.expires_at IS NOT NULL
		       AND a.expires_at <= $1
		       AND `+requestRetentionCloseExpr("r")+` IS NOT NULL
		     ORDER BY a.expires_at, a.id
		     LIMIT $2
		     FOR UPDATE OF a SKIP LOCKED
		)
		DELETE FROM request_attachments d
		 USING due, team_requests r
		 WHERE d.id = due.id AND r.id = d.request_id
		   AND NOT d.keep AND d.expires_at IS NOT NULL AND d.expires_at <= $1
		   AND `+requestRetentionCloseExpr("r")+` IS NOT NULL
		RETURNING d.id, d.request_id, r.org_id, r.team_id, d.size_bytes, d.content_type, r.public_ref`,
		now, limit)
	if err != nil {
		return nil, fmt.Errorf("purge expired request attachments: %w", err)
	}
	defer rows.Close()
	out := make([]RequestAttachmentPurgeRow, 0)
	for rows.Next() {
		var p RequestAttachmentPurgeRow
		if err := rows.Scan(&p.AttachmentID, &p.RequestID, &p.OrgID, &p.TeamID,
			&p.SizeBytes, &p.ContentType, &p.PublicRef); err != nil {
			return nil, fmt.Errorf("scan a purged request attachment: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("purge expired request attachments: %w", err)
	}
	return out, nil
}
