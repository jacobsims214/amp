package mcp

// Team inbox submissions over MCP (epic 202, story 543, AMP #3920; approved
// product contract artifact 36 v2 §2 and §5; the submit half of build slice I1
// in discoveries/team-inbox-approved-build-slices.md).
//
// One tool, amp_submit_request, and it is deliberately the only request tool in
// this package: reading an inbox, updating or commenting on a request, claiming
// it, attaching it to a project and adding attachments are later slices, and
// until each lands the submit-scope gate (token_scope.go, AMP #3772) refuses it
// — the list a bound credential sees and the gate that enforces both already
// name this one tool, so building anything else here would mean advertising a
// surface no scope was designed around.
//
// Three properties this adapter is responsible for, all of them about NOT
// adding a second definition of anything:
//
//  1. **The same submission REST makes.** The handler decodes arguments and
//     calls Service.SubmitRequest — the one authorized intake path (slice S1) —
//     so authorization (Checker.TeamSubmit, re-read per call), validation, the
//     contact snapshot, idempotent replay, the rate limiter and the audit row
//     are the service's, not this file's. Nothing is re-implemented here and
//     nothing is decided here: a drift between the two surfaces would have to
//     be a drift in the service, which both call.
//  2. **No project, and no project-shaped authority.** The tool runs behind
//     s.open, never s.guard: guard resolves a project from project_id/task_id
//     (server.go projectOf) and requires a Level on it, which is exactly the
//     capability a submission must not need — a customer, a connector and a
//     submit-only credential all reach a team's inbox with zero projects. The
//     tool's schema names a team and no project, so "which project?" is not a
//     question a caller can even be asked.
//  3. **Attribution and source come from the call, never the payload.** The
//     requester is the authenticated principal, the contact is its own user row
//     and the source is this transport (domain.RequestSourceMCP). The schema
//     declares none of those, and a payload that carries one is refused rather
//     than silently ignored (refuseSpoofedSubmitArgs) — a silently dropped
//     requester_contact would leave the caller believing a request came from
//     someone it did not.
//
// What this file does not do: no request list or get, no update, comment,
// claim, attach or attachment, no acceptance (which is the requester's alone,
// contract I1/D5, and has no tool anywhere), no planning, approval or dispatch.
// Submitting routes nothing: the team triages the request, and its own links and
// tickets are later slices with their own gates.

import (
	"context"
	"errors"
	"strings"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/service"
)

// registerRequests wires the team inbox's MCP surface. Today that is one tool;
// the rest of the inbox's tools (read, triage, link, attach) are separate
// slices, and each adds its own registration here.
func (s *Server) registerRequests(mcp *server.MCPServer) {
	ctx := context.Background()
	mcp.AddTool(tool("amp_submit_request",
		"Submit a work request to a team's inbox: the same submission POST /api/teams/{id}/requests makes over REST, and the "+
			"only request operation this surface has. Name the team and describe the work; no project is chosen, and submitting "+
			"routes, plans, approves and dispatches nothing — the team triages it. The requester, the contact address and the "+
			"source are taken from the authenticated call and cannot be set by the payload. "+
			"Retries: send the same idempotency_key and the call is answered with the request that key already produced "+
			"(created=false) instead of a second one; with no key, a retry submits twice. "+
			"Refusals carry REST's status and stable code in the error's data (request.invalid, request.conflict, "+
			"request.rate_limited with retry_after_s, request.not_found); a team you may not submit to answers not-found, "+
			"the same answer for every caller.",
		props{
			"team_id": num("REQUIRED. The team whose inbox receives the request. You must be a member or lead of the team, an " +
				"owner or admin of its org, or hold an in-force submit grant for it (amp_whoami lists your teams; grants are " +
				"administered by a team manager in the web UI)."),
			"subject": map[string]interface{}{"type": "string", "maxLength": domain.MaxRequestSubjectLen,
				"description": "REQUIRED. One line saying what the request is about, at most 200 characters. It is shown in the " +
					"team's queue, so write it as the work, not as a greeting."},
			"input_brief": map[string]interface{}{"type": "string", "maxLength": domain.MaxRequestBriefLen,
				"description": "Optional. The detail the team triages from: what is wanted, why, the constraints, links. Markdown, " +
					"at most 20000 characters. It is stored as the requester's words quoted as data — never as instructions to an " +
					"agent, and it can never name a project, an approver or an assignee."},
			"metadata": obj("Optional. Labels and routing hints only, e.g. {\"source_system\": \"pagerduty\", \"severity\": \"page\"}. " +
				"At most 50 keys of 64 characters with string/number/boolean values of at most 1000 characters. It is never an " +
				"authorization decision and never a durable relation: the team links work to its plan explicitly."),
			"idempotency_key": map[string]interface{}{"type": "string", "maxLength": 255,
				"description": "Optional. Your own short token for this submission, at most 255 bytes (ASCII is safest), " +
					"scoped to you, this team and the mcp source. A retry with the same key and the same payload is answered with the " +
					"same request and writes nothing; the same key with a different payload is refused request.conflict, so send a new " +
					"key for a different request."},
		}), wrap(ctx, s.open(s.submitRequest)))
}

