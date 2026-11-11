package repository

// Team requests: intake — one request per submission, however often it is
// retried (epic 202, story 535, AMP #3871; approved product contract artifact 36
// v2 §5 "Idempotency key on submit (replay returns the same request)", decision
// D12 for connector external ids; build slice C1 of
// discoveries/team-inbox-approved-build-slices.md).
//
// A submit is a network call whose answer can be lost: the requester's browser
// retries, the MCP client retries, the connector redelivers. Without a dedup
// mechanism every retry is a second request in a team's queue, which is exactly
// the duplicate the inbox exists to remove. There are two mechanisms, and this
// file owns the first:
//
//	the submit idempotency key   the client's own opaque token, scoped to
//	                             (org, team, source, submitter) — migration 061.
//	the source external id       the vendor's immutable object id, unique per
//	                             (team, source) by 053's partial index (D12).
//
// They differ in what a repeat means, and the difference is deliberate:
//
//   - A repeated KEY must be the same submission. Same key and the same
//     normalized payload is the replay the client asked for and answers with the
//     earlier request. Same key and a DIFFERENT payload is a bug on the client
//     side (a reused key, a mutated draft) and is refused with
//     RequestIdempotencyConflict rather than answering with content this caller
//     did not send.
//   - A repeated EXTERNAL ID is the same *object* seen again, not necessarily the
//     same submission: a connector's second event about one issue is still that
//     issue (the envelope slice appends it, it never creates a second request).
//     So an external-id repeat answers with the existing request whatever its
//     payload says.
//
// Why the key is scoped to the submitter, and what that buys. Keys are
// client-chosen and usually derived (`<form-id>-<draft-id>`, a request id from
// another system), so the same 36 characters can easily belong to two different
// people in one team's inbox. Including the authenticated submitter in the
// uniqueness scope means one person's key can never address, replay or reveal
// another person's request — the strongest of this slice's acceptance criteria,
// and the reason a "key" is never looked up across people. Team and source are
// in the scope for the same reason at the tenant level: a key replayed against
// another team creates that team's own request instead of answering with one in
// a tenant the caller may not read.
//
// Why there is no claim, and no state to get stuck in. `ClaimGitHubDelivery`
// (github_hooks.go) inserts a delivery row with status 0 and updates it when the
// attempt ends, because a webhook delivery must record an attempt that produced
// nothing and because a 5xx must be redeliverable. A submit has no such record to
// keep: the only thing a retry needs is "what did this key produce?", and the
// honest answer when the previous attempt died is "nothing". So the key row is
// written in the SAME TRANSACTION as the request it names and `request_id` is
// NOT NULL — there is no pending row, no lease, no expiry and no takeover rule,
// because there is no window in which one exists. A transaction that fails after
// both inserts rolls both back (the tests prove it by failing on the history
// row), and the retry simply creates the request. That is what "a failed
// transaction retry succeeds without a stuck claim" means here: the state that
// could stick was designed out.
//
// Concurrency. Two submissions with one key both insert their request and then
// race on 061's unique index: the loser's `INSERT ... ON CONFLICT DO NOTHING`
// writes nothing, and because its own request row must not survive either, it
// rolls its transaction back and resolves the winner's committed row — replay
// if the payload matches, conflict if it does not. Exactly one request row
// exists afterwards, and the losing transaction did not have to be retried by
// the caller to find that out.
//
// What this file deliberately does NOT do: decide who may submit (the service's
// authorization gate, slice S1/S3), mint the public reference (the service mints
// a random token; domain.NormalizeRequestPublicRef owns its shape), move the
// request out of `new` (the one transition function, slice S5), rate-limit
// anything (slice S1) or write audit_log rows (the service owns audit context).
// It writes exactly one request row, one submit history row and, when a key was
// sent, one key row — all or nothing.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/simstech/amp-api/internal/domain"
)

