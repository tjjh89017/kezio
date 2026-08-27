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

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
	"github.com/tjjh89017/kezio/internal/agentapi"
	"github.com/tjjh89017/kezio/internal/diskmatch"
	"github.com/tjjh89017/kezio/internal/posthookdefaults"
	"github.com/tjjh89017/kezio/internal/seeder"
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
	// LeecherEzio is the cluster-wide operator default for the ezio
	// AddTorrent tuning every DeployPlan this Builder produces carries,
	// before machine.spec.ezio's per-Machine override is applied. Kept
	// separate from ImageSeederConfig's own MaxUploads/MaxConnections
	// (internal/controller): a seeder serves every leecher at its Site at
	// once, a leecher serves only itself, so the two defaults must never
	// collapse into one setting.
	LeecherEzio LeecherEzioConfig
}

// LeecherEzioConfig is the cluster-wide operator default layer for a
// leecher's ezio AddTorrent tuning (see Builder.LeecherEzio). A zero
// field falls back to its own seeder.Default* constant.
type LeecherEzioConfig struct {
	MaxUploads     int32
	MaxConnections int32
}

// resolvedImage carries one image's build inputs and outputs together
// while Build assembles its DeploySlot list and, for the OS image only,
// contributes to hook templating.
type resolvedImage struct {
	ref        keziov1alpha3.NameRef
	image      *keziov1alpha3.Image
	targetDisk string
	slots      []agentapi.DeploySlot
}

