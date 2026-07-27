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

**Known limitation, not yet addressed:** Streamable HTTP is stateful by default — a client's session lives in `amp-api`'s in-memory `sync.Map` on whichever pod handled `initialize`. With `ampApi.replicas: 2` in prod, a subsequent request landing on the *other* pod (via Envoy's `STRICT_DNS`/`ROUND_ROBIN` cluster to the ClusterIP Service) would get `Invalid session ID` again. In practice this hasn't surfaced because HTTP/1.1 keep-alive tends to pin a client's connection to one backend pod for the connection's lifetime, but it's not guaranteed. If this resurfaces under real multi-replica load, the fix is either Envoy ring-hash load balancing keyed on the `Mcp-Session-Id` header, or splitting the MCP listener into its own single-replica Deployment.

## KB indexing queue (Valkey + asynq)

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



</content>
