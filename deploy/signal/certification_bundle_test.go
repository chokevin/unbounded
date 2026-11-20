// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signal

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

func TestWholeNodeCertificationBundle(t *testing.T) {
	t.Parallel()

	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}

	renderedPath := filepath.Join(t.TempDir(), "tests.yaml")
	command := exec.Command(
		"go",
		"run",
		"./cmd/kubectl-unbounded",
		"signal",
		"render",
		"--tests-only",
		"--filename",
		"deploy/signal/certification/whole-node-24h/certification-plan.yaml",
	)
	command.Dir = repoRoot

	rendered, err := command.Output()
	if err != nil {
		t.Fatalf("render whole-node certification bundle: %v", err)
	}

	if err := os.WriteFile(renderedPath, rendered, 0o600); err != nil {
		t.Fatal(err)
	}

	objects := decodeCertificationBundle(t, rendered)
	workflow := findCertificationObject(t, objects, "WorkflowTemplate", "project-signal-whole-node-burn-v1")
	test := findCertificationObject(t, objects, "CertificationTest", "project-signal-whole-node-burn-v1")

	assertWorkflowArtifactMaterialization(t, workflow)
	assertWorkflowQualificationPublishing(t, workflow)
	assertTestArtifactDigest(t, test)

	materializer := workflowMaterializerCommand(t, workflow)
	testMaterializer(t, materializer)
}

