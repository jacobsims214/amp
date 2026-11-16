package mcp

// Team inbox planning and attachments over MCP: attaching a request to the work
// that serves it, planning a batch of tickets for it, and putting a requester's
// private file on it (epic 202, story 549, AMP #3972; approved product contract
// artifact 36 v2 §1/§3/§5; build slice I3 of
// discoveries/team-inbox-approved-build-slices.md).
//
// Three tools, each a thin adapter over one reviewed service method:
//
//	tool                          service                        slice
//	----------------------------  -----------------------------  -----
//	amp_attach_request            Service.AttachRequest          E1 (#3962)
//	amp_handoff_request           Service.HandoffRequest         E1 (#3965)
//	amp_add_request_attachment    Service.AddRequestAttachment   G2 (#3961)
//
// Five properties this file is responsible for, all of them about what a caller
// can and cannot say through these three tools:
//
//  1. **The same services REST will expose (slice F5).** Every handler decodes
//     its arguments and calls the service once. Who may attach, which link kinds
//     a pair of arguments denotes, whether a request is already routed, whether a
//     replay writes anything, how a partial planning batch reconciles, what a
//     file's sniffed type is, what its scan state is and whether it is held —
//     all of it is the service's, re-read from the rows on every call. Nothing
//     is decided here, so a drift between the two surfaces would have to be a
//     drift in the service both call.
//  2. **`s.open`, never `s.guard`.** An inbox is reached by naming a team, not a
//     project: guard resolves a project from project_id/task_id and requires a
//     Level on it, which a triager working a request does not hold and a
//     requester never will. The services authorize (InboxRole, RequestUpload-
//     CapabilityFor, RequireProject on the *target* of a link) from the rows.
//  3. **No hidden execute or approval semantics.** A handoff plans tickets; it
//     never approves or dispatches one, and the ticket's description carries the
//     requester's words as quoted untrusted data (domain.HandoffBrief). This file
//     offers no approve/dispatch/assign/schedule argument at all — and the ones a
//     caller might try anyway (assigned_to, owner, start_at, metadata, approve,
//     dispatch, agent_id) are **refused by name** rather than silently dropped,
//     because a dropped `start_at` leaves the caller believing it scheduled work
//     that the service's own input type has no field for.
//  4. **No arbitrary remote fetch.** Bytes arrive in the call or they do not
//     arrive: the attachment tool has no url/uri/path argument, imports no
//     network package, and refuses a payload that names one. That is an SSRF
//     posture, not a style preference (056's header makes the same argument for
//     the table: it has no url column at all).
//  5. **No acceptance, anywhere.** `requester_accepted` is the requester's own
//     act in the web UI (invariant I1, decision D5); AcceptRequest refuses a
//     non-person caller before loading the request, and there is no tool that
//     reaches it. Nothing here can set an outcome for a requester.
//
// The refusal envelope is amp_submit_request's (request_submit.go), reused
// through workToolError: a request.* refusal reaches an MCP client with REST's
// status and stable code in the error's data. One request surface must not have
// two error contracts.

import (
	"context"
	"encoding/base64"
	"strconv"
	"strings"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/service"
)

// maxHandoffBatch is the most items one handoff may carry, and it is
// Service.CreateTasks' own bound (plan.go: "at most 100 tasks per batch")
// restated here for one reason only: an over-size batch is then refused with
// request.invalid naming the bound, instead of reaching the client as an uncoded
// 400 from deep inside the planning path. A batch that is over it cannot be
// planned by any caller, so this refuses nothing the service would have allowed.
const maxHandoffBatch = 100

