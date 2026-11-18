// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package adapter

import (
	"strings"
	"testing"

	signalv1 "github.com/Azure/unbounded/api/signal/v1alpha1"
)

func TestDecodeWorkloadPolicy(t *testing.T) {
	t.Parallel()

	policy, err := decodeWorkloadPolicy(`{"hostPaths":[{"path":"/proc/cpuinfo","mountPath":"/host/cpuinfo"}]}`)
	if err != nil {
		t.Fatalf("decodeWorkloadPolicy() error = %v", err)
	}

	if len(policy.HostPaths) != 1 || policy.HostPaths[0].Path != "/proc/cpuinfo" {
		t.Fatalf("decodeWorkloadPolicy() = %#v", policy)
	}
}

func TestDecodeWorkloadPolicyRejectsMalformedContent(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"unknown field":  `{"unexpected":true}`,
		"trailing value": `{} {}`,
		"oversized":      `{"hostPaths":[],"padding":"` + strings.Repeat("a", signalv1.MaxWorkloadPolicyBytes) + `"}`,
	}

	for name, content := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if _, err := decodeWorkloadPolicy(content); err == nil {
				t.Fatal("decodeWorkloadPolicy() error = nil")
			}
		})
	}
}
