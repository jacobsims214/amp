package domain

import "time"

// Remote control of a harness session from the web app (epic 165). The wire
// protocol is v2/docs/remote-control/protocol.md; these are the stored parts:
// the session registry, the devices the relay learned about, and the org
// policy. No message content is ever stored (decision D3).

// Remote session states.
const (
	RemoteSessionActive = "active"
	RemoteSessionEnded  = "ended"
)

// End reasons (protocol 12.2).
const (
	RemoteEndHarnessEnded = "harness_ended"
	RemoteEndDetached     = "detached"
	RemoteEndDisconnected = "disconnected"
	RemoteEndRevoked      = "revoked"
	RemoteEndPolicyOff    = "policy_off"
	RemoteEndIdle         = "idle"
	RemoteEndExpired      = "expired"
	RemoteEndKilled       = "killed"
)

// A session has no time limit by default (#2424, user decision 2026-10-26:
// "keep itself alive until a user disconnects it"). It ends on an explicit
// action: /remote off or quitting the chat, Disconnect or Revoke in the web
// app, the org switch, deactivation or lost access. An org admin can opt into
// an idle limit and a maximum lifetime (RemotePolicy).

// RemoteListEndedFor is how far back GET /api/sessions lists ended sessions,
// and RemoteListEndedMax how many of them at most (#2389).
const (
	RemoteListEndedFor = 7 * 24 * time.Hour
	RemoteListEndedMax = 100
)

// RemoteStatusMeta is what the relay stores from a harness status: the model
// and the current ticket. Nil members are null in the status.
type RemoteStatusMeta struct {
	Model      *string
	TicketID   *int64
	TicketName *string
}

// RemoteMaxLiveSessions is how many live sessions one user may have.
const RemoteMaxLiveSessions = 5

// RemoteSession is one `/remote` on a laptop: a row in remote_sessions.
type RemoteSession struct {
	ID              string     `json:"session_id"`
	OwnerUserID     int        `json:"owner_user_id"`
	ProjectID       int        `json:"project_id"`
	Host            string     `json:"host"`
	OS              string     `json:"os"`
	HarnessVersion  string     `json:"harness_version"`
	HarnessID       string     `json:"harness_id"`
	HarnessPub      string     `json:"harness_pub"`
	ProtocolVersion int        `json:"protocol_version"`
	HolderNode      *string    `json:"holder_node,omitempty"`
	State           string     `json:"state"`
	EndReason       *string    `json:"end_reason,omitempty"`
	StartedAt       time.Time  `json:"started_at"`
	HeartbeatAt     *time.Time `json:"heartbeat_at,omitempty"`
	LastActivityAt  time.Time  `json:"last_activity_at"`
	// ExpiresAt is when the session's maximum lifetime ends: nil (the
	// default) when its org set no lifetime at registration (#2424).
	ExpiresAt *time.Time `json:"expires_at"`
	EndedAt         *time.Time `json:"ended_at,omitempty"`
	// Model and the current ticket are copied from the harness's signed
	// status (protocol 6.3, #2389): metadata for the session list, never
	// message content. CurrentTicketName falls back to the ticket's name in
	// AMP when the harness didn't send one.
	Model             *string `json:"model,omitempty"`
	CurrentTicketID   *int64  `json:"current_ticket_id,omitempty"`
	CurrentTicketName *string `json:"current_ticket_name,omitempty"`
	// ConnectTokenExpiresAt is when the current connect token stops working.
	// The token's hash is never read back out of the store.
	ConnectTokenExpiresAt *time.Time `json:"-"`
}

// Active reports whether the session hasn't ended.
func (s *RemoteSession) Active() bool { return s != nil && s.State == RemoteSessionActive }

// RemoteDevice is a browser paired with one harness install (remote_devices).
type RemoteDevice struct {
	ID          int64      `json:"id"`
	OwnerUserID int        `json:"owner_user_id"`
	HarnessID   string     `json:"harness_id"`
	DeviceID    string     `json:"device_id"`
	DevicePub   string     `json:"device_pub"`
	Name        string     `json:"name"`
	PairedAt    time.Time  `json:"paired_at"`
	ExpiresAt   time.Time  `json:"expires_at"`
	RevokedAt   *time.Time `json:"revoked_at,omitempty"`
}