func TestProviderCertificationInitCreatesPortableWorkspace(t *testing.T) {
	t.Parallel()

	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}

	target := filepath.Join(t.TempDir(), "project-signal")
	command := exec.Command(
		"bash",
		filepath.Join(
			repoRoot,
			"hack/agent/skills/project-signal-provider-certification/scripts/init.sh",
		),
		target,
	)

	command.Dir = repoRoot
	output, runErr := command.CombinedOutput()
	if runErr != nil {
		t.Fatalf("initialize provider certification workspace: %v\n%s", runErr, output)
	}

	if !strings.Contains(string(output), "ACTION=preflight") {
		t.Fatalf("initializer does not direct operators through read-only preflight:\n%s", output)
	}

	config, err := os.ReadFile(filepath.Join(target, "provider-certification.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(config), "metricsOverlay: metrics-overlay") {
		t.Fatal("generated config does not reference its local metrics overlay")
	}

	kustomization, err := os.ReadFile(filepath.Join(target, "metrics-overlay", "kustomization.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(kustomization), "../metrics-base") {
		t.Fatal("generated metrics overlay does not reference the copied metrics base")
	}

	if _, err := os.Stat(filepath.Join(target, "metrics-base", "kustomization.yaml")); err != nil {
		t.Fatalf("generated workspace does not contain its metrics base: %v", err)
	}

	second := exec.Command(
		"bash",
		filepath.Join(
			repoRoot,
			"hack/agent/skills/project-signal-provider-certification/scripts/init.sh",
		),
		target,
	)

	second.Dir = repoRoot
	if output, runErr := second.CombinedOutput(); runErr == nil {
		t.Fatalf("initializer overwrote an existing workspace:\n%s", output)
	}
}

func TestProviderCertificationRunbookDocumentsCompleteLifecycle(t *testing.T) {
	t.Parallel()

	runbook, err := os.ReadFile("provider-certification-runbook.md")
	if err != nil {
		t.Fatal(err)
	}

	required := []string{
		"spec.context",
		"spec.cluster",
		"spec.environment",
		"spec.sourceCommit",
		"spec.nodeSelector",
		"spec.providerLabel",
		"spec.expectedGPUCount",
		"spec.expectedGPUProductRegex",
		"spec.cpuWorkers",
		"spec.memoryMiB",
		"spec.storageMiB",
		"spec.sampleIntervalSeconds",
		"spec.metricsOverlay",
		"spec.execution.ssh",
		"spec.images.controller",
		"spec.images.adapter",
		"ACTION=preflight",
		"ACTION=start",
		"ACTION=status",
		"ACTION=uninstall",
		"status.isolation: none",
		"remote-write",
		"Grafana",
		"clean-room retry",
	}
	for _, value := range required {
		if !strings.Contains(string(runbook), value) {
			t.Errorf("provider certification runbook does not document %q", value)
		}
	}
}

func TestProviderCertificationPreflightDiscoversSelectedNodes(t *testing.T) {
	t.Parallel()

	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}

	tempDir := t.TempDir()
	binDir := filepath.Join(tempDir, "bin")
	metricsDir := filepath.Join(tempDir, "metrics")

	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(metricsDir, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(metricsDir, "kustomization.yaml"), []byte(
		"apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\n",
	), 0o600); err != nil {
		t.Fatal(err)
	}

	writeExecutable(t, filepath.Join(binDir, "git"), `#!/usr/bin/env bash
exit 0
`)
	writeExecutable(t, filepath.Join(binDir, "kubectl"), `#!/usr/bin/env bash
set -euo pipefail

case "$*" in
  *"config current-context")
    printf 'preflight must not depend on the current context\n' >&2
    exit 42
    ;;
  *" version --request-timeout=15s")
    printf 'ok\n'
    ;;
  *" get nodes "*)
    if [[ "${MOCK_NODE_COUNT:-2}" == "0" ]]; then
      printf '{"items":[]}\n'
      exit
    fi
    cat <<'EOF'
{"items":[
  {"metadata":{"name":"provider-node-a","labels":{"unbounded-cloud.io/site":"provider-a"}},"spec":{"unschedulable":false},"status":{"conditions":[{"type":"Ready","status":"True"}],"capacity":{"nvidia.com/gpu":"8"},"allocatable":{"nvidia.com/gpu":"8"}}},
  {"metadata":{"name":"provider-node-b","labels":{"unbounded-cloud.io/site":"provider-a"}},"spec":{"unschedulable":false},"status":{"conditions":[{"type":"Ready","status":"True"}],"capacity":{"nvidia.com/gpu":"8"},"allocatable":{"nvidia.com/gpu":"8"}}}
]}
EOF
    ;;
  *" get machines -o json")
    if [[ "${MOCK_MISSING_MACHINE:-false}" == "true" ]]; then
      printf '{"items":[]}\n'
      exit
    fi
    cat <<EOF
{"items":[
  {"metadata":{"name":"provider-machine-a"},"spec":{"kubernetes":{"nodeRef":{"name":"provider-node-a"}}},"status":{"isolation":"${MOCK_ISOLATION_MODE:-none}"}},
  {"metadata":{"name":"provider-machine-b"},"spec":{"kubernetes":{"nodeRef":{"name":"provider-node-b"}}},"status":{"isolation":"${MOCK_ISOLATION_MODE:-none}"}}
]}
EOF
    ;;
  *" get pods -A -o json")
    if [[ "${MOCK_ACTIVE_GPU_WORKLOAD:-false}" == "true" ]]; then
      cat <<'EOF'
{"items":[{"metadata":{"namespace":"serving","name":"busy"},"spec":{"nodeName":"provider-node-a","containers":[{"resources":{"requests":{"nvidia.com/gpu":"8"}}}]},"status":{"phase":"Running"}}]}
EOF
    else
      printf '{"items":[]}\n'
    fi
    ;;
  kustomize*)
    cat <<'EOF'
apiVersion: v1
kind: ConfigMap
metadata:
  name: project-signal-central-prometheus
data:
  prometheus.yml: |
    global:
      external_labels:
        cluster: test-cluster
    scrape_configs:
      - job_name: test
        metric_relabel_configs:
          - target_label: unbounded_cloud_io_site
    remote_write:
      - url: https://metrics.example.invalid
EOF
    ;;
  *)
    printf 'unexpected kubectl invocation: %s\n' "$*" >&2
    exit 1
    ;;
esac
`)

	configPath := filepath.Join(tempDir, "config.yaml")

	config := fmt.Sprintf(`apiVersion: signal.unbounded-cloud.io/v1alpha1
kind: CertificationDeployment
metadata:
  name: dynamic-provider-test
spec:
  context: test-context
  cluster: test-cluster
  environment: certification
  sourceCommit: %s
  nodeSelector:
    unbounded-cloud.io/site: provider-a
  providerLabel: unbounded-cloud.io/site
  expectedGPUCount: 8
  expectedGPUProductRegex: "^NVIDIA B200$"
  metricsOverlay: %s
  images:
    controller: example.invalid/controller@sha256:%s
    adapter: example.invalid/adapter@sha256:%s
`, strings.Repeat("c", 40), metricsDir, strings.Repeat("a", 64), strings.Repeat("b", 64))
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}

	script := filepath.Join(
		repoRoot,
		"hack/agent/skills/project-signal-provider-certification/scripts/project-signal-certification.sh",
	)
	command := exec.Command(script, "preflight", configPath)
	command.Dir = repoRoot

	command.Env = append(os.Environ(), "PATH="+binDir+":"+os.Getenv("PATH"))

	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("preflight two discovered Nodes: %v\n%s", err, output)
	}

	if !strings.Contains(string(output), "nodes=2") {
		t.Fatalf("preflight output does not report discovered Nodes:\n%s", output)
	}

	command = exec.Command(script, "preflight", configPath)
	command.Dir = repoRoot

	command.Env = append(
		os.Environ(),
		"PATH="+binDir+":"+os.Getenv("PATH"),
		"MOCK_NODE_COUNT=0",
	)

	output, err = command.CombinedOutput()
	if err == nil {
		t.Fatalf("preflight unexpectedly accepted an empty provider selector:\n%s", output)
	}

	if !strings.Contains(string(output), "did not match any provider Nodes") {
		t.Fatalf("empty-selector failure is unclear:\n%s", output)
	}

	command = exec.Command(script, "preflight", configPath)
	command.Dir = repoRoot

	command.Env = append(
		os.Environ(),
		"PATH="+binDir+":"+os.Getenv("PATH"),
		"MOCK_ACTIVE_GPU_WORKLOAD=true",
	)

	output, err = command.CombinedOutput()
	if err == nil {
		t.Fatalf("preflight unexpectedly accepted an active GPU workload:\n%s", output)
	}

	if !strings.Contains(string(output), "provider Nodes have active GPU workloads") {
		t.Fatalf("active GPU workload failure is unclear:\n%s", output)
	}

	command = exec.Command(script, "preflight", configPath)
	command.Dir = repoRoot

	command.Env = append(
		os.Environ(),
		"PATH="+binDir+":"+os.Getenv("PATH"),
		"MOCK_ISOLATION_MODE=nspawn",
	)

	output, err = command.CombinedOutput()
	if err == nil {
		t.Fatalf("preflight unexpectedly accepted nspawn isolation:\n%s", output)
	}

	if !strings.Contains(string(output), "status.isolation=none") {
		t.Fatalf("nspawn isolation failure is unclear:\n%s", output)
	}

	command = exec.Command(script, "preflight", configPath)
	command.Dir = repoRoot

	command.Env = append(
		os.Environ(),
		"PATH="+binDir+":"+os.Getenv("PATH"),
		"MOCK_MISSING_MACHINE=true",
	)

	output, err = command.CombinedOutput()
	if err == nil {
		t.Fatalf("preflight unexpectedly accepted a Node without a Machine:\n%s", output)
	}

	if !strings.Contains(string(output), "Machine not found") {
		t.Fatalf("missing Machine failure is unclear:\n%s", output)
	}
}

