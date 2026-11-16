package service

// Team request submission: the one authorized intake path into a team's inbox
// (epic 202, story 540, AMP #3899; approved product contract artifact 36 v2 §2
// and §5; build slice S1 of discoveries/team-inbox-approved-build-slices.md).
//
// A submission is the only write on the inbox that arrives from outside the
// team, so three questions are answered here once, for every surface (web form,
// MCP tool, REST client, connector) that will call this method:
//
//  1. May this caller submit to this team? TeamSubmit, un-narrowed: a team
//     member or lead, an owner or admin of the team's org, or the holder of an
//     in-force submit grant — all re-read from the rows on this call, never
//     remembered (contract §2's per-request re-check). A submit-only API token
//     is bound to one team by its scope (authz/token_scope.go), so the same
//     check refuses it every other team's inbox, as "not found".
//  2. Who asked, and how did it arrive? The authenticated principal and its own
//     user row, plus the surface the adapter declares. Nothing in the payload
//     can name a requester, an org, a contact address, an actor or a project:
//     SubmitRequestInput has no such fields, and the record's requester columns
//     are written from the principal, never from input.
//  3. What does a repeat mean? The repository's idempotency contract (slice C1):
//     the same key with the same normalized payload is the replay the client
//     asked for and is answered with the earlier request; the same key with a
//     different payload is a conflict; a connector's external id answers with
//     the object that already exists.
//
// The order of the steps is deliberate, and every step is cheap before it is
// expensive: authorize; shape and validate in the domain (a 400 that names the
// field costs no quota and writes nothing); rate-limit (slice S1's limiter, one
// unit per attempt); then create, in the repository's single transaction.
//
// One ordering note, because request_limits.go's Refund documents both halves.
// A *known* submit key could be resolved by the repository's cheap key lookup
// before Allow, so a lost-response retry is answered with its request even when
// the caller has spent the window's last unit. This path does not do that: the
// probe is unexported in the repository's create slice, and exporting it is that
// slice's change rather than this one's. The refund below covers the same
// ground for every submission that reaches the create — a replay never spends
// quota in the end — so what is missing is only the narrower case of a retry
// that arrives when the budget is already exhausted, which the caller answers by
// sending the retry in the next window. request_limits.go's Refund says exactly
// this: "Refund covers the rest ... any ordering a caller chooses still charges
// a replay nothing in the end."
//
// What this file deliberately does not do — each one another slice's concern,
// and none of them reachable from here:
//
//   - no REST route, MCP tool or UI. Surfaces are thin adapters over this method
//     (§5: "one service layer, thin REST and MCP adapters").
//   - no lifecycle. A submission lands in `new` and nothing here moves it: the
//     one transition function (slice S5) owns every move, no state, claim or
//     assignee is written by intake, and nothing is planned, approved or
//     dispatched. Request text reaches agents as quoted data, never as
//     instructions (§3).
//   - no project. A submission picks no project and writes no link: `routes_to`
//     is the team's deliberate triage act (D9), never a side effect of intake.
//   - no attachments. Bytes are a separate upload path with their own capture
//     (§5); this file carries no file, no MIME type and no size accounting.
//   - no access granted. Submitting writes no org role, no team membership and
//     no project link, so a requester holding a grant still reaches zero
//     projects — the reason the grant exists instead of a team role (§2).
//   - no refusal auditing. Refusals are returned, and the listener that served
//     them records them once as access.denied (api/denied.go, mcp/denied.go),
//     the same rule the submit-grant administration service follows.

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/repository"
)

