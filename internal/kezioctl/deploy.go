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
	"encoding/json"
	"fmt"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
)

// DeployOptions configures Deploy.
type DeployOptions struct {
	MachineName    string
	Namespace      string
	ImageName      string
	ImageNamespace string
	// DataImages, PostHookRefs and Params are nil when their flag was not
	// given, so Deploy leaves the corresponding spec field untouched
	// rather than clearing it.
	DataImages   []keziov1alpha2.MachineDataImage
	PostHookRefs []keziov1alpha2.NameRef
	// Params merges into spec.params as a flat string map, the cheap
	// subset of ImageSpec/MachineSpec's schemaless params a CLI flag can
	// express honestly.
	Params map[string]string
}

// Deploy implements `kezioctl deploy`: it sets the named Machine's deploy
// payload (spec.imageRef, spec.dataImages, spec.postHookRefs) and returns.
// Everything else - resolving the Image, deciding whether this counts as a
// new deploy intent, creating the DeployRun, running it - is the Machine
// controller's job (see shouldProvision/intentSubsetEqual in
// internal/controller/machine_controller.go): it only acts once the
// Machine is idle (Available or Provisioned) and the referenced Image is
// Ready, and a deploy issued while a run is already in progress simply
// takes effect once that run finishes. This command does not wait for any
// of that; use `kezioctl status` to follow progress.
func Deploy(ctx context.Context, c client.Client, opts DeployOptions) error {
	machine := &keziov1alpha2.Machine{}
	key := client.ObjectKey{Namespace: opts.Namespace, Name: opts.MachineName}
	if err := c.Get(ctx, key, machine); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("machine %s/%s not found", opts.Namespace, opts.MachineName)
		}
		return fmt.Errorf("get Machine %s/%s: %w", opts.Namespace, opts.MachineName, err)
	}

	if opts.ImageName != "" {
		machine.Spec.ImageRef = &keziov1alpha2.NameRef{
			Name:      opts.ImageName,
			Namespace: opts.ImageNamespace,
		}
	}
	if opts.DataImages != nil {
		machine.Spec.DataImages = opts.DataImages
	}
	if opts.PostHookRefs != nil {
		machine.Spec.PostHookRefs = opts.PostHookRefs
	}
	if opts.Params != nil {
		raw, err := json.Marshal(opts.Params)
		if err != nil {
			return fmt.Errorf("encode --param values: %w", err)
		}
		machine.Spec.Params = &apiextensionsv1.JSON{Raw: raw}
	}

	if err := c.Update(ctx, machine); err != nil {
		return fmt.Errorf("update Machine %s/%s: %w", opts.Namespace, opts.MachineName, err)
	}
	return nil
}
