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

{{/*
Name of the valkey store's PersistentVolumeClaim. The PVC is rendered by the
valkey subchart as its "valkey.fullname"; this mirrors that helper (chart name
"valkey", honoring valkey.nameOverride / valkey.fullnameOverride) so the parent
chart can look the claim up.
*/}}
{{- define "ballast.valkeyPVCName" -}}
{{- $v := .Values.valkey }}
{{- if $v.fullnameOverride }}
{{- $v.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default "valkey" $v.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Renders "true" when the valkey subchart will render its own PVC. Mirrors the
condition at the top of the subchart's templates/pvc.yaml.
*/}}
{{- define "ballast.valkeyRendersPVC" -}}
{{- $v := .Values.valkey }}
{{- $ds := $v.dataStorage }}
{{- if and $v.enabled $ds.enabled (not (dig "replica" "enabled" false $v)) (not (empty $ds.requestedSize)) (empty $ds.persistentVolumeClaimName) -}}
true
{{- end }}
{{- end }}

{{/*
Convert a Kubernetes resource quantity string (e.g. "192Mi", "1Gi", "5G",
"1073741824") to a byte count, rendered as a float. Renders nothing for forms
it does not handle (milli, exponent notation), so callers skip the comparison
rather than act on a misparse.
*/}}
{{- define "ballast.quantityBytes" -}}
{{- $q := toString . | trim }}
{{- $num := regexFind "^[0-9]+(\\.[0-9]+)?" $q }}
{{- $suffix := trimPrefix $num $q }}
{{- $mult := dict "" 1.0 "Ki" 1024.0 "Mi" 1048576.0 "Gi" 1073741824.0 "Ti" 1099511627776.0 "Pi" 1125899906842624.0 "Ei" 1152921504606846976.0 "k" 1000.0 "M" 1000000.0 "G" 1000000000.0 "T" 1000000000000.0 "P" 1000000000000000.0 "E" 1000000000000000000.0 }}
{{- if and $num (hasKey $mult $suffix) }}
{{- mulf (float64 $num) (get $mult $suffix) }}
{{- end }}
{{- end }}
