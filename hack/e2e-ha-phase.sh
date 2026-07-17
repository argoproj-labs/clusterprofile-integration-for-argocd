#!/usr/bin/env bash

set -euo pipefail

: "${E2E_SHARED_KUBECONFIG:?E2E_SHARED_KUBECONFIG must point to the parent e2e kubeconfig}"
: "${E2E_HUB_CLUSTER:?E2E_HUB_CLUSTER must name the parent e2e hub cluster}"
: "${E2E_ARGOCD_NS:?E2E_ARGOCD_NS must name the parent e2e Argo CD namespace}"

HUB_CONTEXT="kind-${E2E_HUB_CLUSTER}"
NAMESPACE="${E2E_ARGOCD_NS}"
CONTROLLER_LABEL="app.kubernetes.io/name=argocd-clusterprofile-controller"
PROFILE_COUNT="${HA_PROFILE_COUNT:-64}"
CONTROLLER_CLIENT_QPS=5
CONTROLLER_CLIENT_BURST=1

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
WORK_DIR="$(mktemp -d)"
export KUBECONFIG="${E2E_SHARED_KUBECONFIG}"
PATCH_PID=""
HELD_POD=""
LEASE_WATCH_PID=""
LEADER_MONITOR_PID=""
PLANNED_LOG_PID=""
PATCH_LOG=""
LEADER_MONITOR_FAILURE="${WORK_DIR}/leader-monitor.failure"
LEADER_MONITOR_SAMPLES="${WORK_DIR}/leader-monitor.samples"
LEADER_MONITOR_ROUND_DIR="${WORK_DIR}/leader-monitor-round"
BACKLOG_STATE_FILE="${WORK_DIR}/backlog-state"

log() {
  printf '[e2e-ha] %s\n' "$*"
}

require_command() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "required command not found: $1" >&2
    exit 1
  fi
}

stop_background() {
  local pid="$1"
  [ -n "${pid}" ] || return 0
  kill "${pid}" >/dev/null 2>&1 || true
  wait "${pid}" 2>/dev/null || true
}

dump_diagnostics() {
  set +e
  log "collecting diagnostics"
  kubectl --context "${HUB_CONTEXT}" get nodes -o wide
  kubectl --context "${HUB_CONTEXT}" -n "${NAMESPACE}" get deploy,rs,pods,pdb,lease -o wide
  kubectl --context "${HUB_CONTEXT}" -n "${NAMESPACE}" get events --sort-by=.lastTimestamp | tail -100
  for pod in $(controller_pods); do
    log "logs for ${pod}"
    kubectl --context "${HUB_CONTEXT}" -n "${NAMESPACE}" logs "pod/${pod}" --all-containers --tail=200
    kubectl --context "${HUB_CONTEXT}" -n "${NAMESPACE}" logs "pod/${pod}" --all-containers --previous --tail=200
  done
}

cleanup() {
  local status=$?
  stop_lease_watch
  stop_leader_monitor
  stop_background "${PLANNED_LOG_PID}"
  stop_background "${PATCH_PID}"
  if [ "${status}" -ne 0 ]; then
    dump_diagnostics
  fi
  if [ -n "${HELD_POD}" ]; then
    kubectl --context "${HUB_CONTEXT}" -n "${NAMESPACE}" \
      patch "pod/${HELD_POD}" --type=json \
      -p='[{"op":"remove","path":"/metadata/finalizers"}]' >/dev/null 2>&1 || true
  fi
  for node in $(kubectl --context "${HUB_CONTEXT}" get nodes -o name 2>/dev/null); do
    kubectl --context "${HUB_CONTEXT}" uncordon "${node}" >/dev/null 2>&1 || true
  done
  if [ "${E2E_SKIP_CLEANUP:-0}" != "1" ]; then
    rm -rf "${WORK_DIR}"
  else
    log "leaving HA diagnostics in ${WORK_DIR}; parent e2e owns cluster cleanup"
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

controller_pods() {
  kubectl --context "${HUB_CONTEXT}" -n "${NAMESPACE}" \
    get pods -l "${CONTROLLER_LABEL}" -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}'
}

ready_controller_pods() {
  kubectl --context "${HUB_CONTEXT}" -n "${NAMESPACE}" \
    get pods -l "${CONTROLLER_LABEL}" -o json | jq -r '
      .items[] |
      select(.metadata.deletionTimestamp == null) |
      select(any(.status.conditions[]?; .type == "Ready" and .status == "True")) |
      .metadata.name'
}

deployment_is_ready_with_replicas() {
  local replicas="$1"
  kubectl --context "${HUB_CONTEXT}" -n "${NAMESPACE}" \
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
  local state
  state="$(
    kubectl --context "${HUB_CONTEXT}" -n "${NAMESPACE}" \
      get pods -l "${CONTROLLER_LABEL}" -o json | jq -c '
        [.items[] |
          select(.metadata.deletionTimestamp == null) |
          select(any(.status.conditions[]?; .type == "Ready" and .status == "True")) |
          .spec.nodeName] | {count: length, uniqueNodes: (unique | length)}'
  )"
  jq -e '.count == 2 and .uniqueNodes == 2' <<<"${state}" >/dev/null
}

lease_holder_identity() {
  kubectl --context "${HUB_CONTEXT}" -n "${NAMESPACE}" \
    get lease clusterprofile.argoproj.io -o jsonpath='{.spec.holderIdentity}'
}

lease_has_live_holder() {
  local holder pod
  holder="$(lease_holder_identity)" || return 1
  [ -n "${holder}" ] || return 1
  pod="${holder%%_*}"
  kubectl --context "${HUB_CONTEXT}" -n "${NAMESPACE}" get "pod/${pod}" >/dev/null 2>&1
}

# Echoes nothing if the holder never changes within the window.
wait_for_new_lease_holder() {
  local attempts="$1" old_identity="$2" holder
  for _ in $(seq 1 "${attempts}"); do
    holder="$(lease_holder_identity 2>/dev/null || true)"
    if [ -n "${holder}" ] && [ "${holder}" != "${old_identity}" ]; then
      printf '%s\n' "${holder}"
      return 0
    fi
    sleep 0.1
  done
}

pod_metrics_url() {
  printf '/api/v1/namespaces/%s/pods/%s:8080/proxy/metrics' "${NAMESPACE}" "$1"
}

pod_metrics() {
  kubectl --context "${HUB_CONTEXT}" --request-timeout=5s \
    get --raw "$(pod_metrics_url "$1")"
}

# bash arithmetic is integer-only, so metric values need awk.
float_add() {
  awk -v left="$1" -v right="$2" 'BEGIN {print left + right}'
}

metric_stats() {
  awk -v metric="$1" '
    index($0, metric "{") == 1 || $1 == metric {sum += $NF; count += 1}
    END {print sum + 0, count + 0}'
}

metric_sum() {
  metric_stats "$1" | cut -d' ' -f1
}

pod_metric_sum() {
  local pod="$1" metric="$2"
  pod_metrics "${pod}" | metric_sum "${metric}"
}

# controller-runtime series that must stay at zero for the whole run.
TERMINAL_METRICS=(
  controller_runtime_reconcile_panics_total
  controller_runtime_terminal_reconcile_errors_total
  controller_runtime_reconcile_timeouts_total
)

