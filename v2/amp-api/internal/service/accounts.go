package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"strings"
	"time"

	"github.com/simstech/amp-api/internal/auth"
	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
)

// Accounts: agent accounts, deactivation, and API tokens.
//
//   - Anyone signed in can create agent accounts. The creator manages them:
//     they can put an agent on teams they belong to, mint and revoke its
//     tokens, and deactivate it. Global admins can do all of this for any
//     account. Agents themselves can't create agents or tokens, and are
//     never global admins.
//   - People can mint tokens for themselves, to run a headless agent under
//     their own name.
//   - Deactivating an account refuses all its requests (sign-ins and
//     tokens alike), revokes its API tokens (a reactivated account needs
//     new ones), and stops it being given work; its tickets, comments,
//     and history stay as they are. Global admins deactivate anyone but
//     themselves; managers deactivate their agents.

// AgentDomain is the email domain of agent accounts. Nobody signs in with
// one; it keeps agent emails unique and recognisable.
const AgentDomain = "agents.amp"

// MaxTokenDays caps a token's lifetime when one is given.
const MaxTokenDays = 3650

// CreateAgent adds an agent account managed by the caller, optionally on
// some of the caller's teams.
func (s *Service) CreateAgent(ctx context.Context, p authz.Principal, name string, teamIDs []int) (*domain.User, error) {
	if !p.HasUser() {
		return nil, authz.BadRequest("agent accounts need a signed-in AMP user to manage them")
	}
	if p.Agent {
		return nil, authz.Forbidden("agent accounts can't create agent accounts")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, authz.BadRequest("name is required")
	}
	slug := Slugify(name)
	if slug == "" {
		slug = "agent"
	}
	// Check every team before creating anything.
	for _, id := range teamIDs {
		if err := s.requireAgentTeam(ctx, p, id); err != nil {
			return nil, err
		}
	}
	suffix := make([]byte, 3)
	if _, err := rand.Read(suffix); err != nil {
		return nil, err
	}
	tag := hex.EncodeToString(suffix)
	email := slug + "-" + tag + "@" + AgentDomain
	u, err := s.Repo.CreateAgentUser(ctx, "agent:"+slug+"-"+tag, email, name, p.UserRef())
	if err != nil {
		return nil, err
	}
	s.audit(ctx, p, domain.AuditEntry{Action: "agent.create", TargetType: "user", TargetID: idStr(u.ID),
		Summary: "created agent account " + email, Details: map[string]any{"name": name, "teams": teamIDs}})
	for _, id := range teamIDs {
		if err := s.Repo.SetTeamMember(ctx, id, u.ID, domain.TeamRoleMember); err != nil {
			return nil, err
		}
		if team, err := s.Repo.GetTeam(ctx, id); err == nil {
			s.audit(ctx, p, domain.AuditEntry{Action: "team.member.set", OrgID: intRef(team.OrgID), TargetType: "team", TargetID: idStr(id),
				Summary: "added agent " + email + " to team " + team.Name, Details: map[string]any{"user_id": u.ID, "role": domain.TeamRoleMember}})
		}
	}
	return u, nil
}

// requireAgentTeam checks the caller may put their agent on a team: they're
// on it, or they could add anyone to it.
func (s *Service) requireAgentTeam(ctx context.Context, p authz.Principal, teamID int) error {
	team, _, err := s.loadTeam(ctx, p, teamID)
	if err != nil {
		return err
	}
	role, err := s.Repo.GetTeamRole(ctx, teamID, p.UserID)
	if err != nil {
		return err
	}
	if role != "" {
		return nil
	}
	if err := s.Authz.RequireTeamManager(ctx, p, team); err != nil {
		return authz.Forbidden("team %d: you can only put your agents on teams you belong to", teamID)
	}
	return nil
}

