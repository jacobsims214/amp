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
{{- if $registry -}}
{{ $registry }}/{{ .image }}:{{ .tag }}
{{- else -}}
{{ .image }}:{{ .tag }}
{{- end -}}
{{- end -}}
