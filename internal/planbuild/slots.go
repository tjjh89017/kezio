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
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
	"github.com/tjjh89017/kezio/internal/agentapi"
	"github.com/tjjh89017/kezio/internal/store"
)

// buildSlots resolves image's ordered layout slots into their wire
// DeploySlot form, each written to targetDisk. A slot with ContentRef
// resolves to Torrent (the referenced PartitionContent must be Ready
// with a reachable seeder, or this reports NotReadyError); a blank swap
// slot resolves to Swap; any other blank slot resolves to Mkfs.
func (b *Builder) buildSlots(ctx context.Context, image *keziov1alpha2.Image, targetDisk string) ([]agentapi.DeploySlot, error) {
	slots := make([]agentapi.DeploySlot, 0, len(image.Spec.Layout.Slots))
	for _, s := range image.Spec.Layout.Slots {
		slot, err := b.buildSlot(ctx, image, s, targetDisk)
		if err != nil {
			return nil, fmt.Errorf("image %s/%s slot %d: %w", image.Namespace, image.Name, s.Number, err)
		}
		slots = append(slots, slot)
	}
	return slots, nil
}

func (b *Builder) buildSlot(ctx context.Context, image *keziov1alpha2.Image, s keziov1alpha2.ImageSlot, targetDisk string) (agentapi.DeploySlot, error) {
	slot := agentapi.DeploySlot{
		Number: s.Number,
		Device: partitionDevicePath(targetDisk, s.Number),
	}

	switch {
	case s.ContentRef != nil:
		torrent, err := b.buildTorrent(ctx, image, *s.ContentRef)
		if err != nil {
			return agentapi.DeploySlot{}, err
		}
		slot.Torrent = torrent

	case s.Role == keziov1alpha2.PartitionRoleSwap:
		slot.Swap = &agentapi.DeploySwap{UUID: s.UUID}

	default:
		slot.Mkfs = &agentapi.DeployMkfs{Filesystem: s.FSType}
	}

	return slot, nil
}

// buildTorrent resolves ref's PartitionContent - in image's own
// namespace, the only namespace ImageSlot.ContentRef's doc comment
// allows - into a DeployTorrent: the content must be Ready, and its
// seeder must have a reachable pod (resolveTorrentURL), or this reports
// NotReadyError.
func (b *Builder) buildTorrent(ctx context.Context, image *keziov1alpha2.Image, ref keziov1alpha2.NameRef) (*agentapi.DeployTorrent, error) {
	ns := resolveNamespace(ref, image.Namespace)

	pc := &keziov1alpha2.PartitionContent{}
	if err := b.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref.Name}, pc); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, &NotReadyError{Reason: fmt.Sprintf("partitioncontent %s/%s not found", ns, ref.Name)}
		}
		return nil, fmt.Errorf("get partitioncontent %s/%s: %w", ns, ref.Name, err)
	}
	if !meta.IsStatusConditionTrue(pc.Status.Conditions, keziov1alpha2.PartitionContentConditionReady) {
		return nil, &NotReadyError{Reason: fmt.Sprintf("partitioncontent %s/%s is not Ready yet", ns, ref.Name)}
	}

	const namePrefix = "pc-"
	if !strings.HasPrefix(ref.Name, namePrefix) {
		return nil, fmt.Errorf("partitioncontent %s/%s: name does not carry the %q prefix", ns, ref.Name, namePrefix)
	}
	hash, err := store.ParseInfoHash(strings.TrimPrefix(ref.Name, namePrefix))
	if err != nil {
		return nil, fmt.Errorf("partitioncontent %s/%s: name is not a valid content hash: %w", ns, ref.Name, err)
	}

	url, err := b.resolveTorrentURL(ctx, ns, hash)
	if err != nil {
		return nil, err
	}

	return &agentapi.DeployTorrent{URL: url, InfoHash: hash.String()}, nil
}