// ErrRequestSubmitter marks a keyed submission that did not say who is
// submitting. Like ErrRequestClaimant and ErrRequestTenantScope it reports a
// caller bug rather than a refusal worth showing a requester: an idempotency key
// is a *person's* token (061 scopes it by submitter), so a submission that has a
// key and no authenticated user has no scope to store it in. It is deliberately
// not ErrConflict and never ErrNotFound — a connector, which has no AMP user,
// dedups by its source external id instead (D12), so this refusal costs no real
// intake path anything.
var ErrRequestSubmitter = errors.New("an idempotency key is scoped to its submitter: pass the authenticated user, or leave the key empty and dedup by external id")

// RequestIdempotencyConflict reports a keyed submission refused because the key
// already produced a request from a DIFFERENT payload. Nothing was written: the
// earlier request, its history and its key row are all untouched.
//
// It carries the request the key already produced, because that is exactly what
// the caller needs to tell its client ("this key belongs to request 42, and you
// just sent something else"). Naming it leaks nothing: the key is only ever
// looked up within one submitter's own scope, so the request it names is the
// caller's own. It matches ErrConflict, so a handler that only distinguishes 409
// from success needs no special case (the `request.conflict` code, slice B4).
type RequestIdempotencyConflict struct {
	RequestID int
	// Submitter is the user the key belongs to; Source is the surface the key was
	// first used on. Both are in the key's scope, so neither can differ between
	// the stored row and the refused attempt — they are here so a log line or a
	// 409 body can say which namespace the key was in.
	Submitter int
	Source    domain.RequestSourceKind
}

func (e *RequestIdempotencyConflict) Error() string {
	return fmt.Sprintf("idempotency key already produced request %d for this submitter on the %s source: the same key with a different payload is a conflict, not a replay", e.RequestID, e.Source)
}

// Is makes errors.Is(err, ErrConflict) true for a reused key.
func (e *RequestIdempotencyConflict) Is(target error) bool { return target == ErrConflict }

// RequestPublicRefConflict reports a create refused because the public reference
// the caller minted is already stored. Requesters hold `public_ref` as the only
// handle they have on their request and it is unique across the whole table
// (053), so an intake path that mints one must be prepared for the collision —
// with 96 random bits it is rare to the point of never happening, but a create
// that reported it as a server fault would be a 500 the caller cannot act on.
//
// It deliberately does NOT match ErrConflict: the caller's request is not in
// conflict with anything it knew about, and answering a submitter with a 409
// would be wrong. A service retries the create with a freshly minted token, which
// is safe because the whole attempt rolled back.
type RequestPublicRefConflict struct{ PublicRef string }

func (e *RequestPublicRefConflict) Error() string {
	return fmt.Sprintf("public_ref %q is already in use: mint another reference and retry (this attempt wrote nothing)", e.PublicRef)
}

// errRequestIdempotencyKeyRaced is the internal signal that this transaction
// lost the key race: its `INSERT ... ON CONFLICT DO NOTHING` wrote no key row, so
// its request row must be rolled back and the winner's committed row resolved.
// It never escapes CreateRequest.
var errRequestIdempotencyKeyRaced = errors.New("idempotency key was claimed by a concurrent submission")

