package auditchain

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

func sp(s string) *string { return &s }
func ip(n int64) *int64   { return &n }

// chain builds n rows of one chain, linked and hashed as the trigger does.
func chain(key string, org *int64, n int) []Row {
	var rows []Row
	prev := ZeroHash
	for i := 1; i <= n; i++ {
		r := Row{ID: int64(100 + i), At: time.Date(2026, 10, 26, 12, 0, i, 123456000, time.UTC), ActorEmail: "a@x",
			OrgID: org, Action: "test.row", Details: `{"i": 1}`, SchemaVersion: 2, Outcome: "success",
			ChainKey: sp(key), ChainSeq: ip(int64(i)), PrevHash: prev}
		r.Hash = LinkHash(prev, Canonical(r))
		prev = r.Hash
		rows = append(rows, r)
	}
	return rows
}

func verify(key string, rows []Row, head *Head, cps []Checkpoint, keys *Keyring) ChainReport {
	return verifyWith(key, rows, head, cps, keys, Policy{})
}

func verifyWith(key string, rows []Row, head *Head, cps []Checkpoint, keys *Keyring, policy Policy) ChainReport {
	v := NewChainVerifier(key, head, cps, keys, policy)
	for _, r := range rows {
		v.Add(r)
	}
	return v.Finish()
}

func headOf(rows []Row) *Head {
	last := rows[len(rows)-1]
	return &Head{Seq: *last.ChainSeq, Hash: last.Hash}
}

func kinds(r ChainReport) string {
	var k []string
	for _, p := range r.Problems {
		k = append(k, p.Kind)
	}
	return strings.Join(k, ",")
}

// Canonical escapes like Postgres's escape_json and orders keys like jsonb
// (length, then bytes). The Postgres side of this is checked against a real
// database in cmd/server/audit_integrity_test.go.
func TestCanonical_EscapingAndKeyOrder(t *testing.T) {
	r := Row{ID: 7, At: time.Date(2026, 1, 2, 3, 4, 5, 6000, time.FixedZone("x", 3600)), ActorEmail: "q\"b\\s\n\t\x01é/",
		Details: `{}`, ChainKey: sp("global"), ChainSeq: ip(1)}
	got := Canonical(r)
	if !strings.HasPrefix(got, `{"at": "2026-01-02T02:04:05.000006Z", "id": 7, "after": null, "action": "", "before": null, "client": ""`) {
		t.Fatalf("key order / time format: %s", got)
	}
	if !strings.Contains(got, `"actor_email": "q\"b\\s\n\t\u0001é/"`) {
		t.Fatalf("escaping: %s", got)
	}
	if !strings.Contains(got, `"org_id": null`) || !strings.Contains(got, `"details": {}`) || !strings.Contains(got, `"via_token": false`) {
		t.Fatalf("nulls and raw jsonb: %s", got)
	}
}

// testKeys is a keyring with a fresh signing key.
func testKeys(t *testing.T) *Keyring {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return NewKeyring(priv)
}

// signed is a checkpoint signed by keys.
func signed(keys *Keyring, id int64, kind string, row Row) Checkpoint {
	c := Checkpoint{ID: id, ChainKey: *row.ChainKey, UptoSeq: *row.ChainSeq, Hash: row.Hash, At: time.Now(), Kind: kind}
	keys.Sign(&c)
	return c
}

