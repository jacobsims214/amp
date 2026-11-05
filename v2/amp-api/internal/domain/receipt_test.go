package domain

import (
	"strings"
	"testing"
	"time"
)

func chk(id int64, role, kind, verdict, by string, at time.Time) ReceiptCheck {
	return ReceiptCheck{ID: id, Role: role, Kind: kind, Verdict: verdict, CheckedBy: by, CreatedAt: at}
}

var t0 = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

// Rule 1: the latest human verdict is pass.
func TestDeriveStatus_HumanAccepted(t *testing.T) {
	r := Receipt{AgentID: "w"}
	cs := []ReceiptCheck{chk(1, CheckRoleWorker, CheckKindTest, VerdictFail, "w", t0),
		chk(2, CheckRoleHuman, CheckKindVerdict, VerdictPass, "ann", t0.Add(time.Minute))}
	if got := DeriveStatus(r, cs); got != StatusHumanAccepted {
		t.Fatalf("got %s", got)
	}
	// A later Reject overrides the Accept.
	cs = append(cs, chk(3, CheckRoleHuman, CheckKindVerdict, VerdictFail, "ann", t0.Add(2*time.Minute)))
	if got := DeriveStatus(r, cs); got != StatusUnverified {
		t.Fatalf("after reject: %s", got)
	}
	if Rejection(cs) == nil || Rejection(cs[:2]) != nil {
		t.Fatal("Rejection")
	}
	// Same timestamp: the higher id is the later one.
	same := []ReceiptCheck{chk(5, CheckRoleHuman, CheckKindVerdict, VerdictFail, "a", t0), chk(6, CheckRoleHuman, CheckKindVerdict, VerdictPass, "a", t0)}
	if got := DeriveStatus(r, same); got != StatusHumanAccepted {
		t.Fatalf("tie: %s", got)
	}
}

// Rule 2: a verifier's pass, by someone other than the worker.
func TestDeriveStatus_IndependentlyVerified(t *testing.T) {
	r := Receipt{AgentID: "w"}
	cs := []ReceiptCheck{chk(1, CheckRoleVerifier, CheckKindVerdict, VerdictPass, "v", t0)}
	if got := DeriveStatus(r, cs); got != StatusIndependentlyVerified {
		t.Fatalf("got %s", got)
	}
	// The worker verifying itself is not independent: falls through to rule 5.
	self := []ReceiptCheck{chk(1, CheckRoleVerifier, CheckKindVerdict, VerdictPass, "w", t0)}
	if got := DeriveStatus(r, self); got != StatusUnverified {
		t.Fatalf("self-verified: %s", got)
	}
}

// Rule 3: the latest verifier verdict failed.
func TestDeriveStatus_VerifierFailed(t *testing.T) {
	r := Receipt{AgentID: "w"}
	cs := []ReceiptCheck{chk(1, CheckRoleWorker, CheckKindTest, VerdictPass, "w", t0),
		chk(2, CheckRoleVerifier, CheckKindVerdict, VerdictFail, "v", t0)}
	if got := DeriveStatus(r, cs); got != StatusUnverified {
		t.Fatalf("got %s", got)
	}
}

// Rule 4: a worker pass and no worker fail; cant_tell doesn't count against.
func TestDeriveStatus_SelfChecked(t *testing.T) {
	r := Receipt{AgentID: "w"}
	cs := []ReceiptCheck{chk(1, CheckRoleWorker, CheckKindTest, VerdictPass, "w", t0),
		chk(2, CheckRoleWorker, CheckKindVisual, VerdictCantTell, "w", t0),
		chk(3, CheckRoleWorker, CheckKindCI, VerdictPass, "w", t0)}
	if got := DeriveStatus(r, cs); got != StatusSelfChecked {
		t.Fatalf("got %s", got)
	}
	cs = append(cs, chk(4, CheckRoleWorker, CheckKindCommand, VerdictFail, "w", t0))
	if got := DeriveStatus(r, cs); got != StatusUnverified {
		t.Fatalf("with a fail: %s", got)
	}
}

// Rule 5: anything else, no checks included.
func TestDeriveStatus_Unverified(t *testing.T) {
	r := Receipt{AgentID: "w"}
	if got := DeriveStatus(r, nil); got != StatusUnverified {
		t.Fatalf("no checks: %s", got)
	}
	only := []ReceiptCheck{chk(1, CheckRoleWorker, CheckKindVisual, VerdictCantTell, "w", t0)}
	if got := DeriveStatus(r, only); got != StatusUnverified {
		t.Fatalf("only cant_tell: %s", got)
	}
}

func TestMeetsMinimum(t *testing.T) {
	cases := []struct {
		status, min string
		want        bool
	}{
		{StatusUnverified, StatusSelfChecked, false},
		{StatusSelfChecked, StatusSelfChecked, true},
		{StatusSelfChecked, StatusIndependentlyVerified, false},
		{StatusIndependentlyVerified, StatusIndependentlyVerified, true},
		{StatusHumanAccepted, StatusIndependentlyVerified, true},
		{StatusHumanAccepted, StatusSelfChecked, true},
		{StatusSelfChecked, "", true}, // unknown minimum reads as self_checked
	}
	for _, c := range cases {
		if got := MeetsMinimum(c.status, c.min); got != c.want {
			t.Errorf("MeetsMinimum(%s, %s) = %v", c.status, c.min, got)
		}
	}
}

