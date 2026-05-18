# Using Cluster Profiles with Kind Clusters

This guide demonstrates how to use Cluster Profiles to connect a spoke cluster to an Argo CD instance running in a hub cluster.

> [!TIP]
> For a similar example, see the ClusterProfile API's [secretreader](https://github.com/kubernetes-sigs/cluster-inventory-api/blob/main/examples/controller-example/README.md).

## Prerequisites

- Docker, Kind, Kubectl

## 1. Create Hub and Spoke Clusters

Create two `kind` clusters:
```bash
kind create cluster --name hub
kind create cluster --name spoke
```

## 2. Install Argo CD

Install Argo CD in `hub`:

```bash
kubectl config use-context kind-hub
kubectl create namespace argocd
kubectl config set-context --current --namespace=argocd
# Install Argo CD. Use server-side apply in case ApplicationSet CRD is too large for client-side kubectl apply
kubectl apply --server-side --force-conflicts -f https://raw.githubusercontent.com/argoproj/argo-cd/stable/manifests/install.yaml

# Install the standalone Cluster Profile Controller
kubectl apply -f artifacts/manifests/install.yaml
```

### \[Alternative\] Local Development

If you have made changes to the controller source code, build and deploy a local image instead. This is only necessary when doing local development of this controller!

```bash
kubectl config use-context kind-hub
kubectl create namespace argocd
kubectl config set-context --current --namespace=argocd

# Build local controller image
make docker-build IMG=controller:dev
kind load docker-image controller:dev --name hub

# Deploy local controller manifests using Kustomize
cd artifacts/manifests/base/clusterprofile-controller
kustomize edit set image controller:latest=controller:dev
kubectl apply -k .
```

## 3. Configure Spoke Cluster Service Account

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

## 5. Create a plugin to use credentials

Create a simple auth plugin. It uses ExecCredential for a token in the format expected by Kubernetes:
```bash
kubectl config use-context kind-hub
kubectl create configmap argocd-custom-auth-plugin --from-literal=get-token.sh='#!/bin/sh
cat <<EOF
{
  "apiVersion": "client.authentication.k8s.io/v1beta1",
  "kind": "ExecCredential",
  "status": {
    "token": "'"$SPOKE_TOKEN"'"
  }
}
EOF
'
```

Mount the auth plugin into the application controller:
```bash
kubectl patch sts/argocd-application-controller --type strategic --patch '
spec:
  template:
    spec:
      volumes:
        - name: auth-script
          configMap:
            name: argocd-custom-auth-plugin
            defaultMode: 0755
      containers:
        - name: argocd-application-controller
          volumeMounts:
            - name: auth-script
              mountPath: /usr/local/bin/custom-auth'
```

## 6. Create access providers file

A Cluster Profile expects an access providers file with an `execConfig` for authentication. This `execConfig` will run our plugin to return a token.

Create an access providers secret:
```bash
kubectl config use-context kind-hub
kubectl create secret generic cp-creds-secret \
  --from-file=cp-creds.json=/dev/stdin <<EOF
{
  "providers": [
    {
      "name": "hub-provider",
      "execConfig": {
        "command": "/usr/local/bin/custom-auth/get-token.sh",
        "apiVersion": "client.authentication.k8s.io/v1beta1"
      },
      "tlsClientConfig": {
        "caData": "${SPOKE_CA}"
      }
    }
  ]
}
EOF
```
This could also be done with a `ConfigMap`.

For this example, the command is a script that will be created in a later step. The secret contents will be read by the Cluster Profile syncer and used to generate an Argo CD cluster secret.

## 7. Create Cluster Profile in hub

Normally, a controller would create the Cluster Profile and update its status. In this example we will create it manually and patch in the status.

Create the Cluster Profile object to represent `spoke`:
```bash
# Create ClusterProfile
kubectl apply -f - <<EOF
apiVersion: "multicluster.x-k8s.io/v1alpha1"
kind: ClusterProfile
metadata:
  name: spoke-cluster
  namespace: argocd
spec:
  clusterManager:
    name: manual
  displayName: "Spoke Cluster"
EOF
```

Add access provider to the ClusterProfile status:
```bash
kubectl patch clusterprofile spoke-cluster --subresource=status --type=merge -p '{"status":{"accessProviders":[{"name":"hub-provider","cluster":{"server":"https://'"${SPOKE_IP}"':6443", "certificate-authority-data": "'"${SPOKE_CA}"'"}}]}}'
```
Note that the provider's `name` refers to the name in the access providers secret/file.

Now we mount the access providers file into the Cluster Profile controller:
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
          args:
            - "/manager"
            - "--cluster-profile-providers-file=/app/cp-creds/cp-creds.json"'
```
Setting a value for `--cluster-profile-providers-file` will enable the Cluster Profile syncer in the clusterprofile controller.

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
