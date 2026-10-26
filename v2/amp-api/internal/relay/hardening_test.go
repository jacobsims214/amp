package relay

// Unit checks for the #2390 hardening: the forwarded request carries no
// caller credentials (L3), and the byte bounds on stream queues (M2,
// and the per-person bound from #2398).

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/simstech/amp-api/internal/domain"
)

type fakeCluster struct{}

func (fakeCluster) ID() string         { return "node-a" }
func (fakeCluster) Token() string      { return "cluster-secret" }
func (fakeCluster) TTL() time.Duration { return time.Second }

// The browser leg's cookie and login token stay on the receiving replica;
// the harness leg keeps its connect token, which the holder checks again.
func TestForwardStripsCallerCredentials(t *testing.T) {
	got := make(chan http.Header, 1)
	holder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.Header.Clone()
	}))
	defer holder.Close()
	f := &clusterForwarder{rl: &Relay{cfg: Config{Cluster: fakeCluster{}}}, client: holder.Client()}
	addr := strings.TrimPrefix(holder.URL, "http://")

	for _, c := range []struct {
		path     string
		keepAuth bool
	}{
		{"/api/sessions/rs_x/events", false},
		{"/api/sessions/rs_x/commands", false},
		{"/relay/v1/sessions/rs_x/events", true},
	} {
		r := httptest.NewRequest("POST", c.path, nil)
		r.Header.Set("Cookie", "_oauth2_proxy=secret-session")
		r.Header.Set("Authorization", "Bearer secret-login")
		r.Header.Set("X-Auth-Request-Email", "alice@example.com")
		r.Header.Set("X-Forwarded-Access-Token", "secret-access")
		r.Header.Set("Origin", "https://amp.test")
		resp, err := f.send(r, addr, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		h := <-got
		for _, k := range []string{"Cookie", "X-Auth-Request-Email", "X-Forwarded-Access-Token"} {
			if h.Get(k) != "" {
				t.Errorf("%s: %s forwarded", c.path, k)
			}
		}
		if (h.Get("Authorization") != "") != c.keepAuth {
			t.Errorf("%s: Authorization forwarded=%v, want %v", c.path, h.Get("Authorization") != "", c.keepAuth)
		}
		if h.Get(tokenHeader) != "cluster-secret" || h.Get("Origin") != "https://amp.test" {
			t.Errorf("%s: cluster token or Origin missing: %v", c.path, h)
		}
	}
}

func TestStreamByteBounds(t *testing.T) {
	shared := newByteBudget(6 << 20)
	a, b := newViewerStream(nil, shared), newViewerStream(nil, shared)
	big := frame{event: "envelope", data: make([]byte, 1<<20)}
	n := 0
	for a.send(big) {
		n++
	}
	if n != 3 { // 4 frames of 1 MiB plus overhead pass 4 MiB
		t.Fatalf("one viewer queued %d MiB frames, want 3", n)
	}
	if a.Queued() > ViewerQueueBytes {
		t.Fatalf("queued %d", a.Queued())
	}
	// The shared bound: b gets what is left of 6 MiB.
	m := 0
	for b.send(big) {
		m++
	}
	if m != 2 || shared.Used() > 6<<20 {
		t.Fatalf("second viewer queued %d frames, shared %d", m, shared.Used())
	}
	// Taking frames off the queue releases them; drain releases the rest.
	f := <-a.ch
	a.dequeued(f)
	if !a.send(big) {
		t.Fatal("no room after a dequeue")
	}
	a.close()
	a.drain()
	b.drain()
	if shared.Used() != 0 || a.Queued() != 0 {
		t.Fatalf("bytes left after drain: shared %d, a %d", shared.Used(), a.Queued())
	}
}

func TestBucketTakeN(t *testing.T) {
	now := time.Unix(0, 0)
	b := newBucket(EventBytesPerSec*60, EventBytesBurst)
	if ok, _ := b.takeN(now, EventBytesBurst); !ok {
		t.Fatal("burst refused")
	}
	ok, wait := b.takeN(now, 2<<20)
	if ok || wait < 900*time.Millisecond || wait > 1100*time.Millisecond {
		t.Fatalf("over the burst: ok=%v wait=%v, want a refusal for about 1s", ok, wait)
	}
	if ok, _ := b.takeN(now.Add(time.Second), 2<<20); !ok {
		t.Fatal("refilled bucket refused")
	}
}

// One person's stalled viewers (#2398, re-review N1): 5 sessions with 3
// viewers each that never read. A dropped viewer keeps its queued bytes until
// its handler's write deadline (30 s), and re-attaching is allowed, so she can
// keep piling up stalled viewers. They fill her own bound, not the replica's,
// so another person's viewer keeps getting frames and is never dropped.
func TestUserBoundKeepsOtherViewers(t *testing.T) {
	shared := newByteBudget(DefaultQueuedBytes)
	users := newUserBudgets(DefaultUserQueuedBytes)
	const alice, bob = 1, 2
	big := frame{event: "envelope", data: make([]byte, 900<<10)}

	var hers []*Live
	for i := 0; i < 5; i++ {
		hers = append(hers, newLive(&domain.RemoteSession{ID: fmt.Sprintf("rs_alice%d", i), OwnerUserID: alice}))
	}
	his := newLive(&domain.RemoteSession{ID: "rs_bob", OwnerUserID: bob})
	reader := newViewerStream(users.acquire(bob), shared)
	his.viewers[reader] = struct{}{}

	// Three rounds of: attach 3 stalled viewers to each of her sessions, and
	// post until every one of them is dropped. Nothing reads or drains them.
	for round := 0; round < 3; round++ {
		for _, l := range hers {
			for j := 0; j < MaxViewers; j++ {
				l.viewers[newViewerStream(users.acquire(alice), shared)] = struct{}{}
			}
		}
		for n := 0; n < 10; n++ {
			for _, l := range hers {
				l.broadcast(big)
			}
		}
		for _, l := range hers {
			if len(l.viewers) != 0 {
				t.Fatalf("alice's stalled viewers weren't all dropped: %d left", len(l.viewers))
			}
		}
	}
	if got := users.used(alice); got > DefaultUserQueuedBytes {
		t.Fatalf("alice holds %d bytes, over her bound", got)
	}

	// Bob's viewer reads as it goes: it gets every frame and stays.
	for i := 0; i < 20; i++ {
		his.broadcast(big)
		if _, ok := his.viewers[reader]; !ok {
			t.Fatalf("bob's reading viewer was dropped at frame %d (replica holds %d bytes, alice %d)", i, shared.Used(), users.used(alice))
		}
		reader.dequeued(<-reader.ch)
	}
}

func TestUserBudgetsRelease(t *testing.T) {
	u := newUserBudgets(10)
	a, b := u.acquire(7), u.acquire(7)
	if a != b || !a.reserve(6) || a.reserve(6) {
		t.Fatal("one person's viewers don't share one bound")
	}
	a.release(6)
	u.release(7)
	u.release(7)
	if len(u.m) != 0 {
		t.Fatal("the bound outlived the person's last viewer")
	}
}
