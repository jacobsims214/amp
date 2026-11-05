package service

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/repository"
	"github.com/simstech/amp-api/internal/secretbox"
)

// ---- Project GitHub credentials (AMP #3154, epic 183 story 453) ----
//
// A project can attach one GitHub API token per host. The repository-facts
// reader (#3151) authenticates with it, which is what makes a private
// repository's default branch and rules readable at all, and what decides
// how much of a repository a project can see (044: facts are a function of
// the credential that read them).
//
// Three rules this file enforces, each with a test:
//
//  1. PROJECT OWNERS ONLY, for every operation. Attaching a token is handing
//     AMP a credential to somebody else's system, and deleting one takes away
//     what the project can read; both are owner-level, like the GitHub merge
//     webhook secret (#2479). A project member or lead is refused.
//  2. THE TOKEN IS WRITE-ONLY. It goes in, sealed, and never comes back out
//     through anything here: reads return metadata (host, label, when), and
//     there is no "show me the token" call. That is also why nothing in this
//     file puts the token in an audit entry, an error, or a summary — the
//     audit records THAT it was attached or rotated, never what it was.
//  3. NO PLAINTEXT, EVER. With no encryption key configured, attaching or
//     rotating is refused (ErrSecretsKeyMissing, surfaced as a 400 that names
//     the env var) instead of storing the token unencrypted.
//
// A rotation replaces the token in place, on the same row: the facts the
// project already holds stay its own view (044's rotated_at comment), and a
// replacement is distinguishable from a first attach by rotated_at.

// ErrGitHubTokenRequired is returned when an attach or rotate is asked for
// without a token.
var ErrGitHubTokenRequired = errors.New("a GitHub token is required")

// githubTokenShape is deliberately loose: GitHub's own tokens are ghp_/
// gho_/ghu_/ghs_/ghr_ prefixed, but GitHub Apps and enterprise installs
// issue others, and guessing at their format would refuse a valid token.
// What is rejected is what can't be a token value at all: whitespace and
// control characters (an accidentally pasted "Authorization: Bearer x", a
// token with a trailing newline), and the characters that only appear in a
// URL or a header line ("/" and ":") — a pasted settings URL is the common
// mistake, and it is refused here rather than stored and used as a token.
// It is a sanity check, not validation: only GitHub can say whether a token
// works, and AMP never calls GitHub from this path.
var githubTokenShape = regexp.MustCompile(`^[A-Za-z0-9_.\-]{8,512}$`)

// checkGitHubToken rejects a value that can't be a token, without ever
// echoing it: the message describes the shape, never the input.
func checkGitHubToken(token string) error {
	switch {
	case token == "":
		return ErrGitHubTokenRequired
	case !githubTokenShape.MatchString(token):
		return authz.BadRequest("a GitHub token is 8 to 512 characters of letters, digits, '_', '-' or '.': " +
			"paste the token itself, not a whole header line or a URL")
	}
	return nil
}

// GitHubCredential is projectID's credential state: whether one is attached
// for host, its metadata (never the token), and whether this server can seal
// a new one (project owners).
func (s *Service) GitHubCredential(ctx context.Context, p authz.Principal, projectID int, host string) (*domain.GitHubCredentialInfo, error) {
	if _, err := s.Authz.RequireProject(ctx, p, projectID, authz.Owner); err != nil {
		return nil, err
	}
	return s.githubCredentialInfo(ctx, projectID, host)
}

// githubCredentialInfo reads a project's credential state without an
// authorization check, for callers that have already made one.
func (s *Service) githubCredentialInfo(ctx context.Context, projectID int, host string) (*domain.GitHubCredentialInfo, error) {
	info := &domain.GitHubCredentialInfo{Host: credentialHost(host), Encrypted: s.Repo.CanSealSecrets()}
	c, err := s.Repo.GetGitHubCredential(ctx, projectID, host)
	if errors.Is(err, repository.ErrNoGitHubCredential) {
		return info, nil
	}
	if err != nil {
		return nil, err
	}
	info.Configured, info.Credential = true, c
	return info, nil
}