// maxRequestIdempotencyKeyLen is migration 061's bound on a client's key,
// restated here for one reason: a key over it is the caller's mistake, so it is
// refused as request.invalid naming the field, instead of reaching the
// repository and surfacing as a server fault. The repository's own check stays
// authoritative.
//
// The unit is BYTES, and the reason is a DIRECTION, not a shared unit: the two
// layers underneath do not count what this check counts.
//   - normalizeRequestIdempotencyKey in the repository measures len(key) —
//     bytes, the same measure as here.
//   - 061's request_idempotency_keys_key_length CHECK is SQL length() over a
//     TEXT column, which counts CHARACTERS under this deployment's UTF8
//     encoding (it counts bytes only under SQL_ASCII).
//
// A byte count is never smaller than the character count of the same string, so
// a key that fits 255 bytes fits 255 characters too: this check is the STRICTER
// side, and "this check passed ⇒ 061 holds" follows from that inequality alone.
// No key this check admits can be refused by the CHECK — the invariant the call
// site below states it must never break.
//
// The converse is deliberately not true, and does not need to be: a key of 200
// two-byte runes is 400 bytes and 200 characters, so it is refused here while
// 061 would have accepted it. That is the intended trade — the stricter side owns
// the refusal, so the caller gets the field-naming 400 instead of the CHECK's
// opaque error deeper in the write.
//
// Counting runes here would invert the direction: a key of ≤255 runes can be
// >255 bytes, so it would pass this check and be refused by the repository as an
// untranslated server fault. The mirror image is the trap worth remembering: had
// 061 counted the tighter side (say octet_length() ≤ 255 against a rune-based
// service bound), this same "we bound it upstream" reasoning would have been a
// live bug. Check which side is stricter; never assume both see the same unit.
const maxRequestIdempotencyKeyLen = 255

// requestPublicRefAttempts bounds the mint/retry loop for a reference
// collision. A 96-bit token colliding is beyond improbable; the bound exists so
// a systematically broken minter fails loudly instead of spinning.
const requestPublicRefAttempts = 3

// SubmitRequestInput is one submission: what the requester asked for, which
// inbox it is going to, and which surface carried it. It carries no identity,
// no tenant half and no project, and that absence is the point — everything the
// record says about *who* asked and *where it belongs* comes from the
// authenticated principal and the team row.
type SubmitRequestInput struct {
	// TeamID is the inbox the request is addressed to, and the only tenant field
	// a submission names. The org is the team's, read from the team row, so a
	// caller cannot aim a request at another tenant's org, and no project is
	// chosen: intake routes nothing (§3).
	TeamID int

	// Source is the surface this submission arrived on (web, mcp, api,
	// connector) and it is set by the adapter from the transport it served —
	// never from the payload, or a caller could claim to have arrived somewhere
	// it did not. Empty means web, the domain's default.
	Source domain.RequestSourceKind
	// SourceExternalID is a connector's vendor object id (D12): the immutable
	// id that recognises the connector's own replay. Domain validation insists a
	// connector sends one and a web form does not.
	SourceExternalID string

	Subject    string
	InputBrief string
	// Metadata is connector labels and routing hints only (domain.Metadata
	// bounds apply). It is never an authorization decision and never a typed
	// relation — durable relations are link rows, and this slice writes none.
	Metadata domain.Metadata

	// IdempotencyKey is the client's own opaque token for this submission,
	// empty when it sent none. It is scoped to (org, team, source, submitter) in
	// the database (061), so one person's key can never address another
	// person's request.
	IdempotencyKey string
}

// SubmitRequestResult is one submission's answer.
type SubmitRequestResult struct {
	// Request is the stored request: the one this call wrote, or — for a replay
	// — the one an earlier submission of the same key already wrote.
	Request *domain.TeamRequest
	// Created distinguishes the two: true when this call wrote the request (its
	// one `submit` history row and its audit row went with it), false when it
	// was a replay, which wrote nothing at all.
	Created bool
}

