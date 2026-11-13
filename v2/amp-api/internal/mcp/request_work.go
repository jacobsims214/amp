package mcp

// Team inbox request work over MCP: reading a queue and one request, and the
// four triage acts (epic 202, story 549, AMP #3969; approved product contract
// artifact 36 v2 §1, §2 and §5; build slice I2 of
// discoveries/team-inbox-approved-build-slices.md).
//
// Seven tools, each a thin adapter over one reviewed service method:
//
//	tool                        service
//	--------------------------  ---------------------------------------------
//	amp_list_requests           Service.ListRequests   (S2, request_views.go)
//	amp_get_request             Service.GetRequest     (S2, request_views.go)
//	amp_update_request          Service.EditRequest    (S3, request_mutations.go)
//	amp_comment_request     Service.CommentOnRequest (S3, request_mutations.go)
//	amp_claim_request           Service.ClaimRequest   (S4, request_mutations.go)
//	amp_release_request         Service.ReleaseRequestClaim (S4)
//	amp_assign_request          Service.AssignRequest  (S4)
//
// Three properties this file is responsible for:
//
//  1. **The same operations REST will expose.** Every handler decodes its
//     arguments and calls the service once. Who may read an inbox, who may
//     claim, who may assign, whether a write is a compare-and-set, what a stale
//     revision means, what a lost claim race says, whose voice a comment speaks
//     in, what a requester may see of their own request — all of it is the
//     service's, re-read from the rows on every call. Nothing is re-decided
//     here, so a drift between the two surfaces would have to be a drift in the
//     service both call.
//  2. **`s.open`, never `s.guard`.** A team inbox is reached by naming a team,
//     not a project: guard resolves a project from project_id/task_id
//     (server.go projectOf) and requires a Level on it, which is a capability
//     neither a triager nor a requester needs or has. The service authorizes
//     through authz.Checker (InboxRole, RequireTeamManager), which is the same
//     decision REST makes.
//  3. **No path to acceptance, delivery or a plan.** There is no acceptance
//     tool, because acceptance is the requester's own act in the web UI and
//     every credential is refused it (I1, D5). Nothing here calls
//     Service.TransitionRequest / AcceptRequest / DeliveryRequest, links a
//     request to planning, or touches an attachment: those are slices I3 and
//     F3/F4, and each has its own gate. An edit cannot write outcome_brief (the
//     delivery note) or metadata, because the service refuses them — this tool
//     hands those arguments over rather than dropping them, so a caller is told
//     why instead of believing it wrote something.
//
// The refusal envelope is amp_submit_request's (request_submit.go): a
// request.* refusal reaches an MCP client with REST's status and stable code in
// the error's data, and the error chain is left intact so the denied
// middleware's 403 audit still sees a 403. That is deliberate — one request
// surface must not have two error contracts.

import (
	"context"
	"strings"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/service"
)

