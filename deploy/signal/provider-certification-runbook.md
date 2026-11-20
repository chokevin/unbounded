# Project Signal provider certification runbook

This runbook takes an operator who is new to Project Signal from a clean
Kubernetes cluster to a monitored 24-hour, eight-GPU-per-node certification
campaign. It uses the repository-supported lifecycle and fails closed when the
target capacity, immutable source, or metrics path is not ready.

The public repository owns Project Signal, Argo integration, the whole-node
burn contract, the Prometheus and DCGM base, and validation gates. Each
organization owns its target cluster configuration, remote-write endpoint and
identity, Grafana assignment, credentials, and evidence retention.

Never commit kubeconfigs, private keys, tokens, internal endpoints, live
infrastructure identities, or completed deployment configuration.

## What `start` does

`ACTION=start` performs this ordered sequence:

1. Validate the exact source revision, immutable images, selected Nodes,
   Machine identity, `status.isolation: none`, eight-GPU inventory, free GPU
   capacity, jumpbox access, and metrics overlay.
2. Install pinned Argo Workflows and Project Signal.
3. Render, approve, bind, and install the repository-owned whole-node Test.
4. Install one central Prometheus collector and the pinned DCGM exporter.
5. Prove controller, GPU, and remote-write metrics are healthy.
6. Run a two-minute burn on every selected Node and require every Run to pass.
7. Launch one 24-hour burn on every selected Node.

The workflow stops before the 24-hour campaign when any prerequisite, smoke
Run, or required metric is unhealthy.

## Prerequisites

The operator workstation requires:

- a clean, reviewed checkout containing the provider-certification lifecycle
  and certification package;
- `bash`, `git`, `make`, `kubectl`, `helm`, `jq`, `yq`, `curl`, and `awk`;
- kubeconfig access to the target cluster, directly or through a jumpbox; and
- an organization-owned remote-write overlay and metrics identity.

Every selected worker must:

- be Ready and schedulable before Signal reserves it;
- be represented by exactly one Machine reporting `status.isolation: none`;
- expose exactly eight capacity and allocatable GPUs;
- carry `unbounded-cloud.io/site`;
- have no active Pod requesting GPUs; and
- have enough CPU, memory, and local storage for the configured burn.

Stop or reconfigure any external workload reconciler that can restore GPU
replicas while certification starts. Scaling a competing Deployment once is
not sufficient when another controller owns its desired state.

## Create a private workspace

```bash
workspace=/secure/path/project-signal-provider

make signal-provider-certification-init \
  SIGNAL_PROVIDER_CERTIFICATION_DIR="$workspace"
```

The initializer refuses to overwrite an existing workspace. It copies:

- `provider-certification.yaml`;
- the version-matched metrics base; and
- a private metrics overlay template.

## Complete `provider-certification.yaml`

Replace every placeholder.

| Field | Required value | How to verify |
|---|---|---|
| `metadata.name` | Stable, non-sensitive installation name | Reuse it for status and teardown |
| `spec.context` | Exact kubeconfig context | `kubectl --context <context> cluster-info` succeeds |
| `spec.cluster` | Stable identity used as the central `cluster` metric label | Must not be `cluster` or `default`; must exactly match the metrics overlay |
| `spec.environment` | Bounded environment classification | Must not contain a placeholder |
| `spec.sourceCommit` | Full 40-character controller and adapter source commit | Must identify the immutable images; it does not need to equal the lifecycle checkout HEAD |
| `spec.nodeSelector` | Smallest labels selecting all intended Nodes and no others | Review `kubectl get nodes -l '<selector>'` |
| `spec.providerLabel` | Canonical provider label | Must remain `unbounded-cloud.io/site` |
| `spec.expectedGPUCount` | GPUs per selected worker | Must remain `8` |
| `spec.expectedGPUProductRegex` | Anchored expression for the approved GPU product | Compare with selected Node inventory |
| `spec.cpuWorkers` | CPU stress workers | Keep within approved worker capacity |
| `spec.memoryMiB` | Memory integrity working set | Keep within approved worker capacity |
| `spec.storageMiB` | Storage integrity working set | Keep within approved worker capacity |
| `spec.sampleIntervalSeconds` | Qualification telemetry cadence | Use the reviewed default unless the Test contract changes |
| `spec.metricsOverlay` | Completed Kustomize overlay path | Rendered Prometheus config must contain remote-write and the exact cluster label |
| `spec.execution.ssh` | Jumpbox host, port, identity file, and optional existing control socket | Required only when the API is not directly reachable |
| `spec.images.controller` | Immutable controller image | Must use `@sha256:<digest>` and match `spec.sourceCommit` |
| `spec.images.adapter` | Immutable adapter image | Must use `@sha256:<digest>` and match `spec.sourceCommit` |

