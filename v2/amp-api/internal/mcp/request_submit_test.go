package mcp

// amp_submit_request's adapter-level contract (epic 202, story 543, AMP #3920).
//
// What this file proves is exactly what the handler itself decides, because
// everything else — who may submit, what a repeat means, the bounds, the
// limiter, the audit row — belongs to Service.SubmitRequest and is proven
// against a real database in cmd/server/request_submit_service_test.go's
// sibling suite and, through this tool, in
// cmd/server/request_submit_mcp_test.go. The split matters: a rule that this
// file had to test twice would be a rule implemented twice, which is what the
// adapter is meant to avoid.
//
// So the four things tested here are the four the adapter owns:
//
//  1. **The schema.** team_id and subject are REQUIRED, the payload bounds are
//     declared next to the fields they bound, and no identity or project field
//     exists at all — a caller cannot be asked for a project, a requester, an
//     org or a source, so it cannot supply one (TestSubmitRequestToolSchema).
//  2. **The registration.** One tool, catalogued, advertised, honest about
//     retries and about why a timeout is not safe to blind-retry, and named the
//     same string the submit-scope gate and list use — so a scoped credential's
//     one tool and the gate that enforces it cannot drift apart
//     (TestSubmitRequestRegistration, TestSubmitRequestIsTheSubmitScopeTool).
//  3. **Spoofing.** A payload naming who asked or how it arrived is refused with
//     request.invalid instead of quietly dropped (TestRefuseSpoofedSubmitArgs).
//  4. **The error envelope.** A refusal reaches the client with REST's status
//     and stable code in the error's data (and retry_after_s on a 429), while
//     the error chain still answers authz.StatusOf/CodeOf as before — which is
//     what keeps the denied middleware's 403 audit working
//     (TestSubmitToolError*).

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/service"
)

// submitTool is the registered tool, found by the name the scope gate knows.
// It registers the real handler set on a bare mcp-go server, so the schema and
// annotations under test are the ones that ship rather than a copy of them.
func submitTool(t *testing.T) mcpgo.Tool {
	t.Helper()
	srv := mcpserver.NewMCPServer("amp-api-test", "2.0.0", mcpserver.WithToolCapabilities(false))
	(&Server{}).registerRequests(srv)
	st := srv.GetTool(SubmitScopeTool)
	if st == nil {
		t.Fatalf("registerRequests did not register %s", SubmitScopeTool)
	}
	return st.Tool
}

// schemaOf decodes a tool's raw input schema into the shape the checks below
// read: properties (name -> schema map) and required names.
type schemaOf struct {
	Properties map[string]map[string]interface{} `json:"properties"`
	Required   []string                          `json:"required"`
}

func decodeSchema(t *testing.T, tl mcpgo.Tool) schemaOf {
	t.Helper()
	// $"tool()" keeps the schema it built in RawInputSchema (presentTool is
	// applied to NewToolWithRawSchema), which is what reaches the wire.
	raw := []byte(tl.RawInputSchema)
	if len(raw) == 0 {
		var err error
		if raw, err = json.Marshal(tl.InputSchema); err != nil {
			t.Fatalf("marshal schema: %v", err)
		}
	}
	var s schemaOf
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("decode schema: %v (%s)", err, raw)
	}
	return s
}