terminal_metric_sum() {
  local metrics="$1" metric sum=0
  for metric in "${TERMINAL_METRICS[@]}"; do
    sum="$(float_add "${sum}" "$(metric_sum "${metric}" <<<"${metrics}")")"
  done
  printf '%s\n' "${sum}"
}

start_lease_watch() {
  local output_file="$1" resource_version
  resource_version="$(
    kubectl --context "${HUB_CONTEXT}" -n "${NAMESPACE}" \
      get lease clusterprofile.argoproj.io -o jsonpath='{.metadata.resourceVersion}'
  )"
  : >"${output_file}"
  kubectl --context "${HUB_CONTEXT}" get --raw \
    "/apis/coordination.k8s.io/v1/namespaces/${NAMESPACE}/leases?watch=true&allowWatchBookmarks=false&fieldSelector=metadata.name%3Dclusterprofile.argoproj.io&resourceVersion=${resource_version}&timeoutSeconds=60" \
    >"${output_file}" 2>"${output_file}.err" &
  LEASE_WATCH_PID=$!
  sleep 0.2
  if ! kill -0 "${LEASE_WATCH_PID}" >/dev/null 2>&1; then
    wait "${LEASE_WATCH_PID}" 2>/dev/null || true
    echo "unable to start Lease watch at resourceVersion ${resource_version}" >&2
    cat "${output_file}.err" >&2 || true
    LEASE_WATCH_PID=""
    return 1
  fi
}

stop_lease_watch() {
  stop_background "${LEASE_WATCH_PID}"
  LEASE_WATCH_PID=""
}