// SubmitRateLimited is the refusal a submission gets when the limiter says no
// (slice S1, request_limits.go). It carries the wait in whole seconds so a
// surface can set Retry-After without parsing the message, and wraps the coded
// 429 (request.rate_limited) so a client can branch on the code as usual:
//
//	var rl *service.SubmitRateLimited
//	if errors.As(err, &rl) { header("Retry-After", rl.RetryAfterS) }
type SubmitRateLimited struct {
	// RetryAfterS is how long until the window this key exhausted ends, in whole
	// seconds, never less than one.
	RetryAfterS int

	err error
}

func (e *SubmitRateLimited) Error() string { return e.err.Error() }

// Unwrap exposes the underlying coded authz error, so errors.As finds the
// request.rate_limited code and authz.StatusOf reads 429.
func (e *SubmitRateLimited) Unwrap() error { return e.err }

// SubmitRequest is the inbox's authorized intake: it checks p may submit to
// in.TeamID, snapshots the requester and the source from the authenticated
// principal, validates the submission in the domain, charges one unit of the
// submission limiter, and creates the request (or answers a replay with the
// request that key already produced).
//
// A created request is audited exactly once (`request.submit`, targeting the
// request) and carries exactly one `submit` history row, both written by the
// repository's single transaction and this call. A replay writes neither, and
// its limiter unit is refunded, so a submission the client retries costs one
// request, one history row, one audit row and no extra quota.
func (s *Service) SubmitRequest(ctx context.Context, p authz.Principal, in SubmitRequestInput) (*SubmitRequestResult, error) {
	team, err := s.loadSubmitTeam(ctx, p, in.TeamID)
	if err != nil {
		return nil, err
	}
	requester, contact, err := s.submitterIdentity(ctx, p)
	if err != nil {
		return nil, err
	}
	// Shaped and validated before the limiter, so a submission the domain
	// refuses is a 400 that names the field and spends no quota: a client
	// fumbling a form should never be rate-limited for it, and a validation
	// error must never arrive as a server fault.
	shaped, key, err := shapeSubmission(team, requester, contact, in)
	if err != nil {
		return nil, err
	}

	limitKey := RequestLimitKey{
		OrgID: team.OrgID, TeamID: team.ID, SubmitterID: p.UserID, Source: shaped.SourceKind,
	}
	now := s.now()
	if limit := s.requestLimits().Allow(limitKey, now); !limit.Allowed {
		return nil, &SubmitRateLimited{RetryAfterS: limit.RetryAfterSeconds(), err: limit.Refusal()}
	}

	req, created, err := s.createSubmission(ctx, p, shaped, key)
	if err != nil {
		return nil, submitRequestError(team.ID, err)
	}
	if !created {
		// The one submission the limiter must not charge: a replay is the same
		// submission seen twice, so charging it again would let a client's own
		// lost-response retry burn its budget (request_limits.go's Refund). The
		// refund is a decrement in the current window only, so it can never
		// carry quota into the next one.
		s.requestLimits().Refund(limitKey, now)
		// A replay is answered with the request the earlier submission produced
		// — but only when the caller may see it (see requireReplayVisible).
		if err := s.requireReplayVisible(ctx, p, req); err != nil {
			return nil, err
		}
		return &SubmitRequestResult{Request: req, Created: false}, nil
	}
	s.recordSubmission(ctx, p, team, req)
	return &SubmitRequestResult{Request: req, Created: true}, nil
}

// loadSubmitTeam loads the inbox a submission is aimed at and checks the
// caller's right to send there.
//
// The team is read first for a reason that is about data, not authorization:
// its org is the record's other tenant half (053's composite foreign key), and
// reading it from the row is what makes "never trust a client's org field"
// structural rather than a convention. The authorization is TeamSubmit's call
// with the same id, so it re-reads the membership and grant rows itself and
// exactly one implementation decides who may submit, scoped tokens included.
func (s *Service) loadSubmitTeam(ctx context.Context, p authz.Principal, teamID int) (*domain.Team, error) {
	team, err := s.Repo.GetTeam(ctx, teamID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			// The same answer TeamSubmit gives for a team that isn't there, so an
			// unauthorized caller cannot tell a missing team from one they may not
			// reach.
			return nil, authz.NotFound("team %d not found", teamID)
		}
		return nil, err
	}
	if err := s.Authz.TeamSubmit(ctx, p, team.ID); err != nil {
		return nil, err
	}
	return team, nil
}

