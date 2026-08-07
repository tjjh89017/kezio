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
	"context"
	"fmt"
	"net"
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
	"github.com/tjjh89017/kezio/internal/agentapi"
	"github.com/tjjh89017/kezio/internal/seederdeploy"
)

// buildDeployPlan builds machine's DeployPlan, or reports that it is not
// ready yet. A nil plan with a nil error means "not ready - answer
// ActionWait", the same outcome as any other not-yet-resolved state; a
// non-nil error means building hit something unexpected (a Ready Image
// missing the ImageLayout it should have, a store I/O failure) that
// is worth logging server-side, but the caller still answers ActionWait
// for it rather than surfacing a 5xx to the agent - the agent has no
// action to take on a broken plan besides waiting for the next poll to
// find a fixed one.
//
// buildDeployPlan is deliberately not memoized or cached anywhere: it
// reads the Image(s) and their ImageLayout fresh on every call and
// builds every content partition's .torrent bytes fresh (see
// buildImagePlan), so a plan handed to the agent always reflects the
// current cluster state - there is nothing here to invalidate.
func buildDeployPlan(ctx context.Context, c client.Client, cfg Config, machine *keziov1alpha1.Machine) (*agentapi.DeployPlan, error) {
	if machine.Status.State != keziov1alpha1.MachineStateProvisioning {
		return nil, nil
	}
	provisioning := machine.Status.Provisioning
	if provisioning == nil {
		return nil, nil
	}

	plan := &agentapi.DeployPlan{
		SchemaVersion: agentapi.PlanSchemaVersion,
		AfterDeploy:   machine.Spec.EffectiveAfterDeploy(),
		MachineName:   machine.Name,
	}
	plan.Ezio = keziov1alpha1.MergeEzioTuning(cfg.EzioDefaults, machine.Spec.Ezio)

	var osImage *keziov1alpha1.Image
	if machine.Spec.ImageRef != nil {
		if provisioning.Image == nil || provisioning.Image.TargetDisk == "" {
			return nil, nil
		}
		osPlan, image, ready, err := buildImagePlan(ctx, c, machine.Namespace, provisioning.Image.ImageRef, provisioning.Image.TargetDisk, machine.Spec.NetworkSite)
		if err != nil {
			return nil, fmt.Errorf("building OS image plan: %w", err)
		}
		if !ready {
			return nil, nil
		}
		plan.OS = osPlan
		osImage = image
	}

	if len(machine.Spec.DataImages) != len(provisioning.DataImages) {
		// resolveTargetDisks has not (re)recorded status.provisioning for
		// the current spec.dataImages yet.
		return nil, nil
	}
	for i, dataImage := range machine.Spec.DataImages {
		rec := provisioning.DataImages[i]
		if rec.ImageRef != dataImage.ImageRef || rec.TargetDisk == "" {
			return nil, nil
		}
		dataPlan, _, ready, err := buildImagePlan(ctx, c, machine.Namespace, rec.ImageRef, rec.TargetDisk, machine.Spec.NetworkSite)
		if err != nil {
			return nil, fmt.Errorf("building dataImages[%d] plan: %w", i, err)
		}
		if !ready {
			return nil, nil
		}
		plan.DataImages = append(plan.DataImages, *dataPlan)
	}

	if plan.OS == nil && len(plan.DataImages) == 0 {
		// Neither an OS image nor any dataImages resolved to anything -
		// nothing to hand out.
		return nil, nil
	}

	// Hooks attach only to the OS Image and the Machine (see
	// keziov1alpha1.ImageSpec.PostHookRefs and
	// MachineSpec.PostHookRefs' doc comments) - never to a dataImages
	// entry - so only osImage (nil when the Machine deploys no OS image)
	// contributes an image side to the merge.
	var imageRefs []keziov1alpha1.NameRef
	var imageParams *apiextensionsv1.JSON
	var imageName, targetDisk string
	if osImage != nil {
		imageRefs = osImage.Spec.PostHookRefs
		imageParams = osImage.Spec.Params
		imageName = osImage.Name
		targetDisk = provisioning.Image.TargetDisk
	}
	hooks, err := resolveHooks(ctx, c, machine.Namespace, imageRefs, machine.Spec.PostHookRefs, imageParams, machine.Spec.Params, machine.Name, imageName, targetDisk)
	if err != nil {
		return nil, fmt.Errorf("resolving post hooks: %w", err)
	}
	plan.Hooks = hooks

	return plan, nil
}