func TestProviderCertificationBindingsUseConfiguredContext(t *testing.T) {
	t.Parallel()

	script, err := os.ReadFile(filepath.Join(
		"..",
		"..",
		"hack/agent/skills/project-signal-provider-certification/scripts/project-signal-certification.sh",
	))
	if err != nil {
		t.Fatal(err)
	}

	bindingsStart := strings.Index(string(script), "go run ./cmd/kubectl-unbounded signal bindings")
	if bindingsStart == -1 {
		t.Fatal("Signal bindings invocation not found")
	}

	bindingsEnd := strings.Index(string(script)[bindingsStart:], `--filename "${plan}"`)
	if bindingsEnd == -1 {
		t.Fatal("Signal bindings filename argument not found")
	}

	invocation := string(script)[bindingsStart : bindingsStart+bindingsEnd]
	if !strings.Contains(invocation, `--context "${CONTEXT}"`) {
		t.Fatal("Signal bindings can silently use the kubeconfig current context")
	}
}

func TestProviderCertificationAllowsIntentionalRemoteWriteFiltering(t *testing.T) {
	t.Parallel()

	script, err := os.ReadFile(filepath.Join(
		"..",
		"..",
		"hack/agent/skills/project-signal-provider-certification/scripts/project-signal-certification.sh",
	))
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(string(script), "prometheus_remote_storage_samples_dropped_total") {
		t.Fatal("remote-write validation rejects samples intentionally filtered by write relabeling")
	}

	if !strings.Contains(string(script), "prometheus_remote_storage_samples_failed_total") {
		t.Fatal("remote-write validation no longer checks non-recoverable failures")
	}
}

func TestProviderCertificationMetricsRequireInstallation(t *testing.T) {
	t.Parallel()

	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}

	binDir := t.TempDir()
	writeExecutable(t, filepath.Join(binDir, "kubectl"), `#!/usr/bin/env bash
exit 1
`)

	script := filepath.Join(
		repoRoot,
		"hack/agent/skills/project-signal-provider-certification/scripts/project-signal-certification.sh",
	)
	command := exec.Command(
		"bash",
		"-c",
		`source "$1"; CONTEXT=test-context; REMOTE_HOST=""; WORK_DIR="$2"; validate_metrics`,
		"bash",
		script,
		t.TempDir(),
	)
	command.Dir = repoRoot
	command.Env = append(
		os.Environ(),
		"PROJECT_SIGNAL_LIB_ONLY=true",
		"PATH="+binDir+":"+os.Getenv("PATH"),
	)

	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatalf("metrics validation unexpectedly accepted a missing installation:\n%s", output)
	}

	if !strings.Contains(string(output), "run ACTION=install or ACTION=start first") {
		t.Fatalf("missing-installation failure is unclear:\n%s", output)
	}
}