// registerRequestPlanning wires the inbox's link, planning and attachment tools.
// It is a separate function so this slice costs Server.Register one line and
// cannot collide with the reading/triage tools' registration (#3969) or the
// submission tool's (#3920).
func (s *Server) registerRequestPlanning(mcp *server.MCPServer) {
	ctx := context.Background()
	mcp.AddTool(tool("amp_attach_request",
		"Link a team request to the work that serves it, or to the project it is routed to. Three relations and what they mean: "+
			"routes_to (target_kind project) is the ONE project this request belongs to — a second, different project is refused "+
			"409 request.conflict naming the current one, and moving it deliberately means sending supersede_route, which appends "+
			"the new routing and keeps the old one as history; planned_as (target_kind story, epic or task) is a plan item serving "+
			"the request; delivered_by (target_kind task, artifact or receipt) is evidence a later delivery may count. Linking is "+
			"triage — a member or lead of the inbox's team, an owner or admin of its org, or a global admin — and the target must "+
			"be work you can see in the request's own org, or it answers 404 exactly as a target that does not exist. Attaching is "+
			"idempotent: re-sending the same relation writes nothing (changed=false) and answers with the row that already exists, "+
			"even if its target has since been deleted. It approves nothing and dispatches nothing. A duplicate_of relation and a "+
			"link to another request are refused by name — each is its own act with its own rules.",
		props{
			"team_id":    num("REQUIRED. The inbox the request lives in."),
			"request_id": num("REQUIRED. The request to link."),
			"link_kind": map[string]interface{}{"type": "string",
				"description": "REQUIRED. The relation: routes_to (the one project), planned_as (a plan item), or delivered_by " +
					"(delivery evidence). duplicate_of and relates_to are named here so the answer is explained, but this tool " +
					"refuses both: folding a duplicate moves the request's state (the lifecycle's act) and a relation between " +
					"two requests is a different act with its own visibility rule.",
				"enum": requestLinkKindNames()},
			"target_kind": map[string]interface{}{"type": "string",
				"description": "REQUIRED. What the relation points at. Which kinds each relation accepts is fixed by the contract: " +
					"routes_to -> project; planned_as -> story, epic or task; delivered_by -> task, artifact or receipt. " +
					"A pair outside that matrix is refused request.invalid.",
				"enum": requestTargetKindNames()},
			"target_id": num("REQUIRED. The id of the target: a project, story, epic, task, artifact or receipt you can see in the request's org."),
			"supersede_route": boolean("Optional, default false. Only meaningful for routes_to: false (the default) refuses a " +
				"second, different project with 409 request.conflict naming the current one; true moves the request deliberately, " +
				"appending the new routing and keeping the previous one as history."),
		}), wrap(ctx, s.open(s.attachRequest)))

	mcp.AddTool(tool("amp_handoff_request",
		"Plan a batch of tickets for a team request and link each one to it as planned_as. This is the one path that turns a "+
			"request into plan items: the tickets are created in the project the request is ROUTED to (route it first), and every "+
			"description quotes the requester's own words as untrusted data — the request is evidence for the plan, never "+
			"instructions, and it cannot decide who does the work, when it runs or whether a plan is approved. Nothing is approved "+
			"and nothing is dispatched: each created ticket waits for a person to approve it, exactly as if it had been planned by "+
			"hand. Retries: send the same handoff_key and the batch is reconciled against what the previous attempt created, so a "+
			"lost response does not plan twice (created=false on the items that already exist); send a new key for a deliberate "+
			"re-plan. A 409 request.conflict means another handoff of this request is running — retry with the same key. A batch "+
			"that stops part-way says how many tickets it made and is safe to retry as it is.",
		props{
			"team_id":    num("REQUIRED. The inbox the request lives in."),
			"request_id": num("REQUIRED. The request to plan."),
			"handoff_key": map[string]interface{}{"type": "string", "maxLength": domain.MaxRequestHandoffKeyLen,
				"description": "REQUIRED. Your own idempotency token for THIS batch: letters, digits, '-' and '_', at most " +
					strconv.Itoa(domain.MaxRequestHandoffKeyLen) + " characters (a UUID works). Send the same key to retry and a " +
					"new one to re-plan deliberately; it is recorded on every ticket the batch creates."},
			"items": map[string]interface{}{
				"type": "array",
				"description": "REQUIRED. The batch: 1 to " + strconv.Itoa(maxHandoffBatch) + " plan items. Every item needs " +
					"ref, story_id and name. There is deliberately no assignee, owner, schedule, performer or metadata field: the " +
					"owner is the triager doing the planning, and a request has no standing to decide any of the others.",
				"items": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"ref": str("REQUIRED. A short name for this item in the batch (letters, digits, '-', '_', '.', at most " +
							strconv.Itoa(domain.MaxHandoffRefLen) + "), so another item can depend on it and a retry can tell " +
							"\"already there\" from \"new\"."),
						"story_id": num("REQUIRED. The story the ticket goes in. It must be in the project the request is routed to."),
						"name":     str("REQUIRED. The ticket's one-line title, in the team's words."),
						"plan": map[string]interface{}{"type": "string",
							"description": "Optional. What the ticket is for and what done looks like — the TEAM's instruction, written " +
								"outside the untrusted block. The requester's words are appended beneath it, quoted."},
						"acceptance_criteria": str("Optional. The plan author's definition of done for this item."),
						"priority":            str("Optional. 0=low, 1=normal (default), 2=high, 3=critical."),
						"depends_on": map[string]interface{}{
							"type": "array",
							"description": "Optional. What this item waits on: refs of other items in this batch, or existing ticket " +
								"ids (\"123\" or \"#123\"). A ref must be in the batch unless an earlier attempt of the same " +
								"handoff_key already created it, in which case the retry wires to that ticket.",
							"items": map[string]interface{}{"type": []string{"string", "number"}},
						},
					},
					"required": []string{"ref", "story_id", "name"},
				},
			},
		}), wrap(ctx, s.open(s.handoffRequest)))

	mcp.AddTool(tool("amp_upload_request_file",
		"Store one private file on a team request. The person who submitted the request may add a file to their own request; a "+
			"member or lead of the inbox's team (or an owner/admin of its org) may add one to any request in that inbox. Anything "+
			"else answers 404 — the same answer an unknown request gets, so an id cannot be walked. The bytes travel inside this "+
			"call, base64-encoded: there is no url, path or \"attach from a link\" argument, and one is refused rather than "+
			"ignored, because fetching a caller-supplied address is how an upload becomes a request-forgery primitive. The stored "+
			"content type is what AMP sniffs from the bytes, never the name and never a client header. Bounded: 25 MiB per file, "+
			"5 files and 60 MiB per request; over either limit is refused request.invalid naming the numbers. Uploading is not "+
			"permission to download: reading a file back is a separate, short-lived signed link, and an archive, an executable or "+
			"a name that says code waits held for a team lead. Uploading writes no lifecycle event and advances no state.",
		props{
			"team_id":    num("REQUIRED. The inbox the request lives in."),
			"request_id": num("REQUIRED. The request the file belongs to."),
			"file_name": map[string]interface{}{"type": "string", "maxLength": domain.MaxRequestAttachmentNameLen,
				"description": "REQUIRED. The display name for the file: one line, not a path, at most " +
					strconv.Itoa(domain.MaxRequestAttachmentNameLen) + " characters. It never decides the content type; at most it " +
					"can add a hold (a name that says an executable or a script waits for a team lead)."},
			"content_base64": map[string]interface{}{
				"type": "string", "maxLength": base64.StdEncoding.EncodedLen(domain.MaxRequestAttachmentBytes),
				"description": "REQUIRED. The file's bytes, standard base64 (RFC 4648) on one line, at most " +
					strconv.FormatInt(domain.MaxRequestAttachmentBytes, 10) + " decoded bytes (" +
					strconv.Itoa(domain.MaxRequestAttachmentBytes>>20) + " MiB). Send the bytes themselves; there is no url to " +
					"fetch and no content-type header to set.",
			},
		}), wrap(ctx, s.open(s.addRequestAttachment)))
}

