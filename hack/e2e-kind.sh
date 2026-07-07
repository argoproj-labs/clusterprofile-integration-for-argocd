#!/usr/bin/env bash

set -euo pipefail

ARGOCD_CHART_VERSION="${ARGOCD_CHART_VERSION:-9.5.19}"
# TODO: Once the first Argo CD release containing 6d92e177b45fcd51bde0dbc169f7f923acc9a79d
# is available, replace the latest default with that released version and document it as the minimum
# supported Argo CD version for ClusterProfile exec config propagation.
ARGOCD_IMAGE_REPOSITORY="${ARGOCD_IMAGE_REPOSITORY:-quay.io/argoproj/argocd}"
ARGOCD_IMAGE_TAG="${ARGOCD_IMAGE_TAG:-latest}"
ARGOCD_IMAGE_PULL_POLICY="${ARGOCD_IMAGE_PULL_POLICY:-Always}"
GUESTBOOK_REVISION="${GUESTBOOK_REVISION:-8088f4c0d970abb09e250248cc97e35623447cb5}"
E2E_IMG="${E2E_IMG:-ghcr.io/argoproj-labs/clusterprofile-integration-for-argocd:latest}"
E2E_PREFIX="${E2E_PREFIX:-cpia-e2e-$$}"
E2E_INSTALL_METHOD="${E2E_INSTALL_METHOD:-helm}"
KIND_NODE_IMAGE="${KIND_NODE_IMAGE:-}"
SECRETREADER_IMAGE="${SECRETREADER_IMAGE:-registry.k8s.io/cluster-inventory-api/secretreader:v0.1.3}"
SECRETREADER_COMMAND="${SECRETREADER_COMMAND:-/plugins/secretreader/bin/secretreader-plugin}"
HUB_CLUSTER="${HUB_CLUSTER:-${E2E_PREFIX}-hub}"
SPOKE_CLUSTER="${SPOKE_CLUSTER:-${E2E_PREFIX}-spoke}"
ARGOCD_NS="${ARGOCD_NS:-argocd}"
CP_NAME="spoke-cluster-full"
CLUSTER_NAME="${ARGOCD_NS}/${CP_NAME}"
# Matches the ApplicationSet template's "guestbook-{{ .nameNormalized }}".
APP_NAME="guestbook-${CLUSTER_NAME//\//-}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
WORK_DIR="$(mktemp -d)"
export KUBECONFIG="${WORK_DIR}/kubeconfig"
HUB_CREATED=0
SPOKE_CREATED=0

log() {
  printf '[e2e] %s\n' "$*"
}

require_command() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "required command not found: $1" >&2
    exit 1
  fi
}

create_kind_cluster() {
  local cluster_name="$1"
  if [ -n "${KIND_NODE_IMAGE}" ]; then
    kind create cluster --name "${cluster_name}" --image "${KIND_NODE_IMAGE}" --wait 120s
  else
    kind create cluster --name "${cluster_name}" --wait 120s
  fi
}

dump_diagnostics() {
  set +e
  log "collecting diagnostics"
  for ctx in "kind-${HUB_CLUSTER}" "kind-${SPOKE_CLUSTER}"; do
    if kubectl --context "${ctx}" cluster-info >/dev/null 2>&1; then
      log "pods for ${ctx}"
      kubectl --context "${ctx}" get pods -A -o wide
      log "events for ${ctx}"
      kubectl --context "${ctx}" get events -A --sort-by=.lastTimestamp | tail -80
    fi
  done
  if kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" get pods >/dev/null 2>&1; then
    log "ClusterProfile controller logs"
    kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" logs deploy/argocd-clusterprofile-controller --tail=200
    log "ApplicationSet controller logs"
    kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" logs deploy/argocd-applicationset-controller --tail=200
    log "Application controller logs"
    kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" logs sts/argocd-application-controller --tail=200
    log "Application state"
    kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" get applications.argoproj.io "${APP_NAME}" -o yaml
  fi
}

