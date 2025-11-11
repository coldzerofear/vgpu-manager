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
The app name for Webhook
*/}}
{{- define "vgpu-manager.webhook" -}}
{{- printf "%s-webhook" ( include "vgpu-manager.fullname" . ) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Image registry secret name
*/}}
{{- define "vgpu-manager.imagePullSecrets" -}}
imagePullSecrets: {{ toYaml .Values.imagePullSecrets | nindent 2 }}
{{- end }}

{{/*
Full image name with tag
*/}}
{{- define "vgpu-manager.fullimage" -}}
{{- $tag := printf "%s" .Chart.AppVersion }}
{{- .Values.image -}}:{{- .Values.imageTag | default $tag -}}
{{- end }}

{{/*
    Resolve the image tag for kubeScheduler.
*/}}
{{- define "kubeSchedulerImageTag" -}}
{{- if .Values.scheduler.kubeScheduler.imageTag }}
{{- .Values.scheduler.kubeScheduler.imageTag | trim -}}
{{- else }}
{{- include "defaultKubeVersion" . | trim -}}
{{- end }}
{{- end }}

{{/*
    Return the stripped Kubernetes version string by removing extra parts after semantic version number.
    v1.31.1+k3s1 -> v1.31.1
    v1.30.8-eks-2d5f260 -> v1.30.8
    v1.31.1 -> v1.31.1
*/}}
{{- define "defaultKubeVersion" -}}
{{ regexReplaceAll "^(v[0-9]+\\.[0-9]+\\.[0-9]+)(.*)$" .Capabilities.KubeVersion.Version "$1" }}
{{- end -}}