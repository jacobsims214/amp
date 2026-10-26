# AMP v2 — Kubernetes / Helm Deployment Architecture

Status: in progress. This document is the working spec for moving AMP off the
single-host docker-compose stack onto Kubernetes (docker-desktop locally,
then the datacenter Talos/Proxmox kube cluster), with real auth for both the
UI and the MCP endpoint, and multi-user support.

## Why

The docker-compose stack has no durability story (local Docker Desktop has
corrupted its VM disk twice, wiping the `amp-pgdata`/`amp-tsdata` volumes) and
has zero authentication — anyone who can reach the ports has full read/write
access to every project. This doc defines a Helm chart that is portable
between a local docker-desktop kube cluster and the target datacenter
cluster, backed by a real operator-managed Postgres (CloudNativePG) instead
of a bind-mounted volume, fronted by Envoy with TLS terminated via
cert-manager, with authn/authz for humans (browser/UI) and machines
(MCP clients: opencode, Claude Code, etc).

## Components

| Component | Role | Notes |
|---|---|---|
| **CloudNativePG (CNPG) operator** | Manages the `Cluster` CR that provisions Postgres | Same operator prod EKS/Talos cluster uses. 1 instance locally, N instances (HA) in prod via values override. |
| **cert-manager** | Issues/renews TLS certs for Envoy | `ClusterIssuer` is swappable: `selfsigned` (local docker-desktop), `letsencrypt-http01`/`letsencrypt-dns01` (real certs anywhere Envoy is internet/DNS reachable). |
| **Envoy** | Single ingress point, TLS termination, routing, JWT validation for MCP bearer tokens | Terminates TLS with the cert-manager-issued cert. Routes `/` → oauth2-proxy (browser flow) → ui; `/mcp` → jwt_authn filter (validates Dex-issued JWT) → amp-api MCP port; `/mcp-oauth/*` and `/.well-known/*` → amp-mcp-oauth broker (unauthenticated, these ARE the auth endpoints); `/dex/*` → dex; `/authadmin/*` → amp-authadmin (requires `admin` group claim). |
| **Public pages** | The home page for signed-out visitors | Envoy skips oauth2-proxy for `/`, `/home/`, `/assets/` and `/favicon.svg` only. The app at `/` checks `/api/me` (still protected); signed out, it sends you to `/home/`, and deep links still go straight to sign-in. |
| **Dex** | OIDC provider / identity source of truth | CNCF project, single small binary, Postgres-backed storage (reuses the CNPG cluster, separate `dex` database). Local user store via the `local` connector (bcrypt passwords in its own storage) — no external IdP dependency. Issues signed RS256 JWTs, exposes JWKS at `/dex/keys` and OIDC discovery at `/dex/.well-known/openid-configuration`. |
| **oauth2-proxy** | Session/cookie auth for the browser UI | Sits in front of the `ui` service. Redirects unauthenticated browsers to Dex, sets an encrypted cookie session, forwards `Authorization: Bearer <id_token>` and `X-Auth-Request-*` headers upstream. |
| **amp-mcp-oauth** (new, small Go service) | Makes Dex speak "standard MCP OAuth" | MCP clients (opencode, Claude Code, etc.) expect RFC 8414 authorization-server metadata discovery + RFC 7591 Dynamic Client Registration + PKCE. Dex supports neither DCR nor per-client dynamic redirect URIs. This service: (1) serves `/.well-known/oauth-authorization-server`, (2) implements `POST /register` (DCR) by minting an ephemeral public client_id mapped to the caller's requested `redirect_uris`, (3) brokers `/authorize` → Dex's single static confidential "mcp-broker" client → mints its own opaque code tied to the original client's PKCE challenge → redirects back to the original dynamic client, (4) validates PKCE on `/token` and hands back the real Dex-issued JWTs unmodified. Envoy's jwt_authn filter validates those Dex JWTs directly — the broker never signs tokens itself. |
| **amp-authadmin** (new, small Go service) | User CRUD | Thin wrapper around Dex's gRPC admin API (`CreatePassword`/`UpdatePassword`/`DeletePassword`/`ListPasswords`), mTLS to Dex's gRPC port. Only reachable via Envoy route gated on the `admin` group JWT claim. Backs the new "Users" page in the React UI. |
| **amp-api** | Existing Go service (MCP tools + REST/SSE) | Adds JWT validation middleware (validates Dex JWKS-signed tokens on both the MCP and REST listeners) and a `users`/`user_roles` migration + `/api/me` endpoint. Trusts Envoy for edge auth but re-validates the token itself (defense in depth, per user request). |
| **ui** | Existing React app | Adds an `AuthContext` (calls `/api/me`), a login redirect handler, and a `Users` admin page calling `amp-authadmin`. |
| **typesense**, **ollama** | Unchanged | Same images, now as k8s Deployments+PVCs instead of compose volumes. Post-install Job pulls `nomic-embed-text` into Ollama (replaces `make kb-setup`). |

