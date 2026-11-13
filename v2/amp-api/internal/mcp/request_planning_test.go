package mcp

// The inbox planning and attachment tools' adapter-level contract (epic 202,
// story 549, AMP #3972; approved contract artifact 36 v2 §1/§3/§5; build slice
// I3).
//
// What is proven here is exactly what these three handlers decide for themselves,
// because everything product-shaped — who may attach, which pairs of link and
// target kinds exist, whether a request is already routed, how a replayed attach
// answers, how a partial planning batch reconciles, what a file sniffs as, what
// its scan state is, whether it is held — belongs to the service methods they
// call and is proven against a real database in
// cmd/server/request_planning_mcp_test.go. A rule this file had to test twice
// would be a rule implemented twice, which is the drift the thin-adapter rule
// exists to prevent.
//
// So the six things tested here are the six the adapters own:
//
//  1. The registered surface: exactly these three tools, each catalogued with
//     truthful flags, none destructive, none hidden — and no fourth tool that
//     would accept an outcome, deliver, approve or dispatch.
//  2. The schemas: required parameters, the domain's own kind vocabularies as
//     enums, the domain's bounds, and the absence of any argument through which a
//     caller could schedule, assign, approve or dispatch — or name a place to
//     fetch bytes from.
//  3. The strict readers: a wrong-typed item field, dependency or flag is refused
//     400 request.invalid naming the argument, rather than silently dropped.
//  4. The two negative-argument lists: fields a handoff and an upload must not be
//     able to set are refused by name (not ignored), because a silently dropped
//     start_at is a caller who believes it scheduled work.
//  5. The base64 reader: an over-size payload is refused before decoding, an
//     unreadable one names the encoding and never echoes the content, and the
//     decoded length is checked again.
//  6. The refusal envelope and the wiring: the same request.* status/code in the
//     error's data that the submit and triage tools give, `s.open` everywhere,
//     and no call to an approval, dispatch or acceptance path anywhere in the
//     file.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/simstech/amp-api/internal/auth"
	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
)

// planningToolNames is the surface this slice promises: three tools, one per
// reviewed service method. Hardcoded here on purpose — the production list is
// compared against it, so a tool added or removed without a decision about its
// scope, catalog entry and boundaries fails this test rather than shipping
// quietly.
var planningToolNames = []string{
	"amp_attach_request",
	"amp_handoff_request",
	"amp_upload_request_file",
}

// requestPlanningServer registers the slice's tools on a bare mcp-go server, so
// the schemas, annotations and tool list under test are the ones that ship.
func requestPlanningServer(t *testing.T) *mcpserver.MCPServer {
	t.Helper()
	srv := mcpserver.NewMCPServer("amp-api-test", "2.0.0", mcpserver.WithToolCapabilities(false))
	(&Server{}).registerRequestPlanning(srv)
	return srv
}

// requestPlanningTool returns one registered tool by name.
func requestPlanningTool(t *testing.T, srv *mcpserver.MCPServer, name string) mcpgo.Tool {
	t.Helper()
	st := srv.GetTool(name)
	if st == nil {
		t.Fatalf("registerRequestPlanning did not register %s", name)
	}
	return st.Tool
}

// ---- 1: the registered surface ----

// TestRequestPlanningSurfaceIsExactlyTheThreeTools: the slice registers the three
// tools it is scoped to and nothing else. "Nothing else" is the load-bearing
// half: a fourth tool that accepted an outcome, delivered a request, approved or
// dispatched work would be a capability this ticket may not ship, and it would be
// reachable the moment it existed.
func TestRequestPlanningSurfaceIsExactlyTheThreeTools(t *testing.T) {
	srv := requestPlanningServer(t)
	got := listedNames(t, srv)
	if len(got) != len(planningToolNames) {
		t.Fatalf("registered tools = %v, want exactly %v", got, planningToolNames)
	}
	for _, name := range planningToolNames {
		if !containsName(got, name) {
			t.Errorf("%s is not registered (got %v)", name, got)
		}
	}
	if prod := requestPlanningToolNames(); len(prod) != len(planningToolNames) {
		t.Fatalf("requestPlanningToolNames() = %v, want %v", prod, planningToolNames)
	} else {
		for i := range prod {
			if prod[i] != planningToolNames[i] {
				t.Fatalf("requestPlanningToolNames() = %v, want %v", prod, planningToolNames)
			}
		}
	}
	// Nothing acceptance-, delivery-, approval- or dispatch-shaped is here, by
	// name. "attach" and "handoff" are this slice's own verbs, so they are not in
	// the list; the ones that are cannot be reached through any tool in this file.
	for _, name := range got {
		for _, forbidden := range []string{"accept", "deliver", "approve", "dispatch", "transition", "reopen", "answer", "reject"} {
			if strings.Contains(name, forbidden) {
				t.Errorf("tool %q is part of the inbox surface this slice may not add (%q)", name, forbidden)
			}
		}
	}
}