// An intact chain is OK only when a valid signature vouches for it; with
// no key, or no signed checkpoint yet, it is unproven, which isn't OK
// (#2439 H2).
func TestVerify_IntactChain(t *testing.T) {
	rows := chain("org:5", ip(5), 4)
	r := verify("org:5", rows, headOf(rows), nil, nil)
	if r.OK || r.Status != StatusUnproven || len(r.Problems) != 0 || r.Rows != 4 || r.LastSeq != 4 {
		t.Fatalf("intact chain, no key: %+v", r)
	}
	keys := testKeys(t)
	if r := verify("org:5", rows, headOf(rows), nil, keys); r.OK || r.Status != StatusUnproven {
		t.Fatalf("intact chain, key but no checkpoint yet: %+v", r)
	}
	cp := signed(keys, 1, KindPeriodic, rows[3])
	if r := verify("org:5", rows, headOf(rows), []Checkpoint{cp}, keys); !r.OK || r.Status != StatusOK {
		t.Fatalf("intact chain, signed: %+v", r)
	}
	// Unsigned checkpoints without a key show where the chain was, but
	// prove nothing.
	unsigned := Checkpoint{ID: 1, ChainKey: "org:5", UptoSeq: 4, Hash: rows[3].Hash, At: time.Now(), Kind: KindPeriodic}
	if r := verify("org:5", rows, headOf(rows), []Checkpoint{unsigned}, nil); r.OK || r.Status != StatusUnproven {
		t.Fatalf("unsigned checkpoints, no key: %+v", r)
	}
	rep := NewReport(nil, []ChainReport{verify("org:5", rows, headOf(rows), []Checkpoint{cp}, keys), r})
	if rep.OK || rep.Status != StatusUnproven {
		t.Fatalf("report of an ok and an unproven chain: %+v", rep)
	}
}

func TestVerify_DetectsTampering(t *testing.T) {
	cases := []struct {
		name   string
		mutate func([]Row) ([]Row, *Head)
		want   string
	}{
		{"altered row", func(rs []Row) ([]Row, *Head) {
			rs[1].Summary = "rewritten"
			return rs, headOf(rs)
		}, ProblemAltered},
		{"deleted row", func(rs []Row) ([]Row, *Head) {
			h := headOf(rs)
			return append(rs[:1:1], rs[2:]...), h
		}, ProblemMissing},
		{"deleted first row", func(rs []Row) ([]Row, *Head) {
			return rs[1:], headOf(rs)
		}, ProblemMissingStart},
		{"deleted last row", func(rs []Row) ([]Row, *Head) {
			h := headOf(rs)
			return rs[:len(rs)-1], h
		}, ProblemMissingTail},
		{"reordered seq", func(rs []Row) ([]Row, *Head) {
			// Swap the positions of rows 2 and 3, keeping their hashes.
			rs[1].ChainSeq, rs[2].ChainSeq = ip(3), ip(2)
			rs[1], rs[2] = rs[2], rs[1]
			return rs, headOf(rs)
		}, ProblemAltered},
		{"moved to another org", func(rs []Row) ([]Row, *Head) {
			rs[0].OrgID = ip(6)
			return rs, headOf(rs)
		}, ProblemMisfiled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rows, head := tc.mutate(chain("org:5", ip(5), 4))
			r := verify("org:5", rows, head, nil, nil)
			if r.OK || r.Status != StatusFailed || !strings.Contains(kinds(r), tc.want) {
				t.Fatalf("want %s, got ok=%v problems=%s", tc.want, r.OK, kinds(r))
			}
		})
	}
}

// Rewriting a row and re-hashing everything after it passes the chain
// check, but not the signed checkpoint taken before.
func TestVerify_RehashedHistoryFailsTheCheckpoint(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(nil)
	keys := NewKeyring(priv)
	rows := chain("global", nil, 3)
	cp := Checkpoint{ID: 1, ChainKey: "global", UptoSeq: 3, Hash: rows[2].Hash, At: time.Now(), Kind: KindPeriodic}
	keys.Sign(&cp)
	if r := verify("global", rows, headOf(rows), []Checkpoint{cp}, keys); !r.OK || r.SignedCheckpoints != 1 || r.CheckpointsMatched != 1 {
		t.Fatalf("before rewrite: %+v", r)
	}
	rows[0].Summary = "rewritten"
	prev := ZeroHash
	for i := range rows {
		rows[i].PrevHash = prev
		rows[i].Hash = LinkHash(prev, Canonical(rows[i]))
		prev = rows[i].Hash
	}
	r := verify("global", rows, headOf(rows), []Checkpoint{cp}, keys)
	if r.OK || kinds(r) != ProblemCheckpoint {
		t.Fatalf("re-hashed history: ok=%v %s", r.OK, kinds(r))
	}
	// A forged checkpoint over the new head doesn't verify either.
	forged := cp
	forged.ID, forged.Hash = 2, rows[2].Hash
	r = verify("global", rows, headOf(rows), []Checkpoint{cp, forged}, keys)
	if r.OK || !strings.Contains(kinds(r), ProblemSignature) {
		t.Fatalf("forged checkpoint: %s", kinds(r))
	}
}

