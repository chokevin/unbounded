#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly script_dir
repo_root="$(cd "${script_dir}/../../../../.." && pwd)"
readonly repo_root
readonly plan="${repo_root}/deploy/signal/certification/whole-node-24h/certification-plan.yaml"
readonly argo_values="${repo_root}/deploy/signal/argo-values.yaml"
readonly argo_chart_version="2.0.12"
readonly namespace="project-signal"
readonly authority_namespace="project-signal-authority"
readonly test_name="project-signal-whole-node-burn-v1"
readonly baseline_test_name="project-signal-gpu-inventory-v1"
readonly baseline_profile_name="project-signal-gpu-baseline-8x-v1"
readonly suite_name="project-signal-whole-node-24h-v1"
readonly profile_name="project-signal-whole-node-24h-8x-v1"

usage() {
  cat <<'EOF'
Usage:
  project-signal-certification.sh preflight CONFIG
  project-signal-certification.sh install CONFIG
  project-signal-certification.sh smoke CONFIG
  project-signal-certification.sh run CONFIG
  project-signal-certification.sh start CONFIG
  project-signal-certification.sh status CONFIG
  project-signal-certification.sh uninstall CONFIG
EOF
}

fail() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || fail "required command not found: $1"
}

require_commands() {
  local commands=(kubectl yq jq go make git python3)
  if [[ -n "${REMOTE_HOST:-}" ]]; then
    commands+=(ssh)
  else
    commands+=(helm curl)
  fi

  for command_name in "${commands[@]}"; do
    require_command "${command_name}"
  done

  if [[ -n "${REMOTE_HOST:-}" ]]; then
    # shellcheck disable=SC2016
    remote_exec_retry bash -c '
      for command_name in kubectl helm jq curl awk; do
        command -v "${command_name}" >/dev/null ||
          { echo "required jumpbox command not found: ${command_name}" >&2; exit 1; }
      done
    '
  fi
}

