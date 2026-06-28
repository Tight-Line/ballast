{{/*
Expand the name of the chart.
*/}}
{{- define "ballast.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "ballast.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Common labels.
*/}}
{{- define "ballast.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | quote }}
{{ include "ballast.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels.
*/}}
{{- define "ballast.selectorLabels" -}}
app.kubernetes.io/name: {{ include "ballast.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Service account name.
*/}}
{{- define "ballast.serviceAccountName" -}}
{{- default (include "ballast.fullname" .) .Values.serviceAccount.name }}
{{- end }}

{{/*
Container image reference.
*/}}
{{- define "ballast.image" -}}
{{- $tag := .Values.image.tag | default .Chart.AppVersion }}
{{- printf "%s:%s" .Values.image.repository $tag }}
{{- end }}

{{/*
Redis/Valkey URL. Uses the official valkey/valkey subchart service when valkey.enabled,
otherwise falls through to store.endpoint.
*/}}
{{- define "ballast.redisURL" -}}
{{- if .Values.valkey.enabled -}}
redis://{{ .Release.Name }}-valkey:6379
{{- else -}}
{{ required "store.endpoint is required when valkey.enabled is false" .Values.store.endpoint }}
{{- end -}}
{{- end }}

{{/*
Name of the webhook TLS Secret created by cert-manager.
*/}}
{{- define "ballast.webhookCertSecret" -}}
{{ include "ballast.fullname" . }}-webhook-cert
{{- end }}

{{/*
Name of the webhook Service.
*/}}
{{- define "ballast.webhookServiceName" -}}
{{ include "ballast.fullname" . }}-webhook
{{- end }}