cleanup() {
  status=$?
  if [ "${status}" -ne 0 ]; then
    dump_diagnostics
  fi
  if [ "${E2E_SKIP_CLEANUP:-0}" != "1" ]; then
    log "deleting kind clusters"
    if [ "${HUB_CREATED}" = "1" ]; then
      kind delete cluster --name "${HUB_CLUSTER}" >/dev/null 2>&1 || true
    fi
    if [ "${SPOKE_CREATED}" = "1" ]; then
      kind delete cluster --name "${SPOKE_CLUSTER}" >/dev/null 2>&1 || true
    fi
    rm -rf "${WORK_DIR}"
  else
    log "leaving clusters and kubeconfig for debugging: ${KUBECONFIG}"
  fi
  exit "${status}"
}

trap cleanup EXIT

retry_until() {
  local attempts="$1" label="$2" i
  shift 2
  for i in $(seq 1 "${attempts}"); do
    if "$@"; then
      return 0
    fi
    if [ $((i % 15)) -eq 0 ]; then
      log "waiting for ${label} (${i}/${attempts})"
    fi
    sleep 1
  done
  return 1
}

find_cluster_secret() {
  SECRET_NAME="$(
    kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" get secrets \
      -l "argocd.argoproj.io/secret-type=cluster,argocd.argoproj.io/cluster-profile-namespace=${ARGOCD_NS},argocd.argoproj.io/cluster-profile-name=${CP_NAME}" \
      -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true
  )"
  [ -n "${SECRET_NAME}" ]
}

wait_for_application() {
  local sync health phase
  for i in $(seq 1 600); do
    sync="$(kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" get application "${APP_NAME}" -o jsonpath='{.status.sync.status}' 2>/dev/null || true)"
    health="$(kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" get application "${APP_NAME}" -o jsonpath='{.status.health.status}' 2>/dev/null || true)"
    phase="$(kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" get application "${APP_NAME}" -o jsonpath='{.status.operationState.phase}' 2>/dev/null || true)"
    if [ "${sync}" = "Synced" ] && [ "${health}" = "Healthy" ]; then
      log "application ${APP_NAME} is Synced and Healthy"
      return 0
    fi
    if [ $((i % 15)) -eq 0 ]; then
      log "waiting for ${APP_NAME}: sync=${sync:-<empty>} health=${health:-<empty>} phase=${phase:-<empty>}"
    fi
    sleep 1
  done
  echo "application ${APP_NAME} did not become Synced and Healthy" >&2
  return 1
}

verify_argocd_server_cluster_access() {
  local admin_password argocd_config argocd_context cluster_json
  admin_password="$(
    kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" get secret argocd-initial-admin-secret \
      -o jsonpath='{.data.password}' | base64 -d
  )"
  argocd_config="${WORK_DIR}/argocd-config"
  argocd_context="${E2E_PREFIX}-server"

  log "verifying Argo CD server can access ClusterProfile cluster"
  argocd login localhost \
    --config "${argocd_config}" \
    --name "${argocd_context}" \
    --kube-context "kind-${HUB_CLUSTER}" \
    --port-forward \
    --port-forward-namespace "${ARGOCD_NS}" \
    --username admin \
    --password "${admin_password}" \
    --insecure \
    --skip-test-tls \
    --grpc-web

  _cluster_connection_successful() {
    cluster_json="$(
      argocd cluster list \
        --config "${argocd_config}" \
        --argocd-context "${argocd_context}" \
        --kube-context "kind-${HUB_CLUSTER}" \
        --port-forward \
        --port-forward-namespace "${ARGOCD_NS}" \
        -o json
    )" || return 1
    [ "$(
      printf '%s' "${cluster_json}" | jq -r \
        --arg name "${CLUSTER_NAME}" \
        --arg server "https://${SPOKE_IP}:6443" \
        'first(.[] | select(.name == $name and .server == $server) | .connectionState.status) // ""'
    )" = "Successful" ]
  }
  if ! retry_until 120 "Argo CD server cluster connection to ${CLUSTER_NAME}" _cluster_connection_successful; then
    echo "Argo CD server did not report a successful connection to ${CLUSTER_NAME}" >&2
    printf '%s\n' "${cluster_json}" | jq . || printf '%s\n' "${cluster_json}"
    return 1
  fi

  _app_logs_available() {
    argocd app logs "${APP_NAME}" \
      --config "${argocd_config}" \
      --argocd-context "${argocd_context}" \
      --kube-context "kind-${HUB_CLUSTER}" \
      --port-forward \
      --port-forward-namespace "${ARGOCD_NS}" \
      --namespace guestbook \
      --kind Pod \
      --container guestbook-ui \
      --tail 1 >/dev/null
  }
  if ! retry_until 60 "Argo CD server to retrieve logs from ${APP_NAME}" _app_logs_available; then
    echo "Argo CD server could not retrieve logs for ${APP_NAME}" >&2
    return 1
  fi
  log "Argo CD server retrieved logs from ${APP_NAME}"
}

