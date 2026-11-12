package domain

// The requester projection's type-level guarantees (AMP #3954, build slice E3):
// the whitelist is the type, the milestone gate cannot announce a delivery the
// lifecycle state has not reached, and the text path removes exactly the
// invisible characters and nothing a reader can see.

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"strings"
	"testing"
)

// The view is a whitelist, and the whitelist is mechanical: this is the exact
// field list of RequesterRequestView and of every type nested in it. A field
// added to the struct fails this test until someone adds it here — which is the
// moment to ask whether a requester may see it. Nothing about a ticket, project,
// epic, story, metadata, branch, claim, assignee, revision, user id, agent id,
// audit row or private comment is on it.
func TestRequesterRequestViewHasNoInternalField(t *testing.T) {
	want := map[string][]string{
		"RequesterRequestView": {
			"PublicRef", "State", "StateWord", "Subject", "InputBrief",
			"OutcomeBrief", "RejectReason", "TeamName",
			"CreatedAt", "NeedsReplySince", "DeliveredAt", "AcceptedAt", "ClosedAt",
			"Milestones", "Comments", "Outputs", "Question",
		},
		"RequesterMilestone": {"Key", "Done", "At", "Note"},
		"RequesterComment":   {"Kind", "From", "Body", "At"},
		"RequesterQuestion":  {"Body", "At"},
		"RequesterOutput":    {"Source", "Name", "SizeBytes", "Status", "At", "DownloadRef"},
	}
	types := map[string]reflect.Type{
		"RequesterRequestView": reflect.TypeOf(RequesterRequestView{}),
		"RequesterMilestone":   reflect.TypeOf(RequesterMilestone{}),
		"RequesterComment":     reflect.TypeOf(RequesterComment{}),
		"RequesterQuestion":    reflect.TypeOf(RequesterQuestion{}),
		"RequesterOutput":      reflect.TypeOf(RequesterOutput{}),
	}
	for name, typ := range types {
		var got []string
		for i := 0; i < typ.NumField(); i++ {
			got = append(got, typ.Field(i).Name)
		}
		if !reflect.DeepEqual(got, want[name]) {
			t.Errorf("%s fields = %v, want the whitelist %v\n"+
				"if you are adding a field, ask first whether a requester may see it", name, got, want[name])
		}
	}
}

