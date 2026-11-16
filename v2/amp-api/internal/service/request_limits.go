package service

// Rate limits on team-request submissions (epic 202, story 540; approved
// product contract artifact 36 v2 §5 and decision D12's intake rules; build
// slice S1 of discoveries/team-inbox-approved-build-slices.md, AMP #3898).
//
// A submission is cheap for the person clicking Submit and expensive for the
// team's inbox: each one is a row, a history row, an audit row and (for a
// connector) a chance to redeliver the same event forever. The inbox exists to
// remove duplicates, so intake needs a burst guard in front of it that is
// cheap, bounded and deterministic. This file is that guard, and nothing else:
// no endpoint, no request lifecycle, no storage. The submit path (slice S1)
// calls Allow before it creates, and gives the unit back when the repository
// reports that the "submission" was an idempotent replay.
//
// The shape follows the limiter AMP already runs for refusal rows
// (audit_denied_limit.go): a fixed window, an injected `now` so tests are
// deterministic, keys sharded by hash so unrelated callers never contend on one
// lock, and a hard ceiling on remembered keys so a caller who rotates scopes
// costs memory nothing. Unlike that limiter it does not suppress anything and
// writes nothing: it answers allow or refuse, and a refusal carries the wait.
//
// What the counters are, stated plainly:
//
//   - Same process only. Each amp-api replica keeps its own counters, so with N
//     replicas a caller can make up to N times the limit in one window. This is
//     a burst guard against a runaway loop or a retrying client, not a billing
//     quota and not a security boundary (authorization is: TeamSubmit decides
//     who may submit at all, and it re-checks the database on every call).
//     Nothing here is shared, replicated or persisted.
//   - A fixed window, not a token bucket: the count resets at a boundary
//     (truncated to the window), so a caller refused late in a window may retry
//     almost at once, and a caller can spend two windows' worth around a
//     boundary. That is the point — retry-after is then a single exact number
//     every surface can put in a header, and the same clock makes it
//     reproducible in a test.
//   - A replay is refunded, not exempted. A submission the repository reports as
//     an idempotent replay (created=false, slice C1) is given its unit back, so a
//     retry never consumes new-request quota. The complementary ordering lives in
//     the submit path: a *known* submit key should be resolved by the cheap
//     repository lookup before Allow, so a lost-response retry is answered with
//     its request and never meets the limiter. See Refund for why the two halves
//     cannot both live here.
//   - Keys are ids, never content. A key is (org, team, submitter, source):
//     four numbers and one of the four source kinds. No subject, no brief, no
//     contact snapshot, no idempotency key, no credential and no address ever
//     reaches a key, so there is nothing to redact when a key is logged or
//     printed in a test failure. The limiter logs nothing itself.
//   - Bounded memory, and what eviction means. At most
//     max(MaxKeys, requestLimitShards) keys are remembered at once; past that
//     the least recently used key in a shard is forgotten, and a forgotten key
//     starts again from zero. Because keys are authenticated identities rather
//     than client-chosen strings, forgetting one costs an attacker a whole
//     account per key — the bound is a memory guard, not the guard itself.
//   - Nothing is logged, audited or persisted: a refusal is not a security event
//     (authorization is, and audited by TeamSubmit's callers) and a submitted
//     payload never reaches this file.

import (
	"container/list"
	"fmt"
	"hash/maphash"
	"math"
	"sync"
	"time"

	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
)

const (
	// requestLimitShards bounds lock contention between unrelated submitters
	// (and is also the floor on the number of remembered keys: one per shard).
	// It mirrors deniedShards.
	requestLimitShards = 32

	// DefaultRequestLimitWindow is how long a submission count lasts. A minute
	// is long enough to flatten a retry storm and short enough that a person
	// who fumbles a form is never meaningfully blocked; it is also the value
	// the surface advertises in Retry-After.
	DefaultRequestLimitWindow = time.Minute

	// DefaultRequestLimitPerWindow is how many submissions one key may make per
	// window. It is deliberately generous — an operational guard against a loop,
	// not a product quota — because a busy requester with several drafts and a
	// browser that retries must never meet it, and because a limit reached in
	// ordinary use would be a product decision this component has no mandate to
	// make. Thirty a minute is roughly one every two seconds.
	DefaultRequestLimitPerWindow = 30

	// DefaultRequestLimitMaxKeys is how many keys the limiter remembers across
	// all shards (rounded up to a multiple of requestLimitShards). 8192 keys at
	// one window each is a few hundred kilobytes at worst.
	DefaultRequestLimitMaxKeys = 8192
)

