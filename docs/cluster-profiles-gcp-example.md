# Using Cluster Profiles with GCP

This guide demonstrates how to use Cluster Profiles to connect a GKE spoke cluster to an Argo CD instance running in a GKE hub cluster.

## Prerequisites

`gcloud` CLI, GCP project with billing enabled, Kubectl, Helm

## 1. Set up environment variables

Set environment variables for your GCP project and desired region. Replace `"your-gcp-project-id"` with your GCP project ID.

```bash
export GCP_PROJECT_ID="your-gcp-project-id"
export GCP_LOCATION="us-central1"
gcloud config set project ${GCP_PROJECT_ID}
gcloud config set compute/region ${GCP_LOCATION}
```

## 2. Create hub and spoke GKE clusters

Create a `hub` cluster with relevant settings:

```bash
gcloud container clusters create hub \
  --location=${GCP_LOCATION} \
  --workload-pool=${GCP_PROJECT_ID}.svc.id.goog \
  --enable-fleet \
  --labels=fleet-clusterinventory-management-cluster=true,fleet-clusterinventory-namespace=argocd,fleet-clusterinventory-access-provider-name=argo-cd-builtin-gcp
```

Workload Identity allows Kubernetes service accounts to impersonate GCP service accounts.
Enabling Fleet with the cluster labels tells the [Fleet Cluster Profile Syncer](https://docs.cloud.google.com/kubernetes-engine/fleet-management/docs/generate-inventory-for-integrations) to automatically create Cluster Profiles for all clusters in the Fleet within the management cluster (`fleet-clusterinventory-management-cluster=true`). The `fleet-clusterinventory-access-provider-name=argo-cd-builtin-gcp` label tells the ClusterProfile to use the access provider name `argo-cd-builtin-gcp`, this `argo-cd-builtin-` prefix indicates that the Cluster Profile controller should generate a secret configured for built-in GCP authentication rather than look for a custom access providers file. For an example with a custom exec config, see the [kind example](cluster-profiles-kind-example.md).

Create a standard GKE Fleet cluster to act as the `spoke`:

```bash
gcloud container clusters create spoke --location=${GCP_LOCATION} --enable-fleet \
  --labels=fleet-clusterinventory-access-provider-name=argo-cd-builtin-gcp
```

Get contexts for both clusters and set `namespace=argocd` for all future `hub` cluster commands:

```bash
gcloud container clusters get-credentials hub --location=${GCP_LOCATION}
kubectl config set-context --current --namespace=argocd
gcloud container clusters get-credentials spoke --location=${GCP_LOCATION}
kubectl create namespace guestbook
```

## 3. Install Argo CD on hub

Install Argo CD in the hub cluster:

```bash
kubectl config use-context gke_${GCP_PROJECT_ID}_${GCP_LOCATION}_hub
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

To include local changes to the controller source code, build a local image and push it to GCP Artifact Registry:

```bash
kubectl config use-context gke_${GCP_PROJECT_ID}_${GCP_LOCATION}_hub
kubectl config set-context --current --namespace=argocd

# Create an artifact registry repo
gcloud services enable artifactregistry.googleapis.com
export REPO_NAME="controller-repo"
gcloud artifacts repositories create ${REPO_NAME} --repository-format=docker --location=${GCP_LOCATION}

# Build and push the image
gcloud auth configure-docker ${GCP_LOCATION}-docker.pkg.dev
export IMAGE_NAMESPACE=${GCP_LOCATION}-docker.pkg.dev/${GCP_PROJECT_ID}/${REPO_NAME}
export IMAGE_TAG=dev-$(date +%Y%m%d%H%M%S)
export CONTROLLER_IMAGE=${IMAGE_NAMESPACE}/clusterprofile-integration-for-argocd:${IMAGE_TAG}
make docker-build IMG=${CONTROLLER_IMAGE}
make docker-push IMG=${CONTROLLER_IMAGE}

# Deploy the controller and point it at the image just pushed
kubectl apply -k artifacts/manifests
kubectl set image deployment/argocd-clusterprofile-controller \
  argocd-clusterprofile-controller=${CONTROLLER_IMAGE} \
  --namespace argocd
kubectl rollout status deployment/argocd-clusterprofile-controller \
  --namespace argocd \
  --timeout=300s
```

For each update, use a new tag so GKE nodes cannot reuse a cached image:

```bash
export IMAGE_TAG=dev-$(date +%Y%m%d%H%M%S)
export CONTROLLER_IMAGE=${IMAGE_NAMESPACE}/clusterprofile-integration-for-argocd:${IMAGE_TAG}
make docker-build IMG=${CONTROLLER_IMAGE}
make docker-push IMG=${CONTROLLER_IMAGE}
kubectl set image deployment/argocd-clusterprofile-controller \
  argocd-clusterprofile-controller=${CONTROLLER_IMAGE} \
  --namespace argocd
kubectl rollout status deployment/argocd-clusterprofile-controller \
  --namespace argocd \
  --timeout=300s
```

## 4. Configure service accounts and permissions

### Hub cluster Workload Identity

The ClusterProfile controller only writes Argo CD cluster Secrets and does not authenticate to the spoke cluster. The Argo CD application controller and server do access the spoke cluster, so configure those two workloads to impersonate one Google service account (GSA):

```bash
export GSA_NAME="argocd-cluster-access"
export GSA_EMAIL=${GSA_NAME}@${GCP_PROJECT_ID}.iam.gserviceaccount.com
gcloud iam service-accounts create ${GSA_NAME} \
  --project=${GCP_PROJECT_ID} \
  --display-name="Argo CD cluster access"
```

Grant the GSA permission to use the Connect Gateway:

```bash
gcloud services enable \
  connectgateway.googleapis.com \
  gkeconnect.googleapis.com \
  gkehub.googleapis.com
gcloud projects add-iam-policy-binding ${GCP_PROJECT_ID} \
  --member="serviceAccount:${GSA_EMAIL}" \
  --role="roles/gkehub.gatewayAdmin" \
  --condition=None
```

Allow both Argo CD Kubernetes service accounts (KSAs) to impersonate the GSA, then annotate the KSAs for Workload Identity:

```bash
for KSA_NAME in argocd-application-controller argocd-server; do
  gcloud iam service-accounts add-iam-policy-binding ${GSA_EMAIL} \
    --project=${GCP_PROJECT_ID} \
    --role="roles/iam.workloadIdentityUser" \
    --member="serviceAccount:${GCP_PROJECT_ID}.svc.id.goog[argocd/${KSA_NAME}]"
  kubectl annotate serviceaccount ${KSA_NAME} \
    "iam.gke.io/gcp-service-account=${GSA_EMAIL}" \
    --overwrite
done

kubectl rollout restart statefulset argocd-application-controller
kubectl rollout restart deployment argocd-server
kubectl rollout status statefulset/argocd-application-controller --timeout=300s
kubectl rollout status deployment/argocd-server --timeout=300s
```

### Spoke cluster RBAC

Connect Gateway forwards the GSA email as the Kubernetes user identity. Generate and apply both the required impersonation policy and the demo's `cluster-admin` binding:

```bash
export SPOKE_CONTEXT=gke_${GCP_PROJECT_ID}_${GCP_LOCATION}_spoke
export KUBECONFIG_PATH="${KUBECONFIG:-${HOME}/.kube/config}"
gcloud container fleet memberships generate-gateway-rbac \
  --membership=spoke \
  --role=clusterrole/cluster-admin \
  --users=${GSA_EMAIL} \
  --project=${GCP_PROJECT_ID} \
  --kubeconfig=${KUBECONFIG_PATH} \
  --context=${SPOKE_CONTEXT} \
  --apply
kubectl config use-context gke_${GCP_PROJECT_ID}_${GCP_LOCATION}_hub
```

This guide uses `cluster-admin` to keep the walkthrough short. In production, bind a narrower ClusterRole instead.

## 5. Create ApplicationSet

At this point, the Cluster Profile and Secret should be generated (you may verify with `kubectl get clusterprofiles` and `kubectl get secrets`). The generated Secret is in `argocd`, the same namespace as the ClusterProfile, so Argo CD can read it. Argo CD will use the built-in GCP provider to authenticate to the spoke cluster using Workload Identity.

With the cluster connection configured, create an `ApplicationSet`:

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

## 6. Sync

Trigger the application to sync:

```bash
kubectl patch application guestbook-spoke-${GCP_LOCATION} -p '{"operation": {"sync": {"prune": true}}}' --type=merge
```

Verify that the `guestbook` application was deployed to the `spoke` cluster:

```bash
kubectl config use-context gke_${GCP_PROJECT_ID}_${GCP_LOCATION}_spoke
kubectl get pods -n guestbook
```

If you see a `guestbook-ui` pod running, congratulations on completing this guide! You now have an automatic flow that prepares new Fleet clusters for Argo CD through a ClusterProfile, generated Secret, and Application.

If not, debug:

```bash
kubectl config use-context gke_${GCP_PROJECT_ID}_${GCP_LOCATION}_hub
echo -e "\nClusterProfile controller errors:" && kubectl logs deployment/argocd-clusterprofile-controller | grep Error
echo -e "\nApplication controller errors:" && kubectl logs statefulset/argocd-application-controller | grep Error
echo -e "\nController:" && kubectl get pods | grep clusterprofile-controller
echo -e "\nClusterProfile:" && kubectl get clusterprofiles | grep spoke-us-central1
echo -e "\nSecret:" && kubectl get secrets | grep cluster-spoke-us-central1
echo -e "\nApplication:" && kubectl get applications | grep guestbook-spoke-us-central1
```

* If you see permission issues when connecting to the cluster, check that you didn't miss any of Step 4.
* If you see server issues (`server.secretkey is missing`), restart the server (`kubectl rollout restart deploy/argocd-server`).
* If everything looks correct, try triggering the sync in the Argo CD UI.

## 7. Cleanup

Delete the GKE clusters.

```bash
gcloud container clusters delete hub --location=${GCP_LOCATION} --quiet --async
gcloud container clusters delete spoke --location=${GCP_LOCATION} --quiet --async
```

Remove the project role and delete the GSA:

```bash
gcloud projects remove-iam-policy-binding ${GCP_PROJECT_ID} \
  --member="serviceAccount:${GSA_EMAIL}" \
  --role="roles/gkehub.gatewayAdmin" \
  --condition=None
gcloud iam service-accounts delete ${GSA_EMAIL} --quiet
```

If you followed the local development alternative, delete its Artifact Registry repository:

```bash
export REPO_NAME="controller-repo"
gcloud artifacts repositories delete ${REPO_NAME} --location=${GCP_LOCATION} --quiet --async
```