// ListAgents returns the caller's agent accounts (global admins: all).
func (s *Service) ListAgents(ctx context.Context, p authz.Principal) ([]domain.User, error) {
	if p.Super {
		return s.Repo.ListAgentUsers(ctx, nil)
	}
	if !p.HasUser() {
		return []domain.User{}, nil
	}
	return s.Repo.ListAgentUsers(ctx, p.UserRef())
}

// accountFor loads a user the caller manages: themselves (when self is
// allowed), an agent they manage, or anyone for a global admin.
func (s *Service) accountFor(ctx context.Context, p authz.Principal, userID int, self bool) (*domain.User, error) {
	u, err := s.Repo.GetUserByID(ctx, userID)
	if err != nil {
		return nil, authz.NotFound("user %d not found", userID)
	}
	switch {
	case p.Super:
		return u, nil
	case self && p.HasUser() && u.ID == p.UserID:
		return u, nil
	case u.IsAgent() && u.ManagedBy != nil && p.HasUser() && *u.ManagedBy == p.UserID:
		return u, nil
	}
	if u.IsAgent() {
		return nil, authz.Forbidden("agent %d is managed by someone else; they or a global admin can do that", userID)
	}
	return nil, authz.Forbidden("only global admins can do that to another person's account")
}

// SetUserDisabled deactivates or reactivates an account.
func (s *Service) SetUserDisabled(ctx context.Context, p authz.Principal, userID int, disabled bool) (*domain.User, error) {
	if p.Agent {
		return nil, authz.Forbidden("agent accounts can't deactivate accounts")
	}
	if disabled && p.HasUser() && userID == p.UserID {
		return nil, authz.BadRequest("you can't deactivate your own account")
	}
	u, err := s.accountFor(ctx, p, userID, false)
	if err != nil {
		return nil, err
	}
	revoked, err := s.Repo.SetUserDisabled(ctx, userID, disabled)
	if err != nil {
		return nil, err
	}
	action, verb := "user.reactivate", "reactivated "
	if disabled {
		action, verb = "user.deactivate", "deactivated "
	}
	s.audit(ctx, p, domain.AuditEntry{Action: action, TargetType: "user", TargetID: idStr(userID), Summary: verb + u.Email})
	if disabled {
		// A deactivated person's remote sessions end now (#2390), which also
		// voids their connect tokens; the relay is told to close the streams.
		ended, err := s.Repo.EndOwnerRemoteSessions(ctx, userID, domain.RemoteEndRevoked)
		if err != nil {
			slog.Warn("ending a deactivated account's remote sessions failed", "user_id", userID, "err", err)
		} else {
			s.remoteEnded(ctx, p, ended, domain.AuditRemoteDisable)
		}
	}
	// The account's tokens were revoked with it, so a reactivated account
	// needs new ones. Each is audited like a normal revoke.
	for _, tok := range revoked {
		s.audit(ctx, p, domain.AuditEntry{Action: "token.revoke", TargetType: "user", TargetID: idStr(userID),
			Summary: "revoked API token " + tok.Prefix + "… (" + tok.Name + ") of " + u.Email + ": account " + strings.TrimSpace(verb),
			Details: map[string]any{"token_id": tok.ID, "reason": action}})
	}
	return s.Repo.GetUserByID(ctx, userID)
}

// SetUserRole grants or revokes a global role (global admins only).
func (s *Service) SetUserRole(ctx context.Context, p authz.Principal, userID int, role string, grant bool) error {
	if !p.Super {
		return authz.Forbidden("only global admins can change global roles")
	}
	u, err := s.Repo.GetUserByID(ctx, userID)
	if err != nil {
		return authz.NotFound("user %d not found", userID)
	}
	if grant && role == domain.RoleAdmin && u.IsAgent() {
		return authz.BadRequest("agent accounts can't be global admins")
	}
	if err := s.Repo.SetUserRole(ctx, userID, role, grant); err != nil {
		return err
	}
	verb := "revoked "
	if grant {
		verb = "granted "
	}
	s.audit(ctx, p, domain.AuditEntry{Action: "user.role", TargetType: "user", TargetID: idStr(userID),
		Summary: verb + role + " for " + u.Email, Details: map[string]any{"role": role, "grant": grant}})
	return nil
}