// TestRequestPlanningCatalogFlags pins what the catalog promises a model:
//
//   - the two writes that have a replay contract are idempotent, and that is TRUE
//     rather than optimistic: an attach of a stored relation writes nothing
//     (changed=false) and a handoff retried with its own key reconciles against
//     the ledger;
//   - the attachment tool is deliberately NOT idempotent: a second identical
//     upload is a second file (the service has no replay token; the request's
//     count bound is the only ceiling), and a model reading an idempotent hint
//     would retry an upload it should check first;
//   - none is destructive and none is personOnly/adminOnly: planning an inbox's
//     work is a team member's own work, so the tools stay listed;
//   - none is in robust.go's read-only list, so a timeout's wording is "check the
//     result before retrying" — the honest instruction for a write.
func TestRequestPlanningCatalogFlags(t *testing.T) {
	idempotent := map[string]bool{
		"amp_attach_request":  true,
		"amp_handoff_request": true,
		// No replay token exists for an upload: see above.
		"amp_upload_request_file": false,
	}
	for _, name := range planningToolNames {
		info, ok := toolCatalog[name]
		if !ok {
			t.Errorf("toolCatalog has no entry for %s (an uncatalogued tool gets no title, no annotations and silently defaults to listed)", name)
			continue
		}
		if info.title == "" || info.keywords == "" {
			t.Errorf("%s catalog entry is thin: title %q, keywords %q", name, info.title, info.keywords)
		}
		if info.readOnly {
			t.Errorf("%s is flagged readOnly; it writes", name)
		}
		if info.idempotent != idempotent[name] {
			t.Errorf("%s idempotent = %v, want %v", name, info.idempotent, idempotent[name])
		}
		if info.destructive {
			t.Errorf("%s is flagged destructive; nothing in this slice deletes", name)
		}
		if info.listing != listed {
			t.Errorf("%s listing = %v, want listed: a team member's planning tools must be findable", name, info.listing)
		}
		if got := readOnly(name); got {
			t.Errorf("readOnly(%q) = true; a timeout would tell a client to retry a write blind", name)
		}
		if got := ToolDeadline(name); got != DefaultToolDeadline {
			t.Errorf("ToolDeadline(%q) = %s; one attach or one batch of at most %d tickets needs no override", name, got, maxHandoffBatch)
		}
	}

	// Annotations reach the wire as the catalog says.
	srv := requestPlanningServer(t)
	for _, name := range planningToolNames {
		tl := requestPlanningTool(t, srv, name)
		want := toolCatalog[name]
		if tl.Annotations.Title != want.title {
			t.Errorf("%s annotations = %+v, want title %q", name, tl.Annotations, want.title)
		}
		if tl.Annotations.ReadOnlyHint == nil || *tl.Annotations.ReadOnlyHint {
			t.Errorf("%s ReadOnlyHint = %v, want false", name, tl.Annotations.ReadOnlyHint)
		}
		if tl.Annotations.DestructiveHint == nil || *tl.Annotations.DestructiveHint {
			t.Errorf("%s DestructiveHint = %v, want false", name, tl.Annotations.DestructiveHint)
		}
		if tl.Annotations.IdempotentHint == nil || *tl.Annotations.IdempotentHint != want.idempotent {
			t.Errorf("%s IdempotentHint = %v, want %v", name, tl.Annotations.IdempotentHint, want.idempotent)
		}
	}
}

