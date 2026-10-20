# Deploy `amp` to `rp-tools-nonprod` (Red-Platform EKS, nonprod)

Operational runbook for taking the `amp` Helm chart from local/offline to live on
the `rp-tools-nonprod` EKS cluster, registered through that cluster's ArgoCD
pattern and served at `amp.rp-tools-nonprod.rvohealth.com` over the shared
ambient-Istio Gateway.

This is a **values/template refinement + one ArgoCD `Application` manifest** —
the only cluster-scoped footprint is the cert-manager operator the chart
self-provisions (see §0), and images are already public. The
flow is: prepare the cluster → refine the chart (already done, on the
`rvoh/eks-tools-nonprod` branch) → apply the `Application` **by hand** → **Sync**
in the ArgoCD UI → point DNS → smoke-test.

| Name | Value |
|---|---|
| Target ArgoCD **cluster** | `rp-tools-nonprod` |
| ArgoCD **hub** context (where ArgoCD runs / where you apply) | `rp-tools-ue1` |
| GitOps repo (holds the `Application`) | `rvo-redplatform/eks-rp-tools-nonprod-crds` |
| Chart source (public, anonymous HTTPS) | `github.com/jacobsims214/amp`, path `v2/deploy/helm/amp`, rev `rvoh/eks-tools-nonprod` |
| In-cluster namespace | `amp` (created by ArgoCD, `CreateNamespace=true`) |
| Public host | `amp.rp-tools-nonprod.rvohealth.com` |
| Images | `ghcr.io/jacobsims214/amp/*` — **public, no pull secret** |
| StorageClass | `gp3` |

---

## 0. cert-manager — RESOLVED (the chart self-provisions it)

The chart **unconditionally** emits cert-manager `Issuer`/`Certificate` CRs via
`templates/cert-issuers.yaml`. It is **not gated** by `global.tls.enabled`:

- **`amp-selfsigned-bootstrap` Issuer + `amp-ca`** — an internal CA.
- **`amp-dex-grpc-server-tls` + `amp-dex-grpc-client-tls`** — Dex gRPC **mTLS**
  (server + client certs). Consumers (`amp-authadmin`, `amp-mcpoauth`) mount
  these to talk to Dex's gRPC admin API.

This internal CA + Dex gRPC mTLS is required **even though `global.tls.enabled:
false`** on this overlay (the edge ALB/NLB terminates public TLS, so Envoy and
every pod run plain HTTP — the *public* Envoy certs are inert, but the *internal*
ones are not). The `rp-tools-nonprod` cluster never had cert-manager, so rather
than request a platform-team install, the chart **self-provisions** it.

**Resolved — not a prerequisite for Sync.** The overlay
`values-eks-tools-nonprod.yaml` turns cert-manager **on**:

```yaml
certmanager:
  enabled: true         # emit the amp-namespaced Issuer(s)/Certificate(s)
  installOperator: true # also install the cluster-scoped operator + CRDs
```

So the `amp` release installs the **cluster-scoped cert-manager operator + CRDs**
(Namespace `cert-manager`, plus the controller/cainjector/webhook Deployments and
RBAC, from `templates/cert-manager-operator.yaml`) **and** emits the
**`amp`-namespaced `Issuer`/`Certificate`** CRs. The operator lives in its own
`cert-manager` namespace; only the Issuer/Certificate layer is namespaced to
`amp`. **No separate platform-team cert-manager install is required** — the amp
release is self-contained, so §0 is no longer a precondition for Sync.

> **Cluster-scoped operator, owned by amp.** The operator is inherently
> cluster-scoped (that is cert-manager's architecture — it must observe Issuer/
> Certificate resources cluster-wide), and because the amp release installs it,
> the operator + CRDs are **owned by the amp release**. For this **nonprod,
> self-contained deploy that is acceptable.** Caveat: if the platform later
> installs its **own** cert-manager on the cluster, there will be **resource
> drift** (operator + CRD ownership collision). Not a blocker now — flag it if
> it ever happens.

> **ArgoCD manifest.** Because the release creates cluster-scoped resources, the
> ArgoCD `Application` (`nonprod/argocd-apps/amp.yaml`) sets
> `syncOptions: [CreateNamespace=true, --allow-extra-resources]` so the operator
> + CRDs can be applied in `cert-manager`, outside the `amp` namespace.

---

## 1. Prerequisites

1. **Refresh the SSO session** (interactive — a **human** does this; it opens a
   browser and cannot be scripted):
   ```bash
   aws sso login --profile tools-nonprod
   ```
   Re-run it whenever `kubectl`/`aws` start returning expired-credential errors.
   (Re-authed to confirm the items below are live.)