// ---- The relation ----

// attachRequest is amp_attach_request: one relation, authorized at the service.
func (s *Server) attachRequest(ctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	teamID, err := reqInt(args, "team_id")
	if err != nil {
		return nil, err
	}
	requestID, err := reqInt(args, "request_id")
	if err != nil {
		return nil, err
	}
	linkKind, err := reqStr(args, "link_kind")
	if err != nil {
		return nil, err
	}
	targetKind, err := reqStr(args, "target_kind")
	if err != nil {
		return nil, err
	}
	targetID, err := reqInt(args, "target_id")
	if err != nil {
		return nil, err
	}
	supersede, _, err := optBoolStrict(args, "supersede_route",
		"a flag; send true to move the request to another project deliberately, or omit it")
	if err != nil {
		return nil, planningToolError(err)
	}
	if supersede && targetKind != string(domain.RequestTargetProject) {
		// A route flag on a non-route link is a caller who believes it is moving
		// something. Saying so is cheaper than a silent no-op.
		return nil, planningToolError(authz.RequestInvalid(
			"supersede_route only applies to a routes_to link to a project: a %s link names its own relation and has no route to move",
			linkKind))
	}

	res, err := s.svc.AttachRequest(ctx, caller(ctx).p, service.AttachRequestInput{
		RequestID: requestID,
		TeamID:    teamID,
		// The pair is validated by the domain before anything is loaded (a
		// planned_as -> artifact is refused as the invalid request it is), so it
		// is handed over as written rather than pre-interpreted here.
		LinkKind:       domain.RequestLinkKind(linkKind),
		TargetKind:     domain.RequestTargetKind(targetKind),
		TargetID:       targetID,
		SupersedeRoute: supersede,
	})
	if err != nil {
		return nil, planningToolError(err)
	}
	out := map[string]interface{}{"link": res.Link, "changed": res.Changed}
	if res.Target != nil {
		// What the request now points at, read from the target's own row — never
		// from anything the caller wrote.
		out["target"] = map[string]interface{}{
			"kind": res.Target.Kind, "id": res.Target.ID, "project_id": res.Target.ProjectID,
		}
	}
	if res.Changed {
		out["next"] = "Linked. The request now carries this relation; attaching approves nothing, dispatches nothing and moves " +
			"no state. A planned_as link makes the work visible on the request's planning rail, and a delivered_by link is " +
			"evidence a later delivery may count."
	} else {
		out["next"] = "Already linked: the request already carried this relation, so nothing was written. There is no second " +
			"row, no second history entry and no second audit entry."
	}
	return jsonResult(out)
}

