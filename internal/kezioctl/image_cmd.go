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

package kezioctl

import (
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/spf13/cobra"
)

// newImageCmd builds the `kezioctl image` command group.
func newImageCmd(flags *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "image",
		Short: "Manage kezio Images",
	}
	cmd.AddCommand(newImageUploadCmd(flags))
	cmd.AddCommand(newImageListCmd(flags))
	cmd.AddCommand(newImageDeleteCmd(flags))
	return cmd
}

func newImageUploadCmd(flags *globalFlags) *cobra.Command {
	var (
		name          string
		format        string
		server        string
		token         string
		tokenFile     string
		uploadTimeout time.Duration
		size          int64
	)

	cmd := &cobra.Command{
		Use:   "upload <file|->",
		Short: "Upload a disk image and create its Image CR",
		Long: `image upload streams a local file (or, given "-", stdin) to the image
service, then creates the Image CR once the service reports the content's
checksum. The ingest Job does all conversion server-side; this command
never needs qemu or partclone.

The image service's URL and bearer token can be given on the command
line, or left to environment variables/a token file:

  --server value      >  ` + ServerEnvVar + ` environment variable
  --token value        >  ` + TokenEnvVar + ` environment variable
                        >  --token-file content
                        >  ` + TokenFileEnvVar + ` file content

The first source in each list that is set wins.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			file := args[0]

			resolvedServer, err := ResolveServerURL(server)
			if err != nil {
				return err
			}
			resolvedToken, err := ResolveToken(token, tokenFile)
			if err != nil {
				return err
			}

			c, kubeconfigNamespace, err := NewClient(flags.kubeconfig)
			if err != nil {
				return err
			}
			namespace := flags.resolveNamespace(kubeconfigNamespace)

			httpClient := &http.Client{Timeout: uploadTimeout}

			ctx := cmd.Context()
			result, err := ImageUpload(ctx, httpClient, c, ImageUploadOptions{
				File:      file,
				Name:      name,
				Namespace: namespace,
				Format:    format,
				Server:    resolvedServer,
				Token:     resolvedToken,
				Progress:  cmd.ErrOrStderr(),
				Size:      size,
			})
			if err != nil {
				return err
			}

			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "image.kezio.kojuro.date/%s created in namespace %q (checksum %s)\n",
				result.Image.Name, result.Image.Namespace, result.Upload.Checksum)
			return nil
		},
	}

	cmd.Flags().StringVar(&name, "name", "", "name of the Image to create (required)")
	_ = cmd.MarkFlagRequired("name")
	cmd.Flags().StringVar(&format, "format", "",
		"source image format: raw, qcow2, or vmdk. Detected from the file's extension when omitted; "+
			"required when uploading from stdin.")
	cmd.Flags().StringVar(&server, "server", "", "image service base URL (or set "+ServerEnvVar+")")
	cmd.Flags().StringVar(&token, "token", "", "bearer token for the image service (or set "+TokenEnvVar+")")
	cmd.Flags().StringVar(&tokenFile, "token-file", "",
		"path to a file holding the bearer token (or set "+TokenFileEnvVar+")")
	cmd.Flags().DurationVar(&uploadTimeout, "upload-timeout", 0,
		"HTTP timeout for the upload request. 0 disables the timeout, appropriate for large files.")
	cmd.Flags().Int64Var(&size, "size", 0,
		"exact size in bytes of a stdin upload (\"-\"), letting it stream directly instead of first "+
			"spooling to a temp file to learn its size. Ignored (and rejected as an error) for a file-path "+
			"upload, whose size is already known from the file. A wrong value is caught by the image "+
			"service's own size checks, not by this command.")

	return cmd
}

func newImageListCmd(flags *globalFlags) *cobra.Command {
	var allNamespaces bool

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List Images",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, kubeconfigNamespace, err := NewClient(flags.kubeconfig)
			if err != nil {
				return err
			}
			namespace := flags.resolveNamespace(kubeconfigNamespace)

			images, err := ImageList(cmd.Context(), c, ImageListOptions{
				Namespace:     namespace,
				AllNamespaces: allNamespaces,
			})
			if err != nil {
				return err
			}
			return WriteImageList(cmd.OutOrStdout(), images, allNamespaces)
		},
	}

	cmd.Flags().BoolVarP(&allNamespaces, "all-namespaces", "A", false, "list Images across every namespace")
	return cmd
}

func newImageDeleteCmd(flags *globalFlags) *cobra.Command {
	var (
		wait        bool
		waitTimeout time.Duration
	)

	cmd := &cobra.Command{
		Use:   "delete <name>",
		Short: "Delete an Image",
		Long: `image delete removes the named Image's CR. If a Machine still
references it, the Image controller's finalizer blocks the actual
removal; this command does a best-effort check for such Machines and
reports them, rather than leaving the caller to guess why deletion
appears to hang.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, kubeconfigNamespace, err := NewClient(flags.kubeconfig)
			if err != nil {
				return err
			}
			namespace := flags.resolveNamespace(kubeconfigNamespace)

			err = ImageDelete(cmd.Context(), c, ImageDeleteOptions{
				Name:        args[0],
				Namespace:   namespace,
				Wait:        wait,
				WaitTimeout: waitTimeout,
				Status:      cmd.ErrOrStderr(),
			})
			if err != nil {
				return err
			}

			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "image.kezio.kojuro.date/%s deleted\n", args[0])
			return nil
		},
	}

	cmd.Flags().BoolVar(&wait, "wait", false, "wait until the Image is actually removed")
	cmd.Flags().DurationVar(&waitTimeout, "wait-timeout", 5*time.Minute,
		"give up waiting after this long (only with --wait)")
	return cmd
}

// Execute runs kezioctl's root command against os.Args, writing errors
// to os.Stderr. cmd/kezioctl's main calls this and exits non-zero on
// failure.
func Execute() {
	if err := NewRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}