// After retention, verification starts at the anchor.
func TestVerify_StartsAtAnchor(t *testing.T) {
	keys := testKeys(t)
	rows := chain("global", nil, 5)
	anchor := signed(keys, 1, KindAnchor, rows[1])
	r := verify("global", rows[2:], headOf(rows), []Checkpoint{anchor}, keys)
	if !r.OK || r.FirstSeq != 3 || r.AnchorSeq != 2 || len(r.Warnings) == 0 {
		t.Fatalf("after purge: %+v", r)
	}
	// Everything purged: the anchor at the head is enough.
	all := signed(keys, 2, KindAnchor, rows[4])
	if r := verify("global", nil, headOf(rows), []Checkpoint{anchor, all}, keys); !r.OK {
		t.Fatalf("all purged: %+v", r)
	}
	if r := verify("global", nil, headOf(rows), []Checkpoint{anchor}, keys); r.Status != StatusFailed {
		t.Fatal("rows gone without an anchor passed")
	}
	// Without a key the same purge has no problems, but is unproven.
	unsignedAnchor := Checkpoint{ID: 1, ChainKey: "global", UptoSeq: 2, Hash: rows[1].Hash, At: time.Now(), Kind: KindAnchor}
	if r := verify("global", rows[2:], headOf(rows), []Checkpoint{unsignedAnchor}, nil); r.Status != StatusUnproven || len(r.Problems) != 0 {
		t.Fatalf("keyless purge: %+v", r)
	}
}

// #2439 H1: forgeries by someone with database access but not the key.
func TestVerify_ForgedStarts(t *testing.T) {
	keys := testKeys(t)
	rows := chain("org:5", ip(5), 6)
	p2, p4 := signed(keys, 1, KindPeriodic, rows[1]), signed(keys, 2, KindPeriodic, rows[3])
	if r := verify("org:5", rows, headOf(rows), []Checkpoint{p2, p4}, keys); !r.OK {
		t.Fatalf("before: %+v", r)
	}
	t.Run("unsigned anchor with id -1", func(t *testing.T) {
		// INSERT an anchor that sorts before every signed checkpoint, then
		// delete the rows it "covers".
		forged := Checkpoint{ID: -1, ChainKey: "org:5", UptoSeq: 3, Hash: rows[2].Hash, At: time.Now(), Kind: KindAnchor}
		r := verify("org:5", rows[3:], headOf(rows), []Checkpoint{p2, p4, forged}, keys)
		if r.Status != StatusFailed || !strings.Contains(kinds(r), ProblemSignature) || !strings.Contains(kinds(r), ProblemMissingStart) {
			t.Fatalf("forged unsigned anchor: %s %+v", kinds(r), r)
		}
	})
	t.Run("signed periodic checkpoint as the start", func(t *testing.T) {
		// Delete the rows up to an hourly signed checkpoint.
		r := verify("org:5", rows[2:], headOf(rows), []Checkpoint{p2, p4}, keys)
		if r.Status != StatusFailed || !strings.Contains(kinds(r), ProblemMissingStart) {
			t.Fatalf("periodic start: %s", kinds(r))
		}
	})
	t.Run("signed anchor younger than retention", func(t *testing.T) {
		a := Checkpoint{ID: 3, ChainKey: "org:5", UptoSeq: 2, Hash: rows[1].Hash, At: time.Now(), Kind: KindAnchor}
		young := a.At.Add(-24 * time.Hour)
		a.RowAt = &young
		keys.Sign(&a)
		policy := Policy{Retention: 365 * 24 * time.Hour}
		r := verifyWith("org:5", rows[2:], headOf(rows), []Checkpoint{p2, p4, a}, keys, policy)
		if r.Status != StatusFailed || kinds(r) != ProblemAnchor {
			t.Fatalf("young anchor: %s", kinds(r))
		}
		old := a.At.Add(-400 * 24 * time.Hour)
		a.RowAt = &old
		if r := verifyWith("org:5", rows[2:], headOf(rows), []Checkpoint{p2, p4, a}, keys, policy); !r.OK {
			t.Fatalf("anchor past retention: %+v", r)
		}
	})
}

