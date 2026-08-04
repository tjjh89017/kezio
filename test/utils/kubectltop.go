/*
Copyright 2026 Date Huang.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package utils

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
)

// ParseKubectlTopLine parses one data line of `kubectl top pod --no-headers`
// output ("<name> <cpu> <memory>", e.g. "kezio-ezio-seeder-6f9d8-abcde 105m
// 130Mi") into the pod name, CPU in millicores, and memory in bytes. It is a
// plain string/quantity parser (no cluster access) so the seeding
// benchmark's periodic seeder-resource sampling can be unit tested without
// a live metrics-server.
func ParseKubectlTopLine(line string) (podName string, cpuMilli int64, memBytes int64, err error) {
	fields := strings.Fields(line)
	if len(fields) != 3 {
		return "", 0, 0, fmt.Errorf(
			"kubectl top line %q: want 3 whitespace-separated fields (name, cpu, memory), got %d", line, len(fields))
	}

	cpuQty, err := resource.ParseQuantity(fields[1])
	if err != nil {
		return "", 0, 0, fmt.Errorf("kubectl top line %q: parse cpu %q: %w", line, fields[1], err)
	}
	memQty, err := resource.ParseQuantity(fields[2])
	if err != nil {
		return "", 0, 0, fmt.Errorf("kubectl top line %q: parse memory %q: %w", line, fields[2], err)
	}

	return fields[0], cpuQty.MilliValue(), memQty.Value(), nil
}
