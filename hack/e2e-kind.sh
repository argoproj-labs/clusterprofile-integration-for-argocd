#!/usr/bin/env bash

set -euo pipefail

ARGOCD_CHART_VERSION="${ARGOCD_CHART_VERSION:-10.4.0}"
ARGOCD_IMAGE_REPOSITORY="${ARGOCD_IMAGE_REPOSITORY:-quay.io/argoproj/argocd}"
ARGOCD_IMAGE_TAG="${ARGOCD_IMAGE_TAG:-v3.5.1}"
ARGOCD_IMAGE_PULL_POLICY="${ARGOCD_IMAGE_PULL_POLICY:-Always}"
GUESTBOOK_REVISION="${GUESTBOOK_REVISION:-8088f4c0d970abb09e250248cc97e35623447cb5}"
E2E_IMG="${E2E_IMG:-ghcr.io/argoproj-labs/clusterprofile-integration-for-argocd:latest}"
E2E_IMAGE_REPOSITORY="${E2E_IMG%:*}"
E2E_IMAGE_TAG="${E2E_IMG##*:}"
E2E_PREFIX="${E2E_PREFIX:-cpia-e2e-$$}"
E2E_INSTALL_METHOD="${E2E_INSTALL_METHOD:-helm}"
KIND_NODE_IMAGE="${KIND_NODE_IMAGE:-}"
SECRETREADER_IMAGE="${SECRETREADER_IMAGE:-registry.k8s.io/cluster-inventory-api/secretreader:v0.1.3}"
SECRETREADER_COMMAND="${SECRETREADER_COMMAND:-/plugins/secretreader/bin/secretreader-plugin}"
HUB_CLUSTER="${HUB_CLUSTER:-${E2E_PREFIX}-hub}"
SPOKE_CLUSTER="${SPOKE_CLUSTER:-${E2E_PREFIX}-spoke}"
SPOKE_TLS_SERVER_NAME="${SPOKE_CLUSTER}-control-plane"
INVALID_SPOKE_TLS_SERVER_NAME="invalid-${SPOKE_CLUSTER}.example.com"
UNREACHABLE_PROXY_URL="http://127.0.0.1:1"
ARGOCD_NS="${ARGOCD_NS:-argocd}"
MIRROR_NS="${MIRROR_NS:-team-a}"
RBAC_UNWATCHED_NS="rbac-unwatched"
APP_NAME="guestbook-spoke-cluster-full"
CP_NAME="spoke-cluster-full"
SECRET_NAME="cluster-${CP_NAME}"
COLLISION_CP_NAME="provenance-collision"
COLLISION_SECRET_NAME="cluster-${COLLISION_CP_NAME}"
COLLISION_SERVER="https://manual-collision.example.com:6443"
LONG_LABEL_CP_NAME="$(printf 'l%.0s' {1..64})"
LONG_SECRET_CP_NAME="$(printf 's%.0s' {1..246})"
MAX_LENGTH_CP_NAME="$(printf 'm%.0s' {1..253})"
# Named so that its raw Secret name is what LONG_SECRET_CP_NAME would bound down to,
# which is the digest TestGeneratedClusterProfileMetadata pins.
RAW_COLLISION_CP_NAME="$(printf 's%.0s' {1..212})-c487d7cd89959dbc1df6f5deec5584b5"
LONG_LABEL_SERVER="https://long-label.example.com:6443"
LONG_SECRET_SERVER="https://long-secret.example.com:6443"
MAX_LENGTH_SERVER="https://max-length.example.com:6443"
RAW_COLLISION_SERVER="https://raw-collision.example.com:6443"
OUT_OF_CLUSTER_CP_NAME="explicit-kubeconfig-context"
OUT_OF_CLUSTER_SECRET_NAME="cluster-${OUT_OF_CLUSTER_CP_NAME}"
OUT_OF_CLUSTER_SERVER="https://explicit-kubeconfig.example.com:6443"
REMOTE_ONLY_CP_NAME="remote-only-rbac"
REMOTE_ONLY_SECRET_NAME="cluster-${REMOTE_ONLY_CP_NAME}"
REMOTE_ONLY_SERVER="https://remote-only.example.com:6443"
DUPLICATE_MEMBER_ID="inventory-member-e2e"
DUPLICATE_OLDEST_CP_NAME="inventory-member-primary"
DUPLICATE_NEWER_CP_NAME="inventory-member-duplicate"
DUPLICATE_MIRROR_CP_NAME="inventory-member-mirror"
DUPLICATE_OLDEST_SECRET_NAME="cluster-${DUPLICATE_OLDEST_CP_NAME}"
DUPLICATE_NEWER_SECRET_NAME="cluster-${DUPLICATE_NEWER_CP_NAME}"
DUPLICATE_MIRROR_SECRET_NAME="cluster-${DUPLICATE_MIRROR_CP_NAME}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
WORK_DIR="$(mktemp -d)"
export KUBECONFIG="${WORK_DIR}/kubeconfig"
ARGOCD_CONFIG="${WORK_DIR}/argocd-config"
ARGOCD_CONTEXT="${E2E_PREFIX}-server"
HUB_CREATED=0
SPOKE_CREATED=0
OUT_OF_CLUSTER_CONTROLLER_PID=""
OUT_OF_CLUSTER_CONTROLLER_BIN="${WORK_DIR}/argocd-clusterprofile-controller"
OUT_OF_CLUSTER_CONTROLLER_LOG="${WORK_DIR}/out-of-cluster-controller.log"
SHUTDOWN_LOG_PID=""
CONTROLLER_SUBJECT=""

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
  shift
  local -a image_args=()
  if [ -n "${KIND_NODE_IMAGE}" ]; then
    image_args=(--image "${KIND_NODE_IMAGE}")
  fi
  kind create cluster --name "${cluster_name}" "${image_args[@]}" --wait 120s "$@"
}

stop_background() {
  local pid="$1"
  [ -n "${pid}" ] || return 0
  kill "${pid}" >/dev/null 2>&1 || true
  wait "${pid}" 2>/dev/null || true
}

stop_out_of_cluster_controller() {
  local i
  [ -n "${OUT_OF_CLUSTER_CONTROLLER_PID}" ] || return 0
  kill "${OUT_OF_CLUSTER_CONTROLLER_PID}" >/dev/null 2>&1 || true
  # The controller shuts down gracefully; escalate only if it overruns its budget.
  for i in $(seq 1 30); do
    if ! kill -0 "${OUT_OF_CLUSTER_CONTROLLER_PID}" >/dev/null 2>&1; then
      break
    fi
    sleep 1
  done
  kill -KILL "${OUT_OF_CLUSTER_CONTROLLER_PID}" >/dev/null 2>&1 || true
  wait "${OUT_OF_CLUSTER_CONTROLLER_PID}" 2>/dev/null || true
  OUT_OF_CLUSTER_CONTROLLER_PID=""
}

stop_shutdown_log_capture() {
  stop_background "${SHUTDOWN_LOG_PID}"
  SHUTDOWN_LOG_PID=""
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
  if kubectl --context "kind-${HUB_CLUSTER}" get namespace "${MIRROR_NS}" >/dev/null 2>&1; then
    log "mirror namespace state"
    kubectl --context "kind-${HUB_CLUSTER}" -n "${MIRROR_NS}" get clusterprofiles,secrets -o wide
  fi
  if kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" get pods >/dev/null 2>&1; then
    log "ClusterProfile controller logs"
    for pod in $(controller_pod_names); do
      log "logs for ${pod}"
      kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" \
        logs "pod/${pod}" --all-containers --tail=200
      kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" \
        logs "pod/${pod}" --all-containers --previous --tail=200
    done
    log "ApplicationSet controller logs"
    kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" logs deploy/argocd-applicationset-controller --tail=200
    log "Application controller logs"
    kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" logs sts/argocd-application-controller --tail=200
    log "Application state"
    kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" get applications.argoproj.io "${APP_NAME}" -o yaml
  fi
  if [ -s "${OUT_OF_CLUSTER_CONTROLLER_LOG}" ]; then
    log "out-of-cluster controller logs"
    tail -200 "${OUT_OF_CLUSTER_CONTROLLER_LOG}"
  fi
}

cleanup() {
  status=$?
  if [ "${status}" -ne 0 ]; then
    dump_diagnostics
  fi
  stop_shutdown_log_capture
  stop_out_of_cluster_controller
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

assert_can_i() {
  local result
  result="$(kubectl --context "kind-${HUB_CLUSTER}" auth can-i --as "${CONTROLLER_SUBJECT}" "$@")"
  if [ "${result}" != "yes" ]; then
    echo "controller ServiceAccount cannot $*: ${result}" >&2
    exit 1
  fi
}

assert_cannot_i() {
  local result
  result="$(kubectl --context "kind-${HUB_CLUSTER}" auth can-i --as "${CONTROLLER_SUBJECT}" "$@" || true)"
  if [ "${result}" != "no" ]; then
    echo "controller ServiceAccount unexpectedly can $*: ${result}" >&2
    exit 1
  fi
}

remote_only_fixture_is_reconciled() {
  local secret_json
  secret_json="$(
    kubectl --context "kind-${HUB_CLUSTER}" -n "${MIRROR_NS}" \
      get secret "${REMOTE_ONLY_SECRET_NAME}" -o json
  )" || return 1
  jq -e \
    --arg name "${REMOTE_ONLY_CP_NAME}" \
    --arg server "${REMOTE_ONLY_SERVER}" \
    '.metadata.labels["argocd.argoproj.io/cluster-profile-name"] == $name and
     .metadata.annotations["argocd.argoproj.io/cluster-profile-name"] == $name and
     (.data.server | @base64d) == $server' <<<"${secret_json}" >/dev/null
}

remote_only_fixture_is_deleted() {
  local profile secret
  profile="$(
    kubectl --context "kind-${HUB_CLUSTER}" -n "${MIRROR_NS}" \
      get clusterprofile "${REMOTE_ONLY_CP_NAME}" --ignore-not-found -o name
  )" || return 1
  secret="$(
    kubectl --context "kind-${HUB_CLUSTER}" -n "${MIRROR_NS}" \
      get secret "${REMOTE_ONLY_SECRET_NAME}" --ignore-not-found -o name
  )" || return 1
  [ -z "${profile}${secret}" ]
}

controller_logs_since_time() {
  local since_time="$1" pod
  for pod in $(controller_pod_names); do
    kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" \
      logs "pod/${pod}" -c "${CONTROLLER_CONTAINER_NAME}" \
      --since-time="${since_time}"
  done
}

controller_logs_since() {
  local since="$1" pod
  for pod in $(controller_pod_names); do
    kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" \
      logs "pod/${pod}" -c "${CONTROLLER_CONTAINER_NAME}" \
      --since="${since}"
  done
}

