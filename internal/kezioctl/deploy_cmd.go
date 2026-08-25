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
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
)

// newDeployCmd builds the top-level `kezioctl deploy` command.
func newDeployCmd(flags *globalFlags) *cobra.Command {
	var (
		imageName      string
		imageNamespace string
		dataImages     []string
		postHooks      []string
		params         []string
	)

	cmd := &cobra.Command{
		Use:   "deploy <machine-name>",
		Short: "Set a Machine's deploy payload",
		Long: `deploy sets spec.imageRef, and optionally spec.dataImages/
spec.postHookRefs/spec.params, on the named Machine and returns
immediately. It does not itself start anything: the Machine controller
only creates a DeployRun once the Machine is idle (Available or
Provisioned) and, when an OS image is given, that Image is Ready - a
deploy issued while a run is already in progress takes effect once that
run finishes. Use "kezioctl status" to follow progress.

--data-image and --post-hook may be repeated; each takes a bare name in
this Machine's own namespace. --param takes "key=value" and may be
repeated; it replaces spec.params wholesale as a flat string map.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dataImageRefs, err := parseDataImageFlags(dataImages)
			if err != nil {
				return err
			}
			postHookRefs := make([]keziov1alpha3.NameRef, 0, len(postHooks))
			for _, name := range postHooks {
				postHookRefs = append(postHookRefs, keziov1alpha3.NameRef{Name: name})
			}
			paramMap, err := parseParamFlags(params)
			if err != nil {
				return err
			}

			c, kubeconfigNamespace, err := NewClient(flags.kubeconfig)
			if err != nil {
				return err
			}
			namespace := flags.resolveNamespace(kubeconfigNamespace)

			opts := DeployOptions{
				MachineName:    args[0],
				Namespace:      namespace,
				ImageName:      imageName,
				ImageNamespace: imageNamespace,
				Params:         paramMap,
			}
			if cmd.Flags().Changed("data-image") {
				opts.DataImages = dataImageRefs
			}
			if cmd.Flags().Changed("post-hook") {
				opts.PostHookRefs = postHookRefs
			}

			if err := Deploy(cmd.Context(), c, opts); err != nil {
				return err
			}

			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "machine.kezio.kojuro.date/%s deploy payload updated\n", args[0])
			return nil
		},
	}

	cmd.Flags().StringVar(&imageName, "image", "", "name of the Image to deploy as this Machine's OS")
	cmd.Flags().StringVar(&imageNamespace, "image-namespace", "", "namespace of --image, if different from this Machine's own")
	cmd.Flags().StringArrayVar(&dataImages, "data-image", nil, "name of an additional, non-OS Image to deploy alongside the OS image (repeatable)")
	cmd.Flags().StringArrayVar(&postHooks, "post-hook", nil, "name of a PostHook to attach to this Machine's deploy, in order given (repeatable)")
	cmd.Flags().StringArrayVar(&params, "param", nil, `"key=value" to merge into spec.params (repeatable)`)

	return cmd
}

// parseDataImageFlags turns repeated --data-image name into
// MachineDataImage entries. It sets only ImageRef: a per-entry TargetDisk
// hint would need its own flag family, which this command does not offer -
// use "machine set-disk" against a data image's own resolved disk after
// inspection if that is ever needed.
func parseDataImageFlags(names []string) ([]keziov1alpha3.MachineDataImage, error) {
	images := make([]keziov1alpha3.MachineDataImage, 0, len(names))
	for _, name := range names {
		if name == "" {
			return nil, fmt.Errorf("--data-image must not be empty")
		}
		images = append(images, keziov1alpha3.MachineDataImage{ImageRef: keziov1alpha3.NameRef{Name: name}})
	}
	return images, nil
}

// parseParamFlags turns repeated --param key=value into a map, or nil when
// no --param flag was given (so Deploy leaves spec.params untouched).
func parseParamFlags(pairs []string) (map[string]string, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	params := make(map[string]string, len(pairs))
	for _, pair := range pairs {
		key, value, ok := strings.Cut(pair, "=")
		if !ok || key == "" {
			return nil, fmt.Errorf("--param %q is not in \"key=value\" form", pair)
		}
		params[key] = value
	}
	return params, nil
}