patch_secretreader_volume() {
  local workload="$1" container="$2"
  kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" patch "${workload}" --type strategic --patch "
spec:
  template:
    spec:
      volumes:
        - name: secretreader-plugin
          image:
            reference: ${SECRETREADER_IMAGE}
            pullPolicy: IfNotPresent
      containers:
        - name: ${container}
          volumeMounts:
            - name: secretreader-plugin
              mountPath: /plugins/secretreader
              readOnly: true"
}

require_command argocd
require_command docker
require_command helm
require_command jq
require_command kind
require_command kubectl

case "${E2E_INSTALL_METHOD}" in
  kustomize|helm) ;;
  *)
    echo "unsupported E2E_INSTALL_METHOD: ${E2E_INSTALL_METHOD} (expected kustomize or helm)" >&2
    exit 1
    ;;
esac

if kind get clusters | grep -qx "${HUB_CLUSTER}"; then
  echo "kind cluster already exists: ${HUB_CLUSTER}" >&2
  exit 1
fi
if kind get clusters | grep -qx "${SPOKE_CLUSTER}"; then
  echo "kind cluster already exists: ${SPOKE_CLUSTER}" >&2
  exit 1
fi

cd "${REPO_ROOT}"

log "creating kind clusters"
HUB_CREATED=1
create_kind_cluster "${HUB_CLUSTER}"
if ! kubectl --context "kind-${HUB_CLUSTER}" explain pod.spec.volumes.image >/dev/null 2>&1; then
  echo "kind cluster ${HUB_CLUSTER} does not support Kubernetes ImageVolume" >&2
  exit 1
fi
SPOKE_CREATED=1
create_kind_cluster "${SPOKE_CLUSTER}"

log "loading controller image ${E2E_IMG}"
kind load docker-image "${E2E_IMG}" --name "${HUB_CLUSTER}"

log "installing Argo CD chart ${ARGOCD_CHART_VERSION}"
helm --kube-context "kind-${HUB_CLUSTER}" upgrade --install argocd argo-cd \
  --repo https://argoproj.github.io/argo-helm \
  --version "${ARGOCD_CHART_VERSION}" \
  --set "global.image.repository=${ARGOCD_IMAGE_REPOSITORY}" \
  --set "global.image.tag=${ARGOCD_IMAGE_TAG}" \
  --set "global.image.imagePullPolicy=${ARGOCD_IMAGE_PULL_POLICY}" \
  --namespace "${ARGOCD_NS}" \
  --create-namespace \
  --wait \
  --timeout 5m

log "creating ClusterProfile provider config secret"
kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" create secret generic cp-creds-secret \
  --from-file=cp-creds.json=/dev/stdin \
  --dry-run=client -o yaml <<EOF | kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" apply -f -
{
  "providers": [
    {
      "name": "secretreader",
      "execConfig": {
        "command": "${SECRETREADER_COMMAND}",
        "apiVersion": "client.authentication.k8s.io/v1",
        "provideClusterInfo": true
      }
    }
  ]
}
EOF

