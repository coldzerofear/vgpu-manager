{{/*
Expand the name of the chart.
*/}}
{{- define "vgpu-manager.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "vgpu-manager.fullname" -}}
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
{{- define "vgpu-manager.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "vgpu-manager.labels" -}}
helm.sh/chart: {{ include "vgpu-manager.chart" . }}
{{ include "vgpu-manager.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "vgpu-manager.selectorLabels" -}}
app.kubernetes.io/name: {{ include "vgpu-manager.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
The app name for Scheduler
*/}}
{{- define "vgpu-manager.scheduler" -}}
{{- printf "%s-scheduler" ( include "vgpu-manager.fullname" . ) | trunc 63 | trimSuffix "-" -}}
{{- end }}

{{/*
The app name for DevicePlugin
*/}}
{{- define "vgpu-manager.device-plugin" -}}
{{- printf "%s-device-plugin" ( include "vgpu-manager.fullname" . ) | trunc 63 | trimSuffix "-" -}}
{{- end -}}


{{/*
Image registry secret name
*/}}
{{- define "vgpu-manager.imagePullSecrets" -}}
imagePullSecrets: {{ toYaml .Values.imagePullSecrets | nindent 2 }}
{{- end }}


