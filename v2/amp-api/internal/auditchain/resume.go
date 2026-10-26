package auditchain

import (
	"bytes"
	"fmt"
)

// Incremental verification (#2441 L2): a chain only grows at its head, so
// once its rows up to some seq are verified, the next verification can
// start from there and read only the new rows. State is where a
// verification ended; Resume continues from it.
//
// Resuming trusts that the rows already verified haven't changed since,
// which only a full re-read can show; callers re-verify in full now and
// then (service.AuditVerifyFullEvery) and whenever Resume says no.

// State is where a finished verification of a chain ended.
type State struct {
	report      ChainReport
	last        int64
	lastH       []byte
	startedAtID int64 // the start anchor's id, 0 if the chain starts at seq 1
	checkpoints map[int64]bool
}

// LastSeq is the last row the state covers.
func (s *State) LastSeq() int64 { return s.last }

// State returns where this verification ended, or nil if it can't be
// continued (it found problems, or saw no rows).
func (v *ChainVerifier) State() *State {
	if !v.started || v.rep.Status == "" || v.rep.Status == StatusFailed {
		return nil
	}
	s := &State{report: v.rep, last: v.last, lastH: append([]byte(nil), v.lastH...), checkpoints: map[int64]bool{}}
	if v.startedAt != nil {
		s.startedAtID = v.startedAt.ID
	}
	for _, c := range v.cps {
		s.checkpoints[c.ID] = true
	}
	return s
}

// Resume continues from s: the verifier (made with the chain's current
// head and checkpoints) then takes only the rows after s.LastSeq() through
// Add. It returns false, leaving the verifier untouched, when the chain
// changed in a way only a full verification can judge: a checkpoint seen
// before is gone, or a new checkpoint lies inside the verified part (a new
// retention anchor, say). A new checkpoint exactly at the end of the
// verified part is compared with that row here.
func (v *ChainVerifier) Resume(s *State) bool {
	if s == nil || v.started {
		return false
	}
	present := map[int64]bool{}
	var atLast []Checkpoint
	for _, c := range v.cps {
		present[c.ID] = true
		if s.checkpoints[c.ID] {
			continue
		}
		switch {
		case c.UptoSeq < s.last || (c.UptoSeq == s.last && c.Kind == KindAnchor):
			return false
		case c.UptoSeq == s.last:
			atLast = append(atLast, c)
		}
	}
	for id := range s.checkpoints {
		if !present[id] {
			return false
		}
	}
	if s.startedAtID != 0 {
		found := false
		for i := range v.cps {
			if v.cps[i].ID == s.startedAtID {
				c := v.cps[i]
				v.startedAt, found = &c, true
			}
		}
		if !found {
			return false
		}
	}
	v.started, v.last, v.lastH = true, s.last, append([]byte(nil), s.lastH...)
	v.rep.Incremental = true
	v.rep.Rows, v.rep.FirstSeq, v.rep.LastSeq = s.report.Rows, s.report.FirstSeq, s.report.LastSeq
	v.rep.CheckpointsMatched = s.report.CheckpointsMatched
	for _, c := range atLast {
		v.rep.CheckpointsMatched++
		if !bytes.Equal(c.Hash, s.lastH) {
			v.problem(Problem{Kind: ProblemCheckpoint, Seq: c.UptoSeq, CheckpointID: c.ID,
				Detail: fmt.Sprintf("the %s checkpoint at seq %d doesn't match that row: history was rewritten after it", c.Kind, c.UptoSeq)})
		}
	}
	return true
}
