// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"errors"
	"fmt"
	"path"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

const MaxWorkloadPolicyBytes = 16 * 1024

var allowedWorkloadHostPathRoots = []string{"/proc", "/sys", "/var/lib/project-signal"}

func ValidateWorkloadPolicy(policy WorkloadPolicy) error {
	if err := validateWorkloadResources(policy.Resources); err != nil {
		return err
	}

	if len(policy.HostPaths) > 16 {
		return errors.New("workload policy must not contain more than 16 host paths")
	}

	mountPaths := map[string]struct{}{"/scripts": {}, "/results": {}}

	for index := range policy.HostPaths {
		hostPath := &policy.HostPaths[index]
		if err := validateWorkloadAbsolutePath(
			fmt.Sprintf("workload policy host path %d mountPath", index),
			hostPath.MountPath,
		); err != nil {
			return err
		}

		if _, exists := mountPaths[hostPath.MountPath]; exists {
			return fmt.Errorf(
				"workload policy host path %d duplicates reserved or existing mountPath %q",
				index,
				hostPath.MountPath,
			)
		}

		mountPaths[hostPath.MountPath] = struct{}{}
		if err := validateWorkloadAbsolutePath(
			fmt.Sprintf("workload policy host path %d path", index),
			hostPath.Path,
		); err != nil {
			return err
		}

		if !allowedWorkloadHostPath(hostPath.Path) {
			return fmt.Errorf("workload policy host path %d source %q is not allowlisted", index, hostPath.Path)
		}
	}

	return nil
}

func validateWorkloadResources(resources corev1.ResourceRequirements) error {
	for name, quantity := range resources.Requests {
		if quantity.Sign() < 0 {
			return fmt.Errorf("workload policy resource request %q must not be negative", name)
		}
	}

	for name, quantity := range resources.Limits {
		if quantity.Sign() < 0 {
			return fmt.Errorf("workload policy resource limit %q must not be negative", name)
		}

		request, exists := resources.Requests[name]
		if exists && quantity.Cmp(request) < 0 {
			return fmt.Errorf("workload policy resource limit %q must not be less than its request", name)
		}
	}

	return nil
}

func allowedWorkloadHostPath(value string) bool {
	for _, root := range allowedWorkloadHostPathRoots {
		if value == root || strings.HasPrefix(value, root+"/") {
			return true
		}
	}

	return false
}

func validateWorkloadAbsolutePath(name, value string) error {
	if value == "" || !strings.HasPrefix(value, "/") || path.Clean(value) != value {
		return fmt.Errorf("%s must be a clean absolute path", name)
	}

	return nil
}