// DeleteUser removes an AMP user row (global admins only).
func (s *Service) DeleteUser(ctx context.Context, p authz.Principal, userID int) error {
	if !p.Super {
		return authz.Forbidden("only global admins can delete users")
	}
	u, err := s.Repo.GetUserByID(ctx, userID)
	if err != nil {
		return authz.NotFound("user %d not found", userID)
	}
	if err := s.Repo.DeleteUser(ctx, userID); err != nil {
		return err
	}
	s.audit(ctx, p, domain.AuditEntry{Action: "user.delete", TargetType: "user", TargetID: idStr(userID), Summary: "deleted user " + u.Email})
	return nil
}

// NewToken is a freshly minted API token; Token is shown only this once.
type NewToken struct {
	domain.APIToken
	Token string `json:"token"`
}

// CreateToken mints an API token for userID (the caller, or an agent they
// manage). expiresInDays 0 means it doesn't expire.
func (s *Service) CreateToken(ctx context.Context, p authz.Principal, userID int, name string, expiresInDays int) (*NewToken, error) {
	if p.Agent {
		return nil, authz.Forbidden("agent accounts can't create API tokens; the person who manages the agent can")
	}
	u, err := s.accountFor(ctx, p, userID, true)
	if err != nil {
		return nil, err
	}
	if u.DisabledAt != nil {
		return nil, authz.BadRequest("user %d is deactivated; reactivate it first", userID)
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, authz.BadRequest("name is required (say where the token will be used)")
	}
	if expiresInDays < 0 || expiresInDays > MaxTokenDays {
		return nil, authz.BadRequest("expires_in_days must be between 0 (never) and %d", MaxTokenDays)
	}
	var expires *time.Time
	if expiresInDays > 0 {
		t := time.Now().Add(time.Duration(expiresInDays) * 24 * time.Hour)
		expires = &t
	}
	raw, hash, prefix, err := auth.NewAPIToken()
	if err != nil {
		return nil, err
	}
	tok, err := s.Repo.CreateAPIToken(ctx, userID, name, hash, prefix, p.UserRef(), expires)
	if err != nil {
		return nil, err
	}
	s.audit(ctx, p, domain.AuditEntry{Action: "token.create", TargetType: "user", TargetID: idStr(userID),
		Summary: "created API token " + prefix + "… (" + name + ") for " + u.Email,
		Details: map[string]any{"token_id": tok.ID, "expires_at": expires}})
	return &NewToken{APIToken: *tok, Token: raw}, nil
}

// ListTokens returns userID's tokens (never the tokens themselves).
func (s *Service) ListTokens(ctx context.Context, p authz.Principal, userID int) ([]domain.APIToken, error) {
	if _, err := s.accountFor(ctx, p, userID, true); err != nil {
		return nil, err
	}
	return s.Repo.ListAPITokens(ctx, userID)
}

// RevokeToken revokes a token. Agents may revoke their own (for a leaked
// token, the faster the better).
func (s *Service) RevokeToken(ctx context.Context, p authz.Principal, tokenID int) error {
	tok, err := s.Repo.GetAPIToken(ctx, tokenID)
	if err != nil {
		return authz.NotFound("token %d not found", tokenID)
	}
	u, err := s.accountFor(ctx, p, tok.UserID, true)
	if err != nil {
		return authz.NotFound("token %d not found", tokenID)
	}
	if err := s.Repo.RevokeAPIToken(ctx, tokenID); err != nil {
		return err
	}
	s.audit(ctx, p, domain.AuditEntry{Action: "token.revoke", TargetType: "user", TargetID: idStr(tok.UserID),
		Summary: "revoked API token " + tok.Prefix + "… (" + tok.Name + ") of " + u.Email, Details: map[string]any{"token_id": tokenID}})
	return nil
}
