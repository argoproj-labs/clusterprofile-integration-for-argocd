#!/usr/bin/env bash

set -euo pipefail

ARGOCD_CHART_VERSION="${ARGOCD_CHART_VERSION:-9.5.19}"
GUESTBOOK_REVISION="${GUESTBOOK_REVISION:-8088f4c0d970abb09e250248cc97e35623447cb5}"
E2E_IMG="${E2E_IMG:-controller:dev}"
E2E_PREFIX="${E2E_PREFIX:-cpia-e2e-$$}"
HUB_CLUSTER="${HUB_CLUSTER:-${E2E_PREFIX}-hub}"
SPOKE_CLUSTER="${SPOKE_CLUSTER:-${E2E_PREFIX}-spoke}"
ARGOCD_NS="${ARGOCD_NS:-argocd}"
APP_NAME="guestbook-spoke-cluster-full"
CP_NAME="spoke-cluster-full"
SECRET_NAME="cluster-${CP_NAME}"

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

require_command docker
require_command helm
require_command jq
require_command kind
require_command kubectl

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
kind create cluster --name "${HUB_CLUSTER}" --wait 120s
SPOKE_CREATED=1
kind create cluster --name "${SPOKE_CLUSTER}" --wait 120s

log "loading controller image ${E2E_IMG}"
kind load docker-image "${E2E_IMG}" --name "${HUB_CLUSTER}"

log "installing Argo CD chart ${ARGOCD_CHART_VERSION} and ClusterProfile controller"
helm --kube-context "kind-${HUB_CLUSTER}" upgrade --install argocd argo-cd \
  --repo https://argoproj.github.io/argo-helm \
  --version "${ARGOCD_CHART_VERSION}" \
  --namespace "${ARGOCD_NS}" \
  --create-namespace \
  --wait \
  --timeout 5m
kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" apply -k artifacts/manifests

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

log "mounting Argo CD exec plugin and ClusterProfile provider config"
kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" create configmap argocd-custom-auth-plugin \
  --from-literal=get-token.sh="#!/bin/sh
cat <<EOF
{
  \"apiVersion\": \"client.authentication.k8s.io/v1beta1\",
  \"kind\": \"ExecCredential\",
  \"status\": {
    \"token\": \"${SPOKE_TOKEN}\"
  }
}
EOF
" \
  --dry-run=client -o yaml | kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" apply -f -

kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" patch sts/argocd-application-controller --type strategic --patch '
spec:
  template:
    spec:
      volumes:
        - name: auth-script
          configMap:
            name: argocd-custom-auth-plugin
            defaultMode: 0755
      containers:
        - name: application-controller
          volumeMounts:
            - name: auth-script
              mountPath: /usr/local/bin/custom-auth'

kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" create secret generic cp-creds-secret \
  --from-literal=cp-creds.json='{"providers":[{"name":"hub-provider","execConfig":{"command":"/usr/local/bin/custom-auth/get-token.sh","apiVersion":"client.authentication.k8s.io/v1beta1"}}]}' \
  --dry-run=client -o yaml | kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" apply -f -

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
            - "--cluster-profile-providers-file=/app/cp-creds/cp-creds.json"'

kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" rollout status sts/argocd-application-controller --timeout=300s
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
kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" patch clusterprofile "${CP_NAME}" --subresource=status --type=merge \
  -p '{"status":{"accessProviders":[{"name":"hub-provider","cluster":{"server":"https://'"${SPOKE_IP}"':6443","certificate-authority-data":"'"${SPOKE_CA}"'"}}]}}'

kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" wait --for=jsonpath='{.metadata.labels.environment}'=e2e "secret/${SECRET_NAME}" --timeout=120s
SECRET_JSON="$(kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" get secret "${SECRET_NAME}" -o json)"
CONFIG="$(printf '%s' "${SECRET_JSON}" | jq -r '.data.config' | base64 -d)"

test "$(printf '%s' "${SECRET_JSON}" | jq -r '.metadata.labels["argocd.argoproj.io/secret-type"]')" = "cluster"
test "$(printf '%s' "${SECRET_JSON}" | jq -r '.metadata.labels["argocd.argoproj.io/cluster-profile-origin"]')" = "${ARGOCD_NS}-${CP_NAME}"
test "$(printf '%s' "${SECRET_JSON}" | jq -r '.metadata.labels.environment')" = "e2e"
test "$(printf '%s' "${SECRET_JSON}" | jq -r '.metadata.labels.team')" = "platform"
test "$(printf '%s' "${SECRET_JSON}" | jq -r '.data.server' | base64 -d)" = "https://${SPOKE_IP}:6443"
test "$(printf '%s' "${CONFIG}" | jq -r '.execProviderConfig.command')" = "/usr/local/bin/custom-auth/get-token.sh"
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

log "full e2e passed"