// submitterIdentity is the immutable identity a submission is recorded under:
// the authenticated principal's own user id and the contact address AMP holds
// for that account. Neither value can be supplied, overridden or hinted at by
// the payload (SubmitRequestInput has no such fields), which is what makes
// requester and source attribution unspoofable.
//
// The contact is read from the user row rather than from the principal's
// claims: a claim is asserted at sign-in, and the row is what an admin corrects.
// The snapshot is taken now and kept even if the user is later deleted (D2), so
// a request always says who asked.
func (s *Service) submitterIdentity(ctx context.Context, p authz.Principal) (*int, string, error) {
	if !p.HasUser() {
		return nil, "", authz.Forbidden("submitting a request needs a signed-in AMP user: the requester is snapshotted at submit, and an unauthenticated caller has nothing to snapshot")
	}
	u, err := s.Repo.GetUserByID(ctx, p.UserID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, "", authz.Forbidden("submitting a request needs an AMP account, and this credential's user %d no longer exists", p.UserID)
		}
		return nil, "", err
	}
	contact, err := domain.NormalizeRequestContact(u.Email)
	if err != nil || contact == "" {
		return nil, "", authz.Forbidden("your AMP account has no contact address a request can record; ask an admin to set one")
	}
	id := p.UserID
	return &id, contact, nil
}

// shapeSubmission builds the record this submission would write, in the exact
// shape the repository will store, and validates it in the domain: one place
// (domain.TeamRequest.Validate) owns the bounds on subject, brief, contact,
// source and reference, so this service cannot accept a record the domain would
// refuse nor store one the schema's CHECKs would.
//
// Everything here is in-process and side-effect free. The public reference is
// minted before validation because the domain owns its shape; a minted token the
// domain would refuse (or would normalize differently) is minted again rather
// than stored in a form nobody asked for.
func shapeSubmission(team *domain.Team, requester *int, contact string, in SubmitRequestInput) (*domain.TeamRequest, string, error) {
	key := strings.TrimSpace(in.IdempotencyKey)
	if n := len(key); n > maxRequestIdempotencyKeyLen {
		return nil, "", authz.RequestInvalid("idempotency_key is over %d bytes (got %d): send a short client-chosen token", maxRequestIdempotencyKeyLen, n)
	}
	// Clean applies domain.Metadata's bounds (50 keys, 64-character keys,
	// 1000-character values) and drops nulls, so a connector's labels can never
	// store something the rest of AMP would refuse to read. The repository cleans
	// again — this call is what lets the refusal name the field as request.invalid
	// instead of arriving as a write failure.
	metadata := in.Metadata
	if metadata == nil {
		metadata = domain.Metadata{}
	}
	metadata, err := metadata.Clean()
	if err != nil {
		return nil, "", authz.RequestInvalid("%s", err)
	}
	ref, err := newRequestPublicRef()
	if err != nil {
		return nil, "", err
	}
	req := &domain.TeamRequest{
		PublicRef:        ref,
		OrgID:            team.OrgID,
		TeamID:           team.ID,
		RequesterUserID:  requester,
		RequesterContact: contact,
		SourceKind:       in.Source,
		SourceExternalID: in.SourceExternalID,
		Subject:          in.Subject,
		InputBrief:       in.InputBrief,
		// Intake lands a request in `new` with rev 1 and nothing else set: no
		// claim, no owner, no state-entry timestamp. 053's defaults say the same,
		// and naming them here keeps the intent explicit.
		State:    domain.RequestStatusNew,
		Rev:      1,
		Metadata: metadata,
	}
	if err := req.Validate(); err != nil {
		// The domain's message names the field ("subject is required: ...",
		// "input_brief is over 20000 characters (got 21000)") and never echoes
		// the rejected content back.
		return nil, "", authz.RequestInvalid("%s", err)
	}
	return req, key, nil
}