// TestSubmitRequestToolSchema is the ticket's "REQUIRED field descriptions,
// bounded payload" half: the model calling the tool directly (not reading the
// prose) must be told which fields it cannot omit, how large they may be, and
// must not be offered a field that would let it name a project or an identity.
func TestSubmitRequestToolSchema(t *testing.T) {
	tl := submitTool(t)
	s := decodeSchema(t, tl)

	wantRequired := []string{"subject", "team_id"}
	if len(s.Required) != len(wantRequired) {
		t.Fatalf("required = %v, want %v", s.Required, wantRequired)
	}
	for i, name := range wantRequired {
		if s.Required[i] != name {
			t.Fatalf("required = %v, want %v (sorted)", s.Required, wantRequired)
		}
	}

	// Declared bounds, matching the domain's own (domain.MaxRequest*): a schema
	// that promised more than the service stores would be the kind of drift that
	// surfaces as a 400 after a long payload was already sent.
	if got := s.Properties["subject"]["maxLength"]; got != float64(domain.MaxRequestSubjectLen) {
		t.Errorf("subject maxLength = %v, want %d", got, domain.MaxRequestSubjectLen)
	}
	if got := s.Properties["input_brief"]["maxLength"]; got != float64(domain.MaxRequestBriefLen) {
		t.Errorf("input_brief maxLength = %v, want %d", got, domain.MaxRequestBriefLen)
	}
	if got := s.Properties["metadata"]["type"]; got != "object" {
		t.Errorf("metadata type = %v, want object", got)
	}

	// Nothing project-shaped, nothing identity-shaped: the submission names a
	// team and describes work (contract §2/§5, "no project", "the adapter sets
	// who and how").
	for _, forbidden := range []string{
		"project_id", "org_id", "requester", "requester_user_id", "requester_contact",
		"source", "source_kind", "actor", "user_id", "story_id", "epic_id", "task_id",
	} {
		if _, ok := s.Properties[forbidden]; ok {
			t.Errorf("schema offers %q; a submission may not name it", forbidden)
		}
	}

	// Every present field says what it is for, and the description of the tool
	// itself carries the two things a model has to know to retry safely.
	for name, p := range s.Properties {
		if d, _ := p["description"].(string); strings.TrimSpace(d) == "" {
			t.Errorf("property %q has no description", name)
		}
	}
	for _, want := range []string{"idempotency_key", "created=false"} {
		if !strings.Contains(tl.Description, want) {
			t.Errorf("tool description doesn't mention %q: %s", want, tl.Description)
		}
	}
}

// TestSubmitRequestRegistration pins what the catalog says about the tool: it
// is listed, it is not read-only, and it is NOT flagged idempotent — because a
// retry without an idempotency_key submits a second request. A model that read
// an idempotent hint would drop the key and double-submit.
func TestSubmitRequestRegistration(t *testing.T) {
	info, ok := toolCatalog[SubmitScopeTool]
	if !ok {
		t.Fatalf("toolCatalog has no entry for %s (a tool missing from the catalog gets no title, no annotations and silently defaults to listed)", SubmitScopeTool)
	}
	if info.listing != listed {
		t.Errorf("%s listing = %v, want listed: a team member submitting a request should find it", SubmitScopeTool, info.listing)
	}
	if info.readOnly {
		t.Error("amp_submit_request is flagged readOnly; it writes a request and an audit row")
	}
	if info.idempotent {
		t.Error("amp_submit_request is flagged idempotent; it only is when the caller sends an idempotency_key, so the hint would promise more than the tool keeps")
	}
	if info.destructive {
		t.Error("amp_submit_request is flagged destructive; it creates a request and changes nothing else")
	}
	if info.title == "" || info.keywords == "" {
		t.Errorf("catalog entry is thin: title %q, keywords %q", info.title, info.keywords)
	}

	// Annotations actually reach the wire, with idempotent false.
	tl := submitTool(t)
	if tl.Annotations.Title != info.title {
		t.Fatalf("annotations = %+v, want title %q", tl.Annotations, info.title)
	}
	if tl.Annotations.IdempotentHint == nil || *tl.Annotations.IdempotentHint {
		t.Errorf("IdempotentHint = %v, want false", tl.Annotations.IdempotentHint)
	}
	if tl.Annotations.ReadOnlyHint == nil || *tl.Annotations.ReadOnlyHint {
		t.Errorf("ReadOnlyHint = %v, want false", tl.Annotations.ReadOnlyHint)
	}

	// Retry semantics on a timeout: a write, so "may still complete — check
	// before retrying" rather than the read-only "retry it once". That wording
	// comes from robust.go's readOnly(), which this tool must stay out of.
	if readOnly(SubmitScopeTool) {
		t.Errorf("readOnly(%q) is true: its timeout error would tell a client to retry a submission blind", SubmitScopeTool)
	}
	if got := ToolDeadline(SubmitScopeTool); got != DefaultToolDeadline {
		t.Errorf("ToolDeadline(%q) = %s; a submission is one transaction and needs no override", SubmitScopeTool, got)
	}
}

