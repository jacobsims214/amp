package mcp

// The inbox work tools' adapter-level contract (epic 202, story 549, AMP #3969;
// approved contract artifact 36 v2 §1/§2/§5; build slice I2).
//
// What is proven here is exactly what these seven handlers decide for
// themselves, because everything product-shaped — who may read an inbox, who
// may claim or assign, whether a rev is a compare-and-set, what a lost claim
// race says, whose voice a comment speaks in, what a requester sees — belongs
// to the service methods they call and is proven against a real database in
// cmd/server/request_work_mcp_test.go. A rule this file had to test twice would
// be a rule implemented twice, which is the drift the thin-adapter rule exists
// to prevent.
//
// So the five things tested here are the five the adapters own:
//
//  1. The registered surface: exactly the seven tools of this slice, each
//     catalogued with truthful flags (two reads readOnly, four writes
//     idempotent only because their replays are the service's own no-ops), with
//     a default deadline, with readOnly() agreeing about the two reads — and no
//     eighth tool that would accept an outcome, deliver, attach or plan.
//  2. The schemas: required parameters declared, bounds matching the domain's,
//     the lifecycle vocabulary as an enum, and no argument through which a
//     caller could name an actor, an acceptance or a plan target.
//  3. The strict argument readers: a present-but-wrong-typed filter, cursor,
//     revision, flag or string is refused 400 request.invalid rather than
//     silently dropped, because each of those defaults changes what the caller
//     believes it did (architecture/mcp-argument-typing.md).
//  4. The two normalizers that run before the service (states, source kind), so
//     a filter typo is request.invalid naming the field instead of a
//     repository error a client reads as a fault — or, worse, an empty queue.
//  5. The refusal envelope: the same request.* status/code in the error's data
//     that amp_submit_request gives, and the error chain left intact so the
//     denied middleware still audits a 403.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/simstech/amp-api/internal/auth"
	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
)

// workToolNames is the surface this slice promises: seven tools, three reads and
// four triage acts. Hardcoded here on purpose — the production list is compared
// against it, so a tool added or removed without a decision about its scope,
// catalog entry and rules fails this test rather than shipping quietly.
var workToolNames = []string{
	"amp_list_requests",
	"amp_get_request",
	"amp_update_request",
	"amp_comment_request",
	"amp_claim_request",
	"amp_release_request",
	"amp_assign_request",
}

// requestWorkServer registers the slice's tools on a bare mcp-go server, so the
// schemas, annotations and tool list under test are the ones that ship.
func requestWorkServer(t *testing.T) *mcpserver.MCPServer {
	t.Helper()
	srv := mcpserver.NewMCPServer("amp-api-test", "2.0.0", mcpserver.WithToolCapabilities(false))
	(&Server{}).registerRequestWork(srv)
	return srv
}

// requestWorkTool returns one registered tool by name.
func requestWorkTool(t *testing.T, srv *mcpserver.MCPServer, name string) mcpgo.Tool {
	t.Helper()
	st := srv.GetTool(name)
	if st == nil {
		t.Fatalf("registerRequestWork did not register %s", name)
	}
	return st.Tool
}

// listedNames asks the server's own tools/list what it advertises.
func listedNames(t *testing.T, srv *mcpserver.MCPServer) []string {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
	b, err := json.Marshal(srv.HandleMessage(context.Background(), raw))
	if err != nil {
		t.Fatalf("marshal tools/list response: %v", err)
	}
	var resp struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(b, &resp); err != nil {
		t.Fatalf("decode tools/list: %v (%s)", err, b)
	}
	out := make([]string, 0, len(resp.Result.Tools))
	for _, tl := range resp.Result.Tools {
		out = append(out, tl.Name)
	}
	return out
}

// ---- 1: the registered surface ----

