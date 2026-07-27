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