// TestRequestPlanningKeepsTheSubmitScopeToOneTool is AMP #3772's boundary
// asserted from this slice's side: a submit-only credential may call
// amp_submit_request and nothing else. Adding these three tools must not widen
// that set — and the scope's own gate refuses them regardless of tools/list,
// because a submit scope is not a planning scope.
func TestRequestPlanningKeepsTheSubmitScopeToOneTool(t *testing.T) {
	allowed := submitScopeToolNames()
	if len(allowed) != 1 || !allowed[SubmitScopeTool] {
		t.Fatalf("submitScopeToolNames() = %v, want only %q: a submission credential plans nothing", allowed, SubmitScopeTool)
	}
	for _, name := range planningToolNames {
		if allowed[name] {
			t.Errorf("%s is callable by a submit-only credential: submitting is not planning and it is not uploading files", name)
		}
	}
}

// TestRequestPlanningUsesOpenNeverGuardAndNoDecisionPath is the wiring rule,
// checked against the source rather than trusted: s.guard resolves a project and
// requires a Level on it, which an inbox tool must never demand (a triager works
// a request, and the link's target project is checked by the service, not by the
// tool). The same scan is why "no approval, no dispatch, no acceptance, no remote
// fetch" is a checkable property and not only a sentence in a comment.
func TestRequestPlanningUsesOpenNeverGuardAndNoDecisionPath(t *testing.T) {
	src, err := os.ReadFile("request_planning.go")
	if err != nil {
		t.Fatalf("read request_planning.go: %v", err)
	}
	code := string(src)
	for _, forbidden := range []string{
		// Authorization must go through the service, not a project ladder.
		"s.guard(", "s.projectOf(",
		// No lifecycle, acceptance or delivery: acceptance is the requester's own
		// act in the UI (I1, D5) and there is no tool for it anywhere.
		"svc.TransitionRequest(", "svc.AcceptRequest(", "svc.AnswerRequest(", "svc.ReopenRequest(",
		"svc.DeliverRequest(",
		// No approval or dispatch path, ever, from a request.
		"svc.ApprovePlan(", "svc.ApproveTickets(", "svc.Dispatch(", "svc.CreateTasks(",
		// No second definition of an attach, a handoff or an upload: the three
		// services are the only writers this file calls.
		"Repo.LinkRequest(", "svc.Repo.",
		// No network: nothing here fetches a caller-supplied location.
		"net/http", "http.Get", "http.NewRequest",
	} {
		if strings.Contains(code, forbidden) {
			t.Errorf("request_planning.go contains %s; the inbox planning tools act only through the reviewed services", forbidden)
		}
	}
	// And each registered handler is wrapped in open, so every call resolves the
	// caller from the request context (never from an argument).
	if got := strings.Count(code, "wrap(ctx, s.open(s."); got != len(planningToolNames) {
		t.Errorf("%d handlers wired through s.open, want %d: every tool in this file authorizes at the service", got, len(planningToolNames))
	}
}

// ---- 2: the schemas ----

