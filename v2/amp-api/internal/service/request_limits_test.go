package service

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
)

// reqKey builds a submission key for a case: four ids and a source, so a
// failure names exactly which bucket was involved.
func reqKey(org, team, submitter int, source domain.RequestSourceKind) RequestLimitKey {
	return RequestLimitKey{OrgID: org, TeamID: team, SubmitterID: submitter, Source: source}
}

// t0 is a window boundary, so "the first window" and "the last instant of it"
// are both expressible without magic numbers.
var t0 = time.Date(2026, 11, 11, 16, 0, 0, 0, time.UTC)

// shardIndex is the shard a key is counted in. Tests use it to drive one
// shard's eviction; nothing in production needs it.
func shardIndex(l *RequestLimiter, key RequestLimitKey) int {
	sh := l.shardFor(key.String())
	for i := range l.shards {
		if sh == &l.shards[i] {
			return i
		}
	}
	return -1
}

// keysInShard returns n distinct keys that all hash to shard want.
func keysInShard(l *RequestLimiter, want, n int) []RequestLimitKey {
	var out []RequestLimitKey
	for submitter := 1; len(out) < n; submitter++ {
		k := reqKey(1, 1, submitter, domain.RequestSourceWeb)
		if shardIndex(l, k) == want {
			out = append(out, k)
		}
	}
	return out
}

// The core contract: a key may spend its budget inside one window, the next
// call is refused with exactly the wait until that window ends, and the next
// window is a fresh budget. Deterministic from the injected clock alone, so
// running it twice gives identical answers.
func TestRequestLimiter_AllowRefuseRetryAfter(t *testing.T) {
	const max = 3
	run := func() []RequestLimit {
		l := NewRequestLimiter(RequestLimitConfig{Window: time.Minute, Max: max})
		key := reqKey(7, 11, 42, domain.RequestSourceWeb)
		var got []RequestLimit
		for i := range max {
			got = append(got, l.Allow(key, t0.Add(time.Duration(i)*10*time.Second)))
		}
		got = append(got,
			// 30s into the window: 30s of it are left.
			l.Allow(key, t0.Add(30*time.Second)),
			// The last instant is still the same window.
			l.Allow(key, t0.Add(time.Minute-time.Nanosecond)),
			// The next window starts from zero, with nothing carried over.
			l.Allow(key, t0.Add(time.Minute)),
			l.Allow(key, t0.Add(time.Minute+20*time.Second)),
			l.Allow(key, t0.Add(time.Minute+20*time.Second)),
		)
		return got
	}

	want := []struct {
		allowed    bool
		retryAfter time.Duration
		remaining  int
	}{
		{true, 0, 2},
		{true, 0, 1},
		{true, 0, 0},
		{false, 30 * time.Second, 0},
		{false, time.Nanosecond, 0},
		{true, 0, 2},
		{true, 0, 1},
		{true, 0, 0},
	}
	for pass := range 2 {
		got := run()
		if len(got) != len(want) {
			t.Fatalf("pass %d produced %d answers, want %d", pass, len(got), len(want))
		}
		for i, w := range want {
			g := got[i]
			if g.Allowed != w.allowed || g.RetryAfter != w.retryAfter || g.Remaining != w.remaining {
				t.Fatalf("pass %d call %d = %+v, want allowed=%v retryAfter=%v remaining=%d",
					pass, i, g, w.allowed, w.retryAfter, w.remaining)
			}
		}
	}
}

// A refusal is the surface's one 429: the stable request.rate_limited code
// (slice B4) with the wait in the message, which the surface turns into
// Retry-After / retry_after_s.
func TestRequestLimiter_RefusalIsTheRateLimitedCode(t *testing.T) {
	l := NewRequestLimiter(RequestLimitConfig{Window: time.Minute, Max: 1})
	// Distinctive ids, so the message below can be checked for any of them.
	key := reqKey(987654, 876543, 765432, domain.RequestSourceMCP)
	if !l.Allow(key, t0).Allowed {
		t.Fatal("the first submission was refused")
	}
	refused := l.Allow(key, t0.Add(20*time.Second))
	if refused.Allowed {
		t.Fatal("the second submission was allowed")
	}
	if got, want := refused.RetryAfterSeconds(), 40; got != want {
		t.Fatalf("RetryAfterSeconds = %d, want %d (40s of the window left)", got, want)
	}
	err := refused.Refusal()
	if err == nil {
		t.Fatal("a refusal produced no error")
	}
	if code := authz.CodeOf(err); code != authz.CodeRequestRateLimited {
		t.Fatalf("code = %q, want %q", code, authz.CodeRequestRateLimited)
	}
	if status := authz.StatusOf(err, 500); status != 429 {
		t.Fatalf("status = %d, want 429", status)
	}
	// The message names what was limited and carries the wait, but no key,
	// tenant, submitter or content — a refusal travels to a client and may reach
	// a log, and the ids are not the client's business.
	msg := err.Error()
	if want := "too many submissions; try again shortly; try again in 40s"; msg != want {
		t.Fatalf("message = %q, want %q", msg, want)
	}
	for _, id := range []string{"987654", "876543", "765432", key.String()} {
		if strings.Contains(msg, id) {
			t.Fatalf("the refusal message %q names %q", msg, id)
		}
	}
	// An allowed check has no refusal and no wait.
	if err := l.Allow(key, t0.Add(2*time.Minute)).Refusal(); err != nil {
		t.Fatalf("an allowed check produced a refusal: %v", err)
	}
}

