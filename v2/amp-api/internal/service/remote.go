package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/repository"
)

// Remote control of a harness session from the web app (epic 165): the
// policy (instance switch, org setting), the capability flag, the session
// registry and device records, and the audit trail. The relay routes
// (protocol.md section 8) sit on top of these.
//
// The audit trail is metadata only (decision D3): details carry ids, sizes
// and hashes, never prompt text, tool output or secrets. remote.enable and
// remote.pair fail closed: when their audit entry can't be written, the
// action is refused (503 audit_unavailable).

// RemoteConfig is the instance-level remote control configuration.
type RemoteConfig struct {
	// InstanceEnabled is the instance switch: Helm relay.enabled, env
	// RELAY_ENABLED. Off by default.
	InstanceEnabled bool
	// AuthEnabled is false for an auth-less server (compose, local dev),
	// where remote control is never available.
	AuthEnabled bool
}

// RemoteAudit is one remote control audit entry's metadata. Only these
// fields reach the audit log's details, so content can't leak into it.
type RemoteAudit struct {
	Action string // a domain.AuditRemote* action
	// Op is the protocol operation or endpoint ("prompt", "stop", "register").
	Op       string
	DeviceID string
	// KeyFP is the fingerprint of the key involved; empty means the
	// device's (DeviceID), or the harness's when no device is involved.
	KeyFP string
	// Reason is an end reason (domain.RemoteEnd*).
	Reason string
	// Code is a refusal code (protocol 9).
	Code string
	// PromptLen and PromptSHA256 describe a prompt without its text; set them
	// with PromptDigest.
	PromptLen    int
	PromptSHA256 string
	// Extra is small non-content metadata (policy values, counts).
	Extra map[string]any
}

// PromptDigest is what the audit log keeps of a prompt: its length in bytes
// and the hex SHA-256 of its text. Never the text.
func PromptDigest(text string) (length int, sha string) {
	sum := sha256.Sum256([]byte(text))
	return len(text), hex.EncodeToString(sum[:])
}