// TestSubmitRequestIsTheSubmitScopeTool is the anti-drift check AMP #3772 names:
// the one tool a submit-only credential may call, the one its tools/list
// advertises, and the tool this slice registered must be the same name. They are
// already one constant, and this asserts that nothing (a rename, a second
// registration) split them.
func TestSubmitRequestIsTheSubmitScopeTool(t *testing.T) {
	tl := submitTool(t)
	if tl.Name != SubmitScopeTool {
		t.Fatalf("registered name %q != SubmitScopeTool %q", tl.Name, SubmitScopeTool)
	}
	allowed := submitScopeToolNames()
	if len(allowed) != 1 || !allowed[tl.Name] {
		t.Fatalf("submitScopeToolNames() = %v, want only %q", allowed, tl.Name)
	}
	// And a submit-only credential is shown exactly this one tool, however many
	// other request tools the surface grows: the read/triage tools (AMP #3969)
	// exist, and each of them must stay outside the scope's one-tool set — the
	// scope is what trims tools/list for a bound credential, not the absence of
	// the other tools. internal/mcp/request_work_test.go asserts the seven names
	// individually; this keeps the two sets from ever overlapping.
	for name := range toolCatalog {
		if name == tl.Name {
			continue
		}
		if allowed[name] {
			t.Errorf("a submit-only credential may call %q: submitting is not reading an inbox and it is not triage", name)
		}
	}
}

// TestRefuseSpoofedSubmitArgs: a payload that names who asked, which tenant the
// request belongs to, how it arrived or which project it is aimed at is refused
// with request.invalid naming the field, rather than silently ignored — a caller
// must never believe it set a requester it did not, nor that a request landed
// next to a project intake never chooses. Absent and blank values are the
// absence they look like.
func TestRefuseSpoofedSubmitArgs(t *testing.T) {
	tests := []struct {
		name string
		args map[string]interface{}
		want string // the field the refusal must name; "" for allowed
	}{
		{"clean payload", map[string]interface{}{"team_id": float64(1), "subject": "x"}, ""},
		{"requester_user_id", map[string]interface{}{"requester_user_id": float64(7)}, "requester_user_id"},
		{"requester_contact", map[string]interface{}{"requester_contact": "someone@example.com"}, "requester_contact"},
		{"requester as object", map[string]interface{}{"requester": map[string]interface{}{"id": float64(7)}}, "requester"},
		{"org_id", map[string]interface{}{"org_id": float64(3)}, "org_id"},
		{"source_kind", map[string]interface{}{"source_kind": "connector"}, "source_kind"},
		{"source", map[string]interface{}{"source": "web"}, "source"},
		{"actor", map[string]interface{}{"actor": "someone"}, "actor"},
		{"project_id", map[string]interface{}{"project_id": float64(9)}, "project_id"},
		{"story_id", map[string]interface{}{"story_id": float64(9)}, "story_id"},
		{"task_id", map[string]interface{}{"task_id": float64(9)}, "task_id"},
		{"source_external_id", map[string]interface{}{"source_external_id": "PD-1234"}, "source_external_id"},
		{"null is absence", map[string]interface{}{"requester_contact": nil, "project_id": nil}, ""},
		{"blank is absence", map[string]interface{}{"requester_contact": "", "source": "  ", "project_id": ""}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := refuseSpoofedSubmitArgs(tt.args)
			if tt.want == "" {
				if err != nil {
					t.Fatalf("refused a payload it should allow: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("%v was allowed", tt.args)
			}
			if authz.CodeOf(err) != authz.CodeRequestInvalid || authz.StatusOf(err, 0) != 400 {
				t.Fatalf("refusal = %d %q, want 400 %s", authz.StatusOf(err, 0), authz.CodeOf(err), authz.CodeRequestInvalid)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("refusal %q doesn't name %q", err, tt.want)
			}
		})
	}
}

// TestSubmitToolErrorCarriesStatusAndCode is the ticket's "coded errors parity
// with REST": the same request.* code REST puts next to "error" reaches an MCP
// client, in the error's data, next to the same status.
func TestSubmitToolErrorCarriesStatusAndCode(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantCode string
		wantStat int
	}{
		{"invalid names the field", authz.RequestInvalid("subject is required: one line saying what the request is about"),
			authz.CodeRequestInvalid, 400},
		{"conflict from a reused key", authz.RequestConflict("this idempotency key already produced request 12 from a different payload"),
			authz.CodeRequestConflict, 409},
		{"rate limited", authz.RequestRateLimited(42, "too many submissions; try again shortly"),
			authz.CodeRequestRateLimited, 429},
		{"not found for a foreign team", authz.NotFound("team 9 not found"), "", 404},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := submitToolError(tt.err)
			if err == nil {
				t.Fatal("submitToolError returned nil")
			}
			// The chain still answers the questions the middleware and any
			// caller already ask of a refusal.
			if got := authz.CodeOf(err); got != tt.wantCode {
				t.Errorf("CodeOf = %q, want %q", got, tt.wantCode)
			}
			if got := authz.StatusOf(err, 0); got != tt.wantStat {
				t.Errorf("StatusOf = %d, want %d", got, tt.wantStat)
			}
			assertToolErrorData(t, err, tt.wantStat, tt.wantCode, nil)
		})
	}
}

