package service

import (
	"container/list"
	"hash/maphash"
	"net/netip"
	"sync"
	"time"
)

// Rate limits on refusal rows (#2440, review #2432 M1). A refusal is written
// at most once per key (actor, action, target) per window; an anonymous
// caller's key is its address block (/64 for IPv6, /32 for IPv4), so
// rotating addresses inside one IPv6 allocation earns nothing. Anonymous
// refusals (auth.token.fail, auth.signup.refused, ...) also share a global
// cap per action per minute: a caller with many address blocks (or forged
// addresses) can't turn requests into audit inserts, which are serialised
// on the chain head, beyond the cap. The excess is counted and written as
// one "N more suppressed" row when the minute ends.
//
// The key map is bounded: sharded by key hash (no single lock), each shard
// an LRU of at most perShard keys with entries expiring after the window.

const (
	deniedShards = 32
	// deniedMaxKeys bounds the keys remembered across all shards.
	deniedMaxKeys = 16384
	// deniedGlobalCap is how many anonymous refusal rows of one action are
	// written per deniedGlobalWindow; the rest go into the summary row.
	deniedGlobalCap    = 60
	deniedGlobalWindow = time.Minute
)

// DeniedLimiter decides which refusals are written to the audit log. The
// zero value is not usable; use NewDeniedLimiter.
type DeniedLimiter struct {
	window   time.Duration
	perShard int
	seed     maphash.Seed
	shards   [deniedShards]deniedShard

	globalCap    int
	globalWindow time.Duration
	globalMu     sync.Mutex
	global       map[string]*deniedGlobal
	// afterFunc schedules the summary row (time.AfterFunc; tests replace it).
	afterFunc func(d time.Duration, f func())
}

type deniedShard struct {
	mu   sync.Mutex
	lru  *list.List // of *deniedKey, most recently written first
	keys map[string]*list.Element
}

type deniedKey struct {
	key        string
	until      time.Time
	suppressed int
}

type deniedGlobal struct {
	start      time.Time
	written    int
	suppressed int
	// flushing: the summary row for this window is scheduled.
	flushing bool
}

// NewDeniedLimiter returns a limiter writing one refusal per key per window,
// remembering at most maxKeys keys, and at most globalCap anonymous refusals
// per action per globalWindow.
func NewDeniedLimiter(window time.Duration, maxKeys, globalCap int, globalWindow time.Duration) *DeniedLimiter {
	l := &DeniedLimiter{window: window, perShard: max(1, maxKeys/deniedShards), seed: maphash.MakeSeed(),
		globalCap: globalCap, globalWindow: globalWindow, global: map[string]*deniedGlobal{}, afterFunc: afterFunc}
	for i := range l.shards {
		l.shards[i].lru = list.New()
		l.shards[i].keys = map[string]*list.Element{}
	}
	return l
}

func afterFunc(d time.Duration, f func()) { time.AfterFunc(d, f) }

// defaultDenied is shared by every Service (the REST and MCP listeners each
// build one) unless Service.Denied is set.
var defaultDenied = NewDeniedLimiter(deniedWindow, deniedMaxKeys, deniedGlobalCap, deniedGlobalWindow)

func (s *Service) deniedLimiter() *DeniedLimiter {
	if s.Denied != nil {
		return s.Denied
	}
	return defaultDenied
}

// allow reports whether a refusal under key is written at now, and how many
// were skipped under it since the last one written.
func (l *DeniedLimiter) allow(key string, now time.Time) (write bool, suppressed int) {
	sh := &l.shards[maphash.String(l.seed, key)%deniedShards]
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if el, ok := sh.keys[key]; ok {
		k := el.Value.(*deniedKey)
		if now.Before(k.until) {
			k.suppressed++
			return false, 0
		}
		suppressed = k.suppressed
		k.until, k.suppressed = now.Add(l.window), 0
		sh.lru.MoveToFront(el)
		return true, suppressed
	}
	// Make room by dropping the least recently written key (the back of
	// the list; with one window for all keys it is also the first to
	// expire, so live keys go only when the shard is full of them).
	for sh.lru.Len() >= l.perShard {
		back := sh.lru.Back()
		delete(sh.keys, back.Value.(*deniedKey).key)
		sh.lru.Remove(back)
	}
	sh.keys[key] = sh.lru.PushFront(&deniedKey{key: key, until: now.Add(l.window)})
	return true, 0
}

// size is how many keys the limiter remembers.
func (l *DeniedLimiter) size() int {
	n := 0
	for i := range l.shards {
		l.shards[i].mu.Lock()
		n += len(l.shards[i].keys)
		l.shards[i].mu.Unlock()
	}
	return n
}

// allowGlobal reports whether an anonymous refusal of action is written at
// now, under the per-action cap. When the first one of a window is
// suppressed, flush is scheduled for the window's end with the number
// suppressed in it.
func (l *DeniedLimiter) allowGlobal(action string, now time.Time, flush func(suppressed int, start, end time.Time)) bool {
	l.globalMu.Lock()
	defer l.globalMu.Unlock()
	g := l.global[action]
	if g == nil || !now.Before(g.start.Add(l.globalWindow)) {
		// A new window. A summary still pending for the old one keeps its
		// own *deniedGlobal through the scheduled func.
		g = &deniedGlobal{start: now}
		l.global[action] = g
	}
	if g.written < l.globalCap {
		g.written++
		return true
	}
	g.suppressed++
	if !g.flushing {
		g.flushing = true
		end := g.start.Add(l.globalWindow)
		l.afterFunc(end.Sub(now), func() {
			l.globalMu.Lock()
			n, start := g.suppressed, g.start
			g.flushing = false
			l.globalMu.Unlock()
			flush(n, start, end)
		})
	}
	return false
}

// anonymousBlock is the address block an anonymous caller is limited by:
// the /64 of an IPv6 address (one allocation; a host can rotate through
// it freely), the address itself for IPv4.
func anonymousBlock(a netip.Addr) string {
	if !a.IsValid() {
		return ""
	}
	a = a.Unmap()
	if a.Is6() {
		p, _ := a.Prefix(64)
		return p.String()
	}
	return a.String()
}
