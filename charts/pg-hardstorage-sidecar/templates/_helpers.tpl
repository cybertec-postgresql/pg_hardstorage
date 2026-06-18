{{/*
Common template helpers for the pg-hardstorage-sidecar chart.
*/}}

{{/*
Expand the name of the chart.
*/}}
{{- define "pg-hardstorage-sidecar.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Fully-qualified app name. Includes the release name unless the
caller pinned fullnameOverride. Truncated to 63 chars to satisfy
the DNS-1123 subdomain limit.
*/}}
{{- define "pg-hardstorage-sidecar.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
Chart name + version, in the form Helm expects on the
helm.sh/chart label.
*/}}
{{- define "pg-hardstorage-sidecar.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Common labels applied to every object the chart renders.
*/}}
{{- define "pg-hardstorage-sidecar.labels" -}}
helm.sh/chart: {{ include "pg-hardstorage-sidecar.chart" . }}
{{ include "pg-hardstorage-sidecar.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: pg-hardstorage
{{- end -}}

{{/*
Selector labels -- the subset of common labels that match-stable.
*/}}
{{- define "pg-hardstorage-sidecar.selectorLabels" -}}
app.kubernetes.io/name: {{ include "pg-hardstorage-sidecar.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/*
Resolve the ServiceAccount name. If serviceAccount.create is
true, default to fullname; otherwise the caller must provide a
name.
*/}}
{{- define "pg-hardstorage-sidecar.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "pg-hardstorage-sidecar.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/*
Image reference. Falls back to .Chart.AppVersion when the user
did not pin a tag explicitly.
*/}}
{{- define "pg-hardstorage-sidecar.image" -}}
{{- $tag := .Values.image.tag | default .Chart.AppVersion -}}
{{- printf "%s:%s" .Values.image.repository $tag -}}
{{- end -}}
