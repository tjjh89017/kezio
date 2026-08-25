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

package kezioctl

import (
	"context"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/util/duration"
	"sigs.k8s.io/controller-runtime/pkg/client"

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
)

// ImageListOptions configures ImageList.
type ImageListOptions struct {
	// Namespace is the namespace to list Images in. Ignored when
	// AllNamespaces is true.
	Namespace string
	// AllNamespaces lists Images across every namespace, matching
	// kubectl's -A/--all-namespaces.
	AllNamespaces bool
}

// ImageList fetches the Images `kezioctl image list` reports on.
func ImageList(ctx context.Context, c client.Client, opts ImageListOptions) ([]keziov1alpha3.Image, error) {
	list := &keziov1alpha3.ImageList{}
	var listOpts []client.ListOption
	if !opts.AllNamespaces {
		listOpts = append(listOpts, client.InNamespace(opts.Namespace))
	}
	if err := c.List(ctx, list, listOpts...); err != nil {
		return nil, fmt.Errorf("list Images: %w", err)
	}
	return list.Items, nil
}

// WriteImageList renders images as a tabwriter table to w, matching
// kubectl's general list shape. The NAMESPACE column only appears when
// allNamespaces is true, the same rule kubectl's -A applies.
func WriteImageList(w io.Writer, images []keziov1alpha3.Image, allNamespaces bool) error {
	tw := tabwriter.NewWriter(w, 0, 8, 2, ' ', 0)

	if allNamespaces {
		_, _ = fmt.Fprintln(tw, "NAMESPACE\tNAME\tSTATE\tREADY\tBOOTABLE\tOSFAMILY\tAGE")
	} else {
		_, _ = fmt.Fprintln(tw, "NAME\tSTATE\tREADY\tBOOTABLE\tOSFAMILY\tAGE")
	}

	now := time.Now()
	for _, img := range images {
		state := img.Status.State
		if state == "" {
			state = keziov1alpha3.ImageStatePending
		}
		ready := meta.IsStatusConditionTrue(img.Status.Conditions, keziov1alpha3.ImageConditionReady)
		age := duration.ShortHumanDuration(now.Sub(img.CreationTimestamp.Time))

		if allNamespaces {
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%t\t%t\t%s\t%s\n",
				img.Namespace, img.Name, state, ready, img.Spec.EffectiveBootable(), img.Spec.EffectiveOSFamily(), age)
		} else {
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%t\t%t\t%s\t%s\n",
				img.Name, state, ready, img.Spec.EffectiveBootable(), img.Spec.EffectiveOSFamily(), age)
		}
	}

	return tw.Flush()
}
