package auditchain

import (
	"bytes"
	"fmt"
	"sort"
	"time"
)

// Problem kinds a verification can report. Any problem fails the chain.
const (
	// ProblemAltered: the row's stored hash isn't the hash of its contents.
	ProblemAltered = "altered"
	// ProblemBrokenLink: the row's prev_hash isn't the previous row's hash.
	ProblemBrokenLink = "broken_link"
	// ProblemMissing: seqs are missing between two rows (rows deleted).
	ProblemMissing = "missing"
	// ProblemMissingStart: the chain starts after seq 1 and no anchor
	// checkpoint anchors where it starts (the oldest rows were deleted, not
	// purged).
	ProblemMissingStart = "missing_start"
	// ProblemMissingTail: the head or a checkpoint is past the last row
	// (the newest rows were deleted).
	ProblemMissingTail = "missing_tail"
	// ProblemHead: the chain head disagrees with the last row.
	ProblemHead = "head_mismatch"
	// ProblemCheckpoint: a checkpoint's hash isn't the row's hash at its
	// seq (history was rewritten and re-hashed after it was signed).
	ProblemCheckpoint = "checkpoint_mismatch"
	// ProblemSignature: a checkpoint's signature is bad, by an unknown key,
	// or missing while a signing key is configured (#2439 H1).
	ProblemSignature = "signature"
	// ProblemAnchor: a retention anchor that retention can't have written:
	// its row was younger than the retention period, or it is unsigned and
	// signed history before it is gone (#2439 H1).
	ProblemAnchor = "anchor"
	// ProblemActor: the row's actor_user_id isn't the actor it was written
	// with (actor_id_at_write, which the hash covers) (#2439 L1).
	ProblemActor = "actor_mismatch"
	// ProblemUnchained: a row has no chain columns (written while the
	// chain trigger was disabled).
	ProblemUnchained = "unchained"
	// ProblemMisfiled: a row's chain isn't the one its org belongs to.
	ProblemMisfiled = "misfiled"
)

// Chain and report statuses.
const (
	// StatusOK: no problems, and signed checkpoints vouch for the chain.
	StatusOK = "ok"
	// StatusUnproven: no problems found, but nothing signed vouches for the
	// chain (no signing key, no signed checkpoint yet, or it starts at an
	// unsigned anchor), so someone with database access could have
	// rewritten it consistently. Not OK (#2439 H2).
	StatusUnproven = "unproven"
	// StatusFailed: tampering or loss detected.
	StatusFailed = "failed"
)

// Policy is what verification checks against besides the keys.
type Policy struct {
	// Retention is the configured retention period (AUDIT_RETENTION_DAYS):
	// an anchor whose row was younger than this when it was written wasn't
	// written by retention. 0 skips the check.
	Retention time.Duration
}

// anchorSlack allows for clock skew between the database (a row's at) and
// the replica that wrote the anchor.
const anchorSlack = time.Hour

// Problem is one thing wrong with a chain.
type Problem struct {
	Kind         string `json:"kind"`
	Seq          int64  `json:"seq,omitempty"`
	AuditID      int64  `json:"audit_id,omitempty"`
	CheckpointID int64  `json:"checkpoint_id,omitempty"`
	Detail       string `json:"detail"`
}

// ChainReport is the result of verifying one chain.
type ChainReport struct {
	ChainKey string `json:"chain_key"`
	// OK is Status == StatusOK.
	OK     bool   `json:"ok"`
	Status string `json:"status"`
	// Rows is how many rows were checked, FirstSeq..LastSeq.
	Rows     int   `json:"rows"`
	FirstSeq int64 `json:"first_seq,omitempty"`
	LastSeq  int64 `json:"last_seq,omitempty"`
	HeadSeq  int64 `json:"head_seq"`
	// AnchorSeq is the latest retention anchor (0: nothing purged).
	AnchorSeq int64 `json:"anchor_seq,omitempty"`
	// CheckpointsMatched is how many checkpoints were compared with a row.
	CheckpointsMatched int `json:"checkpoints_matched"`
	SignedCheckpoints  int `json:"signed_checkpoints"`
	// LastSignedSeq is the newest seq a valid signature covers: rows after
	// it are protected by the hash chain alone until the next checkpoint.
	LastSignedSeq int64     `json:"last_signed_seq,omitempty"`
	Problems      []Problem `json:"problems"`
	Warnings      []string  `json:"warnings,omitempty"`
}

