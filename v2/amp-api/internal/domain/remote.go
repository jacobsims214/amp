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

// RemoteMaxLifetime is how long a session lives after `/remote` (protocol 10).
const RemoteMaxLifetime = 12 * time.Hour

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
	ExpiresAt       time.Time  `json:"expires_at"`
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
type RemotePolicy struct {
	Enabled        bool `json:"enabled"`
	IdleTimeoutMin int  `json:"idle_timeout_min"`
	DevicePinDays  int  `json:"device_pin_days"`
}

// Defaults and bounds of RemotePolicy.
const (
	RemoteIdleTimeoutDefault = 30
	RemoteIdleTimeoutMin     = 5
	RemoteIdleTimeoutMax     = 240
	RemoteDevicePinDefault   = 30
	RemoteDevicePinMax       = 30
)

// DefaultRemotePolicy is every org's policy until an admin changes it: off.
func DefaultRemotePolicy() RemotePolicy {
	return RemotePolicy{Enabled: false, IdleTimeoutMin: RemoteIdleTimeoutDefault, DevicePinDays: RemoteDevicePinDefault}
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