// details builds the audit details map from a session and the metadata.
func (a RemoteAudit) details(sess *domain.RemoteSession) map[string]any {
	d := map[string]any{}
	for k, v := range a.Extra {
		d[k] = v
	}
	if sess != nil {
		d["session_id"] = sess.ID
		d["harness_id"] = sess.HarnessID
		d["harness_host"] = sess.Host
	}
	d["op"] = a.Op
	d["device_id"] = nilIfEmpty(a.DeviceID)
	// The key behind the action: the device's (its id is its key's
	// fingerprint), else the harness's.
	keyFP := a.KeyFP
	if keyFP == "" {
		keyFP = a.DeviceID
	}
	if keyFP == "" && sess != nil {
		keyFP = sess.HarnessID
	}
	d["key_fp"] = nilIfEmpty(keyFP)
	if a.Reason != "" {
		d["reason"] = a.Reason
	}
	if a.Code != "" {
		d["code"] = a.Code
	}
	if a.PromptSHA256 != "" {
		d["len"] = a.PromptLen
		d["sha256"] = a.PromptSHA256
	}
	return d
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ErrAuditUnavailable refuses an action whose audit entry couldn't be written.
func errAuditUnavailable(action string) error {
	return authz.Coded(503, domain.RemoteCodeAuditUnavailable,
		"%s refused: its audit entry could not be recorded; try again", action)
}

func (s *Service) remoteEntry(sess *domain.RemoteSession, a RemoteAudit) domain.AuditEntry {
	e := domain.AuditEntry{Action: a.Action, Details: a.details(sess)}
	if sess != nil {
		e.ProjectID = intRef(sess.ProjectID)
		e.TargetType, e.TargetID = "remote_session", sess.ID
		e.Summary = fmt.Sprintf("%s: session %s on %s", a.Action, sess.ID, sess.Host)
	} else {
		e.Summary = a.Action
	}
	return e
}

// RecordRemote writes a remote control audit entry, best effort (a failure is
// logged). sess may be nil for entries about no one session.
func (s *Service) RecordRemote(ctx context.Context, p authz.Principal, sess *domain.RemoteSession, a RemoteAudit) {
	s.audit(ctx, p, s.remoteEntry(sess, a))
}

// recordRemoteStrict writes a remote control audit entry and returns 503
// audit_unavailable when it can't: for the fail-closed actions.
func (s *Service) recordRemoteStrict(ctx context.Context, p authz.Principal, sess *domain.RemoteSession, a RemoteAudit) error {
	if err := s.writeAudit(ctx, p, s.remoteEntry(sess, a)); err != nil {
		return errAuditUnavailable(a.Action)
	}
	return nil
}

// ---- Capability flag and policy ----

// RemoteCapabilityFor decides /api/me's capabilities.remote_control from its
// inputs, checked in this order: the instance switch, auth, a person, an org
// with the setting on.
func RemoteCapabilityFor(cfg RemoteConfig, person bool, enabledOrgIDs []int) domain.RemoteCapability {
	c := domain.RemoteCapability{OrgIDs: []int{}}
	why := func(r string) domain.RemoteCapability { c.Reason = &r; return c }
	switch {
	case !cfg.InstanceEnabled:
		return why(domain.RemoteUnavailableInstance)
	case !cfg.AuthEnabled:
		return why(domain.RemoteUnavailableAuth)
	case !person:
		return why(domain.RemoteUnavailablePerson)
	case len(enabledOrgIDs) == 0:
		return why(domain.RemoteUnavailableNoOrg)
	}
	c.Available = true
	c.OrgIDs = enabledOrgIDs
	return c
}

// remotePerson reports whether p may use remote control at all: a human AMP
// user signed in through Dex (not an agent account, not an API token, not
// the auth-less local-dev user).
func remotePerson(p authz.Principal) bool {
	return p.HasUser() && !p.Agent && !p.ViaToken
}

// RemoteCapability is the caller's capabilities.remote_control.
func (s *Service) RemoteCapability(ctx context.Context, p authz.Principal) (domain.RemoteCapability, error) {
	cfg := s.Remote
	if !p.HasUser() && p.Super { // the local-dev user: auth is off
		cfg.AuthEnabled = false
	}
	person := remotePerson(p)
	var orgs []int
	if cfg.InstanceEnabled && cfg.AuthEnabled && person {
		var err error
		if orgs, err = s.Repo.RemoteEnabledOrgIDs(ctx, p.UserID); err != nil {
			return domain.RemoteCapability{}, err
		}
	}
	return RemoteCapabilityFor(cfg, person, orgs), nil
}

// GetOrgRemotePolicy is an org's remote control setting, for its admins.
func (s *Service) GetOrgRemotePolicy(ctx context.Context, p authz.Principal, orgID int) (domain.RemotePolicy, error) {
	if _, err := s.Authz.RequireOrgAdmin(ctx, p, orgID); err != nil {
		return domain.RemotePolicy{}, err
	}
	return s.Repo.GetOrgRemotePolicy(ctx, orgID)
}

// ValidateRemotePolicy checks a policy's values are in range.
func ValidateRemotePolicy(pol domain.RemotePolicy) error {
	if pol.IdleTimeoutMin < domain.RemoteIdleTimeoutMin || pol.IdleTimeoutMin > domain.RemoteIdleTimeoutMax {
		return authz.Coded(400, domain.RemoteCodeMalformed, "idle_timeout_min must be %d..%d",
			domain.RemoteIdleTimeoutMin, domain.RemoteIdleTimeoutMax)
	}
	if pol.DevicePinDays < 0 || pol.DevicePinDays > domain.RemoteDevicePinMax {
		return authz.Coded(400, domain.RemoteCodeMalformed, "device_pin_days must be 0..%d", domain.RemoteDevicePinMax)
	}
	return nil
}

// SetOrgRemotePolicy changes an org's remote control setting: org owners and
// admins (or global admins), from the web UI only. Turning it off ends every
// live session on the org's projects (reason policy_off). Audited as
// remote.policy.set.
func (s *Service) SetOrgRemotePolicy(ctx context.Context, p authz.Principal, orgID int, pol domain.RemotePolicy) (domain.RemotePolicy, error) {
	if !p.InUI() {
		return domain.RemotePolicy{}, authz.Coded(403, domain.RemoteCodeNotUIChannel,
			"the remote control setting is changed by a person in the AMP web UI (org settings), not by an agent or over MCP")
	}
	if _, err := s.Authz.RequireOrgAdmin(ctx, p, orgID); err != nil {
		return domain.RemotePolicy{}, err
	}
	if err := ValidateRemotePolicy(pol); err != nil {
		return domain.RemotePolicy{}, err
	}
	before, err := s.Repo.GetOrgRemotePolicy(ctx, orgID)
	if err != nil {
		return domain.RemotePolicy{}, err
	}
	if err := s.Repo.SetOrgRemotePolicy(ctx, orgID, pol); err != nil {
		return domain.RemotePolicy{}, err
	}
	s.audit(ctx, p, domain.AuditEntry{
		OrgID: intRef(orgID), Action: domain.AuditRemotePolicySet, TargetType: "org", TargetID: idStr(orgID),
		Summary: fmt.Sprintf("set remote control for org %s: enabled=%v", s.orgLabel(ctx, orgID), pol.Enabled),
		Details: map[string]any{
			"enabled": pol.Enabled, "idle_timeout_min": pol.IdleTimeoutMin, "device_pin_days": pol.DevicePinDays,
			"was_enabled": before.Enabled,
		},
	})
	if before.Enabled && !pol.Enabled {
		ended, err := s.Repo.EndOrgRemoteSessions(ctx, orgID, domain.RemoteEndPolicyOff)
		if err != nil {
			return pol, err
		}
		s.remoteEnded(ctx, p, ended, domain.AuditRemoteDisable)
	}
	return pol, nil
}

// remoteEnded audits sessions that just ended and tells the relay, so it
// closes their streams.
func (s *Service) remoteEnded(ctx context.Context, p authz.Principal, ended []domain.RemoteSession, action string) {
	for i := range ended {
		sess := &ended[i]
		reason := ""
		if sess.EndReason != nil {
			reason = *sess.EndReason
		}
		s.RecordRemote(ctx, p, sess, RemoteAudit{Action: action, Op: "end", Reason: reason})
	}
	if len(ended) > 0 && s.OnRemoteEnded != nil {
		s.OnRemoteEnded(ended)
	}
}

// ---- Sessions ----

// RemoteRegistration is what a harness sends to turn remote control on
// (protocol 8.2, POST /relay/v1/sessions).
type RemoteRegistration struct {
	ProjectID       int
	HarnessID       string
	HarnessPub      string
	Host            string
	OS              string
	HarnessVersion  string
	ProtocolVersion int
	// Node is the replica taking the session (api_nodes id), if known.
	Node string
}

// RemoteFingerprint is a public key's fingerprint: b64u (no padding) of the
// SHA-256 of its raw bytes. pub is the b64u raw key; it must be an
// uncompressed P-256 point (65 bytes starting 0x04).
func RemoteFingerprint(pub string) (string, error) {
	raw, err := base64.RawURLEncoding.Strict().DecodeString(pub)
	if err != nil || len(raw) != 65 || raw[0] != 0x04 {
		return "", authz.Coded(400, domain.RemoteCodeMalformed, "public key: not a b64u raw P-256 point")
	}
	sum := sha256.Sum256(raw)
	return base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

// newRemoteSessionID is "rs_" plus 128 random bits in b64u.
func newRemoteSessionID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "rs_" + base64.RawURLEncoding.EncodeToString(b), nil
}

// requireRemotePerson refuses callers that can't use remote control.
func (s *Service) requireRemotePerson(p authz.Principal) error {
	switch {
	case !s.Remote.InstanceEnabled || !s.Remote.AuthEnabled || !p.HasUser():
		return authz.Coded(404, domain.RemoteCodeNotFound, "remote control isn't available on this AMP server")
	case p.Agent:
		return authz.Coded(403, domain.RemoteCodeAgentNotAllowed, "agent accounts can't use remote control")
	case p.ViaToken:
		return authz.Coded(403, domain.RemoteCodeTokenNotAllowed, "API tokens can't use remote control; sign in with `amp-harness login`")
	}
	return nil
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	for len(s) > n {
		_, size := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-size]
	}
	return s
}

