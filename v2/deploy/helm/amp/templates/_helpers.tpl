{{/* Common name/labels helpers */}}
{{- define "amp.fullname" -}}
amp
{{- end -}}

{{- define "amp.labels" -}}
app.kubernetes.io/part-of: amp
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "amp.componentLabels" -}}
{{ include "amp.labels" . }}
app.kubernetes.io/component: {{ .component }}
{{- end -}}

{{- define "amp.image" -}}
{{- $registry := .root.Values.image.registry -}}
{{- $tag := .tag -}}
{{/* Empty per-component tag falls back to global.imageTag — lets ArgoCD/CI
     bump every AMP image in one place via a single helm parameter override
     (matching the odoo-operator Application's `image.tag` parameter pattern),
     instead of having to set 4 separate per-component tags per release. */}}
{{- if and (not $tag) .root.Values.global.imageTag -}}
{{- $tag = .root.Values.global.imageTag -}}
{{- end -}}
{{- if $registry -}}
{{ $registry }}/{{ .image }}:{{ $tag }}
{{- else -}}
{{ .image }}:{{ $tag }}
{{- end -}}
{{- end -}}

{{/* asynqmon's own subdomain — see values.yaml asynqmon.subdomain comment
     for why it isn't a path prefix under global.domain. */}}
{{- define "amp.asynqmonDomain" -}}
{{ .Values.asynqmon.subdomain }}.{{ .Values.global.domain }}
{{- end -}}

{{/* ---- Security contexts (Kubernetes "restricted" Pod Security Standard) ----
     Applied to every container in this chart — see docs/deploy-architecture.md
     for the couple of upstream images that needed real config changes (not
     just a securityContext block) to actually run non-root: ui's nginx
     (moved off port 80) and ollama (moved its model dir off /root).

     amp.podSecurityContext: pod-level (spec.securityContext). Pass "." for
     the common case, or a dict with "fsGroup" for pods that own a PVC (e.g.
     typesense, ollama) so the volume is group-writable by the container's
     non-root UID regardless of the storage backend's default ownership.
     amp.containerSecurityContext: container-level, optionally pass a dict
     with "runAsUser" for images that need a specific non-root UID (e.g.
     nginx's default "nginx" user is 101). */}}
{{- define "amp.podSecurityContext" -}}
runAsNonRoot: true
seccompProfile:
  type: RuntimeDefault
{{- if and . .fsGroup }}
fsGroup: {{ .fsGroup }}
{{- end }}
{{- end -}}

{{- define "amp.containerSecurityContext" -}}
allowPrivilegeEscalation: false
capabilities:
  drop: ["ALL"]
runAsNonRoot: true
{{- if and . .runAsUser }}
runAsUser: {{ .runAsUser }}
{{- end }}
{{- end -}}

{{/* amp.chownInitSecurityContext: for the "fix-permissions" pattern used by
     PVC-owning pods (typesense, ollama). fsGroup ownership on volume mount
     isn't applied for every storage backend (notably hostPath-backed PVs,
     e.g. Rancher's local-path-provisioner used by kind/Docker Desktop) —
     a short-lived busybox initContainer that chowns the mount to the same
     UID the main container runs as is the portable fix regardless of the
     storage class/CSI driver in play (this is the same pattern Bitnami's
     charts use for postgresql/redis/etc). Deliberately overrides the pod's
     runAsNonRoot: true (only this initContainer needs root, only for
     CHOWN, only for the seconds it takes to exit). */}}
{{- define "amp.chownInitSecurityContext" -}}
allowPrivilegeEscalation: false
capabilities:
  drop: ["ALL"]
  add: ["CHOWN"]
runAsNonRoot: false
runAsUser: 0
{{- end -}}

{{/* amp.relayHarnessRoutes: the remote control relay's harness leg (epic
     165), only with relay.enabled. It goes to amp-relay, its own service
     (#2387): amp-api serves no relay route, so with amp-relay scaled to 0
     Envoy has no upstream and nobody can reach a harness. Like /mcp,
     ext_authz is off because amp-relay checks the bearer (an OAuth token,
     then the rct_ connect token) itself. The commands stream is long-lived:
     timeout 0s turns off the 15s per-route default, and max_stream_duration
     cuts it at the session's 12-hour lifetime (the harness reconnects on its
     own if it is still valid). Keepalives every 20s keep it under
     stream_idle_timeout. */}}
{{- define "amp.relayHarnessRoutes" -}}
{{- if .Values.relay.enabled }}
# ---- Remote control relay, harness leg (amp-relay): bearer auth in the relay, not ext_authz ----
- match: { prefix: "/relay/" }
  route:
    cluster: amp_relay
    timeout: 0s
    max_stream_duration:
      max_stream_duration: {{ .Values.relay.maxStreamDuration | default "43200s" }}
  typed_per_filter_config:
    envoy.filters.http.ext_authz:
      "@type": type.googleapis.com/envoy.extensions.filters.http.ext_authz.v3.ExtAuthzPerRoute
      disabled: true
{{- end }}
{{- end -}}

{{/* amp.relayBrowserRoutes: the relay's browser leg, /api/sessions, goes to
     amp-relay behind the cookie session (ext_authz stays on); its per-session
     SSE stream needs the unbounded timeout, like /api/events. Matched before
     the /api prefix (amp-api). */}}
{{- define "amp.relayBrowserRoutes" -}}
{{- if .Values.relay.enabled }}
# ---- Remote control relay, browser leg (amp-relay) ----
- match: { safe_regex: { regex: "^/api/sessions/[^/]+/events$" } }
  route: { cluster: amp_relay, timeout: 0s }
- match: { path: "/api/sessions" }
  route: { cluster: amp_relay }
- match: { prefix: "/api/sessions/" }
  route: { cluster: amp_relay }
{{- end }}
{{- end -}}

{{/* amp.relayCluster: Envoy's upstream for amp-relay, only with
     relay.enabled. */}}
{{- define "amp.relayCluster" -}}
{{- if .Values.relay.enabled }}
- name: amp_relay
  type: STRICT_DNS
  lb_policy: ROUND_ROBIN
  load_assignment:
    cluster_name: amp_relay
    endpoints:
      - lb_endpoints:
          - endpoint: { address: { socket_address: { address: amp-relay.{{ .Values.global.namespace }}.svc.cluster.local, port_value: 3002 } } }
{{- end }}
{{- end -}}