// newRequestPublicRef mints the opaque token a requester holds (D1): 96 random
// bits in base64url — never the serial and never a per-team sequence, so
// nothing about a request can be guessed from another one's reference. The
// database's unique index is what actually guarantees uniqueness; this function
// only guarantees the token is one the domain stores as minted.
func newRequestPublicRef() (string, error) {
	for range 8 {
		b := make([]byte, 12)
		if _, err := rand.Read(b); err != nil {
			return "", fmt.Errorf("mint a request reference: %w", err)
		}
		ref := base64.RawURLEncoding.EncodeToString(b)
		// NormalizeRequestPublicRef strips its display prefix and owns the shape
		// check; a candidate it would alter is not the token this function
		// minted, so it is thrown away rather than stored in another form.
		if norm, err := domain.NormalizeRequestPublicRef(ref); err == nil && norm == ref {
			return ref, nil
		}
	}
	return "", errors.New("mint a request reference: every candidate was refused by the domain's shape check")
}

// createSubmission writes the request through the repository and reports whether
// this call created it. A reference collision — the table-wide unique index
// caught a token this process minted — is retried with a fresh token: the
// colliding attempt wrote nothing at all (the whole transaction rolled back), so
// the retry is a first attempt with a different name. Any other answer is the
// repository's and is translated by submitRequestError.
func (s *Service) createSubmission(ctx context.Context, p authz.Principal, shaped *domain.TeamRequest, key string) (*domain.TeamRequest, bool, error) {
	for attempt := 1; ; attempt++ {
		req, created, err := s.Repo.CreateRequest(ctx, repository.CreateRequestInput{
			OrgID:            shaped.OrgID,
			TeamID:           shaped.TeamID,
			PublicRef:        shaped.PublicRef,
			RequesterUserID:  shaped.RequesterUserID,
			RequesterContact: shaped.RequesterContact,
			SourceKind:       shaped.SourceKind,
			SourceExternalID: shaped.SourceExternalID,
			Subject:          shaped.Subject,
			InputBrief:       shaped.InputBrief,
			Metadata:         shaped.Metadata,
			IdempotencyKey:   key,
			// The history row's actor is the authenticated caller — the person for
			// a web submission, the integration account for an agent or a
			// connector — and never a value from the payload.
			Actor:       p.Label(),
			ActorUserID: p.UserRef(),
		})
		var collision *repository.RequestPublicRefConflict
		if errors.As(err, &collision) && attempt < requestPublicRefAttempts {
			ref, mintErr := newRequestPublicRef()
			if mintErr != nil {
				return nil, false, mintErr
			}
			shaped.PublicRef = ref
			continue
		}
		return req, created, err
	}
}

// requireReplayVisible decides whether a replay may be answered with the request
// it resolved to.
//
// An idempotency replay always may: 061 scopes the key to its submitter, so the
// request it names is the caller's own. A connector's external-id repeat (D12)
// is different — that scope is (team, source, external id) and deliberately
// carries no submitter, because a vendor's object id identifies the *object*,
// not the person who sent it — so a second submitter can present an id that
// already belongs to someone else's request. Answering that with the record
// would hand the caller another requester's contact snapshot, which is exactly
// what must never cross requests.
//
// So the decision is the inbox's existing visibility rule rather than a new one
// (authz.RequestAccess): a team member may be answered with a request they could
// have read in the inbox anyway, and a requester who may not see it gets the
// surface's single not-found — the same bytes a request that does not exist
// produces, so an external id being someone else's object is not a fact this
// caller learns either.
func (s *Service) requireReplayVisible(ctx context.Context, p authz.Principal, req *domain.TeamRequest) error {
	view, err := s.Authz.RequestAccess(ctx, p, req)
	if err != nil {
		return err
	}
	if view.None() {
		return authz.RequestNotFound()
	}
	return nil
}

