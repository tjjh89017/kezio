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

// Package planbuild resolves a Machine's deploy intent - its imageRef,
// dataImages, target-disk hints, and postHookRefs - against the current
// cluster state into an agentapi.DeployPlan the agent can execute with no
// further cluster access, plus the Snapshot a DeployRun records at
// creation. It never writes Machine.spec: every hint and reference is
// read, never rewritten.
//
// Build's errors fall into three shapes a caller distinguishes by type:
// NotReadyError means try again later (the deployer's Delayed outcome);
// DiskSelectionError and ValidationError mean the current spec cannot
// resolve to a plan at all (Failed). Any other error is a transient
// infrastructure failure (a network blip talking to the API server) with
// no defined Outcome of its own.
package planbuild

import (
	"context"
	"fmt"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
	"github.com/tjjh89017/kezio/internal/agentapi"
	"github.com/tjjh89017/kezio/internal/diskmatch"
	"github.com/tjjh89017/kezio/internal/posthookdefaults"
)

// +kubebuilder:rbac:groups=kezio.kojuro.date,resources=images;partitioncontents;posthooks;machinehardwares,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=configmaps;secrets;pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch

// Builder resolves a Machine's deploy intent into a DeployPlan.
type Builder struct {
	// Client reads every object Build needs: Image, PartitionContent,
	// PostHook, MachineHardware, ConfigMap, Secret, the seeder Deployment
	// and its Pods. Build never writes through it.
	Client client.Reader
	// ManagerNamespace is where the shipped default PostHook
	// (posthookdefaults.DefaultFinalizeHookName) lives, read when
	// machine.spec.postHookRefs is empty.
	ManagerNamespace string
}

// resolvedImage carries one image's build inputs and outputs together
// while Build assembles its DeploySlot list and, for the OS image only,
// contributes to hook templating.
type resolvedImage struct {
	ref        keziov1alpha2.NameRef
	image      *keziov1alpha2.Image
	targetDisk string
	slots      []agentapi.DeploySlot
}

// Build resolves machine's deploy intent against current cluster state
// into a DeployPlan for run, plus the Snapshot run's spec must record.
// See the package doc comment for how Build's error types map to a
// deployer Outcome.
func (b *Builder) Build(ctx context.Context, machine *keziov1alpha2.Machine, run *keziov1alpha2.DeployRun) (*agentapi.DeployPlan, Snapshot, error) {
	if machine.Spec.ImageRef == nil && len(machine.Spec.DataImages) == 0 {
		return nil, Snapshot{}, &ValidationError{Reason: "machine has neither an OS image nor data images to deploy"}
	}

	hardware, err := b.getMachineHardware(ctx, machine)
	if err != nil {
		return nil, Snapshot{}, err
	}

	selections := make([]diskmatch.Selection, 0, 1+len(machine.Spec.DataImages))
	var osImage *resolvedImage
	if machine.Spec.ImageRef != nil {
		osImage, err = b.resolveImage(ctx, machine.Namespace, *machine.Spec.ImageRef, machine.Spec.TargetDisk, hardware.Spec.Disks, "OS image")
		if err != nil {
			return nil, Snapshot{}, err
		}
		selections = append(selections, diskmatch.Selection{Label: "OS image", Disk: diskForSelection(hardware.Spec.Disks, osImage.targetDisk)})
	}

	dataImages := make([]*resolvedImage, len(machine.Spec.DataImages))
	for i, di := range machine.Spec.DataImages {
		label := fmt.Sprintf("dataImages[%d]", i)
		resolved, err := b.resolveImage(ctx, machine.Namespace, di.ImageRef, di.TargetDisk, hardware.Spec.Disks, label)
		if err != nil {
			return nil, Snapshot{}, err
		}
		dataImages[i] = resolved
		selections = append(selections, diskmatch.Selection{Label: label, Disk: diskForSelection(hardware.Spec.Disks, resolved.targetDisk)})
	}

	if err := checkDistinctDisks(selections); err != nil {
		return nil, Snapshot{}, err
	}

	hooks, err := b.resolveMachineHooks(ctx, machine, osImage)
	if err != nil {
		return nil, Snapshot{}, err
	}

	plan := &agentapi.DeployPlan{
		SchemaVersion: agentapi.AgentSchemaVersion,
		RunName:       run.Name,
		RunUID:        string(run.UID),
		MachineName:   machine.Name,
		Hooks:         hooks,
		AfterDeploy:   effectiveAfterDeploy(machine),
	}
	if osImage != nil {
		plan.TargetDisk = osImage.targetDisk
		plan.SfdiskScript = osImage.image.Spec.Layout.SfdiskJSON
		plan.Slots = osImage.slots
	}
	for i, di := range machine.Spec.DataImages {
		plan.DataImages = append(plan.DataImages, agentapi.DeployDataImagePlan{
			ImageRef:     di.ImageRef,
			TargetDisk:   dataImages[i].targetDisk,
			SfdiskScript: dataImages[i].image.Spec.Layout.SfdiskJSON,
			Slots:        dataImages[i].slots,
		})
	}

	if err := plan.Validate(); err != nil {
		return nil, Snapshot{}, &ValidationError{Reason: fmt.Sprintf("built plan failed validation: %v", err)}
	}

	snapshot, err := b.buildSnapshot(osImage, dataImages, hooks)
	if err != nil {
		return nil, Snapshot{}, err
	}

	return plan, snapshot, nil
}