func TestProviderCertificationMetricsCleanUpFailedPortForward(t *testing.T) {
	t.Parallel()

	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}

	tempDir := t.TempDir()
	binDir := filepath.Join(tempDir, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}

	pidFile := filepath.Join(tempDir, "port-forward.pid")
	writeExecutable(t, filepath.Join(binDir, "kubectl"), `#!/usr/bin/env bash
set -euo pipefail

case "$*" in
  *" get namespace project-signal"|*" get service project-signal-central-prometheus")
    exit 0
    ;;
  *" port-forward service/project-signal-central-prometheus 19090:9090")
    printf '%s\n' "$$" >"${MOCK_PID_FILE}"
    sleep 300
    ;;
  *)
    printf 'unexpected kubectl invocation: %s\n' "$*" >&2
    exit 1
    ;;
esac
`)
	writeExecutable(t, filepath.Join(binDir, "curl"), `#!/usr/bin/env bash
exit 1
`)
	writeExecutable(t, filepath.Join(binDir, "seq"), `#!/usr/bin/env bash
printf '1\n'
`)

	script := filepath.Join(
		repoRoot,
		"hack/agent/skills/project-signal-provider-certification/scripts/project-signal-certification.sh",
	)
	command := exec.Command(
		"bash",
		"-c",
		`source "$1"; CONTEXT=test-context; REMOTE_HOST=""; WORK_DIR="$2"; validate_metrics`,
		"bash",
		script,
		tempDir,
	)
	command.Dir = repoRoot
	command.Env = append(
		os.Environ(),
		"PROJECT_SIGNAL_LIB_ONLY=true",
		"MOCK_PID_FILE="+pidFile,
		"PATH="+binDir+":"+os.Getenv("PATH"),
	)

	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatalf("metrics validation unexpectedly succeeded:\n%s", output)
	}

	pidBytes, readErr := os.ReadFile(pidFile)
	if readErr != nil {
		t.Fatalf("read port-forward PID: %v\n%s", readErr, output)
	}

	pid := strings.TrimSpace(string(pidBytes))
	if pid == "" {
		t.Fatalf("mock port-forward did not record its PID:\n%s", output)
	}

	if processErr := exec.Command("kill", "-0", pid).Run(); processErr == nil {
		_ = exec.Command("kill", pid).Run()
		t.Fatalf("failed metrics validation leaked port-forward process %s", pid)
	}
}

func TestProviderCertificationStatusDoesNotRequireCleanSource(t *testing.T) {
	t.Parallel()

	script, err := os.ReadFile(filepath.Join(
		"..",
		"..",
		"hack/agent/skills/project-signal-provider-certification/scripts/project-signal-certification.sh",
	))
	if err != nil {
		t.Fatal(err)
	}

	source := string(script)
	statusStart := strings.Index(source, "status() {")
	if statusStart == -1 {
		t.Fatal("status function not found")
	}

	statusEnd := strings.Index(source[statusStart:], "\n}\n")
	if statusEnd == -1 {
		t.Fatal("status function end not found")
	}

	status := source[statusStart : statusStart+statusEnd]
	if !strings.Contains(status, "preflight_config") {
		t.Fatal("status no longer validates its deployment configuration")
	}

	if strings.Contains(status, "verify_certification_source") {
		t.Fatal("read-only status requires a clean certification source checkout")
	}

	preflightStart := strings.Index(source, "preflight() {")
	if preflightStart == -1 {
		t.Fatal("preflight function not found")
	}

	preflightEnd := strings.Index(source[preflightStart:], "\n}\n")
	if preflightEnd == -1 {
		t.Fatal("preflight function end not found")
	}

	preflight := source[preflightStart : preflightStart+preflightEnd]
	if !strings.Contains(preflight, "verify_certification_source") {
		t.Fatal("mutating certification lifecycle no longer verifies clean source")
	}
}

func TestProviderCertificationHidesGenericArgoNotes(t *testing.T) {
	t.Parallel()

	script, err := os.ReadFile(filepath.Join(
		"..",
		"..",
		"hack/agent/skills/project-signal-provider-certification/scripts/project-signal-certification.sh",
	))
	if err != nil {
		t.Fatal(err)
	}

	if count := strings.Count(string(script), "--hide-notes"); count != 2 {
		t.Fatalf("expected local and remote Argo installs to hide generic chart notes, found %d", count)
	}
}