// recordSubmission appends the audit row a created request gets: one
// `request.submit` entry, on the org the team belongs to, targeting the request
// it wrote — the same identity the repository's `submit` history row carries, so
// the log and the request's own history name one record.
//
// It runs after the create committed, and how it fails is audit.go's rule: the
// change already happened, so a failure is logged rather than returned. A replay
// never reaches here, which is what keeps the log's count of submissions a count
// of requests instead of a count of retries.
//
// Whoever asked is a person (or the integration account acting for one), so the
// row names the principal through the audit writer's usual context (actor,
// channel, address, token); the details add only ids and the source, never a
// contact address or any of the request's content.
func (s *Service) recordSubmission(ctx context.Context, p authz.Principal, team *domain.Team, req *domain.TeamRequest) {
	s.audit(ctx, p, domain.AuditEntry{
		Action:     "request.submit",
		OrgID:      intRef(team.OrgID),
		TargetType: "request",
		TargetID:   idStr(req.ID),
		TargetName: req.PublicRef,
		Summary:    "submitted request " + req.PublicRef + " to team " + team.Name,
		Details: map[string]any{
			"team_id":           team.ID,
			"public_ref":        req.PublicRef,
			"source_kind":       string(req.SourceKind),
			"requester_user_id": p.UserID,
			// Written out rather than inferred: a submission row is a create, and
			// saying so keeps the log honest if a later slice ever records the
			// replay side too.
			"created": true,
		},
	})
}

// submitRequestError translates the answers a create can come back with into
// the surface's stable codes, and leaves every other failure as itself so a dead
// connection or a cancelled context still reads as what it is.
//
//   - a reused key carrying a different payload is request.conflict (409). It
//     names the request the key already produced, which is safe because the key
//     was only ever looked up inside this submitter's own scope (061). The 409
//     also keeps the repository's ErrConflict identity, so a caller that only
//     distinguishes "conflict" from success needs no special case.
//   - a key with no submitter to scope it to cannot happen here (a submission
//     always has one), but it is mapped rather than leaked as a server fault so
//     the guarantee is stated where a future caller reads it.
//   - a departed requester account is a not-found for that user: the row's
//     foreign key refused the write because it no longer exists.
func submitRequestError(teamID int, err error) error {
	var reused *repository.RequestIdempotencyConflict
	if errors.As(err, &reused) {
		return &codedCause{
			err:   authz.RequestConflict("this idempotency key already produced request %d from a different payload: send a new key for a different request", reused.RequestID),
			cause: repository.ErrConflict,
		}
	}
	if errors.Is(err, repository.ErrRequestSubmitter) {
		return authz.RequestInvalid("an idempotency key is scoped to its submitter: submit as a signed-in user, or send no key")
	}
	if errors.Is(err, repository.ErrNotFound) {
		return authz.NotFound("the requester's AMP account no longer exists")
	}
	return fmt.Errorf("submit request to team %d: %w", teamID, err)
}

// codedCause answers with err's surface shape — its status, its stable code and
// its message — while keeping cause in the error chain. It is what lets a
// refusal be both "409 request.conflict" to a surface and
// errors.Is(err, repository.ErrConflict) to a caller that only branches on the
// repository's two sentinels, without inventing a second kind of conflict.
type codedCause struct {
	err   error
	cause error
}

func (e *codedCause) Error() string   { return e.err.Error() }
func (e *codedCause) Unwrap() []error { return []error{e.err, e.cause} }

// now is the clock this path reads: Service.Now when a test injected one (the
// limiter needs the same instant for its window and its refund, so the submit
// path takes the time once per call rather than letting each step read its own).
func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}