// RequestLimitKey is what a submission is counted against: the tenant, the
// authenticated submitter and the source. There are four fields on purpose.
//
// Org and team are both in the key even though a team belongs to one org: a
// key built from a mismatched pair (a bug in a caller) must not alias the
// bucket of the pair it names half of, or one tenant's exhausted quota would
// refuse another tenant's submission. The submitter is the person's user id —
// 0 only for a caller with no AMP user (a connector acting as an integration),
// whose submissions then share their tenant-and-source bucket because there is
// no person to scope them to; the contract audits such a caller as the
// integration identity, not as a person. The source separates four intake paths
// that have different retry shapes: a web form retries a draft, an MCP client
// retries a call, a connector redelivers an event.
type RequestLimitKey struct {
	OrgID       int
	TeamID      int
	SubmitterID int // 0 when the caller has no AMP user
	Source      domain.RequestSourceKind
}

// String is the storage key: the four ids and the source kind, in a fixed
// order, with no content of the submission in it. It is safe to print.
func (k RequestLimitKey) String() string {
	return fmt.Sprintf("%d|%d|%d|%s", k.OrgID, k.TeamID, k.SubmitterID, k.Source)
}

// RequestLimit is the answer to one check. The zero value is "allowed with no
// quota left", which is only ever produced by a caller ignoring a real answer,
// so it is not a meaningful default — read Allowed.
type RequestLimit struct {
	// Allowed reports whether the submission may proceed.
	Allowed bool
	// RetryAfter is how long until the window this key exhausted ends: zero
	// when allowed, between zero and one window when refused.
	RetryAfter time.Duration
	// Remaining is how many more submissions the key may make in its current
	// window after this one was counted (0 for a refusal).
	Remaining int
}

// RetryAfterSeconds is the wait as whole seconds, rounded up and never less
// than one, ready for authz.RequestRateLimited and for a Retry-After header.
// Zero when the submission was allowed.
func (l RequestLimit) RetryAfterSeconds() int {
	if l.Allowed {
		return 0
	}
	s := int(math.Ceil(l.RetryAfter.Seconds()))
	if s < 1 {
		// A refusal always waits at least a second, even if a caller builds
		// one without a measured wait: "retry in 0s" invites an immediate
		// retry, which is the opposite of what a refusal means.
		return 1
	}
	return s
}

// Refusal turns a refused check into the surface's one 429: the stable
// request.rate_limited code (slice B4) with the wait in the message, to which
// the surface adds Retry-After and, in the relay envelope, retry_after_s. nil
// when the submission was allowed.
//
// The message says what was limited without naming the key: the person already
// knows which team they are submitting to, and a connector's log should not
// grow a tenant id per retry.
func (l RequestLimit) Refusal() error {
	if l.Allowed {
		return nil
	}
	return authz.RequestRateLimited(l.RetryAfterSeconds(), "too many submissions; try again shortly")
}

// RequestLimitConfig is one limiter's limits. Every field has an explicit
// default (DefaultRequestLimitConfig) and a caller may set any of them: main
// wires the operational values, tests inject small ones, and nothing here reads
// the environment.
type RequestLimitConfig struct {
	// Window is the fixed window submissions are counted in. Zero or negative
	// means DefaultRequestLimitWindow.
	Window time.Duration
	// Max is how many submissions one key may make per window. Zero or
	// negative means DefaultRequestLimitPerWindow — a non-positive limit is
	// read as "unset", never as "refuse everything", so a config mistake cannot
	// silently close a team's intake. To turn the guard off, set a limit no
	// real caller reaches; there is deliberately no disable switch that a
	// missing setting could flip.
	Max int
	// MaxKeys is how many keys are remembered across all shards. Zero or
	// negative means DefaultRequestLimitMaxKeys. The effective ceiling is
	// max(MaxKeys, requestLimitShards), because each shard keeps at least one.
	MaxKeys int
}