func TestProviderCertificationUninstallStopsCampaignsBeforeRuns(t *testing.T) {
	t.Parallel()

	script, err := os.ReadFile(filepath.Join(
		"..",
		"..",
		"hack/agent/skills/project-signal-provider-certification/scripts/project-signal-certification.sh",
	))
	if err != nil {
		t.Fatal(err)
	}

	source := string(script)
	uninstallStart := strings.Index(source, "uninstall_all() {")
	if uninstallStart == -1 {
		t.Fatal("uninstall function not found")
	}

	uninstallEnd := strings.Index(source[uninstallStart:], "\n}\n")
	if uninstallEnd == -1 {
		t.Fatal("uninstall function end not found")
	}

	uninstall := source[uninstallStart : uninstallStart+uninstallEnd]
	deleteCampaigns := strings.Index(uninstall, "delete certificationcampaigns --all")
	cancelRuns := strings.Index(uninstall, "cancel_and_release_runs")
	if deleteCampaigns == -1 || cancelRuns == -1 {
		t.Fatal("uninstall campaign or Run cleanup step not found")
	}

	if deleteCampaigns > cancelRuns {
		t.Fatal("uninstall can let an active Campaign replace a canceled Run")
	}

	uninstallArgo := strings.Index(uninstall, "helm uninstall project-signal-argo")
	deleteMetrics := strings.Index(uninstall, `kube_delete_file "${WORK_DIR}/metrics-overlay.yaml"`)
	deleteRuntime := strings.Index(uninstall, `kube_delete_directory "${RENDERED_DIR}"`)
	if uninstallArgo == -1 || deleteMetrics == -1 || deleteRuntime == -1 {
		t.Fatal("uninstall Argo, metrics, or runtime cleanup step not found")
	}

	if uninstallArgo > deleteMetrics || uninstallArgo > deleteRuntime {
		t.Fatal("uninstall deletes Helm release metadata before uninstalling Argo")
	}
}

func TestProviderCertificationAuthorityUsesImageSourceCommit(t *testing.T) {
	t.Parallel()

	script, err := os.ReadFile(filepath.Join(
		"..",
		"..",
		"hack/agent/skills/project-signal-provider-certification/scripts/project-signal-certification.sh",
	))
	if err != nil {
		t.Fatal(err)
	}

	authorityStart := strings.Index(string(script), `if [[ "${phase}" == "authority" ]]`)
	if authorityStart == -1 {
		t.Fatal("authority render phase not found")
	}

	authorityEnd := strings.Index(string(script)[authorityStart:], `elif [[ "${phase}"`)
	if authorityEnd == -1 {
		t.Fatal("authority render phase end not found")
	}

	authority := string(script)[authorityStart : authorityStart+authorityEnd]
	if !strings.Contains(authority, `source_commit="${SOURCE_COMMIT}"`) {
		t.Fatal("execution authority does not use the source commit embedded in the images")
	}

	if strings.Contains(authority, "git -C") {
		t.Fatal("execution authority incorrectly uses the lifecycle checkout commit")
	}
}

func TestProviderCertificationResetsOnlyEmptyExpiredSmokeCampaigns(t *testing.T) {
	t.Parallel()

	script, err := os.ReadFile(filepath.Join(
		"..",
		"..",
		"hack/agent/skills/project-signal-provider-certification/scripts/project-signal-certification.sh",
	))
	if err != nil {
		t.Fatal(err)
	}

	source := string(script)
	if !strings.Contains(source, `.status.phase == "Completed"`) {
		t.Fatal("smoke retry does not require the prior campaign to be completed")
	}

	for _, field := range []string{
		".status.succeededRuns",
		".status.failedRuns",
		".status.inconclusiveRuns",
		".status.canceledRuns",
	} {
		if !strings.Contains(source, field) {
			t.Fatalf("smoke retry does not guard %s", field)
		}
	}

	if !strings.Contains(source, "reset empty smoke campaigns whose admission window expired") {
		t.Fatal("empty expired smoke campaign reset is missing")
	}
}

func TestProviderCertificationAppliesCompiledPackageToSignalNamespace(t *testing.T) {
	t.Parallel()

	script, err := os.ReadFile(filepath.Join(
		"..",
		"..",
		"hack/agent/skills/project-signal-provider-certification/scripts/project-signal-certification.sh",
	))
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(script), `kube_apply_file "${package}" "${namespace}"`) {
		t.Fatal("compiled certification package is not applied to the Signal namespace")
	}
}

func TestWholeNodeBurnProducesQualificationFingerprint(t *testing.T) {
	t.Parallel()

	script, err := os.ReadFile(filepath.Join(
		"certification/whole-node-24h/gpu-burn-canary.sh",
	))
	if err != nil {
		t.Fatal(err)
	}

	source := string(script)
	for _, required := range []string{
		"--query-gpu=driver_version",
		"gpuInventoryDigest",
		"visibleDevices",
		"coveredDevices",
		"/results/qualification.json",
	} {
		if !strings.Contains(source, required) {
			t.Fatalf("qualification fingerprint is missing %q", required)
		}
	}
}