// ---- Planning ----

// handoffRequest is amp_handoff_request: a batch of plan items for one request.
func (s *Server) handoffRequest(ctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	teamID, err := reqInt(args, "team_id")
	if err != nil {
		return nil, err
	}
	requestID, err := reqInt(args, "request_id")
	if err != nil {
		return nil, err
	}
	key, err := reqStr(args, "handoff_key")
	if err != nil {
		return nil, err
	}
	if err := refusePlanningArgs(args); err != nil {
		return nil, planningToolError(err)
	}
	items, err := optHandoffItems(args)
	if err != nil {
		return nil, planningToolError(err)
	}

	res, err := s.svc.HandoffRequest(ctx, caller(ctx).p, service.HandoffRequestInput{
		RequestID:  requestID,
		TeamID:     teamID,
		HandoffKey: key,
		Items:      items,
	})
	if err != nil {
		return nil, planningToolError(err)
	}
	out := map[string]interface{}{
		"project_id": res.ProjectID,
		"items":      res.Items,
		"created":    createdCount(res.Items),
		"already":    len(res.Items) - createdCount(res.Items),
	}
	if len(res.Warnings) > 0 {
		out["warnings"] = res.Warnings
	}
	out["next"] = "Planned into the project the request is routed to, and linked as planned_as. Every ticket waits for a person " +
		"to approve it — a handoff approves and dispatches nothing, and its descriptions quote the requester's words as " +
		"untrusted data, so they cannot change who does the work, when it runs or whether a plan is approved. If this call was a " +
		"retry, the items it reports as already there were created by the earlier attempt and were not planned twice."
	return jsonResult(out)
}