// RemotePolicy is an org's remote control setting (protocol 8.3 "Org policy").
// IdleTimeoutMin and MaxLifetimeHours are 0 when off, the default (#2424).
type RemotePolicy struct {
	Enabled          bool `json:"enabled"`
	IdleTimeoutMin   int  `json:"idle_timeout_min"`
	MaxLifetimeHours int  `json:"max_lifetime_hours"`
	DevicePinDays    int  `json:"device_pin_days"`
}

// Defaults and bounds of RemotePolicy. The idle limit and the lifetime are
// off (0) by default; an admin may set 5..240 minutes and 1..720 hours.
const (
	RemoteIdleTimeoutDefault = 0
	RemoteIdleTimeoutMin     = 5
	RemoteIdleTimeoutMax     = 240
	RemoteMaxLifetimeDefault = 0
	RemoteMaxLifetimeMax     = 720
	RemoteDevicePinDefault   = 30
	RemoteDevicePinMax       = 30
)

// DefaultRemotePolicy is every org's policy until an admin changes it: off,
// and with no time limits once turned on.
func DefaultRemotePolicy() RemotePolicy {
	return RemotePolicy{Enabled: false, IdleTimeoutMin: RemoteIdleTimeoutDefault,
		MaxLifetimeHours: RemoteMaxLifetimeDefault, DevicePinDays: RemoteDevicePinDefault}
}

// IdleTimeout is the idle limit, 0 when there is none.
func (p RemotePolicy) IdleTimeout() time.Duration {
	return time.Duration(p.IdleTimeoutMin) * time.Minute
}

// SessionExpiry is when a session registered at start ends by lifetime: nil
// when the policy sets no lifetime.
func (p RemotePolicy) SessionExpiry(start time.Time) *time.Time {
	if p.MaxLifetimeHours <= 0 {
		return nil
	}
	t := start.Add(time.Duration(p.MaxLifetimeHours) * time.Hour)
	return &t
}

// RemoteCapability is /api/me's capabilities.remote_control: whether the UI
// shows the Sessions entry points.
type RemoteCapability struct {
	Available bool  `json:"available"`
	OrgIDs    []int `json:"org_ids"`
	// Reason says why it isn't available (nil when it is): one of the
	// RemoteUnavailable* values.
	Reason *string `json:"reason"`
}

// Why remote control isn't available (RemoteCapability.Reason), in the order
// they're checked.
const (
	RemoteUnavailableInstance = "instance_disabled"
	RemoteUnavailableAuth     = "auth_disabled"
	RemoteUnavailablePerson   = "not_a_person"
	RemoteUnavailableNoOrg    = "no_org_enabled"
)

// Remote control audit actions (protocol 12.4). Their details hold metadata
// only: never prompt text, tool output or secrets.
const (
	AuditRemoteEnable       = "remote.enable"
	AuditRemoteDisable      = "remote.disable"
	AuditRemoteExpire       = "remote.expire"
	AuditRemotePair         = "remote.pair"
	AuditRemotePairReject   = "remote.pair.reject"
	AuditRemoteDeviceRevoke = "remote.device.revoke"
	AuditRemoteAttach       = "remote.attach"
	AuditRemoteDetach       = "remote.detach"
	AuditRemotePrompt       = "remote.prompt"
	AuditRemoteStop         = "remote.stop"
	AuditRemoteRefused      = "remote.refused"
	AuditRemotePolicySet    = "remote.policy.set"
	AuditRemoteKill         = "remote.kill"
)

// Error codes of the remote control API (protocol 9.1), sent as the "code"
// member next to "error".
const (
	RemoteCodeMalformed        = "malformed"
	RemoteCodeUnsupportedVer   = "unsupported_version"
	RemoteCodeTokenNotAllowed  = "token_not_allowed"
	RemoteCodeAgentNotAllowed  = "agent_not_allowed"
	RemoteCodeNotUIChannel     = "not_ui_channel"
	RemoteCodeDisabled         = "remote_disabled"
	RemoteCodeNotFound         = "not_found"
	RemoteCodeSessionEnded     = "session_ended"
	RemoteCodeTooManySessions  = "too_many_sessions"
	RemoteCodeAuditUnavailable = "audit_unavailable"
)
