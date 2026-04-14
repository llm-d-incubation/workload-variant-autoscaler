{{/*
Expand the name of the chart.
*/}}
{{- define "workload-variant-autoscaler.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "workload-variant-autoscaler.fullname" -}}
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
Create chart name and version as used by the chart label.
*/}}
{{- define "workload-variant-autoscaler.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "workload-variant-autoscaler.labels" -}}
helm.sh/chart: {{ include "workload-variant-autoscaler.chart" . }}
{{ include "workload-variant-autoscaler.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "workload-variant-autoscaler.selectorLabels" -}}
app.kubernetes.io/name: {{ include "workload-variant-autoscaler.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Create a name for cluster-scoped resources (ClusterRole, ClusterRoleBinding).
Appends the release namespace to the fullname to ensure uniqueness when multiple
installations exist on the same cluster in different namespaces.
*/}}
{{- define "workload-variant-autoscaler.clusterResourceName" -}}
{{- printf "%s-%s" (include "workload-variant-autoscaler.fullname" .) .Release.Namespace | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create the name of the service account to use
*/}}
{{- define "workload-variant-autoscaler.serviceAccountName" -}}
{{- default (include "workload-variant-autoscaler.fullname" .) .Values.serviceAccount.name }}
{{- end }}