// registerRequestWork wires the inbox's reading and triage tools. It is a
// separate function from registerRequests (request_submit.go) so this slice
// costs Server.Register one line and cannot collide with the submission tool's
// registration.
func (s *Server) registerRequestWork(mcp *server.MCPServer) {
	ctx := context.Background()
	mcp.AddTool(tool("amp_list_requests",
		"List a team's inbox queue: the work requests people and integrations have submitted to that team, newest first, with "+
			"a per-state count for each filter. This is a triager's read — you must be a member or lead of the team, an owner or "+
			"admin of its org, or a global admin. A submit grant lets someone file a request into the inbox; it never lets them "+
			"read the queue. Pages by keyset: pass the next_before_id you were given back as before_id for the next page. "+
			"Refusals carry REST's status and stable code in the error's data (request.not_found for a team you have no standing "+
			"on, request.invalid for a filter that is not a filter).",
		props{
			"team_id": num("REQUIRED. The team whose inbox you are reading."),
			"states": map[string]interface{}{"type": "array",
				"description": "Optional. Only requests in any of these lifecycle states. Omit for every state. Values: " +
					strings.Join(requestStatusNames(), ", ") + ".",
				"items": map[string]interface{}{"type": "string", "enum": requestStatusNames()}},
			"source_kind":       str("Optional. Only requests that arrived through one intake surface: web, mcp, api or connector."),
			"attached":          boolean("Optional. true lists only requests with work planned or routed (a link exists), false only those with nothing attached yet. Omit for both."),
			"q":                 str("Optional. Case-insensitive substring of the subject, reference or input brief. Wildcards are literal, so a subject saying 100% is found by 100%."),
			"mine":              boolean("Optional. Also return your own slice of this same queue — the requests you hold the triage claim on or own the outcome of — under \"mine\". Shared queues are not your tickets: a request is yours only if you claimed or were assigned it."),
			"requester_user_id": num("Optional. Only requests submitted by this AMP user — the triager asking 'what has this person sent us?'. It is a filter on the team's queue, never a widening of what you may read."),
			"before_id":         num("Optional. Keyset cursor: return rows older than this id. Use the previous page's next_before_id. Omit for the first page."),
			"limit":             num("Optional. Rows per page: default 50, at most 200."),
		}), wrap(ctx, s.open(s.listRequests)))

	mcp.AddTool(tool("amp_get_request",
		"Read one team request. Name it two ways, and the answer says which you got: team_id + request_id returns the team's "+
			"whole record (state, revision, claim, assignee, timestamps), which is what triage works from; public_ref returns the "+
			"requester's whitelist view of their own submission (state word, own words, shared discussion and outputs, never a "+
			"claim, an assignee, an org id or anything else internal). A request you may not see and a request that does not "+
			"exist are the same answer (request.not_found), so ids cannot be walked. The result's view field says which of the "+
			"two you received.",
		props{
			"request_id": num("Optional with team_id. The request's serial, as a queue row's id shows it. Needed for the team's view."),
			"team_id":    num("Optional. The inbox the request lives in: send it with request_id for the team's view. A requester's own read by public_ref names no team."),
			"public_ref": str("Optional. The request's opaque reference (the REQ- prefix is accepted): the requester's own way of naming it, which returns that requester's projection and never the record."),
		}), wrap(ctx, s.open(s.getRequest)))

	mcp.AddTool(tool("amp_update_request",
		"Edit a request's own words: its subject and its input brief. Only the person who submitted it may, and only while it is "+
			"still untriaged (state new, no claim, no assignee); once the desk has picked it up the content is fixed and a change "+
			"is said in the thread instead. A compare-and-set: send the expected_rev you read, and a stale revision is refused "+
			"request.conflict naming the revision the row is actually at — reload and reapply. It cannot set outcome_brief or "+
			"metadata: the outcome brief is the team's statement of what it delivered (a lifecycle act, never an edit) and "+
			"metadata is the team's and its integrations' labels, so both are refused rather than silently dropped.",
		props{
			"team_id":      num("REQUIRED. The inbox the request lives in."),
			"request_id":   num("REQUIRED. The request to edit."),
			"expected_rev": num("REQUIRED. The revision you read (the request's rev). A stale value is refused 409 request.conflict, which is what keeps an edit that raced a triage decision from overwriting anything."),
			"subject": map[string]interface{}{"type": "string", "maxLength": domain.MaxRequestSubjectLen,
				"description": "Optional. The new one-line subject. Omit to leave it as stored; a blank one is refused."},
			"input_brief": map[string]interface{}{"type": "string", "maxLength": domain.MaxRequestBriefLen,
				"description": "Optional. The new detail (markdown). Omit to leave it as stored; send \"\" to clear it."},
			"outcome_brief": map[string]interface{}{"type": "string", "maxLength": domain.MaxRequestBriefLen,
				"description": "Refused here. The delivery note is written by the team's delivery, not by an edit: this argument exists so that sending it is answered (request.invalid) instead of silently ignored."},
			"metadata": obj("Refused here. Metadata is connector labels and routing hints the team and its integrations own; an edit writes subject and input_brief only."),
		}), wrap(ctx, s.open(s.updateRequest)))

	mcp.AddTool(tool("amp_comment_request",
		"Add one row to a request's discussion, in your own voice. A triager posts comment or question; the request's own "+
			"requester posts comment or answer. A question is the desk asking the requester something and an answer is theirs to "+
			"give, so each is refused from the other side rather than putting words in someone's mouth. A comment never changes "+
			"the request's state: answering a question and returning the request to the queue is the requester's act in the web UI, "+
			"and an answer row posted here is only what was said. Repeats: pass the id a previous attempt returned as "+
			"retry_of_comment_id and the retry is answered with that row (wrote=false) instead of a second one.",
		props{
			"team_id":    num("REQUIRED. The inbox the request lives in."),
			"request_id": num("REQUIRED. The request to comment on."),
			"body": map[string]interface{}{"type": "string", "maxLength": domain.MaxRequestBriefLen,
				"description": "REQUIRED. What you are saying, as markdown. Bounded like a brief; it is stored and rendered as data, never as instructions."},
			"kind": map[string]interface{}{"type": "string",
				"description": "Optional. The voice the row speaks in: comment (default), question (a triager asking the requester to clarify), " +
					"or answer (the requester's own reply). system is never a caller's.",
				"enum": []string{"comment", "question", "answer"}},
			"retry_of_comment_id": num("Optional. The id an earlier attempt returned. A retry naming it answers with that same row — unchanged, with its original attribution — and writes nothing."),
		}), wrap(ctx, s.open(s.addRequestComment)))

	mcp.AddTool(tool("amp_claim_request",
		"Claim a request in a team's inbox for yourself: 'I am looking at it now'. A triager's act (team member or lead, org "+
			"owner or admin, global admin), always for the caller — claiming for someone else is an assignment, which is a "+
			"manager's explicit transfer. A claim held by someone else is refused 409 request.conflict naming the holder: there is "+
			"no expiry and no take-over, so work moves by releasing the claim or transferring the assignee. Claiming a request you "+
			"already hold is a no-op (changed=false) with the claim's age kept, so a retry after a lost response is safe. Send "+
			"expected_rev, when you have read the row, to have a stale claim refused instead of applied.",
		props{
			"team_id":      num("REQUIRED. The inbox the request lives in."),
			"request_id":   num("REQUIRED. The request to claim."),
			"expected_rev": num("Optional. The revision you read. When sent, the claim is refused 409 request.conflict if the row has moved since — send it whenever you read the request first."),
		}), wrap(ctx, s.open(s.claimRequest)))

	mcp.AddTool(tool("amp_release_request",
		"Release the claim you hold on a request. Only the holder's own release works: releasing someone else's claim is refused "+
			"409 request.conflict naming them, and a manager who needs a claim cleared transfers the assignee "+
			"(amp_assign_request), which clears it explicitly in the same write. A request you do not hold is already released, so "+
			"the call succeeds and changes nothing (changed=false) — a retry after a lost response is safe.",
		props{
			"team_id":      num("REQUIRED. The inbox the request lives in."),
			"request_id":   num("REQUIRED. The request whose claim to release."),
			"expected_rev": num("Optional. The revision you read. When sent, the release is refused 409 request.conflict if the row has moved since."),
		}), wrap(ctx, s.open(s.releaseRequest)))

	mcp.AddTool(tool("amp_assign_request",
		"Assign a request to a person, or leave it unowned: assignee is a user id, an email, \"me\" (you) or \"none\". A team "+
			"manager's act — a lead of the inbox's team, an owner or admin of its org, or a global admin — and a plain member "+
			"claims instead. A transfer clears any active triage claim in the same write, so a request never has a new owner beside "+
			"a stale claim, and the assignee must be able to see the inbox (handing work to someone who cannot open it is "+
			"refused). Assigning what is already set is a no-op (changed=false).",
		props{
			"team_id":      num("REQUIRED. The inbox the request lives in."),
			"request_id":   num("REQUIRED. The request to assign."),
			"assignee":     str(`REQUIRED. A user id, an email, "me" for yourself, or "none"/"unassigned" to leave the request unowned.`),
			"expected_rev": num("Optional. The revision you read. When sent, the transfer is refused 409 request.conflict if the row has moved since."),
		}), wrap(ctx, s.open(s.assignRequest)))
}

