// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package adapter

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	signalv1 "github.com/Azure/unbounded/api/signal/v1alpha1"
)

func decodeWorkloadPolicy(content string) (signalv1.WorkloadPolicy, error) {
	var policy signalv1.WorkloadPolicy

	if strings.TrimSpace(content) == "" {
		content = "{}"
	}

	if len(content) > signalv1.MaxWorkloadPolicyBytes {
		return policy, fmt.Errorf("workload policy exceeds %d bytes", signalv1.MaxWorkloadPolicyBytes)
	}

	decoder := json.NewDecoder(strings.NewReader(content))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(&policy); err != nil {
		return policy, fmt.Errorf("decode workload policy: %w", err)
	}

	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return policy, errors.New("workload policy contains trailing JSON values")
		}

		return policy, fmt.Errorf("decode trailing workload policy value: %w", err)
	}

	if err := signalv1.ValidateWorkloadPolicy(policy); err != nil {
		return policy, err
	}

	return policy, nil
}

func hostPathVolumeName(index int) string {
	return fmt.Sprintf("host-%02d", index)
}
