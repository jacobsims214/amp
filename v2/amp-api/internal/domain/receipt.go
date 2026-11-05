package domain

import (
	"fmt"
	"strings"
	"time"
)

// Verification receipts (#126, v2/docs/specs/verification-receipts.md): a
// receipt per completing run, the checks that run (and later a verifier
// and a person) recorded, and a verification status derived from them.

// Receipts modes: the org default, the project's setting, and a ticket's
// metadata override.
const (
	ReceiptsOff     = "off"
	ReceiptsInform  = "inform"
	ReceiptsRequire = "require"
)

// Verification statuses, lowest first (the ladder, V5).
const (
	StatusUnverified            = "unverified"
	StatusSelfChecked           = "self_checked"
	StatusIndependentlyVerified = "independently_verified"
	StatusHumanAccepted         = "human_accepted"
)

// Receipt states.
const (
	ReceiptDraft = "draft"
	ReceiptFinal = "final"
)

// Check roles, kinds and verdicts.
const (
	CheckRoleWorker   = "worker"
	CheckRoleVerifier = "verifier"
	CheckRoleHuman    = "human"

	CheckKindCommand = "command"
	CheckKindTest    = "test"
	CheckKindCI      = "ci"
	CheckKindVisual  = "visual"
	CheckKindManual  = "manual"
	CheckKindVerdict = "verdict"

	VerdictPass     = "pass"
	VerdictFail     = "fail"
	VerdictCantTell = "cant_tell"
)

// Receipts metadata keys (project → epic → story → ticket, later wins). An
// agent may only make them stricter (D11).
const (
	MetaReceipts         = "receipts"
	MetaReceiptsMinLevel = "receipts_min_level"
)

// ParseChecks limits.
const (
	MaxReceiptChecks   = 100
	MaxCheckNoteBytes  = 2048
	MaxCheckFieldBytes = 2048
	MaxReceiptSummary  = 4000
	MaxFilesChanged    = 500
)

// Receipt is one completing run's record. The status is never stored: it is
// DeriveStatus over the receipt's checks (D21).
type Receipt struct {
	ID           int64        `json:"id"`
	TaskID       int          `json:"task_id"`
	ProjectID    int          `json:"project_id"`
	DispatchID   string       `json:"dispatch_id,omitempty"`
	RunID        *int64       `json:"run_id,omitempty"`
	State        string       `json:"state"`
	Mode         string       `json:"mode"`
	Held         bool         `json:"held"`
	AgentID      string       `json:"agent_id"`
	Summary      string       `json:"summary"`
	FilesChanged []FileChange `json:"files_changed"`
	CommitSHA    string       `json:"commit_sha"`
	TreeSHA      string       `json:"tree_sha"`
	Branch       string       `json:"branch"`
	BaseBranch   string       `json:"base_branch"`
	PRURL        string       `json:"pr_url"`
	Kept         bool         `json:"kept"`
	CreatedAt    time.Time    `json:"created_at"`
	FinalizedAt  *time.Time   `json:"finalized_at,omitempty"`
}

// FileChange is one file a run changed.
type FileChange struct {
	Path    string `json:"path"`
	Added   int    `json:"added"`
	Removed int    `json:"removed"`
}