// TestRequestPlanningSchemas: each tool declares what it needs, and offers no
// argument through which a caller could schedule, assign, approve, dispatch or
// fetch. The negatives are the point.
func TestRequestPlanningSchemas(t *testing.T) {
	srv := requestPlanningServer(t)
	wantRequired := map[string][]string{
		"amp_attach_request":      {"link_kind", "request_id", "target_id", "target_kind", "team_id"},
		"amp_handoff_request":     {"handoff_key", "items", "request_id", "team_id"},
		"amp_upload_request_file": {"content_base64", "file_name", "request_id", "team_id"},
	}
	// Not one of these tools may offer a way to set who acts, what the outcome is,
	// whether the requester accepted, who does the work, when it runs, whether a
	// plan is approved — or where to fetch bytes from.
	forbiddenProps := []string{
		"user_id", "actor", "actor_user_id", "author", "author_user_id", "requester", "requester_user_id",
		"state", "status", "to", "transition", "outcome", "accept", "accepted", "requester_accepted",
		"deliver", "delivered", "reject_reason", "duplicate_of",
		"assigned_to", "assignee", "agent_id", "owner", "owner_user_id", "start_at",
		"approve", "approved", "dispatch", "dispatch_id", "metadata", "performer", "human_reason",
		"url", "uri", "href", "path", "file_path", "content_url", "download_url", "fetch",
	}
	for _, name := range planningToolNames {
		t.Run(name, func(t *testing.T) {
			tl := requestPlanningTool(t, srv, name)
			s := decodeSchema(t, tl)
			want := wantRequired[name]
			if len(s.Required) != len(want) {
				t.Fatalf("required = %v, want %v", s.Required, want)
			}
			for i := range want {
				if s.Required[i] != want[i] {
					t.Fatalf("required = %v, want %v (sorted)", s.Required, want)
				}
			}
			for prop, schema := range s.Properties {
				if d, _ := schema["description"].(string); strings.TrimSpace(d) == "" {
					t.Errorf("property %q has no description", prop)
				}
				for _, bad := range forbiddenProps {
					if prop == bad {
						t.Errorf("schema offers %q; this slice's tools cannot set it", prop)
					}
				}
			}
			// The team is the tenant: every tool names one, so no write can be
			// aimed at "whichever team".
			if _, ok := s.Properties["team_id"]; !ok {
				t.Error("schema has no team_id: the inbox's tenant is always named")
			}
			if _, ok := s.Properties["request_id"]; !ok {
				t.Error("schema has no request_id: every tool here acts on one request")
			}
		})
	}

	t.Run("attach kinds are the domain's vocabularies", func(t *testing.T) {
		s := decodeSchema(t, requestPlanningTool(t, srv, "amp_attach_request"))
		links, _ := s.Properties["link_kind"]["enum"].([]interface{})
		if len(links) != len(domain.RequestLinkKinds) {
			t.Fatalf("link_kind enum = %v, want the %d link kinds", links, len(domain.RequestLinkKinds))
		}
		for i, k := range domain.RequestLinkKinds {
			if links[i] != string(k) {
				t.Errorf("link_kind enum[%d] = %v, want %q", i, links[i], k)
			}
		}
		// duplicate_of and relates_to are offered so a caller that sends one is
		// answered with the rule that refuses it, rather than "unknown value".
		targets, _ := s.Properties["target_kind"]["enum"].([]interface{})
		if len(targets) != len(domain.RequestTargetKinds) {
			t.Fatalf("target_kind enum = %v, want the %d target kinds", targets, len(domain.RequestTargetKinds))
		}
		if _, ok := s.Properties["supersede_route"]; !ok {
			t.Error("no supersede_route: moving a request's route deliberately would be unreachable")
		}
	})

	t.Run("handoff bounds and item shape", func(t *testing.T) {
		s := decodeSchema(t, requestPlanningTool(t, srv, "amp_handoff_request"))
		if got := s.Properties["handoff_key"]["maxLength"]; got != float64(domain.MaxRequestHandoffKeyLen) {
			t.Errorf("handoff_key maxLength = %v, want %d", got, domain.MaxRequestHandoffKeyLen)
		}
		itemsProp, _ := s.Properties["items"]["items"].(map[string]interface{})
		if itemsProp == nil {
			t.Fatalf("items has no element schema: %v", s.Properties["items"])
		}
		element, _ := itemsProp["properties"].(map[string]interface{})
		for _, want := range []string{"ref", "story_id", "name", "plan", "acceptance_criteria", "priority", "depends_on"} {
			if _, ok := element[want]; !ok {
				t.Errorf("an item has no %q field", want)
			}
		}
		// And nothing that decides who, when or whether approved.
		for _, forbidden := range []string{"assigned_to", "owner", "start_at", "metadata", "performer", "approve", "agent_id"} {
			if _, ok := element[forbidden]; ok {
				t.Errorf("an item offers %q; a handoff cannot set it", forbidden)
			}
		}
		req, _ := itemsProp["required"].([]interface{})
		if len(req) != 3 {
			t.Fatalf("an item's required = %v, want ref, name, story_id", req)
		}
	})

	t.Run("attachment bounds", func(t *testing.T) {
		s := decodeSchema(t, requestPlanningTool(t, srv, "amp_upload_request_file"))
		if got := s.Properties["content_base64"]["maxLength"]; got != float64(base64.StdEncoding.EncodedLen(domain.MaxRequestAttachmentBytes)) {
			t.Errorf("content_base64 maxLength = %v, want the encoded form of the %d-byte bound", got, domain.MaxRequestAttachmentBytes)
		}
		if got := s.Properties["file_name"]["maxLength"]; got != float64(domain.MaxRequestAttachmentNameLen) {
			t.Errorf("file_name maxLength = %v, want %d", got, domain.MaxRequestAttachmentNameLen)
		}
		// There is no content-type argument: the type is sniffed from the bytes,
		// so a client's claim is not merely ignored — it is unexpressible.
		if _, ok := s.Properties["content_type"]; ok {
			t.Error("an upload offers content_type; the stored type is the sniffer's and only the sniffer's")
		}
		if _, ok := s.Properties["scan_state"]; ok {
			t.Error("an upload offers scan_state; no caller can claim a verdict")
		}
	})
}

