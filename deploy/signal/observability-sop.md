# Project Signal certification cluster observability SOP

## Purpose

Use this SOP for every internal certification cluster so Project Signal metrics
reach the same central metrics backend and Grafana dashboards without changing
the upstream Signal runtime.

The standard has two ownership layers:

- The upstream Signal deployment emits metrics and deploys the
  namespace-local `project-signal-prometheus` scraper.
- The internal deployment layer owns central metrics transport, credentials,
  cluster aliases, Grafana data sources, dashboards, and alerts.

Do not commit internal endpoints, credentials, cluster names, node names, or
dashboard JSON to this repository.

## Required inputs

Record these values in the internal deployment system before deployment:

| Input | Requirement |
|---|---|
| Signal source revision | Full commit SHA |
| Controller and adapter images | Immutable digest references |
| Certification cluster alias | Stable, non-sensitive identifier |
| Environment | Bounded value such as `test` or `preproduction` |
| Central metrics endpoint | Internal configuration only |
| Metrics credentials | Secret reference, never a literal in manifests |
| Dashboard revision | Immutable internal artifact revision |

Use the same bounded external labels on every cluster:

```yaml
certification_cluster: <non-sensitive-alias>
environment: <environment>
signal_source_commit: <full-commit-sha>
```

Do not use node names, infrastructure provider names, subscription or account
identifiers, or unbounded user-supplied values as metric labels.

## Deployment procedure

1. Render and apply the upstream Signal package using immutable controller,
   adapter, runner, resolver, and Prometheus image digests.
2. Verify the base package includes:
   - `Service/project-signal-controller-metrics`;
   - `ConfigMap/project-signal-prometheus-config`;
   - `Deployment/project-signal-prometheus`; and
   - `Service/project-signal-prometheus`.
3. Apply the internal metrics transport overlay. It must:
   - preserve the upstream scrape jobs;
   - send metrics to the approved central metrics backend;
   - load credentials from a namespace-scoped Secret;
   - add only the standard external labels above;
   - use bounded retry and queue settings; and
   - fail visibly instead of silently dropping samples.
4. Provision or update the internal Grafana data source against the central
   metrics backend.
5. Provision the approved internal Project Signal dashboard revision.
6. Record the Signal source revision, image digests, dashboard revision, and
   cluster alias in the internal deployment ledger.

The namespace-local Prometheus service is not the cross-cluster integration
boundary. It has no external ingress and stores only 24 hours or 1 GiB in
ephemeral storage. Central transport is required for durable multi-cluster
dashboards and comparisons.

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

- `project_signal_node_certification_status`
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
- failed and dropped samples are zero during the validation window;
- pending samples return to zero; and
- the standard external labels are present.

Run one short certification smoke and confirm:

1. the Run appears in controller metrics;
2. the current stage changes while the workload is active;
3. live workload metrics appear when declared by the Test;
4. terminal status and completed measurements appear after completion; and
5. the internal Grafana dashboard shows the same cluster alias and Run state.

Save detailed command output in the internal validation record. PR-facing
evidence must use the non-sensitive cluster alias and redact node names.

## Acceptance gate

A certification cluster is observability-ready only when all of these are true:

- Signal controller and local Prometheus rollouts are healthy.
- Local controller scrape health is `1`.
- Central transport has no failed or dropped samples.
- Central queries return the cluster alias and Signal source commit.
- The approved dashboard revision renders node, Run, stage, campaign, and
  measurement data.
- A smoke Run is visible from admission through terminal release.
- No credential, internal endpoint, node name, or infrastructure provider name
  appears in public artifacts.

Do not start a long certification or soak campaign before this gate passes.

## Failure handling

| Symptom | First check | Required action |
|---|---|---|
| Controller target is down locally | Service selector, Pod readiness, port 8080 | Fix the upstream deployment before checking Grafana |
| Local metrics work but central metrics are absent | Transport authentication, queue, TLS, and endpoint | Fix the internal overlay; do not alter Signal metric code |
| Central metrics exist but the dashboard is empty | Data source, dashboard revision, cluster alias variables | Fix the internal Grafana layer |
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
4. remove the internal transport and dashboard assignment;
5. remove the Signal deployment; and
6. retain the internal deployment ledger and durable certification evidence
   according to policy.

