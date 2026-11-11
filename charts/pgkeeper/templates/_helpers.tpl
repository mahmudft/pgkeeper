{{- define "pgkeeper.fullname" -}}
{{- if contains .Chart.Name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name .Chart.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "pgkeeper.labels" -}}
app.kubernetes.io/name: pgkeeper
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{- end -}}

{{- define "pgkeeper.selectorLabels" -}}
app.kubernetes.io/name: pgkeeper
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "pgkeeper.webhookService" -}}
{{ include "pgkeeper.fullname" . }}-webhook
{{- end -}}
