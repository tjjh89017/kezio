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
	"github.com/spf13/cobra"
)

// newStatusCmd builds the top-level `kezioctl status` command.
func newStatusCmd(flags *globalFlags) *cobra.Command {
	var watch bool

	cmd := &cobra.Command{
		Use:   "status <machine-name>",
		Short: "Report a Machine's deploy progress",
		Long: `status reports the named Machine's state and, if it has one, its
current or most recently successful DeployRun's phase - the object that
actually carries deploy progress (Machine itself carries only a
reference to it). Without --watch it prints once and exits; with
--watch it keeps polling and prints a new line only when something
changes, until the DeployRun reaches a terminal phase (Succeeded or
Failed) or this command is interrupted.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, kubeconfigNamespace, err := NewClient(flags.kubeconfig)
			if err != nil {
				return err
			}
			namespace := flags.resolveNamespace(kubeconfigNamespace)

			return Status(cmd.Context(), c, StatusOptions{
				MachineName: args[0],
				Namespace:   namespace,
				Watch:       watch,
				Out:         cmd.OutOrStdout(),
			})
		},
	}

	cmd.Flags().BoolVar(&watch, "watch", false, "keep polling and printing until the deploy reaches a terminal state")
	return cmd
}
