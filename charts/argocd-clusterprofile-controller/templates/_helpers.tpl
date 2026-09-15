{{- define "argocd-clusterprofile-controller.name" -}}
{{- default .Chart.Name .Values.nameOverride }}
{{- end }}

{{- define "argocd-clusterprofile-controller.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride }}
{{- else }}
{{- $name := include "argocd-clusterprofile-controller.name" . }}
{{- if contains $name .Release.Name }}
{{- .Release.Name }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{- define "argocd-clusterprofile-controller.controller.fullname" -}}
{{- printf "%s-%s" (include "argocd-clusterprofile-controller.fullname" .) .Values.controller.name | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "argocd-clusterprofile-controller.controller.name" -}}
{{- printf "%s-%s" (include "argocd-clusterprofile-controller.name" .) .Values.controller.name | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "argocd-clusterprofile-controller.namespace" -}}
{{- default .Release.Namespace .Values.namespaceOverride -}}
{{- end }}

{{- define "argocd-clusterprofile-controller.apiVersions.monitoring" -}}
{{- default "monitoring.coreos.com/v1" .Values.apiVersionOverrides.monitoring -}}
{{- end }}

{{- define "argocd-clusterprofile-controller.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "argocd-clusterprofile-controller.selectorLabels" -}}
app.kubernetes.io/name: {{ include "argocd-clusterprofile-controller.controller.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: {{ .Values.controller.name }}
{{- end }}

{{- define "argocd-clusterprofile-controller.labels" -}}
helm.sh/chart: {{ include "argocd-clusterprofile-controller.chart" . }}
{{ include "argocd-clusterprofile-controller.selectorLabels" . }}
app.kubernetes.io/part-of: argocd
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "argocd-clusterprofile-controller.clusterProfileNamespacesArg" -}}
{{- $namespaces := include "argocd-clusterprofile-controller.clusterProfileNamespaces" . | fromJsonArray -}}
{{- if $namespaces -}}{{ join "," $namespaces }}{{- else -}}{{ include "argocd-clusterprofile-controller.namespace" . }}{{- end -}}
{{- end }}

{{- define "argocd-clusterprofile-controller.clusterProfileNamespaces" -}}
{{- toJson .Values.controller.clusterProfileNamespaces -}}
{{- end }}

{{- define "argocd-clusterprofile-controller.watchesAllClusterProfiles" -}}
{{- $namespaces := include "argocd-clusterprofile-controller.clusterProfileNamespaces" . | fromJsonArray -}}
{{- if has "*" $namespaces -}}true{{- else -}}false{{- end -}}
{{- end }}

{{- define "argocd-clusterprofile-controller.watchesReleaseNamespace" -}}
{{- $namespaces := include "argocd-clusterprofile-controller.clusterProfileNamespaces" . | fromJsonArray -}}
{{- $releaseNamespace := include "argocd-clusterprofile-controller.namespace" . -}}
{{- if not $namespaces -}}true{{- else if and (not (has "*" $namespaces)) (has $releaseNamespace $namespaces) -}}true{{- else -}}false{{- end -}}
{{- end }}

{{- define "argocd-clusterprofile-controller.clusterProfileClusterRoleName" -}}
{{- printf "%s-clusterprofiles" (include "argocd-clusterprofile-controller.controller.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "argocd-clusterprofile-controller.watchedNamespaceRules" -}}
- apiGroups:
    - multicluster.x-k8s.io
  resources:
    - clusterprofiles
  verbs:
    - get
    - list
    - watch
- apiGroups:
    - ""
  resources:
    - secrets
  verbs:
    - create
    - delete
    - get
    - list
    - patch
    - update
    - watch
- apiGroups:
    - events.k8s.io
  resources:
    - events
  verbs:
    - create
    - patch
{{- end }}

{{- define "argocd-clusterprofile-controller.localRules" -}}
- apiGroups:
    - ""
  resources:
    - events
  verbs:
    - create
    - patch
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
{{- end }}