// AttachGitHubCredential attaches projectID's GitHub token for host, or
// replaces the one already there (a rotation), and returns the credential's
// metadata (project owners). The token is never part of the answer: it went
// in sealed and can't be read back.
func (s *Service) AttachGitHubCredential(ctx context.Context, p authz.Principal, projectID int, host, label, token string) (*domain.GitHubCredentialInfo, error) {
	if _, err := s.Authz.RequireProject(ctx, p, projectID, authz.Owner); err != nil {
		return nil, err
	}
	if err := checkGitHubToken(token); err != nil {
		return nil, err
	}
	if !s.Repo.CanSealSecrets() {
		return nil, noSecretsKeyError()
	}
	// Whether this is a first attach or a rotation decides the audit action,
	// so read the previous state first. A read failure isn't fatal: the write
	// is what matters, and the worst case is an "attach" action on a rotation.
	prev, _ := s.Repo.GetGitHubCredential(ctx, projectID, host)
	stored, err := s.Repo.SetGitHubCredential(ctx, projectID, host, label, token, p.UserRef())
	if err != nil {
		return nil, redactGitHubCredentialError(err)
	}
	action, summary := "project.github_credential.attach", "attached a GitHub credential"
	if prev != nil {
		action, summary = "project.github_credential.rotate", "rotated the GitHub credential"
	}
	e := domain.AuditEntry{Action: action, ProjectID: intRef(projectID), TargetType: "project", TargetID: idStr(projectID),
		Summary: p.Label() + " " + summary, Details: githubCredentialAuditDetails(stored),
		After: githubCredentialAuditDetails(stored)}
	if prev != nil {
		e.Before = githubCredentialAuditDetails(prev)
	}
	s.audit(ctx, p, e)
	info, err := s.githubCredentialInfo(ctx, projectID, host)
	if err != nil {
		return nil, err
	}
	return info, nil
}

// RotateGitHubCredential replaces projectID's GitHub token for host with
// token. It is AttachGitHubCredential on an existing credential and is
// refused when there is none: "rotate" says a token is being replaced, and a
// rotation of nothing is a mistake worth reporting rather than a quiet
// attach (project owners).
func (s *Service) RotateGitHubCredential(ctx context.Context, p authz.Principal, projectID int, host, label, token string) (*domain.GitHubCredentialInfo, error) {
	if _, err := s.Authz.RequireProject(ctx, p, projectID, authz.Owner); err != nil {
		return nil, err
	}
	if _, err := s.Repo.GetGitHubCredential(ctx, projectID, host); err != nil {
		if errors.Is(err, repository.ErrNoGitHubCredential) {
			return nil, authz.NotFound("this project has no GitHub credential to rotate: attach one instead")
		}
		return nil, err
	}
	return s.AttachGitHubCredential(ctx, p, projectID, host, label, token)
}

// DeleteGitHubCredential removes projectID's GitHub credential for host and,
// with it, every repository fact it produced (the schema cascades), so no
// token-derived visibility outlives the token. It reports whether there was
// one (project owners).
func (s *Service) DeleteGitHubCredential(ctx context.Context, p authz.Principal, projectID int, host string) (bool, error) {
	if _, err := s.Authz.RequireProject(ctx, p, projectID, authz.Owner); err != nil {
		return false, err
	}
	had, err := s.Repo.DeleteGitHubCredential(ctx, projectID, host)
	if err != nil || !had {
		return had, err
	}
	s.audit(ctx, p, domain.AuditEntry{Action: "project.github_credential.delete", ProjectID: intRef(projectID),
		TargetType: "project", TargetID: idStr(projectID), Summary: p.Label() + " deleted the GitHub credential",
		Details: map[string]any{"host": credentialHost(host)}})
	return true, nil
}

// githubCredentialAuditDetails is what the audit log records about a
// credential: which host it is for, its label, and its lifecycle. The token
// is not here and never will be — the shape of this map is the guarantee, and
// TestGitHubCredential_AuditNeverHoldsTheToken holds it to that.
func githubCredentialAuditDetails(c *domain.GitHubCredential) map[string]any {
	if c == nil {
		return nil
	}
	d := map[string]any{"host": c.Host, "credential_id": c.ID}
	if c.Label != "" {
		d["label"] = c.Label
	}
	return d
}

// credentialHost defaults an empty host to the standard one, so "" and
// "github.com" are the same credential.
func credentialHost(host string) string {
	if h := strings.TrimSpace(host); h != "" {
		return h
	}
	return repository.GitHubCredentialHost
}

// noSecretsKeyError refuses an attach or rotate on a server with no
// encryption key. It names the setting an admin sets, and never the token.
func noSecretsKeyError() error {
	return authz.BadRequest("a GitHub token is stored encrypted, and this server has no encryption key: "+
		"an admin sets one (Helm ampApi.secretsEncryption.existingSecret, env %s) before a credential can be attached", secretbox.EnvKey)
}

// redactGitHubCredentialError makes sure a storage failure can't carry the
// token into an answer or a log. The repository layer's errors describe the
// column and the constraint, never the value, but this path is the one place
// the token and a database error meet, so it is also where that is enforced
// rather than assumed.
func redactGitHubCredentialError(err error) error {
	if errors.Is(err, repository.ErrSecretsKeyMissing) {
		return noSecretsKeyError()
	}
	if err == nil {
		return nil
	}
	// Keep the sentinel identity where callers may test for it, and keep the
	// message to what the storage layer said: no wrapped driver detail, no
	// bound parameter.
	var ae *authz.Error
	if errors.As(err, &ae) {
		return ae
	}
	return fmt.Errorf("storing the GitHub credential failed: %w", errors.New("the database refused the write"))
}
