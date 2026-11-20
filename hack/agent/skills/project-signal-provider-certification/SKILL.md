---
name: project-signal-provider-certification
description: Install, validate, run, inspect, and remove a Project Signal provider certification with centralized metrics.
---

## Purpose

Use this skill to turn an active Unbounded cluster with labeled GPU Nodes into
a Project Signal certification environment. Project Signal owns the
controllers, Argo runtime, DCGM exporter, Prometheus base, execution, and
cleanup. The user supplies one certification workspace containing the
CertificationPlan, scripts, WorkflowTemplates, smoke and run inputs, Grafana
domain, and Prometheus remote-write overlay.

For the complete operator-facing field reference, prerequisites,
troubleshooting, monitoring gate, and clean-room retry, follow
`deploy/signal/provider-certification-runbook.md`.

The workflow is intentionally ordered:

1. Preflight the exact context, all selected Nodes, GPU inventory, provider
   labels, immutable images, and metrics overlay.
2. Install pinned Argo Workflows and Project Signal.
3. Compile, review, approve, bind, and install every Test and WorkflowTemplate
   in the configured CertificationPlan.
4. Install one central Prometheus collector and DCGM exporter.
5. Prove controller, GPU, and remote-write metrics are healthy.
6. Run a short smoke on every selected Node.
7. Launch one 24-hour burn on every selected Node.

## Configuration boundary

Create the portable workspace with
`make signal-provider-certification-init`, then replace every placeholder.

The deployment config includes:

- kubeconfig context;
- actual cluster identifier used as the `cluster` metrics label;
- exact source commit embedded in the controller and adapter images;
- provider Node selector;
- canonical provider label key;
- selected Machines with `status.isolation: none`;
- expected GPU count and product;
- bounded CPU, memory, storage, and sampling settings;
- immutable controller and adapter images.
- the user-owned CertificationPlan and smoke/run inputs; and
- the Grafana domain and Prometheus remote-write overlay.

The generated Kustomize overlay is based on
`deploy/signal/observability/base`. It contains the supplied remote-write
endpoint, identity or Secret references, and bounded transport settings. Do
not commit live endpoints or credentials to this repository.

If the Kubernetes API is reachable only from a jumpbox, set
`spec.execution.ssh` in the deployment config. Rendering and digest computation
remain local; the skill sends manifests over standard input and runs only
`kubectl`, Helm, `jq`, `curl`, and `awk` on the jumpbox. No repository checkout
or Go toolchain is required there.

The configured `spec.context` is always passed explicitly to Kubernetes and
Helm commands. The kubeconfig's current context may point somewhere else.

## Commands

Create a portable workspace containing the deployment config, metrics base, and
metrics overlay:

```bash
make signal-provider-certification-init \
  SIGNAL_PROVIDER_CERTIFICATION_DIR=/secure/path/project-signal
```

Replace the placeholders in `provider-certification.yaml` and
`metrics-overlay/*.yaml`, then use the top-level lifecycle target:

```bash
config=/secure/path/project-signal/provider-certification.yaml

# Read-only validation.
make signal-provider-certification ACTION=preflight CONFIG="$config"

# Idempotent controller, catalog, and metrics installation.
make signal-provider-certification ACTION=install CONFIG="$config"

# Two-minute burn on every selected Node, then require every Run to pass.
make signal-provider-certification ACTION=smoke CONFIG="$config"

# Launch one 24-hour burn on every selected Node.
make signal-provider-certification ACTION=run CONFIG="$config"

# Install, smoke, and launch the 24-hour run in one command.
make signal-provider-certification ACTION=start CONFIG="$config"

# Show Campaign, Run, Workflow, Pod, and remote-write state.
make signal-provider-certification ACTION=status CONFIG="$config"

# Cancel active Runs, release Nodes, and remove the complete Signal stack.
make signal-provider-certification ACTION=uninstall CONFIG="$config"
```

`start` is the normal clean-cluster path. Each phase discovers the complete
Node population matching the provider selector. It stops before the 24-hour
launch if any selected Node is not backed by exactly one Machine reporting
`status.isolation: none`, is unhealthy, unschedulable, missing its provider
label, does not expose the expected GPU inventory, or has an active Pod
requesting GPUs; if the smoke fails; if
the cluster label is generic or mismatched; or if remote-write reports
non-recoverable failures. Deliberate `write_relabel_configs` filtering is
allowed; required controller and provider-attributed GPU series must still be
present.

The standalone `smoke`, `run`, and `status` actions require an existing
installation. Use `install` first when running phases individually, or use
`start` for the complete clean-cluster sequence.

## Trust decision

Applying a `CertificationTest` never approves it. The install command computes
the canonical digest of the exact repository-rendered Test specification,
checks the live generation, and records an `Approved` status for that digest.
Running the skill is the operator's explicit approval of the configured plan
and exact source artifacts. Any Test generation change requires a new install
invocation and a new digest.

## Safe removal

Uninstall deletes all Signal CampaignPolicies first, requests cancellation and
release of active Runs, waits for Node release, removes all Signal catalog and
qualification resources, removes the metrics overlay, uninstalls Argo, removes
Signal CRDs and cluster RBAC, and removes only
`signal.unbounded-cloud.io/*` labels and annotations from selected Nodes. It
preserves the canonical `unbounded-cloud.io/site` provider label.