func TestWholeNodeBurnCommandsInvokeScriptsOnSeparateLines(t *testing.T) {
	t.Parallel()

	manifest, err := os.ReadFile(filepath.Join(
		"certification/whole-node-24h/workflow-template.yaml",
	))
	if err != nil {
		t.Fatal(err)
	}

	objects := decodeCertificationBundle(t, manifest)
	if len(objects) != 1 {
		t.Fatalf("got %d workflow objects, want 1", len(objects))
	}

	templates, found, err := unstructured.NestedSlice(objects[0].Object, "spec", "templates")
	if err != nil {
		t.Fatal(err)
	}

	if !found {
		t.Fatal("workflow templates are missing")
	}

	wantComponents := map[string]string{
		"main":              "gpu-burn",
		"cpu-stress":        "cpu-stress",
		"memory-integrity":  "memory-integrity",
		"storage-integrity": "storage-integrity",
	}
	checked := 0

	for _, templateValue := range templates {
		template, ok := templateValue.(map[string]any)
		if !ok {
			continue
		}

		containers, found, err := unstructured.NestedSlice(template, "containerSet", "containers")
		if err != nil {
			t.Fatal(err)
		}

		if !found {
			continue
		}

		for _, containerValue := range containers {
			container, ok := containerValue.(map[string]any)
			if !ok {
				t.Fatal("workflow container has an invalid shape")
			}

			name, _, _ := unstructured.NestedString(container, "name")

			component, ok := wantComponents[name]
			if !ok {
				continue
			}

			args, found, err := unstructured.NestedStringSlice(container, "args")
			if err != nil {
				t.Fatal(err)
			}

			if !found || len(args) != 1 {
				t.Fatalf("container %q has invalid shell arguments", name)
			}

			want := ". /scripts/parameters.env\n/scripts/gpu-burn-canary.sh " + component
			if !strings.Contains(args[0], want) {
				t.Fatalf("container %q does not invoke its script on a separate line", name)
			}

			checked++
		}
	}

	if checked != len(wantComponents) {
		t.Fatalf("checked %d burn containers, want %d", checked, len(wantComponents))
	}
}

func TestWholeNodeBurnOutputsDoNotOverlapSharedVolume(t *testing.T) {
	t.Parallel()

	manifest, err := os.ReadFile(filepath.Join(
		"certification/whole-node-24h/workflow-template.yaml",
	))
	if err != nil {
		t.Fatal(err)
	}

	objects := decodeCertificationBundle(t, manifest)
	if len(objects) != 1 {
		t.Fatalf("got %d workflow objects, want 1", len(objects))
	}

	templates, found, err := unstructured.NestedSlice(objects[0].Object, "spec", "templates")
	if err != nil {
		t.Fatal(err)
	}

	if !found {
		t.Fatal("workflow templates are missing")
	}

	wantPaths := map[string]string{
		"verdict":       "/tmp/project-signal-output/verdict",
		"reason":        "/tmp/project-signal-output/reason",
		"exit-code":     "/tmp/project-signal-output/gpu-burn-exit-code",
		"qualification": "/tmp/project-signal-output/qualification.json",
	}
	checked := 0

	for _, templateValue := range templates {
		template, ok := templateValue.(map[string]any)
		if !ok {
			continue
		}

		name, _, _ := unstructured.NestedString(template, "name")
		if name != "burn" {
			continue
		}

		parameters, found, err := unstructured.NestedSlice(template, "outputs", "parameters")
		if err != nil {
			t.Fatal(err)
		}

		if !found {
			t.Fatal("burn output parameters are missing")
		}

		for _, parameterValue := range parameters {
			parameter, ok := parameterValue.(map[string]any)
			if !ok {
				t.Fatal("burn output parameter has an invalid shape")
			}

			parameterName, _, _ := unstructured.NestedString(parameter, "name")

			wantPath, ok := wantPaths[parameterName]
			if !ok {
				continue
			}

			path, _, _ := unstructured.NestedString(parameter, "valueFrom", "path")
			if path != wantPath {
				t.Fatalf("output %q path = %q, want %q", parameterName, path, wantPath)
			}

			if strings.HasPrefix(path, "/results/") {
				t.Fatalf("output %q overlaps the shared results volume", parameterName)
			}

			checked++
		}
	}

	if checked != len(wantPaths) {
		t.Fatalf("checked %d burn outputs, want %d", checked, len(wantPaths))
	}
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(content), 0o700); err != nil {
		t.Fatal(err)
	}
}

func decodeCertificationBundle(t *testing.T, rendered []byte) []unstructured.Unstructured {
	t.Helper()

	decoder := utilyaml.NewYAMLOrJSONDecoder(strings.NewReader(string(rendered)), 64*1024)
	objects := make([]unstructured.Unstructured, 0, 2)

	for {
		var object unstructured.Unstructured

		err := decoder.Decode(&object.Object)
		if errors.Is(err, io.EOF) {
			return objects
		}

		if err != nil {
			t.Fatalf("decode certification bundle: %v", err)
		}

		if len(object.Object) != 0 {
			objects = append(objects, object)
		}
	}
}