## Namespaces

- `amp-system` — CNPG operator, cert-manager (if not already cluster-wide).
- `amp` — everything application-specific: CNPG `Cluster`, dex, oauth2-proxy,
  envoy, amp-api, ui, amp-authadmin, amp-mcp-oauth, typesense, ollama.

## Auth flows

**Browser (UI):**
`user → Envoy(443, TLS) → oauth2-proxy (no session cookie) → 302 to Dex → login form (local connector) → Dex 302 back to oauth2-proxy /oauth2/callback → cookie set → Envoy → ui (static SPA) → ui calls /api/me with the forwarded Authorization header → amp-api validates JWT against Dex JWKS → returns identity`.

**MCP client (opencode, Claude Code, agents):**
`client discovers /.well-known/oauth-authorization-server on the MCP origin → POST /mcp-oauth/register (DCR) → client opens system browser to /mcp-oauth/authorize?...&code_challenge=... → broker exchanges with Dex using its own static client + its own PKCE → user logs into Dex (or already has a session) → broker mints opaque code → redirects to client's loopback redirect_uri → client POSTs /mcp-oauth/token with code_verifier → broker validates PKCE, returns the real Dex access/id token → client calls MCP endpoint with Authorization: Bearer <token> → Envoy jwt_authn validates signature+issuer+audience against Dex JWKS → amp-api MCP handler also validates + extracts identity for tool-call attribution/authorization`.

## TLS strategy

Envoy always terminates real, cert-manager-managed TLS — this is a stack
capability, not something bypassed even when a deployment happens to sit
behind another SSL gateway (e.g. the Proxmox datacenter gateway). Helm values
select the `ClusterIssuer`:

- `certManager.issuer=selfsigned` — local CA, used on docker-desktop. Browsers/MCP clients need to trust the local CA (documented in chart NOTES).
- `certManager.issuer=letsencrypt-http01` — real publicly-trusted cert, requires Envoy's `:80` reachable from the internet for the ACME HTTP-01 challenge.
- `certManager.issuer=letsencrypt-dns01` — real cert via DNS-01 (works even without inbound :80, needed if the Proxmox gateway is the only public entry point and only forwards 443). Requires a DNS provider API token (Helm secret).

## Data model additions (amp-api migrations)

New migration `003_users.sql`:
- `users` (id, subject (Dex `sub`), email, display_name, created_at, last_seen_at)
- `user_roles` (user_id, role) — roles: `admin`, `member`
Rows are upserted on first successful JWT validation (JIT provisioning) —
Dex/the local connector remains the source of truth for credentials, this
table is just for role assignment + activity attribution inside amp-api.

## Deployment topology differences: local vs prod