// getMachineHardware fetches the MachineHardware object inspection wrote
// for machine, name-aligned with it in the same namespace.
func (b *Builder) getMachineHardware(ctx context.Context, machine *keziov1alpha2.Machine) (*keziov1alpha2.MachineHardware, error) {
	hw := &keziov1alpha2.MachineHardware{}
	key := client.ObjectKey{Namespace: machine.Namespace, Name: machine.Name}
	if err := b.Client.Get(ctx, key, hw); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, &NotReadyError{Reason: fmt.Sprintf("machinehardware %s/%s not found yet", machine.Namespace, machine.Name)}
		}
		return nil, fmt.Errorf("get machinehardware %s/%s: %w", machine.Namespace, machine.Name, err)
	}
	return hw, nil
}

// resolveImage fetches ref's Image, resolves its target disk from hints
// against disks, and builds its DeploySlot list.
func (b *Builder) resolveImage(ctx context.Context, defaultNS string, ref keziov1alpha2.NameRef, hints *keziov1alpha2.TargetDiskHints, disks []keziov1alpha2.MachineHardwareDisk, label string) (*resolvedImage, error) {
	ns := resolveNamespace(ref, defaultNS)

	image := &keziov1alpha2.Image{}
	if err := b.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref.Name}, image); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, &NotReadyError{Reason: fmt.Sprintf("%s: image %s/%s not found", label, ns, ref.Name)}
		}
		return nil, fmt.Errorf("%s: get image %s/%s: %w", label, ns, ref.Name, err)
	}
	if image.Status.State != keziov1alpha2.ImageStateReady {
		return nil, &NotReadyError{Reason: fmt.Sprintf("%s: image %s/%s is not Ready yet", label, ns, ref.Name)}
	}

	disk, err := resolveDisk(disks, hints, label)
	if err != nil {
		return nil, err
	}

	slots, err := b.buildSlots(ctx, image, disk.DeviceName)
	if err != nil {
		return nil, err
	}

	return &resolvedImage{ref: ref, image: image, targetDisk: disk.DeviceName, slots: slots}, nil
}

// diskForSelection returns the disk matching deviceName, for
// diskmatch.CheckDistinct's identity check. deviceName always matches
// exactly one entry in disks: it was itself read off one of them by
// resolveImage just before this is called.
func diskForSelection(disks []keziov1alpha2.MachineHardwareDisk, deviceName string) *keziov1alpha2.MachineHardwareDisk {
	for i := range disks {
		if disks[i].DeviceName == deviceName {
			return &disks[i]
		}
	}
	return nil
}