resolve_config_path() {
  local path="$1"

  if [[ "${path}" == /* ]]; then
    printf '%s\n' "${path}"
  else
    printf '%s/%s\n' "$(cd "$(dirname "${CONFIG}")" && pwd)" "${path}"
  fi
}

remote_exec() {
  local remote_command

  printf -v remote_command '%q ' "$@"
  "${REMOTE_SSH[@]}" "${remote_command}"
}

remote_exec_retry() {
  local attempt status

  for attempt in 1 2 3; do
    if remote_exec "$@"; then
      return
    else
      status=$?
    fi

    if [[ "${status}" != "255" || "${attempt}" == "3" ]]; then
      return "${status}"
    fi

    sleep $((attempt * 5))
  done
}

kube() {
  if [[ -n "${REMOTE_HOST:-}" ]]; then
    remote_exec_retry kubectl --context "${CONTEXT}" "$@"
  else
    kubectl --context "${CONTEXT}" "$@"
  fi
}

kube_apply_file() {
  local path="$1"
  local target_namespace="${2:-}"
  local -a namespace_args=()

  if [[ -n "${target_namespace}" ]]; then
    namespace_args=(-n "${target_namespace}")
  fi

  if [[ -n "${REMOTE_HOST:-}" ]]; then
    remote_exec kubectl --context "${CONTEXT}" "${namespace_args[@]}" apply -f - <"${path}"
  else
    kube "${namespace_args[@]}" apply -f "${path}"
  fi
}

kube_delete_file() {
  local path="$1"

  if [[ -n "${REMOTE_HOST:-}" ]]; then
    remote_exec kubectl --context "${CONTEXT}" delete -f - \
      --ignore-not-found <"${path}"
  else
    kube delete -f "${path}" --ignore-not-found
  fi
}

kube_apply_directory() {
  local directory="$1"
  local file

  for file in "${directory}"/*.yaml; do
    [[ -f "${file}" ]] || continue
    grep -q '[^[:space:]]' "${file}" || continue
    kube_apply_file "${file}"
  done
}

kube_delete_directory() {
  local directory="$1"
  local file

  for file in "${directory}"/*.yaml; do
    [[ -f "${file}" ]] || continue
    grep -q '[^[:space:]]' "${file}" || continue
    kube_delete_file "${file}" || true
  done
}

load_config() {
  local config_path="$1"

  [[ -f "${config_path}" ]] || fail "configuration file not found: ${config_path}"
  CONFIG="$(cd "$(dirname "${config_path}")" && pwd)/$(basename "${config_path}")"

  local config_name
  config_name="$(yq -r '.metadata.name // ""' "${CONFIG}")"
  [[ "${config_name}" =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ ]] ||
    fail "metadata.name must be a DNS label"

  WORK_DIR="${repo_root}/tmp/project-signal-certification/${config_name}"
  mkdir -p "${WORK_DIR}"
  CONFIG_JSON="${WORK_DIR}/config.json"
  yq -o=json "${CONFIG}" >"${CONFIG_JSON}"

  CONTEXT="$(jq -r '.spec.context // ""' "${CONFIG_JSON}")"
  CLUSTER="$(jq -r '.spec.cluster // ""' "${CONFIG_JSON}")"
  ENVIRONMENT="$(jq -r '.spec.environment // ""' "${CONFIG_JSON}")"
  SOURCE_COMMIT="$(jq -r '.spec.sourceCommit // ""' "${CONFIG_JSON}")"
  EXPECTED_NODES=0
  EXPECTED_GPUS="$(jq -r '.spec.expectedGPUCount // 0' "${CONFIG_JSON}")"
  PROVIDER_LABEL="$(jq -r '.spec.providerLabel // ""' "${CONFIG_JSON}")"
  METRICS_OVERLAY="$(jq -r '.spec.metricsOverlay // ""' "${CONFIG_JSON}")"
  REMOTE_HOST="$(jq -r '.spec.execution.ssh.host // ""' "${CONFIG_JSON}")"
  INSTALLATION="${config_name}"
  SELECTOR="$(
    jq -r '.spec.nodeSelector | to_entries |
      map("\(.key)=\(.value|tostring)") | join(",")' "${CONFIG_JSON}"
  )"

  [[ "${METRICS_OVERLAY}" = /* ]] ||
    METRICS_OVERLAY="$(cd "$(dirname "${CONFIG}")" && pwd)/${METRICS_OVERLAY}"

  if [[ -n "${REMOTE_HOST}" ]]; then
    local remote_port identity_file control_path
    remote_port="$(jq -r '.spec.execution.ssh.port // 22' "${CONFIG_JSON}")"
    identity_file="$(jq -r '.spec.execution.ssh.identityFile // ""' "${CONFIG_JSON}")"
    control_path="$(jq -r '.spec.execution.ssh.controlPath // ""' "${CONFIG_JSON}")"
    REMOTE_SSH=(
      ssh
      -p "${remote_port}"
      -o BatchMode=yes
      -o ConnectTimeout=20
      -o IdentitiesOnly=yes
    )
    if [[ -n "${identity_file}" ]]; then
      REMOTE_SSH+=(-i "$(resolve_config_path "${identity_file}")")
    fi
    if [[ -n "${control_path}" ]]; then
      REMOTE_SSH+=(-S "$(resolve_config_path "${control_path}")")
    fi
    REMOTE_SSH+=("${REMOTE_HOST}")
  else
    REMOTE_SSH=()
  fi
}

config_value() {
  jq -r "$1 // empty" "${CONFIG_JSON}"
}

reject_placeholder() {
  local name="$1"
  local value="$2"

  [[ -n "${value}" ]] || fail "${name} is required"
  [[ "${value}" != *replace-with* ]] || fail "${name} still contains a placeholder"
}

preflight_config() {
  reject_placeholder "spec.context" "${CONTEXT}"
  reject_placeholder "spec.cluster" "${CLUSTER}"
  reject_placeholder "spec.environment" "${ENVIRONMENT}"
  reject_placeholder "spec.sourceCommit" "${SOURCE_COMMIT}"
  reject_placeholder "spec.metricsOverlay" "${METRICS_OVERLAY}"
  [[ "${CLUSTER}" != "cluster" && "${CLUSTER}" != "default" ]] ||
    fail "spec.cluster must be the actual cluster identifier"
  [[ "${EXPECTED_GPUS}" == "8" ]] ||
    fail "spec.expectedGPUCount must be exactly 8"
  [[ "${SOURCE_COMMIT}" =~ ^[0-9a-f]{40}$ ]] ||
    fail "spec.sourceCommit must be the exact 40-character controller image commit"
  [[ "${PROVIDER_LABEL}" == "unbounded-cloud.io/site" ]] ||
    fail "spec.providerLabel must be unbounded-cloud.io/site"
  [[ -n "${SELECTOR}" ]] || fail "spec.nodeSelector must not be empty"
  [[ -d "${METRICS_OVERLAY}" && -f "${METRICS_OVERLAY}/kustomization.yaml" ]] ||
    fail "spec.metricsOverlay must name a Kustomize directory"

  local image
  for image_path in '.spec.images.controller' '.spec.images.adapter'; do
    image="$(config_value "${image_path}")"
    [[ "${image}" =~ @sha256:[0-9a-f]{64}$ ]] ||
      fail "${image_path} must be an immutable SHA-256 image reference"
  done
}

verify_certification_source() {
  local tracked_path
  for tracked_path in \
    "${plan#"${repo_root}"/}" \
    deploy/signal/certification/whole-node-24h/workflow-template.yaml \
    deploy/signal/certification/whole-node-24h/gpu-burn-canary.sh \
    deploy/signal/observability/base/kustomization.yaml \
    hack/agent/skills/project-signal-provider-certification/SKILL.md; do
    git -C "${repo_root}" ls-files --error-unmatch "${tracked_path}" >/dev/null ||
      fail "required certification source is not committed: ${tracked_path}"
  done
  git -C "${repo_root}" diff --quiet HEAD -- \
    deploy/signal/certification \
    deploy/signal/observability \
    deploy/signal/argo-values.yaml \
    hack/agent/skills/project-signal-provider-certification ||
    fail "certification source has uncommitted changes"
}

preflight_context() {
  kube version --request-timeout=15s >/dev/null
}

preflight_nodes() {
  local nodes_json="${WORK_DIR}/nodes.json"
  local machines_json="${WORK_DIR}/machines.json"
  local pods_json="${WORK_DIR}/pods.json"
  kube get nodes -l "${SELECTOR}" -o json >"${nodes_json}"

  local count
  count="$(jq '.items | length' "${nodes_json}")"
  [[ "${count}" -gt 0 ]] ||
    fail "selector ${SELECTOR} did not match any provider Nodes"
  EXPECTED_NODES="${count}"

  kube get machines -o json >"${machines_json}"
  local isolation_violations
  isolation_violations="$(
    jq -r --slurpfile nodes "${nodes_json}" '
      .items as $machines |
      $nodes[0].items[].metadata.name as $node |
      [
        $machines[] |
        select((.spec.kubernetes.nodeRef.name // .metadata.name) == $node)
      ] as $matches |
      select(
        ($matches | length) != 1 or
        ($matches[0].status.isolation // "") != "none"
      ) |
      if ($matches | length) == 0 then
        "\($node): Machine not found"
      elif ($matches | length) > 1 then
        "\($node): multiple Machines found"
      else
        "\($node): Machine \($matches[0].metadata.name) reports isolation=\($matches[0].status.isolation // "unset")"
      end
    ' "${machines_json}"
  )"
  if [[ -n "${isolation_violations}" ]]; then
    printf 'error: certification requires every selected Machine to report status.isolation=none\n' >&2
    printf '%s\n' "${isolation_violations}" >&2
    exit 1
  fi

  local violations
  violations="$(
    jq -r \
      --arg providerLabel "${PROVIDER_LABEL}" \
      --arg expectedGPU "${EXPECTED_GPUS}" '
      .items[] |
      . as $node |
      ([.status.conditions[]? | select(.type == "Ready")][0].status // "Unknown") as $ready |
      (.metadata.labels[$providerLabel] // "") as $provider |
      (.status.capacity["nvidia.com/gpu"] // "0") as $capacity |
      (.status.allocatable["nvidia.com/gpu"] // "0") as $allocatable |
      select(
        $ready != "True" or
        (.spec.unschedulable // false) or
        $provider == "" or
        $capacity != $expectedGPU or
        $allocatable != $expectedGPU
      ) |
      "\(.metadata.name): ready=\($ready) unschedulable=\(.spec.unschedulable // false) provider=\($provider) capacity=\($capacity) allocatable=\($allocatable)"
      ' "${nodes_json}"
  )"
  if [[ -n "${violations}" ]]; then
    printf 'error: node preflight failed\n' >&2
    printf '%s\n' "${violations}" >&2
    exit 1
  fi

  kube get pods -A -o json >"${pods_json}"
  local gpu_consumers
  gpu_consumers="$(
    jq -r --slurpfile nodes "${nodes_json}" '
      [$nodes[0].items[].metadata.name] as $selected |
      .items[] |
      select(
        (.spec.nodeName // "") as $node |
        ($selected | index($node)) != null
      ) |
      select(.status.phase != "Succeeded" and .status.phase != "Failed") |
      ([
        .spec.containers[]?.resources.requests["nvidia.com/gpu"] // 0,
        .spec.initContainers[]?.resources.requests["nvidia.com/gpu"] // 0
      ] | map(tonumber) | add // 0) as $requested |
      select($requested > 0) |
      "\(.metadata.namespace)/\(.metadata.name): node=\(.spec.nodeName) requestedGPUs=\($requested)"
    ' "${pods_json}"
  )"
  if [[ -n "${gpu_consumers}" ]]; then
    printf 'error: provider Nodes have active GPU workloads\n' >&2
    printf '%s\n' "${gpu_consumers}" >&2
    exit 1
  fi
}

render_metrics_overlay() {
  local rendered="${WORK_DIR}/metrics-overlay.yaml"
  kubectl kustomize "${METRICS_OVERLAY}" >"${rendered}"

  local prometheus_config
  prometheus_config="$(
    yq eval-all '
      select(
        .kind == "ConfigMap" and
        .metadata.name == "project-signal-central-prometheus"
      ).data."prometheus.yml"
    ' "${rendered}"
  )"
  [[ -n "${prometheus_config}" ]] ||
    fail "metrics overlay does not define project-signal-central-prometheus"
  [[ "${prometheus_config}" == *"remote_write:"* ]] ||
    fail "metrics overlay does not configure remote_write"
  [[ "${prometheus_config}" != *"replace-with"* ]] ||
    fail "metrics overlay still contains placeholders"

  local rendered_cluster
  rendered_cluster="$(
    printf '%s\n' "${prometheus_config}" |
      yq -r '.global.external_labels.cluster // ""'
  )"
  [[ "${rendered_cluster}" == "${CLUSTER}" ]] ||
    fail "metrics cluster label ${rendered_cluster} does not match ${CLUSTER}"
  [[ "${prometheus_config}" == *"unbounded_cloud_io_site"* ]] ||
    fail "metrics overlay does not derive provider from unbounded-cloud.io/site"
}

preflight() {
  require_commands
  preflight_config
  verify_certification_source
  preflight_context
  preflight_nodes
  render_metrics_overlay
  printf 'preflight passed: context=%s cluster=%s nodes=%s selector=%s\n' \
    "${CONTEXT}" "${CLUSTER}" "${EXPECTED_NODES}" "${SELECTOR}"
}

render_signal_phase() {
  local phase="$1"
  local output_dir="${WORK_DIR}/rendered-${phase}"
  local controller_image adapter_image
  local acceptance_enabled=false
  local authority_enabled=false
  local hard_abort=true
  local source_commit=
  local static_package_digest=
  local authority_name=project-signal-execution-authority
  local baseline_test_uid=
  local baseline_test_generation=
  local baseline_test_digest=
  local baseline_profile_uid=

  controller_image="$(config_value '.spec.images.controller')"
  adapter_image="$(config_value '.spec.images.adapter')"
  mkdir -p "${output_dir}"

  if [[ "${phase}" != "base" ]]; then
    acceptance_enabled=true
    baseline_test_uid="$(jq -r '.uid' "${WORK_DIR}/baseline-test.json")"
    baseline_test_generation="$(jq -r '.generation' "${WORK_DIR}/baseline-test.json")"
    baseline_test_digest="$(jq -r '.contentDigest' "${WORK_DIR}/baseline-test.json")"
  fi

  if [[ "${phase}" == "authority" ]]; then
    authority_enabled=true
    hard_abort=false
    source_commit="${SOURCE_COMMIT}"
    static_package_digest="$(cat "${WORK_DIR}/static-package-digest")"
    baseline_profile_uid="$(jq -r '.uid' "${WORK_DIR}/baseline-profile.json")"
    authority_name="project-signal-execution-authority-${source_commit:0:12}-${static_package_digest#sha256:}"
    authority_name="${authority_name:0:60}"
  elif [[ "${phase}" != "base" && "${phase}" != "acceptance" ]]; then
    fail "unknown Signal render phase: ${phase}"
  fi

  (
    cd "${repo_root}"
    make signal-manifests \
      SIGNAL_MANIFEST_RENDERED_DIR="${output_dir}" \
      SIGNAL_CONTROLLER_IMAGE="${controller_image}" \
      SIGNAL_ADAPTER_IMAGE="${adapter_image}" \
      SIGNAL_CERTIFICATION_RUNNER_IMAGE="projectsignalpublic.azurecr.io/premerge/project-signal-certification-runner@sha256:73f80b8659abdabc656a2534f74b03dbf3e62b28e13b13072a43182c74266c14" \
      SIGNAL_ACCEPTANCE_SUITES_ENABLED="${acceptance_enabled}" \
      SIGNAL_EXECUTION_AUTHORITY_ENABLED="${authority_enabled}" \
      SIGNAL_CUTOVER_APPROVED="${authority_enabled}" \
      SIGNAL_HARD_ABORT="${hard_abort}" \
      SIGNAL_SOURCE_COMMIT="${source_commit}" \
      SIGNAL_STATIC_PACKAGE_DIGEST="${static_package_digest}" \
      SIGNAL_AUTHORITY_NAME="${authority_name}" \
      SIGNAL_BASELINE_PROFILE_UID="${baseline_profile_uid}" \
      SIGNAL_GPU_INVENTORY_TEST_UID="${baseline_test_uid}" \
      SIGNAL_GPU_INVENTORY_TEST_GENERATION="${baseline_test_generation}" \
      SIGNAL_GPU_INVENTORY_TEST_CONTENT_DIGEST="${baseline_test_digest}" \
      SIGNAL_VLLM_CANARY_ENABLED=false \
      SIGNAL_QUALIFICATION_GATE_PROTECTED=false
  )
  RENDERED_DIR="${output_dir}"
}

install_argo() {
  if [[ -n "${REMOTE_HOST:-}" ]]; then
    remote_exec_retry helm repo add argo https://argoproj.github.io/argo-helm \
      --force-update >/dev/null
    remote_exec_retry helm repo update argo >/dev/null
    remote_exec helm upgrade --install project-signal-argo argo/argo-workflows \
      --kube-context "${CONTEXT}" \
      --namespace "${namespace}" \
      --create-namespace \
      --version "${argo_chart_version}" \
      --values - \
      --hide-notes \
      --wait \
      --timeout 10m <"${argo_values}"
  else
    helm repo add argo https://argoproj.github.io/argo-helm --force-update >/dev/null
    helm repo update argo >/dev/null
    helm upgrade --install project-signal-argo argo/argo-workflows \
      --kube-context "${CONTEXT}" \
      --namespace "${namespace}" \
      --create-namespace \
      --version "${argo_chart_version}" \
      --values "${argo_values}" \
      --hide-notes \
      --wait \
      --timeout 10m
  fi
}

install_signal_runtime() {
  render_signal_phase base
  kube_apply_directory "${RENDERED_DIR}/crd"
  kube_apply_directory "${RENDERED_DIR}"
  kube -n "${namespace}" rollout status \
    deployment/project-signal-controller --timeout=10m
}

approve_test_manifest() {
  local manifest="$1"
  local name="$2"
  local output="$3"
  local digest generation uid source_commit

  generation="$(kube get certificationtest "${name}" -o jsonpath='{.metadata.generation}')"
  uid="$(kube get certificationtest "${name}" -o jsonpath='{.metadata.uid}')"
  digest="$(
    cd "${repo_root}"
    go run ./hack/cmd/manifest-digest \
      --file "${manifest}" \
      --kind CertificationTest \
      --name "${name}" \
      --field spec
  )"
  source_commit="$(git -C "${repo_root}" rev-parse HEAD)"
  kube patch certificationtest "${name}" \
    --subresource=status \
    --type=merge \
    -p "$(
      jq -cn \
        --argjson generation "${generation}" \
        --arg digest "${digest}" \
        --arg reason "repository plan approved at ${source_commit}" \
        '{status:{observedGeneration:$generation,trust:{state:"Approved",contentDigest:$digest,reason:$reason}}}'
    )"
  jq -n \
    --arg name "${name}" \
    --arg uid "${uid}" \
    --argjson generation "${generation}" \
    --arg contentDigest "${digest}" \
    '{name:$name,uid:$uid,generation:$generation,contentDigest:$contentDigest}' >"${output}"
}

compute_static_package_digest() {
  local rendered_dir="$1"

  python3 - "${rendered_dir}" <<'PY'
import hashlib
import pathlib
import sys

root = pathlib.Path(sys.argv[1])
files = sorted(
    path
    for path in root.rglob("*.yaml")
    if path.name not in {
        "11-execution-authority.yaml",
        "12-execution-authority-transition.yaml",
    }
)
digest = hashlib.sha256()
for path in files:
    digest.update(str(path.relative_to(root)).encode())
    digest.update(b"\0")
    digest.update(path.read_bytes())
print("sha256:" + digest.hexdigest())
PY
}

install_execution_authority() {
  local baseline_test_manifest="${WORK_DIR}/rendered-base/05-gpu-inventory-tests.yaml"
  local transition
  local file

  approve_test_manifest \
    "${baseline_test_manifest}" \
    "${baseline_test_name}" \
    "${WORK_DIR}/baseline-test.json"

  render_signal_phase acceptance
  kube_apply_directory "${RENDERED_DIR}/crd"
  kube_apply_directory "${RENDERED_DIR}"
  kube get certificationprofile "${baseline_profile_name}" -o json |
    jq '{uid:.metadata.uid}' >"${WORK_DIR}/baseline-profile.json"
  compute_static_package_digest "${RENDERED_DIR}" >"${WORK_DIR}/static-package-digest"

  render_signal_phase authority
  for file in "${RENDERED_DIR}"/*.yaml; do
    [[ -f "${file}" ]] || continue
    [[ "$(basename "${file}")" == "12-execution-authority-transition.yaml" ]] && continue
    grep -q '[^[:space:]]' "${file}" || continue
    kube_apply_file "${file}"
  done

  transition="${RENDERED_DIR}/12-execution-authority-transition.yaml"
  kube patch configmap project-signal-execution-state \
    -n "${authority_namespace}" \
    --type=json \
    -p "$(cat "${transition}")" \
    --dry-run=server >/dev/null
  kube patch configmap project-signal-execution-state \
    -n "${authority_namespace}" \
    --type=json \
    -p "$(cat "${transition}")"
  kube -n "${namespace}" rollout status \
    deployment/project-signal-controller --timeout=10m
}

install_catalog() {
  local tests="${WORK_DIR}/certification-tests.yaml"
  local bindings="${WORK_DIR}/certification-bindings.yaml"

  (
    cd "${repo_root}"
    go run ./cmd/kubectl-unbounded signal render \
      --filename "${plan}" \
      --tests-only >"${tests}"
  )
  kube_apply_file "${tests}"

  approve_test_manifest \
    "${tests}" \
    "${test_name}" \
    "${WORK_DIR}/whole-node-test.json"

  (
    cd "${repo_root}"
    go run ./cmd/kubectl-unbounded signal bindings \
      --context "${CONTEXT}" \
      --filename "${plan}" >"${bindings}"
  )
}

install_metrics() {
  kube_apply_file "${WORK_DIR}/metrics-overlay.yaml"
  kube -n "${namespace}" delete \
    deployment/project-signal-prometheus \
    service/project-signal-prometheus \
    configmap/project-signal-prometheus-config \
    serviceaccount/project-signal-prometheus \
    --ignore-not-found
  kube delete \
    clusterrole/project-signal-prometheus-pod-discovery \
    clusterrolebinding/project-signal-prometheus-pod-discovery \
    --ignore-not-found
  kube -n "${namespace}" rollout status \
    deployment/project-signal-central-prometheus --timeout=10m
  kube -n "${namespace}" rollout status \
    daemonset/project-signal-dcgm-exporter --timeout=10m
}

prometheus_query() {
  local query="$1"
  curl -fsS --get \
    --data-urlencode "query=${query}" \
    "http://127.0.0.1:19090/api/v1/query"
}

validate_metrics_remote() {
  "${REMOTE_SSH[@]}" bash -s -- "${CONTEXT}" "${namespace}" \
    "$((EXPECTED_NODES * EXPECTED_GPUS))" <<'REMOTE'
set -euo pipefail

context="$1"
namespace="$2"
expected_gpu_series="$3"

kubectl --context "${context}" -n "${namespace}" port-forward \
  service/project-signal-central-prometheus 19090:9090 \
  >/dev/null 2>&1 &
port_forward_pid=$!
trap 'kill "${port_forward_pid}" 2>/dev/null || true' EXIT

for _ in $(seq 1 30); do
  if curl -fsS "http://127.0.0.1:19090/-/ready" >/dev/null 2>&1; then
    break
  fi
  sleep 1
done
curl -fsS "http://127.0.0.1:19090/-/ready" >/dev/null
sleep 30

query() {
  curl -fsS --get \
    --data-urlencode "query=$1" \
    "http://127.0.0.1:19090/api/v1/query"
}

value="$(query 'sum(prometheus_remote_storage_samples_total)' |
  jq -r '.data.result[0].value[1] // "0"')"
awk -v value="${value}" 'BEGIN { exit !(value > 0) }'

value="$(query 'sum(prometheus_remote_storage_samples_failed_total)' |
  jq -r '.data.result[0].value[1] // "0"')"
awk -v value="${value}" 'BEGIN { exit !(value == 0) }'

value="$(query 'sum(up{job="project-signal-controller"} == 1)' |
  jq -r '.data.result[0].value[1] // "0"')"
awk -v value="${value}" 'BEGIN { exit !(value >= 1) }'

value="$(query 'count(DCGM_FI_DEV_GPU_UTIL{provider!=""})' |
  jq -r '.data.result[0].value[1] // "0"')"
awk -v value="${value}" -v expected="${expected_gpu_series}" \
  'BEGIN { exit !(value >= expected) }'

printf 'metrics validation passed: remote-write and %s GPU series are healthy\n' "${value}"
REMOTE
}

validate_metrics() {
  local port_forward_log="${WORK_DIR}/prometheus-port-forward.log"
  local port_forward_pid
  local response value

  kube get namespace "${namespace}" >/dev/null 2>&1 ||
    fail "Project Signal is not installed; run ACTION=install or ACTION=start first"
  kube -n "${namespace}" get service project-signal-central-prometheus >/dev/null 2>&1 ||
    fail "Project Signal metrics are not installed; run ACTION=install or ACTION=start first"

  if [[ -n "${REMOTE_HOST:-}" ]]; then
    validate_metrics_remote

    return
  fi

  kubectl --context "${CONTEXT}" -n "${namespace}" port-forward \
    service/project-signal-central-prometheus 19090:9090 \
    >"${port_forward_log}" 2>&1 &
  port_forward_pid=$!
  trap "kill ${port_forward_pid} 2>/dev/null || true; wait ${port_forward_pid} 2>/dev/null || true" EXIT

  for _ in $(seq 1 30); do
    if curl -fsS "http://127.0.0.1:19090/-/ready" >/dev/null 2>&1; then
      break
    fi
    sleep 1
  done
  curl -fsS "http://127.0.0.1:19090/-/ready" >/dev/null ||
    fail "central Prometheus did not become ready"

  sleep 30
  response="$(prometheus_query 'sum(prometheus_remote_storage_samples_total)')"
  value="$(jq -r '.data.result[0].value[1] // "0"' <<<"${response}")"
  awk -v value="${value}" 'BEGIN { exit !(value > 0) }' ||
    fail "remote-write has not sent any samples"

  response="$(prometheus_query 'sum(prometheus_remote_storage_samples_failed_total)')"
  value="$(jq -r '.data.result[0].value[1] // "0"' <<<"${response}")"
  awk -v value="${value}" 'BEGIN { exit !(value == 0) }' ||
    fail "prometheus_remote_storage_samples_failed_total is ${value}, expected 0"

  response="$(prometheus_query 'sum(up{job="project-signal-controller"} == 1)')"
  value="$(jq -r '.data.result[0].value[1] // "0"' <<<"${response}")"
  [[ "${value%.*}" -ge 1 ]] || fail "controller metrics target is not up"

  response="$(prometheus_query 'count(DCGM_FI_DEV_GPU_UTIL{provider!=""})')"
  value="$(jq -r '.data.result[0].value[1] // "0"' <<<"${response}")"
  [[ "${value%.*}" -ge $((EXPECTED_NODES * EXPECTED_GPUS)) ]] ||
    fail "only ${value} provider-attributed GPU series are visible"

  kill "${port_forward_pid}" 2>/dev/null || true
  wait "${port_forward_pid}" 2>/dev/null || true
  trap - EXIT
  printf 'metrics validation passed: remote-write and %s GPU series are healthy\n' "${value}"
}

install_all() {
  preflight
  install_argo
  install_signal_runtime
  install_execution_authority
  install_catalog
  install_metrics
  validate_metrics
}

write_inputs() {
  local duration="$1"
  local output="$2"
  local environment_json

  environment_json="$(
    jq -cn \
      --arg duration "${duration}" \
      --arg cpuWorkers "$(config_value '.spec.cpuWorkers')" \
      --arg expectedGPU "${EXPECTED_GPUS}" \
      --arg product "$(config_value '.spec.expectedGPUProductRegex')" \
      --arg memory "$(config_value '.spec.memoryMiB')" \
      --arg sample "$(config_value '.spec.sampleIntervalSeconds')" \
      --arg storage "$(config_value '.spec.storageMiB')" \
      '{
        BURN_DURATION_SECONDS:$duration,
        CPU_WORKERS:$cpuWorkers,
        EXPECTED_GPU_COUNT:$expectedGPU,
        EXPECTED_GPU_PRODUCT_REGEX:$product,
        MEMORY_MIB:$memory,
        OBSERVED_GPU_ALLOCATABLE:$expectedGPU,
        OBSERVED_GPU_CAPACITY:$expectedGPU,
        POWER_LIMIT_CONSECUTIVE_SAMPLES:"3",
        POWER_LIMIT_TOLERANCE_PERCENT:"5",
        SAMPLE_INTERVAL_SECONDS:$sample,
        STORAGE_MIB:$storage
      }'
  )"

  jq -n \
    --arg environment "${environment_json}" \
    '{
      apiVersion:"signal.unbounded-cloud.io/v1alpha1",
      kind:"CertificationPlanInputs",
      metadata:{name:"project-signal-whole-node-24h"},
      spec:{stages:{"whole-node-burn":{parameters:[{
        name:"environmentJson",
        value:{type:"String",stringValue:$environment}
      }]}}}
    }' | yq -P >"${output}"
}

render_run_package() {
  local duration="$1"
  local mode="$2"
  local inputs="${WORK_DIR}/certification-inputs-${mode}.yaml"
  local package="${WORK_DIR}/certification-package-${mode}.yaml"

  write_inputs "${duration}" "${inputs}"
  (
    cd "${repo_root}"
    go run ./cmd/kubectl-unbounded signal render \
      --filename "${plan}" \
      --bindings "${WORK_DIR}/certification-bindings.yaml" \
      --inputs "${inputs}" >"${package}"
  )
  kube_apply_file "${package}" "${namespace}"
}

apply_profile_and_policy() {
  local duration="$1"
  local mode="$2"
  local profile="${WORK_DIR}/profile.yaml"
  local policy="${WORK_DIR}/campaign-policy-${mode}.yaml"
  local campaigns empty_campaigns profile_uid policy_name

  jq -n \
    --arg name "${profile_name}" \
    --arg suite "${suite_name}" \
    --arg installation "${INSTALLATION}" \
    --argjson labels "$(jq '.spec.nodeSelector' "${CONFIG_JSON}")" \
    '{
      apiVersion:"signal.unbounded-cloud.io/v1alpha1",
      kind:"CertificationProfile",
      metadata:{
        name:$name,
        labels:{
          "app.kubernetes.io/part-of":"project-signal",
          "signal.unbounded-cloud.io/installation":$installation
        }
      },
      spec:{
        suite:{name:$suite},
        participants:{count:1,selector:{matchLabels:$labels}}
      }
    }' | yq -P >"${profile}"
  kube_apply_file "${profile}"
  profile_uid="$(kube get certificationprofile "${profile_name}" \
    -o jsonpath='{.metadata.uid}')"

  policy_name="project-signal-${INSTALLATION}-${mode}"
  if [[ "${mode}" == "smoke" ]] &&
    kube get certificationcampaignpolicy "${policy_name}" >/dev/null 2>&1; then
    campaigns="$(kube -n "${namespace}" get certificationcampaigns -o json)"
    empty_campaigns="$(
      jq -r --arg policy "${policy_name}" '
        [.items[] | select(.spec.policy.name == $policy)] as $campaigns |
        if ($campaigns | length) > 0 and
          all($campaigns[];
            .status.phase == "Completed" and
            ((.status.succeededRuns // 0) +
             (.status.failedRuns // 0) +
             (.status.inconclusiveRuns // 0) +
             (.status.canceledRuns // 0)) == 0
          )
        then $campaigns[].metadata.name
        else empty
        end
      ' <<<"${campaigns}"
    )"
    if [[ -n "${empty_campaigns}" ]]; then
      while IFS= read -r campaign; do
        kube -n "${namespace}" delete certificationcampaign "${campaign}" --wait=true
      done <<<"${empty_campaigns}"
      kube delete certificationcampaignpolicy "${policy_name}" --wait=true
      printf 'reset empty smoke campaigns whose admission window expired\n'
    fi
  fi

  jq -n \
    --arg name "${policy_name}" \
    --arg profile "${profile_name}" \
    --arg profileUID "${profile_uid}" \
    --arg duration "${duration}s" \
    --arg installation "${INSTALLATION}" \
    --argjson labels "$(jq '.spec.nodeSelector' "${CONFIG_JSON}")" \
    '{
      apiVersion:"signal.unbounded-cloud.io/v1alpha1",
      kind:"CertificationCampaignPolicy",
      metadata:{
        name:$name,
        labels:{
          "app.kubernetes.io/part-of":"project-signal",
          "signal.unbounded-cloud.io/installation":$installation
        }
      },
      spec:{
        trigger:"OnNodeReady",
        duration:$duration,
        stopOnFailure:false,
        nodeSelector:{matchLabels:$labels},
        profile:{name:$profile,uid:$profileUID}
      }
    }' | yq -P >"${policy}"
  kube_apply_file "${policy}"
}

wait_for_smoke() {
  local deadline=$((SECONDS + 1800))
  local campaigns completed succeeded failed policy_name
  policy_name="project-signal-${INSTALLATION}-smoke"

  while ((SECONDS < deadline)); do
    campaigns="$(kube -n "${namespace}" get certificationcampaigns \
      -o json)"
    completed="$(
      jq --arg policy "${policy_name}" \
        '[.items[] | select(.spec.policy.name == $policy and .status.phase == "Completed")] | length' \
        <<<"${campaigns}"
    )"
    succeeded="$(
      jq --arg policy "${policy_name}" \
        '[.items[] | select(.spec.policy.name == $policy and .status.succeededRuns >= 1)] | length' \
        <<<"${campaigns}"
    )"
    failed="$(
      jq --arg policy "${policy_name}" \
        '[.items[] | select(
          .spec.policy.name == $policy and (
            (.status.failedRuns // 0) > 0 or
            (.status.inconclusiveRuns // 0) > 0 or
            (.status.canceledRuns // 0) > 0
          )
        )] | length' <<<"${campaigns}"
    )"
    if [[ "${failed}" != "0" ]]; then
      cancel_runs_for_policy "${policy_name}"
      fail "smoke produced a non-passing Run"
    fi
    if [[ "${completed}" == "${EXPECTED_NODES}" && "${succeeded}" == "${EXPECTED_NODES}" ]]; then
      printf 'smoke passed on all %s Nodes\n' "${EXPECTED_NODES}"
      return
    fi
    sleep 10
  done
  fail "smoke did not complete on all Nodes within 30 minutes"
}

cancel_runs_for_policy() {
  local policy_name="$1"
  local campaigns deadline runs_json run phase
  campaigns="$(
    kube -n "${namespace}" get certificationcampaigns -o json |
      jq -c --arg policy "${policy_name}" \
        '[.items[] | select(.spec.policy.name == $policy) | .metadata.name]'
  )"

  while IFS= read -r campaign; do
    [[ -n "${campaign}" ]] || continue
    kube -n "${namespace}" delete certificationcampaign "${campaign}" \
      --ignore-not-found --wait=true
  done < <(jq -r '.[]' <<<"${campaigns}")
  kube delete certificationcampaignpolicy "${policy_name}" \
    --ignore-not-found --wait=true

  deadline=$((SECONDS + 900))
  while ((SECONDS < deadline)); do
    runs_json="$(kube -n "${namespace}" get certificationruns -o json)"
    while IFS=$'\t' read -r run phase; do
      [[ -n "${run}" ]] || continue
      if [[ ! "${phase}" =~ ^(Succeeded|Failed|Inconclusive|Canceled)$ ]]; then
        kube -n "${namespace}" patch certificationrun "${run}" \
          --type=merge -p '{"spec":{"cancel":true}}'
      fi
    done < <(
      jq -r --argjson campaigns "${campaigns}" '
        .items[] |
        . as $run |
        select($campaigns | index($run.metadata.labels["signal.unbounded-cloud.io/campaign"])) |
        [.metadata.name, (.status.phase // "")] |
        @tsv
      ' <<<"${runs_json}"
    )
    if jq -e --argjson campaigns "${campaigns}" '
      all(
        .items[] |
        . as $run |
        select($campaigns | index($run.metadata.labels["signal.unbounded-cloud.io/campaign"]));
        (.status.phase // "") | test("^(Succeeded|Failed|Inconclusive|Canceled)$")
      )
    ' <<<"${runs_json}" >/dev/null; then
      break
    fi
    sleep 5
  done

  while IFS= read -r run; do
    [[ -n "${run}" ]] || continue
    kube -n "${namespace}" patch certificationrun "${run}" \
      --type=merge -p '{"spec":{"release":true}}'
  done < <(
    jq -r --argjson campaigns "${campaigns}" '
      .items[] |
      . as $run |
      select($campaigns | index($run.metadata.labels["signal.unbounded-cloud.io/campaign"])) |
      .metadata.name
    ' <<<"${runs_json}"
  )

  deadline=$((SECONDS + 900))
  while ((SECONDS < deadline)); do
    runs_json="$(kube -n "${namespace}" get certificationruns -o json)"
    if jq -e --argjson campaigns "${campaigns}" '
      [
        .items[] |
        . as $run |
        select($campaigns | index($run.metadata.labels["signal.unbounded-cloud.io/campaign"]))
      ] as $runs |
      ($runs | length) > 0 and
      all($runs[]; any(.status.conditions[]?; .type == "Released" and .status == "True"))
    ' <<<"${runs_json}" >/dev/null; then
      return
    fi
    sleep 5
  done
  fail "smoke cleanup did not release all provider Nodes within 15 minutes"
}

run_mode() {
  local mode="$1"
  local duration

  preflight
  validate_metrics
  case "${mode}" in
    smoke)
      duration=120
      ;;
    full)
      duration=86400
      ;;
    *)
      fail "unknown run mode: ${mode}"
      ;;
  esac

  render_run_package "${duration}" "${mode}"
  apply_profile_and_policy "${duration}" "${mode}"

  if [[ "${mode}" == "smoke" ]]; then
    wait_for_smoke
  else
    printf '24-hour campaigns launched for selector %s\n' "${SELECTOR}"
  fi
}

status() {
  preflight_config
  printf 'Campaign policies:\n'
  kube get certificationcampaignpolicies \
    -l "signal.unbounded-cloud.io/installation=${INSTALLATION}" -o wide || true
  printf '\nCampaigns:\n'
  kube -n "${namespace}" get certificationcampaigns -o wide || true
  printf '\nRuns:\n'
  kube -n "${namespace}" get certificationruns -o wide || true
  printf '\nWorkflows:\n'
  kube -n "${namespace}" get workflows -o wide || true
  printf '\nPods:\n'
  kube -n "${namespace}" get pods -o wide || true
  validate_metrics
}

cancel_and_release_runs() {
  local runs_json run phase released deadline
  runs_json="$(kube -n "${namespace}" get certificationruns -o json 2>/dev/null || echo '{"items":[]}')"
  while IFS=$'\t' read -r run phase; do
    [[ -n "${run}" ]] || continue
    if [[ ! "${phase}" =~ ^(Succeeded|Failed|Inconclusive|Canceled)$ ]]; then
      kube -n "${namespace}" patch certificationrun "${run}" \
        --type=merge -p '{"spec":{"cancel":true}}'
    fi
  done < <(jq -r '.items[] | [.metadata.name, (.status.phase // "")] | @tsv' <<<"${runs_json}")

  deadline=$((SECONDS + 900))
  while ((SECONDS < deadline)); do
    runs_json="$(kube -n "${namespace}" get certificationruns -o json 2>/dev/null || echo '{"items":[]}')"
    if jq -e 'all(.items[]; (.status.phase // "") | test("^(Succeeded|Failed|Inconclusive|Canceled)$"))' \
      <<<"${runs_json}" >/dev/null; then
      break
    fi
    sleep 5
  done

  while IFS= read -r run; do
    [[ -n "${run}" ]] || continue
    kube -n "${namespace}" patch certificationrun "${run}" \
      --type=merge -p '{"spec":{"release":true}}'
  done < <(jq -r '.items[].metadata.name' <<<"${runs_json}")

  deadline=$((SECONDS + 900))
  while ((SECONDS < deadline)); do
    runs_json="$(kube -n "${namespace}" get certificationruns -o json 2>/dev/null || echo '{"items":[]}')"
    released="$(jq '[.items[] | select(any(.status.conditions[]?; .type == "Released" and .status == "True"))] | length' <<<"${runs_json}")"
    [[ "${released}" == "$(jq '.items | length' <<<"${runs_json}")" ]] && return
    sleep 5
  done
  fail "not all Runs released their Nodes within 15 minutes"
}

clean_node_markers() {
  local nodes_json="${WORK_DIR}/nodes.json"
  kube get nodes -l "${SELECTOR}" -o json >"${nodes_json}"

  while IFS= read -r node; do
    local patch
    patch="$(
      jq -c --arg name "${node}" '
        .items[] | select(.metadata.name == $name) |
        ({
          metadata:{
            labels: ((.metadata.labels // {}) |
              with_entries(select(.key | startswith("signal.unbounded-cloud.io/"))) |
              with_entries(.value = null)),
            annotations: ((.metadata.annotations // {}) |
              with_entries(select(.key | startswith("signal.unbounded-cloud.io/"))) |
              with_entries(.value = null))
          }
        } + if (.metadata.labels["signal.unbounded-cloud.io/reserved-node-uid"] // "") != ""
          then {spec:{unschedulable:false}}
          else {}
          end)
      ' "${nodes_json}"
    )"
    [[ "${patch}" != '{"metadata":{"labels":{},"annotations":{}}}' ]] &&
      kube patch node "${node}" --type=merge -p "${patch}"
  done < <(jq -r '.items[].metadata.name' "${nodes_json}")
}

uninstall_all() {
  require_commands
  preflight_config
  verify_certification_source
  preflight_context
  render_signal_phase base
  render_metrics_overlay

  kube delete certificationcampaignpolicies --all --ignore-not-found
  cancel_and_release_runs
  kube -n "${namespace}" delete certificationcampaigns --all \
    --ignore-not-found
  kube delete \
    certificationprofiles,certificationsuites,certificationtests,certificationpolicies \
    --all --ignore-not-found

  for manifest in \
    "${WORK_DIR}/campaign-policy-smoke.yaml" \
    "${WORK_DIR}/campaign-policy-full.yaml" \
    "${WORK_DIR}/profile.yaml" \
    "${WORK_DIR}/certification-package-smoke.yaml" \
    "${WORK_DIR}/certification-package-full.yaml" \
    "${WORK_DIR}/certification-package.yaml" \
    "${WORK_DIR}/certification-tests.yaml"; do
    [[ -f "${manifest}" ]] &&
      kube_delete_file "${manifest}"
  done

  kube_delete_file "${WORK_DIR}/metrics-overlay.yaml" || true
  kube_delete_directory "${RENDERED_DIR}"
  if [[ -n "${REMOTE_HOST:-}" ]]; then
    remote_exec helm uninstall project-signal-argo \
      --kube-context "${CONTEXT}" \
      --namespace "${namespace}" \
      --wait || true
  else
    helm uninstall project-signal-argo \
      --kube-context "${CONTEXT}" \
      --namespace "${namespace}" \
      --wait || true
  fi
  clean_node_markers
  kube delete namespace "${namespace}" "${authority_namespace}" \
    --ignore-not-found --wait
  kube_delete_directory "${RENDERED_DIR}/crd"
  kube delete clusterrole,clusterrolebinding \
    -l app.kubernetes.io/part-of=project-signal \
    --ignore-not-found
  printf 'Project Signal uninstall completed for %s\n' "${INSTALLATION}"
}

main() {
  [[ $# -eq 2 ]] || {
    usage
    exit 2
  }
  local command="$1"
  load_config "$2"

  case "${command}" in
    preflight)
      preflight
      ;;
    install)
      install_all
      ;;
    smoke)
      run_mode smoke
      ;;
    run)
      run_mode full
      ;;
    start)
      install_all
      run_mode smoke
      run_mode full
      ;;
    status)
      status
      ;;
    uninstall)
      uninstall_all
      ;;
    *)
      usage
      exit 2
      ;;
  esac
}

if [[ "${PROJECT_SIGNAL_LIB_ONLY:-false}" != "true" ]]; then
  main "$@"
fi
