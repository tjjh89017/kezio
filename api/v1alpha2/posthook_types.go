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

package v1alpha2

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PostHookDefaultTimeoutSeconds is the step timeout used when a step does
// not set TimeoutSeconds.
const PostHookDefaultTimeoutSeconds int32 = 60

// Reserved template placeholder names that a PostHook step's script content
// may reference without declaring them in spec.params: the deploy plan
// builder injects these itself, alongside any declared param, before
// resolving a step's template.
const (
	PostHookReservedParamMachineName = "machineName"
	PostHookReservedParamImageName   = "imageName"
	PostHookReservedParamTargetDisk  = "targetDisk"
)

// PostHookParam declares one input a PostHook's steps may reference through
// templating. Default supplies the value used when the attaching resource's
// merged params do not set it; a nil Default means the param has no
// fallback and an attaching resource must supply it.
type PostHookParam struct {
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// +optional
	Default *string `json:"default,omitempty"`
}

// Builtin step name enum values for PostHookBuiltinStep.Name. These match
// the command each builtin runs, so they stay lowercase rather than the
// PascalCase used for state-machine enums.
const (
	// BuiltinStepMkswap runs mkswap with the source partition's saved UUID.
	BuiltinStepMkswap = "mkswap"
	// BuiltinStepEfibootmgr creates a UEFI boot entry pointing at the ESP.
	BuiltinStepEfibootmgr = "efibootmgr"
	// BuiltinStepGrowLastPartition grows the last partition and its file
	// system when the target disk is larger than the source.
	BuiltinStepGrowLastPartition = "growLastPartition"
	// BuiltinStepInstallRemovableFallback copies a shim/grub bootloader
	// onto the ESP's UEFI removable-media fallback path for an image that
	// does not already carry one there.
	BuiltinStepInstallRemovableFallback = "install-removable-fallback"
)

// PostHookBuiltinStep runs one of the named steps KEZIO ships.
type PostHookBuiltinStep struct {
	// Name selects which shipped builtin runs.
	// +kubebuilder:validation:Enum=mkswap;efibootmgr;growLastPartition;install-removable-fallback
	Name string `json:"name"`
	// TimeoutSeconds bounds how long the builtin may run before the agent
	// treats it as failed. Defaults to PostHookDefaultTimeoutSeconds.
	// +kubebuilder:validation:Minimum=1
	// +optional
	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty"`
}

// EffectiveTimeoutSeconds returns the builtin step's timeout, treating a
// zero value as PostHookDefaultTimeoutSeconds (the default).
func (s PostHookBuiltinStep) EffectiveTimeoutSeconds() int32 {
	if s.TimeoutSeconds == 0 {
		return PostHookDefaultTimeoutSeconds
	}
	return s.TimeoutSeconds
}

// Script source kind values returned by PostHookScriptSource.SourceKind.
const (
	// PostHookScriptSourceInline means the script content is Script.
	PostHookScriptSourceInline = "inline"
	// PostHookScriptSourceConfigMapRef means the script content comes from
	// ConfigMapRef.
	PostHookScriptSourceConfigMapRef = "configMapRef"
	// PostHookScriptSourceSecretRef means the script content comes from
	// SecretRef.
	PostHookScriptSourceSecretRef = "secretRef"
	// PostHookScriptSourceUnknown means none of Script, ConfigMapRef, or
	// SecretRef is set. The CRD's validation rejects this; the constant
	// exists so callers have a defined zero value to check against.
	PostHookScriptSourceUnknown = ""
)

// PostHookScriptSource holds a script step's content, from exactly one of
// an inline script, a ConfigMap key, or a Secret key, plus the step's
// timeout. It backs both the "script" and "chrootScript" step kinds; the
// two differ only in where the agent runs the script (the live OS versus a
// chroot of the deployed root file system), not in how the content is
// sourced.
// +kubebuilder:validation:XValidation:rule="(has(self.script) ? 1 : 0) + (has(self.configMapRef) ? 1 : 0) + (has(self.secretRef) ? 1 : 0) == 1",message="exactly one of script, configMapRef, or secretRef must be set"
type PostHookScriptSource struct {
	// Script is the inline script content.
	// +optional
	Script string `json:"script,omitempty"`
	// ConfigMapRef names the ConfigMap key holding the script content.
	// +optional
	ConfigMapRef *ConfigMapKeyRef `json:"configMapRef,omitempty"`
	// SecretRef names the Secret key holding the script content.
	// +optional
	SecretRef *SecretKeyRef `json:"secretRef,omitempty"`
	// TimeoutSeconds bounds how long the script may run before the agent
	// treats it as failed. Defaults to PostHookDefaultTimeoutSeconds.
	// +kubebuilder:validation:Minimum=1
	// +optional
	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty"`
}

// SourceKind reports which of Script, ConfigMapRef, or SecretRef is set.
// Returns PostHookScriptSourceUnknown when none is set; the CRD's
// validation rule keeps that case out of a stored object, but a
// zero-valued PostHookScriptSource can still reach Go code that builds one
// programmatically.
func (s PostHookScriptSource) SourceKind() string {
	switch {
	case s.Script != "":
		return PostHookScriptSourceInline
	case s.ConfigMapRef != nil:
		return PostHookScriptSourceConfigMapRef
	case s.SecretRef != nil:
		return PostHookScriptSourceSecretRef
	default:
		return PostHookScriptSourceUnknown
	}
}

