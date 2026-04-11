{{/*
Generate a chart-name short enough to survive the 63-char DNS-1123
label limit even when the release name is long. Used for the
Deployment, Service, Secret, ServiceAccount, etc. so every generated
resource is stable across upgrades.
*/}}
{{- define "cozytempl.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end }}

{{/*
Full qualified name with release prefix so multiple cozytempl
instances can live in the same namespace without colliding.
*/}}
{{- define "cozytempl.fullname" -}}
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
{{- end }}

{{/*
Chart label — shows up in `helm list` and annotations so an
operator scanning cluster resources can trace them back to this
release.
*/}}
{{- define "cozytempl.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end }}

{{/*
Standard labels applied to every resource the chart manages.
The app.kubernetes.io/* prefix is what controllers use to find
resources they own; do not change these without a migration plan.
*/}}
{{- define "cozytempl.labels" -}}
helm.sh/chart: {{ include "cozytempl.chart" . }}
{{ include "cozytempl.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- with .Values.commonLabels }}
{{ toYaml . }}
{{- end }}
{{- end }}

{{/*
Selector labels — the minimum set that survives rolling
upgrades so Deployment and Service match the same pods.
*/}}
{{- define "cozytempl.selectorLabels" -}}
app.kubernetes.io/name: {{ include "cozytempl.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
ServiceAccount name. Respects .Values.serviceAccount.name if
set so an operator can hand-roll the SA outside the chart.
*/}}
{{- define "cozytempl.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "cozytempl.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end }}

{{/*
Secret name. If existingSecret is set, the chart mounts that
Secret instead of creating its own.
*/}}
{{- define "cozytempl.secretName" -}}
{{- if .Values.config.existingSecret -}}
{{- .Values.config.existingSecret -}}
{{- else -}}
{{- include "cozytempl.fullname" . -}}
{{- end -}}
{{- end }}