// requestStatusNames is the closed lifecycle vocabulary, in lifecycle order, for
// the states filter's enum and its description. It reads the domain's list so a
// state added there cannot leave this schema advertising a stale set.
func requestStatusNames() []string {
	out := make([]string, len(domain.RequestStatuses))
	for i, st := range domain.RequestStatuses {
		out[i] = string(st)
	}
	return out
}

// ---- The queue ----

// listRequests is amp_list_requests: one page of a team's queue, its per-state
// counts, and (when asked) the caller's own claimed/assigned slice.
//
// The filters are translated, never re-interpreted: states are normalized by the
// domain (so a typo is a named refusal instead of an empty queue), and the
// source kind is checked against the closed set the schema's CHECK pins. Both are
// done here rather than left to the repository because the repository reports
// them as an ordinary error, which would reach a client as an unexplained fault
// instead of request.invalid naming the filter.
func (s *Server) listRequests(ctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	teamID, err := reqInt(args, "team_id")
	if err != nil {
		return nil, err
	}
	states, err := optRequestStates(args, "states")
	if err != nil {
		return nil, workToolError(err)
	}
	sourceKind, err := optStrStrict(args, "source_kind",
		"the intake surface to filter by is one of web, mcp, api, connector; omit it to list every source")
	if err != nil {
		return nil, workToolError(err)
	}
	q, err := optStrStrict(args, "q", "a search string; omit it to list the whole queue")
	if err != nil {
		return nil, workToolError(err)
	}
	// A caller searching with the other obvious spelling is answered rather than
	// handed the unfiltered queue: a filter that silently widens the result is
	// the same lie a silently-dropped one is.
	if argPresent(args, "query") {
		return nil, workToolError(authz.RequestInvalid("the search filter is q, not query: send the words as q"))
	}
	beforeID, _, err := optIntStrict(args, "before_id", "the keyset cursor is a row id; omit it for the first page")
	if err != nil {
		return nil, workToolError(err)
	}
	limit, _, err := optIntStrict(args, "limit", "the page size is a number between 1 and 200; omit it for the default of 50")
	if err != nil {
		return nil, workToolError(err)
	}
	mine, _, err := optBoolStrict(args, "mine", "a filter flag; send true or false, or omit it")
	if err != nil {
		return nil, workToolError(err)
	}
	attached, attachedSet, err := optBoolStrict(args, "attached", "a filter flag; send true or false, or omit it")
	if err != nil {
		return nil, workToolError(err)
	}
	requesterID, requesterSet, err := optIntStrict(args, "requester_user_id", "an AMP user id; omit it to list every requester")
	if err != nil {
		return nil, workToolError(err)
	}

	kind, err := requestFilterSourceKind(sourceKind)
	if err != nil {
		return nil, workToolError(err)
	}
	in := service.RequestQueueInput{
		TeamID:       teamID,
		States:       states,
		SourceKind:   kind,
		Query:        q,
		BeforeID:     beforeID,
		Limit:        limit,
		Mine:         mine,
		MineBeforeID: 0,
		MineLimit:    0,
	}
	if attachedSet {
		v := attached
		in.Attached = &v
	}
	if requesterSet {
		v := requesterID
		in.RequesterUserID = &v
	}
	res, err := s.svc.ListRequests(ctx, caller(ctx).p, in)
	if err != nil {
		return nil, workToolError(err)
	}
	return jsonResult(res)
}

