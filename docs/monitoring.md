# Monitoring

Use Prometheus to monitor the controller and configure alerts.

## Metrics endpoint

The controller listens on `:8080` by default and serves metrics at `/metrics`.
Helm's `controller.metricsPort` sets the Pod's listener port.

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
