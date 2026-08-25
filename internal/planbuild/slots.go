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

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
	"github.com/tjjh89017/kezio/internal/agentapi"
	"github.com/tjjh89017/kezio/internal/store"
)

// buildSlots resolves image's ordered layout slots into their wire
// DeploySlot form, each written to targetDisk. A slot with ContentRef
// resolves to Torrent (the referenced PartitionContent must be Ready
// with a reachable seeder at site's Site, or this reports
// NotReadyError); a blank swap slot resolves to Swap; any other blank
// slot resolves to Mkfs. site lazily resolves the deploying Machine's own
// seeder placement, shared across every slot of every image this
// Machine's plan builds so it is resolved at most once.
func (b *Builder) buildSlots(ctx context.Context, image *keziov1alpha3.Image, targetDisk string, site *lazySiteResolution) ([]agentapi.DeploySlot, error) {
	slots := make([]agentapi.DeploySlot, 0, len(image.Spec.Layout.Slots))
	for _, s := range image.Spec.Layout.Slots {
		slot, err := b.buildSlot(ctx, image, s, targetDisk, site)
		if err != nil {
			return nil, fmt.Errorf("image %s/%s slot %d: %w", image.Namespace, image.Name, s.Number, err)
		}
		slots = append(slots, slot)
	}
	return slots, nil
}

func (b *Builder) buildSlot(ctx context.Context, image *keziov1alpha3.Image, s keziov1alpha3.ImageSlot, targetDisk string, site *lazySiteResolution) (agentapi.DeploySlot, error) {
	slot := agentapi.DeploySlot{
		Number: s.Number,
		Device: partitionDevicePath(targetDisk, s.Number),
	}

	switch {
	case s.ContentRef != nil:
		torrent, err := b.buildTorrent(ctx, image, *s.ContentRef, site)
		if err != nil {
			return agentapi.DeploySlot{}, err
		}
		slot.Torrent = torrent

	case s.Role == keziov1alpha3.PartitionRoleSwap:
		slot.Swap = &agentapi.DeploySwap{UUID: s.UUID}

	default:
		slot.Mkfs = &agentapi.DeployMkfs{Filesystem: s.FSType}
	}

	return slot, nil
}

// buildTorrent resolves ref's PartitionContent - in image's own
// namespace, the only namespace ImageSlot.ContentRef's doc comment
// allows - into a DeployTorrent: the content must be Ready, the
// Machine's Site must resolve and actually run a seeder (HasSeeder), and
// that seeder must have a reachable pod (resolveTorrentURL), or this
// reports NotReadyError (or, for a Site resolution failure, ValidationError
// - see sitederive.Resolve's own error classification for why that is a
// misconfiguration rather than something to wait out).
func (b *Builder) buildTorrent(ctx context.Context, image *keziov1alpha3.Image, ref keziov1alpha3.NameRef, site *lazySiteResolution) (*agentapi.DeployTorrent, error) {
	ns := resolveNamespace(ref, image.Namespace)

	pc := &keziov1alpha3.PartitionContent{}
	if err := b.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref.Name}, pc); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, &NotReadyError{Reason: fmt.Sprintf("partitioncontent %s/%s not found", ns, ref.Name)}
		}
		return nil, fmt.Errorf("get partitioncontent %s/%s: %w", ns, ref.Name, err)
	}
	if !meta.IsStatusConditionTrue(pc.Status.Conditions, keziov1alpha3.PartitionContentConditionReady) {
		return nil, &NotReadyError{Reason: fmt.Sprintf("partitioncontent %s/%s is not Ready yet", ns, ref.Name)}
	}

	siteRes, err := site.resolve(ctx)
	if err != nil {
		return nil, &ValidationError{Reason: fmt.Sprintf("resolving machine's seeder placement: %v", err)}
	}
	if !siteRes.HasSeeder {
		return nil, &NotReadyError{Reason: fmt.Sprintf("machine's Site %s has no seeding subnet configured, but image %s/%s needs one for partitioncontent %s", siteRes.SiteIdentity, image.Namespace, image.Name, ref.Name)}
	}

	// The info hash is read from status, not derived from the name: a
	// content's name is user-chosen and says nothing about its bytes.
	// status.infoHash is written together with Ready, so a Ready content
	// missing it means this read raced a stale cache rather than a
	// misconfiguration - wait it out.
	if pc.Status.InfoHash == "" {
		return nil, &NotReadyError{Reason: fmt.Sprintf("partitioncontent %s/%s has not reported its info hash yet", ns, ref.Name)}
	}
	hash, err := store.ParseInfoHash(pc.Status.InfoHash)
	if err != nil {
		return nil, fmt.Errorf("partitioncontent %s/%s: status.infoHash is not a valid content hash: %w", ns, ref.Name, err)
	}

	// The seeder Deployment lives in the Image's own namespace (see
	// seederdeploy.Name's doc comment), not necessarily pc's - both are
	// the same namespace under the current ContentRef.Namespace
	// restriction, but resolveTorrentURL is keyed by the Image, so this
	// passes image.Namespace explicitly rather than ns.
	url, err := b.resolveTorrentURL(ctx, image.Namespace, image.Name, siteRes.SiteIdentity, hash)
	if err != nil {
		return nil, err
	}

	return &agentapi.DeployTorrent{URL: url, InfoHash: hash.String()}, nil
}
