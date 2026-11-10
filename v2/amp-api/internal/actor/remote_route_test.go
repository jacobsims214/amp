package actor

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"sync"
	"testing"
)

// fakePlacement always names the same other replica as the owner.
type fakePlacement struct {
	mu     sync.Mutex
	owner  string
	claims int
}

func (p *fakePlacement) Alive() bool    { return true }
func (p *fakePlacement) Token() string  { return "t" }
func (p *fakePlacement) OnFence(func()) {}
func (p *fakePlacement) Claim(context.Context, int) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.claims++
	return p.owner, nil
}

// resettingOwner accepts each call, reads the request, and resets the
// connection without answering: an owner going away during a rollout.
func resettingOwner(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = http.ReadRequest(bufio.NewReader(c))
			_ = c.(*net.TCPConn).SetLinger(0) // close with RST
			c.Close()
		}
	}()
	return ln.Addr().String()
}

// A call to the cached owner that fails mid-request isn't retried (the owner
// may have run it), but the owner's address mustn't stay cached: the next
// message has to ask the database again rather than go to a dead replica
// forever (project 1 after the 2026-11-10 rollout).
func TestRoute_FailedCallForgetsCachedOwner(t *testing.T) {
	place := &fakePlacement{owner: resettingOwner(t)}
	r := NewRegistry(nil, nil, nil)
	r.place = place

	for i := 1; i <= 2; i++ {
		err := r.routeOnce(1, &MsgPing{ReplyCh: make(chan ReplySimple, 1)})
		if err == nil {
			t.Fatalf("call %d: want an error from an owner that reset the connection", i)
		}
		if retryable(err) {
			t.Fatalf("call %d: a reset mid-request must not be retried (the owner may have run it): %v", i, err)
		}
		r.mu.RLock()
		_, cached := r.owners[1]
		r.mu.RUnlock()
		if cached {
			t.Fatalf("call %d: the failed owner's address is still cached", i)
		}
	}
	if place.claims != 2 {
		t.Fatalf("want the owner looked up again for each message, got %d lookups", place.claims)
	}
}
