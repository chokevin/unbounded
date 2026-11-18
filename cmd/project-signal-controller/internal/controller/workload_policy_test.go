// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"strings"
	"testing"

	signalv1 "github.com/Azure/unbounded/api/signal/v1alpha1"
)

func TestEncodeWorkloadPolicy(t *testing.T) {
	t.Parallel()

	content, err := encodeWorkloadPolicy(signalv1.WorkloadPolicy{})
	if err != nil {
		t.Fatalf("encodeWorkloadPolicy() error = %v", err)
	}

	if content != `{"resources":{}}` {
		t.Fatalf("encodeWorkloadPolicy() = %q, want empty resource policy", content)
	}
}

func TestEncodeWorkloadPolicyRejectsOversizedContent(t *testing.T) {
	t.Parallel()

	_, err := encodeWorkloadPolicy(signalv1.WorkloadPolicy{
		HostPaths: []signalv1.WorkloadHostPath{{
			Path:      "/sys/" + strings.Repeat("a", signalv1.MaxWorkloadPolicyBytes),
			MountPath: "/host/sys",
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "workload policy exceeds") {
		t.Fatalf("encodeWorkloadPolicy() error = %v, want oversized policy error", err)
	}
}