func findCertificationObject(
	t *testing.T,
	objects []unstructured.Unstructured,
	kind string,
	name string,
) unstructured.Unstructured {
	t.Helper()

	for index := range objects {
		if objects[index].GetKind() == kind && objects[index].GetName() == name {
			return objects[index]
		}
	}

	t.Fatalf("%s/%s not found", kind, name)

	return unstructured.Unstructured{}
}

func assertWorkflowArtifactMaterialization(t *testing.T, workflow unstructured.Unstructured) {
	t.Helper()

	parameters, found, err := unstructured.NestedSlice(
		workflow.Object,
		"spec",
		"arguments",
		"parameters",
	)
	if err != nil || !found {
		t.Fatalf("read WorkflowTemplate parameters: found=%v err=%v", found, err)
	}

	names := make(map[string]bool, len(parameters))
	for index := range parameters {
		parameter, ok := parameters[index].(map[string]any)
		if !ok {
			t.Fatalf("parameter %d has type %T", index, parameters[index])
		}

		name, _ := parameter["name"].(string)
		names[name] = true
	}

	for _, name := range []string{"script-name", "script-content-b64", "script-digest"} {
		if !names[name] {
			t.Errorf("WorkflowTemplate parameter %q is missing", name)
		}
	}

	volumes, found, err := unstructured.NestedSlice(workflow.Object, "spec", "volumes")
	if err != nil || !found {
		t.Fatalf("read WorkflowTemplate volumes: found=%v err=%v", found, err)
	}

	for volumeIndex := range volumes {
		volume, volumeOK := volumes[volumeIndex].(map[string]any)
		if !volumeOK || volume["name"] != "scripts" {
			continue
		}

		if _, emptyDir := volume["emptyDir"]; !emptyDir {
			t.Fatal("scripts volume is not an emptyDir")
		}

		if _, configMap := volume["configMap"]; configMap {
			t.Fatal("scripts volume still depends on a pre-created ConfigMap")
		}

		return
	}

	t.Fatal("WorkflowTemplate scripts volume not found")
}

func assertTestArtifactDigest(t *testing.T, test unstructured.Unstructured) {
	t.Helper()

	artifacts, found, err := unstructured.NestedSlice(test.Object, "spec", "artifacts")
	if err != nil || !found || len(artifacts) != 1 {
		t.Fatalf("read CertificationTest artifacts: count=%d found=%v err=%v", len(artifacts), found, err)
	}

	artifact, ok := artifacts[0].(map[string]any)
	if !ok {
		t.Fatalf("artifact has type %T", artifacts[0])
	}

	content, _ := artifact["content"].(string)
	digest, _ := artifact["digest"].(string)
	sum := sha256.Sum256([]byte(content))

	expected := "sha256:" + hex.EncodeToString(sum[:])
	if digest != expected {
		t.Fatalf("artifact digest = %q, want %q", digest, expected)
	}
}

func workflowMaterializerCommand(t *testing.T, workflow unstructured.Unstructured) string {
	t.Helper()

	templates, found, err := unstructured.NestedSlice(workflow.Object, "spec", "templates")
	if err != nil || !found {
		t.Fatalf("read WorkflowTemplate templates: found=%v err=%v", found, err)
	}

	for index := range templates {
		template, ok := templates[index].(map[string]any)
		if !ok || template["name"] != "burn" {
			continue
		}

		initContainers, ok := template["initContainers"].([]any)
		if !ok || len(initContainers) != 1 {
			t.Fatalf("burn initContainers count = %d, want 1", len(initContainers))
		}

		initContainer, ok := initContainers[0].(map[string]any)
		if !ok || initContainer["name"] != "materialize-script" {
			t.Fatalf("unexpected materializer: %#v", initContainers[0])
		}

		command, ok := initContainer["command"].([]any)
		if !ok || len(command) != 3 {
			t.Fatalf("materializer command = %#v", initContainer["command"])
		}

		script, _ := command[2].(string)
		if !strings.Contains(script, "script digest mismatch") ||
			!strings.Contains(script, "environmentJson keys mismatch") {
			t.Fatal("materializer does not fail closed on digest and parameter schema")
		}

		return script
	}

	t.Fatal("burn template not found")

	return ""
}

func assertWorkflowQualificationPublishing(t *testing.T, workflow unstructured.Unstructured) {
	t.Helper()

	templates, found, err := unstructured.NestedSlice(workflow.Object, "spec", "templates")
	if err != nil || !found {
		t.Fatalf("read WorkflowTemplate templates: found=%v err=%v", found, err)
	}

	for index := range templates {
		template, ok := templates[index].(map[string]any)
		if !ok || template["name"] != "publish-result" {
			continue
		}

		container, ok := template["container"].(map[string]any)
		if !ok {
			t.Fatalf("publish-result container = %#v", template["container"])
		}

		args, ok := container["args"].([]any)
		if !ok || len(args) != 1 {
			t.Fatalf("publish-result args = %#v", container["args"])
		}

		script, _ := args[0].(string)
		for _, required := range []string{
			`"visibleDevices": qualification_start["visibleDevices"]`,
			`"coveredDevices": qualification_start["coveredDevices"]`,
		} {
			if !strings.Contains(script, required) {
				t.Fatalf("publisher does not copy participant qualification field %s", required)
			}
		}

		return
	}

	t.Fatal("publish-result template not found")
}

