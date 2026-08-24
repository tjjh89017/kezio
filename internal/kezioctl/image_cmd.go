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
	"net/http"
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
		imageName     string
		contentPrefix string
		osFamily      string
		server        string
		token         string
		tokenFile     string
		uploadTimeout time.Duration
	)

	cmd := &cobra.Command{
		Use:   "upload <file>",
		Short: "Upload a disk image and create its ImageImport CR",
		Long: `image upload streams a local file to the image service, then creates
an ImageImport CR once the service reports the content's checksum. The
ingest Job does all conversion, slicing, and classification server-side;
this command never needs qemu, partclone, or sfdisk, and never has to
know what is inside the file it uploads.

The import creates one PartitionContent per non-swap partition, named
"<--content-prefix>-p<partition number>", and then the Image binding
them. Both names are rejected if something already holds them: content
and Image are immutable, so an import never writes over one.

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
				File:          file,
				Name:          name,
				Namespace:     namespace,
				ImageName:     imageName,
				ContentPrefix: contentPrefix,
				OSFamily:      osFamily,
				Server:        resolvedServer,
				Token:         resolvedToken,
				Progress:      cmd.ErrOrStderr(),
			})
			if err != nil {
				return err
			}

			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "imageimport.kezio.kojuro.date/%s created in namespace %q (staged at %s, checksum %s); it will create image.kezio.kojuro.date/%s\n",
				result.Import.Name, result.Import.Namespace, result.Upload.URL, result.Upload.Checksum, result.Import.Spec.ImageName)
			return nil
		},
	}

	cmd.Flags().StringVar(&name, "name", "", "name of the ImageImport to create (required)")
	_ = cmd.MarkFlagRequired("name")
	cmd.Flags().StringVar(&imageName, "image-name", "", "name of the Image the import creates (defaults to --name)")
	cmd.Flags().StringVar(&contentPrefix, "content-prefix", "",
		"prefix for the PartitionContent names the import creates, as <prefix>-p<partition number> (defaults to --name)")
	cmd.Flags().StringVar(&osFamily, "os-family", "", "OS family to stamp on the created Image (Linux, Windows, FreeBSD, Other)")
	cmd.Flags().StringVar(&server, "server", "", "image service base URL (or set "+ServerEnvVar+")")
	cmd.Flags().StringVar(&token, "token", "", "bearer token for the image service (or set "+TokenEnvVar+")")
	cmd.Flags().StringVar(&tokenFile, "token-file", "",
		"path to a file holding the bearer token (or set "+TokenFileEnvVar+")")
	cmd.Flags().DurationVar(&uploadTimeout, "upload-timeout", 0,
		"HTTP timeout for the upload request. 0 disables the timeout, appropriate for large files.")

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
		Long: `image delete removes the named Image's CR. Any PartitionContent it
referenced survives deletion while another Image or an active DeployRun
still references it - that is handled entirely server-side by
PartitionContent's own finalizer, so this command does nothing beyond
issuing the delete (and, with --wait, waiting for the Image object
itself to disappear).`,
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