// DefaultRequestLimitConfig is the limiter's explicit defaults.
func DefaultRequestLimitConfig() RequestLimitConfig {
	return RequestLimitConfig{
		Window:  DefaultRequestLimitWindow,
		Max:     DefaultRequestLimitPerWindow,
		MaxKeys: DefaultRequestLimitMaxKeys,
	}
}

// normalized fills unset fields with their defaults.
func (c RequestLimitConfig) normalized() RequestLimitConfig {
	d := DefaultRequestLimitConfig()
	if c.Window <= 0 {
		c.Window = d.Window
	}
	if c.Max <= 0 {
		c.Max = d.Max
	}
	if c.MaxKeys <= 0 {
		c.MaxKeys = d.MaxKeys
	}
	return c
}

// RequestLimiter counts submissions per RequestLimitKey in fixed windows. The
// zero value is not usable; use NewRequestLimiter.
type RequestLimiter struct {
	window   time.Duration
	max      int
	perShard int
	seed     maphash.Seed
	shards   [requestLimitShards]requestLimitShard
}

// requestLimitShard is one shard's counters for one window. The whole shard is
// reset when the window rolls, which is the bulk of the cleanup: expired keys
// are released together, in O(1), instead of being swept one by one.
type requestLimitShard struct {
	mu     sync.Mutex
	window time.Time
	counts map[string]int
	// lru holds the keys of counts, most recently counted first. It bounds the
	// shard inside one window; a key at the back is forgotten first.
	lru   *list.List
	index map[string]*list.Element
}

// NewRequestLimiter returns a limiter with cfg's limits, filling any unset
// field from DefaultRequestLimitConfig.
func NewRequestLimiter(cfg RequestLimitConfig) *RequestLimiter {
	cfg = cfg.normalized()
	l := &RequestLimiter{
		window: cfg.Window,
		max:    cfg.Max,
		// Each shard keeps at least one key, so the remembered ceiling is
		// max(MaxKeys, requestLimitShards) rather than MaxKeys exactly.
		perShard: max(1, cfg.MaxKeys/requestLimitShards),
		seed:     maphash.MakeSeed(),
	}
	for i := range l.shards {
		sh := &l.shards[i]
		sh.counts = map[string]int{}
		sh.lru = list.New()
		sh.index = map[string]*list.Element{}
	}
	return l
}

// defaultRequestLimits is shared by every Service (the REST and MCP listeners
// each build one) unless Service.RequestLimits is set, the way defaultDenied is.
var defaultRequestLimits = NewRequestLimiter(DefaultRequestLimitConfig())

// requestLimits is the limiter this Service uses.
func (s *Service) requestLimits() *RequestLimiter {
	if s.RequestLimits != nil {
		return s.RequestLimits
	}
	return defaultRequestLimits
}

// shardFor picks a key's shard from its hash. A key never moves between
// shards, so a key's counter is always the one lock's responsibility.
func (l *RequestLimiter) shardFor(key string) *requestLimitShard {
	return &l.shards[maphash.String(l.seed, key)%requestLimitShards]
}

// roll resets the shard if now is in a different window than its counters.
// Called with the shard's lock held.
func (sh *requestLimitShard) roll(w time.Time) {
	if sh.window.Equal(w) {
		return
	}
	clear(sh.counts)
	sh.lru.Init()
	clear(sh.index)
	sh.window = w
}