func testMaterializer(t *testing.T, materializer string) {
	t.Helper()

	allowed := map[string]string{
		"BURN_DURATION_SECONDS":           "120",
		"CPU_WORKERS":                     "32",
		"EXPECTED_GPU_COUNT":              "8",
		"EXPECTED_GPU_PRODUCT_REGEX":      "^NVIDIA B200$",
		"MEMORY_MIB":                      "8192",
		"OBSERVED_GPU_ALLOCATABLE":        "8",
		"OBSERVED_GPU_CAPACITY":           "8",
		"POWER_LIMIT_CONSECUTIVE_SAMPLES": "3",
		"POWER_LIMIT_TOLERANCE_PERCENT":   "5",
		"SAMPLE_INTERVAL_SECONDS":         "10",
		"STORAGE_MIB":                     "8192",
	}
	parameters := materializerParameters(t, allowed)
	content := []byte("#!/usr/bin/env bash\necho ok\n")
	sum := sha256.Sum256(content)
	digest := "sha256:" + hex.EncodeToString(sum[:])

	t.Run("success", func(t *testing.T) {
		scriptsDir := t.TempDir()
		runMaterializer(
			t,
			materializer,
			scriptsDir,
			base64.StdEncoding.EncodeToString(content),
			digest,
			parameters,
			true,
		)

		materialized, err := os.ReadFile(filepath.Join(scriptsDir, "burn.sh"))
		if err != nil {
			t.Fatal(err)
		}

		if string(materialized) != string(content) {
			t.Fatalf("materialized script = %q, want %q", materialized, content)
		}

		environment, err := os.ReadFile(filepath.Join(scriptsDir, "parameters.env"))
		if err != nil {
			t.Fatal(err)
		}

		if !strings.Contains(string(environment), "export EXPECTED_GPU_COUNT=8") {
			t.Fatalf("parameters.env does not contain expected GPU count:\n%s", environment)
		}
	})

	t.Run("digest mismatch", func(t *testing.T) {
		output := runMaterializer(
			t,
			materializer,
			t.TempDir(),
			base64.StdEncoding.EncodeToString(content),
			"sha256:"+strings.Repeat("0", 64),
			parameters,
			false,
		)
		if !strings.Contains(output, "script digest mismatch") {
			t.Fatalf("digest failure output = %q", output)
		}
	})

	t.Run("missing parameter", func(t *testing.T) {
		delete(allowed, "STORAGE_MIB")

		output := runMaterializer(
			t,
			materializer,
			t.TempDir(),
			base64.StdEncoding.EncodeToString(content),
			digest,
			materializerParameters(t, allowed),
			false,
		)
		if !strings.Contains(output, "environmentJson keys mismatch") {
			t.Fatalf("parameter failure output = %q", output)
		}
	})
}

func materializerParameters(t *testing.T, environment map[string]string) string {
	t.Helper()

	environmentJSON, err := json.Marshal(environment)
	if err != nil {
		t.Fatal(err)
	}

	parametersJSON, err := json.Marshal([]any{map[string]any{
		"name": "environmentJson",
		"value": map[string]any{
			"type":        "String",
			"stringValue": string(environmentJSON),
		},
	}})
	if err != nil {
		t.Fatal(err)
	}

	return string(parametersJSON)
}

func runMaterializer(
	t *testing.T,
	materializer string,
	scriptsDir string,
	content string,
	digest string,
	parameters string,
	wantSuccess bool,
) string {
	t.Helper()

	materializer = strings.ReplaceAll(materializer, "/scripts/", scriptsDir+"/")
	command := exec.Command("/bin/bash", "-c", materializer)

	command.Env = append(
		os.Environ(),
		"PROJECT_SIGNAL_SCRIPT_NAME=burn.sh",
		"PROJECT_SIGNAL_SCRIPT_CONTENT_B64="+content,
		"PROJECT_SIGNAL_SCRIPT_DIGEST="+digest,
		"PROJECT_SIGNAL_PARAMETERS_JSON="+parameters,
	)

	output, err := command.CombinedOutput()
	if wantSuccess && err != nil {
		t.Fatalf("materializer failed: %v\n%s", err, output)
	}

	if !wantSuccess && err == nil {
		t.Fatalf("materializer unexpectedly succeeded:\n%s", output)
	}

	return string(output)
}
