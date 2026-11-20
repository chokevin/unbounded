# Project Signal observability integration

## Purpose and ownership boundary

Project Signal exposes Prometheus metrics and includes a namespace-local
Prometheus deployment for immediate troubleshooting. Production monitoring requires the user-selected observability system.

This open source document defines the portable integration contract:

- metric endpoints and discovery annotations;
- supported ADX-Mon and Prometheus remote-write integration patterns;
- stable cluster and node-provider label semantics;
- GPU exporter requirements; and
- backend-independent validation and failure handling.

The certification workspace must define:

- the subscription, resource group, and resource instances to use;
- the exact ADX-Mon, metrics backend, and Grafana endpoints;
- managed identities, tenants, role assignments, and Secret references;
- the cluster-to-dashboard and cluster-to-data-source assignments;
- immutable dashboard artifact revisions; and
- environment-specific rollout and rollback commands.

Do not add live deployment values to this repository. The integration contract
remains usable with any conforming deployment.

## Metric label contract

Central telemetry must add these bounded labels:

| Label | Value |
|---|---|
| `cluster` | The actual Kubernetes cluster identifier selected by the deployer |
| `provider` | The node's canonical `unbounded-cloud.io/site` label |
| `environment` | An optional bounded value such as `test` or `preproduction` |
| `signal_source_commit` | The full Project Signal source commit SHA |

`provider` is node-specific. Do not configure it as one global external label.
Do not use the pod Downward API to read `unbounded-cloud.io/site`; pod metadata
does not expose node labels. Enrich telemetry through Kubernetes node discovery
or a collector node-metadata watcher.

Node names and provider values are valid operational labels because node-level
certification and GPU telemetry require them. Redact those values from public
evidence. Never use credentials, endpoints, subscription or account
identifiers, or unbounded user-supplied values as metric labels.

## Upstream deployment

Render and apply the Signal package using immutable controller, adapter,
runner, resolver, and Prometheus image digests. Verify that it includes:

- `Service/project-signal-controller-metrics`;
- `ConfigMap/project-signal-prometheus-config`;
- `Deployment/project-signal-prometheus`; and
- `Service/project-signal-prometheus`.

The controller and live telemetry Pods expose both standard Prometheus and
ADX-Mon discovery annotations. The namespace-local Prometheus service has no
external ingress and stores only 24 hours or 1 GiB in ephemeral storage. It is
for local troubleshooting, not durable cross-cluster monitoring.

## ADX-Mon integration

