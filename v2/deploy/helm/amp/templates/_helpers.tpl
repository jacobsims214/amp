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
