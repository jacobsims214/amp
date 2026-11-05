---
title: Sign in with Amazon Cognito through Dex
tags: [operations, sso, cognito, dex, oauth2-proxy, oidc, identity, sign-in]
---

# Sign in with Amazon Cognito through Dex

Use this when people sign in to AMP with an Amazon Cognito user pool, through Dex (an OIDC
connector) and oauth2-proxy. These are the six problems the first Cognito setup hit (#69) and what
fixed each one. If sign-in fails and you aren't sure why, go through them in order.

## 1. Ask Cognito for the right scopes

Set the connector's scopes to `[openid, email, profile]`. Without `email`, Cognito sends no email
address and AMP cannot create the account (see problem 6).

```yaml
connectors:
  - type: oidc
    id: cognito
    name: Cognito
    config:
      issuer: https://cognito-idp.<region>.amazonaws.com/<user-pool-id>
      scopes: [openid, email, profile]
```

## 2. Map the name claim

If people show up unnamed, Dex is reading a claim your user pool does not fill in. Dex's OIDC
connector takes the display name from `userNameKey` (it defaults to `name`). Point it at the claim
your user pool does fill in, for example `given_name`:

```yaml
    config:
      userNameKey: given_name
```

`claimMapping` is a different setting: it only remaps `preferred_username`, `email` and `groups`.

## 3. Let oauth2-proxy accept the email

Cognito marks emails as unverified unless the user pool verified them. oauth2-proxy then refuses
the sign-in. Starting oauth2-proxy with `--insecure-oidc-allow-unverified-email` only gets the
sign-in past oauth2-proxy. AMP checks the verified flag itself and still will not create the
account, so this flag is not a fix on its own: you also need problem 5. The flag name says
"insecure" on purpose; only use it when the pool is yours and only lets in people you invited.

## 4. Login loop with the cookie domain

The shipped Helm chart sets oauth2-proxy's `--cookie-domain` to `.<your domain>` (with a leading
dot) on purpose, so the session cookie is shared with the queues subdomain (`queues.<domain>`).
In one setup that leading dot made the browser reject the cookie and people were sent back to the
login page in a loop. Only if you see that loop, change the argument to the bare domain
(`example.com`). The argument is hardcoded in `templates/oauth2-proxy.yaml` with no values
setting, so you have to edit the template. Be aware that without the dot, logging in on the main
domain no longer signs you in on the queues subdomain.

## 5. Mark the email verified in Cognito

This is the required fix for the "unverified" refusal. Mark each user's email verified in Cognito.
The console can't always do this for users created by an admin, so use the CLI:

```sh
aws cognito-idp admin-update-user-attributes \
  --user-pool-id <user-pool-id> --username <username> \
  --user-attributes Name=email_verified,Value=true
```

If oauth2-proxy also refuses the sign-in, add the flag from problem 3 as well.

## 6. Read AMP's refusal message

When sign-in works but AMP will not make an account, the page says why and who can fix it:

- **"Your identity provider reports your email as unverified, so AMP did not create your account;
  ask your IdP admin to verify it."** Problem 5 (with problem 3 if oauth2-proxy also refuses).
- **"Your email's domain isn't allowed to sign up to AMP and you have no invite, so AMP did not
  create your account; ask an AMP admin to invite you."** The person needs an invite, or an admin
  has to allow their domain.
- **"Your identity provider didn't send an email address, so AMP has no account for you; ask your
  IdP admin to include the email claim, then sign in again."** Problem 1.
- **"Your sign-in uses the Local Dev identity (local-dev@amp.local), which is reserved for AMP with
  sign-in off, so AMP did not create your account; sign in with another account."** The IdP user's
  subject is `local-dev` or its email is `local-dev@amp.local`. Those belong to AMP's built-in
  local user, so use a different IdP user.

A page that needs a role you don't have (for example People & agents, which needs admin) says the
role and the account you are signed in as, or one of the reasons above if you have no account.

See also `operations/where-amp-keeps-data.md` for where Dex keeps its sign-in data, and
`ui-tour.md` for what AMP runs as when sign-in is off.
