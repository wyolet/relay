{{/* Base name */}}
{{- define "relay.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* Fully qualified app name */}}
{{- define "relay.fullname" -}}
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

{{- define "relay.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* Common labels */}}
{{- define "relay.labels" -}}
helm.sh/chart: {{ include "relay.chart" . }}
{{ include "relay.selectorLabels" . }}
app.kubernetes.io/version: {{ .Values.image.tag | default .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: relay
{{- end -}}

{{/* Selector labels (relay component) */}}
{{- define "relay.selectorLabels" -}}
app.kubernetes.io/name: {{ include "relay.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: relay
{{- end -}}

{{- define "relay.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "relay.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/* Component hostnames (in-cluster service DNS short names) */}}
{{- define "relay.postgresql.fullname" -}}{{ printf "%s-postgresql" (include "relay.fullname" .) }}{{- end -}}
{{- define "relay.clickhouse.fullname" -}}{{ printf "%s-clickhouse" (include "relay.fullname" .) }}{{- end -}}
{{- define "relay.valkey.fullname" -}}{{ printf "%s-valkey" (include "relay.fullname" .) }}{{- end -}}

{{/* Secret name (existing or chart-managed) */}}
{{- define "relay.secretName" -}}
{{- if .Values.secrets.existingSecret -}}
{{- .Values.secrets.existingSecret -}}
{{- else -}}
{{- include "relay.fullname" . -}}
{{- end -}}
{{- end -}}

{{/* Assembled RELAY_PG_DSN (bundled PG or external) */}}
{{- define "relay.pgDsn" -}}
{{- if .Values.postgresql.enabled -}}
{{- $pw := required "postgresql.auth.password is required when postgresql.enabled" .Values.postgresql.auth.password -}}
{{- printf "postgres://%s:%s@%s:5432/%s?sslmode=disable" .Values.postgresql.auth.username $pw (include "relay.postgresql.fullname" .) .Values.postgresql.auth.database -}}
{{- else -}}
{{- required "external.pgDsn is required when postgresql.enabled=false (point at a RELAY-dedicated Postgres, not the shared platform one)" .Values.external.pgDsn -}}
{{- end -}}
{{- end -}}

{{/* Assembled RELAY_CH_DSN (bundled CH or external) */}}
{{- define "relay.chDsn" -}}
{{- if .Values.clickhouse.enabled -}}
{{- $pw := required "clickhouse.auth.password is required when clickhouse.enabled" .Values.clickhouse.auth.password -}}
{{- printf "clickhouse://%s:%s@%s:9000/%s" .Values.clickhouse.auth.username $pw (include "relay.clickhouse.fullname" .) .Values.clickhouse.auth.database -}}
{{- else -}}
{{- .Values.external.chDsn -}}
{{- end -}}
{{- end -}}

{{/* RELAY_REDIS_ADDR (bundled Valkey or external) */}}
{{- define "relay.redisAddr" -}}
{{- if .Values.valkey.enabled -}}
{{- printf "%s:6379" (include "relay.valkey.fullname" .) -}}
{{- else -}}
{{- .Values.external.redisAddr -}}
{{- end -}}
{{- end -}}