// assertToolErrorData checks what a client reads on a refusal: the service's own
// message, its status and stable code in data, and retry_after_s only when the
// caller passed the wait a 429 carries.
func assertToolErrorData(t *testing.T, err error, wantStatus int, wantCode string, wantRetry *int) {
	t.Helper()
	var coded *requestToolError
	if !errors.As(err, &coded) {
		t.Fatalf("error is not a requestToolError (%T)", err)
	}
	rpc := coded.JSONRPCError()
	if rpc.Error.Message != coded.err.Error() {
		t.Errorf("message = %q, want the service's own %q", rpc.Error.Message, coded.err.Error())
	}
	if rpc.Error.Code != mcpgo.INTERNAL_ERROR {
		t.Errorf("JSON-RPC code = %d, want the %d every AMP tool refusal arrives as (the machine-readable half is data)", rpc.Error.Code, mcpgo.INTERNAL_ERROR)
	}
	data, ok := rpc.Error.Data.(map[string]any)
	if !ok {
		t.Fatalf("data = %#v, want a map", rpc.Error.Data)
	}
	if got := data["status"]; got != wantStatus {
		t.Errorf("data.status = %v, want %d", got, wantStatus)
	}
	if got, _ := data["code"].(string); got != wantCode {
		t.Errorf("data.code = %q, want %q", got, wantCode)
	}
	gotRetry, hasRetry := data["retry_after_s"]
	switch {
	case wantRetry == nil && hasRetry:
		t.Errorf("data carries retry_after_s on a refusal that has no wait: %v", data)
	case wantRetry != nil && gotRetry != *wantRetry:
		t.Errorf("data.retry_after_s = %v, want %d", gotRetry, *wantRetry)
	}
}

// TestRateLimitedRefusalCarriesTheWait: a 429's wait reaches the client as
// retry_after_s, the same number REST puts in Retry-After, so a client sleeps
// for exactly as long as the limiter asked.
//
// The wait is read off *service.SubmitRateLimited, whose fields the service owns
// — so it is constructed here the one way a test outside the service package can
// construct it (through submitToolError on a coded 429 it already wraps) and the
// end-to-end proof that a real refusal carries the limiter's number runs through
// the tool in cmd/server/request_submit_mcp_test.go.
func TestRateLimitedRefusalCarriesTheWait(t *testing.T) {
	retry := 42
	err := mcpserver.NewHandlerError(&requestToolError{
		err:    authz.RequestRateLimited(retry, "too many submissions; try again shortly"),
		status: 429, code: authz.CodeRequestRateLimited, retryAfterS: retry,
	})
	assertToolErrorData(t, err, 429, authz.CodeRequestRateLimited, &retry)
}

// TestSubmitToolErrorLeavesFaultsAlone: a bug, a dead connection or a cancelled
// context has no status, so it travels to the client as itself instead of being
// dressed up as a refusal the caller should act on.
func TestSubmitToolErrorLeavesFaultsAlone(t *testing.T) {
	fault := errors.New("submit request to team 4: connection reset")
	if got := submitToolError(fault); got != fault {
		t.Fatalf("a fault was wrapped: %v (%T)", got, got)
	}
	if got := submitToolError(context.Canceled); got != context.Canceled {
		t.Fatalf("a cancellation was wrapped: %v", got)
	}
}

