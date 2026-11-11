package domain

import (
	"reflect"
	"strings"
	"testing"
)

// The customer account kind (AMP #3766, approved contract artifact 36 v2,
// build slice B1): a third users.kind value next to human and agent, and the
// one administration invariant it carries — a customer is an authenticated
// requester, never its org's owner or admin (org owner/admin reaches every
// project in the org, which a requester must never reach). The grant paths
// themselves are covered by the service and repository tests; here the
// vocabulary and the pure decision are pinned.

func TestUserKindsAreTheClosedSet(t *testing.T) {
	if want := []string{"human", "agent", "customer"}; !reflect.DeepEqual(UserKinds, want) {
		t.Fatalf("UserKinds = %v, want %v", UserKinds, want)
	}
	if UserKindCustomer != "customer" {
		t.Fatalf("UserKindCustomer = %q", UserKindCustomer)
	}
}

func TestNormalizeUserKind(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"", UserKindHuman, false}, // JIT provisioning's default (migration 009)
		{"human", UserKindHuman, false},
		{"agent", UserKindAgent, false},
		{"customer", UserKindCustomer, false},
		{" Customer ", UserKindCustomer, false}, // normalized, like every other vocabulary here
		{"CUSTOMER", UserKindCustomer, false},
		{"requester", "", true}, // the intake role's word; not an account kind
		{"customer ", UserKindCustomer, false},
		{"owner", "", true},
		{"human agent", "", true},
	}
	for _, c := range cases {
		got, err := NormalizeUserKind(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("NormalizeUserKind(%q) = %q, want an error", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("NormalizeUserKind(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("NormalizeUserKind(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// The refusal names the whole set, so a caller learns the vocabulary.
	_, err := NormalizeUserKind("client")
	if err == nil || !strings.Contains(err.Error(), "human, agent, customer") {
		t.Fatalf("refusal does not name the kinds: %v", err)
	}
}

func TestUserKindPredicates(t *testing.T) {
	var nilUser *User
	if nilUser.IsCustomer() || nilUser.IsAgent() {
		t.Fatal("nil user must match no kind")
	}
	for _, c := range []struct {
		kind      string
		agent     bool
		customer  bool
		humanKind bool
	}{
		{UserKindHuman, false, false, true},
		{UserKindAgent, true, false, false},
		{UserKindCustomer, false, true, false},
	} {
		u := &User{Kind: c.kind}
		if u.IsAgent() != c.agent || u.IsCustomer() != c.customer {
			t.Errorf("kind %q: IsAgent=%v IsCustomer=%v, want %v/%v", c.kind, u.IsAgent(), u.IsCustomer(), c.agent, c.customer)
		}
	}
}

// CanHoldOrgRole is the invariant: a customer may be a plain org member and
// may never be its owner or admin. Every other account kind is unchanged —
// the guard must not touch an existing human or agent's roles.
func TestCanHoldOrgRole(t *testing.T) {
	for _, role := range []string{OrgRoleOwner, OrgRoleAdmin, OrgRoleMember} {
		for _, kind := range []string{UserKindHuman, UserKindAgent} {
			if !CanHoldOrgRole(kind, role) {
				t.Errorf("%s account must be able to hold org role %q (existing behaviour)", kind, role)
			}
		}
		if role == OrgRoleMember {
			if !CanHoldOrgRole(UserKindCustomer, role) {
				t.Error("a customer may be a plain org member")
			}
			continue
		}
		if CanHoldOrgRole(UserKindCustomer, role) {
			t.Errorf("a customer must never hold org role %q", role)
		}
	}
	// No role at all is not a grant: nothing to refuse.
	if !CanHoldOrgRole(UserKindCustomer, "") {
		t.Error("an empty role is not an org grant")
	}
}
