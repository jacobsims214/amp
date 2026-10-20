# Cognito SSO deployment blockers — rp-tools-nonprod

## Context
We are deploying the `amp` Helm chart (branch `rvoh/eks-tools-nonprod`) to the Red Platform `rp-tools-nonprod` EKS cluster via ArgoCD, and configuring **AWS Cognito** as the OIDC identity provider through **Dex**. This document lists every blocker we hit, its root cause, and its current status (chart fix / IdP action required / upstream ask).

## Auth model (how SSO is wired)
- `oauth2-proxy` fronts the app and speaks OIDC to **Dex** (Dex is the OIDC provider for the proxy).
- Dex's **local password connector** (`passwordConnector: local`, `enablePasswordDB: true`) is always enabled and is how the **bootstrap admin** logs in. The admin's password is seeded into Dex's passwordDB by the `amp-bootstrap-admin` job.
- The **Cognito connector** was an extra `oidc` connector added via `dex.connectors` (removed — see "SSO disabled" below).
- AMP reads the identity from Dex's issued token (`Authorization: Bearer <id_token>`), using the `sub`, `email`, `name`, and `email_verified` claims.

## Issues hit

### 1. Wrong Cognito User Pool ID in the issuer
- **Symptom:** Dex `Failed to get provider: 404 Not Found: {"message":"User pool us-east-1_hdqpj7ijp does not exist."}`
- **Root cause:** the issuer was built from the hosted‑UI domain prefix, not the real pool id. Pool id is case‑sensitive.
- **Fix:** issuer → `https://cognito-idp.us-east-1.amazonaws.com/us-east-1_hdQpJ7iJp`.
- **Status:** ✓ chart fixed.

### 2. `invalid_scope: groups`
- **Symptom:** Dex rejected the login with `invalid_request: invalid_scope`.
- **Root cause:** `groups` is not a valid OIDC scope for Cognito. Valid scopes are `openid`, `email`, `profile`, `phone`, `address`.
- **Fix:** connector scopes → `[openid, email, profile]`.
- **Status:** ✓ chart fixed.

### 3. Unknown flag `--skip-email-verified`
- **Symptom:** `unknown flag: --skip-email-verified` in the oauth2‑proxy logs.
- **Root cause:** wrong flag name. The real flag is `--insecure-oidc-allow-unverified-email`.
- **Fix:** wired `--insecure-oidc-allow-unverified-email={{ .Values.oauth2Proxy.insecureOidcAllowUnverifiedEmail }}` (default `false`; `true` in the EKS overlay).
- **Status:** ✓ chart fixed.

### 4. HTTP 403 CSRF on the apex host (cookie‑domain)
- **Symptom:** `CSRF cookie '_amp_oauth2_proxy_csrf' was not found` → 403 on `/oauth2/callback`; warning `request host "amp.rp-tools-nonprod.rvohealth.com" did not match any of the specific cookie domains of ".amp.rp-tools-nonprod.rvohealth.com"`.
- **Root cause:** the chart hardcoded `--cookie-domain=.{{ global.domain }}` (leading dot). oauth2‑proxy v7.7.1 does not match the apex host against a leading‑dot domain, so the CSRF/session cookie is never persisted on the apex host.
- **Fix:** removed the leading dot → `--cookie-domain={{ global.domain }}`.
- **Tradeoff:** the session cookie is no longer shared with the `queues.amp…` (asynqmon) subdomain.
- **Status:** ✓ chart fixed.

### 5. "missing email claim" — `email_verified:false` gate (app‑side)
- **Symptom:** after a successful login, AMP shows `Internal Server Error — this token has no AMP user (missing email claim); sign in again`.
- **Root cause:** `amp-api/internal/auth/auth.go` hardcodes a gate — if the token's `email_verified` claim is explicitly `false`, it refuses to provision/link a user (`ident.User` stays `nil`). Cognito issues `email_verified:false` for unverified users. There is **no Helm/env knob** to bypass it.
- **Workaround (IdP side):** mark the user's email verified:
  ```bash
  aws cognito-idp admin-update-user-attributes \
    --user-pool-id <POOL_ID> --username <EMAIL> \
    --user-attributes Name="email_verified",Value="true"
  ```
- **Upstream ask:** make the `email_verified` provisioning gate configurable / documented (or document that Cognito emails must be verified for AMP to provision the user).

### 6. "missing 'name' claim" (Dex / IdP side)
- **Symptom:** `Failed to authenticate: missing "name" claim`.
- **Root cause:** the Dex OIDC connector requires a `name` claim. Cognito only returns `name` in the token when the user pool **client's attribute mapping** includes `name` (and the `profile` scope is requested) — merely setting the `name` attribute on the user is not enough.
- **Fix options:** (a) configure the Cognito client attribute mapping to emit `name`, or (b) add a Dex `claimMappings` mapping `name` to a claim Cognito does return (e.g. `given_name`).
- **Status:** ⚠ not resolved — blocked on IdP client configuration; may benefit from upstream Dex `claimMappings` guidance.

## What we changed in the chart (`rvoh/eks-tools-nonprod`)
- Added the Cognito OIDC connector (issuer, client id/secret via the `amp-dex-connectors` Secret, redirect URI, scopes).
- Set scopes to `[openid, email, profile]`.
- Enabled `--insecure-oidc-allow-unverified-email` for oauth2‑proxy.
- Removed the leading‑dot `--cookie-domain`.
- **Disabled the Cognito connector** — `dex.connectors: []` and `dex.connectorSecret: ""`. The deployment now uses **local password (bootstrap‑admin) auth only**.

## SSO disabled
The Cognito connector was removed in commit `6913f02` (`fix(deploy): disable Cognito SSO connector (local password auth only)`). Dex still runs with the local password connector, so bootstrap‑admin login is unaffected. The now‑unused `amp-dex-connectors` Secret in the `amp` namespace can be deleted.

## Upstream asks
1. [#5] Make the `email_verified` provisioning gate configurable / documented.
2. [#6] Document Dex+Cognito `name` claim handling (client attribute mapping vs Dex `claimMappings`).
3. A tested Cognito+Dex deploy example in the repo docs would prevent the scope/flag/cookie digressions.
