{{/*
Expand the name of the chart.
*/}}
{{- define "vgpu-manager-dra-driver.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "vgpu-manager-dra-driver.fullname" -}}
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
{{- define "vgpu-manager-dra-driver.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "vgpu-manager-dra-driver.labels" -}}
helm.sh/chart: {{ include "vgpu-manager-dra-driver.chart" . }}
{{ include "vgpu-manager-dra-driver.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "vgpu-manager-dra-driver.selectorLabels" -}}
app.kubernetes.io/name: {{ include "vgpu-manager-dra-driver.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
The app name for the kubelet-plugin (DRA driver) DaemonSet
*/}}
{{- define "vgpu-manager-dra-driver.kubelet-plugin" -}}
{{- printf "%s-kubelet-plugin" ( include "vgpu-manager-dra-driver.fullname" . ) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
The app name for the Webhook
*/}}
{{- define "vgpu-manager-dra-driver.webhook" -}}
{{- printf "%s-webhook" ( include "vgpu-manager-dra-driver.fullname" . ) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Image registry secret name
*/}}
{{- define "vgpu-manager-dra-driver.imagePullSecrets" -}}
imagePullSecrets: {{ toYaml .Values.imagePullSecrets | nindent 2 }}
{{- end }}

{{/*
Full image name with tag
*/}}
{{- define "vgpu-manager-dra-driver.fullimage" -}}
{{- $tag := printf "%s" .Chart.AppVersion }}
{{- .Values.image -}}:{{- .Values.imageTag | default $tag -}}
{{- end }}

{{/*
Render the `webhooks:` body of the MutatingWebhookConfiguration.
Argument: a dict with "ctx" (the root context) and optional "caBundle" (base64-encoded CA).
When "caBundle" is empty the field is omitted (cert-manager injects it via annotation instead).
*/}}
{{- define "vgpu-manager-dra-driver.mutatingWebhooks" -}}
{{- $ctx := .ctx -}}
{{- $caBundle := .caBundle -}}
webhooks:
  - admissionReviewVersions:
      - v1beta1
    clientConfig:
      {{- if $caBundle }}
      caBundle: {{ $caBundle }}
      {{- end }}
      service:
        name: {{ include "vgpu-manager-dra-driver.webhook" $ctx }}
        namespace: {{ $ctx.Release.Namespace | quote }}
        path: /pods/mutate
        port: 443
    failurePolicy: {{ $ctx.Values.webhook.failurePolicy }}
    matchPolicy: Equivalent
    name: mutatepod.vgpu-manager.io
    namespaceSelector:
      matchExpressions:
        - key: vgpu-manager.io/ignore-webhook
          operator: NotIn
          values:
            - "true"
        {{- if $ctx.Values.webhook.excludeNamespaces }}
        - key: kubernetes.io/metadata.name
          operator: NotIn
          values:
          {{- toYaml $ctx.Values.webhook.excludeNamespaces | nindent 10 }}
        {{- end }}
    objectSelector:
      matchExpressions:
        - key: vgpu-manager.io/ignore-webhook
          operator: NotIn
          values:
            - "true"
    reinvocationPolicy: Never
    rules:
      - apiGroups:
          - ""
        apiVersions:
          - v1
        operations:
          - CREATE
          - UPDATE
        resources:
          - pods
        scope: '*'
      - apiGroups:
          - ""
        apiVersions:
          - v1
        operations:
          - UPDATE
        resources:
          - pods/status
        scope: '*'
    sideEffects: NoneOnDryRun
    timeoutSeconds: 10
{{- end -}}

{{/*
Render the `webhooks:` body of the ValidatingWebhookConfiguration (pods + resourceclaims).
Argument: a dict with "ctx" (the root context) and optional "caBundle" (base64-encoded CA).
*/}}
{{- define "vgpu-manager-dra-driver.validatingWebhooks" -}}
{{- $ctx := .ctx -}}
{{- $caBundle := .caBundle -}}
webhooks:
  - admissionReviewVersions:
      - v1beta1
    clientConfig:
      {{- if $caBundle }}
      caBundle: {{ $caBundle }}
      {{- end }}
      service:
        name: {{ include "vgpu-manager-dra-driver.webhook" $ctx }}
        namespace: {{ $ctx.Release.Namespace | quote }}
        path: /pods/validate
        port: 443
    failurePolicy: {{ $ctx.Values.webhook.failurePolicy }}
    matchPolicy: Equivalent
    name: validatepod.vgpu-manager.io
    namespaceSelector:
      matchExpressions:
        - key: vgpu-manager.io/ignore-webhook
          operator: NotIn
          values:
            - "true"
        {{- if $ctx.Values.webhook.excludeNamespaces }}
        - key: kubernetes.io/metadata.name
          operator: NotIn
          values:
          {{- toYaml $ctx.Values.webhook.excludeNamespaces | nindent 10 }}
        {{- end }}
    objectSelector:
      matchExpressions:
        - key: vgpu-manager.io/ignore-webhook
          operator: NotIn
          values:
            - "true"
    rules:
      - apiGroups:
          - ""
        apiVersions:
          - v1
        operations:
          - CREATE
          - DELETE
        resources:
          - pods
        scope: '*'
    sideEffects: NoneOnDryRun
    timeoutSeconds: 10
  - admissionReviewVersions:
      - v1beta1
    clientConfig:
      {{- if $caBundle }}
      caBundle: {{ $caBundle }}
      {{- end }}
      service:
        name: {{ include "vgpu-manager-dra-driver.webhook" $ctx }}
        namespace: {{ $ctx.Release.Namespace | quote }}
        path: /resourceclaim/validate
        port: 443
    failurePolicy: {{ $ctx.Values.webhook.failurePolicy }}
    matchPolicy: Equivalent
    name: validateresourceclaim.vgpu-manager.io
    namespaceSelector:
      matchExpressions:
        - key: vgpu-manager.io/ignore-webhook
          operator: NotIn
          values:
            - "true"
    objectSelector:
      matchExpressions:
        - key: vgpu-manager.io/ignore-webhook
          operator: NotIn
          values:
            - "true"
    rules:
      - apiGroups:
          - "resource.k8s.io"
        apiVersions:
          - v1beta1
          - v1beta2
          - v1
        operations:
          - CREATE
          - UPDATE
        resources:
          - resourceclaims
          - resourceclaimtemplates
        scope: "Namespaced"
      - apiGroups:
          - "resource.k8s.io"
        apiVersions:
          - v1
        operations:
          - UPDATE
        resources:
          - resourceclaims/status
        scope: "Namespaced"
    sideEffects: NoneOnDryRun
    timeoutSeconds: 10
{{- end -}}

{{/*
Get the resource.k8s.io API version used by DRA resources (DeviceClass, ...).

Priority:
  1. If .Values.resourceApiVersion is set, use that.
  2. Otherwise, return the highest version served by the cluster
     (v1 > v1beta2 > v1beta1), or empty string if none is found.
*/}}
{{- define "vgpu-manager-dra-driver.resourceApiVersion" -}}
{{- if .Values.resourceApiVersion }}
{{- .Values.resourceApiVersion }}
{{- else if .Capabilities.APIVersions.Has "resource.k8s.io/v1" -}}
resource.k8s.io/v1
{{- else if .Capabilities.APIVersions.Has "resource.k8s.io/v1beta2" -}}
resource.k8s.io/v1beta2
{{- else if .Capabilities.APIVersions.Has "resource.k8s.io/v1beta1" -}}
resource.k8s.io/v1beta1
{{- else -}}
resource.k8s.io/v1
{{- end -}}
{{- end -}}
