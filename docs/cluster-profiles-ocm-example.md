# Using Cluster Profiles with Open Cluster Management (OCM)

This guide demonstrates how to use [OCM's ClusterProfile Access Providers](https://open-cluster-management.io/docs/scenarios/clusterprofile-access-providers/) to automatically register OCM managed clusters in Argo CD via ClusterProfile resources.

OCM automatically creates and manages ClusterProfile objects for all managed clusters bound to a namespace. This works on **any Kubernetes distribution** where OCM is installed.

## Prerequisites

- [kubectl](https://kubernetes.io/docs/tasks/tools/)
- [clusteradm](https://open-cluster-management.io/docs/getting-started/installation/start-the-control-plane/) (OCM CLI)
- [Helm](https://helm.sh/docs/intro/install/) v3+
- Two Kubernetes clusters (this guide uses [Kind](https://kind.sigs.k8s.io/) as an example, but any Kubernetes distribution works)

Install `clusteradm`:

```bash
CLUSTERADM_VERSION=v1.3.1
CLUSTERADM_INSTALLER_SHA256=6196ae41931286bd25c1fed0fb114b2a4d0c5a6238db08beb5dade72da46e05f
installer="$(mktemp)"
trap 'rm -f "${installer}"' EXIT

curl -fsSL \
  -o "${installer}" \
  "https://raw.githubusercontent.com/open-cluster-management-io/clusteradm/${CLUSTERADM_VERSION}/install.sh"

if command -v sha256sum >/dev/null 2>&1; then
  printf '%s  %s\n' "${CLUSTERADM_INSTALLER_SHA256}" "${installer}" | sha256sum --check -
elif command -v shasum >/dev/null 2>&1; then
  printf '%s  %s\n' "${CLUSTERADM_INSTALLER_SHA256}" "${installer}" | shasum --algorithm 256 --check
else
  echo "sha256sum or shasum is required to verify the clusteradm installer" >&2
  exit 1
fi

bash "${installer}" "${CLUSTERADM_VERSION}"
```

Add the OCM Helm chart repository:

```bash
helm repo add ocm https://open-cluster-management.io/helm-charts
helm repo update
```

## 1. Create hub and managed clusters

Create two `kind` clusters. If you are using existing clusters, skip this step and substitute your own kubeconfig contexts throughout.

```bash
kind create cluster --name hub
kind create cluster --name managed1
```

## 2. Initialize the OCM hub

Initialize OCM on the hub cluster with the **ClusterProfile** feature gate enabled. This allows OCM to create `ClusterProfile` objects for managed clusters.

```bash
kubectl config use-context kind-hub
clusteradm init --feature-gates=ClusterProfile=true --wait
```

Save the join command token from the output.

```bash
export OCM_TOKEN=<token from clusteradm init output>
export HUB_APISERVER=<hub api server>
```

## 3. Register the managed cluster

On the managed cluster, run the join command:

```bash
kubectl config use-context kind-managed1
clusteradm join --hub-token=${OCM_TOKEN} --hub-apiserver=${HUB_APISERVER} --cluster-name managed1 --wait
```

Accept the managed cluster on the hub:

```bash
kubectl config use-context kind-hub
clusteradm accept --clusters managed1 --wait
```

Verify the managed cluster is registered:

```bash
kubectl get managedclusters
```

You should see `managed1` with `HubAccepted=true` and `Available=True`.

## 4. Install OCM add-ons

Install the **cluster-proxy** and **managed-serviceaccount** add-ons with ClusterProfile support enabled.

### cluster-proxy

The cluster-proxy add-on provides connectivity from the hub to managed cluster API servers and provisions access providers for ClusterProfile objects.

```bash
kubectl config use-context kind-hub
helm install cluster-proxy ocm/cluster-proxy \
  -n open-cluster-management-addon \
  --create-namespace \
  --set userServer.enabled=true \
  --set enableServiceProxy=true \
  --set featureGates.clusterProfileAccessProvider=true
```

### managed-serviceaccount

The managed-serviceaccount add-on creates service accounts on managed clusters and syncs their tokens back to the hub. The `clusterProfileCredSyncer` feature gate syncs credentials into ClusterProfile objects.

```bash
helm install managed-serviceaccount ocm/managed-serviceaccount \
  -n open-cluster-management-addon \
  --create-namespace \
  --set featureGates.clusterProfileCredSyncer=true
```

### cluster-permission (optional)

The cluster-permission add-on lets you define RBAC on managed clusters from the hub. If you prefer to manage RBAC manually on each managed cluster, skip this.

```bash
helm install cluster-permission ocm/cluster-permission \
  -n open-cluster-management \
  --create-namespace
```

Wait for add-on agents to be deployed to the managed cluster:

```bash
kubectl get managedclusteraddons -n managed1
```

You should see `cluster-proxy` and `managed-serviceaccount` with `Available=True`.

## 5. Create ManagedClusterSet and binding

Create a `ManagedClusterSet` that selects your managed clusters, and bind it to the `argocd` namespace. This tells OCM to create `ClusterProfile` objects in the `argocd` namespace.

```bash
kubectl create namespace argocd
kubectl apply -f - <<EOF
apiVersion: cluster.open-cluster-management.io/v1beta2
kind: ManagedClusterSet
metadata:
  name: argocd-clusters
spec:
  clusterSelector:
    labelSelector: {}
    selectorType: LabelSelector
---
apiVersion: cluster.open-cluster-management.io/v1beta2
kind: ManagedClusterSetBinding
metadata:
  name: argocd-clusters
  namespace: argocd
spec:
  clusterSet: argocd-clusters
EOF
```

> [!TIP]
> The empty `labelSelector: {}` selects all managed clusters. To select specific clusters, add labels to your managed clusters and use a `matchLabels` selector.

## 6. Create ManagedServiceAccount

Create a `ManagedServiceAccount` in the managed cluster's namespace on the hub. The label `sync-to-clusterprofile: "true"` syncs the credentials into the ClusterProfile's access provider.

```bash
kubectl apply -f - <<EOF
apiVersion: authentication.open-cluster-management.io/v1beta1
kind: ManagedServiceAccount
metadata:
  name: argocd
  namespace: managed1
  labels:
    authentication.open-cluster-management.io/sync-to-clusterprofile: "true"
spec:
  rotation:
    enabled: true
    validity: 8640h0m0s
EOF
```

To add more managed clusters, create a `ManagedServiceAccount` with the same name (`argocd`) in each managed cluster's namespace.

## 7. Grant permissions on the managed cluster

The `ManagedServiceAccount` add-on creates a ServiceAccount on the managed cluster, but it has no permissions by default. The synced ServiceAccount is created in the `open-cluster-management-agent-addon` namespace. Grant it `cluster-admin` access so Argo CD can deploy applications.

```bash
kubectl config use-context kind-managed1
kubectl create clusterrolebinding argocd-managed-sa \
  --clusterrole=cluster-admin \
  --serviceaccount=open-cluster-management-agent-addon:argocd
```

> [!NOTE]
> If you installed the **cluster-permission** add-on, you can grant permissions from the hub instead:
>
> ```bash
> kubectl config use-context kind-hub
> kubectl apply -f - <<EOF
> apiVersion: rbac.open-cluster-management.io/v1alpha1
> kind: ClusterPermission
> metadata:
>   name: argocd-cluster-admin
>   namespace: managed1
> spec:
>   clusterRole:
>     rules:
>     - apiGroups: ["*"]
>       resources: ["*"]
>       verbs: ["*"]
>   clusterRoleBinding:
>     subject:
>       kind: ServiceAccount
>       name: argocd
>       namespace: open-cluster-management-agent-addon
> EOF
> ```

Create the namespace for the sample application:

```bash
kubectl config use-context kind-managed1
kubectl create namespace guestbook
```

## 8. Verify ClusterProfile

Switch back to the hub and verify that OCM created a `ClusterProfile` with the access provider in the `argocd` namespace:

```bash
kubectl config use-context kind-hub
kubectl config set-context --current --namespace=argocd
kubectl get clusterprofiles
```

Inspect the ClusterProfile to confirm it has the `open-cluster-management` access provider:

```bash
kubectl get clusterprofile managed1 -o yaml
```

You should see the `status.accessProviders` section with `name: open-cluster-management`, a `server` URL pointing through cluster-proxy, `certificate-authority-data`, and a `client.authentication.k8s.io/exec` extension whose `clusterName` matches the managed cluster name.

## 9. Install Argo CD

Install Argo CD on the hub cluster:

```bash
kubectl config use-context kind-hub
kubectl config set-context --current --namespace=argocd

helm repo add argo https://argoproj.github.io/argo-helm
helm repo update argo
helm upgrade --install argocd argo/argo-cd \
  --set global.image.tag=v3.5.0 \
  --namespace argocd \
  --create-namespace \
  --wait

# Install the standalone Cluster Profile Controller
kubectl apply -k artifacts/manifests
```

### \[Alternative\] Local development

If you have made changes to the controller source code, build and deploy a local image instead:

```bash
make docker-build
kind load docker-image ghcr.io/argoproj-labs/clusterprofile-integration-for-argocd:latest --name hub

kubectl apply -k artifacts/manifests
```

## 10. Configure the cp-creds access provider

OCM's `cp-creds` plugin handles authentication to managed clusters via ManagedServiceAccount tokens. It needs to be:

1. Referenced in an **access providers file** (read by the ClusterProfile controller)
2. **Mounted as a binary** at the same path in the Argo CD components that use the resulting cluster Secrets

### Create the access providers file

Create a Secret with the `cp-creds.json` providers file. The provider `name` must match the access provider name in the ClusterProfile status (`open-cluster-management`). With `provideClusterInfo` enabled, Argo CD passes the ClusterProfile exec extension config to `cp-creds` through `KUBERNETES_EXEC_INFO`.

```bash
kubectl create secret generic cp-creds-secret \
  --from-file=cp-creds.json=/dev/stdin <<EOF
{
  "providers": [
    {
      "name": "open-cluster-management",
      "execConfig": {
        "command": "/plugins/cp-creds",
        "args": [
          "--managed-serviceaccount=argocd"
        ],
        "apiVersion": "client.authentication.k8s.io/v1",
        "provideClusterInfo": true
      }
    }
  ]
}
EOF
```

### Mount the providers file in the ClusterProfile controller

```bash
kubectl patch deploy/argocd-clusterprofile-controller --type strategic --patch '
spec:
  template:
    spec:
      volumes:
        - name: cp-creds-vol
          secret:
            secretName: cp-creds-secret
      containers:
        - name: argocd-clusterprofile-controller
          volumeMounts:
            - name: cp-creds-vol
              mountPath: /app/cp-creds
          env:
            - name: ARGOCD_CLUSTERPROFILE_CONTROLLER_CLUSTERPROFILE_PROVIDER_FILE
              value: /app/cp-creds/cp-creds.json
```

### Mount the cp-creds binary in Argo CD

The application controller and server can both execute `cp-creds` when they use the resulting cluster Secrets to access managed clusters. Mount the binary at the same path in both components using an `initContainer`:

```bash
kubectl patch sts/argocd-application-controller --type strategic --patch '
spec:
  template:
    spec:
      initContainers:
        - name: install-cp-creds
          image: quay.io/open-cluster-management/cp-creds:latest
          command:
            - sh
            - -c
            - |
              cp /cp-creds /plugins/cp-creds
              chmod +x /plugins/cp-creds
          volumeMounts:
            - name: clusterprofile-plugins
              mountPath: /plugins
      volumes:
        - name: clusterprofile-plugins
          emptyDir: {}
      containers:
        - name: application-controller
          volumeMounts:
            - name: clusterprofile-plugins
              mountPath: /plugins'

kubectl patch deploy/argocd-server --type strategic --patch '
spec:
  template:
    spec:
      initContainers:
        - name: install-cp-creds
          image: quay.io/open-cluster-management/cp-creds:latest
          command:
            - sh
            - -c
            - |
              cp /cp-creds /plugins/cp-creds
              chmod +x /plugins/cp-creds
          volumeMounts:
            - name: clusterprofile-plugins
              mountPath: /plugins
      volumes:
        - name: clusterprofile-plugins
          emptyDir: {}
      containers:
        - name: server
          volumeMounts:
            - name: clusterprofile-plugins
              mountPath: /plugins'
```

Wait for the updated components to restart:

```bash
kubectl rollout status deploy/argocd-clusterprofile-controller
kubectl rollout status sts/argocd-application-controller
kubectl rollout status deploy/argocd-server
```

## 11. Create ApplicationSet

Create an ApplicationSet with the ClusterGenerator to deploy an application to all registered clusters:

```bash
kubectl apply -f - <<EOF
apiVersion: argoproj.io/v1alpha1
kind: ApplicationSet
metadata:
  name: guestbook
  namespace: argocd
spec:
  generators:
  - clusters: {}
  goTemplate: true
  template:
    metadata:
      name: 'guestbook-{{ .nameNormalized }}'
    spec:
      project: "default"
      source:
        repoURL: https://github.com/argoproj/argocd-example-apps.git
        targetRevision: HEAD
        path: guestbook
      destination:
        server: '{{ .server }}'
        namespace: guestbook
EOF
```

## 12. Verify

Check that the Argo CD cluster Secret was generated from the ClusterProfile. It is created in `argocd`, the same namespace as the ClusterProfile:

```bash
kubectl get secrets -l argocd.argoproj.io/secret-type=cluster
```

Verify the application was created:

```bash
kubectl get applications
```

Check that the guestbook pods are running on the managed cluster:

```bash
kubectl config use-context kind-managed1
kubectl get pods -n guestbook
```

You should see the `guestbook-ui` pod running.

If not, debug with:

```bash
kubectl config use-context kind-hub
kubectl config set-context --current --namespace=argocd
echo -e "\nClusterProfile controller errors:" && kubectl logs deployment/argocd-clusterprofile-controller | grep -i error
echo -e "\nApplication controller errors:" && kubectl logs statefulset/argocd-application-controller | grep -i error
echo -e "\nArgo CD server errors:" && kubectl logs deployment/argocd-server | grep -i error
echo -e "\nClusterProfiles:" && kubectl get clusterprofiles
echo -e "\nCluster Secrets:" && kubectl get secrets -l argocd.argoproj.io/secret-type=cluster
echo -e "\nApplications:" && kubectl get applications
```

## 13. Adding more managed clusters

To add more clusters, repeat these steps for each new cluster:

1. Register the cluster with OCM (`clusteradm join` / `clusteradm accept`)
2. Create a `ManagedServiceAccount` named `argocd` in the cluster's namespace on the hub (with the `sync-to-clusterprofile` label)
3. Grant RBAC on the managed cluster

OCM will automatically create a `ClusterProfile`, the ClusterProfile controller will generate an Argo CD cluster secret, and the ApplicationSet will deploy the application, no manual cluster registration in Argo CD required.

## 14. Cleanup

Delete the Kind clusters:

```bash
kind delete cluster --name hub
kind delete cluster --name managed1
```