// submitRequest is amp_submit_request's handler: decode, hand the one service
// call the arguments, answer with the request's public identity.
//
// The argument checks here are only about being a good adapter — a missing id,
// an identity field the payload may not carry, a metadata object the domain
// would refuse. Every product rule (who may submit, what a repeat means, the
// bounds on subject and brief, the limiter, the audit row) is the service's, so
// REST and MCP cannot disagree about them.
func (s *Server) submitRequest(rctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	teamID, err := reqInt(args, "team_id")
	if err != nil {
		return nil, err
	}
	subject, err := reqStr(args, "subject")
	if err != nil {
		return nil, err
	}
	if err := refuseSpoofedSubmitArgs(args); err != nil {
		return nil, submitToolError(err)
	}
	metadata, err := optNewMetadata(args)
	if err != nil {
		return nil, submitToolError(authz.RequestInvalid("%s", err))
	}
	res, err := s.svc.SubmitRequest(rctx, caller(rctx).p, service.SubmitRequestInput{
		TeamID: teamID,
		// The transport declares the source; the payload cannot (contract §5:
		// the adapter sets it from the transport it served). A connector's
		// vendor object id is the connector adapter's business and has no
		// argument here: an MCP client's retries are the idempotency key.
		Source:         domain.RequestSourceMCP,
		Subject:        subject,
		InputBrief:     optStr(args, "input_brief", ""),
		Metadata:       metadata,
		IdempotencyKey: optStr(args, "idempotency_key", ""),
	})
	if err != nil {
		return nil, submitToolError(err)
	}
	return jsonResult(submitResult(res))
}

// submitResult is the answer to a submission: the request's public identity and
// whether this call created it.
//
// It is deliberately not the stored record. A request row carries its org and
// team ids, the triage claim and the assignee — the team's internal state, which
// a requester must never see (contract §2; the requester projection is slice
// E3) — and a replay can resolve to a row triagers have touched since it was
// written. So the tool answers with what the caller already owns: the opaque
// reference (D1) they will quote, its state word, their own subject and the
// team they named. Nothing here can be used to enumerate the inbox.
func submitResult(res *service.SubmitRequestResult) map[string]interface{} {
	r := res.Request
	out := map[string]interface{}{
		"request": map[string]interface{}{
			"public_ref":  r.PublicRef,
			"state":       string(r.State),
			"subject":     r.Subject,
			"team_id":     r.TeamID,
			"source_kind": string(r.SourceKind),
			"created_at":  r.CreatedAt,
		},
		// created distinguishes the two answers a submission can get, and it is
		// the only thing that tells a client its retry did not double-submit.
		"created": res.Created,
	}
	if res.Created {
		out["next"] = "Submitted: the team triages it, and submitting routes, plans, approves and dispatches nothing. " +
			"Quote public_ref as the request's identity (the UI shows it as REQ-<public_ref>); it is an opaque token, and no " +
			"other request's reference or id can be derived from it."
	} else {
		out["next"] = "Replay of an earlier submission with this idempotency_key: this is that request, and no second one was " +
			"written. Send a new idempotency_key for a different request."
	}
	return out
}

// Two payloads are refused rather than ignored, and the two have different
// reasons, so they get different messages.
//
// spoofedSubmitArgs are facts about the authenticated call: who asked
// (requester_*), which tenant the request belongs to (org_id), how it arrived
// (source/source_kind) and who acted (actor/user_id). Contract §2 is explicit
// that nothing in the payload can name any of them, and none is declared by the
// tool's schema — so a caller sending one is either confused or attempting to
// spoof attribution, and either way deserves an answer rather than having the
// field quietly dropped.
var spoofedSubmitArgs = []string{
	"requester", "requester_user_id", "requester_contact", "requester_email",
	"org_id", "actor", "actor_user_id", "user_id", "source", "source_kind",
}

// connectorSubmitArgs are the connector adapter's arguments, which this tool
// does not have. A vendor's immutable object id dedups a connector's redelivery
// (D12) and is scoped (team, source, external id) rather than to a submitter, so
// accepting it here would hand an MCP client a deduplication rule it was never
// given — and one that can answer with another submitter's request. An MCP
// client's retries are the idempotency key, which is scoped to it.
var connectorSubmitArgs = []string{"source_external_id", "external_id"}