// TestRequestWorkSurfaceIsExactlyTheSevenTools: the slice registers the seven
// tools it is scoped to and nothing else. "Nothing else" is the load-bearing
// half: an eighth tool that accepted an outcome, delivered a request, attached
// it to planning or added a file would be a capability this ticket is explicitly
// not allowed to ship, and it would be reachable the moment it existed.
func TestRequestWorkSurfaceIsExactlyTheSevenTools(t *testing.T) {
	srv := requestWorkServer(t)
	got := listedNames(t, srv)
	if len(got) != len(workToolNames) {
		t.Fatalf("registered tools = %v, want exactly %v", got, workToolNames)
	}
	for i, name := range workToolNames {
		if got[i] != name && !containsName(got, name) {
			t.Errorf("%s is not registered (got %v)", name, got)
		}
	}
	if prod := requestWorkToolNames(); len(prod) != len(workToolNames) {
		t.Fatalf("requestWorkToolNames() = %v, want %v", prod, workToolNames)
	} else {
		for i := range prod {
			if prod[i] != workToolNames[i] {
				t.Fatalf("requestWorkToolNames() = %v, want %v", prod, workToolNames)
			}
		}
	}
	// Nothing plan-, acceptance-, delivery- or file-shaped is here, by name.
	for _, name := range got {
		for _, forbidden := range []string{"accept", "deliver", "attach", "link", "handoff", "transition", "approve", "dispatch"} {
			if strings.Contains(name, forbidden) {
				t.Errorf("tool %q is part of the inbox surface this slice may not add (%q)", name, forbidden)
			}
		}
	}
}