// CreateRequestInput is one submission: what the requester asked for, where it
// is going, and which surface it came from. The repository's input is already
// resolved — the service has authenticated the submitter, decided they may
// submit to this team (slice S1's TeamSubmit check) and minted the public
// reference; nothing here re-derives identity from content.
type CreateRequestInput struct {
	// The request's tenant, in both halves. Required: a create scoped by only one
	// of them could write into another team's queue.
	OrgID  int
	TeamID int

	// PublicRef is the opaque token the requester will hold (D1). The service
	// mints it (domain.NormalizeRequestPublicRef owns the shape); this repository
	// validates it through domain.TeamRequest.Validate and refuses a collision.
	PublicRef string

	// RequesterUserID is the authenticated submitter, and RequesterContact the
	// contact snapshot taken now — the durable record of who asked, which
	// survives the user's deletion (D2). A connector submission has no AMP user
	// and passes nil.
	RequesterUserID  *int
	RequesterContact string

	SourceKind domain.RequestSourceKind
	// SourceExternalID is a connector's vendor object id (D12). Empty for every
	// other source: domain.NormalizeRequestSource refuses it on web, and a
	// connector without it cannot ever recognise its own replay.
	SourceExternalID string

	Subject      string
	InputBrief   string
	OutcomeBrief string

	// Metadata is connector labels and routing hints only (domain.Metadata
	// limits apply); durable relations are link rows. nil is stored as `{}`.
	Metadata domain.Metadata

	// IdempotencyKey is the client's own token for this submission, empty when it
	// sent none. It is trimmed and bounded here; a keyed submission must name its
	// submitter (ErrRequestSubmitter).
	IdempotencyKey string

	// Actor and ActorUserID are who the submit is attributed to in the request's
	// history (the integration identity for a connector, the person for a web
	// submit). A blank actor is recorded as "system": history has no anonymous
	// rows (054), and the service passes the principal behind it.
	Actor       string
	ActorUserID *int
}

// requestInsert writes the request row from a submission and nothing else: the
// triage and lifecycle columns are deliberately absent, so every one of them is
// the schema's default (`new`, rev 1, no claim, no owner, no timestamps,
// `reopened_count` 0, `{}` metadata). An intake path cannot invent a claim, an
// assignee, a state or a revision because it never names those columns, and a
// future column with a default is picked up without this statement changing.
//
// RETURNING is requestCols — the one shared column list — so the row this returns
// is read back by the one shared scanner (request_rows.go), and a column added by
// a later migration is added there, once.
//
// The ON CONFLICT clause names 053's partial unique index for source identity and
// is the whole of the connector replay rule: a second submission with the same
// (team, source, external id) writes nothing at all rather than raising the
// unique violation it would otherwise raise. Empty external ids are excluded by
// the index predicate, so two web submits with no external id never suppress each
// other. Any *other* unique violation — only public_ref can be one here — still
// raises, and is translated for the caller below.
const requestInsert = `INSERT INTO team_requests (
		public_ref, org_id, team_id, requester_user_id, requester_contact,
		source_kind, source_external_id, subject, input_brief, outcome_brief, metadata)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
	ON CONFLICT (team_id, source_kind, source_external_id) WHERE source_external_id <> '' DO NOTHING
	RETURNING ` + requestCols

// CreateRequest stores one submission and reports whether this call created it.
//
//	created  true   this call wrote the request, its one `submit` history row and
//	                (when a key was sent) its key row.
//	created  false  a repeat: nothing at all was written, and the returned request
//	                is the one the earlier submission produced.
//
// Three answers are errors rather than replays, and they mean different things:
//
//	*RequestIdempotencyConflict  the same key now carries a different payload
//	*RequestPublicRefConflict    the minted public_ref is taken; mint another
//	ErrRequestSubmitter          a key with no submitter to scope it to
//	ErrRequestTenantScope        a create with no org or no team
//
// Everything else — a bad subject, a connector with no external id, a web submit
// that sends one — is domain validation (domain.TeamRequest.Validate), returned
// as itself with the field named, so intake's 400 says which field to fix.
func (r *Repo) CreateRequest(ctx context.Context, in CreateRequestInput) (*domain.TeamRequest, bool, error) {
	req, key, payloadHash, actor, err := prepareRequestCreate(in)
	if err != nil {
		return nil, false, err
	}

	// Fast path: a sequential retry (the common case — a lost response) finds its
	// key here and never touches the request table.
	if key != "" {
		existing, found, err := r.lookupRequestIdempotencyKey(ctx, in, key)
		if err != nil {
			return nil, false, err
		}
		if found {
			return resolveRequestIdempotencyKey(ctx, r, in, existing, payloadHash)
		}
	}

	stored, created, err := r.createRequestTx(ctx, in, req, key, payloadHash, actor)
	if errors.Is(err, errRequestIdempotencyKeyRaced) {
		// This transaction lost the race and has already rolled back; the winner
		// committed a key row, which the same resolution answers as a replay or a
		// conflict. It is read outside the rolled-back transaction, on the pool.
		existing, found, lookupErr := r.lookupRequestIdempotencyKey(ctx, in, key)
		if lookupErr != nil {
			return nil, false, lookupErr
		}
		if !found {
			// The winner's row was deleted in between (a cascade with its request).
			// Nothing is left to replay, so the honest answer is to let the caller
			// retry — which will now create the request.
			return nil, false, fmt.Errorf("create request in team %d: the idempotency key was claimed and then removed; retry", in.TeamID)
		}
		return resolveRequestIdempotencyKey(ctx, r, in, existing, payloadHash)
	}
	if err != nil {
		return nil, false, err
	}
	return stored, created, nil
}