// EnableRemoteSession registers a harness session (`/remote` on the laptop):
// a person, on a project they can see, whose org has remote control on, with
// fewer than domain.RemoteMaxLiveSessions live sessions. remote.enable fails
// closed: if its audit entry can't be written, the session is removed again
// and the call fails with 503 audit_unavailable.
func (s *Service) EnableRemoteSession(ctx context.Context, p authz.Principal, reg RemoteRegistration) (*domain.RemoteSession, error) {
	if err := s.requireRemotePerson(p); err != nil {
		return nil, err
	}
	fp, err := RemoteFingerprint(reg.HarnessPub)
	if err != nil {
		return nil, err
	}
	if fp != reg.HarnessID {
		return nil, authz.Coded(400, domain.RemoteCodeMalformed, "harness_id is not the fingerprint of harness_pub")
	}
	if _, err := s.Authz.RequireProject(ctx, p, reg.ProjectID, authz.Member); err != nil {
		return nil, authz.Coded(404, domain.RemoteCodeNotFound, "project %d not found", reg.ProjectID)
	}
	pol, _, err := s.Repo.ProjectRemotePolicy(ctx, reg.ProjectID)
	if err != nil {
		return nil, err
	}
	if !pol.Enabled {
		return nil, authz.Coded(403, domain.RemoteCodeDisabled,
			"remote control is off for this project's org; an org admin turns it on in org settings")
	}
	n, err := s.Repo.CountActiveRemoteSessions(ctx, p.UserID)
	if err != nil {
		return nil, err
	}
	if n >= domain.RemoteMaxLiveSessions {
		return nil, authz.Coded(429, domain.RemoteCodeTooManySessions,
			"you already have %d live remote sessions; end one first", n)
	}
	id, err := newRemoteSessionID()
	if err != nil {
		return nil, err
	}
	sess := domain.RemoteSession{
		ID: id, OwnerUserID: p.UserID, ProjectID: reg.ProjectID,
		Host: clip(reg.Host, 255), OS: clip(reg.OS, 64), HarnessVersion: clip(reg.HarnessVersion, 64),
		HarnessID: reg.HarnessID, HarnessPub: reg.HarnessPub, ProtocolVersion: reg.ProtocolVersion,
		ExpiresAt: time.Now().Add(domain.RemoteMaxLifetime),
	}
	if reg.Node != "" {
		sess.HolderNode = &reg.Node
	}
	created, err := s.Repo.CreateRemoteSession(ctx, sess)
	if err != nil {
		return nil, err
	}
	if err := s.recordRemoteStrict(ctx, p, created, RemoteAudit{Action: domain.AuditRemoteEnable, Op: "register"}); err != nil {
		if derr := s.Repo.DeleteRemoteSession(context.WithoutCancel(ctx), created.ID); derr != nil {
			slog.Error("removing a remote session whose audit failed", "session", created.ID, "err", derr)
		}
		return nil, err
	}
	return created, nil
}