// requestFilterSourceKind normalizes the source filter against the closed set
// migration 053's CHECK pins. It is validated in the adapter for the same reason
// the states are: the repository shares one predicate builder with the write
// paths and reports an unknown source as an ordinary error, which a client would
// read as a fault rather than as the filter mistake it is. The two families of
// intake surface are the same four values every intake write uses, so a filter
// can never name a source a request could not have come from.
func requestFilterSourceKind(raw string) (domain.RequestSourceKind, error) {
	kind := domain.RequestSourceKind(strings.ToLower(strings.TrimSpace(raw)))
	if kind == "" {
		return "", nil
	}
	for _, known := range domain.RequestSourceKinds {
		if kind == known {
			return kind, nil
		}
	}
	names := make([]string, len(domain.RequestSourceKinds))
	for i, k := range domain.RequestSourceKinds {
		names[i] = string(k)
	}
	return "", authz.RequestInvalid("source_kind must be one of %s: omit it to list every source", strings.Join(names, ", "))
}

// optRequestStates reads the states filter as the domain's own values.
//
// An empty list means "every state", so an empty string inside the list is
// refused rather than normalized: the domain's NormalizeRequestStatus reads ""
// as the initial state (`new`), and a caller who sent a blank entry meant "no
// filter", not "only new".
func optRequestStates(args map[string]interface{}, key string) ([]domain.RequestStatus, error) {
	raw, ok := args[key]
	if !ok || raw == nil {
		return nil, nil
	}
	arr, isArr := raw.([]interface{})
	if !isArr {
		return nil, authz.RequestInvalid("%s must be an array of state names (got a %s): "+
			"send [\"new\", \"triaging\"], or omit it for every state", key, jsonKindOf(raw))
	}
	out := make([]domain.RequestStatus, 0, len(arr))
	for _, item := range arr {
		s, isStr := item.(string)
		if !isStr {
			return nil, authz.RequestInvalid("%s must contain state names (got a %s): values are %s",
				key, jsonKindOf(item), strings.Join(requestStatusNames(), ", "))
		}
		if strings.TrimSpace(s) == "" {
			// See the function comment: an empty entry would otherwise silently
			// become "new".
			return nil, authz.RequestInvalid("%s contains an empty state: omit %s to list every state, or name one of %s",
				key, key, strings.Join(requestStatusNames(), ", "))
		}
		st, err := domain.NormalizeRequestStatus(s)
		if err != nil {
			return nil, authz.RequestInvalid("%s: %s", key, err)
		}
		out = append(out, st)
	}
	return out, nil
}

