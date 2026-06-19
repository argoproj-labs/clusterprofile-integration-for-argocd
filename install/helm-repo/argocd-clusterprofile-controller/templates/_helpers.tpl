{{/*
Expand the name of the chart.
*/}}
{{- define "argocd-clusterprofile-controller.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "argocd-clusterprofile-controller.fullname" -}}
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
Expand the namespace of the release.
*/}}
{{- define "argocd-clusterprofile-controller.namespace" -}}
{{- default .Release.Namespace .Values.namespaceOverride | trunc 63 | trimSuffix "-" -}}
{{- end }}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "argocd-clusterprofile-controller.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Selector labels.
*/}}
{{- define "argocd-clusterprofile-controller.selectorLabels" -}}
app.kubernetes.io/name: {{ include "argocd-clusterprofile-controller.name" . }}
app.kubernetes.io/part-of: argocd
app.kubernetes.io/component: clusterprofile-controller
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Common labels.
*/}}
{{- define "argocd-clusterprofile-controller.labels" -}}
helm.sh/chart: {{ include "argocd-clusterprofile-controller.chart" . }}
{{ include "argocd-clusterprofile-controller.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Default image tag.
*/}}
{{- define "argocd-clusterprofile-controller.defaultTag" -}}
{{- default .Chart.AppVersion .Values.image.tag }}
{{- end }}

{{/*
Create the name of the service account to use.
*/}}
{{- define "argocd-clusterprofile-controller.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- if .Values.serviceAccount.name }}
{{- .Values.serviceAccount.name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- include "argocd-clusterprofile-controller.fullname" . }}
{{- end }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Name for resources used by Helm tests.
*/}}
{{- define "argocd-clusterprofile-controller.testResourceName" -}}
{{- printf "%s-test" (include "argocd-clusterprofile-controller.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Argo CD command params ConfigMap name.
*/}}
{{- define "argocd-clusterprofile-controller.cmdParamsConfigMapName" -}}
{{- default "argocd-cmd-params-cm" .Values.controller.argoCDCmdParams.configMapName | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Metrics port derived from controller.metricsAddr (e.g. ":8080" -> 8080).
*/}}
{{- define "argocd-clusterprofile-controller.metricsPort" -}}
{{- splitList ":" .Values.controller.metricsAddr | last }}
{{- end }}

{{/*
Health probe port derived from controller.probeAddr (e.g. ":8081" -> 8081).
*/}}
{{- define "argocd-clusterprofile-controller.probePort" -}}
{{- splitList ":" .Values.controller.probeAddr | last }}
{{- end }}

{{/*
RBAC policy rules shared by the namespaced Role and the cluster-scoped ClusterRole.
Pass a dict with "writeSecrets" set to true to grant write verbs on secrets/configmaps
(the namespaced Role needs them; the ClusterRole only reads).
*/}}
{{- define "argocd-clusterprofile-controller.rbacRules" -}}
- apiGroups:
    - argoproj.io
  resources:
    - applications
  verbs:
    - get
    - list
    - watch
- apiGroups:
    - argoproj.io
  resources:
    - appprojects
  verbs:
    - get
    - list
    - watch
- apiGroups:
    - ""
  resources:
    - events
  verbs:
    - create
    - get
    - list
    - patch
    - watch
- apiGroups:
    - ""
  resources:
    - secrets
    - configmaps
  verbs:
    {{- if .writeSecrets }}
    - create
    - delete
    - get
    - list
    - patch
    - update
    - watch
    {{- else }}
    - get
    - list
    - watch
    {{- end }}
- apiGroups:
    - multicluster.x-k8s.io
  resources:
    - clusterprofiles
  verbs:
    - get
    - list
    - patch
    - update
    - watch
- apiGroups:
    - multicluster.x-k8s.io
  resources:
    - clusterprofiles/finalizers
  verbs:
    - get
    - patch
    - update
- apiGroups:
    - coordination.k8s.io
  resources:
    - leases
  verbs:
    - create
- apiGroups:
    - coordination.k8s.io
  resources:
    - leases
  resourceNames:
    - clusterprofile.argoproj.io
  verbs:
    - get
    - update
    - create
{{- end }}
