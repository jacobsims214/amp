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
	// ProblemMissingStart: the chain starts after seq 1 and no checkpoint
	// anchors where it starts (the oldest rows were deleted, not purged).
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
	// or missing on a checkpoint written after signed ones.
	ProblemSignature = "signature"
	// ProblemUnchained: a row has no chain columns (written while the
	// chain trigger was disabled).
	ProblemUnchained = "unchained"
	// ProblemMisfiled: a row's chain isn't the one its org belongs to.
	ProblemMisfiled = "misfiled"
)

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
	OK       bool   `json:"ok"`
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
	OK        bool          `json:"ok"`
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
	r := Report{OK: true, CheckedAt: time.Now().UTC(), Chains: chains,
		Signing: SigningStatus{Configured: keys.CanSign(), KeyID: keys.SignerID()}}
	if r.Chains == nil {
		r.Chains = []ChainReport{}
	}
	for _, c := range chains {
		r.OK = r.OK && c.OK
	}
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
	cps     []Checkpoint
	trusted map[int64][]Checkpoint // by upto_seq, signature valid or (legitimately) unsigned
	all     map[int64][]Checkpoint // by upto_seq
	started bool
	last    int64
	lastH   []byte
}

// NewChainVerifier starts verifying chain key, whose head is head (nil if
// it has none), with its checkpoints, checked against keys.
func NewChainVerifier(key string, head *Head, checkpoints []Checkpoint, keys *Keyring) *ChainVerifier {
	v := &ChainVerifier{rep: ChainReport{ChainKey: key, Problems: []Problem{}}, head: head,
		trusted: map[int64][]Checkpoint{}, all: map[int64][]Checkpoint{}}
	if head != nil {
		v.rep.HeadSeq = head.Seq
	}
	cps := append([]Checkpoint(nil), checkpoints...)
	sort.Slice(cps, func(i, j int) bool { return cps[i].ID < cps[j].ID }) // the order they were written
	v.cps = cps
	signedSeen, unsigned := false, 0
	for _, c := range cps {
		v.all[c.UptoSeq] = append(v.all[c.UptoSeq], c)
		trusted := false
		switch keys.Check(c) {
		case SigOK:
			v.rep.SignedCheckpoints++
			signedSeen, trusted = true, true
			v.rep.LastSignedSeq = max(v.rep.LastSignedSeq, c.UptoSeq)
		case SigUnsigned:
			if signedSeen {
				v.problem(Problem{Kind: ProblemSignature, Seq: c.UptoSeq, CheckpointID: c.ID,
					Detail: "unsigned checkpoint written after signed ones: the signing key was removed, or someone else wrote it"})
			} else {
				unsigned++
				trusted = true
			}
		case SigBad:
			v.problem(Problem{Kind: ProblemSignature, Seq: c.UptoSeq, CheckpointID: c.ID,
				Detail: fmt.Sprintf("bad signature (key %s)", c.KeyID)})
		case SigUnknownKey:
			v.problem(Problem{Kind: ProblemSignature, Seq: c.UptoSeq, CheckpointID: c.ID,
				Detail: fmt.Sprintf("signed by key %s, which isn't configured: add its public key to %s if it is a retired key", c.KeyID, EnvPublicKeys)})
		}
		if trusted {
			v.trusted[c.UptoSeq] = append(v.trusted[c.UptoSeq], c)
			if c.Kind == KindAnchor {
				v.rep.AnchorSeq = max(v.rep.AnchorSeq, c.UptoSeq)
			}
		}
	}
	if unsigned > 0 {
		v.rep.Warnings = append(v.rep.Warnings, fmt.Sprintf(
			"%d checkpoint(s) are unsigned (no %s was configured when they were written): they show where the chain was, but don't prove it against someone with database access", unsigned, EnvSigningKey))
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
		} else if cps := v.trusted[seq-1]; len(cps) > 0 {
			prev = cps[0].Hash
		} else {
			v.problem(Problem{Kind: ProblemMissingStart, Seq: seq, AuditID: r.ID,
				Detail: fmt.Sprintf("the chain starts at seq %d, and no checkpoint anchors seq %d: rows were deleted outside retention", seq, seq-1)})
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
			ok := false
			for _, c := range v.trusted[v.head.Seq] {
				if c.Kind == KindAnchor && bytes.Equal(c.Hash, v.head.Hash) {
					ok = true
				}
			}
			if !ok {
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
	if v.started && v.rep.LastSignedSeq < v.last && v.rep.SignedCheckpoints > 0 {
		v.rep.Warnings = append(v.rep.Warnings, fmt.Sprintf("seq %d to %d are newer than the last signed checkpoint", v.rep.LastSignedSeq+1, v.last))
	}
	v.rep.OK = len(v.rep.Problems) == 0
	return v.rep
}
