---
name: project-signal-provider-certification
description: Install, validate, run, inspect, and remove a Project Signal provider certification with centralized metrics.
---

## Purpose

Use this skill to turn a clean Kubernetes cluster with labeled GPU Nodes
into a Project Signal certification environment. The repository owns the
controllers, Argo runtime, 24-hour whole-node burn, DCGM exporter, and
Prometheus base. The operator supplies one cluster config and one internal
Kustomize overlay for the remote-write endpoint and identity.

The workflow is intentionally ordered:

1. Preflight the exact context, all selected Nodes, GPU inventory, provider
   labels, immutable images, and metrics overlay.
2. Install pinned Argo Workflows and Project Signal.
3. Render, review, approve, bind, and install the repository-owned burn Test.
4. Install one central Prometheus collector and DCGM exporter.
5. Prove controller, GPU, and remote-write metrics are healthy.
6. Run a short smoke on every selected Node.
7. Launch one 24-hour burn on every selected Node.

## Configuration boundary

Copy
`deploy/signal/certification/whole-node-24h/config.example.yaml` outside the
repository and replace every placeholder.

Public configuration includes:

- kubeconfig context;
- actual cluster identifier used as the `cluster` metrics label;
- provider Node selector;
- canonical provider label key;
- expected GPU count and product;
- bounded CPU, memory, storage, and sampling settings;
- immutable controller and adapter images.

Internal configuration is a Kustomize overlay based on
`deploy/signal/observability/base`. It owns only the remote-write endpoint,
identity, secret references, and internal dashboard link. Start from
`deploy/signal/observability/overlay.example`; do not commit real endpoint or
identity values to this repository.

If the Kubernetes API is reachable only from a jumpbox, set
`spec.execution.ssh` in the external config. Rendering and digest computation
remain local; the skill sends manifests over standard input and runs only
`kubectl`, Helm, `jq`, `curl`, and `awk` on the jumpbox. No repository checkout
or Go toolchain is required there.

The configured `spec.context` is always passed explicitly to Kubernetes and
Helm commands. The kubeconfig's current context may point somewhere else.

## Commands

```bash
skill=./hack/agent/skills/project-signal-provider-certification/scripts/project-signal-certification.sh
config=/secure/path/provider-certification.yaml

# Read-only validation.
"$skill" preflight "$config"

# Idempotent controller, catalog, and metrics installation.
"$skill" install "$config"

# Two-minute burn on every selected Node, then require every Run to pass.
"$skill" smoke "$config"

# Launch one 24-hour burn on every selected Node.
"$skill" run "$config"

# Install, smoke, and launch the 24-hour run in one command.
"$skill" start "$config"

# Show Campaign, Run, Workflow, Pod, and remote-write state.
"$skill" status "$config"

# Cancel active Runs, release Nodes, and remove the complete Signal stack.
"$skill" uninstall "$config"
```

`start` is the normal clean-cluster path. Each phase discovers the complete
Node population matching the provider selector. It stops before the 24-hour
launch if any selected Node is unhealthy, unschedulable, missing its provider
label, or does not expose the expected GPU inventory; if the smoke fails; if
the cluster label is generic or mismatched; or if remote-write reports
non-recoverable failures. Deliberate `write_relabel_configs` filtering is
allowed; required controller and provider-attributed GPU series must still be
present.

## Trust decision

Applying a `CertificationTest` never approves it. The install command computes
the canonical digest of the exact repository-rendered Test specification,
checks the live generation, and records an `Approved` status for that digest.
Running the skill is the operator's explicit approval of the repository-owned
plan at the current Git commit. Any Test generation change requires a new
install invocation and a new digest.

## Safe removal

Uninstall deletes all Signal CampaignPolicies first, requests cancellation and
release of active Runs, waits for Node release, removes all Signal catalog and
qualification resources, removes the metrics overlay, uninstalls Argo, removes
Signal CRDs and cluster RBAC, and removes only
`signal.unbounded-cloud.io/*` labels and annotations from selected Nodes. It
preserves the canonical `unbounded-cloud.io/site` provider label.