// Allow counts one submission under key at now and answers whether it may
// proceed.
//
// now is the caller's clock, never read here: production passes time.Now() at
// the call site, and a test passes the exact instants it wants to reason about.
// The window is now truncated to the limiter's window, so every call in the
// same window sees the same boundary and a refusal's wait is exact.
func (l *RequestLimiter) Allow(key RequestLimitKey, now time.Time) RequestLimit {
	k := key.String()
	w := now.Truncate(l.window)
	sh := l.shardFor(k)

	sh.mu.Lock()
	defer sh.mu.Unlock()
	sh.roll(w)

	// Touch the key's entry before deciding, so the key a caller is actively
	// using stays at the front and is the last to be forgotten — a caller
	// being refused is still using it.
	if el, ok := sh.index[k]; ok {
		sh.lru.MoveToFront(el)
	} else {
		for sh.lru.Len() >= l.perShard {
			back := sh.lru.Back()
			oldest := back.Value.(string)
			delete(sh.counts, oldest)
			delete(sh.index, oldest)
			sh.lru.Remove(back)
		}
		sh.index[k] = sh.lru.PushFront(k)
	}
	n := sh.counts[k]
	if n >= l.max {
		return RequestLimit{RetryAfter: w.Add(l.window).Sub(now)}
	}
	sh.counts[k] = n + 1
	return RequestLimit{Allowed: true, Remaining: l.max - n - 1}
}

// Refund gives one submission back to key's current window.
//
// The submit path uses it for the one case that must not spend quota: a
// submission it counted which turned out to be an idempotent replay of a
// request that already exists (slice C1's created=false). A replay is the same
// submission seen twice, so charging it again would let a client's own retry
// burn its budget — and a browser that retried a lost response twice would lock
// its user out of submitting anything new.
//
// It is deliberately a refund rather than a check that is later committed or
// abandoned: the submit path has to wait for the repository's answer to know
// whether it created or replayed, and a refund that arrives a moment late is a
// far smaller problem than quota held for a call that never happened.
//
// Two things this deliberately does NOT do, because they are the submit path's
// ordering rather than this component's:
//
//   - It does not admit a replay. A key that has spent its whole budget is
//     refused whether or not the submission turns out to be a replay, so a
//     client retrying a lost response needs the id it already has. That is why
//     the submit path should resolve a known submit key with the repository's
//     cheap key lookup *before* calling Allow: a retry the database recognises
//     is then answered with its request and never reaches the limiter at all.
//     Refund covers the rest — the race where the create itself discovers it
//     was a replay — and any ordering a caller chooses still charges a replay
//     nothing in the end.
//   - It does not bank credit. It decrements only a count in the current
//     window, only down to zero, and is a no-op once that window has ended, so
//     a late refund can never carry quota into the next window.
func (l *RequestLimiter) Refund(key RequestLimitKey, now time.Time) {
	k := key.String()
	w := now.Truncate(l.window)
	sh := l.shardFor(k)

	sh.mu.Lock()
	defer sh.mu.Unlock()
	if !sh.window.Equal(w) {
		// The window this submission was counted in is over; its count went
		// with it. Refunding into the new window would be new quota.
		return
	}
	n := sh.counts[k]
	switch {
	case n > 1:
		sh.counts[k] = n - 1
	case n == 1:
		delete(sh.counts, k)
		if el, ok := sh.index[k]; ok {
			delete(sh.index, k)
			sh.lru.Remove(el)
		}
	}
}

// size is how many keys the limiter remembers. Tests use it to prove the
// memory bound and the window cleanup.
func (l *RequestLimiter) size() int {
	n := 0
	for i := range l.shards {
		l.shards[i].mu.Lock()
		n += len(l.shards[i].counts)
		l.shards[i].mu.Unlock()
	}
	return n
}

// windowOf is the window a key's counters are currently held for, and whether
// it is remembered at all. Tests use it to prove cleanup; nothing in production
// reads it.
func (l *RequestLimiter) windowOf(key RequestLimitKey) (time.Time, bool) {
	sh := l.shardFor(key.String())
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if _, ok := sh.counts[key.String()]; !ok {
		return time.Time{}, false
	}
	return sh.window, true
}
