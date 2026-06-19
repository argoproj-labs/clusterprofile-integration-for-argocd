#!/usr/bin/env bash

set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname "$0")" >/dev/null 2>&1; pwd -P)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
CHART_ROOT="${REPO_ROOT}/install/helm-repo"

if ! command -v helm >/dev/null 2>&1; then
  echo "required command not found: helm" >&2
  exit 1
fi

charts=()
if [ "$#" -gt 0 ]; then
  charts=("$@")
else
  while IFS= read -r chart; do
    charts+=("${chart}")
  done < <(find "${CHART_ROOT}" -mindepth 2 -maxdepth 2 -type f -name Chart.yaml -exec dirname '{}' ';' | sort)
fi

if [ "${#charts[@]}" -eq 0 ]; then
  echo "no Helm charts found under ${CHART_ROOT}" >&2
  exit 1
fi

for chart in "${charts[@]}"; do
  if [ ! -f "${chart}/values.schema.json" ]; then
    echo "missing values.schema.json for ${chart}" >&2
    exit 1
  fi
  helm lint --strict "${chart}"
done