// optHandoffItems decodes the batch strictly. Everything product-shaped (a ref's
// shape, a duplicate ref, the 100-item bound) is the service's; what this reader
// owns is the difference between "the caller did not send a field" and "the
// caller sent a field this path must not be able to set".
func optHandoffItems(args map[string]interface{}) ([]service.HandoffItem, error) {
	raw, present := args["items"]
	if !present || raw == nil {
		return nil, authz.RequestInvalid("items is required: a handoff plans at least one ticket")
	}
	list, isList := raw.([]interface{})
	if !isList {
		return nil, authz.RequestInvalid("items must be an array of plan items (got a %s)", jsonKindOf(raw))
	}
	if len(list) == 0 {
		return nil, authz.RequestInvalid("items is empty: a handoff plans at least one ticket")
	}
	if len(list) > maxHandoffBatch {
		return nil, authz.RequestInvalid("items holds %d plan items, and a handoff plans at most %d at a time: split the batch "+
			"and send a handoff_key per batch (a batch that stopped part-way is retried with its own key)",
			len(list), maxHandoffBatch)
	}
	items := make([]service.HandoffItem, 0, len(list))
	for i, entry := range list {
		m, isObj := entry.(map[string]interface{})
		if !isObj {
			return nil, authz.RequestInvalid("items[%d] must be an object (got a %s): ref, story_id and name are required", i, jsonKindOf(entry))
		}
		// The fields this path must not carry are refused by name, per item, so
		// the refusal says which item and which field rather than the caller
		// believing it scheduled or assigned something.
		for _, key := range handoffForbiddenItemArgs {
			if !argPresent(m, key) {
				continue
			}
			return nil, authz.RequestInvalid("items[%d] carries %q, which a handoff cannot set: %s", i, key, handoffNoDecisionRule)
		}
		it := service.HandoffItem{}
		ref, err := handoffItemStr(m, i, "ref")
		if err != nil {
			return nil, err
		}
		it.Ref = ref
		storyID, storySet, err := optIntStrict(m, "story_id", "every ticket belongs to a story in the project the request is routed to")
		if err != nil {
			return nil, authz.RequestInvalid("items[%d]: %s", i, err)
		}
		if !storySet || storyID <= 0 {
			return nil, authz.RequestInvalid("items[%d] (%q): story_id is required: every ticket belongs to a story in the project the request is routed to", i, it.Ref)
		}
		it.StoryID = storyID
		if it.Name, err = handoffItemStr(m, i, "name"); err != nil {
			return nil, err
		}
		if it.Plan, err = handoffItemStr(m, i, "plan"); err != nil {
			return nil, err
		}
		if it.AcceptanceCriteria, err = handoffItemStr(m, i, "acceptance_criteria"); err != nil {
			return nil, err
		}
		if it.Priority, err = handoffItemStr(m, i, "priority"); err != nil {
			return nil, err
		}
		if it.DependsOn, err = handoffDependsOn(m, i); err != nil {
			return nil, err
		}
		items = append(items, it)
	}
	return items, nil
}

// handoffItemStr reads one optional string from an item, refusing a wrong type
// with the item's index in the message. A number where a name belongs is a
// client bug that would otherwise become a ticket named "[object Object]".
func handoffItemStr(m map[string]interface{}, i int, key string) (string, error) {
	s, err := optStrStrict(m, key, "a JSON string")
	if err != nil {
		return "", authz.RequestInvalid("items[%d]: %s", i, err)
	}
	return s, nil
}

// handoffDependsOn reads one item's dependencies as the strings CreateTasks
// parses: refs as written, ticket ids as "123" (the dotted form is its
// business). A number is an id; anything else in the list is refused, because a
// silently dropped dependency is a ticket that starts before the work it needs.
func handoffDependsOn(m map[string]interface{}, i int) ([]string, error) {
	raw, present := m["depends_on"]
	if !present || raw == nil {
		return nil, nil
	}
	list, isList := raw.([]interface{})
	if !isList {
		return nil, authz.RequestInvalid("items[%d].depends_on must be an array of refs or ticket ids (got a %s)", i, jsonKindOf(raw))
	}
	out := make([]string, 0, len(list))
	for j, entry := range list {
		switch v := entry.(type) {
		case string:
			out = append(out, v)
		case float64:
			if v != float64(int(v)) || v <= 0 {
				return nil, authz.RequestInvalid("items[%d].depends_on[%d] must be a ref or a whole ticket id (got %v)", i, j, v)
			}
			out = append(out, strconv.Itoa(int(v)))
		case int:
			out = append(out, strconv.Itoa(v))
		default:
			return nil, authz.RequestInvalid("items[%d].depends_on[%d] must be a ref (string) or a ticket id (number), not a %s",
				i, j, jsonKindOf(entry))
		}
	}
	return out, nil
}