// buildImagePlan builds imageRef's ImageDeployPlan for the disk already
// resolved as targetDisk. ready is false (with a nil error) when the
// Image is missing, not yet Ready, or has no disk layout recorded -
// every one of those means "not ready yet", not "broken"; buildDeployPlan
// takes the same not-ready path in every case, so this distinguishes
// "keep waiting" (ready=false) from "something is actually wrong"
// (err != nil) for its caller.
// buildImagePlan also returns the fetched Image object itself (nil when
// ready is false), so buildDeployPlan can read the OS image's own
// spec.postHookRefs/spec.params for hook resolution without a second
// fetch of the same object. site is the Machine's spec.networkSite,
// needed to resolve each content partition's seeder URL (see
// buildPlanPartition) - a Machine deploys against exactly one site, so
// every partition across every image plan it builds resolves against
// the same one.
func buildImagePlan(ctx context.Context, c client.Client, defaultNS string, imageRef keziov1alpha1.NameRef, targetDisk, site string) (*agentapi.ImageDeployPlan, *keziov1alpha1.Image, bool, error) {
	ns := keziov1alpha1.ResolveNamespace(imageRef, defaultNS)

	image := &keziov1alpha1.Image{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: imageRef.Name}, image); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil, false, nil
		}
		return nil, nil, false, fmt.Errorf("get image %s/%s: %w", ns, imageRef.Name, err)
	}
	if image.Status.State != keziov1alpha1.ImageStateReady || image.Status.Disk == nil || image.Status.Disk.LayoutRef == nil {
		return nil, nil, false, nil
	}

	layoutName := image.Status.Disk.LayoutRef.Name
	layout := &keziov1alpha1.ImageLayout{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: layoutName}, layout); err != nil {
		return nil, nil, false, fmt.Errorf("get imagelayout %s/%s: %w", ns, layoutName, err)
	}
	sfdiskJSON := layout.Spec.SfdiskJSON

	partitions := make([]agentapi.PlanPartition, 0, len(image.Status.Partitions))
	for _, p := range image.Status.Partitions {
		part, err := buildPlanPartition(ctx, c, ns, image.Name, site, targetDisk, p)
		if err != nil {
			return nil, nil, false, err
		}
		partitions = append(partitions, part)
	}

	return &agentapi.ImageDeployPlan{
		ImageRef:   imageRef,
		Disk:       targetDisk,
		SfdiskJSON: sfdiskJSON,
		Partitions: partitions,
	}, image, true, nil
}

// buildPlanPartition builds one PlanPartition from an
// Image.status.partitions[] entry, classifying it exactly as
// internal/store.ImageLayoutSlot.IsBlank documents for the store's own
// per-slot records: a swap partition uses its UUID, a partition with a
// recorded InfoHash resolves to the seeder pod URL serving site, and
// anything else is a blank partition (mkfs FSType, or unformatted when
// FSType is also empty).
func buildPlanPartition(ctx context.Context, c client.Client, imageNS, imageName, site, targetDisk string, p keziov1alpha1.ImagePartitionStatus) (agentapi.PlanPartition, error) {
	part := agentapi.PlanPartition{
		Number: p.Number,
		Device: agentapi.DevicePartitionPath(targetDisk, p.Number),
		Role:   p.Role,
		FSType: p.FSType,
	}

	switch {
	case p.Role == keziov1alpha1.PartitionRoleSwap && p.UUID != "":
		part.SwapUUID = p.UUID
	case p.InfoHash != "":
		torrentURL, err := resolveSeederTorrentURL(ctx, c, imageNS, imageName, site, p.InfoHash)
		if err != nil {
			return agentapi.PlanPartition{}, fmt.Errorf("image %s/%s partition %d: %w", imageNS, imageName, p.Number, err)
		}
		part.InfoHash = p.InfoHash
		part.TorrentURL = torrentURL
	}

	return part, nil
}

// resolveSeederTorrentURL finds the per-Image, per-site seeder
// Deployment by its deterministic name (seederdeploy.Name is the single
// source of truth for that naming scheme, shared with
// internal/controller's own reconciler, so this reuses it rather than
// re-deriving it), then a ready
// pod within it, and returns the URL that pod serves hash's .torrent
// from (see cmd/seeder's HTTP server). Both lookups failing - no such
// Deployment yet, or a Deployment with no pod that has reported a
// PodIP - are reported as errors: buildDeployPlan surfaces them as a
// plan-build failure, and the real, agent-driven Deployer that requested
// this plan already retries plan builds, so failing loudly now is
// strictly better than handing the agent a content partition with no
// way to fetch its content (see agentapi.PlanPartition.TorrentURL's doc
// comment).
func resolveSeederTorrentURL(ctx context.Context, c client.Client, imageNS, imageName, site, hash string) (string, error) {
	depName := seederdeploy.Name(imageName, site)
	dep := &appsv1.Deployment{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: imageNS, Name: depName}, dep); err != nil {
		if apierrors.IsNotFound(err) {
			return "", fmt.Errorf("no seeder deployment for site %q yet", site)
		}
		return "", fmt.Errorf("get seeder deployment %s/%s: %w", imageNS, depName, err)
	}
	if dep.Spec.Selector == nil {
		return "", fmt.Errorf("seeder deployment %s/%s has no pod selector", imageNS, depName)
	}

	pods := &corev1.PodList{}
	if err := c.List(ctx, pods,
		client.InNamespace(imageNS),
		client.MatchingLabels(dep.Spec.Selector.MatchLabels),
	); err != nil {
		return "", fmt.Errorf("list pods for seeder deployment %s/%s: %w", imageNS, depName, err)
	}

	for i := range pods.Items {
		if ip := pods.Items[i].Status.PodIP; ip != "" {
			host := net.JoinHostPort(ip, strconv.Itoa(int(seederdeploy.TorrentHTTPPort)))
			return fmt.Sprintf("http://%s/torrents/%s", host, hash), nil
		}
	}
	return "", fmt.Errorf("no ready seeder pod yet for site %q (deployment %s/%s)", site, imageNS, depName)
}
