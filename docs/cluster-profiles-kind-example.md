# Using Cluster Profiles with Kind clusters

This guide demonstrates how to use Cluster Profiles to connect a spoke cluster to an Argo CD instance running in a hub cluster.

> [!TIP]
> For a similar example, see the ClusterProfile API's [secretreader](https://github.com/kubernetes-sigs/cluster-inventory-api/blob/main/examples/controller-example/plugins/secretreader/README.md).

## Prerequisites

- Docker, Kind, Kubectl, Helm
- A Kubernetes version that supports ImageVolume.

## 1. Create hub and spoke clusters

Create two `kind` clusters:

```bash
kind create cluster --name hub
kind create cluster --name spoke
```

## 2. Install Argo CD

Install Argo CD in `hub`:

```bash
kubectl config use-context kind-hub
kubectl config set-context --current --namespace=argocd

helm repo add argo https://argoproj.github.io/argo-helm
helm repo update argo
helm upgrade --install argocd argo/argo-cd \
  --version 10.4.0 \
  --set global.image.tag=v3.5.1 \
  --namespace argocd \
  --create-namespace \
  --wait

# Install the standalone Cluster Profile Controller
kubectl apply -k artifacts/manifests
```

### \[Alternative\] Local development

If you have made changes to the controller source code, build and deploy a local image instead. This is only necessary when doing local development of this controller!

```bash
kubectl config use-context kind-hub
kubectl config set-context --current --namespace=argocd

# Build local controller image
make docker-build
kind load docker-image ghcr.io/argoproj-labs/clusterprofile-integration-for-argocd:latest --name hub

# Deploy controller manifests
kubectl apply -k artifacts/manifests
```

## 3. Configure spoke cluster service account

Create `argocd-manager` service account in `spoke`:

```bash
kubectl config use-context kind-spoke
kubectl apply -f - <<EOF
apiVersion: v1
kind: ServiceAccount
metadata:
  name: argocd-manager
  namespace: kube-system
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: argocd-manager-role
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: cluster-admin
subjects:
- kind: ServiceAccount
  name: argocd-manager
  namespace: kube-system
---
apiVersion: v1
kind: Secret
metadata:
  name: argocd-manager-token
  namespace: kube-system
  annotations:
    kubernetes.io/service-account.name: argocd-manager
type: kubernetes.io/service-account-token
EOF
```

Create the namespace for the sample application:

```bash
kubectl config use-context kind-spoke
kubectl create namespace guestbook
```

## 4. Get spoke cluster credentials

```bash
SPOKE_IP=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' spoke-control-plane)
SPOKE_CA=$(kubectl --context kind-spoke config view --raw --minify --flatten -o jsonpath='{.clusters[0].cluster.certificate-authority-data}')
SPOKE_TOKEN=$(kubectl --context kind-spoke -n kube-system get secret argocd-manager-token -o jsonpath='{.data.token}' | base64 -d)
```

## 5. Store credentials for the secretreader plugin

The `secretreader` plugin reads a token from a Kubernetes Secret. In this example, the Secret name matches the ClusterProfile name (`spoke-cluster`), the Secret lives in the Argo CD namespace, and the token is stored in `data.token`.

```bash
kubectl config use-context kind-hub

kubectl -n argocd create secret generic spoke-cluster \
  --from-literal=token="${SPOKE_TOKEN}"
```

Grant the Argo CD application controller and server permission to read that Secret:

```bash
APP_CONTROLLER_SA=$(kubectl -n argocd get sts/argocd-application-controller -o jsonpath='{.spec.template.spec.serviceAccountName}')
APP_CONTROLLER_SA=${APP_CONTROLLER_SA:-argocd-application-controller}
SERVER_SA=$(kubectl -n argocd get deploy/argocd-server -o jsonpath='{.spec.template.spec.serviceAccountName}')
SERVER_SA=${SERVER_SA:-argocd-server}

kubectl -n argocd apply -f - <<EOF
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: argocd-secretreader
rules:
  - apiGroups:
      - ""
    resources:
      - secrets
    resourceNames:
      - spoke-cluster
    verbs:
      - get
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: argocd-secretreader
subjects:
  - kind: ServiceAccount
    name: ${APP_CONTROLLER_SA}
    namespace: argocd
  - kind: ServiceAccount
    name: ${SERVER_SA}
    namespace: argocd
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: argocd-secretreader
EOF
```

