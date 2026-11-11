package repository

// Team requests: the discussion thread, appended (epic 202, story 535, AMP
// #3874; approved product contract artifact 36 v2 §1, invariants I5 and I6;
// build slice C5 of discoveries/team-inbox-approved-build-slices.md).
//
// Migration 054 (architecture/team-request-history-schema.md) added
// request_comments and made the one decision this file exists to keep:
// APPEND-ONLY IS AN APPLICATION CONTRACT. Rows are only ever inserted. No
// function here updates a comment's body, kind or attribution, and there is
// deliberately no edit and no delete method — an edit or a retraction is a NEW
// row. `TestMigration_054NoUpdateOrDeletePath` scans this package for a write
// path against the table, so the absence is enforced, not merely intended.
//
// Two writes the database itself makes are legitimate and are not "edits": a
// deleted user nulls author_user_id (ON DELETE SET NULL) and the `author` text
// snapshot keeps the row attributable; and a request takes its thread with it
// (ON DELETE CASCADE). That is why attribution is a snapshot plus a nullable
// link and never a join — the thread must still say who spoke after the user
// is gone.
//
// A question does not move the request. `kind = 'question'` is what I5 obliges
// an answer to, but entering needs_information is a lifecycle transition: it
// writes an event row and stamps needs_info_since, and it belongs to the one
// transition function (slice S5). Appending a comment here changes exactly one
// row — the comment — and a test asserts the request row is untouched, so a
// comment path can never quietly advance a state (I8).

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/simstech/amp-api/internal/domain"
)

// requestCommentCols is the column list of request_comments and
// domain.RequestComment, in 054's order. Like requestCols it is explicit (no
// SELECT *) and positional against the scan targets below.
//
// When a read joins team_requests for its tenant (which is how every read of a
// thread proves the request is in the caller's tenant without a second
// round-trip), the same list is emitted qualified: both tables have `id` and
// `created_at`, and an unqualified list in a join is ambiguous.
func requestCommentCols(alias string) string {
	cols := []string{"id", "request_id", "kind", "body", "author", "author_user_id", "created_at"}
	if alias != "" {
		for i, c := range cols {
			cols[i] = alias + "." + c
		}
	}
	return strings.Join(cols, ", ")
}

func requestCommentFields(c *domain.RequestComment) []any {
	return []any{&c.ID, &c.RequestID, &c.Kind, &c.Body, &c.Author, &c.AuthorUserID, &c.CreatedAt}
}

func scanRequestComment(row rowScanner, c *domain.RequestComment) error {
	return row.Scan(requestCommentFields(c)...)
}

// AppendRequestCommentRequest is one thing said on a request. It is the
// repository's input: the service (slice S3) resolves the caller into an author
// snapshot and passes it, so no request text ever decides who is speaking.
type AppendRequestCommentRequest struct {
	// The request, and the tenant it must belong to. OrgID and TeamID are
	// required; the append is scoped by them, so a caller can never write into
	// another team's thread by naming its request id.
	RequestID int
	OrgID     int
	TeamID    int

	// Kind is the discussion kind (comment/question/answer/system); empty means
	// a plain comment, the schema's default. An unknown kind is refused rather
	// than stored.
	Kind domain.RequestCommentKind
	// Body is what was said. Required, non-blank, bounded like a brief
	// (domain.MaxRequestBriefLen) — the product's bound, tighter than 054's
	// generous backstop.
	Body string

	// Author is the display name captured at write time, and AuthorUserID the
	// AMP user behind it when there is one. Both are the caller's to supply:
	// a service client or connector posts as its own integration identity with
	// no AMP user, and the text snapshot is what keeps that row readable (and
	// what survives the user's deletion).
	Author       string
	AuthorUserID *int

	// RetryOfCommentID makes a retried append idempotent where the caller asks
	// for it. A caller that retries a submit sends the id the previous attempt
	// returned; when that row is this request's own comment the append returns
	// it unchanged — same body, same kind, and, most importantly, the ORIGINAL
	// attribution rather than a second row under whoever retried. When the id
	// names nothing this request owns (the earlier attempt never committed, or
	// it is another request's comment) the append proceeds as a fresh row,
	// because nothing about it is a retry of this one.
	//
	// 0 is "not a retry", the ordinary case. This is the "check before
	// retrying" contract (internal/mcp/robust.go) made mechanical, and it needs
	// no storage of its own: it names the record instead of a key.
	RetryOfCommentID int64
}