// OwnRemoteSession returns session id if p owns it. Someone else's session
// and a missing one are the same 404 not_found, so existence never leaks;
// an ended one of p's own is 410 session_ended.
func (s *Service) OwnRemoteSession(ctx context.Context, p authz.Principal, id string) (*domain.RemoteSession, error) {
	sess, err := s.Repo.GetRemoteSession(ctx, id)
	if errors.Is(err, repository.ErrNotFound) || (err == nil && (!p.HasUser() || sess.OwnerUserID != p.UserID)) {
		return nil, authz.Coded(404, domain.RemoteCodeNotFound, "remote session not found")
	}
	if err != nil {
		return nil, err
	}
	if !sess.Active() {
		return sess, authz.Coded(410, domain.RemoteCodeSessionEnded, "remote session has ended")
	}
	return sess, nil
}

// RecordRemotePair audits a pair request being forwarded to the harness. It
// fails closed: when the entry can't be written the request must not be
// forwarded (503 audit_unavailable).
func (s *Service) RecordRemotePair(ctx context.Context, p authz.Principal, sess *domain.RemoteSession, deviceID string) error {
	return s.recordRemoteStrict(ctx, p, sess, RemoteAudit{Action: domain.AuditRemotePair, Op: "pair", DeviceID: deviceID})
}

// PinRemoteDevice records a device the harness accepted (a signed
// pair_result), trusted until now + the org's device_pin_days; 0 days means
// until the session ends.
func (s *Service) PinRemoteDevice(ctx context.Context, sess *domain.RemoteSession, deviceID, devicePub, name string) (*domain.RemoteDevice, error) {
	fp, err := RemoteFingerprint(devicePub)
	if err != nil {
		return nil, err
	}
	if fp != deviceID {
		return nil, authz.Coded(400, domain.RemoteCodeMalformed, "device_id is not the fingerprint of device_pub")
	}
	pol, _, err := s.Repo.ProjectRemotePolicy(ctx, sess.ProjectID)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	until := sess.ExpiresAt
	if pol.DevicePinDays > 0 {
		until = now.Add(time.Duration(pol.DevicePinDays) * 24 * time.Hour)
	}
	return s.Repo.UpsertRemoteDevice(ctx, domain.RemoteDevice{
		OwnerUserID: sess.OwnerUserID, HarnessID: sess.HarnessID, DeviceID: deviceID, DevicePub: devicePub,
		Name: clip(name, 64), PairedAt: now, ExpiresAt: until,
	})
}