// Report is a verification of one or more chains.
type Report struct {
	// OK is Status == StatusOK: every chain verified and is vouched for by
	// a signature.
	OK bool `json:"ok"`
	// Status is the worst chain status (StatusOK with no chains).
	Status    string        `json:"status"`
	CheckedAt time.Time     `json:"checked_at"`
	Signing   SigningStatus `json:"signing"`
	Chains    []ChainReport `json:"chains"`
}

// SigningStatus says which key this replica signs with.
type SigningStatus struct {
	Configured bool   `json:"configured"`
	KeyID      string `json:"key_id,omitempty"`
}

// NewReport assembles chain reports.
func NewReport(keys *Keyring, chains []ChainReport) Report {
	r := Report{Status: StatusOK, CheckedAt: time.Now().UTC(), Chains: chains,
		Signing: SigningStatus{Configured: keys.CanSign(), KeyID: keys.SignerID()}}
	if r.Chains == nil {
		r.Chains = []ChainReport{}
	}
	for _, c := range chains {
		switch {
		case c.Status == StatusFailed:
			r.Status = StatusFailed
		case c.Status == StatusUnproven && r.Status == StatusOK:
			r.Status = StatusUnproven
		}
	}
	r.OK = r.Status == StatusOK
	return r
}

// Head is a chain's audit_chain_heads row.
type Head struct {
	Seq  int64
	Hash []byte
}

// ChainVerifier checks one chain's rows, fed in seq order with Add.
type ChainVerifier struct {
	rep     ChainReport
	head    *Head
	keys    *Keyring
	cps     []Checkpoint
	trusted map[int64][]Checkpoint // by upto_seq: signature valid, or unsigned where that is allowed
	all     map[int64][]Checkpoint // by upto_seq
	started bool
	last    int64
	lastH   []byte
	// startedAt is the anchor the chain starts from (nil: seq 1, or none).
	startedAt *Checkpoint
	// unproven says why nothing signed vouches for the chain ("" if it is).
	unproven string
}

// NewChainVerifier starts verifying chain key, whose head is head (nil if
// it has none), with its checkpoints, checked against keys and policy.
//
// Trust rules (#2439 H1): when keys holds any key, a checkpoint must carry
// a valid signature, whatever its id or kind; an unsigned one is a problem
// unless it was written before keys.Since() (history from before signing
// started). Without any key nothing can be proven: unsigned checkpoints are
// used, and the chain is at best unproven. Only an anchor may start a
// chain, and an anchor must be older than the retention period.
func NewChainVerifier(key string, head *Head, checkpoints []Checkpoint, keys *Keyring, policy Policy) *ChainVerifier {
	v := &ChainVerifier{rep: ChainReport{ChainKey: key, Problems: []Problem{}}, head: head, keys: keys,
		trusted: map[int64][]Checkpoint{}, all: map[int64][]Checkpoint{}}
	if head != nil {
		v.rep.HeadSeq = head.Seq
	}
	cps := append([]Checkpoint(nil), checkpoints...)
	sort.Slice(cps, func(i, j int) bool { return cps[i].ID < cps[j].ID }) // the order they were written
	v.cps = cps
	unsigned, preKey := 0, 0
	since := keys.Since()
	for _, c := range cps {
		v.all[c.UptoSeq] = append(v.all[c.UptoSeq], c)
		trusted := false
		switch keys.Check(c) {
		case SigOK:
			v.rep.SignedCheckpoints++
			trusted = true
			v.rep.LastSignedSeq = max(v.rep.LastSignedSeq, c.UptoSeq)
		case SigUnsigned:
			switch {
			case !keys.HasKeys():
				unsigned++
				trusted = true
			case !since.IsZero() && c.At.Before(since):
				preKey++
				trusted = true
			default:
				v.problem(Problem{Kind: ProblemSignature, Seq: c.UptoSeq, CheckpointID: c.ID,
					Detail: fmt.Sprintf("unsigned %s checkpoint while a signing key is configured: someone without the key wrote it (set %s if it is history from before signing started)", c.Kind, EnvSigningSince)})
			}
		case SigBad:
			v.problem(Problem{Kind: ProblemSignature, Seq: c.UptoSeq, CheckpointID: c.ID,
				Detail: fmt.Sprintf("bad signature (key %s)", c.KeyID)})
		case SigUnknownKey:
			v.problem(Problem{Kind: ProblemSignature, Seq: c.UptoSeq, CheckpointID: c.ID,
				Detail: fmt.Sprintf("signed by key %s, which isn't configured: add its public key to %s if it is a retired key", c.KeyID, EnvPublicKeys)})
		}
		if !trusted {
			continue
		}
		v.trusted[c.UptoSeq] = append(v.trusted[c.UptoSeq], c)
		if c.Kind == KindAnchor {
			v.rep.AnchorSeq = max(v.rep.AnchorSeq, c.UptoSeq)
			if policy.Retention > 0 && c.RowAt != nil && c.RowAt.After(c.At.Add(-policy.Retention+anchorSlack)) {
				v.problem(Problem{Kind: ProblemAnchor, Seq: c.UptoSeq, CheckpointID: c.ID,
					Detail: fmt.Sprintf("the anchored row was written %s, less than the %d-day retention before the anchor (%s): retention didn't delete it (if retention was lengthened since, the anchor may be older policy)",
						c.RowAt.UTC().Format(time.RFC3339), int(policy.Retention/(24*time.Hour)), c.At.UTC().Format(time.RFC3339))})
			}
		}
	}
	switch {
	case !keys.HasKeys():
		v.unproven = fmt.Sprintf("no signing key is configured (%s): the chain can't be proven against someone with database access", EnvSigningKey)
		if unsigned > 0 {
			v.rep.Warnings = append(v.rep.Warnings, fmt.Sprintf("%d checkpoint(s) are unsigned: they show where the chain was, but prove nothing", unsigned))
		}
	case preKey > 0:
		v.rep.Warnings = append(v.rep.Warnings, fmt.Sprintf("%d unsigned checkpoint(s) from before %s (%s) were accepted as pre-key history", preKey, since.UTC().Format(time.RFC3339), EnvSigningSince))
	}
	return v
}

