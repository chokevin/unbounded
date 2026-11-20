#!/usr/bin/env bash

set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/../../../../.." && pwd)"
target="${1:-${repo_root}/tmp/project-signal-provider-certification}"

config="${target}/provider-certification.yaml"
certification="${target}/certification"
metrics_base="${target}/metrics-base"
metrics_overlay="${target}/metrics-overlay"

for path in "${config}" "${certification}" "${metrics_base}" "${metrics_overlay}"; do
	if [[ -e "${path}" ]]; then
		printf 'error: refusing to overwrite %s\n' "${path}" >&2
		exit 1
	fi
done

mkdir -p "${target}"
cp "${repo_root}/deploy/signal/certification/whole-node-24h/config.example.yaml" "${config}"
mkdir -p "${certification}"
cp \
	"${repo_root}/deploy/signal/certification/whole-node-24h/certification-plan.yaml" \
	"${repo_root}/deploy/signal/certification/whole-node-24h/gpu-burn-canary.sh" \
	"${repo_root}/deploy/signal/certification/whole-node-24h/inputs-run.yaml" \
	"${repo_root}/deploy/signal/certification/whole-node-24h/inputs-smoke.yaml" \
	"${repo_root}/deploy/signal/certification/whole-node-24h/workflow-template.yaml" \
	"${certification}/"
cp -R "${repo_root}/deploy/signal/observability/base" "${metrics_base}"
cp -R "${repo_root}/deploy/signal/observability/overlay.example" "${metrics_overlay}"

sed -i.bak \
	-e 's#  - ../base#  - ../metrics-base#' \
	"${metrics_overlay}/kustomization.yaml"
rm "${metrics_overlay}/kustomization.yaml.bak"

cat <<EOF
Created a self-contained Project Signal provider certification workspace:
  ${config}
  ${certification}/
  ${metrics_overlay}/

Next:
  1. Define the certification scripts, WorkflowTemplates, and inputs under ${certification}/.
  2. Replace every placeholder in the config and metrics overlay, including
     the Grafana domain and Prometheus remote-write configuration.
  3. Create the referenced remote-write identity Secret in the cluster.
  4. Validate the complete configuration without changing the cluster:
     make signal-provider-certification \\
       ACTION=preflight \\
       CONFIG=${config}
  5. Install, smoke-test, and launch certification:
     make signal-provider-certification \\
       ACTION=start \\
       CONFIG=${config}
EOF
