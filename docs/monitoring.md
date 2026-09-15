# Monitoring

Use Prometheus to monitor the controller and configure alerts.

## Metrics endpoint

The controller listens on `:8080` by default and serves metrics at `/metrics`.
Helm's `controller.metricsPort` sets the Pod's listener port.
`controller.metrics.enabled` controls Service and monitoring resource creation;
the Pod's endpoint remains available when this setting is false.

| Installation | Example Service endpoint | Required condition |
| --- | --- | --- |
| Helm release `cpia` in namespace `argocd` | `http://cpia-argocd-clusterprofile-controller.argocd.svc.cluster.local:8080/metrics` | `controller.metrics.enabled=true`, with default names and Service port |
| Kustomize in namespace `argocd` | `http://argocd-clusterprofile-controller.argocd.svc.cluster.local:8080/metrics` | Service included in the base manifests |

The endpoint includes controller-runtime metrics and the custom metrics below:

| Metric | Labels | Description |
| --- | --- | --- |
| `argocd_clusterprofile_inventory_member_groups` | `inventory_namespace`, `resolution` | Count active non-empty member ID groups. `resolution` is `unique`, `duplicate`, or `ambiguous`. |
| `argocd_clusterprofile_inventory_member_conflict_group_size` | `inventory_namespace`, `inventory_member_id`, `resolution` | Count active ClusterProfiles in each `duplicate` or `ambiguous` member ID group. Only conflicts produce a series. |
| `argocd_clusterprofile_inventory_member_id_invalid_profiles` | `inventory_namespace` | Count active ClusterProfiles whose `multicluster.x-k8s.io/inventory-member-id` label is present but empty. These profiles are not deduplicated. |
| `argocd_clusterprofile_secret_changes_total` | `inventory_namespace`, `operation`, `dry_run` | Count successful controller-issued `create`, `update`, and `delete` operations on Argo CD cluster Secrets. No-op reconciles, failed requests, already-missing Secrets, and Kubernetes garbage collection are not counted. |
| `argocd_clusterprofile_inventory_collection_errors_total` | none | Count failures to list ClusterProfiles from the controller cache while serving metrics. The last successful state snapshot remains visible after a failure. |

Terminating ClusterProfiles are excluded from inventory state gauges.
`inventory_namespace` identifies the ClusterProfile or generated Secret's
namespace. Prometheus discovery can separately attach `namespace` for the
controller's scrape target, so the two labels coexist with `honorLabels: false`.
The same member ID in `argocd-prod` and `argocd-dev` produces two independent
inventory groups.

### Queries with multiple controller replicas

Every replica reads the same Kubernetes state from its cache. Use `max` for
state gauges so identical replicas are not added together:

```promql
max by (inventory_namespace, inventory_member_id) (
  argocd_clusterprofile_inventory_member_conflict_group_size{resolution="duplicate"}
)
```

Operation counters are process-local. Sum their rates across replicas:

```promql
sum by (inventory_namespace, operation, dry_run) (
  rate(argocd_clusterprofile_secret_changes_total[5m])
)
```

## Prometheus Operator with Helm

The metrics Service, ServiceMonitor, and PrometheusRule are disabled by default.
The following values enable the Service and a ServiceMonitor for a Prometheus
instance that selects resources with `release: kube-prometheus-stack`:

```yaml
controller:
  metrics:
    enabled: true
    serviceMonitor:
      enabled: true
      additionalLabels:
        release: kube-prometheus-stack
```

The ServiceMonitor's `additionalLabels` must match the Prometheus instance's
`serviceMonitorSelector`. For rules, `controller.metrics.rules.enabled=true`
creates a PrometheusRule while metrics are enabled; `rules.spec` supplies the
rules and `rules.additionalLabels` must match the Prometheus `ruleSelector`.

See the [chart values](../charts/argocd-clusterprofile-controller/README.md#values)
for API version overrides, Service options, and scrape settings.

### Inventory-member alert examples

The [monitoring overlay's PrometheusRule](../artifacts/overlays/monitoring/prometheus-rule.yaml)
contains the alert definitions. To use them with Helm, set
`controller.metrics.rules.spec` to its `spec.groups[0].rules` list.

## Kustomize monitoring overlay

The base `artifacts/manifests` includes the metrics Service. The
`artifacts/overlays/monitoring` overlay adds a ServiceMonitor and a PrometheusRule
with inventory-member alerts.

The overlay uses the `argocd` namespace and has no installation-specific
Prometheus discovery label. If the Prometheus instance selects resources using
a label such as `release: kube-prometheus-stack`, add that label to both
`service-monitor.yaml` and `prometheus-rule.yaml` in a site-specific overlay.
