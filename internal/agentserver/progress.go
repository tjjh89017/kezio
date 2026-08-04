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

package agentserver

import (
	"fmt"
	"sort"
	"strings"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
	"github.com/tjjh89017/kezio/internal/agentapi"
)

// summarizeProgress reduces req.Partitions to the (Reason, Message) pair
// stored on the ProvisioningProgress condition: Reason is the furthest
// phase reached by any partition still short of done (or
// agentapi.PartitionPhaseDone once every partition has reached it, or
// "Unknown" for an empty snapshot - defensive only, the agent always
// reports at least one partition while a plan is being executed).
// Message lists each partition's disk, number, phase, and percent, sorted
// by (disk, number) so repeated reports render as a stable, diffable
// line rather than reshuffling on every poll.
func summarizeProgress(req agentapi.ProgressRequest) (reason, message string) {
	if len(req.Partitions) == 0 {
		return "Unknown", "no partition progress reported"
	}

	parts := append([]agentapi.PartitionProgress(nil), req.Partitions...)
	sort.Slice(parts, func(i, j int) bool {
		if parts[i].Disk != parts[j].Disk {
			return parts[i].Disk < parts[j].Disk
		}
		return parts[i].Number < parts[j].Number
	})

	allDone := true
	furthest := parts[0].Phase
	lines := make([]string, 0, len(parts))
	for _, p := range parts {
		if p.Phase != agentapi.PartitionPhaseDone {
			allDone = false
			furthest = p.Phase
		}
		lines = append(lines, fmt.Sprintf("%s%d: %s %d%%", p.Disk, p.Number, p.Phase, p.PercentDone))
	}
	if allDone {
		furthest = agentapi.PartitionPhaseDone
	}
	return furthest, strings.Join(lines, "; ")
}

// setProvisioningProgressCondition sets machine's ProvisioningProgress
// condition from req, following setAgentRegisteredCondition's shape.
func setProvisioningProgressCondition(machine *keziov1alpha1.Machine, req agentapi.ProgressRequest, now time.Time) {
	reason, message := summarizeProgress(req)
	apimeta.SetStatusCondition(&machine.Status.Conditions, metav1.Condition{
		Type:               keziov1alpha1.MachineConditionProvisioningProgress,
		Status:             metav1.ConditionTrue,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: machine.Generation,
		LastTransitionTime: metav1.NewTime(now),
	})
}