// handoffNoDecisionRule is what every refusal of a plan-shaped-but-forbidden
// field says. It is one sentence because there is one rule: planning decides
// what serves a request, never who does it, when it runs or whether it is
// approved (contract §3, slice S7).
const handoffNoDecisionRule = "planning a request decides what work serves it, never who does it, when it runs, or whether it is " +
	"approved: the ticket's owner is the triager doing the planning, a person approves it afterwards in the web UI, and the " +
	"request's own words cannot set any of those"

// handoffForbiddenItemArgs are the item fields a handoff must not be able to set.
// HandoffItem has no field for any of them (that is the structural half of
// "request text decides nothing"); this list is the adapter's half, so a caller
// that sends one is told why instead of having it dropped.
var handoffForbiddenItemArgs = []string{
	"assigned_to", "assignee", "agent_id", "owner", "owner_user_id",
	"start_at", "schedule", "decide_by", "ask",
	"performer", "human_reason",
	"metadata",
	"approve", "approved", "approval", "dispatch", "dispatch_id",
	"project_id", "epic_id", "task_id", "status", "state",
}

// handoffForbiddenArgs are the same rule one level up: the handoff as a whole
// names the inbox and the batch, and nothing about the plan's decisions.
var handoffForbiddenArgs = []string{
	"project_id", "epic_id", "org_id", "story_id",
	"assigned_to", "assignee", "agent_id", "owner",
	"start_at", "metadata", "approve", "approved", "dispatch", "dispatch_id",
	"actor", "actor_user_id", "user_id", "requester", "requester_user_id",
}

// refusePlanningArgs refuses a handoff payload that tries to set one of the
// decisions above. Absent and blank values are the absence they look like, the
// same rule amp_submit_request's spoof guard uses.
func refusePlanningArgs(args map[string]interface{}) error {
	for _, key := range handoffForbiddenArgs {
		if !argPresent(args, key) {
			continue
		}
		return authz.RequestInvalid("%q is not an argument of a handoff: %s", key, handoffNoDecisionRule)
	}
	return nil
}

// ---- Attachments ----

// addRequestAttachment is amp_add_request_attachment: the caller's bytes onto an
// existing request, authorized at the service.
func (s *Server) addRequestAttachment(ctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	teamID, err := reqInt(args, "team_id")
	if err != nil {
		return nil, err
	}
	requestID, err := reqInt(args, "request_id")
	if err != nil {
		return nil, err
	}
	fileName, err := reqStr(args, "file_name")
	if err != nil {
		return nil, err
	}
	if err := refuseRemoteFetchArgs(args); err != nil {
		return nil, planningToolError(err)
	}
	encoded, err := reqStr(args, "content_base64")
	if err != nil {
		return nil, err
	}
	content, err := decodeAttachmentContent(encoded)
	if err != nil {
		return nil, planningToolError(err)
	}

	// The tenant's org, read from the team row — the same resolution the REST
	// download route makes. The service re-verifies the (org, team) pair against
	// the request's own columns, so a team that is not the request's is an
	// ordinary not-found rather than a cross-tenant write.
	team, err := s.svc.RequestTeamOrg(ctx, teamID)
	if err != nil {
		return nil, planningToolError(err)
	}
	att, err := s.svc.AddRequestAttachment(ctx, caller(ctx).p, service.AddRequestAttachmentInput{
		RequestID: requestID,
		OrgID:     team.OrgID,
		TeamID:    team.ID,
		FileName:  fileName,
		Content:   content,
	})
	if err != nil {
		return nil, planningToolError(err)
	}
	out := map[string]interface{}{"attachment": att}
	next := "Stored as a private file on the request. Its content type is what AMP sniffed from the bytes"
	if att.Held() {
		next += ", and it is HELD: an archive, an executable or a name that says code waits for a team lead to release it before " +
			"anyone can download it. The requester can be told that is why it is not readable yet."
	} else {
		next += ". Reading it back is a separate decision: a download is a short-lived signed link, and having uploaded a file " +
			"grants nothing."
	}
	out["next"] = next
	return jsonResult(out)
}