remote_only_transient_error_is_logged() {
  controller_logs_since_time "${REMOTE_ONLY_STARTED_AT}" 2>/dev/null | \
    jq -Rse '
      split("\n") |
      any(.[];
        (fromjson? // {}) |
        ((.error // "") |
          contains("unsupported built-in access provider \"argo-cd-builtin-unsupported\"")))' \
      >/dev/null
}

controller_metrics_snapshot() {
  local pod metrics=""
  for pod in $(controller_pod_names); do
    metrics+="$(
      kubectl --context "kind-${HUB_CLUSTER}" --request-timeout=5s get --raw \
        "/api/v1/namespaces/${ARGOCD_NS}/pods/${pod}:8080/proxy/metrics"
    )"$'\n'
  done
  [ -n "${metrics}" ] || return 1
  awk '
      $1 ~ /^controller_runtime_active_workers\{/ && $1 ~ /controller="clusterprofile"/ {
        active_workers += $2
        saw_active_workers = 1
      }
      $1 ~ /^controller_runtime_reconcile_errors_total\{/ && $1 ~ /controller="clusterprofile"/ {
        errors += $2
        saw_errors = 1
      }
      $1 ~ /^controller_runtime_reconcile_panics_total\{/ && $1 ~ /controller="clusterprofile"/ {
        panics += $2
        saw_panics = 1
      }
      $1 ~ /^controller_runtime_terminal_reconcile_errors_total\{/ && $1 ~ /controller="clusterprofile"/ {
        terminal_errors += $2
        saw_terminal_errors = 1
      }
      $1 ~ /^controller_runtime_reconcile_timeouts_total\{/ && $1 ~ /controller="clusterprofile"/ {
        timeouts += $2
        saw_timeouts = 1
      }
      $1 ~ /^workqueue_depth\{/ && $1 ~ /controller="clusterprofile"/ && $1 ~ /name="clusterprofile"/ {
        # The priority-labelled depth series is absent before the first enqueue;
        # the zero-initialized sum intentionally treats that state as depth zero.
        queue_depth += $2
      }
      $1 ~ /^workqueue_retries_total\{/ && $1 ~ /controller="clusterprofile"/ && $1 ~ /name="clusterprofile"/ {
        retries += $2
        saw_retries = 1
      }
      END {
        if (!(saw_active_workers && saw_errors && saw_panics && saw_terminal_errors &&
              saw_timeouts && saw_retries)) {
          exit 1
        }
        printf "{\"activeWorkers\":%g,\"errors\":%g,\"panics\":%g,", active_workers, errors, panics
        printf "\"terminalErrors\":%g,\"timeouts\":%g,", terminal_errors, timeouts
        printf "\"queueDepth\":%g,\"retries\":%g}\n", queue_depth, retries
      }
    ' <<<"${metrics}"
}

controller_metrics_delta() {
  jq -cn --argjson before "$1" --argjson after "$2" \
    '$after as $a | $before as $b | {
      errors: ($a.errors - $b.errors),
      retries: ($a.retries - $b.retries),
      panics: ($a.panics - $b.panics),
      terminalErrors: ($a.terminalErrors - $b.terminalErrors),
      timeouts: ($a.timeouts - $b.timeouts)
    }'
}

controller_workqueue_is_idle() {
  local snapshot
  snapshot="$(controller_metrics_snapshot)" || return 1
  jq -e '.activeWorkers == 0 and .queueDepth == 0' <<<"${snapshot}" >/dev/null
}

controller_metrics_are_available() {
  controller_metrics_snapshot >/dev/null
}

controller_is_stopped() {
  local replicas
  replicas="$(
    kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" \
      get deploy/argocd-clusterprofile-controller -o jsonpath='{.status.replicas}'
  )" || return 1
  [ -z "${replicas}" ] || [ "${replicas}" = "0" ]
}

controller_pod_names() {
  kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" \
    get pods -l app.kubernetes.io/name=argocd-clusterprofile-controller -o json | jq -r '
      .items[] |
      select(.metadata.deletionTimestamp == null) |
      .metadata.name'
}

deployment_is_ready_with_replicas() {
  local replicas="$1"
  kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" \
    get deployment argocd-clusterprofile-controller -o json | jq -e --argjson n "${replicas}" '
      .status.observedGeneration == .metadata.generation and
      .spec.replicas == $n and
      .status.replicas == $n and
      .status.updatedReplicas == $n and
      .status.readyReplicas == $n and
      .status.availableReplicas == $n and
      ((.status.unavailableReplicas // 0) == 0)' >/dev/null
}

controllers_are_spread() {
  kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" \
    get pods -l app.kubernetes.io/name=argocd-clusterprofile-controller -o json | jq -e '
      [.items[] |
        select(.metadata.deletionTimestamp == null) |
        select(any(.status.conditions[]?; .type == "Ready" and .status == "True")) |
        .spec.nodeName] |
      length == 2 and (unique | length) == 2' >/dev/null
}

shutdown_controller_container_is_completed() {
  local pod_json
  pod_json="$(
    kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" \
      get "pod/${CONTROLLER_POD}" -o json
  )" || return 1
  jq -e \
    --arg container "${CONTROLLER_CONTAINER_NAME}" '
      (first(.status.containerStatuses[]? | select(.name == $container)) // {}) as $status |
      .metadata.deletionTimestamp != null and
      $status.ready == false and
      $status.restartCount == 0 and
      $status.state.terminated.reason == "Completed" and
      $status.state.terminated.exitCode == 0 and
      ($status.state.terminated.signal // 0) == 0' <<<"${pod_json}" >/dev/null
}

shutdown_controller_pod_is_gone() {
  ! kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" \
    get "pod/${CONTROLLER_POD}" >/dev/null 2>&1
}

controller_pods_state() {
  kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" \
    get pods -l app.kubernetes.io/name=argocd-clusterprofile-controller -o json | jq -c \
      --arg container "${CONTROLLER_CONTAINER_NAME}" \
      --arg image "${E2E_IMG}" \
      '[.items[] |
        select(.metadata.deletionTimestamp == null) as $pod |
        ($pod.spec.containers[]? | select(.name == $container and .image == $image)) as $spec |
        $pod.status.containerStatuses[]? |
        select(.name == $container) |
        {
          pod: $pod.metadata.name,
          serviceAccount: $pod.spec.serviceAccountName,
          image: $spec.image,
          imageID: .imageID,
          containerID: .containerID,
          ready: .ready,
          running: (.state.running != null),
          restartCount: .restartCount
        }]'
}

wait_for_controller_pods() {
  local expected="$1" filter="$2" state="[]" _
  for _ in $(seq 1 60); do
    state="$(controller_pods_state | jq -c "map(select(${filter}))")"
    if [ "$(jq 'length' <<<"${state}")" = "${expected}" ]; then
      break
    fi
    sleep 1
  done
  printf '%s\n' "${state}"
}

collision_secret_snapshot() {
  kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" \
    get secret "${COLLISION_SECRET_NAME}" -o json | jq -cS '{
      uid: .metadata.uid,
      resourceVersion: .metadata.resourceVersion,
      labels: .metadata.labels,
      type: .type,
      data: .data,
      ownerReferences: (.metadata.ownerReferences // [])
    }'
}

provenance_conflict_is_logged() {
  controller_logs_since 5m 2>/dev/null | \
    grep -F "refusing to mutate Secret ${ARGOCD_NS}/${COLLISION_SECRET_NAME} without provenance from ClusterProfile ${ARGOCD_NS}/${COLLISION_CP_NAME}" \
      >/dev/null
}

clusterprofile_uid() {
  local namespace="$1" profile_name="$2"
  kubectl --context "kind-${HUB_CLUSTER}" -n "${namespace}" \
    get clusterprofile "${profile_name}" -o jsonpath='{.metadata.uid}'
}

owned_secret_for_profile() {
  local profile_uid="$1"
  kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" get secrets -o json | jq -c \
    --arg profile_uid "${profile_uid}" \
    'first(.items[] | select(any(.metadata.ownerReferences[]?;
      .apiVersion == "multicluster.x-k8s.io/v1alpha1" and
      .kind == "ClusterProfile" and
      .uid == $profile_uid and
      .controller == true))) // empty'
}

long_name_secret_is_ready() {
  local profile_name="$1" profile_uid="$2" server="$3" secret_json
  secret_json="$(owned_secret_for_profile "${profile_uid}")" || return 1
  [ -n "${secret_json}" ] || return 1
  jq -e \
    --arg profile_name "${profile_name}" \
    --arg server "${server}" \
    '.metadata.annotations["argocd.argoproj.io/cluster-profile-name"] == $profile_name and
     .metadata.labels["argocd.argoproj.io/secret-type"] == "cluster" and
     (.data.name | @base64d) == $profile_name and
     (.data.server | @base64d) == $server' <<<"${secret_json}" >/dev/null
}

secret_exists() {
  local namespace="$1" secret_name="$2"
  kubectl --context "kind-${HUB_CLUSTER}" -n "${namespace}" \
    get secret "${secret_name}" >/dev/null 2>&1
}

secret_is_gone() {
  ! secret_exists "$@"
}

assert_secret_exists() {
  local namespace="$1" secret_name="$2"
  if ! secret_exists "${namespace}" "${secret_name}"; then
    echo "Secret ${namespace}/${secret_name} unexpectedly does not exist" >&2
    exit 1
  fi
}

inventory_warning_is_recorded() {
  local namespace="$1" profile_name="$2" reason="$3"
  kubectl --context "kind-${HUB_CLUSTER}" -n "${namespace}" get events.events.k8s.io -o json \
    --field-selector "reason=${reason},regarding.apiVersion=multicluster.x-k8s.io/v1alpha1,regarding.kind=ClusterProfile,regarding.name=${profile_name}" |
    jq -e 'any(.items[]; .type == "Warning")' >/dev/null
}

clusterprofile_owned_secrets_are_gone() {
  local namespace="$1"
  kubectl --context "kind-${HUB_CLUSTER}" -n "${namespace}" get secrets -o json | jq -e '
    all(.items[];
      all(.metadata.ownerReferences[]?;
        .apiVersion != "multicluster.x-k8s.io/v1alpha1" or
        .kind != "ClusterProfile"))' >/dev/null
}

argocd_workloads_are_stopped() {
  kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" \
    get deployments,statefulsets -o json | jq -e \
      --arg controller "argocd-clusterprofile-controller" '
        all(.items[];
          if .metadata.name == $controller then
            true
          else
            .spec.replicas == 0 and ((.status.replicas // 0) == 0)
          end)' >/dev/null
}

set_builtin_profile_access() {
  local profile_name="$1" server="$2" namespace="${3:-${ARGOCD_NS}}"
  kubectl --context "kind-${HUB_CLUSTER}" -n "${namespace}" \
    patch clusterprofile "${profile_name}" --subresource=status --type=merge -p "{
      \"status\": {
        \"accessProviders\": [
          {
            \"name\": \"argo-cd-builtin-gcp\",
            \"cluster\": {\"server\": \"${server}\"}
          }
        ]
      }
    }" >/dev/null
}

apply_inventory_member_profile() {
  local profile_name="$1" namespace="${2:-${ARGOCD_NS}}"
  kubectl --context "kind-${HUB_CLUSTER}" apply -f - <<EOF
apiVersion: multicluster.x-k8s.io/v1alpha1
kind: ClusterProfile
metadata:
  name: ${profile_name}
  namespace: ${namespace}
  labels:
    multicluster.x-k8s.io/inventory-member-id: ${DUPLICATE_MEMBER_ID}
spec:
  clusterManager:
    name: manual
EOF
  set_builtin_profile_access "${profile_name}" "https://${profile_name}.example.com:6443" "${namespace}"
}

build_live_status_patch() {
  local tls_server_name="$1" insecure="$2" proxy_url="$3" ca_data
  ca_data="${SPOKE_CA}"
  if [ "${insecure}" = "true" ]; then
    ca_data=""
  fi
  jq -cn \
    --arg server "https://${SPOKE_IP}:6443" \
    --arg ca_data "${ca_data}" \
    --arg tls_server_name "${tls_server_name}" \
    --arg proxy_url "${proxy_url}" \
    --arg extension_cluster_name "${CP_NAME}" \
    --argjson insecure "${insecure}" \
    '{
      status: {
        accessProviders: [
          {
            name: "secretreader",
            cluster: ({
                server: $server,
                "tls-server-name": $tls_server_name,
                "insecure-skip-tls-verify": $insecure,
                "proxy-url": $proxy_url,
                "disable-compression": true,
                extensions: [
                  {
                    name: "client.authentication.k8s.io/exec",
                    extension: {clusterName: $extension_cluster_name}
                  }
                ]
              } + if $ca_data == "" then
                {}
              else
                {"certificate-authority-data": $ca_data}
              end)
          }
        ]
      }
    }'
}

patch_live_cluster_connection() {
  local tls_server_name="$1" insecure="$2" proxy_url="$3" patch
  patch="$(build_live_status_patch "${tls_server_name}" "${insecure}" "${proxy_url}")"
  kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" \
    patch clusterprofile "${CP_NAME}" --subresource=status --type=merge -p "${patch}" >/dev/null
}

out_of_cluster_controller_is_running() {
  [ -n "${OUT_OF_CLUSTER_CONTROLLER_PID}" ] &&
    kill -0 "${OUT_OF_CLUSTER_CONTROLLER_PID}" >/dev/null 2>&1
}

managed_cluster_secret_has_exact_data() {
  local secret_json
  secret_json="$(
    kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" \
      get secret "${SECRET_NAME}" -o json
  )" || return 1
  jq -e \
    --arg name "${CP_NAME}" \
    --arg server "https://${SPOKE_IP}:6443" \
    --arg command "${SECRETREADER_COMMAND}" \
    '(.data | keys | sort) == ["config", "name", "server"] and
     (.data.name | @base64d) == $name and
     (.data.server | @base64d) == $server and
     ((.data.config | @base64d | fromjson).execProviderConfig.command) == $command' \
    <<<"${secret_json}" >/dev/null
}

managed_cluster_secret_has_connection_fields() {
  local tls_server_name="$1" insecure="$2" proxy_url="$3" secret_json
  secret_json="$(
    kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" \
      get secret "${SECRET_NAME}" -o json
  )" || return 1
  jq -e \
    --arg tls_server_name "${tls_server_name}" \
    --arg proxy_url "${proxy_url}" \
    --argjson insecure "${insecure}" \
    '(.data.config | @base64d | fromjson) as $config |
     $config.tlsClientConfig.serverName == $tls_server_name and
     $config.tlsClientConfig.insecure == $insecure and
     (if $insecure then
        ($config.tlsClientConfig.caData // "") == ""
      else
        ($config.tlsClientConfig.caData | length) > 100
      end) and
     ($config.proxyUrl // "") == $proxy_url and
     $config.disableCompression == true' <<<"${secret_json}" >/dev/null
}

argocd_cluster_is_absent() {
  local cluster_json
  cluster_json="$(argocd_cluster_list_json)" || return 1
  [ "$(
    printf '%s' "${cluster_json}" | jq -r \
      --arg name "${CP_NAME}" \
      --arg server "https://${SPOKE_IP}:6443" \
      '[.[] | select(.name == $name and .server == $server)] | length'
  )" = "0" ]
}

generated_application_is_gone() {
  ! application_exists
}

argocd_cluster_state_is() {
  local want="$1" cluster_json
  cluster_json="$(argocd_cluster_list_json)" || return 1
  [ "$(
    printf '%s' "${cluster_json}" | jq -r \
      --arg name "${CP_NAME}" \
      --arg server "https://${SPOKE_IP}:6443" \
      'first(.[] | select(.name == $name and .server == $server) | .connectionState.status) // ""'
  )" = "${want}" ]
}

restored_cluster_secret_is_ready() {
  local secret_json
  secret_json="$(
    kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" \
      get secret "${SECRET_NAME}" -o json
  )" || return 1
  jq -e \
    --arg old_uid "${REVOKED_SECRET_UID}" \
    --arg profile_uid "${LIVE_CP_UID}" \
    --arg server "https://${SPOKE_IP}:6443" \
    '.metadata.uid != $old_uid and
     (.data.server | @base64d) == $server and
     any(.metadata.ownerReferences[]?; .uid == $profile_uid)' <<<"${secret_json}" >/dev/null
}

application_exists() {
  kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" \
    get application "${APP_NAME}" >/dev/null 2>&1
}

wait_for_application() {
  local status sync health phase
  for i in $(seq 1 600); do
    status="$(kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" get application "${APP_NAME}" \
      -o jsonpath='{.status.sync.status}|{.status.health.status}|{.status.operationState.phase}' 2>/dev/null || true)"
    IFS='|' read -r sync health phase <<<"${status}"
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

login_argocd_server() {
  local admin_password
  admin_password="$(
    kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" get secret argocd-initial-admin-secret \
      -o jsonpath='{.data.password}' | base64 -d
  )"

  argocd login localhost \
    --config "${ARGOCD_CONFIG}" \
    --name "${ARGOCD_CONTEXT}" \
    --kube-context "kind-${HUB_CLUSTER}" \
    --port-forward \
    --port-forward-namespace "${ARGOCD_NS}" \
    --username admin \
    --password "${admin_password}" \
    --insecure \
    --skip-test-tls \
    --grpc-web
}

argocd_cluster_list_json() {
  argocd cluster list \
    --config "${ARGOCD_CONFIG}" \
    --argocd-context "${ARGOCD_CONTEXT}" \
    --kube-context "kind-${HUB_CLUSTER}" \
    --port-forward \
    --port-forward-namespace "${ARGOCD_NS}" \
    -o json
}

verify_argocd_server_cluster_access() {
  log "verifying Argo CD server can access ClusterProfile cluster"
  login_argocd_server

  if ! retry_until 120 "Argo CD server cluster connection to ${CP_NAME}" \
    argocd_cluster_state_is Successful; then
    echo "Argo CD server did not report a successful connection to ${CP_NAME}" >&2
    argocd_cluster_list_json | jq . || true
    return 1
  fi

  _app_logs_available() {
    argocd app logs "${APP_NAME}" \
      --config "${ARGOCD_CONFIG}" \
      --argocd-context "${ARGOCD_CONTEXT}" \
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
require_command go
require_command helm
require_command jq
require_command kind
require_command kubectl
require_command realpath

case "${E2E_INSTALL_METHOD}" in
  kustomize|helm) ;;
  *)
    echo "unsupported E2E_INSTALL_METHOD: ${E2E_INSTALL_METHOD} (expected kustomize or helm)" >&2
    exit 1
    ;;
esac

if [[ "${E2E_IMG}" == *@* ]] ||
  [ "${E2E_IMAGE_REPOSITORY}" = "${E2E_IMG}" ] ||
  [[ "${E2E_IMAGE_TAG}" == */* ]]; then
  echo "E2E_IMG must be an explicitly tagged image reference: ${E2E_IMG}" >&2
  exit 1
fi
E2E_LOCAL_IMAGE_ID="$(docker image inspect "${E2E_IMG}" --format '{{.Id}}')"

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
create_kind_cluster "${HUB_CLUSTER}" --config=- <<'EOF'
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
  - role: worker
  - role: worker
EOF
if ! kubectl --context "kind-${HUB_CLUSTER}" explain pod.spec.volumes.image >/dev/null 2>&1; then
  echo "kind cluster ${HUB_CLUSTER} does not support Kubernetes ImageVolume" >&2
  exit 1
fi
kubectl --context "kind-${HUB_CLUSTER}" label node -l '!node-role.kubernetes.io/control-plane' \
  e2e.argoproj.io/ha-worker=true >/dev/null
SPOKE_CREATED=1
create_kind_cluster "${SPOKE_CLUSTER}"

log "loading controller image ${E2E_IMG}"
kind load docker-image "${E2E_IMG}" --name "${HUB_CLUSTER}"
E2E_NODE_IMAGE_STATUS="$(
  docker exec "${HUB_CLUSTER}-control-plane" \
    crictl inspecti --output json "${E2E_IMG}"
)"
E2E_NODE_IMAGE_ID="$(jq -r '.status.id // ""' <<<"${E2E_NODE_IMAGE_STATUS}")"
# Kubelet reports a Pod imageID as the image ID or any digest reference of the
# loaded image, depending on the container runtime, so identity checks accept
# every reference the node holds for the image.
E2E_NODE_IMAGE_IDS="$(
  jq -c '[.status.id // empty] + (.status.repoDigests // [])' <<<"${E2E_NODE_IMAGE_STATUS}"
)"
if [ -z "${E2E_NODE_IMAGE_ID}" ] || [ "${E2E_NODE_IMAGE_ID}" != "${E2E_LOCAL_IMAGE_ID}" ]; then
  echo "kind node image ID ${E2E_NODE_IMAGE_ID} does not match local image ID ${E2E_LOCAL_IMAGE_ID}" >&2
  exit 1
fi

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

log "creating mirror namespace ${MIRROR_NS}"
kubectl --context "kind-${HUB_CLUSTER}" create namespace "${MIRROR_NS}"
kubectl --context "kind-${HUB_CLUSTER}" create namespace "${RBAC_UNWATCHED_NS}"
kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" \
  patch configmap argocd-cmd-params-cm --type=merge \
  -p "{\"data\":{\"clusterprofilecontroller.k8s.client.qps\":\"5\",\"clusterprofilecontroller.k8s.client.burst\":\"1\"}}" \
  >/dev/null

log "installing ClusterProfile controller via ${E2E_INSTALL_METHOD}"
if [ "${E2E_INSTALL_METHOD}" = "kustomize" ]; then
  CONTROLLER_CONTAINER_NAME="argocd-clusterprofile-controller"
  kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" patch configmap argocd-cmd-params-cm --type merge \
    -p "{\"data\":{\"clusterprofilecontroller.enable.leader.election\":\"true\",\"clusterprofilecontroller.namespaces\":\"${ARGOCD_NS},${MIRROR_NS}\"}}"
  KUSTOMIZE_E2E_OVERLAY="${WORK_DIR}/kustomize-e2e"
  mkdir -p "${KUSTOMIZE_E2E_OVERLAY}"
  KUSTOMIZE_MANIFESTS_PATH="$(
    realpath --relative-to="${KUSTOMIZE_E2E_OVERLAY}" "${REPO_ROOT}/artifacts/manifests"
  )"
  KUSTOMIZE_CONTROL_PLANE_IP="$(
    kubectl --context "kind-${HUB_CLUSTER}" get nodes \
      -l node-role.kubernetes.io/control-plane \
      -o jsonpath='{.items[0].status.addresses[?(@.type=="InternalIP")].address}'
  )"
  KUSTOMIZE_CONTROL_PLANE_POD_CIDR="$(
    kubectl --context "kind-${HUB_CLUSTER}" get nodes \
      -l node-role.kubernetes.io/control-plane \
      -o jsonpath='{.items[0].spec.podCIDR}'
  )"
  cat >"${KUSTOMIZE_E2E_OVERLAY}/poddisruptionbudget.yaml" <<EOF
apiVersion: policy/v1
kind: PodDisruptionBudget
metadata:
  name: argocd-clusterprofile-controller
  labels:
    app.kubernetes.io/name: argocd-clusterprofile-controller
    app.kubernetes.io/part-of: argocd
    app.kubernetes.io/component: clusterprofile-controller
spec:
  maxUnavailable: 1
  selector:
    matchLabels:
      app.kubernetes.io/name: argocd-clusterprofile-controller
EOF
  cat >"${KUSTOMIZE_E2E_OVERLAY}/kustomization.yaml" <<EOF
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
namespace: "${ARGOCD_NS}"
resources:
  - ${KUSTOMIZE_MANIFESTS_PATH}
  - poddisruptionbudget.yaml
patches:
  - target:
      kind: Deployment
      name: argocd-clusterprofile-controller
    patch: |-
      apiVersion: apps/v1
      kind: Deployment
      metadata:
        name: argocd-clusterprofile-controller
      spec:
        replicas: 2
        template:
          spec:
            nodeSelector:
              kubernetes.io/os: linux
              e2e.argoproj.io/ha-worker: "true"
            topologySpreadConstraints:
              - maxSkew: 1
                topologyKey: kubernetes.io/hostname
                whenUnsatisfiable: DoNotSchedule
                matchLabelKeys:
                  - pod-template-hash
                labelSelector:
                  matchLabels:
                    app.kubernetes.io/name: argocd-clusterprofile-controller
            containers:
              - name: argocd-clusterprofile-controller
                imagePullPolicy: IfNotPresent
  - target:
      kind: NetworkPolicy
      name: argocd-clusterprofile-controller-network-policy
    patch: |-
      apiVersion: networking.k8s.io/v1
      kind: NetworkPolicy
      metadata:
        name: argocd-clusterprofile-controller-network-policy
      spec:
        ingress:
          - from:
              - namespaceSelector: {}
              - ipBlock:
                  cidr: ${KUSTOMIZE_CONTROL_PLANE_IP}/32
              - ipBlock:
                  cidr: ${KUSTOMIZE_CONTROL_PLANE_POD_CIDR}
            ports:
              - protocol: TCP
                port: 8080
          - from:
              - ipBlock:
                  cidr: ${KUSTOMIZE_CONTROL_PLANE_IP}/32
              - ipBlock:
                  cidr: ${KUSTOMIZE_CONTROL_PLANE_POD_CIDR}
            ports:
              - protocol: TCP
                port: 8081
images:
  - name: ghcr.io/argoproj-labs/clusterprofile-integration-for-argocd
    newName: "${E2E_IMAGE_REPOSITORY}"
    newTag: "${E2E_IMAGE_TAG}"
EOF
  kubectl kustomize "${KUSTOMIZE_E2E_OVERLAY}" \
    --load-restrictor=LoadRestrictionsNone | \
    kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" apply -f -
else
  CONTROLLER_CONTAINER_NAME="clusterprofile-controller"
  CLUSTERPROFILE_CRD_MANIFEST="$(grep -o 'https://[^[:space:]]*clusterprofiles\.yaml' artifacts/manifests/base/kustomization.yaml)"
  kubectl --context "kind-${HUB_CLUSTER}" apply -f "${CLUSTERPROFILE_CRD_MANIFEST}"
  helm --kube-context "kind-${HUB_CLUSTER}" upgrade --install cpia charts/argocd-clusterprofile-controller \
    --namespace "${ARGOCD_NS}" \
    --set fullnameOverride=argocd \
    --set-string "image.repository=${E2E_IMAGE_REPOSITORY}" \
    --set-string "image.tag=${E2E_IMAGE_TAG}" \
    --set replicaCount=2 \
    --set-string 'nodeSelector.e2e\.argoproj\.io/ha-worker=true' \
    --set-json 'topologySpreadConstraints=[{"maxSkew":1,"topologyKey":"kubernetes.io/hostname","whenUnsatisfiable":"DoNotSchedule","matchLabelKeys":["pod-template-hash"],"labelSelector":{"matchLabels":{"app.kubernetes.io/name":"argocd-clusterprofile-controller"}}}]' \
    --set "controller.clusterProfileProvidersFile=/app/cp-creds/cp-creds.json" \
    --set-json "controller.clusterProfileNamespaces=[\"${ARGOCD_NS}\",\"${MIRROR_NS}\"]" \
    --set-json 'controller.extraVolumes=[{"name":"cp-creds-vol","secret":{"secretName":"cp-creds-secret"}}]' \
    --set-json 'controller.extraVolumeMounts=[{"name":"cp-creds-vol","mountPath":"/app/cp-creds"}]' \
    --wait \
    --timeout 5m
fi

for resource in $(kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" get deploy,sts -o name); do
  kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" rollout status "${resource}" --timeout=300s
done

CONTROLLER_DEPLOYMENT_JSON="$(
  kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" \
    get deployment/argocd-clusterprofile-controller -o json
)"
CONTROLLER_CONTAINER_SPEC="$(
  jq -c --arg container "${CONTROLLER_CONTAINER_NAME}" \
    'first(.spec.template.spec.containers[] | select(.name == $container)) // {}' \
    <<<"${CONTROLLER_DEPLOYMENT_JSON}"
)"
CONTROLLER_DEPLOYED_IMAGE="$(jq -r '.image // ""' <<<"${CONTROLLER_CONTAINER_SPEC}")"
if [ "${CONTROLLER_DEPLOYED_IMAGE}" != "${E2E_IMG}" ]; then
  echo "controller Deployment uses ${CONTROLLER_DEPLOYED_IMAGE}, expected ${E2E_IMG}" >&2
  exit 1
fi
CONTROLLER_IMAGE_PULL_POLICY="$(jq -r '.imagePullPolicy // ""' <<<"${CONTROLLER_CONTAINER_SPEC}")"
if [ "${CONTROLLER_IMAGE_PULL_POLICY}" != "IfNotPresent" ]; then
  echo "controller Deployment uses imagePullPolicy ${CONTROLLER_IMAGE_PULL_POLICY}, expected IfNotPresent" >&2
  exit 1
fi
CONTROLLER_PROBES="$(
  jq -c '{
    ports: .ports,
    livenessProbe: .livenessProbe,
    readinessProbe: .readinessProbe
  }' <<<"${CONTROLLER_CONTAINER_SPEC}"
)"
if ! jq -e '
  ([.ports[]? | select(.name == "healthz" and .containerPort == 8081)] | length) == 1 and
  .livenessProbe.httpGet.path == "/healthz" and
  .livenessProbe.httpGet.port == "healthz" and
  .readinessProbe.httpGet.path == "/readyz" and
  .readinessProbe.httpGet.port == "healthz"' <<<"${CONTROLLER_PROBES}" >/dev/null; then
  echo "controller Deployment does not expose the expected health probes: ${CONTROLLER_PROBES}" >&2
  exit 1
fi
CONTROLLER_TERMINATION_GRACE="$(
  jq -r '.spec.template.spec.terminationGracePeriodSeconds' <<<"${CONTROLLER_DEPLOYMENT_JSON}"
)"
if [ "${CONTROLLER_TERMINATION_GRACE}" != "30" ]; then
  echo "controller Deployment has an unsafe termination grace period: ${CONTROLLER_TERMINATION_GRACE}" >&2
  exit 1
fi
CONTROLLER_RESOURCES="$(jq -c '.resources // {}' <<<"${CONTROLLER_CONTAINER_SPEC}")"
if ! jq -e '
  .requests.cpu == "10m" and
  .requests.memory == "128Mi" and
  .limits.memory == "256Mi" and
  (.limits | has("cpu") | not)' <<<"${CONTROLLER_RESOURCES}" >/dev/null; then
  echo "controller Deployment does not set the expected resource budget: ${CONTROLLER_RESOURCES}" >&2
  exit 1
fi
CONTROLLER_RESOURCE_STATE="$(
  kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" \
    get pods -l app.kubernetes.io/name=argocd-clusterprofile-controller -o json | jq -c \
      --arg container "${CONTROLLER_CONTAINER_NAME}" \
      '[.items[] |
        select(.metadata.deletionTimestamp == null) as $pod |
        ($pod.spec.containers[]? | select(.name == $container)) as $spec |
        $pod.status.containerStatuses[]? |
        select(.name == $container) |
        {
          pod: $pod.metadata.name,
          qosClass: $pod.status.qosClass,
          resources: $spec.resources,
          ready: .ready,
          running: (.state.running != null),
          restartCount: .restartCount
        }]'
)"
if ! jq -e '
  length == 2 and
  all(.[];
    .qosClass == "Burstable" and
    .resources.requests.cpu == "10m" and
    .resources.requests.memory == "128Mi" and
    .resources.limits.memory == "256Mi" and
    (.resources.limits | has("cpu") | not) and
    .ready == true and
    .running == true and
    .restartCount == 0)' <<<"${CONTROLLER_RESOURCE_STATE}" >/dev/null; then
  echo "controller Pods did not start within the expected resource budget: ${CONTROLLER_RESOURCE_STATE}" >&2
  exit 1
fi
if ! retry_until 60 "controller replicas to spread across workers" controllers_are_spread; then
  echo "controller replicas did not spread across separate worker nodes" >&2
  exit 1
fi

log "verifying readiness stays false until every watched informer cache has synced"
if [ "${E2E_INSTALL_METHOD}" = "helm" ]; then
  READINESS_RBAC_KIND="role"
  READINESS_RBAC_NAMESPACE="${MIRROR_NS}"
else
  READINESS_RBAC_KIND="clusterrole"
  READINESS_RBAC_NAMESPACE=""
fi
READINESS_RBAC_NAME="argocd-clusterprofile-controller"
READINESS_RBAC_ARGS=(--context "kind-${HUB_CLUSTER}")
if [ -n "${READINESS_RBAC_NAMESPACE}" ]; then
  READINESS_RBAC_ARGS+=(-n "${READINESS_RBAC_NAMESPACE}")
fi
READINESS_ORIGINAL_RULES="$(
  kubectl "${READINESS_RBAC_ARGS[@]}" get \
    "${READINESS_RBAC_KIND}/${READINESS_RBAC_NAME}" -o json | jq -c '.rules'
)"
if ! jq -e '
  any(.[].apiGroups[]?; . == "multicluster.x-k8s.io") and
  any(.[];
    any(.resources[]?; . == "clusterprofiles") and
    any(.verbs[]?; . == "list") and
    any(.verbs[]?; . == "watch"))' <<<"${READINESS_ORIGINAL_RULES}" >/dev/null; then
  echo "readiness fixture RBAC does not grant ClusterProfile list and watch: ${READINESS_ORIGINAL_RULES}" >&2
  exit 1
fi
READINESS_DENIED_RULES="$(jq -c '
  map(if
    any(.apiGroups[]?; . == "multicluster.x-k8s.io") and
    any(.resources[]?; . == "clusterprofiles")
  then
    .verbs -= ["list", "watch"]
  else
    .
  end)' <<<"${READINESS_ORIGINAL_RULES}")"

kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" \
  scale deploy/argocd-clusterprofile-controller --replicas=0
if ! retry_until 120 "ClusterProfile controller to stop for readiness fixture" controller_is_stopped; then
  echo "ClusterProfile controller did not stop for readiness fixture" >&2
  exit 1
fi
kubectl "${READINESS_RBAC_ARGS[@]}" patch \
  "${READINESS_RBAC_KIND}/${READINESS_RBAC_NAME}" --type=merge \
  -p "$(jq -cn --argjson rules "${READINESS_DENIED_RULES}" '{rules: $rules}')" >/dev/null
READINESS_CONTROLLER_SUBJECT="system:serviceaccount:${ARGOCD_NS}:argocd-clusterprofile-controller"
_readiness_clusterprofile_access_is() {
  local expected="$1" verb
  for verb in list watch; do
    if [ "$(
      kubectl --context "kind-${HUB_CLUSTER}" auth can-i \
        --as "${READINESS_CONTROLLER_SUBJECT}" "${verb}" \
        clusterprofiles.multicluster.x-k8s.io --namespace "${MIRROR_NS}" 2>/dev/null || true
    )" != "${expected}" ]; then
      return 1
    fi
  done
}
if ! retry_until 60 "readiness fixture RBAC denial" _readiness_clusterprofile_access_is no; then
  echo "ClusterProfile list or watch permission remained allowed after readiness RBAC fixture patch" >&2
  exit 1
fi
kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" \
  scale deploy/argocd-clusterprofile-controller --replicas=2

READINESS_BLOCKED_IDENTITY="$(wait_for_controller_pods 2 '.running')"
if ! jq -e \
  --arg image "${E2E_IMG}" \
  --argjson image_ids "${E2E_NODE_IMAGE_IDS}" \
  'length == 2 and
   all(.[];
     .image == $image and
     (.imageID | IN($image_ids[])) and
     (.containerID | length) > 0 and
     .ready == false and
     .restartCount == 0)' <<<"${READINESS_BLOCKED_IDENTITY}" >/dev/null; then
  echo "cache-blocked controllers did not remain running and unready: ${READINESS_BLOCKED_IDENTITY}" >&2
  exit 1
fi
_blocked_controller_liveness_is_healthy() {
  local pod
  for pod in $(jq -r '.[].pod' <<<"${READINESS_BLOCKED_IDENTITY}"); do
    if [ "$(
      kubectl --context "kind-${HUB_CLUSTER}" --request-timeout=5s get --raw \
        "/api/v1/namespaces/${ARGOCD_NS}/pods/${pod}:8081/proxy/healthz" 2>/dev/null
    )" != "ok" ]; then
      return 1
    fi
  done
}
if ! retry_until 60 "cache-blocked controller liveness" _blocked_controller_liveness_is_healthy; then
  echo "cache-blocked controller liveness did not become healthy" >&2
  exit 1
fi
for READINESS_BLOCKED_POD in $(jq -r '.[].pod' <<<"${READINESS_BLOCKED_IDENTITY}"); do
  CONTROLLER_POD_PROXY="/api/v1/namespaces/${ARGOCD_NS}/pods/${READINESS_BLOCKED_POD}:8081/proxy"
  if kubectl --context "kind-${HUB_CLUSTER}" --request-timeout=5s get --raw \
    "${CONTROLLER_POD_PROXY}/readyz?verbose" \
    >"${WORK_DIR}/blocked-readyz-${READINESS_BLOCKED_POD}.log" 2>&1; then
    echo "controller ${READINESS_BLOCKED_POD} reported ready while a watched ClusterProfile cache could not list or watch" >&2
    exit 1
  fi
done
_readiness_container_status_is() {
  local expected_ready="$1" message="$2" status
  status="$(controller_pods_state)"
  if ! jq -e \
    --argjson blocked "${READINESS_BLOCKED_IDENTITY}" \
    --argjson ready "${expected_ready}" '
    length == 2 and
    all(.[];
      . as $current |
      ($blocked[] | select(.pod == $current.pod)) as $original |
      $current.containerID == $original.containerID and
      $current.running == true and
      $current.ready == $ready and
      $current.restartCount == 0)' <<<"${status}" >/dev/null; then
    echo "${message}: ${status}" >&2
    exit 1
  fi
}
sleep 10
_readiness_container_status_is false "cache-blocked controller was not stably alive and unready"
_blocked_controller_liveness_is_healthy

kubectl "${READINESS_RBAC_ARGS[@]}" patch \
  "${READINESS_RBAC_KIND}/${READINESS_RBAC_NAME}" --type=merge \
  -p "$(jq -cn --argjson rules "${READINESS_ORIGINAL_RULES}" '{rules: $rules}')" >/dev/null
if ! retry_until 60 "readiness fixture RBAC restoration" \
  _readiness_clusterprofile_access_is yes; then
  echo "ClusterProfile list or watch permission did not recover after readiness RBAC fixture" >&2
  exit 1
fi
kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" \
  rollout status deploy/argocd-clusterprofile-controller --timeout=300s
_readiness_container_status_is true "controller did not become ready in place after cache access recovered"
for READINESS_BLOCKED_POD in $(jq -r '.[].pod' <<<"${READINESS_BLOCKED_IDENTITY}"); do
  if [ "$(
    kubectl --context "kind-${HUB_CLUSTER}" --request-timeout=5s get --raw \
      "/api/v1/namespaces/${ARGOCD_NS}/pods/${READINESS_BLOCKED_POD}:8081/proxy/readyz"
  )" != "ok" ]; then
    echo "controller ${READINESS_BLOCKED_POD} readiness endpoint did not recover after informer sync" >&2
    exit 1
  fi
done
log "verified liveness/readiness separation and in-place cache-sync recovery"

CONTROLLER_IDENTITY="$(wait_for_controller_pods 2 '.ready and .running')"
if ! jq -e \
  --arg image "${E2E_IMG}" \
  --argjson image_ids "${E2E_NODE_IMAGE_IDS}" \
  'length == 2 and
   all(.[];
     .image == $image and
     (.imageID | IN($image_ids[])) and
     .serviceAccount == "argocd-clusterprofile-controller" and
     (.containerID | length) > 0 and
     .restartCount == 0)' <<<"${CONTROLLER_IDENTITY}" >/dev/null; then
  echo "controller Pod identities did not converge to the loaded image: ${CONTROLLER_IDENTITY}" >&2
  exit 1
fi
CONTROLLER_SERVICE_ACCOUNT="$(jq -r '.[0].serviceAccount' <<<"${CONTROLLER_IDENTITY}")"
CONTROLLER_SUBJECT="system:serviceaccount:${ARGOCD_NS}:${CONTROLLER_SERVICE_ACCOUNT}"
log "verified controller image identities ${CONTROLLER_IDENTITY}"

log "verifying leader election and least-privilege RBAC"
controller_lease_state() {
  kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" \
    get lease clusterprofile.argoproj.io -o json | jq -c '{
      holderIdentity: (.spec.holderIdentity // ""),
      leaseDurationSeconds: .spec.leaseDurationSeconds,
      acquireTime: .spec.acquireTime,
      renewTime: .spec.renewTime
    }'
}
LEASE_STATE=""
for _ in $(seq 1 60); do
  LEASE_STATE="$(controller_lease_state)"
  if jq -e '
    (.holderIdentity | type == "string" and length > 0) and
    (.acquireTime | type == "string" and length > 0) and
    (.renewTime | type == "string" and length > 0)' <<<"${LEASE_STATE}" >/dev/null; then
    break
  fi
  sleep 1
done
if ! jq -e '
  (.holderIdentity | type == "string" and length > 0) and
  (.acquireTime | type == "string" and length > 0) and
  (.renewTime | type == "string" and length > 0)' <<<"${LEASE_STATE}" >/dev/null; then
  echo "leader-election Lease is incomplete: ${LEASE_STATE}" >&2
  exit 1
fi
INITIAL_LEASE_HOLDER="$(jq -r '.holderIdentity' <<<"${LEASE_STATE}")"
CONTROLLER_POD="${INITIAL_LEASE_HOLDER%%_*}"
if ! jq -e --arg pod "${CONTROLLER_POD}" \
  'any(.[]; .pod == $pod and .ready and .running)' <<<"${CONTROLLER_IDENTITY}" >/dev/null; then
  echo "leader-election holder did not converge to the ready controller Pod: ${LEASE_STATE}" >&2
  exit 1
fi
INITIAL_LEASE_RENEW_TIME="$(jq -r '.renewTime' <<<"${LEASE_STATE}")"
for _ in $(seq 1 30); do
  LEASE_STATE="$(controller_lease_state)"
  if [ "$(jq -r '.renewTime' <<<"${LEASE_STATE}")" != "${INITIAL_LEASE_RENEW_TIME}" ]; then
    break
  fi
  sleep 1
done
if [ "$(jq -r '.holderIdentity' <<<"${LEASE_STATE}")" != "${INITIAL_LEASE_HOLDER}" ] ||
  [ "$(jq -r '.renewTime' <<<"${LEASE_STATE}")" = "${INITIAL_LEASE_RENEW_TIME}" ]; then
  echo "leader-election Lease did not renew under the ready controller Pod: ${LEASE_STATE}" >&2
  exit 1
fi

LEADER_EVENT=""
for _ in $(seq 1 30); do
  LEADER_EVENT="$(
    kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" get events \
      --field-selector involvedObject.kind=Lease,involvedObject.name=clusterprofile.argoproj.io \
      -o json | jq -c --arg holder "${INITIAL_LEASE_HOLDER}" \
        '(first(.items[] | select(.reason == "LeaderElection" and (.message | contains($holder + " became leader")))) // empty) |
         {reason: .reason, message: .message, count: .count}'
  )"
  [ -n "${LEADER_EVENT}" ] && break
  sleep 1
done
if [ -z "${LEADER_EVENT}" ]; then
  echo "controller-runtime did not record the leader-election Event" >&2
  exit 1
fi

for namespace in "${ARGOCD_NS}" "${MIRROR_NS}"; do
  for verb in get list watch; do
    assert_can_i "${verb}" clusterprofiles.multicluster.x-k8s.io --namespace "${namespace}"
  done
  for verb in get list watch create update patch delete; do
    assert_can_i "${verb}" secrets --namespace "${namespace}"
  done
  for verb in create update patch delete; do
    assert_cannot_i "${verb}" clusterprofiles.multicluster.x-k8s.io --namespace "${namespace}"
  done
  assert_can_i create events.events.k8s.io --namespace "${namespace}"
  assert_can_i patch events.events.k8s.io --namespace "${namespace}"
done
assert_can_i create events --namespace "${ARGOCD_NS}"
assert_can_i patch events --namespace "${ARGOCD_NS}"
assert_can_i create leases.coordination.k8s.io --namespace "${ARGOCD_NS}"
assert_can_i get leases.coordination.k8s.io/clusterprofile.argoproj.io --namespace "${ARGOCD_NS}"
assert_can_i update leases.coordination.k8s.io/clusterprofile.argoproj.io --namespace "${ARGOCD_NS}"
for resource in applications.argoproj.io appprojects.argoproj.io configmaps; do
  for verb in get list watch create update patch delete; do
    assert_cannot_i "${verb}" "${resource}" --namespace "${ARGOCD_NS}"
  done
done
for verb in get list watch update delete; do
  assert_cannot_i "${verb}" events --namespace "${ARGOCD_NS}"
  assert_cannot_i "${verb}" events.events.k8s.io --namespace "${ARGOCD_NS}"
done
for verb in list watch patch delete; do
  assert_cannot_i "${verb}" leases.coordination.k8s.io --namespace "${ARGOCD_NS}"
done
for verb in get update; do
  assert_cannot_i "${verb}" leases.coordination.k8s.io/not-the-controller-lock --namespace "${ARGOCD_NS}"
done
if [ "${E2E_INSTALL_METHOD}" = "helm" ]; then
  for resource in clusterprofiles.multicluster.x-k8s.io secrets events.events.k8s.io; do
    for verb in get list watch create update patch delete; do
      assert_cannot_i "${verb}" "${resource}" --namespace "${RBAC_UNWATCHED_NS}"
    done
  done
fi
log "verified renewing leader election ${LEASE_STATE}, Event ${LEADER_EVENT}, and controller RBAC boundary"

log "verifying graceful worker drain and voluntary leader-election release"
SHUTDOWN_LOG="${WORK_DIR}/controller-shutdown.log"
kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" \
  patch "pod/${CONTROLLER_POD}" --type=merge \
  -p '{"metadata":{"finalizers":["e2e.argoproj.io/observe-termination"]}}' >/dev/null
kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" \
  logs --follow --since=5s "pod/${CONTROLLER_POD}" -c "${CONTROLLER_CONTAINER_NAME}" \
  >"${SHUTDOWN_LOG}" 2>&1 &
SHUTDOWN_LOG_PID=$!
sleep 1
if ! kill -0 "${SHUTDOWN_LOG_PID}" >/dev/null 2>&1; then
  echo "unable to follow the controller logs before shutdown" >&2
  cat "${SHUTDOWN_LOG}" >&2 || true
  exit 1
fi
SHUTDOWN_STARTED_NS="$(date +%s%N)"
kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" \
  scale deploy/argocd-clusterprofile-controller --replicas=0
if ! retry_until 30 "controller container to exit cleanly" shutdown_controller_container_is_completed; then
  echo "controller container did not exit cleanly within its termination grace period" >&2
  kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" \
    get "pod/${CONTROLLER_POD}" -o yaml >&2 || true
  exit 1
fi
SHUTDOWN_ELAPSED_MS="$((($(date +%s%N) - SHUTDOWN_STARTED_NS) / 1000000))"
if [ "${SHUTDOWN_ELAPSED_MS}" -ge 30000 ]; then
  echo "controller shutdown exhausted the Pod grace period: ${SHUTDOWN_ELAPSED_MS}ms" >&2
  exit 1
fi
sleep 1
stop_shutdown_log_capture

SHUTDOWN_PREVIOUS_LINE=0
for message in \
  "Stopping and waiting for non leader election runnables" \
  "Stopping and waiting for leader election runnables" \
  "Shutdown signal received, waiting for all workers to finish" \
  "All workers finished" \
  "Stopping and waiting for caches" \
  "Stopping and waiting for HTTP servers" \
  "Wait completed, proceeding to shutdown the manager"; do
  SHUTDOWN_MATCH="$(grep -nF -m1 "${message}" "${SHUTDOWN_LOG}" || true)"
  SHUTDOWN_LINE="${SHUTDOWN_MATCH%%:*}"
  if [ -z "${SHUTDOWN_LINE}" ] || [ "${SHUTDOWN_LINE}" -le "${SHUTDOWN_PREVIOUS_LINE}" ]; then
    echo "controller shutdown log is incomplete or out of order at: ${message}" >&2
    cat "${SHUTDOWN_LOG}" >&2
    exit 1
  fi
  SHUTDOWN_PREVIOUS_LINE="${SHUTDOWN_LINE}"
done
if grep -E -i \
  'failed waiting for all runnables|leader election lost|failed to release lease|error received after stop sequence|problem running manager|signal: killed' \
  "${SHUTDOWN_LOG}" >/dev/null; then
  echo "controller reported an unsafe shutdown" >&2
  cat "${SHUTDOWN_LOG}" >&2
  exit 1
fi

SHUTDOWN_LEASE_STATE=""
for _ in $(seq 1 30); do
  SHUTDOWN_LEASE_STATE="$(controller_lease_state)"
  if jq -e '
    .holderIdentity == "" and
    .leaseDurationSeconds == 1 and
    (.acquireTime | type == "string" and length > 0) and
    (.renewTime | type == "string" and length > 0)' \
    <<<"${SHUTDOWN_LEASE_STATE}" >/dev/null; then
    break
  fi
  sleep 1
done
if ! jq -e '.holderIdentity == "" and .leaseDurationSeconds == 1' \
  <<<"${SHUTDOWN_LEASE_STATE}" >/dev/null; then
  echo "controller did not release its Lease after graceful shutdown: ${SHUTDOWN_LEASE_STATE}" >&2
  exit 1
fi

kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" \
  patch "pod/${CONTROLLER_POD}" --type=merge -p '{"metadata":{"finalizers":null}}' >/dev/null
if ! retry_until 30 "completed controller Pod deletion" shutdown_controller_pod_is_gone; then
  echo "completed controller Pod remained after removing the E2E observation finalizer" >&2
  exit 1
fi
if ! retry_until 120 "in-cluster controller to stop" controller_is_stopped; then
  echo "in-cluster controller did not stop before the explicit kubeconfig fixture" >&2
  exit 1
fi
log "verified ${SHUTDOWN_ELAPSED_MS}ms graceful shutdown, clean exit, ordered drain, and Lease release"

log "building and starting an out-of-cluster controller against the explicit hub context"
go build -o "${OUT_OF_CLUSTER_CONTROLLER_BIN}" .
kubectl config use-context "kind-${SPOKE_CLUSTER}" >/dev/null
test "$(kubectl config current-context)" = "kind-${SPOKE_CLUSTER}"
env -u KUBECONFIG "${OUT_OF_CLUSTER_CONTROLLER_BIN}" \
  --kubeconfig "${KUBECONFIG}" \
  --context "kind-${HUB_CLUSTER}" \
  --namespace "${ARGOCD_NS}" \
  --metrics-addr=:0 \
  --probe-addr=:0 \
  >"${OUT_OF_CLUSTER_CONTROLLER_LOG}" 2>&1 &
OUT_OF_CLUSTER_CONTROLLER_PID=$!
sleep 1
if ! out_of_cluster_controller_is_running; then
  echo "out-of-cluster controller exited during startup" >&2
  tail -200 "${OUT_OF_CLUSTER_CONTROLLER_LOG}" >&2 || true
  exit 1
fi

log "creating a hub-only ClusterProfile for the explicit kubeconfig fixture"
kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" apply -f - <<EOF
apiVersion: multicluster.x-k8s.io/v1alpha1
kind: ClusterProfile
metadata:
  name: ${OUT_OF_CLUSTER_CP_NAME}
  namespace: ${ARGOCD_NS}
spec:
  clusterManager:
    name: manual
EOF
set_builtin_profile_access "${OUT_OF_CLUSTER_CP_NAME}" "${OUT_OF_CLUSTER_SERVER}"
OUT_OF_CLUSTER_CP_UID="$(clusterprofile_uid "${ARGOCD_NS}" "${OUT_OF_CLUSTER_CP_NAME}")"
if ! retry_until 120 "hub Secret from the explicit kubeconfig context" \
  long_name_secret_is_ready \
    "${OUT_OF_CLUSTER_CP_NAME}" \
    "${OUT_OF_CLUSTER_CP_UID}" \
    "${OUT_OF_CLUSTER_SERVER}"; then
  echo "out-of-cluster controller did not reconcile the explicit hub context" >&2
  exit 1
fi
if ! out_of_cluster_controller_is_running; then
  echo "out-of-cluster controller exited after reconciling the hub fixture" >&2
  exit 1
fi
if kubectl --context "kind-${SPOKE_CLUSTER}" get \
  crd clusterprofiles.multicluster.x-k8s.io >/dev/null 2>&1; then
  echo "ClusterProfile CRD unexpectedly appeared on the kubeconfig current-context spoke" >&2
  exit 1
fi
if kubectl --context "kind-${SPOKE_CLUSTER}" -n "${ARGOCD_NS}" \
  get secret "${OUT_OF_CLUSTER_SECRET_NAME}" >/dev/null 2>&1; then
  echo "hub-only Secret unexpectedly appeared on the kubeconfig current-context spoke" >&2
  exit 1
fi

log "stopping the out-of-cluster controller and cleaning its fixture"
stop_out_of_cluster_controller
kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" \
  delete clusterprofile "${OUT_OF_CLUSTER_CP_NAME}" --wait=true --timeout=60s
if ! retry_until 120 "explicit kubeconfig fixture Secret garbage collection" \
  secret_is_gone "${ARGOCD_NS}" "${OUT_OF_CLUSTER_SECRET_NAME}"; then
  echo "explicit kubeconfig fixture Secret was not garbage collected" >&2
  exit 1
fi

log "restarting the in-cluster controller after the explicit kubeconfig fixture"
kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" \
  scale deploy/argocd-clusterprofile-controller --replicas=2
kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" \
  rollout status deploy/argocd-clusterprofile-controller --timeout=300s
if ! retry_until 60 "restored controller spread" controllers_are_spread; then
  echo "controller replicas did not restore their worker spread" >&2
  exit 1
fi

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
            - "--enable-leader-election=true"
            - "--clusterprofile-provider-file=/app/cp-creds/cp-creds.json"'
fi

kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" rollout status sts/argocd-application-controller --timeout=300s
kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" rollout status deploy/argocd-server --timeout=300s
kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" rollout status deploy/argocd-clusterprofile-controller --timeout=300s

log "stopping ClusterProfile controller to create a manual Secret collision fixture"
kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" \
  scale deploy/argocd-clusterprofile-controller --replicas=0
if ! retry_until 120 "ClusterProfile controller to stop" controller_is_stopped; then
  echo "ClusterProfile controller did not stop" >&2
  exit 1
fi

log "creating a manual Argo CD cluster Secret and same-named ClusterProfile"
kubectl --context "kind-${HUB_CLUSTER}" apply -f - <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: ${COLLISION_SECRET_NAME}
  namespace: ${ARGOCD_NS}
  labels:
    argocd.argoproj.io/secret-type: cluster
    e2e.argoproj.io/fixture: provenance-collision
type: Opaque
stringData:
  name: manual-${COLLISION_CP_NAME}
  server: ${COLLISION_SERVER}
  config: '{"tlsClientConfig":{"insecure":true}}'
---
apiVersion: multicluster.x-k8s.io/v1alpha1
kind: ClusterProfile
metadata:
  name: ${COLLISION_CP_NAME}
  namespace: ${ARGOCD_NS}
spec:
  clusterManager:
    name: manual
  displayName: Provenance Collision Fixture
EOF
set_builtin_profile_access "${COLLISION_CP_NAME}" "https://clusterprofile-collision.example.com:6443"
COLLISION_SECRET_BEFORE="$(collision_secret_snapshot)"

log "restarting ClusterProfile controller and verifying the provenance collision"
kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" \
  scale deploy/argocd-clusterprofile-controller --replicas=2
kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" \
  rollout status deploy/argocd-clusterprofile-controller --timeout=300s
if ! retry_until 120 "provenance conflict log" provenance_conflict_is_logged; then
  echo "ClusterProfile controller did not log the expected provenance conflict" >&2
  exit 1
fi
COLLISION_SECRET_AFTER="$(collision_secret_snapshot)"
if [ "${COLLISION_SECRET_AFTER}" != "${COLLISION_SECRET_BEFORE}" ]; then
  echo "manual Secret ${COLLISION_SECRET_NAME} changed during the provenance collision" >&2
  diff <(printf '%s\n' "${COLLISION_SECRET_BEFORE}" | jq .) \
    <(printf '%s\n' "${COLLISION_SECRET_AFTER}" | jq .) || true
  exit 1
fi

log "deleting the colliding ClusterProfile and verifying the manual Secret survives"
kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" \
  delete clusterprofile "${COLLISION_CP_NAME}" --wait=true --timeout=60s
COLLISION_SECRET_AFTER_DELETE="$(collision_secret_snapshot)"
if [ "${COLLISION_SECRET_AFTER_DELETE}" != "${COLLISION_SECRET_BEFORE}" ]; then
  echo "manual Secret ${COLLISION_SECRET_NAME} changed after ClusterProfile deletion" >&2
  exit 1
fi
kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" delete secret "${COLLISION_SECRET_NAME}"

log "creating valid ClusterProfiles across generated metadata length boundaries"
for profile_name in \
  "${LONG_LABEL_CP_NAME}" \
  "${LONG_SECRET_CP_NAME}" \
  "${MAX_LENGTH_CP_NAME}" \
  "${RAW_COLLISION_CP_NAME}"; do
  cat <<EOF
---
apiVersion: multicluster.x-k8s.io/v1alpha1
kind: ClusterProfile
metadata:
  name: ${profile_name}
  namespace: ${ARGOCD_NS}
spec:
  clusterManager:
    name: manual
EOF
done | kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" apply -f -
set_builtin_profile_access "${LONG_LABEL_CP_NAME}" "${LONG_LABEL_SERVER}"
set_builtin_profile_access "${LONG_SECRET_CP_NAME}" "${LONG_SECRET_SERVER}"
set_builtin_profile_access "${MAX_LENGTH_CP_NAME}" "${MAX_LENGTH_SERVER}"
set_builtin_profile_access "${RAW_COLLISION_CP_NAME}" "${RAW_COLLISION_SERVER}"

LONG_LABEL_CP_UID="$(clusterprofile_uid "${ARGOCD_NS}" "${LONG_LABEL_CP_NAME}")"
LONG_SECRET_CP_UID="$(clusterprofile_uid "${ARGOCD_NS}" "${LONG_SECRET_CP_NAME}")"
MAX_LENGTH_CP_UID="$(clusterprofile_uid "${ARGOCD_NS}" "${MAX_LENGTH_CP_NAME}")"
RAW_COLLISION_CP_UID="$(clusterprofile_uid "${ARGOCD_NS}" "${RAW_COLLISION_CP_NAME}")"

if ! retry_until 120 "name-label overflow boundary Secret" \
  long_name_secret_is_ready "${LONG_LABEL_CP_NAME}" "${LONG_LABEL_CP_UID}" "${LONG_LABEL_SERVER}"; then
  echo "controller did not create a valid Secret for the label-overflow boundary" >&2
  exit 1
fi
if ! retry_until 120 "246-character ClusterProfile Secret" \
  long_name_secret_is_ready "${LONG_SECRET_CP_NAME}" "${LONG_SECRET_CP_UID}" "${LONG_SECRET_SERVER}"; then
  echo "controller did not create a valid Secret for the Secret-name overflow boundary" >&2
  exit 1
fi
if ! retry_until 120 "maximum-length ClusterProfile Secret" \
  long_name_secret_is_ready "${MAX_LENGTH_CP_NAME}" "${MAX_LENGTH_CP_UID}" "${MAX_LENGTH_SERVER}"; then
  echo "controller did not create a valid Secret for the maximum ClusterProfile name" >&2
  exit 1
fi
if ! retry_until 120 "raw-name collision regression Secret" \
  long_name_secret_is_ready "${RAW_COLLISION_CP_NAME}" "${RAW_COLLISION_CP_UID}" "${RAW_COLLISION_SERVER}"; then
  echo "raw Secret name that collided with the previous bounded encoding did not coexist" >&2
  exit 1
fi

LONG_LABEL_SECRET_JSON="$(owned_secret_for_profile "${LONG_LABEL_CP_UID}")"
LONG_SECRET_JSON="$(owned_secret_for_profile "${LONG_SECRET_CP_UID}")"
MAX_LENGTH_SECRET_JSON="$(owned_secret_for_profile "${MAX_LENGTH_CP_UID}")"
RAW_COLLISION_SECRET_JSON="$(owned_secret_for_profile "${RAW_COLLISION_CP_UID}")"
LONG_LABEL_SECRET_NAME="$(jq -r '.metadata.name' <<<"${LONG_LABEL_SECRET_JSON}")"
LONG_SECRET_NAME="$(jq -r '.metadata.name' <<<"${LONG_SECRET_JSON}")"
MAX_LENGTH_SECRET_NAME="$(jq -r '.metadata.name' <<<"${MAX_LENGTH_SECRET_JSON}")"
RAW_COLLISION_SECRET_NAME="$(jq -r '.metadata.name' <<<"${RAW_COLLISION_SECRET_JSON}")"
LONG_SECRET_UID="$(jq -r '.metadata.uid' <<<"${LONG_SECRET_JSON}")"

# The generated names themselves are pinned by TestGeneratedClusterProfileMetadata.
# Only the API server can show that a bounded name and the raw name it could have
# collided with are admitted side by side.
test "${LONG_SECRET_NAME}" != "${RAW_COLLISION_SECRET_NAME}"

log "verifying long-name Secret drift is reconciled"
kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" \
  patch secret "${LONG_LABEL_SECRET_NAME}" --type=merge \
  -p '{"stringData":{"server":"https://drift.example.com:6443"}}' >/dev/null
if ! retry_until 120 "long-name Secret drift repair" \
  long_name_secret_is_ready "${LONG_LABEL_CP_NAME}" "${LONG_LABEL_CP_UID}" "${LONG_LABEL_SERVER}"; then
  echo "controller did not repair drift in the long-name Secret" >&2
  exit 1
fi

log "revoking and restoring the 246-character ClusterProfile access"
kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" \
  patch clusterprofile "${LONG_SECRET_CP_NAME}" --subresource=status --type=merge \
  -p '{"status":{"accessProviders":null,"credentialProviders":null}}' >/dev/null
if ! retry_until 120 "long-name Secret revocation" secret_is_gone "${ARGOCD_NS}" "${LONG_SECRET_NAME}"; then
  echo "controller did not revoke the long-name Secret" >&2
  exit 1
fi
set_builtin_profile_access "${LONG_SECRET_CP_NAME}" "${LONG_SECRET_SERVER}"
if ! retry_until 120 "long-name Secret restoration" \
  long_name_secret_is_ready "${LONG_SECRET_CP_NAME}" "${LONG_SECRET_CP_UID}" "${LONG_SECRET_SERVER}"; then
  echo "controller did not restore the long-name Secret" >&2
  exit 1
fi
RESTORED_LONG_SECRET_JSON="$(owned_secret_for_profile "${LONG_SECRET_CP_UID}")"
test "$(jq -r '.metadata.name' <<<"${RESTORED_LONG_SECRET_JSON}")" = "${LONG_SECRET_NAME}"
test "$(jq -r '.metadata.uid' <<<"${RESTORED_LONG_SECRET_JSON}")" != "${LONG_SECRET_UID}"

log "deleting long-name fixtures and verifying owner garbage collection"
kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" delete clusterprofile \
  "${LONG_LABEL_CP_NAME}" "${LONG_SECRET_CP_NAME}" "${MAX_LENGTH_CP_NAME}" "${RAW_COLLISION_CP_NAME}" \
  --wait=true --timeout=120s >/dev/null
for secret_name in \
  "${LONG_LABEL_SECRET_NAME}" \
  "${LONG_SECRET_NAME}" \
  "${MAX_LENGTH_SECRET_NAME}" \
  "${RAW_COLLISION_SECRET_NAME}"; do
  if ! retry_until 120 "long-name Secret garbage collection" secret_is_gone "${ARGOCD_NS}" "${secret_name}"; then
    echo "long-name Secret ${secret_name} was not garbage collected" >&2
    exit 1
  fi
done

log "verifying inventory-scoped duplicate member selection"
apply_inventory_member_profile "${DUPLICATE_OLDEST_CP_NAME}"
apply_inventory_member_profile "${DUPLICATE_MIRROR_CP_NAME}" "${MIRROR_NS}"
if ! retry_until 120 "inventory member Secret ${ARGOCD_NS}/${DUPLICATE_OLDEST_SECRET_NAME}" \
  secret_exists "${ARGOCD_NS}" "${DUPLICATE_OLDEST_SECRET_NAME}"; then
  echo "controller did not create inventory member Secret ${ARGOCD_NS}/${DUPLICATE_OLDEST_SECRET_NAME}" >&2
  exit 1
fi
if ! retry_until 120 "inventory member Secret ${MIRROR_NS}/${DUPLICATE_MIRROR_SECRET_NAME}" \
  secret_exists "${MIRROR_NS}" "${DUPLICATE_MIRROR_SECRET_NAME}"; then
  echo "controller did not create inventory member Secret ${MIRROR_NS}/${DUPLICATE_MIRROR_SECRET_NAME}" >&2
  exit 1
fi

# Kubernetes creation timestamps can have coarse precision. Keep the intended winner unambiguous.
sleep 2
apply_inventory_member_profile "${DUPLICATE_NEWER_CP_NAME}"
if ! retry_until 120 "duplicate inventory member Warning Event" \
  inventory_warning_is_recorded \
    "${ARGOCD_NS}" \
    "${DUPLICATE_NEWER_CP_NAME}" \
    "DuplicateInventoryMemberID"; then
  echo "controller did not publish the duplicate inventory member Warning Event" >&2
  exit 1
fi
if ! retry_until 120 "newer duplicate inventory member Secret absence" \
  secret_is_gone "${ARGOCD_NS}" "${DUPLICATE_NEWER_SECRET_NAME}"; then
  echo "newer duplicate ClusterProfile unexpectedly received an Argo CD Secret" >&2
  exit 1
fi
assert_secret_exists "${ARGOCD_NS}" "${DUPLICATE_OLDEST_SECRET_NAME}"
assert_secret_exists "${MIRROR_NS}" "${DUPLICATE_MIRROR_SECRET_NAME}"

log "deleting the selected member and verifying next-oldest election"
kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" \
  delete clusterprofile "${DUPLICATE_OLDEST_CP_NAME}" --wait=true --timeout=60s
if ! retry_until 120 "next-oldest inventory member Secret" \
  secret_exists "${ARGOCD_NS}" "${DUPLICATE_NEWER_SECRET_NAME}"; then
  echo "controller did not elect the next-oldest ClusterProfile" >&2
  exit 1
fi
if ! retry_until 120 "oldest inventory member Secret garbage collection" \
  secret_is_gone "${ARGOCD_NS}" "${DUPLICATE_OLDEST_SECRET_NAME}"; then
  echo "deleted inventory winner's Secret was not garbage collected" >&2
  exit 1
fi
assert_secret_exists "${MIRROR_NS}" "${DUPLICATE_MIRROR_SECRET_NAME}"

kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" \
  delete clusterprofile "${DUPLICATE_NEWER_CP_NAME}" --wait=true --timeout=60s
kubectl --context "kind-${HUB_CLUSTER}" -n "${MIRROR_NS}" \
  delete clusterprofile "${DUPLICATE_MIRROR_CP_NAME}" --wait=true --timeout=60s
if ! retry_until 120 "duplicate fixture Secret garbage collection" \
  secret_is_gone "${ARGOCD_NS}" "${DUPLICATE_NEWER_SECRET_NAME}"; then
  echo "duplicate fixture Secret ${ARGOCD_NS}/${DUPLICATE_NEWER_SECRET_NAME} was not garbage collected" >&2
  exit 1
fi
if ! retry_until 120 "duplicate fixture Secret garbage collection" \
  secret_is_gone "${MIRROR_NS}" "${DUPLICATE_MIRROR_SECRET_NAME}"; then
  echo "duplicate fixture Secret ${MIRROR_NS}/${DUPLICATE_MIRROR_SECRET_NAME} was not garbage collected" >&2
  exit 1
fi

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
STATUS_PATCH="$(build_live_status_patch "${SPOKE_TLS_SERVER_NAME}" false "")"
kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" patch clusterprofile "${CP_NAME}" --subresource=status --type=merge \
  -p "${STATUS_PATCH}"

kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" wait --for=jsonpath='{.metadata.labels.environment}'=e2e "secret/${SECRET_NAME}" --timeout=120s
SECRET_JSON="$(kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" get secret "${SECRET_NAME}" -o json)"
CONFIG="$(printf '%s' "${SECRET_JSON}" | jq -r '.data.config' | base64 -d)"

test "$(printf '%s' "${SECRET_JSON}" | jq -r '.metadata.labels["argocd.argoproj.io/secret-type"]')" = "cluster"
test "$(printf '%s' "${SECRET_JSON}" | jq -r '.metadata.labels["argocd.argoproj.io/cluster-profile-name"]')" = "${CP_NAME}"
test "$(printf '%s' "${SECRET_JSON}" | jq -r '.metadata.annotations["argocd.argoproj.io/cluster-profile-name"]')" = "${CP_NAME}"
test "$(printf '%s' "${SECRET_JSON}" | jq -r '.metadata.labels.environment')" = "e2e"
test "$(printf '%s' "${SECRET_JSON}" | jq -r '.metadata.labels.team')" = "platform"
test "$(printf '%s' "${SECRET_JSON}" | jq -r '.data.server' | base64 -d)" = "https://${SPOKE_IP}:6443"
test "$(printf '%s' "${CONFIG}" | jq -r '.execProviderConfig.command')" = "${SECRETREADER_COMMAND}"
test "$(printf '%s' "${CONFIG}" | jq -r '.execProviderConfig.args // [] | length')" = "0"
test "$(printf '%s' "${CONFIG}" | jq -r '.execProviderConfig.apiVersion')" = "client.authentication.k8s.io/v1"
test "$(printf '%s' "${CONFIG}" | jq -r '.execProviderConfig.provideClusterInfo')" = "true"
test "$(printf '%s' "${CONFIG}" | jq -r '.execProviderConfig.config.clusterName')" = "${CP_NAME}"
test "$(printf '%s' "${CONFIG}" | jq -r '.tlsClientConfig.caData | length')" -gt 100
test "$(printf '%s' "${CONFIG}" | jq -r '.tlsClientConfig.serverName')" = "${SPOKE_TLS_SERVER_NAME}"
test "$(printf '%s' "${CONFIG}" | jq -r '.tlsClientConfig.insecure')" = "false"
test "$(printf '%s' "${CONFIG}" | jq -r '.proxyUrl // ""')" = ""
test "$(printf '%s' "${CONFIG}" | jq -r '.disableCompression')" = "true"

log "creating same-named ClusterProfile in ${MIRROR_NS}"
kubectl --context "kind-${HUB_CLUSTER}" -n "${MIRROR_NS}" apply -f - <<EOF
apiVersion: multicluster.x-k8s.io/v1alpha1
kind: ClusterProfile
metadata:
  name: ${CP_NAME}
  namespace: ${MIRROR_NS}
  labels:
    environment: e2e-mirror
spec:
  clusterManager:
    name: manual
  displayName: Mirror Namespace E2E
EOF
set_builtin_profile_access "${CP_NAME}" "https://mirror.example.com:6443" "${MIRROR_NS}"

log "verifying the Secret is mirrored into ${MIRROR_NS}"
kubectl --context "kind-${HUB_CLUSTER}" -n "${MIRROR_NS}" wait --for=jsonpath='{.metadata.labels.environment}'=e2e-mirror "secret/${SECRET_NAME}" --timeout=120s
test "$(kubectl --context "kind-${HUB_CLUSTER}" -n "${MIRROR_NS}" get secret "${SECRET_NAME}" -o jsonpath='{.data.server}' | base64 -d)" = "https://mirror.example.com:6443"

log "verifying the ${ARGOCD_NS} Secret was not clobbered by the same-named ClusterProfile"
test "$(kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" get secret "${SECRET_NAME}" -o jsonpath='{.data.server}' | base64 -d)" = "https://${SPOKE_IP}:6443"

log "verifying the mirrored Secret is garbage collected with its ClusterProfile"
kubectl --context "kind-${HUB_CLUSTER}" -n "${MIRROR_NS}" delete clusterprofile "${CP_NAME}" --timeout=60s
if ! retry_until 120 "garbage collection of secret ${SECRET_NAME} in ${MIRROR_NS}" \
  secret_is_gone "${MIRROR_NS}" "${SECRET_NAME}"; then
  echo "mirrored secret ${SECRET_NAME} in ${MIRROR_NS} was not garbage collected" >&2
  exit 1
fi

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

if ! retry_until 120 "application ${APP_NAME} creation" application_exists; then
  echo "application ${APP_NAME} was not created" >&2
  exit 1
fi
log "application ${APP_NAME} created"

wait_for_application
kubectl --context "kind-${SPOKE_CLUSTER}" -n guestbook wait --for=condition=Ready pod -l app=guestbook-ui --timeout=300s

log "verifying the live cluster before exercising access revocation"
login_argocd_server
if ! retry_until 120 "Argo CD cluster connection before access revocation" argocd_cluster_state_is Successful; then
  echo "Argo CD did not report a successful cluster connection before access revocation" >&2
  exit 1
fi

log "proving TLS server-name verification is enforced by the live Argo CD client"
patch_live_cluster_connection "${INVALID_SPOKE_TLS_SERVER_NAME}" false ""
if ! retry_until 120 "rendered invalid TLS server name" \
  managed_cluster_secret_has_connection_fields "${INVALID_SPOKE_TLS_SERVER_NAME}" false ""; then
  echo "generated Secret did not preserve the invalid TLS server name fixture" >&2
  exit 1
fi
if ! retry_until 120 "Argo CD cluster rejection for the invalid TLS server name" \
  argocd_cluster_state_is Failed; then
  echo "Argo CD did not reject the ClusterProfile's invalid TLS server name" >&2
  exit 1
fi

log "proving insecure-skip-tls-verify is honored, then restoring verified TLS"
patch_live_cluster_connection "${INVALID_SPOKE_TLS_SERVER_NAME}" true ""
if ! retry_until 120 "rendered insecure TLS setting" \
  managed_cluster_secret_has_connection_fields "${INVALID_SPOKE_TLS_SERVER_NAME}" true ""; then
  echo "generated Secret did not preserve insecure-skip-tls-verify" >&2
  exit 1
fi
if ! retry_until 120 "Argo CD cluster connection with TLS verification disabled" \
  argocd_cluster_state_is Successful; then
  echo "Argo CD did not honor insecure-skip-tls-verify from the ClusterProfile" >&2
  exit 1
fi
patch_live_cluster_connection "${SPOKE_TLS_SERVER_NAME}" false ""
if ! retry_until 120 "restored verified TLS configuration" \
  managed_cluster_secret_has_connection_fields "${SPOKE_TLS_SERVER_NAME}" false ""; then
  echo "generated Secret did not restore verified TLS configuration" >&2
  exit 1
fi
if ! retry_until 120 "Argo CD cluster connection after verified TLS restoration" \
  argocd_cluster_state_is Successful; then
  echo "Argo CD cluster connection did not recover after verified TLS restoration" >&2
  exit 1
fi

log "proving the live Argo CD client routes through ClusterProfile proxy-url"
patch_live_cluster_connection "${SPOKE_TLS_SERVER_NAME}" false "${UNREACHABLE_PROXY_URL}"
if ! retry_until 120 "rendered unreachable proxy URL" \
  managed_cluster_secret_has_connection_fields \
    "${SPOKE_TLS_SERVER_NAME}" false "${UNREACHABLE_PROXY_URL}"; then
  echo "generated Secret did not preserve proxy-url" >&2
  exit 1
fi
if ! retry_until 120 "Argo CD cluster rejection through the unreachable proxy" \
  argocd_cluster_state_is Failed; then
  echo "Argo CD remained connected despite the ClusterProfile's unreachable proxy-url" >&2
  exit 1
fi
patch_live_cluster_connection "${SPOKE_TLS_SERVER_NAME}" false ""
if ! retry_until 120 "cleared proxy URL" \
  managed_cluster_secret_has_connection_fields "${SPOKE_TLS_SERVER_NAME}" false ""; then
  echo "generated Secret did not clear proxy-url" >&2
  exit 1
fi
if ! retry_until 120 "Argo CD cluster recovery after clearing proxy-url" \
  argocd_cluster_state_is Successful; then
  echo "Argo CD cluster connection did not recover after proxy-url was cleared" >&2
  exit 1
fi
wait_for_application

log "injecting stale Argo CD fields into the owned Secret without changing the ClusterProfile"
kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" \
  patch secret "${SECRET_NAME}" --type=merge \
  -p '{"stringData":{"project":"stale-project","namespaces":"stale-namespace","clusterResources":"true","shard":"99","stale-auth-material":"must-be-removed"}}' \
  >/dev/null
if ! retry_until 120 "owned Secret exact data reconciliation from its Secret watch" \
  managed_cluster_secret_has_exact_data; then
  echo "owned Secret ${SECRET_NAME} retained stale Argo CD data after its own update event" >&2
  exit 1
fi
EXACT_DATA_SECRET_RV="$(
  kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" \
    get secret "${SECRET_NAME}" -o jsonpath='{.metadata.resourceVersion}'
)"

log "verifying exact Secret data remains stable without self-triggered updates"
sleep 10
STABLE_EXACT_DATA_SECRET_RV="$(
  kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" \
    get secret "${SECRET_NAME}" -o jsonpath='{.metadata.resourceVersion}'
)"
test "${STABLE_EXACT_DATA_SECRET_RV}" = "${EXACT_DATA_SECRET_RV}"
managed_cluster_secret_has_exact_data
if ! retry_until 120 "Argo CD cluster connection after Secret data repair" argocd_cluster_state_is Successful; then
  echo "Argo CD cluster connection did not recover after exact Secret data reconciliation" >&2
  exit 1
fi
wait_for_application
kubectl --context "kind-${SPOKE_CLUSTER}" -n guestbook \
  wait --for=condition=Ready pod -l app=guestbook-ui --timeout=300s

LIVE_CP_UID="$(clusterprofile_uid "${ARGOCD_NS}" "${CP_NAME}")"
REVOKED_SECRET_UID="$(
  kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" \
    get secret "${SECRET_NAME}" -o jsonpath='{.metadata.uid}'
)"
log "removing every advertised access provider from the live ClusterProfile"
kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" \
  patch clusterprofile "${CP_NAME}" --subresource=status --type=merge \
  -p '{"status":{"accessProviders":null,"credentialProviders":null}}'
if ! retry_until 120 "owned cluster Secret removal after access revocation" \
  secret_is_gone "${ARGOCD_NS}" "${SECRET_NAME}"; then
  echo "owned Secret ${SECRET_NAME} survived ClusterProfile access revocation" >&2
  exit 1
fi
if ! retry_until 120 "Argo CD to remove the revoked cluster registration" argocd_cluster_is_absent; then
  echo "Argo CD retained an exact cluster entry after access was revoked" >&2
  argocd_cluster_list_json | jq . || true
  exit 1
fi
if ! retry_until 120 "ApplicationSet to remove the revoked cluster Application" generated_application_is_gone; then
  echo "ApplicationSet retained Application ${APP_NAME} after its cluster registration was removed" >&2
  exit 1
fi

log "verifying empty access remains a quiet stable state"
REVOCATION_QUIET_STARTED_AT="$(date -u +%Y-%m-%dT%H:%M:%S.%NZ)"
sleep 10
if ! secret_is_gone "${ARGOCD_NS}" "${SECRET_NAME}"; then
  echo "owned Secret ${SECRET_NAME} was recreated without an advertised access provider" >&2
  exit 1
fi
REVOCATION_LOGS="$(
  controller_logs_since_time "${REVOCATION_QUIET_STARTED_AT}"
)"
if grep -E 'unable to create or update secret for ClusterProfile|unable to remove secret after ClusterProfile access was revoked|Reconciler error' \
  <<<"${REVOCATION_LOGS}" >/dev/null; then
  echo "ClusterProfile controller entered an error loop after explicit access revocation" >&2
  printf '%s\n' "${REVOCATION_LOGS}" >&2
  exit 1
fi

log "restoring ClusterProfile access and verifying live recovery"
kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" \
  patch clusterprofile "${CP_NAME}" --subresource=status --type=merge -p "${STATUS_PATCH}"
if ! retry_until 120 "new cluster Secret owned by the recreated ClusterProfile" restored_cluster_secret_is_ready; then
  echo "ClusterProfile access recovery did not create a new Secret owned by the recreated ClusterProfile" >&2
  exit 1
fi
wait_for_application
kubectl --context "kind-${SPOKE_CLUSTER}" -n guestbook wait --for=condition=Ready pod -l app=guestbook-ui --timeout=300s
verify_argocd_server_cluster_access

if [ "${E2E_INSTALL_METHOD}" = "helm" ]; then
  log "switching Helm to a remote-only watch namespace"
  REMOTE_ONLY_STARTED_AT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  helm --kube-context "kind-${HUB_CLUSTER}" upgrade cpia charts/argocd-clusterprofile-controller \
    --namespace "${ARGOCD_NS}" \
    --reuse-values \
    --set-json "controller.clusterProfileNamespaces=[\"${MIRROR_NS}\"]" \
    --wait \
    --timeout 5m
  kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" \
    rollout status deploy/argocd-clusterprofile-controller --timeout=300s

  REMOTE_ONLY_CONTROLLER_IDENTITY="$(wait_for_controller_pods 2 '.ready and .running')"
  if ! jq -e \
    --arg image "${E2E_IMG}" \
    --argjson image_ids "${E2E_NODE_IMAGE_IDS}" \
    'length == 2 and
     all(.[];
       .serviceAccount == "argocd-clusterprofile-controller" and
       .image == $image and
       (.imageID | IN($image_ids[])) and
       (.containerID | length) > 0 and
       .restartCount == 0)' <<<"${REMOTE_ONLY_CONTROLLER_IDENTITY}" >/dev/null; then
    echo "remote-only controller Pod identities are not clean: ${REMOTE_ONLY_CONTROLLER_IDENTITY}" >&2
    exit 1
  fi
  if ! retry_until 60 "remote-only controller spread" controllers_are_spread; then
    echo "remote-only controller replicas did not remain spread" >&2
    exit 1
  fi
  if ! retry_until 60 "remote-only controller metrics" controller_metrics_are_available; then
    echo "controller metrics did not become available after the remote-only rollout" >&2
    exit 1
  fi
  if ! REMOTE_ONLY_METRICS_BEFORE="$(controller_metrics_snapshot)"; then
    echo "unable to read controller metrics before the remote-only fixture" >&2
    exit 1
  fi

  for verb in get list watch create update patch delete; do
    assert_cannot_i "${verb}" secrets --namespace "${ARGOCD_NS}"
  done
  for verb in get list watch create update patch delete; do
    assert_can_i "${verb}" secrets --namespace "${MIRROR_NS}"
  done
  for verb in get list watch create update patch delete; do
    assert_cannot_i "${verb}" clusterprofiles.multicluster.x-k8s.io --namespace "${ARGOCD_NS}"
  done
  for verb in get list watch; do
    assert_can_i "${verb}" clusterprofiles.multicluster.x-k8s.io --namespace "${MIRROR_NS}"
  done
  for verb in create update patch delete; do
    assert_cannot_i "${verb}" clusterprofiles.multicluster.x-k8s.io --namespace "${MIRROR_NS}"
  done
  for verb in get list watch create update patch delete; do
    assert_cannot_i "${verb}" events.events.k8s.io --namespace "${ARGOCD_NS}"
  done
  assert_can_i create events.events.k8s.io --namespace "${MIRROR_NS}"
  assert_can_i patch events.events.k8s.io --namespace "${MIRROR_NS}"
  for verb in get list watch update delete; do
    assert_cannot_i "${verb}" events.events.k8s.io --namespace "${MIRROR_NS}"
  done

  log "verifying remote-only reconciliation and recovery from a current provider error"
  kubectl --context "kind-${HUB_CLUSTER}" -n "${MIRROR_NS}" apply -f - <<EOF
apiVersion: multicluster.x-k8s.io/v1alpha1
kind: ClusterProfile
metadata:
  name: ${REMOTE_ONLY_CP_NAME}
spec:
  clusterManager:
    name: manual
EOF
  kubectl --context "kind-${HUB_CLUSTER}" -n "${MIRROR_NS}" \
    patch clusterprofile "${REMOTE_ONLY_CP_NAME}" --subresource=status --type=merge -p "{
      \"status\": {
        \"accessProviders\": [
          {
            \"name\": \"argo-cd-builtin-unsupported\",
            \"cluster\": {\"server\": \"${REMOTE_ONLY_SERVER}\"}
          }
        ]
      }
    }" >/dev/null
  if ! retry_until 60 "remote-only transient provider error" remote_only_transient_error_is_logged; then
    echo "remote-only controller did not report the unsupported provider" >&2
    exit 1
  fi
  set_builtin_profile_access "${REMOTE_ONLY_CP_NAME}" "${REMOTE_ONLY_SERVER}" "${MIRROR_NS}"
  if ! retry_until 120 "remote-only Secret reconciliation after provider recovery" \
    remote_only_fixture_is_reconciled; then
    echo "remote-only controller did not reconcile the same-namespace Secret" >&2
    exit 1
  fi
  kubectl --context "kind-${HUB_CLUSTER}" -n "${MIRROR_NS}" \
    delete clusterprofile "${REMOTE_ONLY_CP_NAME}" --wait=true --timeout=60s
  if ! retry_until 120 "remote-only fixture garbage collection" remote_only_fixture_is_deleted; then
    echo "remote-only ClusterProfile or one of its Secrets survived deletion" >&2
    exit 1
  fi
  wait_for_application
  if ! retry_until 30 "remote-only controller workqueue to become idle" controller_workqueue_is_idle; then
    echo "remote-only controller workqueue did not become idle" >&2
    exit 1
  fi
  if ! REMOTE_ONLY_METRICS_SETTLED="$(controller_metrics_snapshot)"; then
    echo "unable to read settled controller metrics" >&2
    exit 1
  fi

  log "verifying the recovered remote-only controller stays quiescent"
  sleep 15
  if ! remote_only_fixture_is_deleted; then
    echo "remote-only fixture reappeared during the quiet window" >&2
    exit 1
  fi
  wait_for_application
  if ! controller_workqueue_is_idle; then
    echo "remote-only controller workqueue became active during the quiet window" >&2
    exit 1
  fi
  if ! REMOTE_ONLY_METRICS_FINAL="$(controller_metrics_snapshot)"; then
    echo "unable to read controller metrics after the quiet window" >&2
    exit 1
  fi
  if ! REMOTE_ONLY_LOGS="$(
    controller_logs_since_time "${REMOTE_ONLY_STARTED_AT}"
  )"; then
    echo "unable to read remote-only controller logs" >&2
    exit 1
  fi
  REMOTE_ONLY_FINAL_CONTROLLER_STATE="$(controller_pods_state)"
  if ! jq -e --argjson initial "${REMOTE_ONLY_CONTROLLER_IDENTITY}" '
    length == 2 and
    all(.[];
      . as $current |
      ($initial[] | select(.pod == $current.pod)) as $original |
      $current.serviceAccount == "argocd-clusterprofile-controller" and
      $current.ready == true and
      $current.running == true and
      $current.restartCount == 0 and
      $current.containerID == $original.containerID)' \
      <<<"${REMOTE_ONLY_FINAL_CONTROLLER_STATE}" >/dev/null; then
    echo "remote-only controllers restarted during the fixture: ${REMOTE_ONLY_FINAL_CONTROLLER_STATE}" >&2
    exit 1
  fi
  REMOTE_ONLY_METRICS_DELTA="$(
    controller_metrics_delta "${REMOTE_ONLY_METRICS_BEFORE}" "${REMOTE_ONLY_METRICS_SETTLED}"
  )"
  if ! jq -e '
    .errors > 0 and
    .retries == .errors and
    .panics == 0 and
    .terminalErrors == 0 and
    .timeouts == 0' <<<"${REMOTE_ONLY_METRICS_DELTA}" >/dev/null; then
    echo "remote-only provider recovery violated its retry contract: ${REMOTE_ONLY_METRICS_DELTA}" >&2
    exit 1
  fi
  REMOTE_ONLY_QUIET_METRICS_DELTA="$(
    controller_metrics_delta "${REMOTE_ONLY_METRICS_SETTLED}" "${REMOTE_ONLY_METRICS_FINAL}"
  )"
  if ! jq -e 'all(.[]; . == 0)' <<<"${REMOTE_ONLY_QUIET_METRICS_DELTA}" >/dev/null; then
    echo "remote-only controller kept retrying after convergence: ${REMOTE_ONLY_QUIET_METRICS_DELTA}" >&2
    exit 1
  fi
  REMOTE_ONLY_RECONCILE_ERRORS="$(
    jq -Rsc '[split("\n")[] | fromjson? | select(.msg == "Reconciler error")]' \
      <<<"${REMOTE_ONLY_LOGS}"
  )"
  if ! jq -e \
    --arg namespace "${MIRROR_NS}" \
    --arg name "${REMOTE_ONLY_CP_NAME}" \
    --argjson expected "$(jq '.errors' <<<"${REMOTE_ONLY_METRICS_DELTA}")" '
      length == $expected and
      all(.[];
        .controller == "clusterprofile" and
        .namespace == $namespace and
        .name == $name and
        ((.error | type) == "string") and
        (.error | contains("unsupported built-in access provider \"argo-cd-builtin-unsupported\"")))' \
      <<<"${REMOTE_ONLY_RECONCILE_ERRORS}" >/dev/null; then
    echo "remote-only controller emitted an unexpected reconcile error: ${REMOTE_ONLY_RECONCILE_ERRORS}" >&2
    exit 1
  fi
  if grep -E -i 'forbidden|unknown namespace for the cache|panic|fatal' \
    <<<"${REMOTE_ONLY_LOGS}" >/dev/null; then
    echo "remote-only controller emitted a fatal, cache, or authorization error" >&2
    printf '%s\n' "${REMOTE_ONLY_LOGS}" >&2
    exit 1
  fi
  log "verified remote-only watch, transient-error recovery, and live Application continuity"
fi

FINAL_CONTROLLER_STATE="$(controller_pods_state)"
if ! jq -e \
  --arg image "${E2E_IMG}" \
  --argjson image_ids "${E2E_NODE_IMAGE_IDS}" \
  'length == 2 and
   all(.[];
     .image == $image and
     (.imageID | IN($image_ids[])) and
     (.containerID | length) > 0 and
     .ready == true and
     .running == true and
     .restartCount == 0)' <<<"${FINAL_CONTROLLER_STATE}" >/dev/null; then
  echo "controllers did not finish the live scenarios healthy and restart-free: ${FINAL_CONTROLLER_STATE}" >&2
  exit 1
fi
if ! controllers_are_spread; then
  echo "controller replicas did not finish the live scenarios spread across workers" >&2
  exit 1
fi

log "live Argo CD integration scenarios passed"

log "cleaning live fixtures before HA fault scenarios"
kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" \
  delete applicationset guestbook-e2e --ignore-not-found --wait=true --timeout=120s >/dev/null
for namespace in "${ARGOCD_NS}" "${MIRROR_NS}"; do
  kubectl --context "kind-${HUB_CLUSTER}" -n "${namespace}" \
    delete clusterprofiles.multicluster.x-k8s.io --all \
    --ignore-not-found --wait=true --timeout=120s >/dev/null
  if ! retry_until 120 "ClusterProfile Secret garbage collection in ${namespace}" \
    clusterprofile_owned_secrets_are_gone "${namespace}"; then
    echo "ClusterProfile-owned Secrets remained in ${namespace} before the HA phase" >&2
    exit 1
  fi
done
kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" \
  delete secret "${COLLISION_SECRET_NAME}" --ignore-not-found >/dev/null
if ! retry_until 120 "live Application removal before HA phase" generated_application_is_gone; then
  echo "live Application remained after removing its ApplicationSet" >&2
  exit 1
fi

log "stopping Argo CD workloads while preserving the ClusterProfile controller"
for resource in $(
  kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" \
    get deployments,statefulsets -o name
); do
  case "${resource}" in
    */argocd-clusterprofile-controller) continue ;;
  esac
  kubectl --context "kind-${HUB_CLUSTER}" -n "${ARGOCD_NS}" \
    scale "${resource}" --replicas=0 >/dev/null
done
if ! retry_until 180 "Argo CD workloads to stop" argocd_workloads_are_stopped; then
  echo "Argo CD workloads did not stop before the HA fault scenarios" >&2
  exit 1
fi

if [ "${E2E_INSTALL_METHOD}" = "helm" ]; then
  log "restoring the Helm controller watch to ${ARGOCD_NS} for HA scenarios"
  helm --kube-context "kind-${HUB_CLUSTER}" upgrade cpia charts/argocd-clusterprofile-controller \
    --namespace "${ARGOCD_NS}" \
    --reuse-values \
    --set-json "controller.clusterProfileNamespaces=[\"${ARGOCD_NS}\"]" \
    --wait \
    --timeout 5m
fi

E2E_SHARED_KUBECONFIG="${KUBECONFIG}" \
E2E_HUB_CLUSTER="${HUB_CLUSTER}" \
E2E_ARGOCD_NS="${ARGOCD_NS}" \
E2E_SKIP_CLEANUP="${E2E_SKIP_CLEANUP:-0}" \
  bash "${SCRIPT_DIR}/e2e-ha-phase.sh"

log "full live and HA e2e passed"
