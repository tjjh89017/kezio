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

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
)

func isPaused(machine *keziov1alpha2.Machine) bool {
	_, ok := machine.Annotations[keziov1alpha2.MachineAnnotationPaused]
	return ok
}

func isDetached(machine *keziov1alpha2.Machine) bool {
	_, ok := machine.Annotations[keziov1alpha2.MachineAnnotationDetached]
	return ok
}

// isRebootAnnotationKey reports whether key is the suffixless reboot
// annotation or a "-<client>" suffixed one.
func isRebootAnnotationKey(key string) bool {
	return key == keziov1alpha2.MachineAnnotationRebootPrefix ||
		strings.HasPrefix(key, keziov1alpha2.MachineAnnotationRebootPrefix+"-")
}

// rebootRequested scans every reboot annotation on machine - suffixless and
// every "-<client>" suffixed holder - and reports whether at least one is
// present, and whether any holder asked for a hard reboot: hard wins across
// every holder, so one holder asking for hard is enough regardless of what
// any other holder (including the suffixless form) asked for.
func rebootRequested(ctx context.Context, machine *keziov1alpha2.Machine) (requested, hard bool) {
	log := logf.FromContext(ctx)
	for key, value := range machine.Annotations {
		if !isRebootAnnotationKey(key) {
			continue
		}
		requested = true

		args, err := parseRebootAnnotation(value)
		if err != nil {
			log.Info("invalid reboot annotation value, treating as a soft reboot request",
				"machine", machine.Name, "annotation", key, "value", value, "error", err.Error())
			continue
		}
		if args.Mode == keziov1alpha2.MachineRebootModeHard {
			hard = true
		}
	}
	return requested, hard
}

// parseRebootAnnotation decodes a reboot annotation's value. An empty value
// is a valid soft request (no arguments given). Invalid JSON is reported as
// an error; the caller treats that as soft too, but reboot presence itself
// does not depend on this succeeding.
func parseRebootAnnotation(value string) (keziov1alpha2.MachineRebootAnnotationArguments, error) {
	result := keziov1alpha2.MachineRebootAnnotationArguments{Mode: keziov1alpha2.MachineRebootModeSoft}
	if value == "" {
		return result, nil
	}
	if err := json.Unmarshal([]byte(value), &result); err != nil {
		return keziov1alpha2.MachineRebootAnnotationArguments{Mode: keziov1alpha2.MachineRebootModeSoft}, err
	}
	return result, nil
}

// clearSuffixlessRebootAnnotation removes only the suffixless reboot
// annotation from machine, once the controller has acted on it. Suffixed
// forms are a client's own hold and are left untouched - only the client
// that set one clears it.
func (r *MachineReconciler) clearSuffixlessRebootAnnotation(ctx context.Context, machine *keziov1alpha2.Machine) error {
	if _, ok := machine.Annotations[keziov1alpha2.MachineAnnotationRebootPrefix]; !ok {
		return nil
	}
	patch := client.MergeFrom(machine.DeepCopy())
	delete(machine.Annotations, keziov1alpha2.MachineAnnotationRebootPrefix)
	if err := r.Patch(ctx, machine, patch); err != nil {
		return fmt.Errorf("machine %q: clearing suffixless reboot annotation: %w", machine.Name, err)
	}
	return nil
}