// ---- One request ----

// getRequest is amp_get_request: the one authorized read of one request, in
// whichever of the two views the service's decision allows.
//
// The answer is assembled here rather than handed over as the service's struct
// because that struct carries both halves (a record field and a projection
// field), and an adapter that serialized it whole would hand a token every
// field of the struct, one of them empty — the shape this read exists to keep
// apart. So exactly one of them is answered, next to the view that produced it.
func (s *Server) getRequest(ctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	in := service.RequestReadInput{}
	teamID, teamSet, err := optIntStrict(args, "team_id", "the inbox the request lives in; omit it to read your own request by public_ref")
	if err != nil {
		return nil, workToolError(err)
	}
	if teamSet {
		in.TeamID = teamID
	}
	requestID, requestSet, err := optIntStrict(args, "request_id", "the request's serial; send it with team_id for the team's view")
	if err != nil {
		return nil, workToolError(err)
	}
	if requestSet {
		in.RequestID = requestID
	}
	publicRef, err := optStrStrict(args, "public_ref", "the requester's reference; omit it when you name the request by request_id")
	if err != nil {
		return nil, workToolError(err)
	}
	in.PublicRef = publicRef

	res, err := s.svc.GetRequest(ctx, caller(ctx).p, in)
	if err != nil {
		return nil, workToolError(err)
	}
	out := map[string]interface{}{"view": res.View.String()}
	switch {
	case res.Record != nil:
		out["request"] = res.Record
	case res.Projection != nil:
		// The requester's whitelist: no claim, assignee, org, metadata or audit
		// detail has a field to reach here (the service builds it), and the
		// adapter does not add one.
		out["request"] = res.Projection
	default:
		// Unreachable through GetRequest, which refuses both halves empty. Fail
		// closed rather than inventing an empty request.
		return nil, workToolError(authz.RequestNotFound())
	}
	return jsonResult(out)
}

// ---- The triage acts ----

// updateRequest is amp_update_request: a submitter's versioned edit of their own
// request, while it is still untriaged.
//
// outcome_brief and metadata are read and handed over, not dropped. The service
// refuses both, and its refusal is the honest answer: a caller that sent
// outcome_brief believing it wrote the delivery note would otherwise get a
// success for an edit that wrote something else.
func (s *Server) updateRequest(ctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	teamID, err := reqInt(args, "team_id")
	if err != nil {
		return nil, err
	}
	requestID, err := reqInt(args, "request_id")
	if err != nil {
		return nil, err
	}
	// expected_rev is required here, and read strictly: reqInt's own message has
	// no code, and a caller branching on request.invalid must not have to parse
	// prose for the one argument this tool cannot do without.
	expectedRev, revSet, err := optIntStrict(args, "expected_rev",
		"an edit needs the revision you read, so a stale write is a conflict instead of a lost update")
	if err != nil {
		return nil, workToolError(err)
	}
	if !revSet {
		// Absent and present-null are both "no revision given" (null is absence
		// everywhere on this surface), and for this tool that is a required
		// argument missing rather than a default: the service refuses a zero rev
		// too, and saying it here names the argument a model has to send.
		return nil, workToolError(authz.RequestInvalid("expected_rev is required on an edit: send the rev you read, so an edit that raced a triage decision is a conflict instead of a stale write"))
	}
	subject, err := optStrPtrStrict(args, "subject",
		"omit it to leave the subject as stored; send a string to change it (a blank subject is refused)")
	if err != nil {
		return nil, workToolError(err)
	}
	inputBrief, err := optStrPtrStrict(args, "input_brief",
		"omit it to leave the brief as stored; send \"\" to clear it")
	if err != nil {
		return nil, workToolError(err)
	}
	outcomeBrief, err := optStrPtrStrict(args, "outcome_brief",
		"the delivery note is not an edit's field; this argument is refused by the service")
	if err != nil {
		return nil, workToolError(err)
	}
	metadata, err := optMetadataPatch(args)
	if err != nil {
		return nil, workToolError(authz.RequestInvalid("%s", err))
	}

	updated, changed, err := s.svc.EditRequest(ctx, caller(ctx).p, teamID, requestID, expectedRev, service.RequestEditInput{
		Subject:      subject,
		InputBrief:   inputBrief,
		OutcomeBrief: outcomeBrief,
		Metadata:     metadata,
	})
	if err != nil {
		return nil, workToolError(err)
	}
	return jsonResult(map[string]interface{}{"request": updated, "changed": changed})
}

