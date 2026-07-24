# argocd-clusterprofile-controller

Argo CD ClusterProfile controller for registering ClusterProfile resources as Argo CD clusters

Source code can be found here:

* <https://github.com/argoproj-labs/clusterprofile-integration-for-argocd>

## Release versions

The source chart uses placeholder `version` and `appVersion` values. Published
chart artifacts are stamped from the Git tag by the release workflow. For local
installs from this checkout, set an explicit image tag such as `main` or a
locally built tag.

## Requirements

Kubernetes: `>=1.27.0-0`

## Vertical Pod Autoscaler

Set `vpa.enabled` to create a `VerticalPodAutoscaler` for the controller
Deployment. The cluster must already provide the `autoscaling.k8s.io/v1` CRD,
a VPA controller, and the Metrics Server; this chart does not install them.

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| affinity | object | `{}` | Affinity rules for the controller pod. |
| containerSecurityContext | object | `{"allowPrivilegeEscalation":false,"capabilities":{"drop":["ALL"]},"readOnlyRootFilesystem":true,"runAsNonRoot":true,"seccompProfile":{"type":"RuntimeDefault"}}` | Container-level security context. |
| controller.argoCDCmdParams.configMapName | string | `"argocd-cmd-params-cm"` | ConfigMap name containing Argo CD command parameters. |
| controller.argoCDCmdParams.enabled | bool | `true` | Read optional Argo CD command parameter keys from a ConfigMap. |
| controller.args | list | `[]` | Extra command-line arguments for the controller. |
| controller.clusterProfileNamespaces | list | `[]` | Namespaces to watch for ClusterProfile resources. Empty uses the release namespace; `*` watches all namespaces. |
| controller.clusterProfileProvidersFile | string | `""` | Path to a mounted ClusterProfile providers file. |
| controller.debug | bool | `false` | Enable debug logging. Takes precedence over logLevel. |
| controller.dryRun | bool | `false` | Enable dry-run mode. |
| controller.extraEnv | list | `[]` | Extra environment variables for the controller container. |
| controller.extraEnvFrom | list | `[]` | Extra envFrom entries for the controller container. |
| controller.extraVolumeMounts | list | `[]` | Extra volume mounts for the controller container. |
| controller.extraVolumes | list | `[]` | Extra volumes for the controller pod. |
| controller.logFormat | string | `""` | Explicit log format (`json` or `text`). Empty keeps the controller default or Argo CD cmd params value. |
| controller.logLevel | string | `""` | Explicit log level (`debug`, `info`, `warn`, `error`). Empty keeps the controller default or Argo CD cmd params value. |
| controller.metricsPort | int | `8080` | Metrics port. |
| controller.name | string | `"clusterprofile-controller"` | Controller component name. |
| controller.probePort | int | `8081` | Health probe port. |
| fullnameOverride | string | `""` | String to fully override the base fully-qualified resource name. |
| global | object | `{}` | Global values reserved for parent charts. |
| image.pullPolicy | string | `"IfNotPresent"` | Image pull policy for the controller container. |
| image.repository | string | `"ghcr.io/argoproj-labs/clusterprofile-integration-for-argocd"` | Container image repository for the controller. |
| image.tag | string | `""` | Overrides the image tag whose default is the chart appVersion. |
| imagePullSecrets | list | `[]` | Image pull secrets for the controller pod. |
| nameOverride | string | `"argocd"` | Provide a name in place of `argocd`. |
| namespaceOverride | string | `""` | Override the Kubernetes namespace used in rendered namespaced resources. |
| networkPolicy.enabled | bool | `false` | Create a NetworkPolicy allowing access to the metrics port. |
| networkPolicy.ingress.namespaceSelector | object | `{}` | Namespace selector allowed to access the metrics port. |
| nodeSelector | object | `{"kubernetes.io/os":"linux"}` | Node selector for the controller pod. |
| podAnnotations | object | `{}` | Extra annotations for the controller pods. |
| podDisruptionBudget.annotations | object | `{}` | Extra annotations for the PodDisruptionBudget. |
| podDisruptionBudget.enabled | bool | `true` | Create a PodDisruptionBudget for the controller. |
| podDisruptionBudget.labels | object | `{}` | Extra labels for the PodDisruptionBudget. |
| podDisruptionBudget.maxUnavailable | int | `1` | Maximum number or percentage of controller Pods that may be unavailable. Set minAvailable to null when using this field. |
| podDisruptionBudget.minAvailable | string | `nil` | Minimum number or percentage of controller Pods that must remain available. Set maxUnavailable to null when using this field. |
| podDisruptionBudget.unhealthyPodEvictionPolicy | string | `""` | Policy for evicting unhealthy Pods. Empty uses the Kubernetes default. |
| podLabels | object | `{}` | Extra labels for the controller pods. |
| podSecurityContext | object | `{}` | Pod-level security context. |
| priorityClassName | string | `""` | Priority class name for the controller pod. |
| rbac.create | bool | `true` | Create RBAC resources for the controller. |
| replicaCount | int | `1` | Number of controller replicas. |
| resources | object | `{"limits":{"memory":"256Mi"},"requests":{"cpu":"10m","memory":"128Mi"}}` | Resource requests and limits for the controller container. |
| service.metrics.annotations | object | `{}` | Extra annotations for the metrics Service. |
| service.metrics.enabled | bool | `true` | Create a metrics Service. |
| service.metrics.port | int | `8080` | Metrics Service port. |
| service.metrics.type | string | `"ClusterIP"` | Metrics Service type. |
| serviceAccount.annotations | object | `{}` | Extra annotations for the service account. |
| serviceAccount.create | bool | `true` | Create a service account for the controller. |
| serviceAccount.labels | object | `{}` | Extra labels for the service account. |
| serviceAccount.name | string | `"argocd-clusterprofile-controller"` | Controller service account name. |
| terminationGracePeriodSeconds | int | `30` | Pod termination grace period in seconds. |
| tolerations | list | `[]` | Tolerations for the controller pod. |
| topologySpreadConstraints | list | `[{"matchLabelKeys":["pod-template-hash"],"maxSkew":1,"topologyKey":"kubernetes.io/hostname","whenUnsatisfiable":"ScheduleAnyway"}]` | Topology spread constraints for the controller pods. |
| vpa.annotations | object | `{}` | Extra annotations for the VerticalPodAutoscaler. |
| vpa.containerPolicy | object | `{}` | VPA policy for the controller container, excluding `containerName`. |
| vpa.enabled | bool | `false` | Create a VerticalPodAutoscaler for the controller. |
| vpa.labels | object | `{}` | Extra labels for the VerticalPodAutoscaler. |
| vpa.updateMode | string | `"Recreate"` | VPA update mode. |