// remoteFetchArgs are the arguments a caller uses when it believes an upload can
// name where the bytes live. Nothing here fetches: the bytes arrived in this call
// or they did not arrive.
var remoteFetchArgs = []string{
	"url", "uri", "href", "link", "source_url", "content_url", "file_url", "download_url",
	"fetch", "fetch_url", "remote", "remote_url", "path", "file_path",
}

// refuseRemoteFetchArgs refuses a payload naming a location instead of bytes.
func refuseRemoteFetchArgs(args map[string]interface{}) error {
	for _, key := range remoteFetchArgs {
		if !argPresent(args, key) {
			continue
		}
		return authz.RequestInvalid("%q is not an argument of an upload and is refused rather than ignored: an attachment's "+
			"bytes travel inside this call as content_base64, and AMP never fetches a location a caller names — a URL here would "+
			"make an upload a request-forgery primitive and a way to pull bytes from somewhere AMP cannot vouch for", key)
	}
	return nil
}

// decodeAttachmentContent turns the base64 argument into bytes, refusing an
// over-size payload BEFORE it is decoded and an unreadable one instead of
// guessing. The bound is checked on the encoded length first so a caller cannot
// make AMP allocate a payload the service will refuse anyway; the decoded length
// is checked again because that is the real bound the domain enforces.
func decodeAttachmentContent(raw string) ([]byte, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, authz.RequestInvalid("content_base64 is empty: an attachment needs bytes, and an empty file has nothing to attach")
	}
	limit := base64.StdEncoding.EncodedLen(domain.MaxRequestAttachmentBytes)
	if len(trimmed) > limit {
		return nil, authz.RequestInvalid("content_base64 holds %d base64 characters, which is more than the per-file limit of "+
			"%d bytes (%d MiB) allows (%d characters): send a smaller file, or ask the team for a different format",
			len(trimmed), domain.MaxRequestAttachmentBytes, domain.MaxRequestAttachmentBytes>>20, limit)
	}
	content, err := base64.StdEncoding.DecodeString(trimmed)
	if err != nil {
		// The refusal names the encoding, never the content: a base64 string is
		// a file, and an error message is not a place to echo one.
		return nil, authz.RequestInvalid("content_base64 is not valid standard base64 (RFC 4648, no line breaks): encode the " +
			"file's bytes with base64.StdEncoding and send them on one line")
	}
	if len(content) > domain.MaxRequestAttachmentBytes {
		return nil, authz.RequestInvalid("content_base64 decodes to %d bytes, over the %d-byte per-file limit (%d MiB): send a "+
			"smaller file", len(content), domain.MaxRequestAttachmentBytes, domain.MaxRequestAttachmentBytes>>20)
	}
	return content, nil
}

// ---- Shared helpers for this surface ----

// planningToolError reports a refusal exactly as the submission and triage tools
// do: submitToolError owns the mapping (the service's message plus REST's status
// and stable code in the error's data) and a second copy here would be a second
// definition of the contract a client reads.
func planningToolError(err error) error { return workToolError(err) }

// requestLinkKindNames and requestTargetKindNames are the closed vocabularies, in
// the domain's own order, for the schemas' enums. They read the domain's lists so
// a kind added there cannot leave this schema advertising a stale set.
func requestLinkKindNames() []string {
	out := make([]string, len(domain.RequestLinkKinds))
	for i, k := range domain.RequestLinkKinds {
		out[i] = string(k)
	}
	return out
}

func requestTargetKindNames() []string {
	out := make([]string, len(domain.RequestTargetKinds))
	for i, k := range domain.RequestTargetKinds {
		out[i] = string(k)
	}
	return out
}

// createdCount is how many items this attempt actually created, which the answer
// reports next to the batch size: "3 created, 1 already there" is the truthful
// report of a retry.
func createdCount(items []service.HandoffItemResult) int {
	n := 0
	for _, it := range items {
		if it.Created {
			n++
		}
	}
	return n
}

// requestPlanningToolNames is the set of tools this file registers. It exists so
// a test can assert the registered set, the catalog's entries and the negative
// boundaries ("no acceptance, no delivery, no fetch") describe the same surface.
func requestPlanningToolNames() []string {
	return []string{
		"amp_attach_request",
		"amp_handoff_request",
		"amp_upload_request_file",
	}
}