// TestSubmitToolErrorIsAuditedAsDenied is the interaction with denied.go that
// makes the wrapper safe: a scope refusal and an ordinary 403 are still seen as
// 403 by the middleware, so the refusal is written once as access.denied. The
// wrapper adds the client's half (status/code/message) and changes nothing the
// audit path reads.
func TestSubmitToolErrorIsAuditedAsDenied(t *testing.T) {
	for _, refusal := range []error{authz.TokenScopeForbidden(), authz.Forbidden("team 4: submitting requires team membership or submit access")} {
		err := submitToolError(refusal)
		if got := authz.StatusOf(err, 0); got != 403 {
			t.Fatalf("%v wrapped to status %d, want 403 (the denied middleware would stop auditing it)", refusal, got)
		}
	}
	// An allowed-scope refusal names only what the credential may do.
	if got := authz.CodeOf(submitToolError(authz.TokenScopeForbidden())); got != authz.CodeTokenScopeDenied {
		t.Fatalf("scope refusal code = %q, want %q", got, authz.CodeTokenScopeDenied)
	}
}

// TestSubmitResultLeaksNoInboxInternals: the answer is the caller's own request,
// not the stored row. A requester never sees the triage claim, the assignee, the
// org, or (on a replay that triagers have since touched) the team's internal
// state — that projection is slice E3's, and this tool must not pre-empt it by
// returning the record.
func TestSubmitResultLeaksNoInboxInternals(t *testing.T) {
	owner, claimer := 11, 12
	req := &domain.TeamRequest{
		ID: 7, PublicRef: "9f2q1x7k4m2p9t5w", OrgID: 3, TeamID: 4,
		RequesterUserID: &owner, RequesterContact: "requester@example.com",
		SourceKind: domain.RequestSourceMCP, Subject: "export the queue",
		State: domain.RequestStatusTriaging, ClaimUserID: &claimer,
		OwnerUserID: &claimer, ReopenedCount: 2, Rev: 5,
		Metadata: domain.Metadata{"secret": "internal"}, RejectReason: "internal note",
	}
	out := submitResult(&service.SubmitRequestResult{Request: req, Created: true})
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	for _, leaked := range []string{"requester_user_id", "requester_contact", "org_id", "claim_user_id", "owner_user_id",
		"reopened_count", "rev", "metadata", "reject_reason", "\"id\""} {
		if strings.Contains(string(b), leaked) {
			t.Errorf("result leaks %s: %s", leaked, b)
		}
	}
	// What a caller does need: the opaque reference it quotes, the state word,
	// its own subject, the team it named, the source, and created.
	res, _ := out["request"].(map[string]interface{})
	for _, want := range []string{"public_ref", "state", "subject", "team_id", "source_kind", "created_at"} {
		if _, ok := res[want]; !ok {
			t.Errorf("result lacks %q: %s", want, b)
		}
	}
	if res["public_ref"] != req.PublicRef || res["state"] != "triaging" || res["source_kind"] != "mcp" {
		t.Errorf("result = %v", res)
	}
	if out["created"] != true {
		t.Errorf("created = %v, want true", out["created"])
	}

	// A replay says so, and says what it means.
	replay := submitResult(&service.SubmitRequestResult{Request: req, Created: false})
	if replay["created"] != false {
		t.Errorf("replay created = %v, want false", replay["created"])
	}
	if next, _ := replay["next"].(string); !strings.Contains(next, "Replay") {
		t.Errorf("replay next = %q, want it to say this is a replay", next)
	}
}