// AUDIT_SIGNING_SINCE: unsigned checkpoints from before signing started
// are accepted as pre-key history (unproven, not failed), but an unsigned
// anchor can't start the chain past a signed checkpoint.
func TestVerify_SigningSince(t *testing.T) {
	keys := testKeys(t)
	rows := chain("global", nil, 6)
	before := time.Now().Add(-48 * time.Hour)
	preKey := Checkpoint{ID: 1, ChainKey: "global", UptoSeq: 2, Hash: rows[1].Hash, At: before, Kind: KindAnchor}
	p4 := signed(keys, 2, KindPeriodic, rows[3])
	if r := verify("global", rows[2:], headOf(rows), []Checkpoint{preKey, p4}, keys); r.Status != StatusFailed || kinds(r) != ProblemSignature+","+ProblemMissingStart {
		t.Fatalf("pre-key anchor without AUDIT_SIGNING_SINCE: %s", kinds(r))
	}
	since := keys.WithSince(before.Add(time.Hour))
	r := verify("global", rows[2:], headOf(rows), []Checkpoint{preKey, p4}, since)
	if r.Status != StatusUnproven || len(r.Problems) != 0 {
		t.Fatalf("pre-key anchor with AUDIT_SIGNING_SINCE: %+v", r)
	}
	// A backdated unsigned anchor past a signed checkpoint.
	forged := Checkpoint{ID: 3, ChainKey: "global", UptoSeq: 4, Hash: rows[3].Hash, At: before, Kind: KindAnchor}
	if r := verify("global", rows[4:], headOf(rows), []Checkpoint{preKey, p4, forged}, since); r.Status != StatusFailed || kinds(r) != ProblemAnchor {
		t.Fatalf("backdated anchor past signed history: %s", kinds(r))
	}
	// An unsigned checkpoint written after AUDIT_SIGNING_SINCE fails.
	late := Checkpoint{ID: 4, ChainKey: "global", UptoSeq: 5, Hash: rows[4].Hash, At: time.Now(), Kind: KindPeriodic}
	if r := verify("global", rows[2:], headOf(rows), []Checkpoint{preKey, p4, late}, since); kinds(r) != ProblemSignature {
		t.Fatalf("unsigned after AUDIT_SIGNING_SINCE: %s", kinds(r))
	}
	env := map[string]string{EnvSigningSince: "2026-10-01"}
	k, err := LoadKeyring(func(n string) string { return env[n] })
	if err != nil || !k.Since().Equal(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("load since: %v %v", k.Since(), err)
	}
	env[EnvSigningSince] = "yesterday"
	if _, err := LoadKeyring(func(n string) string { return env[n] }); err == nil {
		t.Fatal("bad AUDIT_SIGNING_SINCE accepted")
	}
}

// #2439 L1: actor_id_at_write is hashed when set (and left out of the
// text when not, so older rows keep their hashes), and actor_user_id must
// be it or NULL.
func TestVerify_ActorAtWrite(t *testing.T) {
	r := chain("global", nil, 1)[0]
	without := Canonical(r)
	r.ActorIDAtWrite = ip(5)
	with := Canonical(r)
	if strings.Contains(without, "actor_id_at_write") || !strings.Contains(with, `"actor_id_at_write": 5`) {
		t.Fatalf("canonical:\n%s\n%s", without, with)
	}
	keys := testKeys(t)
	build := func(actor *int64) []Row {
		rows := chain("global", nil, 2)
		rows[1].ActorIDAtWrite = ip(5)
		rows[1].Hash = LinkHash(rows[1].PrevHash, Canonical(rows[1]))
		rows[1].ActorUserID = actor
		return rows
	}
	for name, tc := range map[string]struct {
		actor *int64
		want  string
	}{"same": {ip(5), ""}, "user deleted": {nil, ""}, "re-pointed": {ip(6), ProblemActor}} {
		rows := build(tc.actor)
		cp := signed(keys, 1, KindPeriodic, rows[1])
		if got := kinds(verify("global", rows, headOf(rows), []Checkpoint{cp}, keys)); got != tc.want {
			t.Errorf("%s: problems %q, want %q", name, got, tc.want)
		}
	}
}

