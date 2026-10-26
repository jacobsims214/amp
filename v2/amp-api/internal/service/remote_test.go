package service

import (
	"slices"
	"testing"
	"time"

	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
)

// The capability flag for every combination of its inputs (protocol 8.3).
func TestRemoteCapabilityFor(t *testing.T) {
	on := RemoteConfig{InstanceEnabled: true, AuthEnabled: true}
	tests := []struct {
		name      string
		cfg       RemoteConfig
		person    bool
		orgs      []int
		available bool
		reason    string
	}{
		{"everything on", on, true, []int{3}, true, ""},
		{"two orgs on", on, true, []int{2, 3}, true, ""},
		{"instance off", RemoteConfig{AuthEnabled: true}, true, []int{3}, false, domain.RemoteUnavailableInstance},
		{"instance off wins over auth off", RemoteConfig{}, false, nil, false, domain.RemoteUnavailableInstance},
		{"auth off", RemoteConfig{InstanceEnabled: true}, true, []int{3}, false, domain.RemoteUnavailableAuth},
		{"not a person", on, false, []int{3}, false, domain.RemoteUnavailablePerson},
		{"no org on", on, true, nil, false, domain.RemoteUnavailableNoOrg},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RemoteCapabilityFor(tt.cfg, tt.person, tt.orgs)
			if got.Available != tt.available {
				t.Fatalf("available = %v, want %v", got.Available, tt.available)
			}
			reason := ""
			if got.Reason != nil {
				reason = *got.Reason
			}
			if reason != tt.reason {
				t.Fatalf("reason = %q, want %q", reason, tt.reason)
			}
			if got.OrgIDs == nil {
				t.Fatal("org_ids is nil (must serialize as [])")
			}
			if tt.available && !slices.Equal(got.OrgIDs, tt.orgs) || !tt.available && len(got.OrgIDs) != 0 {
				t.Fatalf("org_ids = %v", got.OrgIDs)
			}
		})
	}
}

func TestRemotePerson(t *testing.T) {
	tests := []struct {
		name string
		p    authz.Principal
		want bool
	}{
		{"a person in the web UI", authz.Principal{UserID: 1, Channel: "ui"}, true},
		{"a person over MCP", authz.Principal{UserID: 1, Channel: "mcp"}, true},
		{"an API token", authz.Principal{UserID: 1, ViaToken: true}, false},
		{"an agent account", authz.Principal{UserID: 1, Agent: true}, false},
		{"local dev (auth off)", authz.Principal{Super: true}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := remotePerson(tt.p); got != tt.want {
				t.Fatalf("remotePerson = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestValidateRemotePolicy(t *testing.T) {
	tests := []struct {
		idle, life, pin int
		ok              bool
	}{
		{30, 0, 30, true}, {5, 0, 0, true}, {240, 0, 30, true},
		// #2424: 0 is "no limit" for both, and the default.
		{0, 0, 30, true}, {0, 1, 30, true}, {0, 720, 30, true}, {30, 12, 30, true},
		{4, 0, 30, false}, {241, 0, 30, false}, {-1, 0, 30, false}, {30, 0, -1, false}, {30, 0, 31, false},
		{0, -1, 30, false}, {0, 721, 30, false},
	}
	for _, tt := range tests {
		err := ValidateRemotePolicy(domain.RemotePolicy{IdleTimeoutMin: tt.idle, MaxLifetimeHours: tt.life, DevicePinDays: tt.pin})
		if (err == nil) != tt.ok {
			t.Errorf("idle %d life %d pin %d: err %v, want ok=%v", tt.idle, tt.life, tt.pin, err, tt.ok)
		}
		if err != nil && authz.CodeOf(err) != domain.RemoteCodeMalformed {
			t.Errorf("idle %d life %d pin %d: code %q", tt.idle, tt.life, tt.pin, authz.CodeOf(err))
		}
	}
	if d := domain.DefaultRemotePolicy(); d.IdleTimeoutMin != 0 || d.MaxLifetimeHours != 0 || d.SessionExpiry(time.Now()) != nil {
		t.Errorf("default policy has a limit: %+v", d)
	}
}

func TestPromptDigestKeepsNoText(t *testing.T) {
	n, sha := PromptDigest("héllo")
	if n != 6 || sha != "3c48591d8d098a4538f5e013dfcf406e948eac4d3277b10bf614e295d6068179" {
		t.Fatalf("digest = %d %s", n, sha)
	}
	d := RemoteAudit{Action: domain.AuditRemotePrompt, Op: "prompt", PromptLen: n, PromptSHA256: sha}.details(nil)
	for k, v := range d {
		if s, ok := v.(string); ok && s == "héllo" {
			t.Fatalf("details[%s] holds the text", k)
		}
	}
}
