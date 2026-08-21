/*
Copyright 2026.

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

package planbuild

// NotReadyError means Build cannot produce a plan yet for a reason that
// is not a configuration mistake: a referenced Image/PartitionContent/
// PostHook/MachineHardware has not reached the state Build needs, or a
// content's seeder has no reachable pod yet. Callers map this to the
// deployer's Delayed outcome and retry later without touching Machine
// error state.
type NotReadyError struct {
	Reason string
}

func (e *NotReadyError) Error() string { return e.Reason }

// DiskSelectionError means the target-disk hints on the Machine (or one
// of its dataImages entries) failed to resolve to exactly one disk, or
// two resolved selections named the same physical disk. Callers map this
// to Failed: a hint conflict does not resolve itself by waiting.
type DiskSelectionError struct {
	Reason string
}

func (e *DiskSelectionError) Error() string { return e.Reason }

// ValidationError means the plan itself is malformed: a hook step's
// template placeholder did not resolve against the merged params and
// reserved names, or the assembled DeployPlan failed its own structural
// checks. Callers map this to Failed.
type ValidationError struct {
	Reason string
}

func (e *ValidationError) Error() string { return e.Reason }
