# argocd-clusterprofile-controller

Argo CD ClusterProfile controller for registering ClusterProfile resources as Argo CD clusters

Source code can be found here:

* <https://github.com/argoproj-labs/clusterprofile-integration-for-argocd>

## Release versions

For published and development chart versions, see the
[Helm chart development guide](../../docs/developer-guide/helm-chart-development.md#release-versions).

## Requirements

Kubernetes: `>=1.27.0-0`

## Vertical Pod Autoscaler

Set `vpa.enabled` to create a `VerticalPodAutoscaler` for the controller
Deployment. The cluster must already provide the `autoscaling.k8s.io/v1` CRD,
a VPA controller, and the Metrics Server; this chart does not install them.

## Monitoring

For Prometheus discovery and alert configuration, see
[Monitoring](../../docs/monitoring.md).

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| affinity | object | `{}` | Affinity rules for the controller pod. |
| apiVersionOverrides | object | `{}` | Override the monitoring resource API version with `apiVersionOverrides.monitoring`. Empty uses `monitoring.coreos.com/v1`. |
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
| controller.metrics.enabled | bool | `false` | Create a metrics Service. |
| controller.metrics.rules.additionalLabels | object | `{}` | Additional labels for the PrometheusRule. |
| controller.metrics.rules.annotations | object | `{}` | Annotations for the PrometheusRule. |
| controller.metrics.rules.enabled | bool | `false` | Create a PrometheusRule from rules.spec. |
| controller.metrics.rules.namespace | string | `""` | Namespace for the PrometheusRule. Empty uses the controller namespace. |
| controller.metrics.rules.selector | object | `{}` | Labels added to the PrometheusRule for selection by Prometheus. |
| controller.metrics.rules.spec | list | `[]` | Alerting and recording rules. See the `spec.groups[0].rules` list in the [inventory alert examples](../../artifacts/overlays/monitoring/prometheus-rule.yaml). |
| controller.metrics.service.annotations | object | `{}` | Extra annotations for the metrics Service. |
| controller.metrics.service.clusterIP | string | `""` | Metrics Service cluster IP. Empty lets Kubernetes assign an IP; `None` creates a headless Service when type is ClusterIP. |
| controller.metrics.service.labels | object | `{}` | Extra labels for the metrics Service. |
| controller.metrics.service.portName | string | `"http-metrics"` | Metrics Service port name referenced by the ServiceMonitor. |
| controller.metrics.service.servicePort | int | `8080` | Metrics Service port. |
| controller.metrics.service.type | string | `"ClusterIP"` | Metrics Service type. |
| controller.metrics.serviceMonitor.additionalLabels | object | `{}` | Additional labels for the ServiceMonitor. |
| controller.metrics.serviceMonitor.annotations | object | `{}` | Annotations for the ServiceMonitor. |
| controller.metrics.serviceMonitor.enabled | bool | `false` | Create a Prometheus Operator ServiceMonitor for the metrics Service. |
| controller.metrics.serviceMonitor.honorLabels | bool | `false` | Preserve labels exposed by the controller when they conflict with target labels. |
| controller.metrics.serviceMonitor.interval | string | `"30s"` | Interval between Prometheus scrapes. Empty omits the interval and uses the Prometheus configuration. |
| controller.metrics.serviceMonitor.metricRelabelings | list | `[]` | Relabeling rules applied before samples are ingested. |
| controller.metrics.serviceMonitor.namespace | string | `""` | Namespace for the ServiceMonitor. Empty uses the controller namespace. |
| controller.metrics.serviceMonitor.relabelings | list | `[]` | Relabeling rules applied before scraping. |
| controller.metrics.serviceMonitor.scheme | string | `""` | Metrics endpoint scheme. Empty uses the ServiceMonitor default (`http`). |
| controller.metrics.serviceMonitor.scrapeTimeout | string | `""` | Per-scrape timeout. Empty uses the Prometheus default. |
| controller.metrics.serviceMonitor.selector | object | `{}` | Labels added to the ServiceMonitor for selection by Prometheus. |
| controller.metrics.serviceMonitor.tlsConfig | object | `{}` | TLS configuration for the metrics endpoint. |
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
