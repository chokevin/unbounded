#!/usr/bin/env bash

set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/../../../../.." && pwd)"
target="${1:-${repo_root}/tmp/project-signal-provider-certification}"

config="${target}/provider-certification.yaml"
metrics_base="${target}/metrics-base"
metrics_overlay="${target}/metrics-overlay"

for path in "${config}" "${metrics_base}" "${metrics_overlay}"; do
	if [[ -e "${path}" ]]; then
		printf 'error: refusing to overwrite %s\n' "${path}" >&2
		exit 1
	fi
done

mkdir -p "${target}"
cp "${repo_root}/deploy/signal/certification/whole-node-24h/config.example.yaml" "${config}"
cp -R "${repo_root}/deploy/signal/observability/base" "${metrics_base}"
cp -R "${repo_root}/deploy/signal/observability/overlay.example" "${metrics_overlay}"

sed -i.bak \
	-e 's#metricsOverlay: replace-with-internal-metrics-overlay-directory#metricsOverlay: metrics-overlay#' \
	"${config}"
sed -i.bak \
	-e 's#  - ../base#  - ../metrics-base#' \
	"${metrics_overlay}/kustomization.yaml"
rm "${config}.bak" "${metrics_overlay}/kustomization.yaml.bak"

cat <<EOF
Created a self-contained Project Signal provider certification workspace:
  ${config}
  ${metrics_overlay}/

Next:
  1. Replace every placeholder in the config and metrics overlay.
  2. Create the referenced remote-write identity Secret in the cluster.
  3. Validate the complete configuration without changing the cluster:
     make signal-provider-certification \\
       ACTION=preflight \\
       CONFIG=${config}
  4. Install, smoke-test, and launch certification:
     make signal-provider-certification \\
       ACTION=start \\
       CONFIG=${config}
EOF