// Build resolves claim's deploy intent, for machine, against current
// cluster state into a DeployPlan for run, plus the Snapshot run's spec
// must record. See the package doc comment for how Build's error types
// map to a deployer Outcome. claim nil is the same as one with neither
// ImageRef nor DataImages set: a Machine with no bound claim has nothing
// to deploy.
func (b *Builder) Build(ctx context.Context, machine *keziov1alpha3.Machine, claim *keziov1alpha3.MachineClaim, run *keziov1alpha3.DeployRun) (*agentapi.DeployPlan, Snapshot, error) {
	if claim == nil || (claim.Spec.ImageRef == nil && len(claim.Spec.DataImages) == 0) {
		return nil, Snapshot{}, &ValidationError{Reason: "machine has no bound claim with an OS image or data images to deploy"}
	}

	site := &lazySiteResolution{client: b.Client, machine: machine}

	hardware, err := b.getMachineHardware(ctx, machine)
	if err != nil {
		return nil, Snapshot{}, err
	}

	selections := make([]diskmatch.Selection, 0, 1+len(claim.Spec.DataImages))
	var osImage *resolvedImage
	if claim.Spec.ImageRef != nil {
		osImage, err = b.resolveImage(ctx, machine.Namespace, *claim.Spec.ImageRef, claim.Spec.TargetDisk, hardware.Spec.Disks, "OS image", site, true)
		if err != nil {
			return nil, Snapshot{}, err
		}
		selections = append(selections, diskmatch.Selection{Label: "OS image", Disk: diskForSelection(hardware.Spec.Disks, osImage.targetDisk)})
	}

	dataImages := make([]*resolvedImage, len(claim.Spec.DataImages))
	for i, di := range claim.Spec.DataImages {
		label := fmt.Sprintf("dataImages[%d]", i)
		resolved, err := b.resolveImage(ctx, machine.Namespace, di.ImageRef, di.TargetDisk, hardware.Spec.Disks, label, site, true)
		if err != nil {
			return nil, Snapshot{}, err
		}
		dataImages[i] = resolved
		selections = append(selections, diskmatch.Selection{Label: label, Disk: diskForSelection(hardware.Spec.Disks, resolved.targetDisk)})
	}

	if err := checkDistinctDisks(selections); err != nil {
		return nil, Snapshot{}, err
	}

	hooks, err := b.resolveMachineHooks(ctx, machine, claim, osImage)
	if err != nil {
		return nil, Snapshot{}, err
	}

	plan := &agentapi.DeployPlan{
		SchemaVersion:  agentapi.AgentSchemaVersion,
		RunName:        run.Name,
		RunUID:         string(run.UID),
		MachineName:    machine.Name,
		Hooks:          hooks,
		AfterDeploy:    effectiveAfterDeploy(claim),
		MaxUploads:     seeder.ResolveMaxUploads(b.LeecherEzio.MaxUploads, claimEzioMaxUploads(claim)),
		MaxConnections: seeder.ResolveMaxConnections(b.LeecherEzio.MaxConnections, claimEzioMaxConnections(claim)),
		CacheSizeMB:    claimEzioCacheSizeMB(claim),
		AioThreads:     claimEzioAioThreads(claim),
		Port:           claimEzioPort(claim),
	}
	if osImage != nil {
		plan.TargetDisk = osImage.targetDisk
		plan.SfdiskScript = osImage.image.Spec.Layout.SfdiskJSON
		plan.Slots = osImage.slots
	}
	for i, di := range claim.Spec.DataImages {
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
func (b *Builder) getMachineHardware(ctx context.Context, machine *keziov1alpha3.Machine) (*keziov1alpha3.MachineHardware, error) {
	hw := &keziov1alpha3.MachineHardware{}
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
// against disks, and - when withSlots is true - builds its DeploySlot
// list. site lazily resolves the deploying Machine's own seeder
// placement, threaded through to every content slot this Image builds.
//
// withSlots is false for BuildSnapshot's hooksHash-only resolution: a
// slot's torrent needs a live seeder Deployment (buildTorrent ->
// resolveTorrentURL), which is only ever created on demand and torn down
// after Provisioning finishes - both facts irrelevant to hooksHash, which
// buildSnapshot never reads slots for. Requiring a seeder there would
// make every idle reconcile of an already-Provisioned machine (see
// MachineReconciler.currentHooksHash) flap Delayed for as long as the
// seeder happens to be shut down between deploys.
func (b *Builder) resolveImage(ctx context.Context, defaultNS string, ref keziov1alpha3.NameRef, hints *keziov1alpha3.TargetDiskHints, disks []keziov1alpha3.MachineHardwareDisk, label string, site *lazySiteResolution, withSlots bool) (*resolvedImage, error) {
	ns := resolveNamespace(ref, defaultNS)

	image := &keziov1alpha3.Image{}
	if err := b.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref.Name}, image); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, &NotReadyError{Reason: fmt.Sprintf("%s: image %s/%s not found", label, ns, ref.Name)}
		}
		return nil, fmt.Errorf("%s: get image %s/%s: %w", label, ns, ref.Name, err)
	}
	if image.Status.State != keziov1alpha3.ImageStateReady {
		return nil, &NotReadyError{Reason: fmt.Sprintf("%s: image %s/%s is not Ready yet", label, ns, ref.Name)}
	}

	disk, err := resolveDisk(disks, hints, label)
	if err != nil {
		return nil, err
	}

	var slots []agentapi.DeploySlot
	if withSlots {
		slots, err = b.buildSlots(ctx, image, disk.DeviceName, site)
		if err != nil {
			return nil, err
		}
	}

	return &resolvedImage{ref: ref, image: image, targetDisk: disk.DeviceName, slots: slots}, nil
}