// ---- 3: the strict item readers ----

// TestOptHandoffItemsRefusesWrongTypesAndDecidesNothing: the batch reader owns the
// difference between "absent" and "present but not what this path can set". A
// wrong-typed field names the item and the argument; a decision-shaped field is
// refused by name rather than dropped.
func TestOptHandoffItemsRefusesWrongTypesAndDecidesNothing(t *testing.T) {
	item := func(m map[string]interface{}) map[string]interface{} {
		base := map[string]interface{}{
			"ref": "api", "story_id": float64(7), "name": "Build the API",
		}
		for k, v := range m {
			base[k] = v
		}
		return base
	}
	tests := []struct {
		name    string
		args    map[string]interface{}
		wantErr string // required in the refusal, "" for accepted
		items   int
	}{
		{"absent", map[string]interface{}{}, "items is required", 0},
		{"null", map[string]interface{}{"items": nil}, "items is required", 0},
		{"empty", map[string]interface{}{"items": []interface{}{}}, "at least one ticket", 0},
		{"not a list", map[string]interface{}{"items": "api"}, "must be an array", 0},
		{"an item that is not an object", map[string]interface{}{"items": []interface{}{"api"}}, "must be an object", 0},
		{"one well-formed item", map[string]interface{}{"items": []interface{}{item(nil)}}, "", 1},
		{"missing story_id", map[string]interface{}{"items": []interface{}{map[string]interface{}{"ref": "api", "name": "x"}}},
			"story_id is required", 0},
		{"story_id as a string", map[string]interface{}{"items": []interface{}{item(map[string]interface{}{"story_id": "7"})}},
			"story_id", 0},
		{"fractional story_id", map[string]interface{}{"items": []interface{}{item(map[string]interface{}{"story_id": 7.5})}},
			"whole number", 0},
		{"name as a number", map[string]interface{}{"items": []interface{}{item(map[string]interface{}{"name": 12})}},
			"name", 0},
		{"depends_on a string", map[string]interface{}{"items": []interface{}{item(map[string]interface{}{"depends_on": "x"})}},
			"depends_on must be an array", 0},
		{"depends_on a boolean entry", map[string]interface{}{"items": []interface{}{item(map[string]interface{}{"depends_on": []interface{}{true}})}},
			"depends_on[0]", 0},
		{"assigned_to is refused", map[string]interface{}{"items": []interface{}{item(map[string]interface{}{"assigned_to": "amp-worker"})}},
			"assigned_to", 0},
		{"start_at is refused", map[string]interface{}{"items": []interface{}{item(map[string]interface{}{"start_at": "2027-01-01T00:00:00Z"})}},
			"start_at", 0},
		{"metadata is refused", map[string]interface{}{"items": []interface{}{item(map[string]interface{}{"metadata": map[string]interface{}{"a": "b"}})}},
			"metadata", 0},
		{"approve is refused", map[string]interface{}{"items": []interface{}{item(map[string]interface{}{"approve": true})}},
			"approve", 0},
		{"agent_id is refused", map[string]interface{}{"items": []interface{}{item(map[string]interface{}{"agent_id": "amp-worker"})}},
			"agent_id", 0},
		{"owner is refused", map[string]interface{}{"items": []interface{}{item(map[string]interface{}{"owner": "someone@example.com"})}},
			"owner", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := optHandoffItems(tt.args)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected refusal: %v", err)
				}
				if len(got) != tt.items {
					t.Fatalf("got %d items, want %d", len(got), tt.items)
				}
				// Nothing decision-shaped survived: the struct has no field for it,
				// and this asserts the reader did not smuggle one in.
				if got[0].Ref != "api" || got[0].StoryID != 7 || got[0].Name != "Build the API" {
					t.Fatalf("item = %+v", got[0])
				}
				return
			}
			if err == nil {
				t.Fatalf("%v was accepted as %v", tt.args["items"], got)
			}
			if authz.StatusOf(err, 0) != 400 || authz.CodeOf(err) != authz.CodeRequestInvalid {
				t.Fatalf("refusal = %d %q, want 400 %s", authz.StatusOf(err, 0), authz.CodeOf(err), authz.CodeRequestInvalid)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("refusal %q does not say %q", err, tt.wantErr)
			}
		})
	}
}

