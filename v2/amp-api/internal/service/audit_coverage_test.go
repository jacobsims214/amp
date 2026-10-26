package service

import (
	"testing"
	"time"
)

func TestSeenCache(t *testing.T) {
	c := newSeenCache(time.Minute)
	now := time.Now()
	if !c.claim("a", now) || c.claim("a", now.Add(30*time.Second)) {
		t.Fatal("a key is claimed once per ttl")
	}
	if !c.claim("a", now.Add(2*time.Minute)) {
		t.Fatal("an expired key is claimed again")
	}
	c.forget("a")
	if !c.claim("a", now.Add(2*time.Minute)) {
		t.Fatal("a forgotten key is claimed again")
	}
}

// Refusals are recorded under names a consumer can't mistake for the change.
func TestDeniedActionNames(t *testing.T) {
	for in, want := range map[string]string{
		"plan.approve": "plan.approve.denied", "task.dispatch": "task.dispatch.denied", "access": "access.denied",
		"auth.token.fail": "auth.token.fail", "auth.signup.refused": "auth.signup.refused", "task.human.complete": "task.human.complete.denied",
	} {
		if got := deniedAction(in); got != want {
			t.Errorf("deniedAction(%q) = %q, want %q", in, got, want)
		}
	}
}