// EffectiveTimeoutSeconds returns the script step's timeout, treating a
// zero value as PostHookDefaultTimeoutSeconds (the default).
func (s PostHookScriptSource) EffectiveTimeoutSeconds() int32 {
	if s.TimeoutSeconds == 0 {
		return PostHookDefaultTimeoutSeconds
	}
	return s.TimeoutSeconds
}

// Step type values returned by PostHookStep.Type.
const (
	// PostHookStepTypeBuiltin means Builtin is set.
	PostHookStepTypeBuiltin = "builtin"
	// PostHookStepTypeScript means Script is set: the step runs in the live
	// OS, nothing mounted.
	PostHookStepTypeScript = "script"
	// PostHookStepTypeChrootScript means ChrootScript is set: the step runs
	// inside a chroot of the deployed root file system.
	PostHookStepTypeChrootScript = "chrootScript"
	// PostHookStepTypeUnknown means none of Builtin, Script, or
	// ChrootScript is set. The CRD's validation rejects this; the constant
	// exists so callers have a defined zero value to check against.
	PostHookStepTypeUnknown = ""
)

// PostHookStep is one ordered step of a PostHook, exactly one of a named
// builtin, a live-OS script, or a chroot script.
// +kubebuilder:validation:XValidation:rule="(has(self.builtin) ? 1 : 0) + (has(self.script) ? 1 : 0) + (has(self.chrootScript) ? 1 : 0) == 1",message="exactly one of builtin, script, or chrootScript must be set"
type PostHookStep struct {
	// OSFamily restricts this step to a target OS family. Absent means the
	// step applies regardless of OSFamily. The webhook rejects a builtin
	// step whose Name only makes sense on Linux (mkswap, efibootmgr,
	// growLastPartition) unless OSFamily is Linux.
	// +kubebuilder:validation:Enum=Linux;Windows;FreeBSD;Other
	// +optional
	OSFamily string `json:"osFamily,omitempty"`
	// Builtin runs one of the named steps KEZIO ships.
	// +optional
	Builtin *PostHookBuiltinStep `json:"builtin,omitempty"`
	// Script runs in the live OS; the target disk is available but its
	// file systems are not mounted.
	// +optional
	Script *PostHookScriptSource `json:"script,omitempty"`
	// ChrootScript runs inside a chroot of the deployed root file system.
	// The agent mounts the root partition (plus /proc, /sys, /dev, and the
	// ESP on its fstab path), runs the script, and unmounts.
	// +optional
	ChrootScript *PostHookScriptSource `json:"chrootScript,omitempty"`
}

// Type reports which of Builtin, Script, or ChrootScript is set. Returns
// PostHookStepTypeUnknown when none is set; the CRD's validation rule keeps
// that case out of a stored object, but a zero-valued PostHookStep can
// still reach Go code that builds one programmatically.
func (s PostHookStep) Type() string {
	switch {
	case s.Builtin != nil:
		return PostHookStepTypeBuiltin
	case s.Script != nil:
		return PostHookStepTypeScript
	case s.ChrootScript != nil:
		return PostHookStepTypeChrootScript
	default:
		return PostHookStepTypeUnknown
	}
}

// PostHookSpec defines the desired state of PostHook: the params it
// declares and the ordered steps it runs.
type PostHookSpec struct {
	// Params declares the named inputs this PostHook's steps may reference
	// through templating, with optional defaults. Names are unique within
	// the list.
	// +kubebuilder:validation:MaxItems=64
	// +listType=map
	// +listMapKey=name
	// +optional
	Params []PostHookParam `json:"params,omitempty"`
	// Steps run in list order. Each step is exactly one of a builtin, a
	// live-OS script, or a chroot script.
	// +kubebuilder:validation:MinItems=1
	Steps []PostHookStep `json:"steps"`
}

// Condition types set on PostHookStatus.Conditions.
const (
	// PostHookConditionReady reflects whether this PostHook's steps and
	// templating pass validation.
	PostHookConditionReady = "Ready"
	// PostHookConditionValid reports whether this PostHook passed both
	// schema-level validation (enforced at admission, so always True for an
	// admitted object) and the PostHookReconciler's own checks.
	PostHookConditionValid = "Valid"
)

// PostHookStatus defines the observed state of PostHook.
type PostHookStatus struct {
	// ObservedGeneration is the generation the conditions below were last
	// computed against.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Conditions report the current state of the PostHook, including
	// PostHookConditionReady and PostHookConditionValid. Every write
	// carries observedGeneration (metav1.Condition's own field) set to the
	// generation the write observed.
	// +optional
	// +listType=map
	// +listMapKey=type
	// +patchMergeKey=type
	// +patchStrategy=merge
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName={ezh,posthook}
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// PostHook is the Schema for the posthooks API.
type PostHook struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PostHookSpec   `json:"spec,omitempty"`
	Status PostHookStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PostHookList contains a list of PostHook.
type PostHookList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PostHook `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PostHook{}, &PostHookList{})
}