log "installing ClusterProfile controller via ${E2E_INSTALL_METHOD}"
if [ "${E2E_INSTALL_METHOD}" = "kustomize" ]; then
  kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" apply -k artifacts/manifests
else
  CLUSTERPROFILE_CRD_MANIFEST="$(grep -o 'https://[^[:space:]]*clusterprofiles\.yaml' artifacts/manifests/base/kustomization.yaml)"
  kubectl --context "kind-${HUB_CLUSTER}" apply -f "${CLUSTERPROFILE_CRD_MANIFEST}"
  helm --kube-context "kind-${HUB_CLUSTER}" upgrade --install cpia charts/argocd-clusterprofile-controller \
    --namespace "${ARGOCD_NS}" \
    --set fullnameOverride=argocd \
    --set "image.repository=${E2E_IMG%:*}" \
    --set "image.tag=${E2E_IMG##*:}" \
    --set "controller.clusterProfileProvidersFile=/app/cp-creds/cp-creds.json" \
    --set-json 'controller.extraVolumes=[{"name":"cp-creds-vol","secret":{"secretName":"cp-creds-secret"}}]' \
    --set-json 'controller.extraVolumeMounts=[{"name":"cp-creds-vol","mountPath":"/app/cp-creds"}]' \
    --wait \
    --timeout 5m
fi

for resource in $(kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" get deploy,sts -o name); do
  kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" rollout status "${resource}" --timeout=300s
done

log "configuring spoke cluster credentials"
kubectl --context "kind-${SPOKE_CLUSTER}" apply -f - <<'EOF'
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
kubectl --context "kind-${SPOKE_CLUSTER}" create namespace guestbook
kubectl --context "kind-${SPOKE_CLUSTER}" -n kube-system wait --for=jsonpath='{.data.token}' secret/argocd-manager-token --timeout=120s

SPOKE_IP="$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "${SPOKE_CLUSTER}-control-plane")"
SPOKE_CA="$(kubectl --context "kind-${SPOKE_CLUSTER}" config view --raw --minify --flatten -o jsonpath='{.clusters[0].cluster.certificate-authority-data}')"
SPOKE_TOKEN="$(kubectl --context "kind-${SPOKE_CLUSTER}" -n kube-system get secret argocd-manager-token -o jsonpath='{.data.token}' | base64 -d)"
APP_CONTROLLER_SA="$(
  kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" get sts/argocd-application-controller \
    -o jsonpath='{.spec.template.spec.serviceAccountName}'
)"
APP_CONTROLLER_SA="${APP_CONTROLLER_SA:-argocd-application-controller}"
SERVER_SA="$(
  kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" get deploy/argocd-server \
    -o jsonpath='{.spec.template.spec.serviceAccountName}'
)"
SERVER_SA="${SERVER_SA:-argocd-server}"

log "creating secretreader token Secret and RBAC"
printf '%s' "${SPOKE_TOKEN}" | kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" create secret generic "${CP_NAME}" \
  --from-file=token=/dev/stdin \
  --dry-run=client -o yaml | kubectl --context "kind-${HUB_CLUSTER}" apply -f -

kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" apply -f - <<EOF
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
      - ${CP_NAME}
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
    namespace: ${ARGOCD_NS}
  - kind: ServiceAccount
    name: ${SERVER_SA}
    namespace: ${ARGOCD_NS}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: argocd-secretreader
EOF

log "mounting secretreader plugin"
patch_secretreader_volume sts/argocd-application-controller application-controller
patch_secretreader_volume deploy/argocd-server server

if [ "${E2E_INSTALL_METHOD}" = "kustomize" ]; then
  log "mounting ClusterProfile provider config into controller"
  kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" patch deploy/argocd-clusterprofile-controller --type strategic --patch '
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
            - "--clusterprofile-provider-file=/app/cp-creds/cp-creds.json"'
fi

kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" rollout status sts/argocd-application-controller --timeout=300s
kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" rollout status deploy/argocd-server --timeout=300s
kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" rollout status deploy/argocd-clusterprofile-controller --timeout=300s

