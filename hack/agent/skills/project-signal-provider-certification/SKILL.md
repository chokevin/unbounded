---
name: project-signal-provider-certification
description: Install, validate, run, inspect, and remove a three-node Project Signal provider certification with centralized metrics.
---

## Purpose

Use this skill to turn a clean Kubernetes cluster with three labeled GPU Nodes
into a Project Signal certification environment. The repository owns the
controllers, Argo runtime, 24-hour whole-node burn, DCGM exporter, and
Prometheus base. The operator supplies one cluster config and one internal
Kustomize overlay for the remote-write endpoint and identity.

The workflow is intentionally ordered:

1. Preflight the exact context, three selected Nodes, GPU inventory, provider
   labels, immutable images, and metrics overlay.
2. Install pinned Argo Workflows and Project Signal.
3. Render, review, approve, bind, and install the repository-owned burn Test.
4. Install one central Prometheus collector and DCGM exporter.
5. Prove controller, GPU, and remote-write metrics are healthy.
6. Run a short three-node smoke.
7. Launch one 24-hour burn on each of the three Nodes.

## Configuration boundary

Copy
`deploy/signal/certification/whole-node-24h/config.example.yaml` outside the
repository and replace every placeholder.

Public configuration includes:

- kubeconfig context;
- actual cluster identifier used as the `cluster` metrics label;
- exact three-node selector;
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

## Commands

```bash
skill=./hack/agent/skills/project-signal-provider-certification/scripts/project-signal-certification.sh
config=/secure/path/provider-certification.yaml

# Read-only validation.
"$skill" preflight "$config"

# Idempotent controller, catalog, and metrics installation.
"$skill" install "$config"

# Two-minute burn on all three Nodes, then require three passing Runs.
"$skill" smoke "$config"

# Launch one 24-hour burn on all three Nodes.
"$skill" run "$config"

# Install, smoke, and launch the 24-hour run in one command.
"$skill" start "$config"

# Show Campaign, Run, Workflow, Pod, and remote-write state.
"$skill" status "$config"

# Cancel active Runs, release Nodes, and remove the complete Signal stack.
"$skill" uninstall "$config"
```

`start` is the normal clean-cluster path. It stops before the 24-hour launch if
the smoke fails, the selected Node count changes, a provider label is missing,
the cluster label is generic or mismatched, or remote-write reports failed or
dropped samples.

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