// addRequestComment is amp_comment_request: one row in the request's own
// voice, attributed by the service from the authenticated principal.
func (s *Server) addRequestComment(ctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	teamID, err := reqInt(args, "team_id")
	if err != nil {
		return nil, err
	}
	requestID, err := reqInt(args, "request_id")
	if err != nil {
		return nil, err
	}
	body, err := reqStr(args, "body")
	if err != nil {
		return nil, err
	}
	kind, err := optStrStrict(args, "kind",
		"the voice is comment, question or answer; omit it for comment")
	if err != nil {
		return nil, workToolError(err)
	}
	retryOf, _, err := optIntStrict(args, "retry_of_comment_id",
		"the earlier attempt's comment id is a number; omit it on a first attempt")
	if err != nil {
		return nil, workToolError(err)
	}

	stored, wrote, err := s.svc.CommentOnRequest(ctx, caller(ctx).p, teamID, requestID, service.RequestDiscussionInput{
		Kind:             domain.RequestCommentKind(strings.ToLower(strings.TrimSpace(kind))),
		Body:             body,
		RetryOfCommentID: int64(retryOf),
	})
	if err != nil {
		return nil, workToolError(err)
	}
	return jsonResult(map[string]interface{}{"comment": stored, "wrote": wrote})
}

// claimRequest is amp_claim_request: the caller takes the request's triage claim.
func (s *Server) claimRequest(ctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	teamID, requestID, expectRev, err := requestWorkTarget(args)
	if err != nil {
		return nil, err
	}
	updated, changed, err := s.svc.ClaimRequest(ctx, caller(ctx).p, teamID, requestID, expectRev)
	if err != nil {
		return nil, workToolError(err)
	}
	return jsonResult(map[string]interface{}{"request": updated, "changed": changed})
}

// releaseRequest is amp_release_request: the holder ends their own claim.
func (s *Server) releaseRequest(ctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	teamID, requestID, expectRev, err := requestWorkTarget(args)
	if err != nil {
		return nil, err
	}
	updated, changed, err := s.svc.ReleaseRequestClaim(ctx, caller(ctx).p, teamID, requestID, expectRev)
	if err != nil {
		return nil, workToolError(err)
	}
	return jsonResult(map[string]interface{}{"request": updated, "changed": changed})
}

// assignRequest is amp_assign_request: a manager's explicit transfer, which also
// clears whatever claim the request still carries.
func (s *Server) assignRequest(ctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	teamID, requestID, expectRev, err := requestWorkTarget(args)
	if err != nil {
		return nil, err
	}
	assignee, err := reqStr(args, "assignee")
	if err != nil {
		return nil, err
	}
	updated, changed, err := s.svc.AssignRequest(ctx, caller(ctx).p, teamID, requestID, assignee, expectRev)
	if err != nil {
		return nil, workToolError(err)
	}
	return jsonResult(map[string]interface{}{"request": updated, "changed": changed})
}

