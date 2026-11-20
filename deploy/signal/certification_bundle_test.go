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
	assertTestArtifactDigest(t, test)

	materializer := workflowMaterializerCommand(t, workflow)
	testMaterializer(t, materializer)
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
  nodeSelector:
    unbounded-cloud.io/site: provider-a
  providerLabel: unbounded-cloud.io/site
  expectedGPUCount: 8
  expectedGPUProductRegex: "^NVIDIA B200$"
  metricsOverlay: %s
  images:
    controller: example.invalid/controller@sha256:%s
    adapter: example.invalid/adapter@sha256:%s
`, metricsDir, strings.Repeat("a", 64), strings.Repeat("b", 64))
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
}

func writeExecutable(t *testing.T, path string, content string) {
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
