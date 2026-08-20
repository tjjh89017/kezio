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
	"os"

	"github.com/spf13/cobra"
)

// globalFlags holds the flags shared by every kezioctl command that talks
// to the cluster: how to find it (--kubeconfig) and which namespace to
// operate in (--namespace). Kept as a struct (rather than package-level
// vars) so NewRootCmd can be called more than once, which the test suite
// relies on to exercise cobra wiring without global state leaking between
// cases.
type globalFlags struct {
	kubeconfig string
	namespace  string
}

// NewRootCmd builds the kezioctl root command and its full subcommand
// tree. cmd/kezioctl's main package does nothing but call this and
// Execute() the result; every actual behavior lives in this package so it
// can be unit tested without going through cobra.
//
// Only the image verbs are wired up here; machine verbs against the
// v1alpha2 Machine API are a separate, later addition.
func NewRootCmd() *cobra.Command {
	flags := &globalFlags{}

	root := &cobra.Command{
		Use:   "kezioctl",
		Short: "kezioctl manages kezio Images",
		Long: `kezioctl is the command-line client for kezio, a Kubernetes operator
that deploys bare-metal machines with EZIO.

Every command maps onto CRUD operations against kezio's CustomResources
(Image, ...); kubectl remains usable for all the same operations side by
side with kezioctl.`,
		SilenceUsage: true,
	}

	root.PersistentFlags().StringVar(&flags.kubeconfig, "kubeconfig", "",
		"path to a kubeconfig file. Defaults to the KUBECONFIG environment variable, "+
			"then to ~/.kube/config, matching kubectl.")
	root.PersistentFlags().StringVarP(&flags.namespace, "namespace", "n", "",
		"namespace to operate in. Defaults to the current kubeconfig context's namespace, "+
			"then to \"default\".")

	root.AddCommand(newImageCmd(flags))
	return root
}

// resolveNamespace returns flags.namespace if the user set --namespace,
// otherwise kubeconfigNamespace (the current context's namespace, as
// resolved by NewClient/LoadRESTConfig).
func (f *globalFlags) resolveNamespace(kubeconfigNamespace string) string {
	if f.namespace != "" {
		return f.namespace
	}
	return kubeconfigNamespace
}

// Execute runs kezioctl's root command against os.Args, writing errors to
// os.Stderr. cmd/kezioctl's main calls this and exits non-zero on
// failure.
func Execute() {
	if err := NewRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}
