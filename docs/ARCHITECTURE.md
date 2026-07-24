# Architecture

This project makes clusters represented by Cluster Inventory API
`ClusterProfile` resources available to Argo CD as managed clusters.

It does this by translating ClusterProfile resources into Argo CD cluster
Secrets. The ClusterProfile controller writes each Secret into the namespace of
its source ClusterProfile and does not authenticate to registered clusters.
Argo CD components use the translated Secrets when accessing those clusters and
may execute configured provider plugins to obtain credentials.

## Component model

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

## Reconciliation flow

```mermaid
sequenceDiagram
    participant KubeAPI as Kubernetes API
    participant CPController as ClusterProfile controller
    participant ProviderFile as access providers file
    participant Secret as Argo CD cluster Secret

    KubeAPI->>CPController: ClusterProfile or owned Secret event
    CPController->>CPController: Resolve advertised access
    alt No access is advertised
        CPController->>Secret: Delete with UID/resourceVersion preconditions
    else Access is advertised
        CPController->>ProviderFile: Resolve custom provider when required
        alt Rendering succeeds
            CPController->>Secret: Create or update with optimistic locking
        else Rendering fails
            CPController->>Secret: Retain last-known-good data or delete revoked data
        end
    end
    CPController-->>KubeAPI: Reconcile complete
```

The Secret is created in the same namespace as the ClusterProfile, named
`cluster-<ClusterProfile name>`, with a deterministic shorter name when that
would exceed the Kubernetes name length limit. The
`argocd.argoproj.io/cluster-profile-name` label and annotation record the
source ClusterProfile name. A controller owner reference ties the Secret to
its ClusterProfile: Kubernetes garbage collection deletes the Secret together
with the ClusterProfile, and the controller watches its Secrets through the
same reference, so out-of-band edits or deletions are reconciled back.

The controller mutates or deletes only Secrets that this owner reference marks
as its own. It does not adopt ownerless Secrets or overwrite a Secret owned by
another object; those cases are reported as collisions for an operator to
resolve. Writes and deletes carry preconditions, so a concurrent writer causes
a retry instead of silently lost data.

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
| `data.name` | `ClusterProfile.metadata.name` |
| `data.server` | Selected `AccessProvider.cluster.server` |
| `data.config` | JSON-encoded Argo CD `ClusterConfig`, including TLS, proxy, compression, and optional `execProviderConfig` data |

The current implementation uses
`access.Config.BuildConfigFromCP(clusterProfile)` from the Cluster Inventory API
library to resolve authentication material and exec configuration. The
resulting `rest.Config` is an intermediate representation only: the controller
never uses it to contact the registered cluster, and the selected
`AccessProvider.cluster` remains the source of truth for the connection
settings written into Argo CD's `ClusterConfig` JSON.

### Access loss and last-known-good credentials

An annotation on each generated Secret records the access provider that
produced it. After a render failure the controller keeps the last successfully
written Secret and retries while `ClusterProfile.status` still advertises that
provider, and deletes the Secret once status stops advertising access or that
provider.

Editing the access providers file therefore never revokes access. To revoke
access, remove the provider entry from status.

## Runtime authentication flow

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

## Custom provider resolution

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

## Cluster information for exec plugins

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

## Built-in providers

Access provider names with the `argo-cd-builtin-` prefix are handled without an
access providers file. For example, `argo-cd-builtin-gcp` is translated into an
Argo CD cluster Secret that uses:

```text
argocd-k8s-auth gcp
```

This path is separate from the custom provider plugin flow. Built-in providers
use the authentication commands available in the Argo CD runtime image.

## Deployment invariants

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