// resolveMachineHooks resolves machine's attached hooks: its own
// postHookRefs when set, or exactly the shipped default hook otherwise
// (never both - see MachineSpec.PostHookRefs's doc comment). osImage is
// the resolved OS image, nil for a dataImages-only run: its name and
// target disk feed the imageName/targetDisk reserved template values,
// left empty when there is no OS image.
func (b *Builder) resolveMachineHooks(ctx context.Context, machine *keziov1alpha2.Machine, osImage *resolvedImage) ([]agentapi.ResolvedHook, error) {
	shared, err := mergeParams(imageParamsOf(osImage), machine.Spec.Params)
	if err != nil {
		return nil, &ValidationError{Reason: fmt.Sprintf("merging posthook params: %v", err)}
	}

	var imageName, targetDisk string
	if osImage != nil {
		imageName = osImage.image.Name
		targetDisk = osImage.targetDisk
	}
	// Set after merging so a declared param can never shadow a reserved
	// name.
	shared[keziov1alpha2.PostHookReservedParamMachineName] = machine.Name
	shared[keziov1alpha2.PostHookReservedParamImageName] = imageName
	shared[keziov1alpha2.PostHookReservedParamTargetDisk] = targetDisk

	defaults := deriveBuiltinDefaults(osImage)

	if len(machine.Spec.PostHookRefs) == 0 {
		defaultRef := keziov1alpha2.NameRef{Name: posthookdefaults.DefaultFinalizeHookName}
		return b.resolveHooks(ctx, b.ManagerNamespace, []keziov1alpha2.NameRef{defaultRef}, shared, defaults)
	}
	return b.resolveHooks(ctx, machine.Namespace, machine.Spec.PostHookRefs, shared, defaults)
}

// builtinDefaults carries the values a finalize-shaped builtin
// (efibootmgr, install-removable-fallback, growLastPartition) falls back
// to when a PostHook step's own params does not override them: only the
// OS image (never a dataImages entry) carries the ESP and boot partitions
// these builtins act on, so a dataImages-only deploy derives nothing
// (zero value).
type builtinDefaults struct {
	disk          string
	espPartition  int32
	lastPartition int32
}

// deriveBuiltinDefaults computes builtinDefaults from osImage's own
// target disk and layout, zero-valued when osImage is nil.
func deriveBuiltinDefaults(osImage *resolvedImage) builtinDefaults {
	if osImage == nil {
		return builtinDefaults{}
	}
	d := builtinDefaults{disk: osImage.targetDisk}
	slots := osImage.image.Spec.Layout.Slots
	for _, s := range slots {
		if s.Role == keziov1alpha2.PartitionRoleESP {
			d.espPartition = s.Number
			break
		}
	}
	if n := len(slots); n > 0 {
		d.lastPartition = slots[n-1].Number
	}
	return d
}

// imageParamsOf returns osImage's own Params, or nil (treated as "no
// params set") when osImage is nil.
func imageParamsOf(osImage *resolvedImage) *apiextensionsv1.JSON {
	if osImage == nil {
		return nil
	}
	return osImage.image.Spec.Params
}

// effectiveAfterDeploy returns machine's AfterDeploy, treating an unset
// value as keziov1alpha2.AfterDeployReboot (the CRD schema's own
// default, which a fake or unit-test client that skips defaulting webhooks
// may not have applied).
func effectiveAfterDeploy(machine *keziov1alpha2.Machine) string {
	if machine.Spec.AfterDeploy == "" {
		return keziov1alpha2.AfterDeployReboot
	}
	return machine.Spec.AfterDeploy
}

// buildSnapshot assembles the Snapshot Build returns alongside the plan:
// one DeployRunResolvedDisk per resolved image (OS first, then
// dataImages, in order) and the hash of every resolved hook.
func (b *Builder) buildSnapshot(osImage *resolvedImage, dataImages []*resolvedImage, hooks []agentapi.ResolvedHook) (Snapshot, error) {
	disks := make([]keziov1alpha2.DeployRunResolvedDisk, 0, 1+len(dataImages))
	if osImage != nil {
		disks = append(disks, keziov1alpha2.DeployRunResolvedDisk{ImageRef: osImage.ref, TargetDisk: osImage.targetDisk})
	}
	for _, di := range dataImages {
		disks = append(disks, keziov1alpha2.DeployRunResolvedDisk{ImageRef: di.ref, TargetDisk: di.targetDisk})
	}

	hash, err := hooksHash(hooks)
	if err != nil {
		return Snapshot{}, fmt.Errorf("computing hooks hash: %w", err)
	}

	return Snapshot{ResolvedDisks: disks, HooksHash: hash}, nil
}