// BuildSnapshot resolves claim's deploy intent for machine into the same
// Snapshot Build returns (ResolvedDisks/HooksHash), without requiring
// anything Build needs only to assemble the agent-facing DeployPlan: a
// seeder Deployment need not be running anywhere (see resolveImage's
// withSlots doc comment), and the OS image's own layout slots are never
// resolved. Callers that only need to detect a hooks-only redeploy
// trigger - the provisioning walk's own currentHooksHash, run against a
// Machine regardless of whether it is actually about to (re)provision -
// must use this instead of Build.
func (b *Builder) BuildSnapshot(ctx context.Context, machine *keziov1alpha3.Machine, claim *keziov1alpha3.MachineClaim) (Snapshot, error) {
	if claim == nil || (claim.Spec.ImageRef == nil && len(claim.Spec.DataImages) == 0) {
		return Snapshot{}, &ValidationError{Reason: "machine has no bound claim with an OS image or data images to deploy"}
	}

	hardware, err := b.getMachineHardware(ctx, machine)
	if err != nil {
		return Snapshot{}, err
	}

	var osImage *resolvedImage
	if claim.Spec.ImageRef != nil {
		osImage, err = b.resolveImage(ctx, machine.Namespace, *claim.Spec.ImageRef, claim.Spec.TargetDisk, hardware.Spec.Disks, "OS image", nil, false)
		if err != nil {
			return Snapshot{}, err
		}
	}

	dataImages := make([]*resolvedImage, len(claim.Spec.DataImages))
	for i, di := range claim.Spec.DataImages {
		label := fmt.Sprintf("dataImages[%d]", i)
		resolved, err := b.resolveImage(ctx, machine.Namespace, di.ImageRef, di.TargetDisk, hardware.Spec.Disks, label, nil, false)
		if err != nil {
			return Snapshot{}, err
		}
		dataImages[i] = resolved
	}

	hooks, err := b.resolveMachineHooks(ctx, machine, claim, osImage)
	if err != nil {
		return Snapshot{}, err
	}

	return b.buildSnapshot(osImage, dataImages, hooks)
}

// diskForSelection returns the disk matching deviceName, for
// diskmatch.CheckDistinct's identity check. deviceName always matches
// exactly one entry in disks: it was itself read off one of them by
// resolveImage just before this is called.
func diskForSelection(disks []keziov1alpha3.MachineHardwareDisk, deviceName string) *keziov1alpha3.MachineHardwareDisk {
	for i := range disks {
		if disks[i].DeviceName == deviceName {
			return &disks[i]
		}
	}
	return nil
}