// EndRemoteSession ends one of p's live sessions with reason, audited as
// remote.disable (remote.expire for idle and expired). Ending an ended
// session is a no-op.
func (s *Service) EndRemoteSession(ctx context.Context, p authz.Principal, id, reason string) (*domain.RemoteSession, error) {
	sess, err := s.OwnRemoteSession(ctx, p, id)
	if authz.CodeOf(err) == domain.RemoteCodeSessionEnded {
		return sess, nil
	}
	if err != nil {
		return nil, err
	}
	ended, err := s.Repo.EndRemoteSession(ctx, sess.ID, reason)
	if err != nil || ended == nil {
		return sess, err
	}
	action := domain.AuditRemoteDisable
	if reason == domain.RemoteEndIdle || reason == domain.RemoteEndExpired {
		action = domain.AuditRemoteExpire
	}
	s.remoteEnded(ctx, p, []domain.RemoteSession{*ended}, action)
	return ended, nil
}

// RevokeAllRemote is "Revoke all my remote sessions": ends every live session
// of p (reason revoked) and revokes all of p's devices. Audited as
// remote.device.revoke and remote.disable.
func (s *Service) RevokeAllRemote(ctx context.Context, p authz.Principal) (ended []domain.RemoteSession, devices int, err error) {
	if err := s.requireRemotePerson(p); err != nil {
		return nil, 0, err
	}
	if devices, err = s.Repo.RevokeRemoteDevices(ctx, p.UserID, ""); err != nil {
		return nil, 0, err
	}
	if ended, err = s.Repo.EndOwnerRemoteSessions(ctx, p.UserID, domain.RemoteEndRevoked); err != nil {
		return nil, devices, err
	}
	s.RecordRemote(ctx, p, nil, RemoteAudit{Action: domain.AuditRemoteDeviceRevoke, Op: "revoke_all",
		Extra: map[string]any{"devices": devices, "sessions": len(ended)}})
	s.remoteEnded(ctx, p, ended, domain.AuditRemoteDisable)
	return ended, devices, nil
}

// EndRemoteWithoutAccess ends the live sessions of owners (every live
// session when none are given) whose owner can no longer see the session's
// project, with reason revoked, audited as remote.disable, and tells the
// relay to close their streams: losing project or org access ends a session
// the way deactivation does (re-review #2394 N7). p is who caused it (the
// zero Principal for the relay's own checks).
func (s *Service) EndRemoteWithoutAccess(ctx context.Context, p authz.Principal, owners ...int) ([]domain.RemoteSession, error) {
	ended, err := s.Repo.EndRemoteSessionsWithoutAccess(ctx, domain.RemoteEndRevoked, owners...)
	if err != nil {
		return nil, err
	}
	s.remoteEnded(ctx, p, ended, domain.AuditRemoteDisable)
	return ended, nil
}

// endRemoteAfterAccessChange is EndRemoteWithoutAccess after a membership
// change, best effort: the change itself has happened, and the relay's
// sweep and the next token refresh check again.
func (s *Service) endRemoteAfterAccessChange(ctx context.Context, p authz.Principal, owners ...int) {
	if _, err := s.EndRemoteWithoutAccess(context.WithoutCancel(ctx), p, owners...); err != nil {
		slog.Warn("ending remote sessions after an access change failed", "err", err)
	}
}

// RemoteIdleGrace is how long past the idle timeout the relay waits before
// ending a session itself (the harness is authoritative; this is the backstop).
const RemoteIdleGrace = 60 * time.Second

// ExpireRemoteSessions ends sessions past their 12 h lifetime or idle limit,
// audited as remote.expire, and sessions whose owner lost access to the
// project (remote.disable, reason revoked). The relay calls it periodically.
func (s *Service) ExpireRemoteSessions(ctx context.Context, now time.Time) ([]domain.RemoteSession, error) {
	ended, err := s.Repo.EndExpiredRemoteSessions(ctx, now, RemoteIdleGrace)
	if err != nil {
		return nil, err
	}
	s.remoteEnded(ctx, authz.Principal{}, ended, domain.AuditRemoteExpire)
	// The backstop for access lost by any route (a team or org change made
	// while amp-api couldn't end the session, or a direct database edit).
	lost, err := s.EndRemoteWithoutAccess(ctx, authz.Principal{})
	if err != nil {
		return ended, err
	}
	if err := s.Repo.PruneProofNonces(ctx, now); err != nil {
		slog.Warn("pruning remote refresh-proof nonces failed", "err", err)
	}
	return append(ended, lost...), nil
}
