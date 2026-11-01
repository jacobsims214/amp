package main

import (
	"context"
	"log/slog"

	"github.com/simstech/amp-api/internal/repository"
	"github.com/simstech/amp-api/internal/secretbox"
)

// configureSecretsKeys loads the key that encrypts webhook secrets at rest
// and brings the stored ones under it (#2441). It never fails startup and
// never logs a key, only key ids:
//   - no key: auth headers can't be added (the API says why), signing
//     secrets stay in plaintext, and existing plaintext auth headers keep
//     working, with a warning;
//   - a malformed key: treated as no key, logged as an error;
//   - stored values sealed under a key that isn't configured: their
//     deliveries fail (and retry) until the key is back, logged as an error.
func configureSecretsKeys(ctx context.Context, repo *repository.Repo) {
	keys, err := secretbox.FromEnv()
	if err != nil {
		slog.Error("webhook secrets encryption key is invalid; running without one", "err", err)
		keys = nil
	}
	repo.SetSecretsKeys(keys)
	rep, err := repo.ResealWebhookSecrets(ctx)
	if err != nil {
		slog.Error("encrypting stored webhook secrets failed", "err", err)
		return
	}
	// Projects' GitHub merge webhook secrets (AMP #2479), the same way.
	ghSealed, ghUnopenable, err := repo.ResealGitHubHookSecrets(ctx)
	if err != nil {
		slog.Error("encrypting stored GitHub webhook secrets failed", "err", err)
	}
	rep.Resealed += ghSealed
	rep.Unopenable += ghUnopenable
	switch {
	case keys.CanSeal():
		slog.Info("webhook secrets are encrypted at rest", "key_id", keys.CurrentID(), "sealed_now", rep.Resealed)
	default:
		slog.Warn("no webhook secrets encryption key ("+secretbox.EnvKey+"): new auth headers are refused and signing secrets are stored in plaintext",
			"plaintext_auth_headers", rep.PlaintextHeaders)
	}
	if rep.Unopenable > 0 {
		slog.Error("stored webhook secrets are encrypted under a key that isn't configured: those webhooks can't deliver until it is ("+
			secretbox.EnvKey+", or "+secretbox.EnvRetiredKeys+" for a retired key)", "values", rep.Unopenable)
	}
}