// prepareRequestCreate validates the submission into the stored shape, and
// normalizes the key and the fingerprint. All of it happens before a transaction
// opens: a submission the domain refuses costs no connection and writes nothing.
//
// Validation is domain.TeamRequest.Validate, so the repository cannot accept a
// record the domain would refuse — the 053 CHECKs are generous backstops and
// these are the product's bounds (a 200-character subject, a 20000-character
// brief, a well-shaped external id).
func prepareRequestCreate(in CreateRequestInput) (*domain.TeamRequest, string, string, string, error) {
	if in.OrgID <= 0 || in.TeamID <= 0 {
		return nil, "", "", "", fmt.Errorf("create request in team %d: %w", in.TeamID, ErrRequestTenantScope)
	}
	key, err := normalizeRequestIdempotencyKey(in.IdempotencyKey)
	if err != nil {
		return nil, "", "", "", fmt.Errorf("create request in team %d: %w", in.TeamID, err)
	}
	if key != "" && in.RequesterUserID == nil {
		return nil, "", "", "", fmt.Errorf("create request in team %d: %w", in.TeamID, ErrRequestSubmitter)
	}
	metadata := in.Metadata
	if metadata == nil {
		metadata = domain.Metadata{}
	}
	// Clean applies domain.Metadata's limits (50 keys, 64-character keys,
	// 1000-character values) and drops nulls, so a connector's labels can never
	// store something the rest of AMP would refuse to read.
	metadata, err = metadata.Clean()
	if err != nil {
		return nil, "", "", "", fmt.Errorf("create request in team %d: %w", in.TeamID, err)
	}
	req := &domain.TeamRequest{
		PublicRef:        in.PublicRef,
		OrgID:            in.OrgID,
		TeamID:           in.TeamID,
		RequesterUserID:  in.RequesterUserID,
		RequesterContact: in.RequesterContact,
		SourceKind:       in.SourceKind,
		SourceExternalID: in.SourceExternalID,
		Subject:          in.Subject,
		InputBrief:       in.InputBrief,
		OutcomeBrief:     in.OutcomeBrief,
		// The record starts where intake lands it. Validate normalizes an empty
		// state to `new`; naming it here keeps the intent explicit.
		State: domain.RequestStatusNew,
		// The concurrency token starts at 1 (053's CHECK); Validate refuses 0.
		Rev:      1,
		Metadata: metadata,
	}
	if err := req.Validate(); err != nil {
		return nil, "", "", "", fmt.Errorf("create request in team %d: %w", in.TeamID, err)
	}
	actor := strings.TrimSpace(in.Actor)
	if actor == "" {
		actor = "system"
	}
	return req, key, requestPayloadFingerprint(req), actor, nil
}