// TestOptStrStrictRefusesWrongTypes is the unit half of the refusal this ticket
// exists for: a present-but-not-a-string value for one of the submission's two
// optional strings is a 400 request.invalid naming the argument, instead of the
// silent fall-back to "" that optStr performs.
//
// The last three rows are the properties that make the refusal safe to add:
// absent and null are still absence (REST's decoder reads JSON null for a string
// field as ""), a string passes through untouched (including "" and a blank
// value, which the service normalizes itself), and no extra argument refuses
// because its name changed.
func TestOptStrStrictRefusesWrongTypes(t *testing.T) {
	tests := []struct {
		name    string
		args    map[string]interface{}
		key     string
		want    string
		wantErr bool
		// wantKind, when set, must appear in the refusal: the caller is told the
		// JSON type it sent, so it can fix the argument without guessing.
		wantKind string
	}{
		{name: "absent is absent", args: map[string]interface{}{}, key: "input_brief"},
		{name: "null is absence (REST decodes it as an empty string)",
			args: map[string]interface{}{"input_brief": nil}, key: "input_brief"},
		{name: "a string passes through", args: map[string]interface{}{"input_brief": "we need the export"}, key: "input_brief", want: "we need the export"},
		{name: "a blank string passes (the service normalizes it)", args: map[string]interface{}{"idempotency_key": "   "}, key: "idempotency_key", want: "   "},
		{name: "a number is refused", args: map[string]interface{}{"idempotency_key": float64(123)}, key: "idempotency_key", wantErr: true, wantKind: "number"},
		{name: "a boolean is refused", args: map[string]interface{}{"input_brief": true}, key: "input_brief", wantErr: true, wantKind: "boolean"},
		{name: "an object is refused", args: map[string]interface{}{"input_brief": map[string]interface{}{"text": "x"}}, key: "input_brief", wantErr: true, wantKind: "object"},
		{name: "an array is refused", args: map[string]interface{}{"idempotency_key": []interface{}{"k"}}, key: "idempotency_key", wantErr: true, wantKind: "array"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := optStrStrict(tt.args, tt.key, "because it is a promise")
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("refused a value it should read: %v", err)
				}
				if got != tt.want {
					t.Fatalf("got %q, want %q", got, tt.want)
				}
				return
			}
			if err == nil {
				t.Fatalf("%v was read as %q; a wrong-typed value must be refused, never coerced", tt.args[tt.key], got)
			}
			if s, c := authz.StatusOf(err, 0), authz.CodeOf(err); s != 400 || c != authz.CodeRequestInvalid {
				t.Fatalf("refusal = %d %q, want 400 %s", s, c, authz.CodeRequestInvalid)
			}
			if !strings.Contains(err.Error(), tt.key) {
				t.Errorf("refusal %q doesn't name the argument %q", err, tt.key)
			}
			if tt.wantKind != "" && !strings.Contains(err.Error(), tt.wantKind) {
				t.Errorf("refusal %q doesn't name the type %q the caller sent", err, tt.wantKind)
			}
		})
	}
}

// TestSubmitRequestRefusesWrongTypedOptionalArguments is the handler-level half:
// through the tool's own handler, both arguments are refused before the service
// is reached (s.svc is nil here, so a value that reached the service would
// panic rather than pass silently), the refusal is the coded 400 REST answers
// for the same JSON, and it carries the status and code an MCP client reads.
func TestSubmitRequestRefusesWrongTypedOptionalArguments(t *testing.T) {
	s := &Server{}
	cases := []struct {
		name string
		args map[string]interface{}
		key  string
	}{
		{"wrong-typed idempotency_key",
			map[string]interface{}{"team_id": float64(1), "subject": "x", "idempotency_key": float64(123)}, "idempotency_key"},
		{"wrong-typed input_brief",
			map[string]interface{}{"team_id": float64(1), "subject": "x", "input_brief": true}, "input_brief"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			_, err := s.submitRequest(context.Background(), tt.args)
			if err == nil {
				t.Fatalf("%v was accepted; the argument must be refused, not coerced to empty", tt.args)
			}
			if s, c := authz.StatusOf(err, 0), authz.CodeOf(err); s != 400 || c != authz.CodeRequestInvalid {
				t.Fatalf("refusal = %d %q, want the coded 400 %s REST answers for the same JSON", s, c, authz.CodeRequestInvalid)
			}
			if !strings.Contains(err.Error(), tt.key) {
				t.Errorf("refusal %q doesn't name %q", err, tt.key)
			}
			// The wrapper is the same one every other refusal uses, so the client
			// reads REST's status/code next to the message.
			assertToolErrorData(t, err, 400, authz.CodeRequestInvalid, nil)
		})
	}
}

// TestSubmitRequestRejectsMissingArguments: the two REQUIRED arguments are
// refused by the handler with the argument's name, before the service is
// reached — so a client that dropped one gets a fixable message, not a 500 and
// not a request aimed at whichever team happened to be zero.
func TestSubmitRequestRejectsMissingArguments(t *testing.T) {
	s := &Server{}
	for _, args := range []map[string]interface{}{
		{"subject": "x"},
		{"team_id": float64(4)},
		{},
	} {
		_, err := s.submitRequest(context.Background(), args)
		if err == nil {
			t.Fatalf("%v was accepted", args)
		}
		missing := "team_id"
		if _, ok := args["team_id"]; ok {
			missing = "subject"
		}
		if !strings.Contains(err.Error(), missing) {
			t.Errorf("refusal for %v = %q, want it to name %q", args, err, missing)
		}
	}
}
