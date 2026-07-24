# Cluster Profile Controller

[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](https://opensource.org/licenses/Apache-2.0)
[![Go Report Card](https://goreportcard.com/badge/github.com/argoproj-labs/clusterprofile-integration-for-argocd)](https://goreportcard.com/report/github.com/argoproj-labs/clusterprofile-integration-for-argocd)

The [Cluster Profile API](https://multicluster.sigs.k8s.io/concepts/cluster-profile-api/) provides a standard way to describe and manage clusters. The Cluster Profile controller allows automatic registration of clusters from `ClusterProfile` resources in Argo CD by creating and managing `Secret`s corresponding to `ClusterProfile`s. This avoids having to manually register and unregister these clusters with Argo CD, and notably when using a cluster manager that generates and syncs `ClusterProfile` resources.

When reading the details below, it may be helpful to have concrete examples. See the [kind cluster](docs/cluster-profiles-kind-example.md), [GCP](docs/cluster-profiles-gcp-example.md), or [Open Cluster Management (OCM)](docs/cluster-profiles-ocm-example.md) quickstarts for specifics on how the controller can be used.

## Prerequisites and compatibility

- **Kubernetes**: v1.27+
- **Argo CD**: v3.5+
- **Go** (for local development): v1.26+

## How it works

### Cluster Profiles

The Cluster Profile controller watches for `ClusterProfile` custom resources. For each `ClusterProfile` it finds, it generates a corresponding Argo CD `Secret`, which then allows Argo CD to connect to and manage the remote cluster.

A `ClusterProfile` object looks like this:

```yaml
apiVersion: "multicluster.x-k8s.io/v1alpha1"
kind: ClusterProfile
metadata:
  name: my-cluster
  namespace: argocd
spec:
  clusterManager:
    name: my-cluster-profile-controller
  displayName: "My Cluster"
status:
  accessProviders:
  - name: my-provider
    cluster:
      server: https://my-cluster.example.com
```

The controller uses `status.accessProviders` to build the corresponding Argo CD `Secret`.

These `ClusterProfile` resources may be synced automatically by a cluster manager or created manually. The `ClusterProfile` CRD from the [Cluster Inventory API](https://github.com/kubernetes-sigs/cluster-inventory-api) must exist in the cluster before you deploy the controller with Helm.

Each generated `Secret` is created **in the same namespace as its source `ClusterProfile`**, with an owner reference back to it. Its name is normally `cluster-<ClusterProfile name>`. Names and label values that would exceed Kubernetes metadata limits use deterministic bounded encodings instead.

Generated `Secret`s are labeled with `argocd.argoproj.io/secret-type: cluster` and `argocd.argoproj.io/cluster-profile-name`, and also include labels from the source `ClusterProfile`. See the [architecture](docs/ARCHITECTURE.md) for naming and collision handling details.

### Namespace placement

Because Argo CD reads cluster `Secret`s only from its own namespace, create each `ClusterProfile` in the namespace of the Argo CD instance that should manage the cluster.

One shared controller can watch several namespaces (see `--cluster-profile-namespaces`) and serve one Argo CD instance per team namespace.

### Authentication

These secrets are used by the Argo CD Application controller to authenticate to remote clusters. The Cluster Profile controller can generate these secrets with one of two authentication methods: using built-in cloud provider authentication, or using a custom access providers file.

#### Built-in cloud provider authentication

If you are using a supported cloud provider (such as GCP), the Cluster Profile controller can generate a secret that uses the `argocd-k8s-auth` command to authenticate to the remote cluster.

To use this feature, the access provider name in the `ClusterProfile`'s status must start with `argo-cd-builtin-` followed by the provider's name (e.g., `argo-cd-builtin-gcp`). When the controller encounters an access provider with this prefix, it will automatically configure the generated Argo CD secret to use the `argocd-k8s-auth <provider>` command for authentication. The supported provider names are `gcp`, `aws`, and `azure`. See the [GCP example](docs/cluster-profiles-gcp-example.md) for more.

#### Custom access providers file

For other environments or custom authentication, part of the design of Cluster Profiles (unlike Secrets) is to keep authentication information separate from the ClusterProfile itself. This is achieved using an "access providers" file, which lists named access providers with `execConfig`s that specify how to authenticate to a cluster.

The access providers file would look something like this:

```json
{
  "providers": [
    {
      "name": "secretreader",
      "execConfig": {
        "apiVersion": "client.authentication.k8s.io/v1",
        "command": "<path-to-provider-plugin>",
        "provideClusterInfo": true
      }
    }
  ]
}
```

The Cluster Profile controller reads this file, finds an access provider whose name matches one in the Cluster Profile object's `Status.AccessProviders` field, and generates the Secret to use the provider's `execConfig` for the cluster connection.

To provide this file to the controller, configure the `argocd-clusterprofile-controller` with the `--clusterprofile-provider-file` argument (or `ARGOCD_CLUSTERPROFILE_CONTROLLER_CLUSTERPROFILE_PROVIDER_FILE` environment variable) and mount the file through a Secret or ConfigMap. The plugin binary must be available at the same path to the Argo CD components that use the resulting cluster Secrets; see the [architecture](docs/ARCHITECTURE.md) and [kind cluster example](docs/cluster-profiles-kind-example.md).

### Secret lifecycle

Each generated `Secret` carries an owner reference to its `ClusterProfile`, so when the `ClusterProfile` is deleted, Kubernetes garbage collection removes the corresponding `Secret` automatically.

The controller also removes the `Secret` it owns when the `ClusterProfile` stops advertising all access providers. If the controller cannot generate a `Secret` from the current `ClusterProfile` and access provider configuration, it keeps the existing controller-generated `Secret`. It deletes that `Secret` only when it can safely determine that the `ClusterProfile` no longer advertises the provider from which it was generated. See the [architecture](docs/ARCHITECTURE.md#access-loss-and-last-known-good-credentials) for detailed failure behavior.

## Installation and configuration

The Cluster Profile controller runs as a standalone deployment alongside your Argo CD installation.

Install the Helm chart from GHCR:

```bash
helm install argocd-clusterprofile-controller \
  oci://ghcr.io/argoproj-labs/clusterprofile-integration-for-argocd/argocd-clusterprofile-controller \
  --version 0.1.0 \
  --namespace argocd \
  --create-namespace
```

To deploy the Kustomize manifests from a source checkout instead:

```bash
kubectl apply -k artifacts/manifests
```

The Helm chart does not install the `clusterprofiles.multicluster.x-k8s.io` CRD.
Install the Cluster Inventory API CRD separately, or use a cluster where another
component already owns it.

### Deployment and high availability

The bundled Helm and Kustomize deployments enable leader election. For high availability, set Helm's `replicaCount` to at least 2 or use a Kustomize overlay to run multiple controller replicas.

The Helm chart can create a `VerticalPodAutoscaler` when `vpa.enabled` is true. The cluster must already provide the `autoscaling.k8s.io/v1` CRD, a VPA controller, and the Metrics Server.

Every namespace watched for `ClusterProfile`s is also a namespace where the controller writes `Secret`s. The Helm chart creates a `Role` in each configured namespace, or a `ClusterRole` when watching all namespaces. Wildcard mode therefore grants the controller read and write access to every `Secret` in the cluster. Prefer an explicit namespace list unless cluster-wide watching is required. The Kustomize manifests grant cluster-wide `Secret` access to support `--cluster-profile-namespaces='*'`.

### Configuration parameters

The controller binary can be configured via command-line arguments or equivalent environment variables. The defaults below apply when invoking the binary directly; packaged manifests may set explicit values, as described above. Standard kubeconfig flags are also available through `--help`.

| Argument | Environment Variable | Default | Description |
| --- | --- | --- | --- |
| `--clusterprofile-provider-file` | `ARGOCD_CLUSTERPROFILE_CONTROLLER_CLUSTERPROFILE_PROVIDER_FILE` | `""` | Path to the custom access providers file. |
| `--cluster-profile-namespaces` | `ARGOCD_CLUSTERPROFILE_CONTROLLER_NAMESPACES` | `""` | Comma-separated namespaces to watch for `ClusterProfile`s (defaults to active namespace). Use `'*'` to watch all namespaces (quote it to avoid shell globbing). |
| `--metrics-addr` | — | `":8080"` | Address on which the metrics endpoint listens. |
| `--probe-addr` | — | `":8081"` | Address on which the health and readiness endpoints listen. |
| `--enable-leader-election` | `ARGOCD_CLUSTERPROFILE_CONTROLLER_ENABLE_LEADER_ELECTION` | `false` | Enables leader election for HA/redundancy. |
| `--dry-run` | `ARGOCD_CLUSTERPROFILE_CONTROLLER_DRY_RUN` | `false` | Enable dry-run mode. |
| `--debug` | `ARGOCD_CLUSTERPROFILE_CONTROLLER_DEBUG` | `false` | Print debug logs (takes precedence over log level). |
| `--loglevel` | `ARGOCD_CLUSTERPROFILE_CONTROLLER_LOGLEVEL` | `"info"` | Set logging level (`debug`, `info`, `warn`, `error`). |
| `--logformat` | `ARGOCD_CLUSTERPROFILE_CONTROLLER_LOGFORMAT` | `"json"` | Set logging format (`json`, `text`). |

### Uninstalling

To uninstall a Helm release installed with the command above:

```bash
helm uninstall argocd-clusterprofile-controller --namespace argocd
```

To uninstall the Kustomize manifests:

```bash
kubectl delete -k artifacts/manifests
```

## Local development and contributing

Contributions are welcome!

### Building and testing

- **Run Unit Tests**:

  ```bash
  make test
  ```

- **Build Local Binary**:

  ```bash
  make build
  ```

- **Run Controller Locally**:

  ```bash
  make run
  ```

- **Build Docker Image**:

  ```bash
  make docker-build
  ```

- **Validate Helm chart**:

  ```bash
  make helm-lint
  make validate-values-schema
  make generate-helm-docs
  ```

  See the [Helm chart development guide](docs/developer-guide/helm-chart-development.md)
  for chart versioning and generation details.

## Community and governance

- **Slack**: Join the discussion in the `#argo-cluster-auth` channel on the [CNCF Slack](https://slack.cncf.io/).
- **Code of Conduct**: This project adheres to the [CNCF Code of Conduct](https://github.com/cncf/foundation/blob/main/code-of-conduct.md).
- **Issues & Feature Requests**: Please open an issue in this repository to report bugs or request new features.