func (v *ChainVerifier) problem(p Problem) { v.rep.Problems = append(v.rep.Problems, p) }

// Add checks the next row. Rows must come in chain_seq order.
func (v *ChainVerifier) Add(r Row) {
	v.rep.Rows++
	if r.ChainKey == nil || r.ChainSeq == nil || len(r.Hash) == 0 {
		v.problem(Problem{Kind: ProblemUnchained, AuditID: r.ID, Detail: "row has no chain position or hash"})
		return
	}
	seq := *r.ChainSeq
	if want := ChainKey(r.OrgID); *r.ChainKey != want {
		v.problem(Problem{Kind: ProblemMisfiled, Seq: seq, AuditID: r.ID,
			Detail: fmt.Sprintf("row is in chain %s but its org puts it in %s", *r.ChainKey, want)})
	}
	var prev []byte
	linkChecked := true
	switch {
	case !v.started:
		v.rep.FirstSeq = seq
		if seq == 1 {
			prev = ZeroHash
		} else if a := v.startAnchor(seq - 1); a != nil {
			prev = a.Hash
			v.startedAt = a
		} else {
			v.problem(Problem{Kind: ProblemMissingStart, Seq: seq, AuditID: r.ID,
				Detail: fmt.Sprintf("the chain starts at seq %d, and no anchor checkpoint anchors seq %d: rows were deleted outside retention", seq, seq-1)})
			linkChecked = false
		}
	case seq != v.last+1:
		v.problem(Problem{Kind: ProblemMissing, Seq: v.last + 1, AuditID: r.ID,
			Detail: fmt.Sprintf("seq %d to %d are missing (deleted rows)", v.last+1, seq-1)})
		linkChecked = false
	default:
		prev = v.lastH
	}
	if linkChecked && !bytes.Equal(r.PrevHash, prev) {
		v.problem(Problem{Kind: ProblemBrokenLink, Seq: seq, AuditID: r.ID,
			Detail: "prev_hash isn't the previous row's hash (rows reordered, replaced or re-hashed)"})
	}
	if r.ActorIDAtWrite != nil && r.ActorUserID != nil && *r.ActorIDAtWrite != *r.ActorUserID {
		v.problem(Problem{Kind: ProblemActor, Seq: seq, AuditID: r.ID,
			Detail: fmt.Sprintf("actor_user_id is %d but the row was written by user %d: it was re-pointed", *r.ActorUserID, *r.ActorIDAtWrite)})
	}
	if !bytes.Equal(LinkHash(r.PrevHash, Canonical(r)), r.Hash) {
		v.problem(Problem{Kind: ProblemAltered, Seq: seq, AuditID: r.ID,
			Detail: "the row's contents don't match its hash: it was changed after it was written"})
	}
	for _, c := range v.all[seq] {
		v.rep.CheckpointsMatched++
		if !bytes.Equal(c.Hash, r.Hash) {
			v.problem(Problem{Kind: ProblemCheckpoint, Seq: seq, AuditID: r.ID, CheckpointID: c.ID,
				Detail: fmt.Sprintf("the %s checkpoint of %s doesn't match this row: history was rewritten after it", c.Kind, c.At.UTC().Format(time.RFC3339))})
		}
	}
	v.started, v.last, v.lastH = true, seq, r.Hash
	v.rep.LastSeq = seq
}