// TestOptHandoffItemsBoundsTheBatch: the 100-item ceiling is CreateTasks' own, and
// it is refused here with a coded message so a model is told the bound instead of
// reading an uncoded 400 from inside the planning path.
func TestOptHandoffItemsBoundsTheBatch(t *testing.T) {
	items := make([]interface{}, maxHandoffBatch+1)
	for i := range items {
		items[i] = map[string]interface{}{"ref": "r" + strconv.Itoa(i), "story_id": float64(1), "name": "n"}
	}
	_, err := optHandoffItems(map[string]interface{}{"items": items})
	if err == nil {
		t.Fatalf("%d items were accepted; the bound is %d", len(items), maxHandoffBatch)
	}
	if authz.CodeOf(err) != authz.CodeRequestInvalid || !strings.Contains(err.Error(), strconv.Itoa(maxHandoffBatch)) {
		t.Fatalf("refusal = %q (%q), want request.invalid naming %d", err, authz.CodeOf(err), maxHandoffBatch)
	}
	// Exactly at the bound is fine: this refuses nothing CreateTasks would allow.
	items = items[:maxHandoffBatch]
	if _, err := optHandoffItems(map[string]interface{}{"items": items}); err != nil {
		t.Fatalf("%d items were refused: %v", len(items), err)
	}
}

// TestHandoffDependsOn: a numeric dependency becomes the id CreateTasks parses,
// and anything else in the list is refused — a silently dropped dependency is a
// ticket that starts before the work it needs.
func TestHandoffDependsOn(t *testing.T) {
	tests := []struct {
		name    string
		value   interface{}
		want    []string
		wantErr bool
	}{
		{"absent", nil, nil, false},
		{"refs", []interface{}{"api", "ui"}, []string{"api", "ui"}, false},
		{"ids", []interface{}{float64(12), float64(13)}, []string{"12", "13"}, false},
		{"mixed", []interface{}{"api", float64(12)}, []string{"api", "12"}, false},
		{"fraction", []interface{}{12.5}, nil, true},
		{"zero id", []interface{}{float64(0)}, nil, true},
		{"object entry", []interface{}{map[string]interface{}{}}, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := map[string]interface{}{}
			if tt.value != nil {
				m["depends_on"] = tt.value
			}
			got, err := handoffDependsOn(m, 0)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("%v was accepted as %v", tt.value, got)
				}
				if authz.CodeOf(err) != authz.CodeRequestInvalid {
					t.Fatalf("code = %q, want %q", authz.CodeOf(err), authz.CodeRequestInvalid)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected refusal: %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Fatalf("got %v, want %v", got, tt.want)
				}
			}
		})
	}
}

// ---- 4: the two negative-argument lists ----

// TestRefusePlanningArgs: a handoff as a whole names the inbox, the request and the
// batch. Everything about the plan's decisions is refused by name — never
// dropped, because a caller that sent start_at believes it scheduled work.
func TestRefusePlanningArgs(t *testing.T) {
	tests := []struct {
		name string
		args map[string]interface{}
		want string
	}{
		{"clean payload", map[string]interface{}{"team_id": float64(1), "request_id": float64(2), "handoff_key": "k"}, ""},
		{"nulls are absence", map[string]interface{}{"start_at": nil, "owner": "", "metadata": nil}, ""},
		{"project_id", map[string]interface{}{"project_id": float64(9)}, "project_id"},
		{"assigned_to", map[string]interface{}{"assigned_to": "amp-worker"}, "assigned_to"},
		{"owner", map[string]interface{}{"owner": "someone@example.com"}, "owner"},
		{"start_at", map[string]interface{}{"start_at": "2027-01-01T00:00:00Z"}, "start_at"},
		{"metadata", map[string]interface{}{"metadata": map[string]interface{}{"a": true}}, "metadata"},
		{"approve", map[string]interface{}{"approve": true}, "approve"},
		{"dispatch", map[string]interface{}{"dispatch": true}, "dispatch"},
		{"actor", map[string]interface{}{"actor": "someone"}, "actor"},
		{"requester_user_id", map[string]interface{}{"requester_user_id": float64(3)}, "requester_user_id"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := refusePlanningArgs(tt.args)
			if tt.want == "" {
				if err != nil {
					t.Fatalf("unexpected refusal: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("%q was accepted", tt.want)
			}
			if authz.CodeOf(err) != authz.CodeRequestInvalid || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("refusal = %q (%q), want request.invalid naming %q", err, authz.CodeOf(err), tt.want)
			}
			// The refusal says why, in the one rule this file states everywhere.
			if !strings.Contains(err.Error(), "never who does it") {
				t.Errorf("refusal %q does not carry the planning rule", err)
			}
		})
	}
}