// normalizeRequestIdempotencyKey trims the client's token and enforces 061's
// bound. Only a blank key is "no key": an all-spaces header is the same as no
// header rather than a key of spaces, and 255 characters is the conventional
// Idempotency-Key ceiling, which the column also enforces.
func normalizeRequestIdempotencyKey(key string) (string, error) {
	key = strings.TrimSpace(key)
	if len(key) > 255 {
		return "", fmt.Errorf("idempotency key is over 255 characters (got %d): send a short client-chosen token", len(key))
	}
	return key, nil
}

// requestPayloadFingerprint is the comparison a replayed key is judged by: sha256
// over the submission's NORMALIZED content, so " Hello " and "Hello" are the same
// payload (Validate trimmed them) and an edited brief is not.
//
// It covers every field the submitter supplied that the row stores. It does not
// cover the tenant or the submitter, because those are in the key's scope rather
// than its payload: a key presented in another team or by another person is not a
// replay to compare, it is a different key (and creates that team's own request).
// Timestamps, ids and state are excluded because intake does not accept them.
//
// json.Marshal sorts map keys, so two equivalent metadata maps encode identically
// and the hash depends on the content, not on map iteration order.
func requestPayloadFingerprint(req *domain.TeamRequest) string {
	payload := struct {
		RequesterContact string         `json:"requester_contact"`
		SourceKind       string         `json:"source_kind"`
		SourceExternalID string         `json:"source_external_id"`
		Subject          string         `json:"subject"`
		InputBrief       string         `json:"input_brief"`
		OutcomeBrief     string         `json:"outcome_brief"`
		Metadata         map[string]any `json:"metadata"`
	}{
		RequesterContact: req.RequesterContact,
		SourceKind:       string(req.SourceKind),
		SourceExternalID: req.SourceExternalID,
		Subject:          req.Subject,
		InputBrief:       req.InputBrief,
		OutcomeBrief:     req.OutcomeBrief,
		Metadata:         req.Metadata,
	}
	// The struct holds only strings and a metadata map Clean has already
	// validated, so encoding cannot fail; an empty payload is a safe fingerprint
	// of a bug rather than a reason to refuse a request the domain accepted.
	encoded, err := json.Marshal(payload)
	if err != nil {
		encoded = []byte("{}")
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// requestIdempotencyScope is one key row as resolved for comparison: the request
// it produced and the fingerprint it was recorded with.
type requestIdempotencyScope struct {
	requestID   int
	payloadHash string
}

// lookupRequestIdempotencyKey reads the key row for this submission's scope, if
// one is committed. The predicate is exactly 061's unique index — org, team,
// source, submitter, key — so a lookup can never see another person's or another
// team's row, and a miss means "this submission has not been made before".
func (r *Repo) lookupRequestIdempotencyKey(ctx context.Context, in CreateRequestInput, key string) (requestIdempotencyScope, bool, error) {
	var got requestIdempotencyScope
	err := r.db.QueryRow(ctx, `SELECT request_id, payload_hash FROM request_idempotency_keys
		 WHERE org_id = $1 AND team_id = $2 AND source_kind = $3 AND submitter_user_id = $4 AND idempotency_key = $5`,
		in.OrgID, in.TeamID, string(in.SourceKind), *in.RequesterUserID, key,
	).Scan(&got.requestID, &got.payloadHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return requestIdempotencyScope{}, false, nil
	}
	if err != nil {
		return requestIdempotencyScope{}, false, fmt.Errorf("look up idempotency key in team %d: %w", in.TeamID, err)
	}
	return got, true, nil
}

// resolveRequestIdempotencyKey answers a keyed submission whose key is already
// committed: the replay the client asked for when the payload matches, and
// RequestIdempotencyConflict when it does not.
//
// The request is loaded through the shared tenant-scoped read (getRequest →
// requestSelect + requestTenantWhere + scanRequest), so the row a replay returns
// is proven to be in the caller's org and team and is read with the same column
// list and scanner as every other request read. Nothing is written on either
// branch.
func resolveRequestIdempotencyKey(ctx context.Context, r *Repo, in CreateRequestInput, got requestIdempotencyScope, payloadHash string) (*domain.TeamRequest, bool, error) {
	if got.payloadHash != payloadHash {
		submitter := 0
		if in.RequesterUserID != nil {
			submitter = *in.RequesterUserID
		}
		return nil, false, &RequestIdempotencyConflict{RequestID: got.requestID, Submitter: submitter, Source: in.SourceKind}
	}
	req, err := getRequest(ctx, r.db, got.requestID, in.OrgID, in.TeamID)
	if err != nil {
		// A key row always names its request (061's NOT NULL foreign key with ON
		// DELETE CASCADE), so this is a load failure or a deletion in the instant
		// between the two reads — never a cross-tenant answer. It is returned as
		// it came so a cancelled context still reads as itself.
		return nil, false, fmt.Errorf("load request %d replayed by idempotency key: %w", got.requestID, err)
	}
	return req, false, nil
}

// createRequestTx is one attempt: the request, its submit history row and its key
// row, in one transaction. It returns errRequestIdempotencyKeyRaced when a
// concurrent submission owns the key — the transaction has then rolled back, so
// its request row does not exist and the caller resolves the winner's.
//
// The insert order matters and is the reason a lost race is clean: the request
// goes first because the key row needs its id (061's request_id is NOT NULL), and
// the key statement's `ON CONFLICT DO NOTHING` is what turns a simultaneous
// duplicate into a silent no-op instead of an aborted transaction. Nothing is
// committed before both are written, so the two rows exist or neither does.
func (r *Repo) createRequestTx(ctx context.Context, in CreateRequestInput, req *domain.TeamRequest, key, payloadHash, actor string) (*domain.TeamRequest, bool, error) {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	stored := &domain.TeamRequest{}
	err = scanRequest(tx.QueryRow(ctx, requestInsert,
		req.PublicRef, req.OrgID, req.TeamID, req.RequesterUserID, req.RequesterContact,
		string(req.SourceKind), req.SourceExternalID, req.Subject, req.InputBrief, req.OutcomeBrief, req.Metadata,
	), stored)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// The ON CONFLICT clause did nothing: this connector's (team, source,
		// external id) already exists, so this is the object seen again (D12).
		// Read the committed row — in this transaction, which sees it because the
		// conflict means its writer has committed — and answer with it. Nothing
		// this attempt wrote exists: the request insert wrote no row, and the
		// history and key rows have not run.
		existing, readErr := existingRequestBySource(ctx, tx, in)
		if readErr != nil {
			return nil, false, readErr
		}
		return existing, false, nil
	case err != nil:
		return nil, false, createRequestWriteError(err, in, req)
	}

	if err := insertRequestSubmitEvent(ctx, tx, stored, actor, in.ActorUserID); err != nil {
		return nil, false, err
	}

	if key != "" {
		tag, err := tx.Exec(ctx, `INSERT INTO request_idempotency_keys
				(org_id, team_id, source_kind, submitter_user_id, idempotency_key, payload_hash, request_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT (org_id, team_id, source_kind, submitter_user_id, idempotency_key) DO NOTHING`,
			stored.OrgID, stored.TeamID, string(stored.SourceKind), *in.RequesterUserID, key, payloadHash, stored.ID)
		if err != nil {
			return nil, false, fmt.Errorf("record idempotency key for request %d: %w", stored.ID, err)
		}
		if tag.RowsAffected() == 0 {
			// A concurrent submission with this key won and has committed. This
			// attempt's request row must not survive, so the transaction is rolled
			// back by the deferred Rollback above and the caller resolves the
			// winner: replay, or conflict if the payloads differ.
			return nil, false, errRequestIdempotencyKeyRaced
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, false, err
	}
	return stored, true, nil
}

// existingRequestBySource loads the request that already owns this submission's
// (team, source, external id), through the shared column list and scanner, with
// the tenant still in the predicate — so a connector can never be answered with a
// request outside its own org and team even though 053's index is keyed by team
// alone.
func existingRequestBySource(ctx context.Context, q queryRower, in CreateRequestInput) (*domain.TeamRequest, error) {
	req := &domain.TeamRequest{}
	err := scanRequest(q.QueryRow(ctx, requestSelect+
		` WHERE team_id = $1 AND source_kind = $2 AND source_external_id = $3 AND org_id = $4`,
		in.TeamID, string(in.SourceKind), strings.TrimSpace(in.SourceExternalID), in.OrgID), req)
	if errors.Is(err, pgx.ErrNoRows) {
		// The row that suppressed the insert was deleted between the two
		// statements (a tenant cascade). Report it as a missing request: the
		// caller's retry creates a fresh one.
		return nil, fmt.Errorf("request from source %s/%q in team %d: %w", in.SourceKind, in.SourceExternalID, in.TeamID, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("load request from source %s/%q in team %d: %w", in.SourceKind, in.SourceExternalID, in.TeamID, err)
	}
	return req, nil
}

// insertRequestSubmitEvent appends the one history row a submit gets (I6): action
// `submit`, no from/to state (054's CHECK allows states only on a transition), the
// actor as written, and a detail of ids only.
//
// It is its own function rather than a call into request_claims.go's
// insertRequestEvent because that helper's input is a claim write; keeping the
// submit's write here means the create path owns its single event row without
// editing a file another slice of this wave is working in. Both write the same
// columns of the same table, and 054's CHECKs are what keep them honest.
func insertRequestSubmitEvent(ctx context.Context, tx pgx.Tx, req *domain.TeamRequest, actor string, actorUserID *int) error {
	var id int64
	err := tx.QueryRow(ctx, `INSERT INTO request_events (request_id, org_id, team_id, action, actor, actor_user_id, detail)
		VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id`,
		req.ID, req.OrgID, req.TeamID, string(domain.RequestActionSubmit), actor, actorUserID,
		requestEventDetail(submitEventDetail{
			SourceKind:       string(req.SourceKind),
			SourceExternalID: req.SourceExternalID,
			RequesterUserID:  req.RequesterUserID,
		}),
	).Scan(&id)
	if err != nil {
		return fmt.Errorf("record submit event for request %d: %w", req.ID, err)
	}
	return nil
}

// submitEventDetail is what a submit history row explains: which surface it came
// from, the vendor object id when one identified it, and who submitted. Ids only
// — a request's history is requester-visible, so it never carries a name, an
// address or the request's content.
type submitEventDetail struct {
	SourceKind       string `json:"source_kind"`
	SourceExternalID string `json:"source_external_id,omitempty"`
	RequesterUserID  *int   `json:"requester_user_id"`
}

// createRequestWriteError translates the two writes a create can be refused by
// into errors a caller can act on, and leaves every other failure as itself so a
// dead connection or a cancelled context still reads as what it is.
//
//   - 23505 on team_requests' public_ref index: the caller's token is taken. The
//     transaction rolled back, so the caller mints another and retries.
//   - 23503 on the requester foreign key: the submitter's user row does not
//     exist. That is an ordinary not-found for that user, not a server fault —
//     and on a keyed create it must be reported before the key row is written,
//     which is why the request insert comes first.
func createRequestWriteError(err error, in CreateRequestInput, req *domain.TeamRequest) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505":
			if pgErr.ConstraintName == "uq_team_requests_public_ref" {
				return &RequestPublicRefConflict{PublicRef: req.PublicRef}
			}
			// Any other unique violation here is a bug in the insert (source
			// identity is already handled by ON CONFLICT), so it is reported as
			// itself rather than guessed at.
		case "23503":
			if in.RequesterUserID != nil && strings.Contains(pgErr.ConstraintName, "requester_user_id") {
				return fmt.Errorf("create request in team %d: user %d: %w", in.TeamID, *in.RequesterUserID, ErrNotFound)
			}
		}
	}
	return fmt.Errorf("create request in team %d: %w", in.TeamID, err)
}