2. **Confirm the `gp3` StorageClass exists** on the target cluster. The overlay
   pins `gp3` for `typesense`/`ollama`/`cnpg` (the base chart's `nfs-storage` is
   Talos-only and does not exist here):
   ```bash
   kubectl --context rp-tools-ue1 get sc
   ```
   Expect a `gp3` row. (The cluster's default EBS StorageClass is `gp3`.) **This
   is confirmed live on `rp-tools-nonprod`.**
3. **cert-manager is self-provisioned** (§0) — no separate platform install, so
   there is nothing here to gate Sync on. Confirm the operator + Issuers come up
   in the §6 smoke-test.
4. **Pin the secret placeholders** in §5 **before** the first sync. Do not skip
   this — ArgoCD will desync Dex from oauth2-proxy/valkey otherwise.

---

## 2. How the chart was refined (already on `rvoh/eks-tools-nonprod`)

The chart is portable: the metal bits live in values. The EKS/nonprod work is a
**single additive overlay + two new templates**, no edits to the base
`values.yaml` or `values-production.yaml`.

### 2.1 `values-eks-tools-nonprod.yaml` (the overlay)

Applied on top of the base `values.yaml`; mirrors the `values-production.yaml`
key schema; sets only the keys that differ for EKS/nonprod:

- `global.domain: amp.rp-tools-nonprod.rvohealth.com` — drives the Dex issuer,
  the oauth2-proxy cookie/redirect URLs, and `MCP_PUBLIC_URL`; **must equal the
  exposed host**.
- `global.tls.enabled: false` — the edge ALB/NLB terminates TLS; Envoy and every
  pod run **plain HTTP**, so cert-manager's *public* Envoy certs are inert.
  (`tls.issuer: letsencrypt-dns01` is inert while `enabled: false`.)
- `global.imageTag: "latest"` — single override point for every amp-* tag
  (the build workflow also tags `sha-<shortsha>`; a real release should prefer
  the sha tag, set via the ArgoCD `helm.parameters` override in §3).
- **Images public, no pull secret:** `image.registry:
  ghcr.io/jacobsims214/amp`, `pullPolicy: IfNotPresent`. No `imagePullSecret`
  is created or referenced anywhere in the chart.
- **Storage:** `typesense`/`ollama`/`cnpg` → `storageClassName: gp3`.
- **Nonprod sizing:** `ampApi`/`ui`/`authAdmin`/`mcpOAuth`/`valkey`/`dex`/
  `oauth2Proxy`/`envoy` = 1 replica each. `oauth2Proxy` is deliberately 1 replica
  (with 2 and no shared session-locking, concurrent token refreshes race and
  kill browser sessions). `cnpg` is a single instance.
- `envoy.service.type: ClusterIP` — fronted by the ambient-mesh HTTPRoute, not an
  external LB.

### 2.2 `namespace` template (ambient mesh)

New `templates/namespace.yaml`, **off by default** in the base chart. The overlay
sets:
```yaml
namespace:
  enabled: true
  labels:
    istio.io/dataplane-mode: ambient
    istio.io/rev: rapid
```
Emits one Namespace labelled for the Istio **ambient mesh**. Creates **no**
Gateway/VirtualService/DestinationRule — the shared ambient Gateway already
exists.

### 2.3 `ingress.httpRoute` template (Gateway API)

New `templates/httproute.yaml`, **off by default** in the base chart. The
overlay sets:
```yaml
ingress:
  httpRoute:
    enabled: true
    hostnames:
      - amp.rp-tools-nonprod.rvohealth.com
      - queues.amp.rp-tools-nonprod.rvohealth.com   # optional, only if exposed
    parentRefs:
      - name: rp-ingress            # the shared ambient Gateway
        namespace: istio-ingress
    backend:
      name: envoy
      port: 80                      # TLS terminated at the edge → plain HTTP
```
Emits a single `HTTPRoute` named `amp` attaching to the shared Gateway
`rp-ingress`. It is annotated
`external-dns.alpha.kubernetes.io/ignore: "true"` so external-dns creates no
record for it (DNS is managed at the edge / out-of-band, §4).

---

## 3. Apply the `Application` by hand (manual sync)

The cluster **does NOT auto-watch this manifest**. ArgoCD is applied **manually**
— the `Application` has no `automated` sync block (mirrors `turnstone.yaml` /
`vllm.yaml` in the same folder). You create it, then sync it yourself.

From the `rvo-redplatform/eks-rp-tools-nonprod-crds` repo, apply the `Application`
to the ArgoCD **hub** context (`rp-tools-ue1`):

```bash
kubectl --context rp-tools-ue1 apply -f nonprod/argocd-apps/amp.yaml
```

What that `Application` (`nonprod/argocd-apps/amp.yaml`) does:

- `project: default`, anonymous HTTPS read of the **public** amp repo (no
  repository credential / deploy key to register).
- `source.path: v2/deploy/helm/amp`, `targetRevision: rvoh/eks-tools-nonprod`.
- `helm.valueFiles: [values-eks-tools-nonprod.yaml]` (the overlay on top of the
  base `values.yaml`).
- `helm.parameters: global.imageTag=latest` — the single image-tag override; bump
  this (with the overlay's note) to a `sha-<shortsha>` tag when rolling a release.
- `destination.name: rp-tools-nonprod` (the ArgoCD **cluster**, not a namespace),
  `destination.namespace: amp`, `CreateNamespace=true`.

Then **Sync in the ArgoCD UI** (or `argocd app sync amp`). Do this **only after**
the §5 placeholders are pinned (cert-manager comes up as part of the same release,
see §0).

---

## 4. DNS (out-of-band Route53 CNAME)

This cluster's `external-dns` **only owns `gateway.*`** records, so it will not
create a record for `amp.*` (the HTTPRoute is `external-dns ... ignore`). You must
create the mapping **out of band** in Route53.

Hosted zone: **`Z02795123S97MIMZ1Q3J3`** (`rvohealth.com`).

```
CNAME   amp.rp-tools-nonprod.rvohealth.com
            → gateway.rp-tools-nonprod.rvohealth.com
```

The CNAME points at the cluster's **gateway** (the shared ambient Gateway that
terminates TLS at the edge). Traffic then flows edge → ambient Gateway → HTTPRoute
`amp` → Envoy (`envoy:80`, plain HTTP).

- **Optional** `queues.` subdomain: create
  `queues.amp.rp-tools-nonprod.rvohealth.com` → `gateway.rp-tools-nonprod.rvohealth.com`
  only if you intend to expose that subdomain (it is in the HTTPRoute hostnames).

---

## 5. Secret pinning (BEFORE first sync)

The overlay's `global.secretOverrides` (and `global.bootstrapAdminEmails`) are
**loud `TODO-PIN-BEFORE-APPLY` placeholders**. They are not real secrets. Pin real,
**pre-generated** values in `values-eks-tools-nonprod.yaml` **before the first
ArgoCD sync**:

```yaml
global:
  bootstrapAdminEmails: "TODO-PIN-BEFORE-APPLY"          # real first-admin email
  secretOverrides:
    oauth2ProxyClientSecret: "TODO-PIN-BEFORE-APPLY-oauth2ProxyClientSecret"
    oauth2ProxyCookieSecret: "TODO-PIN-BEFORE-APPLY-oauth2ProxyCookieSecret"
    valkeyPassword:        "TODO-PIN-BEFORE-APPLY-valkeyPassword"
    bootstrapAdminPassword:"TODO-PIN-BEFORE-APPLY-bootstrapAdminPassword"
```

**Why it matters:** ArgoCD's Helm rendering does **not** reliably support the
chart's lookup-based "reuse the existing Secret value" trick — two consecutive
syncs of the same chart have produced two different random values, so pods that
didn't restart kept the *old* env value while the `Secret` got a *new* one,
**desyncing Dex from oauth2-proxy/valkey** until every consumer happened to
restart together. Pinning a literal stable value removes the template-time
randomness entirely.

> If you ever regenerate these values, you must **restart dex, oauth2-proxy,
> amp-valkey, amp-api, and the bootstrap-admin job together** so they pick up the
> new values at the same time.

---

## 6. Smoke-test checklist

Run after Sync reports healthy and the §4 DNS CNAME is live.

- [ ] **Namespace pods all running**
      `kubectl --context rp-tools-ue1 -n amp get pods`
      → `dex`, `oauth2-proxy`, `envoy`, `amp-api`, `ui`, `amp-authadmin`,
      `amp-mcp-oauth`, `valkey`, `cnpg`-instance, `typesense`, `ollama` all
      `Running` (and the bootstrap-admin `Job` complete).
- [ ] **HTTPRoute is Accepted**
      `kubectl --context rp-tools-ue1 -n amp get httproute amp`
      → status `Accepted` (it attached to `rp-ingress`).
- [ ] **Public host serves**
      `https://amp.rp-tools-nonprod.rvohealth.com/` → UI login page (200),
      not a connection error / 502.
- [ ] **`/mcp` endpoint reachable**
      `curl -sk https://amp.rp-tools-nonprod.rvohealth.com/mcp`
      → reachable and returns a `401`/`405` with `WWW-Authenticate` + RFC 9728
      protected-resource metadata (an unauthenticated probe), **not**
      "connection refused" — that confirms the MCP path is wired through Envoy's
      jwt_authn filter.
- [ ] **cert-manager operator + Dex gRPC mTLS**
      `kubectl --context rp-tools-ue1 get ns cert-manager` → present with the
      `cert-manager`/`cainjector`/`webhook` pods `Running`;
      `kubectl --context rp-tools-ue1 -n amp get cert amp-dex-grpc-server
      amp-dex-grpc-client amp-ca` → `READY`/issued; `dex` pod serves gRPC
      (consumers' `wait-for-dex-grpc` init succeeded). The operator is self-
      provisioned by the release (§0), so it should be live after Sync.