// requestWorkTarget reads the (team, request, revision) triple the three triage
// acts share. The revision is optional for the claim, release and assignment
// because that is the service's own contract: zero means "the revision this call
// reads", which is honest for a replay-tolerant caller, while a value the caller
// read turns the write into a compare-and-set. It is refused when it is present
// but not a number, because a dropped revision would silently remove the
// caller's protection instead of being the absence it looked like.
func requestWorkTarget(args map[string]interface{}) (teamID, requestID, expectRev int, err error) {
	if teamID, err = reqInt(args, "team_id"); err != nil {
		return 0, 0, 0, err
	}
	if requestID, err = reqInt(args, "request_id"); err != nil {
		return 0, 0, 0, err
	}
	rev, _, err := optIntStrict(args, "expected_rev",
		"omit it to write against the revision this call reads, or send the rev you read to have a stale write refused")
	if err != nil {
		return 0, 0, 0, workToolError(err)
	}
	return teamID, requestID, rev, nil
}

// ---- Argument readers that refuse instead of coercing ----

// These four readers exist for one reason, the rule in
// architecture/mcp-argument-typing.md: a value that vanishes changes what the
// caller believes it did. A dropped expected_rev removes a compare-and-set; a
// dropped limit silently returns a different page; a dropped attached flips the
// queue the caller asked for back to the unfiltered one. None of those are
// harmless defaults, so a present-but-wrong-typed value is refused (400
// request.invalid naming the argument and its JSON type, never its value).
//
// optStrStrict (request_submit.go) is the same rule for optional strings and is
// reused here.

// optIntStrict reads an optional integer argument. ok is false when the argument
// is absent or null — both are absence, as on every other AMP tool.
func optIntStrict(args map[string]interface{}, key, why string) (v int, ok bool, err error) {
	raw, present := args[key]
	if !present || raw == nil {
		return 0, false, nil
	}
	switch n := raw.(type) {
	case float64:
		// JSON numbers arrive as float64. A fractional value cannot be an id, a
		// revision or a page size; truncating it would write against a revision
		// the caller never read.
		if n != float64(int(n)) {
			return 0, false, authz.RequestInvalid("%s must be a whole number (got %v): %s", key, n, why)
		}
		return int(n), true, nil
	case int:
		return n, true, nil
	case int64:
		return int(n), true, nil
	}
	return 0, false, authz.RequestInvalid("%s must be a number (got a %s): %s", key, jsonKindOf(raw), why)
}

// optBoolStrict reads an optional boolean argument.
func optBoolStrict(args map[string]interface{}, key, why string) (v, ok bool, err error) {
	raw, present := args[key]
	if !present || raw == nil {
		return false, false, nil
	}
	b, isBool := raw.(bool)
	if !isBool {
		return false, false, authz.RequestInvalid("%s must be true or false (got a %s): %s", key, jsonKindOf(raw), why)
	}
	return b, true, nil
}

// optStrPtrStrict reads an optional string argument and keeps "absent" apart
// from "present but empty", which is the difference an edit's clear-the-field
// needs: nil leaves the field as stored, a pointer to "" clears it.
func optStrPtrStrict(args map[string]interface{}, key, why string) (*string, error) {
	raw, present := args[key]
	if !present || raw == nil {
		return nil, nil
	}
	s, isStr := raw.(string)
	if !isStr {
		return nil, authz.RequestInvalid("%s must be a string (got a %s): %s", key, jsonKindOf(raw), why)
	}
	return &s, nil
}

// ---- The refusal envelope ----

// workToolError reports a refusal from one of these tools exactly as
// amp_submit_request reports its own: submitToolError owns the mapping (the
// service's message plus REST's status and stable code in the error's data, and
// the retry wait on a 429), and a second copy here would be a second definition
// of the contract a client reads. Faults stay unwrapped so an unexplained
// failure keeps looking unexplained.
func workToolError(err error) error { return submitToolError(err) }

// requestWorkToolNames is the set of tools this file registers. It exists so a
// test can assert the registered set, the catalog's entries and the
// submit-scope's negative all describe the same surface, and so that "there is
// no acceptance, delivery or attachment tool here" is checkable rather than a
// promise in a comment.
func requestWorkToolNames() []string {
	return []string{
		"amp_list_requests",
		"amp_get_request",
		"amp_update_request",
		"amp_comment_request",
		"amp_claim_request",
		"amp_release_request",
		"amp_assign_request",
	}
}