// resolveMachineHooks resolves every hook attached to this deploy, image
// hooks first: osImage's own Spec.PostHookRefs (resolved in the image's
// own namespace), then claim's own postHookRefs when set, or the shipped
// default hook otherwise - but the default substitutes only when BOTH
// lists are empty and machine deploys an OS image (never all three - see
// MachineClaimSpec.PostHookRefs's doc comment). osImage is the resolved
// OS image, nil for a dataImages-only run: its name, target disk, and
// OSFamily feed the imageName/targetDisk reserved template values and the
// osFamily compatibility check (resolveHook), all left empty/skipped when
// there is no OS image. A dataImages-only run with no postHookRefs on
// either side resolves no hooks at all: per deployer.Deployer.Provision's
// contract it completes at its after-deploy power state with no OS to
// boot, so the shipped default hook's boot-entry builtins (mkswap,
// efibootmgr) would have no OS ESP to act on.
func (b *Builder) resolveMachineHooks(ctx context.Context, machine *keziov1alpha3.Machine, claim *keziov1alpha3.MachineClaim, osImage *resolvedImage) ([]agentapi.ResolvedHook, error) {
	shared, err := mergeParams(imageParamsOf(osImage), claim.Spec.Params)
	if err != nil {
		return nil, &ValidationError{Reason: fmt.Sprintf("merging posthook params: %v", err)}
	}

	var imageName, targetDisk, imageOSFamily, imageNS string
	var imageRefs []keziov1alpha3.NameRef
	if osImage != nil {
		imageName = osImage.image.Name
		targetDisk = osImage.targetDisk
		imageOSFamily = osImage.image.Spec.EffectiveOSFamily()
		imageNS = osImage.image.Namespace
		imageRefs = osImage.image.Spec.PostHookRefs
	}
	// Set after merging so a declared param can never shadow a reserved
	// name.
	shared[keziov1alpha3.PostHookReservedParamMachineName] = machine.Name
	shared[keziov1alpha3.PostHookReservedParamImageName] = imageName
	shared[keziov1alpha3.PostHookReservedParamTargetDisk] = targetDisk

	defaults := deriveBuiltinDefaults(osImage)

	imageHooks, err := b.resolveHooks(ctx, imageNS, imageRefs, shared, defaults, imageOSFamily)
	if err != nil {
		return nil, err
	}

	var machineHooks []agentapi.ResolvedHook
	switch {
	case len(claim.Spec.PostHookRefs) > 0:
		machineHooks, err = b.resolveHooks(ctx, machine.Namespace, claim.Spec.PostHookRefs, shared, defaults, imageOSFamily)
	case len(imageRefs) == 0 && osImage != nil:
		defaultRef := keziov1alpha3.NameRef{Name: posthookdefaults.DefaultFinalizeHookName}
		machineHooks, err = b.resolveHooks(ctx, b.ManagerNamespace, []keziov1alpha3.NameRef{defaultRef}, shared, defaults, imageOSFamily)
	}
	if err != nil {
		return nil, err
	}

	return append(imageHooks, machineHooks...), nil
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
		if s.Role == keziov1alpha3.PartitionRoleESP {
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

// claimEzioMaxUploads returns claim's per-claim max_uploads override
// (claim.spec.ezio.maxUploads), or nil when either claim.spec.ezio or
// that field itself is unset - the "only the fields actually set override
// the layer above" contract seeder.ResolveMaxUploads implements.
func claimEzioMaxUploads(claim *keziov1alpha3.MachineClaim) *int32 {
	if claim.Spec.Ezio == nil {
		return nil
	}
	return claim.Spec.Ezio.MaxUploads
}

// claimEzioMaxConnections is claimEzioMaxUploads's max_connections
// counterpart.
func claimEzioMaxConnections(claim *keziov1alpha3.MachineClaim) *int32 {
	if claim.Spec.Ezio == nil {
		return nil
	}
	return claim.Spec.Ezio.MaxConnections
}

// claimEzioCacheSizeMB, claimEzioAioThreads, and claimEzioPort read
// machine.spec.ezio's daemon-launch overrides straight through to the
// plan: unlike MaxUploads/MaxConnections, these three have no
// cluster-wide operator default layer, so an unset claim override
// carries as nil all the way to the agent, which falls back to an
// auto-computed value (CacheSizeMB) or ezio's own built-in default
// (AioThreads, Port).
func claimEzioCacheSizeMB(claim *keziov1alpha3.MachineClaim) *int32 {
	if claim.Spec.Ezio == nil {
		return nil
	}
	return claim.Spec.Ezio.CacheSizeMB
}

func claimEzioAioThreads(claim *keziov1alpha3.MachineClaim) *int32 {
	if claim.Spec.Ezio == nil {
		return nil
	}
	return claim.Spec.Ezio.AioThreads
}

func claimEzioPort(claim *keziov1alpha3.MachineClaim) *int32 {
	if claim.Spec.Ezio == nil {
		return nil
	}
	return claim.Spec.Ezio.Port
}

// effectiveAfterDeploy returns claim's AfterDeploy, treating an unset
// value as keziov1alpha3.AfterDeployReboot (the CRD schema's own
// default, which a fake or unit-test client that skips defaulting webhooks
// may not have applied).
func effectiveAfterDeploy(claim *keziov1alpha3.MachineClaim) string {
	if claim.Spec.AfterDeploy == "" {
		return keziov1alpha3.AfterDeployReboot
	}
	return claim.Spec.AfterDeploy
}

// buildSnapshot assembles the Snapshot Build returns alongside the plan:
// one DeployRunResolvedDisk per resolved image (OS first, then
// dataImages, in order) and the hash of every resolved hook.
func (b *Builder) buildSnapshot(osImage *resolvedImage, dataImages []*resolvedImage, hooks []agentapi.ResolvedHook) (Snapshot, error) {
	disks := make([]keziov1alpha3.DeployRunResolvedDisk, 0, 1+len(dataImages))
	if osImage != nil {
		disks = append(disks, keziov1alpha3.DeployRunResolvedDisk{ImageRef: osImage.ref, TargetDisk: osImage.targetDisk})
	}
	for _, di := range dataImages {
		disks = append(disks, keziov1alpha3.DeployRunResolvedDisk{ImageRef: di.ref, TargetDisk: di.targetDisk})
	}

	hash, err := hooksHash(hooks)
	if err != nil {
		return Snapshot{}, fmt.Errorf("computing hooks hash: %w", err)
	}

	return Snapshot{ResolvedDisks: disks, HooksHash: hash}, nil
}
