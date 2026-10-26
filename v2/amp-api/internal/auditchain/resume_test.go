package auditchain

import (
	"testing"
)

// resumeFrom verifies rows[:n] in full, then the rest incrementally from
// there, with cps0 and cps1 the checkpoints each time saw.
func resumeFrom(t *testing.T, rows []Row, n int, cps0, cps1 []Checkpoint, keys *Keyring) (ChainReport, bool) {
	t.Helper()
	v := NewChainVerifier("org:5", headOf(rows[:n]), cps0, keys, Policy{})
	for _, r := range rows[:n] {
		v.Add(r)
	}
	if rep := v.Finish(); rep.Status == StatusFailed {
		t.Fatalf("first pass: %s", kinds(rep))
	}
	st := v.State()
	if st == nil || st.LastSeq() != int64(n) {
		t.Fatalf("state = %+v", st)
	}
	v2 := NewChainVerifier("org:5", headOf(rows), cps1, keys, Policy{})
	if !v2.Resume(st) {
		return ChainReport{}, false
	}
	for _, r := range rows[n:] {
		v2.Add(r)
	}
	return v2.Finish(), true
}

// #2441 L2: a verification continued from where the last one ended reads
// only the new rows and reaches the same verdict as a full one.
func TestResume_ReadsOnlyNewRows(t *testing.T) {
	keys := testKeys(t)
	rows := chain("org:5", ip(5), 10)
	cp1 := signed(keys, 1, KindPeriodic, rows[5])
	cp2 := signed(keys, 2, KindPeriodic, rows[9])
	rep, ok := resumeFrom(t, rows, 6, []Checkpoint{cp1}, []Checkpoint{cp1, cp2}, keys)
	if !ok {
		t.Fatal("couldn't resume")
	}
	full := verify("org:5", rows, headOf(rows), []Checkpoint{cp1, cp2}, keys)
	if !rep.OK || !rep.Incremental || rep.RowsRead != 4 || rep.Rows != 10 || rep.FirstSeq != 1 || rep.LastSeq != 10 ||
		rep.CheckpointsMatched != full.CheckpointsMatched || rep.Status != full.Status {
		t.Fatalf("incremental = %+v\nfull = %+v", rep, full)
	}
	if full.RowsRead != 10 || full.Incremental {
		t.Errorf("full read = %d rows, incremental %v", full.RowsRead, full.Incremental)
	}
}

// New rows are checked as strictly as in a full pass: a broken link at
// the seam, an altered new row, and a checkpoint at the seam that doesn't
// match all fail.
func TestResume_ChecksTheSeam(t *testing.T) {
	keys := testKeys(t)
	rows := chain("org:5", ip(5), 8)
	cp1 := signed(keys, 1, KindPeriodic, rows[3])

	bad := append([]Row(nil), rows...)
	bad[4].PrevHash = rows[0].Hash // seq 5 doesn't follow seq 4
	if rep, ok := resumeFrom(t, bad, 4, []Checkpoint{cp1}, []Checkpoint{cp1}, keys); !ok || rep.OK || kinds(rep) == "" {
		t.Errorf("broken seam: ok=%v %s", ok, kinds(rep))
	}
	alt := append([]Row(nil), rows...)
	alt[6].Summary = "edited"
	if rep, ok := resumeFrom(t, alt, 4, []Checkpoint{cp1}, []Checkpoint{cp1}, keys); !ok || kinds(rep) != ProblemAltered {
		t.Errorf("altered new row: %s", kinds(rep))
	}
	// A checkpoint written at exactly where the last pass ended is compared
	// with that row's hash.
	wrong := Checkpoint{ID: 2, ChainKey: "org:5", UptoSeq: 4, Hash: rows[1].Hash, Kind: KindPeriodic}
	keys.Sign(&wrong)
	if rep, ok := resumeFrom(t, rows, 4, []Checkpoint{cp1}, []Checkpoint{cp1, wrong}, keys); !ok || kinds(rep) != ProblemCheckpoint {
		t.Errorf("checkpoint at the seam: ok=%v %s", ok, kinds(rep))
	}
	// Deleting the newest rows still shows (the head is past them).
	v := NewChainVerifier("org:5", headOf(rows[:4]), []Checkpoint{cp1}, keys, Policy{})
	for _, r := range rows[:4] {
		v.Add(r)
	}
	v.Finish()
	v2 := NewChainVerifier("org:5", headOf(rows), []Checkpoint{cp1}, keys, Policy{})
	if !v2.Resume(v.State()) {
		t.Fatal("couldn't resume")
	}
	for _, r := range rows[4:6] { // seq 7 and 8 were deleted
		v2.Add(r)
	}
	if rep := v2.Finish(); kinds(rep) != ProblemMissingTail {
		t.Errorf("deleted tail: %s", kinds(rep))
	}
}

// Changes inside the verified part need a full pass: Resume says no.
func TestResume_RefusesWhenHistoryChanged(t *testing.T) {
	keys := testKeys(t)
	rows := chain("org:5", ip(5), 8)
	cp1 := signed(keys, 1, KindPeriodic, rows[3])
	inside := signed(keys, 2, KindPeriodic, rows[1])
	anchor := signed(keys, 3, KindAnchor, rows[3])
	for name, cps1 := range map[string][]Checkpoint{
		"new checkpoint inside the verified part": {cp1, inside},
		"new anchor at the end of it":             {cp1, anchor},
		"a checkpoint disappeared":                {},
	} {
		if _, ok := resumeFrom(t, rows, 4, []Checkpoint{cp1}, cps1, keys); ok {
			t.Errorf("%s: resumed", name)
		}
	}
	// A failed pass leaves nothing to resume from.
	alt := append([]Row(nil), rows...)
	alt[1].Summary = "edited"
	v := NewChainVerifier("org:5", headOf(alt), nil, keys, Policy{})
	for _, r := range alt {
		v.Add(r)
	}
	v.Finish()
	if v.State() != nil {
		t.Error("state of a failed chain")
	}
}