func TestParseChecks(t *testing.T) {
	got, err := ParseChecks([]any{
		map[string]any{"criterion_ref": float64(2), "kind": "TEST", "verdict": "pass", "command": "go test ./...", "exit_code": float64(0)},
		map[string]any{"criterion_ref": "1", "ok": false},
		map[string]any{"criterion_ref": "3", "kind": "visual", "verdict": "cant_tell", "note": "  no screenshot  "},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].CriterionRef != "2" || got[0].Kind != CheckKindTest || got[0].ExitCode == nil || *got[0].ExitCode != 0 {
		t.Fatalf("first: %+v", got[0])
	}
	if got[1].Verdict != VerdictFail || got[1].Kind != CheckKindManual || got[1].Role != CheckRoleWorker {
		t.Fatalf("legacy ok: %+v", got[1])
	}
	if got[2].Note != "no screenshot" {
		t.Fatalf("note: %q", got[2].Note)
	}
	if ok, _ := ParseChecks([]any{map[string]any{"ok": true}}); ok[0].Verdict != VerdictPass {
		t.Fatal("ok: true is pass")
	}

	bad := map[string][]any{
		"verdict is required": {map[string]any{"kind": "test"}},
		"isn't pass":          {map[string]any{"verdict": "maybe"}},
		"isn't command":       {map[string]any{"kind": "verdict", "verdict": "pass"}},
		"must be an object":   {"pass"},
		"longer than":         {map[string]any{"verdict": "pass", "note": strings.Repeat("x", MaxCheckNoteBytes+1)}},
		"must be an integer":  {map[string]any{"verdict": "pass", "exit_code": 1.5}},
	}
	for want, in := range bad {
		if _, err := ParseChecks(in); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%v: err %v, want %q", in, err, want)
		}
	}
	many := make([]any, MaxReceiptChecks+1)
	for i := range many {
		many[i] = map[string]any{"verdict": "pass"}
	}
	if _, err := ParseChecks(many); err == nil {
		t.Fatal("more than the cap")
	}
}

func TestReceiptsOverride(t *testing.T) {
	m, l := Metadata{"receipts": "Require", "receipts_min_level": "self_checked"}.ReceiptsOverride()
	if m != ReceiptsRequire || l != StatusSelfChecked {
		t.Fatalf("%s %s", m, l)
	}
	if m, l := (Metadata{"receipts": "sometimes", "receipts_min_level": true}).ReceiptsOverride(); m != "" || l != "" {
		t.Fatalf("invalid values: %q %q", m, l)
	}
}

// A visual pass counts only with its own receipt's stored evidence, on the
// receipt's revision.
func TestDowngradeVisualChecks(t *testing.T) {
	zero, five := 0, 5
	ev := []ReceiptEvidence{
		{Key: "k/shot", Status: EvidenceStored, TreeSHA: "abcdef0123456789"},
		{Key: "k/video", Status: EvidenceStored, TreeSHA: "abcdef0123456789", Frames: &five},
		{Key: "k/empty", Status: EvidenceStored, TreeSHA: "abcdef0123456789", Frames: &zero},
		{Key: "k/pending", Status: EvidencePending, TreeSHA: "abcdef0123456789"},
		{Key: "k/notree", Status: EvidenceStored},
	}
	vis := func(key, verdict string) ReceiptCheck {
		return ReceiptCheck{Role: CheckRoleWorker, Kind: CheckKindVisual, Verdict: verdict, EvidenceKey: key, Note: "looked"}
	}
	cases := []struct {
		name, tree, storeErr string
		in                   ReceiptCheck
		want, note           string
	}{
		{"stored screenshot", "abcdef0123456789", "", vis("k/shot", VerdictPass), VerdictPass, "looked"},
		{"abbreviated tree", "abcdef012345", "", vis("k/video", VerdictPass), VerdictPass, "looked"},
		{"no key", "", "", vis("", VerdictPass), VerdictCantTell, "evidence missing: looked"},
		{"foreign key", "", "", vis("other/receipt/shot", VerdictPass), VerdictCantTell, "evidence missing: looked"},
		{"pending", "", "", vis("k/pending", VerdictPass), VerdictCantTell, "evidence missing (pending): looked"},
		{"zero frames", "", "", vis("k/empty", VerdictPass), VerdictCantTell, "no frames: looked"},
		{"tree mismatch", "0000000000aa", "", vis("k/shot", VerdictPass), VerdictCantTell, "evidence recorded on another revision: looked"},
		{"receipt has no tree", "", "", vis("k/shot", VerdictPass), VerdictPass, "looked"},
		{"evidence has no tree", "0000000000aa", "", vis("k/notree", VerdictPass), VerdictPass, "looked"},
		{"no store", "abcdef0123456789", "disk gone", vis("k/shot", VerdictPass), VerdictCantTell, "evidence storage isn't configured: looked"},
		{"a fail stays a fail", "", "", vis("", VerdictFail), VerdictFail, "looked"},
		{"not visual", "", "", ReceiptCheck{Kind: CheckKindTest, Verdict: VerdictPass}, VerdictPass, ""},
	}
	for _, c := range cases {
		in := []ReceiptCheck{c.in}
		got := DowngradeVisualChecks(in, ev, c.tree, c.storeErr)
		if got[0].Verdict != c.want || got[0].Note != c.note {
			t.Errorf("%s: %s %q, want %s %q", c.name, got[0].Verdict, got[0].Note, c.want, c.note)
		}
		if in[0].Verdict != c.in.Verdict {
			t.Errorf("%s: modified its input", c.name)
		}
	}
}