## 6. Configure the secretreader provider

Mount the `secretreader` plugin image into the Argo CD components that use the resulting cluster Secret. The command path must be the same in both components. With an Argo CD image that supports `execProviderConfig.config`, Argo CD passes the ClusterProfile extension data to the plugin through `KUBERNETES_EXEC_INFO` when `provideClusterInfo` is true.

```bash
kubectl patch sts/argocd-application-controller --type strategic --patch '
spec:
  template:
    spec:
      volumes:
        - name: secretreader-plugin
          image:
            reference: registry.k8s.io/cluster-inventory-api/secretreader:v0.1.3
            pullPolicy: IfNotPresent
      containers:
        - name: application-controller
          volumeMounts:
            - name: secretreader-plugin
              mountPath: /plugins/secretreader
              readOnly: true'

kubectl patch deploy/argocd-server --type strategic --patch '
spec:
  template:
    spec:
      volumes:
        - name: secretreader-plugin
          image:
            reference: registry.k8s.io/cluster-inventory-api/secretreader:v0.1.3
            pullPolicy: IfNotPresent
      containers:
        - name: server
          volumeMounts:
            - name: secretreader-plugin
              mountPath: /plugins/secretreader
              readOnly: true'
```

Create an access providers file. The `execConfig.command` points at the mounted `secretreader` binary.

```bash
kubectl config use-context kind-hub
kubectl create secret generic cp-creds-secret \
  --from-file=cp-creds.json=/dev/stdin <<EOF
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
EOF
```

Mount the access providers file into the Cluster Profile controller:

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

Wait for both controllers to roll out:

```bash
kubectl rollout status sts/argocd-application-controller --timeout=300s
kubectl rollout status deploy/argocd-server --timeout=300s
kubectl rollout status deploy/argocd-clusterprofile-controller --timeout=300s
```

## 7. Create Cluster Profile in hub

Normally, a controller would create the Cluster Profile and update its status. In this example we will create it manually and patch in the status.

Create the Cluster Profile object to represent `spoke`:

```bash
kubectl apply -f - <<EOF
apiVersion: "multicluster.x-k8s.io/v1alpha1"
kind: ClusterProfile
metadata:
  name: spoke-cluster
  namespace: argocd
  labels:
    multicluster.x-k8s.io/inventory-member-id: spoke-cluster
spec:
  clusterManager:
    name: manual
  displayName: "Spoke Cluster"
EOF
```

Add the `secretreader` access provider to the ClusterProfile status. The `cluster.extensions` entry passes non-secret plugin configuration to the exec plugin; here, `clusterName` tells `secretreader` which Secret to read.

```bash
kubectl patch clusterprofile spoke-cluster --subresource=status --type=merge -p "{
  \"status\": {
    \"accessProviders\": [
      {
        \"name\": \"secretreader\",
        \"cluster\": {
          \"server\": \"https://${SPOKE_IP}:6443\",
          \"certificate-authority-data\": \"${SPOKE_CA}\",
          \"extensions\": [
            {
              \"name\": \"client.authentication.k8s.io/exec\",
              \"extension\": {
                \"clusterName\": \"spoke-cluster\"
              }
            }
          ]
        }
      }
    ]
  }
}"
```

Note that the provider's `name` refers to the name in the access providers secret/file.

The controller generates the Argo CD cluster Secret `cluster-spoke-cluster` in `argocd`, the same namespace as the ClusterProfile. Argo CD reads its cluster Secrets from that namespace.

## 8. Create ApplicationSet

Create simple ApplicationSet with ClusterGenerator:

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

Everything should now be in place!

Verify that the application was created and synced:

```bash
kubectl config use-context kind-spoke
kubectl get pods -n guestbook
```

You should see the `guestbook-ui` pod appear.

## 9. Cleanup

```bash
kind delete cluster --name hub
kind delete cluster --name spoke
```