lease_watch_transitions() {
  jq -r '
    select(.type == "MODIFIED") |
    [(.object.spec.holderIdentity // ""), (.object.spec.leaseDurationSeconds // 0)] |
    @tsv' "$1"
}

lease_watch_proves_planned_release() {
  local watch_file="$1" new_holder="$2"
  lease_watch_transitions "${watch_file}" | awk -F '\t' -v new_holder="${new_holder}" '
      $1 == "" && $2 == 1 && release == 0 {release = NR}
      $1 == new_holder && acquire == 0 {acquire = NR}
      END {exit !(release > 0 && acquire > release)}'
}

lease_watch_proves_forced_expiry() {
  local watch_file="$1" new_holder="$2"
  lease_watch_transitions "${watch_file}" | awk -F '\t' -v new_holder="${new_holder}" '
      $1 == "" && $2 == 1 {release = 1}
      $1 == new_holder {acquire = 1}
      END {exit !(release == 0 && acquire == 1)}'
}

monitor_leader_uniqueness() {
  local pods pod metrics leader_stats leader active terminal leader_sum active_sum terminal_sum
  local reachable lease_before lease_after pid
  local -a scrape_pids
  : >"${LEADER_MONITOR_SAMPLES}"
  rm -f "${LEADER_MONITOR_FAILURE}"
  mkdir -p "${LEADER_MONITOR_ROUND_DIR}"
  while true; do
    lease_before="$(
      kubectl --context "${HUB_CONTEXT}" -n "${NAMESPACE}" \
        get lease clusterprofile.argoproj.io -o jsonpath='{.metadata.resourceVersion}' \
        2>/dev/null || true
    )"
    pods="$(
      kubectl --context "${HUB_CONTEXT}" -n "${NAMESPACE}" \
        get pods -l "${CONTROLLER_LABEL}" -o json 2>/dev/null | jq -r '
          .items[] | select(.status.phase == "Running") | .metadata.name' || true
    )"
    rm -f "${LEADER_MONITOR_ROUND_DIR}"/*
    scrape_pids=()
    for pod in ${pods}; do
      timeout 2 kubectl --context "${HUB_CONTEXT}" get --raw "$(pod_metrics_url "${pod}")" \
        >"${LEADER_MONITOR_ROUND_DIR}/${pod}" 2>/dev/null &
      scrape_pids+=("$!")
    done
    for pid in "${scrape_pids[@]}"; do
      wait "${pid}" 2>/dev/null || true
    done
    lease_after="$(
      kubectl --context "${HUB_CONTEXT}" -n "${NAMESPACE}" \
        get lease clusterprofile.argoproj.io -o jsonpath='{.metadata.resourceVersion}' \
        2>/dev/null || true
    )"
    leader_sum=0
    active_sum=0
    terminal_sum=0
    reachable=0
    for pod in ${pods}; do
      metrics="$(<"${LEADER_MONITOR_ROUND_DIR}/${pod}")"
      [ -n "${metrics}" ] || continue
      leader_stats="$(metric_stats leader_election_master_status <<<"${metrics}")"
      [ "${leader_stats##* }" -gt 0 ] || continue
      leader="${leader_stats%% *}"
      active="$(metric_sum controller_runtime_active_workers <<<"${metrics}")"
      terminal="$(terminal_metric_sum "${metrics}")"
      leader_sum="$(float_add "${leader_sum}" "${leader}")"
      active_sum="$(float_add "${active_sum}" "${active}")"
      terminal_sum="$(float_add "${terminal_sum}" "${terminal}")"
      reachable=$((reachable + 1))
    done
    # Discard a round that straddled a Lease transition: even concurrent HTTP
    # requests can complete on opposite sides of the handoff.
    if [ -n "${lease_before}" ] && [ "${lease_before}" = "${lease_after}" ] &&
      [ "${reachable}" -gt 0 ]; then
      printf '%s\t%s\t%s\t%s\t%s\t%s\n' \
        "$(date +%s%N)" "${reachable}" "${leader_sum}" "${active_sum}" \
        "${terminal_sum}" "${lease_after}" \
        >>"${LEADER_MONITOR_SAMPLES}"
      if awk -v leaders="${leader_sum}" -v workers="${active_sum}" \
        -v terminal="${terminal_sum}" \
        'BEGIN {exit !(leaders > 1 || workers > 1 || terminal > 0)}'; then
        printf 'unsafe metrics: leaders=%s active-workers=%s terminal=%s rv=%s\n' \
          "${leader_sum}" "${active_sum}" "${terminal_sum}" "${lease_after}" \
          >"${LEADER_MONITOR_FAILURE}"
        return 1
      fi
    fi
    sleep 0.2
  done
}

start_leader_monitor() {
  monitor_leader_uniqueness &
  LEADER_MONITOR_PID=$!
}

leader_monitor_is_healthy() {
  [ -n "${LEADER_MONITOR_PID}" ] &&
    kill -0 "${LEADER_MONITOR_PID}" >/dev/null 2>&1 &&
    [ ! -s "${LEADER_MONITOR_FAILURE}" ]
}

stop_leader_monitor() {
  stop_background "${LEADER_MONITOR_PID}"
  LEADER_MONITOR_PID=""
}

terminal_metrics_are_zero() {
  local pod metrics metric stats value count holder holder_pod
  holder="$(lease_holder_identity)" || return 1
  holder_pod="${holder%%_*}"
  for pod in $(ready_controller_pods); do
    metrics="$(pod_metrics "${pod}")" || return 1
    for metric in "${TERMINAL_METRICS[@]}"; do
      stats="$(metric_stats "${metric}" <<<"${metrics}")"
      value="${stats%% *}"
      count="${stats##* }"
      if [ "${pod}" = "${holder_pod}" ] && [ "${count}" -eq 0 ]; then
        return 1
      fi
      awk -v value="${value}" 'BEGIN {exit !(value == 0)}' || return 1
    done
  done
}

exactly_one_metric_leader() {
  local pod value sum=0 count=0
  for pod in $(ready_controller_pods); do
    value="$(pod_metric_sum "${pod}" leader_election_master_status)" || return 1
    sum="$(float_add "${sum}" "${value}")"
    count=$((count + 1))
  done
  [ "${count}" -eq 2 ] && awk -v sum="${sum}" 'BEGIN {exit !(sum == 1)}'
}

leader_queue_is_backlogged() {
  local holder pod metrics depth active
  if ! holder="$(lease_holder_identity)"; then
    printf 'leader Lease is not readable\n' >"${BACKLOG_STATE_FILE}"
    return 1
  fi
  pod="${holder%%_*}"
  if ! metrics="$(pod_metrics "${pod}")"; then
    printf 'holder=%s pod=%s metrics=unreachable\n' "${holder}" "${pod}" \
      >"${BACKLOG_STATE_FILE}"
    return 1
  fi
  depth="$(metric_sum workqueue_depth <<<"${metrics}")"
  active="$(metric_sum controller_runtime_active_workers <<<"${metrics}")"
  printf 'holder=%s pod=%s workqueue_depth=%s active_workers=%s\n' \
    "${holder}" "${pod}" "${depth}" "${active}" >"${BACKLOG_STATE_FILE}"
  awk -v depth="${depth}" -v active="${active}" 'BEGIN {exit !(depth > 0 && active == 1)}'
}

pdb_allows_one_disruption() {
  kubectl --context "${HUB_CONTEXT}" -n "${NAMESPACE}" \
    get pdb argocd-clusterprofile-controller -o json | jq -e '
      .status.currentHealthy == 2 and
      .status.desiredHealthy == 1 and
      .status.disruptionsAllowed == 1' >/dev/null
}

pdb_blocks_disruption() {
  kubectl --context "${HUB_CONTEXT}" -n "${NAMESPACE}" \
    get pdb argocd-clusterprofile-controller -o json | jq -e '
      .status.currentHealthy == 1 and
      .status.desiredHealthy == 1 and
      .status.disruptionsAllowed == 0' >/dev/null
}

controller_container_completed() {
  local pod="$1"
  kubectl --context "${HUB_CONTEXT}" -n "${NAMESPACE}" get "pod/${pod}" -o json | jq -e '
    .metadata.deletionTimestamp != null and
    .status.containerStatuses[0].ready == false and
    .status.containerStatuses[0].restartCount == 0 and
    .status.containerStatuses[0].state.terminated.reason == "Completed" and
    .status.containerStatuses[0].state.terminated.exitCode == 0 and
    (.status.containerStatuses[0].state.terminated.signal // 0) == 0' >/dev/null
}

planned_shutdown_log_is_ordered() {
  local log_file="$1" signal_line workers_line wait_line
  signal_line="$(grep -n -m1 -F \
    'Shutdown signal received, waiting for all workers to finish' "${log_file}" |
    cut -d: -f1)" || return 1
  workers_line="$(grep -n -m1 -F 'All workers finished' "${log_file}" |
    cut -d: -f1)" || return 1
  wait_line="$(grep -n -m1 -F \
    'Wait completed, proceeding to shutdown the manager' "${log_file}" |
    cut -d: -f1)" || return 1
  [ "${signal_line}" -lt "${workers_line}" ] && [ "${workers_line}" -lt "${wait_line}" ] &&
    ! grep -E -i 'panic|fatal|graceful shutdown timeout' "${log_file}" >/dev/null
}

planned_shutdown_precedes_release() {
  local watch_file="$1" log_file="$2" release_time workers_time wait_time
  local release_ns workers_ns wait_ns
  release_time="$(jq -r '
    select(.type == "MODIFIED") |
    select((.object.spec.holderIdentity // "") == "") |
    select((.object.spec.leaseDurationSeconds // 0) == 1) |
    .object.spec.renewTime' "${watch_file}" | head -1)"
  workers_time="$(jq -Rr '
    fromjson? | select(.msg == "All workers finished") | .time' \
    "${log_file}" | head -1)"
  wait_time="$(jq -Rr '
    fromjson? | select(.msg == "Wait completed, proceeding to shutdown the manager") | .time' \
    "${log_file}" | head -1)"
  [ -n "${release_time}" ] && [ -n "${workers_time}" ] && [ -n "${wait_time}" ] || return 1
  release_ns="$(date --date="${release_time}" +%s%N)" || return 1
  workers_ns="$(date --date="${workers_time}" +%s%N)" || return 1
  wait_ns="$(date --date="${wait_time}" +%s%N)" || return 1
  [ "${workers_ns}" -le "${wait_ns}" ] && [ "${wait_ns}" -le "${release_ns}" ]
}

forced_restart_is_isolated() {
  local killed_pod="$1"
  kubectl --context "${HUB_CONTEXT}" -n "${NAMESPACE}" \
    get pods -l "${CONTROLLER_LABEL}" -o json | jq -e --arg killed "${killed_pod}" '
      ([.items[] | select(.metadata.deletionTimestamp == null)] | length) == 2 and
      all(.items[] | select(.metadata.deletionTimestamp == null);
        if .metadata.name == $killed then
          .status.containerStatuses[0].restartCount == 1 and
          .status.containerStatuses[0].lastState.terminated.exitCode == 137 and
          .status.containerStatuses[0].lastState.terminated.reason == "Error"
        else
          .status.containerStatuses[0].restartCount == 0
        end)' >/dev/null
}

profile_secrets_match_generation() {
  local generation="$1"
  kubectl --context "${HUB_CONTEXT}" -n "${NAMESPACE}" get secrets -o json \
    >"${WORK_DIR}/secrets.json" || return 1
  # The generated Secret shape is pinned by the controller unit tests. What only a
  # real handoff can show is that every profile still has exactly one owned Secret
  # carrying the latest generation, so no write was dropped as leadership moved.
  jq -n -e --arg generation "${generation}" --argjson count "${PROFILE_COUNT}" \
    --slurpfile profiles "${WORK_DIR}/profiles.json" \
    --slurpfile secrets "${WORK_DIR}/secrets.json" '
      $profiles[0] as $uids |
      ($secrets[0].items |
        map(select($uids[.metadata.ownerReferences[0].name // ""] != null))) as $owned |
      ($owned | length) == $count and
      all($owned[];
        .metadata.ownerReferences[0].name as $name |
        .metadata.ownerReferences[0].uid == $uids[$name] and
        (.data.server | @base64d) ==
          ("https://" + $generation + "-" + $name + ".example.com"))' \
    >/dev/null
}

create_profiles() {
  local i name
  log "creating ${PROFILE_COUNT} ClusterProfiles"
  {
    for i in $(seq -w 1 "${PROFILE_COUNT}"); do
      name="ha-profile-${i}"
      printf '%s\n' \
        '---' \
        'apiVersion: multicluster.x-k8s.io/v1alpha1' \
        'kind: ClusterProfile' \
        'metadata:' \
        "  name: ${name}" \
        "  namespace: ${NAMESPACE}" \
        'spec:' \
        '  clusterManager:' \
        '    name: e2e-ha' \
        "  displayName: ${name}"
    done
  } | kubectl --context "${HUB_CONTEXT}" create -f - >/dev/null
  # The name/UID map never changes afterwards, so the convergence poll reads this
  # snapshot instead of re-listing every ClusterProfile on each attempt.
  kubectl --context "${HUB_CONTEXT}" -n "${NAMESPACE}" get clusterprofiles -o json | jq '
    [.items[] | select(.metadata.name | startswith("ha-profile-"))] |
    map({key: .metadata.name, value: .metadata.uid}) | from_entries' \
    >"${WORK_DIR}/profiles.json"
}

patch_profile_status() {
  local name="$1" generation="${PATCH_GENERATION}" payload
  payload="$(printf '{"status":{"accessProviders":[{"name":"argo-cd-builtin-gcp","cluster":{"server":"https://%s-%s.example.com"}}]}}' "${generation}" "${name}")"
  kubectl --context "${HUB_CONTEXT}" -n "${NAMESPACE}" \
    patch clusterprofile "${name}" --subresource=status --type=merge -p "${payload}" >/dev/null
}

export -f patch_profile_status
export HUB_CONTEXT NAMESPACE

patch_all_profiles() {
  local generation="$1"
  export PATCH_GENERATION="${generation}"
  seq -w 1 "${PROFILE_COUNT}" | sed 's/^/ha-profile-/' | \
    xargs -P 32 -n 1 bash -c "patch_profile_status \"\$1\"" _
}

start_profile_patch() {
  local generation="$1"
  PATCH_LOG="${WORK_DIR}/patch-${generation}.log"
  patch_all_profiles "${generation}" >"${PATCH_LOG}" 2>&1 &
  PATCH_PID=$!
}

wait_for_profile_patch() {
  local pid="${PATCH_PID}"
  if ! wait "${pid}"; then
    echo "ClusterProfile status patch failed" >&2
    cat "${PATCH_LOG}" >&2
    return 1
  fi
  PATCH_PID=""
}

dump_backlog_diagnostics() {
  local generation="$1" phase="$2" holder pod patch_state
  echo "backlog diagnostics for ${phase}:" >&2
  if [ -s "${BACKLOG_STATE_FILE}" ]; then
    cat "${BACKLOG_STATE_FILE}" >&2
  else
    echo "no leader queue sample was collected" >&2
  fi

  patch_state="running"
  if ! kill -0 "${PATCH_PID}" >/dev/null 2>&1; then
    if wait "${PATCH_PID}"; then
      patch_state="completed successfully"
    else
      patch_state="failed with exit code $?"
    fi
    PATCH_PID=""
  fi
  echo "ClusterProfile status patch: ${patch_state}" >&2
  if [ -s "${PATCH_LOG}" ]; then
    cat "${PATCH_LOG}" >&2
  else
    echo "ClusterProfile status patch log is empty" >&2
  fi

  kubectl --context "${HUB_CONTEXT}" -n "${NAMESPACE}" \
    get clusterprofiles -o json | jq -c --arg generation "${generation}" '
      [.items[] | select(.metadata.name | startswith("ha-profile-"))] as $profiles |
      {
        total: ($profiles | length),
        matchingGeneration: ([$profiles[] |
          .metadata.name as $name |
          select(any(.status.accessProviders[]?;
            .cluster.server == ("https://" + $generation + "-" + $name + ".example.com"))
        ] | length)
      }' >&2 || true

  kubectl --context "${HUB_CONTEXT}" -n "${NAMESPACE}" get secrets -o json \
    >"${WORK_DIR}/backlog-secrets.json" || true
  if [ -s "${WORK_DIR}/backlog-secrets.json" ]; then
    jq -n -c --arg generation "${generation}" \
      --slurpfile profiles "${WORK_DIR}/profiles.json" \
      --slurpfile secrets "${WORK_DIR}/backlog-secrets.json" '
        $profiles[0] as $uids |
        ($secrets[0].items |
          map(select($uids[.metadata.ownerReferences[0].name // ""] != null))) as $owned |
        {
          owned: ($owned | length),
          matchingGeneration: ([$owned[] |
            .metadata.ownerReferences[0].name as $name |
            select((.data.server // "" | @base64d) ==
              ("https://" + $generation + "-" + $name + ".example.com"))
          ] | length)
        }' >&2 || true
  fi

  holder="$(lease_holder_identity 2>/dev/null || true)"
  pod="${holder%%_*}"
  if [ -n "${pod}" ]; then
    pod_metrics "${pod}" | grep -E \
      '^(workqueue_depth|controller_runtime_active_workers|controller_runtime_reconcile_(total|errors_total))({| )' \
      >&2 || true
  fi
}

# Starts a credential backlog for one disruption scenario and proves the leader
# is busy with clean telemetry before the fault is injected.
begin_backlog() {
  local generation="$1" phase="$2"
  log "creating a ${generation} credential backlog before ${phase}"
  start_profile_patch "${generation}"
  if ! retry_until 30 "${generation} workqueue backlog" leader_queue_is_backlogged; then
    echo "unable to observe a live workqueue backlog before ${phase}" >&2
    dump_backlog_diagnostics "${generation}" "${phase}"
    exit 1
  fi
  if ! terminal_metrics_are_zero || ! leader_monitor_is_healthy; then
    echo "telemetry was not clean immediately before ${phase}" >&2
    exit 1
  fi
}

assert_leader_and_telemetry_clean() {
  local phase="$1"
  if ! leader_monitor_is_healthy ||
    ! retry_until 30 "zero terminal telemetry ${phase}" terminal_metrics_are_zero; then
    echo "leader uniqueness or terminal telemetry failed ${phase}" >&2
    exit 1
  fi
}

evict_pod() {
  local pod="$1"
  kubectl --context "${HUB_CONTEXT}" create \
    --raw "/api/v1/namespaces/${NAMESPACE}/pods/${pod}/eviction" -f - <<EOF >/dev/null
{
  "apiVersion": "policy/v1",
  "kind": "Eviction",
  "metadata": {
    "name": "${pod}",
    "namespace": "${NAMESPACE}"
  }
}
EOF
}

role_resource_verbs() {
  local resource="$1" render="$2"
  awk -v resource="${resource}" '
    $0 == "      - " resource { found = 1 }
    found && $0 == "    verbs:" { verbs = 1; next }
    verbs && /^      - / { sub(/^      - /, ""); print; next }
    verbs { exit }
  ' "${render}" | sort | paste -sd, -
}

validate_static_shapes() {
  local default_render ha_render_127
  local pdb_disabled_render pdb_max_zero_render pdb_min_available_render pdb_percent_render
  local remote_render remote_local_role kustomize_render
  local remote_clusterprofile_verbs remote_secret_verbs
  local invalid_pdb_case
  local -a invalid_pdb_args
  log "verifying safe Helm and Kustomize deployment shapes"
  default_render="${WORK_DIR}/helm-default.yaml"
  ha_render_127="${WORK_DIR}/helm-ha-1.27.yaml"
  pdb_disabled_render="${WORK_DIR}/helm-pdb-disabled.yaml"
  pdb_max_zero_render="${WORK_DIR}/helm-pdb-max-zero.yaml"
  pdb_min_available_render="${WORK_DIR}/helm-pdb-min-available.yaml"
  pdb_percent_render="${WORK_DIR}/helm-pdb-percent.yaml"
  remote_render="${WORK_DIR}/helm-remote.yaml"
  remote_local_role="${WORK_DIR}/helm-remote-local-role.yaml"
  kustomize_render="${WORK_DIR}/kustomize-default.yaml"
  helm template cpia "${REPO_ROOT}/charts/argocd-clusterprofile-controller" \
    --namespace "${NAMESPACE}" >"${default_render}"
  helm template cpia "${REPO_ROOT}/charts/argocd-clusterprofile-controller" \
    --namespace "${NAMESPACE}" \
    --set podDisruptionBudget.enabled=false >"${pdb_disabled_render}"
  helm template cpia "${REPO_ROOT}/charts/argocd-clusterprofile-controller" \
    --namespace "${NAMESPACE}" \
    --set podDisruptionBudget.maxUnavailable=0 \
    --show-only templates/poddisruptionbudget.yaml >"${pdb_max_zero_render}"
  helm template cpia "${REPO_ROOT}/charts/argocd-clusterprofile-controller" \
    --namespace "${NAMESPACE}" \
    --set-json podDisruptionBudget.maxUnavailable=null \
    --set podDisruptionBudget.minAvailable=1 \
    --set podDisruptionBudget.unhealthyPodEvictionPolicy=AlwaysAllow \
    --set-string podDisruptionBudget.labels.test-label=custom \
    --set-string podDisruptionBudget.annotations.test-annotation=custom \
    --show-only templates/poddisruptionbudget.yaml >"${pdb_min_available_render}"
  helm template cpia "${REPO_ROOT}/charts/argocd-clusterprofile-controller" \
    --namespace "${NAMESPACE}" \
    --set-string podDisruptionBudget.maxUnavailable=50% \
    --show-only templates/poddisruptionbudget.yaml >"${pdb_percent_render}"
  helm template cpia "${REPO_ROOT}/charts/argocd-clusterprofile-controller" \
    --namespace "${NAMESPACE}" \
    --set-json 'controller.clusterProfileNamespaces=["team-a"]' \
    --show-only templates/role.yaml \
    --show-only templates/rolebinding.yaml >"${remote_render}"
  kubectl kustomize "${REPO_ROOT}/artifacts/manifests" >"${kustomize_render}"
  if [ "$(grep -c '^kind: Role$' "${remote_render}")" -ne 2 ] ||
    [ "$(grep -c '^kind: RoleBinding$' "${remote_render}")" -ne 2 ] ||
    [ "$(grep -cE '^  namespace: argocd$' "${remote_render}")" -ne 2 ] ||
    [ "$(grep -cE '^  namespace: team-a$' "${remote_render}")" -ne 2 ]; then
    echo "remote-only Helm shape does not isolate release and watched namespace Roles" >&2
    cat "${remote_render}" >&2
    exit 1
  fi
  remote_clusterprofile_verbs="$(role_resource_verbs clusterprofiles "${remote_render}")"
  remote_secret_verbs="$(role_resource_verbs secrets "${remote_render}")"
  if [ "${remote_clusterprofile_verbs}" != "get,list,watch" ] ||
    [ "${remote_secret_verbs}" != "create,delete,get,list,patch,update,watch" ]; then
    echo "remote-only Helm Role has unexpected verbs: ClusterProfile=${remote_clusterprofile_verbs}, Secret=${remote_secret_verbs}" >&2
    cat "${remote_render}" >&2
    exit 1
  fi
  awk 'BEGIN { RS = "---" } index($0, "kind: Role\n") && /namespace: argocd/ { print }' \
    "${remote_render}" >"${remote_local_role}"
  if ! grep -F '    - events' "${remote_local_role}" >/dev/null ||
    ! grep -F '    - leases' "${remote_local_role}" >/dev/null ||
    grep -E '    - (clusterprofiles|secrets)' "${remote_local_role}" >/dev/null; then
    echo "remote-only Helm shape does not isolate release-namespace leader permissions" >&2
    cat "${remote_render}" >&2
    exit 1
  fi
  if helm template cpia "${REPO_ROOT}/charts/argocd-clusterprofile-controller" \
    --namespace "${NAMESPACE}" \
    --set replicaCount=2 \
    --set-json 'controller.args=["--enable-leader-election=false"]' \
    >"${WORK_DIR}/helm-overridden-election.yaml" 2>"${WORK_DIR}/helm-overridden-election.err"; then
    echo "Helm allowed controller.args to override leader election" >&2
    exit 1
  fi
  if ! grep -F 'controller.args must not override' \
    "${WORK_DIR}/helm-overridden-election.err" >/dev/null; then
    echo "Helm rejected the managed-flag override without the expected diagnostic" >&2
    cat "${WORK_DIR}/helm-overridden-election.err" >&2
    exit 1
  fi
  if helm template cpia "${REPO_ROOT}/charts/argocd-clusterprofile-controller" \
    --namespace "${NAMESPACE}" \
    --kube-version 1.26.15 \
    --set replicaCount=2 \
    >"${WORK_DIR}/helm-unsupported.yaml" 2>"${WORK_DIR}/helm-unsupported.err"; then
    echo "Helm accepted unsupported Kubernetes 1.26" >&2
    exit 1
  fi
  if ! grep -F '>=1.27.0-0' "${WORK_DIR}/helm-unsupported.err" >/dev/null; then
    echo "Helm rejected Kubernetes 1.26 without the expected kubeVersion diagnostic" >&2
    cat "${WORK_DIR}/helm-unsupported.err" >&2
    exit 1
  fi
  helm template cpia "${REPO_ROOT}/charts/argocd-clusterprofile-controller" \
    --namespace "${NAMESPACE}" \
    --kube-version 1.27.0 \
    --set replicaCount=2 >"${ha_render_127}"
  if ! grep -F -- '- --enable-leader-election=true' "${default_render}" >/dev/null ||
    grep -F 'ARGOCD_CLUSTERPROFILE_CONTROLLER_ENABLE_LEADER_ELECTION' "${default_render}" >/dev/null; then
    echo "default Helm shape does not manage leader election directly" >&2
    exit 1
  fi
  for expected in 'type: RollingUpdate' 'maxUnavailable: 0' 'maxSurge: 1'; do
    if ! grep -F "${expected}" "${default_render}" >/dev/null; then
      echo "default Helm shape is missing ${expected}" >&2
      exit 1
    fi
  done
  if ! grep -F 'kind: PodDisruptionBudget' "${default_render}" >/dev/null ||
    ! grep -F '  maxUnavailable: 1' "${default_render}" >/dev/null; then
    echo "default Helm shape does not include the configured PodDisruptionBudget" >&2
    exit 1
  fi
  if grep -F 'kind: PodDisruptionBudget' "${pdb_disabled_render}" >/dev/null; then
    echo "disabled PodDisruptionBudget unexpectedly rendered" >&2
    exit 1
  fi
  if ! grep -F '  maxUnavailable: 0' "${pdb_max_zero_render}" >/dev/null ||
    ! grep -F '  maxUnavailable: "50%"' "${pdb_percent_render}" >/dev/null; then
    echo "PodDisruptionBudget maxUnavailable overrides did not preserve their values" >&2
    exit 1
  fi
  for expected in \
    '  minAvailable: 1' \
    '  unhealthyPodEvictionPolicy: AlwaysAllow' \
    '    test-label: custom' \
    '    test-annotation: custom'; do
    if ! grep -F "${expected}" "${pdb_min_available_render}" >/dev/null; then
      echo "custom PodDisruptionBudget shape is missing ${expected}" >&2
      exit 1
    fi
  done
  for invalid_pdb_case in both-set both-null negative invalid-percentage invalid-policy; do
    case "${invalid_pdb_case}" in
      both-set)
        invalid_pdb_args=(--set podDisruptionBudget.minAvailable=1)
        ;;
      both-null)
        invalid_pdb_args=(
          --set-json podDisruptionBudget.minAvailable=null
          --set-json podDisruptionBudget.maxUnavailable=null
        )
        ;;
      negative)
        invalid_pdb_args=(--set podDisruptionBudget.maxUnavailable=-1)
        ;;
      invalid-percentage)
        invalid_pdb_args=(--set-string podDisruptionBudget.maxUnavailable=101%)
        ;;
      invalid-policy)
        invalid_pdb_args=(--set podDisruptionBudget.unhealthyPodEvictionPolicy=Invalid)
        ;;
    esac
    if helm template cpia "${REPO_ROOT}/charts/argocd-clusterprofile-controller" \
      --namespace "${NAMESPACE}" "${invalid_pdb_args[@]}" \
      >"${WORK_DIR}/helm-pdb-${invalid_pdb_case}.out" 2>&1; then
      echo "Helm accepted invalid PodDisruptionBudget values: ${invalid_pdb_case}" >&2
      exit 1
    fi
  done
  if grep -F 'kind: PodDisruptionBudget' "${kustomize_render}" >/dev/null; then
    echo "single-replica Kustomize base unexpectedly includes a PodDisruptionBudget" >&2
    exit 1
  fi
  if ! grep -F 'matchLabelKeys:' "${ha_render_127}" >/dev/null; then
    echo "Kubernetes 1.27 render does not scope spreading to its rollout revision" >&2
    exit 1
  fi
  for expected in \
    '--enable-leader-election=true' \
    'type: RollingUpdate' \
    'maxUnavailable: 0' \
    'maxSurge: 1'; do
    if ! grep -F -- "${expected}" "${kustomize_render}" >/dev/null; then
      echo "default Kustomize shape is missing ${expected}" >&2
      exit 1
    fi
  done
  if grep -F 'ARGOCD_CLUSTERPROFILE_CONTROLLER_ENABLE_LEADER_ELECTION' \
    "${kustomize_render}" >/dev/null; then
    echo "default Kustomize shape still permits a ConfigMap leader-election override" >&2
    exit 1
  fi
}

test_controller_config_is_current() {
  kubectl --context "${HUB_CONTEXT}" -n "${NAMESPACE}" \
    get configmap argocd-cmd-params-cm -o json | jq -e \
      --arg qps "${CONTROLLER_CLIENT_QPS}" \
      --arg burst "${CONTROLLER_CLIENT_BURST}" '
        .data["clusterprofilecontroller.k8s.client.qps"] == $qps and
        .data["clusterprofilecontroller.k8s.client.burst"] == $burst' >/dev/null
}

for command in kubectl helm jq docker awk grep sed xargs timeout; do
  require_command "${command}"
done

validate_static_shapes

log "reusing the live e2e hub for HA fault scenarios"
if ! kubectl --context "${HUB_CONTEXT}" get namespace "${NAMESPACE}" >/dev/null; then
  echo "parent e2e hub or namespace is unavailable: ${HUB_CONTEXT}/${NAMESPACE}" >&2
  exit 1
fi
HA_NODE_SHAPE="$(
  kubectl --context "${HUB_CONTEXT}" get nodes -o json | jq -c '{
    total: (.items | length),
    workers: ([.items[] |
      select(.metadata.labels["node-role.kubernetes.io/control-plane"] == null) |
      select(.metadata.labels["e2e.argoproj.io/ha-worker"] == "true")] | length)
  }'
)"
if ! jq -e '.total == 3 and .workers == 2' <<<"${HA_NODE_SHAPE}" >/dev/null; then
  echo "parent e2e hub does not have the required three-node HA shape: ${HA_NODE_SHAPE}" >&2
  exit 1
fi
if ! test_controller_config_is_current; then
  echo "HA test controller client limits were not preserved by the live e2e phase" >&2
  exit 1
fi
if ! retry_until 120 "two ready controller replicas" deployment_is_ready_with_replicas 2; then
  echo "HA Deployment did not become ready" >&2
  exit 1
fi
if ! retry_until 60 "initial controller spread" controllers_are_spread; then
  echo "controller replicas are not spread across worker nodes" >&2
  exit 1
fi
if ! retry_until 60 "PDB status" pdb_allows_one_disruption; then
  echo "PDB did not allow exactly one disruption" >&2
  exit 1
fi
if ! retry_until 60 "leader Lease" lease_has_live_holder; then
  echo "leader election did not acquire the controller Lease" >&2
  exit 1
fi
if ! retry_until 60 "per-Pod leader metrics" exactly_one_metric_leader; then
  echo "per-Pod metrics did not report exactly one leader" >&2
  exit 1
fi
if ! retry_until 30 "zero terminal controller telemetry" terminal_metrics_are_zero; then
  echo "controller reported panic, terminal-error, or timeout telemetry at startup" >&2
  exit 1
fi
start_leader_monitor

DEPLOYMENT_SHAPE="$(
  kubectl --context "${HUB_CONTEXT}" -n "${NAMESPACE}" \
    get deployment argocd-clusterprofile-controller -o json | jq -c '{
      replicas: .spec.replicas,
      strategy: .spec.strategy,
      grace: .spec.template.spec.terminationGracePeriodSeconds,
      spread: .spec.template.spec.topologySpreadConstraints
    }'
)"
if ! jq -e '
  .replicas == 2 and
  .strategy.type == "RollingUpdate" and
  .strategy.rollingUpdate.maxUnavailable == 0 and
  .strategy.rollingUpdate.maxSurge == 1 and
  .grace == 30 and
  (.spread | length) == 1 and
  .spread[0].topologyKey == "kubernetes.io/hostname" and
  .spread[0].whenUnsatisfiable == "DoNotSchedule" and
  .spread[0].matchLabelKeys == ["pod-template-hash"]' <<<"${DEPLOYMENT_SHAPE}" >/dev/null; then
  echo "live HA Deployment has an unsafe shape: ${DEPLOYMENT_SHAPE}" >&2
  exit 1
fi
log "verified two spread, ready replicas, one leader, and PDB protection"

create_profiles
begin_backlog v1 "a voluntary leader eviction"

PLANNED_OLD_IDENTITY="$(lease_holder_identity)"
PLANNED_OLD_POD="${PLANNED_OLD_IDENTITY%%_*}"
PLANNED_OLD_NODE="$(
  kubectl --context "${HUB_CONTEXT}" -n "${NAMESPACE}" \
    get "pod/${PLANNED_OLD_POD}" -o jsonpath='{.spec.nodeName}'
)"
HELD_POD="${PLANNED_OLD_POD}"
kubectl --context "${HUB_CONTEXT}" -n "${NAMESPACE}" \
  patch "pod/${PLANNED_OLD_POD}" --type=merge \
  -p '{"metadata":{"finalizers":["e2e.argoproj.io/observe-ha-termination"]}}' >/dev/null
kubectl --context "${HUB_CONTEXT}" cordon "${PLANNED_OLD_NODE}" >/dev/null
PLANNED_LOG_FILE="${WORK_DIR}/planned-old-leader.log"
kubectl --context "${HUB_CONTEXT}" -n "${NAMESPACE}" \
  logs -f "pod/${PLANNED_OLD_POD}" >"${PLANNED_LOG_FILE}" 2>&1 &
PLANNED_LOG_PID=$!
sleep 0.2
if ! kill -0 "${PLANNED_LOG_PID}" >/dev/null 2>&1; then
  wait "${PLANNED_LOG_PID}" 2>/dev/null || true
  PLANNED_LOG_PID=""
  echo "unable to follow the old leader logs before eviction" >&2
  cat "${PLANNED_LOG_FILE}" >&2 || true
  exit 1
fi
PLANNED_LEASE_WATCH="${WORK_DIR}/planned-lease-watch.json"
start_lease_watch "${PLANNED_LEASE_WATCH}"
if [ "$(lease_holder_identity)" != "${PLANNED_OLD_IDENTITY}" ] ||
  ! leader_queue_is_backlogged; then
  echo "planned fault target changed or lost its backlog before eviction" >&2
  exit 1
fi
PLANNED_STARTED_NS="$(date +%s%N)"
evict_pod "${PLANNED_OLD_POD}"

PLANNED_NEW_IDENTITY="$(wait_for_new_lease_holder 50 "${PLANNED_OLD_IDENTITY}")"
PLANNED_HANDOFF_MS="$((($(date +%s%N) - PLANNED_STARTED_NS) / 1000000))"
sleep 0.5
stop_lease_watch
if [ -z "${PLANNED_NEW_IDENTITY}" ] || [ "${PLANNED_HANDOFF_MS}" -ge 5000 ]; then
  echo "voluntary leader handoff exceeded 5s: ${PLANNED_HANDOFF_MS}ms" >&2
  exit 1
fi
if ! lease_watch_proves_planned_release "${PLANNED_LEASE_WATCH}" "${PLANNED_NEW_IDENTITY}"; then
  echo "Lease watch did not observe a voluntary release before the new leader" >&2
  cat "${PLANNED_LEASE_WATCH}" >&2
  exit 1
fi
if ! retry_until 30 "clean old leader exit" controller_container_completed "${PLANNED_OLD_POD}"; then
  echo "evicted leader did not exit cleanly" >&2
  exit 1
fi
sleep 0.5
stop_background "${PLANNED_LOG_PID}"
PLANNED_LOG_PID=""
if ! planned_shutdown_log_is_ordered "${PLANNED_LOG_FILE}" ||
  ! planned_shutdown_precedes_release "${PLANNED_LEASE_WATCH}" "${PLANNED_LOG_FILE}"; then
  echo "old leader logs do not prove ordered worker drain" >&2
  cat "${PLANNED_LOG_FILE}" >&2 || true
  exit 1
fi
if ! retry_until 60 "PDB to block the last healthy replica" pdb_blocks_disruption; then
  echo "PDB did not close after one controller disruption" >&2
  exit 1
fi
PLANNED_NEW_POD="${PLANNED_NEW_IDENTITY%%_*}"
if evict_pod "${PLANNED_NEW_POD}" >"${WORK_DIR}/blocked-eviction.out" 2>&1; then
  echo "PDB allowed eviction of the final healthy controller" >&2
  exit 1
fi
if ! grep -E 'TooManyRequests|disruption budget|429' "${WORK_DIR}/blocked-eviction.out" >/dev/null; then
  echo "final controller eviction failed for an unexpected reason" >&2
  cat "${WORK_DIR}/blocked-eviction.out" >&2
  exit 1
fi
wait_for_profile_patch
if ! retry_until 300 "all v1 credentials after planned handoff" profile_secrets_match_generation v1; then
  echo "v1 credentials did not converge after voluntary handoff" >&2
  exit 1
fi
kubectl --context "${HUB_CONTEXT}" -n "${NAMESPACE}" \
  patch "pod/${PLANNED_OLD_POD}" --type=json \
  -p='[{"op":"remove","path":"/metadata/finalizers"}]' >/dev/null
HELD_POD=""
kubectl --context "${HUB_CONTEXT}" uncordon "${PLANNED_OLD_NODE}" >/dev/null
if ! retry_until 120 "replacement controller" deployment_is_ready_with_replicas 2 ||
  ! retry_until 60 "replacement spread" controllers_are_spread ||
  ! retry_until 60 "restored PDB allowance" pdb_allows_one_disruption ||
  ! retry_until 60 "one leader after voluntary handoff" exactly_one_metric_leader; then
  echo "HA shape did not recover after voluntary disruption" >&2
  exit 1
fi
assert_leader_and_telemetry_clean "after voluntary handoff"
log "verified ${PLANNED_HANDOFF_MS}ms planned handoff under backlog and PDB enforcement"

begin_backlog v2 "a forced leader kill"
FORCED_OLD_IDENTITY="$(lease_holder_identity)"
FORCED_OLD_POD="${FORCED_OLD_IDENTITY%%_*}"
FORCED_NODE="$(
  kubectl --context "${HUB_CONTEXT}" -n "${NAMESPACE}" \
    get "pod/${FORCED_OLD_POD}" -o jsonpath='{.spec.nodeName}'
)"
FORCED_CONTAINER_ID="$(
  kubectl --context "${HUB_CONTEXT}" -n "${NAMESPACE}" \
    get "pod/${FORCED_OLD_POD}" -o jsonpath='{.status.containerStatuses[0].containerID}' |
    sed 's#containerd://##'
)"
FORCED_LEASE_WATCH="${WORK_DIR}/forced-lease-watch.json"
start_lease_watch "${FORCED_LEASE_WATCH}"
if [ "$(lease_holder_identity)" != "${FORCED_OLD_IDENTITY}" ] ||
  ! leader_queue_is_backlogged; then
  echo "forced fault target changed or lost its backlog before SIGKILL" >&2
  exit 1
fi
FORCED_STARTED_NS="$(date +%s%N)"
docker exec "${FORCED_NODE}" crictl stop --timeout 0 "${FORCED_CONTAINER_ID}" >/dev/null
FORCED_NEW_IDENTITY="$(wait_for_new_lease_holder 250 "${FORCED_OLD_IDENTITY}")"
FORCED_HANDOFF_MS="$((($(date +%s%N) - FORCED_STARTED_NS) / 1000000))"
sleep 0.5
stop_lease_watch
if [ -z "${FORCED_NEW_IDENTITY}" ] || [ "${FORCED_HANDOFF_MS}" -ge 25000 ]; then
  echo "forced leader handoff exceeded the 25s lease bound: ${FORCED_HANDOFF_MS}ms" >&2
  exit 1
fi
if ! lease_watch_proves_forced_expiry "${FORCED_LEASE_WATCH}" "${FORCED_NEW_IDENTITY}"; then
  echo "Lease watch did not prove a timeout-only forced handoff" >&2
  cat "${FORCED_LEASE_WATCH}" >&2
  exit 1
fi
wait_for_profile_patch
if ! retry_until 300 "all v2 credentials after forced handoff" profile_secrets_match_generation v2; then
  echo "v2 credentials did not converge after forced handoff" >&2
  exit 1
fi
if ! retry_until 120 "two replicas after forced handoff" deployment_is_ready_with_replicas 2 ||
  ! retry_until 60 "one leader after forced handoff" exactly_one_metric_leader; then
  echo "HA shape did not recover after forced failure" >&2
  exit 1
fi
if ! forced_restart_is_isolated "${FORCED_OLD_POD}"; then
  echo "forced fixture did not produce one isolated exit-137 restart" >&2
  kubectl --context "${HUB_CONTEXT}" -n "${NAMESPACE}" \
    get pods -l "${CONTROLLER_LABEL}" -o json | jq '[.items[] | {
      name: .metadata.name,
      deleting: .metadata.deletionTimestamp,
      status: .status.containerStatuses[0]
    }]' >&2
  exit 1
fi
assert_leader_and_telemetry_clean "after forced handoff"
log "verified ${FORCED_HANDOFF_MS}ms lease-expiry handoff and exact backlog recovery"

begin_backlog v3 "a zero-unavailable rolling update"
ROLLING_GENERATION="$(date +%s%N)"
kubectl --context "${HUB_CONTEXT}" -n "${NAMESPACE}" patch deployment argocd-clusterprofile-controller \
  --type=merge -p "{\"spec\":{\"template\":{\"metadata\":{\"annotations\":{\"e2e.argoproj.io/rollout\":\"${ROLLING_GENERATION}\"}}}}}" >/dev/null
ROLLOUT_COMPLETE=0
for i in $(seq 1 600); do
  ROLLOUT_STATE="$(
    kubectl --context "${HUB_CONTEXT}" -n "${NAMESPACE}" \
      get deployment argocd-clusterprofile-controller -o json | jq -c '{
        generation: .metadata.generation,
        observed: (.status.observedGeneration // 0),
        replicas: (.status.replicas // 0),
        updated: (.status.updatedReplicas // 0),
        ready: (.status.readyReplicas // 0),
        available: (.status.availableReplicas // 0),
        unavailable: (.status.unavailableReplicas // 0)
      }'
  )"
  # Deployment surge accounting excludes Pods that are already terminating;
  # their API objects may remain briefly while graceful shutdown completes.
  ROLLOUT_PODS="$(
    kubectl --context "${HUB_CONTEXT}" -n "${NAMESPACE}" \
      get pods -l "${CONTROLLER_LABEL}" -o json | jq '[
        .items[] | select(.metadata.deletionTimestamp == null)
      ] | length'
  )"
  if [ "$(jq -r '.available' <<<"${ROLLOUT_STATE}")" -lt 2 ] || [ "${ROLLOUT_PODS}" -gt 3 ]; then
    echo "rolling update violated availability/surge bounds: ${ROLLOUT_STATE}, pods=${ROLLOUT_PODS}" >&2
    exit 1
  fi
  if jq -e '
    .observed == .generation and
    .replicas == 2 and .updated == 2 and .ready == 2 and .available == 2 and .unavailable == 0' \
    <<<"${ROLLOUT_STATE}" >/dev/null; then
    ROLLOUT_COMPLETE=1
    break
  fi
  if [ $((i % 50)) -eq 0 ]; then
    log "waiting for zero-unavailable rollout (${i}/600)"
  fi
  sleep 0.1
done
if [ "${ROLLOUT_COMPLETE}" != "1" ]; then
  echo "rolling update did not complete" >&2
  exit 1
fi
if ! retry_until 60 "new revision spread" controllers_are_spread ||
  ! retry_until 60 "one leader after rollout" exactly_one_metric_leader ||
  ! retry_until 60 "PDB after rollout" pdb_allows_one_disruption; then
  echo "HA invariants did not recover after rollout" >&2
  exit 1
fi
assert_leader_and_telemetry_clean "after rollout"
wait_for_profile_patch
if ! retry_until 300 "all v3 credentials after rolling update" profile_secrets_match_generation v3; then
  echo "v3 credentials did not converge exactly through the rolling update" >&2
  exit 1
fi

QUIET_BEFORE=0
for pod in $(ready_controller_pods); do
  ERRORS="$(pod_metric_sum "${pod}" controller_runtime_reconcile_errors_total)"
  QUIET_BEFORE="$(float_add "${QUIET_BEFORE}" "${ERRORS}")"
done
sleep 10
QUIET_AFTER=0
for pod in $(ready_controller_pods); do
  POD_METRICS="$(pod_metrics "${pod}")"
  ERRORS="$(metric_sum controller_runtime_reconcile_errors_total <<<"${POD_METRICS}")"
  ACTIVE="$(metric_sum controller_runtime_active_workers <<<"${POD_METRICS}")"
  DEPTH="$(metric_sum workqueue_depth <<<"${POD_METRICS}")"
  if ! awk -v active="${ACTIVE}" -v depth="${DEPTH}" 'BEGIN {exit !(active == 0 && depth == 0)}'; then
    echo "controller did not become idle after HA recovery" >&2
    exit 1
  fi
  QUIET_AFTER="$(float_add "${QUIET_AFTER}" "${ERRORS}")"
done
if ! awk -v before="${QUIET_BEFORE}" -v after="${QUIET_AFTER}" 'BEGIN {exit !(before == after)}'; then
  echo "reconcile errors continued increasing after HA convergence" >&2
  exit 1
fi
if ! exactly_one_metric_leader; then
  echo "leader metrics lost uniqueness after the quiet window" >&2
  exit 1
fi
assert_leader_and_telemetry_clean "in the quiet window"
LEADER_MONITOR_SAMPLE_COUNT="$(wc -l <"${LEADER_MONITOR_SAMPLES}")"
if [ "${LEADER_MONITOR_SAMPLE_COUNT}" -lt 20 ]; then
  echo "continuous leader monitor collected too few samples: ${LEADER_MONITOR_SAMPLE_COUNT}" >&2
  exit 1
fi
if ! awk -F '\t' '$2 >= 2 {covered += 1} END {exit !(covered >= 20)}' \
  "${LEADER_MONITOR_SAMPLES}"; then
  echo "continuous leader monitor did not collect 20 stable two-Pod rounds" >&2
  exit 1
fi
stop_leader_monitor

FINAL_PODS="$(
  kubectl --context "${HUB_CONTEXT}" -n "${NAMESPACE}" \
    get pods -l "${CONTROLLER_LABEL}" -o json | jq -c '[.items[] | {
      name: .metadata.name,
      node: .spec.nodeName,
      ready: any(.status.conditions[]?; .type == "Ready" and .status == "True"),
      restarts: .status.containerStatuses[0].restartCount,
      imageID: .status.containerStatuses[0].imageID
    }]'
)"
if ! jq -e 'length == 2 and all(.[]; .ready and .restarts == 0 and (.imageID | length) > 0)' \
  <<<"${FINAL_PODS}" >/dev/null; then
  echo "final controller Pods are not clean: ${FINAL_PODS}" >&2
  exit 1
fi
log "verified zero-unavailable rollout and a 10s idle/error-stable window"
log "HA e2e passed: ${FINAL_PODS}"