// AppendRequestComment writes one comment and returns the row as stored.
//
// The tenant is part of the insert, not a check around it: the row is selected
// from team_requests by (id, org, team) in the same statement that writes it,
// so a foreign or unknown request inserts nothing and answers ErrNotFound —
// the same single 404 the request surface uses (authz.RequestNotFound), with no
// window in between in which a row is written for a request the caller may not
// see.
//
// It returns exactly one comment row and touches nothing else: no state change,
// no event (a transition writes its own), no request.rev bump. That is what
// keeps a thread read from racing a triage decision.
func (r *Repo) AppendRequestComment(ctx context.Context, req AppendRequestCommentRequest) (*domain.RequestComment, error) {
	if req.RequestID <= 0 {
		// A non-positive id names no row. It is an ordinary missing request,
		// answered exactly like a wrong tenant (request_rows.go's rule).
		return nil, fmt.Errorf("comment on request %d: %w", req.RequestID, ErrNotFound)
	}
	if req.OrgID <= 0 || req.TeamID <= 0 {
		return nil, fmt.Errorf("comment on request %d: %w", req.RequestID, ErrRequestTenantScope)
	}

	// Validation is the domain's, so the repository cannot accept a kind or a
	// body the domain would refuse (the check constraints are 054's generous
	// backstops; these are the product's bounds).
	c := &domain.RequestComment{Kind: req.Kind, Body: req.Body}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("comment on request %d: %w", req.RequestID, err)
	}
	author := strings.TrimSpace(req.Author)
	if author == "" {
		// An unattributed row in a requester-visible thread is a bug, not a
		// state (054). Refusing here means the database's own non-blank CHECK
		// never has to be the thing that reports it.
		return nil, fmt.Errorf("comment on request %d: author is required: a comment says who said it", req.RequestID)
	}
	c.RequestID = req.RequestID
	c.Author = author
	c.AuthorUserID = req.AuthorUserID

	if req.RetryOfCommentID > 0 {
		existing, err := getRequestComment(ctx, r.db, req.RetryOfCommentID, req.RequestID, req.OrgID, req.TeamID)
		switch {
		case err == nil:
			// This request's own earlier attempt: answer with it, unchanged.
			return existing, nil
		case !errors.Is(err, ErrNotFound):
			// A real load failure must not be read as "no earlier attempt" and
			// turned into a second comment.
			return nil, err
		}
	}

	// The tenant rides in the INSERT's own SELECT, so the row is only written
	// when the request is in the caller's org and team. There is no
	// read-then-write window in which the tenant could change under us, and a
	// foreign request inserts nothing at all.
	insert := `INSERT INTO request_comments (request_id, kind, body, author, author_user_id)
		SELECT $1, $2, $3, $4, $5
		  FROM team_requests
		 WHERE id = $1 AND org_id = $6 AND team_id = $7
		RETURNING ` + requestCommentCols("")
	stored := &domain.RequestComment{}
	err := scanRequestComment(r.db.QueryRow(ctx, insert,
		c.RequestID, string(c.Kind), c.Body, c.Author, c.AuthorUserID, req.OrgID, req.TeamID), stored)
	if errors.Is(err, pgx.ErrNoRows) {
		// The INSERT ... SELECT matched no request row: no such request, or not
		// in this tenant. Both are the one 404, and the message names the id
		// only — never the tenant (request_rows.go's rule).
		return nil, fmt.Errorf("comment on request %d: %w", req.RequestID, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("append comment on request %d: %w", req.RequestID, err)
	}
	return stored, nil
}

// getRequestComment loads one comment of one request in one tenant. It backs the
// retry lookup and is not a general read: the thread read is
// ListRequestComments.
//
// It returns ErrNotFound for a comment that belongs to another request (even in
// the same team) as well as for one in another tenant, because both are "not
// this request's comment" and the retry path must not be able to tell them
// apart.
func getRequestComment(ctx context.Context, q queryRower, commentID int64, requestID, orgID, teamID int) (*domain.RequestComment, error) {
	if commentID <= 0 || requestID <= 0 {
		return nil, fmt.Errorf("comment %d on request %d: %w", commentID, requestID, ErrNotFound)
	}
	if orgID <= 0 || teamID <= 0 {
		return nil, fmt.Errorf("comment %d on request %d: %w", commentID, requestID, ErrRequestTenantScope)
	}
	query := `SELECT ` + requestCommentCols("c") + `
		  FROM request_comments c
		  JOIN team_requests r ON r.id = c.request_id
		 WHERE c.id = $1 AND c.request_id = $2 AND r.org_id = $3 AND r.team_id = $4`
	c := &domain.RequestComment{}
	err := scanRequestComment(q.QueryRow(ctx, query, commentID, requestID, orgID, teamID), c)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("comment %d on request %d: %w", commentID, requestID, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("get comment %d on request %d: %w", commentID, requestID, err)
	}
	return c, nil
}

// ListRequestComments returns one request's thread: append order (by id, never
// created_at, which several rows in one transaction share — 054's header), the
// order its index (request_id, id) serves.
//
// afterID pages forward: 0 starts at the oldest comment. limit bounds a page
// (0 means the default, above the ceiling is clamped), because a long thread is
// read a page at a time by both the team view and the requester's.
//
// The tenant is required and enforced by the join, so a foreign or unknown
// request yields an empty thread rather than another team's discussion; callers
// that need "does this request exist?" ask GetRequest, whose answer distinguishes
// missing-row from load failure (this read does not have to).
func (r *Repo) ListRequestComments(ctx context.Context, requestID, orgID, teamID int, afterID int64, limit int) ([]domain.RequestComment, error) {
	if requestID <= 0 {
		return nil, fmt.Errorf("comments of request %d: %w", requestID, ErrNotFound)
	}
	if orgID <= 0 || teamID <= 0 {
		return nil, fmt.Errorf("comments of request %d: %w", requestID, ErrRequestTenantScope)
	}
	if limit <= 0 {
		limit = requestListDefaultLimit
	}
	if limit > requestListMaxLimit {
		limit = requestListMaxLimit
	}
	query := `SELECT ` + requestCommentCols("c") + `
		  FROM request_comments c
		  JOIN team_requests r ON r.id = c.request_id
		 WHERE c.request_id = $1 AND r.org_id = $2 AND r.team_id = $3 AND c.id > $4
		 ORDER BY c.id
		 LIMIT $5`
	rows, err := r.db.Query(ctx, query, requestID, orgID, teamID, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("list comments of request %d: %w", requestID, err)
	}
	out, err := collect(rows, scanRequestComment)
	if err != nil {
		return nil, fmt.Errorf("list comments of request %d: %w", requestID, err)
	}
	return out, nil
}