// Once checkpoints are signed, an unsigned one (key removed, or written by
// someone without the key) is a problem, not a warning.
func TestVerify_UnsignedAfterSigned(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(nil)
	keys := NewKeyring(priv)
	rows := chain("global", nil, 3)
	signed := Checkpoint{ID: 1, ChainKey: "global", UptoSeq: 1, Hash: rows[0].Hash, At: time.Now(), Kind: KindPeriodic}
	keys.Sign(&signed)
	unsigned := Checkpoint{ID: 2, ChainKey: "global", UptoSeq: 2, Hash: rows[1].Hash, At: time.Now(), Kind: KindAnchor}
	r := verify("global", rows, headOf(rows), []Checkpoint{signed, unsigned}, keys)
	if r.OK || kinds(r) != ProblemSignature {
		t.Fatalf("unsigned after signed: %s", kinds(r))
	}
}

func TestKeyring_LoadAndSign(t *testing.T) {
	seed, pub, id, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{EnvSigningKey: seed}
	keys, err := LoadKeyring(func(k string) string { return env[k] })
	if err != nil || !keys.CanSign() || keys.SignerID() != id || keys.PublicKey() != pub {
		t.Fatalf("load: %v %v", keys, err)
	}
	cp := Checkpoint{ChainKey: "org:1", UptoSeq: 9, Hash: bytes.Repeat([]byte{1}, 32), At: time.Now(), Kind: KindPeriodic}
	keys.Sign(&cp)
	if keys.Check(cp) != SigOK {
		t.Fatal("own signature doesn't verify")
	}
	cp.UptoSeq++
	if keys.Check(cp) != SigBad {
		t.Fatal("changed checkpoint still verifies")
	}

	// A retired key verifies through AUDIT_SIGNING_PUBLIC_KEYS only.
	seed2, _, _, _ := GenerateKey()
	newKeys, _ := LoadKeyring(func(k string) string { return map[string]string{EnvSigningKey: seed2}[k] })
	cp.UptoSeq--
	if newKeys.Check(cp) != SigUnknownKey {
		t.Fatal("retired key accepted without being listed")
	}
	both, _ := LoadKeyring(func(k string) string {
		return map[string]string{EnvSigningKey: seed2, EnvPublicKeys: " " + pub + " "}[k]
	})
	if both.Check(cp) != SigOK {
		t.Fatal("listed retired key not accepted")
	}

	// No key: unsigned, no error. A bad key: unsigned, an error that
	// doesn't echo the value.
	none, err := LoadKeyring(func(string) string { return "" })
	if err != nil || none.CanSign() {
		t.Fatalf("no key: %v %v", none.CanSign(), err)
	}
	unsignedCP := Checkpoint{ChainKey: "global", UptoSeq: 1, Hash: ZeroHash, At: time.Now(), Kind: KindPeriodic}
	none.Sign(&unsignedCP)
	if unsignedCP.Signature != nil || none.Check(unsignedCP) != SigUnsigned {
		t.Fatal("keyless signing produced a signature")
	}
	secretish := base64.StdEncoding.EncodeToString([]byte("not-a-key-but-secret"))
	bad, err := LoadKeyring(func(k string) string { return map[string]string{EnvSigningKey: secretish}[k] })
	if err == nil || bad.CanSign() || strings.Contains(err.Error(), secretish) || strings.Contains(err.Error(), "not-a-key") {
		t.Fatalf("bad key: can sign %v, err %v", bad.CanSign(), err)
	}
}