// Rounding is always up and never below one second: a header must not invite a
// client to retry "in 0s", which some clients read as "retry immediately".
func TestRequestLimit_RetryAfterSeconds(t *testing.T) {
	cases := []struct {
		name string
		in   RequestLimit
		want int
	}{
		{"allowed", RequestLimit{Allowed: true}, 0},
		{"refused with no wait measured", RequestLimit{}, 1},
		{"a fraction of a second", RequestLimit{RetryAfter: 200 * time.Millisecond}, 1},
		{"just over a second rounds up", RequestLimit{RetryAfter: 1001 * time.Millisecond}, 2},
		{"whole seconds", RequestLimit{RetryAfter: 59 * time.Second}, 59},
		{"one window", RequestLimit{RetryAfter: time.Minute}, 60},
	}
	for _, tc := range cases {
		if got := tc.in.RetryAfterSeconds(); got != tc.want {
			t.Errorf("%s: RetryAfterSeconds = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// Every part of the key separates buckets. If any one were ignored, one
// tenant's exhausted budget would refuse another tenant's submission, or one
// person's busy minute would refuse their whole team's.
func TestRequestLimiter_KeySeparatesTenantsSubmittersAndSources(t *testing.T) {
	l := NewRequestLimiter(RequestLimitConfig{Window: time.Minute, Max: 1})
	base := reqKey(7, 11, 42, domain.RequestSourceWeb)
	if !l.Allow(base, t0).Allowed {
		t.Fatal("the first submission under the base key was refused")
	}
	if l.Allow(base, t0).Allowed {
		t.Fatal("a repeat under the same key was allowed past the limit")
	}

	others := []struct {
		name string
		key  RequestLimitKey
	}{
		{"another org", reqKey(8, 11, 42, domain.RequestSourceWeb)},
		{"another team in the same org", reqKey(7, 12, 42, domain.RequestSourceWeb)},
		{"another submitter in the same team", reqKey(7, 11, 43, domain.RequestSourceWeb)},
		{"the same submitter through another source", reqKey(7, 11, 42, domain.RequestSourceAPI)},
		{"a caller with no AMP user (a connector)", reqKey(7, 11, 0, domain.RequestSourceConnector)},
	}
	for _, o := range others {
		if !l.Allow(o.key, t0).Allowed {
			t.Errorf("%s: refused although it is a different bucket", o.name)
		}
	}
	// The same submitter on every source is one bucket per intake path, because
	// each path retries differently.
	for _, src := range domain.RequestSourceKinds {
		if !l.Allow(reqKey(7, 11, 44, src), t0).Allowed {
			t.Errorf("source %q: first submission refused", src)
		}
		if l.Allow(reqKey(7, 11, 44, src), t0).Allowed {
			t.Errorf("source %q: second submission allowed past a limit of 1", src)
		}
	}
}

// The key is four ids and a source kind and nothing else: no subject, brief,
// contact snapshot, idempotency key or address can reach it, which is what makes
// it safe to print in a log line or a test failure. The limiter itself logs
// nothing.
func TestRequestLimitKey_HoldsOnlyIdentifiers(t *testing.T) {
	if got, want := reqKey(7, 11, 42, domain.RequestSourceMCP).String(), "7|11|42|mcp"; got != want {
		t.Fatalf("String = %q, want %q", got, want)
	}
	// Zero ids still produce only identifiers (a connector has no user).
	if got, want := reqKey(0, 0, 0, domain.RequestSourceConnector).String(), "0|0|0|connector"; got != want {
		t.Fatalf("String with zero ids = %q, want %q", got, want)
	}
	// Distinct keys never collide in their string form.
	seen := map[string]bool{}
	for _, org := range []int{1, 2} {
		for _, team := range []int{10, 20} {
			for _, sub := range []int{0, 100} {
				for _, src := range domain.RequestSourceKinds {
					s := reqKey(org, team, sub, src).String()
					if seen[s] {
						t.Fatalf("key %q is not unique", s)
					}
					seen[s] = true
				}
			}
		}
	}
}

// An idempotent replay is the same submission seen twice and must not spend
// new-request quota: Allow, then Refund once the repository answers "this key
// already produced request N" (slice C1's created=false). Without it a client's
// own retry of one submission would burn its budget.
func TestRequestLimiter_RefundKeepsReplaysFree(t *testing.T) {
	// A refunded count is exactly restored: one unit back, no more, no less.
	l := NewRequestLimiter(RequestLimitConfig{Window: time.Minute, Max: 2})
	key := reqKey(7, 11, 42, domain.RequestSourceWeb)
	steps := []struct {
		name      string
		refund    bool
		allowed   bool
		remaining int
	}{
		{"first fresh submit", false, true, 1},
		{"second fresh submit", false, true, 0},
		{"third is refused", false, false, 0},
		// The refused call counted nothing, so the refund returns the second
		// submission's unit and no more.
		{"refund a counted submission", true, false, 0},
		{"the returned unit is spendable", false, true, 0},
		{"and only that one unit", false, false, 0},
	}
	for i, s := range steps {
		now := t0.Add(time.Duration(i) * time.Second)
		if s.refund {
			l.Refund(key, now)
			continue
		}
		r := l.Allow(key, now)
		if r.Allowed != s.allowed || r.Remaining != s.remaining {
			t.Fatalf("%s = %+v, want allowed=%v remaining=%d", s.name, r, s.allowed, s.remaining)
		}
	}

	// A stream of replays never consumes anything: every Allow is refunded, so
	// the key keeps its budget and leaves nothing behind.
	replays := NewRequestLimiter(RequestLimitConfig{Window: time.Minute, Max: 1})
	replayKey := reqKey(7, 11, 42, domain.RequestSourceMCP)
	for i := range 100 {
		at := t0.Add(time.Duration(i) * time.Second)
		if r := replays.Allow(replayKey, at); !r.Allowed {
			t.Fatalf("replay %d was refused: %+v", i, r)
		}
		replays.Refund(replayKey, at)
	}
	if n := replays.size(); n != 0 {
		t.Fatalf("%d fully refunded keys are still remembered, want 0", n)
	}
	if r := replays.Allow(replayKey, t0.Add(2*time.Minute)); !r.Allowed || r.Remaining != 0 {
		t.Fatalf("after 100 refunded replays a fresh submission was not allowed: %+v", r)
	}
}

// A refund cannot invent quota. However many refunds arrive, the window still
// allows exactly `max` submissions; refunding the last count forgets the key
// rather than crediting it; and a late refund cannot carry quota into the next
// window.
func TestRequestLimiter_RefundNeverInventsQuota(t *testing.T) {
	l := NewRequestLimiter(RequestLimitConfig{Window: time.Minute, Max: 1})
	key := reqKey(7, 11, 42, domain.RequestSourceWeb)

	// Refunding a key it never counted adds nothing.
	l.Refund(key, t0)
	if r := l.Allow(key, t0); !r.Allowed || r.Remaining != 0 {
		t.Fatalf("after refunding an unknown key: %+v", r)
	}
	// Ten refunds of one count mean at most "this count is gone": they do not
	// bank ten submissions. The key is forgotten, and the one unit it had is the
	// one unit a fresh submission can now spend.
	for range 10 {
		l.Refund(key, t0)
	}
	if _, ok := l.windowOf(key); ok {
		t.Fatal("a fully refunded key is still remembered; it should be forgotten, not credited")
	}
	if r := l.Allow(key, t0.Add(time.Second)); !r.Allowed || r.Remaining != 0 {
		t.Fatalf("a fully refunded key did not start again from zero: %+v", r)
	}
	if r := l.Allow(key, t0.Add(2*time.Second)); r.Allowed {
		t.Fatal("repeated refunds banked credit: a second submission was allowed past a limit of 1")
	}

	// A late refund (its window over) must not credit the new window.
	key2 := reqKey(7, 11, 43, domain.RequestSourceWeb)
	if !l.Allow(key2, t0.Add(10*time.Second)).Allowed {
		t.Fatal("key2's first submission was refused")
	}
	l.Refund(key2, t0.Add(2*time.Minute))
	if r := l.Allow(key2, t0.Add(2*time.Minute)); !r.Allowed || r.Remaining != 0 {
		t.Fatalf("the late refund changed the new window: %+v", r)
	}
	if r := l.Allow(key2, t0.Add(2*time.Minute)); r.Allowed {
		t.Fatal("the late refund bought a second submission in the new window")
	}
}

// Memory is bounded however many keys arrive, and the bound is the effective
// ceiling the config describes: MaxKeys rounded up to a whole shard.
func TestRequestLimiter_MemoryIsBounded(t *testing.T) {
	const maxKeys = 320 // ten per shard
	l := NewRequestLimiter(RequestLimitConfig{Window: time.Minute, Max: 1 << 30, MaxKeys: maxKeys})
	for i := range 20000 {
		if !l.Allow(reqKey(1, 1, 1_000_000+i, domain.RequestSourceWeb), t0).Allowed {
			t.Fatalf("key %d (new) was refused", i)
		}
	}
	if n := l.size(); n > maxKeys {
		t.Fatalf("the limiter holds %d keys after 20000 distinct submitters, want at most %d", n, maxKeys)
	}

	// The ceiling has a floor of one key per shard, so a config smaller than the
	// shard count still bounds memory rather than clamping to zero.
	small := NewRequestLimiter(RequestLimitConfig{Window: time.Minute, MaxKeys: 1})
	for i := range 1000 {
		small.Allow(reqKey(1, 1, i+1, domain.RequestSourceWeb), t0)
	}
	if n := small.size(); n > requestLimitShards {
		t.Fatalf("with MaxKeys=1 the limiter holds %d keys, want at most %d", n, requestLimitShards)
	}
}

// Inside one window, a shard forgets its least recently used key when a new one
// needs room — and never the key that is being used, so pressing on against the
// limit cannot reset an active caller's count.
func TestRequestLimiter_EvictionKeepsTheActiveKey(t *testing.T) {
	// Two keys per shard.
	l := NewRequestLimiter(RequestLimitConfig{
		Window: time.Minute, Max: 1, MaxKeys: requestLimitShards * 2,
	})
	want := shardIndex(l, reqKey(1, 1, 1, domain.RequestSourceWeb))
	keys := keysInShard(l, want, 3)
	stale, active, fresh := keys[0], keys[1], keys[2]

	if !l.Allow(stale, t0).Allowed {
		t.Fatal("stale's first submission was refused")
	}
	if !l.Allow(active, t0).Allowed {
		t.Fatal("active's first submission was refused")
	}
	// A third key in the same shard needs room: stale (least recently used) is
	// forgotten, active stays.
	if !l.Allow(fresh, t0).Allowed {
		t.Fatal("fresh's first submission was refused")
	}
	if r := l.Allow(active, t0.Add(time.Second)); r.Allowed {
		t.Fatal("the active key's count was lost to eviction")
	}
	// The forgotten key starts again from zero — which is what forgetting means
	// and why the bound is a memory guard, not the guard itself.
	if !l.Allow(stale, t0.Add(2*time.Second)).Allowed {
		t.Fatal("the evicted key did not start again from zero")
	}
}

// A window roll clears a shard's counters in one step, which is the cleanup: an
// old window's keys are released together, so a limiter that saw a burst of
// submitters is small again as soon as the new window is used, and no old count
// survives into the new window.
func TestRequestLimiter_WindowRollCleansUp(t *testing.T) {
	const maxKeys = 320
	l := NewRequestLimiter(RequestLimitConfig{Window: time.Minute, Max: 1 << 30, MaxKeys: maxKeys})
	for i := range 5000 {
		l.Allow(reqKey(1, 1, i+1, domain.RequestSourceWeb), t0)
	}
	if l.size() == 0 {
		t.Fatal("no keys were remembered at all, so the cleanup test proves nothing")
	}

	// A key counted heavily in the first window, kept in the shard it lands in.
	old := reqKey(2, 2, 999_999, domain.RequestSourceWeb)
	for range 100 {
		l.Allow(old, t0)
	}
	if _, ok := l.windowOf(old); !ok {
		t.Fatal("the heavily used key was evicted, so the window-roll check proves nothing")
	}

	next := t0.Add(time.Minute)
	for shard := range requestLimitShards {
		key := keysInShard(l, shard, 1)[0]
		if !l.Allow(key, next).Allowed {
			t.Fatalf("shard %d: the first submission of the new window was refused", shard)
		}
		if got, ok := l.windowOf(key); !ok || !got.Equal(next) {
			t.Fatalf("shard %d: window = %v present=%v, want %v", shard, got, ok, next)
		}
	}
	if n := l.size(); n != requestLimitShards {
		t.Fatalf("after the window rolled, %d keys are held, want one per touched shard (%d)", n, requestLimitShards)
	}
	if r := l.Allow(old, next); !r.Allowed || r.Remaining != l.max-1 {
		t.Fatalf("a key from the old window did not start fresh: %+v (limit %d)", r, l.max)
	}
}

// Callers racing on one key in one window get exactly the limit's worth of
// submissions between them: no lost counts, no double spend. Run with -race.
func TestRequestLimiter_ConcurrentAllowIsExact(t *testing.T) {
	const (
		max       = 30
		callers   = 40
		perCaller = 5
	)
	l := NewRequestLimiter(RequestLimitConfig{Window: time.Minute, Max: max})
	key := reqKey(7, 11, 42, domain.RequestSourceWeb)

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		allowed int
	)
	start := make(chan struct{})
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for range perCaller {
				if l.Allow(key, t0.Add(time.Second)).Allowed {
					mu.Lock()
					allowed++
					mu.Unlock()
				}
			}
		}()
	}
	close(start)
	wg.Wait()
	if allowed != max {
		t.Fatalf("%d of %d concurrent submissions were allowed, want exactly %d", allowed, callers*perCaller, max)
	}
}

// Unrelated keys do not interfere: every one of 32 concurrent callers on its own
// key gets its whole budget in the same window.
func TestRequestLimiter_ConcurrentDistinctKeysAreIndependent(t *testing.T) {
	const max = 5
	l := NewRequestLimiter(RequestLimitConfig{Window: time.Minute, Max: max})
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []string
	)
	start := make(chan struct{})
	for i := range requestLimitShards {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			key := reqKey(1, 1, i+1, domain.RequestSourceMCP)
			for range max {
				if !l.Allow(key, t0.Add(time.Second)).Allowed {
					mu.Lock()
					errs = append(errs, "key was refused inside its budget")
					mu.Unlock()
					return
				}
			}
			if l.Allow(key, t0.Add(2*time.Second)).Allowed {
				mu.Lock()
				errs = append(errs, "key was allowed past the limit")
				mu.Unlock()
			}
		}(i)
	}
	close(start)
	wg.Wait()
	for _, e := range errs {
		t.Error(e)
	}
}