When `spec.execution.ssh` is configured, rendering and digest calculation stay
local. The lifecycle sends manifests over standard input and runs only
`kubectl`, Helm, `jq`, `curl`, and `awk` on the jumpbox. The jumpbox does not
need this repository or a Go toolchain.

## Complete the metrics overlay

Start from the generated `metrics-overlay/`. Set:

- the same cluster identity used by `spec.cluster`;
- the organization-owned remote-write URL;
- the approved authentication mechanism and Secret references;
- bounded queue settings; and
- any deliberate write relabeling.

Do not remove the controller, annotated Pod, Prometheus transport, or
provider-label derivation jobs. Filtering is allowed, but validation still
requires the controller target, transport metrics, and provider-attributed GPU
series for every selected GPU.

Create credentials or workload-identity bindings through the organization's
approved secret-management path. Do not place literal credentials in the
overlay or shell history.

Grafana is intentionally organization-owned. Record its instance, datasource
UID, folder, dashboard UID, dashboard revision, and rollback revision outside
this repository. Follow [the observability SOP](observability-sop.md) for the
required panels and acceptance gate.

## Run read-only preflight

```bash
config="$workspace/provider-certification.yaml"

make signal-provider-certification \
  ACTION=preflight \
  CONFIG="$config"
```

Do not continue until preflight passes. Correct the cluster or configuration;
do not weaken the host-native, eight-GPU, free-capacity, image, or metrics
requirements to get past the gate.

Common preflight failures:

| Failure | Required correction |
|---|---|
| No selected Nodes | Correct the selector or provider labels |
| Missing, duplicate, or isolated Machine | Reconcile Machine identity and require `status.isolation: none` |
| Node is unschedulable | Remove a non-Signal cordon before admission |
| GPU capacity or allocatable count differs | Fix device discovery or select the intended worker class |
| Active GPU Pod exists | Stop the workload owner and wait for its exact Pods to terminate |
| Generic or mismatched cluster label | Use the real bounded cluster identity in both config and metrics |
| Overlay still has placeholders | Complete the private overlay |
| Source checkout is dirty | Commit or remove source changes before mutating actions |

Read-only `status` does not require a clean checkout. Mutating actions do.

## Install, smoke-test, and launch

```bash
make signal-provider-certification \
  ACTION=start \
  CONFIG="$config"
```

`start` is the normal clean-cluster path. Use individual actions only when
diagnosing a phase:

```bash
make signal-provider-certification ACTION=install CONFIG="$config"
make signal-provider-certification ACTION=smoke CONFIG="$config"
make signal-provider-certification ACTION=run CONFIG="$config"
```

Standalone `smoke`, `run`, and `status` require an existing installation.

## Monitor

```bash
make signal-provider-certification \
  ACTION=status \
  CONFIG="$config"
```

The acceptance gate is:

- every smoke Run passed and released;
- one 24-hour Run is active for every selected Node;
- Signal owns each worker reservation and cordon;
- each burn Pod requests all eight GPUs;
- controller and GPU targets are up;
- remote-write has sent samples and has no non-recoverable failures;
- provider-attributed GPU series exist for every selected GPU; and
- the organization-owned dashboard agrees with Campaign, Run, stage, GPU, and
  transport state.

Save exact commands and detailed logs in the private evidence location. Shared
evidence must use role-based terms and omit infrastructure identities.

## Teardown and clean-room retry

Do not uninstall an active certification unless the campaign is intentionally
being abandoned.

```bash
make signal-provider-certification \
  ACTION=uninstall \
  CONFIG="$config"
```

Uninstall deletes CampaignPolicies before canceling and releasing Runs, waits
for Node release, removes the metrics overlay, uninstalls Argo before deleting
namespace resources, removes Signal CRDs and cluster RBAC, and removes only
Signal-owned labels and annotations from selected Nodes.

After teardown, verify:

1. Signal and Argo namespaces are absent;
2. Signal CRDs, Campaigns, Runs, Workflows, and cluster RBAC are absent;
3. selected Nodes are schedulable and no longer reserved;
4. organization-owned Grafana and transport assignments are removed when the
   cluster is being retired; and
5. displaced workloads are restored through their actual owner, not by
   creating an unmanaged replacement.

For a clean-room retry, return to this document with a new private workspace.
Do not reuse generated bindings, live object identities, or evidence from the
previous installation.