// Finish checks the end of the chain and returns the report.
func (v *ChainVerifier) Finish() ChainReport {
	end := v.last
	switch {
	case !v.started:
		// Nothing left: retention may have purged everything, up to an
		// anchor at the head.
		if v.head != nil && v.head.Seq > 0 {
			if a := v.startAnchor(v.head.Seq); a != nil && bytes.Equal(a.Hash, v.head.Hash) {
				v.startedAt = a
			} else {
				v.problem(Problem{Kind: ProblemMissingTail, Seq: v.head.Seq,
					Detail: fmt.Sprintf("the chain head is at seq %d but no rows remain, and no anchor covers them", v.head.Seq)})
			}
		}
		end = v.rep.HeadSeq
	case v.head == nil:
		v.problem(Problem{Kind: ProblemHead, Detail: "the chain has rows but no head"})
	case v.head.Seq > v.last:
		v.problem(Problem{Kind: ProblemMissingTail, Seq: v.last + 1,
			Detail: fmt.Sprintf("the chain head is at seq %d but the last row is %d: the newest rows were deleted", v.head.Seq, v.last)})
	case v.head.Seq < v.last || !bytes.Equal(v.head.Hash, v.lastH):
		v.problem(Problem{Kind: ProblemHead, Seq: v.head.Seq,
			Detail: fmt.Sprintf("the chain head (seq %d) doesn't match the last row (seq %d)", v.head.Seq, v.last)})
	}
	for _, c := range v.cps {
		if c.UptoSeq > end {
			v.problem(Problem{Kind: ProblemMissingTail, Seq: c.UptoSeq, CheckpointID: c.ID,
				Detail: fmt.Sprintf("a %s checkpoint covers seq %d but the chain ends at %d: the newest rows were deleted", c.Kind, c.UptoSeq, end)})
		}
	}
	if a := v.startedAt; a != nil && len(a.Signature) == 0 {
		// Only pre-key history (or a keyless setup) may start at an
		// unsigned anchor, and pre-key history can't have signed
		// checkpoints in the part it purged: a signed checkpoint at or
		// below the start was written after signing began, so the rows it
		// vouched for were deleted by someone without the key.
		for _, c := range v.cps {
			if c.UptoSeq <= a.UptoSeq && len(c.Signature) > 0 && v.keys.Check(c) == SigOK {
				v.problem(Problem{Kind: ProblemAnchor, Seq: a.UptoSeq, CheckpointID: a.ID,
					Detail: fmt.Sprintf("the chain starts at an unsigned anchor (seq %d), but signed checkpoint %d at seq %d is before it: signed history was deleted without the key", a.UptoSeq, c.ID, c.UptoSeq)})
				break
			}
		}
		if v.unproven == "" {
			v.unproven = fmt.Sprintf("the chain starts at an unsigned anchor (seq %d): what was deleted before it isn't proven", a.UptoSeq)
		}
	}
	if v.unproven == "" && v.rep.SignedCheckpoints == 0 && (v.started || v.rep.HeadSeq > 0) {
		v.unproven = "no signed checkpoint covers this chain yet (they are written hourly)"
	}
	if v.started && v.rep.LastSignedSeq < v.last && v.rep.SignedCheckpoints > 0 {
		v.rep.Warnings = append(v.rep.Warnings, fmt.Sprintf("seq %d to %d are newer than the last signed checkpoint", v.rep.LastSignedSeq+1, v.last))
	}
	switch {
	case len(v.rep.Problems) > 0:
		v.rep.Status = StatusFailed
	case v.unproven != "":
		v.rep.Status = StatusUnproven
		v.rep.Warnings = append(v.rep.Warnings, "unproven: "+v.unproven)
	default:
		v.rep.Status = StatusOK
	}
	v.rep.OK = v.rep.Status == StatusOK
	return v.rep
}

// startAnchor is the trusted anchor the chain may start after seq (only
// an anchor: a periodic checkpoint marks where the chain was, not where
// retention cut it), or nil.
func (v *ChainVerifier) startAnchor(seq int64) *Checkpoint {
	for i := range v.trusted[seq] {
		if c := v.trusted[seq][i]; c.Kind == KindAnchor {
			return &c
		}
	}
	return nil
}
