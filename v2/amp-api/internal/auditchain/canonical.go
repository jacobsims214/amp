// Package auditchain is the audit log's tamper evidence (KB
// decisions/audit-log-v2.md section 4, ticket #2429).
//
// Every audit_log row belongs to a chain: "org:<id>", or "global" for rows
// with no org. A BEFORE INSERT trigger (migration 033) gives each row the
// next seq in its chain, the previous row's hash, and
//
//	hash = sha256(prev_hash || Canonical(row))
//
// where prev_hash is 32 zero bytes for seq 1. Canonical is the text of a
// jsonb object of every column except actor_user_id (the users FK nulls it
// when a user is deleted) and the hashes; this package builds the same text
// in Go, so verification never trusts a function stored in the database.
//
// amp-api signs each chain's head hourly (Checkpoint) with an Ed25519 key
// that is not in the database, and before retention deletes old rows it
// signs an anchor at the last deleted seq; Verify recomputes every retained
// row from the latest anchor and checks the checkpoints against it.
package auditchain

import (
	"crypto/sha256"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Row is one audit_log row as Verify reads it: nullable columns as
// pointers, jsonb columns as Postgres's own text of them (details::text),
// source_ip as source_ip::text.
type Row struct {
	ID               int64
	At               time.Time
	ActorEmail       string
	ViaToken         bool
	OrgID            *int64
	ProjectID        *int64
	Action           string
	TargetType       string
	TargetID         string
	Summary          string
	Details          string // jsonb text, never empty
	SchemaVersion    int64
	ActorType        string
	AuthMethod       string
	TokenID          *int64
	SessionID        *string
	OnBehalfOfUserID *int64
	AgentProfile     *string
	TargetName       *string
	Outcome          string
	Reason           string
	SourceIP         *string // inet text
	UserAgent        *string
	Client           string
	RequestID        *string
	TraceID          *string
	Before           *string // jsonb text
	After            *string // jsonb text

	ChainKey *string
	ChainSeq *int64
	PrevHash []byte
	Hash     []byte
}

// HashSize is the length of every hash in a chain.
const HashSize = sha256.Size

// ZeroHash is prev_hash for a chain's first row.
var ZeroHash = make([]byte, HashSize)

// ChainKey is the chain a row with this org belongs to.
func ChainKey(orgID *int64) string {
	if orgID == nil {
		return "global"
	}
	return "org:" + strconv.FormatInt(*orgID, 10)
}

// OrgOfChain is the org id in an "org:<id>" chain key (0 for global or a
// malformed key).
func OrgOfChain(key string) int {
	n, err := strconv.Atoi(strings.TrimPrefix(key, "org:"))
	if err != nil || !strings.HasPrefix(key, "org:") {
		return 0
	}
	return n
}

// LinkHash is sha256(prev || canonical).
func LinkHash(prev []byte, canonical string) []byte {
	h := sha256.New()
	h.Write(prev)
	h.Write([]byte(canonical))
	return h.Sum(nil)
}

// Canonical is the text a row's hash covers: exactly what migration 033's
// audit_canonical(row) returns for it.
func Canonical(r Row) string {
	fields := []struct {
		k string
		v string // already JSON
	}{
		{"id", num(&r.ID)},
		{"at", str(ptr(r.At.UTC().Format("2006-01-02T15:04:05.000000Z")))},
		{"actor_email", str(&r.ActorEmail)},
		{"via_token", strconv.FormatBool(r.ViaToken)},
		{"org_id", num(r.OrgID)},
		{"project_id", num(r.ProjectID)},
		{"action", str(&r.Action)},
		{"target_type", str(&r.TargetType)},
		{"target_id", str(&r.TargetID)},
		{"summary", str(&r.Summary)},
		{"details", raw(&r.Details)},
		{"schema_version", num(&r.SchemaVersion)},
		{"actor_type", str(&r.ActorType)},
		{"auth_method", str(&r.AuthMethod)},
		{"token_id", num(r.TokenID)},
		{"session_id", str(r.SessionID)},
		{"on_behalf_of_user_id", num(r.OnBehalfOfUserID)},
		{"agent_profile", str(r.AgentProfile)},
		{"target_name", str(r.TargetName)},
		{"outcome", str(&r.Outcome)},
		{"reason", str(&r.Reason)},
		{"source_ip", str(r.SourceIP)},
		{"user_agent", str(r.UserAgent)},
		{"client", str(&r.Client)},
		{"request_id", str(r.RequestID)},
		{"trace_id", str(r.TraceID)},
		{"before", raw(r.Before)},
		{"after", raw(r.After)},
		{"chain_key", str(r.ChainKey)},
		{"chain_seq", num(r.ChainSeq)},
	}
	// jsonb keeps object keys ordered by length, then bytewise.
	sort.Slice(fields, func(i, j int) bool {
		a, b := fields[i].k, fields[j].k
		if len(a) != len(b) {
			return len(a) < len(b)
		}
		return a < b
	})
	var b strings.Builder
	b.WriteByte('{')
	for i, f := range fields {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(quote(f.k))
		b.WriteString(": ")
		b.WriteString(f.v)
	}
	b.WriteByte('}')
	return b.String()
}

func ptr[T any](v T) *T { return &v }

func num(n *int64) string {
	if n == nil {
		return "null"
	}
	return strconv.FormatInt(*n, 10)
}

func str(s *string) string {
	if s == nil {
		return "null"
	}
	return quote(*s)
}

// raw is jsonb text embedded as is: Postgres prints a nested value exactly
// as it prints it on its own.
func raw(s *string) string {
	if s == nil {
		return "null"
	}
	return *s
}

// quote escapes like Postgres's escape_json: the short escapes, \u00xx
// (lower-case hex) for other control bytes, everything else (UTF-8
// included) as is.
func quote(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		default:
			if c < 0x20 {
				const hex = "0123456789abcdef"
				b.WriteString(`\u00`)
				b.WriteByte(hex[c>>4])
				b.WriteByte(hex[c&0xf])
			} else {
				b.WriteByte(c)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}