// TestRefuseRemoteFetchArgs: an upload's bytes travel in the call, so an argument
// that names a location is refused — not ignored. A dropped url would leave a
// caller believing a file was attached from a link AMP never opened.
func TestRefuseRemoteFetchArgs(t *testing.T) {
	for _, key := range remoteFetchArgs {
		err := refuseRemoteFetchArgs(map[string]interface{}{key: "https://example.com/a.pdf"})
		if err == nil {
			t.Fatalf("%q was accepted", key)
		}
		if authz.CodeOf(err) != authz.CodeRequestInvalid || !strings.Contains(err.Error(), key) {
			t.Errorf("%q refusal = %q (%q), want request.invalid naming it", key, err, authz.CodeOf(err))
		}
		if !strings.Contains(err.Error(), "never fetches") {
			t.Errorf("%q refusal does not say AMP never fetches: %q", key, err)
		}
	}
	if err := refuseRemoteFetchArgs(map[string]interface{}{"team_id": float64(1), "url": nil, "path": "  "}); err != nil {
		t.Fatalf("an empty location was refused: %v", err)
	}
}

// ---- 5: the base64 reader ----

// TestDecodeAttachmentContent: the per-file bound is checked before decoding (so a
// caller cannot make AMP allocate a payload the service will refuse anyway), the
// decoded length is checked again, an unreadable payload names the encoding and
// never echoes the content, and an empty one is the missing file it is.
func TestDecodeAttachmentContent(t *testing.T) {
	body := []byte("the requester's private notes\n")
	encoded := base64.StdEncoding.EncodeToString(body)
	got, err := decodeAttachmentContent(" " + encoded + "\n")
	if err != nil {
		t.Fatalf("a well-formed payload was refused: %v", err)
	}
	if string(got) != string(body) {
		t.Fatalf("decoded %q, want %q", got, body)
	}

	t.Run("empty", func(t *testing.T) {
		for _, in := range []string{"", "   ", "\n"} {
			_, err := decodeAttachmentContent(in)
			if err == nil || !strings.Contains(err.Error(), "content_base64 is empty") {
				t.Fatalf("%q = %v, want an empty-content refusal", in, err)
			}
		}
	})

	t.Run("over the bound before decoding", func(t *testing.T) {
		// One character over what the bound's encoded length allows — and not
		// valid base64 either, so a reader that decoded first would report the
		// wrong problem.
		over := strings.Repeat("A", base64.StdEncoding.EncodedLen(domain.MaxRequestAttachmentBytes)+1)
		_, err := decodeAttachmentContent(over)
		if err == nil {
			t.Fatal("an over-size payload was accepted")
		}
		if !strings.Contains(err.Error(), "per-file limit") {
			t.Fatalf("refusal = %q, want the bound named", err)
		}
		if authz.CodeOf(err) != authz.CodeRequestInvalid {
			t.Fatalf("code = %q", authz.CodeOf(err))
		}
	})

	t.Run("unreadable and never echoed", func(t *testing.T) {
		secret := "not base64 at all: !!secret!!"
		_, err := decodeAttachmentContent(secret)
		if err == nil {
			t.Fatal("an unreadable payload was accepted")
		}
		if !strings.Contains(err.Error(), "RFC 4648") {
			t.Errorf("refusal = %q, want the encoding named", err)
		}
		if strings.Contains(err.Error(), "secret") {
			t.Errorf("refusal echoes the content: %q", err)
		}
	})

	t.Run("exactly at the bound", func(t *testing.T) {
		big := make([]byte, domain.MaxRequestAttachmentBytes)
		if _, err := decodeAttachmentContent(base64.StdEncoding.EncodeToString(big)); err != nil {
			t.Fatalf("a payload of exactly the bound was refused: %v", err)
		}
	})
}

