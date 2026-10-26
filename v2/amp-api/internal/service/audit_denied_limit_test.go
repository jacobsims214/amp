package service

import (
	"fmt"
	"net/netip"
	"sync"
	"testing"
	"time"
)

// #2440 (review #2432 M1): the limiter's memory is bounded however many
// keys (rotating addresses) arrive.
func TestDeniedLimiter_BoundedUnderRotatingIPs(t *testing.T) {
	l := NewDeniedLimiter(deniedWindow, 320, 1<<30, time.Minute)
	t0 := time.Now()
	for i := range 100000 {
		ip := netip.AddrFrom16([16]byte{0x20, 0x01, 0x0d, 0xb8, byte(i >> 16), byte(i >> 8), byte(i), 0, 0, 0, 0, 0, 0, 0, 0, 1})
		if ok, _ := l.allow("0||"+anonymousBlock(ip)+"|auth.token.fail|token:", t0); !ok {
			t.Fatalf("key %d (new) was suppressed", i)
		}
	}
	if n := l.size(); n > 320 {
		t.Fatalf("limiter holds %d keys after 100000 rotating addresses, want at most 320", n)
	}
	// A key still remembered keeps being limited.
	k := "0||2001:db8:1:869f::/64|auth.token.fail|token:"
	l.allow(k, t0)
	if ok, _ := l.allow(k, t0.Add(time.Second)); ok {
		t.Fatal("a repeat inside the window was written")
	}
}

// Addresses of one IPv6 /64 are one caller; IPv4 addresses are each their
// own.
func TestAnonymousBlock(t *testing.T) {
	a := anonymousBlock(netip.MustParseAddr("2001:db8:0:1::1"))
	if b := anonymousBlock(netip.MustParseAddr("2001:db8:0:1:ffff:ffff:ffff:ffff")); a != b || a != "2001:db8:0:1::/64" {
		t.Fatalf("same /64: %q %q", a, b)
	}
	if c := anonymousBlock(netip.MustParseAddr("2001:db8:0:2::1")); c == a {
		t.Fatal("another /64 shares the block")
	}
	if v4 := anonymousBlock(netip.MustParseAddr("::ffff:203.0.113.7")); v4 != "203.0.113.7" {
		t.Fatalf("v4 = %q", v4)
	}
	if anonymousBlock(netip.Addr{}) != "" {
		t.Fatal("invalid address")
	}
}

// Anonymous refusals of one action share a per-minute cap; the excess is
// counted into one summary at the window's end, and the next window starts
// fresh.
func TestDeniedLimiter_GlobalCap(t *testing.T) {
	l := NewDeniedLimiter(deniedWindow, 1000, 60, time.Minute)
	var scheduled []func()
	var delays []time.Duration
	l.afterFunc = func(d time.Duration, f func()) { delays, scheduled = append(delays, d), append(scheduled, f) }
	var flushed []int
	flush := func(n int, start, end time.Time) { flushed = append(flushed, n) }
	t0 := time.Now()
	written := 0
	for i := range 500 {
		if l.allowGlobal("auth.token.fail", t0.Add(time.Duration(i)*time.Millisecond), flush) {
			written++
		}
	}
	if written != 60 || len(scheduled) != 1 || delays[0] > time.Minute || delays[0] < 59*time.Second {
		t.Fatalf("written %d, summaries scheduled %d (%v), want 60 and 1 at the window's end", written, len(scheduled), delays)
	}
	if !l.allowGlobal("auth.signup.refused", t0, flush) {
		t.Fatal("another action shares the cap")
	}
	// Next window: fresh budget; the old window's summary still counts 440.
	if !l.allowGlobal("auth.token.fail", t0.Add(time.Minute), flush) {
		t.Fatal("a new window was refused")
	}
	scheduled[0]()
	if len(flushed) != 1 || flushed[0] != 440 {
		t.Fatalf("summary = %v, want [440]", flushed)
	}
}

// Many goroutines with rotating keys: no data race (go test -race), and the
// bound holds.
func TestDeniedLimiter_Concurrent(t *testing.T) {
	l := NewDeniedLimiter(deniedWindow, 640, 60, time.Minute)
	l.afterFunc = func(time.Duration, func()) {}
	var wg sync.WaitGroup
	for g := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 2000 {
				now := time.Now()
				if ok, _ := l.allow(fmt.Sprintf("0||%d-%d|a|t:", g, i), now); ok {
					l.allowGlobal("a", now, func(int, time.Time, time.Time) {})
				}
			}
		}()
	}
	wg.Wait()
	if n := l.size(); n > 640 {
		t.Fatalf("size %d", n)
	}
}
