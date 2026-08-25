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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
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
	DataImages   []keziov1alpha3.MachineDataImage
	PostHookRefs []keziov1alpha3.NameRef
	// Params merges into spec.params as a flat string map, the cheap
	// subset of ImageSpec/MachineClaimSpec's schemaless params a CLI flag
	// can express honestly.
	Params map[string]string
}

// Deploy implements `kezioctl deploy`: it sets the deploy payload
// (spec.imageRef, spec.dataImages, spec.postHookRefs) on the MachineClaim
// named after the target Machine (MachineName + "-claim"), creating that
// claim - bound to MachineName via spec.machineName - if it does not
// exist yet. Everything else - resolving the Image, deciding whether this
// counts as a new deploy intent, creating the DeployRun, running it - is
// the Machine controller's job (see shouldProvision/intentSubsetEqual in
// internal/controller/machine_controller.go): it only acts once the
// Machine is bound to this claim and idle (Available or Provisioned) and
// the referenced Image is Ready, and a deploy issued while a run is
// already in progress simply takes effect once that run finishes. This
// command does not wait for any of that; use `kezioctl status` to follow
// progress.
func Deploy(ctx context.Context, c client.Client, opts DeployOptions) error {
	machine := &keziov1alpha3.Machine{}
	machineKey := client.ObjectKey{Namespace: opts.Namespace, Name: opts.MachineName}
	if err := c.Get(ctx, machineKey, machine); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("machine %s/%s not found", opts.Namespace, opts.MachineName)
		}
		return fmt.Errorf("get Machine %s/%s: %w", opts.Namespace, opts.MachineName, err)
	}

	claimName := opts.MachineName + "-claim"
	claim := &keziov1alpha3.MachineClaim{}
	claimKey := client.ObjectKey{Namespace: opts.Namespace, Name: claimName}
	create := false
	if err := c.Get(ctx, claimKey, claim); err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("get MachineClaim %s/%s: %w", opts.Namespace, claimName, err)
		}
		claim = &keziov1alpha3.MachineClaim{
			ObjectMeta: metav1.ObjectMeta{Name: claimName, Namespace: opts.Namespace},
			Spec:       keziov1alpha3.MachineClaimSpec{MachineName: opts.MachineName},
		}
		create = true
	}

	if opts.ImageName != "" {
		claim.Spec.ImageRef = &keziov1alpha3.NameRef{
			Name:      opts.ImageName,
			Namespace: opts.ImageNamespace,
		}
	}
	if opts.DataImages != nil {
		claim.Spec.DataImages = opts.DataImages
	}
	if opts.PostHookRefs != nil {
		claim.Spec.PostHookRefs = opts.PostHookRefs
	}
	if opts.Params != nil {
		raw, err := json.Marshal(opts.Params)
		if err != nil {
			return fmt.Errorf("encode --param values: %w", err)
		}
		claim.Spec.Params = &apiextensionsv1.JSON{Raw: raw}
	}

	if create {
		if err := c.Create(ctx, claim); err != nil {
			return fmt.Errorf("create MachineClaim %s/%s: %w", opts.Namespace, claimName, err)
		}
		return nil
	}
	if err := c.Update(ctx, claim); err != nil {
		return fmt.Errorf("update MachineClaim %s/%s: %w", opts.Namespace, claimName, err)
	}
	return nil
}
