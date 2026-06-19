# argocd-clusterprofile-controller

![Version: 0.1.0](https://img.shields.io/badge/Version-0.1.0-informational?style=flat-square) ![Type: application](https://img.shields.io/badge/Type-application-informational?style=flat-square) ![AppVersion: main](https://img.shields.io/badge/AppVersion-main-informational?style=flat-square)

Argo CD ClusterProfile controller for registering ClusterProfile resources as Argo CD clusters

**Homepage:** <https://github.com/argoproj-labs/clusterprofile-integration-for-argocd>

## Maintainers

| Name | Email | Url |
| ---- | ------ | --- |
| Argo Project Maintainers |  | <https://github.com/argoproj-labs/clusterprofile-integration-for-argocd> |

## Source Code

* <https://github.com/argoproj-labs/clusterprofile-integration-for-argocd>

## Requirements

Kubernetes: `>=1.26.0-0`

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| affinity | object | `{}` | Affinity rules for the controller pod. |
| containerSecurityContext | object | `{"allowPrivilegeEscalation":false,"capabilities":{"drop":["ALL"]},"readOnlyRootFilesystem":true,"runAsNonRoot":true,"seccompProfile":{"type":"RuntimeDefault"}}` | Container-level security context. |
| controller.argoCDCmdParams.configMapName | string | `"argocd-cmd-params-cm"` | ConfigMap name containing Argo CD command parameters. |
| controller.argoCDCmdParams.enabled | bool | `true` | Read optional Argo CD command parameter keys from a ConfigMap. |
| controller.args | list | `[]` | Extra command-line arguments appended after the chart-managed arguments. |
| controller.clusterProfileNamespaces | list | `[]` | Namespaces to watch for ClusterProfile resources. Empty means the release namespace unless clusterScoped is true. |
| controller.clusterProfileProvidersFile | string | `""` | Path to a mounted ClusterProfile providers file. |
| controller.clusterScoped | bool | `false` | Watch ClusterProfile resources in all namespaces. When rbac.create=true, cluster-scoped RBAC is created automatically. |
| controller.debug | bool | `false` | Enable debug logging. Takes precedence over logLevel. |
| controller.dryRun | bool | `false` | Enable dry-run mode. |
| controller.enableLeaderElection | bool | `false` | Enable controller-runtime leader election. |
| controller.extraEnv | list | `[]` | Extra environment variables for the controller container. |
| controller.extraEnvFrom | list | `[]` | Extra envFrom entries for the controller container. |
| controller.extraVolumeMounts | list | `[]` | Extra volume mounts for the controller container. |
| controller.extraVolumes | list | `[]` | Extra volumes for the controller pod. |
| controller.logFormat | string | `""` | Explicit log format (`json` or `text`). Empty keeps the controller default or Argo CD cmd params value. |
| controller.logLevel | string | `""` | Explicit log level (`debug`, `info`, `warn`, `error`). Empty keeps the controller default or Argo CD cmd params value. |
| controller.metricsAddr | string | `":8080"` | Metrics bind address passed to the controller. |
| controller.probeAddr | string | `":8081"` | Health probe bind address passed to the controller. |
| fullnameOverride | string | `""` | Override the fully-qualified resource name. |
| global | object | `{}` | Global values reserved for parent charts. |
| image.pullPolicy | string | `"IfNotPresent"` | Image pull policy for the controller container. |
| image.repository | string | `"ghcr.io/argoproj-labs/clusterprofile-integration-for-argocd"` | Container image repository for the controller. |
| image.tag | string | `""` | Overrides the image tag whose default is the chart appVersion. |
| imagePullSecrets | list | `[]` | Image pull secrets for the controller pod. |
| nameOverride | string | `""` | Override the chart name used in `app.kubernetes.io/name`. |
| namespaceOverride | string | `""` | Override the Kubernetes namespace used in rendered namespaced resources. |
| networkPolicy.enabled | bool | `true` | Create a NetworkPolicy allowing access to the metrics port. |
| networkPolicy.ingress.namespaceSelector | object | `{}` | Namespace selector allowed to access the metrics port. |
| nodeSelector | object | `{"kubernetes.io/os":"linux"}` | Node selector for the controller pod. |
| podAnnotations | object | `{}` | Extra annotations for the controller pods. |
| podLabels | object | `{}` | Extra labels for the controller pods. |
| podSecurityContext | object | `{}` | Pod-level security context. |
| priorityClassName | string | `""` | Priority class name for the controller pod. |
| rbac.create | bool | `true` | Create RBAC resources for the controller. Cluster-scoped RBAC is created only when controller.clusterScoped=true. |
| replicaCount | int | `1` | Number of controller replicas. |
| resources | object | `{}` | Resource requests and limits for the controller container. |
| service.metrics.annotations | object | `{}` | Extra annotations for the metrics Service. |
| service.metrics.enabled | bool | `true` | Create a metrics Service. |
| service.metrics.port | int | `8080` | Metrics Service port. |
| service.metrics.type | string | `"ClusterIP"` | Metrics Service type. |
| serviceAccount.annotations | object | `{}` | Extra annotations for the service account. |
| serviceAccount.create | bool | `true` | Create a service account for the controller. |
| serviceAccount.labels | object | `{}` | Extra labels for the service account. |
| serviceAccount.name | string | `""` | Service account name. Generated when empty and create is true. |
| tests.enabled | bool | `false` | Enable Helm chart tests. |
| tests.image | string | `"bitnamilegacy/kubectl"` | Test image repository. |
| tests.tag | string | `"1.33.4"` | Test image tag. |
| tolerations | list | `[]` | Tolerations for the controller pod. |
| topologySpreadConstraints | list | `[]` | Topology spread constraints for the controller pod. |

----------------------------------------------
Autogenerated from chart metadata using [helm-docs v1.14.2](https://github.com/norwoodj/helm-docs/releases/v1.14.2)