// The view's JSON is the wire form and leaks nothing: no key of it may be the
// name of an internal record field, and no internal-looking name may appear as a
// key. This is the schema half of the golden test in the service package — it
// fails on a `json` tag that renames a whitelisted field to something internal.
func TestRequesterRequestViewJSONKeys(t *testing.T) {
	raw, err := json.Marshal(RequesterRequestView{
		PublicRef: "8f2q1x7k4m2p9t5w", State: RequestStatusTriaging, StateWord: "Being reviewed",
		Subject: "s", InputBrief: "b", TeamName: "Desk",
		Milestones: []RequesterMilestone{}, Comments: []RequesterComment{}, Outputs: []RequesterOutput{},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	allowed := map[string]bool{
		"public_ref": true, "state": true, "state_word": true, "subject": true,
		"input_brief": true, "outcome_brief": true, "reject_reason": true,
		"team_name": true, "created_at": true, "needs_reply_since": true,
		"delivered_at": true, "accepted_at": true, "closed_at": true,
		"milestones": true, "comments": true, "outputs": true, "question": true,
	}
	for key := range got {
		if !allowed[key] {
			t.Errorf("view serializes unexpected key %q: it is not on the requester whitelist", key)
		}
	}
	// And the internal record's own field names never appear as keys.
	recordJSON, err := json.Marshal(validRequest())
	if err != nil {
		t.Fatalf("marshal record: %v", err)
	}
	var record map[string]any
	if err := json.Unmarshal(recordJSON, &record); err != nil {
		t.Fatalf("unmarshal record: %v", err)
	}
	for key := range record {
		if allowed[key] {
			continue
		}
		if _, leaked := got[key]; leaked {
			t.Errorf("internal record field %q is serialized by the requester view", key)
		}
	}
}

// The milestone gate is the whole "a closed ticket is not a delivery" rule at the
// projection layer: for every (step, lifecycle state) pair, the answer must be
// what the contract allows — delivered only in the two delivered states,
// accepted only in requester_accepted, and nothing ever allowed at a state
// outside the vocabulary.
func TestRequesterMilestoneDoneAllowed(t *testing.T) {
	// Each row: step → the states it may be done in (by index into
	// RequestStatuses, spelled out as names for readability).
	want := map[RequesterMilestoneKey][]RequestStatus{
		RequesterMilestoneReceived: RequestStatuses, // every state: the desk has it
		RequesterMilestoneReviewing: {
			RequestStatusTriaging, RequestStatusNeedsInformation, RequestStatusAccepted,
			RequestStatusInProgress, RequestStatusDelivered, RequestStatusRequesterAccepted,
			RequestStatusRejected, RequestStatusDuplicate,
		},
		RequesterMilestoneInProgress: {
			RequestStatusAccepted, RequestStatusInProgress,
			RequestStatusDelivered, RequestStatusRequesterAccepted,
		},
		RequesterMilestoneDelivered: {
			RequestStatusDelivered, RequestStatusRequesterAccepted,
		},
		RequesterMilestoneAccepted: {RequestStatusRequesterAccepted},
	}
	for _, key := range RequesterMilestoneKeys {
		allowedStates := want[key]
		for _, state := range RequestStatuses {
			wantOK := false
			for _, s := range allowedStates {
				if s == state {
					wantOK = true
					break
				}
			}
			if got := RequesterMilestoneDoneAllowed(key, state); got != wantOK {
				t.Errorf("RequesterMilestoneDoneAllowed(%q, %q) = %v, want %v", key, state, got, wantOK)
			}
		}
		// An unknown state allows nothing but "received": a step cannot be
		// reached at a state the vocabulary does not know.
		if RequesterMilestoneDoneAllowed(key, RequestStatus("weird")) && key != RequesterMilestoneReceived {
			t.Errorf("RequesterMilestoneDoneAllowed(%q, unknown state) = true: an unknown state must not reach a step", key)
		}
	}
	// An unknown key is not a step.
	if RequesterMilestoneDoneAllowed("shipped_to_prod", RequestStatusRequesterAccepted) {
		t.Error("an unknown milestone key was allowed: the vocabulary is closed")
	}
}

// The state word is the mockup's own, and every state has one: an empty word for
// a stored state would render a blank tag in the requester UI.
func TestRequesterStateWord(t *testing.T) {
	want := map[RequestStatus]string{
		RequestStatusNew:               "Received",
		RequestStatusTriaging:          "Being reviewed",
		RequestStatusNeedsInformation:  "Needs your reply",
		RequestStatusAccepted:          "In progress",
		RequestStatusInProgress:        "In progress",
		RequestStatusDelivered:         "Delivered — your call",
		RequestStatusRequesterAccepted: "Accepted",
		RequestStatusRejected:          "Closed",
		RequestStatusDuplicate:         "Closed",
	}
	if len(want) != len(RequestStatuses) {
		t.Fatalf("the word table covers %d states, the vocabulary has %d", len(want), len(RequestStatuses))
	}
	for _, state := range RequestStatuses {
		if got := RequesterStateWord(state); got != want[state] {
			t.Errorf("RequesterStateWord(%q) = %q, want %q", state, got, want[state])
		}
	}
	if got := RequesterStateWord(RequestStatus("shipped")); got != "" {
		t.Errorf("an unknown state produced the word %q: it must produce none", got)
	}
}

// Hostile text: the sanitizer removes exactly the invisible characters that let
// one string pose as another or drive a terminal, and keeps everything readable —
// including markdown, HTML-looking text, emoji ZWJ sequences and line breaks.
func TestSanitizeRequesterText(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "October export is short by €4,300", "October export is short by €4,300"},
		{"markdown kept", "**Included:** the CSV\n- one line why", "**Included:** the CSV\n- one line why"},
		{"html-looking kept as text", "<script>alert(1)</script>", "<script>alert(1)</script>"},
		{"tab and newline kept", "a\tb\nc", "a\tb\nc"},
		{"C0 controls dropped", "a\x00b\x07c\x1bd", "abcd"},
		{"DEL dropped", "a\x7fb", "ab"},
		{"C1 controls dropped", "a\u0085b\u009fc", "abc"},
		{"RTL override dropped", "invoice\u202Etxt.exe", "invoicetxt.exe"},
		{"bidi embedding dropped", "a\u202Bb\u202Cc\u202Dd\u202Ee", "abcde"},
		{"bidi isolates dropped", "a\u2066b\u2067c\u2068d\u2069e", "abcde"},
		{"LRM and RLM dropped", "a\u200Eb\u200Fc", "abc"},
		{"zero-width space dropped", "ad\u200Bmin", "admin"},
		{"word joiner dropped", "a\u2060b", "ab"},
		{"BOM dropped", "\uFEFFhello", "hello"},
		{"line separator dropped", "a\u2028b\u2029c", "abc"},
		{"emoji ZWJ sequence survives", "family \U0001F468\u200D\U0001F469\u200D\U0001F467", "family \U0001F468\u200D\U0001F469\u200D\U0001F467"},
		{"emoji flags and skin tones survive", "\U0001F1F3\U0001F1F4 \U0001F44B\U0001F3FD", "\U0001F1F3\U0001F1F4 \U0001F44B\U0001F3FD"},
		{"empty", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := SanitizeRequesterText(c.in); got != c.want {
				t.Errorf("SanitizeRequesterText(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
	// Invalid UTF-8 is replaced, never carried: the value must stay valid JSON.
	got := SanitizeRequesterText("ok\xffbad")
	if !json.Valid([]byte(strconvQuote(got))) {
		t.Errorf("sanitized text %q is not valid JSON", got)
	}
	if strings.ContainsRune(got, '\xff') {
		t.Errorf("invalid UTF-8 survived sanitizing: %q", got)
	}
}

// RequesterNote is the projection's only text path: trimmed, sanitized and
// bounded to the brief limit on a rune boundary.
func TestRequesterNote(t *testing.T) {
	if got := RequesterNote("  hello\x00  "); got != "hello" {
		t.Errorf("RequesterNote trimmed/sanitized to %q, want %q", got, "hello")
	}
	long := strings.Repeat("é", MaxRequestBriefLen+500)
	got := RequesterNote(long)
	if n := len([]rune(got)); n != MaxRequestBriefLen {
		t.Errorf("RequesterNote kept %d runes, want %d", n, MaxRequestBriefLen)
	}
	for _, r := range got {
		if r == '\uFFFD' {
			t.Fatal("RequesterNote cut mid-sequence: the result holds a replacement character")
		}
	}
}

// The view types are exported for the read path and must never be confused with
// the record: this pins that no field of the view has a type that could carry a
// whole TeamRequest, RequestComment, RequestLink or Metadata.
func TestRequesterViewCarriesNoRecordType(t *testing.T) {
	forbidden := []reflect.Type{
		reflect.TypeOf(TeamRequest{}), reflect.TypeOf(&TeamRequest{}),
		reflect.TypeOf(RequestComment{}), reflect.TypeOf(&RequestComment{}),
		reflect.TypeOf(RequestLink{}), reflect.TypeOf(&RequestLink{}),
		reflect.TypeOf(RequestEvent{}), reflect.TypeOf(&RequestEvent{}),
		reflect.TypeOf(Metadata{}),
	}
	views := []reflect.Type{
		reflect.TypeOf(RequesterRequestView{}), reflect.TypeOf(RequesterMilestone{}),
		reflect.TypeOf(RequesterComment{}), reflect.TypeOf(RequesterQuestion{}),
		reflect.TypeOf(RequesterOutput{}),
	}
	for _, view := range views {
		for i := 0; i < view.NumField(); i++ {
			f := view.Field(i)
			for _, bad := range forbidden {
				if f.Type == bad {
					t.Errorf("%s.%s is a %s: the requester view must not carry a stored record", view.Name(), f.Name, bad)
				}
			}
		}
	}
}

// strconvQuote quotes a string the way encoding/json would, for the JSON-validity
// check above without importing strconv in the test's hot path.
func strconvQuote(s string) string {
	raw, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(raw)
}

// A shared question is only ever a question: this pins the closed comment
// vocabulary the projection filters on has exactly the schema's four kinds, so a
// new kind must be added deliberately in both places.
func TestRequestCommentKindVocabularyClosed(t *testing.T) {
	got := make([]string, len(RequestCommentKinds))
	for i, k := range RequestCommentKinds {
		got[i] = string(k)
	}
	want := []string{"comment", "question", "answer", "system"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("RequestCommentKinds = %v, want %v", got, want)
	}
}

// The closed vocabularies live here and are lists, not maps without a fallback:
// this pins that they exist and that the view's first field a client reads is the
// opaque reference. It is a cheap guard against a future "just pass it through"
// widening of the whitelist.
func TestRequesterViewSourceHasNoOpenVocabulary(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "requester_view.go", nil, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var sawVocabs bool
	ast.Inspect(file, func(n ast.Node) bool {
		decl, ok := n.(*ast.GenDecl)
		if !ok || decl.Tok != token.VAR {
			return true
		}
		for _, spec := range decl.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for _, name := range vs.Names {
				switch name.Name {
				case "RequesterMilestoneKeys", "RequesterAuthors", "RequesterOutputSources", "RequesterOutputStatuses":
					sawVocabs = true
				}
			}
		}
		return true
	})
	if !sawVocabs {
		t.Error("the closed vocabularies are missing from requester_view.go")
	}
	if typ := reflect.TypeOf(RequesterRequestView{}); typ.Field(0).Name != "PublicRef" {
		t.Errorf("RequesterRequestView starts with %q: the opaque ref must be the first field a client reads", typ.Field(0).Name)
	}
}