// ---- 6: the refusal envelope ----

// TestPlanningToolErrorMatchesTheSubmissionEnvelope: a request.* refusal from these
// tools reaches a client exactly as amp_submit_request's does — the service's
// message, REST's status and stable code in data, and the chain intact so
// authz.CodeOf/StatusOf and the denied middleware keep working. A fault stays
// unwrapped so an unexplained failure keeps looking unexplained.
func TestPlanningToolErrorMatchesTheSubmissionEnvelope(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{"a missing target is the one 404", authz.NotFound("task 9 not found"), 404, ""},
		{"a request you cannot see", authz.RequestNotFound(), 404, authz.CodeRequestNotFound},
		{"a second route", authz.RequestConflict("request REF is routed to project 7"), 409, authz.CodeRequestConflict},
		{"a refused relation pair", authz.RequestInvalid("a planned_as link points at story, not \"artifact\""), 400, authz.CodeRequestInvalid},
		{"a requester planning is a 403", authz.Forbidden("planning work for request REF is the inbox team's act"), 403, ""},
		{"a scope refusal keeps its own code", authz.TokenScopeForbidden(), 403, authz.CodeTokenScopeDenied},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := planningToolError(tt.err)
			if got := authz.StatusOf(err, 0); got != tt.wantStatus {
				t.Errorf("StatusOf = %d, want %d", got, tt.wantStatus)
			}
			if got := authz.CodeOf(err); got != tt.wantCode {
				t.Errorf("CodeOf = %q, want %q", got, tt.wantCode)
			}
			var coded *requestToolError
			if !errors.As(err, &coded) {
				t.Fatalf("error is not a requestToolError (%T)", err)
			}
			rpc := coded.JSONRPCError()
			if rpc.Error.Message != tt.err.Error() {
				t.Errorf("message = %q, want the service's own %q", rpc.Error.Message, tt.err.Error())
			}
			data, ok := rpc.Error.Data.(map[string]any)
			if !ok {
				t.Fatalf("data = %#v, want a map carrying status/code", rpc.Error.Data)
			}
			if got := data["status"]; got != tt.wantStatus {
				t.Errorf("data.status = %v, want %d", got, tt.wantStatus)
			}
		})
	}

	fault := errors.New("handoff request 7: connection reset")
	if got := planningToolError(fault); got != fault {
		t.Fatalf("a fault was wrapped: %v (%T)", got, fault)
	}
	if got := planningToolError(context.Canceled); got != context.Canceled {
		t.Fatalf("a cancellation was wrapped: %v", got)
	}
}

// TestRequestPlanningMissingArgumentsAreNamed: a required argument that is absent
// is the adapter's own refusal, and it names the argument, so a model can fix its
// call without guessing. It runs before the service, so a malformed call costs no
// query — the caller here is a bare Server with no service, and a call that got
// past the decode would panic rather than pass quietly.
func TestRequestPlanningMissingArgumentsAreNamed(t *testing.T) {
	srv := requestPlanningServer(t)
	ctx := auth.WithIdentity(context.Background(), &auth.Identity{Subject: "local-dev", LocalDev: true})
	for _, name := range planningToolNames {
		raw, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0", "id": 1, "method": "tools/call",
			"params": map[string]any{"name": name, "arguments": map[string]any{}},
		})
		b, _ := json.Marshal(srv.HandleMessage(ctx, raw))
		var resp struct {
			Error *struct{ Message string } `json:"error"`
		}
		if err := json.Unmarshal(b, &resp); err != nil {
			t.Fatalf("%s: decode response: %v (%s)", name, err, b)
		}
		if resp.Error == nil {
			t.Fatalf("%s accepted an empty argument set", name)
		}
		if !strings.Contains(resp.Error.Message, "missing required argument") {
			t.Errorf("%s refusal %q does not name the missing argument", name, resp.Error.Message)
		}
	}
}