// ReceiptCheck is one check on a receipt: a worker's command, test, CI run,
// visual or manual check, or a verifier's or person's verdict.
type ReceiptCheck struct {
	ID           int64     `json:"id"`
	ReceiptID    int64     `json:"receipt_id"`
	Role         string    `json:"role"`
	Kind         string    `json:"kind"`
	Verdict      string    `json:"verdict"`
	CriterionRef string    `json:"criterion_ref"`
	Command      string    `json:"command,omitempty"`
	ExitCode     *int      `json:"exit_code,omitempty"`
	DurationMS   *int      `json:"duration_ms,omitempty"`
	Note         string    `json:"note,omitempty"`
	EvidenceKey  string    `json:"evidence_key,omitempty"`
	CheckedBy    string    `json:"checked_by"`
	UserID       *int      `json:"user_id,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
}

// ReceiptEvidence is the metadata of one evidence file. The bytes live in
// the receipt store (Phase 3), never in Postgres (D3).
type ReceiptEvidence struct {
	ID          int64      `json:"id"`
	ReceiptID   int64      `json:"receipt_id"`
	Key         string     `json:"key"`
	Name        string     `json:"name"`
	TreeSHA     string     `json:"tree_sha"`
	ContentType string     `json:"content_type"`
	Bytes       int64      `json:"bytes"`
	SHA256      string     `json:"sha256"`
	DurationMS  *int       `json:"duration_ms,omitempty"`
	Frames      *int       `json:"frames,omitempty"`
	Status      string     `json:"status"`
	CreatedAt   time.Time  `json:"created_at"`
	StoredAt    *time.Time `json:"stored_at,omitempty"`
}

// ValidReceiptsMode reports whether m is a mode (not "": that is a
// project's "use the org's").
func ValidReceiptsMode(m string) bool {
	return m == ReceiptsOff || m == ReceiptsInform || m == ReceiptsRequire
}

// ValidMinLevel reports whether l can be a Require minimum.
func ValidMinLevel(l string) bool {
	return l == StatusSelfChecked || l == StatusIndependentlyVerified
}

// ModeRank orders the modes: off < inform < require (-1 for anything else).
func ModeRank(m string) int {
	switch m {
	case ReceiptsOff:
		return 0
	case ReceiptsInform:
		return 1
	case ReceiptsRequire:
		return 2
	}
	return -1
}

// StatusRank orders the ladder: unverified < self_checked <
// independently_verified < human_accepted (-1 for anything else).
func StatusRank(s string) int {
	switch s {
	case StatusUnverified:
		return 0
	case StatusSelfChecked:
		return 1
	case StatusIndependentlyVerified:
		return 2
	case StatusHumanAccepted:
		return 3
	}
	return -1
}

// MeetsMinimum reports whether status reaches min on the ladder. A person's
// Accept (human_accepted) is above every minimum (D10). An unknown minimum
// is read as self_checked, the v1 default.
func MeetsMinimum(status, min string) bool {
	if !ValidMinLevel(min) {
		min = StatusSelfChecked
	}
	return StatusRank(status) >= StatusRank(min)
}

// latestCheck is the newest check of role and kind (by created_at, then
// id), or nil.
func latestCheck(checks []ReceiptCheck, role, kind string) *ReceiptCheck {
	var out *ReceiptCheck
	for i := range checks {
		c := &checks[i]
		if c.Role != role || c.Kind != kind {
			continue
		}
		if out == nil || c.CreatedAt.After(out.CreatedAt) || (c.CreatedAt.Equal(out.CreatedAt) && c.ID > out.ID) {
			out = c
		}
	}
	return out
}

// DeriveStatus is a final receipt's verification status: the first rule
// that matches.
//
//  1. human_accepted: the latest human verdict is pass (a later fail, a
//     Reject, overrides an earlier Accept).
//  2. independently_verified: the latest verifier verdict is pass and the
//     verifier isn't the receipt's own agent.
//  3. unverified: the latest verifier verdict is fail.
//  4. self_checked: the worker checks include a pass and no fail
//     (cant_tell doesn't count against it).
//  5. unverified: anything else, no checks included.
func DeriveStatus(r Receipt, checks []ReceiptCheck) string {
	human := latestCheck(checks, CheckRoleHuman, CheckKindVerdict)
	if human != nil && human.Verdict == VerdictPass {
		return StatusHumanAccepted
	}
	if v := latestCheck(checks, CheckRoleVerifier, CheckKindVerdict); v != nil {
		if v.Verdict == VerdictPass && v.CheckedBy != r.AgentID {
			return StatusIndependentlyVerified
		}
		if v.Verdict == VerdictFail {
			return StatusUnverified
		}
	}
	pass, fail := false, false
	for _, c := range checks {
		if c.Role != CheckRoleWorker {
			continue
		}
		switch c.Verdict {
		case VerdictPass:
			pass = true
		case VerdictFail:
			fail = true
		}
	}
	if pass && !fail {
		return StatusSelfChecked
	}
	return StatusUnverified
}

// Rejection is the latest human verdict when it is a Reject (fail), or nil.
func Rejection(checks []ReceiptCheck) *ReceiptCheck {
	if h := latestCheck(checks, CheckRoleHuman, CheckKindVerdict); h != nil && h.Verdict == VerdictFail {
		return h
	}
	return nil
}

// ParseChecks reads a worker's checks[] (an amp_complete_task argument):
// each {criterion_ref, kind, verdict, command?, exit_code?, duration_ms?,
// note?, evidence_key?}. A legacy {ok: bool} stands for pass / fail. At
// most MaxReceiptChecks, notes at most MaxCheckNoteBytes. Roles are always
// worker: no tool writes another role's checks.
func ParseChecks(raw []any) ([]ReceiptCheck, error) {
	if len(raw) > MaxReceiptChecks {
		return nil, fmt.Errorf("checks: at most %d, got %d", MaxReceiptChecks, len(raw))
	}
	out := make([]ReceiptCheck, 0, len(raw))
	for i, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("checks[%d]: must be an object", i)
		}
		str := func(key string) (string, error) {
			v, ok := m[key]
			if !ok || v == nil {
				return "", nil
			}
			switch x := v.(type) {
			case string:
				x = strings.TrimSpace(x)
				if len(x) > MaxCheckFieldBytes {
					return "", fmt.Errorf("checks[%d].%s: longer than %d bytes", i, key, MaxCheckFieldBytes)
				}
				return x, nil
			case float64:
				return fmt.Sprintf("%g", x), nil
			}
			return "", fmt.Errorf("checks[%d].%s: must be a string", i, key)
		}
		intp := func(key string) (*int, error) {
			v, ok := m[key]
			if !ok || v == nil {
				return nil, nil
			}
			f, ok := v.(float64)
			if !ok || f != float64(int(f)) {
				return nil, fmt.Errorf("checks[%d].%s: must be an integer", i, key)
			}
			n := int(f)
			return &n, nil
		}
		c := ReceiptCheck{Role: CheckRoleWorker}
		var err error
		if c.CriterionRef, err = str("criterion_ref"); err != nil {
			return nil, err
		}
		if c.Kind, err = str("kind"); err != nil {
			return nil, err
		}
		if c.Verdict, err = str("verdict"); err != nil {
			return nil, err
		}
		if c.Command, err = str("command"); err != nil {
			return nil, err
		}
		if c.EvidenceKey, err = str("evidence_key"); err != nil {
			return nil, err
		}
		if note, ok := m["note"].(string); ok {
			note = strings.TrimSpace(note)
			if len(note) > MaxCheckNoteBytes {
				return nil, fmt.Errorf("checks[%d].note: longer than %d bytes", i, MaxCheckNoteBytes)
			}
			c.Note = note
		} else if v, ok := m["note"]; ok && v != nil {
			return nil, fmt.Errorf("checks[%d].note: must be a string", i)
		}
		if c.ExitCode, err = intp("exit_code"); err != nil {
			return nil, err
		}
		if c.DurationMS, err = intp("duration_ms"); err != nil {
			return nil, err
		}
		if c.Verdict == "" {
			if ok, isBool := m["ok"].(bool); isBool {
				c.Verdict = VerdictFail
				if ok {
					c.Verdict = VerdictPass
				}
			}
		}
		c.Kind = strings.ToLower(c.Kind)
		c.Verdict = strings.ToLower(c.Verdict)
		if c.Kind == "" {
			c.Kind = CheckKindManual
		}
		switch c.Kind {
		case CheckKindCommand, CheckKindTest, CheckKindCI, CheckKindVisual, CheckKindManual:
		default:
			return nil, fmt.Errorf("checks[%d].kind: %q isn't command, test, ci, visual or manual", i, c.Kind)
		}
		switch c.Verdict {
		case VerdictPass, VerdictFail, VerdictCantTell:
		case "":
			return nil, fmt.Errorf("checks[%d]: verdict is required (pass, fail or cant_tell; or ok: true|false)", i)
		default:
			return nil, fmt.Errorf("checks[%d].verdict: %q isn't pass, fail or cant_tell", i, c.Verdict)
		}
		out = append(out, c)
	}
	return out, nil
}

// ParseFilesChanged reads files_changed[]: each {path, added?, removed?}.
func ParseFilesChanged(raw []any) ([]FileChange, error) {
	if len(raw) > MaxFilesChanged {
		return nil, fmt.Errorf("files_changed: at most %d entries", MaxFilesChanged)
	}
	out := make([]FileChange, 0, len(raw))
	for i, item := range raw {
		var fc FileChange
		switch x := item.(type) {
		case string:
			fc.Path = strings.TrimSpace(x)
		case map[string]any:
			p, _ := x["path"].(string)
			fc.Path = strings.TrimSpace(p)
			if n, ok := x["added"].(float64); ok && n >= 0 {
				fc.Added = int(n)
			}
			if n, ok := x["removed"].(float64); ok && n >= 0 {
				fc.Removed = int(n)
			}
		default:
			return nil, fmt.Errorf("files_changed[%d]: must be {path, added, removed}", i)
		}
		if fc.Path == "" || len(fc.Path) > MaxCheckFieldBytes {
			return nil, fmt.Errorf("files_changed[%d]: path is required (at most %d bytes)", i, MaxCheckFieldBytes)
		}
		out = append(out, fc)
	}
	return out, nil
}

// ReceiptsOverride is the receipts mode and minimum level effective
// metadata m sets ("" each when it sets none, or an invalid value).
func (m Metadata) ReceiptsOverride() (mode, minLevel string) {
	if v, ok := m[MetaReceipts].(string); ok {
		if v = strings.ToLower(strings.TrimSpace(v)); ValidReceiptsMode(v) {
			mode = v
		}
	}
	if v, ok := m[MetaReceiptsMinLevel].(string); ok {
		if v = strings.ToLower(strings.TrimSpace(v)); ValidMinLevel(v) {
			minLevel = v
		}
	}
	return mode, minLevel
}

// TouchesReceipts reports whether a metadata change sets or removes a
// receipts key.
func (p MetadataPatch) TouchesReceipts() bool {
	_, a := p[MetaReceipts]
	_, b := p[MetaReceiptsMinLevel]
	return a || b
}
