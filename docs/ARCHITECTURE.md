# Architecture

This project makes clusters represented by Cluster Inventory API
`ClusterProfile` resources available to Argo CD as managed clusters.

It does this by translating ClusterProfile resources into Argo CD cluster
Secrets. The ClusterProfile controller writes those Secrets and does not
authenticate to registered clusters. Argo CD components use the translated
Secrets when accessing those clusters and may execute configured provider
plugins to obtain credentials.

## Component Model

```mermaid
flowchart LR
    CP["ClusterProfile"]
    ProviderFile["access providers file"]
    Controller["ClusterProfile controller"]
    ClusterSecret["Argo CD cluster Secret"]
    ArgoComponents["argocd-application-controller<br/>argocd-server"]
    PluginBinary["plugin binary"]
    RemoteCluster["registered cluster API"]

    Controller watchCP@-->|"watch"| CP
    Controller -.->|"read mounted file"| ProviderFile
    Controller writeSecret@-->|"create/update"| ClusterSecret
    ArgoComponents readSecret@-->|"read"| ClusterSecret
    ArgoComponents -.->|"mounted binary"| PluginBinary
    ArgoComponents readProviderBinary@-->|"read"| PluginBinary
    ArgoComponents clusterTraffic@-->|"sync/API/UI operations"| RemoteCluster

    classDef trafficEdge stroke-dasharray: 6 4, stroke-dashoffset: 24, animation: dash 1s linear infinite;
    class watchCP,writeSecret,readSecret,readProviderBinary,clusterTraffic trafficEdge;
```

The access providers file and the plugin binary are deliberately deployed to
different places:

| Artifact | Mounted into | Purpose |
| --- | --- | --- |
| Access providers file | `argocd-clusterprofile-controller` | Describes named providers and the `execConfig` that should be written into Argo CD cluster Secrets. |
| Plugin binary | Argo CD components that use Argo CD cluster Secrets, normally `argocd-application-controller` and `argocd-server` | Produces credentials at runtime through the Kubernetes exec credential protocol. |

Mounting the plugin binary into the ClusterProfile controller is unnecessary.
The controller only writes the command path into the cluster Secret; it never
executes that command.

## Reconciliation Flow

```mermaid
sequenceDiagram
    participant KubeAPI as Kubernetes API
    participant CPController as ClusterProfile controller
    participant ProviderFile as access providers file
    participant Secret as Argo CD cluster Secret

    KubeAPI->>CPController: ClusterProfile add/update/delete event
    CPController->>ProviderFile: Resolve matching provider by name
    CPController->>CPController: Build Argo CD ClusterConfig
    CPController->>Secret: Create or update metadata, data.name, data.server, data.config
    CPController-->>KubeAPI: Reconcile complete
```

For a custom access provider, the controller:

1. Reads the access providers file configured by
   `--clusterprofile-provider-file`.
2. Finds the provider whose `name` matches the provider advertised in
   `ClusterProfile.status.accessProviders`.
3. Combines the provider's `execConfig` with the selected
   `AccessProvider.cluster` data from the ClusterProfile.
4. Serializes the result into an Argo CD cluster Secret.

The resulting cluster Secret contains:

| Secret field | Source |
| --- | --- |
| `metadata.name` | Generated name, unique per ClusterProfile `<namespace>/<name>`; treat it as opaque and discover Secrets via the labels below |
| `metadata.labels["argocd.argoproj.io/cluster-profile-namespace"]` | ClusterProfile namespace |
| `metadata.labels["argocd.argoproj.io/cluster-profile-name"]` | ClusterProfile name, truncated if it exceeds the label value limit |
| `data.name` | Full `<namespace>/<name>` of the source ClusterProfile |
| `data.server` | Selected `AccessProvider.cluster.server` |
| `data.config` | JSON-encoded Argo CD `ClusterConfig`, including TLS data and optional `execProviderConfig` |

The current implementation uses
`access.Config.BuildConfigFromCP(clusterProfile)` from the Cluster Inventory API
library to resolve the provider and build a client-go `rest.Config`. That
`rest.Config` is an intermediate representation only. This controller does not
use it to contact the registered cluster; it maps the resolved values into Argo
CD's `ClusterConfig` JSON.

## Runtime Authentication Flow

