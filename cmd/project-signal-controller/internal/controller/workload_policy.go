// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"encoding/json"
	"fmt"

	signalv1 "github.com/Azure/unbounded/api/signal/v1alpha1"
)

func encodeWorkloadPolicy(policy signalv1.WorkloadPolicy) (string, error) {
	if err := signalv1.ValidateWorkloadPolicy(policy); err != nil {
		return "", err
	}

	content, err := json.Marshal(policy)
	if err != nil {
		return "", fmt.Errorf("encode workload policy: %w", err)
	}

	if len(content) > signalv1.MaxWorkloadPolicyBytes {
		return "", fmt.Errorf("workload policy exceeds %d bytes", signalv1.MaxWorkloadPolicyBytes)
	}

	return string(content), nil
}