func containsName(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

// TestRequestWorkCatalogFlags pins what the catalog promises a model, which is
// the one place a flag can be a lie:
//
//   - the two reads are readOnly, and readOnly() agrees, so a timeout on them
//     says "retry it once";
//   - the four writes are idempotent, and that is TRUE rather than optimistic:
//     the service makes a repeated claim/release/assignment a changed=false
//     no-op and a comment retried by its id the same row, so a retry after a
//     lost response cannot double-write;
//   - none is destructive and none is personOnly/adminOnly: triage is a team
//     member's work, so the tools are listed for members;
//   - every write stays out of readOnly(), so its timeout wording is "check the
//     result before retrying" — the honest instruction for a write.
func TestRequestWorkCatalogFlags(t *testing.T) {
	reads := map[string]bool{"amp_list_requests": true, "amp_get_request": true}
	for _, name := range workToolNames {
		info, ok := toolCatalog[name]
		if !ok {
			t.Errorf("toolCatalog has no entry for %s (an uncatalogued tool gets no title, no annotations and silently defaults to listed)", name)
			continue
		}
		if info.title == "" || info.keywords == "" {
			t.Errorf("%s catalog entry is thin: title %q, keywords %q", name, info.title, info.keywords)
		}
		if info.readOnly != reads[name] {
			t.Errorf("%s readOnly = %v, want %v", name, info.readOnly, reads[name])
		}
		if !reads[name] && !info.idempotent {
			t.Errorf("%s is a write that is not flagged idempotent; a replay of it writes nothing", name)
		}
		if reads[name] && info.idempotent {
			t.Errorf("%s is flagged idempotent as well as readOnly; the annotations are one hint each", name)
		}
		if info.destructive {
			t.Errorf("%s is flagged destructive; nothing in this slice deletes", name)
		}
		if info.listing != listed {
			t.Errorf("%s listing = %v, want listed: a team member's triage tools must be findable", name, info.listing)
		}
		// Writes must not be in robust.go's read-only list, or a timeout would
		// tell a client to retry blind. Reads must be, or a timeout would tell it
		// to stop and check a result that cannot have changed.
		if got := readOnly(name); got != reads[name] {
			t.Errorf("readOnly(%q) = %v, want %v (robust.go's timeout wording follows it)", name, got, reads[name])
		}
		if got := ToolDeadline(name); got != DefaultToolDeadline {
			t.Errorf("ToolDeadline(%q) = %s; one request read/write is one query set and needs no override", name, got)
		}
	}

	// Annotations reach the wire as the catalog says.
	srv := requestWorkServer(t)
	for _, name := range workToolNames {
		tl := requestWorkTool(t, srv, name)
		want := toolCatalog[name]
		if tl.Annotations.Title != want.title {
			t.Errorf("%s annotations = %+v, want title %q", name, tl.Annotations, want.title)
		}
		if tl.Annotations.ReadOnlyHint == nil || *tl.Annotations.ReadOnlyHint != want.readOnly {
			t.Errorf("%s ReadOnlyHint = %v, want %v", name, tl.Annotations.ReadOnlyHint, want.readOnly)
		}
		if tl.Annotations.DestructiveHint == nil || *tl.Annotations.DestructiveHint {
			t.Errorf("%s DestructiveHint = %v, want false", name, tl.Annotations.DestructiveHint)
		}
		if tl.Annotations.IdempotentHint == nil || *tl.Annotations.IdempotentHint != (want.idempotent || want.readOnly) {
			t.Errorf("%s IdempotentHint = %v, want %v", name, tl.Annotations.IdempotentHint, want.idempotent || want.readOnly)
		}
	}
}

// TestRequestWorkKeepsTheSubmitScopeToOneTool is AMP #3772's boundary asserted
// from this slice's side: a submit-only credential may call amp_submit_request
// and nothing else. Adding these seven tools must not widen that set, and the
// scope's own gate (submitScopeGate) refuses them for a scoped credential
// regardless of what tools/list shows.
func TestRequestWorkKeepsTheSubmitScopeToOneTool(t *testing.T) {
	allowed := submitScopeToolNames()
	if len(allowed) != 1 || !allowed[SubmitScopeTool] {
		t.Fatalf("submitScopeToolNames() = %v, want only %q: a submission credential reads no inbox", allowed, SubmitScopeTool)
	}
	for _, name := range workToolNames {
		if allowed[name] {
			t.Errorf("%s is callable by a submit-only credential: submitting is not seeing, and it is not triaging", name)
		}
	}
	// The refusal is a scope refusal, not a 404 that would confirm a team.
	err := authz.TokenScopeForbidden()
	if authz.StatusOf(err, 0) != 403 || authz.CodeOf(err) != authz.CodeTokenScopeDenied {
		t.Fatalf("scope refusal = %d %q", authz.StatusOf(err, 0), authz.CodeOf(err))
	}
}

// TestRequestWorkUsesOpenNeverGuard is the wiring rule from
// discoveries/team-request-api-contract.md gotcha 2, checked against the source
// rather than trusted: s.guard resolves a project and requires a Level on it,
// which a team inbox must never demand of a triager or a requester. The same
// scan is why "no acceptance, no delivery, no attachment, no planning" is a
// checkable property and not only a sentence in a comment.
func TestRequestWorkUsesOpenNeverGuard(t *testing.T) {
	src, err := os.ReadFile("request_work.go")
	if err != nil {
		t.Fatalf("read request_work.go: %v", err)
	}
	code := string(src)
	for _, forbidden := range []string{
		// Authorization must go through the service, not a project ladder.
		"s.guard(", "s.projectOf(",
		// No lifecycle, acceptance or delivery: those are slices F3/F4/I3 and
		// acceptance is the requester's own act in the UI (I1, D5).
		"svc.TransitionRequest(", "svc.AcceptRequest(", "svc.AnswerRequest(", "svc.ReopenRequest(",
		// No planning link, no handoff, no attachment: slice I3's surface.
		"svc.AttachRequest(", "svc.AddRequestAttachment(",
		// No approval or dispatch path, ever, from a request.
		"svc.ApprovePlan(", "svc.Dispatch(",
	} {
		if strings.Contains(code, forbidden) {
			t.Errorf("request_work.go calls %s; the inbox work tools act only through the reviewed read/triage services", forbidden)
		}
	}
	// And each registered handler is wrapped in open, so every call resolves the
	// caller from the request context (never from an argument).
	if got := strings.Count(code, "wrap(ctx, s.open(s."); got != len(workToolNames) {
		t.Errorf("%d handlers wired through s.open, want %d: every tool in this file authorizes at the service", got, len(workToolNames))
	}
}

// ---- 2: the schemas ----

// TestRequestWorkSchemas: each tool declares what it needs and nothing it must
// not offer. The negatives are the point — no argument can name the actor (a
// claim or a comment is always the caller's own), carry an acceptance or a
// delivery, or aim a request at a project.
func TestRequestWorkSchemas(t *testing.T) {
	srv := requestWorkServer(t)
	wantRequired := map[string][]string{
		"amp_list_requests": {"team_id"},
		// A read names the request one of two ways (`request_id` with
		// `team_id`, or `public_ref`), so neither is unconditionally required and
		// the service refuses "neither" and "both" itself — the one place that
		// can tell the two surfaces' addressing apart.
		"amp_get_request":     nil,
		"amp_update_request":  {"expected_rev", "request_id", "team_id"},
		"amp_comment_request": {"body", "request_id", "team_id"},
		"amp_claim_request":   {"request_id", "team_id"},
		"amp_release_request": {"request_id", "team_id"},
		"amp_assign_request":  {"assignee", "request_id", "team_id"},
	}
	// Nothing in this slice may offer a way to set who is acting, what the
	// outcome was, or whether the requester accepted.
	forbiddenProps := []string{
		"user_id", "actor", "actor_user_id", "author", "author_user_id",
		"state", "status", "to", "transition", "outcome", "accept", "accepted",
		"deliver", "delivered", "requester_accepted", "project_id", "epic_id", "story_id", "task_id",
		"owner_user_id", "claim_user_id", "duplicate_of", "reject_reason",
	}
	for _, name := range workToolNames {
		t.Run(name, func(t *testing.T) {
			tl := requestWorkTool(t, srv, name)
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
			// The team is the tenant: every tool names one, so no read or write
			// can be aimed at "whichever team" and be resolved from a project.
			if _, ok := s.Properties["team_id"]; !ok {
				t.Error("schema has no team_id: the inbox's tenant is always named")
			}
		})
	}

	t.Run("list filter vocabulary", func(t *testing.T) {
		s := decodeSchema(t, requestWorkTool(t, srv, "amp_list_requests"))
		states, ok := s.Properties["states"]["items"].(map[string]interface{})
		if !ok {
			t.Fatalf("states has no items schema: %v", s.Properties["states"])
		}
		enum, _ := states["enum"].([]interface{})
		if len(enum) != len(domain.RequestStatuses) {
			t.Fatalf("states enum = %v, want the %d lifecycle states", enum, len(domain.RequestStatuses))
		}
		for i, st := range domain.RequestStatuses {
			if enum[i] != string(st) {
				t.Errorf("states enum[%d] = %v, want %q", i, enum[i], st)
			}
		}
		// A filter that is offered but not read is worse than one that is not
		// offered: it silently returns a wider queue than the caller asked for.
		for _, declared := range []string{"team_id", "states", "source_kind", "attached", "q", "mine", "requester_user_id", "before_id", "limit"} {
			if _, ok := s.Properties[declared]; !ok {
				t.Errorf("amp_list_requests has no %q argument", declared)
			}
		}
	})

	t.Run("edit bounds and the fields it refuses", func(t *testing.T) {
		s := decodeSchema(t, requestWorkTool(t, srv, "amp_update_request"))
		if got := s.Properties["subject"]["maxLength"]; got != float64(domain.MaxRequestSubjectLen) {
			t.Errorf("subject maxLength = %v, want %d", got, domain.MaxRequestSubjectLen)
		}
		if got := s.Properties["input_brief"]["maxLength"]; got != float64(domain.MaxRequestBriefLen) {
			t.Errorf("input_brief maxLength = %v, want %d", got, domain.MaxRequestBriefLen)
		}
		// outcome_brief and metadata are declared so a caller that sends them is
		// answered instead of silently ignored (the service refuses both); they
		// are not required and their descriptions say so.
		for _, refused := range []string{"outcome_brief", "metadata"} {
			p, ok := s.Properties[refused]
			if !ok {
				t.Errorf("amp_update_request does not declare %q, so sending it would be dropped rather than refused", refused)
				continue
			}
			if d, _ := p["description"].(string); !strings.Contains(strings.ToLower(d), "refus") {
				t.Errorf("%q's description does not say it is refused: %v", refused, d)
			}
		}
	})

	t.Run("comment bounds and kinds", func(t *testing.T) {
		s := decodeSchema(t, requestWorkTool(t, srv, "amp_comment_request"))
		if got := s.Properties["body"]["maxLength"]; got != float64(domain.MaxRequestBriefLen) {
			t.Errorf("body maxLength = %v, want %d", got, domain.MaxRequestBriefLen)
		}
		enum, _ := s.Properties["kind"]["enum"].([]interface{})
		want := []string{"comment", "question", "answer"}
		if len(enum) != len(want) {
			t.Fatalf("kind enum = %v, want %v (system is never a caller's)", enum, want)
		}
		for i, k := range want {
			if enum[i] != k {
				t.Errorf("kind enum = %v, want %v", enum, want)
			}
		}
	})
}

// ---- 3: the strict readers ----

// TestOptIntStrictRefusesWrongTypes: absent and present-null are absence, a
// whole number is the value, and everything else — a boolean, an object, an
// array, a fraction — is refused 400 request.invalid naming the argument and the
// JSON type the caller sent, never the value.
func TestOptIntStrictRefusesWrongTypes(t *testing.T) {
	tests := []struct {
		name    string
		args    map[string]interface{}
		wantV   int
		wantOk  bool
		wantErr bool
	}{
		{"absent", map[string]interface{}{}, 0, false, false},
		{"null", map[string]interface{}{"expected_rev": nil}, 0, false, false},
		{"float64 as JSON number", map[string]interface{}{"expected_rev": float64(7)}, 7, true, false},
		{"int", map[string]interface{}{"expected_rev": 7}, 7, true, false},
		{"int64", map[string]interface{}{"expected_rev": int64(7)}, 7, true, false},
		{"zero is a value", map[string]interface{}{"expected_rev": float64(0)}, 0, true, false},
		{"fraction", map[string]interface{}{"expected_rev": 1.5}, 0, false, true},
		{"string", map[string]interface{}{"expected_rev": "7"}, 0, false, true},
		{"boolean", map[string]interface{}{"expected_rev": true}, 0, false, true},
		{"object", map[string]interface{}{"expected_rev": map[string]interface{}{"n": float64(1)}}, 0, false, true},
		{"array", map[string]interface{}{"expected_rev": []interface{}{float64(1)}}, 0, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v, ok, err := optIntStrict(tt.args, "expected_rev", "the revision you read")
			if tt.wantErr {
				if err == nil {
					t.Fatalf("%v was accepted as %d", tt.args["expected_rev"], v)
				}
				if authz.StatusOf(err, 0) != 400 || authz.CodeOf(err) != authz.CodeRequestInvalid {
					t.Fatalf("refusal = %d %q, want 400 %s", authz.StatusOf(err, 0), authz.CodeOf(err), authz.CodeRequestInvalid)
				}
				if !strings.Contains(err.Error(), "expected_rev") {
					t.Errorf("refusal %q does not name the argument", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected refusal: %v", err)
			}
			if v != tt.wantV || ok != tt.wantOk {
				t.Fatalf("got (%d, %v), want (%d, %v)", v, ok, tt.wantV, tt.wantOk)
			}
		})
	}
}

// TestOptBoolStrictRefusesWrongTypes: the same rule for a filter flag. A
// silently-dropped `attached` flips the queue the caller asked for back to the
// unfiltered one, which is a wrong answer that looks like a right one.
func TestOptBoolStrictRefusesWrongTypes(t *testing.T) {
	tests := []struct {
		name    string
		args    map[string]interface{}
		wantV   bool
		wantOk  bool
		wantErr bool
	}{
		{"absent", map[string]interface{}{}, false, false, false},
		{"null", map[string]interface{}{"attached": nil}, false, false, false},
		{"true", map[string]interface{}{"attached": true}, true, true, false},
		{"false is a value", map[string]interface{}{"attached": false}, false, true, false},
		{"string", map[string]interface{}{"attached": "true"}, false, false, true},
		{"number", map[string]interface{}{"attached": float64(1)}, false, false, true},
		{"object", map[string]interface{}{"attached": map[string]interface{}{}}, false, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v, ok, err := optBoolStrict(tt.args, "attached", "a filter flag")
			if tt.wantErr {
				if err == nil {
					t.Fatalf("%v was accepted as %v", tt.args["attached"], v)
				}
				if authz.CodeOf(err) != authz.CodeRequestInvalid {
					t.Fatalf("code = %q, want %q", authz.CodeOf(err), authz.CodeRequestInvalid)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected refusal: %v", err)
			}
			if v != tt.wantV || ok != tt.wantOk {
				t.Fatalf("got (%v, %v), want (%v, %v)", v, ok, tt.wantV, tt.wantOk)
			}
		})
	}
}

// TestOptStrPtrStrictKeepsAbsentApartFromEmpty: an edit's clear-the-field
// depends on the difference, and neither may be confused with a wrong type.
func TestOptStrPtrStrictKeepsAbsentApartFromEmpty(t *testing.T) {
	if p, err := optStrPtrStrict(map[string]interface{}{}, "input_brief", "why"); err != nil || p != nil {
		t.Fatalf("absent = (%v, %v), want (nil, nil): leave the field as stored", p, err)
	}
	if p, err := optStrPtrStrict(map[string]interface{}{"input_brief": nil}, "input_brief", "why"); err != nil || p != nil {
		t.Fatalf("null = (%v, %v), want (nil, nil): absence, as REST's decoder reads it", p, err)
	}
	p, err := optStrPtrStrict(map[string]interface{}{"input_brief": ""}, "input_brief", "why")
	if err != nil || p == nil || *p != "" {
		t.Fatalf("empty = (%v, %v), want a pointer to \"\": clear the field", p, err)
	}
	for _, bad := range []interface{}{float64(1), true, map[string]interface{}{}, []interface{}{}} {
		if _, err := optStrPtrStrict(map[string]interface{}{"input_brief": bad}, "input_brief", "why"); err == nil {
			t.Errorf("%T was accepted", bad)
		} else if authz.CodeOf(err) != authz.CodeRequestInvalid {
			t.Errorf("%T refusal code = %q", bad, authz.CodeOf(err))
		}
	}
}

// ---- 4: the normalizers that run before the service ----

// TestOptRequestStates: the filter is normalized through the domain, so its
// vocabulary is the lifecycle's own; an unknown state is a named refusal (a typo
// matching nothing reads as "the queue is empty", the one answer a queue must
// never invent); and an empty entry is refused rather than read as the initial
// state, which is what the domain's empty-means-default rule would otherwise do
// inside a list.
func TestOptRequestStates(t *testing.T) {
	tests := []struct {
		name    string
		args    map[string]interface{}
		want    []domain.RequestStatus
		wantErr bool
	}{
		{"absent", map[string]interface{}{}, nil, false},
		{"null", map[string]interface{}{"states": nil}, nil, false},
		{"empty list means every state", map[string]interface{}{"states": []interface{}{}}, nil, false},
		{"one state", map[string]interface{}{"states": []interface{}{"new"}}, []domain.RequestStatus{domain.RequestStatusNew}, false},
		{"two states, normalized", map[string]interface{}{"states": []interface{}{"TRIAGING", " new "}},
			[]domain.RequestStatus{domain.RequestStatusTriaging, domain.RequestStatusNew}, false},
		{"unknown state", map[string]interface{}{"states": []interface{}{"pending"}}, nil, true},
		{"empty entry", map[string]interface{}{"states": []interface{}{""}}, nil, true},
		{"blank entry", map[string]interface{}{"states": []interface{}{"  "}}, nil, true},
		{"not a list", map[string]interface{}{"states": "new"}, nil, true},
		{"number entry", map[string]interface{}{"states": []interface{}{float64(1)}}, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := optRequestStates(tt.args, "states")
			if tt.wantErr {
				if err == nil {
					t.Fatalf("%v was accepted as %v", tt.args["states"], got)
				}
				if authz.StatusOf(err, 0) != 400 || authz.CodeOf(err) != authz.CodeRequestInvalid {
					t.Fatalf("refusal = %d %q, want 400 %s", authz.StatusOf(err, 0), authz.CodeOf(err), authz.CodeRequestInvalid)
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

// TestRequestFilterSourceKind: the source filter is one of the four surfaces a
// request can arrive on — the closed set migration 053's CHECK pins — and
// anything else is refused here so it cannot reach a client as a repository
// fault.
func TestRequestFilterSourceKind(t *testing.T) {
	tests := []struct {
		in      string
		want    domain.RequestSourceKind
		wantErr bool
	}{
		{"", "", false},
		{"  ", "", false},
		{"web", domain.RequestSourceWeb, false},
		{"MCP", domain.RequestSourceMCP, false},
		{"api", domain.RequestSourceAPI, false},
		{"connector", domain.RequestSourceConnector, false},
		{"email", "", true},
		{"webhook", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := requestFilterSourceKind(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("%q was accepted as %q", tt.in, got)
				}
				if authz.CodeOf(err) != authz.CodeRequestInvalid {
					t.Fatalf("code = %q, want %q", authz.CodeOf(err), authz.CodeRequestInvalid)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected refusal: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// ---- 5: the refusal envelope ----

// TestWorkToolErrorMatchesTheSubmissionEnvelope: a request.* refusal from these
// tools reaches a client exactly as amp_submit_request's does — the service's
// message, REST's status and stable code in data, and the chain intact so
// authz.CodeOf/StatusOf and the denied middleware keep working. A fault stays
// unwrapped so an unexplained failure keeps looking unexplained.
func TestWorkToolErrorMatchesTheSubmissionEnvelope(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{"not found is the same answer for every id", authz.RequestNotFound(), 404, authz.CodeRequestNotFound},
		{"stale revision", authz.RequestConflict("request 7 is at rev 4, not rev 3"), 409, authz.CodeRequestConflict},
		{"invalid names the field", authz.RequestInvalid("expected_rev is required on an edit"), 400, authz.CodeRequestInvalid},
		{"a forbidden act is a 403", authz.Forbidden("assigning a request is the inbox team's act"), 403, ""},
		{"a scope refusal keeps its own code", authz.TokenScopeForbidden(), 403, authz.CodeTokenScopeDenied},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := workToolError(tt.err)
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
			if got, _ := data["code"].(string); got != tt.wantCode {
				t.Errorf("data.code = %q, want %q", got, tt.wantCode)
			}
			// Nothing in this slice is rate limited, so no refusal carries a wait.
			if _, has := data["retry_after_s"]; has {
				t.Errorf("data carries retry_after_s on a refusal that has no wait: %v", data)
			}
		})
	}

	fault := errors.New("list requests: connection reset")
	if got := workToolError(fault); got != fault {
		t.Fatalf("a fault was wrapped: %v (%T)", got, fault)
	}
	if got := workToolError(context.Canceled); got != context.Canceled {
		t.Fatalf("a cancellation was wrapped: %v", got)
	}
}

// TestRequestWorkMissingArgumentsAreNamed: a required argument that is absent is
// the adapter's own refusal, and it names the argument, so a model can fix its
// call without guessing. It runs before the service, so a malformed call costs
// no query — the caller here is a bare Server with no service, and a call that
// got past the decode would panic rather than pass quietly.
//
// amp_get_request is deliberately not in the table: it has no unconditionally
// required argument, because a read names the request one of two ways and "which
// of the two" is a decision only the service can make (it refuses neither and
// both with request.invalid). Its schema is covered above.
func TestRequestWorkMissingArgumentsAreNamed(t *testing.T) {
	srv := requestWorkServer(t)
	ctx := auth.WithIdentity(context.Background(), &auth.Identity{Subject: "local-dev", LocalDev: true})
	for _, name := range workToolNames {
		if name == "amp_get_request" {
			continue
		}
		args := map[string]interface{}{}
		raw, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0", "id": 1, "method": "tools/call",
			"params": map[string]any{"name": name, "arguments": args},
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
		if !strings.Contains(resp.Error.Message, "team_id") && !strings.Contains(resp.Error.Message, "missing required argument") {
			t.Errorf("%s refusal %q does not name the missing argument", name, resp.Error.Message)
		}
	}
}