```mermaid
sequenceDiagram
    participant Argo as Argo CD component
    participant Secret as Argo CD cluster Secret
    participant Plugin as plugin binary
    participant Cluster as registered cluster API

    Argo->>Secret: Read Argo CD cluster Secret
    Argo->>Plugin: Execute provider when credentials are needed
    Plugin-->>Argo: Return credentials
    Argo->>Cluster: Access cluster with those credentials
```

The command in `execProviderConfig.command` is interpreted in the filesystem of
the Argo CD component that uses the cluster Secret, not in the filesystem of the
ClusterProfile controller.

Because the cluster Secret stores one command path, every Argo CD component that
can execute the provider must mount the binary at that same path. In a normal
Argo CD installation, mount the plugin binary into both:

- `argocd-application-controller`
- `argocd-server`

The application controller uses the cluster Secret for reconciliation and sync.
The server can also use the same cluster Secret for API and UI operations such
as cluster access checks and resource queries.

The provider is executed when credentials are needed. Returned credentials can
be cached, so the provider may not run for every request. If the binary is
present in one component but missing from another, the component without the
binary will fail when it tries to use the cluster Secret, even though the
ClusterProfile controller reconciled the Secret successfully.

## Custom Provider Resolution

Custom providers use an access providers file. The file is read by the
ClusterProfile controller, but the configured command is executed later by Argo
CD.

```json
{
  "providers": [
    {
      "name": "secretreader",
      "execConfig": {
        "command": "/plugins/secretreader/bin/secretreader-plugin",
        "apiVersion": "client.authentication.k8s.io/v1",
        "provideClusterInfo": true
      }
    }
  ]
}
```

In this example:

- The ClusterProfile controller needs the file containing this JSON.
- `argocd-application-controller` and `argocd-server` need the executable at
  `/plugins/secretreader/bin/secretreader-plugin`.
- The ClusterProfile controller does not need the executable at that path.

The provider name is the join key between the ClusterProfile and the access
providers file. A ClusterProfile status entry like this selects the provider
above:

```yaml
status:
  accessProviders:
    - name: secretreader
      cluster:
        server: https://example-cluster
```

## Cluster Information for Exec Plugins

When `provideClusterInfo` is true, the Kubernetes exec credential protocol
passes cluster information to the exec plugin through the `KUBERNETES_EXEC_INFO`
environment variable. This can include the cluster server, certificate authority
data, and provider-specific configuration.

Cluster-specific, non-secret plugin configuration should be carried in the
ClusterProfile cluster extension named `client.authentication.k8s.io/exec`. The
controller preserves this extension in the resulting Argo CD
`execProviderConfig.config`, so Argo CD can pass it to the exec plugin.

For example, `secretreader` can receive the cluster name through this extension:

```yaml
extensions:
  - name: client.authentication.k8s.io/exec
    extension:
      clusterName: spoke-cluster
```

Secret material should not be stored in the ClusterProfile. A provider such as
`secretreader` should read secrets at runtime using the identity of the Argo CD
component that executes it.

If a provider reads Kubernetes resources at runtime, grant the required RBAC to
the ServiceAccounts of every Argo CD component that can execute the provider.
For standard usage, that means the ServiceAccounts used by
`argocd-application-controller` and `argocd-server`.

## Built-In Providers

Access provider names with the `argo-cd-builtin-` prefix are handled without an
access providers file. For example, `argo-cd-builtin-gcp` is translated into an
Argo CD cluster Secret that uses:

```text
argocd-k8s-auth gcp
```

This path is separate from the custom provider plugin flow. Built-in providers
use the authentication commands available in the Argo CD runtime image.

## Deployment Invariants

The following invariants keep the integration predictable:

1. The ClusterProfile controller has the access providers file when custom
   providers are used.
2. The ClusterProfile controller does not require plugin binaries.
3. Every Argo CD component that uses Argo CD cluster Secrets has the plugin
   binary mounted.
4. The plugin binary path is identical across those Argo CD components.
5. The `execProviderConfig.command` value in the access providers file points to
   that shared Argo CD runtime path.
6. Runtime RBAC for provider plugins is granted to the Argo CD component
   identities that execute them, not to the ClusterProfile controller identity.

These invariants are especially important when using image volumes or
initContainers to install provider plugins. The mechanism used to place the
binary can vary, but the command path seen by Argo CD must remain the same in
each executing component.
