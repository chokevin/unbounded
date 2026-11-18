// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestValidateWorkloadPolicy(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		policy  WorkloadPolicy
		wantErr string
	}{
		"valid": {
			policy: WorkloadPolicy{
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
					Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")},
				},
				HostPaths: []WorkloadHostPath{{
					Path:      "/sys/class/infiniband",
					MountPath: "/host/infiniband",
				}},
			},
		},
		"limit below request": {
			policy: WorkloadPolicy{
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")},
					Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
				},
			},
			wantErr: "must not be less than its request",
		},
		"reserved mount": {
			policy: WorkloadPolicy{HostPaths: []WorkloadHostPath{{
				Path:      "/proc/cpuinfo",
				MountPath: "/results",
			}}},
			wantErr: "duplicates reserved",
		},
		"unapproved source": {
			policy: WorkloadPolicy{HostPaths: []WorkloadHostPath{{
				Path:      "/etc/passwd",
				MountPath: "/host/passwd",
			}}},
			wantErr: "is not allowlisted",
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			err := ValidateWorkloadPolicy(test.policy)
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidateWorkloadPolicy() error = %v", err)
				}

				return
			}

			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("ValidateWorkloadPolicy() error = %v, want containing %q", err, test.wantErr)
			}
		})
	}
}