log "creating ClusterProfile"
kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" apply -f - <<EOF
apiVersion: multicluster.x-k8s.io/v1alpha1
kind: ClusterProfile
metadata:
  name: ${CP_NAME}
  namespace: ${ARGOCD_NS}
  labels:
    environment: e2e
    team: platform
spec:
  clusterManager:
    name: manual
  displayName: Spoke Cluster Full E2E
EOF
STATUS_PATCH="$(cat <<EOF
{
  "status": {
    "accessProviders": [
      {
        "name": "secretreader",
        "cluster": {
          "server": "https://${SPOKE_IP}:6443",
          "certificate-authority-data": "${SPOKE_CA}",
          "extensions": [
            {
              "name": "client.authentication.k8s.io/exec",
              "extension": {
                "clusterName": "${CP_NAME}"
              }
            }
          ]
        }
      }
    ]
  }
}
EOF
)"
kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" patch clusterprofile "${CP_NAME}" --subresource=status --type=merge \
  -p "${STATUS_PATCH}"

if ! retry_until 120 "ClusterProfile cluster Secret for ${CLUSTER_NAME}" find_cluster_secret; then
  echo "ClusterProfile controller did not create a cluster Secret for ${CLUSTER_NAME}" >&2
  kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" get secrets -o wide
  exit 1
fi
SECRET_JSON="$(kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" get secret "${SECRET_NAME}" -o json)"
CONFIG="$(printf '%s' "${SECRET_JSON}" | jq -r '.data.config' | base64 -d)"

test "$(printf '%s' "${SECRET_JSON}" | jq -r '.metadata.labels.environment')" = "e2e"
test "$(printf '%s' "${SECRET_JSON}" | jq -r '.metadata.labels.team')" = "platform"
test "$(printf '%s' "${SECRET_JSON}" | jq -r '.data.server' | base64 -d)" = "https://${SPOKE_IP}:6443"
test "$(printf '%s' "${CONFIG}" | jq -r '.execProviderConfig.command')" = "${SECRETREADER_COMMAND}"
test "$(printf '%s' "${CONFIG}" | jq -r '.execProviderConfig.args // [] | length')" = "0"
test "$(printf '%s' "${CONFIG}" | jq -r '.execProviderConfig.apiVersion')" = "client.authentication.k8s.io/v1"
test "$(printf '%s' "${CONFIG}" | jq -r '.execProviderConfig.provideClusterInfo')" = "true"
test "$(printf '%s' "${CONFIG}" | jq -r '.execProviderConfig.config.clusterName')" = "${CP_NAME}"
test "$(printf '%s' "${CONFIG}" | jq -r '.tlsClientConfig.caData | length')" -gt 100

log "creating ApplicationSet"
kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" apply -f - <<EOF
apiVersion: argoproj.io/v1alpha1
kind: ApplicationSet
metadata:
  name: guestbook-e2e
  namespace: ${ARGOCD_NS}
spec:
  generators:
  - clusters:
      selector:
        matchLabels:
          environment: e2e
  goTemplate: true
  template:
    metadata:
      name: 'guestbook-{{ .nameNormalized }}'
    spec:
      project: default
      source:
        repoURL: https://github.com/argoproj/argocd-example-apps.git
        targetRevision: ${GUESTBOOK_REVISION}
        path: guestbook
      destination:
        server: '{{ .server }}'
        namespace: guestbook
      syncPolicy:
        automated:
          prune: true
          selfHeal: true
        syncOptions:
        - CreateNamespace=true
EOF

for i in $(seq 1 120); do
  if kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" get application "${APP_NAME}" >/dev/null 2>&1; then
    log "application ${APP_NAME} created"
    break
  fi
  if [ "${i}" = 120 ]; then
    echo "application ${APP_NAME} was not created" >&2
    exit 1
  fi
  sleep 1
done

wait_for_application
kubectl --context "kind-${SPOKE_CLUSTER}" -n guestbook wait --for=condition=Ready pod -l app=guestbook-ui --timeout=300s
verify_argocd_server_cluster_access

log "full e2e passed"