// projectSubmitArgs are the plan-shaped arguments a submission has no place for.
// Intake chooses no project and creates no ticket (contract §3: routing is the
// team's deliberate triage act, never a side effect of intake), so a caller that
// aimed a request at one would otherwise get a success and a request that is
// nowhere near the project it named.
var projectSubmitArgs = []string{"project_id", "story_id", "epic_id", "task_id"}

// refuseSpoofedSubmitArgs refuses a payload that tries to set one of the facts
// above. An argument present but null (or blank) is the absence it looks like,
// so it passes.
func refuseSpoofedSubmitArgs(args map[string]interface{}) error {
	for _, key := range spoofedSubmitArgs {
		if !argPresent(args, key) {
			continue
		}
		return authz.RequestInvalid("a submission records who asked, which team's org it belongs to and how it arrived "+
			"(the authenticated caller, the team's own org, the mcp source): %q cannot be set from the payload", key)
	}
	for _, key := range projectSubmitArgs {
		if !argPresent(args, key) {
			continue
		}
		return authz.RequestInvalid("a submission names no project: %q is not an argument, and submitting routes, plans and "+
			"dispatches nothing. Send the request to the team's inbox (team_id) and the team triages it", key)
	}
	for _, key := range connectorSubmitArgs {
		if !argPresent(args, key) {
			continue
		}
		return authz.RequestInvalid("%q belongs to a connector's source identity, not to an MCP submission: send an "+
			"idempotency_key, which is scoped to you and suppresses your own retries", key)
	}
	return nil
}

// argPresent reports whether args carries key with something in it. A null, an
// empty string or a blank string is the absence it looks like.
func argPresent(args map[string]interface{}, key string) bool {
	v, ok := args[key]
	if !ok || v == nil {
		return false
	}
	if s, isStr := v.(string); isStr {
		return strings.TrimSpace(s) != ""
	}
	return true
}

// requestToolError is a refusal reported the way REST's envelope reports one:
// the service's own message, plus the HTTP status and stable code in the MCP
// error's data ({"status": 400, "code": "request.invalid"}), so a client
// branches on the code instead of parsing a message that may be reworded — and
// on the same code it would read from REST (contract §5: one service layer,
// thin adapters, one set of request.* codes).
//
// It keeps the original error in its chain (Unwrap), so everything that already
// inspects a refusal — the denied middleware's 403 audit, authz.StatusOf and
// authz.CodeOf — sees exactly what it saw before this wrapper existed.
type requestToolError struct {
	err    error
	status int
	code   string
	// retryAfterS is a 429's wait, in whole seconds, so a client can sleep for
	// exactly as long as REST's Retry-After says.
	retryAfterS int
}

func (e *requestToolError) Error() string { return e.err.Error() }

// Unwrap exposes the refusal HTTP-shaped and coded, as the service returned it.
func (e *requestToolError) Unwrap() error { return e.err }

// JSONRPCError lets mcp-go carry the status and code to the client
// (server.NewHandlerError's provider interface). The JSON-RPC code stays
// mcpgo.INTERNAL_ERROR, which is what every other AMP tool refusal arrives as —
// the machine-readable half is data.status and data.code, exactly as REST's
// body has "code" next to "error". The provider's own id is ignored by mcp-go,
// which reports the request's.
func (e *requestToolError) JSONRPCError() mcpgo.JSONRPCError {
	data := map[string]any{"status": e.status}
	if e.code != "" {
		data["code"] = e.code
	}
	if e.retryAfterS > 0 {
		data["retry_after_s"] = e.retryAfterS
	}
	// The message is the service's own, unchanged: a model reads it and a person
	// can act on it (it names the field, never the rejected content).
	return mcpgo.NewJSONRPCError(mcpgo.NewRequestId(0), mcpgo.INTERNAL_ERROR, e.err.Error(), data)
}

// submitToolError translates a coded refusal into requestToolError, and leaves
// anything else alone.
//
// A fault (a dead connection, a cancelled context, a bug) has no status:
// authz.StatusOf falls back to the 0 passed here, so it travels to the client
// as itself, which is what makes an unexplained failure look unexplained rather
// than like a refusal the caller should act on.
func submitToolError(err error) error {
	status := authz.StatusOf(err, 0)
	if status == 0 {
		return err
	}
	var limited *service.SubmitRateLimited
	retryAfter := 0
	if errors.As(err, &limited) {
		retryAfter = limited.RetryAfterS
	}
	return server.NewHandlerError(&requestToolError{
		err: err, status: status, code: authz.CodeOf(err), retryAfterS: retryAfter,
	})
}