// A config that leaves fields unset means the documented defaults, never zero
// limits: a missing setting must not close a team's intake. A negative value is
// "unset" for the same reason.
func TestRequestLimiter_ConfigDefaults(t *testing.T) {
	def := DefaultRequestLimitConfig()
	if def.Window != time.Minute || def.Max <= 0 || def.MaxKeys <= 0 {
		t.Fatalf("defaults = %+v, want a positive window, limit and key ceiling", def)
	}

	key := reqKey(1, 2, 3, domain.RequestSourceWeb)
	for name, cfg := range map[string]RequestLimitConfig{
		"empty":    {},
		"negative": {Window: -time.Second, Max: -1, MaxKeys: -1},
	} {
		l := NewRequestLimiter(cfg)
		if got, want := l.window, def.Window; got != want {
			t.Fatalf("%s: window = %v, want the default %v", name, got, want)
		}
		if got, want := l.max, def.Max; got != want {
			t.Fatalf("%s: limit = %d, want the default %d", name, got, want)
		}
		for i := range def.Max {
			if !l.Allow(key, t0).Allowed {
				t.Fatalf("%s: submission %d of the default %d was refused", name, i+1, def.Max)
			}
		}
		if l.Allow(key, t0).Allowed {
			t.Fatalf("%s: a submission past %d in one window was allowed", name, def.Max)
		}
	}
}

// The Service uses its own limiter when it has one and the shared one otherwise
// — the same convention Service.Denied follows — so wiring it in main is
// optional and a test can replace it.
func TestServiceRequestLimits_SharedUnlessSet(t *testing.T) {
	s := &Service{}
	if s.requestLimits() != defaultRequestLimits {
		t.Fatal("a Service with no limiter did not use the shared one")
	}
	own := NewRequestLimiter(RequestLimitConfig{Window: time.Minute, Max: 1})
	s.RequestLimits = own
	if s.requestLimits() != own {
		t.Fatal("a Service with its own limiter did not use it")
	}
}