| | docker-desktop (local) | datacenter (Talos/Proxmox) |
|---|---|---|
| CNPG instances | 1 | 3 (HA, values override) |
| cert-manager issuer | selfsigned (Envoy terminates TLS itself) | none — `global.tls.enabled: false`; an external SSL gateway terminates TLS and forwards plain HTTP into the cluster |
| Envoy TLS | terminates TLS itself (`https_listener`, cert-manager cert) | plain HTTP only (`http_listener`, no cert mounted) — see `templates/envoy-configmap.yaml` / `envoy.yaml`, both branch on `global.tls.enabled` |
| Envoy reachability | Docker Desktop's built-in `kindccm` publishes the LoadBalancer Service directly on `localhost:443`/`:80` | Cilium L2Announcement/BGP hands the `LoadBalancer` Service a real routable IP on the server VLAN; the external SSL gateway targets that IP on port 80 |
| Ollama | in-cluster (CPU) | in-cluster (CPU) — same, no GPU assumed |
| Storage class | `hostpath`/docker-desktop default | `nfs-storage` (the cluster's only StorageClass — see below) |
| Images | built locally with `docker build`, loaded straight into containerd via `ctr images import` (no registry) | built + pushed to GHCR by `.github/workflows/build-images.yml`, pulled by `image.registry: ghcr.io/<owner>/amp` |
| Ingress reachability | port-forward / Docker Desktop LoadBalancer | LoadBalancer IP on the VLAN, behind the existing Proxmox SSL gateway |

Note on the internal CA: regardless of `global.tls.enabled`, `templates/cert-issuers.yaml` **always** creates the internal self-signed CA and Dex gRPC mTLS certs (`amp-ca`, `amp-dex-grpc-server`, `amp-dex-grpc-client`) — that's service-to-service security inside the cluster, unrelated to how the public-facing edge is terminated. Only the public `amp-envoy-tls` Certificate (and its Issuer/ClusterIssuer selection) is gated by `global.tls.enabled`.

## Not yet production-ready

- **CNPG backups are not configured.** `values-production.yaml` has a commented-out `barmanObjectStore` block as a starting point — needs an S3-compatible endpoint (MinIO on Proxmox, real S3, B2, etc.) before this touches real user data.
- **Typesense's API key is still the local-dev default** (`amp-local-dev`) in `values-production.yaml` — needs a real generated secret before go-live.

## Deploying to the datacenter cluster (Talos/Proxmox, GitOps via ArgoCD)

Verified by read-only exploration of the live cluster (nothing was changed):

- **cert-manager and the CloudNativePG operator are already installed cluster-wide** (`cert-manager`, `cnpg-system` namespaces) — this chart only ever creates `Issuer`/`Certificate`/`Cluster` CRs, never the operators themselves, so there's no conflict.
- **Storage**: the cluster's only StorageClass is `nfs-storage` (NFS-backed), already running several healthy 3-instance CNPG clusters (`immich-db`, `simstech-db`, `vault-postgres-cluster`) — `values-production.yaml` uses it directly, no placeholder needed.
- **Cilium L2Announcement** (`service-pool` IP pool, `service-policy`) already hands real routable VLAN IPs to `LoadBalancer` Services (`argocd-server`, `immich`, `simstech-odoo-lb`, etc.) — Envoy's Service uses the same mechanism, no extra wiring needed.
- **Deploys go through ArgoCD**, not manual `helm install`, matching the existing `simstech-odoo`/`odoo-operator` Application pattern: `repoURL` + chart `path` + a named `values-production.yaml` + a single `helm.parameters` override for the image tag. See `deploy/argocd/amp-application.yaml` for the reference manifest (deliberately **not applied** — review and `kubectl apply` it yourself when ready).
- **Image tag bumps**: every `amp-*` component falls back to a single `global.imageTag` value (empty per-component `tag:` fields mean "use it") specifically so a release is just one `helm.parameters` edit — `.github/workflows/build-images.yml` tags every build `sha-<shortsha>` (and `latest` on `main`) in `ghcr.io/jacobsims214/amp/*`.


## Open items / phase 2 (explicitly out of scope for this pass)

- amp-authadmin currently supports only the `local` Dex connector (username/password). No SSO/social connectors.
- No rate limiting / brute-force lockout on Dex login yet (candidate: Envoy rate-limit filter in front of `/dex/auth`).
- No per-project RBAC yet — all authenticated users see all projects (role gate is just `admin` vs `member` for the Users admin page). Project-level ACLs are a later iteration.

## Gotchas found during initial docker-desktop validation (fixed, documented for the datacenter rollout)

These were all real bugs caught by actually deploying and exercising the stack end-to-end (login, MCP tool calls, KB read/write) rather than just rendering the chart:

1. **`ClusterIssuer` of type `ca` looks for its secret in cert-manager's cluster-resource-namespace, not the chart's namespace.** Since this whole stack lives in one namespace anyway, all cert-manager issuers were switched from `ClusterIssuer` to plain namespaced `Issuer` — avoids the cross-namespace secret lookup entirely.
2. **Dex's own HTTP routes (including `/healthz`) are prefixed with its issuer path.** Since `issuer: https://<domain>/dex`, the health endpoint is actually `/dex/healthz`, not `/healthz`. Probes pointed at the bare path caused permanent `CrashLoopBackOff` (liveness kept killing an otherwise-healthy pod).
3. **Envoy's `prefix_rewrite: ""` is silently dropped, not applied.** Proto3 treats an explicit empty string as "unset" on the wire, so setting it to `""` to mean "strip the prefix down to nothing" does nothing — Envoy forwards the untouched original path. Fixed by matching the prefix *with* a trailing slash (`/mcp/`) and rewriting to `"/"`, which correctly collapses to a single leading slash instead of doubling it.
4. **Envoy's default per-route `timeout` is 15 seconds and applies to streaming routes too.** `stream_idle_timeout` on the HTTP connection manager is not enough — it governs idle gaps between frames, not total request duration. Both the MCP SSE route (`/mcp`) and the UI's own live-update SSE route (`/api/events`) need an explicit `timeout: 0s` on the route action or Envoy forcibly severs the connection at 15s, which silently drops the MCP session (`sync.Map` session entry gets deleted when the SSE handler's context is cancelled) and breaks subsequent `tools/call` requests with a confusing "Invalid session ID" error that has nothing to do with the session ID itself.
5. **`go-oidc`'s discovery-based provider always trusts the discovery document's self-reported `jwks_uri`, even if you fetched discovery from a different (internal) URL.** Dex's discovery document always reports its *public, configured* issuer URLs for every field, including `jwks_uri` — so amp-api ended up trying to fetch signing keys over the public hostname, which doesn't resolve inside the cluster. Fixed by bypassing `oidc.NewProvider`/discovery entirely and constructing the verifier manually via `oidc.NewRemoteKeySet(internalJWKSURL)` + `oidc.NewVerifier(publicIssuerURL, keySet, ...)`, fully decoupling "where to fetch keys from" from "what issuer string to expect in tokens."
6. **Dex's gRPC `CreatePassword` requires a non-empty `UserId`.** Omitting it fails with `no user ID supplied` — amp-authadmin now generates a UUID for every created credential.
7. **A pre-existing bug in `kb/service.go`'s Ollama reachability check**: the JSON struct for parsing `/api/tags` had a bogus doubly-nested `{"models":{"models":[...]}}` shape, but Ollama's real response is flat (`{"models":[...]}`). The mismatched shape caused `json.Decode` to error out every time, so `ollamaReachable()` **always returned false** — meaning every KB collection was silently created keyword-only regardless of whether Ollama/the embedding model was actually ready. This is why a KB collection created early (e.g. right after the post-install model-pull Job) can get permanently stuck without semantic search. Fixed the struct shape, bumped the health-check timeout from 2s→5s, and taught `Reindex` to detect a keyword-only collection, snapshot its docs, drop+recreate it with the embedding field, and rewrite the docs back in — so `amp_kb_reindex` now actually does what its tool description promises.
8. **Dex is OIDC-only and rejects any `/auth` request missing the `openid` scope.** Standards-compliant MCP OAuth clients (opencode, Claude Code, etc.) don't request it — per the MCP spec (RFC 8707 resource indicators) they only need a resource-scoped access token, not an ID token. This made every real MCP client fail on first login with `Missing required scope(s) ["openid"]`. Fixed by adding a thin `/authorize` redirect in `amp-mcpoauth` that force-injects `openid` into whatever scope the client requested before handing off to Dex's real `/auth` — Dex still does all actual PKCE/consent/token work.
9. **`mcp-go` v0.5.0 only implements the legacy 2024-11-05 "HTTP+SSE" transport (separate `GET /sse` + `POST /message` endpoints), and real-world MCP clients don't reliably support it.** OpenCode (and likely other modern clients) first probes with a POST to the base URL — the Streamable HTTP (2025-03-26+) handshake — gets a `405`, falls back to the legacy SSE transport, opens the `GET /sse` connection just long enough to read the initial "endpoint" event, then **closes it** rather than holding it open for the session's lifetime (which the legacy transport requires, since all server→client messages including tool call responses are delivered only over that SSE channel). Subsequent `POST /message` calls then fail with `Invalid session ID` because the session was already torn down when the SSE connection closed — this looks identical to bug #4 above but has a completely different root cause (transport mismatch, not a proxy timeout). Fixed by upgrading to `mcp-go` v0.48.0 and switching to `NewStreamableHTTPServer` — a single endpoint handling POST/GET/DELETE, session-correlated via the `Mcp-Session-Id` header, with the JSON-RPC response returned directly in the POST body. This also simplified `amp-api`'s own plumbing: the internal-loopback-address + reverse-proxy indirection the old SSE transport needed (to add auth middleware without forking mcp-go's baseURL-sensitive internal mux) is gone entirely — `StreamableHTTPServer` is a plain `http.Handler` that mounts directly under `verifier.Middleware`. MCP client configs should point at the bare resource URL (`https://<domain>/mcp`), not a `/sse` suffix — Envoy's existing routing already handled this correctly with zero changes needed, since the exact-path `/mcp` route already existed for RFC 9728 protected-resource-metadata purposes.
10. **A Dex replica can freeze with its Postgres pool stuck "idle in transaction" while `/dex/healthz` stays green (prod incident 2026-10-26).** Dex v2.42 refreshes a token inside a SERIALIZABLE transaction and, while that transaction holds a pooled connection, the `local` password connector re-reads the user, which needs a second connection. With Dex's default pool of 5, five concurrent refreshes on one replica deadlock the pool for good. `/dex/healthz` can't see it (its storage check has no timeout and keeps its last result) and `/dex/keys` is cached until key rotation. The chart now sets `dex.storage` (pool of 20, and `idle_in_transaction_session_timeout=60s` for Dex's connections only, through `PGOPTIONS`) and `dex.storageProbe` (an exec probe that POSTs an unknown `client_id` to `/dex/token`, which reads Postgres and must answer 401). The timeout releases the row locks the stuck sessions hold, so the other replica keeps working; the probe takes the frozen replica out of the Service and restarts it. Details: KB `discoveries/dex-frozen-replica-incident.md`.

**Sessions across replicas:** with `mcp-go` v1.x the default session ID manager only checks a session ID's format, and AMP's tools keep no per-session state, so a legacy client's `Mcp-Session-Id` works on whichever replica its next request lands on (pinned by `TestCluster_LegacyMCPSessionAcrossReplicas`). No sticky routing is needed.

## Running more than one amp-api replica

`ampApi.replicas` can be any number. The in-memory actors (`internal/actor`) hold each project's live state, so a project's actors run on exactly one replica at a time, its *owner* (`internal/cluster`):

- **Ownership** is a row in `project_owners`. The first replica to touch an unowned project claims it. Ownership lasts while the owner heartbeats into `api_nodes` (every 2s); once a heartbeat is 10s old, any other replica may claim the project and load it from Postgres.
- **Forwarding:** `Registry.Get` returns a per-project router. On the owner, it hands messages straight to the project actor. Elsewhere, it sends them (gob-encoded) to the owner's internal listener on `:3003` (`CLUSTER_ADDR`, advertised as `$(POD_IP):3003`), which replies over the same call. Callers can't tell the difference. A message is retried on another owner only when it certainly wasn't delivered (the owner was unreachable, or said it isn't the owner), so it never runs twice. Replicas authenticate to each other with a token kept in `cluster_settings`, and Envoy doesn't route to `:3003`.
- **Fencing:** a replica whose heartbeats stop succeeding for 6s stops its project actors and refuses to serve them. That is well before the 10s at which anyone else may take them, so two replicas never run one project's actors at once.
- **Live events:** each replica's hub publishes to Postgres `NOTIFY amp_live_events`. Payloads over 7KB go via the `live_events` table. Every replica delivers the events to its own SSE clients, so the board updates whichever replica a browser is connected to. Webhooks fire once, on the replica that made the change.
- **Shutdown:** on SIGTERM, `/ready` turns 503 and the pod keeps serving for `SHUTDOWN_DELAY` (5s) while it leaves the Service. It then finishes in-flight requests, stops its actors, and deletes its `api_nodes` row, releasing its projects at once. A crashed replica's projects move after the 10s TTL; requests for them wait (up to 30s) rather than fail.
- **Scheduled tickets** unblock from their actor, so every replica starts the actors of projects with a ticket coming due, every 30s. A ticket whose start time passed while no replica ran it unblocks as soon as its actor starts.
- `CLUSTER_MODE=off` restores the old behaviour (every project local, no replica listener). It is only safe with one replica.

`cmd/server/cluster_test.go` runs replicas side by side against one database, covering forwarding, a contended claim, events, takeover after a crash, fencing, handover on shutdown, and scheduled tickets. `TEST_CLUSTER=1 go test ./cmd/server` runs the whole MCP suite through a replica that forwards everything to another.

## MCP 2026-07-28 migration (dual-era transport + token audience)

AMP's MCP endpoint speaks both protocol eras on the same URL (`https://<domain>/mcp`), chosen per request by `mcp-go` v1.x:

- **Legacy clients (2025-11-25 and earlier)** open with `initialize` and get an `Mcp-Session-Id` session, exactly as before. Their responses are byte-identical to the pre-upgrade server (pinned by `cmd/server/transport_test.go`).
- **Modern clients (2026-07-28)** send per-request `_meta` plus `MCP-Protocol-Version`/`Mcp-Method`/`Mcp-Name` headers, need no handshake and no session, and can call `server/discover`. Modern requests hold no session at all.
- `tools/list` carries cache hints (`ttlMs` 5 min, `cacheScope: public`). Requests with a browser `Origin` that isn't `https://<domain>` get 403 (DNS-rebinding protection, a MUST in the spec); CLI/agent clients send no `Origin` and are unaffected.

### Token audience via Dex cross-client trust

2026-07-28 requires an MCP server to accept only tokens issued *for it*. Dex has no RFC 8707 resource indicators, so the audience is bound with Dex's cross-client trust:

1. On startup `amp-mcpoauth` creates a Dex client `amp-mcp` (`auth.mcpAudience`) through the gRPC API. It is confidential, with a random secret nobody holds and no redirect URIs, so it can't be used to log in; it only exists to be the token audience and to hold `trustedPeers`. It must be a storage client, not a `staticClients` entry, because Dex refuses API updates to static clients.
2. `/register` adds each new client to `amp-mcp`'s `trustedPeers`.
3. `/authorize` adds `openid email profile` and `audience:server:client_id:amp-mcp` to the request scope, after re-adding the client to `trustedPeers` (only for public clients that exist in Dex). If that fails, `/authorize` returns 503 instead of letting the client get a token amp-api would reject. This heals clients registered before this release, including ones with Dex-style IDs from earlier broker versions. Dex then issues access tokens with `aud: [amp-mcp, <client>]`, `azp: <client>`, and an `email` claim. Before this release MCP tokens carried no email, which is why amp-api logged "JIT user provisioning skipped".
4. amp-api checks `aud` on `/mcp` for `amp-mcp`, and on `/api` for `oauth2Proxy.clientID` or `amp-mcp`.

Dex's `UpdateClient` replaces `trustedPeers` wholesale, and `amp-mcpoauth` runs more than one replica, so two registrations landing within milliseconds of each other on different pods can drop one entry. `ensureTrusted` re-reads after every write and retries, re-checks a few seconds later, and `/authorize` re-adds on every login. The worst case is one extra login prompt. `cmd/mcpoauth/main_test.go` exercises this with a fake Dex that has Dex's replace semantics. Dex re-checks trust at every token issuance, refreshes included.

Dex does not emit the RFC 9207 `iss` authorization-response parameter, so the broker's metadata advertises `authorization_response_iss_parameter_supported: false`. Advertising `true` would make 2026-07-28 clients reject every login. The metadata `issuer` stays Dex's URL. The official TypeScript SDK (1.32.x) explicitly doesn't compare it to the discovery URL, and the old value is what existing clients already work with.

### Upgrade: everyone signs in once

amp-api rejects any token without the right audience, with `401` + `WWW-Authenticate: Bearer error="invalid_token", resource_metadata=..., scope=...`. Every token issued before this release lacks `amp-mcp`, and refreshing doesn't add it (a refresh keeps the original scopes), so on upgrade every MCP client gets one 401 and goes back through a browser login, after which it has an audience-bound token. UI sessions are unaffected: oauth2-proxy tokens carry `amp-ui`, which `/api` accepts. There is deliberately no grace mode.

### End-to-end test

`test/e2e` drives a deployed stack exactly like an MCP client: PRM discovery from a 401, DCR, `/authorize` through the broker, Dex's login form, Dex `/token`, then MCP calls in both eras with the real `mcp-go` client, plus a UI login through oauth2-proxy to `/api/me`. It is phased to bracket an upgrade (`AMP_E2E_PHASE=pre` against the old release, then `post` against the new one); see the file header for env vars. On docker-desktop, port-forward Envoy to 8443 and set `AMP_E2E_DIAL=127.0.0.1:8443` so `https://amp.local` resolves without binding :443.

## securityContext hardening (Kubernetes "restricted" Pod Security Standard)

Every container in the chart (all Deployments, both post-install/upgrade Jobs, every busybox `initContainer`) now sets an explicit `securityContext` targeting the "restricted" PSS profile: `runAsNonRoot: true`, `seccompProfile: RuntimeDefault` at the pod level; `allowPrivilegeEscalation: false`, `capabilities: {drop: [ALL]}`, `runAsNonRoot: true` at the container level. This is chart-only — no cluster-level PodSecurity admission labels were touched, matching the existing namespace convention (peer namespaces like odoo/vault/immich also run without `enforce` labels).

Reusable helpers added to `_helpers.tpl`: `amp.podSecurityContext` (optionally takes a dict with `fsGroup`), `amp.containerSecurityContext` (optionally takes a dict with `runAsUser`), and `amp.chownInitSecurityContext` (see below).

**Real problems found while actually deploying this (not just rendering the chart):**

1. **`busybox:1.36` has no declared non-root user (`docker inspect --format '{{.Config.User}}'` is empty) — it runs as root by default.** Every `wait-for-*`/`render-config` initContainer using it failed immediately with `container has runAsNonRoot and image will run as root`, since Kubernetes checks the image's declared user *before* even trying to start the container. Fixed by explicitly setting `runAsUser: 65534` (the conventional "nobody" UID — no named user exists in busybox for it, but numeric UIDs work fine for `sh`/`nc`/`sed`, which never call `getpwuid`).
2. **Our own 4 Go service images (`amp-api`, `amp-authadmin`, `amp-mcpoauth`, `amp-asynqmongate`) had the same problem** — none of the Dockerfiles declared a `USER`, so their `alpine:3.20` final stage defaulted to root. Fixed by adding `adduser -D -u 1000 amp` + `USER 1000` to all four Dockerfiles.
3. **`hibiken/asynqmon:0.7.1` (third-party) also runs as root by default** — same fix, explicit `runAsUser: 1000` on just that container (the `gate` sidecar next to it is our own image, already fixed by #2).
4. **UI's `nginx:alpine` listens on port 80, which is privileged** — a non-root process can't bind it. Switched the base image to `nginxinc/nginx-unprivileged:alpine`, which is purpose-built for this: defaults to UID 101, listens on 8080, and `/var/cache/nginx` is already group-writable by that UID. Far simpler than patching stock `nginx:alpine`'s permissions by hand. Updated the `listen` directive to 8080 and the Deployment's `containerPort` to match (the Service didn't need a port-number change since it targets the port by name).
5. **`envoyproxy/envoy` also runs as root by default, but only binds non-privileged ports (8080/8443/9901) already** — no application-level change needed, just `runAsUser: 1000` on the container.
6. **Ollama's official image defaults to `/root/.ollama` for all state** (models + manifests), since it runs as root with `HOME=/root`. Running as a non-root UID needed both `HOME` and `OLLAMA_MODELS` redirected to the PVC mount path (`/data` and `/data/models` respectively) — otherwise Ollama silently tries to write under `/root` again regardless of where the volume is mounted.
7. **`fsGroup` (pod-level `securityContext.fsGroup`) is not applied for every storage backend.** Kubernetes intentionally skips the recursive chown-on-mount for `hostPath`-backed PVs — which is exactly what `rancher.io/local-path` (Docker Desktop's/kind's default StorageClass) provisions under the hood. Typesense and Ollama (the two PVC-owning Deployments) hit this immediately: `fsGroup: 1000` was set, but Typesense crashed with `Permission denied` writing to `/data/db` (owned by root from the volume's initial provisioning) and Ollama would have hit the same on model pull. **Fixed with the "chown init container" pattern** (same one Bitnami's charts use for postgresql/redis/etc., since it works identically regardless of the storage class/CSI driver): a short-lived `fix-permissions` initContainer (busybox, `amp.chownInitSecurityContext` — deliberately `runAsNonRoot: false, runAsUser: 0`, capabilities `drop: [ALL], add: [CHOWN]` only) that runs `chown -R 1000:1000 /data` on the same mount before the real container starts. `fsGroup` is still set too (harmless, and a defense-in-depth win on storage backends that *do* support it, e.g. prod's NFS-backed `nfs-storage` StorageClass — not yet confirmed empirically on prod, watch for this on first prod rollout).

Verified end-to-end on docker-desktop after all of the above: every Deployment/Job pod reached `Running`/`Ready` (including a clean PVC wipe-and-recreate for typesense/ollama to test the chown-init-container path from a fresh volume), both post-install Jobs (`amp-bootstrap-admin`, `amp-pull-embedding-model`) completed successfully — including a real Ollama model pull writing into the new non-root `/data/models` path — and full browser verification passed: login (Dex→oauth2-proxy→UI), Projects list rendering real data, and the asynqmon dashboard at `queues.amp.local` showing a real completed `kb-index` task.



Direct, synchronous writes from `amp-api` to Typesense on every `amp_kb_write`/`amp_kb_delete` call don't scale — a burst of writes from many agents across many projects hits Typesense inline with the request, with no backpressure control. Fixed by adding a queue:

- **Valkey** (`valkey/valkey` chart, standalone mode, ACL auth via a generated `amp-valkey-users` secret) backs an [asynq](https://github.com/hibiken/asynq) task queue.
- `kb.Service.WriteDoc`/`DeleteDoc` now enqueue a task and return immediately with a deterministically-computed `DocSummary` (the doc ID is `sha256(project_id:path:chunk)`, computable without touching Typesense at all) instead of writing inline. If `REDIS_ADDR` is unset (local `make dev` without Valkey) or the enqueue call itself fails, they fall back to writing synchronously — so this degrades gracefully rather than silently dropping writes.
- A worker runs **in-process inside `amp-api`** (a background goroutine started in `main.go`, not a separate Deployment) consuming the `kb-index` queue at a bounded concurrency (`KB_INDEX_CONCURRENCY`, default 4) — this is what actually smooths out a write burst into a steady rate instead of hammering Typesense.
- `AnnotateDoc` intentionally stays synchronous (low-frequency, read-then-write, harder to queue correctly without risking lost updates under concurrent annotation).
- **asynqmon** (the official monitoring UI) is exposed for visibility — but it has *no built-in authentication*, so it sits behind `amp-asynqmon-gate`, a tiny sidecar (same pattern as `amp-authadmin`) that requires the **admin** role, not just any logged-in session, since queue internals (payloads, retry counts) shouldn't be visible to every member.

### Why asynqmon needs its own subdomain, not a path prefix

First attempt was `https://<domain>/asynqmon` (path-based, matching how `/authadmin` and `/api` work). This **does not work** and was reverted — asynqmon's SPA:
1. Has no basename/subpath support in its client-side router — clicking any nav item does `history.push("/")`, navigating clean off the `/asynqmon` prefix entirely.
2. Makes its own internal `/api/queues` calls for live data — which collided with `amp-api`'s own `/api` prefix route, silently sending asynqmon's data requests to the wrong backend.

Both are architectural assumptions baked into asynqmon's build (it assumes it owns 100% of its origin's path space), not something fixable by adjusting Envoy's `prefix_rewrite`. The real fix: asynqmon gets a **dedicated subdomain** (`queues.<domain>` by default, `asynqmon.subdomain` in values) via a second Envoy `virtual_host` matched by `Host` header rather than path — it owns its entire domain's namespace, so neither collision is possible. This needs:
- A DNS record for `queues.<domain>` pointing at the same place as the main domain (same LoadBalancer IP / same Cloudflare setup).
- `oauth2-proxy --cookie-domain=.<domain>` (leading dot) so the session cookie set on login at the main domain is also sent to the subdomain — without this, you'd have to log in twice.
- The cert-manager `Certificate` for Envoy's public TLS now carries both hostnames as SANs.

## Real incident: secrets rotating out from under running pods on ArgoCD

`templates/secrets.yaml` originally used the standard Helm "stable random secret" trick — `{{- $existing := lookup "v1" "Secret" $ns $secretName }}`, reuse its value if found, else generate fresh. This works reliably with plain `helm upgrade` (verified all through the docker-desktop testing in this doc) because it has live cluster access at template time.

**It does not work reliably through ArgoCD.** Caught live in prod: two consecutive `argocd.argoproj.io/refresh=hard` syncs of the exact same chart/values produced two *different* random `client-secret` values for `amp-oauth2-proxy`. Whichever pods (dex, oauth2-proxy) didn't happen to restart in a given sync kept their old value baked into their env (env vars are read once at container start, not re-read live), while the Secret object itself moved on — so Dex and oauth2-proxy silently desynced, breaking every login with `invalid_client` until every consumer happened to restart together. Same issue for `amp-valkey-users`, desyncing Valkey/amp-api/amp-asynqmon's queue password.

Fix: `global.secretOverrides` in `values-production.yaml` pins the already-live, already-working values directly, so there's no template-time randomness to race at all in that environment. Local dev (`values.yaml`) leaves these empty and keeps the lookup-or-random fallback, which is fine there — local always deploys via plain `helm upgrade`. **If you ever need to actually rotate these values**, you must restart every consumer in the same breath: `dex`, `oauth2-proxy` (for `amp-oauth2-proxy`), and `amp-valkey`, `amp-api`, `amp-asynqmon` (for `amp-valkey-users`) — a partial rotation is exactly the bug this fix exists to prevent.




</content>