Install ADX-Mon by following the
[ADX-Mon quick start](https://azure.github.io/adx-mon/quick-start/) or the
reviewed deployment process for your environment. The ADX-Mon collector
discovers the controller and live telemetry Pods through their
`adx-mon/scrape`, `adx-mon/port`, and `adx-mon/path` annotations.

Configure bounded cluster identity and Kubernetes node metadata in the
collector. The exact resource names and endpoints belong in the protected certification
workspace.

```toml
[add-labels]
  cluster = '<cluster-name>'
  environment = '<environment>'
  signal_source_commit = '<full-commit-sha>'

[metadata-watch.kubernetes-node]

[add-metadata-labels]
  [add-metadata-labels.kubernetes-node]
    [add-metadata-labels.kubernetes-node.labels]
      'unbounded-cloud.io/site' = 'provider'
```

See the
[ADX-Mon collector configuration](https://azure.github.io/adx-mon/config/)
for current syntax, filtering, WAL, authentication, and scrape settings.

GPU telemetry requires a DCGM exporter or another exporter that supplies the
dashboard's required GPU metric families. Annotate exporter Pods for ADX-Mon
discovery:

```yaml
metadata:
  annotations:
    adx-mon/scrape: "true"
    adx-mon/port: "9400"
    adx-mon/path: /metrics
```

Alternatively, configure a reviewed static scrape target in ADX-Mon. Pin the
exporter image by digest, apply the minimum required privileges, tolerate the
certification node taints, and verify one healthy target per GPU node.

## Prometheus remote-write integration

Deployments that use a Prometheus-compatible central backend should build a
Kustomize overlay on `deploy/signal/observability/base/`. The base installs one
central collector plus the pinned DCGM exporter and scrapes controller, live
workload, GPU, and its own transport metrics. The overlay must:

- replace the placeholder cluster identity with the actual cluster identifier;
- preserve the upstream controller and annotated Pod scrape jobs;
- add the standard labels from this document;
- derive `provider` from Kubernetes node discovery;
- load credentials from Secret references or workload identity;
- use bounded queue and retry settings; and
- expose sent, failed, retried, pending, and dropped sample metrics.

Backend URLs, identity IDs, tenant IDs, and role assignments belong only in the
user-owned certification workspace. Start from
`deploy/signal/observability/overlay.example/`. The provider certification
skill rejects overlays with placeholders, a missing `remote_write` block, a
cluster label that differs from its cluster config, or no provider-label
derivation.

## Deployment record

Before deploying, record these values with the certification evidence:

| Input | Requirement |
|---|---|
| Signal source revision | Full commit SHA |
| Controller and adapter images | Immutable digest references |
| Cluster identity | Exact value assigned to the `cluster` label |
| Node provider source | `unbounded-cloud.io/site` |
| Central metrics resources | Exact subscription and resource assignments |
| Metrics authentication | Identity and Secret references, never literal credentials |
| Grafana resources | Exact instance, data source, folder, and dashboard UID |
| Dashboard revision | Immutable dashboard artifact revision |
| Rollback revision | Last known-good transport and dashboard revisions |

The deployment record should contain the exact commands needed to configure
the approved resources. It must not redefine the public metric or label
contract.

## Validation procedure

Set the context and namespace without writing their values into shared evidence:

```bash
CTX=<certification-context>
NS=project-signal
```

Verify the runtime and local scraper:

```bash
kubectl --context "$CTX" -n "$NS" rollout status \
  deployment/project-signal-controller --timeout=5m
kubectl --context "$CTX" -n "$NS" rollout status \
  deployment/project-signal-prometheus --timeout=5m
kubectl --context "$CTX" -n "$NS" get service \
  project-signal-controller-metrics project-signal-prometheus
```

Query the local Prometheus API from inside the cluster:

```bash
kubectl --context "$CTX" -n "$NS" run signal-metrics-check \
  --image=curlimages/curl@sha256:<approved-digest> \
  --restart=Never --rm -i -- \
  --fail --silent --show-error --get \
  --data-urlencode 'query=up{job="project-signal-controller"}' \
  http://project-signal-prometheus:9090/api/v1/query
```

The result must contain one healthy controller target. Then verify these metric
families are queryable locally and centrally:

- `project_signal_fleet_node_certification_status`
- `project_signal_fleet_certification_live_stage_verdict`
- `project_signal_fleet_certification_measurement`
- `project_signal_certification_campaign_info`
- `project_signal_certification_campaign_runs`
- `project_signal_certification_metric`
- `project_signal_certification_metric_sample`

Live workload series may be absent when no stage is running. Node status and
controller target health must still be present.

Verify central transport health using the transport implementation's sent,
failed, retried, pending, and dropped sample metrics. Require:

- successful samples are increasing;
- non-recoverable failed samples are zero during the validation window;
- dropped samples are interpreted with the configured write relabeling because
  intentional filtering increments the same counter;
- pending samples return to zero; and
- the standard external labels are present.

Run one short certification smoke and confirm:

1. the Run appears in controller metrics;
2. the current stage changes while the workload is active;
3. live workload metrics appear when declared by the Test;
4. terminal status and completed measurements appear after completion; and
5. the configured Grafana dashboard shows the same cluster identity and Run
   state.

Save detailed command output in the protected validation record. PR-facing
evidence must redact cluster, node, and provider values.

## Acceptance gate

A certification cluster is observability-ready only when all of these are true:

- Signal controller and local Prometheus rollouts are healthy.
- Local controller scrape health is `1`.
- Central transport has no non-recoverable failed samples, and every required
  controller and provider-attributed GPU series is present.
- Central queries return the configured cluster identity and Signal source
  commit.
- The approved dashboard revision renders node, Run, stage, campaign, and
  measurement data.
- A smoke Run is visible from admission through terminal release.
- No credential, internal endpoint, cluster identity, node name, or provider
  value appears in public artifacts.

Do not start a long certification or soak campaign before this gate passes.

## Failure handling

| Symptom | First check | Required action |
|---|---|---|
| Controller target is down locally | Service selector, Pod readiness, port 8080 | Fix the upstream deployment before checking Grafana |
| Local metrics work but central metrics are absent | Transport authentication, queue, TLS, and endpoint | Fix the certification workspace overlay; do not alter Signal metric code |
| Central metrics exist but the dashboard is empty | Data source, dashboard revision, cluster and provider variables | Fix the configured Grafana resources |
| Provider is empty | Node metadata watcher and canonical site label | Derive it from `unbounded-cloud.io/site`; do not use pod Downward API |
| Dashboard joins return duplicates | Empty or stale provider series | Exclude empty provider values and use a non-empty All matcher |
| Live metrics disappear after a stage | Whether the workload exited | Expected for ephemeral live series; use completed measurements and evidence |
| Historical metrics disappear locally | Prometheus Pod restart or retention limit | Use the central backend; local storage is intentionally ephemeral |
| Label cardinality grows unexpectedly | External labels and Test dimensions | Stop rollout and remove unbounded or sensitive labels |

## Upgrade and teardown

For upgrades, validate a smoke Run after changing any Signal image digest,
Prometheus image, transport configuration, dashboard revision, or metric
contract.

For teardown:

1. stop new certification admission;
2. wait for active Runs to release;
3. confirm the central backend has received terminal metrics;
4. remove the configured transport and dashboard assignment;
5. remove the Signal deployment; and
6. retain the deployment ledger and durable certification evidence
   according to policy.

